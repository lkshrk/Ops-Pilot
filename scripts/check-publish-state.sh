#!/bin/sh
set -eu

exec python3 - "$@" <<'PY'
"""Fail-closed, read-only release/GHCR retry classifier."""
import hashlib
import json
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path


def die(message):
    raise SystemExit("check-publish-state: " + message)


def read_json(path, label):
    try:
        value = json.loads(Path(path).read_text())
    except (OSError, json.JSONDecodeError) as error:
        die("invalid " + label + ": " + str(error))
    return value


def candidate(root):
    manifest = read_json(root / "release-manifest.json", "release manifest")
    if not isinstance(manifest, list) or not manifest:
        die("invalid release manifest")
    expected = {}
    for row in manifest:
        if not isinstance(row, dict) or set(row) != {"path", "size", "sha256"}:
            die("invalid release manifest entry")
        path, size, digest = row["path"], row["size"], row["sha256"]
        name = Path(path).name if isinstance(path, str) else ""
        file = root / "dist" / path if isinstance(path, str) else None
        if not name or name in expected or not isinstance(size, int) or size < 0 or not isinstance(digest, str) or not re.fullmatch(r"[0-9a-f]{64}", digest) or not file or not file.is_file():
            die("invalid release manifest entry")
        data = file.read_bytes()
        if len(data) != size or hashlib.sha256(data).hexdigest() != digest:
            die("candidate artifact digest mismatch")
        expected[name] = (path, data, digest)
    identity = read_json(root / "dist" / "oci-identity.json", "OCI identity")
    platforms = identity.get("platforms") if isinstance(identity, dict) else None
    index = identity.get("index") if isinstance(identity, dict) else None
    if not isinstance(index, str) or not re.fullmatch(r"sha256:[0-9a-f]{64}", index) or not isinstance(platforms, dict) or set(platforms) != {"linux/amd64", "linux/arm64"}:
        die("invalid OCI identity")
    for platform, descriptor in platforms.items():
        if not isinstance(descriptor, dict) or set(descriptor) != {"digest", "size", "mediaType"} or not isinstance(descriptor["digest"], str) or not re.fullmatch(r"sha256:[0-9a-f]{64}", descriptor["digest"]) or not isinstance(descriptor["size"], int) or descriptor["size"] < 0 or descriptor["mediaType"] != "application/vnd.oci.image.manifest.v1+json":
            die("invalid OCI platform " + platform)
    return expected, index, platforms


