package oci

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lkshrk/ops-pilot/internal/netpolicy"
	"github.com/lkshrk/ops-pilot/internal/retry"
)

type ociRoundTrip func(*http.Request) (*http.Response, error)

func (f ociRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type ociTrackedBody struct {
	io.Reader
	closed   bool
	closeErr error
}

func (b *ociTrackedBody) Close() error { b.closed = true; return b.closeErr }

type ociErrReader struct{}

func (ociErrReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestRequestRetriesBoundedTransientBodyReadButNotStream(t *testing.T) {
	u, _ := url.Parse("https://registry.test/v2/a/manifests/x")
	first := &ociTrackedBody{Reader: ociErrReader{}}
	hits := 0
	c := &Client{http: &http.Client{Transport: ociRoundTrip(func(*http.Request) (*http.Response, error) {
		hits++
		if hits == 1 {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: first}, nil
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader([]byte("ok")))}, nil
	})}, retry: retry.Schedule{Sleep: func(context.Context, time.Duration) error { return nil }}}
	if _, body, _, err := c.request(context.Background(), http.MethodGet, u, "", 10); err != nil || string(body) != "ok" || hits != 2 || !first.closed {
		t.Fatalf("err=%v body=%q hits=%d closed=%v", err, body, hits, first.closed)
	}
	hits = 0
	stream := io.NopCloser(strings.NewReader("stream"))
	c.http.Transport = ociRoundTrip(func(*http.Request) (*http.Response, error) {
		hits++
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: stream}, nil
	})
	res, _, _, err := c.request(context.Background(), http.MethodGet, u, "", -1)
	if err != nil || hits != 1 || res.Body != stream {
		t.Fatalf("err=%v hits=%d", err, hits)
	}
	res.Body.Close()
	closeErr := errors.New("close failed")
	closed := &ociTrackedBody{Reader: strings.NewReader("ok"), closeErr: closeErr}
	c.http.Transport = ociRoundTrip(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: closed}, nil
	})
	if _, _, _, err := c.request(context.Background(), http.MethodGet, u, "", 10); !errors.Is(err, closeErr) || !closed.closed {
		t.Fatalf("err=%v closed=%v", err, closed.closed)
	}
}

func TestRequestPreservesCancellationCause(t *testing.T) {
	cause := errors.New("operator cancelled")
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(cause)
	u, _ := url.Parse("https://registry.test/v2/a/manifests/x")
	c := &Client{http: &http.Client{Transport: ociRoundTrip(func(*http.Request) (*http.Response, error) { t.Fatal("request made"); return nil, nil })}}
	_, _, _, err := c.request(ctx, http.MethodGet, u, "", 10)
	if !errors.Is(err, cause) || errors.Is(err, ErrUnavailable) || errors.Is(err, ErrIntegrity) {
		t.Fatalf("err=%v", err)
	}
}

func TestResolvePreservesCancellationCauseFromOperation(t *testing.T) {
	cause := errors.New("operator cancelled during request")
	ctx, cancel := context.WithCancelCause(context.Background())
	calls := 0
	c := &Client{http: &http.Client{Transport: ociRoundTrip(func(*http.Request) (*http.Response, error) {
		calls++
		cancel(cause)
		return nil, errors.New("transport failed")
	})}}
	_, err := c.Resolve(ctx, "registry.test/repo:tag")
	if !errors.Is(err, cause) || errors.Is(err, ErrUnavailable) || errors.Is(err, ErrIntegrity) || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

const imageManifestMediaType = "application/vnd.oci.image.manifest.v1+json"

func digest(raw []byte) string {
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestResolveVerifiesManifestAndNormalizesIdentity(t *testing.T) {
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","size":2},"layers":[],"annotations":{"org.opencontainers.image.source":"https://example.test/repo","org.opencontainers.image.revision":"abc"}}`)
	manifestDigest := digest(manifest)
	client, authority := newRegistryClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/":
			w.WriteHeader(http.StatusOK)
		case "/v2/repo/manifests/latest":
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			if r.Method == http.MethodGet {
				_, _ = w.Write(manifest)
			}
		default:
			t.Errorf("path = %s", r.URL.Path)
		}
	})
	got, err := client.Resolve(context.Background(), authority.String()+"/repo:latest")
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest != manifestDigest || got.Identity.Digest != manifestDigest || got.Identity.IndexDigest != "" || got.Identity.Reference != authority.String()+"/repo@"+manifestDigest || got.Source != "https://example.test/repo" || got.Revision != "abc" {
		t.Fatalf("metadata = %+v", got)
	}
}

func TestResolveReturnsSortedMultiPlatformIdentity(t *testing.T) {
	amd64 := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","annotations":{"platform":"amd64"}}`)
	arm64 := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","annotations":{"platform":"arm64"}}`)
	amd64Digest, arm64Digest := digest(amd64), digest(arm64)
	descriptors := fmt.Sprintf(
		`[{"mediaType":%q,"digest":%q,"size":%d,"platform":{"os":"linux","architecture":"arm64","variant":"v8"}},{"mediaType":%q,"digest":%q,"size":%d,"platform":{"os":"linux","architecture":"amd64"}}]`,
		imageManifestMediaType, arm64Digest, len(arm64), imageManifestMediaType, amd64Digest, len(amd64),
	)
	got, indexDigest, err := resolveIndexFixture(t, descriptors, map[string][]byte{amd64Digest: amd64, arm64Digest: arm64}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest != indexDigest || !strings.HasSuffix(got.Identity.Reference, "@"+indexDigest) || got.Identity.IndexDigest != indexDigest || got.Identity.Digest != "" {
		t.Fatalf("identity = %#v", got.Identity)
	}
	if len(got.Identity.Platforms) != 2 || got.Identity.Platforms[0].Architecture != "amd64" || got.Identity.Platforms[0].Digest != amd64Digest || got.Identity.Platforms[1].Architecture != "arm64" || got.Identity.Platforms[1].Variant != "v8" || got.Identity.Platforms[1].Digest != arm64Digest {
		t.Fatalf("platforms = %#v", got.Identity.Platforms)
	}
}

func TestResolveRejectsMissingOrNoncanonicalPlatform(t *testing.T) {
	child := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`)
	childDigest := digest(child)
	for name, platform := range map[string]string{
		"missing":            `{}`,
		"uppercase os":       `{"os":"Linux","architecture":"amd64"}`,
		"os whitespace":      `{"os":" linux","architecture":"amd64"}`,
		"uppercase arch":     `{"os":"linux","architecture":"AMD64"}`,
		"variant whitespace": `{"os":"linux","architecture":"arm64","variant":"v8 "}`,
	} {
		t.Run(name, func(t *testing.T) {
			descriptors := fmt.Sprintf(`[{"mediaType":%q,"digest":%q,"size":%d,"platform":%s}]`, imageManifestMediaType, childDigest, len(child), platform)
			if _, _, err := resolveIndexFixture(t, descriptors, map[string][]byte{childDigest: child}, ""); !errors.Is(err, ErrIntegrity) {
				t.Fatalf("error = %v, want integrity error", err)
			}
		})
	}
}

func TestResolveNormalizesPinnedDigestAndRejectsTampering(t *testing.T) {
	child := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`)
	childDigest := digest(child)
	descriptors := fmt.Sprintf(`[{"mediaType":%q,"digest":%q,"size":%d,"platform":{"os":"linux","architecture":"amd64"}}]`, imageManifestMediaType, childDigest, len(child))
	_, indexDigest, err := resolveIndexFixture(t, descriptors, map[string][]byte{childDigest: child}, "")
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := resolveIndexFixture(t, descriptors, map[string][]byte{childDigest: child}, strings.ToUpper(strings.TrimPrefix(indexDigest, "sha256:")))
	if err != nil {
		t.Fatal(err)
	}
	if got.Identity.IndexDigest != indexDigest || !strings.HasSuffix(got.Identity.Reference, "@"+indexDigest) {
		t.Fatalf("identity = %#v", got.Identity)
	}
	tampered := strings.Repeat("b", 64)
	if _, _, err := resolveIndexFixture(t, descriptors, map[string][]byte{childDigest: child}, tampered); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("tampered pinned reference error = %v", err)
	}
}

func newRegistryClient(t *testing.T, handler http.HandlerFunc) (*Client, netip.AddrPort) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	authority := netip.MustParseAddrPort(strings.TrimPrefix(server.URL, "http://"))
	client, err := New(ClientOptions{HTTPClient: server.Client(), TestOnlyRegistryAuthority: authority})
	if err != nil {
		t.Fatal(err)
	}
	return client, authority
}

func resolveIndexFixture(t *testing.T, descriptors string, children map[string][]byte, pinnedHex string) (Metadata, string, error) {
	t.Helper()
	index := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":` + descriptors + `}`)
	indexDigest := digest(index)
	client, authority := newRegistryClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/":
		case strings.HasPrefix(r.URL.Path, "/v2/repo/manifests/"):
			ref := strings.TrimPrefix(r.URL.Path, "/v2/repo/manifests/")
			if child, ok := children[ref]; ok {
				w.Header().Set("Docker-Content-Digest", ref)
				_, _ = w.Write(child)
				return
			}
			w.Header().Set("Docker-Content-Digest", indexDigest)
			if r.Method == http.MethodGet {
				_, _ = w.Write(index)
			}
		default:
			t.Errorf("path = %s", r.URL.Path)
		}
	})
	reference := authority.String() + "/repo:latest"
	if pinnedHex != "" {
		reference = authority.String() + "/repo@sha256:" + pinnedHex
	}
	got, err := client.Resolve(context.Background(), reference)
	return got, indexDigest, err
}

