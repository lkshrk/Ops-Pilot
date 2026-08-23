package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lkshrk/ops-pilot/internal/composition"
	"github.com/lkshrk/ops-pilot/internal/config"
	"github.com/lkshrk/ops-pilot/internal/diagnostics"
	"github.com/lkshrk/ops-pilot/internal/display"
	"github.com/lkshrk/ops-pilot/internal/domain"
	"github.com/lkshrk/ops-pilot/internal/events"
	"github.com/lkshrk/ops-pilot/internal/run"
	"github.com/spf13/cobra"
)

// The stream quotes prose whose configured values carry no shape for
// ScrubSecrets to match, so the redactor is their only cover.
func TestTheRunCommandGivesItsEventStreamTheConfiguredSecrets(t *testing.T) {
	const token = "configured-token-value"

	original := openEventStream
	t.Cleanup(func() { openEventStream = original })
	var given *diagnostics.Redactor
	openEventStream = func(
		path string,
		now func() time.Time,
		redactor *diagnostics.Redactor,
	) (*events.Emitter, error) {
		given = redactor
		return original(path, now, redactor)
	}

	deps := testDependencies(map[string]string{"GITHUB_TOKEN": token})
	deps.DecodeConfig = func(config.LoadOptions) (config.Loaded, error) {
		return config.Loaded{
			Config: config.Config{GitHub: config.GitHubConfig{TokenEnv: "GITHUB_TOKEN"}},
		}, nil
	}
	deps.Stdin = strings.NewReader("")
	deps.Stdout = &bytes.Buffer{}
	deps.Stderr = &bytes.Buffer{}

	directory := t.TempDir()
	// The run cannot reach a cluster here, so it fails after the stream is open.
	execute(
		context.Background(),
		[]string{"run", "--events", filepath.Join(directory, "run.jsonl")},
		deps,
		VersionInfo{},
		func(context.CancelCauseFunc) *signalController { return nil },
	)

	if given == nil {
		t.Fatal("the run command opened its event stream with no redactor")
	}

	path := filepath.Join(directory, "emitted.jsonl")
	stream, err := original(path, nil, given)
	if err != nil {
		t.Fatalf("open the stream: %v", err)
	}
	stream.Emit(events.Event{
		Kind:   events.Halted,
		Reason: "the forge rejected " + token,
		Error:  "push failed for " + token,
	})
	if err := stream.Close(); err != nil {
		t.Fatalf("close the stream: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the stream: %v", err)
	}
	if line := string(raw); strings.Contains(line, token) {
		t.Fatalf("the configured value reached the stream: %s", line)
	} else if strings.Count(line, "[REDACTED]") != 2 {
		t.Fatalf("the stream redacted neither field: %s", line)
	}
}

// Knowing which redactor events.Open was handed proves nothing about which
// emitter the runner writes through. The stream is redirected to a path the
// flag never named, so a second emitter opened anywhere else in the wiring
// leaves its own file behind and is caught even before it emits.
func TestTheRunCommandPublishesThroughTheStreamItOpened(t *testing.T) {
	const token = "configured-token-value"

	directory := t.TempDir()
	flagPath := filepath.Join(directory, "flag.jsonl")
	openedPath := filepath.Join(directory, "opened.jsonl")

	original := openEventStream
	t.Cleanup(func() { openEventStream = original })
	var opened []*diagnostics.Redactor
	openEventStream = func(
		path string,
		now func() time.Time,
		redactor *diagnostics.Redactor,
	) (*events.Emitter, error) {
		opened = append(opened, redactor)
		if path != flagPath {
			t.Errorf("the stream was opened at %q, not the path the flag named", path)
		}
		return original(openedPath, now, redactor)
	}

	deps := testDependencies(map[string]string{"GITHUB_TOKEN": token})
	deps.DecodeConfig = func(config.LoadOptions) (config.Loaded, error) {
		return config.Loaded{
			Config: config.Config{GitHub: config.GitHubConfig{TokenEnv: "GITHUB_TOKEN"}},
		}, nil
	}
	deps.Stdin = strings.NewReader("")
	deps.Stdout = &bytes.Buffer{}
	deps.Stderr = &bytes.Buffer{}

	execute(
		context.Background(),
		[]string{"run", "--events", flagPath},
		deps,
		VersionInfo{},
		func(context.CancelCauseFunc) *signalController { return nil },
	)

	if len(opened) != 1 {
		t.Fatalf("the command opened %d event streams, want exactly 1", len(opened))
	}
	if opened[0] == nil {
		t.Fatal("the command opened its event stream with no redactor")
	}
	if _, err := os.Stat(openedPath); err != nil {
		t.Fatalf("the stream the command opened left no file: %v", err)
	}
	if _, err := os.Stat(flagPath); err == nil {
		t.Fatal("a second emitter was opened outside the stream the command returned")
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat the flag path: %v", err)
	}
}

