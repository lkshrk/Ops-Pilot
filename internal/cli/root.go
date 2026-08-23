package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/lkshrk/ops-pilot/internal/config"
	"github.com/lkshrk/ops-pilot/internal/diagnostics"
	"github.com/lkshrk/ops-pilot/internal/domain"
	"github.com/lkshrk/ops-pilot/internal/run"
	"github.com/spf13/cobra"
)

var ErrSignalInterrupt = errors.New("CLI signal interrupt")

// strconvDetail matches the low-level clause pflag appends to a numeric flag
// error, which names a stdlib function an operator has no reason to know.
var strconvDetail = regexp.MustCompile(`: strconv\.\w+: parsing ".*": .+$`)

func dropStrconvDetail(err error) string {
	return strconvDetail.ReplaceAllString(err.Error(), "")
}

var (
	unknownCommandLine = regexp.MustCompile(`^unknown command (".+" for ".+")`)
	suggestedCommand   = regexp.MustCompile(`(?m)^\t(.+)$`)
)

// classifyUnknownCommand gives an unrecognised command the same usage hint an
// unrecognised flag gets. Cobra reports it as a bare error rather than routing
// it through SetFlagErrorFunc, since the root command has no Run of its own
// for ValidateArgs to run against. Its own near-miss suggestion is a multi-line,
// tab-indented block, which the single-line renderer would otherwise flatten
// into a run of doubled spaces, so it is reformatted onto the one line here.
func classifyUnknownCommand(err error, root *cobra.Command) error {
	if err == nil {
		return err
	}
	match := unknownCommandLine.FindStringSubmatch(err.Error())
	if match == nil {
		return err
	}
	message := "unknown command " + match[1]
	if suggestion := suggestNames(err.Error()); suggestion != "" {
		message += " (did you mean " + suggestion + "?)"
	}
	return &domain.Error{
		Class:     domain.ErrorUsage,
		Operation: "parse command",
		Cause: fmt.Errorf(
			"%s; run %q to see the available commands", message, root.CommandPath()+" --help",
		),
	}
}

func suggestNames(message string) string {
	names := suggestedCommand.FindAllStringSubmatch(message, -1)
	if len(names) == 0 {
		return ""
	}
	quoted := make([]string, len(names))
	for i, name := range names {
		quoted[i] = fmt.Sprintf("%q", name[1])
	}
	switch len(quoted) {
	case 1:
		return quoted[0]
	case 2:
		return quoted[0] + " or " + quoted[1]
	default:
		return strings.Join(quoted[:len(quoted)-1], ", ") + ", or " + quoted[len(quoted)-1]
	}
}

type VersionInfo struct {
	Version   string
	Commit    string
	BuildDate string
}

type CommandDependencies struct {
	DecodeConfig         func(config.LoadOptions) (config.Loaded, error)
	ApplyConfigDefaults  func(*config.Loaded)
	ApplyConfigOverrides func(*config.Loaded, config.Overrides)
	ValidateConfig       func(config.Loaded, config.ValidationContext) error
	LookupEnv            func(string) (string, bool)
	LoadDotenv           func(string) (map[string]string, error)
	CheckPrerequisites   prerequisiteCheck
	Stdin                io.Reader
	Stdout               io.Writer
	Stderr               io.Writer
}

