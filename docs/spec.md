# Ops-Pilot v2 — Product Spec

Status: authoritative; wins wherever the code disagrees with it.
on merge of the `rewrite` branch. Where code disagrees with this file, this file wins.

Ops-Pilot v2 is the `upgrade-deps` skill compiled into a CLI: every mechanical step
is Go, every judgement step is an AI agent with tools.

## Purpose

Process open Renovate pull requests on a Flux GitOps repository, one at a time:
decide whether the bump is safe, merge it, watch the cluster reconcile, and repair
or revert if it breaks.

## Non-goals

Explicitly abandoned from v1. None of these return.

- Artifact identity proof. No binding of git change to registry digest to running
  pod. No preflight verification, no target resolution, no coverage invariant.
- Durable state machine. No epochs, leases, fencing, mutation journal, or resume.
- Evidence classification. No `unobservable` class, no target-correlation predicate,
  no decision-precedence table.
- Quote verification of AI output. The AI is advisory; the post-merge watch is the gate.
- HTTP endpoint health checks.
- Publication embargo, explicit queue ordering.

## The loop

```
load config, open sqlite, clone/fetch repo
discover open Renovate PRs
close superseded PRs
skip PRs labelled ops-pilot/reverted

for each PR:
  1. parse        dependency, from, to, bump class, release notes from body
  2. major?       -> soft unresolved
  3. changelog    PR body -> OCI image.source -> config override -> AI search
                  none found -> soft unresolved
  4. assess       AI agent: breaking change? does it touch our config?
                  upstream open issues about this version?
                  breaking-and-relevant, or unsure -> soft unresolved
  5. discussion   real TTY: open chat (no merge/decline/later prompt)
                  show thinking immediately; stream concise assistant status/conclusion
                  user replies at "you >"; reassess until a terminal outcome
                  safe + evidence -> merge; approved exact diff -> commit, reassess
                  hard hold, non-interactive, local control command/EOF, model defer, or error -> pending, next PR
  6. snapshot     cluster health baseline
  7. merge        PR merged via GitHub API
  8. reconcile    trigger Flux, wait until the source artifact revision is the merge SHA
  9. watch        poll until settled or timeout; diff against baseline
 10. verdict      PASS  -> record, next PR
                  FAIL or STALLED -> repair
 11. repair       AI diagnosis: benign-wait | fix proposal | unfixable
                  benign-wait  -> extend watch once
                  fix proposal -> show diff, approve -> commit to main -> re-watch
                                  decline -> revert
                  unfixable    -> revert
 12. revert       revert commit on main, watch until baseline restored,
                  label ops-pilot/reverted, comment diagnosis, next PR

print summary
```

## Cluster health snapshot

A snapshot is taken immediately before merge and re-taken throughout the watch.
It records, cluster-wide:

- Flux `Kustomization`: `namespace/name` -> Ready condition + observed revision
- Flux `HelmRelease`: `namespace/name` -> Ready condition
- `Deployment` / `StatefulSet` / `DaemonSet`: `namespace/name` -> ready vs desired replicas
- `Pod`: `namespace/name` -> phase, container ready, restart count, waiting reason

Pods inform diagnosis only. The verdict is computed from the first three.

**Diff rule.** A failure counts only if the object was healthy in the baseline and is
unhealthy now, if the object did not exist in the baseline and is unhealthy now, or
if it was healthy in the baseline and has since disappeared — a merge that deletes a
running application must not read as a clean window. Pre-existing breakage is
invisible by construction, including when it disappears. Attribution is sound because
exactly one PR is ever in flight.

## Watch termination

Failures are attributed only once the Flux source carries the expected revision:
before that the merge has not been applied, so nothing that breaks is its fault.

An object is *reconciling*, not failed, when its controller has not yet observed
the current generation, or when it reports `Progressing`,
`ProgressingWithRetry`, or `DependencyNotReady`. Fetching a commit reconciles
the whole tree, so every Kustomization behind a `dependsOn` chain goes
not-Ready for a while; counting those would blame the merge for objects it
never touched.

- **PASS** — the Flux source artifact revision equals the merge SHA, no object is
  still reconciling, the diff is empty, and that has held for `watch.stabilityHold`.
- **FAIL** — the diff is non-empty.
- **STALLED** — `watch.settleTimeout` elapsed with objects still progressing.

## Halting

A pull request that needs a human never stops the run. A pull request that
leaves the cluster in a state later attempts cannot be judged against does:

- a merge whose reaction could not be observed,
- a revert that could not be committed,
- a revert that landed without restoring the baseline.

