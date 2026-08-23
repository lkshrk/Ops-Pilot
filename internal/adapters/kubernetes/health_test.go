package kubernetes

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/lkshrk/ops-pilot/internal/domain"
)

// A workload whose controller has seen the current generation, has every pod on
// the new revision, and still has no ready replicas is broken. Reading it as
// reconciling would hold every watch open forever and report it against
// whichever merge happened to be in flight.
func TestStuckWorkloadIsBrokenRatherThanReconciling(t *testing.T) {
	got := workloadHealth(rollout{
		kind: "Deployment", namespace: "productivity", name: "karakeep-meilisearch",
		generation: 29, observed: 29, desired: 1, ready: 0, updated: 1,
	})
	if got.Healthy || got.Reconciling {
		t.Fatalf("want a stuck workload reported as broken, got %+v", got)
	}
}

func TestRolloutStillReplacingPodsIsReconciling(t *testing.T) {
	got := workloadHealth(rollout{
		kind: "Deployment", namespace: "media", name: "sonarr",
		generation: 4, observed: 4, desired: 3, ready: 1, updated: 2,
	})
	if !got.Reconciling || !got.Healthy {
		t.Fatalf("want a rollout in progress held open, got %+v", got)
	}
}

// Kubernetes publishes its own verdict once a rollout stops advancing.
func TestProgressDeadlineExceededIsBroken(t *testing.T) {
	got := workloadHealth(rollout{
		kind: "Deployment", namespace: "media", name: "sonarr",
		generation: 4, observed: 4, desired: 3, ready: 1, updated: 2,
		phase: phaseStopped,
	})
	if got.Healthy || got.Reconciling {
		t.Fatalf("want a stalled rollout reported as broken, got %+v", got)
	}
}

// Every pod is already on the new revision but none is ready yet, which is what
// a one-replica Deployment looks like the moment its replacement starts.
func TestReplacedButNotYetReadyRolloutIsReconciling(t *testing.T) {
	got := workloadHealth(rollout{
		kind: "Deployment", namespace: "media", name: "sonarr",
		generation: 4, observed: 4, desired: 1, ready: 0, updated: 1,
		phase: phaseRunning,
	})
	if !got.Reconciling || !got.Healthy {
		t.Fatalf("want a rollout in progress held open, got %+v", got)
	}
}

// Kubernetes leaves Progressing=True after a rollout finishes, so a Deployment
// that completed and then lost a replica must not read as still rolling out.
func TestCompletedRolloutThatLostAReplicaIsBroken(t *testing.T) {
	got := workloadHealth(rollout{
		kind: "Deployment", namespace: "media", name: "sonarr",
		generation: 4, observed: 4, desired: 3, ready: 2, updated: 3,
		phase: phaseFinished,
	})
	if got.Healthy || got.Reconciling {
		t.Fatalf("want a degraded Deployment reported as broken, got %+v", got)
	}
}

func TestProgressConditionSeparatesAFinishedRolloutFromARunningOne(t *testing.T) {
	for _, testcase := range []struct {
		name      string
		condition appsv1.DeploymentCondition
		phase     rolloutPhase
	}{
		{
			name:      "still replacing pods",
			condition: appsv1.DeploymentCondition{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue, Reason: "ReplicaSetUpdated"},
			phase:     phaseRunning,
		},
		{
			name:      "rollout finished",
			condition: appsv1.DeploymentCondition{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionTrue, Reason: rolloutComplete},
			phase:     phaseFinished,
		},
		{
			name:      "progress deadline exceeded",
			condition: appsv1.DeploymentCondition{Type: appsv1.DeploymentProgressing, Status: corev1.ConditionFalse, Reason: "ProgressDeadlineExceeded"},
			phase:     phaseStopped,
		},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			if got := deploymentPhase([]appsv1.DeploymentCondition{testcase.condition}); got != testcase.phase {
				t.Fatalf("want phase %d, got %d", testcase.phase, got)
			}
		})
	}
}

// Without a Progressing condition there is no verdict to believe, so the replica
// counts decide.
func TestAbsentProgressConditionFallsBackToReplicaCounts(t *testing.T) {
	if got := deploymentPhase(nil); got != phaseUnknown {
		t.Fatalf("want no verdict, got phase %d", got)
	}
}

// A Deployment whose scale-up pods are refused - a ResourceQuota denial, say -
// never advances updatedReplicas, so the controller never relabels Progressing
// away from NewReplicaSetAvailable and DeploymentTimedOut refuses to re-evaluate
// once that reason is set. Letting the counts override the finished verdict
// leaves it reconciling for good.
func TestFinishedDeploymentThatNeverCreatesItsScaleUpPodsIsBroken(t *testing.T) {
	got := workloadHealth(rollout{
		kind: "Deployment", namespace: "media", name: "sonarr",
		generation: 4, observed: 4, desired: 3, ready: 1, updated: 1,
		phase: phaseFinished,
	})
	if got.Healthy || got.Reconciling {
		t.Fatalf("want a Deployment that cannot place its pods reported as broken, got %+v", got)
	}
}

