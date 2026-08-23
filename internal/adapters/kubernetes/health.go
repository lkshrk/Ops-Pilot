package kubernetes

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/lkshrk/ops-pilot/internal/domain"
)

// Objects returns the health of every Deployment, StatefulSet and DaemonSet.
// A workload whose controller has not yet observed the current generation is
// reconciling rather than unhealthy.
func (c *Client) Objects(ctx context.Context) ([]domain.ObjectHealth, error) {
	deployments, err := c.ListDeployments(ctx)
	if err != nil {
		return nil, fmt.Errorf("list Deployments: %w", err)
	}
	statefulSets, err := c.ListStatefulSets(ctx)
	if err != nil {
		return nil, fmt.Errorf("list StatefulSets: %w", err)
	}
	daemonSets, err := c.ListDaemonSets(ctx)
	if err != nil {
		return nil, fmt.Errorf("list DaemonSets: %w", err)
	}
	namespaces := make([]string, 0, len(statefulSets)+len(daemonSets))
	for _, item := range statefulSets {
		namespaces = append(namespaces, item.Namespace)
	}
	for _, item := range daemonSets {
		namespaces = append(namespaces, item.Namespace)
	}
	delivering, err := c.deliveringPods(ctx, namespaces)
	if err != nil {
		return nil, err
	}

	result := make([]domain.ObjectHealth, 0, len(deployments)+len(statefulSets)+len(daemonSets))
	for _, item := range deployments {
		result = append(result, workloadHealth(rollout{
			kind: "Deployment", namespace: item.Namespace, name: item.Name,
			generation: item.Generation, observed: item.Status.ObservedGeneration,
			desired: desiredReplicas(item.Spec.Replicas), ready: item.Status.ReadyReplicas,
			updated: item.Status.UpdatedReplicas,
			phase:   deploymentPhase(item.Status.Conditions),
		}))
	}
	for _, item := range statefulSets {
		result = append(result, workloadHealth(rollout{
			kind: "StatefulSet", namespace: item.Namespace, name: item.Name,
			generation: item.Generation, observed: item.Status.ObservedGeneration,
			desired: desiredReplicas(item.Spec.Replicas), ready: item.Status.ReadyReplicas,
			updated:  item.Status.UpdatedReplicas,
			phase:    statefulSetPhase(item.Status),
			starting: delivering[item.UID],
		}))
	}
	for _, item := range daemonSets {
		// A DaemonSet publishes neither a revision nor a condition, so nothing in
		// its status separates the tail of a rolling update from a node that has
		// been degraded for hours. Only its pods do.
		result = append(result, workloadHealth(rollout{
			kind: "DaemonSet", namespace: item.Namespace, name: item.Name,
			generation: item.Generation, observed: item.Status.ObservedGeneration,
			desired: item.Status.DesiredNumberScheduled, ready: item.Status.NumberReady,
			updated:  item.Status.UpdatedNumberScheduled,
			starting: delivering[item.UID],
		}))
	}
	return result, nil
}

// rolloutPhase is a controller's own verdict on its rollout, in the terms every
// kind can be reduced to.
type rolloutPhase int

const (
	// phaseUnknown means the controller published no verdict at all, so only the
	// replica counts and the pods are left to decide on.
	phaseUnknown rolloutPhase = iota
	// phaseRunning means the controller says pods are still being delivered,
	// against a progress deadline of its own that turns into phaseStopped. Only a
	// Deployment publishes one, so it is the only verdict trusted on its own.
	phaseRunning
	// phaseRolling means the controller is replacing pods but publishes no
	// deadline, so it cannot hold the benefit of the doubt by itself.
	phaseRolling
	// phaseFinished means the controller says the rollout converged, so whatever
	// the workload is still short of is not delivery in flight.
	phaseFinished
	// phaseStopped means the controller gave up against its own deadline.
	phaseStopped
)

// rollout is what one workload's status says about whether it is still
// delivering pods or has stopped short.
type rollout struct {
	kind, namespace, name string
	generation, observed  int64
	desired, ready        int32
	// updated is how many pods are already on the current revision. While it is
	// short of desired the controller is still replacing pods.
	updated int32
	phase   rolloutPhase
	// starting is whether any of the workload's own pods is still being placed.
	// It is what bounds the benefit of the doubt for the kinds whose controller
	// publishes no deadline.
	starting bool
}

