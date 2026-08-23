package github

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/lkshrk/ops-pilot/internal/domain"
	"github.com/lkshrk/ops-pilot/internal/testutil/githubserver"
)

func pullFixture(number int, base, state string, merged bool) map[string]any {
	return map[string]any{
		"number":   number,
		"state":    state,
		"title":    "chore(deps): update podinfo",
		"html_url": "https://github.com/acme/cluster/pull/42",
		"merged":   merged,
		"user":     map[string]any{"login": "renovate[bot]"},
		"head": map[string]any{
			"sha":  "aaaaaaaaaaaa",
			"ref":  "renovate/podinfo",
			"repo": map[string]any{"full_name": "renovate/cluster"},
		},
		"base": map[string]any{"sha": "bbbbbbbbbbbb", "ref": base},
	}
}

func newTestClient(t *testing.T, handler http.Handler, branch string) *Client {
	t.Helper()
	return newTestClientFor(t, handler, domain.RepositoryRef{
		Owner:  "acme",
		Name:   "cluster",
		Branch: branch,
	})
}

func newTestClientFor(t *testing.T, handler http.Handler, repository domain.RepositoryRef) *Client {
	t.Helper()
	server := githubserver.New(t, fixtureToken, handler.ServeHTTP)
	t.Cleanup(server.Close)
	client, err := New(server.Client(), fixtureToken, server.URL, repository)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

func pullHandler(t *testing.T, body map[string]any) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/cluster/pulls/42" {
			t.Errorf("unexpected request path %q", r.URL.Path)
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})
}

func TestGetReturnsPullRequestOnConfiguredBranch(t *testing.T) {
	client := newTestClient(t, pullHandler(t, pullFixture(42, "main", "open", false)), "main")

	request, err := client.Get(context.Background(), 42)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if request.Number != 42 || request.BaseRef != "main" || request.HeadSHA != "aaaaaaaaaaaa" ||
		request.HeadRef != "renovate/podinfo" || request.HeadRepository != "renovate/cluster" {
		t.Fatalf("unexpected pull request %+v", request)
	}
}

func TestGetAllowsAMissingHeadRepository(t *testing.T) {
	fixture := pullFixture(42, "main", "open", false)
	fixture["head"].(map[string]any)["repo"] = nil
	client := newTestClient(t, pullHandler(t, fixture), "main")

	request, err := client.Get(context.Background(), 42)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if request.HeadRepository != "" {
		t.Fatalf("HeadRepository = %q, want empty", request.HeadRepository)
	}
}

func TestGetRejectsPullRequestOnAnotherBranch(t *testing.T) {
	client := newTestClient(t, pullHandler(t, pullFixture(42, "develop", "open", false)), "main")

	_, err := client.Get(context.Background(), 42)
	if err == nil {
		t.Fatal("expected an error for a pull request targeting another branch")
	}
	for _, want := range []string{"#42", `"develop"`, `"main"`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %s", err, want)
		}
	}
}

func TestGetUsesDefaultBranchWhenUnconfigured(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/acme/cluster":
			_ = json.NewEncoder(w).Encode(map[string]any{"default_branch": "main"})
		case "/repos/acme/cluster/pulls/42":
			_ = json.NewEncoder(w).Encode(pullFixture(42, "develop", "open", false))
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	})
	client := newTestClient(t, handler, "")

	_, err := client.Get(context.Background(), 42)
	if err == nil || !strings.Contains(err.Error(), `"main"`) {
		t.Fatalf("expected a rejection naming the default branch, got %v", err)
	}
}

func TestGetRejectsMergedAndClosedPullRequests(t *testing.T) {
	for name, fixture := range map[string]map[string]any{
		"merged": pullFixture(42, "main", "closed", true),
		"closed": pullFixture(42, "main", "closed", false),
	} {
		t.Run(name, func(t *testing.T) {
			client := newTestClient(t, pullHandler(t, fixture), "main")

			_, err := client.Get(context.Background(), 42)
			if err == nil || !strings.Contains(err.Error(), name) {
				t.Fatalf("expected a %s rejection, got %v", name, err)
			}
		})
	}
}

