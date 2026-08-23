package github

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/lkshrk/ops-pilot/internal/domain"
	"github.com/lkshrk/ops-pilot/internal/retry"
)

// recordBackoff routes the read path's backoff through a clock that records what
// it was asked for and spends none of it.
func recordBackoff(c *Client) *[]time.Duration {
	asked := new([]time.Duration)
	c.retry = retry.Schedule{
		Sleep: func(_ context.Context, d time.Duration) error {
			*asked = append(*asked, d)
			return nil
		},
		Jitter: func(d time.Duration) time.Duration { return d },
	}
	return asked
}

type forgeCall struct {
	name string
	call func(*Client) error
}

func readCalls() []forgeCall {
	return []forgeCall{
		{name: "file at", call: func(c *Client) error {
			_, _, err := c.FileAt(context.Background(), "clusters/prod.yaml", "beforesha")
			return err
		}},
		{name: "get", call: func(c *Client) error {
			_, err := c.Get(context.Background(), 42)
			return err
		}},
		{name: "branch head", call: func(c *Client) error {
			_, err := c.BranchHead(context.Background(), "main")
			return err
		}},
		{name: "list open", call: func(c *Client) error {
			_, err := c.ListOpen(context.Background(), domain.PullRequestFilter{})
			return err
		}},
		{name: "changed files", call: func(c *Client) error {
			_, err := c.ChangedFiles(context.Background(), 42)
			return err
		}},
	}
}

func writeCalls() []forgeCall {
	return []forgeCall{
		{name: "merge", call: func(c *Client) error {
			_, err := c.Merge(context.Background(), 42, "aaaaaaaaaaaa", "squash")
			return err
		}},
		{name: "comment", call: func(c *Client) error { return c.Comment(context.Background(), 42, "hello") }},
		{name: "label", call: func(c *Client) error { return c.AddLabel(context.Background(), 42, "held") }},
		{name: "close", call: func(c *Client) error { return c.Close(context.Background(), 42) }},
	}
}

// A read is replayable, so a blip on it must not become a hold the operator has
// to clear by hand. A write is not: a merge, comment, label or close replayed
// after a timeout either applies twice or reports a failure for something GitHub
// already accepted, and a merge reported as failed is a merge nothing watches.
// The asymmetry at request()'s method switch is the safe one; both halves of it
// are pinned here.
func TestATransientFailureIsReplayedOnAReadAndNeverOnAWrite(t *testing.T) {
	statuses := []struct {
		name   string
		status int
	}{
		{name: "server error", status: http.StatusInternalServerError},
		{name: "gateway", status: http.StatusBadGateway},
		{name: "rate limited", status: http.StatusTooManyRequests},
	}
	sides := []struct {
		kind     string
		calls    []forgeCall
		attempts int
	}{
		{kind: "read", calls: readCalls(), attempts: retry.MaxAttempts},
		{kind: "write", calls: writeCalls(), attempts: 1},
	}
	for _, side := range sides {
		for _, call := range side.calls {
			for _, status := range statuses {
				t.Run(side.kind+"/"+call.name+"/"+status.name, func(t *testing.T) {
					handler, requests := recording(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
						writeAPIError(w, status.status, "transient")
					})
					client := newTestClient(t, handler, "main")
					recordBackoff(client)

					if err := call.call(client); err == nil {
						t.Fatalf("%s accepted status %d", call.name, status.status)
					}
					if seen := requests(); len(seen) != side.attempts {
						t.Fatalf("GitHub saw %d requests, want %d for a %s", len(seen), side.attempts, side.kind)
					}
				})
			}
		}
	}
}

// A read that succeeds on the replay must return the answer, not the blip that
// preceded it: the whole point of replaying is that the run does not stop.
func TestAReadThatSucceedsOnTheReplayReturnsTheAnswer(t *testing.T) {
	handler, requests := recording(t, func(attempt int, w http.ResponseWriter, _ *http.Request) {
		if attempt < retry.MaxAttempts {
			writeAPIError(w, http.StatusBadGateway, "Bad gateway")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"object": map[string]any{"sha": "aaaaaaaaaaaa"}})
	})
	client := newTestClient(t, handler, "main")
	recordBackoff(client)

	sha, err := client.BranchHead(context.Background(), "main")
	if err != nil {
		t.Fatalf("BranchHead: %v", err)
	}
	if sha != "aaaaaaaaaaaa" {
		t.Fatalf("BranchHead returned %q, want the answer the replay carried", sha)
	}
	if seen := requests(); len(seen) != retry.MaxAttempts {
		t.Fatalf("GitHub saw %d requests, want the read replayed to %d", len(seen), retry.MaxAttempts)
	}
}

