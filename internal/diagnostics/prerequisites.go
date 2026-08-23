package diagnostics

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// MinimumGitMajor and MinimumGitMinor are the git floor CheckPrerequisites
// enforces. They are exported so callers gate on this floor instead of
// restating it; a second copy is a copy that drifts.
const (
	MinimumGitMajor = 2
	MinimumGitMinor = 38
)

const (
	defaultGitExecutable = "git"
	maxVersionOutput     = 4 << 10
	// Grace for the pipes to drain once the child is gone or the deadline has
	// blown; a wrapper's forked grandchild holds them open past both.
	versionWaitDelay = time.Second
)

var gitVersionPattern = regexp.MustCompile(`(?m)^git version ([0-9]+)\.([0-9]+)(?:\.([0-9]+))?(?:[.-][A-Za-z0-9.-]+)?(?: \([^)]*\))?\r?$`)

// CheckPrerequisites requires git at MinimumGitMajor.MinimumGitMinor or newer.
func CheckPrerequisites(ctx context.Context, gitExecutable string) error {
	if ctx == nil {
		return errors.New("check prerequisites: context is required")
	}
	if gitExecutable == "" {
		gitExecutable = defaultGitExecutable
	}
	requirement := fmt.Sprintf("Git %d.%d or newer", MinimumGitMajor, MinimumGitMinor)

	gitOutput, err := commandVersion(ctx, gitExecutable)
	if err != nil {
		return prerequisiteCommandError(ctx, requirement, gitExecutable, err)
	}
	_, major, minor, patch, ok := parseGitVersion(gitOutput)
	if !ok {
		return fmt.Errorf(
			"%s is required; %q returned unrecognized version output; install or upgrade Git",
			requirement, gitExecutable,
		)
	}
	if major < MinimumGitMajor || major == MinimumGitMajor && minor < MinimumGitMinor {
		return fmt.Errorf(
			"%s is required (found %d.%d.%d); upgrade Git and ensure %q selects the upgraded executable",
			requirement, major, minor, patch, gitExecutable,
		)
	}
	return nil
}

func prerequisiteCommandError(ctx context.Context, requirement, executable string, err error) error {
	if cause := context.Cause(ctx); cause != nil {
		return fmt.Errorf("check %s prerequisite: %w", requirement, cause)
	}
	return fmt.Errorf(
		"%s is required; install it and ensure %q is executable: %w",
		requirement, executable, err,
	)
}

// commandVersion never reads a pipe it is not also waiting on: Wait is what
// enforces WaitDelay, so a read done ahead of it outlives every deadline.
func commandVersion(ctx context.Context, executable string) (string, error) {
	command := exec.CommandContext(ctx, executable, "--version")
	command.Env = []string{"LC_ALL=C"}
	command.WaitDelay = versionWaitDelay
	output := &cappedBuffer{limit: maxVersionOutput}
	command.Stdout = output
	command.Stderr = io.Discard

	if err := command.Run(); err != nil {
		if cause := context.Cause(ctx); cause != nil {
			return "", cause
		}
		return "", err
	}
	return strings.TrimSpace(string(output.data)), nil
}

type cappedBuffer struct {
	data  []byte
	limit int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if len(b.data)+len(p) > b.limit {
		return 0, errors.New("version output exceeds limit")
	}
	b.data = append(b.data, p...)
	return len(p), nil
}

// Scanning every line means a line that only mentions a version can be taken
// for git's own answer, so disagreeing lines are no answer at all.
func parseGitVersion(output string) (canonical string, major, minor, patch int, ok bool) {
	matches := gitVersionPattern.FindAllStringSubmatch(output, -1)
	if len(matches) == 0 {
		return "", 0, 0, 0, false
	}
	for index, match := range matches {
		lineCanonical, lineMajor, lineMinor, linePatch, lineOK := parseGitVersionLine(match)
		if !lineOK || index > 0 && lineCanonical != canonical {
			return "", 0, 0, 0, false
		}
		canonical, major, minor, patch = lineCanonical, lineMajor, lineMinor, linePatch
	}
	return canonical, major, minor, patch, true
}

func parseGitVersionLine(match []string) (canonical string, major, minor, patch int, ok bool) {
	major, err := strconv.Atoi(match[1])
	if err != nil {
		return "", 0, 0, 0, false
	}
	minor, err = strconv.Atoi(match[2])
	if err != nil {
		return "", 0, 0, 0, false
	}
	if match[3] != "" {
		patch, err = strconv.Atoi(match[3])
		if err != nil {
			return "", 0, 0, 0, false
		}
	}
	return fmt.Sprintf("%d.%d.%d", major, minor, patch), major, minor, patch, true
}
