#!/bin/sh
set -eu

source_root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd -P)
tmp=$(mktemp -d "${TMPDIR:-/tmp}/ops-pilot-builder.XXXXXX")
trap 'rm -rf "$tmp"' EXIT HUP INT TERM
repo=$tmp/repo
mkdir -p "$repo/scripts" "$repo/build" "$repo/.github" "$tmp/bin"
cp "$source_root/scripts/build-release-candidate.sh" "$source_root/scripts/check-release-candidate.py" "$repo/scripts/"
chmod +x "$repo/scripts/"*.sh
printf '%s\n' 'BUILDX_VERSION=v0.34.1' 'BUILDKIT_IMAGE=docker.io/moby/buildkit:v0.30.0@sha256:0168606be2315b7c807a03b3d8aa79beefdb31c98740cebdffdfeebf31190c9f' 'GORELEASER_VERSION=v2.17.0' >"$repo/build/tool-versions.env"
printf '%s\n' signer >"$repo/.github/release-signers"

cat >"$tmp/bin/git" <<'SH'
#!/bin/sh
set -eu
printf 'git %s\n' "$*" >>"$OPS_PILOT_TEST_LOG"
case "$*" in
  *'rev-parse --verify HEAD') echo aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa ;;
  *'tag --points-at HEAD') echo v1.2.3 ;;
  *'cat-file -t refs/tags/v1.2.3') echo tag ;;
  *'rev-parse refs/tags/v1.2.3^{}') echo aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa ;;
  *'verify-tag v1.2.3') [ "${OPS_PILOT_TEST_BAD_SIGNATURE:-}" != 1 ] ;;
  *'remote get-url origin') echo https://example.invalid/ops-pilot.git ;;
  *'fetch --no-tags origin +refs/heads/main:refs/remotes/origin/main') exit 0 ;;
  *'merge-base --is-ancestor aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa refs/remotes/origin/main') exit 0 ;;
  *'show -s --format=%ct aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa') echo 1700000000 ;;
  *'worktree add --detach '*) mkdir -p "$6"; : >"$6/Dockerfile" ;;
  *'worktree remove --force '*) rm -rf "$6" ;;
  *) echo "unexpected git: $*" >&2; exit 9 ;;
esac
SH
cat >"$tmp/bin/goreleaser" <<'SH'
#!/bin/sh
set -eu
printf 'goreleaser %s\n' "$*" >>"$OPS_PILOT_TEST_LOG"
case "$*" in
  --version) printf 'GitVersion:    2.17.0\nGoVersion:     go1.26.4\n' ;;
  'release --clean --skip=publish')
    python3 - <<'PY'
import gzip, hashlib, io, json, tarfile
from pathlib import Path
dist = Path('dist'); (dist / 'homebrew/Casks').mkdir(parents=True)
raw=[{'name':'metadata.json','path':'dist/metadata.json','type':'Metadata'}]
(dist/'metadata.json').write_text('{}'); (dist/'config.yaml').write_text('{}')
def release_archive():
    payload=(
        ('ops-pilot',b'ops-pilot binary\n',0o755),
        ('README.md',b'readme\n',0o644),
        ('docs/install.md',b'install\n',0o644),
        ('docs/configuration.md',b'config\n',0o644),
        ('docs/cli.md',b'cli\n',0o644),
    )
    out=io.BytesIO()
    with gzip.GzipFile(filename='',fileobj=out,mode='wb',mtime=0) as compressed:
        with tarfile.open(fileobj=compressed,mode='w|',format=tarfile.USTAR_FORMAT) as archive:
            for name,data,mode in payload:
                info=tarfile.TarInfo(name); info.size=len(data); info.mode=mode
                info.mtime=info.uid=info.gid=0; info.uname=info.gname=''
                archive.addfile(info,io.BytesIO(data))
    return out.getvalue()
archives=[]
for os_name, arch in (('darwin','amd64'),('darwin','arm64'),('linux','amd64'),('linux','arm64')):
    binary=dist/f'ops-pilot_{os_name}_{arch}_v1'; binary.mkdir(); (binary/'ops-pilot').write_text('intermediate')
    raw.append({'name':'ops-pilot','path':f'dist/{binary.name}/ops-pilot','type':'Binary','goos':os_name,'goarch':arch})
    name=f'ops-pilot_1.2.3_{os_name}_{arch}.tar.gz'; (dist/name).write_bytes(release_archive()); archives.append(name)
    raw.append({'name':name,'path':'dist/'+name,'type':'Archive','goos':os_name,'goarch':arch})
    sbom={'spdxVersion':'SPDX-2.3','SPDXID':'SPDXRef-DOCUMENT','packages':[{'SPDXID':'SPDXRef-Syft','name':'syft-package','versionInfo':'1.0.0','licenseConcluded':'MIT'}],'relationships':[]}
    (dist/(name+'.sbom.json')).write_text(json.dumps(sbom,separators=(',',':')))
    raw.append({'name':name+'.sbom.json','path':'dist/'+name+'.sbom.json','type':'SBOM'})