// A 409 naming the head is GitHub refusing a SHA other than the one the request
// carried. Waiting cannot help - the request carries the head that was assessed
// and the branch has moved past it - so it is reported at once, and typed, in
// contrast to a mergeability answer GitHub has not finished computing.
func TestAHeadModifiedRefusalIsReportedAtOnceRatherThanWaitedOut(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		message string
		moved   bool
		waits   bool
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
			name:    "mergeability not computed yet",
			status:  http.StatusMethodNotAllowed,
			message: "Pull Request is not mergeable",
			waits:   true,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			client := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(test.status)
				_ = json.NewEncoder(w).Encode(map[string]any{"message": test.message})
			}), "main")

			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			_, err := client.Merge(ctx, 42, "aaaaaaaaaaaa", "squash")

			if err == nil {
				t.Fatal("expected a refusal")
			}
			if errors.Is(err, ErrHeadModified) != test.moved {
				t.Fatalf("want ErrHeadModified=%v, got %v", test.moved, err)
			}
			if errors.Is(err, context.DeadlineExceeded) != test.waits {
				t.Fatalf("want the merge to wait=%v, got %v", test.waits, err)
			}
		})
	}
}

// A base ref other than the effective branch is dropped inside the pagination
// loop, so a repository whose Renovate pull requests all target another branch is
// indistinguishable from an idle one. OtherBases exists to count them, and only
// to count them: it must narrow by author and label as any other listing does.
func TestAnOtherBasesListingSeesThePullRequestsTheBaseBranchNarrowingDropped(t *testing.T) {
	page := []map[string]any{
		pullFixture(1, "master", "open", false),
		pullFixture(2, "master", "open", false),
		pullFixture(3, "main", "open", false),
		pullFixture(4, "release-1", "open", false),
	}
	page[3]["user"] = map[string]any{"login": "someone-else"}
	cases := []struct {
		name    string
		filter  domain.PullRequestFilter
		numbers []int
	}{
		{
			name:    "the base branch narrowing is what hides them",
			numbers: []int{3},
		},
		{
			name:    "the other base refs, still narrowed by author",
			filter:  domain.PullRequestFilter{OtherBases: true, Authors: []string{"renovate[bot]"}},
			numbers: []int{1, 2},
		},
		{
			name:    "the other base refs and every author",
			filter:  domain.PullRequestFilter{OtherBases: true},
			numbers: []int{1, 2, 4},
		},
		{
			name:    "no pull request is counted twice",
			filter:  domain.PullRequestFilter{OtherBases: true, Authors: []string{"nobody"}},
			numbers: nil,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/repos/acme/cluster/pulls" {
					t.Errorf("unexpected request path %q", r.URL.Path)
					http.Error(w, "unexpected path", http.StatusNotFound)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(page)
			})
			client := newTestClient(t, handler, "main")

			requests, err := client.ListOpen(context.Background(), test.filter)
			if err != nil {
				t.Fatalf("ListOpen: %v", err)
			}
			var got []int
			for _, request := range requests {
				got = append(got, request.Number)
			}
			if len(got) != len(test.numbers) {
				t.Fatalf("ListOpen returned %v, want %v", got, test.numbers)
			}
			for i := range got {
				if got[i] != test.numbers[i] {
					t.Fatalf("ListOpen returned %v, want %v", got, test.numbers)
				}
			}
		})
	}
}

// A merge method GitHub does not know is refused before the request leaves, so
// a typo in the configuration cannot reach the API as an opaque 422.
func TestAMergeMethodTheForgeDoesNotKnowIsRefusedBeforeAnythingIsSent(t *testing.T) {
	tests := []struct {
		method  string
		refused bool
	}{
		{method: "merge"},
		{method: "squash"},
		{method: "rebase"},
		{method: "", refused: true},
		{method: "Squash", refused: true},
		{method: "fast-forward", refused: true},
	}
	for _, test := range tests {
		t.Run(cmp.Or(test.method, "empty"), func(t *testing.T) {
			handler, requests := recording(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, map[string]any{"merged": true, "sha": "cccccccccccc"})
			})
			client := newTestClient(t, handler, "main")

			sha, err := client.Merge(context.Background(), 42, "aaaaaaaaaaaa", test.method)
			if test.refused {
				if err == nil {
					t.Fatalf("Merge accepted method %q, returning %q", test.method, sha)
				}
				if seen := requests(); len(seen) != 0 {
					t.Fatalf("GitHub saw %d requests, want the refusal before anything was sent", len(seen))
				}
				return
			}
			if err != nil {
				t.Fatalf("Merge with method %q: %v", test.method, err)
			}
			if sha != "cccccccccccc" {
				t.Fatalf("Merge returned %q, want the merge commit", sha)
			}
		})
	}
}