// From the replica counts alone the tail of a rolling update is indistinguishable
// from a stuck workload: every pod is on the new revision and none is ready yet.
// The pod's own age separates them, and the grace has to expire, because a
// StatefulSet whose new revision never passes its probe is exactly what a broken
// image bump looks like.
func TestStatefulSetRolloutProgressIsBoundedByItsPods(t *testing.T) {
	for _, testcase := range []struct {
		name        string
		replicas    int32
		status      appsv1.StatefulSetStatus
		pods        []*corev1.Pod
		reconciling bool
	}{
		{
			name:     "last pod replaced and not ready yet",
			replicas: 1,
			status: appsv1.StatefulSetStatus{
				ObservedGeneration: 7, ReadyReplicas: 0, UpdatedReplicas: 1,
				CurrentRevision: "postgres-6f4", UpdateRevision: "postgres-9ab",
			},
			pods:        []*corev1.Pod{workloadPod("media", "postgres-0", "StatefulSet", "sts", false, 20*time.Second)},
			reconciling: true,
		},
		{
			name:     "new revision never passes its probe",
			replicas: 1,
			status: appsv1.StatefulSetStatus{
				ObservedGeneration: 7, ReadyReplicas: 0, UpdatedReplicas: 1,
				CurrentRevision: "postgres-6f4", UpdateRevision: "postgres-9ab",
			},
			pods:        []*corev1.Pod{workloadPod("media", "postgres-0", "StatefulSet", "sts", false, time.Hour)},
			reconciling: false,
		},
		{
			name:     "rollout converged and a pod has since gone unready",
			replicas: 3,
			status: appsv1.StatefulSetStatus{
				ObservedGeneration: 7, ReadyReplicas: 2, UpdatedReplicas: 3,
				CurrentRevision: "postgres-9ab", UpdateRevision: "postgres-9ab",
			},
			pods:        []*corev1.Pod{workloadPod("media", "postgres-2", "StatefulSet", "sts", false, time.Hour)},
			reconciling: false,
		},
		{
			name:     "scaling up with no revision change",
			replicas: 3,
			status: appsv1.StatefulSetStatus{
				ObservedGeneration: 7, ReadyReplicas: 2, UpdatedReplicas: 2,
				CurrentRevision: "postgres-9ab", UpdateRevision: "postgres-9ab",
			},
			reconciling: true,
		},
		{
			name:     "no revision published yet",
			replicas: 3,
			status: appsv1.StatefulSetStatus{
				ObservedGeneration: 7, ReadyReplicas: 1, UpdatedReplicas: 2,
			},
			reconciling: true,
		},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			replicas := testcase.replicas
			objects := []runtime.Object{&appsv1.StatefulSet{
				ObjectMeta: metav1.ObjectMeta{Namespace: "media", Name: "postgres", Generation: 7, UID: "sts"},
				Spec:       appsv1.StatefulSetSpec{Replicas: &replicas},
				Status:     testcase.status,
			}}
			for _, item := range testcase.pods {
				objects = append(objects, item)
			}
			got := onlyObject(t, objects...)
			if got.Reconciling != testcase.reconciling || got.Healthy != testcase.reconciling {
				t.Fatalf("want reconciling=%t, got %+v", testcase.reconciling, got)
			}
		})
	}
}

// A DaemonSet publishes no revision and no condition, so "every node has the new
// pod and the last one is not ready yet" is indistinguishable from breakage in
// its status. Its pods are not: one just created is starting up.
func TestDaemonSetWhoseLastNodeIsNotReadyYetIsReconciling(t *testing.T) {
	got := onlyObject(t,
		degradedDaemonSet(),
		workloadPod("kube-system", "node-exporter-9g5kx", "DaemonSet", "ds", false, 20*time.Second),
	)
	if !got.Reconciling || !got.Healthy {
		t.Fatalf("want a DaemonSet mid-rollout held open, got %+v", got)
	}
}

// One NotReady node or one crash-looping node-exporter leaves a DaemonSet short
// of ready for days. Reading that as reconciling is unbounded, and a DaemonSet
// that goes short after a window's baseline is still attributable to it: it holds
// that window open to settleTimeout and comes back a stall with nothing
// confirmed, rather than the breakage it is.
func TestChronicallyDegradedDaemonSetIsBrokenRatherThanForeverReconciling(t *testing.T) {
	got := onlyObject(t,
		degradedDaemonSet(),
		workloadPod("kube-system", "node-exporter-9g5kx", "DaemonSet", "ds", false, 6*time.Hour),
	)
	if got.Healthy || got.Reconciling {
		t.Fatalf("want a chronically degraded DaemonSet reported as broken, got %+v", got)
	}
}

// A pod on its way out is the controller replacing it, not the workload failing.
func TestWorkloadReplacingAPodIsStillDelivering(t *testing.T) {
	deleted := workloadPod("kube-system", "node-exporter-9g5kx", "DaemonSet", "ds", true, 6*time.Hour)
	deleted.DeletionTimestamp = ptr(metav1.NewTime(time.Now().Add(-5 * time.Second)))
	deleted.Finalizers = []string{"ops-pilot.test/hold"}
	got := onlyObject(t, degradedDaemonSet(), deleted)
	if !got.Reconciling || !got.Healthy {
		t.Fatalf("want a workload mid-replacement held open, got %+v", got)
	}
}

// Snapshot is re-taken every PollInterval for the length of a watch and any
// error there aborts the watch outright, so the pod read stays one request
// wherever one request can answer it. Splitting it per namespace multiplies the
// chances of an independent transient failure by the namespace count, on the
// highest-frequency read in the system, and buys nothing below the collection
// limit.
func TestThePodReadIsOneRequestUntilTheClusterWideListFails(t *testing.T) {
	overTheLimit := func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetNamespace() != metav1.NamespaceAll {
			return false, nil, nil
		}
		return true, &corev1.PodList{ListMeta: metav1.ListMeta{Continue: "next-page"}}, nil
	}
	for _, testcase := range []struct {
		name     string
		objects  []runtime.Object
		truncate bool
		listed   []string
	}{
		{
			name:    "no workload consults pods",
			objects: []runtime.Object{deployment("media", "sonarr")},
			listed:  nil,
		},
		{
			name:    "one request answers the whole cluster",
			objects: []runtime.Object{deployment("media", "sonarr"), degradedDaemonSet(), readyStatefulSet("data", "postgres")},
			listed:  []string{""},
		},
		{
			name:     "past the limit, re-read once per namespace that needs it",
			objects:  []runtime.Object{deployment("media", "sonarr"), readyStatefulSet("data", "postgres"), degradedDaemonSet()},
			truncate: true,
			listed:   []string{"", "data", "kube-system"},
		},
		{
			name:     "past the limit, two workloads sharing a namespace are read once",
			objects:  []runtime.Object{readyStatefulSet("data", "postgres"), readyStatefulSet("data", "redis")},
			truncate: true,
			listed:   []string{"", "data"},
		},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			client := fake.NewClientset(testcase.objects...)
			var listed []string
			client.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
				listed = append(listed, action.GetNamespace())
				if testcase.truncate {
					return overTheLimit(action)
				}
				return false, nil, nil
			})
			c, err := New(client)
			if err != nil {
				t.Fatalf("new: %v", err)
			}
			if _, err := c.Objects(context.Background()); err != nil {
				t.Fatalf("objects: %v", err)
			}
			if !slices.Equal(listed, testcase.listed) {
				t.Fatalf("want pods listed in %q, got %q", testcase.listed, listed)
			}
		})
	}
}