// progressing reports whether pods are still being delivered. A rollout that has
// already created every pod but has none of them ready yet is still in flight,
// and reading that as broken blames a merge for an ordinary restart - but the
// benefit of the doubt has to run out, because an object that reconciles forever
// denies every later merge the pass path.
func (r rollout) progressing() bool {
	switch r.phase {
	case phaseStopped, phaseFinished:
		return false
	case phaseRunning:
		return true
	}
	return r.starting || r.updated < r.desired
}

// workloadHealth decides whether a workload that is not fully ready is still
// rolling out or has stopped short.
//
// The distinction matters because a stuck workload otherwise reads as
// permanently reconciling: the watch never settles, every window times out, and
// the object is reported against whichever merge happens to be in flight.
func workloadHealth(r rollout) domain.ObjectHealth {
	result := domain.ObjectHealth{
		Ref: domain.ObjectRef{Kind: r.kind, Namespace: r.namespace, Name: r.name},
	}
	if r.generation > 0 && r.generation != r.observed {
		result.Healthy, result.Reconciling = true, true
		result.Reason = "generation not yet observed"
		return result
	}
	if r.desired == 0 || r.ready >= r.desired {
		result.Healthy = true
		return result
	}
	if r.progressing() {
		result.Healthy, result.Reconciling = true, true
		result.Reason = fmt.Sprintf("rolling out, %d/%d replicas ready", r.ready, r.desired)
		return result
	}
	result.Reason = fmt.Sprintf("%d/%d replicas ready", r.ready, r.desired)
	switch r.phase {
	case phaseStopped:
		result.Reason += ", rollout stopped making progress"
	case phaseRolling:
		result.Reason += ", rollout to the new revision has not converged"
	}
	return result
}

// rolloutComplete is the reason Kubernetes gives the Progressing condition once
// a rollout has finished. The condition stays True afterwards, so the status
// alone cannot distinguish a rollout still in flight from one that completed and
// later lost a replica; only the reason can.
const rolloutComplete = "NewReplicaSetAvailable"

// deploymentPhase reads a Deployment's own verdict on its rollout: False means
// it stopped advancing (ProgressDeadlineExceeded), and True means it is running
// unless the reason says it already finished.
func deploymentPhase(conditions []appsv1.DeploymentCondition) rolloutPhase {
	for _, condition := range conditions {
		if condition.Type != appsv1.DeploymentProgressing {
			continue
		}
		if condition.Status != corev1.ConditionTrue {
			return phaseStopped
		}
		if condition.Reason == rolloutComplete {
			return phaseFinished
		}
		return phaseRunning
	}
	return phaseUnknown
}

// statefulSetPhase reads the revisions, the only thing a StatefulSet publishes
// about its rollout: the controller advances currentRevision to updateRevision
// exactly when every pod is both updated and ready, so while they differ the
// rollout has not converged. Equal revisions say nothing about a scale-up, which
// delivers pods without a template change, so they leave the verdict to the
// counts. Neither answer decides on its own - a StatefulSet stuck on a probe
// that never passes keeps them apart for good - so this only names the state a
// report has to explain.
func statefulSetPhase(status appsv1.StatefulSetStatus) rolloutPhase {
	if status.UpdateRevision == "" || status.CurrentRevision == status.UpdateRevision {
		return phaseUnknown
	}
	return phaseRolling
}

// RolloutGrace is how long a pod that is not ready yet is read as still starting
// up. Every kind but Deployment publishes no deadline of its own, so without a
// bound here a workload that never comes up reads as reconciling for good - and
// one that turns forever-reconciling after a window's baseline was healthy and
// settled in it, so it holds that window open to settleTimeout and is never named
// as breakage. It has to stay well inside settleTimeout, or breakage can only
// ever be reported as a stall.
//
// It is exported because internal/config computes the minimum settleTimeout from
// it and must not import this package: the two are held equal by
// TestTheSettleTimeoutFloorUsesTheSameRolloutGraceAsTheHealthAdapter.
const RolloutGrace = 3 * time.Minute

