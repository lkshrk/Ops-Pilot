package flux

import (
	"strings"
	"testing"

	"github.com/fluxcd/pkg/apis/meta"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const wantSHA = "9f2c1b7e4a8d0c3f6b5e2a1d7c4f8b0e3a6d9c25"

func gitRepository(generation, observed int64, revision string, conditions ...metav1.Condition) *sourcev1.GitRepository {
	item := &sourcev1.GitRepository{}
	item.Generation = generation
	item.Status.ObservedGeneration = observed
	item.Status.Conditions = conditions
	if revision != "" {
		item.Status.Artifact = &meta.Artifact{Revision: revision}
	}
	return item
}

// source-controller deletes Stalled on every retryable error, so it is only set
// for a misconfiguration no amount of waiting resolves. Reporting that as "no
// revision yet" spends the whole settle timeout and then hands the agent a
// stalled window with no cause, which is how a good merge gets reverted.
func TestStalledSourceReportsWhyItStoppedFetching(t *testing.T) {
	_, err := sourceRevision(gitRepository(3, 3, "", metav1.Condition{
		Type:               meta.StalledCondition,
		Status:             metav1.ConditionTrue,
		Reason:             "URLInvalid",
		Message:            "first path segment in URL cannot contain colon",
		ObservedGeneration: 3,
	}))

	if err == nil {
		t.Fatal("want the source's own failure surfaced")
	}
	if got := err.Error(); got != "Flux source has stopped retrying: first path segment in URL cannot contain colon" {
		t.Fatalf("want the stall message, got %q", got)
	}
}

// A stall never clears the artifact, so the commit on disk can be the merge
// itself: the merge edits the GitRepository's own spec, source-controller fetches
// it under the old spec, then stalls on the new one. Reading the artifact first
// would call that fetched and let the window pass, but the GitRepository is not
// among the objects health is read from, so nothing else would notice that the
// source can no longer fetch anything - including the revert. The stall has to
// outrank the artifact however current the artifact looks.
func TestALiveStallHaltsEvenWhenTheArtifactAlreadyCarriesTheMerge(t *testing.T) {
	revision, err := sourceRevision(gitRepository(5, 5, "main@sha1:"+wantSHA, metav1.Condition{
		Type:               meta.StalledCondition,
		Status:             metav1.ConditionTrue,
		Reason:             "URLInvalid",
		Message:            "failed to parse url 'ssh//git@example.com/x': parse error",
		ObservedGeneration: 5,
	}))

	if err == nil {
		t.Fatalf("want a live stall to halt the run, got revision %q", revision)
	}
	if got := err.Error(); got != "Flux source has stopped retrying: failed to parse url 'ssh//git@example.com/x': parse error" {
		t.Fatalf("want the stall's own cause, got %q", got)
	}
	if revision != "" {
		t.Fatalf("want no revision alongside the stall, got %q", revision)
	}
}

// A Stalled left over from the spec that caused it must not condemn the merge
// that fixed it; source-controller only clears the condition once it reconciles
// again.
func TestStalledSourceFromASupersededSpecIsIgnored(t *testing.T) {
	revision, err := sourceRevision(gitRepository(4, 3, "main@sha1:"+wantSHA, metav1.Condition{
		Type:               meta.StalledCondition,
		Status:             metav1.ConditionTrue,
		Reason:             "URLInvalid",
		Message:            "first path segment in URL cannot contain colon",
		ObservedGeneration: 3,
	}))

	if err != nil {
		t.Fatalf("want a superseded stall ignored, got %v", err)
	}
	if revision != wantSHA {
		t.Fatalf("want the artifact revision, got %q", revision)
	}
}

// The artifact revision is the commit on disk that every Kustomization is
// applying, so it answers "has the merge reached the cluster" whatever the
// source's summary condition says. Withholding it while the source is not Ready
// would stall out a merge Flux had in fact applied.
func TestArtifactCountsEvenWhileTheSourceIsNotReady(t *testing.T) {
	revision, err := sourceRevision(gitRepository(4, 3, "main@sha1:"+strings.ToUpper(wantSHA), metav1.Condition{
		Type:               meta.ReadyCondition,
		Status:             metav1.ConditionFalse,
		Reason:             sourcev1.IncludeUnavailableCondition,
		Message:            "could not fetch include",
		ObservedGeneration: 4,
	}))

	if err != nil {
		t.Fatalf("want the artifact reported, got %v", err)
	}
	if revision != wantSHA {
		t.Fatalf("want the normalized artifact revision, got %q", revision)
	}
}

func TestSourceWithoutAnArtifactReportsNoRevision(t *testing.T) {
	revision, err := sourceRevision(gitRepository(1, 0, "", metav1.Condition{
		Type:               meta.ReconcilingCondition,
		Status:             metav1.ConditionTrue,
		Reason:             meta.ProgressingReason,
		ObservedGeneration: 1,
	}))

	if err != nil {
		t.Fatalf("want a fetch in progress to be no revision, got %v", err)
	}
	if revision != "" {
		t.Fatalf("want no revision, got %q", revision)
	}
}
