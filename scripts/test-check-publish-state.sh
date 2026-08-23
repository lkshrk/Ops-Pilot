#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
checker="$root/scripts/check-publish-state.sh"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/ops-pilot-publish-state.XXXXXX")
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

candidate="$tmp/candidate"
mkdir -p "$candidate/dist/homebrew/Casks"
printf archive >"$candidate/dist/ops-pilot_1.2.3_linux_amd64.tar.gz"
printf cask >"$candidate/dist/homebrew/Casks/ops-pilot.rb"
printf oci >"$candidate/dist/ops-pilot-oci.tar"
python3 - "$candidate" <<'PY'
import hashlib, json, sys
from pathlib import Path
root = Path(sys.argv[1]); dist = root / "dist"
identity = {
  "index": "sha256:" + "a" * 64,
  "platforms": {
    "linux/amd64": {"digest": "sha256:" + "b" * 64, "size": 10, "mediaType": "application/vnd.oci.image.manifest.v1+json"},
    "linux/arm64": {"digest": "sha256:" + "c" * 64, "size": 11, "mediaType": "application/vnd.oci.image.manifest.v1+json"},
  },
}
(dist / "oci-identity.json").write_text(json.dumps(identity))
rows=[]
for path in sorted(p for p in dist.rglob("*") if p.is_file()):
  data=path.read_bytes()
  rows.append({"path": path.relative_to(dist).as_posix(), "size": len(data), "sha256": hashlib.sha256(data).hexdigest()})
(root / "release-manifest.json").write_text(json.dumps(rows))
PY

fixture() {
	dir="$tmp/$1"
	mkdir -p "$dir"
	printf '%s\n' "$2" >"$dir/release.json"
	printf '%s\n' "$3" >"$dir/assets-page-1.json"
	printf '[]\n' >"$dir/assets-page-2.json"
	printf '%s\n' "$4" >"$dir/ghcr.json"
	printf '%s\n' "$dir"
}

asset_json() {
	python3 - "$candidate" "$1" <<'PY'
import json, sys
from pathlib import Path
root=Path(sys.argv[1]); name=sys.argv[2]
for index, row in enumerate(json.loads((root/'release-manifest.json').read_text()), 1):
    if Path(row['path']).name == name:
        print(json.dumps({"id": index, "name": name, "size": row['size'], "state": "uploaded"}))
        break
PY
}
oci_manifest='{"schemaVersion":2,"manifests":[{"digest":"sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","size":10,"mediaType":"application/vnd.oci.image.manifest.v1+json","platform":{"os":"linux","architecture":"amd64"}},{"digest":"sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc","size":11,"mediaType":"application/vnd.oci.image.manifest.v1+json","platform":{"os":"linux","architecture":"arm64"}}]}'
release='{"id":9,"assets_url":"fixture://assets"}'
absent='{"state":"absent"}'

run() {
	OPS_PILOT_PUBLISH_STATE_FIXTURE_DIR="$1" GITHUB_REPOSITORY=owner/repo GITHUB_REF_NAME=v1.2.3 "$checker" "$candidate"
}
pass() { run "$1" >/dev/null 2>&1; }
fail() { if pass "$1"; then echo "expected failure: $2" >&2; exit 1; fi; }

# Initial publication: no release or registry version.
initial=$(fixture initial '{"status":404}' '[]' "$absent")
run "$initial" | grep -Fx "$(printf 'asset\thomebrew/Casks/ops-pilot.rb')" >/dev/null
run "$initial" | grep -Fx "$(printf 'ghcr\tghcr.io/owner/repo:v1.2.3')" >/dev/null

# Exact retry skips everything already visible and identical.
exact="$tmp/exact"; mkdir "$exact"
printf '%s\n' "$release" >"$exact/release.json"
python3 - "$candidate" "$exact" >"$exact/assets-page-1.json" <<'PY'
import json, sys
from pathlib import Path
r=Path(sys.argv[1])
out=Path(sys.argv[2]); rows=json.loads((r/'release-manifest.json').read_text())
print(json.dumps([{"id": i+1,"name":Path(x['path']).name,"size":x['size'],"state":"uploaded"} for i,x in enumerate(rows)]))
for i, row in enumerate(rows): out.joinpath("asset-%d.bin" % (i+1)).write_bytes((r / "dist" / row["path"]).read_bytes())
PY
printf '[]\n' >"$exact/assets-page-2.json"
printf '%s\n' "{\"digest\":\"sha256:$(printf 'a%.0s' $(seq 1 64))\",\"manifest\":$oci_manifest}" >"$exact/ghcr.json"
if [ -n "$(run "$exact")" ]; then echo 'exact retry emitted entries' >&2; exit 1; fi

# Partial identical release emits only missing assets.
partial="$tmp/partial"; cp -R "$exact" "$partial"
printf '[%s]\n' "$(asset_json ops-pilot_1.2.3_linux_amd64.tar.gz)" >"$partial/assets-page-1.json"
run "$partial" | grep -Fx "$(printf 'asset\thomebrew/Casks/ops-pilot.rb')" >/dev/null
if run "$partial" | grep -F 'ops-pilot_1.2.3_linux_amd64.tar.gz' >/dev/null; then echo 'partial retry emitted identical asset' >&2; exit 1; fi

# Adversarial remote states fail closed.
conflict="$tmp/conflict"; cp -R "$exact" "$conflict"; sed -i.bak 's/"size":[0-9]*/"size":999/' "$conflict/assets-page-1.json"; rm "$conflict/assets-page-1.json.bak"; fail "$conflict" conflicting-asset
duplicate="$tmp/duplicate"; cp -R "$exact" "$duplicate"; python3 - "$duplicate/assets-page-1.json" <<'PY'
import json,sys
p=sys.argv[1]; rows=json.load(open(p)); rows.append(dict(rows[0])); open(p,'w').write(json.dumps(rows))
PY
fail "$duplicate" duplicate
pagination="$tmp/pagination"; cp -R "$exact" "$pagination"; printf '[%s]\n' "$(asset_json ops-pilot_1.2.3_linux_amd64.tar.gz)" >"$pagination/assets-page-1.json"; printf '[{"id":99,"name":"unknown","size":1,"state":"uploaded"}]\n' >"$pagination/assets-page-2.json"; printf '[]\n' >"$pagination/assets-page-3.json"; fail "$pagination" pagination
ghcr_conflict="$tmp/ghcr-conflict"; cp -R "$exact" "$ghcr_conflict"; sed -i.bak 's/sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa/sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd/' "$ghcr_conflict/ghcr.json"; rm "$ghcr_conflict/ghcr.json.bak"; fail "$ghcr_conflict" conflicting-ghcr
incomplete="$tmp/incomplete"; cp -R "$exact" "$incomplete"; python3 - "$incomplete/assets-page-1.json" <<'PY'
import json,sys
p=sys.argv[1]; rows=json.load(open(p)); rows[0]['state']='starter'; open(p,'w').write(json.dumps(rows))
PY
fail "$incomplete" incomplete
unknown="$tmp/unknown"; cp -R "$exact" "$unknown"; printf '{"state":"mystery"}\n' >"$unknown/ghcr.json"; fail "$unknown" unknown

printf '%s\n' 'check-publish-state: PASS'
