package flux

import (
	"context"
	"testing"

	helmv2 "github.com/fluxcd/helm-controller/api/v2"
	"github.com/fluxcd/pkg/apis/meta"
	"github.com/lkshrk/ops-pilot/internal/domain"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
)

func conditions(reason string, status metav1.ConditionStatus) []metav1.Condition {
	return []metav1.Condition{{
		Type:    meta.ReadyCondition,
		Status:  status,
		Reason:  reason,
		Message: "waiting on something",
	}}
}

// Fetching a new commit reconciles the whole tree, so Kustomizations behind a
// dependsOn chain report Ready=False for a while. Reading those as failures
// blames the merge for objects it never touched.
func TestObjectWaitingOnItsDependenciesIsReconcilingNotBroken(t *testing.T) {
	for _, reason := range []string{
		meta.DependencyNotReadyReason,
		meta.ProgressingReason,
		meta.ProgressingWithRetryReason,
	} {
		t.Run(reason, func(t *testing.T) {
			got := health("Kustomization", "cert-manager", "trust-manager",
				2, 2, conditions(reason, metav1.ConditionFalse), "")

			if !got.Reconciling {
				t.Fatalf("want %s treated as reconciling", reason)
			}
			if !got.Healthy {
				t.Fatalf("want %s held open rather than attributed as a failure", reason)
			}
		})
	}
}

func TestGenuinelyFailedObjectIsAFailure(t *testing.T) {
	for _, reason := range []string{
		meta.ReconciliationFailedReason,
		meta.HealthCheckFailedReason,
		meta.BuildFailedReason,
		meta.ArtifactFailedReason,
	} {
		t.Run(reason, func(t *testing.T) {
			got := health("Kustomization", "media", "sonarr",
				2, 2, conditions(reason, metav1.ConditionFalse), "")

			if got.Healthy || got.Reconciling {
				t.Fatalf("want %s reported as a failure, got %+v", reason, got)
			}
			if got.Reason == "" {
				t.Fatal("a failure must carry a reason")
			}
		})
	}
}

func TestReadyObjectIsHealthy(t *testing.T) {
	got := health("HelmRelease", "productivity", "vaultwarden",
		3, 3, conditions(meta.ReconciliationSucceededReason, metav1.ConditionTrue), "")

	if !got.Healthy || got.Reconciling {
		t.Fatalf("want healthy and settled, got %+v", got)
	}
}

// A controller that has not yet observed the current generation has not looked
// at the merge at all.
func TestUnobservedGenerationIsReconciling(t *testing.T) {
	got := health("Kustomization", "media", "sonarr",
		4, 3, conditions(meta.ReconciliationFailedReason, metav1.ConditionFalse), "")

	if !got.Reconciling || !got.Healthy {
		t.Fatalf("want reconciling while the generation is unobserved, got %+v", got)
	}
}

// A merge sets the whole tree reconciling, and Flux reports that as Ready
// Unknown. Reading Unknown as a failure blamed the merge for every object that
// happened to be mid-reconcile, whatever the reason string said.
func TestUnknownReadyIsInProgressNotBroken(t *testing.T) {
	for _, reason := range []string{
		"Progressing", "UpgradeStarted", "InstallStarted", "ReconciliationInProgress", "",
	} {
		t.Run("reason="+reason, func(t *testing.T) {
			got := health("HelmRelease", "productivity", "invoiceshelf",
				2, 2, conditions(reason, metav1.ConditionUnknown), "")

			if !got.Reconciling || !got.Healthy {
				t.Fatalf("Unknown must hold the watch open, got %+v", got)
			}
		})
	}
}

// An object whose controller reported nothing has equally not decided.
func TestMissingReadyConditionIsInProgress(t *testing.T) {
	got := health("Kustomization", "media", "sonarr", 1, 1, nil, "")
	if !got.Reconciling || !got.Healthy {
		t.Fatalf("want reconciling with no Ready condition, got %+v", got)
	}
}

