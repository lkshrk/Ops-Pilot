package cluster_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/lkshrk/ops-pilot/internal/cluster"
	"github.com/lkshrk/ops-pilot/internal/domain"
)

// scriptedFlux returns one state per poll, so a test can describe how the
// cluster evolves without any real waiting.
type scriptedFlux struct {
	states    [][]domain.ObjectHealth
	revisions []string
	polls     int
	Triggers  int
}

func (f *scriptedFlux) Objects(context.Context) ([]domain.ObjectHealth, error) {
	state := f.states[min(f.polls, len(f.states)-1)]
	return state, nil
}

func (f *scriptedFlux) Object(_ context.Context, kind, namespace, name string) (domain.ObjectHealth, error) {
	return domain.ObjectHealth{Ref: domain.ObjectRef{Kind: kind, Namespace: namespace, Name: name}}, nil
}

func (f *scriptedFlux) SourceRevision(context.Context) (string, error) {
	if len(f.revisions) == 0 {
		return "", nil
	}
	return f.revisions[min(f.polls, len(f.revisions)-1)], nil
}

func (f *scriptedFlux) Reconcile(context.Context, time.Time) error {
	f.Triggers++
	return nil
}

// unreadableFlux is a cluster that cannot be reached at all.
type unreadableFlux struct{}

var errUnreachable = errors.New("connection refused")

func (unreadableFlux) Objects(context.Context) ([]domain.ObjectHealth, error) {
	return nil, errUnreachable
}

func (unreadableFlux) Object(context.Context, string, string, string) (domain.ObjectHealth, error) {
	return domain.ObjectHealth{}, errUnreachable
}

func (unreadableFlux) SourceRevision(context.Context) (string, error) { return "", errUnreachable }

func (unreadableFlux) Reconcile(context.Context, time.Time) error { return errUnreachable }

type noWorkloads struct{}

func (noWorkloads) Objects(context.Context) ([]domain.ObjectHealth, error) { return nil, nil }
func (noWorkloads) Pods(context.Context, string) (string, error)           { return "", nil }
func (noWorkloads) Events(context.Context, string, string) (string, error) { return "", nil }
func (noWorkloads) Logs(context.Context, string, string, string, int64) (string, error) {
	return "", nil
}

func object(name string, healthy, reconciling bool) domain.ObjectHealth {
	return domain.ObjectHealth{
		Ref:         domain.ObjectRef{Kind: "HelmRelease", Namespace: "media", Name: name},
		Healthy:     healthy,
		Reconciling: reconciling,
	}
}

func snapshotOf(objects ...domain.ObjectHealth) domain.HealthSnapshot {
	result := domain.HealthSnapshot{Objects: map[string]domain.ObjectHealth{}}
	for _, object := range objects {
		result.Objects[object.Ref.String()] = object
	}
	return result
}

// build wires a cluster whose clock advances one second per sleep, so the
// settle timeout and stability hold are reached deterministically.
func build(t *testing.T, flux *scriptedFlux, settle, hold time.Duration) *cluster.Cluster {
	t.Helper()
	now := time.Date(2026, 8, 2, 14, 0, 0, 0, time.UTC)
	return cluster.New(flux, noWorkloads{}, cluster.Options{
		SettleTimeout: settle,
		StabilityHold: hold,
		PollInterval:  time.Second,
		Now:           func() time.Time { return now },
		Sleep: func(context.Context, time.Duration) error {
			now = now.Add(time.Second)
			flux.polls++
			return nil
		},
	})
}

// Status is the agent's only read answering with an object's health rather than
// text, and Flux answers it: the source the watch that failed the merge read.
func TestTheDiagnosisAgentsSingleObjectReadIsAnsweredByFlux(t *testing.T) {
	got, err := build(t, &scriptedFlux{}, time.Minute, time.Second).
		Status(context.Background(), "HelmRelease", "media", "sonarr")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	want := domain.ObjectRef{Kind: "HelmRelease", Namespace: "media", Name: "sonarr"}
	if got.Ref != want {
		t.Fatalf("status read %v, want %v", got.Ref, want)
	}
}

// An unreachable cluster must not read as an object with no failure on it: the
// agent would diagnose a merge from a health report nothing ever measured.
func TestAnUnreachableClusterFailsTheDiagnosisReadRatherThanAnsweringItEmpty(t *testing.T) {
	c := cluster.New(unreadableFlux{}, noWorkloads{}, cluster.Options{})

	_, err := c.Status(context.Background(), "HelmRelease", "media", "sonarr")

	if !errors.Is(err, errUnreachable) {
		t.Fatalf("status did not report the cluster was unreachable: %v", err)
	}
}

func TestWatchPassesOnceTheRevisionLandsAndHoldsStable(t *testing.T) {
	flux := &scriptedFlux{
		states:    [][]domain.ObjectHealth{{object("sonarr", true, true)}, {object("sonarr", true, false)}},
		revisions: []string{"old", "abc123"},
	}
	baseline := snapshotOf(object("sonarr", true, false))

	outcome, err := build(t, flux, time.Minute, 2*time.Second).Watch(context.Background(), baseline, "abc123")
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	if outcome.Result != domain.WatchPass {
		t.Fatalf("want pass, got %q", outcome.Result)
	}
}

// A revision that never arrives means the merge was never applied, so the
// window must not pass on a cluster that looks fine. It must not report an
// ordinary stall either: that reaches the repair loop with no objects, Broken
// returns nil for an empty list, and the run records a merge it never saw
// applied as healthy. There is nothing to diagnose and nothing to revert, so the
// only honest answer is that the merge could not be observed.
func TestWatchRefusesToJudgeAMergeWhoseRevisionNeverReachedTheCluster(t *testing.T) {
	flux := &scriptedFlux{
		states:    [][]domain.ObjectHealth{{object("sonarr", true, false)}},
		revisions: []string{"old"},
	}
	baseline := snapshotOf(object("sonarr", true, false))

	outcome, err := build(t, flux, 3*time.Second, time.Second).Watch(context.Background(), baseline, "abc123")
	if err == nil {
		t.Fatalf("want an error the caller halts on, got %+v", outcome)
	}
	if outcome.Result != "" {
		t.Fatalf("want no result alongside the error, got %q", outcome.Result)
	}
}

