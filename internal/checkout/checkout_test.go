package checkout

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lkshrk/ops-pilot/internal/domain"
)

const testToken = "ghs_notARealTokenJustLongEnough"

type invocation struct {
	args []string
	env  []string
}

func (i invocation) argv() string { return strings.Join(i.args, "\x00") }

func (i invocation) environ() string { return strings.Join(i.env, "\x00") }

// subcommand drops the leading `-c <value>` pairs git accepts before the verb.
func (i invocation) subcommand() []string {
	args := i.args
	for len(args) >= 2 && args[0] == "-c" {
		args = args[2:]
	}
	return args
}

func (i invocation) lookup(key string) (string, bool) {
	for _, entry := range i.env {
		if name, value, found := strings.Cut(entry, "="); found && name == key {
			return value, true
		}
	}
	return "", false
}

// recordingGit replaces the git executable with a script that appends its own
// argv and environment to a file and exits successfully. A recorded clone also
// creates its target so the fetch that follows has somewhere to run.
func recordingGit(t *testing.T) (string, func() []invocation) {
	t.Helper()
	dir := t.TempDir()
	record := filepath.Join(dir, "record")
	path := filepath.Join(dir, "git")
	script := "#!/bin/sh\n" +
		"{ echo INVOCATION; for a in \"$@\"; do echo \"ARG $a\"; done; env | sed 's/^/ENV /'; } >> " + record + "\n" +
		"clone=0\n" +
		"for a in \"$@\"; do [ \"$a\" = clone ] && clone=1; last=$a; done\n" +
		"[ \"$clone\" = 1 ] && mkdir -p \"$last/.git\"\n" +
		"exit 0\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake git: %v", err)
	}
	return path, func() []invocation {
		raw, err := os.ReadFile(record)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			t.Fatalf("read fake git record: %v", err)
		}
		var got []invocation
		for _, line := range strings.Split(string(raw), "\n") {
			switch {
			case line == "INVOCATION":
				got = append(got, invocation{})
			case strings.HasPrefix(line, "ARG "):
				got[len(got)-1].args = append(got[len(got)-1].args, strings.TrimPrefix(line, "ARG "))
			case strings.HasPrefix(line, "ENV "):
				got[len(got)-1].env = append(got[len(got)-1].env, strings.TrimPrefix(line, "ENV "))
			}
		}
		return got
	}
}

// Whether the root already holds a .git is what selects the fetch path over the
// clone path, so the two fixtures cannot be one with a flag.
func seededCheckout(t *testing.T, token string) (*Checkout, func() []invocation) {
	t.Helper()
	git, recorded := recordingGit(t)
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".git"), 0o700); err != nil {
		t.Fatalf("seed checkout: %v", err)
	}
	c := New(root, token, domain.RepositoryRef{Owner: "acme", Name: "gitops"})
	c.executable = git
	return c, recorded
}

func cloningCheckout(t *testing.T, token string) (*Checkout, func() []invocation) {
	t.Helper()
	git, recorded := recordingGit(t)
	c := New(filepath.Join(t.TempDir(), "checkout"), token, domain.RepositoryRef{Owner: "acme", Name: "gitops"})
	c.executable = git
	return c, recorded
}

func TestTokenNeverAppearsInGitProcessArguments(t *testing.T) {
	c, recorded := seededCheckout(t, testToken)

	if err := c.SyncPullRequest(context.Background(), 7); err != nil {
		t.Fatalf("SyncPullRequest: %v", err)
	}

	invocations := recorded()
	if len(invocations) != 2 {
		t.Fatalf("recorded %d git invocations, want 2", len(invocations))
	}
	for _, got := range invocations {
		if strings.Contains(got.argv(), testToken) || strings.Contains(got.argv(), basic(testToken)) {
			t.Errorf("token is readable in git argv: %q", got.args)
		}
	}
}

func TestTokenReachesGitAsAnExactlyCountedConfigEnvironment(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "")
	c, recorded := cloningCheckout(t, testToken)

	if err := c.SyncBranch(context.Background(), "main"); err != nil {
		t.Fatalf("SyncBranch: %v", err)
	}

	invocations := recorded()
	if len(invocations) != 3 {
		t.Fatalf("recorded %d git invocations, want clone, fetch and checkout", len(invocations))
	}
	clone, fetch := invocations[0], invocations[1]

	for name, got := range map[string]invocation{"clone": clone, "fetch": fetch} {
		count, ok := got.lookup("GIT_CONFIG_COUNT")
		if !ok || count != "1" {
			t.Errorf("%s: GIT_CONFIG_COUNT is %q, want \"1\"", name, count)
		}
		if key, _ := got.lookup("GIT_CONFIG_KEY_0"); key != "http.https://github.com/.extraheader" {
			t.Errorf("%s: GIT_CONFIG_KEY_0 is %q", name, key)
		}
		if value, _ := got.lookup("GIT_CONFIG_VALUE_0"); value != "Authorization: Basic "+basic(testToken) {
			t.Errorf("%s: GIT_CONFIG_VALUE_0 does not carry the credential", name)
		}
		if key, _ := got.lookup("GIT_CONFIG_KEY_1"); key == "http.https://github.com/.extraheader" {
			t.Errorf("%s: the credential is also at index 1, past GIT_CONFIG_COUNT", name)
		}
	}
}