// Flux stamps every condition it writes with the generation it judged, while
// status.observedGeneration is advanced by a separate mechanism. A Ready left
// over from the previous generation is a verdict on a spec that is no longer
// live, and must not settle the watch.
func TestReadyLeftOverFromAnEarlierGenerationDoesNotSettleTheWatch(t *testing.T) {
	got := health("Kustomization", "media", "sonarr", 8, 8, []metav1.Condition{
		{
			Type:               meta.ReadyCondition,
			Status:             metav1.ConditionTrue,
			Reason:             meta.ReconciliationSucceededReason,
			ObservedGeneration: 7,
		},
		{
			Type:               meta.ReconcilingCondition,
			Status:             metav1.ConditionTrue,
			Reason:             meta.ProgressingReason,
			ObservedGeneration: 8,
		},
	}, "")

	if !got.Reconciling || !got.Healthy {
		t.Fatalf("want the watch held open until generation 8 is judged, got %+v", got)
	}
}

// kustomize-controller advances status.observedGeneration only once Ready is
// true, so an object that never succeeds at the new generation leaves it at the
// last good one forever while its conditions describe the live failure.
func TestFailureStampedWithTheLiveGenerationIsBelievedThoughStatusLags(t *testing.T) {
	got := health("Kustomization", "media", "sonarr", 8, 7, []metav1.Condition{{
		Type:               meta.ReadyCondition,
		Status:             metav1.ConditionFalse,
		Reason:             meta.BuildFailedReason,
		Message:            "kustomize build failed",
		ObservedGeneration: 8,
	}}, "")

	if got.Healthy || got.Reconciling {
		t.Fatalf("want the generation-8 failure believed, got %+v", got)
	}
	if got.Reason != "kustomize build failed" {
		t.Fatalf("want the controller's own message, got %q", got.Reason)
	}
}

// A Reconciling left behind by an earlier generation is not evidence that the
// current one is still in flight; believing it holds every window open to the
// settle timeout.
func TestReconcilingLeftOverFromAnEarlierGenerationDoesNotHoldTheWatchOpen(t *testing.T) {
	got := health("HelmRelease", "media", "sonarr", 9, 9, []metav1.Condition{
		{
			Type:               meta.ReadyCondition,
			Status:             metav1.ConditionFalse,
			Reason:             meta.ReconciliationFailedReason,
			Message:            "upgrade retries exhausted",
			ObservedGeneration: 9,
		},
		{
			Type:               meta.ReconcilingCondition,
			Status:             metav1.ConditionTrue,
			Reason:             meta.ProgressingReason,
			ObservedGeneration: 8,
		},
	}, "")

	if got.Healthy || got.Reconciling {
		t.Fatalf("want a stale Reconciling ignored, got %+v", got)
	}
}

// Older Flux releases, and any controller that writes conditions without the
// stamp, leave observedGeneration at zero; the object-level field is then the
// only freshness signal there is.
func TestUnstampedConditionsFallBackToTheObjectLevelGeneration(t *testing.T) {
	stale := health("Kustomization", "media", "sonarr", 8, 7,
		conditions(meta.ReconciliationFailedReason, metav1.ConditionFalse), "")
	if !stale.Healthy || !stale.Reconciling {
		t.Fatalf("want unstamped conditions at a lagging generation held open, got %+v", stale)
	}

	current := health("Kustomization", "media", "sonarr", 8, 8,
		conditions(meta.ReconciliationFailedReason, metav1.ConditionFalse), "")
	if current.Healthy || current.Reconciling {
		t.Fatalf("want unstamped conditions at the observed generation believed, got %+v", current)
	}
}

