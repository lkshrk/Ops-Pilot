package github

import (
	"cmp"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/lkshrk/ops-pilot/internal/domain"
)

type pull struct {
	Number  int    `json:"number"`
	State   string `json:"state"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	User    struct{ Login string }
	Labels  []struct{ Name string }
	Head    struct {
		SHA  string
		Ref  string
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	}
	Base           struct{ SHA, Ref string }
	CreatedAt      time.Time `json:"created_at"`
	Draft          bool      `json:"draft"`
	Merged         bool      `json:"merged"`
	MergeCommitSHA string    `json:"merge_commit_sha"`
}

func (c *Client) pullRequest(p pull) domain.PullRequest {
	labels := make([]string, len(p.Labels))
	for i := range p.Labels {
		labels[i] = p.Labels[i].Name
	}
	headRepository := ""
	if p.Head.Repo != nil {
		headRepository = p.Head.Repo.FullName
	}
	return domain.PullRequest{
		Number:         p.Number,
		Title:          p.Title,
		Body:           p.Body,
		Author:         p.User.Login,
		Labels:         labels,
		URL:            p.HTMLURL,
		HeadSHA:        p.Head.SHA,
		HeadRef:        p.Head.Ref,
		HeadRepository: headRepository,
		BaseSHA:        p.Base.SHA,
		BaseRef:        p.Base.Ref,
		Draft:          p.Draft,
		CreatedAt:      p.CreatedAt,
	}
}

func (c *Client) repositoryPath(suffix string) string {
	return "/repos/" + c.repository.Owner + "/" + c.repository.Name + suffix
}

// ListOpen returns the open pull requests targeting the effective base branch,
// oldest first, narrowed by filter. A filter asking for the other bases returns
// the complement: everything open that is aimed somewhere else.
func (c *Client) ListOpen(ctx context.Context, filter domain.PullRequestFilter) ([]domain.PullRequest, error) {
	branch, err := c.effectiveBranch(ctx)
	if err != nil {
		return nil, err
	}
	collection := c.repositoryPath("/pulls")
	var result []domain.PullRequest
	err = paginate(ctx, c, collection+"?state=open&per_page=100", collection, func(page []pull) error {
		for _, item := range page {
			if item.State != "" && item.State != "open" || item.Base.Ref == "" {
				continue
			}
			if onBranch := item.Base.Ref == branch; onBranch == filter.OtherBases {
				continue
			}
			request := c.pullRequest(item)
			if !matchesFilter(request, filter) {
				continue
			}
			result = append(result, request)
			if len(result) > maxObjects {
				return fmt.Errorf("GitHub pull request result exceeds object limit")
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(result, func(i, j int) bool { return result[i].CreatedAt.Before(result[j].CreatedAt) })
	return result, nil
}

func matchesFilter(request domain.PullRequest, filter domain.PullRequestFilter) bool {
	if len(filter.Authors) > 0 && !slices.Contains(filter.Authors, request.Author) {
		return false
	}
	if len(filter.Labels) == 0 {
		return true
	}
	for _, label := range request.Labels {
		if slices.Contains(filter.Labels, label) {
			return true
		}
	}
	return false
}

// Get returns a single open pull request that targets the effective base
// branch. Anything else is refused: the workflow merges into, verifies, and
// reverts on that one branch, so acting on a pull request aimed elsewhere would
// merge and revert different branches.
func (c *Client) Get(ctx context.Context, number int) (domain.PullRequest, error) {
	if number <= 0 {
		return domain.PullRequest{}, fmt.Errorf("pull request number is required")
	}
	branch, err := c.effectiveBranch(ctx)
	if err != nil {
		return domain.PullRequest{}, err
	}
	var item pull
	if err := c.getJSON(ctx, c.repositoryPath(fmt.Sprintf("/pulls/%d", number)), &item); err != nil {
		return domain.PullRequest{}, err
	}
	if item.Number != number || item.Head.SHA == "" || item.Base.Ref == "" {
		return domain.PullRequest{}, fmt.Errorf("partial GitHub pull request response")
	}
	if item.Base.Ref != branch {
		return domain.PullRequest{}, fmt.Errorf(
			"pull request #%d targets branch %q, but ops-pilot is configured for branch %q",
			number, item.Base.Ref, branch,
		)
	}
	if item.Merged {
		return domain.PullRequest{}, fmt.Errorf("pull request #%d is already merged", number)
	}
	if item.State != "" && item.State != "open" {
		return domain.PullRequest{}, fmt.Errorf("pull request #%d is %s, not open", number, item.State)
	}
	return c.pullRequest(item), nil
}

// MergeState reports whether the pull request merged and which commit it produced.
func (c *Client) MergeState(ctx context.Context, number int) (domain.MergeState, error) {
	if number <= 0 {
		return domain.MergeState{}, fmt.Errorf("pull request number is required")
	}
	var item pull
	if err := c.getJSON(ctx, c.repositoryPath(fmt.Sprintf("/pulls/%d", number)), &item); err != nil {
		return domain.MergeState{}, err
	}
	if item.Number != number {
		return domain.MergeState{}, fmt.Errorf("partial GitHub pull request response")
	}
	if !item.Merged {
		return domain.MergeState{}, nil
	}
	if item.MergeCommitSHA == "" {
		return domain.MergeState{}, fmt.Errorf("pull request #%d is merged with no merge commit", number)
	}
	return domain.MergeState{Merged: true, SHA: item.MergeCommitSHA}, nil
}

// ChangedFiles returns the paths the pull request touches, carrying the former
// path of a rename so a revert can restore both ends of it.
func (c *Client) ChangedFiles(ctx context.Context, number int) ([]domain.FileDelta, error) {
	collection := c.repositoryPath(fmt.Sprintf("/pulls/%d/files", number))
	var files []domain.FileDelta
	err := paginate(ctx, c, collection+"?per_page=100", collection, func(page []struct {
		Filename         string `json:"filename"`
		PreviousFilename string `json:"previous_filename"`
		Status           string `json:"status"`
	}) error {
		for _, file := range page {
			files = append(files, domain.FileDelta{
				Path:         file.Filename,
				PreviousPath: file.PreviousFilename,
				Status:       fileStatus(file.Status),
			})
			if len(files) > maxObjects {
				return fmt.Errorf("GitHub file result exceeds object limit")
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}

func fileStatus(value string) domain.FileDeltaStatus {
	switch value {
	case "added", "copied":
		return domain.FileAdded
	case "removed":
		return domain.FileDeleted
	case "renamed":
		return domain.FileRenamed
	default:
		return domain.FileModified
	}
}

// mergeabilityAttempts bounds the wait for GitHub to compute mergeability.
const (
	mergeabilityAttempts = 6
	mergeabilityBackoff  = 2 * time.Second
)

// Merge merges the pull request at the exact head it was assessed at and
// returns the resulting commit SHA on the base branch.
//
// GitHub computes mergeability asynchronously and answers 405 while it is still
// working, so a freshly pushed or rebased pull request is briefly unmergeable
// through no fault of its own. That case is waited out rather than failed.
func (c *Client) Merge(ctx context.Context, number int, headSHA, method string) (string, error) {
	var err error
	for attempt := 0; attempt < mergeabilityAttempts; attempt++ {
		var sha string
		sha, err = c.merge(ctx, number, headSHA, method)
		if err == nil || !mergeabilityPending(err) {
			return sha, err
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-c.wait(mergeabilityBackoff):
		}
	}
	return "", err
}

// ErrHeadModified is GitHub refusing to merge because the pull request's head
// is no longer the SHA the request named. Waiting cannot help: the request
// carries the head that was assessed and the branch has moved past it, so every
// retry sends the same stale SHA. It is never treated as pending.
var ErrHeadModified = errors.New("the pull request head moved after it was assessed")

func headModified(err error) error {
	var transport *TransportError
	if !errors.As(err, &transport) || transport.Status != http.StatusConflict {
		return err
	}
	if !strings.Contains(strings.ToLower(transport.Message), "head branch was modified") {
		return err
	}
	return fmt.Errorf("%w: %w", ErrHeadModified, err)
}

// mergeabilityPending reports GitHub still deciding, as opposed to a genuine
// conflict or a branch-protection refusal, both of which also answer 405.
func mergeabilityPending(err error) bool {
	var transport *TransportError
	if !errors.As(err, &transport) || transport.Status != http.StatusMethodNotAllowed {
		return false
	}
	message := strings.ToLower(transport.Message)
	return strings.Contains(message, "base branch was modified") ||
		strings.Contains(message, "not mergeable")
}

func (c *Client) merge(ctx context.Context, number int, headSHA, method string) (string, error) {
	if number <= 0 || headSHA == "" {
		return "", fmt.Errorf("pull request number and head SHA are required")
	}
	if !slices.Contains([]string{"merge", "squash", "rebase"}, method) {
		return "", fmt.Errorf("invalid merge method %q", method)
	}
	res, err := c.request(
		ctx,
		http.MethodPut,
		c.repositoryPath(fmt.Sprintf("/pulls/%d/merge", number)),
		map[string]string{"sha": headSHA, "merge_method": method},
	)
	if err != nil {
		return "", headModified(err)
	}
	defer func() { _ = res.Body.Close() }()
	var body struct {
		SHA     string `json:"sha"`
		Merged  bool   `json:"merged"`
		Message string `json:"message"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return "", err
	}
	if !body.Merged || body.SHA == "" {
		return "", fmt.Errorf("merge not applied: %s", body.Message)
	}
	return body.SHA, nil
}

