package composition

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/lkshrk/ops-pilot/internal/adapters/flux"
	kube "github.com/lkshrk/ops-pilot/internal/adapters/kubernetes"
	"github.com/lkshrk/ops-pilot/internal/config"
	"github.com/lkshrk/ops-pilot/internal/diagnostics"
	"github.com/lkshrk/ops-pilot/internal/domain"
	"github.com/lkshrk/ops-pilot/internal/events"
	"github.com/lkshrk/ops-pilot/internal/run"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestTheRunnerPublishesThroughTheEventStreamTheCallerPassedIn(t *testing.T) {
	cfg, _ := fakeCluster(t, serve(corev1.PodList{
		TypeMeta: metav1.TypeMeta{Kind: "PodList", APIVersion: "v1"},
	}))
	stream := &recordingEvents{}
	handle, err := New(context.Background(), config.Loaded{Config: cfg}, Dependencies{
		LookupEnv: environment(map[string]string{
			"TEST_GITHUB_TOKEN": "a-token",
			"TEST_AI_KEY":       "a-key",
		}),
		Out:    io.Discard,
		Log:    diagnostics.DiscardLogger(),
		Events: stream,
	}, run.Options{}, nil)
	if err != nil {
		t.Fatalf("build the run: %v", err)
	}
	t.Cleanup(func() { _ = handle.Close() })

	// A cancelled run ends at the first recorded write, which is past the bind.
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := handle.Runner.Run(cancelled); err == nil {
		t.Fatal("the cancelled run reported success")
	}

	if len(stream.bound) != 1 {
		t.Fatalf("the runner published through a stream the caller never passed: %d binds", len(stream.bound))
	}
	if stream.bound[0].Repository != cfg.Repository {
		t.Fatalf("bound to %v, want %v", stream.bound[0].Repository, cfg.Repository)
	}
}

func TestNewHandsTheCallersEventStreamToTheRunnerUnwrappedSoEmitStaysRedacted(t *testing.T) {
	cfg, _ := fakeCluster(t, serve(corev1.PodList{
		TypeMeta: metav1.TypeMeta{Kind: "PodList", APIVersion: "v1"},
	}))
	stream := &recordingEvents{}

	var got run.Events
	original := newRunner
	newRunner = func(deps run.Dependencies, opts run.Options) *run.Runner {
		got = deps.Events
		return original(deps, opts)
	}
	t.Cleanup(func() { newRunner = original })

	handle, err := New(context.Background(), config.Loaded{Config: cfg}, Dependencies{
		LookupEnv: environment(map[string]string{
			"TEST_GITHUB_TOKEN": "a-token",
			"TEST_AI_KEY":       "a-key",
		}),
		Out:    io.Discard,
		Log:    diagnostics.DiscardLogger(),
		Events: stream,
	}, run.Options{}, nil)
	if err != nil {
		t.Fatalf("build the run: %v", err)
	}
	t.Cleanup(func() { _ = handle.Close() })

	if got != run.Events(stream) {
		t.Fatal("New wrapped or substituted the caller's event stream; a shadow sink can receive Emit and leak unredacted payloads")
	}
}

func TestNewThreadsTheRunLoggerIntoTheDegradedReadObserver(t *testing.T) {
	cfg, _ := fakeCluster(t, forbidden)
	var written bytes.Buffer
	recording := diagnostics.NewLogger(&written, nil)

	var workloads *kube.Client
	original := buildClusterClients
	buildClusterClients = func(c config.Config, log *diagnostics.Logger) (*flux.Client, *kube.Client, error) {
		f, w, err := original(c, log)
		workloads = w
		return f, w, err
	}
	t.Cleanup(func() { buildClusterClients = original })

	handle, err := New(context.Background(), config.Loaded{Config: cfg}, Dependencies{
		LookupEnv: environment(map[string]string{
			"TEST_GITHUB_TOKEN": "a-token",
			"TEST_AI_KEY":       "a-key",
		}),
		Out:    io.Discard,
		Log:    recording,
		Events: &recordingEvents{},
	}, run.Options{}, nil)
	if err != nil {
		t.Fatalf("build the run: %v", err)
	}
	t.Cleanup(func() { _ = handle.Close() })

	if _, err := workloads.Objects(context.Background()); err != nil {
		t.Fatalf("read the cluster: %v", err)
	}
	if !strings.Contains(written.String(), "not permitted") {
		t.Fatalf("New did not thread the run logger into the degraded-read observer: %q", written.String())
	}
}

type recordingEvents struct {
	bound     []domain.Run
	published []events.Event
}

func (r *recordingEvents) Bind(current domain.Run) { r.bound = append(r.bound, current) }
func (r *recordingEvents) Emit(event events.Event) { r.published = append(r.published, event) }

func environment(values map[string]string) func(string) (string, bool) {
	return func(name string) (string, bool) {
		value, found := values[name]
		return value, found
	}
}

