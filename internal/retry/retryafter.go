package retry

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// MaxRetryAfter is the longest server-directed delay a read will wait out. A
// longer one waits this instead of its full length, but still spends the wait
// and keeps its remaining attempts: giving up on the retry entirely would turn
// one blip into a failed read, which is the failure the retry exists to absorb.
const MaxRetryAfter = time.Minute

// RetryAfter is implemented by errors carrying a server-directed retry delay.
type RetryAfter interface{ RetryAfterDelay() time.Duration }

type statusRetryAfter struct {
	StatusError
	delay time.Duration
}

func (e statusRetryAfter) Unwrap() error                  { return e.StatusError }
func (e statusRetryAfter) RetryAfterDelay() time.Duration { return e.delay }

// StatusErrorFor reports a response status as a retry error, carrying the
// Retry-After delay when the server sent one.
func StatusErrorFor(status int, header http.Header) error {
	if delay, ok := RetryAfterHeader(header, status); ok {
		return statusRetryAfter{StatusError: StatusError(status), delay: delay}
	}
	return StatusError(status)
}

// RetryableStatusError returns the retry error for a retryable response status, or nil.
func RetryableStatusError(status int, header http.Header) error {
	if delay, ok := RetryAfterHeader(header, status); ok {
		return statusRetryAfter{StatusError: StatusError(status), delay: delay}
	}
	if RetryStatus(status) {
		return StatusError(status)
	}
	return nil
}

// RetryAfterHeader parses Retry-After at its stated length, in seconds or
// HTTP-date form, for the statuses that carry one: the retryable ones plus 403,
// which is how GitHub reports a secondary rate limit. Elsewhere the header is
// not an instruction to come back, so honouring it would invent a retry.
func RetryAfterHeader(header http.Header, status int) (time.Duration, bool) {
	if !RetryStatus(status) && status != http.StatusForbidden || header == nil {
		return 0, false
	}
	value := strings.TrimSpace(header.Get("Retry-After"))
	if value == "" {
		return 0, false
	}
	if seconds, err := strconv.ParseInt(value, 10, 32); err == nil {
		return atLeastZero(time.Duration(seconds) * time.Second), true
	}
	if at, err := http.ParseTime(value); err == nil {
		return atLeastZero(time.Until(at)), true
	}
	return 0, false
}

// RetryAfterDelay reports how long a caller should wait on the server-directed
// delay carried by err, if any: its stated length, capped at MaxRetryAfter.
// Callers metering a delay budget rely on this never exceeding that cap.
func RetryAfterDelay(err error) (time.Duration, bool) {
	var carrier RetryAfter
	if !errors.As(err, &carrier) {
		return 0, false
	}
	delay := carrier.RetryAfterDelay()
	if delay <= 0 {
		return 0, false
	}
	return min(delay, MaxRetryAfter), true
}

func atLeastZero(delay time.Duration) time.Duration {
	if delay < 0 {
		return 0
	}
	return delay
}
