package config

import (
	"strings"
	"testing"
	"time"

	"github.com/lkshrk/ops-pilot/internal/domain"
	"github.com/lkshrk/ops-pilot/internal/repopath"
)

func validLoaded() Loaded {
	return Loaded{Config: Config{
		Repository: domain.RepositoryRef{Owner: "lkshrk", Name: "home-ops"},
		PullRequests: PullRequestsConfig{
			Authors:       []string{"renovate[bot]"},
			RevertedLabel: "reverted",
			DeclinedLabel: "declined",
		},
		GitHub:  GitHubConfig{TokenEnv: "GITHUB_TOKEN", MergeMethod: "squash"},
		Cluster: ClusterConfig{Context: "prod"},
		Flux:    FluxConfig{Source: ObjectRef{Kind: "GitRepository", Namespace: "flux-system", Name: "flux-system"}},
		AI: AIConfig{
			Provider:  "openai",
			Model:     "gpt-5",
			BaseURL:   DefaultOpenAIBaseURL,
			APIKeyEnv: DefaultOpenAIKeyEnv,
		},
		Watch: WatchConfig{
			SettleTimeout: DefaultSettleTimeout,
			StabilityHold: DefaultStabilityHold,
			PollInterval:  DefaultPollInterval,
		},
		Logging: LoggingConfig{Level: "info"},
	}}
}

func TestAConfigurationThatNamesNoPullRequestFilterIsRefused(t *testing.T) {
	cases := []struct {
		name    string
		authors []string
		labels  []string
		valid   bool
	}{
		{name: "neither authors nor labels"},
		{name: "both keys present but empty", authors: []string{}, labels: []string{}},
		{name: "an author that is only whitespace", authors: []string{" "}},
		{name: "an empty author beside a real one", authors: []string{"renovate[bot]", ""}},
		{name: "an empty label", labels: []string{""}},
		{name: "authors alone", authors: []string{"renovate[bot]"}, valid: true},
		{name: "labels alone", labels: []string{"dependencies"}, valid: true},
		{name: "both", authors: []string{"renovate[bot]"}, labels: []string{"dependencies"}, valid: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			loaded := validLoaded()
			loaded.Config.PullRequests.Authors = test.authors
			loaded.Config.PullRequests.Labels = test.labels
			err := Validate(loaded, ValidationContext{Command: CommandRun})
			if test.valid && err != nil {
				t.Fatalf("Validate rejected a configured filter: %v", err)
			}
			if !test.valid && err == nil {
				t.Fatal("Validate accepted a filter that matches every open pull request")
			}
		})
	}
}

func TestTheHistoryCommandIsNotValidatedAgainstKeysOnlyARunUses(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"a missing AI model", func(c *Config) { c.AI.Model = "" }},
		{"an unknown AI provider", func(c *Config) { c.AI.Provider = "" }},
		{"an unusable AI baseURL", func(c *Config) { c.AI.BaseURL = "not a url" }},
		{"no repository binding", func(c *Config) { c.Repository = domain.RepositoryRef{} }},
		{"no cluster context", func(c *Config) { c.Cluster.Context = "" }},
		{"no flux source", func(c *Config) { c.Flux.Source = ObjectRef{} }},
		{"no pull request filter", func(c *Config) { c.PullRequests.Authors = nil }},
		{"an unusable watch window", func(c *Config) { c.Watch = WatchConfig{} }},
		{"a malformed changelog override", func(c *Config) {
			c.Changelog.Overrides = []ChangelogOverride{{Dependency: "sonarr", Repository: "Sonarr"}}
		}},
		{"a malformed allowed path", func(c *Config) { c.Fixes.AllowedPaths = []string{"/etc/**"} }},
		{"an invalid merge method", func(c *Config) { c.GitHub.MergeMethod = "fast-forward" }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			loaded := validLoaded()
			test.mutate(&loaded.Config)
			if err := Validate(loaded, ValidationContext{Command: CommandHistory}); err != nil {
				t.Fatalf("history refused a configuration it never reads: %v", err)
			}
			if err := Validate(loaded, ValidationContext{Command: CommandRun}); err == nil {
				t.Fatal("run accepted the same configuration")
			}
		})
	}
}

