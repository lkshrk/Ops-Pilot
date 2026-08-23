package config

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/lkshrk/ops-pilot/internal/repopath"
)

var (
	registryHostLabel           = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	environmentVariableName     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	repositorySegmentCharacters = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
)

const (
	// A literal allowedPaths pattern must be able to name the longest legal
	// path, so the pattern bound tracks repopath.MaxLength rather than moving on
	// its own; it is not a second path-length limit.
	maxFixPatternLength  = repopath.MaxLength
	maxRepositorySegment = 100
)

// Validate is the sole semantic validation pass for decoded configuration.
func Validate(loaded Loaded, context ValidationContext) error {
	if err := validate(loaded, context); err != nil {
		return configurationError("validate configuration", err)
	}
	return nil
}

// Only a run merges, watches and reverts, so only a run is held to the bindings
// and timings that decide those. Every other command is still held to what it
// does read, and an unrecognised command is validated as strictly as a run.
func validate(loaded Loaded, context ValidationContext) error {
	config := loaded.Config
	if err := validateAlways(config); err != nil {
		return err
	}
	if context.Command == CommandHistory {
		return nil
	}
	if err := validateBindings(config); err != nil {
		return err
	}
	if err := validateGitHub(config.GitHub); err != nil {
		return err
	}
	if err := validateAI(config.AI); err != nil {
		return err
	}
	if err := validateWatch(config.Watch); err != nil {
		return err
	}
	if err := validateChangelog(config.Changelog); err != nil {
		return err
	}
	return validateFixes(config.Fixes)
}

// The logging level and the three environment variable names the redactor reads
// decide what every command prints, including the ones that never reach a forge.
func validateAlways(config Config) error {
	if !oneOf(config.Logging.Level, "debug", "info", "warn") {
		return fmt.Errorf("invalid logging level %q", config.Logging.Level)
	}
	if !environmentVariableName.MatchString(config.GitHub.TokenEnv) {
		return fmt.Errorf("github tokenEnv must be an environment variable name")
	}
	if !environmentVariableName.MatchString(config.AI.APIKeyEnv) {
		return fmt.Errorf("AI apiKeyEnv must be an environment variable name")
	}
	return validateRegistries(config.Registries)
}

func validateBindings(config Config) error {
	if config.Repository.Owner == "" || config.Repository.Name == "" {
		return fmt.Errorf("repository owner and name are required")
	}
	if !repositorySegment(config.Repository.Owner) || !repositorySegment(config.Repository.Name) {
		return fmt.Errorf(
			"repository %q/%q must be one owner and one repository name, each without a separator, "+
				"whitespace or .git suffix",
			config.Repository.Owner, config.Repository.Name,
		)
	}
	if strings.TrimSpace(config.Cluster.Context) == "" {
		return fmt.Errorf("cluster context is required")
	}
	if config.Flux.Source.Namespace == "" || config.Flux.Source.Name == "" {
		return fmt.Errorf("flux source namespace and name are required")
	}
	if config.Flux.Source.Kind != "GitRepository" {
		return fmt.Errorf("invalid flux source kind %q", config.Flux.Source.Kind)
	}
	if config.PullRequests.RevertedLabel == "" {
		return fmt.Errorf("pullRequests revertedLabel is required")
	}
	if config.PullRequests.DeclinedLabel == "" {
		return fmt.Errorf("pullRequests declinedLabel is required")
	}
	if config.PullRequests.DeclinedLabel == config.PullRequests.RevertedLabel {
		return fmt.Errorf("pullRequests declinedLabel and revertedLabel must differ")
	}
	return validatePullRequestFilter(config.PullRequests)
}

// The filter is the only trust boundary around what gets merged unattended, and
// an absent one admits every open pull request, so it has to be stated rather
// than defaulted.
func validatePullRequestFilter(pullRequests PullRequestsConfig) error {
	for _, author := range pullRequests.Authors {
		if strings.TrimSpace(author) == "" {
			return fmt.Errorf("pullRequests authors must not contain a blank entry")
		}
	}
	for _, label := range pullRequests.Labels {
		if strings.TrimSpace(label) == "" {
			return fmt.Errorf("pullRequests labels must not contain a blank entry")
		}
	}
	if len(pullRequests.Authors) == 0 && len(pullRequests.Labels) == 0 {
		return fmt.Errorf(
			"pullRequests must name at least one author or label: " +
				"an empty filter makes every open pull request a merge candidate",
		)
	}
	return nil
}

func validateGitHub(github GitHubConfig) error {
	if !oneOf(github.MergeMethod, "merge", "squash", "rebase") {
		return fmt.Errorf("invalid github mergeMethod %q", github.MergeMethod)
	}
	return nil
}

