#!/usr/bin/env python3
"""Offline, no-extraction verifier for a release candidate."""
import hashlib
import gzip
import json
import os
import posixpath
import re
import sys
import tarfile
import zlib
from pathlib import Path

ARCHIVE = re.compile(r"^ops-pilot_(?P<version>(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)\.(?:0|[1-9][0-9]*)(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?(?:\+[0-9A-Za-z][0-9A-Za-z.-]*)?)_(?P<os>darwin|linux)_(?P<arch>amd64|arm64)\.tar\.gz$")
HEX = re.compile(r"^[0-9a-f]{64}$")
OCI_MANIFEST = "application/vnd.oci.image.manifest.v1+json"
OCI_CONFIG = "application/vnd.oci.image.config.v1+json"
OCI_INDEX = "application/vnd.oci.image.index.v1+json"
OCI_LAYERS = {
    "application/vnd.oci.image.layer.v1.tar",
    "application/vnd.oci.image.layer.v1.tar+gzip",
    "application/vnd.oci.image.layer.v1.tar+zstd",
}
RELEASE_ARCHIVE_FILES = {
    "ops-pilot": 0o755,
    "README.md": 0o644,
    "docs/install.md": 0o644,
    "docs/configuration.md": 0o644,
    "docs/cli.md": 0o644,
}


def fail(message):
    raise ValueError(message)


def read_json(data, name):
    try:
        return json.loads(data)
    except (UnicodeDecodeError, json.JSONDecodeError) as err:
        fail(f"invalid JSON {name}: {err}")


def sha(data):
    return hashlib.sha256(data).hexdigest()


def descriptor(data, desc, allowed, where):
    if not isinstance(desc, dict) or set(desc) - {"mediaType", "digest", "size", "platform", "annotations"}:
        fail(f"invalid descriptor {where}")
    media = desc.get("mediaType")
    digest = desc.get("digest")
    size = desc.get("size")
    if media not in allowed or not isinstance(digest, str) or not digest.startswith("sha256:") or not HEX.fullmatch(digest[7:]) or not isinstance(size, int) or size < 0:
        fail(f"invalid descriptor {where}")
    if len(data) != size or sha(data) != digest[7:]:
        fail(f"descriptor digest or size mismatch {where}")
    return digest


def safe_name(name, is_dir):
    if not isinstance(name, str) or not name or "\\" in name or name.startswith("/"):
        fail("unsafe OCI tar path")
    normal = posixpath.normpath(name)
    if normal in (".", "..") or normal.startswith("../") or normal != name.rstrip("/"):
        fail("non-normalized OCI tar path")
    if is_dir and name not in (normal, normal + "/"):
        fail("non-normalized OCI directory path")
    if not is_dir and name != normal:
        fail("non-normalized OCI file path")
    return normal


def validate_release_archive(path):
    seen = set()
    try:
        with gzip.open(path, "rb") as compressed:
            with tarfile.open(fileobj=compressed, mode="r|") as archive:
                archive.ignore_zeros = True
                for member in archive:
                    name = safe_name(member.name, False)
                    if member.pax_headers or member.sparse is not None or member.type not in (tarfile.REGTYPE, tarfile.AREGTYPE) or member.linkname:
                        fail("unsupported release archive entry")
                    if name in seen or any(parent in seen for parent in Path(name).parents if parent.as_posix() != ".") or any(old.startswith(name + "/") for old in seen):
                        fail("duplicate or conflicting release archive path")
                    if name not in RELEASE_ARCHIVE_FILES:
                        fail("unexpected release archive entry")
                    if member.mode != RELEASE_ARCHIVE_FILES[name]:
                        fail("wrong release archive entry mode")
                    source = archive.extractfile(member)
                    if source is None:
                        fail("unreadable release archive entry")
                    size = 0
                    while chunk := source.read(1024 * 1024):
                        size += len(chunk)
                    if size != member.size:
                        fail("truncated release archive entry")
                    seen.add(name)
            while compressed.read(1024 * 1024):
                pass
    except (tarfile.TarError, OSError, EOFError, zlib.error) as err:
        fail(f"invalid release archive: {err}")
    if seen != set(RELEASE_ARCHIVE_FILES):
        fail("incomplete release archive")


