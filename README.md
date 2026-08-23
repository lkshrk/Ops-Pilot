# Ops-Pilot

Ops-Pilot processes Renovate dependency-update pull requests against a Flux
GitOps repository, one at a time: it decides whether a bump is safe, merges it,
watches the cluster reconcile, and repairs or reverts it when it breaks.

Everything mechanical is Go. The two questions that need reading comprehension —
*does this changelog contain a breaking change that affects my deployment?* and
*what went wrong with this reconcile?* — go to an AI agent with tools.

```
$ ops-pilot run --non-interactive     # it narrates each one, then:

Run summary
PR     DEP                             BUMP              RESULT                                TOOK
#1204  ghcr.io/home-operations/sonarr  4.0.14 -> 4.0.19  MERGED                                1m32s
#1207  postgres                        15 -> 16          SKIPPED major version bump            8s
#1209  ghcr.io/twin/gatus              0.3.6 -> 0.4.0    REVERTED values.env was renamed to…   3m52s

1 merged  1 reverted (#1209)  1 needs you (#1207)  in 6m12s
```

## How it decides

1. **Parse** the dependency, versions and bump class from the Renovate body.
2. **Resolve the changelog**: a configured override first, then the Renovate
   body, the image's `org.opencontainers.image.source`, then search.
3. **Assess** it against your manifests at that head, and upstream issues.
4. **Hold** a major bump, a downgrade, an unclassifiable change, or an unread
   changelog. Evidence clears a bump hold; only found release notes clear a
   changelog hold; a downgrade never clears.
5. **Discuss** the rest, at a terminal only — which is why #1207 is skipped
   above: [docs/cli.md#discussion](docs/cli.md#discussion).
6. **Merge**, trigger Flux, and **watch** the cluster.
7. **Diagnose** a failure or stall, then wait, propose a fix, or give up on it.
8. **Revert** if the fix is declined or nothing works — at a terminal you are
   asked first, and may keep the merge or grant another settle window.

Safety comes from one pull request at a time and comparing cluster health
before and after: a failure counts only if that object was healthy beforehand.

## Install

You need Git 2.38 or newer, a Kubernetes context with read access to Flux
objects and workloads, a GitHub token with `contents: write` and
`pull-requests: write`, and an OpenAI-compatible endpoint.

- **Archive** for macOS or Linux, amd64 or arm64, from
  <https://github.com/lkshrk/ops-pilot/releases>; verify its checksum and SBOM.
- **Go**: `go install github.com/lkshrk/ops-pilot/cmd/ops-pilot@latest`
- **Container**: build the pinned Dockerfile. All three, plus cluster RBAC:
  [docs/install.md](docs/install.md).

## Quick start

```sh
curl -fsSL https://raw.githubusercontent.com/lkshrk/ops-pilot/main/configs/ops-pilot.example.yaml -o ops-pilot.yaml
# edit repository, cluster, flux, and ai: {provider: openai, model: gpt-5.1, baseURL: ..., apiKeyEnv: OPENAI_API_KEY}
export GITHUB_TOKEN=... OPENAI_API_KEY=...

ops-pilot run --dry-run             # decide everything, change nothing
ops-pilot run                       # for real
```

### Before the first real run

- **`pullRequests.authors` must match your bot's login exactly** — `renovate`
  does not match `renovate[bot]`, and a mismatch silently empties the queue.
- **`watch.stabilityHold` must exceed those objects' `retryInterval`**, or the
  default `1m` reverts merges Flux would have fixed:
  [docs/configuration.md#watch](docs/configuration.md#watch).
- **The cluster grants go to the identity you run as**, and a `--dry-run` needs
  none of them: [docs/install.md#cluster-permissions](docs/install.md#cluster-permissions).

## Commands

```
ops-pilot run [--config FILE] [--repo owner/name] [--pr N]
              [--non-interactive] [--dry-run] [--all] [--events FILE]
ops-pilot history [--config FILE] [--run ID] [--last N] [--json]
ops-pilot version

Global: [--verbose] [--quiet]
```

`--dry-run` changes nothing external. `--non-interactive` never prompts, so a
scheduled run takes the safe bumps and leaves the rest. `--pr N` takes one pull
request; `--all` reconsiders what an earlier run reverted or you declined. Every
flag, [exit code](docs/cli.md#exit-status) and discussion rule: [docs/cli.md](docs/cli.md).

## What it remembers, what it writes

**Nothing that changes what it does, except two GitHub labels.** Ops-Pilot
applies `pullRequests.revertedLabel` (default `ops-pilot/reverted`) after a
revert and comments the diagnosis; you apply `pullRequests.declinedLabel`
(default `ops-pilot/declined`) yourself and it only reads that one. Later runs
skip either — remove the label, or pass `--all`. SQLite records what happened
for `ops-pilot history`, and never gates behaviour.

It writes only through the GitHub API, so every commit is signed: merge a pull
request; commit an approved fix to its head and reassess it; commit a revert to
the base branch; comment on and label a pull request; close a superseded one;
annotate the Flux `GitRepository`. There is no local push path.

## Configuration

Every key: [docs/configuration.md](docs/configuration.md); a complete file:
[configs/ops-pilot.example.yaml](configs/ops-pilot.example.yaml). Secrets are
only named there. The one you will likely need is `changelog.overrides`,
because repackaged images publish no source annotation:

```yaml
changelog:
  overrides: [{dependency: ghcr.io/home-operations/sonarr, repository: Sonarr/Sonarr}]
```

## Design

[docs/spec.md](docs/spec.md) states the intended behaviour and
wins wherever the code disagrees with it.