// helm-controller marks Reconciling=ProgressingWithRetry only once an attempt
// at this generation has already failed, and with remediation.retries -1 it
// never stops. Reading that as in-progress leaves a broken release healthy for
// as long as the run lasts.
func TestObjectRetryingAfterAFailedAttemptIsBroken(t *testing.T) {
	got := health("HelmRelease", "media", "sonarr", 5, 4, []metav1.Condition{
		{
			Type:               meta.ReadyCondition,
			Status:             metav1.ConditionFalse,
			Reason:             "UpgradeFailed",
			Message:            "Helm upgrade failed: timed out waiting for the condition",
			ObservedGeneration: 5,
		},
		{
			Type:               meta.ReconcilingCondition,
			Status:             metav1.ConditionTrue,
			Reason:             meta.ProgressingWithRetryReason,
			Message:            "Helm upgrade failed: timed out waiting for the condition",
			ObservedGeneration: 5,
		},
	}, "")

	if got.Healthy || got.Reconciling {
		t.Fatalf("want a retrying failure reported as broken, got %+v", got)
	}
	if got.Reason != "Helm upgrade failed: timed out waiting for the condition" {
		t.Fatalf("want the Helm error as the cause, got %q", got.Reason)
	}
}

// Both controllers mark Ready Unknown while a pass is genuinely working -
// kustomize-controller at the top of every reconcile, helm-controller before
// every release action - so Unknown, not the Reconciling condition, is what
// holds the window open for work in flight.
func TestPassInFlightIsHeldOpenByAnUnknownReady(t *testing.T) {
	for _, kind := range []string{"Kustomization", "HelmRelease"} {
		t.Run(kind, func(t *testing.T) {
			got := health(kind, "media", "sonarr", 5, 4, []metav1.Condition{
				{
					Type:               meta.ReadyCondition,
					Status:             metav1.ConditionUnknown,
					Reason:             meta.ProgressingReason,
					Message:            "Reconciliation in progress",
					ObservedGeneration: 5,
				},
				{
					Type:               meta.ReconcilingCondition,
					Status:             metav1.ConditionTrue,
					Reason:             meta.ProgressingReason,
					ObservedGeneration: 5,
				},
			}, "")

			if !got.Healthy || !got.Reconciling {
				t.Fatalf("want a pass in flight held open, got %+v", got)
			}
		})
	}
}

// helm-controller refuses to overwrite Ready for a non-release action because
// that would lose "more important failure state from an earlier action" - its
// words. So a Ready=False carried through a Progressing pass is a failure the
// controller chose to keep, not an attempt that has yet to be judged. This is
// the remediation half of the remediation.retries -1 loop, where the previous
// fix only caught the sub-second window between reconciles.
func TestFailurePreservedAcrossTheNextActionIsBroken(t *testing.T) {
	got := health("HelmRelease", "media", "sonarr", 5, 4, []metav1.Condition{
		{
			Type:               meta.ReadyCondition,
			Status:             metav1.ConditionFalse,
			Reason:             "UpgradeFailed",
			Message:            "Helm upgrade failed: timed out waiting for the condition",
			ObservedGeneration: 5,
		},
		{
			Type:               meta.ReconcilingCondition,
			Status:             metav1.ConditionTrue,
			Reason:             meta.ProgressingReason,
			Message:            "Running 'rollback' action with timeout of 5m0s",
			ObservedGeneration: 5,
		},
	}, "")

	if got.Healthy || got.Reconciling {
		t.Fatalf("want the preserved failure believed, got %+v", got)
	}
	if got.Reason != "Helm upgrade failed: timed out waiting for the condition" {
		t.Fatalf("want the Helm error as the cause, got %q", got.Reason)
	}
}

// helm-controller runs its pre-flight checks before the release action loop is
// entered, so these reasons mean the release has not been attempted at all -
// not that it failed. Every chart-version bump passes through SourceNotReady
// while source-controller fetches the new chart, which is the most common thing
// Renovate does to this repository.
func TestHelmReleasePreflightCheckIsHeldOpen(t *testing.T) {
	for _, ready := range []metav1.Condition{
		{
			Reason:  "SourceNotReady",
			Message: "HelmChart 'media/sonarr' is not ready: latest generation of object has not been reconciled",
		},
		{
			Reason:  meta.ArtifactFailedReason,
			Message: "could not get Source object: helmcharts.source.toolkit.fluxcd.io \"media-sonarr\" not found",
		},
		{
			Reason:  meta.ArtifactFailedReason,
			Message: "Source not ready: artifact not found. Retrying in 30s",
		},
	} {
		t.Run(ready.Reason+"/"+ready.Message[:12], func(t *testing.T) {
			ready.Type, ready.Status, ready.ObservedGeneration = meta.ReadyCondition, metav1.ConditionFalse, 5
			got := health("HelmRelease", "media", "sonarr", 5, 4, []metav1.Condition{
				ready,
				{
					Type:               meta.ReconcilingCondition,
					Status:             metav1.ConditionTrue,
					Reason:             meta.ProgressingReason,
					Message:            "Fulfilling prerequisites",
					ObservedGeneration: 5,
				},
			}, "")

			if !got.Healthy || !got.Reconciling {
				t.Fatalf("want a release not yet attempted held open, got %+v", got)
			}
		})
	}
}

