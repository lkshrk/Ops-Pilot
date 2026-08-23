// Package config loads the operator-controlled ops-pilot configuration.
package config

import (
	"fmt"
	"time"

	"github.com/lkshrk/ops-pilot/internal/domain"
	"gopkg.in/yaml.v3"
)

type Config struct {
	Repository   domain.RepositoryRef `yaml:"repository"`
	PullRequests PullRequestsConfig   `yaml:"pullRequests"`
	GitHub       GitHubConfig         `yaml:"github"`
	Cluster      ClusterConfig        `yaml:"cluster"`
	Flux         FluxConfig           `yaml:"flux"`
	AI           AIConfig             `yaml:"ai"`
	Watch        WatchConfig          `yaml:"watch"`
	Changelog    ChangelogConfig      `yaml:"changelog"`
	Registries   []RegistryConfig     `yaml:"registries"`
	Logging      LoggingConfig        `yaml:"logging"`
	Paths        PathsConfig          `yaml:"paths"`
	Fixes        FixesConfig          `yaml:"fixes"`
}

type FixesConfig struct {
	AllowedPaths []string `yaml:"allowedPaths"`
}

type PullRequestsConfig struct {
	Authors       []string `yaml:"authors"`
	Labels        []string `yaml:"labels"`
	RevertedLabel string   `yaml:"revertedLabel"`
	DeclinedLabel string   `yaml:"declinedLabel"`
}

type GitHubConfig struct {
	TokenEnv    string `yaml:"tokenEnv"`
	MergeMethod string `yaml:"mergeMethod"`
}

type ClusterConfig struct {
	Context    string `yaml:"context"`
	Kubeconfig string `yaml:"kubeconfig"`
}

type FluxConfig struct {
	Source ObjectRef `yaml:"source"`
}

type ObjectRef struct {
	Kind      string `yaml:"kind"`
	Namespace string `yaml:"namespace"`
	Name      string `yaml:"name"`
}

type AIConfig struct {
	Provider  string `yaml:"provider"`
	Model     string `yaml:"model"`
	BaseURL   string `yaml:"baseURL"`
	APIKeyEnv string `yaml:"apiKeyEnv"`
}

type WatchConfig struct {
	SettleTimeout  time.Duration `yaml:"settleTimeout"`
	StabilityHold  time.Duration `yaml:"stabilityHold"`
	PollInterval   time.Duration `yaml:"pollInterval"`
	MaxFixAttempts int           `yaml:"maxFixAttempts"`

	maxFixAttemptsSet bool
}

func (c *WatchConfig) UnmarshalYAML(value *yaml.Node) error {
	raw, err := decodeKnown[struct {
		SettleTimeout  time.Duration `yaml:"settleTimeout"`
		StabilityHold  time.Duration `yaml:"stabilityHold"`
		PollInterval   time.Duration `yaml:"pollInterval"`
		MaxFixAttempts *int          `yaml:"maxFixAttempts"`
	}](value, "settleTimeout", "stabilityHold", "pollInterval", "maxFixAttempts")
	if err != nil {
		return err
	}
	c.SettleTimeout, c.StabilityHold, c.PollInterval = raw.SettleTimeout, raw.StabilityHold, raw.PollInterval
	if raw.MaxFixAttempts != nil {
		c.MaxFixAttempts = *raw.MaxFixAttempts
		c.maxFixAttemptsSet = true
	}
	return nil
}

type ChangelogConfig struct {
	Overrides []ChangelogOverride `yaml:"overrides"`
}

type ChangelogOverride struct {
	Dependency string `yaml:"dependency"`
	Repository string `yaml:"repository"`
}

type RegistryConfig struct {
	Host        string `yaml:"host"`
	Username    string `yaml:"username"`
	PasswordEnv string `yaml:"passwordEnv"`
}

type LoggingConfig struct {
	Level string `yaml:"level"`
}

type PathsConfig struct {
	HistoryDatabase   string `yaml:"historyDatabase"`
	CheckoutDirectory string `yaml:"checkoutDirectory"`
}

type LoadOptions struct {
	ExplicitPath     string
	EnvironmentPath  string
	WorkingDirectory string
}

type Loaded struct {
	Config         Config
	Path           string
	RepositoryPath string
}

type Overrides struct {
	Repository   *domain.RepositoryRef
	LoggingLevel *string
}

type Command string

const (
	CommandRun     Command = "run"
	CommandHistory Command = "history"
)

type ValidationContext struct {
	Command Command
}

// decodeKnown rejects unknown keys before decoding into the caller's shadow
// struct, which distinguishes an absent scalar from an explicit zero. The inner
// Decode does not inherit KnownFields, so every shadow field must stay scalar.
func decodeKnown[T any](value *yaml.Node, allowed ...string) (T, error) {
	var raw T
	if err := rejectUnknownFields(value, allowed...); err != nil {
		return raw, err
	}
	if err := value.Decode(&raw); err != nil {
		return raw, err
	}
	return raw, nil
}

func rejectUnknownFields(value *yaml.Node, allowed ...string) error {
	if value.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i < len(value.Content); i += 2 {
		known := false
		for _, name := range allowed {
			if value.Content[i].Value == name {
				known = true
				break
			}
		}
		if !known {
			return fmt.Errorf("field %q not found in configuration", value.Content[i].Value)
		}
	}
	return nil
}
