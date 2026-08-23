// Package cluster snapshots cluster health and watches a merge land.
package cluster

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/lkshrk/ops-pilot/internal/domain"
)

// FluxReader reads Flux objects and triggers reconciliation.
type FluxReader interface {
	Objects(ctx context.Context) ([]domain.ObjectHealth, error)
	Object(ctx context.Context, kind, namespace, name string) (domain.ObjectHealth, error)
	SourceRevision(ctx context.Context) (string, error)
	Reconcile(ctx context.Context, at time.Time) error
}

// WorkloadReader reads workload health and the evidence used to diagnose it.
type WorkloadReader interface {
	Objects(ctx context.Context) ([]domain.ObjectHealth, error)
	Pods(ctx context.Context, namespace string) (string, error)
	Events(ctx context.Context, namespace, name string) (string, error)
	Logs(ctx context.Context, namespace, pod, container string, lines int64) (string, error)
}

type Options struct {
	SettleTimeout time.Duration
	StabilityHold time.Duration
	PollInterval  time.Duration
	// Now is injectable so the watch is testable without sleeping.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
	// Observe, when set, is called once per poll so a caller can report that a
	// window lasting minutes is still making progress.
	Observe func(Status)
}

// Status is what one poll of the watch saw, including which objects are still
// working so a caller can name them rather than only counting them.
type Status struct {
	Elapsed     time.Duration
	Fetched     bool
	Reconciling []domain.ObjectHealth
	Settled     bool
}

type Cluster struct {
	flux      FluxReader
	workloads WorkloadReader
	options   Options
}

func New(flux FluxReader, workloads WorkloadReader, options Options) *Cluster {
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.Sleep == nil {
		options.Sleep = sleep
	}
	return &Cluster{flux: flux, workloads: workloads, options: options}
}

func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Observe installs the per-poll callback.
func (c *Cluster) Observe(observe func(Status)) { c.options.Observe = observe }

// Snapshot records the health of every Flux object and workload in the cluster.
func (c *Cluster) Snapshot(ctx context.Context) (domain.HealthSnapshot, error) {
	snapshot := domain.HealthSnapshot{
		TakenAt: c.options.Now(),
		Objects: map[string]domain.ObjectHealth{},
	}
	fluxObjects, err := c.flux.Objects(ctx)
	if err != nil {
		return domain.HealthSnapshot{}, err
	}
	workloads, err := c.workloads.Objects(ctx)
	if err != nil {
		return domain.HealthSnapshot{}, err
	}
	for _, object := range append(fluxObjects, workloads...) {
		snapshot.Objects[object.Ref.String()] = object
	}
	return snapshot, nil
}

// Reconcile asks Flux to fetch now.
func (c *Cluster) Reconcile(ctx context.Context) error {
	return c.flux.Reconcile(ctx, c.options.Now())
}

func (c *Cluster) Pods(ctx context.Context, namespace string) (string, error) {
	return c.workloads.Pods(ctx, namespace)
}

func (c *Cluster) Events(ctx context.Context, namespace, name string) (string, error) {
	return c.workloads.Events(ctx, namespace, name)
}

func (c *Cluster) Logs(ctx context.Context, namespace, pod, container string, lines int64) (string, error) {
	return c.workloads.Logs(ctx, namespace, pod, container, lines)
}

// Status reads one Flux object, for the diagnosis agent.
func (c *Cluster) Status(ctx context.Context, kind, namespace, name string) (domain.ObjectHealth, error) {
	return c.flux.Object(ctx, kind, namespace, name)
}

// Outcome is the result of one watch window.
type Outcome struct {
	Result   domain.WatchResult
	Failures []domain.ObjectHealth
	// Pending is what was still reconciling when a stalled window expired.
	Pending []domain.ObjectHealth
	Final   domain.HealthSnapshot
}