func (c *Client) Comment(ctx context.Context, number int, body string) error {
	if number <= 0 || strings.TrimSpace(body) == "" {
		return fmt.Errorf("pull request number and comment body are required")
	}
	res, err := c.request(
		ctx,
		http.MethodPost,
		c.repositoryPath(fmt.Sprintf("/issues/%d/comments", number)),
		map[string]string{"body": body},
	)
	if err != nil {
		return err
	}
	return res.Body.Close()
}

func (c *Client) AddLabel(ctx context.Context, number int, labels ...string) error {
	if number <= 0 || len(labels) == 0 {
		return fmt.Errorf("pull request number and at least one label are required")
	}
	res, err := c.request(
		ctx,
		http.MethodPost,
		c.repositoryPath(fmt.Sprintf("/issues/%d/labels", number)),
		map[string][]string{"labels": labels},
	)
	if err != nil {
		return err
	}
	return res.Body.Close()
}

func (c *Client) Close(ctx context.Context, number int) error {
	if number <= 0 {
		return fmt.Errorf("pull request number is required")
	}
	res, err := c.request(
		ctx,
		http.MethodPatch,
		c.repositoryPath(fmt.Sprintf("/pulls/%d", number)),
		map[string]string{"state": "closed"},
	)
	if err != nil {
		return err
	}
	return res.Body.Close()
}

