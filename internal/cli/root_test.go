package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lkshrk/ops-pilot/internal/adapters/github"
	"github.com/lkshrk/ops-pilot/internal/composition"
	"github.com/lkshrk/ops-pilot/internal/config"
	"github.com/lkshrk/ops-pilot/internal/diagnostics"
	"github.com/lkshrk/ops-pilot/internal/domain"
	"github.com/lkshrk/ops-pilot/internal/run"
)

func recordingDependencies(
	t *testing.T,
	check func(context.Context, string) error,
) (CommandDependencies, *bytes.Buffer, *int) {
	t.Helper()

	calls := 0
	stderr := &bytes.Buffer{}
	deps := testDependencies(nil)
	deps.CheckPrerequisites = func(
		ctx context.Context,
		gitExecutable string,
	) error {
		calls++
		return check(ctx, gitExecutable)
	}
	deps.Stdin = strings.NewReader("")
	deps.Stdout = &bytes.Buffer{}
	deps.Stderr = stderr
	return deps, stderr, &calls
}

func failingCheck(err error) func(context.Context, string) error {
	return func(context.Context, string) error {
		return err
	}
}

// A missing git leaves the agent unable to read the manifests, and run.go only
// warns about that, so the whole run would merge on degraded evidence.
func TestRunHaltsBeforeMergingAnythingWhenGitIsMissing(t *testing.T) {
	deps, stderr, calls := recordingDependencies(
		t,
		failingCheck(errors.New(`Git 2.38 or newer is required; install it and ensure "git" is executable`)),
	)

	code := execute(
		context.Background(),
		[]string{"run"},
		deps,
		VersionInfo{},
		func(context.CancelCauseFunc) *signalController { return nil },
	)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if *calls != 1 {
		t.Fatalf("CheckPrerequisites called %d times, want 1", *calls)
	}
	if !strings.Contains(stderr.String(), "Git 2.38 or newer is required") {
		t.Fatalf("stderr = %q, want the git prerequisite message", stderr)
	}
}

// Every way git can fail the check has to stop the run before it merges, since
// a checkout the agent cannot read makes the whole run decide on no evidence.
func TestEveryGitPrerequisiteFailureHaltsTheRun(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantHalt bool
	}{
		{
			name:     "git not on PATH",
			err:      errors.New(`exec: "git": executable file not found in $PATH`),
			wantHalt: true,
		},
		{
			name:     "git version output unrecognized",
			err:      errors.New("unrecognized version output"),
			wantHalt: true,
		},
		{
			name:     "git older than the checkout requirement",
			err:      errors.New("Git 2.38 or newer is required (found 2.37.9)"),
			wantHalt: true,
		},
		{
			name:     "git one major behind",
			err:      errors.New("Git 2.38 or newer is required (found 1.99.0)"),
			wantHalt: true,
		},
		{
			name:     "git present and current",
			wantHalt: false,
		},
		{
			name:     "a prerequisite other than git failed on a machine whose git passes",
			err:      errors.New("kubectl 1.29 or newer is required"),
			wantHalt: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			check := func(context.Context, string) error {
				return test.err
			}
			err := gitPrerequisite(context.Background(), check)
			if test.wantHalt != (err != nil) {
				t.Fatalf("gitPrerequisite() = %v, want halt=%t", err, test.wantHalt)
			}
			if test.wantHalt && domain.ErrorClassOf(err) != domain.ErrorPrerequisite {
				t.Fatalf("error class = %q, want %q", domain.ErrorClassOf(err), domain.ErrorPrerequisite)
			}
		})
	}
}

// history opens a local database and version prints a string; neither shells out
// to git, so neither may be blocked by a machine that lacks it.
func TestHistoryAndVersionRunOnAMachineWithoutGit(t *testing.T) {
	for _, name := range []string{"history", "version"} {
		t.Run(name, func(t *testing.T) {
			deps, _, calls := recordingDependencies(t, failingCheck(errors.New("git is missing")))
			command, _ := newCommand(deps, VersionInfo{Version: "test"})
			command.SetArgs([]string{name})
			err := command.ExecuteContext(context.Background())
			if *calls != 0 {
				t.Fatalf("%s checked prerequisites %d times, want 0", name, *calls)
			}
			if domain.ErrorClassOf(err) == domain.ErrorPrerequisite {
				t.Fatalf("%s failed on a prerequisite it does not use: %v", name, err)
			}
		})
	}
}