func TestTheHistoryCommandStillValidatesWhatItReads(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"an unknown logging level", func(c *Config) { c.Logging.Level = "trace" }},
		{"a registry password environment variable that is not a name", func(c *Config) {
			c.Registries = []RegistryConfig{{Host: "ghcr.io", Username: "u", PasswordEnv: "not a name"}}
		}},
		{"a github token environment variable that is not a name", func(c *Config) { c.GitHub.TokenEnv = "not a name" }},
		{"an AI key environment variable that is not a name", func(c *Config) { c.AI.APIKeyEnv = "not a name" }},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			loaded := validLoaded()
			test.mutate(&loaded.Config)
			if err := Validate(loaded, ValidationContext{Command: CommandHistory}); err == nil {
				t.Fatal("history accepted a configuration it reads and cannot use")
			}
		})
	}
}

// An unrecognised command must get the strict pass, so a command added later is
// validated fully until it says otherwise.
func TestAnUnnamedCommandIsValidatedLikeARun(t *testing.T) {
	loaded := validLoaded()
	loaded.Config.AI.Model = ""
	if err := Validate(loaded, ValidationContext{}); err == nil {
		t.Fatal("Validate accepted a run-invalid configuration for an unnamed command")
	}
}

func TestEachSemanticRuleRefusesTheConfigurationItGuards(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Config)
	}{
		{"a revertedLabel that is empty", func(c *Config) { c.PullRequests.RevertedLabel = "" }},
		{"a declinedLabel that is empty", func(c *Config) { c.PullRequests.DeclinedLabel = "" }},
		{"a declinedLabel equal to the revertedLabel", func(c *Config) { c.PullRequests.DeclinedLabel = c.PullRequests.RevertedLabel }},
		{"a flux source without a namespace", func(c *Config) { c.Flux.Source.Namespace = "" }},
		{"a flux source without a name", func(c *Config) { c.Flux.Source.Name = "" }},
		{"a flux source of the wrong kind", func(c *Config) { c.Flux.Source.Kind = "OCIRepository" }},
		{"an unknown merge method", func(c *Config) { c.GitHub.MergeMethod = "fast-forward" }},
		{"an unknown AI provider", func(c *Config) { c.AI.Provider = "anthropic" }},
		{"an AI model that is empty", func(c *Config) { c.AI.Model = "" }},
		{"an AI baseURL that is not absolute", func(c *Config) { c.AI.BaseURL = "/v1" }},
		{"an AI baseURL that is not http", func(c *Config) { c.AI.BaseURL = "ftp://ai.example/v1" }},
		{"an AI baseURL carrying credentials", func(c *Config) { c.AI.BaseURL = "https://user:secret@ai.example/v1" }},
		{"an AI baseURL carrying a query", func(c *Config) { c.AI.BaseURL = "https://ai.example/v1?key=value" }},
		{"an AI baseURL carrying a fragment", func(c *Config) { c.AI.BaseURL = "https://ai.example/v1#section" }},
		{"a negative maxFixAttempts", func(c *Config) { c.Watch.MaxFixAttempts = -1 }},
		{"a registry host that is not a bare host", func(c *Config) {
			c.Registries = []RegistryConfig{{Host: "https://ghcr.io", Username: "bot", PasswordEnv: "GHCR_TOKEN"}}
		}},
		{"a duplicate registry host", func(c *Config) {
			c.Registries = []RegistryConfig{
				{Host: "ghcr.io", Username: "bot", PasswordEnv: "GHCR_TOKEN"},
				{Host: "ghcr.io", Username: "other", PasswordEnv: "OTHER_TOKEN"},
			}
		}},
		{"a registry username with a colon", func(c *Config) {
			c.Registries = []RegistryConfig{{Host: "ghcr.io", Username: "bot:extra", PasswordEnv: "GHCR_TOKEN"}}
		}},
		{"a registry passwordEnv that is not an environment variable name", func(c *Config) {
			c.Registries = []RegistryConfig{{Host: "ghcr.io", Username: "bot", PasswordEnv: "not a name"}}
		}},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			loaded := validLoaded()
			test.mutate(&loaded.Config)
			if err := Validate(loaded, ValidationContext{Command: CommandRun}); err == nil {
				t.Fatal("Validate accepted a configuration a semantic rule guards against")
			}
		})
	}
}

