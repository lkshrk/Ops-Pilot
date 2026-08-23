#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
validator="$root/scripts/check-release-candidate.py"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/ops-pilot-release-candidate.XXXXXX")
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

make_candidate() {
	python3 - "$1" "${2:-1.2.3}" <<'PY'
import hashlib, io, json, os, sys, tarfile
from pathlib import Path

root = Path(sys.argv[1]); version = sys.argv[2]; dist = root / "dist"; dist.mkdir(parents=True)
def put(name, data):
    p = dist / name; p.parent.mkdir(parents=True, exist_ok=True); p.write_bytes(data)
def digest(data): return "sha256:" + hashlib.sha256(data).hexdigest()
def desc(data, media, **more): return {"mediaType": media, "digest": digest(data), "size": len(data), **more}
def blob(data): put("blobs/sha256/" + digest(data).split(":", 1)[1], data); return data
def release_archive():
    payload = (
        ("ops-pilot", b"ops-pilot binary\n", 0o755),
        ("README.md", b"readme\n", 0o644),
        ("docs/install.md", b"install\n", 0o644),
        ("docs/configuration.md", b"config\n", 0o644),
        ("docs/cli.md", b"cli\n", 0o644),
    )
    compressed = io.BytesIO()
    import gzip
    with gzip.GzipFile(filename="", fileobj=compressed, mode="wb", mtime=0) as gz:
        with tarfile.open(fileobj=gz, mode="w|", format=tarfile.USTAR_FORMAT) as tar:
            for name, data, mode in payload:
                info = tarfile.TarInfo(name)
                info.size = len(data)
                info.mode = mode
                info.mtime = info.uid = info.gid = 0
                info.uname = info.gname = ""
                tar.addfile(info, io.BytesIO(data))
    return compressed.getvalue()
platforms=[]
for arch in ("amd64", "arm64"):
    layer = blob(("layer-" + arch).encode())
    cfg = json.dumps({"architecture": arch, "os": "linux", "rootfs": {"type":"layers", "diff_ids": []}}, separators=(",", ":")).encode()
    cfg = blob(cfg)
    manifest = json.dumps({"schemaVersion":2, "config":desc(cfg, "application/vnd.oci.image.config.v1+json"), "layers":[desc(layer, "application/vnd.oci.image.layer.v1.tar+gzip")]}, separators=(",", ":")).encode()
    manifest = blob(manifest)
    platforms.append(desc(manifest, "application/vnd.oci.image.manifest.v1+json", platform={"os":"linux", "architecture":arch}))
index = json.dumps({"schemaVersion":2, "manifests":platforms}, separators=(",", ":")).encode()
oci = io.BytesIO()
with tarfile.open(fileobj=oci, mode="w") as tar:
    for name, data in [("blobs", None), ("blobs/sha256", None)]:
        info=tarfile.TarInfo(name); info.mtime=0
        if data is None: info.type=tarfile.DIRTYPE; tar.addfile(info)
        else: info.size=len(data); tar.addfile(info, io.BytesIO(data))
    for p in sorted((dist / "blobs/sha256").iterdir()):
        data=p.read_bytes(); info=tarfile.TarInfo("blobs/sha256/"+p.name); info.size=len(data); info.mtime=0; tar.addfile(info,io.BytesIO(data))
    for name, data in [("index.json", index), ("oci-layout", b'{"imageLayoutVersion":"1.0.0"}')]:
        info=tarfile.TarInfo(name); info.size=len(data); info.mtime=0; tar.addfile(info,io.BytesIO(data))
put("ops-pilot-oci.tar", oci.getvalue())
put("oci-identity.json", json.dumps({"index":digest(index), "platforms": {"linux/amd64": {k: platforms[0][k] for k in ("digest", "size", "mediaType")}, "linux/arm64": {k: platforms[1][k] for k in ("digest", "size", "mediaType")}}}, sort_keys=True, separators=(",", ":")).encode())
archives=[]; artifact_records=[]
for os_name, arch in (("darwin","amd64"),("darwin","arm64"),("linux","amd64"),("linux","arm64")):
    name=f"ops-pilot_{version}_{os_name}_{arch}.tar.gz"; put(name, release_archive()); put(name+".sbom.json", json.dumps({"spdxVersion":"SPDX-2.3", "SPDXID":"SPDXRef-DOCUMENT", "documentDescribes":["SPDXRef-Archive"], "packages":[{"SPDXID":"SPDXRef-Archive", "name":name, "versionInfo":version, "licenseConcluded":"MIT", "checksums":[{"algorithm":"SHA256", "checksumValue":hashlib.sha256((dist / name).read_bytes()).hexdigest()}], "externalRefs":[{"referenceType":"purl", "referenceLocator":f"pkg:generic/ops-pilot@{version}"}]}], "relationships":[]}, separators=(",", ":")).encode()); archives.append(name); artifact_records.extend([{"name":name,"path":"dist/"+name,"goos":os_name,"goarch":arch,"type":"Archive"},{"name":name+".sbom.json","path":"dist/"+name+".sbom.json","type":"SBOM"}])
put("checksums.txt", "".join(f"{hashlib.sha256((dist / n).read_bytes()).hexdigest()}  {n}\n" for n in archives).encode())
put("homebrew/Casks/ops-pilot.rb", b"cask 'ops-pilot'\n")
artifact_records.extend([{"name":"checksums.txt","path":"dist/checksums.txt","type":"Checksum"},{"name":"ops-pilot.rb","path":"dist/homebrew/Casks/ops-pilot.rb","type":"Homebrew Cask"}])
put("artifacts.json", json.dumps(artifact_records, separators=(",", ":")).encode())
for p in (dist / "blobs").rglob("*"):
    if p.is_file(): p.unlink()
for p in sorted((dist / "blobs").rglob("*"), reverse=True):
    if p.is_dir(): p.rmdir()
(dist / "blobs").rmdir()
entries=[]
for p in sorted(dist.rglob("*")):
    if p.is_file() and p.name != "release-manifest.json":
        data=p.read_bytes(); entries.append({"path":p.relative_to(dist).as_posix(),"size":len(data),"sha256":hashlib.sha256(data).hexdigest()})
(root / "release-manifest.json").write_text(json.dumps(entries, separators=(",", ":")))
PY
}