// The object bound is checked as the listing accumulates, so a page that busts
// it stops the walk there. Judging the total after the walk would reach the same
// verdict having first fetched every remaining page, which is why the request
// count is asserted and not only the error.
func TestAPullRequestListingPastTheObjectLimitIsRefusedWithoutFetchingMore(t *testing.T) {
	tests := []struct {
		name    string
		count   int
		refused bool
	}{
		{name: "exactly the object limit", count: maxObjects},
		{name: "one past the object limit", count: maxObjects + 1, refused: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			page := make([]map[string]any, 0, test.count)
			for i := range test.count {
				page = append(page, pullFixture(i+1, "main", "open", false))
			}
			handler, requests := recording(t, func(_ int, w http.ResponseWriter, r *http.Request) {
				if test.refused {
					number, err := strconv.Atoi(cmp.Or(r.URL.Query().Get("page"), "1"))
					if err != nil {
						t.Errorf("page %q is not a number", r.URL.Query().Get("page"))
						http.Error(w, "unexpected page", http.StatusBadRequest)
						return
					}
					w.Header().Set("Link", fmt.Sprintf(
						`<http://%s/repos/acme/cluster/pulls?state=open&per_page=100&page=%d>; rel="next"`,
						r.Host, number+1))
				}
				writeJSON(w, http.StatusOK, page)
			})
			client := newTestClient(t, handler, "main")

			open, err := client.ListOpen(context.Background(), domain.PullRequestFilter{})
			if seen := requests(); len(seen) != 1 {
				t.Fatalf("GitHub saw %d requests, want the verdict from the first page alone", len(seen))
			}
			if !test.refused {
				if err != nil {
					t.Fatalf("ListOpen: %v", err)
				}
				if len(open) != test.count {
					t.Fatalf("ListOpen returned %d pull requests, want %d", len(open), test.count)
				}
				return
			}
			if err == nil {
				t.Fatalf("ListOpen returned %d pull requests, want a refusal past the object limit", len(open))
			}
			if open != nil {
				t.Errorf("ListOpen returned %d pull requests alongside an error", len(open))
			}
			if !strings.Contains(err.Error(), "object limit") {
				t.Errorf("ListOpen failed with %v, want the object limit named", err)
			}
		})
	}
}

// The same bound guards the changed-file walk, and there the short listing is
// sharper than a missing candidate: the paths are what the agent is shown of the
// pull request, so a truncated list is an assessment of a change with its
// dangerous file removed. paginate turns any visitor error wrapping
// errStopPagination into success, so the refusal has to arrive as an error and
// not merely stop the walk.
func TestAChangedFileListingPastTheObjectLimitIsRefusedRatherThanTruncated(t *testing.T) {
	tests := []struct {
		name    string
		count   int
		refused bool
	}{
		{name: "exactly the object limit", count: maxObjects},
		{name: "one past the object limit", count: maxObjects + 1, refused: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			page := make([]map[string]any, 0, test.count)
			for i := range test.count {
				page = append(page, map[string]any{
					"filename": fmt.Sprintf("clusters/prod/app%d.yaml", i+1),
					"status":   "modified",
				})
			}
			handler, requests := recording(t, func(_ int, w http.ResponseWriter, r *http.Request) {
				if test.refused {
					number, err := strconv.Atoi(cmp.Or(r.URL.Query().Get("page"), "1"))
					if err != nil {
						t.Errorf("page %q is not a number", r.URL.Query().Get("page"))
						http.Error(w, "unexpected page", http.StatusBadRequest)
						return
					}
					w.Header().Set("Link", fmt.Sprintf(
						`<http://%s/repos/acme/cluster/pulls/42/files?per_page=100&page=%d>; rel="next"`,
						r.Host, number+1))
				}
				writeJSON(w, http.StatusOK, page)
			})
			client := newTestClient(t, handler, "main")

			files, err := client.ChangedFiles(context.Background(), 42)
			if seen := requests(); len(seen) != 1 {
				t.Fatalf("GitHub saw %d requests, want the verdict from the first page alone", len(seen))
			}
			if !test.refused {
				if err != nil {
					t.Fatalf("ChangedFiles: %v", err)
				}
				if len(files) != test.count {
					t.Fatalf("ChangedFiles returned %d files, want %d", len(files), test.count)
				}
				return
			}
			if err == nil {
				t.Fatalf("ChangedFiles returned %d files, want a refusal past the object limit", len(files))
			}
			if files != nil {
				t.Errorf("ChangedFiles returned %d files alongside an error", len(files))
			}
			if !strings.Contains(err.Error(), "object limit") {
				t.Errorf("ChangedFiles failed with %v, want the object limit named", err)
			}
		})
	}
}

