package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/lkshrk/ops-pilot/internal/composition"
	"github.com/lkshrk/ops-pilot/internal/config"
	"github.com/lkshrk/ops-pilot/internal/domain"
	"github.com/lkshrk/ops-pilot/internal/events"
	"github.com/lkshrk/ops-pilot/internal/history"
	"github.com/lkshrk/ops-pilot/internal/report"
	"github.com/lkshrk/ops-pilot/internal/run"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// Indirect so a test can see which redactor this command hands to the stream,
// and which emitter it then hands to the runner.
var (
	openEventStream = events.Open
	newComposition  = composition.New
)

func newRunCommand(deps CommandDependencies, state *executionState) *cobra.Command {
	var (
		configPath     string
		repository     string
		pullRequest    int
		nonInteractive bool
		dryRun         bool
		all            bool
		eventsPath     string
	)
	command := &cobra.Command{
		Use:   "run",
		Short: "Process open dependency update pull requests",
		Example: `  # decide everything, change nothing
  ops-pilot run --dry-run

  # process one pull request, asking you before anything risky
  ops-pilot run --pr 822

  # unattended: skip whatever would need an answer
  ops-pilot run --non-interactive

  # reconsider what an earlier run reverted or you declined
  ops-pilot run --all --config /etc/ops-pilot/ops-pilot.yaml

  # keep a machine-readable record of every decision
  ops-pilot run --events ./run.jsonl`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := checkPullRequestFlag(cmd, pullRequest); err != nil {
				return err
			}
			state.commandRan = true
			override, err := repositoryOverride(repository)
			if err != nil {
				return err
			}
			loaded, err := prepare(&deps, state, config.CommandRun, configPath, config.Overrides{Repository: override})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			stream, err := openEventStream(eventsPath, nil, state.redactor)
			if err != nil {
				return err
			}
			defer func() { _ = stream.Close() }()
			in := cmd.InOrStdin()
			approver := NewApprover(in, out, !nonInteractive && interactiveTerminal(in), state.redactor)
			runner := &activityReporter{}
			handle, err := newComposition(
				cmd.Context(),
				loaded,
				composition.Dependencies{
					LookupEnv: deps.LookupEnv,
					Out:       out,
					Log:       state.logger,
					Redactor:  state.redactor,
					Events:    stream,
					Activity:  runner.report,
				},
				run.Options{
					OnlyPullRequest: pullRequest,
					DryRun:          dryRun,
					All:             all,
					Verbosity:       state.verbosity(),
				},
				approver,
			)
			if err != nil {
				return err
			}
			defer func() { _ = handle.Close() }()

			runner.runner = handle.Runner
			result, runErr := handle.Runner.Run(cmd.Context())
			if err := stream.Err(); err != nil {
				state.logger.Warnf("event stream stopped early: %v", err)
			}
			if err := reportRun(cmd.Context(), out, result,
				runErr, state.verbosity() >= run.VerbosityNormal); err != nil {
				return err
			}
			return runErr
		},
	}
	command.Flags().StringVar(&configPath, "config", "", "path to the configuration file")
	command.Flags().StringVar(&repository, "repo", "", "repository to process as owner/name")
	command.Flags().IntVar(&pullRequest, "pr", 0, "process only this pull request")
	command.Flags().BoolVar(
		&nonInteractive,
		"non-interactive",
		false,
		"skip pull requests that need approval instead of prompting",
	)
	command.Flags().BoolVar(&dryRun, "dry-run", false, "decide but make no external change")
	command.Flags().StringVar(&eventsPath, "events", "",
		"append a JSON record of every decision and change to this file")
	command.Flags().BoolVar(
		&all,
		"all",
		false,
		"reconsider pull requests an earlier run reverted or you declined",
	)
	return command
}

// Every filter gates on OnlyPullRequest > 0; a smaller number is the whole queue.
func checkPullRequestFlag(cmd *cobra.Command, number int) error {
	if !cmd.Flags().Changed("pr") || number > 0 {
		return nil
	}
	return usageError("parse --pr", fmt.Errorf("pull request number must be positive, got %d", number))
}