func TestResolveRejectsBadReferenceAndProductionHTTP(t *testing.T) {
	client, err := New(ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, reference := range []string{"repo", "registry/repo", "registry/repo@sha256:bad", "127.0.0.1:5000/repo:tag", "Registry/repo:tag", "registry/-repo:tag", "registry/repo/:tag", "registry/re:po:tag", "registry/Repo:tag"} {
		if _, err := client.Resolve(context.Background(), reference); err == nil {
			t.Fatalf("Resolve(%q) succeeded", reference)
		}
	}
}

func TestResolveExchangesBearerTokenWithoutLeakingAuthorization(t *testing.T) {
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","size":2},"layers":[]}`)
	manifestDigest := digest(manifest)
	var authority string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			if r.Header.Get("Authorization") != "" {
				t.Fatal("registry authorization leaked to token endpoint")
			}
			_, _ = w.Write([]byte(`{"access_token":"fixture-token"}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer fixture-token" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="http://`+authority+`/token",service="fixture",scope="repository:repo:pull"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/v2/repo/manifests/latest" {
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			if r.Method == http.MethodGet {
				_, _ = w.Write(manifest)
			}
		}
	}))
	defer server.Close()
	authority = strings.TrimPrefix(server.URL, "http://")
	client, err := New(ClientOptions{HTTPClient: server.Client(), TestOnlyRegistryAuthority: netip.MustParseAddrPort(authority)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Resolve(context.Background(), authority+"/repo:latest"); err != nil {
		t.Fatal(err)
	}
}

// TestTokenHostsPinsTheCredentialTrustSet fails on any change to tokenHosts, including an
// addition. Each row permanently authorises a new host to receive a registry's Basic
// credential, so widening the set must be a deliberate edit made in this test in the same
// commit — never a silent one-line map literal change.
func TestTokenHostsPinsTheCredentialTrustSet(t *testing.T) {
	want := map[string]string{dockerHubAuthority: "auth.docker.io"}
	if !reflect.DeepEqual(tokenHosts, want) {
		t.Fatalf("tokenHosts = %v, want %v; a change to the credential token-host trust set must update this test in the same commit", tokenHosts, want)
	}
}

func TestBearerChallengeActsOnlyOnA401(t *testing.T) {
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","size":2},"layers":[]}`)
	manifestDigest := digest(manifest)

	t.Run("challenge on a 200 success is ignored and no token is exchanged", func(t *testing.T) {
		var authority string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/token" {
				t.Error("token exchange attempted for a challenge advertised on a 2xx response")
				_, _ = w.Write([]byte(`{"access_token":"unwanted"}`))
				return
			}
			w.Header().Set("WWW-Authenticate", `Basic realm="http://`+authority+`/basic", Bearer realm="http://`+authority+`/token",service="fixture",scope="repository:repo:pull"`)
			switch r.URL.Path {
			case "/v2/":
				w.WriteHeader(http.StatusOK)
			case "/v2/repo/manifests/latest":
				w.Header().Set("Docker-Content-Digest", manifestDigest)
				if r.Method == http.MethodGet {
					_, _ = w.Write(manifest)
				}
			}
		}))
		defer server.Close()
		authority = strings.TrimPrefix(server.URL, "http://")
		client, err := New(ClientOptions{HTTPClient: server.Client(), TestOnlyRegistryAuthority: netip.MustParseAddrPort(authority)})
		if err != nil {
			t.Fatal(err)
		}
		got, err := client.Resolve(context.Background(), authority+"/repo:latest")
		if err != nil {
			t.Fatalf("Resolve = %v, want success without a token exchange", err)
		}
		if got.Digest != manifestDigest {
			t.Fatalf("digest = %q, want %q", got.Digest, manifestDigest)
		}
	})

	t.Run("challenge on a genuine 401 still exchanges a token", func(t *testing.T) {
		var authority string
		tokenExchanged := false
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/token" {
				tokenExchanged = true
				_, _ = w.Write([]byte(`{"access_token":"fixture-token"}`))
				return
			}
			if r.Header.Get("Authorization") != "Bearer fixture-token" {
				w.Header().Set("WWW-Authenticate", `Basic realm="http://`+authority+`/basic", Bearer realm="http://`+authority+`/token",service="fixture",scope="repository:repo:pull"`)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if r.URL.Path == "/v2/repo/manifests/latest" {
				w.Header().Set("Docker-Content-Digest", manifestDigest)
				if r.Method == http.MethodGet {
					_, _ = w.Write(manifest)
				}
			}
		}))
		defer server.Close()
		authority = strings.TrimPrefix(server.URL, "http://")
		client, err := New(ClientOptions{HTTPClient: server.Client(), TestOnlyRegistryAuthority: netip.MustParseAddrPort(authority)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.Resolve(context.Background(), authority+"/repo:latest"); err != nil {
			t.Fatalf("Resolve = %v, want the 401 challenge exchanged for a token", err)
		}
		if !tokenExchanged {
			t.Fatal("token endpoint was never contacted for a genuine 401 challenge")
		}
	})
}

func TestResolvePreservesCancellationCauseFromBearerTokenRequest(t *testing.T) {
	cause := errors.New("operator cancelled token request")
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	calls := 0
	client := &Client{http: &http.Client{Transport: ociRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path == "/token" {
			cancel(cause)
			return nil, errors.New("token transport failed")
		}
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Header:     http.Header{"Www-Authenticate": {`Bearer realm="https://tokens.test/token",service="registry.test",scope="repository:repo:pull"`}},
			Body:       io.NopCloser(strings.NewReader("")),
		}, nil
	})}}
	// Ping, then the manifest challenge, then the token request that is cancelled: the ping does
	// not exchange its placeholder challenge for a token.
	_, err := client.Resolve(ctx, "registry.test/repo:tag")
	if !errors.Is(err, cause) || errors.Is(err, ErrUnavailable) || errors.Is(err, ErrIntegrity) || errors.Is(err, ErrAuth) || calls != 3 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}

func TestVerifyArtifactReturnsSortedProofOnlyAfterAllBlobsVerify(t *testing.T) {
	config, layer := []byte("{}"), []byte("chart")
	configDigest, layerDigest := digest(config), digest(layer)
	manifest := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":"application/vnd.cncf.helm.config.v1+json","digest":%q,"size":%d},"layers":[{"mediaType":"application/vnd.cncf.helm.chart.content.v1.tar+gzip","digest":%q,"size":%d}]}`, imageManifestMediaType, configDigest, len(config), layerDigest, len(layer)))
	manifestDigest := digest(manifest)
	client, authority := newRegistryClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/":
		case "/v2/charts/app/manifests/1.0.0":
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			if r.Method == http.MethodGet {
				_, _ = w.Write(manifest)
			}
		case "/v2/charts/app/blobs/" + configDigest:
			_, _ = w.Write(config)
		case "/v2/charts/app/blobs/" + layerDigest:
			_, _ = w.Write(layer)
		default:
			t.Errorf("path = %s", r.URL.Path)
		}
	})
	got, err := client.VerifyArtifact(context.Background(), authority.String()+"/charts/app:1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Blobs) != 2 || got.Blobs[0].Kind != "config" || got.Blobs[0].Digest != configDigest || got.Blobs[1].Kind != "layer" || got.Blobs[1].Digest != layerDigest {
		t.Fatalf("proof = %+v", got.Blobs)
	}
}