// The boundary this walks is derived from the floor diagnostics exports rather
// than written out again here, and it walks a run of versions above the floor
// rather than one: drift is not always monotone, and a check that rejects one
// version in the middle of the supported range strands the run exactly as a
// raised floor would.
func TestTheRunGateAgreesWithTheRealPrerequisiteCheckAcrossTheSupportedGitRange(t *testing.T) {
	tests := []struct {
		name       string
		gitVersion string
		wantHalt   bool
	}{
		{
			name:       "below the floor",
			gitVersion: fmt.Sprintf("git version %d.%d.9", diagnostics.MinimumGitMajor, diagnostics.MinimumGitMinor-1),
			wantHalt:   true,
		},
		{
			name:       "a major below the floor",
			gitVersion: fmt.Sprintf("git version %d.99.0", diagnostics.MinimumGitMajor-1),
			wantHalt:   true,
		},
		{
			name:       "a major above the floor",
			gitVersion: fmt.Sprintf("git version %d.0.0", diagnostics.MinimumGitMajor+1),
			wantHalt:   false,
		},
	}
	for minor := diagnostics.MinimumGitMinor; minor <= diagnostics.MinimumGitMinor+12; minor++ {
		version := fmt.Sprintf("git version %d.%d.0", diagnostics.MinimumGitMajor, minor)
		tests = append(tests, struct {
			name       string
			gitVersion string
			wantHalt   bool
		}{name: version, gitVersion: version, wantHalt: false})
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			writeFakeExecutable(t, filepath.Join(directory, "git"), test.gitVersion)
			t.Setenv("PATH", directory)

			checkErr := diagnostics.CheckPrerequisites(context.Background(), "")
			err := gitPrerequisite(context.Background(), diagnostics.CheckPrerequisites)
			if (checkErr != nil) != (err != nil) {
				t.Fatalf(
					"gitPrerequisite() = %v while CheckPrerequisites() = %v on git %q: the two git floors have drifted apart",
					err, checkErr, test.gitVersion,
				)
			}
			if test.wantHalt != (err != nil) {
				t.Fatalf("gitPrerequisite() = %v, want halt=%t", err, test.wantHalt)
			}
		})
	}
}

// The gate used to keep its own opinion about which of the check's failures
// were fatal. These are every way the real check can fail; all of them halt,
// which is what makes deleting that opinion a no-op on today's inputs.
func TestTheRunGateHaltsOnEveryFailureTheRealPrerequisiteCheckCanReturn(t *testing.T) {
	var absentContext context.Context
	tests := []struct {
		name  string
		check prerequisiteCheck
	}{
		{
			name: "no context",
			check: func(context.Context, string) error {
				return diagnostics.CheckPrerequisites(absentContext, "")
			},
		},
		{
			name: "git will not execute",
			check: func(ctx context.Context, _ string) error {
				return diagnostics.CheckPrerequisites(ctx, filepath.Join(t.TempDir(), "absent-git"))
			},
		},
		{
			name:  "git returns unparseable version output",
			check: realCheckAgainstFakeGit(t, "not a version at all"),
		},
		{
			name: "git is below the floor",
			check: realCheckAgainstFakeGit(t, fmt.Sprintf(
				"git version %d.%d.9", diagnostics.MinimumGitMajor, diagnostics.MinimumGitMinor-1,
			)),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := gitPrerequisite(context.Background(), test.check)
			if domain.ErrorClassOf(err) != domain.ErrorPrerequisite {
				t.Fatalf("gitPrerequisite() = %v, want a prerequisite error", err)
			}
		})
	}
}

func realCheckAgainstFakeGit(t *testing.T, output string) prerequisiteCheck {
	t.Helper()

	git := filepath.Join(t.TempDir(), "git")
	writeFakeExecutable(t, git, output)
	return func(ctx context.Context, _ string) error {
		return diagnostics.CheckPrerequisites(ctx, git)
	}
}