func newCommand(deps CommandDependencies, version VersionInfo) (*cobra.Command, *executionState) {
	deps = defaultCommandDependencies(deps)
	state := &executionState{}
	root := &cobra.Command{
		Use:   "ops-pilot",
		Short: "Safely merge and verify dependency update pull requests",
		Example: `  # decide everything, change nothing
  ops-pilot run --dry-run

  # process one pull request
  ops-pilot run --pr 822

  # what the last runs did
  ops-pilot history --last 20`,
		SilenceErrors: true,
		SilenceUsage:  true,
	}
	root.CompletionOptions.DisableDefaultCmd = true
	root.SetFlagErrorFunc(func(command *cobra.Command, err error) error {
		return &domain.Error{
			Class:     domain.ErrorUsage,
			Operation: "parse flags",
			Cause: fmt.Errorf(
				"%s; run %q to see the available flags", dropStrconvDetail(err), command.CommandPath()+" --help",
			),
		}
	})
	root.PersistentPreRunE = func(*cobra.Command, []string) error {
		_, err := state.loggingOverride()
		return err
	}
	root.PersistentFlags().BoolVar(&state.verbose, "verbose", false,
		"explain each decision and show the agent's working")
	root.PersistentFlags().BoolVar(&state.quiet, "quiet", false,
		"print only the final summary")
	root.SetIn(deps.Stdin)
	root.SetOut(deps.Stdout)
	root.SetErr(deps.Stderr)
	root.AddCommand(
		requireGit(newRunCommand(deps, state), deps.CheckPrerequisites),
		newHistoryCommand(deps, state),
		newVersionCommand(version, state),
	)
	return root, state
}

type prerequisiteCheck func(context.Context, string) error

const prerequisiteTimeout = 15 * time.Second

// requireGit gates the commands that shell out to git. history reads a local
// database and version prints a string, so neither may be held up by it.
func requireGit(command *cobra.Command, check prerequisiteCheck) *cobra.Command {
	command.PreRunE = func(cmd *cobra.Command, _ []string) error {
		return gitPrerequisite(cmd.Context(), check)
	}
	return command
}

// The probe runs off to the side because cancelling its context does not make
// it return: it shells out, and a wrapper that forks holds the pipe open.
func gitPrerequisite(ctx context.Context, check prerequisiteCheck) error {
	probeCtx, cancel := context.WithTimeout(ctx, prerequisiteTimeout)
	defer cancel()

	probed := make(chan error, 1)
	go func() {
		probed <- check(probeCtx, "")
	}()

	select {
	case err := <-probed:
		if err == nil {
			return nil
		}
		return &domain.Error{
			Class:     domain.ErrorPrerequisite,
			Operation: "check prerequisites",
			Cause:     err,
		}
	case <-probeCtx.Done():
		return deadlineVerdict(ctx, probeCtx, probed)
	}
}

// The read must stay non-blocking, and a caller that has said stop outranks a
// probe that proved git: only this function's own deadline may be overruled.
func deadlineVerdict(ctx, probeCtx context.Context, probed <-chan error) error {
	cause := context.Cause(probeCtx)
	if ctx.Err() == nil && errors.Is(cause, context.DeadlineExceeded) {
		select {
		case err := <-probed:
			if err == nil {
				return nil
			}
			cause = err
		default:
		}
	}
	return &domain.Error{
		Class:     domain.ErrorPrerequisite,
		Operation: "check prerequisites",
		Cause:     cause,
	}
}

func Execute(
	ctx context.Context,
	args []string,
	deps CommandDependencies,
	version VersionInfo,
) int {
	return execute(ctx, args, deps, version, newSignalController)
}

func execute(
	ctx context.Context,
	args []string,
	deps CommandDependencies,
	version VersionInfo,
	startSignals func(context.CancelCauseFunc) *signalController,
) int {
	deps = defaultCommandDependencies(deps)
	commandCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	signals := startSignals(cancel)
	defer signals.Stop()

	command, state := newCommand(deps, version)
	command.SetArgs(append([]string(nil), args...))
	err := command.ExecuteContext(commandCtx)
	err = classifyUnknownCommand(err, command)
	signals.Stop()

	if errors.Is(context.Cause(commandCtx), ErrSignalInterrupt) &&
		(err == nil || errors.Is(err, context.Canceled)) {
		err = ErrSignalInterrupt
	}
	if err != nil {
		_, _ = fmt.Fprintln(
			deps.Stderr,
			"ops-pilot: error:", diagnostics.RenderError(err, state.redactor),
		)
	}
	return state.exitCode(err, context.Cause(commandCtx))
}

type executionState struct {
	logger     *diagnostics.Logger
	redactor   *diagnostics.Redactor
	verbose    bool
	quiet      bool
	commandRan bool
}