// kustomize-controller spends the same ArtifactFailed reason on a path that is
// missing from the built manifests, which no amount of waiting fixes. Only
// helm-controller's pre-flight use of it means "not tried yet".
func TestKustomizationWithAMissingPathIsStillBroken(t *testing.T) {
	got := health("Kustomization", "media", "sonarr", 5, 4, []metav1.Condition{
		{
			Type:               meta.ReadyCondition,
			Status:             metav1.ConditionFalse,
			Reason:             meta.ArtifactFailedReason,
			Message:            "kustomization path not found: stat /tmp/x/apps/sonarr: no such file or directory",
			ObservedGeneration: 5,
		},
		{
			Type:               meta.ReconcilingCondition,
			Status:             metav1.ConditionTrue,
			Reason:             meta.ProgressingWithRetryReason,
			ObservedGeneration: 5,
		},
	}, "")

	if got.Healthy || got.Reconciling {
		t.Fatalf("want a missing path reported as broken, got %+v", got)
	}
}

// A failure the controller has not yet re-judged at the live generation is not
// this merge's failure, whatever the Reconciling condition says. This is what
// keeps the rule above from condemning an object the moment a new generation
// lands on one that was already failing.
func TestFailureFromThepreviousGenerationIsNotJudgedYet(t *testing.T) {
	got := health("HelmRelease", "media", "sonarr", 6, 5, []metav1.Condition{
		{
			Type:               meta.ReadyCondition,
			Status:             metav1.ConditionFalse,
			Reason:             "UpgradeFailed",
			Message:            "Helm upgrade failed: timed out waiting for the condition",
			ObservedGeneration: 5,
		},
		{
			Type:               meta.ReconcilingCondition,
			Status:             metav1.ConditionTrue,
			Reason:             meta.ProgressingReason,
			Message:            "Running 'unlock' action with timeout of 5m0s",
			ObservedGeneration: 6,
		},
	}, "")

	if !got.Healthy || !got.Reconciling {
		t.Fatalf("want an unjudged generation held open, got %+v", got)
	}
}

// Stalled is Flux reporting that it has stopped retrying. Nothing will move
// again without a human, so a window that keeps waiting on it can only end
// stalled with no cause - including when the last thing written to Ready was an
// Unknown from the action that then gave up.
func TestStalledObjectIsBrokenWhateverReadySays(t *testing.T) {
	for _, ready := range []metav1.ConditionStatus{metav1.ConditionFalse, metav1.ConditionUnknown} {
		t.Run(string(ready), func(t *testing.T) {
			got := health("HelmRelease", "media", "sonarr", 5, 5, []metav1.Condition{
				{
					Type:               meta.ReadyCondition,
					Status:             ready,
					Reason:             meta.ProgressingReason,
					Message:            "Running 'upgrade' action with timeout of 5m0s",
					ObservedGeneration: 5,
				},
				{
					Type:               meta.StalledCondition,
					Status:             metav1.ConditionTrue,
					Reason:             "RetriesExceeded",
					Message:            "Failed to upgrade after 3 attempt(s)",
					ObservedGeneration: 5,
				},
			}, "")

			if got.Healthy || got.Reconciling {
				t.Fatalf("want a stalled object reported as broken, got %+v", got)
			}
			if got.Reason != "Failed to upgrade after 3 attempt(s)" {
				t.Fatalf("want the stall as the cause, got %q", got.Reason)
			}
		})
	}
}