// A commit landing on the branch after this merge - a human, another automation
// - makes Flux fetch a descendant, which carries this merge without ever
// equalling it. The merge was applied, so refusing to judge the window would
// halt the rest of the queue over a claim that is false.
func TestWatchDoesNotClaimAMergeWasNeverAppliedWhenTheBranchMovedPastIt(t *testing.T) {
	flux := &scriptedFlux{
		states:    [][]domain.ObjectHealth{{object("sonarr", true, false)}},
		revisions: []string{"aaaa1111", "bbbb2222"},
	}
	baseline := snapshotOf(object("sonarr", true, false))

	outcome, err := build(t, flux, 3*time.Second, time.Second).
		Watch(context.Background(), baseline, "cccc3333")
	if err != nil {
		t.Fatalf("want a stall that keeps the merge, got %v", err)
	}
	if outcome.Result != domain.WatchStalled || len(outcome.Pending) != 0 || len(outcome.Failures) != 0 {
		t.Fatalf("want a stall with nothing attributed, got %+v", outcome)
	}
}

// The post-revert heal check passes an empty revision because it does not care
// which one is applied; that must not turn into an unobservable window.
func TestWatchAcceptsAnEmptyRevisionWithoutWaitingForAFetch(t *testing.T) {
	flux := &scriptedFlux{states: [][]domain.ObjectHealth{{object("sonarr", true, false)}}}
	baseline := snapshotOf(object("sonarr", true, false))

	outcome, err := build(t, flux, time.Minute, time.Second).Watch(context.Background(), baseline, "")
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	if outcome.Result != domain.WatchPass {
		t.Fatalf("want pass, got %q", outcome.Result)
	}
}

// A window that runs out of time with the revision applied, nothing this merge
// owns still moving and nothing broken has found no fault. Calling that stalled
// sends a cluster that answered "well" to repair, so the hold that was already
// running is allowed to finish and answer instead. Reachable only on a
// settleTimeout shorter than the hold, which validateWatch rejects.
func TestWatchPassesWhenOnlyTheHoldOutlastedTheSettleTimeout(t *testing.T) {
	flux := &scriptedFlux{
		states: [][]domain.ObjectHealth{
			{object("sonarr", true, true)},
			{object("sonarr", true, false)},
		},
		revisions: []string{"abc123"},
	}
	baseline := snapshotOf(object("sonarr", true, false))

	outcome, err := build(t, flux, 2*time.Second, time.Minute).Watch(context.Background(), baseline, "abc123")
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	if outcome.Result != domain.WatchPass || len(outcome.Pending) != 0 || len(outcome.Failures) != 0 {
		t.Fatalf("want a pass with nothing attributed, got %+v", outcome)
	}
}

func TestWatchFailsAsSoonAsAnObjectNewlyBreaks(t *testing.T) {
	flux := &scriptedFlux{
		states:    [][]domain.ObjectHealth{{object("sonarr", false, false)}},
		revisions: []string{"abc123"},
	}
	baseline := snapshotOf(object("sonarr", true, false))

	outcome, err := build(t, flux, time.Minute, time.Second).Watch(context.Background(), baseline, "abc123")
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	if outcome.Result != domain.WatchFail || len(outcome.Failures) != 1 {
		t.Fatalf("want one new failure, got %+v", outcome)
	}
	if outcome.Failures[0].Ref.Name != "sonarr" {
		t.Fatalf("wrong object attributed: %+v", outcome.Failures[0])
	}
}

// A workload is briefly unready for ordinary reasons - a rollout replacing its
// last pod - and ending the window on the first sighting discards a healthy
// update for it.
func TestWatchIgnoresAFailureThatClearsWithinTheStabilityHold(t *testing.T) {
	flux := &scriptedFlux{
		states: [][]domain.ObjectHealth{
			{object("sonarr", false, false)},
			{object("sonarr", true, false)},
		},
		revisions: []string{"abc123"},
	}
	baseline := snapshotOf(object("sonarr", true, false))

	outcome, err := build(t, flux, time.Minute, 3*time.Second).Watch(context.Background(), baseline, "abc123")
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	if outcome.Result != domain.WatchPass {
		t.Fatalf("want a pass once the object recovered, got %+v", outcome)
	}
}

func TestWatchFailsOnlyOnceTheFailurePersists(t *testing.T) {
	flux := &scriptedFlux{
		states:    [][]domain.ObjectHealth{{object("sonarr", false, false)}},
		revisions: []string{"abc123"},
	}
	baseline := snapshotOf(object("sonarr", true, false))

	outcome, err := build(t, flux, time.Minute, 3*time.Second).Watch(context.Background(), baseline, "abc123")
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	if outcome.Result != domain.WatchFail || len(outcome.Failures) != 1 {
		t.Fatalf("want one new failure, got %+v", outcome)
	}
	if flux.polls < 3 {
		t.Fatalf("want the failure held for the stability hold, failed after %d polls", flux.polls)
	}
}

// An object that is unhealthy on every other poll never survives a hold, so the
// window stalls and the merge is kept. That is the accepted cost of deciding
// persistence from elapsed time rather than from a count of failing polls: a
// count would make the verdict depend on the poll interval, which is the
// property the sweep below forbids. Any rule that catches this flapper must
// still pass every other test in this file.
func TestAnObjectFlappingEveryPollNeverConfirmsAsPersistent(t *testing.T) {
	cases := []struct {
		name    string
		clearer domain.ObjectHealth
	}{
		{"unhealthy alternating with reconciling", object("sonarr", true, true)},
		{"unhealthy alternating with healthy", object("sonarr", true, false)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var states [][]domain.ObjectHealth
			for poll := range 40 {
				state := tc.clearer
				if poll%2 == 0 {
					state = object("sonarr", false, false)
				}
				states = append(states, []domain.ObjectHealth{state})
			}
			flux := &scriptedFlux{states: states, revisions: []string{"abc123"}}
			baseline := snapshotOf(object("sonarr", true, false))

			outcome, err := build(t, flux, 20*time.Second, 5*time.Second).
				Watch(context.Background(), baseline, "abc123")
			if err != nil {
				t.Fatalf("watch: %v", err)
			}
			if outcome.Result != domain.WatchStalled || len(outcome.Failures) != 0 {
				t.Fatalf("want a stall that keeps the merge, got %+v", outcome)
			}
			if len(outcome.Pending) != 1 || outcome.Pending[0].Ref.Name != "sonarr" {
				t.Fatalf("want the flapper named to the operator, got %+v", outcome.Pending)
			}
		})
	}
}

