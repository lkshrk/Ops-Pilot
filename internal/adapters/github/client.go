// Package github is the deliberately small GitHub boundary used by the workflow.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/lkshrk/ops-pilot/internal/domain"
	"github.com/lkshrk/ops-pilot/internal/retry"
)

const (
	// A per_page=100 listing of pull requests carries full head/base repository objects per item,
	// which routinely exceeds a megabyte on busy repositories.
	maxResponseBytes = 16 << 20
	maxPages         = 100
	maxObjects       = 10000
)

// TransportError exposes safe HTTP diagnostics. It deliberately omits request
// headers and response bodies because either may contain credentials.
type TransportError struct {
	Method, Path, RequestID string
	Message                 string
	Status                  int
	Err                     error
	retryAfter              time.Duration
}

func (e *TransportError) RetryAfterDelay() time.Duration { return e.retryAfter }

func (e *TransportError) Error() string {
	if e.Status != 0 {
		// The API's own message is the difference between "status 405" and
		// "Pull Request is not mergeable"; without it the operator has nothing.
		if e.Message != "" {
			return fmt.Sprintf("GitHub %s %s: %s (status %d, request %s)",
				e.Method, e.Path, e.Message, e.Status, e.RequestID)
		}
		return fmt.Sprintf("GitHub %s %s: status %d (request %s)", e.Method, e.Path, e.Status, e.RequestID)
	}
	return fmt.Sprintf("GitHub %s %s transport failure: %v", e.Method, e.Path, e.Err)
}
func (e *TransportError) Unwrap() error { return e.Err }

type Client struct {
	http       *http.Client
	token      string
	base       *url.URL
	repository domain.RepositoryRef
	retry      retry.Schedule
	after      func(time.Duration) <-chan time.Time
}

// wait is the mergeability backoff's clock; the read path's is c.retry. Between
// them a test can account for every delay an unattended run spends without
// spending it, so a new wait must go through one of the two.
func (c *Client) wait(d time.Duration) <-chan time.Time {
	if c.after != nil {
		return c.after(d)
	}
	return time.After(d)
}

func New(httpClient *http.Client, token, baseURL string, repository domain.RepositoryRef) (*Client, error) {
	if httpClient == nil || token == "" || repository.Owner == "" || repository.Name == "" {
		return nil, fmt.Errorf("http client, token, repository owner, and name are required")
	}
	u, err := url.Parse(baseURL)
	if err != nil || !u.IsAbs() || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("base URL must be absolute HTTP(S) without userinfo, query, or fragment")
	}
	clone := *httpClient
	priorRedirect := clone.CheckRedirect
	clone.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || !sameOrigin(u, req.URL) {
			return http.ErrUseLastResponse
		}
		if priorRedirect != nil {
			return priorRedirect(req, via)
		}
		return nil
	}
	return &Client{http: &clone, token: token, base: u, repository: repository}, nil
}

func sameOrigin(base, other *url.URL) bool {
	return other != nil && base.Scheme == other.Scheme && base.Host == other.Host
}

func (c *Client) endpoint(p string) (string, error) {
	rel, err := url.Parse(p)
	if err != nil {
		return "", fmt.Errorf("GitHub request path is not a URL reference: %w", err)
	}
	// Refusing rather than resolving keeps the request in the caller's repo;
	// re-decoding covers a proxy that unescapes the path more than once.
	for probe := rel.Path; ; {
		for segment := range strings.SplitSeq(probe, "/") {
			if segment == "." || segment == ".." {
				return "", fmt.Errorf("GitHub request path carries a %q segment", segment)
			}
		}
		next, err := url.PathUnescape(probe)
		if err != nil || next == probe {
			break
		}
		probe = next
	}
	u := *c.base
	// rel.RawPath is deliberately dropped, so per-segment escaping does not reach the wire.
	u.Path = strings.TrimSuffix(u.Path, "/") + rel.Path
	u.RawQuery = rel.RawQuery
	return u.String(), nil
}
func (c *Client) request(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var raw []byte
	var err error
	if body != nil {
		raw, err = json.Marshal(body)
		if err != nil {
			return nil, err
		}
	}
	if method != http.MethodGet && method != http.MethodHead {
		return c.requestAttempt(ctx, method, path, raw, body)
	}
	result, err := retry.Do(ctx, c.retry, func(err error) bool {
		var transport *TransportError
		return errors.As(err, &transport) && (retry.TransientTransport(transport.Err) || retry.RetryStatus(transport.Status) || transport.retryAfter > 0)
	}, func() (*http.Response, error) {
		return c.requestAttempt(ctx, method, path, raw, body)
	})
	if err != nil {
		var transport *TransportError
		if errors.As(err, &transport) {
			return nil, err
		}
		return nil, &TransportError{Method: method, Path: path, Err: err}
	}
	return result, nil
}

