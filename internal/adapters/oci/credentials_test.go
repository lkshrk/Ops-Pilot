package oci

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"testing"
)

const credentialSecret = "ghp_totallysecrettokenvalue"

// credentialRegistry serves one manifest behind a bearer challenge and records the
// Authorization header presented on every path.
type credentialRegistry struct {
	server    *httptest.Server
	authority string
	manifest  []byte
	digest    string
	realmHost string

	mu        sync.Mutex
	seen      []string
	tokenAuth []string
}

func newCredentialRegistry(t *testing.T) *credentialRegistry {
	t.Helper()
	manifest := []byte(`{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"mediaType":"application/vnd.oci.image.config.v1+json","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","size":2},"layers":[]}`)
	registry := &credentialRegistry{manifest: manifest, digest: digest(manifest)}
	registry.server = httptest.NewServer(http.HandlerFunc(registry.serve))
	registry.authority = strings.TrimPrefix(registry.server.URL, "http://")
	registry.realmHost = registry.authority
	t.Cleanup(registry.server.Close)
	return registry
}

func (r *credentialRegistry) record(req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.seen = append(r.seen, req.URL.Path+" "+req.Header.Get("Authorization"))
	if req.URL.Path == "/token" {
		r.tokenAuth = append(r.tokenAuth, req.Header.Get("Authorization"))
	}
}

func (r *credentialRegistry) observed() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.seen...)
}

func (r *credentialRegistry) tokenRequests() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.tokenAuth...)
}

func (r *credentialRegistry) challenge(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", fmt.Sprintf(
		`Bearer realm="http://%s/token",service="registry",scope="repository:repo:pull"`, r.realmHost,
	))
	w.WriteHeader(http.StatusUnauthorized)
}

func (r *credentialRegistry) serve(w http.ResponseWriter, req *http.Request) {
	r.record(req)
	switch req.URL.Path {
	case "/v2/":
		w.WriteHeader(http.StatusOK)
	case "/token":
		_, _ = w.Write([]byte(`{"token":"issued-token"}`))
	case "/v2/repo/manifests/latest":
		if req.Header.Get("Authorization") != "Bearer issued-token" {
			r.challenge(w)
			return
		}
		w.Header().Set("Docker-Content-Digest", r.digest)
		if req.Method == http.MethodGet {
			_, _ = w.Write(r.manifest)
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (r *credentialRegistry) handle(handler http.HandlerFunc) {
	r.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.record(req)
		handler(w, req)
	})
}

func (r *credentialRegistry) client(t *testing.T, credentials map[string]Credential) *Client {
	t.Helper()
	client, err := New(ClientOptions{
		HTTPClient:                r.server.Client(),
		TestOnlyRegistryAuthority: netip.MustParseAddrPort(r.authority),
		Credentials:               credentials,
	})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func (r *credentialRegistry) credentials() map[string]Credential {
	return map[string]Credential{r.authority: {Username: "lkshrk", Secret: credentialSecret}}
}

func basicHeader(username, secret string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(username+":"+secret))
}

func assertNoBasicOutsideTokenExchange(t *testing.T, registry *credentialRegistry) {
	t.Helper()
	for _, entry := range registry.observed() {
		path, authorization, _ := strings.Cut(entry, " ")
		if path != "/token" && strings.HasPrefix(authorization, "Basic ") {
			t.Fatalf("credential presented outside the token exchange: %q", entry)
		}
	}
}

func TestTokenExchangePresentsCredentialsOnlyToMatchingRegistry(t *testing.T) {
	registry := newCredentialRegistry(t)
	client := registry.client(t, registry.credentials())
	got, err := client.Resolve(context.Background(), registry.authority+"/repo:latest")
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest != registry.digest {
		t.Fatalf("digest = %q, want %q", got.Digest, registry.digest)
	}
	requests := registry.tokenRequests()
	if len(requests) == 0 {
		t.Fatal("no token request observed")
	}
	want := basicHeader("lkshrk", credentialSecret)
	for _, authorization := range requests {
		if authorization != want {
			t.Fatalf("token request authorization = %q, want the configured basic credential", authorization)
		}
	}
	assertNoBasicOutsideTokenExchange(t, registry)
}

func TestTokenExchangeOmitsCredentialsForAnotherRegistry(t *testing.T) {
	registry := newCredentialRegistry(t)
	client := registry.client(t, map[string]Credential{
		"ghcr.io": {Username: "lkshrk", Secret: credentialSecret},
	})
	if _, err := client.Resolve(context.Background(), registry.authority+"/repo:latest"); err != nil {
		t.Fatal(err)
	}
	requests := registry.tokenRequests()
	if len(requests) == 0 {
		t.Fatal("no token request observed")
	}
	for _, authorization := range requests {
		if authorization != "" {
			t.Fatalf("token request to a non-matching registry carried %q", authorization)
		}
	}
}

func TestAnonymousTokenExchangeIsUnaffectedByCredentialSupport(t *testing.T) {
	registry := newCredentialRegistry(t)
	client := registry.client(t, nil)
	got, err := client.Resolve(context.Background(), registry.authority+"/repo:latest")
	if err != nil {
		t.Fatal(err)
	}
	if got.Digest != registry.digest {
		t.Fatalf("digest = %q, want %q", got.Digest, registry.digest)
	}
	for _, authorization := range registry.tokenRequests() {
		if authorization != "" {
			t.Fatalf("anonymous token request carried %q", authorization)
		}
	}
	assertNoBasicOutsideTokenExchange(t, registry)
}

func TestTokenExchangeRefusesRealmOnAnotherHost(t *testing.T) {
	registry := newCredentialRegistry(t)
	registry.realmHost = "realm.example"
	client := registry.client(t, registry.credentials())
	_, err := client.Resolve(context.Background(), registry.authority+"/repo:latest")
	if !errors.Is(err, ErrTrustBoundary) {
		t.Fatalf("err = %v, want a trust boundary refusal", err)
	}
	if requests := registry.tokenRequests(); len(requests) != 0 {
		t.Fatalf("token request issued despite the foreign realm: %v", requests)
	}
	if strings.Contains(err.Error(), credentialSecret) {
		t.Fatalf("error leaked the credential: %v", err)
	}
}

func TestTokenExchangeNeverFollowsARedirectWithCredentials(t *testing.T) {
	registry := newCredentialRegistry(t)
	stolen := 0
	registry.handle(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/v2/":
			w.WriteHeader(http.StatusOK)
		case "/token":
			w.Header().Set("Location", "/steal")
			w.WriteHeader(http.StatusTemporaryRedirect)
		case "/steal":
			stolen++
			w.WriteHeader(http.StatusOK)
		default:
			registry.challenge(w)
		}
	})
	client := registry.client(t, registry.credentials())
	_, err := client.Resolve(context.Background(), registry.authority+"/repo:latest")
	if err == nil {
		t.Fatal("redirected token exchange succeeded")
	}
	if stolen != 0 {
		t.Fatalf("redirect target received %d requests", stolen)
	}
	requests := registry.tokenRequests()
	if len(requests) == 0 || requests[0] != basicHeader("lkshrk", credentialSecret) {
		t.Fatalf("token requests = %v, want the credential presented before the redirect", requests)
	}
	for _, entry := range registry.observed() {
		if path, _, _ := strings.Cut(entry, " "); path == "/steal" {
			t.Fatalf("client followed the token redirect: %q", entry)
		}
	}
	assertNoBasicOutsideTokenExchange(t, registry)
	if strings.Contains(err.Error(), credentialSecret) {
		t.Fatalf("error leaked the credential: %v", err)
	}
}

