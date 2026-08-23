package config

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/lkshrk/ops-pilot/internal/domain"
	"gopkg.in/yaml.v3"
)

// Decode selects and decodes one configuration file. It resolves filesystem
// paths, but deliberately applies no defaults, overrides, or validation.
func Decode(options LoadOptions) (Loaded, error) {
	path, source := configPath(options)
	path, err := filepath.Abs(path)
	if err != nil {
		return Loaded{}, configurationError("resolve config path", err)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Clean(path))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return Loaded{}, configurationError("read configuration", missingConfiguration(path, source))
		}
		return Loaded{}, configurationError("resolve config path", fmt.Errorf("%q: %w", path, err))
	}
	path = resolved
	file, err := os.Open(path)
	if err != nil {
		return Loaded{}, configurationError("open configuration", fmt.Errorf("%q: %w", path, err))
	}
	defer file.Close()

	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	var config Config
	if err := decoder.Decode(&config); err != nil {
		return Loaded{}, configurationError("decode configuration", fmt.Errorf("%q: %w", path, err))
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return Loaded{}, configurationError(
				"decode configuration",
				fmt.Errorf("%q: multiple YAML documents are not allowed", path),
			)
		}
		return Loaded{}, configurationError("decode configuration", fmt.Errorf("%q: %w", path, err))
	}

	resolveRelativePaths(&config, filepath.Dir(path))
	return Loaded{
		Config:         config,
		Path:           path,
		RepositoryPath: repositoryPath(path),
	}, nil
}

func resolveRelativePaths(config *Config, directory string) {
	config.Paths.HistoryDatabase = resolvePath(directory, config.Paths.HistoryDatabase)
	config.Paths.CheckoutDirectory = resolvePath(directory, config.Paths.CheckoutDirectory)
}

func resolvePath(directory, path string) string {
	if path == "" {
		return path
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Clean(filepath.Join(directory, path))
}

// ApplyOverrides assigns command-line overrides without validating the result.
func ApplyOverrides(loaded *Loaded, overrides Overrides) {
	if loaded == nil {
		return
	}
	if overrides.Repository != nil {
		loaded.Config.Repository.Owner = overrides.Repository.Owner
		loaded.Config.Repository.Name = overrides.Repository.Name
	}
	if overrides.LoggingLevel != nil {
		loaded.Config.Logging.Level = *overrides.LoggingLevel
	}
}

func repositoryPath(configPath string) string {
	worktree := enclosingGitWorktree(filepath.Dir(configPath))
	if worktree == "" {
		return ""
	}
	relative, err := filepath.Rel(worktree, configPath)
	if err != nil || relative == "." || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return ""
	}
	return filepath.ToSlash(filepath.Clean(relative))
}

func enclosingGitWorktree(directory string) string {
	for {
		if _, err := os.Lstat(filepath.Join(directory, ".git")); err == nil {
			canonical, err := filepath.EvalSymlinks(directory)
			if err == nil {
				return filepath.Clean(canonical)
			}
			return ""
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return ""
		}
		directory = parent
	}
}

func configurationError(operation string, cause error) error {
	return &domain.Error{
		Class:     domain.ErrorConfiguration,
		Operation: operation,
		Cause:     cause,
	}
}

type configSource int

const (
	sourceDefault configSource = iota
	sourceFlag
	sourceEnvironment
)

func configPath(options LoadOptions) (string, configSource) {
	if options.ExplicitPath != "" {
		return options.ExplicitPath, sourceFlag
	}
	if options.EnvironmentPath != "" {
		return options.EnvironmentPath, sourceEnvironment
	}
	return filepath.Join(options.WorkingDirectory, "ops-pilot.yaml"), sourceDefault
}

func missingConfiguration(path string, source configSource) error {
	switch source {
	case sourceFlag:
		return fmt.Errorf("no configuration file at %q; check the path given to --config", path)
	case sourceEnvironment:
		return fmt.Errorf("no configuration file at %q; check OPS_PILOT_CONFIG", path)
	}
	return fmt.Errorf(
		"no ops-pilot.yaml in %q; create one there or pass --config <path>", filepath.Dir(path),
	)
}
