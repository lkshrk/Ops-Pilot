package cli

import (
	"fmt"
	"io"
	"runtime"

	"github.com/lkshrk/ops-pilot/internal/run"
	"github.com/spf13/cobra"
)

func newVersionCommand(version VersionInfo, state *executionState) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print version information",
		Args:  cobra.NoArgs,
		RunE: func(command *cobra.Command, _ []string) error {
			return printVersion(command.OutOrStdout(), version, state.verbosity())
		},
	}
}

func printVersion(out io.Writer, version VersionInfo, verbosity run.Verbosity) error {
	if verbosity <= run.VerbosityQuiet {
		_, err := fmt.Fprintf(out, "ops-pilot %s\n", version.Version)
		return err
	}
	if _, err := fmt.Fprintf(
		out, "ops-pilot %s (commit %s, built %s)\n",
		version.Version, versionField(version.Commit), versionField(version.BuildDate),
	); err != nil {
		return err
	}
	if verbosity < run.VerbosityVerbose {
		return nil
	}
	_, err := fmt.Fprintf(out, "%s %s/%s\n", runtime.Version(), runtime.GOOS, runtime.GOARCH)
	return err
}

// versionField names an ldflags value the build did not inject.
func versionField(value string) string {
	if value == "" {
		return "unknown"
	}
	return value
}
