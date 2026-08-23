package github

import (
	"context"
	"net/http"
	"testing"

	"github.com/lkshrk/ops-pilot/internal/domain"
)

// emptyBody serves status with no payload at all, the way a 204 or a proxy that
// drops response bodies answers.
func emptyBody(t *testing.T, status int, payload string) (http.Handler, func() []recordedRequest) {
	t.Helper()
	return recording(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		if payload != "" {
			_, _ = w.Write([]byte(payload))
		}
	})
}

// The write already happened by the time the body is read, so reporting a 2xx
// that carried no payload as a failure tells the operator a comment or label
// they can see on the pull request was never made.
func TestAWriteAnsweredWithASuccessCarryingNoPayloadIsNotReportedAsAFailure(t *testing.T) {
	writes := []struct {
		name string
		call func(*Client) error
	}{
		{name: "comment", call: func(c *Client) error { return c.Comment(context.Background(), 42, "hello") }},
		{name: "label", call: func(c *Client) error { return c.AddLabel(context.Background(), 42, "held") }},
		{name: "close", call: func(c *Client) error { return c.Close(context.Background(), 42) }},
	}
	replies := []struct {
		name    string
		status  int
		payload string
	}{
		{name: "204 no content", status: http.StatusNoContent},
		{name: "200 with no body", status: http.StatusOK},
		{name: "201 with no body", status: http.StatusCreated},
		{name: "200 with only a newline", status: http.StatusOK, payload: "\n"},
		{name: "200 with only whitespace", status: http.StatusOK, payload: " \r\n\t"},
	}
	for _, write := range writes {
		for _, reply := range replies {
			t.Run(write.name+"/"+reply.name, func(t *testing.T) {
				handler, requests := emptyBody(t, reply.status, reply.payload)
				client := newTestClient(t, handler, "main")

				if err := write.call(client); err != nil {
					t.Fatalf("%s against %s: %v", write.name, reply.name, err)
				}
				if seen := requests(); len(seen) != 1 {
					t.Fatalf("GitHub saw %d requests, want one", len(seen))
				}
			})
		}
	}
}

