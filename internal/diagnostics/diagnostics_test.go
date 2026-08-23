package diagnostics_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lkshrk/ops-pilot/internal/diagnostics"
)

func TestRenderErrorRedactsNestedAndJoinedErrorsByValue(t *testing.T) {
	t.Parallel()

	redactor := diagnostics.NewRedactor([]string{
		"github-secret-value",
		"openai-secret-value",
		"header-secret-value",
	})
	err := errors.Join(
		fmt.Errorf("fetch with github-secret-value: %w", errors.New("openai-secret-value rejected")),
		fmt.Errorf("health check: %w", errors.New("header-secret-value rejected")),
	)

	got := diagnostics.RenderError(err, redactor)
	for _, secret := range []string{"github-secret-value", "openai-secret-value", "header-secret-value"} {
		if strings.Contains(got, secret) {
			t.Fatalf("RenderError leaked %q in %q", secret, got)
		}
	}
	if gotCount := strings.Count(got, "[REDACTED]"); gotCount != 3 {
		t.Fatalf("RenderError redaction count = %d, want 3: %q", gotCount, got)
	}
	if !strings.Contains(got, "fetch with") || !strings.Contains(got, "health check") {
		t.Fatalf("RenderError lost error context: %q", got)
	}
}

func TestRedactorCopiesInputAndDoesNotRedactVariableNamesAlone(t *testing.T) {
	t.Parallel()

	secrets := []string{"actual-secret-value", "actual-secret-value", ""}
	redactor := diagnostics.NewRedactor(secrets)
	secrets[0] = "changed-after-construction"

	got := diagnostics.RenderError(
		errors.New("GITHUB_TOKEN actual-secret-value changed-after-construction"),
		redactor,
	)
	if got != "GITHUB_TOKEN [REDACTED] changed-after-construction" {
		t.Fatalf("RenderError() = %q", got)
	}
}

func TestRenderErrorNil(t *testing.T) {
	t.Parallel()

	if got := diagnostics.RenderError(nil, diagnostics.NewRedactor(nil)); got != "" {
		t.Fatalf("RenderError(nil) = %q, want empty", got)
	}
}

func TestLoggerWritesHumanRedactedLines(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	logger := diagnostics.NewLogger(
		&output,
		diagnostics.NewRedactor([]string{"secret-value"}),
	)
	logger.Warnf("checking %s", "secret-value")

	if got := output.String(); !strings.HasSuffix(got, " WARN  checking [REDACTED]\n") {
		t.Fatalf("log line = %q", got)
	}
}

// ops-pilot builds an HTTP basic-auth blob from the GitHub token to talk to
// git. "x-access-token:" is fifteen bytes and therefore three-byte aligned, so
// the blob contains the token's own base64 and the raw token appears nowhere in
// it - a replacer over configured values has nothing to match.
func TestARenderedErrorLosesACredentialDerivedFromAConfiguredValue(t *testing.T) {
	t.Parallel()

	const token = "ghp_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789"
	blob := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	if strings.Contains(blob, token) {
		t.Fatalf("the fixture is not the derived form: %q", blob)
	}
	redactor := diagnostics.NewRedactor([]string{token})

	got := diagnostics.RenderError(
		fmt.Errorf("fetch: git trace: header Authorization: Basic %s", blob),
		redactor,
	)
	if strings.Contains(got, blob) {
		t.Fatalf("the derived credential survived: %q", got)
	}
	if !strings.Contains(got, "fetch: git trace") {
		t.Fatalf("the rendered error lost its context: %q", got)
	}
}

// A short configured value has no invisible runes, so it rides the verbatim
// entry and the auto-derived {4,} floor must not weaken it.
func TestRedactorRedactsAShortConfiguredValueVerbatim(t *testing.T) {
	t.Parallel()

	redactor := diagnostics.NewRedactor([]string{"xy"})
	got := redactor.Redact("token xy and xy again")
	if got != "token [REDACTED] and [REDACTED] again" {
		t.Fatalf("a two-byte configured value stopped redacting: %q", got)
	}
}

func TestRedactorDoesNotNukeAShortRenderedForm(t *testing.T) {
	t.Parallel()

	// "max\u200b" renders to the three-byte "max"; below the floor it must
	// not be contributed, or it eats the word out of unrelated prose.
	redactor := diagnostics.NewRedactor([]string{"max\u200b"})
	got := redactor.Redact("exceeded max retries")
	if got != "exceeded max retries" {
		t.Fatalf("a three-byte rendered form garbled prose: %q", got)
	}
}

