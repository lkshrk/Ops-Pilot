package e2e_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/lkshrk/ops-pilot/internal/adapters/github"
	"github.com/lkshrk/ops-pilot/internal/adapters/renovate"
	"github.com/lkshrk/ops-pilot/internal/ai"
	"github.com/lkshrk/ops-pilot/internal/cluster"
	"github.com/lkshrk/ops-pilot/internal/domain"
	"github.com/lkshrk/ops-pilot/internal/run"
)

// forge is an in-process GitHub. It records every external write so a test can
// assert what ops-pilot actually did, not only what it concluded.
type forge struct {
	mu       sync.Mutex
	requests map[int]*domain.PullRequest
	files    map[int][]domain.FileDelta
	// contents is keyed by ref then path.
	contents map[string]map[string][]byte
	branch   string
	head     string
	merged   map[int]string

	Merges   []int
	Comments map[int][]string
	Labels   map[int][]string
	Closed   []int
	Commits  []commit

	MergeErr      error
	MergeStateErr error
	CommentErr    error
	CommitErr     error
}

type commit struct {
	Branch  string
	Message string
	Changes []github.FileChange
}

func newForge() *forge {
	return &forge{
		requests: map[int]*domain.PullRequest{},
		files:    map[int][]domain.FileDelta{},
		contents: map[string]map[string][]byte{},
		branch:   "main",
		head:     "base000",
		merged:   map[int]string{},
		Comments: map[int][]string{},
		Labels:   map[int][]string{},
	}
}

func (f *forge) add(request domain.PullRequest, paths []string) *forge {
	f.requests[request.Number] = &request
	files := make([]domain.FileDelta, 0, len(paths))
	for _, path := range paths {
		files = append(files, domain.FileDelta{Path: path, Status: domain.FileModified})
	}
	f.files[request.Number] = files
	return f
}

func (f *forge) renamed(number int, from, to string) *forge {
	f.files[number] = []domain.FileDelta{
		{Path: to, PreviousPath: from, Status: domain.FileRenamed},
	}
	return f
}

func (f *forge) fileAt(ref, path string, contents []byte) *forge {
	if f.contents[ref] == nil {
		f.contents[ref] = map[string][]byte{}
	}
	f.contents[ref][path] = contents
	return f
}

func (f *forge) ListOpen(_ context.Context, filter domain.PullRequestFilter) ([]domain.PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []domain.PullRequest
	for _, request := range f.requests {
		if onBranch := request.BaseRef == f.branch; onBranch == filter.OtherBases {
			continue
		}
		if !matchesFilter(*request, filter) {
			continue
		}
		result = append(result, *request)
	}
	sortByNumber(result)
	return result, nil
}

// Mirrors matchesFilter in internal/adapters/github/pulls.go.
func matchesFilter(request domain.PullRequest, filter domain.PullRequestFilter) bool {
	if len(filter.Authors) > 0 && !slices.Contains(filter.Authors, request.Author) {
		return false
	}
	if len(filter.Labels) == 0 {
		return true
	}
	for _, label := range request.Labels {
		if slices.Contains(filter.Labels, label) {
			return true
		}
	}
	return false
}

func sortByNumber(requests []domain.PullRequest) {
	for i := 1; i < len(requests); i++ {
		for j := i; j > 0 && requests[j].Number < requests[j-1].Number; j-- {
			requests[j], requests[j-1] = requests[j-1], requests[j]
		}
	}
}

func (f *forge) Get(_ context.Context, number int) (domain.PullRequest, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	request, found := f.requests[number]
	if !found {
		return domain.PullRequest{}, fmt.Errorf("no pull request %d", number)
	}
	return *request, nil
}

func (f *forge) ChangedFiles(_ context.Context, number int) ([]domain.FileDelta, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]domain.FileDelta(nil), f.files[number]...), nil
}

func (f *forge) FileAt(_ context.Context, path, ref string) ([]byte, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	contents, found := f.contents[ref][path]
	return contents, found, nil
}

