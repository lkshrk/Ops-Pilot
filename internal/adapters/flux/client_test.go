package flux

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lkshrk/ops-pilot/internal/domain"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	discoveryfake "k8s.io/client-go/discovery/fake"
	"k8s.io/client-go/dynamic"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	clienttesting "k8s.io/client-go/testing"
)

var probeSource = domain.ObjectRef{Kind: "GitRepository", Namespace: "flux-system", Name: "flux-system"}

func servedAPIs(served ...schema.GroupVersionResource) *discoveryfake.FakeDiscovery {
	byGroupVersion := map[string]*metav1.APIResourceList{}
	order := make([]string, 0, len(served))
	for _, item := range served {
		version := item.GroupVersion().String()
		if byGroupVersion[version] == nil {
			byGroupVersion[version] = &metav1.APIResourceList{GroupVersion: version}
			order = append(order, version)
		}
		byGroupVersion[version].APIResources = append(
			byGroupVersion[version].APIResources, metav1.APIResource{Name: item.Resource},
		)
	}
	lists := make([]*metav1.APIResourceList, 0, len(order))
	for _, version := range order {
		lists = append(lists, byGroupVersion[version])
	}
	return &discoveryfake.FakeDiscovery{Fake: &clienttesting.Fake{Resources: lists}}
}

func dynamicClient(objects ...*unstructured.Unstructured) *dynamicfake.FakeDynamicClient {
	listKinds := map[schema.GroupVersionResource]string{
		gitRepositories: "GitRepositoryList",
		kustomizations:  "KustomizationList",
		helmReleases:    "HelmReleaseList",
	}
	items := make([]runtime.Object, 0, len(objects))
	for _, object := range objects {
		items = append(items, object)
	}
	return dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), listKinds, items...)
}

func gitRepositoryObject(revision string, conditions ...metav1.Condition) *unstructured.Unstructured {
	status := map[string]any{"observedGeneration": int64(1)}
	if revision != "" {
		status["artifact"] = map[string]any{"revision": revision}
	}
	items := make([]any, 0, len(conditions))
	for i := range conditions {
		conditions[i].LastTransitionTime = metav1.Now()
		raw, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&conditions[i])
		if err != nil {
			panic(err)
		}
		items = append(items, raw)
	}
	if len(items) > 0 {
		status["conditions"] = items
	}
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "source.toolkit.fluxcd.io/v1",
		"kind":       "GitRepository",
		"metadata": map[string]any{
			"namespace":  probeSource.Namespace,
			"name":       probeSource.Name,
			"generation": int64(1),
		},
		"spec":   map[string]any{},
		"status": status,
	}}
}

func TestNewRefusesAnIncompleteClientOrSource(t *testing.T) {
	served := servedAPIs(gitRepositories, kustomizations, helmReleases)
	for _, test := range []struct {
		name      string
		client    dynamic.Interface
		discovery discovery.DiscoveryInterface
		source    domain.ObjectRef
		want      string
	}{
		{"no dynamic client", nil, served, probeSource,
			"Flux dynamic and discovery clients are required"},
		{"no discovery client", dynamicClient(), nil, probeSource,
			"Flux dynamic and discovery clients are required"},
		{"a typed nil dynamic client", (*dynamicfake.FakeDynamicClient)(nil), served, probeSource,
			"Flux dynamic and discovery clients are required"},
		{"another source kind", dynamicClient(), served,
			domain.ObjectRef{Kind: "OCIRepository", Namespace: "flux-system", Name: "flux-system"},
			"configured Flux source must be a GitRepository"},
		{"no source namespace", dynamicClient(), served,
			domain.ObjectRef{Kind: "GitRepository", Name: "flux-system"},
			"configured Flux source must be a GitRepository"},
		{"no source name", dynamicClient(), served,
			domain.ObjectRef{Kind: "GitRepository", Namespace: "flux-system"},
			"configured Flux source must be a GitRepository"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, err := New(test.client, test.discovery, test.source)
			if err == nil {
				t.Fatalf("want a refusal, got %+v", client)
			}
			if err.Error() != test.want {
				t.Fatalf("want %q, got %q", test.want, err)
			}
		})
	}
}