func TestAFullyPopulatedConfigurationPassesValidation(t *testing.T) {
	loaded := validLoaded()
	loaded.Config.GitHub.MergeMethod = "rebase"
	loaded.Config.AI.BaseURL = "http://ai.internal:8080/v1"
	loaded.Config.Watch.MaxFixAttempts = 0
	loaded.Config.Registries = []RegistryConfig{{Host: "ghcr.io:443", Username: "bot", PasswordEnv: "GHCR_TOKEN"}}
	loaded.Config.Changelog.Overrides = []ChangelogOverride{{Dependency: "plex", Repository: "plexinc/plex"}}
	loaded.Config.Fixes.AllowedPaths = []string{"clusters/**"}
	if err := Validate(loaded, ValidationContext{Command: CommandRun}); err != nil {
		t.Fatalf("Validate rejected a fully populated, well-formed configuration: %v", err)
	}
}

func TestAChangelogOverrideRepositoryMustBeExactlyOwnerName(t *testing.T) {
	cases := []struct {
		repository string
		valid      bool
	}{
		{repository: "Sonarr/Sonarr", valid: true},
		{repository: "home-operations/containers", valid: true},
		{repository: "prometheus/alertmanager.io", valid: true},
		{repository: "https://github.com/Sonarr/Sonarr"},
		{repository: "http://github.com/Sonarr/Sonarr"},
		{repository: "github.com/Sonarr/Sonarr"},
		{repository: "git@github.com:Sonarr/Sonarr"},
		{repository: "Sonarr/Sonarr.git"},
		{repository: "Sonarr/Sonarr/"},
		{repository: "/Sonarr/Sonarr"},
		{repository: "Sonarr/"},
		{repository: "Sonarr"},
		{repository: "a/b/c"},
		{repository: " Sonarr/Sonarr"},
		{repository: "Sonarr/Son arr"},
		{repository: "Sonarr/.."},
		{repository: "../Sonarr"},
		{repository: "Sonarr/Sonarr?ref=main"},
		{repository: "Sonarr/Sonarr#readme"},
		{repository: "Sonarr/Sonarr\n"},
		{repository: "Sonarr/" + strings.Repeat("a", 200)},
	}
	for _, test := range cases {
		t.Run(test.repository, func(t *testing.T) {
			loaded := validLoaded()
			loaded.Config.Changelog.Overrides = []ChangelogOverride{
				{Dependency: "sonarr", Repository: test.repository},
			}
			err := Validate(loaded, ValidationContext{Command: CommandRun})
			if test.valid && err != nil {
				t.Fatalf("Validate rejected the override %q: %v", test.repository, err)
			}
			if !test.valid && err == nil {
				t.Fatalf("Validate accepted the override %q, which resolves no releases", test.repository)
			}
		})
	}
}

