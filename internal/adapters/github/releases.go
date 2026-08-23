package github

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"
)

type Release struct {
	TagName     string    `json:"tag_name"`
	Name        string    `json:"name"`
	Body        string    `json:"body"`
	HTMLURL     string    `json:"html_url"`
	Draft       bool      `json:"draft"`
	PublishedAt time.Time `json:"published_at"`
}

const (
	maxReleasePages = 10
	releasesPerPage = 100
)

// ReleaseHistory is what a repository's release listing yielded. Truncated
// marks a history longer than the page budget, whose oldest releases were never
// read: the listing is newest first, so the releases present are the newest
// ones and nothing can be concluded from the absence of an older one.
type ReleaseHistory struct {
	Releases  []Release
	Truncated bool
}

// Releases lists the published releases of any repository, not only the one
// this client is scoped to, so upstream changelogs can be read. A history
// longer than the page budget comes back as its newest releases with Truncated
// set rather than as a short list indistinguishable from a history that ends
// there: a caller that needs a whole version range has to establish from the
// releases it did get that nothing paged away could belong to that range.
func (c *Client) Releases(ctx context.Context, repository string) (ReleaseHistory, error) {
	owner, name, found := strings.Cut(repository, "/")
	if !found || owner == "" || name == "" {
		return ReleaseHistory{}, fmt.Errorf("repository %q must be owner/name", repository)
	}
	collection := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name) + "/releases"
	var history ReleaseHistory
	pages := 0
	path := fmt.Sprintf("%s?per_page=%d", collection, releasesPerPage)
	err := paginate(ctx, c, path, collection, func(page []Release) error {
		pages++
		// One page past the budget is only fetched to tell a history that ends at
		// the budget from one that continues; its contents are never collected.
		if pages > maxReleasePages {
			history.Truncated = len(page) > 0
			return errStopPagination
		}
		for _, release := range page {
			if release.Draft {
				continue
			}
			history.Releases = append(history.Releases, release)
		}
		return nil
	})
	if err != nil {
		return ReleaseHistory{}, err
	}
	return history, nil
}

// Issues searches a repository's open issues for a query string. It is used to
// surface regression reports about the exact version being taken.
func (c *Client) Issues(ctx context.Context, repository, query string) ([]string, error) {
	owner, name, found := strings.Cut(repository, "/")
	if !found || owner == "" || name == "" {
		return nil, fmt.Errorf("repository %q must be owner/name", repository)
	}
	search := fmt.Sprintf("repo:%s/%s is:issue is:open %s", owner, name, query)
	var body struct {
		Items []struct {
			Title   string `json:"title"`
			HTMLURL string `json:"html_url"`
		} `json:"items"`
	}
	path := "/search/issues?per_page=20&q=" + url.QueryEscape(search)
	if err := c.getJSON(ctx, path, &body); err != nil {
		return nil, err
	}
	titles := make([]string, 0, len(body.Items))
	for _, item := range body.Items {
		titles = append(titles, item.Title+" ("+item.HTMLURL+")")
	}
	return titles, nil
}

// SearchRepositories finds candidate upstream repositories by name, so the
// agent can locate a changelog that nothing links to.
func (c *Client) SearchRepositories(ctx context.Context, query string) ([]string, error) {
	var body struct {
		Items []struct {
			FullName    string `json:"full_name"`
			Description string `json:"description"`
			Stars       int    `json:"stargazers_count"`
		} `json:"items"`
	}
	path := "/search/repositories?per_page=10&q=" + url.QueryEscape(query)
	if err := c.getJSON(ctx, path, &body); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(body.Items))
	for _, item := range body.Items {
		names = append(names, fmt.Sprintf("%s (%d stars) %s", item.FullName, item.Stars, item.Description))
	}
	return names, nil
}