// The probe spawns subprocesses before anything else happens, so an unattended
// run would stall at startup on a binary that never answers.
func TestThePrerequisiteProbeRunsUnderADeadline(t *testing.T) {
	var deadline time.Time
	check := func(ctx context.Context, _ string) error {
		deadline, _ = ctx.Deadline()
		return nil
	}
	if err := gitPrerequisite(context.Background(), check); err != nil {
		t.Fatalf("gitPrerequisite() = %v", err)
	}
	if deadline.IsZero() {
		t.Fatal("prerequisite probe ran without a deadline")
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > time.Minute {
		t.Fatalf("probe deadline is %s away, want a short positive bound", remaining)
	}
}

// A git wrapper that forks keeps the probe's stdout pipe open after the kill,
// so the check can outlive the deadline it was handed. The gate is the first
// thing an unattended run does, and it must stop rather than wait forever.
func TestTheRunGateHaltsRatherThanWaitingOnAProbeThatOutlivesItsDeadline(t *testing.T) {
	tests := []struct {
		name    string
		context func() (context.Context, context.CancelFunc)
	}{
		{
			name: "deadline already passed",
			context: func() (context.Context, context.CancelFunc) {
				return context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
			},
		},
		{
			name: "operator interrupted before the probe answered",
			context: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancelCause(context.Background())
				cancel(ErrSignalInterrupt)
				return ctx, func() { cancel(nil) }
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			released := make(chan struct{})
			t.Cleanup(func() { close(released) })
			check := func(context.Context, string) error {
				<-released
				return nil
			}

			ctx, cancel := test.context()
			defer cancel()

			returned := make(chan error, 1)
			go func() { returned <- gitPrerequisite(ctx, check) }()

			select {
			case err := <-returned:
				if domain.ErrorClassOf(err) != domain.ErrorPrerequisite {
					t.Fatalf("gitPrerequisite() = %v, want a prerequisite error", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("gitPrerequisite waited on a probe that had already outlived its deadline")
			}
		})
	}
}

// The deadline that matters fires while the probe is still running, not before
// it starts: a gate that only pre-checks the context and then calls the probe
// in line is still a gate an unattended run can hang on forever.
func TestTheRunGateHaltsWhenItsDeadlineFiresMidProbe(t *testing.T) {
	released := make(chan struct{})
	t.Cleanup(func() { close(released) })
	entered := make(chan struct{})
	check := func(context.Context, string) error {
		close(entered)
		<-released
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	returned := make(chan error, 1)
	go func() { returned <- gitPrerequisite(ctx, check) }()

	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("gitPrerequisite never started the probe")
	}

	select {
	case err := <-returned:
		if domain.ErrorClassOf(err) != domain.ErrorPrerequisite {
			t.Fatalf("gitPrerequisite() = %v, want a prerequisite error", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("gitPrerequisite waited on a probe that was still running when the deadline fired")
	}
}

// select picks at random when the probe answers as the deadline fires, so half
// of those runs used to halt a machine whose git had just been proved. The
// answer wins over the timeout this function set for itself, and never over a
// caller that has said stop.
func TestAProbeThatAnsweredAtTheDeadlineOutranksTheDeadline(t *testing.T) {
	expired := func() context.Context {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		t.Cleanup(cancel)
		return ctx
	}
	probeError := errors.New("git is missing")
	tests := []struct {
		name      string
		ctx       context.Context
		probeCtx  context.Context
		answer    []error
		wantHalt  bool
		wantCause error
	}{
		{
			name:     "the probe proved git as the deadline fired",
			ctx:      context.Background(),
			probeCtx: expired(),
			answer:   []error{nil},
			wantHalt: false,
		},
		{
			name:      "the probe had not answered when the deadline fired",
			ctx:       context.Background(),
			probeCtx:  expired(),
			wantHalt:  true,
			wantCause: context.DeadlineExceeded,
		},
		{
			name:      "the probe answered with a failure as the deadline fired",
			ctx:       context.Background(),
			probeCtx:  expired(),
			answer:    []error{probeError},
			wantHalt:  true,
			wantCause: probeError,
		},
		{
			name:      "the caller stopped while the probe was proving git",
			ctx:       expired(),
			probeCtx:  expired(),
			answer:    []error{nil},
			wantHalt:  true,
			wantCause: context.DeadlineExceeded,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			probed := make(chan error, 1)
			for _, answer := range test.answer {
				probed <- answer
			}

			err := deadlineVerdict(test.ctx, test.probeCtx, probed)
			if test.wantHalt != (err != nil) {
				t.Fatalf("deadlineVerdict() = %v, want halt=%t", err, test.wantHalt)
			}
			if test.wantHalt && !errors.Is(err, test.wantCause) {
				t.Fatalf("deadlineVerdict() = %v, want cause %v", err, test.wantCause)
			}
		})
	}
}

// The exit code is the whole contract with whatever runs ops-pilot unattended:
// 1 means it never got as far as touching the repository, 2 means it did and
// something went wrong afterwards. The switch names every class that is actually
// produced, so a class it does not name falls through to the commandRan default -
// and a class produced before the command ran would exit 1 where a named one
// exits 2. Anything added to domain.ErrorClass belongs in this table and in the
// switch, or it changes an exit code silently.
func TestEveryErrorClassExitsOnItsOwnCode(t *testing.T) {
	classified := func(class domain.ErrorClass) error {
		return &domain.Error{Class: class, Operation: "probe", Cause: errors.New("boom")}
	}
	// Every class is asked under both commandRan values, because a class the
	// switch stops naming still answers 1 while the command has not run and only
	// diverges afterwards - so a single-column table would pass through the very
	// edit it exists to catch.
	classes := []struct {
		name  string
		class domain.ErrorClass
		want  int
	}{
		{name: "usage", class: domain.ErrorUsage, want: 1},
		{name: "configuration", class: domain.ErrorConfiguration, want: 1},
		{name: "prerequisite", class: domain.ErrorPrerequisite, want: 1},
		{name: "system", class: domain.ErrorSystem, want: 2},
	}

	for _, class := range classes {
		for _, commandRan := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s with commandRan=%t", class.name, commandRan), func(t *testing.T) {
				state := &executionState{commandRan: commandRan}
				if got := state.exitCode(classified(class.class), nil); got != class.want {
					t.Fatalf("exitCode() = %d, want %d", got, class.want)
				}
			})
		}
	}

	// A class nobody wired into the switch, and no class at all, share the
	// fallback: what the exit code says is only whether the command got as far as
	// running. That is what a new class silently inherits.
	tests := []struct {
		name       string
		err        error
		cause      error
		commandRan bool
		want       int
	}{
		{name: "no error", want: 0},
		{name: "unclassified before the command ran", err: errors.New("boom"), want: 1},
		{name: "unclassified once the command ran", err: errors.New("boom"), commandRan: true, want: 2},
		{name: "an unnamed class before the command ran", err: classified("invented"), want: 1},
		{name: "an unnamed class once the command ran", err: classified("invented"), commandRan: true, want: 2},
		{name: "interrupted", err: ErrSignalInterrupt, cause: ErrSignalInterrupt, want: 130},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := &executionState{commandRan: test.commandRan}
			if got := state.exitCode(test.err, test.cause); got != test.want {
				t.Fatalf("exitCode() = %d, want %d", got, test.want)
			}
		})
	}
}

