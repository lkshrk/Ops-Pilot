# Configuration

Ops-Pilot reads one YAML file. Start from
[configs/ops-pilot.example.yaml](../configs/ops-pilot.example.yaml).

The file is chosen in this order:

1. `--config FILE`
2. `OPS_PILOT_CONFIG`
3. `ops-pilot.yaml` in the current working directory

Parsing is strict: exactly one YAML document, unknown fields rejected, relative
paths resolved from the configuration file's own directory.

**Secrets are never read from this file.** Tokens, API keys and registry
passwords may only name an environment variable. See
[Environment variables](#environment-variables).

A `run` needs, at minimum: `repository.owner`, `repository.name`,
`cluster.context`, `flux.source.namespace`, `flux.source.name`, `ai.provider`,
`ai.model`, and at least one entry in `pullRequests.authors` or
`pullRequests.labels`. Everything else is defaulted.

## `repository`

| Key | Type | Default | Required | Meaning |
| --- | --- | --- | --- | --- |
| `owner` | string | — | yes | GitHub owner. One segment: no `/`, whitespace, or `.git` suffix. |
| `name` | string | — | yes | GitHub repository name, same rule. |
| `branch` | string | the repository's default branch | no | The base branch pull requests must target. |

## `pullRequests`

At least one of `authors` or `labels` must be set. An empty filter would make
every open pull request a merge candidate, so it is refused rather than
defaulted.

| Key | Type | Default | Required | Meaning |
| --- | --- | --- | --- | --- |
| `authors` | list of string | — | one of the two | Pull request author logins, matched exactly. |
| `labels` | list of string | — | one of the two | Labels, any one of which a pull request must carry. |
| `revertedLabel` | string | `ops-pilot/reverted` | no | Applied by Ops-Pilot after it reverts a merge; later runs skip it. |
| `declinedLabel` | string | `ops-pilot/declined` | no | Applied by you; Ops-Pilot only reads it. Must differ from `revertedLabel`. |

`authors` is matched by exact login, which is the first-run trap the
[README](../README.md#before-the-first-real-run) warns about. Both labels are
permanent until you remove them or pass `--all`.

## `github`

| Key | Type | Default | Required | Meaning |
| --- | --- | --- | --- | --- |
| `tokenEnv` | string | `GITHUB_TOKEN` | no | Name of the environment variable holding the token. |
| `mergeMethod` | string | `squash` | no | One of `merge`, `squash`, `rebase`. |

A repository plan that hides an endpoint answers `403` with a message of its
own — "Upgrade to GitHub Pro or make this repository public to enable this
feature." Ops-Pilot never mistakes that refusal for an empty listing. A `403`
on the open pull request listing or on reading a single pull request stops the
run; a `403` on the releases endpoint is logged and the changelog degrades to
unreadable; a `403` on a pull request's changed files is logged and that pull
request is left open.

## `cluster`

| Key | Type | Default | Required | Meaning |
| --- | --- | --- | --- | --- |
| `context` | string | — | yes | Kubernetes context name to read and annotate. |
| `kubeconfig` | string | `KUBECONFIG`, then the client-go default | no | Path to a kubeconfig file. |

See [Cluster permissions](install.md#cluster-permissions) for the RBAC this
context needs.

## `flux`

| Key | Type | Default | Required | Meaning |
| --- | --- | --- | --- | --- |
| `source.kind` | string | `GitRepository` | no | The only accepted value; anything else is rejected. |
| `source.namespace` | string | — | yes | Namespace of the source to annotate. |
| `source.name` | string | — | yes | Name of the source to annotate. |

Ops-Pilot annotates this object to trigger a reconcile after a merge. There is
no default: a source pointing at the wrong object fails quietly, warning that
the trigger was refused while the cluster fetches at its own next interval.

## `ai`

| Key | Type | Default | Required | Meaning |
| --- | --- | --- | --- | --- |
| `provider` | string | — | yes | `openai` is the only accepted value. |
| `model` | string | — | yes | Model identifier passed to the endpoint. |
| `baseURL` | string | `https://api.openai.com/v1` | no | Absolute HTTP(S) URL, no user information, query, or fragment. |
| `apiKeyEnv` | string | `OPENAI_API_KEY` | no | Name of the environment variable holding the key. |

Any OpenAI-compatible endpoint works: set `baseURL` and `apiKeyEnv` to match it.

## `watch`

| Key | Type | Default | Required | Meaning |
| --- | --- | --- | --- | --- |
| `settleTimeout` | duration | `10m` | no | How long one merge may be watched before the run calls it a stall. |
| `stabilityHold` | duration | `1m` | no | How long a failure must persist before it is confirmed and the merge reverted. |
| `pollInterval` | duration | `10s` | no | How often the cluster is read during a watch. |
| `maxFixAttempts` | integer | `2` | no | How many repairs one merge may be offered. `0` disables repair. |

**`stabilityHold` must exceed the retry interval of the objects a merge can
touch.** A failure that lasts the whole hold is reverted, including one the
object's own controller would have cleared on its next retry. A Flux
`HelmRelease` or `Kustomization` retries after `spec.retryInterval`, which
defaults to `spec.interval` — commonly `5m` to `60m`. Nothing derives the hold
from the cluster, so it is yours to set.

Either raise `stabilityHold` above the longest `retryInterval` among those
objects and raise `settleTimeout` with it, or lower `spec.retryInterval` on the
objects Ops-Pilot watches. Startup rejects two shapes:

```
settleTimeout < 3m + stabilityHold + pollInterval
pollInterval  > stabilityHold / 2
```

The `3m` is the fixed grace an unready workload gets before it counts as
broken; the second rule keeps the hold spanning at least two polls, so no
single sample decides a verdict. Worked example: with the defaults
`settleTimeout: 10m` and `pollInterval: 10s`, the largest admissible hold is
`6m50s`; a `stabilityHold: 8m` needs `settleTimeout` at `11m10s` or more, and
is refused at startup rather than at 3am.

## `fixes`

| Key | Type | Default | Required | Meaning |
| --- | --- | --- | --- | --- |
| `allowedPaths` | list of glob | — | no | Repository-relative patterns an approved fix may write. |

**An empty or absent `allowedPaths` refuses every fix.** There is no permissive
default: until you declare where a repair may write, an unhealthy merge is
reverted rather than repaired. Nothing else changes.

```yaml
fixes:
  allowedPaths:
    - kubernetes/apps/**
    - clusters/staging/**
```

Patterns are matched segment by segment:

| Pattern | Meaning |
| --- | --- |
| `clusters/staging/**` | `clusters/staging` and everything beneath it |
| `clusters/staging` | that path exactly, and nothing beneath it |
| `clusters/*/apps/**` | one segment in place of `*`; `*` never crosses `/` |
| `**/values.yaml` | a file of that name at any depth |
| `**` | every path in the repository |

`*` and `**` are the only wildcards; `?` and `[…]` are rejected. Matching is
case-sensitive. A pattern that is absolute, ends in `/`, or contains `.` or
`..` as a segment is rejected when the configuration loads, not when a fix is
being applied at three in the morning.

Independently of `allowedPaths`, a fix may never write:

- anything under a `.git`-prefixed segment, including `.github` workflows;
- the Flux bootstrap manifests — `gotk-*` and the kustomizations inside a
  `flux-system` directory — which decide where the cluster reconciles from;
- `CODEOWNERS`, Renovate's configuration, `package.json`, or `ops-pilot.yaml`
  at the repository root, which decide who may change the repository.

A directory named `flux-system` that holds ordinary workloads is not refused;
only the files that decide what it reconciles are.

## `changelog`

| Key | Type | Default | Required | Meaning |
| --- | --- | --- | --- | --- |
| `overrides[].dependency` | string | — | no | Dependency name as Renovate writes it. |
| `overrides[].repository` | string | — | no | Upstream `owner/name` to read release notes from. |

An override is consulted **before** the pull request's own release notes and
before the image's annotation, so it corrects them rather than filling in behind
them. The [README](../README.md#configuration) has the worked example.

An override that resolves no releases does not fall back silently: the update is
held for you, exactly as an unreadable changelog is.

## `registries`

Public registries are read anonymously and need no configuration.

| Key | Type | Default | Required | Meaning |
| --- | --- | --- | --- | --- |
| `host` | string | — | yes | Bare registry authority with an optional port: no scheme, path, or user information. Each host may appear once. |
| `username` | string | — | yes | Username offered to that registry's token endpoint. |
| `passwordEnv` | string | — | yes | Name of the environment variable holding the password or token. |

```yaml
registries:
  - {host: ghcr.io, username: lkshrk, passwordEnv: GHCR_TOKEN}
```

Credentials are offered only to the matching registry's own token endpoint,
using Basic authentication on the Docker registry v2 bearer-token exchange.
They are never sent to another registry, never sent across a redirect, and
refused outright if the registry's bearer challenge names a realm on a
different host. A configured environment variable that is unset or blank fails
startup rather than silently falling back to anonymous access.

## `logging`

| Key | Type | Default | Required | Meaning |
| --- | --- | --- | --- | --- |
| `level` | string | `info` | no | One of `debug`, `info`, `warn`. `--verbose` and `--quiet` override it. |

## `paths`

| Key | Type | Default | Required | Meaning |
| --- | --- | --- | --- | --- |
| `historyDatabase` | path | per-OS, below | no (default below) | SQLite file for `ops-pilot history`. |
| `checkoutDirectory` | path | per-OS, below | no | Where the read-only local checkout lives. |

| OS | `historyDatabase` | `checkoutDirectory` |
| --- | --- | --- |
| macOS | `$HOME/Library/Application Support/ops-pilot/history.db` | `$HOME/Library/Caches/ops-pilot/checkouts` |
| Linux | `$XDG_STATE_HOME/ops-pilot/history.db` | `$XDG_CACHE_HOME/ops-pilot/checkouts` |
| Linux, neither variable set | `$HOME/.local/state/ops-pilot/history.db` | `$HOME/.cache/ops-pilot/checkouts` |

## Environment variables

| Variable | Use |
| --- | --- |
| `OPS_PILOT_CONFIG` | configuration path when `--config` is absent |
| the name in `github.tokenEnv` | GitHub API and clone/fetch authentication |
| the name in `ai.apiKeyEnv` | AI provider authentication |
| names in `registries[].passwordEnv` | OCI registry passwords or tokens |
| `KUBECONFIG` | Kubernetes client configuration |

Ops-Pilot loads `./.env` from the directory it is launched in, and only from
there. Values already in the process environment win over `.env`. Start from
[.env.example](../.env.example); the format supports blank lines, whole-line
`#` comments, `NAME=value`, an optional `export`, and single- or double-quoted
single-line values. It supports no interpolation, multiline values, escape
sequences, or inline comments.