pass() { "$validator" "$1" >/dev/null 2>&1; }
fail() { if pass "$1"; then echo "expected failure: $2" >&2; exit 1; fi; }
refresh_manifest() {
	python3 - "$1" <<'PY'
import hashlib, json, sys
from pathlib import Path
root = Path(sys.argv[1]); dist = root / "dist"; rows=[]
for p in sorted(dist.rglob("*")):
    if p.is_file():
        data=p.read_bytes(); rows.append({"path":p.relative_to(dist).as_posix(),"size":len(data),"sha256":hashlib.sha256(data).hexdigest()})
(root / "release-manifest.json").write_text(json.dumps(rows,separators=(",", ":")))
PY
}

refresh_archive_metadata() {
	python3 - "$1" <<'PY'
import hashlib, json, sys
from pathlib import Path
root = Path(sys.argv[1]); dist = root / "dist"; archives=sorted(dist.glob("ops-pilot_*.tar.gz"))
for archive in archives:
    sbom=Path(str(archive)+".sbom.json"); doc=json.loads(sbom.read_text())
    doc["packages"][0]["checksums"]=[{"algorithm":"SHA256","checksumValue":hashlib.sha256(archive.read_bytes()).hexdigest()}]
    sbom.write_text(json.dumps(doc,separators=(",",":")))
(dist / "checksums.txt").write_text("".join(f"{hashlib.sha256(p.read_bytes()).hexdigest()}  {p.name}\n" for p in archives))
rows=[]
for p in sorted(dist.rglob("*")):
    if p.is_file():
        data=p.read_bytes(); rows.append({"path":p.relative_to(dist).as_posix(),"size":len(data),"sha256":hashlib.sha256(data).hexdigest()})
(root / "release-manifest.json").write_text(json.dumps(rows,separators=(",",":")))
PY
}

