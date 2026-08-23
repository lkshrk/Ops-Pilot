package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lkshrk/ops-pilot/internal/domain"
)

func TestRepositoryPathRefusesEveryConfigThatResolvesOutsideItsWorktree(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("canonicalise the temporary root: %v", err)
	}
	worktree := filepath.Join(root, "repo")
	if err := os.MkdirAll(filepath.Join(worktree, "clusters", "prod"), 0o700); err != nil {
		t.Fatalf("create the worktree: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(worktree, ".git"), 0o700); err != nil {
		t.Fatalf("create .git: %v", err)
	}
	outside := filepath.Join(root, "elsewhere")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatalf("create the outside directory: %v", err)
	}
	aliased := filepath.Join(root, "aliased")
	if err := os.Symlink(worktree, aliased); err != nil {
		t.Fatalf("link to the worktree: %v", err)
	}

	for _, test := range []struct {
		name string
		path string
		want string
	}{
		{"at the worktree root", filepath.Join(worktree, "ops-pilot.yaml"), "ops-pilot.yaml"},
		{"nested in the worktree", filepath.Join(worktree, "clusters", "prod", "ops-pilot.yaml"), "clusters/prod/ops-pilot.yaml"},
		{"in no worktree at all", filepath.Join(outside, "ops-pilot.yaml"), ""},
		{"named through a symlink to the worktree", filepath.Join(aliased, "ops-pilot.yaml"), ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := repositoryPath(test.path); got != test.want {
				t.Fatalf("repositoryPath(%q) = %q, want %q", test.path, got, test.want)
			}
		})
	}
}

// The repository index was deleted, so a config still naming it describes a
// feature that will never run. Accepting the key silently leaves the operator
// believing it is configured; refusing the load says so while the merge is
// still untouched.
func TestAConfigNamingTheDeletedRepositoryIndexIsRejectedRatherThanIgnored(t *testing.T) {
	tests := map[string]string{
		"the repositoryIndex section": "repositoryIndex:\n  executable: codebase-memory-mcp\n" +
			"  cacheDirectory: .ops-pilot/cache/codebase-memory\n",
		"the index directory path": "paths:\n  indexDirectory: /var/cache/ops-pilot/index\n",
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "ops-pilot.yaml")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}

			_, err := Decode(LoadOptions{ExplicitPath: path})
			if err == nil {
				t.Fatal("a config naming the deleted repository index loaded without complaint, " +
					"so the operator is told nothing about a setting that drives nothing")
			}
			if !strings.Contains(err.Error(), "field") {
				t.Fatalf("the load failed for some other reason than the unknown key: %v", err)
			}
		})
	}
}

func TestAMissingConfigurationNamesTheFileAndTheFix(t *testing.T) {
	directory := t.TempDir()
	missing := filepath.Join(directory, "absent.yaml")
	tests := []struct {
		name    string
		options LoadOptions
		want    []string
	}{
		{
			name:    "named by --config",
			options: LoadOptions{ExplicitPath: missing},
			want:    []string{"absent.yaml", "--config"},
		},
		{
			name:    "named by OPS_PILOT_CONFIG",
			options: LoadOptions{EnvironmentPath: missing},
			want:    []string{"absent.yaml", "OPS_PILOT_CONFIG"},
		},
		{
			name:    "defaulted beside the operator",
			options: LoadOptions{WorkingDirectory: directory},
			want:    []string{"ops-pilot.yaml", directory, "--config"},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Decode(test.options)
			if err == nil {
				t.Fatal("a missing configuration file loaded without complaint")
			}
			if got := domain.ErrorClassOf(err); got != domain.ErrorConfiguration {
				t.Fatalf("error class = %q, want %q", got, domain.ErrorConfiguration)
			}
			message := err.Error()
			if strings.Contains(message, "lstat") {
				t.Fatalf("the error still hands the operator a syscall name: %s", message)
			}
			for _, want := range test.want {
				if !strings.Contains(message, want) {
					t.Fatalf("error = %q, want it to mention %q", message, want)
				}
			}
		})
	}
}

func TestAConfigurationThatExistsIsNotReportedAsMissing(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "ops-pilot.yaml")
	if err := os.WriteFile(path, []byte("logging:\n  level: debug\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	loaded, err := Decode(LoadOptions{ExplicitPath: path})
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if loaded.Config.Logging.Level != "debug" {
		t.Fatalf("logging level = %q, want %q", loaded.Config.Logging.Level, "debug")
	}
}
