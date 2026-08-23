package httpfetch

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/lkshrk/ops-pilot/internal/retry"
)

type resolverFunc func(context.Context, string, string) ([]netip.Addr, error)

func (f resolverFunc) LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error) {
	return f(ctx, network, host)
}

type dialerFunc func(context.Context, string, string) (net.Conn, error)

func (f dialerFunc) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	return f(ctx, network, address)
}

type remoteConn struct {
	net.Conn
	remote net.Addr
}

func (c remoteConn) RemoteAddr() net.Addr { return c.remote }

func resolver(addrs ...string) Resolver {
	return resolverFunc(func(context.Context, string, string) ([]netip.Addr, error) {
		out := make([]netip.Addr, len(addrs))
		for i, raw := range addrs {
			out[i] = netip.MustParseAddr(raw)
		}
		return out, nil
	})
}

func peerDialer(t *testing.T, peer string, calls *[]string) Dialer {
	t.Helper()
	return dialerFunc(func(_ context.Context, _, address string) (net.Conn, error) {
		*calls = append(*calls, address)
		left, right := net.Pipe()
		t.Cleanup(func() { _ = right.Close() })
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		if peer == "" {
			peer = host
		}
		return remoteConn{Conn: left, remote: &net.TCPAddr{IP: net.ParseIP(peer), Port: mustPort(t, port)}}, nil
	})
}

func mustPort(t *testing.T, raw string) int {
	t.Helper()
	p, err := net.LookupPort("tcp", raw)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFetchRejectsNonPublicDestinations(t *testing.T) {
	for _, address := range []string{"127.0.0.1", "10.0.0.1", "100.64.0.1", "192.0.2.1", "198.18.0.1", "192.88.99.1", "[::ffff:127.0.0.1]", "[64:ff9b:1::1]", "[100:0:0:1::1]", "[2002::1]", "[2001:db8::1]", "[3fff::1]", "[4000::1]", "[5f00::1]", "[6000::1]"} {
		t.Run(address, func(t *testing.T) {
			_, err := New(Options{}).Fetch(context.Background(), "https://"+address+"/notes", Limits{})
			if !errors.Is(err, ErrPrivateDestination) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestFetchRejectsMixedPublicPrivateDNS(t *testing.T) {
	for _, answers := range [][]string{{"1.1.1.1", "127.0.0.1"}, {"127.0.0.1", "1.1.1.1"}} {
		client := New(Options{Resolver: resolver(answers...), Dialer: dialerFunc(func(context.Context, string, string) (net.Conn, error) {
			t.Fatal("dialer called")
			return nil, nil
		})})
		if _, err := client.Fetch(context.Background(), "https://example.test/notes", Limits{}); !errors.Is(err, ErrPrivateDestination) {
			t.Fatalf("answers %v: error = %v", answers, err)
		}
	}
}

func TestFetchRejectsSpecialPurposeDNSAnswer(t *testing.T) {
	client := New(Options{Resolver: resolver("1.1.1.1", "64:ff9b:1::1"), Dialer: dialerFunc(func(context.Context, string, string) (net.Conn, error) {
		t.Fatal("dialer called")
		return nil, nil
	})})
	if _, err := client.Fetch(context.Background(), "https://example.test/notes", Limits{}); !errors.Is(err, ErrPrivateDestination) {
		t.Fatalf("error = %v", err)
	}
}

func TestFetchDialsValidatedNumericAddressAndRejectsRebinding(t *testing.T) {
	var calls []string
	client := New(Options{Resolver: resolver("1.1.1.1"), Dialer: peerDialer(t, "2.2.2.2", &calls)})
	_, err := client.Fetch(context.Background(), "https://example.test/notes", Limits{Timeout: 20 * time.Millisecond})
	if !errors.Is(err, ErrPeerMismatch) {
		t.Fatalf("error = %v", err)
	}
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "1.1.1.1:") {
		t.Fatalf("dialed = %v", calls)
	}
}

func TestFetchRejectsUnsafeRedirects(t *testing.T) {
	for name, target := range map[string]string{"downgrade": "http://example.test/next", "private": "https://127.0.0.1/next", "special": "https://[64:ff9b:1::1]/next"} {
		t.Run(name, func(t *testing.T) {
			client := New(Options{Resolver: resolver("1.1.1.1"), Dialer: peerDialer(t, "1.1.1.1", new([]string))})
			client.transport = roundTripperFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{target}}, Body: io.NopCloser(strings.NewReader(""))}, nil
			})
			_, err := client.Fetch(context.Background(), "https://example.test/notes", Limits{})
			if err == nil {
				t.Fatal("expected redirect error")
			}
		})
	}
}