func TestTheDegradedPodReadRegimeIsReportedWhileItSucceeds(t *testing.T) {
	cfg, _ := fakeCluster(t, forbidden)
	var written bytes.Buffer
	workloads := workloadClient(t, cfg, diagnostics.NewLogger(&written, nil))

	objects, err := workloads.Objects(context.Background())
	if err != nil {
		t.Fatalf("read the cluster: %v", err)
	}

	if len(objects) != 1 {
		t.Fatalf("the per-namespace fallback did not carry the verdict: %d objects", len(objects))
	}
	if !strings.Contains(written.String(), "not permitted") {
		t.Fatalf("the degraded pod-read regime went unreported: %q", written.String())
	}
}

func TestTheDegradedPodReadRegimeIsReportedOncePerRunNotOncePerPoll(t *testing.T) {
	cfg, _ := fakeCluster(t, forbidden)
	var written bytes.Buffer
	workloads := workloadClient(t, cfg, diagnostics.NewLogger(&written, nil))

	for poll := 0; poll < 5; poll++ {
		if _, err := workloads.Objects(context.Background()); err != nil {
			t.Fatalf("read the cluster: %v", err)
		}
	}

	if reported := reports(written.String()); len(reported) != 1 {
		t.Fatalf("want one report over five polls, got %d: %q", len(reported), reported)
	}
}

func TestATransientDegradedReadIsReportedApartFromThePermanentForbiddenRegime(t *testing.T) {
	cfg, _ := fakeCluster(t, inTurn(forbidden, unavailable, forbidden, unavailable))
	var written bytes.Buffer
	workloads := workloadClient(t, cfg, diagnostics.NewLogger(&written, nil))

	for poll := 0; poll < 4; poll++ {
		if _, err := workloads.Objects(context.Background()); err != nil {
			t.Fatalf("read the cluster: %v", err)
		}
	}

	reported := reports(written.String())
	if len(reported) != 2 {
		t.Fatalf("want the two regimes reported once each, got %d: %q", len(reported), reported)
	}
	permanent := 0
	for _, line := range reported {
		if strings.Contains(line, "not permitted") {
			permanent++
		}
	}
	if permanent != 1 {
		t.Fatalf("the two regimes are not distinguishable: %q", reported)
	}
}

func reports(logged string) []string {
	var found []string
	for _, line := range strings.Split(strings.TrimSpace(logged), "\n") {
		if strings.Contains(line, "degraded cluster read") {
			found = append(found, line)
		}
	}
	return found
}

// inTurn answers each successive request with the next handler, repeating the
// last one, so one server can put the cluster through both degraded regimes.
func inTurn(handlers ...http.HandlerFunc) http.HandlerFunc {
	var mu sync.Mutex
	call := 0
	return func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		handler := handlers[min(call, len(handlers)-1)]
		call++
		mu.Unlock()
		handler(w, r)
	}
}

func workloadClient(t *testing.T, cfg config.Config, log *diagnostics.Logger) *kube.Client {
	t.Helper()
	_, workloads, err := clusterClients(cfg, log)
	if err != nil {
		t.Fatalf("build the cluster clients: %v", err)
	}
	return workloads
}

