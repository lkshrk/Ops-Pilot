// Package composition builds the production run from configuration.
package composition

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/lkshrk/ops-pilot/internal/adapters/flux"
	"github.com/lkshrk/ops-pilot/internal/adapters/github"
	"github.com/lkshrk/ops-pilot/internal/adapters/httpfetch"
	kube "github.com/lkshrk/ops-pilot/internal/adapters/kubernetes"
	"github.com/lkshrk/ops-pilot/internal/adapters/oci"
	"github.com/lkshrk/ops-pilot/internal/ai"
	"github.com/lkshrk/ops-pilot/internal/ai/openai"
	"github.com/lkshrk/ops-pilot/internal/changelog"
	"github.com/lkshrk/ops-pilot/internal/checkout"
	"github.com/lkshrk/ops-pilot/internal/cluster"
	"github.com/lkshrk/ops-pilot/internal/config"
	"github.com/lkshrk/ops-pilot/internal/diagnostics"
	"github.com/lkshrk/ops-pilot/internal/domain"
	"github.com/lkshrk/ops-pilot/internal/history"
	"github.com/lkshrk/ops-pilot/internal/run"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

const defaultGitHubAPI = "https://api.github.com"

var (
	buildClusterClients = clusterClients
	newRunner           = run.New
)

// run takes the base-branch reset as an optional upgrade, so losing it here is a
// silent loss of the reset rather than a compile error at the port.
var _ run.BaseWorkspace = (*checkout.Checkout)(nil)

type Dependencies struct {
	// Activity reports each tool the agent invokes, so a long assessment shows
	// progress instead of going silent.
	Activity  ai.Activity
	LookupEnv func(string) (string, bool)
	Out       io.Writer
	Log       *diagnostics.Logger
	Redactor  *diagnostics.Redactor
	Events    run.Events
	Now       func() time.Time
}

type Handle struct {
	Runner  *run.Runner
	History *history.Store
}

func (h *Handle) Close() error {
	if h.History == nil {
		return nil
	}
	return h.History.Close()
}

// New assembles the production runner. Secrets are read from the environment
// variables configuration names; none are ever read from the file itself.
func New(
	ctx context.Context,
	loaded config.Loaded,
	deps Dependencies,
	options run.Options,
	approver run.Approver,
) (*Handle, error) {
	cfg := loaded.Config
	if deps.LookupEnv == nil {
		deps.LookupEnv = os.LookupEnv
	}
	if deps.Now == nil {
		deps.Now = time.Now
	}

	token, found := deps.LookupEnv(cfg.GitHub.TokenEnv)
	if !found || token == "" {
		return nil, missingCredential(cfg.GitHub.TokenEnv, "github.tokenEnv")
	}
	apiKey, found := deps.LookupEnv(cfg.AI.APIKeyEnv)
	if !found || apiKey == "" {
		return nil, missingCredential(cfg.AI.APIKeyEnv, "ai.apiKeyEnv")
	}

	forge, err := github.New(&http.Client{Timeout: time.Minute}, token, defaultGitHubAPI, cfg.Repository)
	if err != nil {
		return nil, fmt.Errorf("build the GitHub client: %w", err)
	}

	fluxClient, workloads, err := buildClusterClients(cfg, deps.Log)
	if err != nil {
		return nil, err
	}
	observer := cluster.New(fluxClient, workloads, cluster.Options{
		SettleTimeout: cfg.Watch.SettleTimeout,
		StabilityHold: cfg.Watch.StabilityHold,
		PollInterval:  cfg.Watch.PollInterval,
		Now:           deps.Now,
	})

	images, err := imageSource(cfg, deps.LookupEnv)
	if err != nil {
		return nil, err
	}
	overrides := make([]changelog.Override, 0, len(cfg.Changelog.Overrides))
	for _, override := range cfg.Changelog.Overrides {
		overrides = append(overrides, changelog.Override{
			Dependency: override.Dependency,
			Repository: override.Repository,
		})
	}

	workspace := checkout.New(checkoutRoot(cfg), token, cfg.Repository)

	agent, err := openai.New(openai.Options{
		HTTPClient: &http.Client{Timeout: 10 * time.Minute},
		BaseURL:    cfg.AI.BaseURL,
		APIKey:     apiKey,
		Model:      cfg.AI.Model,
		Activity:   deps.Activity,
		Tools: &ai.Toolbox{
			Root:              workspace.Root(),
			GitHub:            forge,
			Cluster:           observer,
			Fetch:             fetcher{client: httpfetch.New(httpfetch.Options{}), allowedHosts: fetchAllowlist},
			AllowedFetchHosts: fetchAllowlist,
		},
	})
	if err != nil {
		return nil, fmt.Errorf("build the AI agent: %w", err)
	}

	store, err := history.Open(cfg.Paths.HistoryDatabase)
	if err != nil {
		return nil, err
	}

	options.Repository = cfg.Repository
	options.RevertedLabel = cfg.PullRequests.RevertedLabel
	options.DeclinedLabel = cfg.PullRequests.DeclinedLabel
	options.MergeMethod = cfg.GitHub.MergeMethod
	options.MaxFixAttempts = cfg.Watch.MaxFixAttempts
	options.FixAllowedPaths = cfg.Fixes.AllowedPaths
	options.SettleTimeout = cfg.Watch.SettleTimeout
	options.Filter = domain.PullRequestFilter{
		Authors: cfg.PullRequests.Authors,
		Labels:  cfg.PullRequests.Labels,
	}

	runner := newRunner(run.Dependencies{
		Forge:      forge,
		Observer:   observer,
		Agent:      agent,
		Changelogs: changelog.NewResolver(images, forge, overrides, deps.Log),
		Recorder:   store,
		Approver:   approver,
		Workspace:  workspace,
		Out:        deps.Out,
		Log:        deps.Log,
		Redactor:   deps.Redactor,
		Events:     deps.Events,
		Now:        deps.Now,
	}, options)

	return &Handle{Runner: runner, History: store}, nil
}