func TestASettleTimeoutTooShortToConfirmAFailureIsRefused(t *testing.T) {
	cases := []struct {
		name  string
		watch WatchConfig
		valid bool
	}{
		{
			name:  "the shipped defaults",
			watch: WatchConfig{SettleTimeout: DefaultSettleTimeout, StabilityHold: DefaultStabilityHold, PollInterval: DefaultPollInterval},
			valid: true,
		},
		{
			name:  "exactly the detection floor",
			watch: WatchConfig{SettleTimeout: rolloutGrace + time.Minute + 10*time.Second, StabilityHold: time.Minute, PollInterval: 10 * time.Second},
			valid: true,
		},
		{
			name:  "one poll under the detection floor",
			watch: WatchConfig{SettleTimeout: rolloutGrace + time.Minute, StabilityHold: time.Minute, PollInterval: 10 * time.Second},
		},
		{
			name:  "the two minutes from the finding",
			watch: WatchConfig{SettleTimeout: 2 * time.Minute, StabilityHold: time.Minute, PollInterval: 10 * time.Second},
		},
		{
			name:  "the ninety seconds measured end to end",
			watch: WatchConfig{SettleTimeout: 90 * time.Second, StabilityHold: time.Minute, PollInterval: 10 * time.Second},
		},
		{
			name:  "a settleTimeout of two seconds",
			watch: WatchConfig{SettleTimeout: 2 * time.Second, StabilityHold: time.Second, PollInterval: time.Second},
		},
		{
			name:  "a hold long enough to outgrow a generous window",
			watch: WatchConfig{SettleTimeout: 5 * time.Minute, StabilityHold: 3 * time.Minute, PollInterval: 10 * time.Second},
		},
		{
			name:  "a hold longer than the window",
			watch: WatchConfig{SettleTimeout: 10 * time.Minute, StabilityHold: 20 * time.Minute, PollInterval: 10 * time.Second},
		},
		{
			name:  "a poll slower than the hold",
			watch: WatchConfig{SettleTimeout: 30 * time.Minute, StabilityHold: time.Minute, PollInterval: 2 * time.Minute},
		},
		{
			name:  "an unset watch window",
			watch: WatchConfig{},
		},
		{
			name:  "a negative hold",
			watch: WatchConfig{SettleTimeout: 30 * time.Minute, StabilityHold: -time.Minute, PollInterval: 10 * time.Second},
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			loaded := validLoaded()
			loaded.Config.Watch = test.watch
			err := Validate(loaded, ValidationContext{Command: CommandRun})
			if test.valid && err != nil {
				t.Fatalf("Validate rejected a workable watch window: %v", err)
			}
			if !test.valid && err == nil {
				t.Fatal("Validate accepted a window that can only ever report a stall")
			}
		})
	}
}

func TestAPollIntervalTooCoarseToObserveTheStabilityHoldIsRefused(t *testing.T) {
	const hold = time.Minute
	cases := []struct {
		name  string
		poll  time.Duration
		valid bool
	}{
		{"the shipped ten-second poll against a one-minute hold", 10 * time.Second, true},
		{"a poll of exactly half the hold", 30 * time.Second, true},
		{"a poll just over half the hold", 31 * time.Second, false},
		{"a poll equal to the hold", time.Minute, false},
		{"a poll coarser than the hold", 2 * time.Minute, false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			loaded := validLoaded()
			loaded.Config.Watch = WatchConfig{
				SettleTimeout: 30 * time.Minute,
				StabilityHold: hold,
				PollInterval:  test.poll,
			}
			err := Validate(loaded, ValidationContext{Command: CommandRun})
			if test.valid && err != nil {
				t.Fatalf("Validate rejected a poll fine enough to observe the hold: %v", err)
			}
			if !test.valid && err == nil {
				t.Fatalf(
					"Validate accepted pollInterval %s against stabilityHold %s: the hold spans too few "+
						"polls, so the poll rate aliasing into a workload's unhealthy phase decides the revert",
					test.poll, hold,
				)
			}
		})
	}
}

func TestARepositoryBindingMustNameOneOwnerAndOneRepository(t *testing.T) {
	cases := []struct {
		owner, name string
		valid       bool
	}{
		{owner: "lkshrk", name: "home-ops", valid: true},
		{owner: "home-operations", name: "home_ops.v2", valid: true},
		{owner: "lkshrk", name: "home-ops/clusters"},
		{owner: "lkshrk/home-ops", name: "clusters"},
		{owner: "lkshrk", name: "a/b/c"},
		{owner: "", name: "home-ops"},
		{owner: "lkshrk", name: ""},
		{owner: " ", name: "home-ops"},
		{owner: "lkshrk", name: " "},
		{owner: " lkshrk", name: "home-ops"},
		{owner: "lkshrk", name: "home ops"},
		{owner: "..", name: "home-ops"},
		{owner: "lkshrk", name: ".."},
		{owner: "lkshrk", name: "home-ops.git"},
		{owner: "lkshrk", name: "home-ops?ref=main"},
		{owner: "lkshrk", name: "home-ops\n"},
		{owner: "lkshrk", name: strings.Repeat("a", 200)},
	}
	for _, test := range cases {
		t.Run(test.owner+"|"+test.name, func(t *testing.T) {
			loaded := validLoaded()
			loaded.Config.Repository = domain.RepositoryRef{Owner: test.owner, Name: test.name}
			err := Validate(loaded, ValidationContext{Command: CommandRun})
			if test.valid && err != nil {
				t.Fatalf("Validate rejected the binding %q/%q: %v", test.owner, test.name, err)
			}
			if !test.valid && err == nil {
				t.Fatalf("Validate accepted the binding %q/%q", test.owner, test.name)
			}
		})
	}
}