// A blobless checkout can trigger a promisor fetch, so it must authenticate too.
func TestTheCheckoutInvocationAlsoCarriesTheCredential(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "")
	c, recorded := seededCheckout(t, testToken)

	if err := c.SyncPullRequest(context.Background(), 7); err != nil {
		t.Fatalf("SyncPullRequest: %v", err)
	}

	invocations := recorded()
	checkout := invocations[len(invocations)-1]
	if checkout.subcommand()[0] != "checkout" {
		t.Fatalf("last invocation is not the checkout: %q", checkout.args)
	}
	if key, _ := checkout.lookup("GIT_CONFIG_KEY_0"); key != "http.https://github.com/.extraheader" {
		t.Errorf("the checkout is not given the authorization header override: GIT_CONFIG_KEY_0 is %q", key)
	}
	if value, _ := checkout.lookup("GIT_CONFIG_VALUE_0"); value != "Authorization: Basic "+basic(testToken) {
		t.Errorf("the checkout credential does not carry the token: %q", value)
	}
	if strings.Contains(checkout.argv(), testToken) || strings.Contains(checkout.argv(), basic(testToken)) {
		t.Errorf("the token is readable in the checkout argv: %q", checkout.args)
	}
}

func TestAnEmptyTokenSetsNoConfigEnvironmentAtAll(t *testing.T) {
	c, recorded := cloningCheckout(t, "")

	if err := c.SyncBranch(context.Background(), "main"); err != nil {
		t.Fatalf("SyncBranch: %v", err)
	}

	for _, got := range recorded() {
		if _, ok := got.lookup("GIT_CONFIG_COUNT"); ok {
			t.Errorf("GIT_CONFIG_COUNT is set for an unauthenticated checkout: %q", got.args)
		}
	}
}

func TestFetchSeparatesTheRefFromTheOptions(t *testing.T) {
	for _, tc := range []struct {
		name string
		sync func(*Checkout) error
		ref  string
	}{
		{"branch", func(c *Checkout) error { return c.SyncBranch(context.Background(), "main") }, "main"},
		{"pull request", func(c *Checkout) error { return c.SyncPullRequest(context.Background(), 7) }, "pull/7/head"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, recorded := seededCheckout(t, testToken)

			if err := tc.sync(c); err != nil {
				t.Fatalf("sync: %v", err)
			}

			fetch := recorded()[0].subcommand()
			if fetch[0] != "fetch" {
				t.Fatalf("first invocation is not the fetch: %q", fetch)
			}
			separator := -1
			for i, arg := range fetch {
				if arg == "--" {
					separator = i
				}
			}
			if separator < 0 {
				t.Fatalf("fetch does not separate its operands from its options: %q", fetch)
			}
			if got := fetch[separator+1:]; len(got) != 2 || got[0] != "origin" || got[1] != tc.ref {
				t.Errorf("operands after -- are %q, want [origin %s]", got, tc.ref)
			}
		})
	}
}

