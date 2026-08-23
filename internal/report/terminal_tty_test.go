//go:build linux || darwin

package report_test

import (
	"bytes"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/term"

	"github.com/lkshrk/ops-pilot/internal/display"
	"github.com/lkshrk/ops-pilot/internal/domain"
	"github.com/lkshrk/ops-pilot/internal/report"
)

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
// Width to columns, the only way a test can drive Render or Interrupted
// through the real terminal-width detection rather than display.DefaultWidth.
// Closing the slave discards whatever the master has not yet read, so the
// returned function polls a background reader instead of reading after close.
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

// The 125-column interrupt sentence wraps on any real terminal; a 100-column
// bytes.Buffer test would never see it.
func TestAnInterruptedRunReportsOneLineAndNoTable(t *testing.T) {
	const width = 76
	tty, read := terminalAtWidth(t, width)
	run := domain.Run{
		ID: "20260822-222132.123048000",
		Attempts: []domain.Attempt{{
			PullRequest: 823,
			Dependency:  domain.Dependency{Name: "ghcr.io/goauthentik/server"},
			Verdict:     domain.VerdictError,
			Error: "GitHub GET /repos/lkshrk/h-cloud/pulls/823/files?per_page=100 " +
				"transport failure: CLI signal interrupt",
		}},
	}

	if err := report.Interrupted(tty, run, false); err != nil {
		t.Fatalf("interrupted: %v", err)
	}

	got := read()
	for i, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
		if got := display.Width(line); got > width {
			t.Errorf("line %d is %d columns wide, want at most %d: %q", i+1, got, width, line)
		}
	}
	for _, forbidden := range []string{"ERROR", "/repos/", "per_page", "RESULT", "#823"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("the interrupt line carries %q:\n%s", forbidden, got)
		}
	}
	if !strings.Contains(got, run.ID) {
		t.Fatalf("the interrupt line does not name the run:\n%s", got)
	}
}

// A destination narrower than six columns' worth of the eight-column floor
// used to force every column to that floor regardless of the budget, leaving
// the rendered total wider than the destination that asked for it.
func TestNoRenderedTableLineIsWiderThanTheDestination(t *testing.T) {
	tests := []struct {
		name   string
		width  int
		header []string
		rows   [][]string
		fixed  []int
	}{
		{
			name:   "the history listing of a pull request bumping five images",
			width:  display.DefaultWidth,
			header: []string{"RUN", "PR", "DEP", "BUMP", "RESULT", "TOOK"},
			rows: [][]string{{
				"20260822-082304.078918000", "#738",
				"ghcr.io/siderolabs/kubelet, registry.k8s.io/kube-apiserver, " +
					"registry.k8s.io/kube-controller-manager, registry.k8s.io/kube-proxy, " +
					"registry.k8s.io/kube-scheduler",
				" -> ", "SKIPPED", "0s",
			}},
		},
		{
			name:   "a run summary carrying a sixty-column reason",
			width:  display.DefaultWidth,
			header: []string{"PR", "DEP", "BUMP", "RESULT", "TOOK"},
			rows: [][]string{{
				"#823", "ghcr.io/goauthentik/server", "2026.5.6 -> 2026.8.0",
				"ERROR " + strings.Repeat("a", 60), "0s",
			}},
		},
		{
			name:   "six columns all naturally wider than the floor at a narrow budget",
			width:  41,
			header: []string{"RUN", "PR", "DEP", "BUMP", "RESULT", "TOOK"},
			rows: [][]string{{
				"20260822-082304.078918000", "#99999999",
				"ghcr.io/siderolabs/kubelet, registry.k8s.io/kube-apiserver",
				"1.0.0 -> 2.0.0", "SKIPPEDSKIP", "12h34m56s",
			}},
			fixed: []int{0},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			table := report.NewTable(test.header...)
			for _, column := range test.fixed {
				table.Fixed(column)
			}
			for _, row := range test.rows {
				table.Add(row...)
			}
			tty, read := terminalAtWidth(t, test.width)
			if err := table.Render(tty); err != nil {
				t.Fatalf("render: %v", err)
			}
			got := read()
			for i, line := range strings.Split(strings.TrimSuffix(got, "\n"), "\n") {
				if width := display.Width(line); width > test.width {
					t.Errorf("line %d is %d columns wide, want at most %d: %q",
						i+1, width, test.width, line)
				}
			}
		})
	}
}