mutate_release_archive() {
	python3 - "$1" "$2" <<'PY'
import gzip, io, sys, tarfile
from pathlib import Path

root, kind = Path(sys.argv[1]), sys.argv[2]
archive = next((root / "dist").glob("ops-pilot_*_darwin_amd64.tar.gz"))
if kind == "corrupt":
    archive.write_bytes(b"not a gzip tar")
    raise SystemExit
with tarfile.open(archive, "r:gz") as src:
    entries=[{"name":m.name,"data":src.extractfile(m).read(),"mode":m.mode,"type":tarfile.REGTYPE,"linkname":"","pax":{}} for m in src]
target=entries[0]
if kind == "wrong-target": target["name"]="ops-pilot.exe"
elif kind == "executable-mode": target["mode"]=0o644
elif kind == "doc-mode": entries[1]["mode"]=0o755
elif kind == "absolute": target["name"]="/ops-pilot"
elif kind == "traversal": target["name"]="../ops-pilot"
elif kind == "nonnormal": target["name"]="docs/../ops-pilot"
elif kind == "backslash": target["name"]="bad\\ops-pilot"
elif kind == "duplicate": entries.append(dict(target))
elif kind == "ancestor": entries.append({"name":"docs","data":b"x","mode":0o644,"type":tarfile.REGTYPE,"linkname":"","pax":{}})
elif kind == "symlink": target.update(type=tarfile.SYMTYPE, data=b"", linkname="elsewhere")
elif kind == "hardlink": target.update(type=tarfile.LNKTYPE, data=b"", linkname="README.md")
elif kind == "directory": target.update(type=tarfile.DIRTYPE, data=b"")
elif kind == "character": target.update(type=tarfile.CHRTYPE, data=b"")
elif kind == "block": target.update(type=tarfile.BLKTYPE, data=b"")
elif kind == "fifo": target.update(type=tarfile.FIFOTYPE, data=b"")
elif kind == "pax": target["pax"]={"comment":"untrusted"}
elif kind == "sparse": target.update(type=tarfile.GNUTYPE_SPARSE, data=b"")
else: raise ValueError(kind)
fmt = tarfile.GNU_FORMAT if kind == "sparse" else tarfile.PAX_FORMAT if kind == "pax" else tarfile.USTAR_FORMAT
out=io.BytesIO()
with gzip.GzipFile(filename="", fileobj=out, mode="wb", mtime=0) as gz:
    with tarfile.open(fileobj=gz, mode="w|", format=fmt) as dst:
        for entry in entries:
            info=tarfile.TarInfo(entry["name"])
            info.type=entry["type"]; info.mode=entry["mode"]; info.linkname=entry["linkname"]
            info.size=len(entry["data"]) if info.type in (tarfile.REGTYPE, tarfile.AREGTYPE) else 0
            info.mtime=info.uid=info.gid=0; info.uname=info.gname=""
            info.pax_headers=entry["pax"]
            if info.type in (tarfile.CHRTYPE, tarfile.BLKTYPE): info.devmajor=1
            dst.addfile(info, io.BytesIO(entry["data"]) if info.size else None)
archive.write_bytes(out.getvalue())
PY
	refresh_archive_metadata "$1"
}

make_candidate "$tmp/good"
pass "$tmp/good"
make_candidate "$tmp/build-metadata" "1.2.3+build.1"
pass "$tmp/build-metadata"

for kind in corrupt wrong-target executable-mode doc-mode absolute traversal nonnormal backslash duplicate ancestor symlink hardlink directory character block fifo pax sparse; do
	cp -R "$tmp/good" "$tmp/archive-$kind"
	mutate_release_archive "$tmp/archive-$kind" "$kind"
	fail "$tmp/archive-$kind" "release-archive-$kind"