// Two short unready episodes closer together than one hold are an ordinary
// application - a liveness probe restarting one of three pods twice - and each
// gets the full hold to clear on its own. A rule that keeps an object's clock
// running across a recovery shorter than a hold confirms the second episode with
// no grace at all and reverts this merge.
func TestTwoBriefUnreadyEpisodesLessThanAHoldApartStillKeepTheMerge(t *testing.T) {
	states := make([][]domain.ObjectHealth, 0, 24)
	for poll := range 24 {
		state := object("sonarr", true, false)
		if poll == 0 || poll == 10 || poll == 11 {
			state = object("sonarr", false, false)
		}
		states = append(states, []domain.ObjectHealth{state})
	}
	flux := &scriptedFlux{states: states, revisions: []string{"abc123"}}
	baseline := snapshotOf(object("sonarr", true, false))

	outcome, err := build(t, flux, 40*time.Second, 10*time.Second).
		Watch(context.Background(), baseline, "abc123")
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	if outcome.Result != domain.WatchPass {
		t.Fatalf("want the merge kept, got %+v", outcome)
	}
}

// dutyCycleFlux is unhealthy for the first unhealthyFor of every period, as a
// function of the clock rather than of the poll count, so the same continuous
// cluster behaviour can be sampled at different rates.
type dutyCycleFlux struct {
	now          func() time.Time
	started      time.Time
	period       time.Duration
	unhealthyFor time.Duration
}

func (f *dutyCycleFlux) Objects(context.Context) ([]domain.ObjectHealth, error) {
	phase := f.now().Sub(f.started) % f.period
	return []domain.ObjectHealth{object("sonarr", phase >= f.unhealthyFor, false)}, nil
}

func (f *dutyCycleFlux) Object(_ context.Context, kind, namespace, name string) (domain.ObjectHealth, error) {
	return domain.ObjectHealth{Ref: domain.ObjectRef{Kind: kind, Namespace: namespace, Name: name}}, nil
}

func (f *dutyCycleFlux) SourceRevision(context.Context) (string, error) { return "abc123", nil }

func (f *dutyCycleFlux) Reconcile(context.Context, time.Time) error { return nil }

// watch.pollInterval exists to bound API load, not to set revert policy, so one
// cluster behaviour must get one verdict at every rate it is sampled at. A rule
// that counts failing polls instead of measuring elapsed failing time confirms
// this 8%-duty-cycle object at 10s, 15s and 30s and never at 5s - same cluster,
// opposite verdicts, chosen by a knob nobody thinks of as a safety setting.
func TestTheRevertVerdictDoesNotDependOnHowOftenTheClusterIsPolled(t *testing.T) {
	// Every interval here is shorter than the duty cycle's period; sampling at
	// exactly the period aliases the duty cycle away and answers about nothing.
	for _, poll := range []time.Duration{5 * time.Second, 10 * time.Second, 15 * time.Second, 30 * time.Second} {
		t.Run(poll.String(), func(t *testing.T) {
			start := time.Date(2026, 8, 2, 14, 0, 0, 0, time.UTC)
			now := start
			flux := &dutyCycleFlux{
				now:          func() time.Time { return now },
				started:      start,
				period:       time.Minute,
				unhealthyFor: 5 * time.Second,
			}
			c := cluster.New(flux, noWorkloads{}, cluster.Options{
				SettleTimeout: 10 * time.Minute,
				StabilityHold: time.Minute,
				PollInterval:  poll,
				Now:           func() time.Time { return now },
				Sleep: func(_ context.Context, d time.Duration) error {
					now = now.Add(d)
					return nil
				},
			})
			baseline := snapshotOf(object("sonarr", true, false))

			outcome, err := c.Watch(context.Background(), baseline, "abc123")
			if err != nil {
				t.Fatalf("watch: %v", err)
			}
			if outcome.Result != domain.WatchStalled || len(outcome.Failures) != 0 {
				t.Fatalf("want the same stalled verdict at every poll interval, got %+v", outcome)
			}
		})
	}
}

// The hold is the whole budget a failure gets, and nothing extends it for an
// object whose own controller retries on a longer interval. A HelmRelease whose
// retryInterval defaults to a spec.interval of minutes is reverted at the hold
// even though its next retry would have fixed it, so stabilityHold has to be set
// against the retry intervals of the objects being watched - see
// docs/install.md.
func TestAFailureOutlastingTheHoldIsConfirmedEvenWhenItsOwnRetryWouldHaveFixedIt(t *testing.T) {
	states := make([][]domain.ObjectHealth, 0, 12)
	for poll := range 12 {
		state := object("sonarr", true, false)
		if poll < 5 {
			state = object("sonarr", false, false)
		}
		states = append(states, []domain.ObjectHealth{state})
	}
	flux := &scriptedFlux{states: states, revisions: []string{"abc123"}}
	baseline := snapshotOf(object("sonarr", true, false))

	outcome, err := build(t, flux, time.Minute, 3*time.Second).
		Watch(context.Background(), baseline, "abc123")
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	if outcome.Result != domain.WatchFail || len(outcome.Failures) != 1 {
		t.Fatalf("want the failure confirmed at the hold, got %+v", outcome)
	}
	if flux.polls >= 5 {
		t.Fatalf("want the verdict reached before the controller's own retry, took %d polls", flux.polls)
	}
}

// A merge that deletes a tracked object leaves the survivors healthy, which must
// not read as a clean window.
func TestWatchFailsWhenABaselineObjectDisappears(t *testing.T) {
	flux := &scriptedFlux{
		states:    [][]domain.ObjectHealth{{object("sonarr", true, false)}},
		revisions: []string{"abc123"},
	}
	baseline := snapshotOf(object("sonarr", true, false), object("gatus", true, false))

	outcome, err := build(t, flux, time.Minute, time.Second).Watch(context.Background(), baseline, "abc123")
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	if outcome.Result != domain.WatchFail || len(outcome.Failures) != 1 {
		t.Fatalf("want one new failure, got %+v", outcome)
	}
	if outcome.Failures[0].Ref.Name != "gatus" {
		t.Fatalf("wrong object attributed: %+v", outcome.Failures[0])
	}
}

