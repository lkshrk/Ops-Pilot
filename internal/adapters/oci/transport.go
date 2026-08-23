package oci

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/lkshrk/ops-pilot/internal/retry"
)

var errBodyLimit = errors.New("OCI registry response exceeds body limit")

func requestTimeout(limit int64) time.Duration {
	if limit <= 0 {
		return registryResponseTimeout
	}
	return registryResponseTimeout + time.Duration(limit)*time.Second/minRegistryThroughput
}

// tokenHosts names the one other host each registry may collect a token from. The set is closed and
// ops-pilot's own: the operator, not the registry, decides where a credential travels, so a realm
// host is matched whole against this table and never by domain, suffix or configuration.
var tokenHosts = map[string]string{dockerHubAuthority: "auth.docker.io"}

func permittedRealmHost(authority, host, scheme string) bool {
	if host == "" {
		return false
	}
	host = canonicalRealmHost(host, scheme)
	if host == canonicalRealmHost(authority, scheme) {
		return true
	}
	mapped, ok := tokenHosts[authority]
	return ok && host == canonicalRealmHost(mapped, scheme)
}

// canonicalRealmHost folds only the two differences DNS treats as the same host: ASCII case and an
// explicit default port. It never folds Unicode, so a homograph stays a distinct, refused host, and
// it strips only the scheme's default port, so a non-default port stays a distinct destination.
func canonicalRealmHost(host, scheme string) string {
	if h, port, err := net.SplitHostPort(host); err == nil {
		if port == defaultPort(scheme) {
			return asciiLower(h)
		}
		return net.JoinHostPort(asciiLower(h), port)
	}
	return asciiLower(host)
}

func defaultPort(scheme string) string {
	if scheme == "http" {
		return "80"
	}
	return "443"
}

func asciiLower(s string) string {
	var b []byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			if b == nil {
				b = []byte(s)
			}
			b[i] = c + ('a' - 'A')
		}
	}
	if b == nil {
		return s
	}
	return string(b)
}

// pingV2 confirms the endpoint speaks the registry API. An unauthenticated ping may answer 401
// with a bearer challenge, which still proves v2 support; that challenge carries a placeholder
// scope naming no real repository, so it must never be exchanged for a token.
func (c *Client) pingV2(ctx context.Context, authority string) error {
	res, _, err := c.do(ctx, authority, http.MethodGet, "/v2/", "", 0, true)
	if err != nil {
		return err
	}
	res.Body.Close()
	return nil
}

func (c *Client) operation(ctx context.Context, authority, method, path, authorization string, limit int64) (*http.Response, []byte, error) {
	return c.do(ctx, authority, method, path, authorization, limit, false)
}

func (c *Client) do(ctx context.Context, authority, method, path, authorization string, limit int64, ping bool) (*http.Response, []byte, error) {
	base, err := c.baseURL(authority)
	if err != nil {
		return nil, nil, category(ErrTrustBoundary, "unsafe registry authority")
	}
	u := *base
	u.Path = path
	redirects, challenged := 0, false
	for {
		res, body, challenge, err := c.request(ctx, method, &u, authorization, limit)
		if err != nil {
			if cause := context.Cause(ctx); cause != nil {
				return nil, nil, cause
			}
			if cause := contextCause(err); cause != nil {
				return nil, nil, cause
			}
			if knownCategory(err) {
				return nil, nil, err
			}
			return nil, nil, unavailable(err, "registry request")
		}
		// RFC 7235 confines WWW-Authenticate to a 401; a challenge on any other status is not an auth demand.
		if challenge != "" && res.StatusCode == http.StatusUnauthorized {
			// A ping only proves the endpoint speaks the registry API; its challenge names a
			// placeholder repository, so no token is requested for it.
			if ping {
				return res, body, nil
			}
			if authorization != "" {
				res.Body.Close()
				// Being re-challenged after presenting a token is an ordinary refusal — a private
				// or absent repository — not a protocol violation, so it must not be fatal.
				return nil, nil, category(ErrAuth, "registry re-challenged an authenticated request")
			}
			if challenged {
				res.Body.Close()
				return nil, nil, category(ErrTrustBoundary, "multiple bearer challenges")
			}
			res.Body.Close()
			token, err := c.token(ctx, authority, challenge)
			if err != nil {
				if cause := context.Cause(ctx); cause != nil {
					return nil, nil, cause
				}
				if knownCategory(err) {
					return nil, nil, err
				}
				return nil, nil, err
			}
			authorization = "Bearer " + token
			challenged = true
			continue
		}
		if res.StatusCode >= 300 && res.StatusCode < 400 {
			if redirects >= 5 {
				res.Body.Close()
				return nil, nil, category(ErrTrustBoundary, "too many registry redirects")
			}
			location := res.Header.Get("Location")
			res.Body.Close()
			next, err := u.Parse(location)
			if err != nil {
				return nil, nil, category(ErrTrustBoundary, "invalid redirect")
			}
			if err := c.validateURL(next); err != nil {
				return nil, nil, category(ErrTrustBoundary, "unsafe redirect")
			}
			u = *next
			authorization = ""
			redirects++
			continue
		}
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			// An unauthenticated ping answering 401 still proves v2 support.
			if ping && res.StatusCode == http.StatusUnauthorized {
				return res, body, nil
			}
			res.Body.Close()
			if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
				return nil, nil, category(ErrAuth, "registry authorization denied")
			}
			return nil, nil, category(ErrUnavailable, fmt.Sprintf("registry status %d", res.StatusCode))
		}
		return res, body, nil
	}
}