(dist/'checksums.txt').write_text(''.join(f'{hashlib.sha256((dist/n).read_bytes()).hexdigest()}  {n}\n' for n in archives))
raw.append({'name':'checksums.txt','path':'dist/checksums.txt','type':'Checksum'})
(dist/'homebrew/Casks/ops-pilot.rb').write_text("cask 'ops-pilot'\n")
raw.append({'name':'ops-pilot.rb','path':'dist/homebrew/Casks/ops-pilot.rb','type':'Homebrew Cask'})
(dist/'artifacts.json').write_text(json.dumps(raw,separators=(',',':')))
PY
    ;;
  *) exit 9 ;;
esac
SH
cat >"$tmp/bin/docker" <<'SH'
#!/bin/sh
set -eu
printf 'docker %s\n' "$*" >>"$OPS_PILOT_TEST_LOG"
case "$*" in
  'buildx version') echo 'github.com/docker/buildx v0.34.1' ;;
  'buildx create '*) exit 0 ;;
  'buildx inspect --bootstrap '*) [ "${OPS_PILOT_TEST_BOOTSTRAP_FAIL:-}" != 1 ] ;;
  'buildx rm -f '*) exit 0 ;;
  'buildx build '*)
    case "$*" in
      *'--build-arg VERSION=1.2.3 --build-arg COMMIT=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa --build-arg BUILD_DATE=2023-11-14T22:13:20Z '*) ;;
      *) echo "wrong or missing release build arguments: $*" >&2; exit 8 ;;
    esac
    dest=$(printf '%s\n' "$*" | sed -n 's/.*type=oci,dest=\([^ ]*\).*/\1/p')
    DEST=$dest python3 - <<'PY'
import hashlib, io, json, os, tarfile
def digest(data): return 'sha256:'+hashlib.sha256(data).hexdigest()
def desc(data, media, **extra): return {'mediaType':media,'digest':digest(data),'size':len(data),**extra}
blobs={}; manifests=[]
for arch in ('amd64','arm64'):
    layer=(arch+' layer').encode(); blobs[digest(layer)[7:]]=layer
    cfg=json.dumps({'architecture':arch,'os':'linux'},separators=(',',':')).encode(); blobs[digest(cfg)[7:]]=cfg
    manifest=json.dumps({'schemaVersion':2,'config':desc(cfg,'application/vnd.oci.image.config.v1+json'),'layers':[desc(layer,'application/vnd.oci.image.layer.v1.tar+gzip')]},separators=(',',':')).encode(); blobs[digest(manifest)[7:]]=manifest
    manifests.append(desc(manifest,'application/vnd.oci.image.manifest.v1+json',platform={'os':'linux','architecture':arch}))
index=json.dumps({'schemaVersion':2,'manifests':manifests},separators=(',',':')).encode()
with tarfile.open(os.environ['DEST'],'w') as tar:
    for name in ('blobs','blobs/sha256'):
        info=tarfile.TarInfo(name); info.type=tarfile.DIRTYPE; info.mtime=0; tar.addfile(info)
    for name,data in sorted(blobs.items()):
        info=tarfile.TarInfo('blobs/sha256/'+name); info.size=len(data); info.mtime=0; tar.addfile(info,io.BytesIO(data))
    for name,data in (('index.json',index),('oci-layout',b'{"imageLayoutVersion":"1.0.0"}')):
        info=tarfile.TarInfo(name); info.size=len(data); info.mtime=0; tar.addfile(info,io.BytesIO(data))
PY
    ;;
  *) exit 9 ;;
esac
SH
chmod +x "$tmp/bin/"*

