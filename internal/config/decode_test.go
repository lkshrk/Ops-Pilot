package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func decodeBody(t *testing.T, body string) (Loaded, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ops-pilot.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return Decode(LoadOptions{ExplicitPath: path})
}

func TestEverySectionCarriesEveryFieldItDeclaresThroughDecode(t *testing.T) {
	loaded, err := decodeBody(t, `flux:
  source:
    kind: GitRepository
    namespace: flux-system
    name: fleet
registries:
  - host: ghcr.io
    username: bot
    passwordEnv: GHCR_TOKEN
  - host: registry.example.com
    username: other
    passwordEnv: OTHER_TOKEN
logging:
  level: debug
watch:
  settleTimeout: 15m
  stabilityHold: 2m
  pollInterval: 5s
  maxFixAttempts: 3
`)
	if err != nil {
		t.Fatalf("decode a fully populated configuration: %v", err)
	}

	wantSource := ObjectRef{Kind: "GitRepository", Namespace: "flux-system", Name: "fleet"}
	if loaded.Config.Flux.Source != wantSource {
		t.Errorf("flux.source decoded to %+v, want %+v", loaded.Config.Flux.Source, wantSource)
	}

	wantRegistries := []RegistryConfig{
		{Host: "ghcr.io", Username: "bot", PasswordEnv: "GHCR_TOKEN"},
		{Host: "registry.example.com", Username: "other", PasswordEnv: "OTHER_TOKEN"},
	}
	if !reflect.DeepEqual(loaded.Config.Registries, wantRegistries) {
		t.Errorf("registries decoded to %+v, want %+v", loaded.Config.Registries, wantRegistries)
	}

	if loaded.Config.Logging != (LoggingConfig{Level: "debug"}) {
		t.Errorf("logging decoded to %+v, want level debug", loaded.Config.Logging)
	}

	wantWatch := WatchConfig{
		SettleTimeout:     15 * time.Minute,
		StabilityHold:     2 * time.Minute,
		PollInterval:      5 * time.Second,
		MaxFixAttempts:    3,
		maxFixAttemptsSet: true,
	}
	if loaded.Config.Watch != wantWatch {
		t.Errorf("watch decoded to %+v, want %+v", loaded.Config.Watch, wantWatch)
	}
}

// Each nesting shape reaches the unknown-key check by a different decoder path:
// top level, nested struct, sequence element, and a type with its own decoder.
func TestAnUnknownKeyInAnyNestedSectionRefusesTheLoad(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{"at the top level", "bogus: 1\n"},
		{"in a nested struct", "flux:\n  source:\n    kind: GitRepository\n    bogus: 1\n"},
		{"in a sequence element", "registries:\n  - host: ghcr.io\n    bogus: 1\n"},
		{"in a single-field section", "logging:\n  level: info\n  bogus: 1\n"},
		{"in the section with a custom decoder", "watch:\n  pollInterval: 5s\n  bogus: 1\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := decodeBody(t, test.body)
			if err == nil {
				t.Fatal("an unknown key loaded without complaint, so an operator who misspells a " +
					"setting is told nothing and runs with the default they did not choose")
			}
			if !strings.Contains(err.Error(), "bogus") {
				t.Fatalf("the load failed without naming the offending key: %v", err)
			}
		})
	}
}

func TestASectionThatIsNotAMappingIsRefusedRatherThanIgnored(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{"flux.source", "flux:\n  source: GitRepository\n"},
		{"a registries element", "registries:\n  - ghcr.io\n"},
		{"logging", "logging: debug\n"},
		{"watch", "watch: 5s\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := decodeBody(t, test.body); err == nil {
				t.Fatal("a scalar in place of a section decoded silently, leaving every field of " +
					"that section at its default without telling the operator")
			}
		})
	}
}

func TestAnEmptySectionDecodesToItsZeroValueWithoutError(t *testing.T) {
	loaded, err := decodeBody(t, "flux:\n  source:\nlogging:\nwatch:\n")
	if err != nil {
		t.Fatalf("decode a configuration with empty sections: %v", err)
	}
	if loaded.Config.Flux.Source != (ObjectRef{}) ||
		loaded.Config.Logging != (LoggingConfig{}) ||
		loaded.Config.Watch != (WatchConfig{}) {
		t.Fatalf("an empty section did not decode to its zero value: %+v", loaded.Config)
	}
}

// The only reason WatchConfig decodes through a shadow struct: an operator who
// writes maxFixAttempts: 0 has forbidden autonomous fix commits, and that must
// not be read as unset and silently raised to the default.
func TestAnExplicitZeroMaxFixAttemptsSurvivesTheDefaultsAnAbsentOneReceives(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
		want int
	}{
		{"absent", "watch:\n  pollInterval: 5s\n", DefaultMaxFixAttempts},
		{"absent section", "logging:\n  level: info\n", DefaultMaxFixAttempts},
		{"explicitly zero", "watch:\n  maxFixAttempts: 0\n", 0},
		{"explicitly set", "watch:\n  maxFixAttempts: 5\n", 5},
	} {
		t.Run(test.name, func(t *testing.T) {
			loaded, err := decodeBody(t, test.body)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			ApplyDefaults(&loaded)
			if loaded.Config.Watch.MaxFixAttempts != test.want {
				t.Fatalf("maxFixAttempts %s became %d, want %d",
					test.name, loaded.Config.Watch.MaxFixAttempts, test.want)
			}
		})
	}
}