// A generation that only edits spec.interval or spec.timeout leaves the release
// digest alone, so an already-exhausted HelmRelease stalls again immediately -
// ErrExceededMaxRetries is terminal, which advances status.observedGeneration
// while the failure in Ready is never rewritten. Nothing will requeue, so
// waiting for Ready to catch up waits forever.
func TestStallAtTheLiveGenerationOutranksAnUnjudgedReady(t *testing.T) {
	got := health("HelmRelease", "media", "sonarr", 6, 6, []metav1.Condition{
		{
			Type:               meta.ReadyCondition,
			Status:             metav1.ConditionFalse,
			Reason:             "UpgradeFailed",
			Message:            "Helm upgrade failed: timed out",
			ObservedGeneration: 5,
		},
		{
			Type:               meta.StalledCondition,
			Status:             metav1.ConditionTrue,
			Reason:             "RetriesExceeded",
			Message:            "Failed to upgrade after 3 attempt(s)",
			ObservedGeneration: 6,
		},
	}, "")

	if got.Healthy || got.Reconciling {
		t.Fatalf("want the stall believed over an unjudged Ready, got %+v", got)
	}
	if got.Reason != "Failed to upgrade after 3 attempt(s)" {
		t.Fatalf("want the stall as the cause, got %q", got.Reason)
	}
}

// Neither controller deletes Stalled when a new generation supersedes the spec
// that caused it, so a Stalled left over from generation 4 must not condemn a
// merge that fixed it.
func TestStalledLeftOverFromAnEarlierGenerationDoesNotCondemnTheObject(t *testing.T) {
	got := health("HelmRelease", "media", "sonarr", 5, 4, []metav1.Condition{
		{
			Type:               meta.ReadyCondition,
			Status:             metav1.ConditionUnknown,
			Reason:             meta.ProgressingReason,
			ObservedGeneration: 5,
		},
		{
			Type:               meta.StalledCondition,
			Status:             metav1.ConditionTrue,
			Reason:             "RetriesExceeded",
			Message:            "Failed to upgrade after 3 attempt(s)",
			ObservedGeneration: 4,
		},
	}, "")

	if !got.Healthy || !got.Reconciling {
		t.Fatalf("want a superseded stall ignored, got %+v", got)
	}
}

// Ready false with a failure reason is still a failure.
func TestFalseReadyIsStillBroken(t *testing.T) {
	got := health("HelmRelease", "media", "sonarr",
		2, 2, conditions(meta.ReconciliationFailedReason, metav1.ConditionFalse), "")
	if got.Healthy || got.Reconciling {
		t.Fatalf("want a failure, got %+v", got)
	}
}

// helm-controller marks Ready=False/DependencyNotReady before the release is
// attempted and withholds the observed-generation bump on that path, so the
// stamp on Ready is live while status.observedGeneration lags - the one shape
// where the freshness gate looks like it should rescue the object and does not.
// It must still be held open: a dependsOn chain reports this on the ordinary
// path of every merge, so condemning it discards good merges wholesale. The
// waiting is bounded by Reconciling, not by the reason.
func TestAReleaseWaitingOnADependencyIsHeldOpenAtTheLiveGeneration(t *testing.T) {
	got := health(helmReleaseKind, "media", "sonarr", 7, 6, []metav1.Condition{
		{
			Type:               meta.ReadyCondition,
			Status:             metav1.ConditionFalse,
			Reason:             meta.DependencyNotReadyReason,
			Message:            "dependency 'media/prowlarr' is not ready",
			ObservedGeneration: 7,
		},
		{
			Type:               meta.ReconcilingCondition,
			Status:             metav1.ConditionTrue,
			Reason:             meta.ProgressingReason,
			Message:            "Fulfilling prerequisites",
			ObservedGeneration: 7,
		},
	}, "")

	if !got.Healthy {
		t.Fatalf("want a release that has not been attempted held open, got %+v", got)
	}
	if !got.Reconciling {
		t.Fatalf("want the wait visible so no window can settle behind it, got %+v", got)
	}
	if got.Reason != "dependency 'media/prowlarr' is not ready" {
		t.Fatalf("want the dependency named as the cause, got %q", got.Reason)
	}
}