// deliveringPods answers deliveringOwners for the workloads that consult it. One
// cluster-wide list answers every namespace at once and is tried first, because
// this runs on every poll of a watch and any error aborts that watch: splitting
// it per namespace unconditionally would multiply the exposure to independent
// transient failures by the namespace count, and buys nothing until the pod
// count passes the collection limit.
//
// The narrower reads are the fallback for when it cannot succeed. Any error
// takes that path rather than only the collection limit, because ListPods
// reports the limit as a bare error indistinguishable from a transport failure
// without matching on its text - and re-reading is the right answer to both. The
// page returned alongside that error is discarded rather than used: it is
// truncated, and a workload whose pods fell past the cut would lose its starting
// flag and read as stuck at the tail of an ordinary rollout.
//
// A failure in the fallback is reported together with the cluster-wide failure
// that caused it to run at all, because the two are read very differently: the
// namespace one alone looks like a blip, while the pair names the systematic
// cause an operator can act on.
//
// Taking any error also means namespace-scoped pod RBAC is supported: with no
// ClusterRole to list pods across the cluster, every poll's first read is
// Forbidden and the fallback carries the verdict, correctly, from exactly the
// namespaces that matter. What it costs is one list per such namespace on every
// poll, permanently - twelve namespaces is thirteen reads a poll, 780 over a
// ten-minute watch - and thirteen independent chances of the transient failure
// that aborts the watch, where a cluster-wide list has one.
func (c *Client) deliveringPods(ctx context.Context, namespaces []string) (map[types.UID]bool, error) {
	delivering := map[types.UID]bool{}
	if len(namespaces) == 0 {
		return delivering, nil
	}
	now := time.Now()
	pods, wide := c.ListPods(ctx, metav1.NamespaceAll)
	if wide == nil {
		return deliveringOwners(pods, now), nil
	}
	reason := degradedBecause(wide)
	if c.degraded != nil {
		c.degraded(reason)
	}
	read := map[string]bool{}
	for _, namespace := range namespaces {
		if read[namespace] {
			continue
		}
		read[namespace] = true
		pods, err := c.ListPods(ctx, namespace)
		if err != nil {
			return nil, fmt.Errorf("%s: %w; and list Pods in %s: %w",
				reason, wide, namespace, err)
		}
		for owner := range deliveringOwners(pods, now) {
			delivering[owner] = true
		}
	}
	return delivering, nil
}

// degradedBecause names why pods are being read one namespace at a time.
// Forbidden is worth separating from every other error because it is the only
// one that says the cluster-wide read will never succeed: a retry resolves a
// transport failure and a shrinking cluster resolves the collection limit, but
// namespace-scoped RBAC holds the degraded regime open until an operator changes
// it, and an operator told only "failed" will wait for it to clear.
func degradedBecause(err error) string {
	if apierrors.IsForbidden(err) {
		return "cluster-wide list Pods is not permitted, so pods are read one namespace at a time on every poll"
	}
	return "cluster-wide list Pods failed, so pods are read one namespace at a time"
}

// deliveringOwners reports, per controlling owner, whether any of its pods is
// still being placed: one created too recently to have passed its probes, or one
// on its way out while it is replaced. Both are bounded by RolloutGrace, because
// a pod the controller has left unready for that long is not starting up, it is
// stuck - which is exactly what an image bump that breaks a probe looks like.
func deliveringOwners(pods []corev1.Pod, now time.Time) map[types.UID]bool {
	owners := map[types.UID]bool{}
	for i := range pods {
		pod := &pods[i]
		owner := metav1.GetControllerOf(pod)
		if owner == nil || !beingPlaced(pod, now) {
			continue
		}
		owners[owner.UID] = true
	}
	return owners
}

func beingPlaced(pod *corev1.Pod, now time.Time) bool {
	if pod.DeletionTimestamp != nil {
		return now.Sub(pod.DeletionTimestamp.Time) < RolloutGrace
	}
	if podReady(pod) {
		return false
	}
	return now.Sub(pod.CreationTimestamp.Time) < RolloutGrace
}