// Breakage seen while the source still carries the pre-merge revision belongs to
// something else; the merge has not been applied yet.
func TestWatchDoesNotBlameTheMergeBeforeItsRevisionLands(t *testing.T) {
	flux := &scriptedFlux{
		states: [][]domain.ObjectHealth{
			{object("sonarr", false, false)},
			{object("sonarr", true, false)},
		},
		revisions: []string{"old", "abc123"},
	}
	baseline := snapshotOf(object("sonarr", true, false))

	outcome, err := build(t, flux, time.Minute, time.Second).Watch(context.Background(), baseline, "abc123")
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	if outcome.Result != domain.WatchPass {
		t.Fatalf("want pass, got %q: %+v", outcome.Result, outcome.Failures)
	}
}

// Something already broken before the merge must never be attributed to it.
func TestWatchIgnoresBreakageThatPredatesTheMerge(t *testing.T) {
	flux := &scriptedFlux{
		states: [][]domain.ObjectHealth{{
			object("sonarr", true, false),
			object("nfs", false, false),
		}},
		revisions: []string{"abc123"},
	}
	baseline := snapshotOf(object("sonarr", true, false), object("nfs", false, false))

	outcome, err := build(t, flux, time.Minute, time.Second).Watch(context.Background(), baseline, "abc123")
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	if outcome.Result != domain.WatchPass {
		t.Fatalf("want pass despite the pre-existing failure, got %q: %+v", outcome.Result, outcome.Failures)
	}
}

// A reconciling object holds the window open; it is not a pass and not a failure.
func TestWatchWillNotPassWhileAnObjectIsStillReconciling(t *testing.T) {
	flux := &scriptedFlux{
		states:    [][]domain.ObjectHealth{{object("sonarr", true, true)}},
		revisions: []string{"abc123"},
	}
	baseline := snapshotOf(object("sonarr", true, false))

	outcome, err := build(t, flux, 3*time.Second, time.Second).Watch(context.Background(), baseline, "abc123")
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	if outcome.Result != domain.WatchStalled || len(outcome.Pending) != 1 {
		t.Fatalf("want stalled with one pending object, got %+v", outcome)
	}
}

// Six shapes reach this one state, all of them permanent: a DaemonSet short a
// node, a StatefulSet wedged on an ordered rollout or held back by a partition,
// a Deployment with its progress deadline disabled, a pod recreated faster than
// the adapter's rollout grace, and a repository reconciling on its own interval.
// Each one used to deny every later merge the pass path while never appearing in
// Pending, so every window cost a full settle timeout and no merge could ever be
// confirmed good again.
func TestWatchPassesWhileSomethingAlreadyReconcilingAtBaselineKeepsReconciling(t *testing.T) {
	flux := &scriptedFlux{
		states: [][]domain.ObjectHealth{{
			object("sonarr", true, false),
			object("node-exporter", true, true),
		}},
		revisions: []string{"abc123"},
	}
	baseline := snapshotOf(object("sonarr", true, false), object("node-exporter", true, true))

	outcome, err := build(t, flux, 10*time.Second, 2*time.Second).
		Watch(context.Background(), baseline, "abc123")
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	if outcome.Result != domain.WatchPass {
		t.Fatalf("want pass, got %q with pending %+v", outcome.Result, outcome.Pending)
	}
}

// Turning this pass into a stall re-opens the C-H11 false positive C-H14 removed:
// an object reconciling forever would again deny every later merge the pass path.
func TestWatchPassesWhenAMergeWorsensAnObjectAlreadyUnattributableAtBaseline(t *testing.T) {
	flux := &scriptedFlux{
		states: [][]domain.ObjectHealth{{
			object("sonarr", true, false),
			object("nfs", false, true),
		}},
		revisions: []string{"abc123"},
	}
	baseline := snapshotOf(object("sonarr", true, false), object("nfs", true, true))

	outcome, err := build(t, flux, 10*time.Second, 2*time.Second).
		Watch(context.Background(), baseline, "abc123")
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	if outcome.Result != domain.WatchPass {
		t.Fatalf("want pass, got %q with pending %+v", outcome.Result, outcome.Pending)
	}
}

// Scoping the settle check must not let churn elsewhere hide real breakage. The
// DaemonSet case is the one to hold on to: past the adapter's rollout grace it
// reads unhealthy and settled, and that is the only route by which DaemonSet
// breakage reaches a verdict at all.
func TestWatchStillFailsWhileUnrelatedChurnHoldsTheClusterUnsettled(t *testing.T) {
	broken := domain.ObjectHealth{
		Ref:    domain.ObjectRef{Kind: "DaemonSet", Namespace: "kube-system", Name: "cilium"},
		Reason: "2/3 replicas ready",
	}
	healthy := broken
	healthy.Healthy = true

	flux := &scriptedFlux{
		states:    [][]domain.ObjectHealth{{broken, object("node-exporter", true, true)}},
		revisions: []string{"abc123"},
	}
	baseline := snapshotOf(healthy, object("node-exporter", true, true))

	outcome, err := build(t, flux, time.Minute, 2*time.Second).
		Watch(context.Background(), baseline, "abc123")
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	if outcome.Result != domain.WatchFail || len(outcome.Failures) != 1 {
		t.Fatalf("want one new failure, got %+v", outcome)
	}
	if outcome.Failures[0].Ref.Name != "cilium" {
		t.Fatalf("wrong object attributed: %+v", outcome.Failures[0])
	}
}

// throttledFlux advances a shared clock on every source read, standing in for a
// directed Retry-After delay the Kubernetes transport honours inside a poll.
type throttledFlux struct {
	scriptedFlux
	now       *time.Time
	readDelay time.Duration
}

