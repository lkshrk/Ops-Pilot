package domain

import (
	"strings"
	"time"
)

type RepositoryRef struct {
	Owner  string `json:"owner"`
	Name   string `json:"name"`
	Branch string `json:"branch"`
}

func (r RepositoryRef) String() string { return r.Owner + "/" + r.Name }

type PullRequest struct {
	Number  int      `json:"number"`
	Title   string   `json:"title"`
	Body    string   `json:"body"`
	Author  string   `json:"author"`
	Labels  []string `json:"labels"`
	URL     string   `json:"url"`
	HeadSHA string   `json:"headSha"`
	HeadRef string   `json:"headRef"`
	// HeadRepository identifies the repository that owns HeadRef. It is empty
	// when GitHub cannot resolve a deleted or otherwise unavailable head.
	HeadRepository string    `json:"headRepository"`
	BaseSHA        string    `json:"baseSha"`
	BaseRef        string    `json:"baseRef"`
	Draft          bool      `json:"draft"`
	CreatedAt      time.Time `json:"createdAt"`
}

func (p PullRequest) HasLabel(name string) bool {
	for _, label := range p.Labels {
		if label == name {
			return true
		}
	}
	return false
}

// PullRequestFilter narrows open pull requests. Non-empty fields are combined.
type PullRequestFilter struct {
	Authors []string
	Labels  []string
	// OtherBases inverts the base branch narrowing: the listing returns the open
	// pull requests aimed at any branch except the one the run merges into. It
	// exists so a run can count what that narrowing removed, and nothing may ever
	// be merged from such a listing, because the merge and the revert that undoes
	// it both target one branch.
	OtherBases bool
}

// MergeState is what the forge says about a pull request's merge after the fact.
type MergeState struct {
	Merged bool   `json:"merged"`
	SHA    string `json:"sha,omitempty"`
}

type FileDeltaStatus string

const (
	FileAdded    FileDeltaStatus = "added"
	FileModified FileDeltaStatus = "modified"
	FileDeleted  FileDeltaStatus = "deleted"
	FileRenamed  FileDeltaStatus = "renamed"
)

type FileDelta struct {
	Path         string          `json:"path"`
	PreviousPath string          `json:"previousPath,omitempty"`
	Status       FileDeltaStatus `json:"status"`
	Before       []byte          `json:"before,omitempty"`
	After        []byte          `json:"after,omitempty"`
	Patch        []byte          `json:"patch"`
}

type TreeDelta struct {
	BaseSHA   string      `json:"baseSha"`
	ResultSHA string      `json:"resultSha"`
	Files     []FileDelta `json:"files"`
}

type ArtifactIdentity struct {
	Kind        string             `json:"kind"`
	Name        string             `json:"name"`
	Version     string             `json:"version"`
	Reference   string             `json:"reference,omitempty"`
	Digest      string             `json:"digest,omitempty"`
	IndexDigest string             `json:"indexDigest,omitempty"`
	Platforms   []PlatformIdentity `json:"platforms,omitempty"`
}

type PlatformIdentity struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
	Variant      string `json:"variant,omitempty"`
	Digest       string `json:"digest"`
}

type ArtifactBlobProof struct {
	Kind   string `json:"kind"`
	Digest string `json:"digest"`
	Size   int64  `json:"size"`
}

type BumpClass string

const (
	BumpMajor   BumpClass = "major"
	BumpMinor   BumpClass = "minor"
	BumpPatch   BumpClass = "patch"
	BumpDigest  BumpClass = "digest"
	BumpUnknown BumpClass = "unknown"
)

const (
	DependencyKindHelm   = "helm"
	DependencyKindDocker = "docker"
)

type Dependency struct {
	Name        string    `json:"name"`
	Kind        string    `json:"kind"`
	FromVersion string    `json:"fromVersion"`
	ToVersion   string    `json:"toVersion"`
	Bump        BumpClass `json:"bump"`
}

type ChangelogSource string

const (
	ChangelogFromPullRequest ChangelogSource = "pull_request_body"
	ChangelogFromAnnotation  ChangelogSource = "image_source_annotation"
	ChangelogFromOverride    ChangelogSource = "config_override"
	ChangelogFromSearch      ChangelogSource = "ai_search"
	ChangelogNotFound        ChangelogSource = "none"
	// A configured override resolved nothing, so unlike ChangelogNotFound the agent may not merge on its verdict alone.
	ChangelogOverrideEmpty ChangelogSource = "config_override_empty"
)

type Changelog struct {
	Source     ChangelogSource `json:"source"`
	Repository string          `json:"repository,omitempty"`
	URL        string          `json:"url,omitempty"`
	Text       string          `json:"text,omitempty"`
}

type AssessmentVerdict string