// Past the collection limit a real apiserver returns a full page plus a continue
// token, and ListPods reports that as an error with the truncated page still
// attached. Reading that page instead of re-reading per namespace would blank
// the starting flag for every workload whose pods fell past the cut, and a
// StatefulSet at the tail of an ordinary rolling update would read as stuck -
// the C-H01 false positive, which reverts a merge that was fine.
func TestATruncatedPodPageIsRereadPerNamespaceRatherThanUsed(t *testing.T) {
	client := fake.NewClientset(
		statefulSet("data", "postgres", 1, appsv1.StatefulSetStatus{
			ObservedGeneration: 7, ReadyReplicas: 0, UpdatedReplicas: 1,
			CurrentRevision: "postgres-6f4", UpdateRevision: "postgres-9ab",
		}),
		workloadPod("data", "postgres-0", "StatefulSet", "data/postgres", false, 20*time.Second),
	)
	client.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetNamespace() != metav1.NamespaceAll {
			return false, nil, nil
		}
		return true, &corev1.PodList{ListMeta: metav1.ListMeta{Continue: "next-page"}}, nil
	})
	c, err := New(client)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	got, err := c.Objects(context.Background())
	if err != nil {
		t.Fatalf("objects: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want one object, got %d", len(got))
	}
	if !got[0].Reconciling || !got[0].Healthy {
		t.Fatalf("want the StatefulSet's own pods re-read and its rollout held open, got %+v", got[0])
	}
}

// The per-namespace read only runs because the cluster-wide one failed, so a
// halt that names the namespace alone hands the operator a one-off network fault
// on a pull request comment saying nothing is watching their merge - and never
// mentions that the cluster is systematically past the collection limit, which
// is the half they can act on.
func TestAFailedFallbackNamesTheClusterWideCauseTogetherWithTheNamespaceOne(t *testing.T) {
	client := fake.NewClientset(readyStatefulSet("data", "postgres"))
	client.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetNamespace() == metav1.NamespaceAll {
			return true, &corev1.PodList{ListMeta: metav1.ListMeta{Continue: "next-page"}}, nil
		}
		return true, nil, fmt.Errorf("connection refused")
	})
	c, err := New(client)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	_, err = c.Objects(context.Background())
	if err == nil {
		t.Fatal("want the pod read reported as failed")
	}
	for _, want := range []string{"collection limit exceeded", "connection refused", "data"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("want %q named in the halt, got: %v", want, err)
		}
	}
}

// Remembering "this cluster cannot answer a cluster-wide list" would save the
// wasted first read of every later poll, and must not be added. The fallback
// triggers on any error, not on a detected limit, so a memo cannot tell a
// permanent condition from one blip: a single transient failure would latch the
// client into one list per namespace per poll for the life of the process, which
// is the amplification 20c81b9 exists to remove. Every poll re-probes instead,
// and a cluster that recovers is back to one read on the next one.
func TestTheClusterWidePodReadIsReprobedOnEveryPollRatherThanLatchedOff(t *testing.T) {
	client := fake.NewClientset(readyStatefulSet("data", "postgres"))
	var listed []string
	blip := true
	client.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
		listed = append(listed, action.GetNamespace())
		if action.GetNamespace() == metav1.NamespaceAll && blip {
			return true, nil, fmt.Errorf("connection refused")
		}
		return false, nil, nil
	})
	c, err := New(client)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if _, err := c.Objects(context.Background()); err != nil {
		t.Fatalf("objects during the blip: %v", err)
	}
	blip = false
	if _, err := c.Objects(context.Background()); err != nil {
		t.Fatalf("objects after the blip: %v", err)
	}
	if want := []string{"", "data", ""}; !slices.Equal(listed, want) {
		t.Fatalf("want pods listed in %q, got %q", want, listed)
	}
}

// Namespace-scoped pod RBAC makes the cluster-wide list Forbidden on every poll
// while the namespaced reads all succeed, so the fallback carries the whole
// verdict. That has to keep working - refusing it would halt every such cluster
// forever - and it has to be named when something else fails, because a
// Forbidden is not a blip that retrying resolves: the degraded regime is
// permanent, one list per namespace per poll, until the RBAC changes.
func TestNamespaceScopedPodRBACStillProducesTheVerdictAndIsNamedWhenItFails(t *testing.T) {
	forbidden := apierrors.NewForbidden(
		schema.GroupResource{Resource: "pods"}, "", fmt.Errorf("cluster scope"))
	for _, testcase := range []struct {
		name      string
		namespace error
		wants     []string
	}{
		{name: "the namespaces answer"},
		{
			name:      "a namespace fails too",
			namespace: fmt.Errorf("connection refused"),
			wants:     []string{"not permitted", "connection refused", "data"},
		},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			client := fake.NewClientset(
				statefulSet("data", "postgres", 1, appsv1.StatefulSetStatus{
					ObservedGeneration: 7, ReadyReplicas: 0, UpdatedReplicas: 1,
					CurrentRevision: "postgres-6f4", UpdateRevision: "postgres-9ab",
				}),
				workloadPod("data", "postgres-0", "StatefulSet", "data/postgres", false, 20*time.Second),
			)
			client.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
				if action.GetNamespace() == metav1.NamespaceAll {
					return true, nil, forbidden
				}
				if testcase.namespace != nil {
					return true, nil, testcase.namespace
				}
				return false, nil, nil
			})
			c, err := New(client)
			if err != nil {
				t.Fatalf("new: %v", err)
			}
			got, err := c.Objects(context.Background())
			if len(testcase.wants) == 0 {
				if err != nil {
					t.Fatalf("want namespace-scoped RBAC to answer, got: %v", err)
				}
				if len(got) != 1 || !got[0].Reconciling || !got[0].Healthy {
					t.Fatalf("want the StatefulSet's own pods read and its rollout held open, got %+v", got)
				}
				return
			}
			if err == nil {
				t.Fatal("want the pod read reported as failed")
			}
			for _, want := range testcase.wants {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("want %q named in the halt, got: %v", want, err)
				}
			}
		})
	}
}