func TestVerifyArtifactRetriesRetryableBlobResponse(t *testing.T) {
	config, layer := []byte("{}"), []byte("chart")
	configDigest, layerDigest := digest(config), digest(layer)
	manifest := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":"application/vnd.cncf.helm.config.v1+json","digest":%q,"size":%d},"layers":[{"mediaType":"application/vnd.cncf.helm.chart.content.v1.tar+gzip","digest":%q,"size":%d}]}`, imageManifestMediaType, configDigest, len(config), layerDigest, len(layer)))
	configRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/":
		case "/v2/charts/app/manifests/1.0.0":
			w.Header().Set("Docker-Content-Digest", digest(manifest))
			if r.Method == http.MethodGet {
				_, _ = w.Write(manifest)
			}
		case "/v2/charts/app/blobs/" + configDigest:
			configRequests++
			if configRequests == 1 {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write(config)
		case "/v2/charts/app/blobs/" + layerDigest:
			_, _ = w.Write(layer)
		default:
			t.Errorf("path = %s", r.URL.Path)
		}
	}))
	defer server.Close()
	authority := netip.MustParseAddrPort(strings.TrimPrefix(server.URL, "http://"))
	client, err := New(ClientOptions{
		HTTPClient:                server.Client(),
		TestOnlyRegistryAuthority: authority,
		Retry:                     retry.Schedule{Sleep: func(context.Context, time.Duration) error { return nil }},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.VerifyArtifact(context.Background(), authority.String()+"/charts/app:1.0.0"); err != nil || configRequests != 2 {
		t.Fatalf("err=%v config requests=%d", err, configRequests)
	}
}

func TestVerifyArtifactRejectsImageConfigAndLeavesNoPartialProof(t *testing.T) {
	config, layer := []byte("{}"), []byte("chart")
	manifest := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":%d},"layers":[{"mediaType":"application/vnd.cncf.helm.chart.content.v1.tar+gzip","digest":%q,"size":%d}]}`, imageManifestMediaType, digest(config), len(config), digest(layer), len(layer)))
	client, authority := newRegistryClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/":
		case "/v2/charts/app/manifests/1":
			w.Header().Set("Docker-Content-Digest", digest(manifest))
			if r.Method == http.MethodGet {
				_, _ = w.Write(manifest)
			}
		default:
			t.Fatalf("unexpected blob request %s", r.URL.Path)
		}
	})
	got, err := client.VerifyArtifact(context.Background(), authority.String()+"/charts/app:1")
	if err == nil || len(got.Blobs) != 0 {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

func TestEachChartBlobDisagreementKeepsTheBlobOutOfTheProof(t *testing.T) {
	layer, other := []byte("chart"), []byte("forged")
	config := []byte("{}")
	cases := []struct {
		name         string
		layerDigest  string
		layerSize    int
		servedDigest string
	}{
		{name: "size disagrees with the served body", layerDigest: digest(layer), layerSize: len(layer) + 1, servedDigest: digest(layer)},
		{name: "content digest disagrees with the served body", layerDigest: digest(other), layerSize: len(layer)},
		{name: "header digest disagrees with the served body", layerDigest: digest(layer), layerSize: len(layer), servedDigest: digest(other)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manifest := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":"application/vnd.cncf.helm.config.v1+json","digest":%q,"size":%d},"layers":[{"mediaType":"application/vnd.cncf.helm.chart.content.v1.tar+gzip","digest":%q,"size":%d}]}`, imageManifestMediaType, digest(config), len(config), tc.layerDigest, tc.layerSize))
			client, authority := newRegistryClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v2/":
				case "/v2/charts/app/manifests/1.0.0":
					w.Header().Set("Docker-Content-Digest", digest(manifest))
					if r.Method == http.MethodGet {
						_, _ = w.Write(manifest)
					}
				case "/v2/charts/app/blobs/" + digest(config):
					w.Header().Set("Docker-Content-Digest", digest(config))
					_, _ = w.Write(config)
				case "/v2/charts/app/blobs/" + tc.layerDigest:
					if tc.servedDigest != "" {
						w.Header().Set("Docker-Content-Digest", tc.servedDigest)
					}
					_, _ = w.Write(layer)
				default:
					t.Errorf("path = %s", r.URL.Path)
				}
			})
			got, err := client.VerifyArtifact(context.Background(), authority.String()+"/charts/app:1.0.0")
			if err == nil || !errors.Is(err, ErrIntegrity) || len(got.Blobs) != 0 {
				t.Fatalf("got=%+v err=%v", got, err)
			}
			// Each case leaves exactly one of the three comparisons disagreeing, so any other
			// message means a different check caught it and this one is unpinned.
			if !strings.Contains(err.Error(), "chart blob verification failed") {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestClientCapsResponseHeaders(t *testing.T) {
	client, authority := newRegistryClient(t, func(w http.ResponseWriter, r *http.Request) { w.Header().Set("X-Large", strings.Repeat("x", 33<<10)) })
	if _, err := client.Resolve(context.Background(), authority.String()+"/repo:tag"); err == nil {
		t.Fatal("oversized headers accepted")
	}
}

func TestFixtureAuthorityDoesNotBlockPublicNumericAddresses(t *testing.T) {
	fixture := netip.MustParseAddrPort("127.0.0.1:5000")
	if !allowed(netip.MustParseAddr("8.8.8.8"), "443", fixture) || allowed(netip.MustParseAddr("127.0.0.2"), "5000", fixture) || !allowed(netip.MustParseAddr("127.0.0.1"), "5000", fixture) {
		t.Fatal("fixture allowance is not exact and additive")
	}
}

func TestAllowedRefusesPrivateAndSpecialUseWithoutAFixture(t *testing.T) {
	for _, raw := range []string{"10.0.0.1", "192.168.1.5", "169.254.169.254", "4000::1", "fc00::1"} {
		if allowed(netip.MustParseAddr(raw), "443", netip.AddrPort{}) {
			t.Errorf("%s allowed as a registry destination", raw)
		}
	}
	if !allowed(netip.MustParseAddr("8.8.8.8"), "443", netip.AddrPort{}) {
		t.Fatal("an ordinary public address was refused, so the cases above prove nothing")
	}
}

func TestPublicRejectsSpecialUseIPv6AndMixedAnswers(t *testing.T) {
	for _, raw := range []string{"64:ff9b:1::1", "100::1", "100:0:0:1::1", "2001::1", "2002::1", "3fff::1", "4000::1", "5f00::1"} {
		if netpolicy.Public(netip.MustParseAddr(raw)) {
			t.Fatalf("special-use %s accepted", raw)
		}
	}
	if !allPublic([]netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("2001:4860:4860::8888")}) {
		t.Fatal("ordinary public answer set rejected")
	}
	if allPublic([]netip.Addr{netip.MustParseAddr("8.8.8.8"), netip.MustParseAddr("64:ff9b:1::1")}) {
		t.Fatal("mixed public/special DNS answer set accepted")
	}
}

func TestResolveRejectsIndexChildSizeAndDuplicateDescriptors(t *testing.T) {
	child := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","size":2},"layers":[]}`)
	childDigest := digest(child)
	for name, descriptors := range map[string]string{
		"size":      fmt.Sprintf(`[{"mediaType":%q,"digest":%q,"size":%d,"platform":{"os":"linux","architecture":"amd64"}}]`, imageManifestMediaType, childDigest, len(child)+1),
		"duplicate": fmt.Sprintf(`[{"mediaType":%q,"digest":%q,"size":%d,"platform":{"os":"linux","architecture":"amd64"}},{"mediaType":%q,"digest":%q,"size":%d,"platform":{"os":"linux","architecture":"arm64"}}]`, imageManifestMediaType, childDigest, len(child), imageManifestMediaType, childDigest, len(child)),
	} {
		t.Run(name, func(t *testing.T) {
			index := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":` + descriptors + `}`)
			client, authority := newRegistryClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v2/":
				case "/v2/repo/manifests/latest":
					w.Header().Set("Docker-Content-Digest", digest(index))
					if r.Method == http.MethodGet {
						_, _ = w.Write(index)
					}
				case "/v2/repo/manifests/" + childDigest:
					w.Header().Set("Docker-Content-Digest", childDigest)
					_, _ = w.Write(child)
				}
			})
			if _, err := client.Resolve(context.Background(), authority.String()+"/repo:latest"); !errors.Is(err, ErrIntegrity) {
				t.Fatalf("error = %v, want integrity error", err)
			}
		})
	}
}

