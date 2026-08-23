package retry

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"
)

func header(value string) http.Header {
	h := http.Header{}
	if value != "" {
		h.Set("Retry-After", value)
	}
	return h
}

func TestRetryAfterHeaderParsesSecondsAndDatesForRateLimitStatuses(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		header http.Header
		want   time.Duration
		ok     bool
	}{
		{name: "seconds on 429", status: 429, header: header("30"), want: 30 * time.Second, ok: true},
		{name: "seconds on 403", status: 403, header: header("45"), want: 45 * time.Second, ok: true},
		{name: "seconds on 503", status: 503, header: header("30"), want: 30 * time.Second, ok: true},
		{name: "seconds on 500", status: 500, header: header("5"), want: 5 * time.Second, ok: true},
		{name: "seconds on 504", status: 504, header: header("5"), want: 5 * time.Second, ok: true},
		{name: "reported at its stated length", status: 429, header: header("3600"), want: time.Hour, ok: true},
		{name: "http date", status: 403, header: header(time.Now().UTC().Add(90 * time.Second).Format(http.TimeFormat)), want: 90 * time.Second, ok: true},
		{name: "past http date", status: 429, header: header(time.Now().UTC().Add(-time.Hour).Format(http.TimeFormat)), want: 0, ok: true},
		{name: "absent", status: 429, header: header(""), ok: false},
		{name: "unparseable", status: 429, header: header("soon"), ok: false},
		{name: "not found never carries a retry instruction", status: 404, header: header("30"), ok: false},
		{name: "redirect never carries a retry instruction", status: 301, header: header("30"), ok: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, ok := RetryAfterHeader(test.header, test.status)
			if ok != test.ok {
				t.Fatalf("ok = %v, want %v", ok, test.ok)
			}
			if ok && (got < test.want-2*time.Second || got > test.want) {
				t.Fatalf("delay = %v, want ~%v", got, test.want)
			}
		})
	}
}

func TestRetryableStatusErrorHonorsRetryAfterOnForbidden(t *testing.T) {
	err := RetryableStatusError(403, header("12"))
	if err == nil || !ReadRetryable(err) {
		t.Fatalf("403 with Retry-After is not retryable: %v", err)
	}
	var status StatusError
	if !errors.As(err, &status) || int(status) != 403 {
		t.Fatalf("status not recoverable from %v", err)
	}
	if RetryableStatusError(403, header("")) != nil {
		t.Fatal("403 without Retry-After became retryable")
	}
}

func TestScheduleUsesRetryAfterDelayInsteadOfBackoff(t *testing.T) {
	var waits []time.Duration
	attempts := 0
	schedule := Schedule{Sleep: func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }, Jitter: func(d time.Duration) time.Duration { return d }}
	err := schedule.Run(context.Background(), ReadRetryable, func() error {
		attempts++
		if attempts == 1 {
			return StatusErrorFor(429, header("7"))
		}
		return StatusErrorFor(503, nil)
	})
	if err == nil || attempts != 3 {
		t.Fatalf("err=%v attempts=%d", err, attempts)
	}
	if len(waits) != 2 || waits[0] != 7*time.Second || waits[1] != time.Second {
		t.Fatalf("waits = %v, want [7s 1s]", waits)
	}
}

func TestAnOverlongRetryAfterIsCappedRatherThanAbandoned(t *testing.T) {
	var waits []time.Duration
	attempts := 0
	schedule := Schedule{Sleep: func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }}
	err := schedule.Run(context.Background(), ReadRetryable, func() error {
		attempts++
		if attempts == 1 {
			return StatusErrorFor(429, header("3600"))
		}
		return nil
	})
	if err != nil || attempts != 2 {
		t.Fatalf("err=%v attempts=%d, want the retry to still happen", err, attempts)
	}
	if len(waits) != 1 || waits[0] != MaxRetryAfter {
		t.Fatalf("waits = %v, want one wait at the bound", waits)
	}
}

func TestRetryAfterDelayNeverExceedsTheBoundACallerMetersAgainst(t *testing.T) {
	for _, value := range []string{"61", "121", "600", "3600"} {
		delay, ok := RetryAfterDelay(StatusErrorFor(503, header(value)))
		if !ok || delay != MaxRetryAfter {
			t.Fatalf("Retry-After %s reported as %v (ok=%v), want %v", value, delay, ok, MaxRetryAfter)
		}
	}
	delay, ok := RetryAfterDelay(StatusErrorFor(503, header("45")))
	if !ok || delay != 45*time.Second {
		t.Fatalf("a delay inside the bound was reshaped to %v", delay)
	}
}