func TestFetchClosesEachConnection(t *testing.T) {
	client := New(Options{Resolver: resolver("1.1.1.1"), Dialer: peerDialer(t, "1.1.1.1", new([]string))})
	client.transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if r.Header.Get("Connection") != "close" {
			t.Fatalf("Connection = %q", r.Header.Get("Connection"))
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	got, err := client.Fetch(context.Background(), "https://example.test/notes", Limits{})
	if err != nil || string(got.Content) != "ok" {
		t.Fatalf("result=%+v err=%v", got, err)
	}
}

func TestFetchBoundsDecodedGzipBody(t *testing.T) {
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	_, _ = zw.Write([]byte("abcdef"))
	_ = zw.Close()
	client := New(Options{Resolver: resolver("1.1.1.1"), Dialer: peerDialer(t, "1.1.1.1", new([]string))})
	client.transport = roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Encoding": []string{"gzip"}}, Body: io.NopCloser(bytes.NewReader(compressed.Bytes()))}, nil
	})
	got, err := client.Fetch(context.Background(), "https://example.test/notes", Limits{MaxBytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	if string(got.Content) != "abcd" || !got.Truncated {
		t.Fatalf("result = %+v", got)
	}
}

func TestFetchTimeoutAndHeaderLimit(t *testing.T) {
	t.Run("timeout", func(t *testing.T) {
		client := New(Options{Resolver: resolver("1.1.1.1"), Dialer: peerDialer(t, "1.1.1.1", new([]string))})
		client.transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
			<-r.Context().Done()
			return nil, r.Context().Err()
		})
		_, err := client.Fetch(context.Background(), "https://example.test/notes", Limits{Timeout: time.Millisecond})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("headers", func(t *testing.T) {
		client := New(Options{Resolver: resolver("1.1.1.1"), Dialer: peerDialer(t, "1.1.1.1", new([]string))})
		client.transport = roundTripperFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"X-Large": []string{strings.Repeat("x", 33<<10)}}, Body: io.NopCloser(strings.NewReader("ok"))}, nil
		})
		_, err := client.Fetch(context.Background(), "https://example.test/notes", Limits{MaxHeaderBytes: 32 << 10})
		if !errors.Is(err, ErrHeadersTooLarge) {
			t.Fatalf("error = %v", err)
		}
	})
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFetchRetriesTransientFinalBodyRead(t *testing.T) {
	hits := 0
	first := &trackedBody{Reader: errReader{}}
	c := New(Options{Retry: retry.Schedule{Sleep: func(context.Context, time.Duration) error { return nil }}})
	c.transport = roundTripperFunc(func(*http.Request) (*http.Response, error) {
		hits++
		if hits == 1 {
			return &http.Response{StatusCode: 200, Header: make(http.Header), Body: first}, nil
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
	})
	result, err := c.Fetch(context.Background(), "https://example.test/a", Limits{Timeout: time.Second, MaxBytes: 8})
	if err != nil || string(result.Content) != "ok" || hits != 2 || !first.closed {
		t.Fatalf("result=%+v err=%v hits=%d closed=%v", result, err, hits, first.closed)
	}
}

// What decides whether a server-directed Retry-After is waited out is the fetch
// deadline, not the retry bound: internal/composition passes an empty Limits, so
// every model fetch runs on defaultTimeout, and retry.fitsBeforeDeadline refuses a
// delay that would leave no attempt worth making. Refusing is the point - the
// model is handed a status it can reason about instead of a context deadline it
// cannot - so the cliff is behaviour, and 9s and 11s straddle it here to catch a
// change to either constant moving it.
func TestARetryAfterIsWaitedOutOnlyWhileTheDefaultFetchDeadlineStillLeavesAnAttempt(t *testing.T) {
	if got := normalizedLimits(Limits{}).Timeout; got != 10*time.Second {
		t.Fatalf("this test straddles the fetch deadline with 9s and 11s; an empty Limits now means %s", got)
	}
	for _, test := range []struct {
		name       string
		status     int
		retryAfter string
		wantHits   int
		wantRetry  bool
	}{
		{name: "503 inside the deadline", status: http.StatusServiceUnavailable, retryAfter: "9", wantHits: 2, wantRetry: true},
		{name: "503 past the deadline", status: http.StatusServiceUnavailable, retryAfter: "11", wantHits: 1},
		{name: "403 inside the deadline", status: http.StatusForbidden, retryAfter: "9", wantHits: 2, wantRetry: true},
		{name: "403 past the deadline", status: http.StatusForbidden, retryAfter: "11", wantHits: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			hits := 0
			c := New(Options{Retry: retry.Schedule{Sleep: func(context.Context, time.Duration) error { return nil }}})
			c.transport = roundTripperFunc(func(*http.Request) (*http.Response, error) {
				hits++
				if hits > 1 {
					return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("ok"))}, nil
				}
				return &http.Response{
					StatusCode: test.status,
					Header:     http.Header{"Retry-After": []string{test.retryAfter}},
					Body:       io.NopCloser(strings.NewReader("")),
				}, nil
			})

			result, err := c.Fetch(context.Background(), "https://example.test/a", Limits{})

			if hits != test.wantHits {
				t.Fatalf("want %d attempt(s), got %d", test.wantHits, hits)
			}
			if test.wantRetry {
				if err != nil || string(result.Content) != "ok" {
					t.Fatalf("want the directed wait taken and the retry served, got %+v err=%v", result, err)
				}
				return
			}
			var status retry.StatusError
			if !errors.As(err, &status) || int(status) != test.status {
				t.Fatalf("want %d handed back as a status the model can read, got %v", test.status, err)
			}
			if result.StatusCode != 0 || result.Content != nil {
				t.Fatalf("want no result alongside the refused wait, got %+v", result)
			}
		})
	}
}