func (f *throttledFlux) SourceRevision(ctx context.Context) (string, error) {
	*f.now = f.now.Add(f.readDelay)
	return f.scriptedFlux.SourceRevision(ctx)
}

// A cluster that recovers with a hold's worth of budget to spare is stalled if
// the wall-clock settle deadline is spent by time the poll spent blocked inside
// throttled reads. The settle budget is meant to bound how long the cluster gets
// to recover, not how slowly ops-pilot is allowed to read it.
func TestWatchDoesNotSpendItsSettleBudgetOnTimeBlockedInsideThrottledReads(t *testing.T) {
	now := time.Date(2026, 8, 2, 14, 0, 0, 0, time.UTC)
	flux := &throttledFlux{
		scriptedFlux: scriptedFlux{
			states: [][]domain.ObjectHealth{
				{object("sonarr", true, true)},
				{object("sonarr", true, true)},
				{object("sonarr", true, true)},
				{object("sonarr", true, true)},
				{object("sonarr", true, false)},
			},
			revisions: []string{"abc123"},
		},
		now:       &now,
		readDelay: time.Second,
	}
	c := cluster.New(flux, noWorkloads{}, cluster.Options{
		SettleTimeout: 10 * time.Second,
		StabilityHold: 3 * time.Second,
		PollInterval:  time.Second,
		Now:           func() time.Time { return now },
		Sleep: func(context.Context, time.Duration) error {
			now = now.Add(time.Second)
			flux.polls++
			return nil
		},
	})
	baseline := snapshotOf(object("sonarr", true, false))

	outcome, err := c.Watch(context.Background(), baseline, "abc123")
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	if outcome.Result != domain.WatchPass {
		t.Fatalf("want pass, got %q with pending %+v", outcome.Result, outcome.Pending)
	}
}

func TestRestoredReportsAnObjectThatNeverRecovers(t *testing.T) {
	flux := &scriptedFlux{states: [][]domain.ObjectHealth{{object("sonarr", false, false)}}}
	baseline := snapshotOf(object("sonarr", true, false))
	broken := []domain.ObjectHealth{object("sonarr", false, false)}

	if _, err := build(t, flux, 2*time.Second, time.Second).Restored(context.Background(), baseline, broken); err == nil {
		t.Fatal("want an error when the revert does not restore health")
	}
}

// A revert that has already restored every object and needs only a moment more
// to confirm it is a success. Calling it a failure halts the run with "did not
// recover after the revert" - and with an empty object list, so the message
// names "the cluster" - while the cluster is in fact well.
func TestRestoredLetsARecoveryAlreadyUnderWayFinishConfirming(t *testing.T) {
	flux := &scriptedFlux{states: [][]domain.ObjectHealth{
		{object("sonarr", false, false)},
		{object("sonarr", true, false)},
	}}
	baseline := snapshotOf(object("sonarr", true, false))
	broken := []domain.ObjectHealth{object("sonarr", false, false)}

	outcome, err := build(t, flux, 3*time.Second, 3*time.Second).
		Restored(context.Background(), baseline, broken)
	if err != nil {
		t.Fatalf("want the recovery confirmed rather than called a failure: %v", err)
	}
	if outcome.Result != domain.WatchPass {
		t.Fatalf("want pass, got %q", outcome.Result)
	}
}

// The grace is only for a recovery that holds. One healthy poll followed by the
// same breakage is not a revert that worked, and the run must still halt.
func TestRestoredStillFailsWhenAMomentaryRecoveryDoesNotHold(t *testing.T) {
	flux := &scriptedFlux{states: [][]domain.ObjectHealth{
		{object("sonarr", false, false)},
		{object("sonarr", true, false)},
		{object("sonarr", false, false)},
	}}
	baseline := snapshotOf(object("sonarr", true, false))
	broken := []domain.ObjectHealth{object("sonarr", false, false)}

	if _, err := build(t, flux, 3*time.Second, 3*time.Second).
		Restored(context.Background(), baseline, broken); err == nil {
		t.Fatal("want an error when the revert does not hold the cluster healthy")
	}
}

// A GitOps repository reconciles continuously, so unrelated churn must not make
// a successful revert look like a failure. This is what halted a real run.
func TestRestoredIgnoresChurnInObjectsTheMergeNeverBroke(t *testing.T) {
	flux := &scriptedFlux{states: [][]domain.ObjectHealth{{
		object("sonarr", true, false),
		object("unrelated", true, true),
	}}}
	baseline := snapshotOf(object("sonarr", true, false))
	broken := []domain.ObjectHealth{object("sonarr", false, false)}

	outcome, err := build(t, flux, time.Minute, time.Second).
		Restored(context.Background(), baseline, broken)
	if err != nil {
		t.Fatalf("want the revert accepted despite unrelated churn: %v", err)
	}
	if outcome.Result != domain.WatchPass {
		t.Fatalf("want pass, got %q", outcome.Result)
	}
}

// throttledObjectsFlux advances a shared clock on every object read, standing in
// for a directed Retry-After delay the Kubernetes transport honours inside a
// Snapshot during the post-revert heal check.
type throttledObjectsFlux struct {
	scriptedFlux
	now       *time.Time
	readDelay time.Duration
}

func (f *throttledObjectsFlux) Objects(ctx context.Context) ([]domain.ObjectHealth, error) {
	*f.now = f.now.Add(f.readDelay)
	return f.scriptedFlux.Objects(ctx)
}