The run records why it halted and reports it in the summary. Everything already
processed keeps its verdict.

## Repair

The AI diagnosis agent receives the failing objects, their conditions, pod events and
container logs, the PR, and repo access. It returns exactly one of:

- `benign_wait` — nothing is wrong, it is slow. Extends the watch once by
  `watch.settleTimeout`. Available at most once per PR.
- `fix` — a concrete unified diff against repo files, with a stated cause. Shown for
  approval; on approval it is committed to `main` via the GitHub commit API and the
  watch restarts. Bounded by `watch.maxFixAttempts`.
- `unfixable` — with a stated reason. Goes straight to revert.

Declining a proposed fix reverts.

## Revert

A revert restores every path the pull request touched — both ends of a rename — to
its content on the branch **immediately before this merge**, not to the pull
request's own base, which may be many merges old. It is committed directly to
`main` through the GitHub commit API, so it is signed and satisfies branch
protection. There is no revert PR and no local push path — the cluster is never
left broken waiting on a human.

After the revert lands, the watch runs again until the baseline snapshot is restored.
The Renovate PR is labelled `ops-pilot/reverted` and receives a comment carrying the
diagnosis. The run continues with the next PR.

## Memory

Two stores, neither authoritative over the other.

- **GitHub labels** are the only behavioural memory. `ops-pilot/reverted` marks a merge
  that had to be undone. A declined exact diff leaves the pull request pending without a
  decline label. Removing the reverted label by hand, or running `--all`, re-arms it.

  Only a separately approved exact diff or merge decision is recorded. A non-interactive
  run, cancelled chat, EOF, and errors leave soft unresolved bumps pending and are
  never written back.
- **SQLite** is a passive history log. It never gates behaviour.

```
runs(id, started_at, finished_at, repo, mode)
attempts(id, run_id, pr, dependency, from_version, to_version, bump_class,
         decision, ai_reason, changelog_source, merge_sha, verdict,
         fix_diff, fix_attempts, revert_sha, duration_ms, error)
```

`--events FILE` appends one JSON object per decision and per external write as
they happen, so an unattended run can be alerted on rather than only read
afterwards. It records what was decided and changed, never narration, and a
write failure is reported once and then ignored: the stream observes a run that
is changing a cluster and must never be able to stop one.

A crash mid-watch is not recovered. The PR is merged and unwatched; the next run's
baseline absorbs it. This is accepted.

## AI agent

One agent, two prompts (assessment, diagnosis). Tools available to both:

| Tool | Purpose |
|---|---|
| `read_repo_file(path, ref)` | any file in the GitOps repo at any ref |
| `search_code(query)` / `search_graph(query)` | codebase-memory MCP over the repo |
| `list_repo_files(glob)` | locate manifests |
| `github_search_repos(q)` | find the upstream project when unlinked |
| `github_releases(repo, from, to)` | release notes between two versions |
| `github_issues(repo, query)` | open issue check for the target version |
| `fetch_url(url)` | changelogs, docs, release pages |

Diagnosis additionally gets `kube_events(ns, name)`, `kube_logs(ns, pod, container)`,
`flux_status(kind, ns, name)`.

No redaction. Secrets in the GitOps repo are SOPS-encrypted; the agent reads
ciphertext. `internal/evidence/sanitize.go` is deleted.

Assessment returns `{verdict: safe|clarify|needs_approval, reason, evidence[], question?, diff?}`.
Diagnosis returns `{action: benign_wait|fix|unfixable, cause, diff?}`.

In an interactive bump discussion, the terminal shows a thinking status before
the assistant response and streams only concise status and conclusion text.
It does not expose hidden reasoning. The user replies at `you >`; there is no
fixed turn limit. Lines that look like credentials may be buffered or redacted
before display. Plain `skip`, `later`, and `defer` remain chat input; the model
may return a structured defer that leaves only the current PR pending. Pressing
Enter, `/skip`, `q`, `quit`, or `cancel` is a local control command and is
never sent to the model.

## CLI surface

```
ops-pilot run [--non-interactive] [--dry-run] [--all] [--events FILE]
              [--repo owner/name] [--pr N]
ops-pilot history [--run ID] [--last N] [--json]
ops-pilot version
```

`analyze` and `resume` are removed.

## Config

