// Package httpfetch fetches untrusted public HTTPS content through a bounded
// transport that resolves and validates every connection target itself.
package httpfetch

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/lkshrk/ops-pilot/internal/netpolicy"
	"github.com/lkshrk/ops-pilot/internal/retry"
)

var (
	ErrInvalidURL         = errors.New("httpfetch: URL must be public HTTPS without userinfo")
	ErrPrivateDestination = errors.New("httpfetch: destination is not public")
	ErrPeerMismatch       = errors.New("httpfetch: connected peer differs from resolved address")
	ErrHeadersTooLarge    = errors.New("httpfetch: response headers exceed limit")
	ErrTooManyRedirects   = errors.New("httpfetch: too many redirects")
	ErrMissingRedirect    = errors.New("httpfetch: redirect lacks location")
	ErrHostNotAllowed     = errors.New("httpfetch: host is not on the allowlist")
)

const (
	defaultTimeout       = 10 * time.Second
	defaultMaxRedirects  = 5
	defaultMaxHeaderSize = 32 << 10
	defaultMaxBodySize   = 512 << 10
)

type Resolver interface {
	LookupNetIP(context.Context, string, string) ([]netip.Addr, error)
}

type Dialer interface {
	DialContext(context.Context, string, string) (net.Conn, error)
}

type Options struct {
	Resolver Resolver
	Dialer   Dialer
	Retry    retry.Schedule
}

type Limits struct {
	Timeout                  time.Duration
	MaxRedirects             int
	MaxHeaderBytes, MaxBytes int64
	// AllowedHosts, when non-empty, gates every hop including redirect targets; empty imposes no host restriction.
	AllowedHosts []string
}

type Result struct {
	FinalURL   string
	StatusCode int
	Content    []byte
	Truncated  bool
}

type Client struct {
	resolver  Resolver
	dialer    Dialer
	transport http.RoundTripper
	retry     retry.Schedule
}

func New(options Options) *Client {
	resolver := options.Resolver
	if resolver == nil {
		resolver = net.DefaultResolver
	}
	dialer := options.Dialer
	if dialer == nil {
		dialer = &net.Dialer{}
	}
	c := &Client{resolver: resolver, dialer: dialer, retry: options.Retry}
	c.transport = &http.Transport{
		Proxy:              nil,
		DisableCompression: true,
		DisableKeepAlives:  true,
		DialContext:        c.dialContext,
	}
	return c
}

