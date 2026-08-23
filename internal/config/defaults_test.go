package config

import (
	"reflect"
	"testing"
	"time"

	"github.com/lkshrk/ops-pilot/internal/adapters/kubernetes"
)

func TestApplyDefaultsFillsEveryUnsetFieldWithItsDocumentedDefault(t *testing.T) {
	loaded := Loaded{Config: Config{
		Paths: PathsConfig{HistoryDatabase: "history.db", CheckoutDirectory: "checkouts"},
	}}
	ApplyDefaults(&loaded)
	config := loaded.Config
	checks := []struct {
		field string
		got   any
		want  any
	}{
		{"logging.level", config.Logging.Level, DefaultLoggingLevel},
		{"pullRequests.revertedLabel", config.PullRequests.RevertedLabel, DefaultRevertedLabel},
		{"pullRequests.declinedLabel", config.PullRequests.DeclinedLabel, DefaultDeclinedLabel},
		{"github.tokenEnv", config.GitHub.TokenEnv, DefaultGitHubTokenEnv},
		{"github.mergeMethod", config.GitHub.MergeMethod, DefaultMergeMethod},
		{"flux.source.kind", config.Flux.Source.Kind, "GitRepository"},
		{"ai.baseURL", config.AI.BaseURL, DefaultOpenAIBaseURL},
		{"ai.apiKeyEnv", config.AI.APIKeyEnv, DefaultOpenAIKeyEnv},
		{"watch.settleTimeout", config.Watch.SettleTimeout, DefaultSettleTimeout},
		{"watch.stabilityHold", config.Watch.StabilityHold, DefaultStabilityHold},
		{"watch.pollInterval", config.Watch.PollInterval, DefaultPollInterval},
		{"watch.maxFixAttempts", config.Watch.MaxFixAttempts, DefaultMaxFixAttempts},
	}
	for _, check := range checks {
		if check.got != check.want {
			t.Errorf("%s defaulted to %v, want %v", check.field, check.got, check.want)
		}
	}
}

func TestApplyDefaultsLeavesEveryExplicitlySetFieldUntouched(t *testing.T) {
	loaded := Loaded{Config: Config{
		Logging:      LoggingConfig{Level: "debug"},
		PullRequests: PullRequestsConfig{RevertedLabel: "kept-reverted", DeclinedLabel: "kept-declined"},
		GitHub:       GitHubConfig{TokenEnv: "MY_TOKEN", MergeMethod: "merge"},
		Flux:         FluxConfig{Source: ObjectRef{Kind: "OCIRepository", Namespace: "flux", Name: "root"}},
		AI:           AIConfig{BaseURL: "http://ai.internal:8080/v1", APIKeyEnv: "MY_KEY"},
		Watch: WatchConfig{
			SettleTimeout:     20 * time.Minute,
			StabilityHold:     3 * time.Minute,
			PollInterval:      5 * time.Second,
			MaxFixAttempts:    7,
			maxFixAttemptsSet: true,
		},
		Paths: PathsConfig{HistoryDatabase: "history.db", CheckoutDirectory: "checkouts"},
	}}
	before := loaded.Config
	ApplyDefaults(&loaded)
	if !reflect.DeepEqual(loaded.Config, before) {
		t.Fatalf("ApplyDefaults overwrote an explicitly set field: got %+v, want %+v", loaded.Config, before)
	}
}

func TestApplyDefaultsIgnoresANilLoaded(t *testing.T) {
	ApplyDefaults(nil)
}

func TestTheSettleTimeoutFloorUsesTheSameRolloutGraceAsTheHealthAdapter(t *testing.T) {
	if rolloutGrace != kubernetes.RolloutGrace {
		t.Fatalf(
			"rolloutGrace in internal/config/defaults.go is %s but kubernetes.RolloutGrace in "+
				"internal/adapters/kubernetes/health.go is %s: validateWatch computes the settleTimeout "+
				"floor from its own copy, so any difference lets Validate accept a settleTimeout the "+
				"detection path cannot meet",
			rolloutGrace, kubernetes.RolloutGrace,
		)
	}
}

func TestValidateWatchRefusesASettleTimeoutUnderTheAdaptersOwnRolloutGrace(t *testing.T) {
	const hold = time.Minute
	const poll = 10 * time.Second
	floor := kubernetes.RolloutGrace + hold + poll

	cases := []struct {
		name          string
		settleTimeout time.Duration
		valid         bool
	}{
		{name: "one poll under the adapter's detection floor", settleTimeout: floor - poll},
		{name: "one nanosecond under the adapter's detection floor", settleTimeout: floor - 1},
		{name: "exactly the adapter's detection floor", settleTimeout: floor, valid: true},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := validateWatch(WatchConfig{
				SettleTimeout: testCase.settleTimeout,
				StabilityHold: hold,
				PollInterval:  poll,
			})
			if testCase.valid && err != nil {
				t.Fatalf("settleTimeout %s clears the floor derived from kubernetes.RolloutGrace %s but validateWatch refused it: %v",
					testCase.settleTimeout, kubernetes.RolloutGrace, err)
			}
			if !testCase.valid && err == nil {
				t.Fatalf("validateWatch accepted settleTimeout %s, under the %s a failure takes to confirm with kubernetes.RolloutGrace %s, so breakage could only ever be reported as a stall",
					testCase.settleTimeout, floor, kubernetes.RolloutGrace)
			}
		})
	}
}