// A revert whose objects recover with the stability hold to spare is stalled if
// the wall-clock settle deadline is spent by time the poll spent blocked inside
// throttled reads. The budget bounds how long the cluster gets to recover, not
// how slowly ops-pilot is allowed to read it - and a false stall here escalates
// the revert as "did not recover".
func TestRestoredDoesNotSpendItsSettleBudgetOnTimeBlockedInsideThrottledReads(t *testing.T) {
	now := time.Date(2026, 8, 2, 14, 0, 0, 0, time.UTC)
	flux := &throttledObjectsFlux{
		scriptedFlux: scriptedFlux{
			states: [][]domain.ObjectHealth{
				{object("sonarr", false, false)},
				{object("sonarr", false, false)},
				{object("sonarr", false, false)},
				{object("sonarr", false, false)},
				{object("sonarr", false, false)},
				{object("sonarr", false, false)},
				{object("sonarr", false, false)},
				{object("sonarr", true, false)},
			},
		},
		now:       &now,
		readDelay: time.Second,
	}
	c := cluster.New(flux, noWorkloads{}, cluster.Options{
		SettleTimeout: 10 * time.Second,
		StabilityHold: 3 * time.Second,
		PollInterval:  time.Second,
		Now:           func() time.Time { return now },
		Sleep: func(context.Context, time.Duration) error {
			now = now.Add(time.Second)
			flux.polls++
			return nil
		},
	})
	baseline := snapshotOf(object("sonarr", true, false))
	broken := []domain.ObjectHealth{object("sonarr", false, false)}

	outcome, err := c.Restored(context.Background(), baseline, broken)
	if err != nil {
		t.Fatalf("want the revert accepted once the cluster recovered: %v", err)
	}
	if outcome.Result != domain.WatchPass {
		t.Fatalf("want pass, got %q with pending %+v", outcome.Result, outcome.Pending)
	}
}

// Broken is the last read before a merge is discarded, so what it does with each
// shape of snapshot decides whether a production change survives.
func TestBrokenReportsWhichNamedObjectsAreStillNotWell(t *testing.T) {
	asked := domain.ObjectHealth{
		Ref:    domain.ObjectRef{Kind: "HelmRelease", Namespace: "media", Name: "sonarr"},
		Reason: "0/1 replicas ready",
	}
	tests := []struct {
		name    string
		cluster []domain.ObjectHealth
		want    []string
		reason  string
	}{
		{
			name:    "recovered while the operator was deciding",
			cluster: []domain.ObjectHealth{object("sonarr", true, false)},
		},
		{
			name:    "still unhealthy, carrying the cluster's current reason",
			cluster: []domain.ObjectHealth{{Ref: asked.Ref, Reason: "CrashLoopBackOff"}},
			want:    []string{"media/HelmRelease/sonarr"},
			reason:  "CrashLoopBackOff",
		},
		{
			// This is the post-revert heal check. An object churning again has not
			// finished healing, so it is not yet restored.
			name:    "healthy again but still reconciling",
			cluster: []domain.ObjectHealth{object("sonarr", true, true)},
			want:    []string{"media/HelmRelease/sonarr"},
		},
		{
			name:    "gone from the cluster entirely",
			cluster: []domain.ObjectHealth{object("gatus", true, false)},
			want:    []string{"media/HelmRelease/sonarr"},
			reason:  "0/1 replicas ready",
		},
		{
			name: "breakage and churn in objects nobody asked about",
			cluster: []domain.ObjectHealth{
				object("sonarr", true, false),
				object("nfs", false, false),
				object("node-exporter", true, true),
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			flux := &scriptedFlux{states: [][]domain.ObjectHealth{test.cluster}}
			got, err := build(t, flux, time.Minute, time.Second).
				Broken(context.Background(), []domain.ObjectHealth{asked})
			if err != nil {
				t.Fatalf("broken: %v", err)
			}
			names := make([]string, 0, len(got))
			for _, object := range got {
				names = append(names, object.Ref.String())
			}
			if !slices.Equal(names, test.want) {
				t.Fatalf("want %v still broken, got %v", test.want, names)
			}
			if test.reason != "" && got[0].Reason != test.reason {
				t.Fatalf("want reason %q, got %q", test.reason, got[0].Reason)
			}
		})
	}
}

// An empty list is the answer, not a question: no object was attributed to this
// merge, so there is nothing to re-read. Reading the cluster anyway would let an
// unrelated API error decide the fate of a merge nothing was ever wrong with.
func TestBrokenAnswersAnEmptyListWithoutReadingTheCluster(t *testing.T) {
	broken, err := cluster.New(unreadableFlux{}, noWorkloads{}, cluster.Options{}).
		Broken(context.Background(), nil)
	if err != nil {
		t.Fatalf("want an empty list answered without a read: %v", err)
	}
	if len(broken) != 0 {
		t.Fatalf("want nothing broken, got %+v", broken)
	}
}

// A caller that cannot read the cluster must be told so. Answering "nothing is
// broken" would let a transient API error keep a merge that broke production.
func TestBrokenReportsAFailedReadRatherThanAHealthyCluster(t *testing.T) {
	_, err := cluster.New(unreadableFlux{}, noWorkloads{}, cluster.Options{}).
		Broken(context.Background(), []domain.ObjectHealth{object("sonarr", false, false)})
	if err == nil {
		t.Fatal("want the read failure surfaced, not an empty result")
	}
}

func TestReconcileTriggersFluxOnce(t *testing.T) {
	flux := &scriptedFlux{states: [][]domain.ObjectHealth{{}}}
	if err := build(t, flux, time.Minute, time.Second).Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if flux.Triggers != 1 {
		t.Fatalf("want one trigger, got %d", flux.Triggers)
	}
}

// Churn that predates the merge must not be reported against it. A workload
// already stuck before the run poisoned every window this way.
func TestStalledWindowIgnoresObjectsAlreadyUnsettledAtBaseline(t *testing.T) {
	flux := &scriptedFlux{
		states: [][]domain.ObjectHealth{{
			object("sonarr", true, true),
			object("karakeep", true, true),
		}},
		revisions: []string{"abc123"},
	}
	// karakeep was already churning when the baseline was taken; sonarr was not.
	baseline := snapshotOf(object("sonarr", true, false), object("karakeep", true, true))

	outcome, err := build(t, flux, 3*time.Second, time.Second).
		Watch(context.Background(), baseline, "abc123")
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	if outcome.Result != domain.WatchStalled {
		t.Fatalf("want stalled, got %q", outcome.Result)
	}
	if len(outcome.Pending) != 1 || outcome.Pending[0].Ref.Name != "sonarr" {
		t.Fatalf("want only sonarr attributed, got %+v", outcome.Pending)
	}
}

// timelineFlux drives health from the absolute clock rather than the poll count,
// and advances that clock on every object read, so one cluster behaviour can be
// sampled at different rates and different read latencies.
type timelineFlux struct {
	now         *time.Time
	started     time.Time
	unhealthy   func(time.Duration) bool
	reconciling func(time.Duration) bool
	readDelay   time.Duration
}

