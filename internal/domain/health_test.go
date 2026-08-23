package domain_test

import (
	"strings"
	"testing"

	"github.com/lkshrk/ops-pilot/internal/domain"
)

func snapshot(objects ...domain.ObjectHealth) domain.HealthSnapshot {
	result := domain.HealthSnapshot{Objects: map[string]domain.ObjectHealth{}}
	for _, object := range objects {
		result.Objects[object.Ref.String()] = object
	}
	return result
}

func object(name string, healthy bool) domain.ObjectHealth {
	return domain.ObjectHealth{
		Ref:     domain.ObjectRef{Kind: "HelmRelease", Namespace: "media", Name: name},
		Healthy: healthy,
	}
}

func reconcilingObject(name string) domain.ObjectHealth {
	return domain.ObjectHealth{
		Ref:         domain.ObjectRef{Kind: "HelmRelease", Namespace: "media", Name: name},
		Healthy:     true,
		Reconciling: true,
	}
}

func TestNewFailuresIgnoresPreExistingBreakage(t *testing.T) {
	baseline := snapshot(object("sonarr", true), object("nfs", false))
	current := snapshot(object("sonarr", true), object("nfs", false))

	if failures := current.NewFailures(baseline); len(failures) != 0 {
		t.Fatalf("want no new failures, got %v", failures)
	}
}

func TestNewFailuresReportsNewlyBrokenObject(t *testing.T) {
	baseline := snapshot(object("sonarr", true), object("nfs", false))
	current := snapshot(object("sonarr", false), object("nfs", false))

	failures := current.NewFailures(baseline)
	if len(failures) != 1 || failures[0].Ref.Name != "sonarr" {
		t.Fatalf("want sonarr as the only new failure, got %v", failures)
	}
}

// A merge that deletes or renames a tracked object destroys a running
// application, so its disappearance has to count as a new failure.
func TestNewFailuresReportsHealthyObjectThatDisappeared(t *testing.T) {
	baseline := snapshot(object("sonarr", true), object("nfs", false))
	current := snapshot(object("nfs", false))

	failures := current.NewFailures(baseline)
	if len(failures) != 1 || failures[0].Ref.Name != "sonarr" {
		t.Fatalf("want sonarr as the only new failure, got %v", failures)
	}
	if failures[0].Healthy {
		t.Fatal("want the vanished object reported as unhealthy")
	}
	if !strings.Contains(failures[0].Reason, "vanished") {
		t.Fatalf("want a reason naming the disappearance, got %q", failures[0].Reason)
	}
}

// An object that was already broken before the merge tells the operator nothing
// new by disappearing.
func TestNewFailuresIgnoresUnhealthyObjectThatDisappeared(t *testing.T) {
	if failures := snapshot().NewFailures(snapshot(object("nfs", false))); len(failures) != 0 {
		t.Fatalf("want no new failures, got %v", failures)
	}
}

func TestSettledSinceWaitsForEveryObjectTheMergeSetInMotionToStop(t *testing.T) {
	baseline := snapshot(object("sonarr", true), object("gatus", true))

	if snapshot(reconcilingObject("sonarr"), object("gatus", true)).SettledSince(baseline) {
		t.Fatal("want unsettled while an object the merge touched is still reconciling")
	}
	if !snapshot(object("sonarr", true), object("gatus", true)).SettledSince(baseline) {
		t.Fatal("want settled when nothing is reconciling")
	}
}

// A DaemonSet short a node, a StatefulSet held on an ordered rollout or by a
// partition, a Deployment with its progress deadline disabled, a repository on
// its own interval: each reconciles forever and each was doing so before the
// merge, so none of them may hold this window open.
func TestSettledSinceIgnoresAnObjectAlreadyReconcilingAtBaseline(t *testing.T) {
	baseline := snapshot(reconcilingObject("node-exporter"), object("sonarr", true))
	current := snapshot(reconcilingObject("node-exporter"), object("sonarr", true))

	if !current.SettledSince(baseline) {
		t.Fatal("want settled: the churn predates the merge")
	}
}

// A merge that creates an object owns it, so the window waits for it to converge.
func TestSettledSinceWaitsForAnObjectAbsentFromTheBaseline(t *testing.T) {
	if snapshot(reconcilingObject("gatus")).SettledSince(snapshot()) {
		t.Fatal("want unsettled while an object the merge created is still reconciling")
	}
}

// Breakage that predates the merge is invisible to NewFailures by construction;
// it must be invisible here too, or it denies the pass path from the other side.
func TestSettledSinceIgnoresAnObjectAlreadyBrokenAtBaseline(t *testing.T) {
	if !snapshot(reconcilingObject("nfs")).SettledSince(snapshot(object("nfs", false))) {
		t.Fatal("want settled: the object was already broken before the merge")
	}
}

// A reconciling object reads as healthy so the watch holds open for it, which
// meant an object already mid-flight at baseline could be blamed for failing
// afterwards. Its state predates the merge either way.
func TestNewFailuresIgnoresObjectAlreadyReconcilingAtBaseline(t *testing.T) {
	reconciling := domain.ObjectHealth{
		Ref:         domain.ObjectRef{Kind: "HelmRelease", Namespace: "media", Name: "sonarr"},
		Healthy:     true,
		Reconciling: true,
	}
	baseline := snapshot(reconciling)
	current := snapshot(object("sonarr", false))

	if failures := current.NewFailures(baseline); len(failures) != 0 {
		t.Fatalf("want no attribution for an object already in flight, got %v", failures)
	}
}

// The same object disappearing tells the operator nothing about this merge.
func TestNewFailuresIgnoresReconcilingObjectThatDisappeared(t *testing.T) {
	reconciling := domain.ObjectHealth{
		Ref:         domain.ObjectRef{Kind: "HelmRelease", Namespace: "media", Name: "sonarr"},
		Healthy:     true,
		Reconciling: true,
	}
	if failures := snapshot().NewFailures(snapshot(reconciling)); len(failures) != 0 {
		t.Fatalf("want no attribution, got %v", failures)
	}
}

// A merge that creates a broken object is squarely responsible for it.
func TestNewFailuresAttributesAnObjectAbsentFromTheBaseline(t *testing.T) {
	failures := snapshot(object("gatus", false)).NewFailures(snapshot())

	if len(failures) != 1 || failures[0].Ref.Name != "gatus" {
		t.Fatalf("want the new object attributed, got %v", failures)
	}
}
