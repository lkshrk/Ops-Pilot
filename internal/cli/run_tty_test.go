//go:build linux || darwin

package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"

	"github.com/lkshrk/ops-pilot/internal/display"
	"github.com/lkshrk/ops-pilot/internal/domain"

	"github.com/spf13/cobra"
)

func terminalReader(t *testing.T) *os.File {
	t.Helper()
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	t.Cleanup(func() { ptmx.Close(); tty.Close() })
	return tty
}

// terminalCapture accumulates whatever a pty master reads, safely for a
// background reader goroutine and a polling test goroutine to share.
type terminalCapture struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (c *terminalCapture) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.Write(p)
}

func (c *terminalCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

// terminalAtWidth opens a pty sized so display.NewStyle resolves its usable
// Width to columns. Closing the slave discards whatever the master has not
// yet read, so the returned function polls a background reader instead of
// reading after close.
func terminalAtWidth(t *testing.T, columns int) (*os.File, func() string) {
	t.Helper()
	t.Setenv("NO_COLOR", "1")
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no pty available: %v", err)
	}
	t.Cleanup(func() { ptmx.Close(); tty.Close() })
	if _, err := term.MakeRaw(int(tty.Fd())); err != nil {
		t.Fatalf("set the pty to raw mode: %v", err)
	}
	if err := pty.Setsize(tty, &pty.Winsize{Rows: 24, Cols: uint16(columns + 4)}); err != nil {
		t.Fatalf("size the pty: %v", err)
	}
	capture := &terminalCapture{}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				capture.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	return tty, func() string {
		deadline := time.Now().Add(2 * time.Second)
		var stable string
		for time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
			current := capture.String()
			if current != "" && current == stable {
				return current
			}
			stable = current
		}
		return capture.String()
	}
}

// A history line the operator cannot copy whole is useless: `history --run
// <id>` needs the id exactly as printed, so the RUN column must never be the
// one fit() shrinks to make room for the rest.
func TestTheHistoryRunColumnIsNeverTruncated(t *testing.T) {
	tty, read := terminalAtWidth(t, 76)
	command := &cobra.Command{}
	command.SetOut(tty)
	runID := "20260822-082304.078918000"
	runs := []domain.Run{{
		ID: runID,
		Attempts: []domain.Attempt{{
			PullRequest: 738,
			Dependency: domain.Dependency{
				Name: "ghcr.io/siderolabs/kubelet, registry.k8s.io/kube-apiserver, " +
					"registry.k8s.io/kube-controller-manager, registry.k8s.io/kube-proxy, " +
					"registry.k8s.io/kube-scheduler",
			},
			Verdict:  domain.VerdictSkipped,
			Duration: time.Second,
		}},
	}}

	if err := renderHistory(command, runs); err != nil {
		t.Fatalf("render history: %v", err)
	}

	got := read()
	if !strings.Contains(got, runID) {
		t.Fatalf("the history listing truncated the run id:\n%s", got)
	}
}

// At 41-47 columns the RUN column's Fixed(0) used to collapse the fit floor to 1, where display.Clip no-ops and renders the column at its full natural width.
func TestNoHistoryLineExceedsNarrowTerminals(t *testing.T) {
	runID := "20260822-082304.078918000"
	run := domain.Run{
		ID: runID,
		Attempts: []domain.Attempt{{
			PullRequest: 738,
			Dependency: domain.Dependency{
				Name: "ghcr.io/siderolabs/kubelet, registry.k8s.io/kube-apiserver, " +
					"registry.k8s.io/kube-controller-manager, registry.k8s.io/kube-proxy, " +
					"registry.k8s.io/kube-scheduler",
			},
			Verdict:  domain.VerdictSkipped,
			Duration: time.Second,
		}},
	}
	for width := 41; width <= 48; width++ {
		t.Run(fmt.Sprintf("%d columns", width), func(t *testing.T) {
			tty, read := terminalAtWidth(t, width)
			command := &cobra.Command{}
			command.SetOut(tty)

			if err := renderHistory(command, []domain.Run{run}); err != nil {
				t.Fatalf("render history: %v", err)
			}

			got := read()
			for i, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
				if w := display.Width(line); w > width {
					t.Errorf("line %d is %d columns wide, want at most %d: %q", i+1, w, width, line)
				}
			}
		})
	}
}

// The approver sniffs the reader it was handed to decide how to take an answer,
// so deciding interactivity from a different reader can promise a prompt that is
// then read from a pipe. The revert prompt is the one that matters: an
// unanswerable one returns keep, leaving a merge the watch already found broken.
func TestInteractivityFollowsTheReaderTheApproverWillRead(t *testing.T) {
	terminal := func(t *testing.T) io.Reader { return terminalReader(t) }
	pipe := func(t *testing.T) io.Reader { return pipeReader(t) }
	tests := []struct {
		name         string
		stdin, input func(*testing.T) io.Reader
		args         []string
		want         bool
	}{
		{name: "the command was given the terminal", stdin: pipe, input: terminal, want: true},
		{name: "the command was given a pipe", stdin: terminal, input: pipe, want: false},
		{name: "both are the terminal", stdin: terminal, input: terminal, want: true},
		{
			name:  "a terminal with --non-interactive",
			stdin: terminal, input: terminal,
			args: []string{"--non-interactive"},
			want: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			approver := runApprover(t, test.stdin(t), test.input(t), test.args...)
			if got := approver.Interactive(); got != test.want {
				t.Fatalf("Interactive() = %v, want %v", got, test.want)
			}
		})
	}
}
