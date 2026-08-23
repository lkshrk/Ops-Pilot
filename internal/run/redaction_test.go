package run

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lkshrk/ops-pilot/internal/cluster"
	"github.com/lkshrk/ops-pilot/internal/diagnostics"
	"github.com/lkshrk/ops-pilot/internal/domain"
	"github.com/lkshrk/ops-pilot/internal/events"
)

const (
	podToken   = "eyJhbGciOiJSUzI1NiIsImtpZCI6ImFiYyJ9.eyJzdWIiOiJzeXN0ZW06c2VydmljZWFjY291bnQifQ.c2lnbmF0dXJlLWJ5dGVzMDAw"
	podSecret  = "eyJzdWIiOiJzeXN0ZW06c2VydmljZWFjY291bnQifQ"
	namedValue = "api_key=Ai8fkq2LmZx0Rt7Yb3Nc"
	namedInner = "Ai8fkq2LmZx0Rt7Yb3Nc"
	databasees = "postgres://app:hunter2correcthorse@db:5432/app"
	dbPassword = "hunter2correcthorse"
)

type recordingRecorder struct {
	attempts  []domain.Attempt
	halted    string
	starts    int
	finished  int
	startErr  error
	finishErr error
}

func (r *recordingRecorder) StartRun(context.Context, domain.Run) error {
	r.starts++
	return r.startErr
}

func (r *recordingRecorder) FinishRun(_ context.Context, _ string, _ time.Time, halted string) error {
	r.halted = halted
	r.finished++
	return r.finishErr
}

func (r *recordingRecorder) RecordAttempt(_ context.Context, attempt domain.Attempt) error {
	r.attempts = append(r.attempts, attempt)
	return nil
}

// The database outlives the terminal, and the agent reads pod logs and
// repository files, so its prose quotes credentials belonging to the workload.
// A replacer over ops-pilot's own configured values cannot match one.
func TestTheHistoryDatabaseIsScrubbedOfCredentialsOpsPilotNeverHeld(t *testing.T) {
	recorder := &recordingRecorder{}
	runner := New(Dependencies{
		Recorder: recorder,
		Redactor: diagnostics.NewRedactor([]string{"a-configured-value"}),
	}, Options{})

	runner.record(context.Background(), domain.Attempt{
		Reason:         "reverted because " + podToken + " was rejected",
		DiagnosisCause: "the pod printed " + namedValue,
		Error:          "clone failed: " + databasees,
		Evidence:       []string{"the log line was " + podToken},
		Fixes:          []string{"- password: " + dbPassword},
	})

	if len(recorder.attempts) != 1 {
		t.Fatalf("want one recorded attempt, got %d", len(recorder.attempts))
	}
	stored := recorder.attempts[0]
	for field, value := range map[string]string{
		"Reason":         stored.Reason,
		"DiagnosisCause": stored.DiagnosisCause,
		"Error":          stored.Error,
		"Evidence":       strings.Join(stored.Evidence, " "),
		"Fixes":          strings.Join(stored.Fixes, " "),
	} {
		for _, secret := range []string{podSecret, namedInner, dbPassword} {
			if strings.Contains(value, secret) {
				t.Errorf("%s reached the database with %q: %q", field, secret, value)
			}
		}
	}
}

// Held as code points, not literals: a literal carrier is invisible to review
// and staticcheck rejects several of them outright.
var invisibleCarriers = []struct {
	name    string
	carrier rune
}{
	{name: "zero width space", carrier: 0x200B},
	{name: "zero width non joiner", carrier: 0x200C},
	{name: "zero width joiner", carrier: 0x200D},
	{name: "word joiner", carrier: 0x2060},
	{name: "byte order mark", carrier: 0xFEFF},
	{name: "soft hyphen", carrier: 0x00AD},
	{name: "right to left override", carrier: 0x202E},
	{name: "private use", carrier: 0xE000},
	{name: "carriage return", carrier: 0x000D},
}

func splitBy(carrier rune, head, tail string) string {
	return head + string(carrier) + tail
}