func TestResolveErrorCategories(t *testing.T) {
	t.Run("digest mismatch", func(t *testing.T) {
		manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`)
		client, authority := newRegistryClient(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v2/repo/manifests/latest" {
				w.Header().Set("Docker-Content-Digest", "sha256:"+strings.Repeat("a", 64))
				if r.Method == http.MethodGet {
					_, _ = w.Write(manifest)
				}
			}
		})
		if _, err := client.Resolve(context.Background(), authority.String()+"/repo:latest"); !errors.Is(err, ErrIntegrity) {
			t.Fatalf("error = %v", err)
		}
	})
	// A registry that re-challenges a request already carrying a token is refusing access — a
	// private or absent repository — which must degrade the proposal, not fail the run.
	t.Run("re-challenge after token", func(t *testing.T) {
		var authority string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/token" {
				_, _ = w.Write([]byte(`{"token":"x"}`))
				return
			}
			w.Header().Set("WWW-Authenticate", `Bearer realm="http://`+authority+`/token"`)
			w.WriteHeader(http.StatusUnauthorized)
		}))
		defer server.Close()
		authority = strings.TrimPrefix(server.URL, "http://")
		client, _ := New(ClientOptions{HTTPClient: server.Client(), TestOnlyRegistryAuthority: netip.MustParseAddrPort(authority)})
		if _, err := client.Resolve(context.Background(), authority+"/repo:latest"); !errors.Is(err, ErrAuth) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("redirect cap", func(t *testing.T) {
		var authority string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Location", "http://"+authority+"/v2/")
			w.WriteHeader(http.StatusFound)
		}))
		defer server.Close()
		authority = strings.TrimPrefix(server.URL, "http://")
		client, _ := New(ClientOptions{HTTPClient: server.Client(), TestOnlyRegistryAuthority: netip.MustParseAddrPort(authority)})
		if _, err := client.Resolve(context.Background(), authority+"/repo:latest"); !errors.Is(err, ErrTrustBoundary) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("transport", func(t *testing.T) {
		server := httptest.NewServer(http.NotFoundHandler())
		authority := netip.MustParseAddrPort(strings.TrimPrefix(server.URL, "http://"))
		server.Close()
		client, _ := New(ClientOptions{HTTPClient: server.Client(), TestOnlyRegistryAuthority: authority})
		if _, err := client.Resolve(context.Background(), authority.String()+"/repo:latest"); !errors.Is(err, ErrUnavailable) {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestResolveRejectsConflictingPlatformAsIntegrity(t *testing.T) {
	childA := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`)
	childB := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","annotations":{"x":"y"}}`)
	a, b := digest(childA), digest(childB)
	index := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[{"mediaType":%q,"digest":%q,"size":%d,"platform":{"os":"linux","architecture":"amd64"}},{"mediaType":%q,"digest":%q,"size":%d,"platform":{"os":"linux","architecture":"amd64"}}]}`, imageManifestMediaType, a, len(childA), imageManifestMediaType, b, len(childB)))
	client, authority := newRegistryClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/":
		case "/v2/repo/manifests/latest":
			w.Header().Set("Docker-Content-Digest", digest(index))
			if r.Method == http.MethodGet {
				_, _ = w.Write(index)
			}
		case "/v2/repo/manifests/" + a:
			w.Header().Set("Docker-Content-Digest", a)
			_, _ = w.Write(childA)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	})
	if _, err := client.Resolve(context.Background(), authority.String()+"/repo:latest"); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("error = %v", err)
	}
}

func TestVerifyArtifactPreservesBlobOperationCategories(t *testing.T) {
	for name, want := range map[string]error{"auth": ErrAuth, "unavailable": ErrUnavailable, "trust": ErrTrustBoundary, "zero size": ErrIntegrity} {
		t.Run(name, func(t *testing.T) {
			config, layer := []byte("{}"), []byte("chart")
			configDigest, layerDigest := digest(config), digest(layer)
			configSize := len(config)
			if name == "zero size" {
				configSize = 0
			}
			manifest := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":"application/vnd.cncf.helm.config.v1+json","digest":%q,"size":%d},"layers":[{"mediaType":"application/vnd.cncf.helm.chart.content.v1.tar+gzip","digest":%q,"size":%d}]}`, imageManifestMediaType, configDigest, configSize, layerDigest, len(layer)))
			client, authority := newRegistryClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v2/":
				case "/v2/charts/app/manifests/1":
					w.Header().Set("Docker-Content-Digest", digest(manifest))
					if r.Method == http.MethodGet {
						_, _ = w.Write(manifest)
					}
				case "/v2/charts/app/blobs/" + configDigest:
					_, _ = w.Write(config)
				case "/v2/charts/app/blobs/" + layerDigest:
					switch name {
					case "auth":
						w.WriteHeader(http.StatusUnauthorized)
						_, _ = w.Write([]byte(strings.Repeat("x", len(layer)+1)))
					case "unavailable":
						w.WriteHeader(http.StatusInternalServerError)
					case "trust":
						w.Header().Set("Location", "http://127.0.0.2:5000/blob")
						w.WriteHeader(http.StatusFound)
					}
				}
			})
			got, err := client.VerifyArtifact(context.Background(), authority.String()+"/charts/app:1")
			if !errors.Is(err, want) || len(got.Blobs) != 0 {
				t.Fatalf("got=%+v err=%v, want %v", got, err, want)
			}
		})
	}
}

func TestTokenPreservesTrustAndIntegrityCategories(t *testing.T) {
	for name, want := range map[string]error{"private realm": ErrTrustBoundary, "oversized response": ErrIntegrity} {
		t.Run(name, func(t *testing.T) {
			var authority string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					_, _ = w.Write([]byte(`{"token":"` + strings.Repeat("x", manifestLimit) + `"}`))
					return
				}
				realm := "https://localhost/token"
				if name == "oversized response" {
					realm = "http://" + authority + "/token"
				}
				w.Header().Set("WWW-Authenticate", `Bearer realm="`+realm+`"`)
				w.WriteHeader(http.StatusUnauthorized)
			}))
			defer server.Close()
			authority = strings.TrimPrefix(server.URL, "http://")
			client, _ := New(ClientOptions{HTTPClient: server.Client(), TestOnlyRegistryAuthority: netip.MustParseAddrPort(authority)})
			if _, err := client.Resolve(context.Background(), authority+"/repo:tag"); !errors.Is(err, want) {
				t.Fatalf("error = %v, want %v", err, want)
			}
		})
	}
}

func TestResolveHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client, _ := New(ClientOptions{TestOnlyRegistryAuthority: netip.MustParseAddrPort("127.0.0.1:5000")})
	if _, err := client.Resolve(ctx, "127.0.0.1:5000/repo:tag"); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

func TestResolvePreservesDeadlineExceeded(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	client, _ := New(ClientOptions{TestOnlyRegistryAuthority: netip.MustParseAddrPort("127.0.0.1:5000")})
	if _, err := client.Resolve(ctx, "127.0.0.1:5000/repo:tag"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v", err)
	}
}

func TestRequestPreservesBodyReadCancellation(t *testing.T) {
	started := make(chan struct{})
	client, authority := newRegistryClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, _, _, err := client.request(ctx, http.MethodGet, &url.URL{Scheme: "http", Host: authority.String(), Path: "/body"}, "", manifestLimit)
		result <- err
	}()
	<-started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

func TestResolveMapsTruncatedResponseToUnavailable(t *testing.T) {
	client, authority := newRegistryClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/":
		case "/v2/repo/manifests/tag":
			if r.Method == http.MethodHead {
				return
			}
			conn, buffer, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Fatal(err)
			}
			_, _ = buffer.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 20\r\n\r\nshort")
			_ = buffer.Flush()
			_ = conn.Close()
		}
	})
	if _, err := client.Resolve(context.Background(), authority.String()+"/repo:tag"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("error = %v", err)
	}
}

func TestVerifyArtifactPreservesBlobStreamFailures(t *testing.T) {
	for name, want := range map[string]error{"canceled": context.Canceled, "truncated": ErrUnavailable} {
		t.Run(name, func(t *testing.T) {
			config, layer := []byte("{}"), []byte("chart")
			configDigest, layerDigest := digest(config), digest(layer)
			manifest := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":"application/vnd.cncf.helm.config.v1+json","digest":%q,"size":%d},"layers":[{"mediaType":"application/vnd.cncf.helm.chart.content.v1.tar+gzip","digest":%q,"size":%d}]}`, imageManifestMediaType, configDigest, len(config), layerDigest, len(layer)))
			started := make(chan struct{})
			client, authority := newRegistryClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v2/":
				case "/v2/charts/app/manifests/1":
					w.Header().Set("Docker-Content-Digest", digest(manifest))
					if r.Method == http.MethodGet {
						_, _ = w.Write(manifest)
					}
				case "/v2/charts/app/blobs/" + configDigest:
					_, _ = w.Write(config)
				case "/v2/charts/app/blobs/" + layerDigest:
					if name == "truncated" {
						conn, buffer, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Fatal(err)
						}
						_, _ = buffer.WriteString("HTTP/1.1 200 OK\r\nContent-Length: 10\r\n\r\nshort")
						_ = buffer.Flush()
						_ = conn.Close()
						return
					}
					w.WriteHeader(http.StatusOK)
					w.(http.Flusher).Flush()
					close(started)
					<-r.Context().Done()
				}
			})
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(context.Canceled)
			result := make(chan struct {
				got ArtifactVerification
				err error
			}, 1)
			go func() {
				got, err := client.VerifyArtifact(ctx, authority.String()+"/charts/app:1")
				result <- struct {
					got ArtifactVerification
					err error
				}{got, err}
			}()
			if name == "canceled" {
				<-started
				cancel(context.Canceled)
			}
			out := <-result
			if !errors.Is(out.err, want) || len(out.got.Blobs) != 0 {
				t.Fatalf("got=%+v err=%v, want %v", out.got, out.err, want)
			}
		})
	}
}

func TestVerifyArtifactPreservesOperationCancellationCause(t *testing.T) {
	started := make(chan struct{})
	client, authority := newRegistryClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v2/" {
			close(started)
			<-r.Context().Done()
		}
	})
	cause := errors.New("cancel manifest")
	ctx, cancel := context.WithCancelCause(context.Background())
	result := make(chan error, 1)
	go func() { _, err := client.VerifyArtifact(ctx, authority.String()+"/charts/app:1"); result <- err }()
	<-started
	cancel(cause)
	err := <-result
	if !errors.Is(err, cause) || errors.Is(err, ErrUnavailable) || errors.Is(err, ErrIntegrity) {
		t.Fatalf("err=%v", err)
	}
}

func TestParseReferenceSeparatesTagFromDigest(t *testing.T) {
	const digest = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	got, err := parseReference("ghcr.io/home-operations/gatus-sidecar:0.4.0@" + digest)
	if err != nil {
		t.Fatal(err)
	}
	if got.authority != "ghcr.io" || got.name != "home-operations/gatus-sidecar" ||
		got.tag != "0.4.0" || got.digest != digest {
		t.Fatalf("reference = %+v", got)
	}
	// The digest identifies the artifact even when a tag is present.
	if got.ref() != digest {
		t.Fatalf("ref() = %q", got.ref())
	}
	if got.normalized() != "ghcr.io/home-operations/gatus-sidecar@"+digest {
		t.Fatalf("normalized() = %q", got.normalized())
	}
	if _, err := parseReference("ghcr.io/acme/app:@" + digest); err == nil {
		t.Fatal("empty tag accepted")
	}
}