func (f *timelineFlux) Objects(context.Context) ([]domain.ObjectHealth, error) {
	*f.now = f.now.Add(f.readDelay)
	at := f.now.Sub(f.started)
	return []domain.ObjectHealth{object("sonarr", !f.unhealthy(at), f.reconciling(at))}, nil
}

func (f *timelineFlux) Object(_ context.Context, kind, namespace, name string) (domain.ObjectHealth, error) {
	return domain.ObjectHealth{Ref: domain.ObjectRef{Kind: kind, Namespace: namespace, Name: name}}, nil
}

func (f *timelineFlux) SourceRevision(context.Context) (string, error) { return "abc123", nil }

func (f *timelineFlux) Reconcile(context.Context, time.Time) error { return nil }

func never(time.Duration) bool { return false }

// A cluster's health is a fact about wall-clock time, and watch.pollInterval is a
// knob for bounding API load, so one timeline has to get one verdict at every
// rate it is sampled at. TestTheRevertVerdictDoesNotDependOnHowOftenTheCluster
// IsPolled pins that at readLatency=0 only, which is exactly where the tempting
// repairs of the stability hold are invisible: both counting healthy observations
// and crediting the hold back the time a poll spent blocked in a read make the
// hold's real length a multiple of the sample spacing pollInterval+readLatency
// rather than a duration. At a 20s read latency that turns a 60s hold into 100s
// at a 30s poll and 180s at a 10s poll, so a merge that breaks something at 130s
// is passed at one rate and reverted at the other - the poll interval deciding a
// revert, which is the failure this rule exists to prevent.
//
// Scoped to verdicts the hold decides, sampled densely enough to resolve it.
// Verdicts the settle deadline decides are pinned separately, by
// TestTheSettleDeadlineClosesTheWindowAtOneInstantAtEveryPollRate for Watch;
// Restored still credits read time back to its own deadline, so its
// deadline-decided verdicts are not invariant here at readLatency>0.
func TestOneHealthTimelineGetsOneVerdictAtEveryPollRateAndReadLatency(t *testing.T) {
	unhealthyFrom := func(d time.Duration) func(time.Duration) bool {
		return func(at time.Duration) bool { return at >= d }
	}
	until := func(d time.Duration) func(time.Duration) bool {
		return func(at time.Duration) bool { return at < d }
	}
	timelines := []struct {
		name        string
		unhealthy   func(time.Duration) bool
		reconciling func(time.Duration) bool
	}{
		{name: "healthy throughout", unhealthy: never, reconciling: never},
		{name: "breaks at 130s and stays broken", unhealthy: unhealthyFrom(130 * time.Second), reconciling: never},
		{name: "broken throughout", unhealthy: func(time.Duration) bool { return true }, reconciling: never},
		{name: "broken until 200s", unhealthy: until(200 * time.Second), reconciling: never},
		{name: "reconciling until 3m", unhealthy: never, reconciling: until(3 * time.Minute)},
		{name: "reconciles again for 15s at 100s", unhealthy: never,
			reconciling: func(at time.Duration) bool { return at >= 100*time.Second && at < 115*time.Second }},
	}
	// Every pollInterval+readLatency here stays under the hold, so at least one
	// sample always lands inside it; past that the grid aliases and no rule is
	// invariant.
	polls := []time.Duration{5 * time.Second, 10 * time.Second, 15 * time.Second, 30 * time.Second}

	for _, timeline := range timelines {
		for _, latency := range []time.Duration{0, 5 * time.Second, 20 * time.Second} {
			t.Run(timeline.name+" at "+latency.String(), func(t *testing.T) {
				for _, path := range []string{"watch", "restored"} {
					want := verdictAt(t, path, polls[0], latency, timeline.unhealthy, timeline.reconciling)
					for _, poll := range polls[1:] {
						got := verdictAt(t, path, poll, latency, timeline.unhealthy, timeline.reconciling)
						if got != want {
							t.Fatalf("%s: %s at a %v poll but %s at a %v poll - the poll interval decided the verdict",
								path, want, polls[0], got, poll)
						}
					}
				}
			})
		}
	}
}

func verdictAt(
	t *testing.T,
	path string,
	poll, latency time.Duration,
	unhealthy, reconciling func(time.Duration) bool,
) string {
	t.Helper()
	started := time.Date(2026, 8, 2, 14, 0, 0, 0, time.UTC)
	now := started
	c := cluster.New(
		&timelineFlux{now: &now, started: started, unhealthy: unhealthy, reconciling: reconciling, readDelay: latency},
		noWorkloads{},
		cluster.Options{
			SettleTimeout: 10 * time.Minute,
			StabilityHold: time.Minute,
			PollInterval:  poll,
			Now:           func() time.Time { return now },
			Sleep:         func(_ context.Context, d time.Duration) error { now = now.Add(d); return nil },
		})
	baseline := snapshotOf(object("sonarr", true, false))

	var outcome cluster.Outcome
	var err error
	if path == "watch" {
		outcome, err = c.Watch(context.Background(), baseline, "abc123")
	} else {
		outcome, err = c.Restored(context.Background(), baseline, []domain.ObjectHealth{object("sonarr", false, false)})
	}
	if err != nil {
		return "error"
	}
	return string(outcome.Result)
}