func TestRedactorRedactsAFourByteRenderedForm(t *testing.T) {
	t.Parallel()

	// "pass\u00adword" renders to the eight-byte "password", above the floor,
	// so the scrub-first rendered form still redacts.
	redactor := diagnostics.NewRedactor([]string{"pass\u00adword"})
	got := redactor.Redact("login with password now")
	if got != "login with [REDACTED] now" {
		t.Fatalf("a rendered form above the floor stopped redacting: %q", got)
	}
	verbatim := redactor.Redact("login with pass\u00adword now")
	if verbatim != "login with [REDACTED] now" {
		t.Fatalf("the verbatim value stopped redacting: %q", verbatim)
	}
}

// Scrubbing the rendered error changes what an operator reads on the one line
// they always read, so the ordinary failures must pass through untouched.
func TestRenderErrorLeavesAnOrdinaryFailureIntact(t *testing.T) {
	t.Parallel()

	for _, text := range []string{
		"fatal: repository 'https://github.com/org/gitops.git/' not found",
		"fatal: could not read Username for 'https://github.com': terminal prompts disabled",
		"fatal: Authentication failed for 'https://github.com/org/gitops.git/'",
		"fatal: unable to access 'https://github.com/org/gitops.git/': The requested URL returned error: 403",
		"error: pathspec 'clusters/prod/apps/media.yaml' did not match any file(s) known to git",
		"CONFLICT (content): Merge conflict in clusters/prod/apps/kustomization.yaml",
		"POST https://api.github.com/repos/org/gitops/merges: 409 Merge conflict []",
		"github: 401 Bad credentials [message: Bad credentials]",
		"GET https://ghcr.io/v2/org/app/manifests/1.2.3: 401 unauthorized",
		`Get "https://registry-1.docker.io/v2/": dial tcp: lookup registry-1.docker.io: no such host`,
		"error: cannot lock ref 'refs/heads/main': is at 9f8d7c6 but expected abc1234",
		"remote: Permission to org/gitops.git denied to ops-pilot[bot].",
		"error: git config --get http.extraHeader failed: exit status 1",
		"open /home/runner/.config/ops-pilot/config.yaml: no such file or directory",
		`config: field "github.token" is required`,
		"prerequisite: git 2.34.0 is older than the required 2.40.0",
	} {
		t.Run(text, func(t *testing.T) {
			t.Parallel()

			if got := diagnostics.RenderError(errors.New(text), diagnostics.NewRedactor(nil)); got != text {
				t.Fatalf("a plain failure was mangled\n want: %s\n  got: %s", text, got)
			}
		})
	}
}

// The log carries controller messages and model prose, so it quotes credentials
// belonging to the workload, which no configured value can match.
func TestLoggerScrubsACredentialItWasNeverConfiguredWith(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		text   string
		secret string
	}{
		{
			name:   "a service account token from a pod log",
			text:   "eyJhbGciOiJSUzI1NiIsImtpZCI6ImFiYyJ9.eyJzdWIiOiJzeXN0ZW06c2VydmljZWFjY291bnQifQ.c2lnbmF0dXJlLWJ5dGVzMDAw",
			secret: "eyJzdWIiOiJzeXN0ZW06c2VydmljZWFjY291bnQifQ",
		},
		{
			name:   "a database url a controller echoed",
			text:   "postgres://app:hunter2correcthorse@db:5432/app",
			secret: "hunter2correcthorse",
		},
		{
			name:   "a key-named value read out of a secret",
			text:   "api_key=Ai8fkq2LmZx0Rt7Yb3Nc",
			secret: "Ai8fkq2LmZx0Rt7Yb3Nc",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var output bytes.Buffer
			logger := diagnostics.NewLogger(&output, diagnostics.NewRedactor([]string{"unrelated"}))
			logger.Warnf("could not reconcile: %s", test.text)

			if got := output.String(); strings.Contains(got, test.secret) {
				t.Fatalf("the log kept %q: %s", test.secret, got)
			}
		})
	}
}

// A run lasts minutes, so the line carries the UTC time of day rather than a
// full date, and the level is padded so messages align.
func TestLoggerStampsUTCTimeAndPaddedLevel(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	moment := time.Date(2026, 8, 1, 10, 30, 0, 0, time.FixedZone("CEST", 2*60*60))
	logger := diagnostics.NewLeveledLogger(
		&output, nil, diagnostics.LevelInfo, func() time.Time { return moment },
	)
	logger.Warnf("disk %s", "full")

	if got := output.String(); got != "2026-08-01 08:30:00Z WARN  disk full\n" {
		t.Fatalf("log line = %q", got)
	}
}

