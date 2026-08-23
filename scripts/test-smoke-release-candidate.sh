#!/bin/sh
set -eu

source_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/ops-pilot-release-smoke.XXXXXX")
trap 'rm -rf "$tmp"' EXIT HUP INT TERM
repo=$tmp/repo
mkdir -p "$repo/scripts" "$repo/build" "$tmp/bin" "$tmp/candidate/dist"
cp "$source_root/scripts/smoke-release-candidate.sh" "$repo/scripts/"
chmod +x "$repo/scripts/smoke-release-candidate.sh"

make_candidate() {
	python3 - "$1" <<'PY'
import hashlib, io, json, sys, tarfile
from pathlib import Path
root=Path(sys.argv[1]); dist=root/'dist'
def digest(data): return 'sha256:'+hashlib.sha256(data).hexdigest()
def descriptor(data, media, **extra): return {'mediaType':media, 'digest':digest(data), 'size':len(data), **extra}
blobs={}; manifests=[]
for arch in ('amd64','arm64'):
    layer=(arch+' layer').encode(); blobs[digest(layer)[7:]]=layer
    config=json.dumps({'architecture':arch,'os':'linux'},separators=(',',':')).encode(); blobs[digest(config)[7:]]=config
    manifest=json.dumps({'schemaVersion':2,'mediaType':'application/vnd.oci.image.manifest.v1+json','config':descriptor(config,'application/vnd.oci.image.config.v1+json'),'layers':[descriptor(layer,'application/vnd.oci.image.layer.v1.tar+gzip')]},separators=(',',':')).encode()
    blobs[digest(manifest)[7:]]=manifest
    manifests.append(descriptor(manifest,'application/vnd.oci.image.manifest.v1+json',platform={'os':'linux','architecture':arch}))
index=json.dumps({'schemaVersion':2,'mediaType':'application/vnd.oci.image.index.v1+json','manifests':manifests},separators=(',',':')).encode()
with tarfile.open(dist/'ops-pilot-oci.tar','w') as out:
    for name in ('blobs','blobs/sha256'):
        entry=tarfile.TarInfo(name); entry.type=tarfile.DIRTYPE; out.addfile(entry)
    for name,data in sorted(blobs.items()):
        entry=tarfile.TarInfo('blobs/sha256/'+name); entry.size=len(data); out.addfile(entry,io.BytesIO(data))
    for name,data in (('index.json',index),('oci-layout',b'{"imageLayoutVersion":"1.0.0"}')):
        entry=tarfile.TarInfo(name); entry.size=len(data); out.addfile(entry,io.BytesIO(data))
identity={'index':digest(index),'platforms':{}}
for manifest in manifests:
    identity['platforms']['linux/'+manifest['platform']['architecture']]={key:manifest[key] for key in ('digest','size','mediaType')}
(dist/'oci-identity.json').write_text(json.dumps(identity,sort_keys=True,separators=(',',':')))
PY
}

make_candidate "$tmp/candidate"
cat >"$repo/scripts/check-release-candidate.py" <<'PY'
#!/usr/bin/env python3
import os, sys
with open(os.environ["OPS_PILOT_TEST_LOG"], "a") as log:
    print("validator " + " ".join(sys.argv[1:]), file=log)
assert len(sys.argv) == 2
PY
chmod +x "$repo/scripts/check-release-candidate.py"
cat >"$tmp/crane" <<'SH'
#!/bin/sh
set -eu
printf 'crane %s\n' "$*" >>"$OPS_PILOT_TEST_LOG"
case "$1" in
  version) printf '%s\n' "${OPS_PILOT_TEST_CRANE_VERSION:-v0.21.7}" ;;
  push) exit 0 ;;
  digest)
    case " $* " in
      *' --platform linux/amd64 '*) printf '%s\n' "$OPS_PILOT_TEST_AMD64_DIGEST" ;;
      *' --platform linux/arm64 '*) printf '%s\n' "$OPS_PILOT_TEST_ARM64_DIGEST" ;;
      *) printf '%s\n' "$OPS_PILOT_TEST_ROOT_DIGEST" ;;
    esac ;;
  *) exit 9 ;;
esac
SH
chmod +x "$tmp/crane"
tar -C "$tmp" -czf "$tmp/crane.tar.gz" crane
sha=$(shasum -a 256 "$tmp/crane.tar.gz" | awk '{print $1}')
cat >"$repo/build/tool-versions.env" <<EOF
CRANE_VERSION=v0.21.7
CRANE_LINUX_AMD64_SHA256=$sha
CRANE_LINUX_ARM64_SHA256=$sha
REGISTRY_IMAGE=docker.io/library/registry:2@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373
EOF
cat >"$tmp/bin/uname" <<'SH'
#!/bin/sh
printf '%s\n' "${OPS_PILOT_TEST_UNAME:-x86_64}"
SH
cat >"$tmp/bin/curl" <<'SH'
#!/bin/sh
set -eu
while [ "$#" -gt 0 ]; do case "$1" in --output) output=$2; shift 2 ;; *) shift ;; esac; done
cp "$OPS_PILOT_TEST_CRANE_ARCHIVE" "$output"
SH
cat >"$tmp/bin/docker" <<'SH'
#!/bin/sh
set -eu
printf 'docker %s\n' "$*" >>"$OPS_PILOT_TEST_LOG"
case "$1" in
  run)
    case " $* " in
      *' -d -p 127.0.0.1::5000 docker.io/library/registry:2@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373 '*) printf '%s\n' registry-id ;;
      *' --rm --pull=never --platform linux/'*) exit 0 ;;
      *) exit 9 ;;
    esac ;;
  pull) case " $* " in *' --platform linux/'*' 127.0.0.1:50123/ops-pilot:smoke@sha256:'*) exit 0 ;; *) exit 9 ;; esac ;;
  port) printf '%s\n' '127.0.0.1:50123' ;;
  rm) exit 0 ;;
  *) exit 9 ;;