// A file left at the flag path only rules out a second emitter opened there:
// events.Open creates its file eagerly, so a nil Events field and a shadow
// emitter at a path the flag never named both survive that check. Only the
// emitter the composition was actually handed says which stream the runner
// publishes through, so it is compared by identity to the one openEventStream
// returned.
func TestTheRunCommandHandsTheRunnerTheVeryStreamItOpened(t *testing.T) {
	const token = "configured-token-value"

	directory := t.TempDir()
	flagPath := filepath.Join(directory, "flag.jsonl")

	originalOpen := openEventStream
	t.Cleanup(func() { openEventStream = originalOpen })
	var returned []*events.Emitter
	openEventStream = func(
		path string,
		now func() time.Time,
		redactor *diagnostics.Redactor,
	) (*events.Emitter, error) {
		stream, err := originalOpen(path, now, redactor)
		if err == nil {
			returned = append(returned, stream)
		}
		return stream, err
	}

	originalNew := newComposition
	t.Cleanup(func() { newComposition = originalNew })
	var composed []composition.Dependencies
	var built int
	newComposition = func(
		ctx context.Context,
		loaded config.Loaded,
		deps composition.Dependencies,
		options run.Options,
		approver run.Approver,
	) (*composition.Handle, error) {
		built++
		composed = append(composed, deps)
		return originalNew(ctx, loaded, deps, options, approver)
	}

	deps := testDependencies(map[string]string{"GITHUB_TOKEN": token})
	deps.DecodeConfig = func(config.LoadOptions) (config.Loaded, error) {
		return config.Loaded{
			Config: config.Config{GitHub: config.GitHubConfig{TokenEnv: "GITHUB_TOKEN"}},
		}, nil
	}
	deps.Stdin = strings.NewReader("")
	deps.Stdout = &bytes.Buffer{}
	deps.Stderr = &bytes.Buffer{}

	execute(
		context.Background(),
		[]string{"run", "--events", flagPath},
		deps,
		VersionInfo{},
		func(context.CancelCauseFunc) *signalController { return nil },
	)

	if built != 1 {
		t.Fatalf("the command composed the runner %d times, want exactly 1", built)
	}
	if len(returned) != 1 {
		t.Fatalf("the command opened %d event streams, want exactly 1", len(returned))
	}
	if composed[0].Events == nil {
		t.Fatal("the runner was composed with no event stream at all")
	}
	if composed[0].Events != run.Events(returned[0]) {
		t.Fatalf("the runner publishes through %#v, not the stream the command opened", composed[0].Events)
	}
}

var errStopBeforeTheRunner = errors.New("stop before the runner")

// runApprover builds the run command with stdin as the dependency and input as
// the reader set on the command, then returns the approver it composed.
func runApprover(t *testing.T, stdin, input io.Reader, args ...string) run.Approver {
	t.Helper()
	originalNew := newComposition
	t.Cleanup(func() { newComposition = originalNew })
	var captured run.Approver
	newComposition = func(
		_ context.Context,
		_ config.Loaded,
		_ composition.Dependencies,
		_ run.Options,
		approver run.Approver,
	) (*composition.Handle, error) {
		captured = approver
		return nil, errStopBeforeTheRunner
	}

	deps := testDependencies(nil)
	deps.Stdin = stdin
	deps.Stdout = &bytes.Buffer{}
	deps.Stderr = &bytes.Buffer{}
	deps.CheckPrerequisites = func(context.Context, string) error { return nil }

	root, _ := newCommand(deps, VersionInfo{})
	root.SetIn(input)
	root.SetArgs(append([]string{"run"}, args...))
	if err := root.ExecuteContext(context.Background()); !errors.Is(err, errStopBeforeTheRunner) {
		t.Fatalf("want the run to stop at composition, got %v", err)
	}
	if captured == nil {
		t.Fatal("the run command composed no approver")
	}
	return captured
}

