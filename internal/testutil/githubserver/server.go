// Package githubserver provides a small authenticated GitHub HTTP fixture.
package githubserver

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func New(t testing.TB, token string, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		handler(w, r)
	}))
}