func reportRun(
	ctx context.Context,
	out io.Writer,
	result domain.Run,
	runErr error,
	narrated bool,
) error {
	if errors.Is(context.Cause(ctx), ErrSignalInterrupt) {
		return report.Interrupted(out, result, narrated)
	}
	// A run that failed before any pull request has nothing to summarise; "nothing to do" would contradict the error.
	if runErr == nil || len(result.Attempts) > 0 {
		return report.Summary(out, result, narrated)
	}
	return nil
}

func newHistoryCommand(deps CommandDependencies, state *executionState) *cobra.Command {
	var (
		configPath string
		runID      string
		last       int
		asJSON     bool
	)
	command := &cobra.Command{
		Use:   "history",
		Short: "Show past runs",
		Example: `  # the last ten runs
  ops-pilot history

  # a wider window
  ops-pilot history --last 20

  # one run in full, as JSON
  ops-pilot history --run 20260816-055238.348317000 --json`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			state.commandRan = true
			loaded, err := prepare(&deps, state, config.CommandHistory, configPath, config.Overrides{})
			if err != nil {
				return err
			}
			store, err := history.Open(loaded.Config.Paths.HistoryDatabase)
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()

			runs, err := selectRuns(cmd, store, runID, last)
			if err != nil {
				return err
			}
			if asJSON {
				return encodeHistoryJSON(cmd.OutOrStdout(), runs)
			}
			return renderHistory(cmd, runs)
		},
	}
	command.Flags().StringVar(&configPath, "config", "", "path to the configuration file")
	command.Flags().StringVar(&runID, "run", "", "show a single run by id")
	command.Flags().IntVar(&last, "last", 10, "number of runs to show")
	command.Flags().BoolVar(&asJSON, "json", false, "emit the full record rather than a table")
	return command
}

func selectRuns(cmd *cobra.Command, store *history.Store, runID string, last int) ([]domain.Run, error) {
	if runID == "" {
		return store.Runs(cmd.Context(), last)
	}
	run, err := store.Run(cmd.Context(), runID)
	if err != nil {
		return nil, err
	}
	return []domain.Run{run}, nil
}

func encodeHistoryJSON(out io.Writer, runs []domain.Run) error {
	if runs == nil {
		runs = []domain.Run{}
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(runs)
}

func renderHistory(cmd *cobra.Command, runs []domain.Run) error {
	table := report.NewTable("RUN", "PR", "DEP", "BUMP", "RESULT", "TOOK")
	table.Fixed(0)
	rows := 0
	for _, run := range runs {
		for _, attempt := range run.Attempts {
			rows++
			table.Add(
				run.ID,
				"#"+strconv.Itoa(attempt.PullRequest),
				attempt.Dependency.Name,
				attempt.Dependency.FromVersion+" -> "+attempt.Dependency.ToVersion,
				report.Label(attempt.Verdict),
				attempt.Duration.Round(time.Second).String(),
			)
		}
	}
	if rows == 0 {
		return emptyHistory(cmd.OutOrStdout(), len(runs))
	}
	return table.Render(cmd.OutOrStdout())
}

func emptyHistory(out io.Writer, runs int) error {
	if runs == 0 {
		_, err := fmt.Fprintln(out, `No runs yet. Run "ops-pilot run" to record one.`)
		return err
	}
	_, err := fmt.Fprintln(out, "No pull requests were processed in the runs shown.")
	return err
}

// interactiveTerminal reports whether prompting the operator can actually work.
// NewApprover repeats this sniff on its own reader; ask about the same one.
func interactiveTerminal(in io.Reader) bool {
	input, ok := in.(*os.File)
	return ok && term.IsTerminal(int(input.Fd()))
}

// activityReporter forwards the agent's tool calls to the runner's narrative.
// The runner does not exist until composition has built it, so the callback is
// installed through this indirection.
type activityReporter struct{ runner *run.Runner }

func (a *activityReporter) report(tool, argument string) {
	if a.runner != nil {
		a.runner.Activity(tool, argument)
	}
}