esac
SH
chmod +x "$tmp/bin/"*

identity=$tmp/candidate/dist/oci-identity.json
root_digest=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["index"])' "$identity")
amd64_digest=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["platforms"]["linux/amd64"]["digest"])' "$identity")
arm64_digest=$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["platforms"]["linux/arm64"]["digest"])' "$identity")
log=$tmp/log
run() {
	PATH="$tmp/bin:$PATH" OPS_PILOT_TEST_LOG=$log OPS_PILOT_TEST_CRANE_ARCHIVE=$tmp/crane.tar.gz OPS_PILOT_TEST_ROOT_DIGEST=$root_digest OPS_PILOT_TEST_AMD64_DIGEST=$amd64_digest OPS_PILOT_TEST_ARM64_DIGEST=$arm64_digest "$repo/scripts/smoke-release-candidate.sh" "$@"
}

if ! run "$tmp/candidate" linux/amd64; then echo 'expected smoke success' >&2; exit 1; fi
grep -F "validator $tmp/candidate" "$log" >/dev/null
grep -F 'crane push --index --insecure ' "$log" | grep -F '127.0.0.1:50123/ops-pilot:smoke' >/dev/null
grep -F "crane digest --insecure 127.0.0.1:50123/ops-pilot:smoke" "$log" >/dev/null
grep -F "crane digest --insecure --platform linux/amd64 127.0.0.1:50123/ops-pilot:smoke" "$log" >/dev/null
grep -F "docker pull --platform linux/amd64 127.0.0.1:50123/ops-pilot:smoke@$amd64_digest" "$log" >/dev/null
grep -F 'docker run -d -p 127.0.0.1::5000 docker.io/library/registry:2@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373' "$log" >/dev/null
grep -F "docker run --rm --pull=never --platform linux/amd64" "$log" >/dev/null
[ "$(grep -Fc 'docker rm -f registry-id' "$log")" -eq 1 ]

if run "$tmp/candidate" linux/ppc64le >/dev/null 2>&1; then echo 'expected bad platform rejection' >&2; exit 1; fi
cp "$repo/build/tool-versions.env" "$tmp/tool-versions.good"
printf '%s\n' 'CRANE_VERSION=v0.21.7' "CRANE_LINUX_AMD64_SHA256=$(printf 0%.0s $(seq 1 64))" "CRANE_LINUX_ARM64_SHA256=$(printf 0%.0s $(seq 1 64))" >"$repo/build/tool-versions.env"
if run "$tmp/candidate" linux/amd64 >/dev/null 2>&1; then echo 'expected checksum rejection' >&2; exit 1; fi
cp "$tmp/tool-versions.good" "$repo/build/tool-versions.env"
if OPS_PILOT_TEST_CRANE_VERSION=v0.0.0 PATH="$tmp/bin:$PATH" OPS_PILOT_TEST_LOG=$log OPS_PILOT_TEST_CRANE_ARCHIVE=$tmp/crane.tar.gz OPS_PILOT_TEST_ROOT_DIGEST=$root_digest OPS_PILOT_TEST_AMD64_DIGEST=$amd64_digest OPS_PILOT_TEST_ARM64_DIGEST=$arm64_digest "$repo/scripts/smoke-release-candidate.sh" "$tmp/candidate" linux/amd64 >/dev/null 2>&1; then echo 'expected version rejection' >&2; exit 1; fi
if OPS_PILOT_TEST_ROOT_DIGEST=sha256:bad PATH="$tmp/bin:$PATH" OPS_PILOT_TEST_LOG=$log OPS_PILOT_TEST_CRANE_ARCHIVE=$tmp/crane.tar.gz OPS_PILOT_TEST_AMD64_DIGEST=$amd64_digest OPS_PILOT_TEST_ARM64_DIGEST=$arm64_digest "$repo/scripts/smoke-release-candidate.sh" "$tmp/candidate" linux/amd64 >/dev/null 2>&1; then echo 'expected digest mismatch rejection' >&2; exit 1; fi
[ "$(grep -Fc 'docker rm -f registry-id' "$log")" -eq 2 ]
printf '%s\n' 'smoke-release-candidate: PASS'
