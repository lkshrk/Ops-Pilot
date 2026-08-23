//go:build linux || darwin

package cli

import (
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/lkshrk/ops-pilot/internal/run"
)

// terminalLog accumulates everything the pty master sees: the echo of what an
// operator types and everything the approver prints.
type terminalLog struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *terminalLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

func (l *terminalLog) waitFor(t *testing.T, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(l.String(), want) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("never saw %q on the terminal:\n%s", want, l.String())
}

func operatorTerminal(t *testing.T) (*terminalLog, func(string), *Approver) {
	t.Helper()
	plainOutput(t)
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	t.Cleanup(func() { ptmx.Close(); tty.Close() })
	log := &terminalLog{}
	go func() {
		buf := make([]byte, 1024)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				log.mu.Lock()
				log.buf.Write(buf[:n])
				log.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	typed := func(s string) {
		if _, err := ptmx.WriteString(s); err != nil {
			t.Errorf("typing %q: %v", s, err)
		}
	}
	return log, typed, NewApprover(tty, tty, true, nil)
}

// A line typed while no question was on screen was aimed at an earlier one and
// must not answer the revert prompt — least of all as consent to discard a
// merge nobody was asked about.
func TestTypeAheadAtATerminalCannotAnswerTheRevertPrompt(t *testing.T) {
	log, typed, approver := operatorTerminal(t)
	typed("y\n")
	log.waitFor(t, "y")

	done := make(chan run.RevertChoice, 1)
	go func() {
		choice, err := approver.ConfirmRevert(testRevert())
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		done <- choice
	}()
	log.waitFor(t, "Resolve this rollout?")
	typed("n\n")
	select {
	case choice := <-done:
		if choice != run.RevertKeep {
			t.Errorf("choice = %q, want %q: the stale yes answered instead of the operator", choice, run.RevertKeep)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the revert prompt never accepted the operator's answer:\n%s", log.String())
	}
}

func TestClarifyAtATerminalReturnsTheAnswerForThatQuestion(t *testing.T) {
	log, typed, approver := operatorTerminal(t)
	typed("stale answer\n")
	log.waitFor(t, "stale answer")

	type result struct {
		answer   string
		answered bool
		err      error
	}
	done := make(chan result, 1)
	go func() {
		answer, answered, err := approver.Clarify(testApproval(), "Which values file needs changing?")
		done <- result{answer, answered, err}
	}()
	log.waitFor(t, "you        > ")
	typed("charts/nginx/values.yaml\n")
	select {
	case got := <-done:
		if got.err != nil || !got.answered || got.answer != "charts/nginx/values.yaml" {
			t.Errorf("Clarify() = (%q, %t, %v)", got.answer, got.answered, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the clarification prompt never accepted the operator's answer:\n%s", log.String())
	}
}

// Canonical mode holds a line back until enter, so a half-typed line is
// invisible to a read yet still delivered later — the case draining reads
// cannot catch. It must be flushed all the same, or it prefixes the real
// answer.
func TestAHalfTypedLineIsFlushedNotPrefixedToTheAnswer(t *testing.T) {
	log, typed, approver := operatorTerminal(t)
	typed("y")
	log.waitFor(t, "y")

	done := make(chan run.RevertChoice, 1)
	go func() {
		choice, err := approver.ConfirmRevert(testRevert())
		if err != nil {
			t.Errorf("unexpected error: %v", err)
		}
		done <- choice
	}()
	log.waitFor(t, "Resolve this rollout?")
	typed("n\n")
	select {
	case choice := <-done:
		if choice != run.RevertKeep {
			t.Errorf("choice = %q, want %q", choice, run.RevertKeep)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("the prompt never resolved — the half-typed line prefixed the answer:\n%s", log.String())
	}
}

// Canonical mode never hands over a line longer than its buffer, so an operator
// cannot produce the over-long answer that leaves a pipe out of step: reading
// past the tail at a terminal would only block on a line nobody was asked for,
// while the flush before each question already clears it. Blocking here would
// strand the run at a prompt the operator has no way to finish.
func TestATerminalIsResynchronisedByTheFlushRatherThanByReadingPastTheTail(t *testing.T) {
	plainOutput(t)
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	t.Cleanup(func() { ptmx.Close(); tty.Close() })

	dropped := make(chan bool, 1)
	go func() { dropped <- NewApprover(tty, tty, true, nil).dropRest() }()
	select {
	case ok := <-dropped:
		if !ok {
			t.Error("a terminal was declared out of step, silencing every later question")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("dropping the tail blocked on a terminal with nothing queued to read")
	}

	// A pipe has no flush behind it, so the tail has to be read past instead.
	if NewApprover(&bytesWithoutANewline{budget: 16 * maxAnswer}, io.Discard, true, nil).dropRest() {
		t.Error("a stream that never ends a line was reported as resynchronised")
	}
}
