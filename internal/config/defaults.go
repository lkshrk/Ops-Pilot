package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

const (
	DefaultSettleTimeout  = 10 * time.Minute
	DefaultStabilityHold  = time.Minute
	DefaultPollInterval   = 10 * time.Second
	DefaultMaxFixAttempts = 2
	DefaultMergeMethod    = "squash"
	DefaultRevertedLabel  = "ops-pilot/reverted"
	DefaultDeclinedLabel  = "ops-pilot/declined"
	DefaultGitHubTokenEnv = "GITHUB_TOKEN"
	DefaultOpenAIKeyEnv   = "OPENAI_API_KEY"
	DefaultOpenAIBaseURL  = "https://api.openai.com/v1"
	DefaultLoggingLevel   = "info"
)

// rolloutGrace mirrors kubernetes.RolloutGrace, which this package must not
// import: a workload that is not ready yet is read as still starting for that
// long, so nothing can be reported as broken before it elapses. The copies are
// held equal by TestTheSettleTimeoutFloorUsesTheSameRolloutGraceAsTheHealthAdapter.
const rolloutGrace = 3 * time.Minute

func DefaultPaths(goos, home, xdgStateHome, xdgCacheHome string) (PathsConfig, error) {
	switch goos {
	case "darwin":
		return PathsConfig{
			HistoryDatabase:   filepath.Join(home, "Library", "Application Support", "ops-pilot", "history.db"),
			CheckoutDirectory: filepath.Join(home, "Library", "Caches", "ops-pilot", "checkouts"),
		}, nil
	case "linux":
		if xdgStateHome == "" {
			xdgStateHome = filepath.Join(home, ".local", "state")
		}
		if xdgCacheHome == "" {
			xdgCacheHome = filepath.Join(home, ".cache")
		}
		return PathsConfig{
			HistoryDatabase:   filepath.Join(xdgStateHome, "ops-pilot", "history.db"),
			CheckoutDirectory: filepath.Join(xdgCacheHome, "ops-pilot", "checkouts"),
		}, nil
	default:
		return PathsConfig{}, fmt.Errorf("unsupported operating system %q", goos)
	}
}

// ApplyDefaults applies all configuration defaults exactly once after Decode.
func ApplyDefaults(loaded *Loaded) {
	if loaded == nil {
		return
	}
	config := &loaded.Config
	if config.Logging.Level == "" {
		config.Logging.Level = DefaultLoggingLevel
	}
	if config.PullRequests.RevertedLabel == "" {
		config.PullRequests.RevertedLabel = DefaultRevertedLabel
	}
	if config.PullRequests.DeclinedLabel == "" {
		config.PullRequests.DeclinedLabel = DefaultDeclinedLabel
	}
	if config.GitHub.TokenEnv == "" {
		config.GitHub.TokenEnv = DefaultGitHubTokenEnv
	}
	if config.GitHub.MergeMethod == "" {
		config.GitHub.MergeMethod = DefaultMergeMethod
	}
	if config.Flux.Source.Kind == "" {
		config.Flux.Source.Kind = "GitRepository"
	}
	if config.AI.BaseURL == "" {
		config.AI.BaseURL = DefaultOpenAIBaseURL
	}
	if config.AI.APIKeyEnv == "" {
		config.AI.APIKeyEnv = DefaultOpenAIKeyEnv
	}
	if config.Watch.SettleTimeout == 0 {
		config.Watch.SettleTimeout = DefaultSettleTimeout
	}
	if config.Watch.StabilityHold == 0 {
		config.Watch.StabilityHold = DefaultStabilityHold
	}
	if config.Watch.PollInterval == 0 {
		config.Watch.PollInterval = DefaultPollInterval
	}
	if !config.Watch.maxFixAttemptsSet {
		config.Watch.MaxFixAttempts = DefaultMaxFixAttempts
	}
	applyDefaultPaths(config)
}

func applyDefaultPaths(config *Config) {
	home := os.Getenv("HOME")
	if home == "" {
		return
	}
	paths, err := DefaultPaths(runtime.GOOS, home, os.Getenv("XDG_STATE_HOME"), os.Getenv("XDG_CACHE_HOME"))
	if err != nil {
		// An unsupported platform leaves the paths unset so the run reports the
		// missing configuration instead of inventing a layout for it.
		return
	}
	if config.Paths.HistoryDatabase == "" {
		config.Paths.HistoryDatabase = paths.HistoryDatabase
	}
	if config.Paths.CheckoutDirectory == "" {
		config.Paths.CheckoutDirectory = paths.CheckoutDirectory
	}
}