func TestLoggerFiltersBelowItsLevel(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name  string
		level diagnostics.Level
		want  []string
	}{
		{name: "debug", level: diagnostics.LevelDebug, want: []string{"debug", "info", "warn"}},
		{name: "info", level: diagnostics.LevelInfo, want: []string{"info", "warn"}},
		{name: "warn", level: diagnostics.LevelWarn, want: []string{"warn"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var output bytes.Buffer
			logger := diagnostics.NewLeveledLogger(&output, nil, test.level, nil)
			logger.Debugf("at %s", "debug")
			logger.Infof("at %s", "info")
			logger.Warnf("at %s", "warn")

			lines := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
			if len(lines) != len(test.want) {
				t.Fatalf("lines = %#v, want %d", lines, len(test.want))
			}
			for i, want := range test.want {
				label := strings.ToUpper(want)
				if !strings.Contains(lines[i], " "+label+" ") || !strings.HasSuffix(lines[i], "at "+want) {
					t.Fatalf("line %d = %q, want level %q", i, lines[i], want)
				}
			}
		})
	}
}

func TestParseLevelRejectsUnknownNames(t *testing.T) {
	t.Parallel()

	if level, ok := diagnostics.ParseLevel("debug"); !ok || level != diagnostics.LevelDebug {
		t.Fatalf("ParseLevel(debug) = %v, %v", level, ok)
	}
	if level, ok := diagnostics.ParseLevel("trace"); ok || level != diagnostics.LevelInfo {
		t.Fatalf("ParseLevel(trace) = %v, %v", level, ok)
	}
}

func TestCheckPrerequisitesAcceptsGitExactlyAtTheFloor(t *testing.T) {
	t.Parallel()

	git := versionExecutable(t, "git version 2.38.0")

	if err := diagnostics.CheckPrerequisites(context.Background(), git); err != nil {
		t.Fatalf("CheckPrerequisites() error = %v", err)
	}
}

// A prerequisite ops-pilot never invokes still refuses to start the run, so
// PATH holds git and nothing else: a second executable added to the check has
// nowhere to resolve from and this goes red.
func TestGitIsTheOnlyExecutableThePrerequisiteCheckLooksUp(t *testing.T) {
	directory := t.TempDir()
	writeExecutable(t, filepath.Join(directory, "git"), "#!/bin/sh\nprintf '%s\\n' 'git version 2.45.1'\n")
	t.Setenv("PATH", directory)

	if err := diagnostics.CheckPrerequisites(context.Background(), ""); err != nil {
		t.Fatalf("CheckPrerequisites() error = %v", err)
	}
}

func TestCheckPrerequisitesDoesNotForwardAmbientEnvironment(t *testing.T) {
	t.Setenv("HOME", "/attacker/home")
	t.Setenv("SSH_AUTH_SOCK", "/attacker/socket")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "secret")

	git := isolatedVersionExecutable(t, "git version 2.38.0")
	if err := diagnostics.CheckPrerequisites(context.Background(), git); err != nil {
		t.Fatalf("CheckPrerequisites() error = %v", err)
	}
}

func TestCheckPrerequisitesAcceptsNewerVendorGitVersion(t *testing.T) {
	t.Parallel()

	git := versionExecutable(t, "git version 2.44.0.windows.1")

	if err := diagnostics.CheckPrerequisites(context.Background(), git); err != nil {
		t.Fatalf("CheckPrerequisites() error = %v", err)
	}
}

func TestCheckPrerequisitesRejectsGitBelowTheFloor(t *testing.T) {
	t.Parallel()

	git := versionExecutable(t, "git version 2.37.9")

	err := diagnostics.CheckPrerequisites(context.Background(), git)
	if err == nil {
		t.Fatal("CheckPrerequisites() unexpectedly succeeded")
	}
	for _, text := range []string{"Git 2.38", "2.37.9", "upgrade"} {
		if !strings.Contains(err.Error(), text) {
			t.Fatalf("error %q lacks %q", err, text)
		}
	}
}

func TestCheckPrerequisitesMissingGitIsActionable(t *testing.T) {
	t.Parallel()

	missing := filepath.Join(t.TempDir(), "missing-git")
	err := diagnostics.CheckPrerequisites(context.Background(), missing)
	if err == nil {
		t.Fatal("CheckPrerequisites() unexpectedly succeeded")
	}
	for _, text := range []string{"Git 2.38", "install", missing} {
		if !strings.Contains(err.Error(), text) {
			t.Fatalf("error %q lacks %q", err, text)
		}
	}
}

func TestCheckPrerequisitesRejectsMalformedVersionOutputWithoutEchoingIt(t *testing.T) {
	t.Parallel()

	const unsafeOutput = "not-a-version secret-looking-output"
	git := versionExecutable(t, unsafeOutput)

	err := diagnostics.CheckPrerequisites(context.Background(), git)
	if err == nil {
		t.Fatal("CheckPrerequisites() unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), unsafeOutput) {
		t.Fatalf("error echoed untrusted command output: %q", err)
	}
	if !strings.Contains(err.Error(), "Git 2.38") || !strings.Contains(err.Error(), "version output") {
		t.Fatalf("error is not actionable: %q", err)
	}
}

func TestCheckPrerequisitesPreservesCancellation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := diagnostics.CheckPrerequisites(ctx, "git")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CheckPrerequisites() error = %v, want context cancellation", err)
	}
}

