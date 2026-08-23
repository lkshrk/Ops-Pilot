package github

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/lkshrk/ops-pilot/internal/domain"
)

// pagingCaller is one of the collection walks the adapter performs, with the
// path it pins and the repository-ID path GitHub answers that collection with.
type pagingCaller struct {
	name       string
	collection string
	idPath     string
	page       func(number int) any
	walk       func(*Client) (int, error)
}

func pagingCallers() []pagingCaller {
	return []pagingCaller{
		{
			name:       "release listing",
			collection: "/repos/acme/upstream/releases",
			idPath:     "/repositories/139727567/releases",
			page: func(number int) any {
				return []map[string]any{{"tag_name": fmt.Sprintf("v1.%d.0", number)}}
			},
			walk: func(c *Client) (int, error) {
				history, err := c.Releases(context.Background(), "acme/upstream")
				return len(history.Releases), err
			},
		},
		{
			name:       "open pull requests",
			collection: "/repos/acme/cluster/pulls",
			idPath:     "/repositories/139727567/pulls",
			page: func(number int) any {
				return []map[string]any{pullFixture(number, "main", "open", false)}
			},
			walk: func(c *Client) (int, error) {
				requests, err := c.ListOpen(context.Background(), domain.PullRequestFilter{})
				return len(requests), err
			},
		},
		{
			name:       "changed files",
			collection: "/repos/acme/cluster/pulls/42/files",
			idPath:     "/repositories/139727567/pulls/42/files",
			page: func(number int) any {
				return []map[string]any{{"filename": fmt.Sprintf("clusters/prod/app%d.yaml", number)}}
			},
			walk: func(c *Client) (int, error) {
				files, err := c.ChangedFiles(context.Background(), 42)
				return len(files), err
			},
		},
	}
}

// pagingServer serves one item per page, answering the page it was asked for
// with the link that page returns, or none when link returns "". A link built
// from the origin the request arrived on is same-origin by construction. Every
// path is served, so a request that escaped the pinned collection is recorded
// rather than refused by the fixture.
func pagingServer(t *testing.T, caller pagingCaller, link func(origin string, number int) string) (*Client, func() []recordedRequest) {
	t.Helper()
	handler, recorded := recording(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		number, err := strconv.Atoi(cmp.Or(r.URL.Query().Get("page"), "1"))
		if err != nil {
			t.Errorf("page %q is not a number", r.URL.Query().Get("page"))
			http.Error(w, "unexpected page", http.StatusBadRequest)
			return
		}
		if next := link("http://"+r.Host, number); next != "" {
			w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next"`, next))
		}
		writeJSON(w, http.StatusOK, caller.page(number))
	})
	client := newTestClient(t, handler, "main")
	return client, recorded
}

// firstPageOnly serves link on the first page and ends the walk after the second.
func firstPageOnly(link func(origin string) string) func(string, int) string {
	return func(origin string, number int) string {
		if number != 1 {
			return ""
		}
		return link(origin)
	}
}

func requestPaths(requests []recordedRequest) []string {
	paths := make([]string, len(requests))
	for i := range requests {
		paths[i] = requests[i].path
	}
	return paths
}

// GitHub answers a /repos/{owner}/{name} listing with a next link that names the
// repository by ID, and a link may name any path at all, so only the page cursor
// is taken from it and re-applied to the collection the caller pinned.
func TestANextLinkIsWalkedAsAPageCursorOnThePinnedCollectionWhateverPathItNames(t *testing.T) {
	for _, caller := range pagingCallers() {
		for _, shape := range []struct {
			name string
			link func(caller pagingCaller, origin string) string
		}{
			{
				name: "GitHub's absolute repository-ID form",
				link: func(caller pagingCaller, origin string) string {
					return origin + caller.idPath + "?per_page=30&page=2"
				},
			},
			{
				name: "a relative repository-ID form",
				link: func(caller pagingCaller, _ string) string {
					return caller.idPath + "?page=2"
				},
			},
			{
				name: "an absolute link naming the collection",
				link: func(caller pagingCaller, origin string) string {
					return origin + caller.collection + "?per_page=100&page=2"
				},
			},
			{
				name: "a relative link naming the collection",
				link: func(caller pagingCaller, _ string) string {
					return caller.collection + "?per_page=100&page=2"
				},
			},
			{
				name: "a query-only link",
				link: func(pagingCaller, string) string { return "?page=2" },
			},
		} {
			t.Run(caller.name+"/"+shape.name, func(t *testing.T) {
				client, recorded := pagingServer(t, caller, firstPageOnly(func(origin string) string {
					return shape.link(caller, origin)
				}))

				count, err := caller.walk(client)

				if err != nil {
					t.Fatalf("walk: %v", err)
				}
				if count != 2 {
					t.Fatalf("want both pages collected, got %d items", count)
				}
				requests := recorded()
				if len(requests) != 2 {
					t.Fatalf("want two requests, got %v", requestPaths(requests))
				}
				for _, request := range requests {
					if request.path != caller.collection {
						t.Fatalf("request went to %q, want the pinned %q", request.path, caller.collection)
					}
				}
				if page := requests[1].query.Get("page"); page != "2" {
					t.Fatalf("second request asked for page %q, want 2", page)
				}
			})
		}
	}
}