// The other direction: an empty 2xx where the answer itself is the result must
// stay an error. Merge reads the commit SHA out of it, and a caller handed ""
// would revert a commit that was never made.
func TestASuccessCarryingNoPayloadIsStillAnErrorWhereThePayloadIsTheAnswer(t *testing.T) {
	reads := []struct {
		name string
		call func(*Client) error
	}{
		{name: "merge", call: func(c *Client) error {
			_, err := c.Merge(context.Background(), 42, "aaaaaaaaaaaa", "squash")
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
		{name: "file at", call: func(c *Client) error {
			_, _, err := c.FileAt(context.Background(), "clusters/prod.yaml", "beforesha")
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
	for _, read := range reads {
		for _, status := range []int{http.StatusNoContent, http.StatusOK} {
			t.Run(read.name+"/"+http.StatusText(status), func(t *testing.T) {
				handler, _ := emptyBody(t, status, "")
				client := newTestClient(t, handler, "main")

				if err := read.call(client); err == nil {
					t.Fatalf("%s accepted a %d with no payload", read.name, status)
				}
			})
		}
	}
}

// escapePath exists so a path segment cannot smuggle a delimiter into the URL.
// Only half of that survives endpoint(), which rebuilds the URL from rel.Path
// and drops rel.RawPath: characters url.URL re-escapes on its own reach GitHub
// escaped, so a segment holding # or ? still cannot cut the path short. This is
// the half the unit test on escapePath cannot see, so it is pinned at the wire.
func TestASegmentThatWouldCutThePathShortReachesGitHubStillEscaped(t *testing.T) {
	tests := []struct{ name, path, want string }{
		{
			name: "hash would start a fragment",
			path: "charts/podinfo#rc.tgz",
			want: "/repos/acme/cluster/contents/charts/podinfo%23rc.tgz",
		},
		{
			name: "question mark would start a query",
			path: "charts/what?ref=other.tgz",
			want: "/repos/acme/cluster/contents/charts/what%3Fref=other.tgz",
		},
		{
			name: "percent would open an escape",
			path: "charts/100%.tgz",
			want: "/repos/acme/cluster/contents/charts/100%25.tgz",
		},
		{
			name: "space",
			path: "clusters/app v2.yaml",
			want: "/repos/acme/cluster/contents/clusters/app%20v2.yaml",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, requests := recording(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, map[string]any{
					"type": "file", "encoding": "base64", "content": "",
				})
			})
			client := newTestClient(t, handler, "main")

			if _, _, err := client.FileAt(context.Background(), test.path, "beforesha"); err != nil {
				t.Fatalf("FileAt: %v", err)
			}
			seen := requests()
			if len(seen) != 1 {
				t.Fatalf("GitHub saw %d requests, want one", len(seen))
			}
			if seen[0].path != test.want {
				t.Fatalf("GitHub received %q, want %q", seen[0].path, test.want)
			}
			if got := seen[0].query.Get("ref"); got != "beforesha" {
				t.Fatalf("ref arrived as %q, want beforesha", got)
			}
		})
	}
}

// endpoint() takes rel.Path, so a %2F is a separator again before the URL is rebuilt.
func TestAnEscapedSeparatorCollapsesBackToASeparatorTheGitRefEndpointCanRead(t *testing.T) {
	tests := []struct{ name, branch, want string }{
		{name: "flat", branch: "main", want: "/repos/acme/cluster/git/ref/heads/main"},
		{name: "one slash", branch: "release/1.2", want: "/repos/acme/cluster/git/ref/heads/release/1.2"},
		{name: "several slashes", branch: "team/ops/prod", want: "/repos/acme/cluster/git/ref/heads/team/ops/prod"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, requests := recording(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, map[string]any{"object": map[string]any{"sha": "aaaaaaaaaaaa"}})
			})
			client := newTestClient(t, handler, "main")

			sha, err := client.BranchHead(context.Background(), test.branch)
			if err != nil {
				t.Fatalf("BranchHead: %v", err)
			}
			if sha != "aaaaaaaaaaaa" {
				t.Fatalf("BranchHead returned %q", sha)
			}
			seen := requests()
			if len(seen) != 1 {
				t.Fatalf("GitHub saw %d requests, want one", len(seen))
			}
			if seen[0].path != test.want {
				t.Fatalf("GitHub received %q, want %q", seen[0].path, test.want)
			}
		})
	}
}

// A repository the URL parser cannot read reaches endpoint() unescaped, because
// repositoryPath concatenates the configured owner and name verbatim. The whole
// point of the adapter is that an unattended run reports what went wrong; a
// panic takes the process down with the merge window open instead.
func TestARequestPathTheURLParserRejectsIsReportedRatherThanCrashingTheRun(t *testing.T) {
	calls := []struct {
		name string
		call func(*Client) error
	}{
		{name: "list open", call: func(c *Client) error {
			_, err := c.ListOpen(context.Background(), domain.PullRequestFilter{})
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
		{name: "comment", call: func(c *Client) error { return c.Comment(context.Background(), 42, "hello") }},
		// FileAt reports a 404 as absence, so the refusal must not arrive shaped like
		// one: a revert planner told the file is absent restores nothing.
		{name: "file at", call: func(c *Client) error {
			_, found, err := c.FileAt(context.Background(), "clusters/prod.yaml", "beforesha")
			if found {
				return nil
			}
			return err
		}},
	}
	for _, call := range calls {
		t.Run(call.name, func(t *testing.T) {
			handler, requests := recording(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, map[string]any{})
			})
			client := newTestClientFor(t, handler, domain.RepositoryRef{
				Owner:  "acme%zz",
				Name:   "cluster",
				Branch: "main",
			})

			if err := call.call(client); err == nil {
				t.Fatal("an unparseable request path produced an answer instead of a refusal")
			}
			if seen := requests(); len(seen) != 0 {
				t.Fatalf("GitHub saw %d requests, want none", len(seen))
			}
		})
	}
}

