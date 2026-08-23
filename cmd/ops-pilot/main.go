package main

import (
	"context"
	"os"

	"github.com/lkshrk/ops-pilot/internal/cli"
)

var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:], cli.CommandDependencies{}, versionInfo()))
}

func run(ctx context.Context, args []string, deps cli.CommandDependencies, info cli.VersionInfo) int {
	return cli.Execute(ctx, args, deps, info)
}

func versionInfo() cli.VersionInfo {
	return cli.VersionInfo{Version: version, Commit: commit, BuildDate: buildDate}
}