// ghcr.io answers an unauthenticated /v2/ ping with a placeholder scope naming no real
// repository; exchanging it for a token is denied and must never be attempted.
func TestResolveIgnoresPlaceholderScopeFromRegistryPing(t *testing.T) {
	var scopes []string
	client := &Client{http: &http.Client{Transport: ociRoundTrip(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.URL.Path == "/token":
			scopes = append(scopes, r.URL.Query().Get("scope"))
			return nil, errors.New("token requested")
		case r.URL.Path == "/v2/":
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Header:     http.Header{"Www-Authenticate": {`Bearer realm="https://registry.test/token",service="registry.test",scope="repository:user/image:pull"`}},
				Body:       io.NopCloser(strings.NewReader("")),
			}, nil
		default:
			return &http.Response{
				StatusCode: http.StatusUnauthorized,
				Header:     http.Header{"Www-Authenticate": {`Bearer realm="https://registry.test/token",service="registry.test",scope="repository:acme/app:pull"`}},
				Body:       io.NopCloser(strings.NewReader("")),
			}, nil
		}
	})}}
	if _, err := client.Resolve(context.Background(), "registry.test/acme/app:1.0.0"); err == nil {
		t.Fatal("expected the stubbed token request to fail")
	}
	for _, scope := range scopes {
		if scope == "repository:user/image:pull" {
			t.Fatalf("token requested for the ping placeholder scope: %v", scopes)
		}
	}
	if len(scopes) != 1 || scopes[0] != "repository:acme/app:pull" {
		t.Fatalf("scopes = %v, want only the manifest scope", scopes)
	}
}

// Build tooling attaches provenance and SBOM manifests to an index as unknown/unknown platforms.
// Several may share that placeholder without describing conflicting images.
func TestResolveIgnoresAttestationManifestsInIndex(t *testing.T) {
	amd64 := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","annotations":{"platform":"amd64"}}`)
	arm64 := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","annotations":{"platform":"arm64"}}`)
	provenance := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","annotations":{"kind":"provenance"}}`)
	sbom := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","annotations":{"kind":"sbom"}}`)
	amd64Digest, arm64Digest := digest(amd64), digest(arm64)
	provenanceDigest, sbomDigest := digest(provenance), digest(sbom)

	descriptors := fmt.Sprintf(
		`[{"mediaType":%q,"digest":%q,"size":%d,"platform":{"os":"linux","architecture":"amd64"}},`+
			`{"mediaType":%q,"digest":%q,"size":%d,"platform":{"os":"unknown","architecture":"unknown"},"annotations":{"vnd.docker.reference.type":"attestation-manifest"}},`+
			`{"mediaType":%q,"digest":%q,"size":%d,"platform":{"os":"linux","architecture":"arm64"}},`+
			`{"mediaType":%q,"digest":%q,"size":%d,"platform":{"os":"unknown","architecture":"unknown"},"annotations":{"vnd.docker.reference.type":"attestation-manifest"}}]`,
		imageManifestMediaType, amd64Digest, len(amd64),
		imageManifestMediaType, provenanceDigest, len(provenance),
		imageManifestMediaType, arm64Digest, len(arm64),
		imageManifestMediaType, sbomDigest, len(sbom),
	)
	got, indexDigest, err := resolveIndexFixture(t, descriptors, map[string][]byte{
		amd64Digest: amd64, arm64Digest: arm64,
		provenanceDigest: provenance, sbomDigest: sbom,
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Identity.IndexDigest != indexDigest {
		t.Fatalf("index digest = %q, want %q", got.Identity.IndexDigest, indexDigest)
	}
	if len(got.Identity.Platforms) != 2 {
		t.Fatalf("platforms = %#v, want only the two runnable images", got.Identity.Platforms)
	}
	for _, platform := range got.Identity.Platforms {
		if platform.OS == "unknown" || platform.Architecture == "unknown" {
			t.Fatalf("attestation surfaced as a platform: %#v", got.Identity.Platforms)
		}
	}
}

func TestResolveRejectsIndexOfOnlyAttestations(t *testing.T) {
	provenance := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","annotations":{"kind":"provenance"}}`)
	provenanceDigest := digest(provenance)
	descriptors := fmt.Sprintf(
		`[{"mediaType":%q,"digest":%q,"size":%d,"platform":{"os":"unknown","architecture":"unknown"},"annotations":{"vnd.docker.reference.type":"attestation-manifest"}}]`,
		imageManifestMediaType, provenanceDigest, len(provenance),
	)
	_, _, err := resolveIndexFixture(t, descriptors, map[string][]byte{provenanceDigest: provenance}, "")
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("err = %v, want an integrity failure", err)
	}
}

// A well-formed tag must never excuse a malformed digest: the digest reaches the request path
// verbatim, so path segments smuggled into it would address a different artifact.
func TestParseReferenceRejectsMalformedDigestBesideValidTag(t *testing.T) {
	for _, raw := range []string{
		"ghcr.io/ns/app:1.2.3@sha256:../../../v2/other/manifests/latest",
		"ghcr.io/ns/app:1.2.3@sha256:short",
		"ghcr.io/ns/app:1.2.3@sha256:" + strings.Repeat("g", 64),
	} {
		if got, err := parseReference(raw); err == nil {
			t.Fatalf("parseReference(%q) accepted a malformed digest: %+v", raw, got)
		}
	}
	if _, err := parseReference("ghcr.io/ns/app:BADTAG!@sha256:" + strings.Repeat("a", 64)); err == nil {
		t.Fatal("accepted a malformed tag beside a valid digest")
	}
}

func TestReferenceWithoutARegistryHostResolvesAgainstDockerHub(t *testing.T) {
	for _, testCase := range []struct{ raw, authority, name, tag string }{
		{"grafana/grafana:11.0.0", "registry-1.docker.io", "grafana/grafana", "11.0.0"},
		{"nginx:1.27", "registry-1.docker.io", "library/nginx", "1.27"},
		{"docker.io/grafana/grafana:11.0.0", "registry-1.docker.io", "grafana/grafana", "11.0.0"},
		{"index.docker.io/nginx:1.27", "registry-1.docker.io", "library/nginx", "1.27"},
		{"registry-1.docker.io/library/nginx:1.27", "registry-1.docker.io", "library/nginx", "1.27"},
		{"ghcr.io/org/app:1.0", "ghcr.io", "org/app", "1.0"},
		{"localhost:5000/app:1.0", "localhost:5000", "app", "1.0"},
		{"localhost/app:1.0", "localhost", "app", "1.0"},
		{"registry.example.org:5000/team/app:1.0", "registry.example.org:5000", "team/app", "1.0"},
	} {
		t.Run(testCase.raw, func(t *testing.T) {
			got, err := parseReference(testCase.raw)
			if err != nil {
				t.Fatalf("parseReference(%q) = %v", testCase.raw, err)
			}
			if got.authority != testCase.authority || got.name != testCase.name || got.tag != testCase.tag {
				t.Fatalf("parseReference(%q) = %+v, want authority %q name %q tag %q", testCase.raw, got, testCase.authority, testCase.name, testCase.tag)
			}
		})
	}
}

func TestReferencesAcceptEveryTagAndNameTheDistributionSpecAllows(t *testing.T) {
	for _, raw := range []string{
		"minio/minio:RELEASE.2025-04-22T22-12-26Z",
		"quay.io/org/app:V1.2.3",
		"quay.io/org/app:_leading_underscore",
		"quay.io/org/app:1.2.3-Beta_1",
		"quay.io/some__namespace/app:1.0",
		"quay.io/a--b/app:1.0",
		"quay.io/org/app:" + strings.Repeat("a", 128),
	} {
		if _, err := parseReference(raw); err != nil {
			t.Errorf("parseReference(%q) = %v, want a legal reference", raw, err)
		}
	}
	for _, raw := range []string{
		"quay.io/org/app:.leadingdot",
		"quay.io/org/app:-leadingdash",
		"quay.io/org/app:bad!tag",
		"quay.io/org/app:" + strings.Repeat("a", 129),
		"quay.io/_org/app:1.0",
		"quay.io/org-/app:1.0",
		"quay.io/ORG/app:1.0",
	} {
		if got, err := parseReference(raw); err == nil {
			t.Errorf("parseReference(%q) accepted an illegal reference: %+v", raw, got)
		}
	}
}

func TestPlatformTokensStayStricterThanRepositoryNames(t *testing.T) {
	child := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json"}`)
	childDigest := digest(child)
	descriptors := fmt.Sprintf(`[{"mediaType":%q,"digest":%q,"size":%d,"platform":{"os":"linux","architecture":"amd__64"}}]`, imageManifestMediaType, childDigest, len(child))
	if _, _, err := resolveIndexFixture(t, descriptors, map[string][]byte{childDigest: child}, ""); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("error = %v, want integrity error", err)
	}
}