// Without the class this exits 2, not 1.
func missingCredential(variable, key string) error {
	return &domain.Error{
		Class:     domain.ErrorConfiguration,
		Operation: "resolve credential variables",
		Cause: fmt.Errorf(
			"environment variable %s is not set; export it, add it to ./.env, "+
				"or point %s at the variable you use", variable, key,
		),
	}
}

func checkoutRoot(cfg config.Config) string {
	if cfg.Paths.CheckoutDirectory == "" {
		return ""
	}
	return filepath.Join(cfg.Paths.CheckoutDirectory, cfg.Repository.Owner+"-"+cfg.Repository.Name)
}

func clusterClients(cfg config.Config, log *diagnostics.Logger) (*flux.Client, *kube.Client, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if cfg.Cluster.Kubeconfig != "" {
		rules.ExplicitPath = cfg.Cluster.Kubeconfig
	}
	restConfig, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		rules,
		&clientcmd.ConfigOverrides{CurrentContext: cfg.Cluster.Context},
	).ClientConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("load the kubeconfig context %q: %w", cfg.Cluster.Context, err)
	}
	restConfig.Wrap(func(base http.RoundTripper) http.RoundTripper {
		return kube.RetryReads(base, func(method, path string, retries int) {
			log.Debugf("retried %s %s %d time(s)", method, path, retries)
		})
	})
	dynamicClient, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("build the dynamic Kubernetes client: %w", err)
	}
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(restConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("build the Kubernetes discovery client: %w", err)
	}
	typedClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, nil, fmt.Errorf("build the Kubernetes client: %w", err)
	}
	fluxClient, err := flux.New(dynamicClient, discoveryClient, domain.ObjectRef(cfg.Flux.Source))
	if err != nil {
		return nil, nil, err
	}
	workloads, err := kube.New(typedClient, kube.WithDegradedReadObserver(reportOnce(log)))
	if err != nil {
		return nil, nil, err
	}
	return fluxClient, workloads, nil
}

// The observer fires every poll the degraded regime runs, so reports are keyed
// by reason: one line per regime rather than one per poll.
func reportOnce(log *diagnostics.Logger) func(string) {
	var mu sync.Mutex
	reported := map[string]bool{}
	return func(reason string) {
		mu.Lock()
		defer mu.Unlock()
		if reported[reason] {
			return
		}
		reported[reason] = true
		log.Warnf("degraded cluster read: %s", reason)
	}
}

// imageSource reads the org.opencontainers.image.source annotation of an image.
type imageSourceReader struct{ client *oci.Client }

func (r imageSourceReader) Resolve(ctx context.Context, reference string) (string, error) {
	metadata, err := r.client.Resolve(ctx, reference)
	if err != nil {
		return "", err
	}
	return metadata.Source, nil
}

func imageSource(cfg config.Config, lookupEnv func(string) (string, bool)) (changelog.ImageSourceReader, error) {
	credentials := map[string]oci.Credential{}
	for _, registry := range cfg.Registries {
		secret, found := lookupEnv(registry.PasswordEnv)
		if !found || secret == "" {
			return nil, missingCredential(
				registry.PasswordEnv, fmt.Sprintf("registries[%s].passwordEnv", registry.Host),
			)
		}
		credentials[registry.Host] = oci.Credential{Username: registry.Username, Secret: secret}
	}
	client, err := oci.New(oci.ClientOptions{
		HTTPClient:  &http.Client{Timeout: time.Minute},
		Credentials: credentials,
	})
	if err != nil {
		return nil, fmt.Errorf("build the registry client: %w", err)
	}
	return imageSourceReader{client: client}, nil
}

// fetchAllowlist must feed both the first-hop and per-hop gates; splitting the two reopens the redirect exfiltration hole.
var fetchAllowlist = []string{
	"api.github.com",
	"bitbucket.org",
	"codeberg.org",
	"gist.githubusercontent.com",
	"github.com",
	"gitlab.com",
	"objects.githubusercontent.com",
	"raw.githubusercontent.com",
}

type fetcher struct {
	client       *httpfetch.Client
	allowedHosts []string
}

func (f fetcher) Fetch(ctx context.Context, url string) ([]byte, error) {
	result, err := f.client.Fetch(ctx, url, httpfetch.Limits{AllowedHosts: f.allowedHosts})
	if err != nil {
		return nil, err
	}
	return result.Content, nil
}