done

cp -R "$tmp/good" "$tmp/compare"
"$validator" "$tmp/good" --compare "$tmp/compare" >/dev/null
python3 - "$tmp/compare/dist" <<'PY'
import json, sys
from pathlib import Path
p = next(Path(sys.argv[1]).glob("*.sbom.json")); data=json.loads(p.read_text()); data["creationInfo"]={"created":"tomorrow"}; p.write_text(json.dumps(data,separators=(",", ":")))
PY
refresh_manifest "$tmp/compare"
"$validator" "$tmp/good" --compare "$tmp/compare" >/dev/null

for kind in missing-subject unrelated-subject multiple-subject wrong-subject-digest partial-subject; do
	cp -R "$tmp/good" "$tmp/$kind"
	python3 - "$tmp/$kind/dist" "$kind" <<'PY'
import json, sys
from pathlib import Path
dist, kind = Path(sys.argv[1]), sys.argv[2]; p=next(dist.glob('*.sbom.json')); doc=json.loads(p.read_text())
if kind == 'missing-subject': doc['documentDescribes']=[]
elif kind == 'unrelated-subject': doc['packages'][0]['name']='other.tar.gz'
elif kind == 'multiple-subject':
    second=dict(doc['packages'][0]); second['SPDXID']='SPDXRef-Second'; doc['packages'].append(second); doc['documentDescribes'].append('SPDXRef-Second')
elif kind == 'wrong-subject-digest': doc['packages'][0]['checksums'][0]['checksumValue']='0'*64
elif kind == 'partial-subject': del doc['packages'][0]['versionInfo']
p.write_text(json.dumps(doc,separators=(',',':')))
PY
	refresh_manifest "$tmp/$kind"
	fail "$tmp/$kind" "$kind"
done

for kind in extra-record duplicate-archive partial-record bogus-record cross-version; do
	cp -R "$tmp/good" "$tmp/$kind"
	python3 - "$tmp/$kind/dist/artifacts.json" "$kind" <<'PY'
import json, sys
from pathlib import Path
p, kind = Path(sys.argv[1]), sys.argv[2]; records=json.loads(p.read_text())
if kind == 'extra-record': records.append(dict(records[0]))
elif kind == 'duplicate-archive': records[2]['name']=records[0]['name']; records[2]['path']=records[0]['path']
elif kind == 'partial-record': del records[0]['path']
elif kind == 'bogus-record': records[0]['type']='Bogus'
elif kind == 'cross-version': records[0]['name']=records[0]['name'].replace('1.2.3','9.9.9'); records[0]['path']='dist/'+records[0]['name']
p.write_text(json.dumps(records,separators=(',',':')))
PY
	refresh_manifest "$tmp/$kind"
	fail "$tmp/$kind" "$kind"
done

cp -R "$tmp/good" "$tmp/extra"; : >"$tmp/extra/dist/extra"; refresh_manifest "$tmp/extra"; fail "$tmp/extra" extra-path
cp -R "$tmp/good" "$tmp/orphan"; mkdir -p "$tmp/orphan/dist/blobs/sha256"; printf x >"$tmp/orphan/dist/blobs/sha256/$(printf x | shasum -a 256 | awk '{print $1}')"; refresh_manifest "$tmp/orphan"; fail "$tmp/orphan" orphan-blob
cp -R "$tmp/good" "$tmp/manifest"; printf '[]' >"$tmp/manifest/release-manifest.json"; fail "$tmp/manifest" bad-release-manifest
cp -R "$tmp/good" "$tmp/cask"; rm "$tmp/cask/dist/homebrew/Casks/ops-pilot.rb"; fail "$tmp/cask" missing-cask
cp -R "$tmp/good" "$tmp/identity"; printf '{}' >"$tmp/identity/dist/oci-identity.json"; fail "$tmp/identity" oci-identity

