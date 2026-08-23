#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
manifest=${TOOL_VERSIONS_FILE:-"$root/build/tool-versions.env"}
workflows=${1:-"$root/.github/workflows"}

fail() { printf '%s\n' "check-workflow-pins: $*" >&2; exit 1; }
[ -f "$manifest" ] || fail "missing manifest: $manifest"
[ -d "$workflows" ] || fail "missing workflows: $workflows"

tmp=$(mktemp "${TMPDIR:-/tmp}/ops-pilot-workflow-pins.XXXXXX")
trap 'rm -f "$tmp"' EXIT HUP INT TERM

expected_action_keys='ACTIONS_CHECKOUT_SHA
ACTIONS_SETUP_GO_SHA
ACTIONS_UPLOAD_ARTIFACT_SHA
ACTIONS_DOWNLOAD_ARTIFACT_SHA
GITHUB_CODEQL_ACTION_SHA
DOCKER_LOGIN_ACTION_SHA
DOCKER_METADATA_ACTION_SHA
SETUP_QEMU_ACTION_SHA
SETUP_BUILDX_ACTION_SHA
DOCKER_BUILD_PUSH_ACTION_SHA
SOFTPROPS_ACTION_GH_RELEASE_SHA'

while IFS= read -r line || [ -n "$line" ]; do
	printf '%s\n' "$line" | grep -Eq '^[A-Z][A-Z0-9_]*=[A-Za-z0-9._:/@+-]+$' || fail "malformed manifest line"
	key=${line%%=*}
	value=${line#*=}
	grep -q "^${key}	" "$tmp" && fail "duplicate manifest key: $key"
	printf '%s\t%s\n' "$key" "$value" >>"$tmp"
done <"$manifest"

for key in $expected_action_keys; do
	grep -q "^${key}	" "$tmp" || fail "missing manifest action key: $key"
done
awk -F '\t' '$1 ~ /_ACTION_SHA$/ { print $1 " " $2 }' "$tmp" |
while IFS=' ' read -r key value; do
	printf '%s\n' "$expected_action_keys" | grep -qx "$key" || fail "unknown manifest action key: $key"
	printf '%s\n' "$value" | grep -Eq '^[0-9a-f]{40}$' || fail "invalid action SHA: $key"
done

value() { awk -F '\t' -v key="$1" '$1 == key { print $2; exit }' "$tmp"; }
action_key() {
	case "$1" in
		actions/checkout) printf '%s\n' ACTIONS_CHECKOUT_SHA ;;
		actions/setup-go) printf '%s\n' ACTIONS_SETUP_GO_SHA ;;
		actions/upload-artifact) printf '%s\n' ACTIONS_UPLOAD_ARTIFACT_SHA ;;
		actions/download-artifact) printf '%s\n' ACTIONS_DOWNLOAD_ARTIFACT_SHA ;;
		github/codeql-action) printf '%s\n' GITHUB_CODEQL_ACTION_SHA ;;
		docker/login-action) printf '%s\n' DOCKER_LOGIN_ACTION_SHA ;;
		docker/metadata-action) printf '%s\n' DOCKER_METADATA_ACTION_SHA ;;
		docker/setup-qemu-action) printf '%s\n' SETUP_QEMU_ACTION_SHA ;;
		docker/setup-buildx-action) printf '%s\n' SETUP_BUILDX_ACTION_SHA ;;
		docker/build-push-action) printf '%s\n' DOCKER_BUILD_PUSH_ACTION_SHA ;;
		softprops/action-gh-release) printf '%s\n' SOFTPROPS_ACTION_GH_RELEASE_SHA ;;
		*) return 1 ;;
	esac
}