func pipeReader(t *testing.T) *os.File {
	t.Helper()
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatalf("open a pipe: %v", err)
	}
	t.Cleanup(func() { read.Close(); write.Close() })
	return read
}

func TestARunWithNoTerminalNeverPromisesToPromptTheOperator(t *testing.T) {
	tests := []struct {
		name  string
		stdin func(*testing.T) io.Reader
		args  []string
	}{
		{
			name:  "stdin is a pipe",
			stdin: func(t *testing.T) io.Reader { return pipeReader(t) },
		},
		{
			name:  "stdin is not a file at all",
			stdin: func(*testing.T) io.Reader { return strings.NewReader("y\n") },
		},
		{
			name:  "a pipe with --non-interactive",
			stdin: func(t *testing.T) io.Reader { return pipeReader(t) },
			args:  []string{"--non-interactive"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reader := test.stdin(t)
			if runApprover(t, reader, reader, test.args...).Interactive() {
				t.Fatal("a run with no terminal must not offer to prompt")
			}
		})
	}
}

func TestTheHistoryListingIsClippedToTheDestinationWidth(t *testing.T) {
	command := &cobra.Command{}
	var out bytes.Buffer
	command.SetOut(&out)
	runs := []domain.Run{{
		ID: "20260822-082304.078918000",
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

	for i, line := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
		if width := display.Width(line); width > display.DefaultWidth {
			t.Errorf("line %d is %d columns wide, want at most %d: %q",
				i+1, width, display.DefaultWidth, line)
		}
	}
}

func TestAnInterruptedRunPrintsOneLineInsteadOfTheSummary(t *testing.T) {
	result := domain.Run{
		ID: "20260822-222132.123048000",
		Attempts: []domain.Attempt{{
			PullRequest: 823,
			Dependency:  domain.Dependency{Name: "ghcr.io/goauthentik/server"},
			Verdict:     domain.VerdictError,
			Error:       "GitHub GET /repos/lkshrk/h-cloud/pulls/823/files?per_page=100 transport failure",
		}},
	}
	tests := []struct {
		name      string
		cause     error
		wantTable bool
	}{
		{name: "the operator interrupted the run", cause: ErrSignalInterrupt},
		{name: "the run finished on its own", wantTable: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancelCause(context.Background())
			t.Cleanup(func() { cancel(nil) })
			if test.cause != nil {
				cancel(test.cause)
			}

			var out bytes.Buffer
			if err := reportRun(ctx, &out, result, nil, true); err != nil {
				t.Fatalf("report: %v", err)
			}

			got := out.String()
			if table := strings.Contains(got, "RESULT"); table != test.wantTable {
				t.Fatalf("printed a table=%v, want %v:\n%s", table, test.wantTable, got)
			}
			if !test.wantTable && strings.Contains(got, "/repos/") {
				t.Fatalf("the interrupted run leaked a request path:\n%s", got)
			}
		})
	}
}

func TestARunRejectsAPullRequestNumberItCannotFilterOn(t *testing.T) {
	tests := []struct {
		name string
		flag string
	}{
		{name: "zero", flag: "--pr=0"},
		{name: "negative", flag: "--pr=-1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			original := newComposition
			t.Cleanup(func() { newComposition = original })
			composed := 0
			newComposition = func(
				context.Context,
				config.Loaded,
				composition.Dependencies,
				run.Options,
				run.Approver,
			) (*composition.Handle, error) {
				composed++
				return nil, errStopBeforeTheRunner
			}

			stderr := &bytes.Buffer{}
			deps := testDependencies(nil)
			deps.Stdin = strings.NewReader("")
			deps.Stdout = &bytes.Buffer{}
			deps.Stderr = stderr
			deps.CheckPrerequisites = func(context.Context, string) error { return nil }

			code := execute(
				context.Background(),
				[]string{"run", test.flag, "--dry-run", "--non-interactive"},
				deps,
				VersionInfo{},
				func(context.CancelCauseFunc) *signalController { return nil },
			)
			if code != 1 {
				t.Fatalf("exit code = %d, want 1", code)
			}
			if composed != 0 {
				t.Fatalf("the run composed a runner %d times on a --pr it cannot filter on", composed)
			}
			if !strings.Contains(stderr.String(), "--pr") {
				t.Fatalf("stderr = %q, want it to name --pr", stderr)
			}
		})
	}
}