const (
	AssessmentSafe           AssessmentVerdict = "safe"
	AssessmentClarify        AssessmentVerdict = "clarify"
	AssessmentNeedsApproval  AssessmentVerdict = "needs_approval"
	AssessmentDefer          AssessmentVerdict = "defer"
	assessmentVerdictInvalid AssessmentVerdict = ""
)

type Assessment struct {
	Verdict AssessmentVerdict `json:"verdict"`
	Reason  string            `json:"reason"`
	// ChangelogURL is set when the agent located release notes that automatic
	// resolution missed, so the attempt records how the changelog was found.
	ChangelogURL string   `json:"changelogUrl,omitempty"`
	Evidence     []string `json:"evidence,omitempty"`
	// Question is only valid with clarify. It asks the operator for one fact
	// needed to assess this pull request.
	Question string `json:"question,omitempty"`
	// Diff is only valid with needs_approval. The runner validates and applies
	// it only after showing the exact diff to the operator.
	Diff string `json:"diff,omitempty"`
	// Message is the assistant prose shown during an interactive assessment.
	// It is never supplied by the model's submit_assessment tool.
	Message string `json:"-"`
}

func (a Assessment) Valid() bool {
	question := strings.TrimSpace(a.Question)
	diff := strings.TrimSpace(a.Diff)
	switch a.Verdict {
	case AssessmentSafe:
		return strings.TrimSpace(a.Reason) != "" && hasAssessmentEvidence(a.Evidence) && question == "" && diff == ""
	case AssessmentClarify:
		return question != "" && diff == ""
	case AssessmentNeedsApproval:
		return strings.TrimSpace(a.Reason) != "" && question == ""
	case AssessmentDefer:
		return strings.TrimSpace(a.Reason) != "" && question == "" && diff == ""
	default:
		return false
	}
}

func hasAssessmentEvidence(evidence []string) bool {
	for _, item := range evidence {
		if strings.TrimSpace(item) != "" {
			return true
		}
	}
	return false
}

type DiagnosisAction string

const (
	DiagnoseBenignWait DiagnosisAction = "benign_wait"
	DiagnoseFix        DiagnosisAction = "fix"
	DiagnoseUnfixable  DiagnosisAction = "unfixable"
)

type Diagnosis struct {
	Action DiagnosisAction `json:"action"`
	Cause  string          `json:"cause"`
	Diff   string          `json:"diff,omitempty"`
}

// Decision is what the pre-merge gate concluded for one pull request.
type Decision string

const (
	DecideMerge          Decision = "merge"
	DecideNeedsApproval  Decision = "needs_approval"
	DecideSkipSuperseded Decision = "skip_superseded"
	DecideSkipReverted   Decision = "skip_reverted"
	DecideSkipDeclined   Decision = "skip_declined"
)

// Verdict is how processing one pull request ended.
type Verdict string

const (
	VerdictMerged     Verdict = "merged"
	VerdictFixed      Verdict = "fixed"
	VerdictReverted   Verdict = "reverted"
	VerdictKept       Verdict = "kept"
	VerdictSkipped    Verdict = "skipped"
	VerdictWouldMerge Verdict = "would_merge"
	VerdictError      Verdict = "error"
)

// WatchResult is the outcome of one post-merge observation window.
type WatchResult string

const (
	WatchPass    WatchResult = "pass"
	WatchFail    WatchResult = "fail"
	WatchStalled WatchResult = "stalled"
)

