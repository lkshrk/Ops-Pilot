package docs_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/lkshrk/ops-pilot/internal/cli"
	"github.com/lkshrk/ops-pilot/internal/config"
	"gopkg.in/yaml.v3"
)

func projectRoot(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(filepath.Join(directory, "..", ".."))
}

func read(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(content)
}

func page(t *testing.T, name ...string) string {
	t.Helper()
	return read(t, filepath.Join(append([]string{projectRoot(t)}, name...)...))
}

// documents is the shipped pages only: the glob is non-recursive, so docs/review and docs/superpowers are out.
func documents(t *testing.T) []string {
	t.Helper()
	pages, err := filepath.Glob(filepath.Join(projectRoot(t), "docs", "*.md"))
	if err != nil {
		t.Fatal(err)
	}
	return append([]string{filepath.Join(projectRoot(t), "README.md")}, pages...)
}

var (
	anchoredLink = regexp.MustCompile(`\]\(([^)#\s]*)#([^)\s]+)\)`)
	heading      = regexp.MustCompile(`(?m)^#{1,6}[ \t]+(.+?)[ \t]*$`)
	slugDrop     = regexp.MustCompile(`[^a-z0-9 -]`)
)

func slug(title string) string {
	lowered := strings.ToLower(title)
	return strings.ReplaceAll(slugDrop.ReplaceAllString(lowered, ""), " ", "-")
}

var helpFlag = regexp.MustCompile(`(?m)^\s{2,}(?:-[a-zA-Z], )?--([a-z][a-z-]*)`)

func helpText(t *testing.T, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	code := cli.Execute(
		context.Background(),
		args,
		cli.CommandDependencies{
			Stdin:              strings.NewReader(""),
			Stdout:             &out,
			Stderr:             &out,
			CheckPrerequisites: func(context.Context, string) error { return nil },
		},
		cli.VersionInfo{},
	)
	if code != 0 {
		t.Fatalf("%v exited %d: %s", args, code, out.String())
	}
	return out.String()
}

func TestEveryCommandFlagAppearsInBothCLISynopsisDocuments(t *testing.T) {
	pages := map[string]string{
		"README.md":   page(t, "README.md"),
		"docs/cli.md": page(t, "docs", "cli.md"),
	}
	for _, command := range []string{"run", "history"} {
		for _, match := range helpFlag.FindAllStringSubmatch(helpText(t, command, "--help"), -1) {
			name := match[1]
			if name == "help" {
				continue
			}
			flagBoundary := regexp.MustCompile("--" + regexp.QuoteMeta(name) + `([^a-z-]|$)`)
			for page, content := range pages {
				if !flagBoundary.MatchString(content) {
					t.Errorf("%s never mentions %s's --%s", page, command, name)
				}
			}
		}
	}
}

// The README is the thirty-second read; anything longer belongs behind a link.
func TestTheReadmeStaysShortEnoughToRead(t *testing.T) {
	const limit = 120
	if lines := strings.Count(page(t, "README.md"), "\n"); lines > limit {
		t.Errorf("README.md is %d lines, want at most %d", lines, limit)
	}
}

func TestTheReadmeNamesBothDurableLabels(t *testing.T) {
	readme := page(t, "README.md")
	for _, want := range []string{
		config.DefaultRevertedLabel,
		config.DefaultDeclinedLabel,
		"pullRequests.revertedLabel",
		"pullRequests.declinedLabel",
	} {
		if !strings.Contains(readme, want) {
			t.Errorf("README.md never mentions %q", want)
		}
	}
	if strings.Contains(readme, "except one GitHub label") {
		t.Error("README.md still claims a single label changes what a later run does")
	}
}

// containsRow finds one documentation table row naming a key and its value,
// rather than the two strings anywhere in the page.
func containsRow(document, key, value string) bool {
	for _, line := range strings.Split(document, "\n") {
		if strings.Contains(line, "`"+key+"`") && strings.Contains(line, "`"+value+"`") {
			return true
		}
	}
	return false
}