for kind in missing-platform duplicate-platform wrong-platform extra-root attestation digest-drift size-drift media-drift; do
	cp -R "$tmp/good" "$tmp/$kind"
	python3 - "$tmp/$kind/dist/ops-pilot-oci.tar" "$kind" <<'PY'
import io, json, sys, tarfile
p, kind = sys.argv[1:]; entries=[]
with tarfile.open(p, 'r:') as src:
    for m in src:
        entries.append((m.name, m.isdir(), src.extractfile(m).read() if m.isfile() else None))
for n, isdir, data in entries:
    if n != 'index.json': continue
    index=json.loads(data)
    if kind == 'missing-platform': index['manifests']=index['manifests'][:1]
    elif kind == 'duplicate-platform': index['manifests'][1]=index['manifests'][0]
    elif kind == 'wrong-platform': index['manifests'][1]['platform']['architecture']='ppc64le'
    elif kind == 'extra-root': index['manifests'].append(index['manifests'][0])
    elif kind == 'attestation': index['manifests'][0]['subject']={'digest':'sha256:'+'0'*64}
    elif kind == 'digest-drift': index['manifests'][0]['digest']='sha256:'+'0'*64
    elif kind == 'size-drift': index['manifests'][0]['size'] += 1
    elif kind == 'media-drift': index['manifests'][0]['mediaType']='application/unknown'
    entries[entries.index((n,isdir,data))]=(n,isdir,json.dumps(index,separators=(',',':')).encode())
out=io.BytesIO()
with tarfile.open(fileobj=out, mode='w') as dst:
    for n, isdir, data in entries:
        m=tarfile.TarInfo(n)
        if isdir: m.type=tarfile.DIRTYPE; dst.addfile(m)
        else: m.size=len(data); dst.addfile(m,io.BytesIO(data))
open(p,'wb').write(out.getvalue())
PY
	refresh_manifest "$tmp/$kind"
	fail "$tmp/$kind" "$kind"
done

for kind in absolute traversal symlink hardlink char block fifo socket sparse pax unknown duplicate unordered ancestor; do
	cp -R "$tmp/good" "$tmp/$kind"
	python3 - "$tmp/$kind/dist/ops-pilot-oci.tar" "$kind" <<'PY'
import io, sys, tarfile
p, kind = sys.argv[1:]; raw=open(p,'rb').read(); out=io.BytesIO()
with tarfile.open(fileobj=out, mode='w') as t:
    if kind == 'absolute': name='/bad'
    elif kind == 'traversal': name='../bad'
    elif kind == 'unordered': name='aaa'
    elif kind == 'ancestor': name='blobs'
    else: name='bad'
    i=tarfile.TarInfo(name); i.size=1
    if kind == 'symlink': i.type=tarfile.SYMTYPE; i.linkname='target'; i.size=0
    elif kind == 'hardlink': i.type=tarfile.LNKTYPE; i.linkname='target'; i.size=0
    elif kind == 'char': i.type=tarfile.CHRTYPE; i.size=0
    elif kind == 'block': i.type=tarfile.BLKTYPE; i.size=0
    elif kind == 'fifo': i.type=tarfile.FIFOTYPE; i.size=0
    elif kind == 'socket': i.type=b's'; i.size=0
    elif kind == 'sparse': i.pax_headers={'GNU.sparse.map':'0,1'}
    elif kind == 'pax': i.pax_headers={'comment':'bad'}
    elif kind == 'unknown': i.type=b'V'; i.size=0
    t.addfile(i, None if i.size == 0 else io.BytesIO(b'x'))
    t.fileobj.write(raw)
open(p,'wb').write(out.getvalue())
PY
	refresh_manifest "$tmp/$kind"
	fail "$tmp/$kind" "tar-$kind"
done

printf '%s\n' 'check-release-candidate: PASS'