type ObjectRef struct {
	Kind      string `json:"kind"`
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

func (r ObjectRef) String() string { return r.Namespace + "/" + r.Kind + "/" + r.Name }

type ObjectHealth struct {
	Ref         ObjectRef `json:"ref"`
	Healthy     bool      `json:"healthy"`
	Reconciling bool      `json:"reconciling"`
	Reason      string    `json:"reason,omitempty"`
	Revision    string    `json:"revision,omitempty"`
}

// HealthSnapshot is the cluster-wide health of every object the watch tracks.
type HealthSnapshot struct {
	TakenAt time.Time               `json:"takenAt"`
	Objects map[string]ObjectHealth `json:"objects"`
}

// Attributable reports whether a change to this object can fairly be blamed on
// the merge being watched. Only an object that was genuinely settled and
// healthy beforehand qualifies: one already failing, or still moving, was in
// that state for reasons that predate the merge. An object absent from the
// baseline qualifies, because a merge that creates a broken object is at fault
// for it.
func (s HealthSnapshot) Attributable(ref string) bool {
	previous, known := s.Objects[ref]
	if !known {
		return true
	}
	return previous.Healthy && !previous.Reconciling
}

// NewFailures returns the objects this merge can be held responsible for: ones
// that were settled and healthy in the baseline and are unhealthy now, ones
// absent from the baseline that are unhealthy now, and ones that were settled
// and healthy and have since disappeared. Pre-existing breakage, and anything
// that was already mid-flight, are invisible by construction.
func (s HealthSnapshot) NewFailures(baseline HealthSnapshot) []ObjectHealth {
	var failures []ObjectHealth
	for key, current := range s.Objects {
		if current.Healthy || !baseline.Attributable(key) {
			continue
		}
		failures = append(failures, current)
	}
	for key, previous := range baseline.Objects {
		if !baseline.Attributable(key) {
			continue
		}
		if _, present := s.Objects[key]; present {
			continue
		}
		vanished := previous
		vanished.Healthy, vanished.Reconciling = false, false
		vanished.Reason = "object vanished from the cluster after being healthy in the baseline"
		failures = append(failures, vanished)
	}
	return failures
}

// SettledSince reports whether everything this merge could be answerable for has
// stopped moving. It is scoped by the same baseline as NewFailures, and for the
// same reason: an object already in flight predates the merge. Without that
// scope an object that reconciles forever - a DaemonSet short a node, a
// StatefulSet held on an ordered rollout, a repository on its own interval -
// denies every later merge the pass path while never appearing as a failure,
// which is the one asymmetry that made unattributable churn cost more than
// unattributable breakage.
func (s HealthSnapshot) SettledSince(baseline HealthSnapshot) bool {
	for key, object := range s.Objects {
		if object.Reconciling && baseline.Attributable(key) {
			return false
		}
	}
	return true
}

// Attempt is the durable record of processing one pull request. It carries what
// an operator needs months later, when the terminal is gone: what was decided
// and why, which commits were written, what broke, and when.
type Attempt struct {
	RunID       string     `json:"runId"`
	PullRequest int        `json:"pullRequest"`
	Dependency  Dependency `json:"dependency"`
	Decision    Decision   `json:"decision"`
	// Reason is why the pre-merge gate decided as it did.
	Reason          string          `json:"reason,omitempty"`
	ChangelogSource ChangelogSource `json:"changelogSource"`
	ChangelogURL    string          `json:"changelogUrl,omitempty"`
	// HeadSHA is the commit the assessment was made against, and the commit the
	// merge was guarded on.
	HeadSHA string `json:"headSha,omitempty"`
	// PreMergeSHA is the branch head before this merge. A revert restores to it,
	// so an operator repairing a failed revert by hand needs it.
	PreMergeSHA string      `json:"preMergeSha,omitempty"`
	MergeSHA    string      `json:"mergeSha,omitempty"`
	Watch       WatchResult `json:"watch,omitempty"`
	// Broken names the objects attributed to this merge, with the cluster's own
	// reason for each. The agent's diagnosis is an interpretation; this is the
	// raw signal, and the operator's only defence when that interpretation is wrong.
	Broken []ObjectHealth `json:"broken,omitempty"`
	// DiagnosisCause is what the agent concluded went wrong. It is kept apart
	// from Reason because one explains a merge and the other explains a failure.
	DiagnosisCause string  `json:"diagnosisCause,omitempty"`
	Verdict        Verdict `json:"verdict"`
	// Fixes holds every diff applied, not only the last, since each one is a
	// commit that reached the branch.
	Fixes       []string `json:"fixes,omitempty"`
	FixAttempts int      `json:"fixAttempts"`
	// Waited records that the agent judged a stall benign and the window was
	// extended, which otherwise leaves no trace of why an attempt took so long.
	Waited    bool   `json:"waited,omitempty"`
	RevertSHA string `json:"revertSha,omitempty"`
	// Evidence is the agent's stated support for its verdict: the justification
	// for an autonomous change to production.
	Evidence   []string      `json:"evidence,omitempty"`
	StartedAt  time.Time     `json:"startedAt"`
	FinishedAt time.Time     `json:"finishedAt"`
	Duration   time.Duration `json:"duration"`
	Error      string        `json:"error,omitempty"`
}

type Run struct {
	ID         string        `json:"id"`
	Repository RepositoryRef `json:"repository"`
	StartedAt  time.Time     `json:"startedAt"`
	FinishedAt time.Time     `json:"finishedAt"`
	Mode       string        `json:"mode"`
	Attempts   []Attempt     `json:"attempts,omitempty"`
	// Halted names why the run stopped before draining its queue.
	Halted string `json:"halted,omitempty"`
	// Discovered counts the open pull requests on the base branch before the
	// configured author and label filters. Nil means it was not measured, which
	// a reader may not report as zero.
	Discovered *int `json:"discovered,omitempty"`
	// OtherBranches counts the open pull requests aimed at any branch except the
	// one this run merges into. They are dropped by every listing, so Discovered
	// cannot see them and a repository whose updates all target another branch
	// reads as idle. Nil means it was not measured, which a reader may not report
	// as zero.
	OtherBranches *int `json:"otherBranches,omitempty"`
}