func TestEveryConfigurationDefaultIsDocumentedBesideItsKey(t *testing.T) {
	reference := page(t, "docs", "configuration.md")
	for _, row := range []struct{ key, value string }{
		{"tokenEnv", config.DefaultGitHubTokenEnv},
		{"mergeMethod", config.DefaultMergeMethod},
		{"apiKeyEnv", config.DefaultOpenAIKeyEnv},
		{"baseURL", config.DefaultOpenAIBaseURL},
		{"revertedLabel", config.DefaultRevertedLabel},
		{"declinedLabel", config.DefaultDeclinedLabel},
		{"level", config.DefaultLoggingLevel},
		{"source.kind", "GitRepository"},
	} {
		if !containsRow(reference, row.key, row.value) {
			t.Errorf("docs/configuration.md documents no %q default of %q", row.key, row.value)
		}
	}
}

// The documented durations are spelled for an operator, so each is parsed back
// and compared with the constant rather than matched against its Go form.
func TestEveryWatchDefaultIsDocumentedAndMeansWhatTheCodeMeans(t *testing.T) {
	reference := page(t, "docs", "configuration.md")
	for _, row := range []struct {
		key, documented string
		want            time.Duration
	}{
		{"settleTimeout", "10m", config.DefaultSettleTimeout},
		{"stabilityHold", "1m", config.DefaultStabilityHold},
		{"pollInterval", "10s", config.DefaultPollInterval},
	} {
		parsed, err := time.ParseDuration(row.documented)
		if err != nil {
			t.Fatal(err)
		}
		if parsed != row.want {
			t.Errorf("docs say %s is %s, the code says %s", row.key, row.documented, row.want)
		}
		if !containsRow(reference, row.key, row.documented) {
			t.Errorf("docs/configuration.md documents no %q default of %q", row.key, row.documented)
		}
	}
	if !containsRow(reference, "maxFixAttempts", "2") || config.DefaultMaxFixAttempts != 2 {
		t.Errorf("the documented maxFixAttempts default does not match %d", config.DefaultMaxFixAttempts)
	}
}

func TestBothDefaultPathLayoutsAreDocumented(t *testing.T) {
	reference := page(t, "docs", "configuration.md")
	for _, layout := range []struct{ goos, home, state, cache string }{
		{goos: "darwin", home: "$HOME"},
		{goos: "linux", home: "$HOME", state: "$XDG_STATE_HOME", cache: "$XDG_CACHE_HOME"},
		{goos: "linux", home: "$HOME"},
	} {
		paths, err := config.DefaultPaths(layout.goos, layout.home, layout.state, layout.cache)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{paths.HistoryDatabase, paths.CheckoutDirectory} {
			if !strings.Contains(reference, want) {
				t.Errorf("docs/configuration.md never gives the %s default %q", layout.goos, want)
			}
		}
	}
}

func TestEveryEnvExampleNameIsReferencedByTheShippedConfiguration(t *testing.T) {
	example := page(t, "configs", "ops-pilot.example.yaml")
	reference := page(t, "docs", "configuration.md")
	for _, line := range strings.Split(page(t, ".env.example"), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		name, _, ok := strings.Cut(trimmed, "=")
		if !ok {
			t.Errorf(".env.example line %q is not NAME=value", line)
			continue
		}
		if !strings.Contains(example, name) && !strings.Contains(reference, "`"+name+"`") {
			t.Errorf(".env.example lists %s, which neither the example configuration nor the environment table references", name)
		}
	}
}

var plainLink = regexp.MustCompile(`\]\(([^)#\s]+)\)`)

// An anchored link is checked against a heading below; this is the other half,
// so a moved or renamed file cannot be linked from a shipped page.
func TestEveryPlainDocumentationLinkResolvesToAFile(t *testing.T) {
	for _, page := range documents(t) {
		for _, link := range plainLink.FindAllStringSubmatch(read(t, page), -1) {
			target := link[1]
			if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
				continue
			}
			decoded, err := url.PathUnescape(target)
			if err != nil {
				t.Errorf("%s links to %q, which is not a usable path: %v", page, target, err)
				continue
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(page), decoded)); err != nil {
				t.Errorf("%s links to %q, which does not exist", page, target)
			}
		}
	}
}

