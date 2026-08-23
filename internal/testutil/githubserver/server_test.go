package githubserver

import (
	"net/http"
	"sync/atomic"
	"testing"
)

func TestNewAcceptsConfiguredBearerToken(t *testing.T) {
	var called atomic.Bool
	server := New(t, "github-sentinel", func(w http.ResponseWriter, _ *http.Request) {
		called.Store(true)
		w.WriteHeader(http.StatusNoContent)
	})
	t.Cleanup(server.Close)

	request, err := http.NewRequest(http.MethodGet, server.URL, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer github-sentinel")

	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatalf("send request: %v", err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })

	if response.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusNoContent)
	}
	if !called.Load() {
		t.Fatal("authenticated request did not reach handler")
	}
}

func TestNewRejectsMissingOrWrongBearerToken(t *testing.T) {
	tests := []struct {
		name          string
		authorization string
	}{
		{name: "missing"},
		{name: "wrong", authorization: "Bearer another-token"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var called atomic.Bool
			server := New(t, "github-sentinel", func(http.ResponseWriter, *http.Request) {
				called.Store(true)
			})
			t.Cleanup(server.Close)

			request, err := http.NewRequest(http.MethodGet, server.URL, nil)
			if err != nil {
				t.Fatalf("new request: %v", err)
			}
			if test.authorization != "" {
				request.Header.Set("Authorization", test.authorization)
			}

			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatalf("send request: %v", err)
			}
			t.Cleanup(func() { _ = response.Body.Close() })

			if response.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want %d", response.StatusCode, http.StatusUnauthorized)
			}
			if called.Load() {
				t.Fatal("unauthenticated request reached handler")
			}
		})
	}
}
