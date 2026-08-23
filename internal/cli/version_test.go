package cli

import (
	"bytes"
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/lkshrk/ops-pilot/internal/run"
)

func versionOutput(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	stdout := &bytes.Buffer{}
	stderr := &bytes.Buffer{}
	deps := testDependencies(nil)
	deps.Stdin = strings.NewReader("")
	deps.Stdout = stdout
	deps.Stderr = stderr

	code := execute(
		context.Background(),
		append([]string{"version"}, args...),
		deps,
		VersionInfo{Version: "1.2.3", Commit: "abcdef0", BuildDate: "2026-01-01T00:00:00Z"},
		func(context.CancelCauseFunc) *signalController { return nil },
	)
	return stdout.String(), stderr.String(), code
}

func TestVersionHonoursTheGlobalVerbosityFlags(t *testing.T) {
	tests := []struct {
		name   string
		args   []string
		want   []string
		unwant []string
	}{
		{
			name:   "by default, the full build identity",
			want:   []string{"ops-pilot 1.2.3", "commit abcdef0", "built 2026-01-01T00:00:00Z"},
			unwant: []string{runtime.Version()},
		},
		{
			name:   "quiet prints the version and nothing else",
			args:   []string{"--quiet"},
			want:   []string{"ops-pilot 1.2.3"},
			unwant: []string{"commit", "built"},
		},
		{
			name: "verbose adds the toolchain and platform",
			args: []string{"--verbose"},
			want: []string{"ops-pilot 1.2.3", "commit abcdef0", runtime.Version(), runtime.GOOS + "/" + runtime.GOARCH},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stdout, _, code := versionOutput(t, test.args...)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0", code)
			}
			for _, want := range test.want {
				if !strings.Contains(stdout, want) {
					t.Fatalf("version printed %q, want it to contain %q", stdout, want)
				}
			}
			for _, unwant := range test.unwant {
				if strings.Contains(stdout, unwant) {
					t.Fatalf("version printed %q, want it to omit %q", stdout, unwant)
				}
			}
		})
	}
}

func TestVersionRefusesTheContradictoryVerbosityPair(t *testing.T) {
	stdout, stderr, code := versionOutput(t, "--quiet", "--verbose")

	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr, "mutually exclusive") {
		t.Fatalf("stderr = %q, want it to say the flags are mutually exclusive", stderr)
	}
	if strings.Contains(stdout, "1.2.3") {
		t.Fatalf("version printed %q after refusing its flags", stdout)
	}
}

func TestVersionNamesTheFieldsABuildDidNotInject(t *testing.T) {
	stdout := &bytes.Buffer{}
	if err := printVersion(stdout, VersionInfo{Version: "dev"}, run.VerbosityNormal); err != nil {
		t.Fatalf("printVersion: %v", err)
	}
	if !strings.Contains(stdout.String(), "commit unknown") ||
		!strings.Contains(stdout.String(), "built unknown") {
		t.Fatalf("version printed %q, want the missing fields named", stdout)
	}
}