func TestCloneFetchAndCheckoutWorkAgainstALocalRemoteWithoutStoringTheToken(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	base := t.TempDir()
	remote := filepath.Join(base, "remote")
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command(git, args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}

	global := filepath.Join(base, "gitconfig")
	if err := os.WriteFile(global, []byte("[url \""+remote+"\"]\n\tinsteadOf = https://github.com/acme/gitops.git\n"), 0o600); err != nil {
		t.Fatalf("write global config: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_CONFIG_SYSTEM", filepath.Join(base, "absent"))
	t.Setenv("GIT_CONFIG_COUNT", "")

	if err := os.MkdirAll(remote, 0o700); err != nil {
		t.Fatalf("create remote: %v", err)
	}
	run(remote, "init", "-q", "-b", "main", ".")
	run(remote, "config", "uploadpack.allowFilter", "true")
	if err := os.WriteFile(filepath.Join(remote, "a.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatalf("seed remote: %v", err)
	}
	run(remote, "add", "a.txt")
	run(remote, "commit", "-qm", "one")

	root := filepath.Join(base, "work")
	c := New(root, testToken, domain.RepositoryRef{Owner: "acme", Name: "gitops"})
	if err := c.SyncBranch(context.Background(), "main"); err != nil {
		t.Fatalf("SyncBranch after clone: %v", err)
	}
	if got := readFile(t, filepath.Join(root, "a.txt")); got != "one\n" {
		t.Errorf("checked out %q, want %q", got, "one\n")
	}

	if err := os.WriteFile(filepath.Join(remote, "a.txt"), []byte("two\n"), 0o600); err != nil {
		t.Fatalf("update remote: %v", err)
	}
	run(remote, "commit", "-qam", "two")
	head := run(remote, "rev-parse", "HEAD")
	run(remote, "update-ref", "refs/pull/9/head", head)

	if err := c.SyncPullRequest(context.Background(), 9); err != nil {
		t.Fatalf("SyncPullRequest: %v", err)
	}
	if got := readFile(t, filepath.Join(root, "a.txt")); got != "two\n" {
		t.Errorf("checked out %q, want %q", got, "two\n")
	}

	config := run(root, "config", "--list", "--show-origin")
	if strings.Contains(config, testToken) || strings.Contains(config, basic(testToken)) {
		t.Errorf("the token reached the checkout's stored config:\n%s", config)
	}
}

func TestTheOperatorsAmbientGitConfigSurvivesAlongsideTheCredential(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("GIT_CONFIG_COUNT", "2")
	t.Setenv("GIT_CONFIG_KEY_0", "http.proxy")
	t.Setenv("GIT_CONFIG_VALUE_0", "http://proxy.corp.invalid:8080")
	t.Setenv("GIT_CONFIG_KEY_1", "http.sslVerify")
	t.Setenv("GIT_CONFIG_VALUE_1", "false")

	c, recorded := seededCheckout(t, testToken)

	if err := c.SyncBranch(context.Background(), "main"); err != nil {
		t.Fatalf("SyncBranch: %v", err)
	}

	fetch := recorded()[0]
	if got, _ := fetch.lookup("GIT_CONFIG_COUNT"); got != "3" {
		t.Errorf("GIT_CONFIG_COUNT is %q, want \"3\"", got)
	}
	if got, _ := fetch.lookup("GIT_CONFIG_KEY_2"); got != "http.https://github.com/.extraheader" {
		t.Errorf("the credential was not appended at index 2, GIT_CONFIG_KEY_2 is %q", got)
	}

	// Ask real git what it resolves from exactly the environment the production
	// path built, rather than from a re-derivation of it.
	cmd := exec.Command(realGit, "config", "--list")
	cmd.Env = fetch.env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git config --list under the production environment: %v: %s", err, out)
	}
	for _, want := range []string{
		"http.proxy=http://proxy.corp.invalid:8080",
		"http.sslverify=false",
		"http.https://github.com/.extraheader=Authorization: Basic " + basic(testToken),
	} {
		if !strings.Contains(string(out), want) {
			t.Errorf("git does not see %q:\n%s", want, out)
		}
	}
}

func TestAmbientGitConfigCountDecidesWhereTheCredentialIsAppended(t *testing.T) {
	for _, tc := range []struct {
		name    string
		count   string
		set     bool
		index   string
		total   string
		halting bool
	}{
		{name: "unset", index: "0", total: "1"},
		{name: "empty is no config at all to git", count: "", set: true, index: "0", total: "1"},
		{name: "zero", count: "0", set: true, index: "0", total: "1"},
		{name: "two operator pairs", count: "2", set: true, index: "2", total: "3"},
		{name: "whitespace git itself would reject", count: " 2\n", set: true, index: "2", total: "3"},
		{name: "trailing space git itself would reject", count: "2 ", set: true, index: "2", total: "3"},
		{name: "not a number", count: "two", set: true, halting: true},
		{name: "negative", count: "-1", set: true, halting: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("GIT_CONFIG_COUNT", tc.count)
			if !tc.set {
				os.Unsetenv("GIT_CONFIG_COUNT")
			}
			c, recorded := seededCheckout(t, testToken)

			err := c.SyncBranch(context.Background(), "main")
			if tc.halting {
				if err == nil {
					t.Fatalf("a malformed GIT_CONFIG_COUNT was accepted")
				}
				if !strings.Contains(err.Error(), "GIT_CONFIG_COUNT") {
					t.Errorf("the error does not name the offending variable: %v", err)
				}
				if got := recorded(); len(got) != 0 {
					t.Errorf("git ran anyway, %d times", len(got))
				}
				return
			}
			if err != nil {
				t.Fatalf("SyncBranch: %v", err)
			}
			fetch := recorded()[0]
			if got, _ := fetch.lookup("GIT_CONFIG_COUNT"); got != tc.total {
				t.Errorf("GIT_CONFIG_COUNT is %q, want %q", got, tc.total)
			}
			if got, _ := fetch.lookup("GIT_CONFIG_KEY_" + tc.index); got != "http.https://github.com/.extraheader" {
				t.Errorf("GIT_CONFIG_KEY_%s is %q", tc.index, got)
			}
		})
	}
}

