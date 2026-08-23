package kubernetes

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/lkshrk/ops-pilot/internal/retry"
)

// maxDirectedDelay caps how long an apiserver may hold up one read. A watch
// window polls against its own wall-clock settle deadline, so a read that waits
// longer than a poll spends the window rather than the server's patience. A
// longer Retry-After waits this much and retries anyway; it is never a reason
// to stop retrying, because the read failing is what loses the window.
const maxDirectedDelay = 10 * time.Second

// readRetryTransport bounds replayable Kubernetes reads and removes Retry-After
// so client-go cannot add an independent retry loop.
type readRetryTransport struct {
	base  http.RoundTripper
	retry retry.Schedule
	// observe reports that a read had to be retried, so a window that spent
	// minutes fighting a flaky API is distinguishable from a healthy one.
	observe func(method, path string, retries int)
}

// RetryReads wraps a transport so replayable Kubernetes reads survive a
// transient API failure. Without it a single blip during a ten-minute watch
// fails the whole window.
func RetryReads(base http.RoundTripper, observe func(string, string, int)) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return readRetryTransport{base: base, observe: observe}
}

func (t readRetryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		response, err := t.base.RoundTrip(req)
		clearRetryAfter(response)
		return response, err
	}
	attempts := 0
	schedule := t.retry
	schedule.MaxRetryAfter = maxDirectedDelay
	response, err := retry.Do(req.Context(), schedule, retry.ReadRetryable, func() (*http.Response, error) {
		attempts++
		response, err := t.base.RoundTrip(req.Clone(req.Context()))
		if err != nil {
			clearRetryAfter(response)
			return nil, err
		}
		if !retry.RetryStatus(response.StatusCode) {
			clearRetryAfter(response)
			return response, nil
		}
		statusErr := retry.StatusErrorFor(response.StatusCode, response.Header)
		clearRetryAfter(response)
		if attempts == retry.MaxAttempts {
			return response, nil
		}
		response.Body.Close()
		return nil, statusErr
	})
	// A window that spent four of its ten minutes retrying a flaky API looked
	// identical to a healthy one, so the retries are reported once they happen.
	if attempts > 1 && t.observe != nil {
		t.observe(req.Method, req.URL.Path, attempts-1)
	}
	if err != nil && retry.TransientTransport(err) && context.Cause(req.Context()) == nil {
		return nil, boundedReadError{err}
	}
	return response, err
}

type boundedReadError struct{ err error }

func (boundedReadError) Error() string {
	return "bounded Kubernetes read failed after " + strconv.Itoa(retry.MaxAttempts) + " attempts"
}

func clearRetryAfter(response *http.Response) {
	if response != nil {
		response.Header.Del("Retry-After")
	}
}