func TestManifestRedirectDropsAuthorization(t *testing.T) {
	registry := newCredentialRegistry(t)
	registry.handle(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/v2/":
			w.WriteHeader(http.StatusOK)
		case "/token":
			_, _ = w.Write([]byte(`{"token":"issued-token"}`))
		case "/v2/repo/manifests/latest":
			if req.Header.Get("Authorization") != "Bearer issued-token" {
				registry.challenge(w)
				return
			}
			w.Header().Set("Location", "/relocated")
			w.WriteHeader(http.StatusTemporaryRedirect)
		case "/relocated":
			w.Header().Set("Docker-Content-Digest", registry.digest)
			if req.Method == http.MethodGet {
				_, _ = w.Write(registry.manifest)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	client := registry.client(t, registry.credentials())
	if _, err := client.Resolve(context.Background(), registry.authority+"/repo:latest"); err != nil {
		t.Fatal(err)
	}
	for _, entry := range registry.observed() {
		path, authorization, _ := strings.Cut(entry, " ")
		if path == "/relocated" && authorization != "" {
			t.Fatalf("redirect target received an authorization header: %q", entry)
		}
	}
	assertNoBasicOutsideTokenExchange(t, registry)
}

func TestRejectedCredentialsStayOutOfTheError(t *testing.T) {
	registry := newCredentialRegistry(t)
	registry.handle(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/v2/":
			w.WriteHeader(http.StatusOK)
		case "/token":
			w.WriteHeader(http.StatusUnauthorized)
		default:
			registry.challenge(w)
		}
	})
	client := registry.client(t, registry.credentials())
	_, err := client.Resolve(context.Background(), registry.authority+"/repo:latest")
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("err = %v, want an auth refusal", err)
	}
	encoded := base64.StdEncoding.EncodeToString([]byte("lkshrk:" + credentialSecret))
	for _, forbidden := range []string{credentialSecret, encoded} {
		if strings.Contains(err.Error(), forbidden) {
			t.Fatalf("error leaked credential material: %v", err)
		}
	}
	if !strings.Contains(err.Error(), "configured credentials rejected") {
		t.Fatalf("error did not distinguish a rejected credential: %v", err)
	}
}

func TestCredentialRedactsItsSecret(t *testing.T) {
	credential := Credential{Username: "lkshrk", Secret: credentialSecret}
	rendered := fmt.Sprintf("%v %s", credential, credential)
	if strings.Contains(rendered, credentialSecret) {
		t.Fatalf("formatted credential leaked the secret: %s", rendered)
	}
	if !strings.Contains(rendered, "redacted") || !strings.Contains(rendered, "lkshrk") {
		t.Fatalf("formatted credential = %s", rendered)
	}
}

func TestNewRejectsUnusableCredentials(t *testing.T) {
	for name, credentials := range map[string]map[string]Credential{
		"empty authority":         {"": {Username: "lkshrk", Secret: credentialSecret}},
		"authority with path":     {"ghcr.io/lkshrk": {Username: "lkshrk", Secret: credentialSecret}},
		"authority with userinfo": {"lkshrk@ghcr.io": {Username: "lkshrk", Secret: credentialSecret}},
		"empty username":          {"ghcr.io": {Secret: credentialSecret}},
		"username with colon":     {"ghcr.io": {Username: "lks:hrk", Secret: credentialSecret}},
		"empty secret":            {"ghcr.io": {Username: "lkshrk"}},
	} {
		client, err := New(ClientOptions{Credentials: credentials})
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
		if client != nil {
			t.Fatalf("%s: returned a client", name)
		}
		if strings.Contains(err.Error(), credentialSecret) {
			t.Fatalf("%s: error leaked the secret: %v", name, err)
		}
	}
}