// "Other" is measured against the branch this run would merge into, which an
// unconfigured repository.branch leaves to the forge - the case where the quiet
// line cannot name the branch and the count is the only clue there is.
func TestAnOtherBasesListingMeasuresAgainstTheDefaultBranchWhenNoneIsConfigured(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/acme/cluster":
			_ = json.NewEncoder(w).Encode(map[string]any{"default_branch": "main"})
		case "/repos/acme/cluster/pulls":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				pullFixture(1, "master", "open", false),
				pullFixture(2, "main", "open", false),
			})
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
			http.Error(w, "unexpected path", http.StatusNotFound)
		}
	})
	client := newTestClient(t, handler, "")

	requests, err := client.ListOpen(context.Background(), domain.PullRequestFilter{OtherBases: true})
	if err != nil {
		t.Fatalf("ListOpen: %v", err)
	}
	if len(requests) != 1 || requests[0].Number != 1 {
		t.Fatalf("ListOpen returned %+v, want only the pull request aimed off the default branch", requests)
	}
}

func TestMergeStateAnswersWhetherAPullRequestActuallyMerged(t *testing.T) {
	tests := []struct {
		name    string
		body    map[string]any
		want    domain.MergeState
		wantErr bool
	}{
		{
			name: "merged, with the commit it produced",
			body: map[string]any{
				"number": 42, "state": "closed", "merged": true,
				"merge_commit_sha": "cccccccccccc",
				"head":             map[string]any{"sha": "aaaaaaaaaaaa", "ref": "renovate/podinfo"},
				"base":             map[string]any{"sha": "bbbbbbbbbbbb", "ref": "main"},
			},
			want: domain.MergeState{Merged: true, SHA: "cccccccccccc"},
		},
		{
			name: "still open, so nothing landed",
			body: map[string]any{
				"number": 42, "state": "open", "merged": false,
				"merge_commit_sha": "dddddddddddd",
				"head":             map[string]any{"sha": "aaaaaaaaaaaa", "ref": "renovate/podinfo"},
				"base":             map[string]any{"sha": "bbbbbbbbbbbb", "ref": "main"},
			},
			want: domain.MergeState{},
		},
		{
			name: "merged with no commit is not an answer",
			body: map[string]any{
				"number": 42, "state": "closed", "merged": true,
				"head": map[string]any{"sha": "aaaaaaaaaaaa", "ref": "renovate/podinfo"},
				"base": map[string]any{"sha": "bbbbbbbbbbbb", "ref": "main"},
			},
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := newTestClient(t, pullHandler(t, test.body), "main")

			state, err := client.MergeState(context.Background(), 42)

			if test.wantErr {
				if err == nil {
					t.Fatalf("MergeState accepted %v", test.body)
				}
				return
			}
			if err != nil {
				t.Fatalf("MergeState: %v", err)
			}
			if state != test.want {
				t.Fatalf("MergeState = %+v, want %+v", state, test.want)
			}
		})
	}
}

func TestMergeStateAnswersForAPullRequestGetRefuses(t *testing.T) {
	body := map[string]any{
		"number": 42, "state": "closed", "merged": true,
		"merge_commit_sha": "cccccccccccc",
		"head":             map[string]any{"sha": "aaaaaaaaaaaa", "ref": "renovate/podinfo"},
		"base":             map[string]any{"sha": "bbbbbbbbbbbb", "ref": "main"},
	}
	client := newTestClient(t, pullHandler(t, body), "main")

	if _, err := client.Get(context.Background(), 42); err == nil {
		t.Fatal("Get accepted a merged pull request; this test pins the wrong seam")
	}
	state, err := client.MergeState(context.Background(), 42)
	if err != nil {
		t.Fatalf("MergeState: %v", err)
	}
	if !state.Merged || state.SHA != "cccccccccccc" {
		t.Fatalf("MergeState = %+v, want the merge and its commit", state)
	}
}