func (c *Client) Fetch(ctx context.Context, rawURL string, limits Limits) (Result, error) {
	limits = normalizedLimits(limits)
	ctx, cancel := context.WithTimeout(ctx, limits.Timeout)
	defer cancel()

	u, err := validateURL(rawURL)
	if err != nil {
		return Result{}, err
	}
	for redirects := 0; ; redirects++ {
		if redirects > limits.MaxRedirects {
			return Result{}, ErrTooManyRedirects
		}
		if !hostAllowed(u.Hostname(), limits.AllowedHosts) {
			return Result{}, ErrHostNotAllowed
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		if err != nil {
			return Result{}, err
		}
		req.Header.Set("Connection", "close")
		hop, err := c.fetchHop(ctx, req, limits)
		if err != nil {
			return Result{}, err
		}
		if isRedirect(hop.status) {
			location := hop.location
			if location == "" {
				return Result{}, ErrMissingRedirect
			}
			next, err := u.Parse(location)
			if err != nil {
				return Result{}, ErrInvalidURL
			}
			u, err = validateURL(next.String())
			if err != nil {
				return Result{}, err
			}
			continue
		}
		return Result{FinalURL: u.String(), StatusCode: hop.status, Content: hop.content, Truncated: hop.truncated}, nil
	}
}

type hopResult struct {
	status    int
	location  string
	content   []byte
	truncated bool
}

func (c *Client) fetchHop(ctx context.Context, req *http.Request, limits Limits) (hopResult, error) {
	return retry.Do(ctx, c.retry, retry.ReadRetryable, func() (hopResult, error) {
		res, err := c.roundTrip(req.Clone(ctx), limits.MaxHeaderBytes)
		if err != nil {
			return hopResult{}, err
		}
		if headerBytes(res.Header) > limits.MaxHeaderBytes {
			res.Body.Close()
			return hopResult{}, ErrHeadersTooLarge
		}
		if statusErr := retry.RetryableStatusError(res.StatusCode, res.Header); statusErr != nil {
			res.Body.Close()
			return hopResult{}, statusErr
		}
		if isRedirect(res.StatusCode) {
			location := res.Header.Get("Location")
			res.Body.Close()
			return hopResult{status: res.StatusCode, location: location}, nil
		}
		content, truncated, err := readBody(res, limits.MaxBytes)
		res.Body.Close()
		if err != nil {
			return hopResult{}, err
		}
		return hopResult{status: res.StatusCode, content: content, truncated: truncated}, nil
	})
}

func normalizedLimits(l Limits) Limits {
	if l.Timeout <= 0 {
		l.Timeout = defaultTimeout
	}
	if l.MaxRedirects <= 0 {
		l.MaxRedirects = defaultMaxRedirects
	}
	if l.MaxHeaderBytes <= 0 {
		l.MaxHeaderBytes = defaultMaxHeaderSize
	}
	if l.MaxBytes <= 0 {
		l.MaxBytes = defaultMaxBodySize
	}
	return l
}

func (c *Client) roundTrip(req *http.Request, maxHeaderBytes int64) (*http.Response, error) {
	if transport, ok := c.transport.(*http.Transport); ok {
		clone := transport.Clone()
		clone.MaxResponseHeaderBytes = maxHeaderBytes
		return clone.RoundTrip(req)
	}
	return c.transport.RoundTrip(req)
}

func (c *Client) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	addresses, err := c.resolve(ctx, host)
	if err != nil {
		return nil, err
	}
	selected := addresses[0]
	conn, err := c.dialer.DialContext(ctx, network, net.JoinHostPort(selected.String(), port))
	if err != nil {
		return nil, err
	}
	peer, err := peerAddress(conn.RemoteAddr())
	if err != nil || peer != selected {
		conn.Close()
		return nil, ErrPeerMismatch
	}
	return conn, nil
}

func (c *Client) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if literal, err := netip.ParseAddr(host); err == nil {
		literal = literal.Unmap()
		if !netpolicy.Public(literal) {
			return nil, ErrPrivateDestination
		}
		return []netip.Addr{literal}, nil
	}
	addresses, err := c.resolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return nil, err
	}
	if len(addresses) == 0 {
		return nil, fmt.Errorf("httpfetch: no DNS addresses")
	}
	for i := range addresses {
		addresses[i] = addresses[i].Unmap()
		if !netpolicy.Public(addresses[i]) {
			return nil, ErrPrivateDestination
		}
	}
	return addresses, nil
}

func validateURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return nil, ErrInvalidURL
	}
	if _, err := netip.ParseAddr(u.Hostname()); err == nil {
		if _, err := New(Options{}).resolve(context.Background(), u.Hostname()); err != nil {
			return nil, err
		}
	}
	return u, nil
}

func peerAddress(addr net.Addr) (netip.Addr, error) {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return netip.Addr{}, err
	}
	peer, err := netip.ParseAddr(host)
	return peer.Unmap(), err
}

func readBody(res *http.Response, max int64) ([]byte, bool, error) {
	var body io.Reader = res.Body
	if strings.EqualFold(strings.TrimSpace(res.Header.Get("Content-Encoding")), "gzip") {
		zr, err := gzip.NewReader(res.Body)
		if err != nil {
			return nil, false, err
		}
		defer zr.Close()
		body = zr
	}
	content, err := io.ReadAll(io.LimitReader(body, max+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(content)) > max {
		return content[:max], true, nil
	}
	return content, false, nil
}

func headerBytes(header http.Header) int64 {
	var n int64
	for key, values := range header {
		for _, value := range values {
			n += int64(len(key) + len(value) + 4)
		}
	}
	return n
}

func isRedirect(status int) bool { return status >= 300 && status <= 399 }

func hostAllowed(host string, allowed []string) bool {
	if len(allowed) == 0 {
		return true
	}
	for _, candidate := range allowed {
		if strings.EqualFold(candidate, host) {
			return true
		}
	}
	return false
}