```yaml
repo: lkshrk/h-cloud
workDir: ~/.ops-pilot
kubeconfig: ~/.kube/config

github:
  tokenEnv: GITHUB_TOKEN
  mergeMethod: squash

ai:
  provider: openai
  model: <pinned>
  apiKeyEnv: OPENAI_API_KEY

watch:
  settleTimeout: 10m
  stabilityHold: 60s
  pollInterval: 10s
  maxFixAttempts: 2

changelog:
  overrides:
    ghcr.io/home-operations/sonarr: Sonarr/Sonarr
```

Secrets are referenced by environment-variable name only, never inline.

## Verdicts

`MERGED` · `FIXED` · `REVERTED` · `SKIPPED(reason)` · `ERROR`

## Summary

Printed at end of run, and replayable via `ops-pilot history`:

```
PR    DEP            BUMP           RESULT
1204  sonarr         4.0.14>4.0.19  MERGED    2m14s
1207  postgres       15>16          SKIPPED   major
1209  gatus-sidecar  0.3.6>0.4.0    REVERTED  envs.TZ renamed
1211  grafana        11.3>11.4      MERGED    1m02s

2 merged  1 reverted  1 needs you
```

## Testing gate

Behavioural, fake-backed, in-process. Fake GitHub, fake Kubernetes/Flux, fake AI.

Required end-to-end flows:

1. clean merge — safe bump, healthy reconcile, PASS
2. major bump — soft unresolved, streamed open chat in a real TTY; plain `skip`, `later`, or `defer` reaches the model, which may conclude structured defer, while Enter, `/skip`, `q`, `quit`, and `cancel` stay local; pending in non-interactive
3. breaking change touching our config — soft unresolved, open chat, exact diff approval, reassessment
4. no changelog anywhere — soft unresolved, open chat or pending
5. failure, AI fix approved, re-watch passes — FIXED
6. failure, AI fix declined — REVERTED, labelled, commented
7. failure, unfixable — REVERTED
8. stalled, AI says benign, extended watch passes — MERGED
9. pre-existing cluster breakage is ignored by the diff
10. PR labelled `ops-pilot/reverted` is skipped

Unit tests only where logic is fiddly: PR body parsing, semver bump class, health
diff, changelog chain fallback. No per-package mock suites, no coverage thresholds.

## Decisions, all closed

- **OPEN-1 — a stall is diagnosed exactly like a failure.** The agent may answer
  `benign_wait` once; when that extension expires it is told the wait is spent and
  must choose `fix` or `unfixable`. There is no separate rule for "never observed
  either way" — the agent decides with the evidence in front of it.
- **OPEN-2 — merge method.** Defaults to `squash`, configurable as
  `github.mergeMethod`.
- **OPEN-3 — no model pinned, and none needed.** The two provider failures seen in
  a 31-pull-request run were a gateway in front of the model timing out, not the
  model. `ai.model` carries whatever is configured; a provider error degrades that
  pull request to manual approval and the run continues.
- **OPEN-4 — Flux is triggered** by patching `reconcile.fluxcd.io/requestedAt` onto
  the configured GitRepository through the Kubernetes API. No `flux` binary.
- **OPEN-5 — the defaults hold.** A real merge settled in 2m46s against a
  143-Kustomization cluster, well inside `settleTimeout: 10m` and
  `stabilityHold: 1m`.

## Known coverage gap

The revert path is exercised by the behavioural suite but has never fired against a
real cluster: that needs a dependency bump that genuinely breaks a deployment, which
is not worth manufacturing in production. Everything up to it — merge, reconcile
trigger, watch, diagnosis, `benign_wait`, and the fix path's approval gate — has run
live.

## Implementation notes

Two deliberate deviations from the original plan, both recorded here rather than
left to be rediscovered:

- The v1 git adapter (985 lines of worktree locking and recovery) was replaced by
  `internal/checkout`: a blobless clone that fetches the pull request's head before
  the agent assesses it. Nothing is pushed from it. A checkout failure degrades the
  assessment rather than failing the run — the agent still sees the changelog and
  the changed-file list, and asks when it cannot read the manifests.
- The OCI client was kept whole instead of being cut to a reference parser. It
  already returns the `org.opencontainers.image.source` annotation from `Resolve`,
  and rewriting its manifest walk would risk reintroducing the ghcr placeholder-scope
  ping bug and the attestation-manifest collision.
- Fixes and reverts are applied to file contents in memory (`internal/patch`) and
  committed through GitHub's commit mutation, rather than being applied in a working
  tree and pushed. A revert restores every path the pull request touched to its
  content at the merge base; a fix hunk whose context cannot be located is refused
  rather than forced. The local checkout exists only so the agent and
  codebase-memory can read the repository.