// cancelledForge delivers the interrupt itself, from inside the call the
// runner is blocked on, rather than before the command even starts - the
// gitPrerequisite gate races an already-cancelled context against its own
// probe, so cancelling any earlier makes the test flake on that gate instead
// of exercising the path it means to cover.
type cancelledForge struct{ cancel context.CancelCauseFunc }

func (f cancelledForge) ListOpen(ctx context.Context, _ domain.PullRequestFilter) ([]domain.PullRequest, error) {
	f.cancel(ErrSignalInterrupt)
	<-ctx.Done()
	return nil, ctx.Err()
}
func (f cancelledForge) Get(ctx context.Context, _ int) (domain.PullRequest, error) {
	f.cancel(ErrSignalInterrupt)
	<-ctx.Done()
	return domain.PullRequest{}, ctx.Err()
}
func (cancelledForge) ChangedFiles(context.Context, int) ([]domain.FileDelta, error) { return nil, nil }
func (cancelledForge) FileAt(context.Context, string, string) ([]byte, bool, error) {
	return nil, false, nil
}
func (cancelledForge) Merge(context.Context, int, string, string) (string, error) { return "", nil }
func (cancelledForge) MergeState(context.Context, int) (domain.MergeState, error) {
	return domain.MergeState{}, nil
}
func (cancelledForge) Comment(context.Context, int, string) error     { return nil }
func (cancelledForge) AddLabel(context.Context, int, ...string) error { return nil }
func (cancelledForge) Close(context.Context, int) error               { return nil }
func (cancelledForge) Branch(context.Context) (string, error)         { return "", nil }
func (cancelledForge) BranchHead(context.Context, string) (string, error) {
	return "", nil
}
func (cancelledForge) CreateCommit(
	context.Context, string, string, string, []github.FileChange,
) (string, error) {
	return "", nil
}