// Normalising the text before the needles run closes C-M89's split, but it lets
// a configured value that happens to be a key token eat the key, after which the
// shape rules no longer recognise "[REDACTED]: value" as a key-value pair and the
// workload's own credential is written out whole. Neither pass may destroy the
// evidence the other matches on.
func TestRedactingAKeyTokenDoesNotBlindTheShapeRulesToItsValue(t *testing.T) {
	const workload = "correcthorsebatterystaple"

	for _, carrier := range invisibleCarriers {
		t.Run(carrier.name, func(t *testing.T) {
			key := splitBy(carrier.carrier, "pass", "word")
			recorder := &recordingRecorder{}
			runner := New(Dependencies{
				Recorder: recorder,
				Redactor: diagnostics.NewRedactor([]string{"password"}),
			}, Options{})

			runner.record(context.Background(), domain.Attempt{
				Reason: key + ": |\n  " + workload,
			})

			if stored := recorder.attempts[0].Reason; strings.Contains(stored, workload) {
				t.Errorf("the workload credential reached the database verbatim: %q", stored)
			}
		})
	}
}

// The shape rules match a span rather than a whole configured value, so a secret
// whose head is credential shaped leaves a scrub-first sink as "[REDACTED]"
// followed by its own tail.
func TestAConfiguredSecretWhoseHeadIsCredentialShapedIsRedactedWhole(t *testing.T) {
	const (
		secret = "AKIAIOSFODNN7EXAMPLEtailofthesecret"
		tail   = "tailofthesecret"
	)
	recorder := &recordingRecorder{}
	runner := New(Dependencies{
		Recorder: recorder,
		Redactor: diagnostics.NewRedactor([]string{secret}),
	}, Options{})

	runner.record(context.Background(), domain.Attempt{
		Reason: "the pod printed " + secret + " and died",
	})

	stored := recorder.attempts[0].Reason
	if strings.Contains(stored, tail) {
		t.Errorf("a shape rule ate the head and left the tail: %q", stored)
	}
	if strings.Contains(stored, secret) {
		t.Errorf("the configured secret reached the database whole: %q", stored)
	}
}

// C-L130's closures, pinned at rest: a shape credential ops-pilot never held is
// still scrubbed when a rune splits it, and a configured value that carries such
// a rune is still matched in both the form configured and the form rendered.
func TestTheInvisibleRuneClosuresHoldAtTheHistorySink(t *testing.T) {
	carrying := splitBy(0x200B, "hunter2", "correcthorse")
	shapeSplit := splitBy(0x200B, "AKIA", "IOSFODNN7EXAMPLE")

	tests := []struct {
		name       string
		configured []string
		reason     string
		forbidden  string
	}{
		{
			name:       "a shape credential split by an invisible rune",
			configured: []string{"a-configured-value"},
			reason:     "the pod printed " + shapeSplit,
			forbidden:  "AKIAIOSFODNN7EXAMPLE",
		},
		{
			name:       "a configured secret carrying a rune, quoted verbatim",
			configured: []string{carrying},
			reason:     "the pod printed " + carrying,
			forbidden:  "hunter2correcthorse",
		},
		{
			name:       "a configured secret carrying a rune, quoted rendered",
			configured: []string{carrying},
			reason:     "the pod printed hunter2correcthorse",
			forbidden:  "hunter2correcthorse",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := &recordingRecorder{}
			runner := New(Dependencies{
				Recorder: recorder,
				Redactor: diagnostics.NewRedactor(test.configured),
			}, Options{})

			runner.record(context.Background(), domain.Attempt{Reason: test.reason})

			if stored := recorder.attempts[0].Reason; strings.Contains(stored, test.forbidden) {
				t.Errorf("the sink kept %q: %q", test.forbidden, stored)
			}
		})
	}
}

type collectingEvents struct{ emitted []events.Event }

func (c *collectingEvents) Bind(domain.Run) {}

func (c *collectingEvents) Emit(event events.Event) { c.emitted = append(c.emitted, event) }

type unobservableCluster struct {
	*gateObserver
	err error
}

func (o *unobservableCluster) Watch(
	context.Context, domain.HealthSnapshot, string,
) (cluster.Outcome, error) {
	return cluster.Outcome{}, o.err
}

