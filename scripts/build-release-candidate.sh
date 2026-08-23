#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)
die() { printf '%s\n' "build-release-candidate: $*" >&2; exit 1; }

[ "$#" -eq 1 ] || die "usage: build-release-candidate.sh OUTPUT_DIR"
base=$(basename -- "$1")
[ "$base" != . ] && [ "$base" != .. ] && [ -n "$base" ] || die "invalid output directory"
parent=$(CDPATH= cd -- "$(dirname -- "$1")" && pwd -P) || die "output parent does not exist"
output=$parent/$base
[ ! -e "$output" ] && [ ! -L "$output" ] || die "output directory already exists: $output"

pins=$root/build/tool-versions.env
[ -r "$pins" ] || die "missing tool manifest"
while IFS= read -r line || [ -n "$line" ]; do
	case $line in ''|'#'*) continue ;; [A-Z][A-Z0-9_]*=[A-Za-z0-9._:/@+-]*) ;; *) die "invalid tool manifest" ;; esac
done <"$pins"
awk -F= '/^$|^#/ { next } { if (seen[$1]++) exit 1 }' "$pins" || die "duplicate tool manifest key"
pin() { awk -F= -v key="$1" '$1 == key { if (++n == 1) print substr($0, length(key) + 2); else exit 1 } END { exit n != 1 }' "$pins" || die "missing or duplicate pin: $1"; }
buildx_version=$(pin BUILDX_VERSION)
buildkit_image=$(pin BUILDKIT_IMAGE)
goreleaser_version=$(pin GORELEASER_VERSION); goreleaser_version=${goreleaser_version#v}

head=$(git -C "$root" rev-parse --verify HEAD) || die "cannot resolve HEAD"
tag=
for candidate in $(git -C "$root" tag --points-at HEAD); do
	printf '%s' "$candidate" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$' || continue
	[ -z "$tag" ] || die "multiple strict semver tags point at HEAD"
	tag=$candidate
done
[ -n "$tag" ] || die "HEAD must have one strict semver tag"
[ "$(git -C "$root" cat-file -t "refs/tags/$tag")" = tag ] || die "tag must be annotated"
[ "$(git -C "$root" rev-parse "refs/tags/$tag^{}")" = "$head" ] || die "tag peeled commit is not HEAD"
[ -r "$root/.github/release-signers" ] || die "missing release signers"
git -C "$root" -c gpg.format=ssh -c "gpg.ssh.allowedSignersFile=$root/.github/release-signers" verify-tag "$tag" >/dev/null 2>&1 || die "tag signature verification failed"

branch=${OPS_PILOT_PROTECTED_DEFAULT_BRANCH:-}
printf '%s' "$branch" | grep -Eq '^[A-Za-z0-9][A-Za-z0-9._/-]*$' || die "OPS_PILOT_PROTECTED_DEFAULT_BRANCH must be a branch name"
case $branch in HEAD|refs/*|origin/*|*..*|*/./*|*//*) die "OPS_PILOT_PROTECTED_DEFAULT_BRANCH must be a branch name" ;; esac
printf '%s' "$branch" | grep -Eq '^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?(\+[0-9A-Za-z-]+(\.[0-9A-Za-z-]+)*)?$' && die "OPS_PILOT_PROTECTED_DEFAULT_BRANCH must be a branch name"
git -C "$root" remote get-url origin >/dev/null || die "origin remote is required"
remote_ref=refs/remotes/origin/$branch
git -C "$root" fetch --no-tags origin "+refs/heads/$branch:$remote_ref" >/dev/null || die "cannot fetch protected default branch"
git -C "$root" merge-base --is-ancestor "$head" "$remote_ref" || die "tag commit is not reachable from protected default branch"
epoch=$(git -C "$root" show -s --format=%ct "$head") || die "cannot read commit timestamp"
printf '%s' "$epoch" | grep -Eq '^[0-9]+$' || die "invalid commit timestamp"
version=${tag#v}
build_date=$(python3 - "$epoch" <<'PY'
from datetime import datetime, timezone
import sys
print(datetime.fromtimestamp(int(sys.argv[1]), timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"))
PY
) || die "cannot normalize commit timestamp"

buildx_actual=$(docker buildx version 2>&1) || die "Buildx unavailable"
printf '%s\n' "$buildx_actual" | grep -Eq "(^|[^0-9.])$buildx_version([^0-9.]|$)" || die "wrong Buildx version"
goreleaser_actual=$(goreleaser --version 2>&1) || die "GoReleaser unavailable"
printf '%s\n' "$goreleaser_actual" | grep -Eq "(^|[^0-9.])$goreleaser_version([^0-9.]|$)" || die "wrong GoReleaser version"

workbase=$(mktemp -d "${TMPDIR:-/tmp}/ops-pilot-candidate.XXXXXX") || die "cannot create temporary worktree parent"
worktree=$workbase/src
owned_output=0
worktree_added=0
builder_created=0
builder=ops-pilot-candidate-$$-$(date +%s)
success=0
cleanup() {
	[ "$builder_created" -eq 0 ] || docker buildx rm -f "$builder" >/dev/null 2>&1 || true
	[ "$worktree_added" -eq 0 ] || git -C "$root" worktree remove --force "$worktree" >/dev/null 2>&1 || true
	rm -rf "$workbase"
	[ "$success" -eq 1 ] || [ "$owned_output" -eq 0 ] || rm -rf "$output"
}
trap cleanup EXIT HUP INT TERM

git -C "$root" worktree add --detach "$worktree" "$head" >/dev/null || die "cannot create detached build worktree"
worktree_added=1
[ ! -e "$worktree/dist" ] && [ ! -L "$worktree/dist" ] || die "detached build worktree already contains dist"
mkdir "$output"
owned_output=1

docker buildx create --name "$builder" --driver docker-container --driver-opt "image=$buildkit_image" >/dev/null
builder_created=1
docker buildx inspect --bootstrap "$builder" >/dev/null

cd "$worktree"
SOURCE_DATE_EPOCH=$epoch BUILDKIT_MULTI_PLATFORM=1 goreleaser release --clean --skip=publish
[ -d dist ] && [ ! -L dist ] || die "GoReleaser did not produce dist"
mv dist "$output/dist"
SOURCE_DATE_EPOCH=$epoch BUILDKIT_MULTI_PLATFORM=1 docker buildx build --builder "$builder" --platform linux/amd64,linux/arm64 --build-arg "VERSION=$version" --build-arg "COMMIT=$head" --build-arg "BUILD_DATE=$build_date" --provenance=false --sbom=false --output "type=oci,dest=$output/dist/ops-pilot-oci.tar" -f Dockerfile .

python3 - "$output" <<'PY'
import hashlib, json, shutil, sys, tarfile
from pathlib import Path
root = Path(sys.argv[1]); dist = root / "dist"
archive_suffix = ".tar.gz"
matrix = {(os_name, arch) for os_name in ("darwin", "linux") for arch in ("amd64", "arm64")}

def remove_raw(path):
    if not isinstance(path, str) or not path.startswith("dist/"):
        raise SystemExit("build-release-candidate: invalid GoReleaser artifact path")
    target = (dist / path[5:]).resolve()
    if target.parent != dist.resolve() and dist.resolve() not in target.parents:
        raise SystemExit("build-release-candidate: unsafe GoReleaser artifact path")
    if target.is_dir(): shutil.rmtree(target)
    elif target.exists(): target.unlink()

try:
    raw = json.loads((dist / "artifacts.json").read_text())
except (OSError, json.JSONDecodeError) as error:
    raise SystemExit(f"build-release-candidate: invalid GoReleaser artifacts: {error}")
if not isinstance(raw, list): raise SystemExit("build-release-candidate: invalid GoReleaser artifacts")
for record in raw:
    if isinstance(record, dict) and record.get("type") in {"Metadata", "Binary"}:
        remove_raw(record.get("path"))
for name in ("metadata.json", "config.yaml"):
    path = dist / name
    if path.exists(): path.unlink()
for path in sorted(dist.iterdir(), reverse=True):
    if path.is_dir() and path.name.startswith("ops-pilot_"):
        try: path.rmdir()
        except OSError: pass

archives = {}
for record in raw:
    if not isinstance(record, dict): continue
    path = record.get("path")
    if record.get("type") != "Archive" or not isinstance(path, str) or not path.startswith("dist/"):
        continue
    name = path[5:]
    parts = name.removesuffix(archive_suffix).rsplit("_", 2)
    if len(parts) != 3 or not name.endswith(archive_suffix): raise SystemExit("build-release-candidate: invalid release archive")
    version, os_name, arch = parts[0].removeprefix("ops-pilot_"), parts[1], parts[2]
    if not version or (os_name, arch) not in matrix or (os_name, arch) in archives: raise SystemExit("build-release-candidate: invalid release archive matrix")
    archives[(os_name, arch)] = (name, version)
if set(archives) != matrix or len({version for _, version in archives.values()}) != 1:
    raise SystemExit("build-release-candidate: incomplete release archive matrix")
version = next(iter(archives.values()))[1]

allowed_artifact_keys = {"name", "path", "goos", "goarch", "goamd64", "goarm64", "target", "internal_type", "type", "extra"}
def normalized_record(kind, name):
    matches = [record for record in raw if isinstance(record, dict) and record.get("type") == kind and record.get("name") == name]
    if len(matches) != 1: raise SystemExit(f"build-release-candidate: missing {kind} artifact")
    return {key: value for key, value in matches[0].items() if key in allowed_artifact_keys}

records = []
for (os_name, arch), (name, _) in sorted(archives.items()):
    archive = dist / name
    sbom = dist / (name + ".sbom.json")
    if not archive.is_file() or not sbom.is_file(): raise SystemExit("build-release-candidate: missing archive or SBOM")
    try: document = json.loads(sbom.read_text())
    except (OSError, json.JSONDecodeError) as error: raise SystemExit(f"build-release-candidate: invalid SPDX SBOM: {error}")
    if not isinstance(document, dict) or document.get("spdxVersion") != "SPDX-2.3" or not isinstance(document.get("packages"), list) or not isinstance(document.get("relationships"), list):
        raise SystemExit("build-release-candidate: unsupported SPDX SBOM")
    subject = "SPDXRef-Archive-" + hashlib.sha256(name.encode()).hexdigest()[:24]
    if any(isinstance(package, dict) and package.get("SPDXID") == subject for package in document["packages"]):
        raise SystemExit("build-release-candidate: duplicate archive SPDX subject")
    document["packages"].append({"SPDXID": subject, "name": name, "versionInfo": version, "licenseConcluded": "NOASSERTION", "checksums": [{"algorithm": "SHA256", "checksumValue": hashlib.sha256(archive.read_bytes()).hexdigest()}]})
    document["documentDescribes"] = [subject]
    sbom.write_text(json.dumps(document, sort_keys=True, separators=(",", ":")) + "\n")
    archive_record = normalized_record("Archive", name)
    archive_record.update({"name": name, "path": "dist/" + name, "type": "Archive", "goos": os_name, "goarch": arch})
    sbom_name = name + ".sbom.json"
    sbom_record = normalized_record("SBOM", sbom_name)
    sbom_record.update({"name": sbom_name, "path": "dist/" + sbom_name, "type": "SBOM"})
    sbom_record.pop("goos", None); sbom_record.pop("goarch", None)
    records.extend((archive_record, sbom_record))
for name, kind in (("checksums.txt", "Checksum"), ("homebrew/Casks/ops-pilot.rb", "Homebrew Cask")):
    if not (dist / name).is_file(): raise SystemExit(f"build-release-candidate: missing {kind}")
    record = normalized_record(kind, Path(name).name)
    record.update({"name": Path(name).name, "path": "dist/" + name, "type": kind})
    records.append(record)
(dist / "artifacts.json").write_text(json.dumps(sorted(records, key=lambda record: record["path"]), sort_keys=True, separators=(",", ":")) + "\n")

with tarfile.open(dist / "ops-pilot-oci.tar", "r:") as archive:
    source = archive.extractfile("index.json")
    if source is None: raise SystemExit("build-release-candidate: OCI index missing")
    index_bytes = source.read()
index = json.loads(index_bytes); platforms = {}
for descriptor in index.get("manifests", []):
    platform = descriptor.get("platform", {}); key = f"{platform.get('os')}/{platform.get('architecture')}"
    platforms[key] = {name: descriptor[name] for name in ("digest", "size", "mediaType")}
identity = {"index": "sha256:" + hashlib.sha256(index_bytes).hexdigest(), "platforms": dict(sorted(platforms.items()))}
(dist / "oci-identity.json").write_text(json.dumps(identity, sort_keys=True, separators=(",", ":")) + "\n")
rows = []
for path in sorted(dist.rglob("*")):
    if path.is_file():
        data = path.read_bytes(); rows.append({"path": path.relative_to(dist).as_posix(), "size": len(data), "sha256": hashlib.sha256(data).hexdigest()})
(root / "release-manifest.json").write_text(json.dumps(rows, separators=(",", ":")) + "\n")
PY
"$root/scripts/check-release-candidate.py" "$output"
success=1
printf '%s\n' "$output"