// GitHub reports a secondary rate limit as 403 with Retry-After, so that pairing
// is the one read failure worth waiting out. A bare 403 is a permission answer
// that no amount of waiting changes, and replaying it would spend the merge
// window three times over on a run that was never going to proceed.
func TestOnlyA403CarryingRetryAfterIsWaitedOutAndOnlyOnTheReadPath(t *testing.T) {
	tests := []struct {
		name       string
		retryAfter string
		attempts   int
		asked      []time.Duration
	}{
		{name: "secondary rate limit", retryAfter: "2", attempts: retry.MaxAttempts, asked: []time.Duration{2 * time.Second, 2 * time.Second}},
		{name: "bare forbidden", attempts: 1},
	}
	for _, test := range tests {
		t.Run("read/"+test.name, func(t *testing.T) {
			handler, requests := recording(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				if test.retryAfter != "" {
					w.Header().Set("Retry-After", test.retryAfter)
				}
				writeAPIError(w, http.StatusForbidden, "Resource not accessible by integration")
			})
			client := newTestClient(t, handler, "main")
			asked := recordBackoff(client)

			if _, err := client.BranchHead(context.Background(), "main"); err == nil {
				t.Fatal("BranchHead accepted a 403")
			}
			if seen := requests(); len(seen) != test.attempts {
				t.Fatalf("GitHub saw %d requests, want %d", len(seen), test.attempts)
			}
			if len(*asked) != len(test.asked) {
				t.Fatalf("the client waited %v, want %v", *asked, test.asked)
			}
			for i, want := range test.asked {
				if (*asked)[i] != want {
					t.Fatalf("wait %d was %s, want the server-directed %s", i+1, (*asked)[i], want)
				}
			}
		})
		t.Run("write/"+test.name, func(t *testing.T) {
			handler, requests := recording(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				if test.retryAfter != "" {
					w.Header().Set("Retry-After", test.retryAfter)
				}
				writeAPIError(w, http.StatusForbidden, "Resource not accessible by integration")
			})
			client := newTestClient(t, handler, "main")
			asked := recordBackoff(client)

			if err := client.Comment(context.Background(), 42, "hello"); err == nil {
				t.Fatal("Comment accepted a 403")
			}
			if seen := requests(); len(seen) != 1 {
				t.Fatalf("GitHub saw %d requests, want the write reported at once", len(seen))
			}
			if len(*asked) != 0 {
				t.Fatalf("a write waited %v before being reported", *asked)
			}
		})
	}
}

// A 429 is a definitive rejection: GitHub never created the comment, so unlike a
// 5xx or a transport timeout it carries no risk of a second, duplicate post. The
// write path still does not auto-replay it, because that path is the merge PUT's
// too and a replayed merge is one nothing watches. The rejection's Retry-After is
// surfaced intact so the decision to retry an annotation can move to the caller
// without this adapter guessing on the shared path. This pins that split: no
// second request, but the delay is available.
func TestTheAnnotationWrite429IsSurfacedWithItsRetryAfterButNeverAutoReplayed(t *testing.T) {
	handler, requests := recording(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "2")
		writeAPIError(w, http.StatusTooManyRequests, "API rate limit exceeded")
	})
	client := newTestClient(t, handler, "main")
	asked := recordBackoff(client)

	err := client.Comment(context.Background(), 42, "hello")
	if err == nil {
		t.Fatal("Comment accepted a 429")
	}
	var transport *TransportError
	if !errors.As(err, &transport) {
		t.Fatalf("Comment returned %T, want a *TransportError", err)
	}
	if transport.Status != http.StatusTooManyRequests {
		t.Fatalf("TransportError.Status is %d, want 429", transport.Status)
	}
	if transport.RetryAfterDelay() <= 0 {
		t.Fatal("the 429's Retry-After was dropped, so a caller cannot honour it")
	}
	if seen := requests(); len(seen) != 1 {
		t.Fatalf("GitHub saw %d comment requests, want the write reported at once", len(seen))
	}
	if len(*asked) != 0 {
		t.Fatalf("the write waited %v before being reported", *asked)
	}
}

// A server-directed delay longer than the package bound is waited at the bound,
// not at its stated length: an unattended run that honoured a ten-minute
// Retry-After in full would hold its merge window on one read.
func TestAServerDirectedDelayIsCappedAtThePackageBound(t *testing.T) {
	handler, _ := recording(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "600")
		writeAPIError(w, http.StatusTooManyRequests, "API rate limit exceeded")
	})
	client := newTestClient(t, handler, "main")
	asked := recordBackoff(client)

	if _, err := client.BranchHead(context.Background(), "main"); err == nil {
		t.Fatal("BranchHead accepted a 429")
	}
	for i, waited := range *asked {
		if waited != retry.MaxRetryAfter {
			t.Fatalf("wait %d was %s, want it capped at %s", i+1, waited, retry.MaxRetryAfter)
		}
	}
	if len(*asked) == 0 {
		t.Fatal("no wait was measured at all")
	}
}
