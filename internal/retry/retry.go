// Package retry provides the bounded schedule shared by external read paths.
package retry

import (
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"net/url"
	"strconv"
	"syscall"
	"time"
)

type StatusError int

func (e StatusError) Error() string { return "retryable HTTP status " + strconv.Itoa(int(e)) }

var delays = [...]time.Duration{250 * time.Millisecond, time.Second}

// MaxAttempts is the total attempt count the single bounded schedule allows.
const MaxAttempts = len(delays) + 1

type Schedule struct {
	Sleep  func(context.Context, time.Duration) error
	Jitter func(time.Duration) time.Duration
	// MaxRetryAfter caps a server-directed delay below the package bound, for a
	// caller whose own window is spent by the wait. Zero means the package bound.
	MaxRetryAfter time.Duration
}

// Do executes a replayable operation through the single bounded schedule.
func Do[T any](ctx context.Context, schedule Schedule, retryable func(error) bool, attempt func() (T, error)) (T, error) {
	return doN(ctx, schedule, MaxAttempts, retryable, attempt)
}

func doN[T any](ctx context.Context, schedule Schedule, attempts int, retryable func(error) bool, attempt func() (T, error)) (T, error) {
	var result T
	if attempts < 1 {
		attempts = 1
	}
	if attempts > MaxAttempts {
		attempts = MaxAttempts
	}
	err := schedule.run(ctx, attempts, retryable, func() error {
		var err error
		result, err = attempt()
		return err
	})
	return result, err
}

// TransientTransport reports errors that are safe to retry for a replayable read.
func TransientTransport(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		err = urlErr.Err
	}
	if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
		return true
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return dns.IsTimeout || dns.IsTemporary
	}
	var op *net.OpError
	if !errors.As(err, &op) {
		return false
	}
	if op.Timeout() {
		return true
	}
	return errors.Is(op, syscall.ECONNRESET) || errors.Is(op, syscall.ECONNREFUSED) || errors.Is(op, syscall.EPIPE) || errors.Is(op, syscall.ECONNABORTED) || errors.Is(op, syscall.ETIMEDOUT)
}

func RetryStatus(status int) bool { return status == 429 || status >= 500 && status <= 599 }
func ReadRetryable(err error) bool {
	if TransientTransport(err) {
		return true
	}
	if _, ok := RetryAfterDelay(err); ok {
		return true
	}
	var status StatusError
	return errors.As(err, &status) && RetryStatus(int(status))
}

// Run executes a replayable operation that yields no value through the
// single bounded schedule.
func (s Schedule) Run(ctx context.Context, retryable func(error) bool, attempt func() error) error {
	return s.run(ctx, MaxAttempts, retryable, attempt)
}

func (s Schedule) run(ctx context.Context, attempts int, retryable func(error) bool, attempt func() error) error {
	for i := 0; ; i++ {
		if err := context.Cause(ctx); err != nil {
			return err
		}
		err := attempt()
		if err == nil || i+1 >= attempts || !retryable(err) {
			return err
		}
		if delay, ok := RetryAfterDelay(err); ok {
			if s.MaxRetryAfter > 0 {
				delay = min(delay, s.MaxRetryAfter)
			}
			if !fitsBeforeDeadline(ctx, delay) {
				return err
			}
			if err := s.sleep(ctx, delay); err != nil {
				return err
			}
			continue
		}
		if err := s.wait(ctx, i); err != nil {
			return err
		}
	}
}

// fitsBeforeDeadline reports whether the wait could finish in time to leave an
// attempt worth making. Sleeping past the caller's deadline buys no retry and
// only trades a diagnosable status for a context error.
func fitsBeforeDeadline(ctx context.Context, delay time.Duration) bool {
	deadline, ok := ctx.Deadline()
	return !ok || time.Until(deadline) > delay
}

func (s Schedule) wait(ctx context.Context, attempt int) error {
	delay := delays[attempt]
	if s.Jitter != nil {
		delay = s.Jitter(delay)
	} else {
		delay += time.Duration(rand.Int64N(int64(delay) + 1))
	}
	if delay > 2*time.Second {
		delay = 2 * time.Second
	}
	if delay < 0 {
		delay = 0
	}
	return s.sleep(ctx, delay)
}

func (s Schedule) sleep(ctx context.Context, delay time.Duration) error {
	if s.Sleep != nil {
		return s.Sleep(ctx, delay)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return context.Cause(ctx)
	case <-timer.C:
		return nil
	}
}