func (f *forge) Merge(_ context.Context, number int, headSHA, method string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.MergeErr != nil {
		return "", f.MergeErr
	}
	request, found := f.requests[number]
	if !found || request.HeadSHA != headSHA {
		return "", fmt.Errorf("stale merge target for %d", number)
	}
	f.Merges = append(f.Merges, number)
	f.head = fmt.Sprintf("merge%03d", number)
	f.merged[number] = f.head
	return f.head, nil
}

func (f *forge) MergeState(_ context.Context, number int) (domain.MergeState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.MergeStateErr != nil {
		return domain.MergeState{}, f.MergeStateErr
	}
	sha, merged := f.merged[number]
	return domain.MergeState{Merged: merged, SHA: sha}, nil
}

func (f *forge) Comment(_ context.Context, number int, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Comments[number] = append(f.Comments[number], body)
	return f.CommentErr
}

func (f *forge) AddLabel(_ context.Context, number int, labels ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Labels[number] = append(f.Labels[number], labels...)
	return nil
}

func (f *forge) Close(_ context.Context, number int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Closed = append(f.Closed, number)
	return nil
}

func (f *forge) Branch(context.Context) (string, error) { return f.branch, nil }

func (f *forge) BranchHead(_ context.Context, _ string) (string, error) { return f.head, nil }

func (f *forge) CreateCommit(
	_ context.Context,
	branch, expectedHeadSHA, message string,
	changes []github.FileChange,
) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.CommitErr != nil {
		return "", f.CommitErr
	}
	if expectedHeadSHA != f.head {
		return "", fmt.Errorf("stale head: want %s, got %s", f.head, expectedHeadSHA)
	}
	f.Commits = append(f.Commits, commit{Branch: branch, Message: message, Changes: changes})
	f.head = fmt.Sprintf("commit%03d", len(f.Commits))
	for _, request := range f.requests {
		if request.HeadRef == branch && request.HeadSHA == expectedHeadSHA {
			request.HeadSHA = f.head
		}
	}
	if f.contents[f.head] == nil {
		f.contents[f.head] = map[string][]byte{}
	}
	for path, contents := range f.contents[expectedHeadSHA] {
		f.contents[f.head][path] = contents
	}
	for _, change := range changes {
		if change.Delete {
			delete(f.contents[f.head], change.Path)
			continue
		}
		f.contents[f.head][change.Path] = change.Contents
	}
	return f.head, nil
}

// observer is a scripted cluster. Each Watch call consumes the next outcome.
type observer struct {
	outcomes     []cluster.Outcome
	restored     []cluster.Outcome
	Watches      int
	Restores     int
	Snapshots    int
	Reconciles   int
	BrokenChecks int
	RestoreErr   error
	// Recovered makes the pre-revert re-check report everything healthy again.
	Recovered bool
}

func (o *observer) Snapshot(context.Context) (domain.HealthSnapshot, error) {
	o.Snapshots++
	return domain.HealthSnapshot{Objects: map[string]domain.ObjectHealth{
		"media/HelmRelease/sonarr": {
			Ref:     domain.ObjectRef{Kind: "HelmRelease", Namespace: "media", Name: "sonarr"},
			Healthy: true,
		},
	}}, nil
}

func (o *observer) Observe(func(cluster.Status)) {}

func (o *observer) Reconcile(context.Context) error {
	o.Reconciles++
	return nil
}

func (o *observer) Watch(context.Context, domain.HealthSnapshot, string) (cluster.Outcome, error) {
	if o.Watches >= len(o.outcomes) {
		return cluster.Outcome{}, fmt.Errorf("unexpected watch call %d", o.Watches+1)
	}
	outcome := o.outcomes[o.Watches]
	o.Watches++
	return outcome, nil
}