// fakeCluster serves the discovery and list reads composition performs at
// build time, with the cluster-wide pod list answered by clusterWidePods.
func fakeCluster(t *testing.T, clusterWidePods http.HandlerFunc) (config.Config, *httptest.Server) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/apis/source.toolkit.fluxcd.io/v1", serve(apiResources("source.toolkit.fluxcd.io/v1", "gitrepositories")))
	mux.HandleFunc("/apis/kustomize.toolkit.fluxcd.io/v1", serve(apiResources("kustomize.toolkit.fluxcd.io/v1", "kustomizations")))
	mux.HandleFunc("/apis/helm.toolkit.fluxcd.io/v2", serve(apiResources("helm.toolkit.fluxcd.io/v2", "helmreleases")))
	mux.HandleFunc("/apis/apps/v1/deployments", serve(appsv1.DeploymentList{
		TypeMeta: metav1.TypeMeta{Kind: "DeploymentList", APIVersion: "apps/v1"},
	}))
	mux.HandleFunc("/apis/apps/v1/statefulsets", serve(appsv1.StatefulSetList{
		TypeMeta: metav1.TypeMeta{Kind: "StatefulSetList", APIVersion: "apps/v1"},
		Items: []appsv1.StatefulSet{{
			ObjectMeta: metav1.ObjectMeta{Name: "database", Namespace: "data"},
		}},
	}))
	mux.HandleFunc("/apis/apps/v1/daemonsets", serve(appsv1.DaemonSetList{
		TypeMeta: metav1.TypeMeta{Kind: "DaemonSetList", APIVersion: "apps/v1"},
	}))
	mux.HandleFunc("/api/v1/pods", clusterWidePods)
	mux.HandleFunc("/api/v1/namespaces/data/pods", serve(corev1.PodList{
		TypeMeta: metav1.TypeMeta{Kind: "PodList", APIVersion: "v1"},
	}))
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	directory := t.TempDir()
	kubeconfig := filepath.Join(directory, "kubeconfig")
	if err := os.WriteFile(kubeconfig, []byte(fmt.Sprintf(`apiVersion: v1
kind: Config
current-context: fake
clusters:
- name: fake
  cluster:
    server: %s
contexts:
- name: fake
  context:
    cluster: fake
    user: fake
users:
- name: fake
  user: {}
`, server.URL)), 0o600); err != nil {
		t.Fatalf("write the kubeconfig: %v", err)
	}

	return config.Config{
		Repository: domain.RepositoryRef{Owner: "operator", Name: "cluster"},
		GitHub:     config.GitHubConfig{TokenEnv: "TEST_GITHUB_TOKEN", MergeMethod: "squash"},
		Cluster:    config.ClusterConfig{Context: "fake", Kubeconfig: kubeconfig},
		Flux: config.FluxConfig{Source: config.ObjectRef{
			Kind: "GitRepository", Namespace: "flux-system", Name: "cluster",
		}},
		AI: config.AIConfig{
			Model:     "test-model",
			BaseURL:   "http://127.0.0.1:1/v1",
			APIKeyEnv: "TEST_AI_KEY",
		},
		Watch: config.WatchConfig{
			SettleTimeout: config.DefaultSettleTimeout,
			StabilityHold: config.DefaultStabilityHold,
			PollInterval:  config.DefaultPollInterval,
		},
		Paths: config.PathsConfig{
			HistoryDatabase:   filepath.Join(directory, "history.db"),
			CheckoutDirectory: filepath.Join(directory, "checkouts"),
		},
	}, server
}

func apiResources(groupVersion, resource string) metav1.APIResourceList {
	return metav1.APIResourceList{
		TypeMeta:     metav1.TypeMeta{Kind: "APIResourceList", APIVersion: "v1"},
		GroupVersion: groupVersion,
		APIResources: []metav1.APIResource{{Name: resource, Namespaced: true}},
	}
}

func serve(body any) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}
}

func forbidden(w http.ResponseWriter, _ *http.Request) {
	status(w, http.StatusForbidden, metav1.StatusReasonForbidden, "pods is forbidden")
}

func unavailable(w http.ResponseWriter, _ *http.Request) {
	status(w, http.StatusConflict, metav1.StatusReasonConflict, "the collection is being rebuilt")
}

func status(w http.ResponseWriter, code int, reason metav1.StatusReason, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(metav1.Status{
		TypeMeta: metav1.TypeMeta{Kind: "Status", APIVersion: "v1"},
		Status:   "Failure",
		Reason:   reason,
		Code:     int32(code),
		Message:  message,
	})
}

func TestAMissingCredentialNamesWhereToSetIt(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want []string
	}{
		{
			name: "the forge token",
			env:  map[string]string{},
			want: []string{"TEST_GITHUB_TOKEN", ".env", "github.tokenEnv"},
		},
		{
			name: "the model API key",
			env:  map[string]string{"TEST_GITHUB_TOKEN": "a-token"},
			want: []string{"TEST_AI_KEY", ".env", "ai.apiKeyEnv"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := config.Config{
				GitHub: config.GitHubConfig{TokenEnv: "TEST_GITHUB_TOKEN"},
				AI:     config.AIConfig{APIKeyEnv: "TEST_AI_KEY"},
			}

			_, err := New(context.Background(), config.Loaded{Config: cfg}, Dependencies{
				LookupEnv: environment(test.env),
				Out:       io.Discard,
				Log:       diagnostics.DiscardLogger(),
			}, run.Options{}, nil)
			if err == nil {
				t.Fatal("the run was built with no credential at all")
			}
			if got := domain.ErrorClassOf(err); got != domain.ErrorConfiguration {
				t.Fatalf("error class = %q, want %q", got, domain.ErrorConfiguration)
			}
			for _, want := range test.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error = %q, want it to mention %q", err, want)
				}
			}
		})
	}
}

func TestAMissingRegistryPasswordNamesWhichRegistryWantsIt(t *testing.T) {
	_, err := imageSource(config.Config{
		Registries: []config.RegistryConfig{
			{Host: "ghcr.io", Username: "bot", PasswordEnv: "TEST_GHCR_TOKEN"},
		},
	}, environment(map[string]string{}))

	if err == nil {
		t.Fatal("the registry client was built with no password at all")
	}
	if got := domain.ErrorClassOf(err); got != domain.ErrorConfiguration {
		t.Fatalf("error class = %q, want %q", got, domain.ErrorConfiguration)
	}
	for _, want := range []string{"TEST_GHCR_TOKEN", ".env", "ghcr.io", "passwordEnv"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error = %q, want it to mention %q", err, want)
		}
	}
}
