package github

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"testing"
)

// releasesHandler serves pages of the upstream release listing newest first,
// linking to a next page until the last one. The oldest page carries the
// breaking release, so a truncated read is the read that loses it.
func releasesHandler(t *testing.T, pages, perPage int, requests *int) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/upstream/releases" {
			t.Errorf("unexpected request path %q", r.URL.Path)
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		page, err := strconv.Atoi(cmp.Or(r.URL.Query().Get("page"), "1"))
		if err != nil || page < 1 {
			t.Errorf("unexpected page %q", r.URL.Query().Get("page"))
			http.Error(w, "unexpected page", http.StatusBadRequest)
			return
		}
		*requests++
		body := make([]map[string]any, 0, perPage)
		for item := range perPage {
			release := map[string]any{
				"tag_name": fmt.Sprintf("v1.%d.%d", pages-page, perPage-item),
				"body":     "fix",
			}
			if page == pages && item == perPage-1 {
				release["body"] = "BREAKING: database schema rewritten"
			}
			body = append(body, release)
		}
		if page < pages {
			w.Header().Set("Link", fmt.Sprintf(
				`</repos/acme/upstream/releases?per_page=100&page=%d>; rel="next"`, page+1))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})
}

// The newest releases of a history too long to read are worth having, but only
// to a caller that is told they are a prefix: a short list passed off as the
// whole history is one whose absent releases read as releases that were never
// published, and the breaking one this handler puts on the oldest page is
// exactly what that loses.
func TestAReleaseHistoryLongerThanThePageBudgetIsMarkedTruncatedRatherThanLookingWhole(t *testing.T) {
	requests := 0
	client := newTestClient(t, releasesHandler(t, maxReleasePages+4, 2, &requests), "main")

	history, err := client.Releases(context.Background(), "acme/upstream")

	if err != nil {
		t.Fatalf("Releases: %v", err)
	}
	if !history.Truncated {
		t.Fatal("a history longer than the page budget was reported as read whole")
	}
	if len(history.Releases) != maxReleasePages*2 {
		t.Fatalf("want the %d releases the budget covers, got %d", maxReleasePages*2, len(history.Releases))
	}
	for _, release := range history.Releases {
		if release.Body == "BREAKING: database schema rewritten" {
			t.Fatal("the oldest page was collected past the page budget")
		}
	}
	if requests != maxReleasePages+1 {
		t.Fatalf("want the walk to stop one page past the budget, got %d requests", requests)
	}
}

func TestAReleaseHistoryThatFitsThePageBudgetIsReturnedWhole(t *testing.T) {
	for _, test := range []struct {
		name  string
		pages int
	}{
		{name: "well inside the budget", pages: 2},
		{name: "exactly the budget", pages: maxReleasePages},
	} {
		t.Run(test.name, func(t *testing.T) {
			requests := 0
			client := newTestClient(t, releasesHandler(t, test.pages, 2, &requests), "main")

			history, err := client.Releases(context.Background(), "acme/upstream")

			if err != nil {
				t.Fatalf("Releases: %v", err)
			}
			if history.Truncated {
				t.Fatal("a history that fits the page budget was reported as truncated")
			}
			releases := history.Releases
			if len(releases) != test.pages*2 {
				t.Fatalf("want %d releases, got %d", test.pages*2, len(releases))
			}
			if requests != test.pages {
				t.Fatalf("want %d requests, got %d", test.pages, requests)
			}
			if releases[len(releases)-1].Body != "BREAKING: database schema rewritten" {
				t.Fatalf("the oldest release is missing its body: %+v", releases[len(releases)-1])
			}
		})
	}
}

// draftHeavyHandler serves pages of mixed published and draft entries, always
// linking to a next page, so the number of releases collected is far below the
// number of entries the budget was spent on.
func draftHeavyHandler(t *testing.T, published, drafts int) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, err := strconv.Atoi(cmp.Or(r.URL.Query().Get("page"), "1"))
		if err != nil {
			t.Errorf("unexpected page %q", r.URL.Query().Get("page"))
			http.Error(w, "unexpected page", http.StatusBadRequest)
			return
		}
		body := make([]map[string]any, 0, published+drafts)
		for item := range published {
			body = append(body, map[string]any{"tag_name": fmt.Sprintf("v%d.%d.0", page, item)})
		}
		for item := range drafts {
			body = append(body, map[string]any{"tag_name": fmt.Sprintf("d%d.%d.0", page, item), "draft": true})
		}
		w.Header().Set("Link", fmt.Sprintf(
			`</repos/acme/upstream/releases?per_page=100&page=%d>; rel="next"`, page+1))
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})
}

// The budget is spent on pages of entries, and a draft occupies an entry slot
// like any other, so a repository whose listing is mostly drafts is refused
// while holding fewer published releases than the budget nominally covers.
func TestDraftsSpendThePageBudgetTheyOccupy(t *testing.T) {
	client := newTestClient(t, draftHeavyHandler(t, 2, 8), "main")

	history, err := client.Releases(context.Background(), "acme/upstream")

	if err != nil {
		t.Fatalf("Releases: %v", err)
	}
	if !history.Truncated {
		t.Fatal("a listing mostly of drafts spent the budget and was still reported as read whole")
	}
	if len(history.Releases) != 2*maxReleasePages {
		t.Fatalf("want the %d published releases the budget reached, got %d",
			2*maxReleasePages, len(history.Releases))
	}
}

// A next link is the server's claim that more exists, not proof of it. Trusting
// the claim over the page it leads to would suppress the changelog of a
// dependency whose history does fit.
func TestANextLinkToAnEmptyPageIsNotATruncatedHistory(t *testing.T) {
	requests := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(cmp.Or(r.URL.Query().Get("page"), "1"))
		requests++
		w.Header().Set("Link", fmt.Sprintf(
			`</repos/acme/upstream/releases?per_page=100&page=%d>; rel="next"`, page+1))
		w.Header().Set("Content-Type", "application/json")
		if page > maxReleasePages {
			_ = json.NewEncoder(w).Encode([]map[string]any{})
			return
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{"tag_name": fmt.Sprintf("v1.%d.0", page)}})
	})
	client := newTestClient(t, handler, "main")

	history, err := client.Releases(context.Background(), "acme/upstream")

	if err != nil {
		t.Fatalf("Releases: %v", err)
	}
	if history.Truncated {
		t.Fatal("a next link to an empty page was read as a history that continues")
	}
	if len(history.Releases) != maxReleasePages {
		t.Fatalf("want %d releases, got %d", maxReleasePages, len(history.Releases))
	}
	if requests != maxReleasePages+1 {
		t.Fatalf("want the walk to stop one page past the budget, got %d requests", requests)
	}
}

func TestADraftReleaseIsNotPartOfTheHistory(t *testing.T) {
	requests := 0
	handler := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"tag_name": "v1.2.0", "draft": true},
			{"tag_name": "v1.1.0"},
		})
	})
	client := newTestClient(t, handler, "main")

	history, err := client.Releases(context.Background(), "acme/upstream")

	if err != nil {
		t.Fatalf("Releases: %v", err)
	}
	if len(history.Releases) != 1 || history.Releases[0].TagName != "v1.1.0" {
		t.Fatalf("want only the published release, got %+v", history.Releases)
	}
}