def oci_files(path):
    files, dirs, names = {}, set(), []
    try:
        tar = tarfile.open(path, "r:")
    except (tarfile.TarError, OSError) as err:
        fail(f"invalid OCI tar: {err}")
    with tar:
        for member in tar:
            if member.pax_headers or member.sparse or member.type not in (tarfile.REGTYPE, tarfile.AREGTYPE, tarfile.DIRTYPE):
                fail("unsupported OCI tar entry")
            is_dir = member.isdir()
            name = safe_name(member.name, is_dir)
            if names and name <= names[-1]:
                fail("OCI tar entries are not sorted and unique")
            names.append(name)
            parent = posixpath.dirname(name)
            while parent:
                if parent in files:
                    fail("OCI tar writes through file ancestor")
                parent = posixpath.dirname(parent)
            if name in files or name in dirs:
                fail("duplicate OCI tar path")
            if is_dir:
                dirs.add(name)
                continue
            source = tar.extractfile(member)
            if source is None:
                fail("unreadable OCI tar file")
            data = source.read()
            if len(data) != member.size:
                fail("truncated OCI tar file")
            files[name] = data
    required_dirs = {"blobs", "blobs/sha256"}
    if not dirs <= required_dirs:
        fail("unexpected OCI tar directories")
    if set(files) & required_dirs or set(files) - {"oci-layout", "index.json"} - {f"blobs/sha256/{x}" for x in [n[13:] for n in files if n.startswith("blobs/sha256/")]}:
        fail("unexpected OCI tar file")
    if "oci-layout" not in files or "index.json" not in files or not any(n.startswith("blobs/sha256/") for n in files):
        fail("incomplete OCI tar")
    for name in files:
        if name.startswith("blobs/sha256/") and not HEX.fullmatch(name[13:]):
            fail("invalid OCI blob name")
    return files


def validate_oci(tar_path):
    files = oci_files(tar_path)
    layout = read_json(files["oci-layout"], "oci-layout")
    if layout != {"imageLayoutVersion": "1.0.0"}:
        fail("invalid oci-layout")
    index = read_json(files["index.json"], "index")
    if not isinstance(index, dict) or set(index) - {"schemaVersion", "mediaType", "manifests", "annotations"} or index.get("schemaVersion") != 2 or index.get("mediaType", OCI_INDEX) != OCI_INDEX or not isinstance(index.get("manifests"), list) or len(index["manifests"]) != 2:
        fail("invalid OCI index")
    used = set()
    platforms = {}
    for d in index["manifests"]:
        if "subject" in d or "artifactType" in d:
            fail("OCI attestation or artifact descriptor")
        platform = d.get("platform")
        if not isinstance(platform, dict) or set(platform) - {"os", "architecture", "variant", "os.version", "os.features", "features"} or platform.get("os") != "linux" or platform.get("architecture") not in {"amd64", "arm64"} or set(platform) - {"os", "architecture"}:
            fail("invalid OCI platform")
        key = f"linux/{platform['architecture']}"
        if key in platforms:
            fail("duplicate OCI platform")
        digest = d.get("digest", "")
        blob_name = "blobs/sha256/" + digest[7:] if isinstance(digest, str) and digest.startswith("sha256:") else ""
        if blob_name not in files:
            fail("missing OCI manifest blob")
        descriptor(files[blob_name], d, {OCI_MANIFEST}, key)
        used.add(blob_name)
        manifest = read_json(files[blob_name], key)
        if not isinstance(manifest, dict) or set(manifest) - {"schemaVersion", "mediaType", "config", "layers", "annotations"} or manifest.get("schemaVersion") != 2 or manifest.get("mediaType", OCI_MANIFEST) != OCI_MANIFEST or not isinstance(manifest.get("config"), dict) or not isinstance(manifest.get("layers"), list) or not manifest["layers"]:
            fail("invalid OCI manifest")
        cfg_desc = manifest["config"]
        cfg_digest = cfg_desc.get("digest", "") if isinstance(cfg_desc, dict) else ""
        cfg_name = "blobs/sha256/" + cfg_digest[7:] if isinstance(cfg_digest, str) and cfg_digest.startswith("sha256:") else ""
        if cfg_name not in files:
            fail("missing OCI config blob")
        descriptor(files[cfg_name], cfg_desc, {OCI_CONFIG}, key + " config")
        used.add(cfg_name)
        cfg = read_json(files[cfg_name], key + " config")
        if not isinstance(cfg, dict) or cfg.get("os") != "linux" or cfg.get("architecture") != platform["architecture"]:
            fail("OCI config platform mismatch")
        for n, layer in enumerate(manifest["layers"]):
            layer_digest = layer.get("digest", "") if isinstance(layer, dict) else ""
            layer_name = "blobs/sha256/" + layer_digest[7:] if isinstance(layer_digest, str) and layer_digest.startswith("sha256:") else ""
            if layer_name not in files:
                fail("missing OCI layer blob")
            descriptor(files[layer_name], layer, OCI_LAYERS, f"{key} layer {n}")
            used.add(layer_name)
        platforms[key] = d
    if set(platforms) != {"linux/amd64", "linux/arm64"}:
        fail("wrong OCI platform set")
    blob_paths = {name for name in files if name.startswith("blobs/sha256/")}
    if blob_paths != used:
        fail("orphan OCI blob")
    return {"index": "sha256:" + sha(files["index.json"]), "platforms": {key: {"digest": d["digest"], "size": d["size"], "mediaType": d["mediaType"]} for key, d in sorted(platforms.items())}}