func TestARunKeepsAPullRequestNumberItCanFilterOn(t *testing.T) {
	original := newComposition
	t.Cleanup(func() { newComposition = original })
	var options run.Options
	newComposition = func(
		_ context.Context,
		_ config.Loaded,
		_ composition.Dependencies,
		opts run.Options,
		_ run.Approver,
	) (*composition.Handle, error) {
		options = opts
		return nil, errStopBeforeTheRunner
	}

	deps := testDependencies(nil)
	deps.Stdin = strings.NewReader("")
	deps.Stdout = &bytes.Buffer{}
	deps.Stderr = &bytes.Buffer{}
	deps.CheckPrerequisites = func(context.Context, string) error { return nil }

	execute(
		context.Background(),
		[]string{"run", "--pr=822", "--dry-run", "--non-interactive"},
		deps,
		VersionInfo{},
		func(context.CancelCauseFunc) *signalController { return nil },
	)

	if options.OnlyPullRequest != 822 {
		t.Fatalf("OnlyPullRequest = %d, want 822", options.OnlyPullRequest)
	}
}

func TestAMissingCredentialExitsLikeEveryOtherStartupFailure(t *testing.T) {
	deps := testDependencies(nil)
	deps.DecodeConfig = func(config.LoadOptions) (config.Loaded, error) {
		return config.Loaded{
			Config: config.Config{
				GitHub: config.GitHubConfig{TokenEnv: "TEST_GITHUB_TOKEN"},
				AI:     config.AIConfig{APIKeyEnv: "TEST_AI_KEY"},
			},
		}, nil
	}
	stderr := &bytes.Buffer{}
	deps.Stdin = strings.NewReader("")
	deps.Stdout = &bytes.Buffer{}
	deps.Stderr = stderr
	deps.CheckPrerequisites = func(context.Context, string) error { return nil }

	code := execute(
		context.Background(),
		[]string{"run", "--dry-run", "--non-interactive"},
		deps,
		VersionInfo{},
		func(context.CancelCauseFunc) *signalController { return nil },
	)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "environment variable TEST_GITHUB_TOKEN is not set") {
		t.Fatalf("stderr = %q, want the sentence intact, not scrubbed", stderr)
	}
}

// A consumer scripting against --json expects a list either way; a nil slice
// serialises to null, forcing every caller to special-case an empty database.
func TestHistoryJSONEncodesAnEmptyDatabaseAsAnEmptyListNotNull(t *testing.T) {
	out := &bytes.Buffer{}
	if err := encodeHistoryJSON(out, nil); err != nil {
		t.Fatalf("encodeHistoryJSON: %v", err)
	}
	if got := strings.TrimSpace(out.String()); got != "[]" {
		t.Fatalf("history --json printed %q, want []", got)
	}
}

func TestHistoryWithNothingToShowSaysSoRatherThanPrintingABareHeader(t *testing.T) {
	tests := []struct {
		name string
		runs []domain.Run
		want string
	}{
		{
			name: "no runs at all",
			want: "No runs yet",
		},
		{
			name: "runs that reached no pull request",
			runs: []domain.Run{{ID: "20260823-101500.000000000"}},
			want: "No pull requests were processed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			out := &bytes.Buffer{}
			command := &cobra.Command{}
			command.SetOut(out)

			if err := renderHistory(command, test.runs); err != nil {
				t.Fatalf("renderHistory: %v", err)
			}
			if strings.Contains(out.String(), "RUN") {
				t.Fatalf("history printed a bare table header: %q", out)
			}
			if !strings.Contains(out.String(), test.want) {
				t.Fatalf("history printed %q, want it to contain %q", out, test.want)
			}
		})
	}
}

func TestHistoryStillRendersTheTableWhenThereIsSomethingToShow(t *testing.T) {
	out := &bytes.Buffer{}
	command := &cobra.Command{}
	command.SetOut(out)
	runs := []domain.Run{{
		ID: "20260823-101500.000000000",
		Attempts: []domain.Attempt{{
			PullRequest: 822,
			Dependency: domain.Dependency{
				Name:        "postgres",
				FromVersion: "18.4",
				ToVersion:   "18.6",
			},
			Verdict:  domain.VerdictMerged,
			Duration: 3 * time.Minute,
		}},
	}}

	if err := renderHistory(command, runs); err != nil {
		t.Fatalf("renderHistory: %v", err)
	}
	for _, want := range []string{"RUN", "#822", "postgres", "18.4 -> 18.6", "MERGED", "3m0s"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("history printed %q, want it to contain %q", out, want)
		}
	}
}