func TestAClusterContextThatNamesNothingIsRefused(t *testing.T) {
	cases := []struct {
		context string
		valid   bool
	}{
		{context: "prod", valid: true},
		{context: "arn:aws:eks:eu-central-1:000000000000:cluster/prod", valid: true},
		{context: "admin@home.local", valid: true},
		{context: ""},
		{context: " "},
		{context: "\t\n"},
	}
	for _, test := range cases {
		t.Run(test.context, func(t *testing.T) {
			loaded := validLoaded()
			loaded.Config.Cluster.Context = test.context
			err := Validate(loaded, ValidationContext{Command: CommandRun})
			if test.valid && err != nil {
				t.Fatalf("Validate rejected the cluster context %q: %v", test.context, err)
			}
			if !test.valid && err == nil {
				t.Fatalf("Validate accepted the cluster context %q", test.context)
			}
		})
	}
}

func TestAFixOutsideTheDeclaredAllowedPathsIsRefused(t *testing.T) {
	allowed := []string{"clusters/prod/apps/**", "clusters/staging/**"}
	cases := []struct {
		name string
		path string
		want bool
	}{
		{"the cluster-scoped binding from the finding", "clusters/prod/attacker-rbac.yaml", false},
		{"a reconcile source planted beside the allowed subtree", "clusters/prod/sources/evil-gitrepository.yaml", false},
		{"a kustomization at the repository root", "kustomization.yaml", false},
		{"an ordinary workload inside an allowed subtree", "clusters/prod/apps/media/deployment.yaml", true},
		{"a file directly inside an allowed subtree", "clusters/staging/kustomization.yaml", true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := AllowsFixPath(allowed, test.path); got != test.want {
				t.Fatalf("AllowsFixPath(%q) = %v, want %v", test.path, got, test.want)
			}
		})
	}
}

// The commonest breakage a version bump causes is an upstream config key the new
// release renamed, and the file carrying that key belongs to the application, not
// to Kubernetes: a file-type restriction here refuses the repair and reverts a
// merge that a two-line edit would have kept.
func TestTheFixAllowlistAdmitsByPatternAloneAndNotByFileType(t *testing.T) {
	allowed := []string{"clusters/prod/apps/**"}
	for _, path := range []string{
		"clusters/prod/apps/media/helmrelease.yaml",
		"clusters/prod/apps/media/config.toml",
		"clusters/prod/apps/media/settings.json",
		"clusters/prod/apps/media/nginx.conf",
		"clusters/prod/apps/media/prometheus.rules",
		"clusters/prod/apps/media/entrypoint.sh",
		"clusters/prod/apps/media/README.md",
		"clusters/prod/apps/media/Dockerfile",
	} {
		if !AllowsFixPath(allowed, path) {
			t.Fatalf("AllowsFixPath(%q, %q) = false; the operator's pattern is the only allowlist", allowed[0], path)
		}
	}
	loaded := validLoaded()
	loaded.Config.Fixes.AllowedPaths = []string{"clusters/prod/apps/**/*.toml", "clusters/prod/apps/*.json"}
	if err := Validate(loaded, ValidationContext{Command: CommandRun}); err != nil {
		t.Fatalf("Validate rejected a non-YAML allowlist pattern: %v", err)
	}
}

func TestAnEmptyAllowlistAllowsNoPathAtAll(t *testing.T) {
	for _, allowed := range [][]string{nil, {}} {
		for _, path := range []string{"clusters/prod/apps/media/deployment.yaml", "README.md", "a"} {
			if AllowsFixPath(allowed, path) {
				t.Fatalf("AllowsFixPath(%v, %q) = true, want false", allowed, path)
			}
		}
	}
}