used="$tmp.used"
: >"$used"
found=0
for workflow in "$workflows"/*.yml "$workflows"/*.yaml; do
	[ -f "$workflow" ] || continue
	found=1
	grep -Eq '(^|[^A-Za-z0-9_])pull_request_target([^A-Za-z0-9_]|$)' "$workflow" && fail "pull_request_target: $workflow"
	grep -Eq '^[[:space:]]*on:[[:space:]].*\$\{\{' "$workflow" && fail "dynamic event form: $workflow"
	grep -Eq '^[[:space:]]*permissions:[[:space:]]*[^#[:space:]]' "$workflow" && fail "unsupported permission form: $workflow"
	grep -Eq '^[[:space:]]*(id-token|attestations):' "$workflow" && fail "forbidden permission: $workflow"
	grep -Eq 'cache-(from|to)[[:space:]]*:' "$workflow" && fail "cache import/export: $workflow"
	grep -Eq '\{[^#]*uses[[:space:]]*:' "$workflow" && fail "unsupported flow-map action: $workflow"

	awk '/^[[:space:]]*(-[[:space:]]+)?uses:[[:space:]]*/ { sub(/^[[:space:]]*(-[[:space:]]+)?uses:[[:space:]]*/, ""); print }' "$workflow" |
	while IFS= read -r use; do
		printf '%s\n' "$use" | grep -Fq '${{' && fail "dynamic action ref: $use"
		case "$use" in
			./*) continue ;;
		esac
		repo=${use%@*}
		ref=${use#*@}
		[ "$repo" != "$use" ] || fail "action ref missing: $use"
		printf '%s\n' "$ref" | grep -Eq '^[0-9a-f]{40}$' || fail "non-immutable action ref: $use"
		case "$repo" in github/codeql-action/*) repo=github/codeql-action ;; esac
		key=$(action_key "$repo") || fail "unmanifested action: $repo"
		[ "$ref" = "$(value "$key")" ] || fail "action SHA mismatch: $repo"
		printf '%s\n' "$key" >>"$used"
	done

	awk -v qemu_sha="$(value SETUP_QEMU_ACTION_SHA)" -v qemu_image="$(value QEMU_BINFMT_IMAGE)" \
		-v buildx_sha="$(value SETUP_BUILDX_ACTION_SHA)" -v buildx_version="$(value BUILDX_VERSION)" -v buildkit_image="$(value BUILDKIT_IMAGE)" '
		function clear() { kind=""; image_count=version_count=driver_count=buildkit_count=state_count=0 }
		function finish() {
			if (kind == "qemu" && image_count != 1) invalid=1
			if (kind == "buildx" && (version_count != 1 || driver_count != 1 || buildkit_count != 1 || state_count != 1)) invalid=1
			clear()
		}
		function setup(kind_name) { finish(); kind=kind_name }
		/^[[:space:]]*-[[:space:]]+/ { finish() }
		/^[[:space:]]*(-[[:space:]]+)?uses:[[:space:]]*/ {
			if ($0 ~ "docker/setup-qemu-action@" qemu_sha "[[:space:]]*$") setup("qemu")
			else if ($0 ~ "docker/setup-buildx-action@" buildx_sha "[[:space:]]*$") setup("buildx")
			next
		}
		kind == "qemu" && $0 ~ "^[[:space:]]*image:[[:space:]]*" qemu_image "[[:space:]]*$" { image_count++ }
		kind == "buildx" && $0 ~ "^[[:space:]]*version:[[:space:]]*" buildx_version "[[:space:]]*$" { version_count++ }
		kind == "buildx" && $0 ~ "^[[:space:]]*driver:[[:space:]]*docker-container[[:space:]]*$" { driver_count++ }
		kind == "buildx" && $0 ~ "^[[:space:]]*image=" buildkit_image "[[:space:]]*$" { buildkit_count++ }
		kind == "buildx" && $0 ~ "^[[:space:]]*keep-state:[[:space:]]*false[[:space:]]*$" { state_count++ }
		END { finish(); exit invalid ? 1 : 0 }
	' "$workflow" || fail "setup action configuration mismatch: $workflow"

	awk -v release="$(basename "$workflow")" '
	function bad(message) { print message > "/dev/stderr"; exit 1 }
	function check_global(  count) {
		if (!global_seen || global_count != 1 || global["contents"] != "read") bad("invalid workflow permissions")
	}
	function check_job(  allowed) {
		if (!in_perm) return
		if (job == "publish" && release == "release.yml") {
			if (perm_count != 2 || perm["contents"] != "write" || perm["packages"] != "write") bad("invalid publish permissions")
		} else if (job == "verify-published" && release == "release.yml") {
			if (perm_count != 2 || perm["contents"] != "read" || perm["packages"] != "read") bad("invalid verify-published permissions")
		} else if (release == "codeql.yml") {
			if (perm_count != 2 || perm["contents"] != "read" || perm["security-events"] != "write") bad("invalid CodeQL permissions")
		} else if (perm_count != 1 || perm["contents"] != "read") bad("invalid job permissions")
		delete perm; perm_count=0; in_perm=0
	}
	function finish(  x) { check_job(); check_global() }
	/^permissions:[[:space:]]*$/ { global_seen=1; global_mode=1; next }
	global_mode && /^  [a-z-]+:[[:space:]]*(read|write)[[:space:]]*$/ {
		sub(/^  /, ""); split($0, a, /:[[:space:]]*/); global[a[1]]=a[2]; global_count++; next
	}
	global_mode { global_mode=0 }
	/^  [A-Za-z0-9_-]+:[[:space:]]*$/ { check_job(); job=$0; sub(/^  /, "", job); sub(/:.*/, "", job); next }
	/^    permissions:[[:space:]]*$/ { check_job(); in_perm=1; next }
	in_perm && /^      [a-z-]+:[[:space:]]*(read|write)[[:space:]]*$/ {
		sub(/^      /, ""); split($0, a, /:[[:space:]]*/); perm[a[1]]=a[2]; perm_count++; next
	}
	in_perm { check_job() }
	END { finish() }
	' "$workflow" || fail "permission mismatch: $workflow"
done
[ "$found" -eq 1 ] || fail "no workflows found"

for key in $expected_action_keys; do
	grep -qx "$key" "$used" || fail "unused manifest action: $key"
done
