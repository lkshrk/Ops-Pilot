package github

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"sync"
	"testing"
)

// fixtureToken differs from every literal in the adapter on purpose: a client
// that sent a hardcoded bearer would still satisfy an assertion made against
// the same word the adapter contains.
const fixtureToken = "github-pat-11SENTINEL-4c7f9a"

type recordedRequest struct {
	method        string
	path          string
	query         url.Values
	body          map[string]any
	authorization string
	contentType   string
}

// recording serves respond and keeps every request the adapter actually sent,
// so a write can be checked for its payload and for how many times it was made.
func recording(t *testing.T, respond func(attempt int, w http.ResponseWriter, r *http.Request)) (http.Handler, func() []recordedRequest) {
	t.Helper()
	var mu sync.Mutex
	var seen []recordedRequest
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		entry := recordedRequest{
			method:        r.Method,
			path:          r.URL.EscapedPath(),
			query:         r.URL.Query(),
			authorization: r.Header.Get("Authorization"),
			contentType:   r.Header.Get("Content-Type"),
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &entry.body); err != nil {
				t.Errorf("request body %q is not a JSON object: %v", raw, err)
			}
		}
		mu.Lock()
		seen = append(seen, entry)
		attempt := len(seen)
		mu.Unlock()
		respond(attempt, w, r)
	})
	return handler, func() []recordedRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]recordedRequest(nil), seen...)
	}
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeAPIError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{"message": message})
}