func TestATighterScheduleBoundShortensTheWaitWithoutEndingTheRetry(t *testing.T) {
	var waits []time.Duration
	attempts := 0
	schedule := Schedule{
		Sleep:         func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil },
		MaxRetryAfter: 10 * time.Second,
	}
	err := schedule.Run(context.Background(), ReadRetryable, func() error {
		attempts++
		if attempts == 1 {
			return StatusErrorFor(503, header("120"))
		}
		return nil
	})
	if err != nil || attempts != 2 {
		t.Fatalf("err=%v attempts=%d, want the retry to still happen", err, attempts)
	}
	if len(waits) != 1 || waits[0] != 10*time.Second {
		t.Fatalf("waits = %v, want one wait at the tighter bound", waits)
	}
}

// RetryableStatusError feeding Do is the shape both the httpfetch and OCI read
// paths use, so a change to it silently changes them.
func TestARetryableStatusStillRecoversWhateverRetryAfterItCarries(t *testing.T) {
	for _, test := range []struct {
		name   string
		status int
		value  string
		want   time.Duration
	}{
		{name: "no header falls back to the schedule", status: 503, value: "", want: 250 * time.Millisecond},
		{name: "inside the bound is honoured in full", status: 503, value: "45", want: 45 * time.Second},
		{name: "just past the bound", status: 503, value: "121", want: MaxRetryAfter},
		{name: "far past the bound", status: 502, value: "600", want: MaxRetryAfter},
		{name: "forbidden with a secondary rate limit", status: 403, value: "60", want: MaxRetryAfter},
	} {
		t.Run(test.name, func(t *testing.T) {
			var waits []time.Duration
			attempts := 0
			schedule := Schedule{
				Sleep:  func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil },
				Jitter: func(d time.Duration) time.Duration { return d },
			}
			_, err := Do(context.Background(), schedule, ReadRetryable, func() (int, error) {
				attempts++
				if attempts == 1 {
					return 0, RetryableStatusError(test.status, header(test.value))
				}
				return 200, nil
			})
			if err != nil || attempts != 2 {
				t.Fatalf("err=%v attempts=%d, want a recovered second attempt", err, attempts)
			}
			if len(waits) != 1 || waits[0] != test.want {
				t.Fatalf("waits = %v, want [%v]", waits, test.want)
			}
		})
	}
}

// The OCI client meters a fixed per-call budget against RetryAfterDelay and
// refuses the retry once a delay will not fit, so an uncapped delay there ends
// the call on its first throttled request.
func TestAFixedDelayBudgetSurvivesEveryRetryAfterAServerCanSend(t *testing.T) {
	const budget = 2 * time.Minute
	remaining := budget
	attempts := 0
	schedule := Schedule{Sleep: func(context.Context, time.Duration) error { return nil }}
	_, err := Do(context.Background(), schedule, ReadRetryable, func() (int, error) {
		attempts++
		if attempts > 2 {
			return 200, nil
		}
		err := RetryableStatusError(403, header("3600"))
		delay, ok := RetryAfterDelay(err)
		if !ok {
			t.Fatalf("attempt %d carried no delay", attempts)
		}
		if delay > remaining {
			t.Fatalf("attempt %d wanted %v of a %v budget with %v left", attempts, delay, budget, remaining)
		}
		remaining -= delay
		return 0, err
	})
	if err != nil || attempts != 3 {
		t.Fatalf("err=%v attempts=%d, want the call to survive its full schedule", err, attempts)
	}
}

func TestScheduleWillNotStartARetryAfterWaitItCannotFinishBeforeTheDeadline(t *testing.T) {
	var waits []time.Duration
	attempts := 0
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	schedule := Schedule{Sleep: func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }}
	err := schedule.Run(ctx, ReadRetryable, func() error {
		attempts++
		return StatusErrorFor(503, header("30"))
	})
	var status StatusError
	if !errors.As(err, &status) || int(status) != 503 {
		t.Fatalf("err = %v, want the 503 surfaced", err)
	}
	if attempts != 1 || len(waits) != 0 {
		t.Fatalf("attempts=%d waits=%v, want one attempt and no wait", attempts, waits)
	}
}

func TestScheduleStillHonoursARetryAfterThatFitsTheDeadline(t *testing.T) {
	var waits []time.Duration
	attempts := 0
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	schedule := Schedule{Sleep: func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }}
	err := schedule.Run(ctx, ReadRetryable, func() error {
		attempts++
		return StatusErrorFor(503, header("5"))
	})
	if err == nil || attempts != 3 {
		t.Fatalf("err=%v attempts=%d", err, attempts)
	}
	if len(waits) != 2 || waits[0] != 5*time.Second || waits[1] != 5*time.Second {
		t.Fatalf("waits = %v, want [5s 5s]", waits)
	}
}