func TestAnAllowedPathsPatternMayBeAsLongAsTheLongestLegalPath(t *testing.T) {
	if maxFixPatternLength < repopath.MaxLength {
		t.Fatalf("pattern bound %d is below the longest legal path %d; a literal pattern naming that path would be refused", maxFixPatternLength, repopath.MaxLength)
	}
	if err := validFixPathPattern(strings.Repeat("a", maxFixPatternLength)); err != nil {
		t.Fatalf("a pattern at the length limit was refused: %v", err)
	}
	err := validFixPathPattern(strings.Repeat("a", maxFixPatternLength+1))
	if err == nil {
		t.Fatal("a pattern past the length limit was accepted")
	}
	if !strings.Contains(err.Error(), "longer than") {
		t.Fatalf("the length refusal did not name the bound: %v", err)
	}
}

func TestADoubleStarSegmentMatchesAnyNumberOfSegmentsIncludingNone(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"apps/**", "apps", true},
		{"apps/**", "apps/deployment.yaml", true},
		{"apps/**", "apps/media/plex/deployment.yaml", true},
		{"apps/**", "apps-staging/deployment.yaml", false},
		{"apps/**", "clusters/apps/deployment.yaml", false},
		{"**/deployment.yaml", "deployment.yaml", true},
		{"**/deployment.yaml", "apps/media/deployment.yaml", true},
		{"**/deployment.yaml", "apps/media/service.yaml", false},
		{"**/apps/deployment.yaml", "apps/apps/deployment.yaml", true},
		{"apps/**/deployment.yaml", "apps/deployment.yaml", true},
		{"apps/**/media/**", "apps/media/media/plex.yaml", true},
		{"apps/**/media/**", "apps/staging/plex.yaml", false},
		{"**/*.yaml", "clusters/prod/apps/plex.yaml", true},
		{"**", "clusters/prod/attacker-rbac.yaml", true},
	}
	for _, test := range cases {
		t.Run(test.pattern+" vs "+test.path, func(t *testing.T) {
			if got := AllowsFixPath([]string{test.pattern}, test.path); got != test.want {
				t.Fatalf("AllowsFixPath(%q, %q) = %v, want %v", test.pattern, test.path, got, test.want)
			}
		})
	}
}

func TestABareDirectoryPatternDoesNotCoverItsContents(t *testing.T) {
	allowed := []string{"clusters/staging"}
	if !AllowsFixPath(allowed, "clusters/staging") {
		t.Fatal("a wildcard-free pattern must match exactly that path")
	}
	for _, path := range []string{"clusters/staging/app.yaml", "clusters/staging/apps/deployment.yaml"} {
		if AllowsFixPath(allowed, path) {
			t.Fatalf("AllowsFixPath(%q, %q) = true, want false", allowed[0], path)
		}
	}
}

func TestAllowedPathMatchingIsCaseSensitive(t *testing.T) {
	allowed := []string{"clusters/prod/apps/**"}
	if !AllowsFixPath(allowed, "clusters/prod/apps/deployment.yaml") {
		t.Fatal("the declared casing must match")
	}
	for _, path := range []string{
		"Clusters/Prod/Apps/deployment.yaml",
		"clusters/PROD/apps/deployment.yaml",
	} {
		if AllowsFixPath(allowed, path) {
			t.Fatalf("AllowsFixPath(%q, %q) = true, want false", allowed[0], path)
		}
	}
}

func TestASingleStarDoesNotCrossAPathSeparator(t *testing.T) {
	cases := []struct {
		pattern string
		path    string
		want    bool
	}{
		{"clusters/*/apps/**", "clusters/prod/apps/deployment.yaml", true},
		{"clusters/*/apps/**", "clusters/prod/extra/apps/deployment.yaml", false},
		{"apps/*.yaml", "apps/deployment.yaml", true},
		{"apps/*.yaml", "apps/media/deployment.yaml", false},
		{"apps/*.yaml", "apps/deployment.yml", false},
		{"apps/*", "apps/deployment.yaml", true},
		{"apps/*", "apps/media/deployment.yaml", false},
	}
	for _, test := range cases {
		t.Run(test.pattern+" vs "+test.path, func(t *testing.T) {
			if got := AllowsFixPath([]string{test.pattern}, test.path); got != test.want {
				t.Fatalf("AllowsFixPath(%q, %q) = %v, want %v", test.pattern, test.path, got, test.want)
			}
		})
	}
}