// Holding a not-Ready object open reads Healthy, which is only safe because it
// reads Reconciling in the same breath: a reconciling object never lets a window
// settle, so the merge behind it is stalled out and reverted rather than passed.
// helm-controller spends ArtifactFailed on a genuinely corrupt chart with the
// same reason it uses for a release it has not attempted yet, and the messages
// are not a safe discriminator, so that case is bounded by this invariant rather
// than by the reason list. Healthy without Reconciling is the one reading that
// would let it through.
func TestAFailedReadyNeverReadsHealthyAndSettled(t *testing.T) {
	for _, item := range []struct {
		kind     string
		reason   string
		message  string
		heldOpen bool
	}{
		{helmReleaseKind, meta.ArtifactFailedReason, "Could not load chart: integrity failure: computed checksum does not match", true},
		{helmReleaseKind, meta.ArtifactFailedReason, "Could not load chart: Chart.yaml file is missing", true},
		{helmReleaseKind, meta.ArtifactFailedReason, "Source not ready: artifact not found. Retrying in 30s", true},
		{helmReleaseKind, sourceNotReadyReason, "HelmChart 'media/sonarr' is not ready", true},
		{helmReleaseKind, meta.DependencyNotReadyReason, "dependency 'media/prowlarr' is not ready", true},
		{helmReleaseKind, "UpgradeFailed", "Helm upgrade failed: timed out", false},
		{kustomizationKind, meta.ArtifactFailedReason, "kustomization path not found", false},
		{kustomizationKind, meta.ProgressingWithRetryReason, "reconciliation in progress", true},
	} {
		t.Run(item.kind+"/"+item.reason+"/"+item.message[:12], func(t *testing.T) {
			got := health(item.kind, "media", "sonarr", 5, 4, []metav1.Condition{{
				Type:               meta.ReadyCondition,
				Status:             metav1.ConditionFalse,
				Reason:             item.reason,
				Message:            item.message,
				ObservedGeneration: 5,
			}}, "")

			if got.Healthy != got.Reconciling {
				t.Fatalf("a judged not-Ready object must be held open or broken, never healthy and settled: %+v", got)
			}
			if got.Reconciling != item.heldOpen {
				t.Fatalf("want reconciling=%t for %s/%s, got %+v", item.heldOpen, item.kind, item.reason, got)
			}
			if got.Reason != item.message {
				t.Fatalf("want the controller's own message carried through, got %q", got.Reason)
			}
		})
	}
}

func fluxObject(apiVersion, kind, namespace, name string, generation int64, ready metav1.Condition) *unstructured.Unstructured {
	ready.Type = meta.ReadyCondition
	ready.Status = metav1.ConditionFalse
	ready.ObservedGeneration = generation
	ready.LastTransitionTime = metav1.Now()
	condition, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&ready)
	if err != nil {
		panic(err)
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata": map[string]any{
			"namespace":  namespace,
			"name":       name,
			"generation": generation,
		},
		"spec": map[string]any{},
		"status": map[string]any{
			"observedGeneration": generation - 1,
			"conditions":         []any{condition},
		},
	}}
}

func fakeClient(objects ...*unstructured.Unstructured) *Client {
	listKinds := map[schema.GroupVersionResource]string{
		gitRepositories: "GitRepositoryList",
		kustomizations:  "KustomizationList",
		helmReleases:    "HelmReleaseList",
	}
	items := make([]runtime.Object, 0, len(objects))
	for _, object := range objects {
		items = append(items, object)
	}
	return &Client{
		client: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, items...),
		source: domain.ObjectRef{Kind: "GitRepository", Namespace: "flux-system", Name: "flux-system"},
	}
}