// The cause commandCtx carries has to survive prepare, composition, and the
// runner untouched for reportRun to see it, and the exit code has to come from
// the same cause rather than from whatever error the runner happened to
// return - this is the only test that drives all of that through execute.
func TestADeliveredInterruptReportsItselfAndExitsOneThirty(t *testing.T) {
	deps := testDependencies(map[string]string{"GITHUB_TOKEN": "token", "OPENAI_API_KEY": "key"})
	deps.DecodeConfig = func(config.LoadOptions) (config.Loaded, error) {
		return config.Loaded{}, nil
	}
	deps.CheckPrerequisites = func(context.Context, string) error { return nil }
	var out, errOut bytes.Buffer
	deps.Stdin = strings.NewReader("")
	deps.Stdout = &out
	deps.Stderr = &errOut

	var deliverInterrupt context.CancelCauseFunc

	originalNew := newComposition
	t.Cleanup(func() { newComposition = originalNew })
	newComposition = func(
		_ context.Context,
		_ config.Loaded,
		compositionDeps composition.Dependencies,
		options run.Options,
		approver run.Approver,
	) (*composition.Handle, error) {
		return &composition.Handle{
			Runner: run.New(
				run.Dependencies{Forge: cancelledForge{cancel: deliverInterrupt}, Out: compositionDeps.Out},
				options,
			),
		}, nil
	}

	code := execute(
		context.Background(),
		[]string{"run"},
		deps,
		VersionInfo{},
		func(cancel context.CancelCauseFunc) *signalController {
			deliverInterrupt = cancel
			return nil
		},
	)

	if code != 130 {
		t.Fatalf("exit code = %d, want 130", code)
	}
	if !strings.Contains(out.String(), "! Interrupted.") {
		t.Fatalf("stdout = %q, stderr = %q, want stdout to report the interrupt", out.String(), errOut.String())
	}
}

func writeFakeExecutable(t *testing.T, path, output string) {
	t.Helper()

	script := "#!/bin/sh\nprintf '%s\\n' '" + output + "'\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write executable: %v", err)
	}
}

func TestAnUnknownFlagPointsAtTheCommandsOwnHelp(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "on a subcommand", args: []string{"run", "--nope"}, want: `"ops-pilot run --help"`},
		{name: "on the root command", args: []string{"--nope"}, want: `"ops-pilot --help"`},
		{name: "on history", args: []string{"history", "--nope"}, want: `"ops-pilot history --help"`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			deps, stderr, _ := recordingDependencies(t, func(context.Context, string) error { return nil })

			code := execute(
				context.Background(),
				test.args,
				deps,
				VersionInfo{},
				func(context.CancelCauseFunc) *signalController { return nil },
			)
			if code != 1 {
				t.Fatalf("exit code = %d, want 1", code)
			}
			if !strings.Contains(stderr.String(), "unknown flag: --nope") {
				t.Fatalf("stderr = %q, want it to name the rejected flag", stderr)
			}
			if !strings.Contains(stderr.String(), test.want) {
				t.Fatalf("stderr = %q, want it to point at %s", stderr, test.want)
			}
		})
	}
}

func TestTheUnknownFlagHintSurvivesErrorRendering(t *testing.T) {
	deps, stderr, _ := recordingDependencies(t, func(context.Context, string) error { return nil })

	execute(
		context.Background(),
		[]string{"run", "--nope"},
		deps,
		VersionInfo{},
		func(context.CancelCauseFunc) *signalController { return nil },
	)

	lines := strings.Split(strings.TrimSpace(stderr.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("stderr spans %d lines, want the whole error on one: %q", len(lines), stderr)
	}
	if !strings.HasSuffix(lines[0], `run "ops-pilot run --help" to see the available flags`) {
		t.Fatalf("stderr = %q, want it to end with the help hint", stderr)
	}
}

func TestAnInvalidNumericFlagDropsTheStrconvDetail(t *testing.T) {
	deps, stderr, _ := recordingDependencies(t, func(context.Context, string) error { return nil })

	code := execute(
		context.Background(),
		[]string{"run", "--pr", "abc"},
		deps,
		VersionInfo{},
		func(context.CancelCauseFunc) *signalController { return nil },
	)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), `invalid argument "abc" for "--pr" flag; run "ops-pilot run --help"`) {
		t.Fatalf("stderr = %q, want the strconv detail dropped", stderr)
	}
	if strings.Contains(stderr.String(), "strconv.") {
		t.Fatalf("stderr = %q, want no strconv detail", stderr)
	}
}