def canonical_json(data, name):
    value = read_json(data, name)
    return json.dumps(value, sort_keys=True, separators=(",", ":"))


def sbom_fallback(data, archive, archive_digest):
    """The only allowed non-byte-reproducible SBOM comparison surface."""
    doc = read_json(data, archive + " SBOM")
    if not isinstance(doc, dict) or doc.get("spdxVersion") != "SPDX-2.3" or not isinstance(doc.get("SPDXID"), str) or not isinstance(doc.get("packages"), list) or not isinstance(doc.get("relationships"), list):
        fail("unsupported or partial SBOM format")
    packages = []
    subjects = []
    described = doc.get("documentDescribes", [])
    if not isinstance(described, list) or len(described) != 1 or not isinstance(described[0], str): fail("invalid SBOM subject")
    for package in doc["packages"]:
        if not isinstance(package, dict) or not all(isinstance(package.get(k), str) for k in ("SPDXID", "name", "versionInfo", "licenseConcluded")):
            fail("partial SBOM package")
        purl = ""
        refs = package.get("externalRefs", [])
        if not isinstance(refs, list): fail("invalid SBOM package references")
        for ref in refs:
            if isinstance(ref, dict) and ref.get("referenceType") == "purl" and isinstance(ref.get("referenceLocator"), str): purl = ref["referenceLocator"]
        identity = (purl, package["name"], package["versionInfo"], package["licenseConcluded"])
        packages.append(identity)
        if package["SPDXID"] == described[0]:
            checksums = package.get("checksums", [])
            if not isinstance(checksums, list): fail("invalid SBOM subject checksum")
            digests = sorted((c.get("algorithm"), c.get("checksumValue")) for c in checksums if isinstance(c, dict) and isinstance(c.get("algorithm"), str) and isinstance(c.get("checksumValue"), str))
            subjects.append((package["name"], tuple(digests)))
    if len(subjects) != 1 or subjects[0][0] != archive:
        fail("SBOM subject does not identify archive")
    if [value for algorithm, value in subjects[0][1] if algorithm == "SHA256"] != [archive_digest]:
        fail("SBOM subject checksum does not match archive")
    relationships = []
    for relationship in doc["relationships"]:
        if not isinstance(relationship, dict) or not all(isinstance(relationship.get(k), str) for k in ("spdxElementId", "relationshipType", "relatedSpdxElement")):
            fail("partial SBOM relationship")
        relationships.append((relationship["spdxElementId"], relationship["relationshipType"], relationship["relatedSpdxElement"]))
    return json.dumps({"format": "SPDX", "version": doc["spdxVersion"], "subjects": sorted(subjects), "packages": sorted(packages), "relationships": sorted(relationships)}, separators=(",", ":"))


def artifacts_metadata(data, archives):
    records = read_json(data, "artifacts")
    if not isinstance(records, list) or len(records) != 10:
        fail("invalid artifacts record count")
    known = {"name", "path", "goos", "goarch", "goamd64", "goarm64", "target", "internal_type", "type", "extra"}
    by_type = {"Archive": [], "SBOM": [], "Checksum": [], "Homebrew Cask": []}
    for record in records:
        if not isinstance(record, dict) or set(record) - known or not isinstance(record.get("type"), str) or record["type"] not in by_type or not isinstance(record.get("name"), str) or not isinstance(record.get("path"), str):
            fail("invalid artifacts record")
        if record.get("internal_type") is not None and not isinstance(record["internal_type"], int): fail("invalid artifacts record")
        if record.get("extra") is not None and not isinstance(record["extra"], dict): fail("invalid artifacts record")
        by_type[record["type"]].append(record)
    if len(by_type["Archive"]) != 4 or len(by_type["SBOM"]) != 4 or len(by_type["Checksum"]) != 1 or len(by_type["Homebrew Cask"]) != 1:
        fail("wrong artifacts type matrix")
    seen = set()
    for (goos, goarch), name in archives.items():
        matches = [r for r in by_type["Archive"] if r["name"] == name]
        if len(matches) != 1 or matches[0].get("path") != "dist/" + name or matches[0].get("goos") != goos or matches[0].get("goarch") != goarch:
            fail("invalid archive artifact")
        if name in seen: fail("duplicate archive artifact")
        seen.add(name)
        sbom = name + ".sbom.json"
        matches = [r for r in by_type["SBOM"] if r["name"] == sbom]
        if len(matches) != 1 or matches[0].get("path") != "dist/" + sbom or "goos" in matches[0] or "goarch" in matches[0]:
            fail("invalid SBOM artifact")
    checksum, cask = by_type["Checksum"][0], by_type["Homebrew Cask"][0]
    if checksum["name"] != "checksums.txt" or checksum["path"] != "dist/checksums.txt" or cask["name"] != "ops-pilot.rb" or cask["path"] != "dist/homebrew/Casks/ops-pilot.rb":
        fail("invalid checksum or cask artifact")