// Origin checks alone would still let a link pivot into another repository's
// collection, which is a listing the caller never asked for and would read as
// its own.
func TestALinkNamingAnotherRepositoryNeverProducesARequestToThatRepository(t *testing.T) {
	const pivot = "/repos/attacker/evil/releases"
	for _, caller := range pagingCallers() {
		t.Run(caller.name, func(t *testing.T) {
			client, recorded := pagingServer(t, caller, firstPageOnly(func(origin string) string {
				return origin + pivot + "?page=2"
			}))

			count, err := caller.walk(client)

			if err != nil {
				t.Fatalf("walk: %v", err)
			}
			if count != 2 {
				t.Fatalf("want both pages collected, got %d items", count)
			}
			requests := recorded()
			for _, request := range requests {
				if request.path == pivot {
					t.Fatalf("the walk requested the pivot path %q", pivot)
				}
				if request.path != caller.collection {
					t.Fatalf("request went to %q, want the pinned %q", request.path, caller.collection)
				}
			}
		})
	}
}

// A link that cannot be reduced to a page of the pinned collection stops the
// walk with an error. Ending it quietly instead would hand the caller a prefix
// of the listing that is indistinguishable from the whole of it.
func TestAnUnusableNextLinkStopsTheWalkWithAnErrorRatherThanAShortListing(t *testing.T) {
	for _, caller := range pagingCallers() {
		for _, shape := range []struct {
			name string
			link string
			want string
		}{
			{
				name: "another origin",
				link: "https://evil.example/repos/acme/upstream/releases?page=2",
				want: "cross-origin",
			},
			{
				name: "another host without a scheme",
				link: "//evil.example/repos/acme/upstream/releases?page=2",
				want: "cross-origin",
			},
			{
				name: "unparsable",
				link: "http://example.com:notaport/releases?page=2",
				want: "invalid GitHub pagination link",
			},
			{
				name: "no page cursor",
				link: "/repos/acme/upstream/releases?per_page=100",
				want: "page number",
			},
			{
				name: "a page cursor that is not a positive number",
				link: "/repos/acme/upstream/releases?page=%20or%201",
				want: "page number",
			},
		} {
			t.Run(caller.name+"/"+shape.name, func(t *testing.T) {
				client, recorded := pagingServer(t, caller, firstPageOnly(func(string) string { return shape.link }))

				_, err := caller.walk(client)

				if err == nil {
					t.Fatal("want the walk refused")
				}
				if !strings.Contains(err.Error(), shape.want) {
					t.Fatalf("error %q does not mention %q", err, shape.want)
				}
				if requests := recorded(); len(requests) != 1 {
					t.Fatalf("want the walk stopped after the first page, got %v", requestPaths(requests))
				}
			})
		}
	}
}

// The link's own query is never adopted, so a link that widens the page size or
// rewrites the caller's narrowing cannot change what the next page holds.
func TestTheCallersPinnedQueryIsCarriedToTheLinkedPageRatherThanTheLinksOwn(t *testing.T) {
	handler, recorded := recording(t, func(_ int, w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("page") == "" {
			w.Header().Set("Link", fmt.Sprintf(
				`<http://%s/repositories/139727567/pulls?state=closed&per_page=1&page=2>; rel="next"`, r.Host))
		}
		writeJSON(w, http.StatusOK, []map[string]any{pullFixture(1, "main", "open", false)})
	})
	client := newTestClient(t, handler, "main")

	if _, err := client.ListOpen(context.Background(), domain.PullRequestFilter{}); err != nil {
		t.Fatalf("ListOpen: %v", err)
	}

	requests := recorded()
	if len(requests) != 2 {
		t.Fatalf("want two requests, got %v", requestPaths(requests))
	}
	next := requests[1]
	if next.path != "/repos/acme/cluster/pulls" {
		t.Fatalf("second request went to %q", next.path)
	}
	for parameter, want := range map[string]string{"state": "open", "per_page": "100", "page": "2"} {
		if got := next.query.Get(parameter); got != want {
			t.Fatalf("second request sent %s=%q, want %q", parameter, got, want)
		}
	}
}