func versionExecutable(t *testing.T, output string) string {
	t.Helper()

	script := "#!/bin/sh\nif [ \"$1\" != \"--version\" ]; then exit 97; fi\n" +
		"printf '%s\\n' " + shellQuote(output) + "\n"
	path := filepath.Join(t.TempDir(), "version-tool")
	writeExecutable(t, path, script)
	return path
}

func isolatedVersionExecutable(t *testing.T, output string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "version-tool")
	writeExecutable(t, path, "#!/bin/sh\n"+
		"[ \"$LC_ALL\" = C ] || exit 91\n"+
		"[ -z \"$HOME$SSH_AUTH_SOCK$AWS_SECRET_ACCESS_KEY\" ] || exit 92\n"+
		"printf '%s\\n' "+shellQuote(output)+"\n")
	return path
}

func writeExecutable(t *testing.T, path, script string) {
	t.Helper()

	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write executable: %v", err)
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

// The one security-relevant behaviour of the handler had no coverage.
func TestLoggerStripsTerminalControlFromUntrustedText(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	logger := diagnostics.NewLeveledLogger(&output, nil, diagnostics.LevelWarn, nil)
	logger.Warnf("registry said %s", "fine\x1b[2K\rIMPOSTOR\x07")

	got := output.String()
	if strings.ContainsAny(got, "\x1b\x07\r") {
		t.Fatalf("control characters survived: %q", got)
	}
	if !strings.Contains(got, "IMPOSTOR") {
		t.Fatalf("the text itself must survive, only its control codes removed: %q", got)
	}
}

// Folded first, the outer key claims the inner label as its value and the secret behind it stands.
var nestedCredentialBlocks = []struct {
	name   string
	text   string
	secret string
}{
	{
		name:   "a password nested under a credentials key",
		text:   "credentials:\n  password: hunter2correcthorse",
		secret: "hunter2correcthorse",
	},
	{
		name:   "a password nested under an auth_token key",
		text:   "auth_token:\n  password: s3cr3tvalue0000",
		secret: "s3cr3tvalue0000",
	},
	{
		name:   "a client_secret nested under a credentials key",
		text:   "credentials:\r\n  client_secret: Ai8fkq2LmZx0Rt7Yb3Nc",
		secret: "Ai8fkq2LmZx0Rt7Yb3Nc",
	},
}

func TestARenderedErrorScrubsBeforeItFoldsTheNewlinesTheKeyRulesRead(t *testing.T) {
	t.Parallel()

	for _, test := range nestedCredentialBlocks {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			got := diagnostics.RenderError(errors.New(test.text), diagnostics.NewRedactor([]string{"unrelated"}))
			if strings.Contains(got, test.secret) {
				t.Fatalf("the nested secret survived: %q", got)
			}
			if !strings.Contains(got, "[REDACTED]") {
				t.Fatalf("nothing was redacted at all: %q", got)
			}
		})
	}
}

func TestTheLogScrubsBeforeItFoldsTheNewlinesTheKeyRulesRead(t *testing.T) {
	t.Parallel()

	for _, test := range nestedCredentialBlocks {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var output bytes.Buffer
			logger := diagnostics.NewLogger(&output, diagnostics.NewRedactor([]string{"unrelated"}))
			logger.Warnf("could not apply: %s", test.text)

			got := output.String()
			if strings.Contains(got, test.secret) {
				t.Fatalf("the nested secret survived: %q", got)
			}
			if !strings.Contains(got, "[REDACTED]") {
				t.Fatalf("nothing was redacted at all: %q", got)
			}
		})
	}
}

func TestHandlerIsSafeForConcurrentUse(t *testing.T) {
	t.Parallel()

	var (
		output bytes.Buffer
		mu     sync.Mutex
		wait   sync.WaitGroup
	)
	logger := diagnostics.NewLeveledLogger(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return output.Write(p)
	}), nil, diagnostics.LevelWarn, nil)

	for i := 0; i < 50; i++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			logger.Warnf("line")
		}()
	}
	wait.Wait()

	if lines := strings.Count(output.String(), "\n"); lines != 50 {
		t.Fatalf("want 50 lines, got %d", lines)
	}
}

type writerFunc func([]byte) (int, error)

func (w writerFunc) Write(p []byte) (int, error) { return w(p) }