def validate_candidate(raw):
    root = Path(raw)
    if not root.is_dir() or root.is_symlink() or any(part in ("", ".", "..") for part in root.parts):
        fail("candidate must be a real directory")
    root = root.resolve()
    manifest_path = root / "release-manifest.json"
    dist = root / "dist"
    if not manifest_path.is_file() or not dist.is_dir() or dist.is_symlink():
        fail("candidate layout must contain release-manifest.json and dist")
    actual = {}
    for path in dist.rglob("*"):
        if path.is_symlink():
            fail("candidate contains symlink")
        if path.is_file():
            rel = path.relative_to(dist).as_posix()
            actual[rel] = path.read_bytes()
        elif not path.is_dir():
            fail("candidate contains unsupported path")
    manifest = read_json(manifest_path.read_bytes(), "release-manifest")
    if not isinstance(manifest, list): fail("release-manifest must be a list")
    expected = {}
    previous = ""
    for item in manifest:
        if not isinstance(item, dict) or set(item) != {"path", "size", "sha256"}: fail("invalid release-manifest record")
        name, size, digest = item["path"], item["size"], item["sha256"]
        if not isinstance(name, str) or name != posixpath.normpath(name) or name.startswith("/") or name.startswith("../") or not isinstance(size, int) or size < 0 or not isinstance(digest, str) or not HEX.fullmatch(digest) or not name > previous: fail("noncanonical release-manifest")
        previous = name; expected[name] = (size, digest)
    if set(expected) != set(actual): fail("release-manifest file set mismatch")
    for name, data in actual.items():
        if expected[name] != (len(data), sha(data)): fail("release-manifest digest mismatch")
    archives = {}
    versions = set()
    for name in actual:
        match = ARCHIVE.fullmatch(name)
        if match:
            archives[(match["os"], match["arch"])] = name
            versions.add(match["version"])
    if len(archives) != 4 or set(archives) != {(a,b) for a in ("darwin","linux") for b in ("amd64","arm64")}:
        fail("wrong release archives")
    if len(versions) != 1:
        fail("release archives have inconsistent versions")
    required = {"checksums.txt", "artifacts.json", "ops-pilot-oci.tar", "oci-identity.json", "homebrew/Casks/ops-pilot.rb"}
    required |= set(archives.values()) | {name + ".sbom.json" for name in archives.values()}
    allowed = required
    if set(actual) != allowed: fail("unexpected candidate artifact")
    for name in archives.values(): validate_release_archive(root / "dist" / name)
    checks = actual["checksums.txt"].decode("utf-8", "strict").splitlines()
    parsed = {}
    for line in checks:
        fields = line.split()
        if len(fields) != 2 or not HEX.fullmatch(fields[0]) or fields[1] in parsed: fail("invalid checksum file")
        parsed[fields[1]] = fields[0]
    if set(parsed) != set(archives.values()) or any(parsed[n] != sha(actual[n]) for n in parsed): fail("checksum file mismatch")
    artifacts_metadata(actual["artifacts.json"], archives)
    for name in archives.values(): sbom_fallback(actual[name + ".sbom.json"], name, sha(actual[name]))
    identity = canonical_json(actual["oci-identity.json"], "OCI identity")
    oci = validate_oci(root / "dist/ops-pilot-oci.tar")
    if identity != json.dumps(oci, sort_keys=True, separators=(",", ":")): fail("OCI identity mismatch")
    return {"archives": {f"{a}/{b}": n for (a,b),n in sorted(archives.items())}, "checksums": sha(actual["checksums.txt"]), "cask": sha(actual["homebrew/Casks/ops-pilot.rb"]), "oci": oci, "sbom_bytes": {n: sha(actual[n + ".sbom.json"]) for n in sorted(archives.values())}, "sbom_fallback": {n: sbom_fallback(actual[n + ".sbom.json"], n, sha(actual[n])) for n in sorted(archives.values())}}


def main(args):
    if len(args) == 1:
        validate_candidate(args[0]); return
    if len(args) == 3 and args[1] == "--compare":
        left, right = validate_candidate(args[0]), validate_candidate(args[2])
        if left == right: return
        left.pop("sbom_bytes"); right.pop("sbom_bytes")
        if left != right: fail("release candidates are not reproducible")
        return
    fail("usage: check-release-candidate.py CANDIDATE [--compare OTHER]")


if __name__ == "__main__":
    try:
        main(sys.argv[1:])
    except ValueError as err:
        print(f"check-release-candidate: {err}", file=sys.stderr)
        sys.exit(1)