func TestNewRefusesAClusterThatDoesNotServeEveryRequiredAPI(t *testing.T) {
	absent := servedAPIs(kustomizations, helmReleases)
	if _, err := New(dynamicClient(), absent, probeSource); err == nil ||
		!strings.HasPrefix(err.Error(), "discover required Flux API source.toolkit.fluxcd.io/v1: ") {
		t.Fatalf("want the missing group version reported, got %v", err)
	}

	partial := servedAPIs(gitRepositories, helmReleases)
	partial.Resources = append(partial.Resources, &metav1.APIResourceList{
		GroupVersion: kustomizations.GroupVersion().String(),
		APIResources: []metav1.APIResource{{Name: "kustomizeconfigs"}},
	})
	_, err := New(dynamicClient(), partial, probeSource)
	if err == nil || err.Error() != "required Flux API kustomize.toolkit.fluxcd.io/v1 is not served" {
		t.Fatalf("want the unserved resource reported, got %v", err)
	}
}

func TestNewAcceptsAClusterServingEveryRequiredAPI(t *testing.T) {
	client, err := New(dynamicClient(), servedAPIs(gitRepositories, kustomizations, helmReleases), probeSource)

	if err != nil {
		t.Fatalf("want the client built, got %v", err)
	}
	if client.source != probeSource {
		t.Fatalf("want the configured source kept, got %+v", client.source)
	}
}

func TestSourceRevisionReadsTheConfiguredGitRepository(t *testing.T) {
	client := &Client{
		client: dynamicClient(gitRepositoryObject("main@sha1:" + strings.ToUpper(wantSHA))),
		source: probeSource,
	}

	revision, err := client.SourceRevision(context.Background())

	if err != nil {
		t.Fatalf("read the source: %v", err)
	}
	if revision != wantSHA {
		t.Fatalf("want the normalized artifact revision, got %q", revision)
	}
}

func TestSourceRevisionCarriesTheSourcesOwnStall(t *testing.T) {
	stalled := gitRepositoryObject("main@sha1:"+wantSHA, metav1.Condition{
		Type:               "Stalled",
		Status:             metav1.ConditionTrue,
		Reason:             "URLInvalid",
		Message:            "first path segment in URL cannot contain colon",
		ObservedGeneration: 1,
	})
	client := &Client{client: dynamicClient(stalled), source: probeSource}

	revision, err := client.SourceRevision(context.Background())

	if err == nil {
		t.Fatalf("want a live stall to halt the run, got revision %q", revision)
	}
	const want = "Flux source flux-system/flux-system: Flux source has stopped retrying: " +
		"first path segment in URL cannot contain colon"
	if err.Error() != want {
		t.Fatalf("want %q, got %q", want, err)
	}
}

func TestSourceRevisionSaysWhichReadFailed(t *testing.T) {
	missing := &Client{client: dynamicClient(), source: probeSource}
	if _, err := missing.SourceRevision(context.Background()); err == nil ||
		!strings.HasPrefix(err.Error(), "read Flux source: ") {
		t.Fatalf("want an absent source reported as a read failure, got %v", err)
	}

	undecodable := gitRepositoryObject("")
	undecodable.Object["status"].(map[string]any)["artifact"] = "not an object"
	broken := &Client{client: dynamicClient(undecodable), source: probeSource}
	if _, err := broken.SourceRevision(context.Background()); err == nil ||
		!strings.HasPrefix(err.Error(), "decode Flux source: ") {
		t.Fatalf("want an undecodable source reported as a decode failure, got %v", err)
	}
}

func TestReconcileStampsTheRequestInUTC(t *testing.T) {
	client := &Client{client: dynamicClient(gitRepositoryObject("")), source: probeSource}
	at := time.Date(2026, 8, 23, 12, 30, 0, 123456789, time.FixedZone("CEST", 2*60*60))

	if err := client.Reconcile(context.Background(), at); err != nil {
		t.Fatalf("trigger a reconciliation: %v", err)
	}

	item, err := client.get(context.Background(), gitRepositories, probeSource.Namespace, probeSource.Name)
	if err != nil {
		t.Fatalf("read the patched source: %v", err)
	}
	if got := item.GetAnnotations()[reconcileAnnotation]; got != "2026-08-23T10:30:00.123456789Z" {
		t.Fatalf("want the request stamped in UTC, got %q", got)
	}
}

func TestReconcileReportsAPatchItCouldNotApply(t *testing.T) {
	client := &Client{client: dynamicClient(), source: probeSource}

	err := client.Reconcile(context.Background(), time.Unix(0, 0))

	if err == nil || !strings.HasPrefix(err.Error(), "trigger Flux reconciliation: ") {
		t.Fatalf("want the failed patch reported, got %v", err)
	}
}