log=$tmp/log
cd "$tmp"
PATH="$tmp/bin:$PATH" OPS_PILOT_TEST_LOG=$log OPS_PILOT_PROTECTED_DEFAULT_BRANCH=main "$repo/scripts/build-release-candidate.sh" candidate >/dev/null
candidate=$tmp/candidate
[ -d "$candidate/dist" ] && [ -f "$candidate/release-manifest.json" ]
"$repo/scripts/check-release-candidate.py" "$candidate"
grep -F 'verify-tag v1.2.3' "$log" >/dev/null
grep -F 'gpg.format=ssh' "$log" | grep -F 'gpg.ssh.allowedSignersFile=' >/dev/null
grep -F 'fetch --no-tags origin +refs/heads/main:refs/remotes/origin/main' "$log" >/dev/null
grep -F 'merge-base --is-ancestor aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa refs/remotes/origin/main' "$log" >/dev/null
grep -F 'worktree add --detach ' "$log" >/dev/null
grep -F 'worktree remove --force ' "$log" >/dev/null
grep -F 'goreleaser release --clean --skip=publish' "$log" >/dev/null
grep -F 'docker buildx create --name ' "$log" | grep -F -- '--driver docker-container --driver-opt image=docker.io/moby/buildkit:v0.30.0@sha256:0168606be2315b7c807a03b3d8aa79beefdb31c98740cebdffdfeebf31190c9f' >/dev/null
grep -F 'docker buildx inspect --bootstrap ' "$log" >/dev/null
grep -F 'docker buildx build ' "$log" | grep -F -- '--platform linux/amd64,linux/arm64 --build-arg VERSION=1.2.3 --build-arg COMMIT=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa --build-arg BUILD_DATE=2023-11-14T22:13:20Z --provenance=false --sbom=false --output type=oci,dest=' >/dev/null
grep -F 'docker buildx rm -f ' "$log" >/dev/null
python3 - "$candidate/dist/oci-identity.json" "$candidate/dist/artifacts.json" "$candidate/dist" <<'PY'
import json, sys
from pathlib import Path
identity=json.load(open(sys.argv[1])); records=json.load(open(sys.argv[2]))
assert set(identity['platforms']) == {'linux/amd64','linux/arm64'}
assert len(records) == 10 and [record['path'] for record in records] == sorted(record['path'] for record in records)
assert {record['type'] for record in records} == {'Archive','SBOM','Checksum','Homebrew Cask'}
assert not (Path(sys.argv[3])/'metadata.json').exists() and not (Path(sys.argv[3])/'config.yaml').exists()
assert not any(path.is_dir() and path.name.startswith('ops-pilot_') for path in Path(sys.argv[3]).iterdir())
for record in records:
    if record['type'] != 'SBOM': continue
    document=json.load(open(sys.argv[3]+'/'+record['path'].removeprefix('dist/')))
    assert len(document['documentDescribes']) == 1
    subject=document['documentDescribes'][0]
    assert sum(package['SPDXID'] == subject for package in document['packages']) == 1
    assert any(package['SPDXID'] == 'SPDXRef-Syft' for package in document['packages'])
PY
PATH="$tmp/bin:$PATH" OPS_PILOT_TEST_LOG=$log OPS_PILOT_PROTECTED_DEFAULT_BRANCH=main "$repo/scripts/build-release-candidate.sh" candidate-b >/dev/null
diff -ru "$candidate" "$tmp/candidate-b"
printf tampered >>"$candidate/dist/ops-pilot-oci.tar"
if "$repo/scripts/check-release-candidate.py" "$candidate" >/dev/null 2>&1; then echo 'expected OCI mutation rejection' >&2; exit 1; fi

if PATH="$tmp/bin:$PATH" OPS_PILOT_TEST_LOG=$log OPS_PILOT_PROTECTED_DEFAULT_BRANCH=HEAD "$repo/scripts/build-release-candidate.sh" bad-ref >/dev/null 2>&1; then echo 'expected arbitrary protected ref rejection' >&2; exit 1; fi
if PATH="$tmp/bin:$PATH" OPS_PILOT_TEST_LOG=$log OPS_PILOT_PROTECTED_DEFAULT_BRANCH=v1.2.3 "$repo/scripts/build-release-candidate.sh" tag-ref >/dev/null 2>&1; then echo 'expected tag protected ref rejection' >&2; exit 1; fi
if PATH="$tmp/bin:$PATH" OPS_PILOT_TEST_LOG=$log OPS_PILOT_PROTECTED_DEFAULT_BRANCH=main OPS_PILOT_TEST_BOOTSTRAP_FAIL=1 "$repo/scripts/build-release-candidate.sh" failed-bootstrap >/dev/null 2>&1; then echo 'expected bootstrap failure' >&2; exit 1; fi
[ ! -e "$tmp/failed-bootstrap" ] && grep -F 'docker buildx rm -f ' "$log" >/dev/null
printf '%s\n' 'build-release-candidate: PASS'
