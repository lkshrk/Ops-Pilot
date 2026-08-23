// Package oci verifies immutable registry artifacts without trusting registry input.
package oci

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"time"

	"github.com/lkshrk/ops-pilot/internal/retry"
)

const (
	manifestLimit             = 4 << 20
	maxDescriptors            = 128
	maxManifestRequests       = 129
	maxManifestBytes          = 32 << 20
	maxBlobs                  = 16
	maxBlobBytes        int64 = 64 << 20
	maxArtifactBytes    int64 = 128 << 20
	manifestOCI               = "application/vnd.oci.image.manifest.v1+json"
	indexOCI                  = "application/vnd.oci.image.index.v1+json"
	manifestDocker            = "application/vnd.docker.distribution.manifest.v2+json"
	indexDocker               = "application/vnd.docker.distribution.manifest.list.v2+json"
	chartConfig               = "application/vnd.cncf.helm.config.v1+json"
	chartLayer                = "application/vnd.cncf.helm.chart.content.v1.tar+gzip"
	// docker.io and index.docker.io are catalogue names, not registry API endpoints; only
	// registry-1.docker.io answers /v2/.
	dockerHubAuthority = "registry-1.docker.io"
	dockerHubNamespace = "library"
	imageConfigOCI     = "application/vnd.oci.image.config.v1+json"
	imageConfigDocker  = "application/vnd.docker.container.image.v1+json"
	sourceAnnotation   = "org.opencontainers.image.source"
	revisionAnnotation = "org.opencontainers.image.revision"
	// A registry gets this long to start answering, plus one second per minRegistryThroughput
	// bytes it may return. A fixed whole-request deadline cannot both fail a silent registry
	// quickly and deliver the byte limits above.
	registryResponseTimeout = 10 * time.Second
	minRegistryThroughput   = 1 << 20
	// Per-request bounds multiply by the request cap — 129 manifests at registryResponseTimeout is
	// half an hour — so only a whole-call deadline bounds a resolve. It must stay comfortably above
	// maxRetryAfterBudget, or a registry directing legitimate backoff spends the whole window.
	maxCallDuration = 5 * time.Minute
)

var (
	// These are stable policy outcomes for callers. Invalid input references are
	// deliberately ordinary validation errors rather than policy outcomes.
	ErrUnavailable   = errors.New("OCI registry unavailable")
	ErrAuth          = errors.New("OCI registry authentication failed")
	ErrTrustBoundary = errors.New("OCI registry trust boundary violation")
	ErrIntegrity     = errors.New("OCI registry integrity verification failed")
)

func category(kind error, message string) error { return fmt.Errorf("%w: %s", kind, message) }
func knownCategory(err error) bool {
	return errors.Is(err, ErrUnavailable) || errors.Is(err, ErrAuth) || errors.Is(err, ErrTrustBoundary) || errors.Is(err, ErrIntegrity)
}
func contextCause(err error) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return nil
}
func unavailable(err error, message string) error {
	if cause := contextCause(err); cause != nil {
		return cause
	}
	return category(ErrUnavailable, message)
}
func integrity(err error, message string) error {
	if cause := contextCause(err); cause != nil {
		return cause
	}
	return category(ErrIntegrity, message)
}

type ClientOptions struct {
	HTTPClient                *http.Client
	TestOnlyRegistryAuthority netip.AddrPort
	TestOnlyCallTimeout       time.Duration
	Retry                     retry.Schedule
	// Credentials are keyed by registry authority and are only ever offered to
	// that authority's own token endpoint.
	Credentials map[string]Credential
}

type Client struct {
	http        *http.Client
	fixture     netip.AddrPort
	retry       retry.Schedule
	credentials map[string]Credential
	callTimeout time.Duration
}

// callDeadline bounds one whole client call. The cause is the client's own category error so the
// caller can tell this bound from its own cancellation.
func (c *Client) callDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	limit := c.callTimeout
	if limit <= 0 {
		limit = maxCallDuration
	}
	return context.WithTimeoutCause(ctx, limit, category(ErrUnavailable, "registry call exceeded its deadline"))
}

func New(options ClientOptions) (*Client, error) {
	credentials, err := checkedCredentials(options.Credentials)
	if err != nil {
		return nil, err
	}
	base := options.HTTPClient
	if base == nil {
		base = &http.Client{}
	}
	clone := *base
	transport, ok := base.Transport.(*http.Transport)
	if !ok || transport == nil {
		transport = http.DefaultTransport.(*http.Transport)
	}
	t := transport.Clone()
	t.Proxy = nil
	t.DisableCompression = true
	t.MaxResponseHeaderBytes = 32 << 10
	t.ResponseHeaderTimeout = registryResponseTimeout
	t.DialContext = safeDialer(options.TestOnlyRegistryAuthority)
	clone.Transport = t
	clone.Timeout = requestTimeout(maxBlobBytes)
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{http: &clone, fixture: options.TestOnlyRegistryAuthority, retry: options.Retry, credentials: credentials, callTimeout: options.TestOnlyCallTimeout}, nil
}