func validateAI(ai AIConfig) error {
	if ai.Provider != "openai" {
		return fmt.Errorf("invalid AI provider %q", ai.Provider)
	}
	if ai.Model == "" {
		return fmt.Errorf("AI model is required")
	}
	if !validAIBaseURL(ai.BaseURL) {
		return fmt.Errorf("AI baseURL must be an absolute HTTP URL without credentials, query, or fragment")
	}
	return nil
}

func validateWatch(watch WatchConfig) error {
	if watch.SettleTimeout <= 0 || watch.StabilityHold <= 0 || watch.StabilityHold > watch.SettleTimeout {
		return fmt.Errorf("watch stabilityHold must be positive and no greater than settleTimeout")
	}
	if watch.PollInterval <= 0 {
		return fmt.Errorf("watch pollInterval must be positive")
	}
	// The hold is judged in elapsed time but sampled every pollInterval, so a hold
	// spanning fewer than two polls lets a single aliased sample decide the revert.
	if watch.PollInterval > watch.StabilityHold/2 {
		return fmt.Errorf(
			"watch pollInterval %s must be at most half of stabilityHold %s, so the hold spans at "+
				"least two polls; nearer the hold the poll rate, not the cluster, decides the revert when "+
				"a workload is only intermittently unhealthy",
			watch.PollInterval, watch.StabilityHold,
		)
	}
	// A window shorter than the detection path does not fail sooner, it stops
	// distinguishing a broken merge from a slow one.
	if floor := rolloutGrace + watch.StabilityHold + watch.PollInterval; watch.SettleTimeout < floor {
		return fmt.Errorf(
			"watch settleTimeout %s is shorter than the %s a failure takes to confirm "+
				"(%s before an unready workload counts as broken, then stabilityHold %s, then one poll %s), "+
				"so breakage could only ever be reported as a stall",
			watch.SettleTimeout, floor, rolloutGrace, watch.StabilityHold, watch.PollInterval,
		)
	}
	if watch.MaxFixAttempts < 0 {
		return fmt.Errorf("watch maxFixAttempts must not be negative")
	}
	return nil
}

func validateChangelog(changelog ChangelogConfig) error {
	seen := make(map[string]struct{}, len(changelog.Overrides))
	for _, override := range changelog.Overrides {
		if override.Dependency == "" || override.Repository == "" {
			return fmt.Errorf("changelog override needs both dependency and repository")
		}
		if _, exists := seen[override.Dependency]; exists {
			return fmt.Errorf("duplicate changelog override for %q", override.Dependency)
		}
		seen[override.Dependency] = struct{}{}
		if !ownerName(override.Repository) {
			return fmt.Errorf(
				"changelog override repository %q must be exactly owner/name, without a scheme, host, "+
					"trailing slash or .git suffix",
				override.Repository,
			)
		}
	}
	return nil
}

func ownerName(value string) bool {
	owner, name, found := strings.Cut(value, "/")
	return found && repositorySegment(owner) && repositorySegment(name)
}

// An owner or repository name is interpolated into a forge URL path unescaped,
// so it may hold nothing a forge would not accept there.
func repositorySegment(value string) bool {
	if value == "" || len(value) > maxRepositorySegment || strings.HasSuffix(value, ".git") {
		return false
	}
	if value == "." || value == ".." {
		return false
	}
	return repositorySegmentCharacters.MatchString(value)
}

func validateFixes(fixes FixesConfig) error {
	seen := make(map[string]struct{}, len(fixes.AllowedPaths))
	for _, pattern := range fixes.AllowedPaths {
		if err := validFixPathPattern(pattern); err != nil {
			return err
		}
		if _, exists := seen[pattern]; exists {
			return fmt.Errorf("duplicate fixes allowedPaths pattern %q", pattern)
		}
		seen[pattern] = struct{}{}
	}
	return nil
}

// The grammar accepted here is exactly the one AllowsFixPath implements.
func validFixPathPattern(pattern string) error {
	switch {
	case pattern == "":
		return fmt.Errorf("fixes allowedPaths pattern must not be empty")
	case len(pattern) > maxFixPatternLength:
		return fmt.Errorf("fixes allowedPaths pattern is longer than %d bytes", maxFixPatternLength)
	case strings.HasPrefix(pattern, "/"):
		return fmt.Errorf("fixes allowedPaths pattern %q must be relative to the repository root", pattern)
	case strings.HasSuffix(pattern, "/"):
		return fmt.Errorf("fixes allowedPaths pattern %q must not end in a separator; append %q to allow a subtree", pattern, "**")
	case strings.Contains(pattern, "//"):
		return fmt.Errorf("fixes allowedPaths pattern %q has an empty path segment", pattern)
	case strings.ContainsAny(pattern, "\x00\n\r\\"):
		return fmt.Errorf("fixes allowedPaths pattern %q is not a plain repository path", pattern)
	case strings.ContainsAny(pattern, "?[]"):
		return fmt.Errorf(`fixes allowedPaths pattern %q may only use the wildcards "*" and "**"`, pattern)
	}
	for _, segment := range strings.Split(pattern, "/") {
		if segment == "." || segment == ".." {
			return fmt.Errorf("fixes allowedPaths pattern %q must stay inside the repository", pattern)
		}
		if strings.Contains(segment, "**") && segment != "**" {
			return fmt.Errorf(`fixes allowedPaths pattern %q must use "**" as a whole path segment`, pattern)
		}
	}
	return nil
}

