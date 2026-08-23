# Installation

[Prerequisites](../README.md#install) lists what a run needs. Two notes that do
not fit there: interactive prompting requires an attached terminal, and the
health verdicts were established against specific Flux controller versions —
[flux-controller-compatibility.md](flux-controller-compatibility.md) says which,
and when to re-check them.

Releases are built for macOS and Linux on amd64 and arm64. Windows is not
supported; choose the archive matching both your operating system and your
architecture.

Verify what you installed:

```sh
git --version
ops-pilot version
```

## Releases and verification

Download from <https://github.com/lkshrk/ops-pilot/releases>. For an archive
installation, take three files from the same release: the archive for your
target, the release checksum file, and the SBOM for that archive.

Compare the archive with its unique checksum entry before extraction:

```bash
ARCHIVE=/path/to/downloaded-archive
CHECKSUMS=/path/to/downloaded-checksum-file

archive_name=$(basename "$ARCHIVE")
expected=$(
  awk -v name="$archive_name" '$2 == name { print $1 }' "$CHECKSUMS"
)
test -n "$expected"
test "$(printf '%s\n' "$expected" | wc -l | tr -d ' ')" -eq 1

if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$ARCHIVE" | awk '{ print $1 }')
else
  actual=$(shasum -a 256 "$ARCHIVE" | awk '{ print $1 }')
fi

test "$actual" = "$expected"
```

Do not install if the archive has no corresponding SBOM, if the SBOM describes
a different archive, or if any published digest differs. The pinned build
inputs used to produce the distributions are recorded in
[build/tool-versions.env](../build/tool-versions.env).

After verification, inspect and extract into a new temporary directory, then
install onto `PATH`:

```sh
destination=$(mktemp -d)
tar -tzf "$ARCHIVE"
tar -xzf "$ARCHIVE" -C "$destination"

install -d "$HOME/.local/bin"
install -m 0755 "$destination/ops-pilot" "$HOME/.local/bin/ops-pilot"
```

## Go install

```sh
go install github.com/lkshrk/ops-pilot/cmd/ops-pilot@latest
```

This builds from source and reports `dev` as its version, because the release
version is injected at link time.

## Homebrew

Each release publishes a cask to the `lkshrk/tap` tap; it installs the release
binary and nothing else:

```sh
brew install --cask lkshrk/tap/ops-pilot
```

## Container

Build the pinned multi-stage Dockerfile from a source checkout:

```sh
docker build --tag ops-pilot:local .
```

The runtime image runs as `ops-pilot` with fixed UID and GID `65532`, and
contains Git and no Go toolchain. It sets `XDG_STATE_HOME=/state` and
`XDG_CACHE_HOME=/cache`, which is exactly where the defaults put the history
database and the checkout: drop the example config's `paths` block and no
override is needed. `/checkout` is declared as well, for an operator who would
rather give the checkout a volume of its own.

```sh
docker volume create ops-pilot-state
docker volume create ops-pilot-cache

export KUBECONFIG="${KUBECONFIG:-$HOME/.kube/config}"

docker run --rm \
  --mount type=volume,src=ops-pilot-state,dst=/state \
  --mount type=volume,src=ops-pilot-cache,dst=/cache \
  --mount type=bind,src="$PWD/ops-pilot.yaml",dst=/config/ops-pilot.yaml,readonly \
  --mount type=bind,src="$KUBECONFIG",dst=/config/kubeconfig,readonly \
  --env OPS_PILOT_CONFIG=/config/ops-pilot.yaml \
  --env KUBECONFIG=/config/kubeconfig \
  --env GITHUB_TOKEN \
  --env OPENAI_API_KEY \
  ops-pilot:local run --non-interactive
```

Add `--env NAME` for every variable a registry password references. Both data
mounts must stay writable by the image's non-root user. The container does not
inherit the host `.env`: pass secrets with `--env`, or mount it at
`/home/ops-pilot`, the image's working directory.

## Cluster permissions

Ops-Pilot reads cluster health to attribute a merge, and annotates the
configured Flux `GitRepository` to trigger reconciliation — no other cluster
write.

**A `--dry-run` needs none of this.** It needs a reachable `cluster.context`
whose API server serves the three Flux CRDs, because the clients are built and
the APIs discovered at startup; it never reads workload health and never
annotates anything. The grants below, and a `watch.stabilityHold` sized against
your objects, matter from the first real `run`.

[docs/rbac.yaml](rbac.yaml) grants them to a **ServiceAccount**, which is what a
scheduled or in-cluster run uses. A run under your own kubeconfig user needs
that user to hold the same grants; applying this file will not give them to you.
Every line marked `SET ME` is yours to fill in — the ServiceAccount name and
namespace in both bindings, `metadata.namespace` on the `Role` and `RoleBinding`
(which must equal `flux.source.namespace`), and `resourceNames` on the `Role`
(which must equal `flux.source.name`):

```sh
curl -fsSL https://raw.githubusercontent.com/lkshrk/ops-pilot/main/docs/rbac.yaml -o rbac.yaml
# edit every SET ME line, then:
kubectl apply -f rbac.yaml
```

Whatever identity you run as, verify against the context and the Flux source
ops-pilot is configured to use — this probe matters more than the apply:

```sh
CONTEXT='<cluster.context>'          # cluster.context from your ops-pilot.yaml
FLUX_NS='<flux.source.namespace>'    # flux.source.namespace from your ops-pilot.yaml
FLUX_NAME='<flux.source.name>'       # flux.source.name from your ops-pilot.yaml

for probe in \
  "get kustomizations.kustomize.toolkit.fluxcd.io" \
  "list kustomizations.kustomize.toolkit.fluxcd.io" \
  "get helmreleases.helm.toolkit.fluxcd.io" \
  "list helmreleases.helm.toolkit.fluxcd.io" \
  "get gitrepositories.source.toolkit.fluxcd.io" \
  "list gitrepositories.source.toolkit.fluxcd.io" \
  "list deployments.apps" \
  "list statefulsets.apps" \
  "list daemonsets.apps" \
  "list replicasets.apps" \
  "list jobs.batch" \
  "list pods" \
  "list events" \
  "get pods --subresource=log"
do
  printf '%-52s ' "$probe"
  kubectl --context "$CONTEXT" auth can-i $probe --all-namespaces || true
done

printf '%-52s ' "get gitrepositories.../$FLUX_NAME"
kubectl --context "$CONTEXT" auth can-i get gitrepositories.source.toolkit.fluxcd.io/"$FLUX_NAME" -n "$FLUX_NS" || true
printf '%-52s ' "patch gitrepositories.../$FLUX_NAME"
kubectl --context "$CONTEXT" auth can-i patch gitrepositories.source.toolkit.fluxcd.io/"$FLUX_NAME" -n "$FLUX_NS" || true
```

Every line must answer `yes`. They are not equally serious:

| Probe | If it answers `no` |
| --- | --- |
| the three Flux kinds | fatal at startup: `required Flux API ... is not served`, or `Forbidden` |
| `deployments`, `statefulsets`, `daemonsets` | fatal: the health snapshot cannot be built and the pull request fails with `could not read the cluster` |
| `pods` | fatal under the same failure once any StatefulSet or DaemonSet exists and the per-namespace fallback is also refused; otherwise diagnosis loses the failing workload's own account of itself |
| `events` | diagnosis loses the failing workload's own account of itself |
| `replicasets`, `jobs` | lineage falls back to matching pods on names alone |
| `pods/log` | diagnosis degrades rather than failing the run |
| the namespaced `patch` | the reconcile trigger is warned about and skipped; Flux fetches at its next interval |

## Next

Configure it: [configuration.md](configuration.md). Run it:
[cli.md](cli.md).