// http.extraheader is multi-valued, so an operator who already set one for
// github.com gets both on the wire rather than having theirs replaced. This
// pins that, because it decides whether the agent authenticates as itself.
func TestAnAmbientExtraheaderIsAddedToRatherThanReplacedByTheCredential(t *testing.T) {
	realGit, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	key := "http.https://github.com/.extraheader"
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", key)
	t.Setenv("GIT_CONFIG_VALUE_0", "Authorization: Basic AMBIENT")

	c, recorded := seededCheckout(t, testToken)

	if err := c.SyncBranch(context.Background(), "main"); err != nil {
		t.Fatalf("SyncBranch: %v", err)
	}

	cmd := exec.Command(realGit, "config", "--get-all", key)
	cmd.Dir = t.TempDir()
	cmd.Env = append(recorded()[0].env, "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git config --get-all under the production environment: %v: %s", err, out)
	}
	got := strings.Split(strings.TrimSpace(string(out)), "\n")
	want := []string{"Authorization: Basic AMBIENT", "Authorization: Basic " + basic(testToken)}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("git resolves %q, want %q", got, want)
	}
}

func TestRealGitAppliesTheEnvironmentCredentialToTheGitHubURLOnly(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("GIT_CONFIG_COUNT", "")
	c := New(t.TempDir(), testToken, domain.RepositoryRef{Owner: "acme", Name: "gitops"})
	credential, err := c.credentialEnv()
	if err != nil {
		t.Fatalf("credentialEnv: %v", err)
	}

	urlmatch := func(url string) string {
		cmd := exec.Command(git, "config", "--get-urlmatch", "http", url)
		cmd.Env = append(os.Environ(), credential...)
		out, err := cmd.Output()
		if err != nil && cmd.ProcessState.ExitCode() != 1 {
			t.Fatalf("git config --get-urlmatch %s: %v", url, err)
		}
		return string(out)
	}

	want := "http.extraheader Authorization: Basic " + basic(testToken)
	if got := urlmatch("https://github.com/acme/gitops.git"); !strings.Contains(got, want) {
		t.Errorf("git does not apply the environment credential to the GitHub URL, it reports:\n%s", got)
	}
	if got := urlmatch("https://evil.example.com/acme/gitops.git"); strings.Contains(got, basic(testToken)) {
		t.Errorf("git applies the credential to an unrelated host:\n%s", got)
	}
}

// The wrappers version managers and corporate installs put in front of git fork
// helpers that inherit the command's stdout, so killing the direct child on the
// deadline does not end the read the buffer is waiting on.
func TestAGitCommandReturnsWhileAForkedGrandchildHoldsItsStdoutOpen(t *testing.T) {
	tests := []struct {
		name    string
		script  string
		timeout time.Duration
		// A wrapper that exited cleanly leaves nothing but the delay to report,
		// while one still running is killed on the deadline first, so only the
		// second reports the process's own end.
		reports func(error) bool
		wants   string
	}{
		{
			name:    "the wrapper exits and leaves the grandchild on the pipe",
			script:  "#!/bin/sh\nsleep 20 &\nexit 0\n",
			timeout: 10 * time.Second,
			reports: func(err error) bool { return errors.Is(err, exec.ErrWaitDelay) },
			wants:   "exec.ErrWaitDelay",
		},
		{
			name:    "the wrapper outlives the deadline with the grandchild on the pipe",
			script:  "#!/bin/sh\nsleep 20 &\nsleep 20\n",
			timeout: 200 * time.Millisecond,
			reports: func(err error) bool { return errors.As(err, new(*exec.ExitError)) },
			wants:   "the killed process's own exit error",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			git := filepath.Join(dir, "git")
			if err := os.WriteFile(git, []byte(test.script), 0o700); err != nil {
				t.Fatalf("write fake git: %v", err)
			}
			c := New(dir, testToken, domain.RepositoryRef{Owner: "acme", Name: "gitops"})
			c.executable = git

			ctx, cancel := context.WithTimeout(context.Background(), test.timeout)
			defer cancel()

			returned := make(chan error, 1)
			go func() {
				_, err := c.git(ctx, dir, "version")
				returned <- err
			}()

			select {
			case err := <-returned:
				if err == nil {
					t.Fatal("git() reported success for a command that never released its stdout")
				}
				if !test.reports(err) {
					t.Fatalf("want %s, got %v", test.wants, err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("git() did not return while a grandchild held its stdout open")
			}
		})
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}
