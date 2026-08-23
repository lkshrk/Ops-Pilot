package flux

import (
	"context"
	"fmt"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	"github.com/lkshrk/ops-pilot/internal/domain"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	reconcileAnnotation = "reconcile.fluxcd.io/requestedAt"
	// helm-controller writes this reason as a bare string, with no constant.
	sourceNotReadyReason = "SourceNotReady"
	// inProgress reads these back, so a read must stamp the same spelling it matches.
	kustomizationKind = "Kustomization"
	helmReleaseKind   = "HelmRelease"
)

// SourceRevision returns the git revision of the artifact the configured
// GitRepository has currently fetched.
func (c *Client) SourceRevision(ctx context.Context) (string, error) {
	item, err := c.get(ctx, gitRepositories, c.source.Namespace, c.source.Name)
	if err != nil {
		return "", fmt.Errorf("read Flux source: %w", err)
	}
	source, err := decodeGitRepository(item)
	if err != nil {
		return "", fmt.Errorf("decode Flux source: %w", err)
	}
	revision, err := sourceRevision(source)
	if err != nil {
		return "", fmt.Errorf("Flux source %s/%s: %w", c.source.Namespace, c.source.Name, err)
	}
	return revision, nil
}

// Reconcile asks Flux to fetch the source now rather than at its next interval.
func (c *Client) Reconcile(ctx context.Context, at time.Time) error {
	patch := fmt.Sprintf(
		`{"metadata":{"annotations":{%q:%q}}}`,
		reconcileAnnotation, at.UTC().Format(time.RFC3339Nano),
	)
	_, err := c.client.Resource(gitRepositories).
		Namespace(c.source.Namespace).
		Patch(ctx, c.source.Name, types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	if err != nil {
		return fmt.Errorf("trigger Flux reconciliation: %w", err)
	}
	return nil
}

// Objects returns the health of every Flux Kustomization and HelmRelease.
func (c *Client) Objects(ctx context.Context) ([]domain.ObjectHealth, error) {
	kustomizations, err := c.list(ctx, kustomizations)
	if err != nil {
		return nil, fmt.Errorf("list Flux Kustomizations: %w", err)
	}
	releases, err := c.list(ctx, helmReleases)
	if err != nil {
		return nil, fmt.Errorf("list Flux HelmReleases: %w", err)
	}
	result := make([]domain.ObjectHealth, 0, len(kustomizations.Items)+len(releases.Items))
	for i := range kustomizations.Items {
		item, err := decodeKustomization(&kustomizations.Items[i])
		if err != nil {
			return nil, fmt.Errorf("decode Flux Kustomization: %w", err)
		}
		result = append(result, health(
			kustomizationKind, item.Namespace, item.Name,
			item.Generation, item.Status.ObservedGeneration,
			item.Status.Conditions, item.Status.LastAppliedRevision,
		))
	}
	for i := range releases.Items {
		item, err := decodeHelmRelease(&releases.Items[i])
		if err != nil {
			return nil, fmt.Errorf("decode Flux HelmRelease: %w", err)
		}
		result = append(result, health(
			helmReleaseKind, item.Namespace, item.Name,
			item.Generation, item.Status.ObservedGeneration,
			item.Status.Conditions, "",
		))
	}
	return result, nil
}

// Object returns the health of a single Flux object.
func (c *Client) Object(ctx context.Context, kind, namespace, name string) (domain.ObjectHealth, error) {
	switch kind {
	case kustomizationKind:
		item, err := c.get(ctx, kustomizations, namespace, name)
		if err != nil {
			return domain.ObjectHealth{}, err
		}
		decoded, err := decodeKustomization(item)
		if err != nil {
			return domain.ObjectHealth{}, err
		}
		return health(kind, namespace, name,
			decoded.Generation, decoded.Status.ObservedGeneration,
			decoded.Status.Conditions, decoded.Status.LastAppliedRevision), nil
	case helmReleaseKind:
		item, err := c.get(ctx, helmReleases, namespace, name)
		if err != nil {
			return domain.ObjectHealth{}, err
		}
		decoded, err := decodeHelmRelease(item)
		if err != nil {
			return domain.ObjectHealth{}, err
		}
		return health(kind, namespace, name,
			decoded.Generation, decoded.Status.ObservedGeneration,
			decoded.Status.Conditions, ""), nil
	default:
		return domain.ObjectHealth{}, fmt.Errorf("unsupported Flux kind %q", kind)
	}
}

// health reads one Flux object's conditions.
//
// The Ready condition's status carries the answer directly: Unknown means the
// controller has not determined an outcome yet, which is the definition of
// in-progress and not a failure. Reading it as one blamed whichever merge was in
// flight for every object that happened to be reconciling, and a merge is
// precisely what sets the whole tree reconciling.
func health(
	kind, namespace, name string,
	generation, observed int64,
	conditions []metav1.Condition,
	revision string,
) domain.ObjectHealth {
	result := domain.ObjectHealth{
		Ref:      domain.ObjectRef{Kind: kind, Namespace: namespace, Name: name},
		Revision: normalizeRevision(revision),
	}
	ready, judged := currentCondition(conditions, meta.ReadyCondition, generation, observed)
	if judged && ready.Status == metav1.ConditionTrue {
		result.Healthy = true
		return result
	}
	// Stalled is Flux reporting that it has stopped retrying, so nothing moves
	// again without a human and waiting only costs the run its cause. It is read
	// before the freshness gate because the pass that gives up need not rewrite
	// Ready at all, and no later pass will.
	if stalled, ok := currentCondition(conditions, meta.StalledCondition, generation, observed); ok &&
		stalled.Status == metav1.ConditionTrue {
		result.Reason = message(stalled)
		return result
	}
	if !judged {
		result.Healthy, result.Reconciling = true, true
		result.Reason = "generation not yet observed"
		return result
	}
	if ready.Status == metav1.ConditionUnknown {
		result.Healthy, result.Reconciling = true, true
		result.Reason = "reconciliation in progress"
		return result
	}
	// Ready is false and stamped with the live generation, so the controller has
	// judged this spec and rejected it. A pass that is genuinely working marks
	// Ready Unknown, so the Reconciling condition cannot rescue a False; only
	// the Ready reason itself can, because Flux still distinguishes an object
	// waiting its turn from one that failed and a dependsOn chain means most of
	// the tree waits.
	if inProgress(kind, ready.Reason) {
		result.Healthy, result.Reconciling = true, true
	}
	result.Reason = message(ready)
	if result.Reason == "" {
		result.Reason = "not ready"
	}
	return result
}

// currentCondition returns the named condition when it is a verdict on the live
// spec rather than a leftover from an earlier one.
//
// Flux stamps every condition it writes with the generation it judged, but
// status.observedGeneration is advanced by a separate mechanism that can move in
// a pass where a condition was never re-evaluated - and, on a Kustomization that
// never reaches Ready, never moves at all. Only the per-condition stamp says
// which spec a verdict is about; the object-level field is the fallback for
// controllers that leave the stamp empty.
func currentCondition(
	conditions []metav1.Condition,
	want string,
	generation, observed int64,
) (metav1.Condition, bool) {
	for _, item := range conditions {
		if item.Type != want {
			continue
		}
		switch {
		case generation <= 0:
			return item, true
		case item.ObservedGeneration > 0:
			return item, item.ObservedGeneration >= generation
		default:
			return item, generation == observed
		}
	}
	return metav1.Condition{}, false
}

func message(item metav1.Condition) string {
	if item.Message != "" {
		return item.Message
	}
	return item.Reason
}

// inProgress reports whether a not-Ready reason means the object has not
// reached its turn yet rather than that it broke. Fetching a commit reconciles
// the whole tree, so every Kustomization behind a dependsOn chain reports
// Ready=False for a while.
//
// helm-controller writes the last two from pre-flight checks that run before the
// release is attempted at all, so on a HelmRelease they mean untried: a chart
// bump reports SourceNotReady for as long as source-controller takes to fetch
// the new chart. kustomize-controller spends the same ArtifactFailed reason on a
// path missing from the built manifests, which no waiting fixes, hence the kind.
func inProgress(kind, reason string) bool {
	switch reason {
	case meta.ProgressingReason, meta.ProgressingWithRetryReason, meta.DependencyNotReadyReason:
		return true
	case sourceNotReadyReason, meta.ArtifactFailedReason:
		return kind == helmReleaseKind
	default:
		return false
	}
}
