package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/lkshrk/ops-pilot/internal/config"
	"github.com/lkshrk/ops-pilot/internal/diagnostics"
	"github.com/lkshrk/ops-pilot/internal/domain"
)

// prepare loads ./.env, then decodes, defaults and validates configuration for
// one command. It layers the dotenv values onto deps.LookupEnv in place, so
// every later credential lookup sees them too.
func prepare(
	deps *CommandDependencies,
	state *executionState,
	command config.Command,
	explicitPath string,
	overrides config.Overrides,
) (config.Loaded, error) {
	workingDirectory, err := os.Getwd()
	if err != nil {
		return config.Loaded{}, usageError("resolve working directory", err)
	}
	if err := applyDotenv(deps, workingDirectory); err != nil {
		return config.Loaded{}, err
	}
	environmentPath, _ := deps.LookupEnv("OPS_PILOT_CONFIG")
	loaded, err := deps.DecodeConfig(config.LoadOptions{
		ExplicitPath:     explicitPath,
		EnvironmentPath:  environmentPath,
		WorkingDirectory: workingDirectory,
	})
	if err != nil {
		return config.Loaded{}, err
	}
	deps.ApplyConfigDefaults(&loaded)
	level, err := state.loggingOverride()
	if err != nil {
		return config.Loaded{}, err
	}
	overrides.LoggingLevel = level
	deps.ApplyConfigOverrides(&loaded, overrides)
	if err := deps.ValidateConfig(loaded, config.ValidationContext{Command: command}); err != nil {
		return config.Loaded{}, err
	}
	state.installLogger(loaded.Config, deps)
	return loaded, nil
}

func repositoryOverride(value string) (*domain.RepositoryRef, error) {
	if value == "" {
		return nil, nil
	}
	owner, name, found := strings.Cut(value, "/")
	if !found || owner == "" || name == "" {
		return nil, usageError("parse --repo", errors.New("repository must be owner/name"))
	}
	return &domain.RepositoryRef{Owner: owner, Name: name}, nil
}

func usageError(operation string, cause error) error {
	return &domain.Error{Class: domain.ErrorUsage, Operation: operation, Cause: cause}
}

// applyDotenv reads ./.env from the launch directory. A real environment
// variable always wins, so a shell export overrides the file rather than the
// other way round. An absent file is not an error; an unparseable one is.
func applyDotenv(deps *CommandDependencies, directory string) error {
	values, err := deps.LoadDotenv(filepath.Join(directory, ".env"))
	if err != nil {
		return &domain.Error{
			Class:     domain.ErrorConfiguration,
			Operation: "load .env",
			Cause:     err,
		}
	}
	if len(values) == 0 {
		return nil
	}
	deps.LookupEnv = environmentLookup(deps.LookupEnv, values)
	return nil
}

// installLogger builds the leveled, redacting logger the run reports through.
// Diagnostics go to stderr so the narrative and summary on stdout stay clean.
func (s *executionState) installLogger(cfg config.Config, deps *CommandDependencies) {
	level, _ := diagnostics.ParseLevel(cfg.Logging.Level)
	s.redactor = diagnostics.NewRedactor(secrets(cfg, deps.LookupEnv))
	s.logger = diagnostics.NewLeveledLogger(deps.Stderr, s.redactor, level, nil)
}

// secrets collects the values that must never appear in output. They are read
// through the same lookup the run uses, so a value supplied by .env is redacted
// exactly like an exported one.
func secrets(cfg config.Config, lookup func(string) (string, bool)) []string {
	names := []string{cfg.GitHub.TokenEnv, cfg.AI.APIKeyEnv}
	for _, registry := range cfg.Registries {
		names = append(names, registry.PasswordEnv)
	}
	values := make([]string, 0, len(names))
	for _, name := range names {
		if value, found := lookup(name); found && value != "" {
			values = append(values, value)
		}
	}
	return values
}
