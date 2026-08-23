package run

import (
	"context"
	"time"

	"github.com/lkshrk/ops-pilot/internal/adapters/github"
	"github.com/lkshrk/ops-pilot/internal/adapters/renovate"
	"github.com/lkshrk/ops-pilot/internal/ai"
	"github.com/lkshrk/ops-pilot/internal/cluster"
	"github.com/lkshrk/ops-pilot/internal/domain"
	"github.com/lkshrk/ops-pilot/internal/events"
)

// Forge is the pull request host. Every external write in ops-pilot goes
// through one of these methods.
type Forge interface {
	ListOpen(ctx context.Context, filter domain.PullRequestFilter) ([]domain.PullRequest, error)
	Get(ctx context.Context, number int) (domain.PullRequest, error)
	ChangedFiles(ctx context.Context, number int) ([]domain.FileDelta, error)
	FileAt(ctx context.Context, path, ref string) ([]byte, bool, error)
	Merge(ctx context.Context, number int, headSHA, method string) (string, error)
	MergeState(ctx context.Context, number int) (domain.MergeState, error)
	Comment(ctx context.Context, number int, body string) error
	AddLabel(ctx context.Context, number int, labels ...string) error
	Close(ctx context.Context, number int) error
	Branch(ctx context.Context) (string, error)
	BranchHead(ctx context.Context, branch string) (string, error)
	CreateCommit(ctx context.Context, branch, expectedHeadSHA, message string, changes []github.FileChange) (string, error)
}

// Observer watches the cluster react to a merge.
type Observer interface {
	Snapshot(ctx context.Context) (domain.HealthSnapshot, error)
	Reconcile(ctx context.Context) error
	Watch(ctx context.Context, baseline domain.HealthSnapshot, revision string) (cluster.Outcome, error)
	Restored(ctx context.Context, baseline domain.HealthSnapshot, broken []domain.ObjectHealth) (cluster.Outcome, error)
	// Broken reads the named objects once and returns those still unhealthy.
	Broken(ctx context.Context, objects []domain.ObjectHealth) ([]domain.ObjectHealth, error)
	// Observe installs a per-poll callback so a watch lasting minutes can report
	// that it is still moving.
	Observe(func(cluster.Status))
}

// Changelogs resolves upstream release notes for an update.
type Changelogs interface {
	Resolve(ctx context.Context, update renovate.Update) (domain.Changelog, error)
}

// Recorder is the passive history log.
type Recorder interface {
	StartRun(ctx context.Context, run domain.Run) error
	FinishRun(ctx context.Context, id string, at time.Time, halted string) error
	RecordAttempt(ctx context.Context, attempt domain.Attempt) error
}

// Approver asks the operator.
type Approver interface {
	Interactive() bool
	// Stream renders an assistant assessment as it arrives. Implementations may
	// ignore it when no operator can see an interactive terminal.
	Stream(ai.StreamEvent)
	// Clarify collects an operator's free-form answer to an assessment question.
	// answered is false when the operator defers; an answer never authorizes a
	// merge or a repository change.
	Clarify(approval Approval, question string) (answer string, answered bool, err error)
	ApproveFix(request domain.PullRequest, diagnosis domain.Diagnosis) (bool, error)
	// ConfirmRevert asks before a merge is discarded. A non-interactive run
	// answers RevertNow, which is the unattended behaviour.
	ConfirmRevert(Revert) (RevertChoice, error)
}

// RevertChoice is what an operator answered when asked to discard a merge.
type RevertChoice string

const (
	RevertNow  RevertChoice = "revert"
	RevertKeep RevertChoice = "keep"
	RevertWait RevertChoice = "wait"
)

// Revert is what an operator needs to answer whether a merge should be undone:
// which update it was, why the run wants to discard it, what is still unhealthy,
// and how long another window would last.
type Revert struct {
	PullRequest domain.PullRequest
	Dependency  domain.Dependency
	Cause       string
	Broken      []domain.ObjectHealth
	Window      time.Duration
}

// Approval is everything an operator needs to answer without leaving the
// terminal: what is changing, why it was escalated, and where to read more.
type Approval struct {
	PullRequest domain.PullRequest
	Dependency  domain.Dependency
	Assessment  domain.Assessment
	Changelog   domain.Changelog
}

// Workspace keeps a local copy of the repository current, so the agent reads the
// pull request's own head rather than whatever was last checked out.
type Workspace interface {
	SyncPullRequest(ctx context.Context, number int) error
}

// BaseWorkspace repoints the working tree at a branch. A Workspace that
// implements it is put back on the base branch whenever a pull request sync
// fails, so the tree the agent reads is never another pull request's head.
type BaseWorkspace interface {
	SyncBranch(ctx context.Context, branch string) error
}

// Events receives a machine-readable record of decisions and external writes as
// they happen, for a run nobody is watching.
type Events interface {
	Bind(run domain.Run)
	Emit(event events.Event)
}

// Agent is the judgement boundary.
type Agent = ai.Agent
