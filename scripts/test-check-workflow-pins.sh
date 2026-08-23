#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
validator="$root/scripts/check-workflow-pins.sh"
tmp=$(mktemp -d "${TMPDIR:-/tmp}/ops-pilot-workflow-pins.XXXXXX")
trap 'rm -rf "$tmp"' EXIT HUP INT TERM

sha() { printf '%040d' "$1" | tr '0' 'a'; }

write_manifest() {
	cat >"$tmp/tool-versions.env" <<EOF
ACTIONS_CHECKOUT_SHA=$(sha 1)
ACTIONS_SETUP_GO_SHA=$(sha 2)
ACTIONS_UPLOAD_ARTIFACT_SHA=$(sha 3)
ACTIONS_DOWNLOAD_ARTIFACT_SHA=$(sha 4)
GITHUB_CODEQL_ACTION_SHA=$(sha 5)
DOCKER_LOGIN_ACTION_SHA=$(sha 6)
DOCKER_METADATA_ACTION_SHA=$(sha 7)
SETUP_QEMU_ACTION_SHA=$(sha 8)
SETUP_BUILDX_ACTION_SHA=$(sha 9)
DOCKER_BUILD_PUSH_ACTION_SHA=$(sha 10)
SOFTPROPS_ACTION_GH_RELEASE_SHA=$(sha 11)
QEMU_BINFMT_IMAGE=docker.io/tonistiigi/binfmt@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
BUILDX_VERSION=v0.34.1
BUILDKIT_IMAGE=docker.io/moby/buildkit:v0.30.0@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
EOF
}

write_workflows() {
	mkdir -p "$tmp/workflows"
	cat >"$tmp/workflows/ci.yml" <<EOF
name: CI
permissions:
  contents: read
jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@$(sha 1)
      - uses: actions/setup-go@$(sha 2)
      - uses: actions/upload-artifact@$(sha 3)
      - uses: actions/download-artifact@$(sha 4)
      - uses: docker/login-action@$(sha 6)
        with:
	          username: \${{ github.actor }}
	          password: \${{ secrets.GITHUB_TOKEN }}
      - uses: docker/metadata-action@$(sha 7)
      - uses: docker/setup-qemu-action@$(sha 8)
        with:
          image: docker.io/tonistiigi/binfmt@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa
      - uses: docker/setup-buildx-action@$(sha 9)
        with:
          version: v0.34.1
          driver: docker-container
          driver-opts: |
            image=docker.io/moby/buildkit:v0.30.0@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb
          keep-state: false
      - uses: docker/build-push-action@$(sha 10)
EOF
	cat >"$tmp/workflows/codeql.yml" <<EOF
name: CodeQL
permissions:
  contents: read
jobs:
  codeql:
    permissions:
      contents: read
      security-events: write
    steps:
      - uses: github/codeql-action/init@$(sha 5)
      - uses: github/codeql-action/autobuild@$(sha 5)
      - uses: github/codeql-action/analyze@$(sha 5)
EOF
	cat >"$tmp/workflows/release.yml" <<EOF
name: Release
concurrency:
	  group: release-\${{ github.repository }}-\${{ github.ref_name }}
permissions:
  contents: read
jobs:
  publish:
    permissions:
      contents: write
      packages: write
    steps:
      - uses: softprops/action-gh-release@$(sha 11)
  verify-published:
    permissions:
      contents: read
      packages: read
    steps:
      - run: true
EOF
}

pass() {
	TOOL_VERSIONS_FILE="$tmp/tool-versions.env" "$validator" "$tmp/workflows"
}

fail() {
	if pass >/dev/null 2>&1; then
		echo "expected validator failure: $1" >&2
		exit 1
	fi
}

write_manifest
write_workflows
pass

sed -i.bak "s#actions/checkout@$(sha 1)#actions/checkout@v4#" "$tmp/workflows/ci.yml"; rm "$tmp/workflows/ci.yml.bak"
fail moving-ref
write_workflows
sed -i.bak "s#actions/checkout@$(sha 1)#actions/checkout@$(sha 2)#" "$tmp/workflows/ci.yml"; rm "$tmp/workflows/ci.yml.bak"
fail sha-mismatch
write_workflows
sed -i.bak 's#docker/build-push-action#evil/build-push-action#' "$tmp/workflows/ci.yml"; rm "$tmp/workflows/ci.yml.bak"
fail unmanifested-action
write_workflows
sed -i.bak '/docker\/build-push-action/d' "$tmp/workflows/ci.yml"; rm "$tmp/workflows/ci.yml.bak"
fail unused-manifest-action
write_workflows
sed -i.bak "s#github/codeql-action/analyze@$(sha 5)#github/codeql-action/analyze@$(sha 4)#" "$tmp/workflows/codeql.yml"; rm "$tmp/workflows/codeql.yml.bak"
fail codeql-drift
write_workflows
sed -i.bak 's#tonistiigi/binfmt@sha256:[a-z]*#tonistiigi/binfmt:latest#' "$tmp/workflows/ci.yml"; rm "$tmp/workflows/ci.yml.bak"
fail qemu-image
write_workflows
sed -i.bak 's/version: v0.34.1/version: v0.33.0/' "$tmp/workflows/ci.yml"; rm "$tmp/workflows/ci.yml.bak"
fail buildx-version
write_workflows
sed -i.bak 's/driver: docker-container/driver: docker/' "$tmp/workflows/ci.yml"; rm "$tmp/workflows/ci.yml.bak"
fail buildx-driver
write_workflows
sed -i.bak 's/keep-state: false/keep-state: true/' "$tmp/workflows/ci.yml"; rm "$tmp/workflows/ci.yml.bak"
fail buildx-state
write_workflows
printf '%s\n' '          cache-to: type=gha' >>"$tmp/workflows/ci.yml"
fail cache-export
write_workflows
sed -i.bak 's#image=docker.io/moby/buildkit:[^ ]*#image=docker.io/moby/buildkit:latest#' "$tmp/workflows/ci.yml"; rm "$tmp/workflows/ci.yml.bak"
fail buildkit-image
write_workflows
sed -i.bak 's/  contents: read/  contents: write/' "$tmp/workflows/ci.yml"; rm "$tmp/workflows/ci.yml.bak"
fail workflow-permissions
write_workflows
sed -i.bak '/^      contents: read$/d' "$tmp/workflows/codeql.yml"; rm "$tmp/workflows/codeql.yml.bak"
fail codeql-contents-permission
write_workflows
sed -i.bak 's/security-events: write/security-events: write\
      packages: write/' "$tmp/workflows/codeql.yml"; rm "$tmp/workflows/codeql.yml.bak"