// podReady reads the Ready condition rather than the container statuses, since
// that is what the controllers count their ready replicas from.
func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// Events renders recent Warning events, newest first. An empty name returns the
// whole namespace.
func (c *Client) Events(ctx context.Context, namespace, name string) (string, error) {
	items, err := c.ListEvents(ctx, namespace)
	if err != nil {
		return "", err
	}
	sort.Slice(items, func(i, j int) bool {
		return observedAt(&items[i]).After(observedAt(&items[j]))
	})
	var traced lineage
	if name != "" {
		traced = c.podLineage(ctx, namespace)
	}
	var out renderBound
	for i := range items {
		item := &items[i]
		if name != "" && !involves(item.InvolvedObject, name, traced) {
			continue
		}
		if item.Type != corev1.EventTypeWarning {
			continue
		}
		out.add(fmt.Sprintf("%s %s/%s %s: %s\n",
			observedAt(item).Format("15:04:05"),
			item.InvolvedObject.Kind, item.InvolvedObject.Name,
			item.Reason, item.Message))
		if out.cut {
			break
		}
	}
	if out.empty() {
		return "no Warning events", nil
	}
	return out.String(), nil
}

// ownerKey identifies one object on the chain from a pod up to the workload
// that created it.
type ownerKey struct{ kind, name string }

// lineage is how a namespace's pods reach the workloads that created them: each
// pod's controller, and the controller of each intermediate object that could be
// read. Absence from parents means the object was not read, not that its chain
// ends - a nil entry is what says it ends.
type lineage struct {
	controllers map[string]metav1.OwnerReference
	parents     map[ownerKey]*metav1.OwnerReference
}

// podLineage traces every pod in a namespace back towards the workload that
// created it. The intermediate kinds a controller inserts - a Deployment's
// ReplicaSet, a CronJob's Job - are read so the chain is followed rather than
// guessed from their names, which carry the same generated-segment ambiguity the
// pod names do. Any failed read degrades to matching on names alone rather than
// failing the caller, who is already asking why something broke.
func (c *Client) podLineage(ctx context.Context, namespace string) lineage {
	traced := lineage{
		controllers: map[string]metav1.OwnerReference{},
		parents:     map[ownerKey]*metav1.OwnerReference{},
	}
	pods, err := c.ListPods(ctx, namespace)
	if err != nil {
		return traced
	}
	for i := range pods {
		if owner := metav1.GetControllerOf(&pods[i]); owner != nil {
			traced.controllers[pods[i].Name] = *owner
		}
	}
	if sets, err := c.client.AppsV1().ReplicaSets(namespace).List(ctx, listOptions()); err == nil && complete(sets.Continue, len(sets.Items), "ReplicaSet") == nil {
		for i := range sets.Items {
			traced.parents[ownerKey{"ReplicaSet", sets.Items[i].Name}] = metav1.GetControllerOf(&sets.Items[i])
		}
	}
	if jobs, err := c.client.BatchV1().Jobs(namespace).List(ctx, listOptions()); err == nil && complete(jobs.Continue, len(jobs.Items), "Job") == nil {
		for i := range jobs.Items {
			traced.parents[ownerKey{"Job", jobs.Items[i].Name}] = metav1.GetControllerOf(&jobs.Items[i])
		}
	}
	return traced
}

// involves reports whether an event is about the named workload. Neither a
// substring nor a prefix can decide that, because a sibling's name extends it -
// "sonarr-exporter" starts with "sonarr-" too - and for a pod no rule over the
// name can, since a StatefulSet and a DaemonSet add exactly as many generated
// segments to their pod names as the sibling's name contributes. So a pod is
// attributed by its controller reference, which names its creator outright, and
// a pod whose controller cannot answer - one already gone, or one created by a
// kind that names no workload - falls back to the shape of its name: a template
// hash and a random tail, or a stateful ordinal. Everything else is decided by
// the involved object's kind - a ReplicaSet adds only the hash, the rest must
// match exactly.
func involves(involved corev1.ObjectReference, name string, traced lineage) bool {
	if involved.Name == name {
		return true
	}
	if involved.Kind == "PersistentVolumeClaim" {
		return ordinalClaim(involved.Name, name)
	}
	if involved.Kind == "Pod" {
		if owner, controlled := traced.controllers[involved.Name]; controlled {
			if matched, decided := createdBy(owner, name, traced.parents); decided {
				return matched
			}
		}
	}
	suffix, generated := strings.CutPrefix(involved.Name, name+"-")
	if !generated {
		return false
	}
	switch involved.Kind {
	case "Pod":
		return strings.Count(suffix, "-") < 2
	case "ReplicaSet":
		return !strings.Contains(suffix, "-")
	}
	return false
}

