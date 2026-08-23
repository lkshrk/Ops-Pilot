package retry

import (
	"context"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/url"
	"syscall"
	"testing"
	"time"
)

func TestReadRetriesAtMostThreeTimesWithSchedule(t *testing.T) {
	var waits []time.Duration
	attempts := 0
	err := Schedule{Sleep: func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }, Jitter: func(d time.Duration) time.Duration { return d }}.Run(context.Background(), func(error) bool { return true }, func() error {
		attempts++
		return errors.New("temporary")
	})
	if err == nil || attempts != 3 {
		t.Fatalf("err=%v attempts=%d, want error and 3 attempts", err, attempts)
	}
	if len(waits) != 2 || waits[0] != 250*time.Millisecond || waits[1] != time.Second {
		t.Fatalf("waits=%v, want [250ms 1s]", waits)
	}
}

func TestTransientTransportIsNarrow(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{"reset", &net.OpError{Err: syscall.ECONNRESET}, true},
		{"url reset", &url.Error{Err: &net.OpError{Err: syscall.ECONNRESET}}, true},
		{"temporary dns", &net.DNSError{IsTemporary: true}, true},
		{"unexpected eof", io.ErrUnexpectedEOF, true},
		{"context", context.Canceled, false},
		{"arbitrary net error", net.UnknownNetworkError("no"), false},
		{"ordinary", errors.New("no"), false},
		{"tls authority", x509.UnknownAuthorityError{}, false},
		{"redirect policy", &url.Error{Err: errors.New("redirect policy")}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := TransientTransport(test.err); got != test.want {
				t.Fatalf("got %v", got)
			}
		})
	}
}

func TestRunPreservesCancellationCause(t *testing.T) {
	cause := errors.New("caller stopped")
	ctx, cancel := context.WithCancelCause(context.Background())
	err := Schedule{Sleep: func(context.Context, time.Duration) error { cancel(cause); return context.Cause(ctx) }}.Run(ctx, func(error) bool { return true }, func() error { return io.ErrUnexpectedEOF })
	if !errors.Is(err, cause) {
		t.Fatalf("err=%v, want cause", err)
	}
}

func TestRetryStatusIsBounded(t *testing.T) {
	for _, test := range []struct {
		status int
		want   bool
	}{{429, true}, {500, true}, {599, true}, {400, false}, {499, false}, {600, false}, {999, false}} {
		if got := RetryStatus(test.status); got != test.want {
			t.Fatalf("status %d: got %v", test.status, got)
		}
	}
}

func TestStatusErrorNamesTheStatusItCarries(t *testing.T) {
	for _, test := range []struct {
		err  error
		want string
	}{
		{StatusError(500), "retryable HTTP status 500"},
		{StatusError(503), "retryable HTTP status 503"},
		{StatusErrorFor(429, header("30")), "retryable HTTP status 429"},
	} {
		if got := test.err.Error(); got != test.want {
			t.Fatalf("got %q, want %q", got, test.want)
		}
	}
}

func TestARequestedAttemptCountIsClampedToTheScheduleBound(t *testing.T) {
	calls := 0
	_, _ = doN(context.Background(), Schedule{Sleep: func(context.Context, time.Duration) error { return nil }}, 4, func(error) bool { return true }, func() (int, error) { calls++; return 0, io.ErrUnexpectedEOF })
	if calls != 3 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestRunAndDoStopAtTheSameBoundedAttemptCount(t *testing.T) {
	schedule := Schedule{Sleep: func(context.Context, time.Duration) error { return nil }}
	always := func(error) bool { return true }

	runCalls := 0
	_ = schedule.Run(context.Background(), always, func() error { runCalls++; return io.ErrUnexpectedEOF })
	doCalls := 0
	_, _ = Do(context.Background(), schedule, always, func() (int, error) { doCalls++; return 0, io.ErrUnexpectedEOF })

	if runCalls != MaxAttempts || doCalls != MaxAttempts {
		t.Fatalf("Run made %d attempts, Do made %d, want MaxAttempts=%d for both", runCalls, doCalls, MaxAttempts)
	}
}

func TestReadStopsWhenContextCanceledDuringWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	attempts := 0
	err := Schedule{Sleep: func(context.Context, time.Duration) error { cancel(); return ctx.Err() }}.Run(ctx, func(error) bool { return true }, func() error {
		attempts++
		return errors.New("temporary")
	})
	if !errors.Is(err, context.Canceled) || attempts != 1 {
		t.Fatalf("err=%v attempts=%d, want canceled after one attempt", err, attempts)
	}
}