// BranchHead returns the current commit SHA of a branch.
func (c *Client) BranchHead(ctx context.Context, branch string) (string, error) {
	if branch == "" {
		return "", fmt.Errorf("branch is required")
	}
	var body struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := c.getJSON(ctx, c.repositoryPath("/git/ref/heads/"+url.PathEscape(branch)), &body); err != nil {
		return "", err
	}
	if body.Object.SHA == "" {
		return "", fmt.Errorf("GitHub response omitted the branch head")
	}
	return body.Object.SHA, nil
}

// Branch is the effective base branch every mutation targets.
func (c *Client) Branch(ctx context.Context) (string, error) { return c.effectiveBranch(ctx) }

func (c *Client) getJSON(ctx context.Context, path string, out any) error {
	res, err := c.request(ctx, http.MethodGet, path, nil)
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	return json.NewDecoder(res.Body).Decode(out)
}

// paginationPathFor reads a server-supplied Link for its page cursor alone and
// re-applies that to the collection and query the caller pinned in current. No
// path the server names is ever requested: GitHub answers a /repos/{owner}/{name}
// listing with the equivalent /repositories/{id} path, and following any path it
// names would let a Link pivot into another repository. The cursor has to be the
// page after the one just read, so no page can be stepped over. An empty return
// ends the walk; anything else unusable ends it with an error, because a walk cut
// short quietly is a listing whose absent entries read as entries that do not exist.
func (c *Client) paginationPathFor(link, current, collection string) (string, error) {
	next := nextLink(link)
	if next == "" {
		return "", nil
	}
	parsed, err := url.Parse(next)
	if err != nil {
		return "", fmt.Errorf("invalid GitHub pagination link")
	}
	if (parsed.IsAbs() || parsed.Host != "") && !sameOrigin(c.base, parsed) {
		return "", fmt.Errorf("cross-origin GitHub pagination link")
	}
	linked, err := url.ParseQuery(parsed.RawQuery)
	if err != nil {
		return "", fmt.Errorf("invalid GitHub pagination link")
	}
	page, err := strconv.Atoi(linked.Get("page"))
	if err != nil || page < 1 {
		return "", fmt.Errorf("GitHub pagination link carries no page number")
	}
	pinned, err := url.Parse(current)
	if err != nil {
		return "", fmt.Errorf("invalid GitHub collection path")
	}
	query, err := url.ParseQuery(pinned.RawQuery)
	if err != nil {
		return "", fmt.Errorf("invalid GitHub collection path")
	}
	previous, err := strconv.Atoi(cmp.Or(query.Get("page"), "1"))
	if err != nil || previous < 1 {
		return "", fmt.Errorf("invalid GitHub collection path")
	}
	if page != previous+1 {
		return "", fmt.Errorf(
			"GitHub pagination link does not advance from page %d to page %d", previous, page)
	}
	query.Set("page", strconv.Itoa(page))
	return collection + "?" + query.Encode(), nil
}

// FileAt reads a repository file at a ref. A file that does not exist there is
// reported as absent rather than as an error, because a revert has to delete
// paths the pull request created.
func (c *Client) FileAt(ctx context.Context, path, ref string) ([]byte, bool, error) {
	if path == "" || ref == "" {
		return nil, false, fmt.Errorf("path and ref are required")
	}
	endpoint := c.repositoryPath("/contents/" + escapePath(path) + "?ref=" + url.QueryEscape(ref))
	res, err := c.request(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		var transport *TransportError
		if errors.As(err, &transport) && transport.Status == http.StatusNotFound {
			return nil, false, nil
		}
		return nil, false, err
	}
	defer func() { _ = res.Body.Close() }()
	var body struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
		Type     string `json:"type"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return nil, false, err
	}
	if body.Type != "file" {
		return nil, false, fmt.Errorf("%q is not a file at %s", path, ref)
	}
	if body.Encoding != "base64" {
		return nil, false, fmt.Errorf("unexpected content encoding %q", body.Encoding)
	}
	decoded, err := base64.StdEncoding.DecodeString(body.Content)
	if err != nil {
		return nil, false, fmt.Errorf("decode %q: %w", path, err)
	}
	return decoded, true, nil
}

func escapePath(path string) string {
	segments := strings.Split(path, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	return strings.Join(segments, "/")
}