// Namespace-scoped RBAC makes the cluster-wide list Forbidden on every poll
// while the namespaces answer, so the fallback carries the whole verdict and
// nothing else fails - the regime was invisible. The observer names it every
// poll it is active, including that steady state, and stays silent when one
// cluster-wide read answers.
func TestTheDegradedPerNamespacePodReadIsObservedEvenWhenItSucceeds(t *testing.T) {
	forbidden := apierrors.NewForbidden(
		schema.GroupResource{Resource: "pods"}, "", fmt.Errorf("cluster scope"))
	for _, testcase := range []struct {
		name      string
		forbid    bool
		wantFires int
	}{
		{name: "cluster-wide answers, regime never entered", forbid: false, wantFires: 0},
		{name: "forbidden cluster-wide, namespaces answer", forbid: true, wantFires: 1},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			client := fake.NewClientset(readyStatefulSet("data", "postgres"))
			client.PrependReactor("list", "pods", func(action k8stesting.Action) (bool, runtime.Object, error) {
				if testcase.forbid && action.GetNamespace() == metav1.NamespaceAll {
					return true, nil, forbidden
				}
				return false, nil, nil
			})
			var reasons []string
			c, err := New(client, WithDegradedReadObserver(func(reason string) {
				reasons = append(reasons, reason)
			}))
			if err != nil {
				t.Fatalf("new: %v", err)
			}
			if _, err := c.Objects(context.Background()); err != nil {
				t.Fatalf("objects: %v", err)
			}
			if len(reasons) != testcase.wantFires {
				t.Fatalf("want observer fired %d time(s), got %d: %q", testcase.wantFires, len(reasons), reasons)
			}
			if testcase.wantFires > 0 && !strings.Contains(reasons[0], "not permitted") {
				t.Fatalf("want the Forbidden regime named, got %q", reasons[0])
			}
		})
	}
}

func readyStatefulSet(namespace, name string) *appsv1.StatefulSet {
	return statefulSet(namespace, name, 1, appsv1.StatefulSetStatus{
		ObservedGeneration: 7, ReadyReplicas: 1, UpdatedReplicas: 1,
	})
}

// Measured at 10 001 pods: the cluster-wide pod list trips the collection limit,
// every snapshot fails, and both Watch call sites treat that as terminal - so a
// mid-sized cluster could never observe a merge at all.
func TestHealthSnapshotSurvivesMorePodsThanTheCollectionLimit(t *testing.T) {
	objects := []runtime.Object{
		statefulSet("data", "postgres", 1, appsv1.StatefulSetStatus{
			ObservedGeneration: 7, ReadyReplicas: 1, UpdatedReplicas: 1,
		}),
	}
	for namespace := range 10 {
		for i := range 1001 {
			objects = append(objects, &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Namespace: fmt.Sprintf("app-%d", namespace),
				Name:      fmt.Sprintf("worker-%d", i),
			}})
		}
	}
	c, err := New(fake.NewClientset(objects...))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	got, err := c.Objects(context.Background())
	if err != nil {
		t.Fatalf("objects: %v", err)
	}
	if len(got) != 1 || !got[0].Healthy {
		t.Fatalf("want the one StatefulSet reported healthy, got %+v", got)
	}
}

func deployment(namespace, name string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Generation: 4},
		Spec:       appsv1.DeploymentSpec{Replicas: ptr(int32(1))},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration: 4, ReadyReplicas: 1, UpdatedReplicas: 1,
		},
	}
}

func statefulSet(namespace, name string, replicas int32, status appsv1.StatefulSetStatus) *appsv1.StatefulSet {
	return &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, Generation: 7, UID: types.UID(namespace + "/" + name)},
		Spec:       appsv1.StatefulSetSpec{Replicas: &replicas},
		Status:     status,
	}
}

func degradedDaemonSet() *appsv1.DaemonSet {
	return &appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: "kube-system", Name: "node-exporter", Generation: 3, UID: "ds"},
		Status: appsv1.DaemonSetStatus{
			ObservedGeneration:     3,
			DesiredNumberScheduled: 5,
			NumberReady:            4,
			UpdatedNumberScheduled: 5,
		},
	}
}

func workloadPod(namespace, name, kind string, owner types.UID, ready bool, age time.Duration) *corev1.Pod {
	readiness := corev1.ConditionFalse
	if ready {
		readiness = corev1.ConditionTrue
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace, Name: name,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-age)),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: kind, Name: name, UID: owner, Controller: ptr(true),
			}},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
		Status: corev1.PodStatus{
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: readiness}},
		},
	}
}

func ptr[T any](value T) *T { return &value }

func onlyObject(t *testing.T, objects ...runtime.Object) domain.ObjectHealth {
	t.Helper()
	c, err := New(fake.NewClientset(objects...))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	got, err := c.Objects(context.Background())
	if err != nil {
		t.Fatalf("objects: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("want one object, got %d", len(got))
	}
	return got[0]
}

func TestFullyReadyWorkloadIsHealthy(t *testing.T) {
	got := workloadHealth(rollout{
		kind: "Deployment", namespace: "media", name: "sonarr",
		generation: 4, observed: 4, desired: 3, ready: 3, updated: 3,
	})
	if !got.Healthy || got.Reconciling {
		t.Fatalf("want healthy and settled, got %+v", got)
	}
}

// events.k8s.io/v1 records eventTime and leaves lastTimestamp empty, so sorting
// on lastTimestamp alone buries the newest warnings behind every older one - and
// the 16 KiB cap drops from the end, so those are the first to be lost.
func TestNewestWarningSortsFirstWithoutALastTimestamp(t *testing.T) {
	base := time.Date(2026, 8, 2, 3, 0, 0, 0, time.UTC)
	c, err := New(fake.NewClientset(
		warning("media", "older", "sonarr-1", func(event *corev1.Event) {
			event.LastTimestamp = metav1.NewTime(base)
		}),
		warning("media", "newer", "sonarr-1", func(event *corev1.Event) {
			event.EventTime = metav1.NewMicroTime(base.Add(time.Hour))
		}),
		warning("media", "newest", "sonarr-1", func(event *corev1.Event) {
			event.EventTime = metav1.NewMicroTime(base.Add(time.Hour))
			event.Series = &corev1.EventSeries{
				Count: 4, LastObservedTime: metav1.NewMicroTime(base.Add(2 * time.Hour)),
			}
		}),
	))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	got, err := c.Events(context.Background(), "media", "")
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	newest, newer, older := strings.Index(got, "newest"), strings.Index(got, "newer"), strings.Index(got, "older")
	if newest > newer || newer > older {
		t.Fatalf("want the newest warning first, got:\n%s", got)
	}
	if !strings.Contains(got, "05:00:00 Pod/sonarr-1 newest") {
		t.Fatalf("want the series' last observation rendered, got:\n%s", got)
	}
}

// A sibling whose name merely extends the one asked about is a different
// workload. Its warnings crowd the real ones out of the 16 KiB budget and invite
// a diagnosis to blame it for a failure it had no part in.
func TestEventsExcludeASiblingWorkloadWhoseNameExtendsTheOneAskedAbout(t *testing.T) {
	for _, testcase := range []struct {
		name    string
		kind    string
		object  string
		include bool
	}{
		{name: "the workload itself", kind: "Deployment", object: "sonarr", include: true},
		{name: "its replicaset", kind: "ReplicaSet", object: "sonarr-79bcbcb5cb", include: true},
		{name: "its pod", kind: "Pod", object: "sonarr-79bcbcb5cb-z6cfz", include: true},
		{name: "a stateful pod's ordinal", kind: "Pod", object: "sonarr-0", include: true},
		{name: "a claim from its volumeClaimTemplates", kind: "PersistentVolumeClaim", object: "data-sonarr-0", include: true},
		{name: "a sibling's claim", kind: "PersistentVolumeClaim", object: "data-sonarr-exporter-0", include: false},
		{name: "a sibling workload", kind: "Deployment", object: "sonarr-exporter", include: false},
		{name: "a sibling's replicaset", kind: "ReplicaSet", object: "sonarr-exporter-6d4f7b9c8", include: false},
		{name: "a sibling's pod", kind: "Pod", object: "sonarr-exporter-6d4f7b9c8-lk2mn", include: false},
		{name: "a workload that merely contains the name", kind: "Deployment", object: "my-sonarr-backup", include: false},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			c, err := New(fake.NewClientset(
				warning("media", "probe failed", testcase.object, func(event *corev1.Event) {
					event.InvolvedObject.Kind = testcase.kind
				}),
			))
			if err != nil {
				t.Fatalf("new: %v", err)
			}
			got, err := c.Events(context.Background(), "media", "sonarr")
			if err != nil {
				t.Fatalf("events: %v", err)
			}
			if strings.Contains(got, testcase.object) != testcase.include {
				t.Fatalf("want included=%t for %s/%s, got:\n%s",
					testcase.include, testcase.kind, testcase.object, got)
			}
		})
	}
}