func TestAPathThatIsNotAPlainRepositoryPathIsNeverAllowed(t *testing.T) {
	allowed := []string{"**"}
	for _, path := range []string{
		"",
		"/etc/passwd",
		"clusters/prod/../../etc/passwd",
		"clusters/./prod/app.yaml",
		"clusters//prod/app.yaml",
		"clusters/prod/",
		"clusters/prod/app.yaml\n",
		"clusters/prod/app\x00.yaml",
		strings.Repeat("a/", 3000) + "app.yaml",
	} {
		if AllowsFixPath(allowed, path) {
			t.Fatalf("AllowsFixPath(%q) = true, want false", path)
		}
	}
}

// A diff names its own paths, so matching one may not become superlinear in the
// number of segments however many "**" the operator wrote.
func TestAPathThatCannotMatchIsRefusedWithoutBacktrackingBlowup(t *testing.T) {
	pattern := "**/a/**/a/**/a/**/a/**/a/**/z"
	path := strings.TrimSuffix(strings.Repeat("a/", 400), "/")
	done := make(chan bool, 1)
	go func() { done <- AllowsFixPath([]string{pattern}, path) }()
	select {
	case allowed := <-done:
		if allowed {
			t.Fatalf("AllowsFixPath(%q, <400 segments>) = true, want false", pattern)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("AllowsFixPath(%q, <400 segments>) did not settle", pattern)
	}
}

func TestAnUnvalidatedPatternCanNeverWidenTheAllowlist(t *testing.T) {
	for _, pattern := range []string{"/clusters/**", "clusters/../**", "clusters/**apps/**", "clusters/prod/"} {
		if err := validFixPathPattern(pattern); err == nil {
			t.Fatalf("validFixPathPattern(%q) = nil, want an error", pattern)
		}
		if AllowsFixPath([]string{pattern}, "clusters/prod/apps/deployment.yaml") {
			t.Fatalf("AllowsFixPath(%q) = true, want false for a pattern validation rejects", pattern)
		}
	}
}

func TestAMalformedAllowedPathIsRejectedAtStartup(t *testing.T) {
	cases := []struct {
		name    string
		pattern string
	}{
		{"empty", ""},
		{"absolute", "/clusters/prod/**"},
		{"parent traversal", "clusters/../prod/**"},
		{"current directory", "clusters/./prod/**"},
		{"trailing separator", "clusters/prod/"},
		{"empty segment", "clusters//prod/**"},
		{"partial double star", "clusters/**apps/**"},
		{"double star suffix", "clusters/apps**"},
		{"backslash separator", `clusters\prod\**`},
		{"newline", "clusters/prod\n/**"},
		{"nul byte", "clusters/prod\x00/**"},
		{"question mark wildcard", "clusters/pro?/**"},
		{"character class", "clusters/[ps]rod/**"},
		{"over long", strings.Repeat("a/", 3000) + "**"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			loaded := validLoaded()
			loaded.Config.Fixes.AllowedPaths = []string{test.pattern}
			if err := Validate(loaded, ValidationContext{Command: CommandRun}); err == nil {
				t.Fatalf("Validate accepted allowedPaths pattern %q", test.pattern)
			}
		})
	}
}

func TestADuplicateAllowedPathIsRejectedAtStartup(t *testing.T) {
	loaded := validLoaded()
	loaded.Config.Fixes.AllowedPaths = []string{"clusters/prod/apps/**", "clusters/prod/apps/**"}
	if err := Validate(loaded, ValidationContext{Command: CommandRun}); err == nil {
		t.Fatal("Validate accepted a duplicate allowedPaths pattern")
	}
}

func TestAWellFormedAllowlistPassesValidation(t *testing.T) {
	loaded := validLoaded()
	loaded.Config.Fixes.AllowedPaths = []string{"clusters/prod/apps/**", "clusters/staging/**", "apps/*.yaml"}
	if err := Validate(loaded, ValidationContext{Command: CommandRun}); err != nil {
		t.Fatalf("Validate rejected a well-formed allowlist: %v", err)
	}
}

func TestAnAbsentAllowlistDoesNotStopTheRunFromStarting(t *testing.T) {
	if err := Validate(validLoaded(), ValidationContext{Command: CommandRun}); err != nil {
		t.Fatalf("Validate rejected a configuration without fixes.allowedPaths: %v", err)
	}
}