func (o *observer) Restored(context.Context, domain.HealthSnapshot, []domain.ObjectHealth) (cluster.Outcome, error) {
	o.Restores++
	if o.RestoreErr != nil {
		return cluster.Outcome{}, o.RestoreErr
	}
	if o.Restores <= len(o.restored) {
		outcome := o.restored[o.Restores-1]
		// The real observer reports "did not recover" as an error, and the
		// difference is what tells a wait apart from a heal.
		if outcome.Result != domain.WatchPass {
			return outcome, fmt.Errorf("the cluster did not recover")
		}
		return outcome, nil
	}
	return cluster.Outcome{Result: domain.WatchPass}, nil
}

// Broken answers that the objects are still broken unless a test says they
// healed, which is what the pre-revert re-check reads.
func (o *observer) Broken(_ context.Context, objects []domain.ObjectHealth) ([]domain.ObjectHealth, error) {
	o.BrokenChecks++
	if o.Recovered {
		return nil, nil
	}
	return objects, nil
}

func pass() cluster.Outcome { return cluster.Outcome{Result: domain.WatchPass} }

func broke(name string) cluster.Outcome {
	return cluster.Outcome{
		Result: domain.WatchFail,
		Failures: []domain.ObjectHealth{{
			Ref:    domain.ObjectRef{Kind: "HelmRelease", Namespace: "media", Name: name},
			Reason: "CreateContainerConfigError",
		}},
	}
}

func stalled(name string) cluster.Outcome {
	return cluster.Outcome{
		Result: domain.WatchStalled,
		Pending: []domain.ObjectHealth{{
			Ref:         domain.ObjectRef{Kind: "HelmRelease", Namespace: "media", Name: name},
			Reconciling: true,
			Reason:      "pulling image",
		}},
	}
}

// agent is a scripted judgement boundary.
type agent struct {
	assessments []domain.Assessment
	diagnoses   []domain.Diagnosis
	stream      [][]ai.StreamEvent
	AssessErr   error
	DiagnoseErr error

	Assessed           int
	Diagnosed          int
	AssessmentRequests []ai.AssessmentRequest
	LastDiagnosis      ai.DiagnosisRequest
}

func (a *agent) Assess(_ context.Context, request ai.AssessmentRequest) (domain.Assessment, error) {
	a.AssessmentRequests = append(a.AssessmentRequests, request)
	if request.Stream != nil && a.Assessed < len(a.stream) {
		for _, event := range a.stream[a.Assessed] {
			request.Stream(event)
		}
	}
	if a.AssessErr != nil {
		return domain.Assessment{}, a.AssessErr
	}
	if a.Assessed >= len(a.assessments) {
		return domain.Assessment{}, fmt.Errorf("unexpected assess call %d", a.Assessed+1)
	}
	assessment := a.assessments[a.Assessed]
	a.Assessed++
	return assessment, nil
}

func (a *agent) Diagnose(_ context.Context, request ai.DiagnosisRequest) (domain.Diagnosis, error) {
	a.LastDiagnosis = request
	if a.DiagnoseErr != nil {
		return domain.Diagnosis{}, a.DiagnoseErr
	}
	if a.Diagnosed >= len(a.diagnoses) {
		return domain.Diagnosis{}, fmt.Errorf("unexpected diagnose call %d", a.Diagnosed+1)
	}
	diagnosis := a.diagnoses[a.Diagnosed]
	a.Diagnosed++
	return diagnosis, nil
}

func safe(reason string) domain.Assessment {
	return domain.Assessment{
		Verdict:  domain.AssessmentSafe,
		Reason:   reason,
		Evidence: []string{"apps/app.yaml: the affected setting is not enabled"},
	}
}

func needsApproval(reason string) domain.Assessment {
	return domain.Assessment{Verdict: domain.AssessmentNeedsApproval, Reason: reason}
}

// changelogs is a scripted changelog chain.
type changelogs struct {
	result domain.Changelog
	err    error
}

func (c changelogs) Resolve(context.Context, renovate.Update) (domain.Changelog, error) {
	return c.result, c.err
}

