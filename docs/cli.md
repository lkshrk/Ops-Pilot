# CLI reference

```text
ops-pilot run [--config FILE] [--repo owner/name] [--pr N]
              [--non-interactive] [--dry-run] [--all] [--events FILE]
ops-pilot history [--config FILE] [--run ID] [--last N] [--json]
ops-pilot version

Global: [--verbose] [--quiet]
```

Every key these commands read is documented in
[configuration.md](configuration.md).

## Run

Processes every open pull request matching `pullRequests.authors` and
`pullRequests.labels`, oldest first, one at a time.

| Flag | Meaning |
| --- | --- |
| `--config FILE` | configuration file to load, ahead of `OPS_PILOT_CONFIG` and `./ops-pilot.yaml` |
| `--repo owner/name` | override the configured repository |
| `--pr N` | process only this pull request |
| `--dry-run` | decide everything, change nothing: nothing merged, committed, commented or labelled |
| `--non-interactive` | never prompt; anything needing approval is recorded and reported instead |
| `--all` | reconsider pull requests set aside by `revertedLabel` or `declinedLabel` |
| `--events FILE` | append one JSON object per decision and per external change to FILE |

One `--events` line, as it is written:

```json
{"ts":"2026-08-23T09:14:02Z","event":"merged","runId":"20260823-091152.004881000","repository":"lkshrk/h-cloud","pr":1204,"dependency":"ghcr.io/home-operations/sonarr","from":"4.0.14","to":"4.0.19","sha":"8f3a21c9d4e5f60718293a4b5c6d7e8f90a1b2c3"}
```

`event` is one of `run_started`, `assessed`, `skipped`, `merged`, `diagnosed`,
`fix_applied`, `reverted`, `kept`, `labelled`, `closed`, `watch_result`,
`failed`, `halted`, `run_finished`. Fields a kind does not carry are omitted
rather than emitted empty, so a consumer can match on presence.

Prompting needs an attached terminal. Without one, the run behaves as if
`--non-interactive` were passed.

A pull request that needs approval, is superseded, or was reverted by an
earlier run never stops the run; it is recorded and the next one begins.

Worked invocations: `ops-pilot run --help`.

## Discussion

An assessment Ops-Pilot cannot settle on its own opens a free-form discussion,
but only at an attached terminal. There is no merge/decline/later menu.

Ops-Pilot shows that it is thinking, streams concise assistant updates and a
conclusion, then prints:

```text
  Enter or /skip to leave this PR pending.
  you        >
```

Anything you type goes to the AI — including plain `skip`, `later` and `defer`,
which it weighs in context and may answer with a structured defer. Only Enter
on an empty line, `/skip`, `q`, `quit`, `cancel` and Esc are local escapes, any
letter case; they are never sent to the AI and leave the pull request pending.

There is no turn limit. A discussion ends when:

- the agent reaches a safe assessment **carrying evidence** — then the pull
  request merges;
- the agent defers, or you use a local escape, or the input stream ends;
- the pull request's head moves while you are answering, or cannot be re-read;
- the discussion produced no evidence that the update is safe.

Everything except the first leaves the pull request pending for a later run.

The AI is read-only. Instead of a question it may propose one repository
change, which replaces the discussion with an exact diff and its own approval:

```text
  ? Apply this repair?
    [a]      Apply fix
    [enter]  Skip fix; choose whether to revert (default)
```

An approved diff must match `fixes.allowedPaths` and counts against
`watch.maxFixAttempts`. It is committed to the Renovate pull request's own
head, after which the checkout and changed files are re-read and the update is
assessed again — only that later safe assessment can merge it. Nothing is
applied under `--dry-run`, with no `fixes.allowedPaths` configured, or when the
pull request's head is not a writable branch in this repository: forks and
foreign heads are left for manual handling.

Different holds clear differently:

| Hold | What clears it |
| --- | --- |
| a major bump, or a version change that could not be classified | a safe assessment carrying evidence that the breaking change misses your deployment |
| a changelog that could not be read, resolved no releases, or is incomplete over the version range | only the agent finding the release notes and returning their URL; evidence alone does not |
| a version downgrade, or a pull request that could not be checked out | nothing — no discussion clears these |

A `--non-interactive` run, or one with no terminal, never discusses anything.
The pull request is left pending and listed as `SKIPPED` in the summary.

## History

Replays past runs from the SQLite history log.

| Flag | Meaning |
| --- | --- |
| `--config FILE` | configuration file to load |
| `--run ID` | show a single run by id |
| `--last N` | how many runs to show; defaults to `10` |
| `--json` | emit the full record rather than the table |

Worked invocations: `ops-pilot history --help`.

## Version

```text
ops-pilot version
```

Prints `ops-pilot VERSION (commit SHA, built DATE)`. Help and version load no
configuration and check no runtime prerequisites.

## Global flags

`--verbose` explains each decision and shows the agent's working. `--quiet`
prints only the final summary. Both are accepted by every command, including
`version`, where `--quiet` prints only `ops-pilot VERSION` and `--verbose` adds
the Go runtime and platform.

Passing `--quiet` and `--verbose` together is a usage error and exits `1`.

## Exit status

| Code | Meaning |
| --- | --- |
| `0` | help, version, or a completed run, whatever the individual verdicts were |
| `1` | usage, configuration, or prerequisite failure |
| `2` | a failure that stopped the run before it finished, including a run halted after a merge whose result could not be established |
| `130` | interrupt |

`1` covers what the configuration loader rejects. A configuration that loads but
does not work — the commonest being a `cluster.context` that is not in your
kubeconfig — surfaces while the run is starting and exits `2`.
