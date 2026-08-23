package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/lkshrk/ops-pilot/internal/config"
)

func decodeNothing(config.LoadOptions) (config.Loaded, error) { return config.Loaded{}, nil }

func testDependencies(environment map[string]string) CommandDependencies {
	return defaultCommandDependencies(CommandDependencies{
		DecodeConfig:         decodeNothing,
		ApplyConfigDefaults:  func(*config.Loaded) {},
		ApplyConfigOverrides: func(*config.Loaded, config.Overrides) {},
		ValidateConfig:       func(config.Loaded, config.ValidationContext) error { return nil },
		LookupEnv: func(name string) (string, bool) {
			value, found := environment[name]
			return value, found
		},
	})
}

func writeDotenv(t *testing.T, contents string) {
	t.Helper()
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, ".env"), []byte(contents), 0o600); err != nil {
		t.Fatalf("write .env: %v", err)
	}
	t.Chdir(directory)
}

// Credentials live in ./.env by design, so a run launched beside one must see
// them without the operator exporting anything.
func TestPrepareLoadsDotenvFromTheLaunchDirectory(t *testing.T) {
	writeDotenv(t, "GITHUB_TOKEN=from-file\nOPENAI_API_KEY='quoted-key'\n")
	deps := testDependencies(nil)

	if _, err := prepare(&deps, &executionState{}, config.CommandRun, "", config.Overrides{}); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	for name, want := range map[string]string{
		"GITHUB_TOKEN":   "from-file",
		"OPENAI_API_KEY": "quoted-key",
	} {
		got, found := deps.LookupEnv(name)
		if !found || got != want {
			t.Fatalf("%s: got %q found=%t, want %q", name, got, found, want)
		}
	}
}

// A shell export is the more deliberate act, so it wins over the file.
func TestExportedEnvironmentBeatsDotenv(t *testing.T) {
	writeDotenv(t, "GITHUB_TOKEN=from-file\n")
	deps := testDependencies(map[string]string{"GITHUB_TOKEN": "from-shell"})

	if _, err := prepare(&deps, &executionState{}, config.CommandRun, "", config.Overrides{}); err != nil {
		t.Fatalf("prepare: %v", err)
	}
	if got, _ := deps.LookupEnv("GITHUB_TOKEN"); got != "from-shell" {
		t.Fatalf("want the exported value to win, got %q", got)
	}
}

func TestMissingDotenvIsNotAnError(t *testing.T) {
	t.Chdir(t.TempDir())
	deps := testDependencies(map[string]string{"GITHUB_TOKEN": "from-shell"})

	if _, err := prepare(&deps, &executionState{}, config.CommandRun, "", config.Overrides{}); err != nil {
		t.Fatalf("an absent .env must not fail the run: %v", err)
	}
}

// Silently ignoring a broken .env would look exactly like a missing credential.
func TestUnparseableDotenvFailsLoudly(t *testing.T) {
	writeDotenv(t, "this line has no equals sign\n")
	deps := testDependencies(nil)

	if _, err := prepare(&deps, &executionState{}, config.CommandRun, "", config.Overrides{}); err == nil {
		t.Fatal("want an error for an unparseable .env")
	}
}