// A page the walk never asked for is a page whose entries are absent from the
// listing without anything saying so: a release skipped this way reads as a
// release that was never published. Only the page after the one just read is
// accepted, which also refuses a link back to a page already served.
func TestAPageNumberOtherThanTheNextOneIsRefusedRatherThanSkippedOrRepeated(t *testing.T) {
	for _, caller := range pagingCallers() {
		for _, shape := range []struct {
			name     string
			link     func(caller pagingCaller, origin string, number int) string
			requests int
		}{
			{
				name: "a link that skips over pages",
				link: func(caller pagingCaller, origin string, number int) string {
					if number != 1 {
						return ""
					}
					return origin + caller.idPath + "?page=5"
				},
				requests: 1,
			},
			{
				name: "a link back to the page just served",
				link: func(caller pagingCaller, origin string, number int) string {
					if number != 1 {
						return ""
					}
					return origin + caller.idPath + "?page=1"
				},
				requests: 1,
			},
			{
				name: "a link back to an earlier page",
				link: func(caller pagingCaller, origin string, number int) string {
					if number > 2 {
						return ""
					}
					return fmt.Sprintf("%s%s?page=%d", origin, caller.idPath, 3-number)
				},
				requests: 2,
			},
		} {
			t.Run(caller.name+"/"+shape.name, func(t *testing.T) {
				client, recorded := pagingServer(t, caller, func(origin string, number int) string {
					return shape.link(caller, origin, number)
				})

				_, err := caller.walk(client)

				if err == nil {
					t.Fatal("want the walk refused")
				}
				if !strings.Contains(err.Error(), "does not advance") {
					t.Fatalf("error %q does not name the page the link failed to advance to", err)
				}
				if requests := recorded(); len(requests) != shape.requests {
					t.Fatalf("want %d requests, got %v", shape.requests, requestPaths(requests))
				}
			})
		}
	}
}

// The counterpart of the refusal above: a server walking its pages in order is
// followed to the end of them.
func TestAWalkFollowsEveryPageInOrderToTheEndOfTheListing(t *testing.T) {
	const pages = 4
	for _, caller := range pagingCallers() {
		t.Run(caller.name, func(t *testing.T) {
			client, recorded := pagingServer(t, caller, func(origin string, number int) string {
				if number >= pages {
					return ""
				}
				return fmt.Sprintf("%s%s?per_page=30&page=%d", origin, caller.idPath, number+1)
			})

			count, err := caller.walk(client)

			if err != nil {
				t.Fatalf("walk: %v", err)
			}
			if count != pages {
				t.Fatalf("want %d items, one per page, got %d", pages, count)
			}
			requests := recorded()
			if len(requests) != pages {
				t.Fatalf("want %d requests, got %v", pages, requestPaths(requests))
			}
			for i, request := range requests {
				if request.path != caller.collection {
					t.Fatalf("request went to %q, want the pinned %q", request.path, caller.collection)
				}
				if want := strconv.Itoa(i + 1); i > 0 && request.query.Get("page") != want {
					t.Fatalf("request %d asked for page %q, want %s", i+1, request.query.Get("page"), want)
				}
			}
		})
	}
}

// A repository plan that hides an endpoint answers 403 with a message of its
// own. No walk may turn that into an empty collection: absent entries read as
// entries that do not exist, and the run would decide against a listing it
// never saw.
func TestARefusedWalkIsReportedWithGitHubsOwnMessageRatherThanAsAnEmptyCollection(t *testing.T) {
	const says = "Upgrade to GitHub Pro or make this repository public to enable this feature."
	for _, caller := range pagingCallers() {
		t.Run(caller.name, func(t *testing.T) {
			handler, _ := recording(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				writeAPIError(w, http.StatusForbidden, says)
			})
			client := newTestClient(t, handler, "main")

			count, err := caller.walk(client)
			if err == nil {
				t.Fatalf("a refused walk returned %d items and no error", count)
			}
			if !strings.Contains(err.Error(), says) {
				t.Errorf("error %q does not carry GitHub's own message", err)
			}
			if count != 0 {
				t.Errorf("a refused walk collected %d items", count)
			}
		})
	}
}