// The settle timeout is the budget the cluster gets to recover in, and
// watch.pollInterval is a knob for bounding API load, so the instant the window
// closes has to be one instant of wall clock however fast the cluster is polled.
// Crediting each poll's read time back to the deadline made the real window
// settleTimeout x (1 + readLatency/pollInterval) instead: at a 5s read latency a
// cluster still reconciling at 12m was passed at a 10s poll and stalled at a 30s
// one, and at a 20s latency the same 10m budget bought 50m of wall clock at a 5s
// poll and 13m40s at a 60s one. Every timeline here clears the deadline by more
// than one sample spacing; a recovery landing inside one sample of it is the
// quantisation TestOneHealthTimelineGetsOneVerdictAtEveryPollRateAndReadLatency
// already accepts as the floor.
func TestTheSettleDeadlineClosesTheWindowAtOneInstantAtEveryPollRate(t *testing.T) {
	until := func(d time.Duration) func(time.Duration) bool {
		return func(at time.Duration) bool { return at < d }
	}
	timelines := []struct {
		name        string
		reconciling func(time.Duration) bool
	}{
		{name: "reconciling until 9m30s", reconciling: until(9*time.Minute + 30*time.Second)},
		{name: "reconciling until 12m", reconciling: until(12 * time.Minute)},
		{name: "reconciling until 20m", reconciling: until(20 * time.Minute)},
		{name: "reconciling for the whole window", reconciling: func(time.Duration) bool { return true }},
	}
	polls := []time.Duration{5 * time.Second, 10 * time.Second, 15 * time.Second, 30 * time.Second}

	for _, timeline := range timelines {
		for _, latency := range []time.Duration{0, 5 * time.Second, 20 * time.Second} {
			t.Run(timeline.name+" at "+latency.String(), func(t *testing.T) {
				want := verdictAt(t, "watch", polls[0], latency, never, timeline.reconciling)
				for _, poll := range polls[1:] {
					got := verdictAt(t, "watch", poll, latency, never, timeline.reconciling)
					if got != want {
						t.Fatalf("%s at a %v poll but %s at a %v poll - the poll interval decided the verdict",
							want, polls[0], got, poll)
					}
				}
			})
		}
	}
}

// A cluster that never settles has to be given up on at the settle timeout, and
// the only slack a sampled window may take is the one poll it takes to notice.
// Read time credited back to the deadline is unbounded in the poll rate, so the
// faster ops-pilot polled the longer a merge nothing could confirm stayed
// deployed unwatched.
func TestAWindowThatNeverSettlesClosesWithinOneSampleOfTheSettleTimeout(t *testing.T) {
	for _, poll := range []time.Duration{5 * time.Second, 10 * time.Second, 30 * time.Second} {
		for _, latency := range []time.Duration{0, 5 * time.Second, 20 * time.Second} {
			t.Run(poll.String()+" at "+latency.String(), func(t *testing.T) {
				result, elapsed := watchElapsed(t, poll, latency, func(time.Duration) bool { return true })
				if result != string(domain.WatchStalled) {
					t.Fatalf("want stalled, got %q", result)
				}
				if limit := 10*time.Minute + poll + latency; elapsed > limit {
					t.Fatalf("window ran %v, more than the %v settle timeout plus one %v sample",
						elapsed, 10*time.Minute, poll+latency)
				}
			})
		}
	}
}

// A stability hold already under way is the cluster answering that it recovered,
// and cutting it off at the settle deadline throws that answer away: the merge is
// reported as stalled, reaches repair and can be reverted although it was well.
// Restored has let a recovery already under way finish confirming since 86b5975,
// and Watch measures the same hold against the same kind of deadline.
func TestAStabilityHoldAlreadyUnderWayFinishesPastTheSettleDeadline(t *testing.T) {
	for _, poll := range []time.Duration{5 * time.Second, 10 * time.Second, 30 * time.Second} {
		t.Run(poll.String(), func(t *testing.T) {
			// Settled at 9m30s of a 10m budget, so the 1m hold can only complete
			// after the deadline.
			result, _ := watchElapsed(t, poll, 0, func(at time.Duration) bool {
				return at < 9*time.Minute+30*time.Second
			})
			if result != string(domain.WatchPass) {
				t.Fatalf("want pass, got %q", result)
			}
		})
	}
}

func watchElapsed(
	t *testing.T,
	poll, latency time.Duration,
	reconciling func(time.Duration) bool,
) (string, time.Duration) {
	t.Helper()
	started := time.Date(2026, 8, 2, 14, 0, 0, 0, time.UTC)
	now := started
	c := cluster.New(
		&timelineFlux{now: &now, started: started, unhealthy: never, reconciling: reconciling, readDelay: latency},
		noWorkloads{},
		cluster.Options{
			SettleTimeout: 10 * time.Minute,
			StabilityHold: time.Minute,
			PollInterval:  poll,
			Now:           func() time.Time { return now },
			Sleep:         func(_ context.Context, d time.Duration) error { now = now.Add(d); return nil },
		})

	outcome, err := c.Watch(context.Background(), snapshotOf(object("sonarr", true, false)), "abc123")
	if err != nil {
		t.Fatalf("watch: %v", err)
	}
	return string(outcome.Result), now.Sub(started)
}

// persistent measures a failure from the first sample that saw it, so what it
// confirms is "unhealthy at every sample across a whole hold" - the elapsed time
// it reads is a floor on the breakage, never a guess at it. That is what keeps a
// coarsely sampled window from reverting a good merge: however far apart throttled
// reads land, a breakage shorter than the hold can never fill one. Measuring
// instead from the last healthy sample would credit the unobserved gap to the
// failure and confirm breakages well under the hold, which is the same merge
// reverted at 3am that the hold exists to prevent.
//
// The phases here walk a sample right through the breakage, because which side of
// the hold a breakage straddling it lands on is decided by where the samples fall
// - the quantisation TestOneHealthTimelineGetsOneVerdictAtEveryPollRateAndRead
// Latency accepts. This asserts only the direction that costs a merge.
func TestABreakageShorterThanTheStabilityHoldIsNeverConfirmedAtAnyPollRate(t *testing.T) {
	for _, poll := range []time.Duration{5 * time.Second, 10 * time.Second, 15 * time.Second, 30 * time.Second} {
		for _, latency := range []time.Duration{0, 5 * time.Second, 20 * time.Second} {
			for _, breaksAt := range []time.Duration{0, 5 * time.Second, 12 * time.Second, 30 * time.Second} {
				name := poll.String() + " at " + latency.String() + " breaking at " + breaksAt.String()
				t.Run(name, func(t *testing.T) {
					// 45s of breakage against the 60s hold, wherever the samples land.
					unhealthy := func(at time.Duration) bool {
						return at >= breaksAt && at < breaksAt+45*time.Second
					}
					if got := verdictAt(t, "watch", poll, latency, unhealthy, never); got == string(domain.WatchFail) {
						t.Fatalf("confirmed a 45s breakage against a 1m hold, so a merge is reverted for it")
					}
				})
			}
		}
	}
}
