#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)
die() { printf '%s\n' "smoke-release-candidate: $*" >&2; exit 1; }

[ "$#" -eq 2 ] || die "usage: smoke-release-candidate.sh CANDIDATE linux/amd64|linux/arm64"
candidate=$1
platform=$2
case "$platform" in linux/amd64|linux/arm64) ;; *) die "unsupported platform: $platform" ;; esac

"$root/scripts/check-release-candidate.py" "$candidate"
pins=$root/build/tool-versions.env
[ -r "$pins" ] || die "missing tool manifest"
while IFS= read -r line || [ -n "$line" ]; do
	case $line in ''|'#'*) continue ;; [A-Z][A-Z0-9_]*=[A-Za-z0-9._:/@+-]*) ;; *) die "invalid tool manifest" ;; esac
done <"$pins"
awk -F= '/^$|^#/ { next } { if (seen[$1]++) exit 1 }' "$pins" || die "duplicate tool manifest key"
pin() { awk -F= -v key="$1" '$1 == key { if (++n == 1) print substr($0, length(key) + 2); else exit 1 } END { exit n != 1 }' "$pins" || die "missing or duplicate pin: $1"; }
crane_version=$(pin CRANE_VERSION)
registry_image=$(pin REGISTRY_IMAGE)
case "$(uname -m)" in
	x86_64|amd64) crane_arch=amd64; crane_asset=x86_64 ;;
	aarch64|arm64) crane_arch=arm64; crane_asset=arm64 ;;
	*) die "unsupported runner architecture: $(uname -m)" ;;
esac
crane_sha=$(pin "CRANE_LINUX_$(printf '%s' "$crane_arch" | tr a-z A-Z)_SHA256")

tmp=$(mktemp -d "${TMPDIR:-/tmp}/ops-pilot-release-smoke.XXXXXX")
registry=
cleanup() {
	status=$?
	if [ -n "$registry" ]; then docker rm -f "$registry" >/dev/null 2>&1 || :; fi
	rm -rf "$tmp"
	exit "$status"
}
trap cleanup EXIT HUP INT TERM

crane_archive=$tmp/crane.tar.gz
curl --fail --location --proto '=https' --tlsv1.2 --output "$crane_archive" \
	"https://github.com/google/go-containerregistry/releases/download/${crane_version}/go-containerregistry_Linux_${crane_asset}.tar.gz"
actual_sha=$(shasum -a 256 "$crane_archive" | awk '{print $1}')
[ "$actual_sha" = "$crane_sha" ] || die "Crane checksum mismatch"
mkdir "$tmp/crane"
tar -xzf "$crane_archive" -C "$tmp/crane" crane
crane=$tmp/crane/crane
[ -x "$crane" ] || die "Crane archive missing executable"
[ "$("$crane" version)" = "${crane_version#v}" ] || die "unexpected Crane version"

layout=$tmp/layout
python3 - "$candidate/dist/ops-pilot-oci.tar" "$layout" <<'PY'
import posixpath, shutil, sys, tarfile
from pathlib import Path
source, target = Path(sys.argv[1]), Path(sys.argv[2])
try:
    archive = tarfile.open(source, "r:")
except (OSError, tarfile.TarError) as error:
    raise SystemExit(f"invalid OCI tar: {error}")
files, dirs = set(), set()
with archive:
    for member in archive:
        if member.pax_headers or member.sparse or member.type not in (tarfile.REGTYPE, tarfile.AREGTYPE, tarfile.DIRTYPE):
            raise SystemExit("unsafe OCI tar entry")
        raw = member.name
        normalized = posixpath.normpath(raw)
        if not raw or raw.startswith("/") or "\\" in raw or normalized in (".", "..") or normalized.startswith("../") or raw.rstrip("/") != normalized:
            raise SystemExit("unsafe OCI tar path")
        if normalized in files or normalized in dirs:
            raise SystemExit("duplicate OCI tar path")
        parent = posixpath.dirname(normalized)
        while parent:
            if parent in files:
                raise SystemExit("OCI tar writes through file ancestor")
            parent = posixpath.dirname(parent)
        if member.isdir():
            if normalized not in {"blobs", "blobs/sha256"}:
                raise SystemExit("unexpected OCI tar directory")
            dirs.add(normalized)
            continue
        if normalized not in {"oci-layout", "index.json"} and not (normalized.startswith("blobs/sha256/") and len(normalized) == len("blobs/sha256/") + 64 and all(ch in "0123456789abcdef" for ch in normalized.rsplit("/", 1)[1])):
            raise SystemExit("unexpected OCI tar file")
        data = archive.extractfile(member)
        if data is None:
            raise SystemExit("unreadable OCI tar file")
        destination = target / normalized
        destination.parent.mkdir(parents=True, exist_ok=True)
        with destination.open("xb") as output:
            shutil.copyfileobj(data, output)
        files.add(normalized)
if dirs != {"blobs", "blobs/sha256"} or not {"oci-layout", "index.json"} <= files:
    raise SystemExit("incomplete OCI layout")
PY

read_identity() {
	python3 - "$candidate/dist/oci-identity.json" "$platform" <<'PY'
import json, re, sys
try:
    data=json.load(open(sys.argv[1]))
    root=data["index"]; platform=data["platforms"][sys.argv[2]]["digest"]
except (OSError, KeyError, TypeError, json.JSONDecodeError) as error:
    raise SystemExit(f"invalid OCI identity: {error}")
if not all(isinstance(value, str) and re.fullmatch(r"sha256:[0-9a-f]{64}", value) for value in (root, platform)):
    raise SystemExit("invalid OCI identity digest")
print(root)
print(platform)
PY
}
# read_identity prints two validated sha256 digests, one per line, so the
# unquoted split into positional parameters is intended.
# shellcheck disable=SC2086
set -- $(read_identity)
root_digest=$1
platform_digest=$2

registry=$(docker run -d -p 127.0.0.1::5000 "$registry_image")
[ -n "$registry" ] || die "registry did not start"
registry_address=$(docker port "$registry" 5000/tcp)
printf '%s\n' "$registry_address" | grep -Eq '^127\.0\.0\.1:[0-9]+$' || die "invalid registry address"
remote=$registry_address/ops-pilot:smoke
"$crane" push --index --insecure "$layout" "$remote"
[ "$("$crane" digest --insecure "$remote")" = "$root_digest" ] || die "registry root digest mismatch"
[ "$("$crane" digest --insecure --platform "$platform" "$remote")" = "$platform_digest" ] || die "registry platform digest mismatch"

docker pull --platform "$platform" "$remote@$platform_digest"
docker run --rm --pull=never --platform "$platform" --entrypoint /bin/sh "$remote@$platform_digest" -ceu '
test "$(id -u)" -ne 0
git --version
! command -v go
! test -e /usr/local/go/bin/go
touch /state/smoke /checkout/smoke /cache/smoke
'
printf '%s\n' 'smoke-release-candidate: PASS'