// maxRetryAfterBudget bounds the total server-directed delay one client call may absorb across all
// of its requests, so a registry repeating Retry-After cannot stall a resolve for hours.
const maxRetryAfterBudget = 2 * time.Minute

type retryBudgetKey struct{}

type retryBudget struct {
	mu        sync.Mutex
	remaining time.Duration
}

func withRetryBudget(ctx context.Context) context.Context {
	if _, ok := ctx.Value(retryBudgetKey{}).(*retryBudget); ok {
		return ctx
	}
	return context.WithValue(ctx, retryBudgetKey{}, &retryBudget{remaining: maxRetryAfterBudget})
}

// chargeRetryAfter debits a server-directed delay from the call's budget and refuses the retry once
// the budget cannot cover it.
func chargeRetryAfter(ctx context.Context, err error) error {
	delay, ok := retry.RetryAfterDelay(err)
	if !ok {
		return err
	}
	budget, ok := ctx.Value(retryBudgetKey{}).(*retryBudget)
	if !ok {
		return err
	}
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if delay > budget.remaining {
		return category(ErrUnavailable, "registry retry delay exceeds the call budget")
	}
	budget.remaining -= delay
	return err
}

func (c *Client) request(ctx context.Context, method string, u *url.URL, authorization string, limit int64) (*http.Response, []byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		if cause := context.Cause(ctx); cause != nil {
			return nil, nil, "", cause
		}
		return nil, nil, "", err
	}
	req.Header.Set("Accept", strings.Join([]string{manifestOCI, indexOCI, manifestDocker, indexDocker}, ", "))
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	if limit < 0 {
		res, err := c.http.Do(req)
		if err != nil {
			return nil, nil, "", err
		}
		return res, nil, bearerChallenge(res.Header.Values("WWW-Authenticate")), nil
	}
	type bounded struct {
		response *http.Response
		body     []byte
	}
	result, err := retry.Do(ctx, c.retry, retry.ReadRetryable, func() (bounded, error) {
		attemptCtx, cancel := context.WithTimeout(ctx, requestTimeout(limit))
		defer cancel()
		// The attempt deadline is the client's own bound, so it must not be reported as the
		// caller's cancellation.
		attemptTimeout := func(err error) error {
			if attemptCtx.Err() != nil && ctx.Err() == nil {
				return category(ErrUnavailable, "registry response timed out")
			}
			return err
		}
		res, err := c.http.Do(req.Clone(attemptCtx))
		if err != nil {
			return bounded{}, attemptTimeout(err)
		}
		if statusErr := retry.RetryableStatusError(res.StatusCode, res.Header); statusErr != nil {
			res.Body.Close()
			return bounded{}, chargeRetryAfter(ctx, statusErr)
		}
		if method == http.MethodHead || res.StatusCode < 200 || res.StatusCode >= 300 {
			// Nothing reads this body, and the attempt deadline is cancelled on return.
			res.Body.Close()
			res.Body = http.NoBody
			return bounded{response: res}, nil
		}
		body, err := readLimit(res.Body, limit)
		if err != nil {
			res.Body.Close()
			if errors.Is(err, errBodyLimit) {
				return bounded{}, category(ErrIntegrity, "registry response exceeds body limit")
			}
			err = attemptTimeout(err)
			if cause := contextCause(err); cause != nil {
				return bounded{}, cause
			}
			return bounded{}, err
		}
		if err := res.Body.Close(); err != nil {
			return bounded{}, err
		}
		res.Body = io.NopCloser(bytes.NewReader(body))
		return bounded{response: res, body: body}, nil
	})
	if err != nil {
		return nil, nil, "", err
	}
	return result.response, result.body, bearerChallenge(result.response.Header.Values("WWW-Authenticate")), nil
}
func (c *Client) token(ctx context.Context, authority, challenge string) (string, error) {
	params, ok := bearerParams(challenge)
	if !ok {
		return "", category(ErrTrustBoundary, "invalid bearer challenge")
	}
	realm, err := url.Parse(params["realm"])
	if err != nil || realm.User != nil || realm.Host == "" {
		return "", category(ErrTrustBoundary, "invalid bearer realm")
	}
	if err := c.validateURL(realm); err != nil {
		return "", category(ErrTrustBoundary, "unsafe bearer realm")
	}
	credential, configured := c.credentials[authority]
	// A registry names its own realm, so a credentialed exchange is refused outright rather than
	// degraded to anonymous unless that realm is the registry itself or its host in tokenHosts.
	if configured && !permittedRealmHost(authority, realm.Host, realm.Scheme) {
		return "", category(ErrTrustBoundary, "bearer realm host is neither the credentialed registry nor its known token host")
	}
	q := realm.Query()
	for _, k := range []string{"service", "scope"} {
		if params[k] != "" {
			q.Set(k, params[k])
		}
	}
	realm.RawQuery = q.Encode()
	authorization := ""
	if configured {
		authorization = "Basic " + base64.StdEncoding.EncodeToString(
			[]byte(credential.Username+":"+credential.Secret),
		)
	}
	res, body, _, err := c.request(ctx, http.MethodGet, realm, authorization, manifestLimit)
	if err != nil {
		if cause := context.Cause(ctx); cause != nil {
			return "", cause
		}
		if cause := contextCause(err); cause != nil {
			return "", cause
		}
		if knownCategory(err) {
			return "", err
		}
		return "", unavailable(err, "token request")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		// The scope names the repository the registry refused, which distinguishes a private image
		// from a throttled anonymous client.
		if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
			access := "anonymous access only"
			if configured {
				access = "configured credentials rejected"
			}
			return "", category(ErrAuth, fmt.Sprintf(
				"token authorization denied for %s (status %d, %s)",
				params["scope"], res.StatusCode, access,
			))
		}
		return "", category(ErrUnavailable, fmt.Sprintf("token request failed with status %d", res.StatusCode))
	}
	var value struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if json.Unmarshal(body, &value) != nil {
		return "", category(ErrTrustBoundary, "invalid token response")
	}
	if value.Token != "" {
		return value.Token, nil
	}
	if value.AccessToken != "" {
		return value.AccessToken, nil
	}
	return "", category(ErrTrustBoundary, "missing bearer token")
}

func (c *Client) baseURL(authority string) (*url.URL, error) {
	if authority == "" {
		return nil, errors.New("missing registry authority")
	}
	scheme := "https"
	if c.fixture.IsValid() && authority == c.fixture.String() {
		scheme = "http"
	} else if _, _, err := net.SplitHostPort(authority); err == nil {
		host, port, _ := net.SplitHostPort(authority)
		if ip, err := netip.ParseAddr(host); err == nil && !allowed(ip.Unmap(), port, c.fixture) {
			return nil, errors.New("unsafe registry authority")
		}
	}
	return &url.URL{Scheme: scheme, Host: authority}, nil
}

func (c *Client) validateURL(u *url.URL) error {
	base, err := c.baseURL(u.Host)
	if err != nil || u.User != nil || u.Scheme != base.Scheme {
		return errors.New("unsafe registry URL")
	}
	return nil
}
func readLimit(r io.Reader, limit int64) ([]byte, error) {
	if limit == 0 {
		return nil, nil
	}
	raw, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, errBodyLimit
	}
	return raw, nil
}