type trackedBody struct {
	io.Reader
	closed bool
}

func (b *trackedBody) Close() error { b.closed = true; return nil }

type errReader struct{}

func (errReader) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func TestFetchAllowlistRefusesRedirectToDisallowedHost(t *testing.T) {
	var visited []string
	client := New(Options{})
	client.transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		visited = append(visited, r.URL.Host)
		if r.URL.Host == "raw.githubusercontent.com" {
			return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://attacker.example/collect?d=STOLEN-SA-TOKEN"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("STOLEN-SA-TOKEN"))}, nil
	})
	result, err := client.Fetch(context.Background(), "https://raw.githubusercontent.com/o/r/main/CHANGELOG.md", Limits{AllowedHosts: []string{"raw.githubusercontent.com"}})
	if !errors.Is(err, ErrHostNotAllowed) {
		t.Fatalf("error = %v, want ErrHostNotAllowed", err)
	}
	if len(result.Content) != 0 {
		t.Fatalf("attacker body leaked: %q", result.Content)
	}
	for _, host := range visited {
		if host == "attacker.example" {
			t.Fatalf("attacker host was contacted: %v", visited)
		}
	}
}

func TestFetchAllowlistFollowsRedirectBetweenAllowedHosts(t *testing.T) {
	client := New(Options{})
	client.transport = roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		if r.URL.Host == "github.com" {
			return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://raw.githubusercontent.com/o/r/CHANGELOG.md"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("changelog"))}, nil
	})
	result, err := client.Fetch(context.Background(), "https://github.com/o/r/releases", Limits{AllowedHosts: []string{"github.com", "raw.githubusercontent.com"}})
	if err != nil || string(result.Content) != "changelog" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestFetchAllowlistRejectsDisallowedFirstHop(t *testing.T) {
	client := New(Options{})
	client.transport = roundTripperFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("transport called for a disallowed host")
		return nil, nil
	})
	_, err := client.Fetch(context.Background(), "https://attacker.example/x", Limits{AllowedHosts: []string{"github.com"}})
	if !errors.Is(err, ErrHostNotAllowed) {
		t.Fatalf("error = %v, want ErrHostNotAllowed", err)
	}
}

func TestFetchAllowlistBoundedByMaxRedirects(t *testing.T) {
	client := New(Options{})
	client.transport = roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"https://github.com/next"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	_, err := client.Fetch(context.Background(), "https://github.com/start", Limits{AllowedHosts: []string{"github.com"}, MaxRedirects: 2})
	if !errors.Is(err, ErrTooManyRedirects) {
		t.Fatalf("error = %v, want ErrTooManyRedirects", err)
	}
}
