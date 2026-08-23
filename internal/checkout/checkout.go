// Package checkout keeps a local copy of the GitOps repository for the agent
// to read. Nothing is ever pushed from it: every write
// goes through GitHub's commit mutation.
package checkout

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/lkshrk/ops-pilot/internal/domain"
)

const (
	commandTimeout = 5 * time.Minute
	// Grace for the pipes to drain once the child is gone or the deadline has
	// blown; a wrapper's forked grandchild holds them open past both.
	commandWaitDelay = time.Second
)

type Checkout struct {
	root       string
	executable string
	token      string
	repository domain.RepositoryRef
}

func New(root, token string, repository domain.RepositoryRef) *Checkout {
	return &Checkout{root: root, executable: "git", token: token, repository: repository}
}

// Root is the directory the agent reads from.
func (c *Checkout) Root() string { return c.root }

// SyncPullRequest makes the working tree match a pull request's head. The
// clone is blobless so a large GitOps repository stays cheap to keep current.
func (c *Checkout) SyncPullRequest(ctx context.Context, number int) error {
	if number <= 0 {
		return fmt.Errorf("pull request number is required")
	}
	return c.sync(ctx, fmt.Sprintf("pull/%d/head", number))
}

// SyncBranch makes the working tree match the repository's base branch.
func (c *Checkout) SyncBranch(ctx context.Context, branch string) error {
	if branch == "" {
		return fmt.Errorf("branch is required")
	}
	return c.sync(ctx, branch)
}

func (c *Checkout) sync(ctx context.Context, ref string) error {
	if err := c.ensureClone(ctx); err != nil {
		return err
	}
	if _, err := c.git(ctx, c.root, "fetch", "--filter=blob:none", "--", "origin", ref); err != nil {
		return fmt.Errorf("fetch %s: %w", ref, err)
	}
	if _, err := c.git(ctx, c.root, "checkout", "--detach", "--force", "FETCH_HEAD"); err != nil {
		return fmt.Errorf("check out %s: %w", ref, err)
	}
	return nil
}

func (c *Checkout) ensureClone(ctx context.Context) error {
	if c.root == "" {
		return fmt.Errorf("checkout directory is not configured")
	}
	if _, err := os.Stat(filepath.Join(c.root, ".git")); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(c.root), 0o700); err != nil {
		return fmt.Errorf("create the checkout directory: %w", err)
	}
	url := fmt.Sprintf("https://github.com/%s/%s.git", c.repository.Owner, c.repository.Name)
	if _, err := c.git(ctx, filepath.Dir(c.root), "clone", "--filter=blob:none", "--", url, c.root); err != nil {
		return fmt.Errorf("clone %s: %w", url, err)
	}
	return nil
}

// git runs one command. The credential is an environment config override, never
// a URL component or a `-c` argument: argv is readable by every local account
// for the life of the process, and a stored config outlives the process.
func (c *Checkout) git(ctx context.Context, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	full := append([]string{"-c", "credential.helper="}, args...)

	command := exec.CommandContext(ctx, c.executable, full...)
	command.Dir = dir
	command.WaitDelay = commandWaitDelay
	command.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=true")
	// A blobless clone can promisor-fetch from any git command, not just clone/fetch.
	credential, err := c.credentialEnv()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	command.Env = append(command.Env, credential...)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(stdout.String()), nil
}

// credentialEnv is git's own config-from-environment encoding. The credential
// is appended after whatever pairs the operator already set, because
// GIT_CONFIG_COUNT bounds how many git reads: writing our own at index 0 would
// silently discard theirs.
func (c *Checkout) credentialEnv() ([]string, error) {
	if c.token == "" {
		return nil, nil
	}
	next, err := ambientConfigCount()
	if err != nil {
		return nil, err
	}
	return []string{
		fmt.Sprintf("GIT_CONFIG_COUNT=%d", next+1),
		fmt.Sprintf("GIT_CONFIG_KEY_%d=http.https://github.com/.extraheader", next),
		fmt.Sprintf("GIT_CONFIG_VALUE_%d=Authorization: Basic %s", next, basic(c.token)),
	}, nil
}

// An unset or empty count is how git itself spells "no configuration here".
func ambientConfigCount() (uint, error) {
	raw := strings.TrimSpace(os.Getenv("GIT_CONFIG_COUNT"))
	if raw == "" {
		return 0, nil
	}
	count, err := strconv.ParseUint(raw, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("GIT_CONFIG_COUNT is %q, which is not a count", raw)
	}
	return uint(count), nil
}

func basic(token string) string {
	return base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
}