func (c *Client) requestAttempt(ctx context.Context, method, path string, raw []byte, body any) (*http.Response, error) {
	target, err := c.endpoint(path)
	if err != nil {
		return nil, &TransportError{Method: method, Path: path, Err: err}
	}
	r, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(raw))
	if err != nil {
		return nil, &TransportError{Method: method, Path: path, Err: err}
	}
	r.Header.Set("Authorization", "Bearer "+c.token)
	r.Header.Set("Accept", "application/vnd.github+json")
	if body != nil {
		r.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(r)
	if err != nil {
		return nil, &TransportError{Method: method, Path: path, Err: err}
	}
	if res.StatusCode >= 200 && res.StatusCode <= 299 {
		raw, readErr := readJSONResponse(res.Body)
		res.Body.Close()
		if readErr != nil {
			return nil, &TransportError{Method: method, Path: path, RequestID: res.Header.Get("X-GitHub-Request-Id"), Err: readErr}
		}
		res.Body = io.NopCloser(bytes.NewReader(raw))
		return res, nil
	}
	requestID := res.Header.Get("X-GitHub-Request-Id")
	status := res.StatusCode
	retryAfter, _ := retry.RetryAfterHeader(res.Header, status)
	message := readErrorMessage(res.Body)
	res.Body.Close()
	return nil, &TransportError{
		Method: method, Path: path, Status: status, RequestID: requestID,
		Message: message, retryAfter: retryAfter,
	}
}

// readErrorMessage returns the API's own message, which is the difference
// between "status 405" and "Pull Request is not mergeable".
func readErrorMessage(body io.Reader) string {
	raw, err := io.ReadAll(io.LimitReader(body, maxResponseBytes+1))
	if err != nil || len(raw) > maxResponseBytes {
		return ""
	}
	var response struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &response) != nil {
		return ""
	}
	return response.Message
}

func readJSONResponse(body io.Reader) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(body, maxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > maxResponseBytes {
		return nil, fmt.Errorf("GitHub response exceeds %d bytes", maxResponseBytes)
	}
	// A 2xx carrying no payload is a completed write; a caller that needs one
	// fails at decode instead, so this must not be turned back into an error.
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("GitHub response is not a single JSON value")
	}
	return raw, nil
}
func (c *Client) defaultBranch(ctx context.Context) (string, error) {
	res, err := c.request(ctx, http.MethodGet, "/repos/"+c.repository.Owner+"/"+c.repository.Name, nil)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	var v struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := json.NewDecoder(res.Body).Decode(&v); err != nil {
		return "", err
	}
	if v.DefaultBranch == "" {
		return "", fmt.Errorf("GitHub response omitted default branch")
	}
	return v.DefaultBranch, nil
}
func (c *Client) effectiveBranch(ctx context.Context) (string, error) {
	if c.repository.Branch != "" {
		return c.repository.Branch, nil
	}
	return c.defaultBranch(ctx)
}
func (c *Client) scoped(r domain.RepositoryRef) bool {
	return r.Owner == c.repository.Owner && r.Name == c.repository.Name
}

func nextLink(link string) string {
	for _, part := range strings.Split(link, ",") {
		if !strings.Contains(part, `rel="next"`) {
			continue
		}
		a, b := strings.Index(part, "<"), strings.Index(part, ">")
		if a >= 0 && b > a {
			return part[a+1 : b]
		}
	}
	return ""
}
