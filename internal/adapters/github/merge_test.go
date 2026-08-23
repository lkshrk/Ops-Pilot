package github

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

const mergePath = "/repos/acme/cluster/pulls/42/merge"

// recordMergeBackoff routes the mergeability backoff through the adapter's clock
// seam, recording every delay it asks for and spending none of them.
func recordMergeBackoff(c *Client) *[]time.Duration {
	asked := new([]time.Duration)
	c.after = func(d time.Duration) <-chan time.Time {
		*asked = append(*asked, d)
		ready := make(chan time.Time, 1)
		ready <- time.Now()
		return ready
	}
	return asked
}

// GitHub computes mergeability asynchronously and answers 405 while it is still
// working. Every retry has to carry the same head the pull request was assessed
// at: a retry that re-read the branch would merge a commit nothing looked at.
func TestAMergeGitHubHasNotFinishedAssessingIsRetriedAtTheAssessedHead(t *testing.T) {
	for _, message := range []string{
		"Base branch was modified. Review and try the merge again.",
		"Pull Request is not mergeable",
	} {
		t.Run(message, func(t *testing.T) {
			handler, requests := recording(t, func(attempt int, w http.ResponseWriter, _ *http.Request) {
				if attempt == 1 {
					writeAPIError(w, http.StatusMethodNotAllowed, message)
					return
				}
				writeJSON(w, http.StatusOK, map[string]any{"merged": true, "sha": "cccccccccccc"})
			})
			client := newTestClient(t, handler, "main")
			asked := recordMergeBackoff(client)

			sha, err := client.Merge(context.Background(), 42, "aaaaaaaaaaaa", "squash")
			if err != nil {
				t.Fatalf("Merge: %v", err)
			}
			if sha != "cccccccccccc" {
				t.Fatalf("Merge returned %q, want the merge commit on the base branch", sha)
			}
			if want := []time.Duration{mergeabilityBackoff}; !slices.Equal(*asked, want) {
				t.Fatalf("the retry waited %v, want %v", *asked, want)
			}

			seen := requests()
			if len(seen) != 2 {
				t.Fatalf("GitHub saw %d requests, want the refused attempt and the retry", len(seen))
			}
			for i, request := range seen {
				if request.method != http.MethodPut || request.path != mergePath {
					t.Errorf("attempt %d was %s %s, want PUT %s", i+1, request.method, request.path, mergePath)
				}
				want := map[string]any{"sha": "aaaaaaaaaaaa", "merge_method": "squash"}
				if !reflect.DeepEqual(request.body, want) {
					t.Errorf("attempt %d sent %v, want %v", i+1, request.body, want)
				}
			}
		})
	}
}

// A refusal waiting cannot fix must be reported at once. A head that moved is
// the sharp case: the request carries the assessed head, so every retry sends
// the same stale SHA - but a branch-protection or conflict refusal is equally
// final, and GitHub answers some of them with the same 405 as a pending one.
func TestAMergeRefusalWaitingCannotFixIsNotRetried(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		message string
		moved   bool
	}{
		{
			name:    "head modified",
			status:  http.StatusConflict,
			message: "Head branch was modified. Review and try the merge again.",
			moved:   true,
		},
		{
			name:    "merge conflict",
			status:  http.StatusConflict,
			message: "Merge conflict",
		},
		{
			name:    "branch protection also answers 405",
			status:  http.StatusMethodNotAllowed,
			message: "At least 1 approving review is required by reviewers with write access.",
		},
		{
			name:    "required status check also answers 405",
			status:  http.StatusMethodNotAllowed,
			message: "Required status check \"build\" is expected.",
		},
		{
			name:    "forbidden",
			status:  http.StatusForbidden,
			message: "Resource not accessible by integration",
		},
		{
			name:    "unprocessable",
			status:  http.StatusUnprocessableEntity,
			message: "Invalid request.",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, requests := recording(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				writeAPIError(w, test.status, test.message)
			})
			client := newTestClient(t, handler, "main")

			sha, err := client.Merge(context.Background(), 42, "aaaaaaaaaaaa", "squash")
			if err == nil {
				t.Fatalf("Merge returned %q, want a refusal", sha)
			}
			if sha != "" {
				t.Errorf("Merge returned %q alongside an error", sha)
			}
			if errors.Is(err, ErrHeadModified) != test.moved {
				t.Errorf("want ErrHeadModified=%v, got %v", test.moved, err)
			}
			if seen := requests(); len(seen) != 1 {
				t.Fatalf("GitHub saw %d requests, want the refusal reported without a retry", len(seen))
			}
		})
	}
}

// A merge whose arguments are wrong is a caller bug, not a forge answer, and it
// must not reach GitHub: the retry loop would otherwise spend the window on it.
func TestAMergeRequestTheAdapterCannotFormNeverReachesGitHub(t *testing.T) {
	tests := []struct {
		name   string
		number int
		head   string
		method string
	}{
		{name: "no number", number: 0, head: "aaaaaaaaaaaa", method: "squash"},
		{name: "negative number", number: -1, head: "aaaaaaaaaaaa", method: "squash"},
		{name: "no head", number: 42, method: "squash"},
		{name: "no method", number: 42, head: "aaaaaaaaaaaa"},
		{name: "unknown method", number: 42, head: "aaaaaaaaaaaa", method: "fast-forward"},
		{name: "method is case sensitive", number: 42, head: "aaaaaaaaaaaa", method: "Squash"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, requests := recording(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, map[string]any{"merged": true, "sha": "cccccccccccc"})
			})
			client := newTestClient(t, handler, "main")

			if _, err := client.Merge(context.Background(), test.number, test.head, test.method); err == nil {
				t.Fatal("expected a refusal")
			}
			if seen := requests(); len(seen) != 0 {
				t.Fatalf("GitHub saw %d requests, want none", len(seen))
			}
		})
	}
}