func TestQuotedCommasInABearerChallengeAreNotParameterSeparators(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		challenge string
		want      map[string]string
	}{
		{
			name:      "gitlab scope with pull,push",
			challenge: `Bearer realm="https://gitlab.example.com/jwt/auth",service="container_registry",scope="repository:group/project:pull,push"`,
			want:      map[string]string{"realm": "https://gitlab.example.com/jwt/auth", "service": "container_registry", "scope": "repository:group/project:pull,push"},
		},
		{
			name:      "comma inside a realm query",
			challenge: `Bearer realm="https://auth.test/token?a=1,2"`,
			want:      map[string]string{"realm": "https://auth.test/token?a=1,2"},
		},
		{
			name:      "escaped quote inside a value",
			challenge: `Bearer realm="https://auth.test/token",error="a \"quoted\" refusal"`,
			want:      map[string]string{"realm": "https://auth.test/token", "error": `a "quoted" refusal`},
		},
		{
			name:      "unquoted token values",
			challenge: `Bearer realm=https://auth.test/token,service=registry`,
			want:      map[string]string{"realm": "https://auth.test/token", "service": "registry"},
		},
		{
			name:      "surrounding whitespace and repeated commas",
			challenge: "Bearer  realm = \"https://auth.test/token\" , , service=\"registry\"",
			want:      map[string]string{"realm": "https://auth.test/token", "service": "registry"},
		},
		{
			name:      "a second challenge does not contribute parameters",
			challenge: `Bearer realm="https://auth.test/token", Basic realm="https://evil.test/"`,
			want:      map[string]string{"realm": "https://auth.test/token"},
		},
		{
			name:      "an empty optional parameter is tolerated",
			challenge: `Bearer realm="https://auth.test/token",scope=""`,
			want:      map[string]string{"realm": "https://auth.test/token", "scope": ""},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got, ok := bearerParams(testCase.challenge)
			if !ok {
				t.Fatalf("bearerParams(%q) refused a well-formed challenge", testCase.challenge)
			}
			if len(got) != len(testCase.want) {
				t.Fatalf("params = %v, want %v", got, testCase.want)
			}
			for key, want := range testCase.want {
				if got[key] != want {
					t.Fatalf("params[%q] = %q, want %q", key, got[key], want)
				}
			}
		})
	}
}

func TestMalformedBearerChallengesAreRefusedRatherThanGuessed(t *testing.T) {
	for _, testCase := range []struct{ name, challenge string }{
		{"not bearer", `Basic realm="https://auth.test/"`},
		{"scheme without parameters", "Bearer"},
		{"scheme with only spaces", "Bearer   "},
		{"no realm", `Bearer service="registry"`},
		{"empty realm", `Bearer realm=""`},
		{"realm with no value", "Bearer realm="},
		{"unterminated quote", `Bearer realm="https://auth.test/token`},
		{"trailing escape", `Bearer realm="https://auth.test/token\`},
		{"duplicate realm", `Bearer realm="https://good.test/token",realm="https://evil.test/token"`},
		{"duplicate realm differing in case", `Bearer realm="https://good.test/token",REALM="https://evil.test/token"`},
		{"missing separator", `Bearer realm="https://auth.test/token" service="registry"`},
		{"parameter with no name", `Bearer ="https://evil.test/"`},
		{"commas only", "Bearer ,,,"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got, ok := bearerParams(testCase.challenge); ok {
				t.Fatalf("bearerParams(%q) accepted a malformed challenge: %v", testCase.challenge, got)
			}
		})
	}
}

func TestGitLabScopedChallengeCompletesTheTokenExchange(t *testing.T) {
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","size":2},"layers":[]}`)
	manifestDigest := digest(manifest)
	var authority string
	var scope string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/jwt/auth" {
			scope = r.URL.Query().Get("scope")
			_, _ = w.Write([]byte(`{"token":"fixture-token"}`))
			return
		}
		if r.Header.Get("Authorization") != "Bearer fixture-token" {
			w.Header().Set("WWW-Authenticate", `Bearer realm="http://`+authority+`/jwt/auth",service="container_registry",scope="repository:group/project:pull,push"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path == "/v2/group/project/manifests/1.0.0" {
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			if r.Method == http.MethodGet {
				_, _ = w.Write(manifest)
			}
		}
	}))
	defer server.Close()
	authority = strings.TrimPrefix(server.URL, "http://")
	client, err := New(ClientOptions{HTTPClient: server.Client(), TestOnlyRegistryAuthority: netip.MustParseAddrPort(authority)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Resolve(context.Background(), authority+"/group/project:1.0.0"); err != nil {
		t.Fatalf("Resolve = %v, want the scoped challenge exchanged for a token", err)
	}
	if scope != "repository:group/project:pull,push" {
		t.Fatalf("scope = %q, want the challenge scope forwarded whole", scope)
	}
}

func TestSourceFallsBackToAChildManifestAnnotation(t *testing.T) {
	child := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","annotations":{"org.opencontainers.image.source":"https://example.test/repo","org.opencontainers.image.revision":"deadbeef"}}`)
	childDigest := digest(child)
	descriptors := fmt.Sprintf(`[{"mediaType":%q,"digest":%q,"size":%d,"platform":{"os":"linux","architecture":"amd64"}}]`, imageManifestMediaType, childDigest, len(child))
	got, _, err := resolveIndexFixture(t, descriptors, map[string][]byte{childDigest: child}, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Source != "https://example.test/repo" || got.Revision != "deadbeef" {
		t.Fatalf("source = %q revision = %q, want the child manifest's declaration", got.Source, got.Revision)
	}
}

func TestSourceFallsBackToTheImageConfigLabel(t *testing.T) {
	for _, testCase := range []struct{ name, config, source string }{
		{"dockerfile label", `{"config":{"Labels":{"org.opencontainers.image.source":"https://example.test/repo","org.opencontainers.image.revision":"deadbeef"}}}`, "https://example.test/repo"},
		{"no label", `{"config":{"Labels":{"maintainer":"someone"}}}`, ""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			config := []byte(testCase.config)
			configDigest := digest(config)
			manifest := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":%d},"layers":[]}`, imageManifestMediaType, configDigest, len(config)))
			got, err := resolveImageFixture(t, manifest, map[string][]byte{configDigest: config})
			if err != nil {
				t.Fatal(err)
			}
			if got.Source != testCase.source {
				t.Fatalf("source = %q, want %q", got.Source, testCase.source)
			}
		})
	}
}

func TestAnUnverifiableImageConfigLeavesTheResolveIntact(t *testing.T) {
	config := []byte(`{"config":{"Labels":{"org.opencontainers.image.source":"https://example.test/repo"}}}`)
	configDigest := digest(config)
	manifest := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":%d},"layers":[]}`, imageManifestMediaType, configDigest, len(config)))
	got, err := resolveImageFixture(t, manifest, map[string][]byte{configDigest: []byte(`{"config":{"Labels":{"org.opencontainers.image.source":"https://attacker.test/repo"}}}`)})
	if err != nil {
		t.Fatalf("Resolve = %v, want an optional config read not to fail the resolve", err)
	}
	if got.Source != "" || got.Digest != digest(manifest) {
		t.Fatalf("metadata = %+v, want the tampered label ignored and the manifest still resolved", got)
	}
}

func TestEachImageConfigDisagreementLeavesTheSourceUnset(t *testing.T) {
	labelled := []byte(`{"config":{"Labels":{"org.opencontainers.image.source":"https://example.test/repo","org.opencontainers.image.revision":"deadbeef"}}}`)
	// Equal in length to labelled, so naming it as the descriptor's digest moves only the
	// content comparison and leaves the size comparison agreeing.
	other := bytes.Repeat([]byte("x"), len(labelled))
	cases := []struct {
		name         string
		configDigest string
		configSize   int
		servedDigest string
	}{
		{name: "size disagrees with the served config", configDigest: digest(labelled), configSize: len(labelled) + 1, servedDigest: digest(labelled)},
		{name: "content digest disagrees with the served config", configDigest: digest(other), configSize: len(labelled)},
		{name: "header digest disagrees with the served config", configDigest: digest(labelled), configSize: len(labelled), servedDigest: digest(other)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manifest := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":%q,"size":%d},"layers":[]}`, imageManifestMediaType, tc.configDigest, tc.configSize))
			client, authority := newRegistryClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v2/":
				case "/v2/repo/manifests/latest":
					w.Header().Set("Docker-Content-Digest", digest(manifest))
					if r.Method == http.MethodGet {
						_, _ = w.Write(manifest)
					}
				case "/v2/repo/blobs/" + tc.configDigest:
					if tc.servedDigest != "" {
						w.Header().Set("Docker-Content-Digest", tc.servedDigest)
					}
					_, _ = w.Write(labelled)
				default:
					t.Errorf("path = %s", r.URL.Path)
				}
			})
			got, err := client.Resolve(context.Background(), authority.String()+"/repo:latest")
			if err != nil {
				t.Fatalf("Resolve = %v, want an unverifiable config not to fail the resolve", err)
			}
			if got.Digest != digest(manifest) {
				t.Fatalf("digest = %q, want the manifest still resolved", got.Digest)
			}
			// The served config always carries a usable source label, so a source here means the
			// blob was trusted and this comparison is the only thing that could have refused it.
			if got.Source != "" || got.Revision != "" {
				t.Fatalf("source = %q revision = %q, want both unset", got.Source, got.Revision)
			}
		})
	}
}

func resolveImageFixture(t *testing.T, manifest []byte, blobs map[string][]byte) (Metadata, error) {
	t.Helper()
	manifestDigest := digest(manifest)
	client, authority := newRegistryClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v2/":
		case r.URL.Path == "/v2/repo/manifests/latest":
			w.Header().Set("Docker-Content-Digest", manifestDigest)
			if r.Method == http.MethodGet {
				_, _ = w.Write(manifest)
			}
		case strings.HasPrefix(r.URL.Path, "/v2/repo/blobs/"):
			blob, ok := blobs[strings.TrimPrefix(r.URL.Path, "/v2/repo/blobs/")]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			_, _ = w.Write(blob)
		default:
			t.Errorf("path = %s", r.URL.Path)
		}
	})
	return client.Resolve(context.Background(), authority.String()+"/repo:latest")
}