class Source:
    def __init__(self):
        self.fixture = os.environ.get("OPS_PILOT_PUBLISH_STATE_FIXTURE_DIR")
        self.api = os.environ.get("GITHUB_API_URL", "https://api.github.com").rstrip("/")
        self.repository = os.environ.get("GITHUB_REPOSITORY", "")
        self.tag = os.environ.get("GITHUB_REF_NAME", "")
        if not self.repository or not self.tag:
            die("GITHUB_REPOSITORY and GITHUB_REF_NAME are required")
        if self.fixture:
            self.fixture = Path(self.fixture)
            if not self.fixture.is_dir(): die("invalid fixture directory")

    def fixture_json(self, name):
        return read_json(self.fixture / name, name)

    def request(self, url, accept="application/vnd.github+json"):
        headers = {"Accept": accept, "User-Agent": "ops-pilot-publish-state"}
        token = os.environ.get("GITHUB_TOKEN")
        if token: headers["Authorization"] = "Bearer " + token
        try:
            with urllib.request.urlopen(urllib.request.Request(url, headers=headers), timeout=30) as response:
                return response.status, response.read()
        except urllib.error.HTTPError as error:
            return error.code, error.read()
        except OSError as error:
            die("GitHub API unavailable: " + str(error))

    def release(self):
        if self.fixture:
            value = self.fixture_json("release.json")
            if isinstance(value, dict) and value.get("status") == 404: return None
            return value
        status, body = self.request(self.api + "/repos/" + self.repository + "/releases/tags/" + urllib.parse.quote(self.tag, safe=""))
        if status == 404: return None
        if status != 200: die("GitHub release lookup is unknown")
        try: return json.loads(body)
        except json.JSONDecodeError: die("invalid GitHub release response")

    def assets(self, release):
        if not isinstance(release, dict) or not isinstance(release.get("id"), int) or release["id"] <= 0 or not isinstance(release.get("assets_url"), str) or not release["assets_url"]:
            die("GitHub release visibility is unknown")
        if release.get("draft") is True:
            die("GitHub release visibility is unknown")
        rows = []
        for page in range(1, 10001):
            if self.fixture:
                path = self.fixture / ("assets-page-%d.json" % page)
                if not path.exists(): die("GitHub asset pagination is incomplete")
                value = self.fixture_json(path.name)
            else:
                separator = "&" if "?" in release["assets_url"] else "?"
                status, body = self.request(release["assets_url"] + separator + "per_page=100&page=%d" % page)
                if status != 200: die("GitHub asset pagination is unknown")
                try: value = json.loads(body)
                except json.JSONDecodeError: die("invalid GitHub assets response")
            if not isinstance(value, list): die("GitHub asset pagination is unknown")
            if not value: return rows
            rows.extend(value)
        die("GitHub asset pagination is incomplete")

    def asset_bytes(self, asset):
        if self.fixture:
            path = self.fixture / ("asset-%d.bin" % asset["id"])
            if not path.is_file(): die("GitHub asset content is unknown")
            return path.read_bytes()
        if not isinstance(asset.get("url"), str) or not asset["url"]:
            die("GitHub asset content is unknown")
        status, body = self.request(asset["url"], "application/octet-stream")
        if status != 200: die("GitHub asset content is unknown")
        return body

    def registry(self):
        if self.fixture:
            value = self.fixture_json("ghcr.json")
            if isinstance(value, dict) and value.get("state") == "absent": return None
            return value
        image = "ghcr.io/%s:%s" % (self.repository.lower(), self.tag)
        crane = os.environ.get("OPS_PILOT_CRANE", "crane")
        import subprocess
        digest = subprocess.run([crane, "digest", image], text=True, capture_output=True)
        if digest.returncode:
            if "MANIFEST_UNKNOWN" in digest.stderr or "not found" in digest.stderr.lower(): return None
            die("GHCR state is unknown")
        manifest = subprocess.run([crane, "manifest", image], text=True, capture_output=True)
        if manifest.returncode: die("GHCR manifest is unknown")
        try: return {"digest": digest.stdout.strip(), "manifest": json.loads(manifest.stdout)}
        except json.JSONDecodeError: die("invalid GHCR manifest")


def main(argv):
    if len(argv) != 2: die("usage: check-publish-state.sh CANDIDATE")
    root = Path(argv[1])
    expected, root_digest, platforms = candidate(root)
    source = Source()
    absent = []
    release = source.release()
    if release is None:
        absent.extend(expected)
    else:
        visible = {}
        for asset in source.assets(release):
            if not isinstance(asset, dict) or not isinstance(asset.get("id"), int) or asset["id"] <= 0 or not isinstance(asset.get("name"), str) or not isinstance(asset.get("size"), int) or asset["size"] < 0 or asset.get("state") != "uploaded":
                die("GitHub asset visibility is unknown")
            if asset["name"] in visible: die("duplicate GitHub release asset")
            visible[asset["name"]] = asset
        if set(visible) - set(expected): die("unexpected GitHub release asset")
        for name, (_, data, digest) in expected.items():
            asset = visible.get(name)
            if asset is None:
                absent.append(name); continue
            if asset["size"] != len(data): die("conflicting GitHub release asset")
            remote = source.asset_bytes(asset)
            if len(remote) != len(data) or hashlib.sha256(remote).hexdigest() != digest:
                die("conflicting GitHub release asset")
    registry = source.registry()
    image = "ghcr.io/%s:%s" % (source.repository.lower(), source.tag)
    if registry is None:
        absent.append(("ghcr", image))
    else:
        if not isinstance(registry, dict) or registry.get("digest") != root_digest or not isinstance(registry.get("manifest"), dict): die("conflicting GHCR version")
        manifests = registry["manifest"].get("manifests")
        if not isinstance(manifests, list): die("GHCR platform state is unknown")
        actual = {}
        for entry in manifests:
            platform = entry.get("platform") if isinstance(entry, dict) else None
            key = platform.get("os", "") + "/" + platform.get("architecture", "") if isinstance(platform, dict) else ""
            if key in actual or key not in platforms: die("conflicting GHCR platform")
            actual[key] = {field: entry.get(field) for field in ("digest", "size", "mediaType")}
        if actual != platforms: die("conflicting GHCR platform")
    for name in sorted(item for item in absent if isinstance(item, str)):
        print("asset\t" + expected[name][0])
    if any(not isinstance(item, str) for item in absent): print("ghcr\t" + image)


try:
    main(sys.argv)
except SystemExit:
    raise
except Exception as error:
    die("unknown state: " + str(error))
PY