// A StatefulSet and a DaemonSet add only ONE generated segment to their pod
// names, so "sonarr-postgres-0" and "sonarr-exporter-lk2mn" have exactly the
// shape of a Deployment's own "sonarr-<hash>-<tail>". No rule over the name can
// separate them - not even one that knows the asked-about workload's kind - but
// the pod's controller reference names its creator outright.
func TestAPodIsAttributedToTheWorkloadThatCreatedItNotToOneItsNameResembles(t *testing.T) {
	for _, testcase := range []struct {
		name       string
		pod        string
		ownerKind  string
		ownerName  string
		controller bool
		include    bool
	}{
		{
			name: "its own replicaset's pod", pod: "sonarr-79bcbcb5cb-z6cfz",
			ownerKind: "ReplicaSet", ownerName: "sonarr-79bcbcb5cb", controller: true, include: true,
		},
		{
			name: "its own stateful pod", pod: "sonarr-0",
			ownerKind: "StatefulSet", ownerName: "sonarr", controller: true, include: true,
		},
		{
			name: "a sibling StatefulSet's pod", pod: "sonarr-postgres-0",
			ownerKind: "StatefulSet", ownerName: "sonarr-postgres", controller: true, include: false,
		},
		{
			name: "a sibling DaemonSet's pod", pod: "sonarr-exporter-lk2mn",
			ownerKind: "DaemonSet", ownerName: "sonarr-exporter", controller: true, include: false,
		},
		{
			name: "a sibling Deployment's pod", pod: "sonarr-exporter-6d4f7b9c8-lk2mn",
			ownerKind: "ReplicaSet", ownerName: "sonarr-exporter-6d4f7b9c8", controller: true, include: false,
		},
		{
			name: "a job from its cronjob", pod: "sonarr-28234-abcde",
			ownerKind: "Job", ownerName: "sonarr-28234", controller: true, include: true,
		},
		{
			name: "a pod nothing controls falls back to its name", pod: "sonarr-0",
			ownerKind: "StatefulSet", ownerName: "sonarr-postgres", controller: false, include: true,
		},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			c, err := New(fake.NewClientset(
				ownedPod("media", testcase.pod, testcase.ownerKind, testcase.ownerName, testcase.controller),
				warning("media", "probe failed", testcase.pod, func(*corev1.Event) {}),
			))
			if err != nil {
				t.Fatalf("new: %v", err)
			}
			got, err := c.Events(context.Background(), "media", "sonarr")
			if err != nil {
				t.Fatalf("events: %v", err)
			}
			if strings.Contains(got, testcase.pod) != testcase.include {
				t.Fatalf("want included=%t for %s owned by %s/%s, got:\n%s",
					testcase.include, testcase.pod, testcase.ownerKind, testcase.ownerName, got)
			}
		})
	}
}

// A ReplicaSet's and a Job's own name is as ambiguous as the pod's: an indexed
// Job "sonarr-migrate" has exactly the shape of a ReplicaSet of Deployment
// "sonarr", so matching it presents a foreign Job's failure as evidence about a
// Deployment that had no part in it. The object itself says who owns it.
func TestAnIntermediateOwnerIsFollowedRatherThanMatchedOnItsName(t *testing.T) {
	for _, testcase := range []struct {
		name       string
		pod        string
		ownerKind  string
		ownerName  string
		parentKind string
		parentName string
		include    bool
	}{
		{
			name: "an indexed job of its own cronjob", pod: "sonarr-migrate-0-abcde",
			ownerKind: "Job", ownerName: "sonarr-migrate",
			parentKind: "CronJob", parentName: "sonarr", include: true,
		},
		{
			name: "a foreign job whose name resembles a replicaset", pod: "sonarr-migrate-abcde",
			ownerKind: "Job", ownerName: "sonarr-migrate", include: false,
		},
		{
			name: "an indexed foreign job", pod: "sonarr-migrate-0-abcde",
			ownerKind: "Job", ownerName: "sonarr-migrate", include: false,
		},
		{
			name: "its own replicaset", pod: "sonarr-79bcbcb5cb-z6cfz",
			ownerKind: "ReplicaSet", ownerName: "sonarr-79bcbcb5cb",
			parentKind: "Deployment", parentName: "sonarr", include: true,
		},
		{
			name: "a replicaset of another deployment that only looks generated", pod: "sonarr-canary-z6cfz",
			ownerKind: "ReplicaSet", ownerName: "sonarr-canary",
			parentKind: "Deployment", parentName: "sonarr-canary", include: false,
		},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			objects := []runtime.Object{
				ownedPod("media", testcase.pod, testcase.ownerKind, testcase.ownerName, true),
				warning("media", "probe failed", testcase.pod, func(*corev1.Event) {}),
			}
			switch testcase.ownerKind {
			case "Job":
				objects = append(objects, ownedJob("media", testcase.ownerName, testcase.parentKind, testcase.parentName))
			case "ReplicaSet":
				objects = append(objects, ownedReplicaSet("media", testcase.ownerName, testcase.parentKind, testcase.parentName))
			}
			c, err := New(fake.NewClientset(objects...))
			if err != nil {
				t.Fatalf("new: %v", err)
			}
			got, err := c.Events(context.Background(), "media", "sonarr")
			if err != nil {
				t.Fatalf("events: %v", err)
			}
			if strings.Contains(got, testcase.pod) != testcase.include {
				t.Fatalf("want included=%t for %s under %s/%s, got:\n%s",
					testcase.include, testcase.pod, testcase.ownerKind, testcase.ownerName, got)
			}
		})
	}
}