func fromBody() changelogs {
	return changelogs{result: domain.Changelog{
		Source: domain.ChangelogFromPullRequest,
		Text:   "Fixed a crash on import.",
	}}
}

func notFound() changelogs {
	return changelogs{result: domain.Changelog{Source: domain.ChangelogNotFound}}
}

// approver is a scripted operator.
type approver struct {
	interactive  bool
	fixAnswer    bool
	revertAnswer run.RevertChoice
	clarify      []string

	FixAsks           int
	RevertAsks        int
	ClarifyAsks       int
	LastClarification run.Approval
	LastQuestion      string
	LastRevert        run.Revert
	Streamed          []ai.StreamEvent
}

func (a *approver) Interactive() bool { return a.interactive }

func (a *approver) Stream(event ai.StreamEvent) { a.Streamed = append(a.Streamed, event) }

func (a *approver) Clarify(approval run.Approval, question string) (string, bool, error) {
	a.ClarifyAsks++
	a.LastClarification, a.LastQuestion = approval, question
	if a.ClarifyAsks > len(a.clarify) {
		return "", false, nil
	}
	return a.clarify[a.ClarifyAsks-1], true, nil
}

func (a *approver) ApproveFix(domain.PullRequest, domain.Diagnosis) (bool, error) {
	a.FixAsks++
	return a.fixAnswer, nil
}

func (a *approver) ConfirmRevert(revert run.Revert) (run.RevertChoice, error) {
	a.RevertAsks++
	a.LastRevert = revert
	if a.revertAnswer == "" {
		return run.RevertNow, nil
	}
	return a.revertAnswer, nil
}

// recorder captures what the history log would have stored.
type recorder struct {
	Runs     []domain.Run
	Attempts []domain.Attempt
	Finished []string
}

func (r *recorder) StartRun(_ context.Context, run domain.Run) error {
	r.Runs = append(r.Runs, run)
	return nil
}

func (r *recorder) FinishRun(_ context.Context, id string, _ time.Time, _ string) error {
	r.Finished = append(r.Finished, id)
	return nil
}

func (r *recorder) RecordAttempt(_ context.Context, attempt domain.Attempt) error {
	r.Attempts = append(r.Attempts, attempt)
	return nil
}

// renovateBody builds a pull request body in Renovate's shape.
func renovateBody(name, kind, update, from, to, notes string) string {
	var body strings.Builder
	body.WriteString("This PR contains the following updates:\n\n")
	body.WriteString("| Package | Type | Update | Change |\n|---|---|---|---|\n")
	fmt.Fprintf(&body, "| [%s](https://example.test) | %s | %s | `%s` -> `%s` |\n", name, kind, update, from, to)
	if notes != "" {
		body.WriteString("\n---\n\n### Release Notes\n\n<details>\n<summary>upstream/project (" + name + ")</summary>\n\n")
		body.WriteString(notes + "\n\n</details>\n")
	}
	return body.String()
}

var errMergeConflict = fmt.Errorf("merge not applied: Pull Request is not mergeable")

// workspace records which pull request head the agent was given to read.
type workspace struct {
	Synced []int
	Err    error
}

func (w *workspace) SyncPullRequest(_ context.Context, number int) error {
	w.Synced = append(w.Synced, number)
	return w.Err
}

var errCheckout = fmt.Errorf("clone: repository not found")

var errCommitRejected = fmt.Errorf("refusing to update a protected branch")

var errClusterUnreachable = fmt.Errorf("Kubernetes API is unreachable")

// watchErr makes the next Watch call fail rather than returning an outcome.
type failingObserver struct {
	observer
	err error
}

func (o *failingObserver) Watch(context.Context, domain.HealthSnapshot, string) (cluster.Outcome, error) {
	o.Watches++
	return cluster.Outcome{}, o.err
}

var errCommentRejected = fmt.Errorf("resource not accessible by integration")