// AllowsFixPath reports whether the operator's allowlist covers a repository
// path. An empty allowlist covers nothing.
func AllowsFixPath(patterns []string, path string) bool {
	if !repopath.Plain(path) {
		return false
	}
	segments := strings.Split(path, "/")
	for _, pattern := range patterns {
		if validFixPathPattern(pattern) != nil {
			continue
		}
		if matchPathSegments(strings.Split(pattern, "/"), segments) {
			return true
		}
	}
	return false
}

// Only the most recent "**" is ever reconsidered, which keeps a path with many
// segments from backtracking exponentially.
func matchPathSegments(pattern, path []string) bool {
	patternIndex, pathIndex, star, retry := 0, 0, -1, 0
	for pathIndex < len(path) {
		switch {
		case patternIndex < len(pattern) && pattern[patternIndex] == "**":
			star = patternIndex
			patternIndex++
			retry = pathIndex
		case patternIndex < len(pattern) && matchSegment(pattern[patternIndex], path[pathIndex]):
			patternIndex++
			pathIndex++
		case star >= 0:
			patternIndex = star + 1
			retry++
			pathIndex = retry
		default:
			return false
		}
	}
	for patternIndex < len(pattern) && pattern[patternIndex] == "**" {
		patternIndex++
	}
	return patternIndex == len(pattern)
}

// Matching is case-sensitive because folding case can only ever admit more paths.
func matchSegment(pattern, segment string) bool {
	patternIndex, segmentIndex, star, retry := 0, 0, -1, 0
	for segmentIndex < len(segment) {
		switch {
		case patternIndex < len(pattern) && pattern[patternIndex] == segment[segmentIndex]:
			patternIndex++
			segmentIndex++
		case patternIndex < len(pattern) && pattern[patternIndex] == '*':
			star = patternIndex
			patternIndex++
			retry = segmentIndex
		case star >= 0:
			patternIndex = star + 1
			retry++
			segmentIndex = retry
		default:
			return false
		}
	}
	for patternIndex < len(pattern) && pattern[patternIndex] == '*' {
		patternIndex++
	}
	return patternIndex == len(pattern)
}

func validateRegistries(registries []RegistryConfig) error {
	hosts := make(map[string]struct{}, len(registries))
	for _, registry := range registries {
		if !validRegistryHost(registry.Host) {
			return fmt.Errorf("registry host %q must be a bare host with an optional port", registry.Host)
		}
		if _, exists := hosts[registry.Host]; exists {
			return fmt.Errorf("duplicate registry credential for host %q", registry.Host)
		}
		hosts[registry.Host] = struct{}{}
		if !validRegistryUsername(registry.Username) {
			return fmt.Errorf("registry %q username must be printable ASCII without a colon", registry.Host)
		}
		if !environmentVariableName.MatchString(registry.PasswordEnv) {
			return fmt.Errorf("registry %q passwordEnv must be an environment variable name", registry.Host)
		}
	}
	return nil
}

// validRegistryHost mirrors the OCI adapter's authority rules so a configured
// credential can only ever key an authority the client itself would accept.
func validRegistryHost(value string) bool {
	if value == "" || len(value) > 253 || strings.ContainsAny(value, "@/?# ") {
		return false
	}
	host := value
	if candidate, port, err := net.SplitHostPort(value); err == nil {
		number, err := strconv.ParseUint(port, 10, 16)
		if candidate == "" || err != nil || number == 0 {
			return false
		}
		host = candidate
	} else if strings.Contains(value, ":") {
		return false
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.String() == host
	}
	for _, label := range strings.Split(host, ".") {
		if !registryHostLabel.MatchString(label) {
			return false
		}
	}
	return true
}

// A colon would move the Basic credential boundary, so the username may not carry one.
func validRegistryUsername(value string) bool {
	if value == "" || len(value) > 255 {
		return false
	}
	for _, r := range value {
		if r < '!' || r > '~' || r == ':' {
			return false
		}
	}
	return true
}

func validAIBaseURL(value string) bool {
	if value == "" {
		return false
	}
	parsed, err := url.Parse(value)
	if err != nil || !parsed.IsAbs() {
		return false
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return false
	}
	return parsed.Host != "" && parsed.User == nil && parsed.RawQuery == "" && parsed.Fragment == ""
}

func oneOf(value string, allowed ...string) bool {
	for _, candidate := range allowed {
		if value == candidate {
			return true
		}
	}
	return false
}