func TestEveryAnchoredDocumentationLinkResolvesToAHeading(t *testing.T) {
	for _, page := range documents(t) {
		for _, link := range anchoredLink.FindAllStringSubmatch(read(t, page), -1) {
			target, anchor := link[1], link[2]
			resolved := page
			if target != "" {
				resolved = filepath.Join(filepath.Dir(page), target)
			}
			if filepath.Ext(resolved) != ".md" {
				continue
			}
			if _, err := os.Stat(resolved); err != nil {
				t.Errorf("%s links to %q#%s, which does not exist", page, target, anchor)
				continue
			}
			found := false
			for _, title := range heading.FindAllStringSubmatch(read(t, resolved), -1) {
				if slug(title[1]) == anchor {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("%s links to %q#%s, which no heading in %s anchors",
					page, target, anchor, resolved)
			}
		}
	}
}

// Each of the three references is reachable from the README, and none of them
// is the deleted page a reader may still have bookmarked.
func TestTheReadmeLinksEveryReferencePage(t *testing.T) {
	readme := page(t, "README.md")
	for _, reference := range []string{"docs/install.md", "docs/configuration.md", "docs/cli.md"} {
		if _, err := os.Stat(filepath.Join(projectRoot(t), reference)); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(readme, "("+reference+")") {
			t.Errorf("README.md never links %s", reference)
		}
	}
	for _, document := range documents(t) {
		if strings.Contains(read(t, document), "installation.md") {
			t.Errorf("%s still links the removed installation page", document)
		}
	}
}

var (
	typedResource = regexp.MustCompile(`(?:AppsV1|CoreV1|BatchV1)\(\)\.([A-Za-z]+)\(`)
	rbacResources = regexp.MustCompile(`resources:\s*\[([^\]]*)\]`)
)

func TestEveryTypedKubernetesResourceReadIsGrantedByTheRBACManifest(t *testing.T) {
	root := projectRoot(t)
	granted := map[string]bool{}
	manifest := read(t, filepath.Join(root, "docs", "rbac.yaml"))
	for _, list := range rbacResources.FindAllStringSubmatch(manifest, -1) {
		for _, resource := range strings.Split(list[1], ",") {
			granted[strings.TrimSpace(resource)] = true
		}
	}
	sources, err := filepath.Glob(filepath.Join(root, "internal", "adapters", "kubernetes", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range sources {
		if strings.HasSuffix(source, "_test.go") {
			continue
		}
		content := read(t, source)
		for _, call := range typedResource.FindAllStringSubmatch(content, -1) {
			if resource := strings.ToLower(call[1]); !granted[resource] {
				t.Errorf("%s reads %s, which docs/rbac.yaml does not grant",
					filepath.Base(source), resource)
			}
		}
		if strings.Contains(content, ".GetLogs(") && !granted["pods/log"] {
			t.Errorf("%s reads pod logs, which docs/rbac.yaml does not grant",
				filepath.Base(source))
		}
	}
}

type rbacRule struct {
	APIGroups []string `yaml:"apiGroups"`
	Resources []string `yaml:"resources"`
	Verbs     []string `yaml:"verbs"`
}

func rbacRules(t *testing.T) []rbacRule {
	t.Helper()
	file, err := os.Open(filepath.Join(projectRoot(t), "docs", "rbac.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()

	var rules []rbacRule
	decoder := yaml.NewDecoder(file)
	for {
		var document struct {
			Rules []rbacRule `yaml:"rules"`
		}
		switch err := decoder.Decode(&document); {
		case errors.Is(err, io.EOF):
			return rules
		case err != nil:
			t.Fatal(err)
		}
		rules = append(rules, document.Rules...)
	}
}

// probe is the `kubectl auth can-i` spelling of one granted verb.
func probe(group, resource, verb string) string {
	switch {
	case resource == "pods/log":
		return verb + " pods --subresource=log"
	case group == "":
		return verb + " " + resource
	default:
		return verb + " " + resource + "." + group
	}
}

func TestEveryRBACGrantIsProbedByTheInstallationVerification(t *testing.T) {
	install := page(t, "docs", "install.md")
	for _, rule := range rbacRules(t) {
		for _, group := range rule.APIGroups {
			for _, resource := range rule.Resources {
				for _, verb := range rule.Verbs {
					want := probe(group, resource, verb)
					if !strings.Contains(install, want) {
						t.Errorf("docs/rbac.yaml grants %q, which the verification section never probes", want)
					}
				}
			}
		}
	}
}

// The page wraps its prose, so the phrases are matched against it unwrapped.
func TestTheConfigurationReferencePinsEachAuthorizationFailureOutcome(t *testing.T) {
	reference := strings.Join(strings.Fields(page(t, "docs", "configuration.md")), " ")
	for _, want := range []string{
		"reading a single pull request stops the run",
		"the changelog degrades to unreadable",
		"that pull request is left open",
	} {
		if !strings.Contains(reference, want) {
			t.Errorf("docs/configuration.md never says %q", want)
		}
	}
}

func TestTheRBACVerificationNeverPinsTheFluxNamespace(t *testing.T) {
	for _, line := range strings.Split(page(t, "docs", "install.md"), "\n") {
		if strings.Contains(line, "auth can-i") && strings.Contains(line, "flux-system") {
			t.Errorf("%q pins the Flux namespace instead of reading flux.source.namespace", line)
		}
	}
}

// clarificationEscapes is the one `case` arm in Clarify that leaves a pull
// request pending without sending the line to the agent.
var clarificationEscapes = regexp.MustCompile(`(?m)^\s*case ((?:"[^"]*", )*"/skip"):`)

func TestTheDocumentedDiscussionEscapesAreTheOnesTheApproverAccepts(t *testing.T) {
	source := page(t, "internal", "cli", "approver.go")
	arm := clarificationEscapes.FindStringSubmatch(source)
	if arm == nil {
		t.Fatal("internal/cli/approver.go no longer has a /skip escape arm to read")
	}
	discussion := strings.Join(strings.Fields(page(t, "docs", "cli.md")), " ")
	for _, token := range regexp.MustCompile(`"([^"]*)"`).FindAllStringSubmatch(arm[1], -1) {
		// The empty line and the escape byte are documented as Enter and Esc.
		if token[1] == "" || token[1] == `\x1b` {
			continue
		}
		if !strings.Contains(discussion, "`"+token[1]+"`") {
			t.Errorf("the approver accepts %q as a local escape, which docs/cli.md never names", token[1])
		}
	}
	if !strings.Contains(discussion, "any letter case") {
		t.Error("docs/cli.md does not say the escapes are matched case-insensitively")
	}
}

func TestTheDocumentedExitCodesCoverWhatTheCLIReturns(t *testing.T) {
	reference := page(t, "docs", "cli.md")
	for _, code := range []string{"0", "1", "2", "130"} {
		if !strings.Contains(reference, "| `"+code+"` |") {
			t.Errorf("the exit status table has no row for %s", code)
		}
	}
	for _, want := range []struct {
		code int
		args []string
	}{
		{code: 0, args: []string{"version"}},
		{code: 1, args: []string{"--quiet", "--verbose", "version"}},
	} {
		var out bytes.Buffer
		got := cli.Execute(
			context.Background(),
			want.args,
			cli.CommandDependencies{Stdin: strings.NewReader(""), Stdout: &out, Stderr: &out},
			cli.VersionInfo{},
		)
		if got != want.code {
			t.Errorf("%v exited %d, which docs/cli.md documents as %d: %s", want.args, got, want.code, out.String())
		}
	}
}
