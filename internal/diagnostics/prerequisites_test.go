package diagnostics_test

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lkshrk/ops-pilot/internal/diagnostics"
)

// The exported floor exists so no caller keeps a second copy, which only works
// while the exported pair is the pair the check actually enforces.
func TestCheckPrerequisitesEnforcesExactlyTheExportedGitFloor(t *testing.T) {
	tests := []struct {
		name       string
		gitVersion string
		wantError  bool
	}{
		{
			name:       "one minor below the floor",
			gitVersion: fmt.Sprintf("git version %d.%d.9", diagnostics.MinimumGitMajor, diagnostics.MinimumGitMinor-1),
			wantError:  true,
		},
		{
			name:       "exactly the floor",
			gitVersion: fmt.Sprintf("git version %d.%d.0", diagnostics.MinimumGitMajor, diagnostics.MinimumGitMinor),
			wantError:  false,
		},
		{
			name:       "one minor above the floor",
			gitVersion: fmt.Sprintf("git version %d.%d.0", diagnostics.MinimumGitMajor, diagnostics.MinimumGitMinor+1),
			wantError:  false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			git := versionExecutable(t, test.gitVersion)

			err := diagnostics.CheckPrerequisites(context.Background(), git)
			if test.wantError != (err != nil) {
				t.Fatalf("CheckPrerequisites() = %v on %q, want error=%t", err, test.gitVersion, test.wantError)
			}
		})
	}
}

// Wrappers print to stdout before they exec the real git, so the version line
// is not always the whole output. Refusing one is a startup halt on a machine
// where every git operation the run performs succeeds.
func TestCheckPrerequisitesFindsTheVersionLineAmongAWrappersOtherOutput(t *testing.T) {
	tests := []struct {
		name   string
		output string
	}{
		{
			name:   "banner before the version",
			output: "warning: some corporate wrapper banner\ngit version 2.45.1",
		},
		{
			name:   "notice after the version",
			output: "git version 2.45.1\nrun `wrapper login` to refresh your credentials",
		},
		{
			name:   "carriage returns around the version",
			output: "warning: banner\r\ngit version 2.45.1\r",
		},
		{
			name:   "parenthesized vendor tag on the version line",
			output: "git version 2.39.5 (Apple Git-154)",
		},
		{
			name:   "an upgrade nag mentioning a newer version is not a rival claim",
			output: "git version 2.45.1\ngit version 2.99.0 is available for download",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			git := versionExecutable(t, test.output)

			if err := diagnostics.CheckPrerequisites(context.Background(), git); err != nil {
				t.Fatalf("CheckPrerequisites() error = %v", err)
			}
		})
	}
}

// Reading past the first line means a line that merely mentions a version can
// be mistaken for git's answer, and an overstated version is the direction that
// waves an unusable git through.
func TestCheckPrerequisitesRefusesOutputThatClaimsTwoGitVersions(t *testing.T) {
	tests := []struct {
		name   string
		output string
	}{
		{
			name:   "wrapper claims a different clean version above the real one",
			output: "git version 2.99.0\ngit version 2.45.1",
		},
		{
			name:   "two wrappers disagree",
			output: "git version 2.45.1\ngit version 2.30.0",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			git := versionExecutable(t, test.output)

			err := diagnostics.CheckPrerequisites(context.Background(), git)
			if err == nil {
				t.Fatal("CheckPrerequisites() succeeded, want a refusal")
			}
			if strings.Contains(err.Error(), test.output) {
				t.Fatalf("error echoed untrusted command output: %q", err)
			}
		})
	}
}

// A wrapper that answers --version itself can dress a lone line as a version
// with trailing prose; genuine git output carries only a parenthesized vendor
// tag, so the run must halt rather than trust the invented line.
func TestCheckPrerequisitesRefusesALoneVersionLineCarryingFreeProse(t *testing.T) {
	t.Parallel()

	git := versionExecutable(t, "corporate wrapper v3\ngit version 9.9.9 upgrade recommended")

	if err := diagnostics.CheckPrerequisites(context.Background(), git); err == nil {
		t.Fatal("CheckPrerequisites() succeeded, want a refusal")
	}
}

// The wrappers version managers and corporate installs put in front of git fork
// helpers that inherit the probe's stdout, so killing the direct child on the
// deadline does not end the read. The probe is the first thing a run does.
func TestThePrerequisiteProbeReturnsWhileAForkedGrandchildHoldsItsStdoutOpen(t *testing.T) {
	tests := []struct {
		name    string
		script  string
		timeout time.Duration
	}{
		{
			name:    "the wrapper exits and leaves the grandchild on the pipe",
			script:  "#!/bin/sh\nsleep 30 &\nexit 0\n",
			timeout: 10 * time.Second,
		},
		{
			name:    "the wrapper outlives the deadline with the grandchild on the pipe",
			script:  "#!/bin/sh\nsleep 30 &\nsleep 30\n",
			timeout: 200 * time.Millisecond,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			git := filepath.Join(t.TempDir(), "git")
			writeExecutable(t, git, test.script)

			ctx, cancel := context.WithTimeout(context.Background(), test.timeout)
			defer cancel()

			returned := make(chan error, 1)
			go func() {
				returned <- diagnostics.CheckPrerequisites(ctx, git)
			}()

			select {
			case err := <-returned:
				if err == nil {
					t.Fatal("CheckPrerequisites() accepted a git that never reported a version")
				}
			case <-time.After(8 * time.Second):
				t.Fatal("CheckPrerequisites did not return while a grandchild held its stdout open")
			}
		})
	}
}