// verbosity is how much of the narrative the operator asked for, which is a
// separate question from how much diagnostic detail to log.
func (s *executionState) verbosity() run.Verbosity {
	switch {
	case s.quiet:
		return run.VerbosityQuiet
	case s.verbose:
		return run.VerbosityVerbose
	default:
		return run.VerbosityNormal
	}
}

func (s *executionState) loggingOverride() (*string, error) {
	switch {
	case s.verbose && s.quiet:
		return nil, &domain.Error{
			Class:     domain.ErrorUsage,
			Operation: "select log level",
			Cause:     errors.New("--verbose and --quiet are mutually exclusive"),
		}
	case s.verbose:
		level := diagnostics.LevelDebug.String()
		return &level, nil
	case s.quiet:
		level := diagnostics.LevelWarn.String()
		return &level, nil
	}
	return nil, nil
}

func (s *executionState) exitCode(err, cause error) int {
	if errors.Is(cause, ErrSignalInterrupt) {
		return 130
	}
	switch domain.ErrorClassOf(err) {
	case domain.ErrorUsage, domain.ErrorConfiguration, domain.ErrorPrerequisite:
		return 1
	case domain.ErrorSystem:
		return 2
	}
	if err == nil {
		return 0
	}
	if s.commandRan {
		return 2
	}
	return 1
}

func defaultCommandDependencies(deps CommandDependencies) CommandDependencies {
	if deps.DecodeConfig == nil {
		deps.DecodeConfig = config.Decode
	}
	if deps.ApplyConfigDefaults == nil {
		deps.ApplyConfigDefaults = config.ApplyDefaults
	}
	if deps.ApplyConfigOverrides == nil {
		deps.ApplyConfigOverrides = config.ApplyOverrides
	}
	if deps.ValidateConfig == nil {
		deps.ValidateConfig = config.Validate
	}
	if deps.LookupEnv == nil {
		deps.LookupEnv = os.LookupEnv
	}
	if deps.LoadDotenv == nil {
		deps.LoadDotenv = loadDotenv
	}
	if deps.CheckPrerequisites == nil {
		deps.CheckPrerequisites = diagnostics.CheckPrerequisites
	}
	if deps.Stdin == nil {
		deps.Stdin = os.Stdin
	}
	if deps.Stdout == nil {
		deps.Stdout = os.Stdout
	}
	if deps.Stderr == nil {
		deps.Stderr = os.Stderr
	}
	return deps
}

type signalController struct {
	cancel context.CancelCauseFunc
	stderr io.Writer
	exit   func(int)

	interrupts chan os.Signal
	done       chan struct{}
	stopped    chan struct{}
	stopOnce   sync.Once
}

func newSignalController(cancel context.CancelCauseFunc) *signalController {
	return newSignalControllerWith(cancel, os.Stderr, os.Exit)
}

func newSignalControllerWith(
	cancel context.CancelCauseFunc,
	stderr io.Writer,
	exit func(int),
) *signalController {
	controller := &signalController{
		cancel:     cancel,
		stderr:     stderr,
		exit:       exit,
		interrupts: make(chan os.Signal, 2),
		done:       make(chan struct{}),
		stopped:    make(chan struct{}),
	}
	signal.Notify(controller.interrupts, os.Interrupt)
	go controller.watch()
	return controller
}

// watch cancels on the first interrupt and forces the exit on the second. A
// prompt blocked reading stdin never observes a cancelled context, so without
// the second signal the program would appear to hang exactly when the operator
// most wants out.
func (s *signalController) watch() {
	defer close(s.stopped)
	interrupted := false
	for {
		select {
		case <-s.done:
			return
		case <-s.interrupts:
			if interrupted {
				fmt.Fprintln(s.stderr, "\nops-pilot: forced exit")
				s.exit(130)
				return
			}
			interrupted = true
			fmt.Fprintln(s.stderr, "\nops-pilot: interrupt received, stopping after the current step (press Ctrl+C again to force quit)")
			s.cancel(ErrSignalInterrupt)
		}
	}
}

func (s *signalController) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		signal.Stop(s.interrupts)
		close(s.done)
		<-s.stopped
	})
}
