# Flux controller compatibility

The health verdicts in `internal/adapters/flux` rest on controller *behaviour* verified at
specific versions, not just on the `/api` types `go.mod` pins. The cluster runs whatever
controller version the operator installed; nothing at build time checks it. This note names
the versions three closed review arguments were established against and where to re-check
them when they might have moved.

**Verified against:** source-controller **v1.8.5**, helm-controller **v1.5.5**
(kustomize-controller claims were checked at **v1.8.5** during the B02 review wave).

**Trigger for a re-check:** any bump of the `fluxcd/*-controller/api` pins in `go.mod`, or a
cluster running controllers newer than the versions above.

| Argument (ledger ID) | Behaviour it depends on | Re-check site |
|---|---|---|
| C-L12 — stall is checked ahead of the artifact, deliberately | `MarkStalled` fires only on the `*serror.Stalling` branch; every other class deletes the Stalled condition; `status.artifact` is cleared only when the file is gone from storage or the object is deleted, so a live stall retains the last artifact | source-controller `internal/reconcile/reconcile.go:140`; `internal/controller/gitrepository_controller.go:417`, `:1221` |
| C-M28 — the `Could not load chart` prefix must not be condemned | `v2.ArtifactFailedReason` is spent on both a genuine corrupt chart (`:338`) and transient artifact unavailability (`:333`), distinguishable only by message prefix; `"SourceNotReady"` at `:301` is a bare literal with no API constant | helm-controller `internal/controller/helmrelease_controller.go:301`, `:333`, `:338` |
| C-M25 — `DependencyNotReady` is held open, not condemned | kustomize-controller spends the same reason on the ordinary path of every merge touching a `dependsOn` chain; the dependent object is already `Reconciling` at baseline so `Attributable()` drops it either way | helm-controller `helmrelease_controller.go:236`, `:258`; the `dependsOn` wait path in kustomize-controller |
| Wire strings generally | The literal condition/reason strings the cluster serves match the `meta.*` / `helmv2.*` constants ops-pilot compiles against | enforced by `TestTheWireStringsFluxServesAreStillTheOnesOpsPilotMatches` in `internal/adapters/flux` — but that test only catches a *renamed constant*, not a behaviour change at an unchanged value, which is what this note exists for |

If a re-check finds a drift, reopen the corresponding ledger row rather than patching around
it: each of these arguments closed a finding whose wrong direction is a reverted good merge
or a kept broken one.