// The kind a read stamps onto an ObjectRef is the kind the in-progress rule is
// asked about, and the kind a later re-read is looked up by. Nothing in the
// compiler ties those three spellings together, so they are exercised through
// the real read path rather than by handing health() a literal: a divergence
// silently disables the pre-flight rescue that keeps every HelmRelease chart
// bump from being condemned, and every object-level test still passes.
func TestTheKindAReadReportsIsTheKindTheProgressRuleMatches(t *testing.T) {
	client := fakeClient(
		fluxObject("helm.toolkit.fluxcd.io/v2", "HelmRelease", "media", "sonarr", 5, metav1.Condition{
			Reason:  sourceNotReadyReason,
			Message: "HelmChart 'media/sonarr' is not ready",
		}),
		fluxObject("kustomize.toolkit.fluxcd.io/v1", "Kustomization", "media", "apps", 5, metav1.Condition{
			Reason:  meta.ArtifactFailedReason,
			Message: "kustomization path not found: stat /tmp/x/apps: no such file or directory",
		}),
	)

	objects, err := client.Objects(context.Background())
	if err != nil {
		t.Fatalf("read objects: %v", err)
	}
	byRef := map[string]domain.ObjectHealth{}
	for _, object := range objects {
		byRef[object.Ref.String()] = object
	}
	if len(byRef) != 2 {
		t.Fatalf("want both objects read, got %+v", objects)
	}

	release, ok := byRef["media/HelmRelease/sonarr"]
	if !ok {
		t.Fatalf("want the HelmRelease reported under its own kind, got %+v", objects)
	}
	if !release.Healthy || !release.Reconciling {
		t.Fatalf("want a release not yet attempted held open, got %+v", release)
	}
	kustomization, ok := byRef["media/Kustomization/apps"]
	if !ok {
		t.Fatalf("want the Kustomization reported under its own kind, got %+v", objects)
	}
	if kustomization.Healthy || kustomization.Reconciling {
		t.Fatalf("want a missing path reported as broken, got %+v", kustomization)
	}

	for _, want := range []domain.ObjectHealth{release, kustomization} {
		got, err := client.Object(context.Background(), want.Ref.Kind, want.Ref.Namespace, want.Ref.Name)
		if err != nil {
			t.Fatalf("re-read %s: %v", want.Ref, err)
		}
		if got != want {
			t.Fatalf("want a re-read of %s to agree with the listing: %+v vs %+v", want.Ref, got, want)
		}
	}
}

// Other tests name both sides through one constant, so a module-side rename stays green.
func TestTheWireStringsFluxServesAreStillTheOnesOpsPilotMatches(t *testing.T) {
	for _, item := range []struct {
		source string
		read   string
		wire   string
	}{
		{"meta.ReadyCondition", meta.ReadyCondition, "Ready"},
		{"meta.StalledCondition", meta.StalledCondition, "Stalled"},
		{"meta.ArtifactFailedReason", meta.ArtifactFailedReason, "ArtifactFailed"},
		{"meta.DependencyNotReadyReason", meta.DependencyNotReadyReason, "DependencyNotReady"},
		{"meta.ProgressingReason", meta.ProgressingReason, "Progressing"},
		{"meta.ProgressingWithRetryReason", meta.ProgressingWithRetryReason, "ProgressingWithRetry"},
		{"helmv2.ArtifactFailedReason", helmv2.ArtifactFailedReason, "ArtifactFailed"},
		{"helmv2.DependencyNotReadyReason", helmv2.DependencyNotReadyReason, "DependencyNotReady"},
		{"sourceNotReadyReason", sourceNotReadyReason, "SourceNotReady"},
		{"kustomizationKind", kustomizationKind, "Kustomization"},
		{"helmReleaseKind", helmReleaseKind, "HelmRelease"},
	} {
		t.Run(item.source, func(t *testing.T) {
			if item.read != item.wire {
				t.Fatalf("Flux serves %q, but the pinned API module now spells %s as %q: re-check the controller behaviour every verdict here rests on before moving this line",
					item.wire, item.source, item.read)
			}
		})
	}
}