func TestTheRequestCeilingCanDeliverTheLargestDeclaredResponse(t *testing.T) {
	client, err := New(ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	floor := time.Duration(maxBlobBytes) * time.Second / minRegistryThroughput
	if client.http.Timeout < floor {
		t.Fatalf("client timeout = %s, want at least %s to deliver a %d byte blob", client.http.Timeout, floor, maxBlobBytes)
	}
	transport, ok := client.http.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T", client.http.Transport)
	}
	if transport.ResponseHeaderTimeout == 0 || transport.ResponseHeaderTimeout > registryResponseTimeout {
		t.Fatalf("response header timeout = %s, want a bound no looser than %s so a silent registry still fails fast", transport.ResponseHeaderTimeout, registryResponseTimeout)
	}
}

func TestEachRequestGetsADeadlineScaledToItsByteLimit(t *testing.T) {
	var deadline time.Duration
	client := &Client{http: &http.Client{Transport: ociRoundTrip(func(r *http.Request) (*http.Response, error) {
		if at, ok := r.Context().Deadline(); ok {
			deadline = time.Until(at)
		} else {
			deadline = 0
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})}}
	u, _ := url.Parse("https://registry.test/v2/a/blobs/x")
	for _, limit := range []int64{maxBlobBytes, manifestLimit, 0} {
		if _, _, _, err := client.request(context.Background(), http.MethodGet, u, "", limit); err != nil {
			t.Fatalf("request(limit=%d) = %v", limit, err)
		}
		floor := registryResponseTimeout + time.Duration(limit)*time.Second/minRegistryThroughput
		if deadline < floor-time.Second || deadline > floor {
			t.Fatalf("deadline for a %d byte limit = %s, want %s", limit, deadline, floor)
		}
	}
}

func TestDockerHubCredentialsStayAttachedToTheCanonicalAuthority(t *testing.T) {
	for _, alias := range []string{"docker.io", "index.docker.io", "registry.hub.docker.com", dockerHubAuthority} {
		credentials, err := checkedCredentials(map[string]Credential{alias: {Username: "lkshrk", Secret: credentialSecret}})
		if err != nil {
			t.Fatalf("checkedCredentials(%q) = %v", alias, err)
		}
		if _, ok := credentials[dockerHubAuthority]; len(credentials) != 1 || !ok {
			t.Fatalf("checkedCredentials(%q) = %v, want one credential keyed by %q", alias, credentials, dockerHubAuthority)
		}
	}
	_, err := checkedCredentials(map[string]Credential{
		"docker.io":       {Username: "lkshrk", Secret: credentialSecret},
		"index.docker.io": {Username: "other", Secret: credentialSecret},
	})
	if err == nil {
		t.Fatal("checkedCredentials accepted two aliases of one registry")
	}
	if !strings.Contains(err.Error(), dockerHubAuthority) {
		t.Fatalf("conflict error = %v, want it to name the canonical authority", err)
	}
	if strings.Contains(err.Error(), credentialSecret) {
		t.Fatalf("conflict error leaked the secret: %v", err)
	}
}

func TestACredentialTravelsOnlyToItsRegistryOrThatRegistrysMappedTokenHost(t *testing.T) {
	for _, testCase := range []struct {
		name, authority, realm, wantHost string
		anonymous                        bool
	}{
		{name: "the registry's own host", authority: "ghcr.io", realm: "https://ghcr.io/token", wantHost: "ghcr.io"},
		{name: "docker hub's mapped token host", authority: dockerHubAuthority, realm: "https://auth.docker.io/token", wantHost: "auth.docker.io"},
		{name: "a sibling of the mapped token host", authority: dockerHubAuthority, realm: "https://evil.docker.io/token"},
		{name: "a suffix extension of the mapped token host", authority: dockerHubAuthority, realm: "https://auth.docker.io.attacker.test/token"},
		{name: "the mapped token host of another registry", authority: "ghcr.io", realm: "https://auth.docker.io/token"},
		{name: "an unmapped registry naming another host", authority: "ghcr.io", realm: "https://tokens.example/token"},
		{name: "the registry a mapped token host belongs to", authority: "auth.docker.io", realm: "https://registry-1.docker.io/token"},
		{name: "an anonymous exchange is unrestricted", authority: "ghcr.io", realm: "https://tokens.example/token", wantHost: "tokens.example", anonymous: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var reached []string
			credentials := map[string]Credential{testCase.authority: {Username: "lkshrk", Secret: credentialSecret}}
			wantAuthorization := basicHeader("lkshrk", credentialSecret)
			if testCase.anonymous {
				credentials, wantAuthorization = nil, ""
			}
			client := &Client{
				credentials: credentials,
				http: &http.Client{Transport: ociRoundTrip(func(r *http.Request) (*http.Response, error) {
					reached = append(reached, r.URL.Host+" "+r.Header.Get("Authorization"))
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"token":"issued"}`))}, nil
				})},
			}
			token, err := client.token(context.Background(), testCase.authority, `Bearer realm="`+testCase.realm+`"`)
			if testCase.wantHost == "" {
				if !errors.Is(err, ErrTrustBoundary) {
					t.Fatalf("token() = %q, %v, want a trust boundary refusal", token, err)
				}
				if len(reached) != 0 {
					t.Fatalf("a refused realm still received requests: %v", reached)
				}
				if strings.Contains(err.Error(), credentialSecret) {
					t.Fatalf("error leaked the credential: %v", err)
				}
				return
			}
			if err != nil || token != "issued" {
				t.Fatalf("token() = %q, %v", token, err)
			}
			want := strings.TrimSpace(testCase.wantHost + " " + wantAuthorization)
			if len(reached) != 1 || strings.TrimSpace(reached[0]) != want {
				t.Fatalf("requests = %q, want exactly %q", reached, want)
			}
		})
	}
}

func TestARealmDifferingOnlyByDefaultPortOrAsciiCaseReachesTheMappedTokenHost(t *testing.T) {
	for _, testCase := range []struct {
		name, authority, realm, wantHost string
	}{
		{name: "explicit default https port on the mapped token host", authority: dockerHubAuthority, realm: "https://auth.docker.io:443/token", wantHost: "auth.docker.io:443"},
		{name: "uppercase mapped token host", authority: dockerHubAuthority, realm: "https://AUTH.DOCKER.IO/token", wantHost: "AUTH.DOCKER.IO"},
		{name: "explicit default https port on the registry's own host", authority: "ghcr.io", realm: "https://ghcr.io:443/token", wantHost: "ghcr.io:443"},
		{name: "mixed case registry's own host", authority: "ghcr.io", realm: "https://GHCR.io/token", wantHost: "GHCR.io"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var reached []string
			client := &Client{
				credentials: map[string]Credential{testCase.authority: {Username: "lkshrk", Secret: credentialSecret}},
				http: &http.Client{Transport: ociRoundTrip(func(r *http.Request) (*http.Response, error) {
					reached = append(reached, r.URL.Host+" "+r.Header.Get("Authorization"))
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"token":"issued"}`))}, nil
				})},
			}
			token, err := client.token(context.Background(), testCase.authority, `Bearer realm="`+testCase.realm+`"`)
			if err != nil || token != "issued" {
				t.Fatalf("token() = %q, %v; a realm differing only by default port or ASCII case is the same host", token, err)
			}
			want := testCase.wantHost + " " + basicHeader("lkshrk", credentialSecret)
			if len(reached) != 1 || reached[0] != want {
				t.Fatalf("requests = %q, want exactly %q", reached, want)
			}
		})
	}
}