// An intermediate owner that cannot be read leaves the chain unfinished, and
// deciding a Deployment's own pods out on that would blind the most common
// diagnosis there is. The pod's name shape is what is left.
func TestAnUnreadableIntermediateOwnerFallsBackToTheNameRule(t *testing.T) {
	for _, testcase := range []struct {
		name    string
		pod     string
		owner   string
		include bool
	}{
		{name: "its own replicaset", pod: "sonarr-79bcbcb5cb-z6cfz", owner: "sonarr-79bcbcb5cb", include: true},
		{name: "a sibling's replicaset", pod: "sonarr-exporter-6d4f7b9c8-lk2mn", owner: "sonarr-exporter-6d4f7b9c8", include: false},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			client := fake.NewClientset(
				ownedPod("media", testcase.pod, "ReplicaSet", testcase.owner, true),
				ownedReplicaSet("media", testcase.owner, "Deployment", "sonarr"),
				warning("media", "probe failed", testcase.pod, func(*corev1.Event) {}),
			)
			client.PrependReactor("list", "replicasets", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, nil, apierrors.NewForbidden(schema.GroupResource{Resource: "replicasets"}, "", fmt.Errorf("no"))
			})
			c, err := New(client)
			if err != nil {
				t.Fatalf("new: %v", err)
			}
			got, err := c.Events(context.Background(), "media", "sonarr")
			if err != nil {
				t.Fatalf("events: %v", err)
			}
			if strings.Contains(got, testcase.pod) != testcase.include {
				t.Fatalf("want included=%t for %s, got:\n%s", testcase.include, testcase.pod, got)
			}
		})
	}
}

// A truncated intermediate-owner list is as unread as a failed one: using its
// partial page attributes some pods by lineage and leaks the rest, so
// truncation degrades to the name rule the way every other list in the adapter
// turns truncation into an error.
func TestATruncatedIntermediateOwnerListFallsBackToTheNameRule(t *testing.T) {
	for _, testcase := range []struct {
		name    string
		pod     string
		owner   string
		include bool
	}{
		{name: "its own replicaset", pod: "sonarr-79bcbcb5cb-z6cfz", owner: "sonarr-79bcbcb5cb", include: true},
		{name: "a sibling's replicaset", pod: "sonarr-exporter-6d4f7b9c8-lk2mn", owner: "sonarr-exporter-6d4f7b9c8", include: false},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			rs := ownedReplicaSet("media", testcase.owner, "Deployment", "sonarr")
			client := fake.NewClientset(
				ownedPod("media", testcase.pod, "ReplicaSet", testcase.owner, true),
				rs,
				warning("media", "probe failed", testcase.pod, func(*corev1.Event) {}),
			)
			client.PrependReactor("list", "replicasets", func(k8stesting.Action) (bool, runtime.Object, error) {
				return true, &appsv1.ReplicaSetList{
					ListMeta: metav1.ListMeta{Continue: "more"},
					Items:    []appsv1.ReplicaSet{*rs},
				}, nil
			})
			c, err := New(client)
			if err != nil {
				t.Fatalf("new: %v", err)
			}
			got, err := c.Events(context.Background(), "media", "sonarr")
			if err != nil {
				t.Fatalf("events: %v", err)
			}
			if strings.Contains(got, testcase.pod) != testcase.include {
				t.Fatalf("want included=%t for %s, got:\n%s", testcase.include, testcase.pod, got)
			}
		})
	}
}

// Only a kind that creates its pods itself can rule an event out by its name
// alone. A Node holding a static pod and a CRD's own runner name something that
// is not a workload, so answering "not this workload" from them drops every
// warning about those pods - silently, and again for every new kind that
// appears. The kinds that do own their pods still decide, or a sibling
// StatefulSet's pod leaks straight back in.
func TestAPodOwnedByAKindThatDoesNotNameAWorkloadKeepsTheNameRule(t *testing.T) {
	for _, testcase := range []struct {
		name      string
		pod       string
		ownerKind string
		ownerName string
		asked     string
		include   bool
	}{
		{
			name: "a static pod owned by its node", pod: "kube-apiserver-node1",
			ownerKind: "Node", ownerName: "node1", asked: "kube-apiserver", include: true,
		},
		{
			name: "a pod owned by a custom runner", pod: "release-build-pod",
			ownerKind: "TaskRun", ownerName: "release-build", asked: "release", include: true,
		},
		{
			name: "a sibling StatefulSet's pod", pod: "sonarr-postgres-0",
			ownerKind: "StatefulSet", ownerName: "sonarr-postgres", asked: "sonarr", include: false,
		},
		{
			name: "a sibling DaemonSet's pod", pod: "sonarr-exporter-lk2mn",
			ownerKind: "DaemonSet", ownerName: "sonarr-exporter", asked: "sonarr", include: false,
		},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			c, err := New(fake.NewClientset(
				ownedPod("media", testcase.pod, testcase.ownerKind, testcase.ownerName, true),
				warning("media", "probe failed", testcase.pod, func(*corev1.Event) {}),
			))
			if err != nil {
				t.Fatalf("new: %v", err)
			}
			got, err := c.Events(context.Background(), "media", testcase.asked)
			if err != nil {
				t.Fatalf("events: %v", err)
			}
			if strings.Contains(got, testcase.pod) != testcase.include {
				t.Fatalf("want included=%t for %s owned by %s/%s, got:\n%s",
					testcase.include, testcase.pod, testcase.ownerKind, testcase.ownerName, got)
			}
		})
	}
}