// createdBy reports whether a pod's controller chain reaches the named workload,
// and whether the chain was complete enough to decide. A Deployment and a CronJob
// own their pods one level down, so the chain is walked rather than guessed from
// the intermediate object's name: an indexed Job "sonarr-migrate" has exactly the
// shape of a ReplicaSet of Deployment "sonarr", and only the Job itself says
// which. Only a kind that creates its pods itself settles a mismatch as a no,
// because its own name is the workload's; a Node holding a static pod, a custom
// runner, or an intermediate that could not be read names nothing that answers
// the question, so the caller falls back to the pod's name.
func createdBy(owner metav1.OwnerReference, name string, parents map[ownerKey]*metav1.OwnerReference) (matched, decided bool) {
	seen := map[ownerKey]bool{}
	for {
		if owner.Name == name {
			return true, true
		}
		key := ownerKey{owner.Kind, owner.Name}
		if seen[key] {
			return false, true
		}
		seen[key] = true
		parent, read := parents[key]
		if !read {
			return false, namesAWorkload(owner.Kind)
		}
		if parent == nil {
			return false, true
		}
		owner = *parent
	}
}

// The kinds whose objects are workloads in their own right, so a pod they
// created belongs to the workload their name gives and to no other.
func namesAWorkload(kind string) bool {
	switch kind {
	case "Deployment", "StatefulSet", "DaemonSet", "ReplicationController", "CronJob":
		return true
	}
	return false
}

// A StatefulSet's volumeClaimTemplates produce claims named
// <template>-<set>-<ordinal>, so the workload's name sits in the middle rather
// than at the front.
func ordinalClaim(claim, name string) bool {
	_, ordinal, found := strings.Cut(claim, "-"+name+"-")
	if !found || ordinal == "" {
		return false
	}
	return strings.IndexFunc(ordinal, func(r rune) bool { return r < '0' || r > '9' }) < 0
}

// observedAt is when an event was last seen. No single field carries that for
// every writer: events.k8s.io/v1 records eventTime and a series' lastObservedTime
// and leaves the core API's lastTimestamp empty, so the latest of them wins and
// creationTimestamp - which the API server always sets - is the floor.
func observedAt(event *corev1.Event) time.Time {
	latest := event.CreationTimestamp.Time
	candidates := []time.Time{
		event.LastTimestamp.Time,
		event.EventTime.Time,
		event.FirstTimestamp.Time,
	}
	if event.Series != nil {
		candidates = append(candidates, event.Series.LastObservedTime.Time)
	}
	for _, candidate := range candidates {
		if candidate.After(latest) {
			latest = candidate
		}
	}
	return latest
}

// Logs returns a container's recent output. Pod names carry a generated suffix,
// so a caller that knows only the workload name is resolved to a matching pod
// rather than told the pod does not exist.
func (c *Client) Logs(ctx context.Context, namespace, pod, container string, lines int64) (string, error) {
	name, err := c.resolvePod(ctx, namespace, pod)
	if err != nil {
		return "", err
	}
	return c.PodLogs(ctx, namespace, name, container, lines)
}

// resolvePod accepts an exact pod name or the workload name it was generated
// from, preferring a pod that is not running cleanly since that is the one a
// diagnosis wants.
func (c *Client) resolvePod(ctx context.Context, namespace, name string) (string, error) {
	pods, err := c.ListPods(ctx, namespace)
	if err != nil {
		return "", err
	}
	var candidates []corev1.Pod
	for _, pod := range pods {
		if pod.Name == name {
			return pod.Name, nil
		}
		if strings.HasPrefix(pod.Name, name+"-") {
			candidates = append(candidates, pod)
		}
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("no pod in %s is named %q or generated from it", namespace, name)
	}
	for _, pod := range candidates {
		if !allReady(pod) {
			return pod.Name, nil
		}
	}
	return candidates[0].Name, nil
}

