// Package events emits a machine-readable record of what a run decided and
// changed, as it happens.
//
// The history database answers questions after a run; this answers them during
// one, which is what an unattended run needs to be alertable. It records
// decisions and external writes, not narration: anything a person would want
// prose for belongs on stdout instead.
package events

import (
	"encoding/json"
	"io"
	"os"
	"sync"
	"time"

	"github.com/lkshrk/ops-pilot/internal/diagnostics"
	"github.com/lkshrk/ops-pilot/internal/domain"
)

type Kind string

const (
	RunStarted  Kind = "run_started"
	RunFinished Kind = "run_finished"
	Halted      Kind = "halted"

	Assessed Kind = "assessed"
	Skipped  Kind = "skipped"

	Merged      Kind = "merged"
	Diagnosed   Kind = "diagnosed"
	FixApplied  Kind = "fix_applied"
	Reverted    Kind = "reverted"
	Kept        Kind = "kept"
	Labelled    Kind = "labelled"
	Closed      Kind = "closed"
	WatchResult Kind = "watch_result"
	Failed      Kind = "failed"
)

// Event is one line of the stream. Fields absent from a given kind are omitted
// rather than emitted empty, so a consumer can match on presence.
type Event struct {
	Time       time.Time `json:"ts"`
	Kind       Kind      `json:"event"`
	RunID      string    `json:"runId"`
	Repository string    `json:"repository,omitempty"`

	PullRequest int    `json:"pr,omitempty"`
	Dependency  string `json:"dependency,omitempty"`
	From        string `json:"from,omitempty"`
	To          string `json:"to,omitempty"`

	Decision string `json:"decision,omitempty"`
	Reason   string `json:"reason,omitempty"`
	Watch    string `json:"watch,omitempty"`
	Action   string `json:"action,omitempty"`
	Label    string `json:"label,omitempty"`
	SHA      string `json:"sha,omitempty"`
	Error    string `json:"error,omitempty"`

	// Objects carries what broke, with the cluster's reason for each, so an
	// alert can name the affected service rather than only the pull request.
	Objects []Object `json:"objects,omitempty"`
}

type Object struct {
	Ref    string `json:"ref"`
	Reason string `json:"reason,omitempty"`
}

// Emitter writes one JSON object per line. A write failure is recorded once and
// then ignored: the stream is an observation channel and must never be able to
// stop a run that is midway through changing a cluster.
type Emitter struct {
	mu       sync.Mutex
	out      io.WriteCloser
	now      func() time.Time
	redactor *diagnostics.Redactor
	run      string
	repo     string
	failed   error
}

// Open creates or appends to the stream at path. An empty path yields a nil
// emitter, which is safe to call and does nothing. The redactor covers the
// configured values, which carry no shape for ScrubSecrets to find.
func Open(path string, now func() time.Time, redactor *diagnostics.Redactor) (*Emitter, error) {
	if path == "" {
		return nil, nil
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	return &Emitter{out: file, now: now, redactor: redactor}, nil
}

// Bind records the run this stream belongs to, so every later event carries it.
func (e *Emitter) Bind(run domain.Run) {
	if e == nil {
		return
	}
	e.mu.Lock()
	e.run, e.repo = run.ID, run.Repository.String()
	e.mu.Unlock()
}

func (e *Emitter) Close() error {
	if e == nil {
		return nil
	}
	return e.out.Close()
}

// Err reports a write failure, so a caller can mention it once at the end
// rather than per event.
func (e *Emitter) Err() error {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.failed
}

func (e *Emitter) Emit(event Event) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.failed != nil {
		return
	}
	event.Time, event.RunID, event.Repository = e.now().UTC(), e.run, e.repo
	e.scrub(&event)
	raw, err := json.Marshal(event)
	if err != nil {
		e.failed = err
		return
	}
	if _, err := e.out.Write(append(raw, '\n')); err != nil {
		e.failed = err
	}
}

// scrub covers only the prose fields: every other field is an identifier a
// consumer matches on, where a redaction is silent and permanent.
func (e *Emitter) scrub(event *Event) {
	event.Reason = e.clean(event.Reason)
	event.Error = e.clean(event.Error)
	if len(event.Objects) == 0 {
		return
	}
	// The caller still renders the same objects elsewhere and shares this array.
	objects := make([]Object, len(event.Objects))
	copy(objects, event.Objects)
	for i := range objects {
		objects[i].Reason = e.clean(objects[i].Reason)
	}
	event.Objects = objects
}

func (e *Emitter) clean(value string) string {
	return storable(diagnostics.ScrubSecrets(e.redactor.Redact(value)))
}

// storable is the shared rule: JSON escaping makes control bytes inert in the
// file, but a consumer reads the stream with `jq -r` and hands the decoded bytes
// straight to a terminal.
func storable(value string) string { return diagnostics.Storable(value) }

// About starts an event already carrying the pull request it concerns.
func About(kind Kind, request domain.PullRequest, dependency domain.Dependency) Event {
	return Event{
		Kind:        kind,
		PullRequest: request.Number,
		Dependency:  dependency.Name,
		From:        dependency.FromVersion,
		To:          dependency.ToVersion,
	}
}

// Objects renders cluster health for the stream.
func Objects(health []domain.ObjectHealth) []Object {
	if len(health) == 0 {
		return nil
	}
	objects := make([]Object, 0, len(health))
	for _, item := range health {
		objects = append(objects, Object{Ref: item.Ref.String(), Reason: item.Reason})
	}
	return objects
}