// The pod an event names is often already gone - evicted, OOMKilled, garbage
// collected - and a diagnosis that dropped those warnings would lose the ones
// most likely to explain the failure.
func TestEventsAboutAPodThatNoLongerExistsStillMatchOnItsName(t *testing.T) {
	c, err := New(fake.NewClientset(
		warning("media", "probe failed", "sonarr-79bcbcb5cb-z6cfz", func(*corev1.Event) {}),
	))
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	got, err := c.Events(context.Background(), "media", "sonarr")
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	if !strings.Contains(got, "sonarr-79bcbcb5cb-z6cfz") {
		t.Fatalf("want a departed pod's warning kept, got:\n%s", got)
	}
}

func ownedPod(namespace, name, ownerKind, ownerName string, controller bool) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace, Name: name,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: ownerKind, Name: ownerName,
				UID: types.UID(ownerName), Controller: ptr(controller),
			}},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
	}
}

func ownedJob(namespace, name, ownerKind, ownerName string) *batchv1.Job {
	return &batchv1.Job{ObjectMeta: owned(namespace, name, ownerKind, ownerName)}
}

func ownedReplicaSet(namespace, name, ownerKind, ownerName string) *appsv1.ReplicaSet {
	return &appsv1.ReplicaSet{ObjectMeta: owned(namespace, name, ownerKind, ownerName)}
}

func owned(namespace, name, ownerKind, ownerName string) metav1.ObjectMeta {
	meta := metav1.ObjectMeta{Namespace: namespace, Name: name}
	if ownerKind != "" {
		meta.OwnerReferences = []metav1.OwnerReference{{
			APIVersion: "apps/v1", Kind: ownerKind, Name: ownerName,
			UID: types.UID(ownerName), Controller: ptr(true),
		}}
	}
	return meta
}

// C-L04 ordered the event list newest-first so a cut drops the least useful
// warnings. That only holds if the model is told a cut happened: an unannounced
// prefix reads as "these are all the warnings" when it is the ones that fit.
func TestEveryClusterRenderSaysSoWhenItsOutputWasCut(t *testing.T) {
	tests := []struct {
		name    string
		objects []runtime.Object
		render  func(*Client) (string, error)
	}{
		{
			name:    "warning events",
			objects: crowdedNamespace(40, 0),
			render:  func(c *Client) (string, error) { return c.Events(context.Background(), "media", "") },
		},
		{
			name:    "pods",
			objects: crowdedNamespace(0, 600),
			render:  func(c *Client) (string, error) { return c.Pods(context.Background(), "media") },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c, err := New(fake.NewClientset(test.objects...))
			if err != nil {
				t.Fatalf("new: %v", err)
			}
			got, err := test.render(c)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if !strings.HasSuffix(got, "\n... [truncated]") {
				t.Fatalf("a %d-byte render carries no truncation marker, ending:\n%s",
					len(got), got[max(0, len(got)-200):])
			}
			if len(got) > maxRenderedBytes+len("\n... [truncated]") {
				t.Errorf("render is %d bytes, want at most %d", len(got), maxRenderedBytes+len("\n... [truncated]"))
			}
		})
	}
}

// An event message can itself span lines, so a newline is not a record
// boundary. A cut inside a multi-line warning can keep a PEM block's BEGIN and
// body while dropping its END, and boundPrivateKeyBlocks deliberately leaves an
// unbalanced block alone - so the half that survives is the half holding the
// key material, and it reaches the model unscrubbed.
func TestACutNeverSplitsAMultiLineEventRecord(t *testing.T) {
	const begin, end = "-----BEGIN PRIVATE KEY-----", "-----END PRIVATE KEY-----"
	key := begin + "\n" + strings.Repeat(strings.Repeat("k", 64)+"\n", 30) + end
	base := time.Date(2026, 8, 2, 3, 0, 0, 0, time.UTC)
	for fillers := 10; fillers <= 18; fillers++ {
		t.Run(fmt.Sprintf("behind %d warnings", fillers), func(t *testing.T) {
			objects := []runtime.Object{
				warning("media", "leaked-key", "sonarr-1", func(event *corev1.Event) {
					event.Message = key
					event.LastTimestamp = metav1.NewTime(base)
				}),
			}
			for i := range fillers {
				objects = append(objects, warning("media", fmt.Sprintf("reason-%03d", i), "sonarr-1",
					func(event *corev1.Event) {
						event.Message = strings.Repeat("m", 1000)
						event.LastTimestamp = metav1.NewTime(base.Add(time.Duration(i+1) * time.Hour))
					}))
			}
			c, err := New(fake.NewClientset(objects...))
			if err != nil {
				t.Fatalf("new: %v", err)
			}
			got, err := c.Events(context.Background(), "media", "")
			if err != nil {
				t.Fatalf("events: %v", err)
			}
			if strings.Contains(got, begin) != strings.Contains(got, end) {
				t.Fatalf("the key block was cut in half: BEGIN present=%v, END present=%v",
					strings.Contains(got, begin), strings.Contains(got, end))
			}
		})
	}
}

// A record the bound cannot fit is dropped whole, so the render is always a
// prefix of the record list rather than a prefix of the bytes.
func TestARenderCutDropsTheCrossingRecordWhole(t *testing.T) {
	record := strings.Repeat("m", 1000) + "\n"
	tests := []struct {
		name    string
		records []string
		want    string
	}{
		{
			name:    "everything fits",
			records: slices.Repeat([]string{record}, 4),
			want:    strings.Repeat(record, 4),
		},
		{
			name:    "the last record ends exactly on the bound",
			records: []string{strings.Repeat("m", maxRenderedBytes-1) + "\n"},
			want:    strings.Repeat("m", maxRenderedBytes-1) + "\n",
		},
		{
			name:    "one record crosses the bound",
			records: slices.Repeat([]string{record}, 20),
			want:    strings.Repeat(record, 16) + "\n... [truncated]",
		},
		{
			name:    "a record spanning lines crosses the bound",
			records: []string{strings.Repeat(record, 16), strings.Repeat("a\n", 500)},
			want:    strings.Repeat(record, 16) + "\n... [truncated]",
		},
		{
			name:    "the first record alone is larger than the bound",
			records: []string{strings.Repeat("m", maxRenderedBytes+1)},
			want:    "\n... [truncated]",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var out renderBound
			for _, record := range test.records {
				out.add(record)
				if out.cut {
					break
				}
			}
			if got := out.String(); got != test.want {
				t.Fatalf("render is %d bytes ending %q, want %d ending %q",
					len(got), got[max(0, len(got)-40):], len(test.want), test.want[max(0, len(test.want)-40):])
			}
		})
	}
}