// GitHub answers 200 with merged:false when it accepted the request and applied
// nothing. Reporting a SHA there would hand the watcher a commit to revert that
// was never made.
func TestAMergeGitHubAcceptedButDidNotApplyIsAnError(t *testing.T) {
	tests := []struct {
		name string
		body map[string]any
		says string
	}{
		{
			name: "not merged",
			body: map[string]any{"merged": false, "message": "Pull Request is not mergeable"},
			says: "Pull Request is not mergeable",
		},
		{
			name: "not merged but a sha is offered anyway",
			body: map[string]any{"merged": false, "sha": "deadbeefdead", "message": "Pull Request is not mergeable"},
			says: "Pull Request is not mergeable",
		},
		{name: "merged without a sha", body: map[string]any{"merged": true, "sha": ""}},
		{name: "empty object", body: map[string]any{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, requests := recording(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, test.body)
			})
			client := newTestClient(t, handler, "main")

			sha, err := client.Merge(context.Background(), 42, "aaaaaaaaaaaa", "merge")
			if err == nil {
				t.Fatalf("Merge returned %q, want an error", sha)
			}
			if sha != "" {
				t.Errorf("Merge returned %q alongside an error", sha)
			}
			if test.says != "" && !strings.Contains(err.Error(), test.says) {
				t.Errorf("error %q does not carry GitHub's own message %q", err, test.says)
			}
			if seen := requests(); len(seen) != 1 {
				t.Fatalf("GitHub saw %d requests, want one", len(seen))
			}
		})
	}
}

// Waiting out a pending assessment is bounded. A GitHub that never finishes
// deciding has to end as a reported failure with the merge not made, because
// an unattended run that waits forever holds the window open with no verdict
// and no operator watching it.
func TestAMergeGitHubNeverFinishesAssessingFailsAfterABoundedWait(t *testing.T) {
	handler, requests := recording(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		writeAPIError(w, http.StatusMethodNotAllowed, "Pull Request is not mergeable")
	})
	client := newTestClient(t, handler, "main")
	asked := recordMergeBackoff(client)

	sha, err := client.Merge(context.Background(), 42, "aaaaaaaaaaaa", "squash")
	if err == nil {
		t.Fatalf("Merge returned %q, want a reported failure", sha)
	}
	if sha != "" {
		t.Errorf("Merge returned %q alongside an error", sha)
	}
	if !strings.Contains(err.Error(), "Pull Request is not mergeable") {
		t.Errorf("error %q does not carry GitHub's last answer", err)
	}
	if seen := requests(); len(seen) != mergeabilityAttempts {
		t.Fatalf("GitHub saw %d requests, want the wait bounded at %d attempts", len(seen), mergeabilityAttempts)
	}
	for i, waited := range *asked {
		if waited != mergeabilityBackoff {
			t.Fatalf("wait %d was %s, want %s", i+1, waited, mergeabilityBackoff)
		}
	}
	if len(*asked) != mergeabilityAttempts {
		t.Fatalf("the loop waited %d times, want one wait per attempt", len(*asked))
	}
}

// The bound is only a safety property while it is short: a merge that keeps
// waiting holds the window open with nothing watching it. Asserting the product
// of the two constants pins the wrong thing, because the call site decides how
// often and how long it waits - time.After(mergeabilityBackoff * 4) is a 48s
// unattended wait that a product assertion still calls 12s.
//
// So what is measured is how long the loop actually takes: the delays it asks
// the adapter's clock for, plus the wall-clock it spent regardless. A wait
// moved off the clock seam shows up in the second term, so neither constant nor
// call site can grow the wait without this failing.
func TestTheMergeWaitStaysShortEnoughForAnUnattendedRunToHoldItsWindow(t *testing.T) {
	const ceiling = 15 * time.Second

	handler, requests := recording(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		writeAPIError(w, http.StatusMethodNotAllowed, "Pull Request is not mergeable")
	})
	client := newTestClient(t, handler, "main")
	recorded := recordMergeBackoff(client)

	started := time.Now()
	sha, err := client.Merge(context.Background(), 42, "aaaaaaaaaaaa", "squash")
	spent := time.Since(started)

	if err == nil {
		t.Fatalf("Merge returned %q, want a reported failure", sha)
	}
	var asked time.Duration
	for _, waited := range *recorded {
		asked += waited
	}
	if total := asked + spent; total > ceiling {
		t.Fatalf("a merge waits up to %s for GitHub to decide (%s asked of the clock, %s spent off it), unattended and holding the window open",
			total, asked, spent)
	}
	if seen := requests(); len(seen) < 2 {
		t.Fatalf("GitHub saw %d requests, so no wait was measured at all", len(seen))
	}
}

func TestAMergeCarriesTheBearerTokenAndAJSONContentType(t *testing.T) {
	handler, requests := recording(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"merged": true, "sha": "cccccccccccc"})
	})
	client := newTestClient(t, handler, "main")

	if _, err := client.Merge(context.Background(), 42, "aaaaaaaaaaaa", "rebase"); err != nil {
		t.Fatalf("Merge: %v", err)
	}
	seen := requests()
	if len(seen) != 1 {
		t.Fatalf("GitHub saw %d requests, want one", len(seen))
	}
	if want := "Bearer " + fixtureToken; seen[0].authorization != want {
		t.Errorf("Authorization = %q, want the configured token %q", seen[0].authorization, want)
	}
	if seen[0].contentType != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", seen[0].contentType)
	}
}
