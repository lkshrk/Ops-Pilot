package github

import (
	"context"
	"net/http"
	"reflect"
	"testing"
)

// The annotation is the only signal an operator gets about what the run did,
// so its body has to arrive verbatim on the pull request's issue timeline.
func TestACommentIsPostedToTheIssueTimelineWithItsBodyVerbatim(t *testing.T) {
	body := "ops-pilot reverted this merge.\n\n- cluster did not recover within 10m0s\n- revert commit `abc1234`\n"
	handler, requests := recording(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusCreated, map[string]any{"id": 1})
	})
	client := newTestClient(t, handler, "main")

	if err := client.Comment(context.Background(), 42, body); err != nil {
		t.Fatalf("Comment: %v", err)
	}
	seen := requests()
	if len(seen) != 1 {
		t.Fatalf("GitHub saw %d requests, want one", len(seen))
	}
	if seen[0].method != http.MethodPost || seen[0].path != "/repos/acme/cluster/issues/42/comments" {
		t.Fatalf("comment was %s %s, want POST /repos/acme/cluster/issues/42/comments", seen[0].method, seen[0].path)
	}
	if want := (map[string]any{"body": body}); !reflect.DeepEqual(seen[0].body, want) {
		t.Fatalf("comment payload = %#v, want %#v", seen[0].body, want)
	}
}

// A label is what makes a later run skip the pull request, so the set sent has
// to be exactly the set asked for - dropping one re-opens a pull request the
// run decided against, adding one hides a pull request nothing decided about.
func TestALabelSetIsPostedToTheIssueTimelineExactlyAsAsked(t *testing.T) {
	tests := []struct {
		name   string
		labels []string
	}{
		{name: "one", labels: []string{"ops-pilot/reverted"}},
		{name: "several", labels: []string{"ops-pilot/reverted", "ops-pilot/held", "needs-review"}},
		{name: "a label with a space", labels: []string{"do not merge"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, requests := recording(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, []any{})
			})
			client := newTestClient(t, handler, "main")

			if err := client.AddLabel(context.Background(), 42, test.labels...); err != nil {
				t.Fatalf("AddLabel: %v", err)
			}
			seen := requests()
			if len(seen) != 1 {
				t.Fatalf("GitHub saw %d requests, want one", len(seen))
			}
			if seen[0].method != http.MethodPost || seen[0].path != "/repos/acme/cluster/issues/42/labels" {
				t.Fatalf("label add was %s %s, want POST /repos/acme/cluster/issues/42/labels", seen[0].method, seen[0].path)
			}
			want := make([]any, len(test.labels))
			for i, label := range test.labels {
				want[i] = label
			}
			if got := (map[string]any{"labels": want}); !reflect.DeepEqual(seen[0].body, got) {
				t.Fatalf("label payload = %#v, want %#v", seen[0].body, got)
			}
		})
	}
}

func TestClosingAPullRequestPatchesItsStateToClosed(t *testing.T) {
	handler, requests := recording(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, pullFixture(42, "main", "closed", false))
	})
	client := newTestClient(t, handler, "main")

	if err := client.Close(context.Background(), 42); err != nil {
		t.Fatalf("Close: %v", err)
	}
	seen := requests()
	if len(seen) != 1 {
		t.Fatalf("GitHub saw %d requests, want one", len(seen))
	}
	if seen[0].method != http.MethodPatch || seen[0].path != "/repos/acme/cluster/pulls/42" {
		t.Fatalf("close was %s %s, want PATCH /repos/acme/cluster/pulls/42", seen[0].method, seen[0].path)
	}
	if want := (map[string]any{"state": "closed"}); !reflect.DeepEqual(seen[0].body, want) {
		t.Fatalf("close payload = %#v, want %#v", seen[0].body, want)
	}
}

func TestAnAnnotationWithNothingToSayNeverReachesGitHub(t *testing.T) {
	tests := []struct {
		name string
		call func(*Client) error
	}{
		{name: "comment on no pull request", call: func(c *Client) error { return c.Comment(context.Background(), 0, "hello") }},
		{name: "comment on a negative number", call: func(c *Client) error { return c.Comment(context.Background(), -1, "hello") }},
		{name: "empty comment", call: func(c *Client) error { return c.Comment(context.Background(), 42, "") }},
		{name: "whitespace comment", call: func(c *Client) error { return c.Comment(context.Background(), 42, " \n\t ") }},
		{name: "label on no pull request", call: func(c *Client) error { return c.AddLabel(context.Background(), 0, "held") }},
		{name: "no labels", call: func(c *Client) error { return c.AddLabel(context.Background(), 42) }},
		{name: "close no pull request", call: func(c *Client) error { return c.Close(context.Background(), 0) }},
		{name: "close a negative number", call: func(c *Client) error { return c.Close(context.Background(), -1) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, requests := recording(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, map[string]any{})
			})
			client := newTestClient(t, handler, "main")

			if err := test.call(client); err == nil {
				t.Fatal("expected a refusal")
			}
			if seen := requests(); len(seen) != 0 {
				t.Fatalf("GitHub saw %d requests, want none", len(seen))
			}
		})
	}
}

// These three are not replayable: a retried comment is a second comment on the
// operator's pull request. A failure is reported once, not attempted again.
func TestAFailedAnnotationIsReportedRatherThanRetriedIntoADuplicate(t *testing.T) {
	tests := []struct {
		name string
		call func(*Client) error
	}{
		{name: "comment", call: func(c *Client) error { return c.Comment(context.Background(), 42, "hello") }},
		{name: "label", call: func(c *Client) error { return c.AddLabel(context.Background(), 42, "held") }},
		{name: "close", call: func(c *Client) error { return c.Close(context.Background(), 42) }},
	}
	for _, status := range []int{http.StatusInternalServerError, http.StatusTooManyRequests, http.StatusForbidden} {
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				handler, requests := recording(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
					writeAPIError(w, status, "Server Error")
				})
				client := newTestClient(t, handler, "main")

				if err := test.call(client); err == nil {
					t.Fatalf("expected status %d to be reported", status)
				}
				if seen := requests(); len(seen) != 1 {
					t.Fatalf("GitHub saw %d requests for status %d, want exactly one", len(seen), status)
				}
			})
		}
	}
}