fail codeql-permissions
write_workflows
sed -i.bak 's/contents: write/contents: write\
      security-events: write/' "$tmp/workflows/release.yml"; rm "$tmp/workflows/release.yml.bak"
fail publish-permissions
write_workflows
sed -i.bak 's/packages: read/packages: read\
      id-token: write/' "$tmp/workflows/release.yml"; rm "$tmp/workflows/release.yml.bak"
fail forbidden-permission
write_workflows
sed -i.bak 's/  verify-published:/  verify-published:/' "$tmp/workflows/release.yml"; rm "$tmp/workflows/release.yml.bak"
pass

write_manifest
printf '%s\n' 'not a manifest record' >>"$tmp/tool-versions.env"
fail malformed-manifest
write_manifest
printf 'ACTIONS_CHECKOUT_SHA=%s\n' "$(sha 1)" >>"$tmp/tool-versions.env"
fail duplicate-manifest-key
write_manifest
sed -i.bak '/ACTIONS_SETUP_GO_SHA/d' "$tmp/tool-versions.env"; rm "$tmp/tool-versions.env.bak"
fail missing-manifest-action
write_manifest
printf 'UNKNOWN_ACTION_SHA=%s\n' "$(sha 12)" >>"$tmp/tool-versions.env"
fail unknown-manifest-action

write_manifest
write_workflows
cat >>"$tmp/workflows/ci.yml" <<EOF
  reusable:
    uses: evil/reusable@main
EOF
fail reusable-job-moving-ref
write_workflows
cat >>"$tmp/workflows/ci.yml" <<EOF
      - name: hidden action
        uses: evil/reusable@main
EOF
fail named-step-moving-ref
write_workflows
printf '%s\n' '      - { name: hidden action, uses: evil/reusable@main }' >>"$tmp/workflows/ci.yml"
fail flow-map-moving-ref
write_workflows
cat >>"$tmp/workflows/ci.yml" <<EOF
on: [pull_request_target]
EOF
fail flow-trigger
write_workflows
cat >>"$tmp/workflows/ci.yml" <<EOF
  unsafe:
    permissions: write-all
    steps:
      - run: true
EOF
fail write-all-permissions
write_workflows
cat >>"$tmp/workflows/ci.yml" <<EOF
  verify-published:
    permissions:
      contents: read
      packages: read
    steps:
      - run: true
EOF
fail verify-published-outside-release
write_workflows
cat >>"$tmp/workflows/ci.yml" <<EOF
      - uses: docker/setup-buildx-action@$(sha 9)
        with:
          version: v0.34.1
EOF
fail incomplete-second-buildx
write_workflows
cat >>"$tmp/workflows/ci.yml" <<EOF
      - uses: docker/setup-qemu-action@$(sha 8)
        with:
          platforms: arm64
EOF
fail incomplete-second-qemu
write_workflows
cat >>"$tmp/workflows/ci.yml" <<EOF
      - uses: actions/checkout@$(sha 1)
        with: { cache-to: type=gha }
EOF
fail inline-cache
write_workflows
printf '%s\n' '      - uses: ./${{ github.action_path }}' >>"$tmp/workflows/ci.yml"
fail dynamic-local-uses
write_workflows
printf '%s\n' 'on: ${{ github.event_name }}' >>"$tmp/workflows/ci.yml"
fail dynamic-event
write_workflows
sed -i.bak 's#image: docker.io/tonistiigi/binfmt@sha256:[a-z]*#image: ${{ env.QEMU_BINFMT_IMAGE }}#' "$tmp/workflows/ci.yml"; rm "$tmp/workflows/ci.yml.bak"
fail dynamic-qemu-pin
write_workflows
sed -i.bak 's/version: v0.34.1/version: ${{ env.BUILDX_VERSION }}/' "$tmp/workflows/ci.yml"; rm "$tmp/workflows/ci.yml.bak"
fail dynamic-buildx-pin

printf '%s\n' 'check-workflow-pins: PASS'