// Watch waits for Flux to fetch revision, then for the cluster to settle.
//
// It returns pass only when the source carries the expected revision, nothing
// this merge could be answerable for is still reconciling, no object newly
// broke, and that has held for the stability hold. A new failure once the
// revision has landed ends the window immediately; running out of time with work
// still in flight is stalled, not failed.
func (c *Cluster) Watch(
	ctx context.Context,
	baseline domain.HealthSnapshot,
	revision string,
) (Outcome, error) {
	started := c.options.Now()
	deadline := started.Add(c.options.SettleTimeout)
	var stableSince time.Time
	var firstSource string
	var sawSource bool
	failingSince := map[string]time.Time{}
	for {
		fetched, source, err := c.fetched(ctx, revision)
		if err != nil {
			return Outcome{}, err
		}
		if !sawSource {
			firstSource, sawSource = source, true
		}
		snapshot, err := c.Snapshot(ctx)
		if err != nil {
			return Outcome{}, err
		}
		var unconfirmed []domain.ObjectHealth
		// Reported on every poll, including before the revision lands, so a
		// caller can say what the window is waiting on.
		if c.options.Observe != nil {
			c.options.Observe(Status{
				Elapsed:     c.options.Now().Sub(started),
				Fetched:     fetched,
				Reconciling: newlyReconciling(snapshot, baseline),
				Settled:     snapshot.SettledSince(baseline),
			})
		}
		// Blaming the merge before its revision is on the cluster is a misattribution.
		if fetched {
			confirmed, pending := c.persistent(snapshot.NewFailures(baseline), failingSince)
			if len(confirmed) > 0 {
				return Outcome{Result: domain.WatchFail, Failures: confirmed, Final: snapshot}, nil
			}
			unconfirmed = pending
		}
		switch {
		case !fetched || !snapshot.SettledSince(baseline) || len(unconfirmed) > 0:
			stableSince = time.Time{}
		case stableSince.IsZero():
			stableSince = c.options.Now()
		case c.options.Now().Sub(stableSince) >= c.options.StabilityHold:
			return Outcome{Result: domain.WatchPass, Final: snapshot}, nil
		}
		// A stability hold already under way gets the rest of that hold to finish,
		// exactly as Restored gives a recovery already under way. Only a cluster
		// that never settled, or that unsettled again and reset the clock, runs out
		// of time - so the grace is bounded by a single hold.
		limit := deadline
		if !stableSince.IsZero() {
			limit = stableSince.Add(c.options.StabilityHold)
		}
		if !c.options.Now().Before(limit) {
			// A window that never saw its own revision has observed nothing. Reported
			// as a stall it reaches repair with no objects, and a merge nobody ever
			// watched is recorded as healthy. Only a source that never moved at all
			// proves that: a commit landing after this merge makes Flux fetch a
			// descendant, which carries this merge without equalling it.
			if !fetched && source == firstSource {
				return Outcome{}, fmt.Errorf(
					"Flux did not fetch %s within %s, so this merge was never applied",
					revision, c.options.SettleTimeout)
			}
			return Outcome{
				Result:  domain.WatchStalled,
				Pending: append(newlyReconciling(snapshot, baseline), unconfirmed...),
				Final:   snapshot,
			}, nil
		}
		if err := c.options.Sleep(ctx, c.options.PollInterval); err != nil {
			return Outcome{}, err
		}
	}
}

// persistent splits the failures into the ones that have lasted the stability
// hold and the ones first seen too recently to judge, and forgets whatever
// recovered. A workload is briefly unready for ordinary reasons - a rollout
// replacing its last pod, a probe between restarts - and reporting that the
// instant it is seen discards a healthy update for it.
func (c *Cluster) persistent(
	failures []domain.ObjectHealth,
	since map[string]time.Time,
) (confirmed, pending []domain.ObjectHealth) {
	now := c.options.Now()
	seen := make(map[string]bool, len(failures))
	for _, failure := range failures {
		key := failure.Ref.String()
		seen[key] = true
		if _, known := since[key]; !known {
			since[key] = now
		}
		if now.Sub(since[key]) >= c.options.StabilityHold {
			confirmed = append(confirmed, failure)
			continue
		}
		pending = append(pending, failure)
	}
	for key := range since {
		if !seen[key] {
			delete(since, key)
		}
	}
	return confirmed, pending
}

// Broken reads the named objects once and returns those that are still not
// healthy, so a caller about to discard a merge can check whether the cluster
// recovered while it was deciding.
//
// An empty list answers empty, and that is the intended answer rather than an
// absence of one: it means the window found no object this merge is answerable
// for, so there is nothing to revert and nothing a wider read could add. Every
// window that does know of breakage names it - a failure carries Failures, a
// stall carries Pending, and a window that never saw its revision errors instead
// of returning at all - so an empty list here is a verdict, not a gap.
func (c *Cluster) Broken(ctx context.Context, objects []domain.ObjectHealth) ([]domain.ObjectHealth, error) {
	if len(objects) == 0 {
		return nil, nil
	}
	snapshot, err := c.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	return stillBroken(snapshot, objects), nil
}