// url.PathEscape leaves a dot segment alone and endpoint() rebuilds the URL from
// the decoded path, so a branch or file path holding ".." reaches the wire as a
// traversal out of the configured repository. api.github.com does not resolve it,
// but a normalising proxy or GHES in front of it would, and then BranchHead
// answers with another repository's head - the SHA a revert is planned against.
func TestARequestPathWithDotSegmentsIsRefusedRatherThanSentForAProxyToResolve(t *testing.T) {
	calls := []struct {
		name string
		call func(*Client) error
	}{
		{name: "branch head climbs out", call: func(c *Client) error {
			_, err := c.BranchHead(context.Background(), "../../../other/repo/git/ref/heads/main")
			return err
		}},
		{name: "file at climbs out", call: func(c *Client) error {
			_, found, err := c.FileAt(context.Background(), "../../../other/repo/contents/prod.yaml", "beforesha")
			if found {
				return nil
			}
			return err
		}},
		{name: "single dot", call: func(c *Client) error {
			_, found, err := c.FileAt(context.Background(), "clusters/./prod.yaml", "beforesha")
			if found {
				return nil
			}
			return err
		}},
	}
	for _, call := range calls {
		t.Run(call.name, func(t *testing.T) {
			handler, requests := recording(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, map[string]any{
					"type": "file", "encoding": "base64", "content": "",
					"object": map[string]any{"sha": "aaaaaaaaaaaa"},
				})
			})
			client := newTestClient(t, handler, "main")

			if err := call.call(client); err == nil {
				t.Fatal("a path carrying dot segments produced an answer instead of a refusal")
			}
			if seen := requests(); len(seen) != 0 {
				t.Fatalf("GitHub received %q, want the request refused at the client", seen[0].path)
			}
		})
	}
}

// url.Parse decodes one escape layer, so a double-encoded dot segment reaches
// endpoint() as %2e%2e rather than .., slips the literal-segment check, and is
// re-emitted on the wire for a proxy that decodes a second time to resolve to a
// traversal out of the configured repository.
func TestADoubleEncodedDotSegmentIsRefusedBeforeAProxyCanResolveIt(t *testing.T) {
	client := newTestClient(t, http.NotFoundHandler(), "main")

	refused := []string{
		"/repos/acme/cluster/git/ref/heads/%2e%2e/x",
		"/repos/acme/cluster/git/ref/heads/%2E%2E/x",
		"/repos/acme/cluster/git/ref/heads/%252e%252e/x",
		"/repos/acme/cluster/contents/%252e%252e%252fsecret.yaml",
		"/repos/acme/cluster/git/ref/heads/%25252e%25252e/x",
	}
	for _, path := range refused {
		if _, err := client.endpoint(path); err == nil {
			t.Fatalf("endpoint(%q) produced a target instead of refusing an encoded traversal", path)
		}
	}

	allowed := []struct{ name, path string }{
		{name: "literal percent in a filename", path: "/repos/acme/cluster/contents/charts/100%25.tgz"},
		{name: "encoded separator inside a branch ref", path: "/repos/acme/cluster/git/ref/heads/release%2F1.2"},
		{name: "encoded space", path: "/repos/acme/cluster/contents/app%20v2.yaml"},
	}
	for _, test := range allowed {
		t.Run(test.name, func(t *testing.T) {
			if _, err := client.endpoint(test.path); err != nil {
				t.Fatalf("endpoint(%q) refused a legitimate path: %v", test.path, err)
			}
		})
	}
}

// A 2xx body that is present but not JSON is a proxy or an error page, never a
// GitHub answer, and it is still refused.
func TestASuccessCarryingSomethingThatIsNotJSONIsStillRefused(t *testing.T) {
	for _, payload := range []string{"<html>gateway</html>", "{", `{"ok":1}{"ok":2}`, "\x00"} {
		t.Run(payload, func(t *testing.T) {
			handler, _ := emptyBody(t, http.StatusOK, payload)
			client := newTestClient(t, handler, "main")

			if err := client.Comment(context.Background(), 42, "hello"); err == nil {
				t.Fatalf("a 2xx carrying %q was accepted", payload)
			}
		})
	}
}