func TestAnUnknownCommandPointsAtTheRootHelp(t *testing.T) {
	deps, stderr, _ := recordingDependencies(t, func(context.Context, string) error { return nil })

	code := execute(
		context.Background(),
		[]string{"bogus"},
		deps,
		VersionInfo{},
		func(context.CancelCauseFunc) *signalController { return nil },
	)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	const want = `unknown command "bogus" for "ops-pilot"; run "ops-pilot --help" to see the available commands`
	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr = %q, want it to contain %q", stderr, want)
	}
}

// Cobra's own suggestion block is multi-line and tab-indented; rendered on the
// single line ops-pilot prints errors on, that reads as noise, not a hint.
func TestAnUnknownCommandNearAMatchReadsOnOneLine(t *testing.T) {
	deps, stderr, _ := recordingDependencies(t, func(context.Context, string) error { return nil })

	code := execute(
		context.Background(),
		[]string{"histroy"},
		deps,
		VersionInfo{},
		func(context.CancelCauseFunc) *signalController { return nil },
	)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	const want = `unknown command "histroy" for "ops-pilot" (did you mean "history"?); run "ops-pilot --help" to see the available commands`
	if !strings.Contains(stderr.String(), want) {
		t.Fatalf("stderr = %q, want it to contain %q", stderr, want)
	}
	if strings.Contains(stderr.String(), "Did you mean") {
		t.Fatalf("stderr = %q, want cobra's own suggestion block reformatted away", stderr)
	}
	if lines := strings.Split(strings.TrimSpace(stderr.String()), "\n"); len(lines) != 1 {
		t.Fatalf("stderr spans %d lines, want the whole error on one: %q", len(lines), stderr)
	}
}

// The flag conflict is a usage mistake the operator can see without touching
// disk, so it should not wait behind a configuration error that never runs.
func TestTheVerbosityConflictIsReportedBeforeAConfigurationError(t *testing.T) {
	stderr := &bytes.Buffer{}
	deps := CommandDependencies{
		Stdin:              strings.NewReader(""),
		Stdout:             &bytes.Buffer{},
		Stderr:             stderr,
		CheckPrerequisites: func(context.Context, string) error { return nil },
	}

	code := execute(
		context.Background(),
		[]string{"run", "--quiet", "--verbose", "--config", "/nonexistent"},
		deps,
		VersionInfo{},
		func(context.CancelCauseFunc) *signalController { return nil },
	)
	if code != 1 {
		t.Fatalf("exit code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "mutually exclusive") {
		t.Fatalf("stderr = %q, want the flag conflict reported", stderr)
	}
	if strings.Contains(stderr.String(), "configuration") {
		t.Fatalf("stderr = %q, want the configuration error never reached", stderr)
	}
}

func TestEveryCommandsHelpCarriesAWorkedExample(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "root", args: []string{"--help"}, want: "ops-pilot run --dry-run"},
		{name: "run", args: []string{"run", "--help"}, want: "ops-pilot run --pr 822"},
		{name: "history", args: []string{"history", "--help"}, want: "ops-pilot history --last 20"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			deps, _, _ := recordingDependencies(t, func(context.Context, string) error { return nil })
			stdout := &bytes.Buffer{}
			deps.Stdout = stdout

			code := execute(
				context.Background(),
				test.args,
				deps,
				VersionInfo{},
				func(context.CancelCauseFunc) *signalController { return nil },
			)
			if code != 0 {
				t.Fatalf("exit code = %d, want 0", code)
			}
			if !strings.Contains(stdout.String(), "Examples:") {
				t.Fatalf("help has no Examples block:\n%s", stdout)
			}
			if !strings.Contains(stdout.String(), test.want) {
				t.Fatalf("help does not show %q:\n%s", test.want, stdout)
			}
		})
	}
}