// fetched reports whether the Flux source carries the expected revision, and
// which revision it actually carries. An empty revision means the caller does
// not care which revision is applied, which is what the post-revert heal check
// wants.
func (c *Cluster) fetched(ctx context.Context, revision string) (bool, string, error) {
	if revision == "" {
		return true, "", nil
	}
	current, err := c.flux.SourceRevision(ctx)
	if err != nil {
		return false, "", err
	}
	return sameRevision(current, revision), current, nil
}

func sameRevision(current, want string) bool {
	if current == "" || want == "" {
		return false
	}
	current, want = strings.ToLower(current), strings.ToLower(want)
	return strings.HasPrefix(current, want) || strings.HasPrefix(want, current)
}

// newlyReconciling drops objects that were already unsettled before the merge.
// A stalled window otherwise blames whichever pull request happens to be in
// flight for churn that predates it, exactly as the failure path already avoids.
func newlyReconciling(snapshot, baseline domain.HealthSnapshot) []domain.ObjectHealth {
	var pending []domain.ObjectHealth
	for _, object := range reconciling(snapshot) {
		if !baseline.Attributable(object.Ref.String()) {
			continue
		}
		pending = append(pending, object)
	}
	return pending
}

func reconciling(snapshot domain.HealthSnapshot) []domain.ObjectHealth {
	var pending []domain.ObjectHealth
	for _, object := range snapshot.Objects {
		if object.Reconciling {
			pending = append(pending, object)
		}
	}
	sort.Slice(pending, func(i, j int) bool {
		return pending[i].Ref.String() < pending[j].Ref.String()
	})
	return pending
}

// Restored waits for the objects a merge broke to become healthy again, which
// is what a revert has to achieve before the run moves on.
//
// It deliberately asks about those objects rather than about the whole cluster.
// A GitOps repository reconciles continuously - an unrelated merge, a routine
// interval, an operator's own change - so requiring global quiescence would
// blame the revert for churn it did not cause.
func (c *Cluster) Restored(
	ctx context.Context,
	baseline domain.HealthSnapshot,
	broken []domain.ObjectHealth,
) (Outcome, error) {
	if len(broken) == 0 {
		return c.Watch(ctx, baseline, "")
	}
	started := c.options.Now()
	deadline := started.Add(c.options.SettleTimeout)
	var healthySince time.Time
	for {
		readStarted := c.options.Now()
		snapshot, err := c.Snapshot(ctx)
		if err != nil {
			return Outcome{}, err
		}
		// The settle budget bounds the cluster's time to recover, not the time a
		// throttled apiserver holds up a poll's reads.
		deadline = deadline.Add(c.options.Now().Sub(readStarted))
		pending := stillBroken(snapshot, broken)
		if c.options.Observe != nil {
			c.options.Observe(Status{
				Elapsed:     c.options.Now().Sub(started),
				Fetched:     true,
				Reconciling: pending,
				Settled:     len(pending) == 0,
			})
		}
		switch {
		case len(pending) > 0:
			healthySince = time.Time{}
		case healthySince.IsZero():
			healthySince = c.options.Now()
		case c.options.Now().Sub(healthySince) >= c.options.StabilityHold:
			return Outcome{Result: domain.WatchPass, Final: snapshot}, nil
		}
		// A recovery already under way gets the rest of its stability hold to
		// confirm. Only one that never began, or that broke again and reset the
		// clock, runs out of time - so the grace is bounded by a single hold.
		limit := deadline
		if !healthySince.IsZero() {
			limit = healthySince.Add(c.options.StabilityHold)
		}
		if !c.options.Now().Before(limit) {
			return Outcome{Result: domain.WatchStalled, Pending: pending, Final: snapshot},
				fmt.Errorf("%s did not recover after the revert", describeRefs(pending))
		}
		if err := c.options.Sleep(ctx, c.options.PollInterval); err != nil {
			return Outcome{}, err
		}
	}
}

// stillBroken returns the named objects that are not yet healthy and settled.
func stillBroken(snapshot domain.HealthSnapshot, broken []domain.ObjectHealth) []domain.ObjectHealth {
	var pending []domain.ObjectHealth
	for _, object := range broken {
		current, present := snapshot.Objects[object.Ref.String()]
		if !present {
			pending = append(pending, object)
			continue
		}
		if !current.Healthy || current.Reconciling {
			pending = append(pending, current)
		}
	}
	return pending
}

func describeRefs(objects []domain.ObjectHealth) string {
	names := make([]string, 0, len(objects))
	for _, object := range objects {
		names = append(names, object.Ref.String())
	}
	if len(names) == 0 {
		return "the cluster"
	}
	return strings.Join(names, ", ")
}