// TestTheRealmGateFoldsNothingBeyondDefaultPortAndAsciiCase pins the two properties that keep the
// default-port/ASCII-case fold from widening the credential trust set. A non-default port stays a
// distinct destination, and a Unicode homograph stays a distinct host — swapping asciiLower for
// strings.ToLower folds the Kelvin sign to 'k' and leaks the credential, which this test forbids.
func TestTheRealmGateFoldsNothingBeyondDefaultPortAndAsciiCase(t *testing.T) {
	for _, testCase := range []struct{ name, authority, realm string }{
		{name: "non-default port on the mapped token host", authority: dockerHubAuthority, realm: "https://auth.docker.io:8443/token"},
		{name: "non-default port on the registry's own host", authority: "ghcr.io", realm: "https://ghcr.io:8443/token"},
		{name: "http default port is not a valid https port to strip", authority: dockerHubAuthority, realm: "https://auth.docker.io:80/token"},
		{name: "kelvin-sign homograph of the mapped token host", authority: dockerHubAuthority, realm: "https://auth.docKer.io/token"},
		{name: "cyrillic homograph of the mapped token host", authority: dockerHubAuthority, realm: "https://аuth.docker.io/token"},
		{name: "punycode is not decoded before matching", authority: dockerHubAuthority, realm: "https://xn--auth-docker.io/token"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var reached []string
			client := &Client{
				credentials: map[string]Credential{testCase.authority: {Username: "lkshrk", Secret: credentialSecret}},
				http: &http.Client{Transport: ociRoundTrip(func(r *http.Request) (*http.Response, error) {
					reached = append(reached, r.URL.Host)
					return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"token":"issued"}`))}, nil
				})},
			}
			token, err := client.token(context.Background(), testCase.authority, `Bearer realm="`+testCase.realm+`"`)
			if !errors.Is(err, ErrTrustBoundary) {
				t.Fatalf("token() = %q, %v, want a trust boundary refusal", token, err)
			}
			if len(reached) != 0 {
				t.Fatalf("a refused realm still received requests: %v", reached)
			}
			if strings.Contains(err.Error(), credentialSecret) {
				t.Fatalf("error leaked the credential: %v", err)
			}
		})
	}
}

func TestVerifyArtifactBoundsCumulativeRetryAfterAcrossRequests(t *testing.T) {
	config, layer := []byte("{}"), []byte("chart")
	configDigest, layerDigest := digest(config), digest(layer)
	manifest := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"config":{"mediaType":"application/vnd.cncf.helm.config.v1+json","digest":%q,"size":%d},"layers":[{"mediaType":"application/vnd.cncf.helm.chart.content.v1.tar+gzip","digest":%q,"size":%d}]}`, imageManifestMediaType, configDigest, len(config), layerDigest, len(layer)))
	throttled := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		if !throttled[key] {
			throttled[key] = true
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/v2/":
		case "/v2/charts/app/manifests/1.0.0":
			w.Header().Set("Docker-Content-Digest", digest(manifest))
			if r.Method == http.MethodGet {
				_, _ = w.Write(manifest)
			}
		case "/v2/charts/app/blobs/" + configDigest:
			_, _ = w.Write(config)
		case "/v2/charts/app/blobs/" + layerDigest:
			_, _ = w.Write(layer)
		default:
			t.Errorf("path = %s", r.URL.Path)
		}
	}))
	defer server.Close()
	authority := netip.MustParseAddrPort(strings.TrimPrefix(server.URL, "http://"))
	var slept time.Duration
	client, err := New(ClientOptions{
		HTTPClient:                server.Client(),
		TestOnlyRegistryAuthority: authority,
		Retry: retry.Schedule{Sleep: func(_ context.Context, delay time.Duration) error {
			slept += delay
			return nil
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.VerifyArtifact(context.Background(), authority.String()+"/charts/app:1.0.0")
	if !errors.Is(err, ErrUnavailable) {
		t.Fatalf("err = %v, want the call refused once its retry budget is spent", err)
	}
	if slept > maxRetryAfterBudget {
		t.Fatalf("cumulative Retry-After sleep = %s, want at most %s", slept, maxRetryAfterBudget)
	}
}

func TestAResolveIsBoundedWholeAndNotOnlyPerRequest(t *testing.T) {
	const perRequest = 100 * time.Millisecond
	children, descriptors := map[string][]byte{}, make([]string, 0, 12)
	for i := range 12 {
		body := []byte(fmt.Sprintf(`{"schemaVersion":2,"mediaType":%q,"annotations":{"n":"%d"}}`, imageManifestMediaType, i))
		childDigest := digest(body)
		children[childDigest] = body
		descriptors = append(descriptors, fmt.Sprintf(
			`{"mediaType":%q,"digest":%q,"size":%d,"platform":{"os":"linux","architecture":"arch%d"}}`,
			imageManifestMediaType, childDigest, len(body), i,
		))
	}
	index := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.index.v1+json","manifests":[` + strings.Join(descriptors, ",") + `]}`)
	indexDigest := digest(index)
	unbounded := time.Duration(3+len(children)) * perRequest
	var mu sync.Mutex
	served := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		served++
		mu.Unlock()
		time.Sleep(perRequest)
		switch {
		case r.URL.Path == "/v2/":
		case strings.HasPrefix(r.URL.Path, "/v2/repo/manifests/"):
			ref := strings.TrimPrefix(r.URL.Path, "/v2/repo/manifests/")
			if child, ok := children[ref]; ok {
				w.Header().Set("Docker-Content-Digest", ref)
				_, _ = w.Write(child)
				return
			}
			w.Header().Set("Docker-Content-Digest", indexDigest)
			if r.Method == http.MethodGet {
				_, _ = w.Write(index)
			}
		default:
			t.Errorf("path = %s", r.URL.Path)
		}
	}))
	defer server.Close()
	authority := netip.MustParseAddrPort(strings.TrimPrefix(server.URL, "http://"))
	reference := authority.String() + "/repo:latest"
	for name, call := range map[string]func(*Client, context.Context) error{
		"Resolve": func(client *Client, ctx context.Context) error {
			_, err := client.Resolve(ctx, reference)
			return err
		},
		"VerifyArtifact": func(client *Client, ctx context.Context) error {
			_, err := client.VerifyArtifact(ctx, reference)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			client, err := New(ClientOptions{
				HTTPClient:                server.Client(),
				TestOnlyRegistryAuthority: authority,
				TestOnlyCallTimeout:       3 * perRequest,
			})
			if err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			served = 0
			mu.Unlock()
			start := time.Now()
			err = call(client, context.Background())
			elapsed := time.Since(start)
			if !errors.Is(err, ErrUnavailable) {
				t.Fatalf("err = %v, want the call refused once its whole-call deadline passed", err)
			}
			// A caller inspecting for its own cancellation must not mistake the client's own bound.
			if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				t.Fatalf("err = %v, want the client's own bound rather than a context error", err)
			}
			mu.Lock()
			requests := served
			mu.Unlock()
			if requests >= 3+len(children) {
				t.Fatalf("registry served %d requests, want the call abandoned before the last child", requests)
			}
			if elapsed >= unbounded {
				t.Fatalf("call took %s, want it bounded well below the %s the requests would take", elapsed, unbounded)
			}
		})
	}
}

func TestACallWithoutAConfiguredBoundStillCarriesThePackageDeadline(t *testing.T) {
	client, err := New(ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := client.callDeadline(context.Background())
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("call context carries no deadline")
	}
	if remaining := time.Until(deadline); remaining > maxCallDuration || remaining < maxCallDuration-time.Minute {
		t.Fatalf("call deadline = %s away, want %s", remaining, maxCallDuration)
	}
	parent, cancelParent := context.WithTimeout(context.Background(), time.Second)
	defer cancelParent()
	narrowed, cancelNarrowed := client.callDeadline(parent)
	defer cancelNarrowed()
	if deadline, _ := narrowed.Deadline(); time.Until(deadline) > time.Second {
		t.Fatalf("call deadline = %s away, want the caller's shorter deadline kept", time.Until(deadline))
	}
}

func TestOnlyARealChallengeInTheListIsTakenAsTheBearerChallenge(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		values []string
		want   string
	}{
		{"single bearer challenge", []string{`Bearer realm="https://auth.test/token"`}, `Bearer realm="https://auth.test/token"`},
		{"bearer after basic in one value", []string{`Basic realm="registry", Bearer realm="https://auth.test/token",service="r"`}, `Bearer realm="https://auth.test/token",service="r"`},
		{"bearer in a later header value", []string{`Basic realm="registry"`, `Bearer realm="https://auth.test/token"`}, `Bearer realm="https://auth.test/token"`},
		{"lowercase scheme", []string{`basic realm="registry", bearer realm="https://auth.test/token"`}, `bearer realm="https://auth.test/token"`},
		{"basic only", []string{`Basic realm="registry"`}, ""},
		{"no challenge at all", nil, ""},
		{"a quoted parameter cannot forge a challenge", []string{`Basic realm="Bearer realm=https://evil.test/token"`}, ""},
		{"an unquoted parameter cannot forge a challenge", []string{`Basic realm=Bearer,realm2=x`}, ""},
		{"a malformed value is refused rather than guessed", []string{`Basic realm="registry" Bearer realm="https://evil.test/token"`}, ""},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := bearerChallenge(testCase.values); got != testCase.want {
				t.Fatalf("bearerChallenge(%q) = %q, want %q", testCase.values, got, testCase.want)
			}
		})
	}
}

func TestABearerChallengeIsHonouredWhenAnotherSchemeIsOfferedFirst(t *testing.T) {
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","size":2},"layers":[]}`)
	manifestDigest := digest(manifest)
	for name, offer := range map[string]func(http.Header, string){
		"two separate headers": func(h http.Header, authority string) {
			h.Add("WWW-Authenticate", `Basic realm="registry"`)
			h.Add("WWW-Authenticate", `Bearer realm="http://`+authority+`/token",service="fixture",scope="repository:repo:pull"`)
		},
		"one header listing both schemes": func(h http.Header, authority string) {
			h.Add("WWW-Authenticate", `Basic realm="registry", Bearer realm="http://`+authority+`/token",service="fixture",scope="repository:repo:pull"`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			var authority string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/token" {
					_, _ = w.Write([]byte(`{"token":"fixture-token"}`))
					return
				}
				if r.Header.Get("Authorization") != "Bearer fixture-token" {
					offer(w.Header(), authority)
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				if r.URL.Path == "/v2/repo/manifests/latest" {
					w.Header().Set("Docker-Content-Digest", manifestDigest)
					if r.Method == http.MethodGet {
						_, _ = w.Write(manifest)
					}
				}
			}))
			defer server.Close()
			authority = strings.TrimPrefix(server.URL, "http://")
			client, err := New(ClientOptions{HTTPClient: server.Client(), TestOnlyRegistryAuthority: netip.MustParseAddrPort(authority)})
			if err != nil {
				t.Fatal(err)
			}
			got, err := client.Resolve(context.Background(), authority+"/repo:latest")
			if err != nil {
				t.Fatalf("Resolve = %v, want the Bearer challenge exchanged for a token", err)
			}
			if got.Digest != manifestDigest {
				t.Fatalf("digest = %q, want %q", got.Digest, manifestDigest)
			}
		})
	}
}