// The cut falls between add() calls, so the no-half-key invariant the PEM fix
// rests on holds only while no single record splits a private-key block. A
// caller that streams one key record across add() calls, with the half carrying
// the closing END dropped by the bound, must not leave key material reachable -
// and a chunk with a matching count of markers is still a fragment when its END
// precedes an unterminated BEGIN, so counting markers is not enough.
func TestARenderDropsARecordThatSplitsAPrivateKeyBlock(t *testing.T) {
	const begin, end = "-----BEGIN RSA PRIVATE KEY-----", "-----END RSA PRIVATE KEY-----"
	tests := []struct {
		name     string
		records  []string
		survives string
		leaked   string
	}{
		{
			name: "the begin half fits and the end half is cut",
			records: []string{
				begin + "\nLEAKEDKEY\n" + strings.Repeat("k", maxRenderedBytes-100),
				strings.Repeat("k", 300) + "\n" + end + "\n",
			},
			leaked: "LEAKEDKEY",
		},
		{
			name: "an end preceding an unterminated begin balances by count",
			records: []string{
				end + "\nlog\n" + begin + "\nLEAKEDKEY\n" + strings.Repeat("k", maxRenderedBytes-100),
				strings.Repeat("k", 300) + "\n" + end + "\n",
			},
			leaked: "LEAKEDKEY",
		},
		{
			name:     "a complete key block is kept whole",
			records:  []string{begin + "\nGOODBODY\n" + end + "\n"},
			survives: "GOODBODY",
		},
		{
			name:     "a record with no key markers is kept",
			records:  []string{"plain warning line\n"},
			survives: "plain warning line",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var out renderBound
			for _, record := range test.records {
				out.add(record)
				if out.cut {
					break
				}
			}
			got := out.String()
			if test.leaked != "" && strings.Contains(got, test.leaked) {
				t.Fatalf("a split key block leaked %q, output ends:\n%s",
					test.leaked, got[max(0, len(got)-80):])
			}
			if test.leaked != "" && strings.Contains(got, begin) != strings.Contains(got, end) {
				t.Fatalf("a key block was cut in half: BEGIN present=%v, END present=%v",
					strings.Contains(got, begin), strings.Contains(got, end))
			}
			if test.survives != "" && !strings.Contains(got, test.survives) {
				t.Fatalf("a whole record was over-dropped, want %q in:\n%s", test.survives, got)
			}
		})
	}
}

// crowdedNamespace fills one namespace with more warnings and pods than any
// render bound admits.
func crowdedNamespace(warnings, pods int) []runtime.Object {
	var objects []runtime.Object
	for i := range warnings {
		objects = append(objects, warning("media", fmt.Sprintf("reason-%03d", i), "sonarr-1",
			func(event *corev1.Event) { event.Message = strings.Repeat("m", 1000) }))
	}
	for i := range pods {
		objects = append(objects, pod("media", fmt.Sprintf("sonarr-%04d", i), true))
	}
	return objects
}

func warning(namespace, message, involved string, apply func(*corev1.Event)) *corev1.Event {
	event := &corev1.Event{
		ObjectMeta:     metav1.ObjectMeta{Namespace: namespace, Name: namespace + "-" + message},
		InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: involved},
		Type:           corev1.EventTypeWarning,
		Reason:         message,
		Message:        message,
	}
	apply(event)
	return event
}

// A revert of a healthy deployment happened because the agent asked for logs
// from "matter-server" while the pod was "matter-server-79bcbcb5cb-z6cfz",
// read the miss as proof the workload was gone, and reverted a working update.
func TestResolvePodAcceptsTheWorkloadName(t *testing.T) {
	client := fake.NewClientset(
		pod("smarthome", "matter-server-79bcbcb5cb-z6cfz", true),
		pod("smarthome", "matter-bridge-7c74b9fbb7-9g5kx", true),
	)
	c, err := New(client)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	got, err := c.resolvePod(context.Background(), "smarthome", "matter-server")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "matter-server-79bcbcb5cb-z6cfz" {
		t.Fatalf("resolved to %q", got)
	}
}

// A diagnosis wants the pod that is failing, not an arbitrary healthy replica.
func TestResolvePodPrefersOneThatIsNotReady(t *testing.T) {
	client := fake.NewClientset(
		pod("media", "sonarr-1", true),
		pod("media", "sonarr-2", false),
	)
	c, _ := New(client)
	got, err := c.resolvePod(context.Background(), "media", "sonarr")
	if err != nil || got != "sonarr-2" {
		t.Fatalf("got %q err=%v, want the unready pod", got, err)
	}
}

// A pod that never started has no container statuses at all, so folding an empty
// list to "ready" is exactly backwards: the unschedulable or image-pull-stuck pod
// is skipped and the diagnosis is handed a healthy replica's logs.
func TestResolvePodPrefersOneThatNeverStarted(t *testing.T) {
	for _, testcase := range []struct {
		name   string
		status corev1.PodStatus
	}{
		{
			name:   "unschedulable, no container statuses yet",
			status: corev1.PodStatus{Phase: corev1.PodPending},
		},
		{
			name: "one container up, another that never started",
			status: corev1.PodStatus{
				Phase:             corev1.PodRunning,
				ContainerStatuses: []corev1.ContainerStatus{{Name: "app", Ready: true}, {Name: "sidecar", Ready: false}},
			},
		},
	} {
		t.Run(testcase.name, func(t *testing.T) {
			stuck := pod("media", "sonarr-2", true)
			stuck.Status = testcase.status
			c, err := New(fake.NewClientset(pod("media", "sonarr-1", true), stuck))
			if err != nil {
				t.Fatalf("new: %v", err)
			}
			got, err := c.resolvePod(context.Background(), "media", "sonarr")
			if err != nil || got != "sonarr-2" {
				t.Fatalf("got %q err=%v, want the pod that never started", got, err)
			}
		})
	}
}

func TestResolvePodReportsAGenuinelyAbsentWorkload(t *testing.T) {
	c, _ := New(fake.NewClientset())
	if _, err := c.resolvePod(context.Background(), "media", "sonarr"); err == nil {
		t.Fatal("want an error naming what was looked for")
	}
}

func pod(namespace, name string, ready bool) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
		Status: corev1.PodStatus{
			Phase:             corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{{Name: "app", Ready: ready}},
		},
	}
}