// A halt reason is built from a controller's error, so it carries the same
// untrusted text as attempt.Error - which is redacted before it is stored. The
// halt was not, on either of the two sinks it reaches.
func TestAHaltReasonIsRedactedBeforeItIsEmittedAndStored(t *testing.T) {
	g := newGate(t)
	recorder := &recordingRecorder{}
	stream := &collectingEvents{}
	g.runner.recorder = recorder
	g.runner.events = stream
	g.runner.options.OnlyPullRequest = 1204
	request := g.forge.requests[1204]
	request.Body = renovateBody("sonarr", "4.0.14", "4.0.19")
	g.forge.requests[1204] = request
	g.runner.observer = &unobservableCluster{
		gateObserver: g.observer,
		err: fmt.Errorf("flux said api_key=%s using %s",
			workloadSecret, configuredSecret),
	}

	_, err := g.runner.Run(context.Background())
	if !errors.Is(err, ErrHalted) {
		t.Fatalf("run: %v, want a halt", err)
	}

	if recorder.halted == "" {
		t.Fatal("the run did not halt; the fixture no longer reaches the halt path")
	}
	if !strings.Contains(recorder.halted, "#1204 merged but could not be observed") {
		t.Fatalf("the halt lost the reason an operator acts on: %q", recorder.halted)
	}
	for _, secret := range []string{configuredSecret, workloadSecret} {
		if strings.Contains(recorder.halted, secret) {
			t.Errorf("the stored halt reason kept %q: %q", secret, recorder.halted)
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("the error Run returned kept %q: %q", secret, err.Error())
		}
		for _, event := range stream.emitted {
			if event.Kind == events.Halted && strings.Contains(event.Reason, secret) {
				t.Errorf("the emitted halt reason kept %q: %q", secret, event.Reason)
			}
		}
	}
}

// Broken carries the cluster's own message for each object, which is the
// largest untrusted surface on the attempt and the one no redaction covered.
func TestBrokenObjectMessagesAreScrubbedBeforeTheyAreStored(t *testing.T) {
	recorder := &recordingRecorder{}
	runner := New(Dependencies{
		Recorder: recorder,
		Redactor: diagnostics.NewRedactor([]string{"a-configured-value"}),
	}, Options{})

	broken := []domain.ObjectHealth{{
		Ref:    domain.ObjectRef{Kind: "HelmRelease", Namespace: "media", Name: "sonarr"},
		Reason: "upgrade failed: " + namedValue,
	}}
	runner.record(context.Background(), domain.Attempt{Broken: broken})

	stored := recorder.attempts[0].Broken
	if len(stored) != 1 {
		t.Fatalf("want one broken object, got %d", len(stored))
	}
	if strings.Contains(stored[0].Reason, namedInner) {
		t.Errorf("the controller message reached the database: %q", stored[0].Reason)
	}
	if stored[0].Ref.String() != "media/HelmRelease/sonarr" {
		t.Errorf("the object lost its identity: %q", stored[0].Ref.String())
	}
	if broken[0].Reason != "upgrade failed: "+namedValue {
		t.Errorf("record rewrote the caller's slice: %q", broken[0].Reason)
	}
}

// The narrative is the operator's only live view of what the cluster said, and
// every word of it originates outside ops-pilot.
func TestTheNarrativeIsScrubbedOfCredentialsOpsPilotNeverHeld(t *testing.T) {
	tests := []struct {
		name   string
		render func(*Runner)
		secret string
	}{
		{
			name:   "a failure reason",
			render: func(r *Runner) { r.outcome(outcomeBad, "Reverted", "the pod printed "+podToken) },
			secret: podSecret,
		},
		{
			name:   "a step",
			render: func(r *Runner) { r.step("connecting to %s", databasees) },
			secret: dbPassword,
		},
		{
			name: "a broken object's controller message",
			render: func(r *Runner) {
				r.broken([]domain.ObjectHealth{{
					Ref:    domain.ObjectRef{Kind: "HelmRelease", Namespace: "media", Name: "sonarr"},
					Reason: "upgrade failed: " + namedValue,
				}})
			},
			secret: namedInner,
		},
		{
			name:   "the agent's evidence",
			render: func(r *Runner) { r.evidence(true, []string{"the log line was " + podToken}) },
			secret: podSecret,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var out bytes.Buffer
			runner := New(Dependencies{
				Out:      &out,
				Redactor: diagnostics.NewRedactor([]string{"a-configured-value"}),
			}, Options{Verbosity: VerbosityVerbose})
			test.render(runner)

			if strings.Contains(out.String(), test.secret) {
				t.Fatalf("the narrative kept %q:\n%s", test.secret, out.String())
			}
		})
	}
}