// A pod the kubelet has not started yet reports no container statuses at all, so
// an empty list is the unschedulable and image-pull-stuck case rather than a
// clean one.
func allReady(pod corev1.Pod) bool {
	if len(pod.Status.ContainerStatuses) == 0 {
		return false
	}
	for _, status := range pod.Status.ContainerStatuses {
		if !status.Ready {
			return false
		}
	}
	return true
}

// Pods describes every pod in a namespace: what it is running, whether it is
// ready, and why not. Without this the agent has to guess names that carry a
// generated suffix, and a failed guess reads as "the workload does not exist".
func (c *Client) Pods(ctx context.Context, namespace string) (string, error) {
	pods, err := c.ListPods(ctx, namespace)
	if err != nil {
		return "", err
	}
	if len(pods) == 0 {
		return fmt.Sprintf("no pods in %s", namespace), nil
	}
	sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
	var out renderBound
	for i := range pods {
		pod := &pods[i]
		ready, total, restarts := 0, len(pod.Spec.Containers), int32(0)
		reason := string(pod.Status.Phase)
		for _, status := range pod.Status.ContainerStatuses {
			if status.Ready {
				ready++
			}
			restarts += status.RestartCount
			if status.State.Waiting != nil {
				reason = status.State.Waiting.Reason
			}
		}
		var record strings.Builder
		fmt.Fprintf(&record, "%s %d/%d %s restarts=%d age=%s\n",
			pod.Name, ready, total, reason, restarts,
			duration(pod.Status.StartTime))
		for _, container := range pod.Spec.Containers {
			fmt.Fprintf(&record, "    %s %s\n", container.Name, container.Image)
		}
		out.add(record.String())
		if out.cut {
			break
		}
	}
	return out.String(), nil
}

func duration(start *metav1.Time) string {
	if start == nil {
		return "unknown"
	}
	return time.Since(start.Time).Round(time.Second).String()
}

const (
	maxRenderedBytes = 16 << 10
	truncationMarker = "\n... [truncated]"
)

// renderBound accumulates a bounded render one whole record at a time. The cut
// falls between records, never inside one: an event message may itself span
// lines, and a record kept in half can be a PEM block whose END marker was
// dropped, which boundPrivateKeyBlocks then leaves unscrubbed.
type renderBound struct {
	text strings.Builder
	kept int
	cut  bool
}

func (b *renderBound) add(record string) {
	if splitsPrivateKeyBlock(record) {
		b.cut = true
		return
	}
	b.text.WriteString(record)
	if b.text.Len() > maxRenderedBytes {
		b.cut = true
		return
	}
	b.kept = b.text.Len()
}

// The markers must track diagnostics.boundPrivateKeyBlocks: a record it treats
// as atomic that opens a key block without closing it can survive a cut as a lone
// BEGIN, which that scrubber leaves unredacted.
var (
	pemPrivateBegin = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)
	pemPrivateEnd   = regexp.MustCompile(`-----END [A-Z0-9 ]*PRIVATE KEY-----`)
)

// splitsPrivateKeyBlock reports whether a record is a fragment of a private-key
// block rather than a whole record, so add() drops it before an unterminated
// BEGIN can reach the cut boundary. Counting markers is not enough: a chunk
// whose END precedes an unterminated BEGIN balances by count yet still leaks, so
// the markers are paired in order the way boundPrivateKeyBlocks reads them - a
// BEGIN must be closed by a later END, with no BEGIN open at the end and no END
// before its BEGIN.
func splitsPrivateKeyBlock(record string) bool {
	if !strings.Contains(record, "PRIVATE KEY-----") {
		return false
	}
	begins := pemPrivateBegin.FindAllStringIndex(record, -1)
	ends := pemPrivateEnd.FindAllStringIndex(record, -1)
	i, j := 0, 0
	open := false
	for i < len(begins) || j < len(ends) {
		if i < len(begins) && (j == len(ends) || begins[i][0] < ends[j][0]) {
			if open {
				return true
			}
			open = true
			i++
			continue
		}
		if !open {
			return true
		}
		open = false
		j++
	}
	return open
}

func (b *renderBound) empty() bool { return b.text.Len() == 0 }

func (b *renderBound) String() string {
	if !b.cut {
		return b.text.String()
	}
	return b.text.String()[:b.kept] + truncationMarker
}
