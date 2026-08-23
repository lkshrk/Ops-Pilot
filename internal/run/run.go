package run

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/lkshrk/ops-pilot/internal/adapters/github"
	"github.com/lkshrk/ops-pilot/internal/adapters/renovate"
	"github.com/lkshrk/ops-pilot/internal/ai"
	"github.com/lkshrk/ops-pilot/internal/changelog"
	"github.com/lkshrk/ops-pilot/internal/diagnostics"
	"github.com/lkshrk/ops-pilot/internal/display"
	"github.com/lkshrk/ops-pilot/internal/domain"
	"github.com/lkshrk/ops-pilot/internal/events"
)

// Verbosity is how much of the narrative the operator asked for. It is
// deliberately separate from the diagnostic log level: what the program reports
// about its work is a product decision, and was previously a side effect of a
// logging setting, so `logging.level: warn` silently deleted the output.
type Verbosity int

const (
	VerbosityQuiet Verbosity = iota
	VerbosityNormal
	VerbosityVerbose
)

type Options struct {
	Repository     domain.RepositoryRef
	Filter         domain.PullRequestFilter
	RevertedLabel  string
	DeclinedLabel  string
	MergeMethod    string
	MaxFixAttempts int
	// FixAllowedPaths is the operator's declaration of where an approved fix may
	// write. Empty allows nothing.
	FixAllowedPaths []string
	// SettleTimeout is how long one watch window lasts, which is what an
	// operator is agreeing to when they ask to wait rather than revert.
	SettleTimeout time.Duration
	// OnlyPullRequest restricts the run to one pull request.
	OnlyPullRequest int
	// DryRun stops before any external write.
	DryRun bool
	// All processes every pull request, including ones a previous run set aside.
	All bool
	// Verbosity selects how much of the narrative to print.
	Verbosity Verbosity
}

type Runner struct {
	forge        Forge
	observer     Observer
	agent        Agent
	changelogs   Changelogs
	recorder     Recorder
	approver     Approver
	workspace    Workspace
	out          io.Writer
	style        display.Style
	verbosity    Verbosity
	building     *domain.Attempt
	lastActivity string
	events       Events
	redactor     *diagnostics.Redactor
	log          *diagnostics.Logger
	options      Options
	now          func() time.Time
	newID        func() string
	changedFiles map[int][]domain.FileDelta
}

type Dependencies struct {
	Forge      Forge
	Observer   Observer
	Agent      Agent
	Changelogs Changelogs
	Recorder   Recorder
	Approver   Approver
	Workspace  Workspace
	Out        io.Writer
	Log        *diagnostics.Logger
	Redactor   *diagnostics.Redactor
	Events     Events
	Now        func() time.Time
	NewID      func() string
}

func New(deps Dependencies, options Options) *Runner {
	if deps.Now == nil {
		deps.Now = time.Now
	}
	if deps.Out == nil {
		deps.Out = io.Discard
	}
	if deps.Log == nil {
		deps.Log = diagnostics.DiscardLogger()
	}
	if deps.Redactor == nil {
		deps.Redactor = diagnostics.NewRedactor(nil)
	}
	if options.MergeMethod == "" {
		options.MergeMethod = "squash"
	}
	runner := &Runner{
		forge:      deps.Forge,
		observer:   deps.Observer,
		agent:      deps.Agent,
		changelogs: deps.Changelogs,
		recorder:   deps.Recorder,
		approver:   deps.Approver,
		workspace:  deps.Workspace,
		out:        deps.Out,
		style:      display.NewStyle(deps.Out),
		verbosity:  options.Verbosity,
		redactor:   deps.Redactor,
		events:     deps.Events,
		log:        deps.Log,
		options:    options,
		now:        deps.Now,
		newID:      deps.NewID,
	}
	if runner.newID == nil {
		runner.newID = func() string {
			return runner.now().UTC().Format("20060102-150405.000000000")
		}
	}
	return runner
}

// ErrHalted is a run stopped because external state was left in a condition no later attempt could be judged against.
var ErrHalted = errors.New("the run halted")

func halted(reason string) error {
	return &domain.Error{
		Class:     domain.ErrorSystem,
		Operation: "process the queue",
		Cause:     fmt.Errorf("%w: %s", ErrHalted, reason),
	}
}

// Run processes the queue and returns what happened. A pull request that needs
// a human is recorded and the run continues; a pull request that leaves the
// cluster in an unknown state halts it, because every later attempt would be
// attributed against a baseline that is no longer true.
func (r *Runner) Run(ctx context.Context) (domain.Run, error) {
	current := domain.Run{
		ID:         r.newID(),
		Repository: r.options.Repository,
		StartedAt:  r.now(),
		Mode:       r.mode(),
	}
	if r.events != nil {
		r.events.Bind(current)
	}
	if r.recorder != nil {
		if err := r.recorder.StartRun(ctx, current); err != nil {
			r.log.Warnf("could not record the run start: %v", err)
		}
	}
	r.emit(events.Event{Kind: events.RunStarted})
	r.header(current)
	candidates, counts, err := r.discover(ctx)
	if err != nil {
		// Not a halt: nothing was written, so Halted stays empty.
		return r.finish(ctx, current), err
	}
	current.Discovered, current.OtherBranches = counts.discovered, counts.otherBranches
	if r.observer != nil {
		watch := &waiting{runner: r}
		r.observer.Observe(watch.observe)
	}
	r.announce(candidates)
	pending := 0
	for _, candidate := range candidates {
		if candidate.Skip == "" {
			pending++
		}
	}
	position := 0
	for _, candidate := range candidates {
		if ctx.Err() != nil {
			break
		}
		if candidate.Skip != "" {
			attempt := r.skipped(current.ID, candidate)
			current.Attempts = append(current.Attempts, attempt)
			r.record(ctx, attempt)
			r.emit(events.Event{
				Kind:        events.Skipped,
				PullRequest: candidate.PullRequest.Number,
				Dependency:  candidate.Update.Dependency.Name,
				Decision:    string(candidate.Skip),
				Reason:      candidate.Reason,
			})
			if candidate.Skip == domain.DecideSkipSuperseded && !r.options.DryRun {
				r.closeSuperseded(ctx, candidate)
			}
			continue
		}
		position++
		attempt, halt := r.process(ctx, current.ID, candidate, position, pending)
		current.Attempts = append(current.Attempts, attempt)
		r.record(ctx, attempt)
		if halt != "" {
			// Cleaned where it is set, not at each sink: the halt reaches the
			// stream, FinishRun and the summary, and one of them always got missed.
			current.Halted = r.clean(halt)
			r.log.Warnf("run halted: %s", halt)
			break
		}
	}
	current = r.finish(ctx, current)
	if current.Halted != "" {
		return current, halted(current.Halted)
	}
	return current, nil
}

// finish is the only end a run has; every early return goes through it.
func (r *Runner) finish(ctx context.Context, current domain.Run) domain.Run {
	current.FinishedAt = r.now()
	if current.Halted != "" {
		r.emit(events.Event{Kind: events.Halted, Reason: current.Halted})
	}
	if r.recorder != nil {
		writing, done := outliving(ctx)
		defer done()
		if err := r.recorder.FinishRun(writing, current.ID, current.FinishedAt, current.Halted); err != nil {
			r.log.Warnf("could not record the run finish: %v", err)
		}
	}
	r.emit(events.Event{Kind: events.RunFinished})
	return current
}

func (r *Runner) mode() string {
	switch {
	case r.options.DryRun:
		return "dry-run"
	case r.approver != nil && r.approver.Interactive():
		return "interactive"
	default:
		return "non-interactive"
	}
}

// counts is what a run that attempted nothing needs to explain itself. A nil
// count is not zero: it is a number this run could not read.
type counts struct {
	discovered    *int
	otherBranches *int
}

// discover returns the queue and, when they could be established, the counts a
// quiet run is explained by.
func (r *Runner) discover(ctx context.Context) ([]Candidate, counts, error) {
	if r.options.OnlyPullRequest > 0 {
		request, err := r.forge.Get(ctx, r.options.OnlyPullRequest)
		if err != nil {
			return nil, counts{}, err
		}
		return Queue([]domain.PullRequest{request}, r.labels(), r.touched(ctx)), counts{}, nil
	}
	requests, err := r.forge.ListOpen(ctx, r.options.Filter)
	if err != nil {
		return nil, counts{}, err
	}
	// The forge applies the author and label filter, and the base branch
	// narrowing, while it pages, so what either removed can only be counted by
	// asking again without it. Those extra listings are only worth paying for
	// when nothing was left behind, which is the only run that cannot otherwise
	// explain itself.
	var measured counts
	if len(requests) == 0 {
		unfiltered, err := r.forge.ListOpen(ctx, domain.PullRequestFilter{})
		if err != nil {
			r.log.Warnf("could not count the pull requests the filter removed: %v", err)
		} else {
			measured.discovered = counted(len(unfiltered))
		}
		// Unnarrowed by author or label, or the sentence and the number disagree.
		elsewhere, err := r.forge.ListOpen(ctx, domain.PullRequestFilter{OtherBases: true})
		if err != nil {
			r.log.Warnf("could not count the pull requests aimed at other branches: %v", err)
		} else {
			measured.otherBranches = counted(len(elsewhere))
		}
	}
	return Queue(requests, r.labels(), r.touched(ctx)), measured, nil
}

func counted(n int) *int { return &n }

// touched answers which files a pull request rewrites, so supersession can tell
// two deployments of one dependency apart. A lookup that fails leaves the pull
// request open: closing one is a write ops-pilot cannot undo, and Renovate does
// not raise a pull request again once it is closed without a merge.
func (r *Runner) touched(ctx context.Context) Touched {
	return func(number int) ([]string, bool) {
		files, err := r.changedFilesOf(ctx, number)
		if err != nil {
			r.log.Warnf("could not read the files #%d changes: %v", number, err)
			return nil, false
		}
		return pathsOf(files), true
	}
}

// changedFilesOf memoises a pull request's changed files for the run. They are
// stable within a run and a contested pull request is read once in discovery and
// again when it is decided, so only successful reads are cached: a failed one
// retries on the same un-memoised path it always took.
func (r *Runner) changedFilesOf(ctx context.Context, number int) ([]domain.FileDelta, error) {
	if files, ok := r.changedFiles[number]; ok {
		return files, nil
	}
	files, err := r.forge.ChangedFiles(ctx, number)
	if err != nil {
		return nil, err
	}
	if r.changedFiles == nil {
		r.changedFiles = map[int][]domain.FileDelta{}
	}
	r.changedFiles[number] = files
	return files, nil
}

// pathsOf includes a rename's previous path, so a pull request that moved a
// file still shares it with one that edits it where it was.
func pathsOf(files []domain.FileDelta) []string {
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.Path)
		if file.PreviousPath != "" {
			paths = append(paths, file.PreviousPath)
		}
	}
	return paths
}

// emit records a decision or an external write. A nil stream is the normal case.
func (r *Runner) emit(event events.Event) {
	if r.events != nil {
		r.events.Emit(event)
	}
}

func (r *Runner) labels() Labels {
	return Labels{
		Reverted: r.options.RevertedLabel,
		Declined: r.options.DeclinedLabel,
		All:      r.options.All,
	}
}

// clean removes both kinds of secret from untrusted prose: the configured values
// ops-pilot was given, and the credential shapes it was not. The agent reads pod
// logs and repository files, so the second kind is the one it actually meets.
func (r *Runner) clean(value string) string {
	return storable(diagnostics.ScrubSecrets(r.redactor.Redact(value)))
}

// storable is the shared rule: history and the summary table print a stored
// reason back verbatim, so it is kept in the form the whole house agrees on.
func storable(value string) string { return diagnostics.Storable(value) }

func (r *Runner) record(ctx context.Context, attempt domain.Attempt) {
	if r.recorder == nil {
		return
	}
	attempt.Reason = r.clean(attempt.Reason)
	attempt.DiagnosisCause = r.clean(attempt.DiagnosisCause)
	attempt.Error = r.clean(attempt.Error)
	for i := range attempt.Evidence {
		attempt.Evidence[i] = r.clean(attempt.Evidence[i])
	}
	for i := range attempt.Fixes {
		attempt.Fixes[i] = r.clean(attempt.Fixes[i])
	}
	if len(attempt.Broken) > 0 {
		// The caller still renders these objects to stdout and shares the array.
		broken := make([]domain.ObjectHealth, len(attempt.Broken))
		copy(broken, attempt.Broken)
		for i := range broken {
			broken[i].Reason = r.clean(broken[i].Reason)
		}
		attempt.Broken = broken
	}
	writing, done := outliving(ctx)
	defer done()
	if err := r.recorder.RecordAttempt(writing, attempt); err != nil {
		r.log.Warnf("#%d could not record attempt: %v", attempt.PullRequest, err)
	}
}

const outlivingWindow = 5 * time.Second

// outliving detaches work that has to finish even though the caller gave up.
func outliving(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx.Err() == nil {
		return ctx, func() {}
	}
	return context.WithTimeout(context.WithoutCancel(ctx), outlivingWindow)
}

func (r *Runner) skipped(runID string, candidate Candidate) domain.Attempt {
	dependency := candidate.Update.Dependency
	if dependency.Name == "" {
		// The body parsed to nothing, so the title is the only identification.
		dependency.Name = candidate.PullRequest.Title
	}
	return domain.Attempt{
		RunID:       runID,
		PullRequest: candidate.PullRequest.Number,
		Dependency:  dependency,
		Decision:    candidate.Skip,
		Reason:      candidate.Reason,
		Verdict:     domain.VerdictSkipped,
	}
}

// closeSuperseded closes a pull request a newer one replaced. Failing to close
// it is not worth failing the run over.
func (r *Runner) closeSuperseded(ctx context.Context, candidate Candidate) {
	number := candidate.PullRequest.Number
	r.step("Closing #%d: %s.", number, candidate.Reason)
	if err := r.forge.Comment(ctx, number, "Superseded by a newer update. Closed by ops-pilot."); err != nil {
		r.log.Warnf("could not comment on #%d: %v", number, err)
	}
	if err := r.forge.Close(ctx, number); err != nil {
		r.log.Warnf("could not close #%d: %v", number, err)
		return
	}
	r.emit(events.Event{
		Kind:        events.Closed,
		PullRequest: number,
		Dependency:  candidate.Update.Dependency.Name,
		Reason:      candidate.Reason,
	})
}

// state carries one pull request's attempt and, when the cluster is left in a
// state later attempts cannot be judged against, the reason to stop the run.
type state struct {
	attempt domain.Attempt
	halt    string
	now     func() time.Time
}

// failed closes a pull request's section on stdout as well as the log. Every
// success path already reported a conclusion; an error path that only warned on
// stderr left the section with no ending at all.
func (s *state) failed(r *Runner, number int, what string, err error) (domain.Attempt, string) {
	s.attempt.Verdict, s.attempt.Error = domain.VerdictError, err.Error()
	r.emit(events.Event{Kind: events.Failed, PullRequest: number, Reason: what, Error: err.Error()})
	r.outcome(outcomeBad, "Failed", what+": "+err.Error())
	r.log.Warnf("#%d %s: %v", number, what, err)
	return s.done()
}

// setAside closes the section for a pull request this run did not get to act
// on. Nothing was written to the forge and the next run assesses whatever head
// is there from scratch, so it is not a fault anybody has to answer: an ERROR
// row for a routine Renovate rebase is how an operator learns to skim red ones.
func (s *state) setAside(r *Runner, number int, what string, err error) (domain.Attempt, string) {
	s.attempt.Verdict = domain.VerdictSkipped
	s.attempt.Reason, s.attempt.Error = what+": "+err.Error(), err.Error()
	r.emit(events.Event{Kind: events.Skipped, PullRequest: number, Reason: s.attempt.Reason})
	r.outcome(outcomeAsk, "Not merged", err.Error())
	r.log.Infof("#%d %s: %v", number, what, err)
	return s.done()
}

func (s *state) done() (domain.Attempt, string) {
	s.attempt.FinishedAt = s.now()
	s.attempt.Duration = s.attempt.FinishedAt.Sub(s.attempt.StartedAt)
	return s.attempt, s.halt
}

// process runs one pull request all the way to a verdict, and reports whether
// the run may continue afterwards.
func (r *Runner) process(
	ctx context.Context,
	runID string,
	candidate Candidate,
	position, total int,
) (domain.Attempt, string) {
	request, dependency := candidate.PullRequest, candidate.Update.Dependency
	current := &state{
		attempt: domain.Attempt{
			RunID:       runID,
			PullRequest: request.Number,
			Dependency:  dependency,
			HeadSHA:     request.HeadSHA,
			StartedAt:   r.now(),
		},
		now: r.now,
	}
	r.building, r.lastActivity = &current.attempt, ""
	defer func() { r.building = nil }()
	r.headline(position, total, request, dependency)

	decision, reason, changelogSource, err := r.decideCandidate(ctx, &candidate)
	request = candidate.PullRequest
	assessed := events.About(events.Assessed, request, dependency)
	assessed.Decision, assessed.Reason = string(decision), reason
	r.emit(assessed)
	current.attempt.Decision = decision
	current.attempt.Reason = reason
	current.attempt.ChangelogSource = changelogSource
	if err != nil {
		return current.failed(r, request.Number, "could not be assessed", err)
	}
	if decision != domain.DecideMerge {
		current.attempt.Verdict = domain.VerdictSkipped
		r.outcome(outcomeAsk, "Needs your approval", reason)
		return current.done()
	}
	if r.options.DryRun {
		current.attempt.Verdict = domain.VerdictWouldMerge
		current.attempt.Reason = reason
		r.outcome(outcomeGood, "Would merge", reason)
		return current.done()
	}
	return r.mergeAndWatch(ctx, candidate, current)
}

// decide runs the pre-merge gate: a major bump always asks, and everything else
// is put to the agent with whatever changelog could be resolved.
func (r *Runner) decide(
	ctx context.Context,
	candidate Candidate,
) (domain.Decision, string, domain.ChangelogSource, error) {
	return r.decideCandidate(ctx, &candidate)
}

func (r *Runner) decideCandidate(
	ctx context.Context,
	candidate *Candidate,
) (domain.Decision, string, domain.ChangelogSource, error) {
	dependency := candidate.Update.Dependency
	r.step("Reading the pull request.")
	checkoutErr := r.syncWorkspace(ctx, candidate.PullRequest.Number)
	resolved, discarded := r.resolveChangelog(ctx, *candidate)
	r.step("%s", changelogLabel(resolved, discarded))

	files, err := r.changedFilesOf(ctx, candidate.PullRequest.Number)
	if err != nil {
		return "", "", resolved.Source, err
	}
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.Path)
	}
	r.step("Assessing the update against your manifests.")
	// A held bump is assessed like anything else, and only then held back: the
	// agent's changelog search and evidence are the whole content of the question
	// the operator is about to be asked, and skipping it asked them blind.
	bumpHold := bumpHoldReason(dependency)
	var clarifications []ai.Clarification
	for {
		assessment, assessmentFailed, hardHold := r.assess(ctx, *candidate, resolved, paths, clarifications,
			r.assessmentStream(checkoutErr, dependency))
		if assessment.Verdict == domain.AssessmentSafe && hasEvidence(assessment.Evidence) && !dependencyDowngrade(dependency) {
			bumpHold = ""
		}
		// Reads the URL assess stripped, not the one the agent wrote.
		resolved, source, url := reconciledChangelog(resolved, assessment.ChangelogURL)
		var held bool
		assessment, held = applyHolds(assessment, bumpHold, resolved, candidate.PullRequest.Number, checkoutErr, dependencyDowngrade(dependency))
		hardHold = hardHold || held
		r.remember(func(a *domain.Attempt) {
			a.ChangelogURL, a.Evidence = url, assessment.Evidence
		})
		r.evidence(assessment.Verdict != domain.AssessmentSafe, assessment.Evidence)
		if assessment.Verdict == domain.AssessmentDefer {
			return domain.DecideNeedsApproval, assessment.Reason, source, nil
		}
		needsApprovalDiscussion := assessment.Verdict == domain.AssessmentNeedsApproval && strings.TrimSpace(assessment.Diff) == "" && !hardHold
		if assessment.Verdict == domain.AssessmentClarify || needsApprovalDiscussion {
			if strings.TrimSpace(assessment.Question) == "" {
				assessment.Question = "How should ops-pilot proceed with this update?"
			}
			if r.approver == nil || !r.approver.Interactive() {
				if needsApprovalDiscussion {
					return domain.DecideNeedsApproval, assessment.Reason, source, nil
				}
				return domain.DecideNeedsApproval, "needs your answer: " + assessment.Question, source, nil
			}
			answer, answered, err := r.approver.Clarify(Approval{
				PullRequest: candidate.PullRequest,
				Dependency:  dependency,
				Assessment:  assessment,
				Changelog:   resolved,
			}, assessment.Question)
			if err != nil {
				return domain.DecideNeedsApproval, "clarification could not be read: " + err.Error(), source, nil
			}
			if !answered || strings.TrimSpace(answer) == "" {
				return domain.DecideNeedsApproval, "clarification deferred: " + assessment.Question, source, nil
			}
			clarifications = append(clarifications, ai.Clarification{
				Assistant: assistantMessage(assessment),
				Question:  assessment.Question,
				Answer:    answer,
			})
			// The answer may have taken long enough for Renovate to rebase the
			// pull request. Do not let it influence an assessment of a different
			// head; a fresh run can begin a new bounded discussion there.
			refreshed, err := r.forge.Get(ctx, candidate.PullRequest.Number)
			if err != nil {
				return domain.DecideNeedsApproval, "the pull request could not be refreshed after clarification: " + err.Error(), source, nil
			}
			if refreshed.HeadSHA != candidate.PullRequest.HeadSHA {
				return domain.DecideNeedsApproval, "the pull request head moved during clarification", source, nil
			}
			continue
		}
		if assessment.Verdict == domain.AssessmentSafe {
			if len(clarifications) > 0 && !hasEvidence(assessment.Evidence) {
				return domain.DecideNeedsApproval, "the discussion did not produce evidence that the update is safe", source, nil
			}
			return domain.DecideMerge, assessment.Reason, source, nil
		}
		if assessmentFailed && bumpHold == "" {
			return domain.DecideNeedsApproval, assessment.Reason, source, nil
		}
		if strings.TrimSpace(assessment.Diff) != "" {
			if decision, reason, retry := r.applyAssessmentDiff(ctx, candidate, assessment); retry {
				return decision, reason, source, nil
			}
			// The diff landed. The refreshed checkout and changed files are the
			// only inputs that may be reassessed.
			checkoutErr = r.syncWorkspace(ctx, candidate.PullRequest.Number)
			delete(r.changedFiles, candidate.PullRequest.Number)
			files, err = r.changedFilesOf(ctx, candidate.PullRequest.Number)
			if err != nil {
				return "", "", source, err
			}
			paths = paths[:0]
			for _, file := range files {
				paths = append(paths, file.Path)
			}
			clarifications = nil
			continue
		}
		return domain.DecideNeedsApproval, assessment.Reason, source, nil
	}
}

// applyAssessmentDiff is the one pre-merge write path. The operator still sees
// and approves the exact bytes through the established fix prompt; this merely
// points the existing patch validator at the PR's own head.
func (r *Runner) applyAssessmentDiff(
	ctx context.Context,
	candidate *Candidate,
	assessment domain.Assessment,
) (domain.Decision, string, bool) {
	request := candidate.PullRequest
	if r.options.DryRun {
		return domain.DecideNeedsApproval, "dry-run: the proposed change was not applied", true
	}
	if r.approver == nil || !r.approver.Interactive() {
		return domain.DecideNeedsApproval, assessment.Reason, true
	}
	if len(r.options.FixAllowedPaths) == 0 {
		return domain.DecideNeedsApproval, "no fixes.allowedPaths are configured, so the proposed change was not applied", true
	}
	if request.HeadRef == "" || request.HeadSHA == "" || request.HeadRepository == "" || request.HeadRepository != r.options.Repository.String() {
		return domain.DecideNeedsApproval, "the proposed change was not applied because the pull request head is not a writable branch in this repository", true
	}
	if r.building != nil && r.building.FixAttempts >= r.options.MaxFixAttempts {
		return domain.DecideNeedsApproval, "fix attempts exhausted: " + assessment.Reason, true
	}
	diagnosis := domain.Diagnosis{Action: domain.DiagnoseFix, Cause: assessment.Reason, Diff: assessment.Diff}
	approved, err := r.approveFix(request, diagnosis)
	if err != nil {
		return domain.DecideNeedsApproval, "proposed change could not be approved: " + err.Error(), true
	}
	if !approved {
		return domain.DecideNeedsApproval, assessment.Reason, true
	}
	sha, err := r.applyFixAt(ctx, diagnosis, request.HeadRef, request.HeadSHA)
	if r.building != nil {
		r.building.FixAttempts++
	}
	if err != nil {
		return domain.DecideNeedsApproval, "proposed change was not applied: " + err.Error(), true
	}
	r.remember(func(a *domain.Attempt) {
		a.Fixes = append(a.Fixes, assessment.Diff)
		a.HeadSHA = sha
	})
	refreshed, err := r.forge.Get(ctx, request.Number)
	if err != nil {
		return domain.DecideNeedsApproval, "the proposed change landed but the pull request could not be refreshed: " + err.Error(), true
	}
	if refreshed.HeadSHA != sha {
		return domain.DecideNeedsApproval, "the pull request head moved after the proposed change was applied", true
	}
	candidate.PullRequest = refreshed
	return "", "", false
}

// syncWorkspace returns the failure rather than stopping on it. The run
// continues on a tree that is not this pull request's, and the hold applyHolds
// adds is the whole of what keeps that tree from being merged on.
func (r *Runner) syncWorkspace(ctx context.Context, number int) error {
	if r.workspace == nil {
		return nil
	}
	err := r.workspace.SyncPullRequest(ctx, number)
	if err != nil {
		r.log.Warnf("#%d could not be checked out: %v", number, err)
		r.repointWorkspace(ctx, number)
	}
	return err
}

// repointWorkspace puts the tree back on the base branch, which is what the
// cluster is running. It reports nothing upwards: the tree is still not this
// pull request's, so the hold the caller's error carries has to stand whether
// the repointing worked or not.
func (r *Runner) repointWorkspace(ctx context.Context, number int) {
	workspace, ok := r.workspace.(BaseWorkspace)
	if !ok {
		return
	}
	branch, err := r.forge.Branch(ctx)
	if err != nil {
		r.log.Warnf("#%d the base branch could not be read to reset the checkout: %v", number, err)
		return
	}
	if err := workspace.SyncBranch(ctx, branch); err != nil {
		r.log.Warnf("#%d the checkout could not be reset to %s: %v", number, branch, err)
	}
}

// resolveChangelog also reports whether an override discarded release notes the
// pull request itself carried, which changes what the operator is told even
// though the hold comes from the source alone.
func (r *Runner) resolveChangelog(ctx context.Context, candidate Candidate) (domain.Changelog, bool) {
	resolved, err := r.changelogs.Resolve(ctx, candidate.Update)
	if err != nil {
		r.log.Warnf("#%d changelog lookup failed: %v", candidate.PullRequest.Number, err)
		return domain.Changelog{Source: domain.ChangelogNotFound}, false
	}
	return resolved, resolved.Source == domain.ChangelogOverrideEmpty &&
		strings.TrimSpace(candidate.Update.ReleaseNotes) != "" &&
		namesRepository(candidate.Update.Upstream)
}

// assess puts the update to the agent inside the fence-forgery bracket and
// applies the two holds that bracket answers, reporting whether the agent
// failed. A failure is an assessment that holds, not an error: the pull request
// still has to be judged on everything else the run knows.
func (r *Runner) assess(
	ctx context.Context,
	candidate Candidate,
	resolved domain.Changelog,
	paths []string,
	clarifications []ai.Clarification,
	stream func(ai.StreamEvent),
) (domain.Assessment, bool, bool) {
	forged := forgesFence(candidate, resolved, paths)
	if forged {
		// This input is already known to be trying to escape its data fence.
		// Do not render model prose derived from it before the hard hold below.
		stream = nil
	}
	// Only what the agent fences from here on speaks for this pull request.
	ai.TakeFenceForgery()
	assessment, err := r.agent.Assess(ctx, ai.AssessmentRequest{
		PullRequest:    candidate.PullRequest,
		Dependency:     candidate.Update.Dependency,
		Changelog:      asked(resolved),
		ChangedFiles:   paths,
		Clarifications: clarifications,
		Stream:         stream,
	})
	forged = ai.TakeFenceForgery() || forged
	failed := err != nil
	if failed {
		r.log.Warnf("#%d assessment failed: %v", candidate.PullRequest.Number, err)
		assessment = hardHeld(domain.Assessment{
			Verdict: domain.AssessmentNeedsApproval,
			Reason:  "assessment failed: " + err.Error(),
		}, "")
	}
	if forged {
		assessment = hardHeld(assessment, fenceForgedReason)
	}
	// fenceEchoed strips the identifier out of the assessment as well as
	// reporting it, so everything downstream reads the stripped copy.
	echoed := r.fenceEchoed(&assessment)
	if echoed {
		assessment = hardHeld(assessment, fenceEchoReason)
	}
	return assessment, failed, failed || forged || echoed
}

// assessmentStream is intentionally only enabled for an interactive run that
// is still eligible for a conversation. A runner-known hard hold cannot be
// resolved by the model, so there is no assistant turn to render.
func (r *Runner) assessmentStream(checkoutErr error, dependency domain.Dependency) func(ai.StreamEvent) {
	if checkoutErr != nil || dependencyDowngrade(dependency) || r.approver == nil || !r.approver.Interactive() {
		return nil
	}
	return r.approver.Stream
}

func assistantMessage(assessment domain.Assessment) string {
	for _, message := range []string{assessment.Message, assessment.Question, assessment.Reason} {
		if message = strings.TrimSpace(message); message != "" {
			return message
		}
	}
	return "How should ops-pilot proceed with this update?"
}

// reconciledChangelog lets the agent's own find stand in for a changelog the
// resolver could not reach. The source and URL it returns are what the attempt
// records; resolved keeps its own URL where it had one, so the approval prompt
// and the recorded attempt can name different pages.
func reconciledChangelog(
	resolved domain.Changelog,
	found string,
) (domain.Changelog, domain.ChangelogSource, string) {
	source, url := resolved.Source, resolved.URL
	if found != "" &&
		(source == domain.ChangelogNotFound ||
			source == domain.ChangelogOverrideEmpty ||
			source == changelog.SourceIncomplete ||
			source == changelog.SourceUnreadable) {
		source, url = domain.ChangelogFromSearch, found
	}
	resolved.Source = source
	if resolved.URL == "" {
		resolved.URL = url
	}
	return resolved, source, url
}

// applyHolds overrules the agent on what the run knows and it does not. held()
// prepends, so the order these rules run in is the order an operator reads them
// in, outermost first.
func applyHolds(
	assessment domain.Assessment,
	bumpHold string,
	resolved domain.Changelog,
	number int,
	checkoutErr error,
	downgrade bool,
) (domain.Assessment, bool) {
	hardHold := false
	if bumpHold != "" {
		if downgrade {
			assessment = hardHeld(assessment, bumpHold)
			hardHold = true
		} else {
			assessment = held(assessment, bumpHold)
		}
	}
	if resolved.Source == domain.ChangelogOverrideEmpty {
		assessment = held(assessment, "a changelog override is configured for this dependency but "+
			"resolved no releases, so the breaking-change evidence you expected could not be found")
	}
	if resolved.Source == changelog.SourceIncomplete {
		assessment = held(assessment, "the upstream releases for this version range are incomplete - one of "+
			"them could not be placed in the range - so a breaking-change note may be missing from this assessment")
	}
	if resolved.Source == changelog.SourceUnreadable {
		assessment = held(assessment, "the releases of "+resolved.Repository+" could not be read, so this "+
			"assessment was formed without release notes that may exist and may announce a breaking change")
	}
	if checkoutErr != nil {
		assessment.Evidence = markStaleTree(assessment.Evidence)
		assessment = hardHeld(assessment, fmt.Sprintf(
			"#%d could not be checked out, so any manifest the agent read may be from another commit: %v",
			number, checkoutErr))
		hardHold = true
	}
	return assessment, hardHold
}

// bumpHoldReason names the update classes that may never merge on the agent's
// verdict alone. BumpUnknown has two sources - a declared update type ops-pilot
// does not read, such as replacement or rollback, and a version pair the
// arithmetic cannot classify - and only the second is visible from here, since
// the declared column is not carried past the parser. A downgrade is read
// before either, because a rollback both declares a type and moves the numbers
// backwards, so whichever source produced it the direction is the true reason.
func bumpHoldReason(dependency domain.Dependency) string {
	switch dependency.Bump {
	case domain.BumpMajor:
		return "major version bump"
	case domain.BumpUnknown:
		if renovate.Newer(dependency.ToVersion, dependency.FromVersion) < 0 {
			return "the new version is older than the one deployed, so this would roll the dependency back"
		}
		if renovate.CompareVersions(dependency.FromVersion, dependency.ToVersion) != domain.BumpUnknown {
			return "unrecognised update type"
		}
		if why := digestHoldReason(dependency.FromVersion, dependency.ToVersion); why != "" {
			return why
		}
		return "the version change could not be classified, so it may be hiding a major bump"
	default:
		return ""
	}
}

func dependencyDowngrade(dependency domain.Dependency) bool {
	if renovate.Newer(dependency.ToVersion, dependency.FromVersion) < 0 {
		return true
	}
	_, from, fromAt := strings.Cut(dependency.FromVersion, "@")
	_, to, toAt := strings.Cut(dependency.ToVersion, "@")
	return fromAt && toAt && renovate.Newer(to, from) < 0
}

func hasEvidence(evidence []string) bool {
	for _, item := range evidence {
		if strings.TrimSpace(item) != "" {
			return true
		}
	}
	return false
}

// digestHoldReason names the side an unreadable digest sits on, for the update
// whose version text is identical and whose digest is the only thing that moved.
func digestHoldReason(from, to string) string {
	fromTag, fromDigest, fromPinned := strings.Cut(strings.TrimSpace(from), "@")
	toTag, toDigest, toPinned := strings.Cut(strings.TrimSpace(to), "@")
	// Renovate's body parser takes whatever sits between the backticks, so the
	// text before the @ has to read as a version before this may call it one: in
	// a name@version cell it is the name, and the version is what moved.
	if !fromPinned && !toPinned || fromTag != toTag || !parsesAsVersion(fromTag) {
		return ""
	}
	unread := ""
	fromOK := renovate.IsDigest(strings.TrimSpace(fromDigest))
	toOK := renovate.IsDigest(strings.TrimSpace(toDigest))
	switch {
	case fromOK && toOK:
		return ""
	case !fromOK && !toOK:
		unread = "neither reference carries a readable digest"
	case !fromOK:
		unread = "the old reference carries no readable digest"
	default:
		unread = "the new reference carries no readable digest"
	}
	return "the version text is identical on both sides and only the image digest moved, but " +
		unread + ", so it cannot be shown that only the image layers changed"
}

// parsesAsVersion must agree with the parser's own versionParts, or this calls
// text a version that the classification never read as one.
func parsesAsVersion(value string) bool {
	value = strings.TrimPrefix(strings.TrimSpace(value), "v")
	if index := strings.IndexAny(value, "-+"); index >= 0 {
		value = value[:index]
	}
	if value == "" {
		return false
	}
	for _, segment := range strings.Split(value, ".") {
		if segment == "" {
			return false
		}
		for _, r := range segment {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

const staleTreeMarker = "[unverified: checkout failed; may describe another commit]"

// Every sink keeps evidence apart from the reason, so the mark rides per point.
func markStaleTree(points []string) []string {
	if len(points) == 0 {
		return nil
	}
	marked := make([]string, len(points))
	for i, point := range points {
		marked[i] = staleTreeMarker + " " + point
	}
	return marked
}

const (
	fenceEchoReason = "this assessment named ops-pilot's data-fence identifier, " +
		"which the model reports only when untrusted input tried to instruct it"
	fenceEchoMarker   = "[fence marker]"
	fenceForgedReason = "untrusted text about this pull request forged ops-pilot's data fence, " +
		"so something in it is written to be read as an instruction"
)

// forgesFence reports untrusted input impersonating the data fence. It covers
// what the runner hands the agent; anything the agent fetches for itself is
// caught where the toolbox fences it. Both directions of this signal are one
// way: a forgery only ever adds a hold, and its absence grants nothing, because
// an injection that never touches a marker looks exactly like a changelog.
func forgesFence(candidate Candidate, resolved domain.Changelog, paths []string) bool {
	fields := append([]string{
		candidate.PullRequest.Title,
		candidate.PullRequest.Body,
		candidate.Update.ReleaseNotes,
		candidate.Update.Upstream,
		candidate.Update.Dependency.Name,
		candidate.Update.Dependency.FromVersion,
		candidate.Update.Dependency.ToVersion,
		resolved.Text,
		resolved.Repository,
		resolved.URL,
	}, paths...)
	for _, field := range fields {
		if ai.FenceForged(field) {
			return true
		}
	}
	return false
}

// fenceEchoed reports that this assessment named a fence identifier ops-pilot
// has issued, live or retired, and strips it from the published text. The identifier is stated to the model
// outside every fence (ai.FenceIdentifier) and every payload is stripped of it
// before fencing, so its appearance is the model describing the fence, never
// untrusted text leaking through. The hold falls on this assessment alone: the
// identifier is retired per request, so naming it says nothing about the next
// one. The marker is stripped because the reason is printed, emitted and posted
// publicly.
func (r *Runner) fenceEchoed(assessment *domain.Assessment) bool {
	reason, echoed := ai.StripFenceIdentifiers(assessment.Reason, fenceEchoMarker)
	url, urlEchoed := ai.StripFenceIdentifiers(assessment.ChangelogURL, fenceEchoMarker)
	question, questionEchoed := ai.StripFenceIdentifiers(assessment.Question, fenceEchoMarker)
	diff, diffEchoed := ai.StripFenceIdentifiers(assessment.Diff, fenceEchoMarker)
	assessment.Reason, assessment.ChangelogURL, assessment.Question, assessment.Diff = reason, url, question, diff
	echoed = echoed || urlEchoed || questionEchoed || diffEchoed
	for i, point := range assessment.Evidence {
		masked, pointEchoed := ai.StripFenceIdentifiers(point, fenceEchoMarker)
		assessment.Evidence[i] = masked
		echoed = echoed || pointEchoed
	}
	return echoed
}

// held puts the operator back in the loop, keeping the agent's own words behind
// the reason it was overruled: the approval prompt shows that text verbatim and
// it is the whole content of the question being asked.
func held(assessment domain.Assessment, why string) domain.Assessment {
	// An ordinary hold (major bump, incomplete changelog) is additional context,
	// not a fact that makes the model's question meaningless. Preserve a valid
	// clarification or an explicit defer; hardHeld is used where runner-owned
	// facts must suppress the model's conclusion instead.
	if (assessment.Verdict == domain.AssessmentClarify && strings.TrimSpace(assessment.Question) == "") ||
		(assessment.Verdict != domain.AssessmentClarify && assessment.Verdict != domain.AssessmentDefer) {
		assessment.Verdict = domain.AssessmentNeedsApproval
	}
	assessment.Reason = strings.TrimSpace(assessment.Reason)
	if assessment.Reason == "" {
		assessment.Reason = why
		return assessment
	}
	assessment.Reason = why + ": " + assessment.Reason
	return assessment
}

// hardHeld is for runner-known conditions that discussion cannot change. A
// model-supplied question or diff would otherwise turn a checkout, fence, or
// downgrade safety hold into an interaction or write path.
func hardHeld(assessment domain.Assessment, why string) domain.Assessment {
	assessment.Question, assessment.Diff = "", ""
	assessment.Verdict = domain.AssessmentNeedsApproval
	if why == "" {
		return assessment
	}
	return held(assessment, why)
}

// mergeAndWatch merges, triggers Flux, and watches the cluster react. A failed
// or stalled window goes to the repair loop.
func (r *Runner) mergeAndWatch(
	ctx context.Context,
	candidate Candidate,
	current *state,
) (domain.Attempt, string) {
	request := candidate.PullRequest
	baseline, err := r.observer.Snapshot(ctx)
	if err != nil {
		return current.failed(r, request.Number, "could not read the cluster", err)
	}
	// The branch head before the merge is what a revert has to restore. The
	// pull request's own base SHA is where it was branched from, which may be
	// several merges old by now.
	preMergeHead, err := r.branchHead(ctx)
	if err != nil {
		return current.failed(r, request.Number, "could not read the branch head", err)
	}
	if err := r.confirmAssessedHead(ctx, request); err != nil {
		if errors.Is(err, errHeadMoved) {
			return current.setAside(r, request.Number, notMerged, err)
		}
		return current.failed(r, request.Number, notMerged, err)
	}
	r.step("Merging.")
	mergeSHA, err := r.forge.Merge(ctx, request.Number, request.HeadSHA, r.options.MergeMethod)
	if err != nil {
		if errors.Is(err, github.ErrHeadModified) {
			return current.setAside(r, request.Number, notMerged, err)
		}
		asking, done := outliving(ctx)
		landed, readBack := r.forge.MergeState(asking, request.Number)
		if readBack != nil {
			lost, cause := publishable(err.Error()), publishable(readBack.Error())
			current.attempt.Verdict = domain.VerdictError
			current.attempt.PreMergeSHA = preMergeHead
			current.attempt.Error = "the merge answer was lost (" + lost + ") and could not be re-read: " + cause
			current.halt = fmt.Sprintf("#%d may or may not have merged: %s", request.Number, cause)
			r.log.Warnf("#%d the merge answer was lost and could not be re-read: %v", request.Number, readBack)
			r.outcome(outcomeBad, "The merge outcome is unknown; the run stopped", cause)
			if annotation := r.annotateUnknownMerge(asking, request.Number, preMergeHead, err, readBack); annotation != nil {
				current.attempt.Error = unannounced(current.attempt.Error, annotation)
				current.halt = unannounced(current.halt, annotation)
			}
			done()
			return current.done()
		}
		done()
		if !landed.Merged {
			return current.failed(r, request.Number, "could not be merged", err)
		}
		r.log.Warnf("#%d the merge answer was lost, but the forge reports it merged as %s: %v",
			request.Number, shortSHA(landed.SHA), err)
		mergeSHA = landed.SHA
	}
	current.attempt.PreMergeSHA, current.attempt.MergeSHA = preMergeHead, mergeSHA
	merged := events.About(events.Merged, request, candidate.Update.Dependency)
	merged.SHA = mergeSHA
	r.emit(merged)
	r.step("Merged as %s onto %s.", shortSHA(mergeSHA), shortSHA(preMergeHead))
	r.resetWaiting()

	if err := r.observer.Reconcile(ctx); err != nil {
		r.log.Warnf("#%d could not trigger reconciliation: %v", request.Number, err)
	}
	outcome, err := r.observer.Watch(ctx, baseline, mergeSHA)
	if err != nil {
		// The merge landed and the cluster's reaction is unknown, so no later
		// attempt can be attributed against this baseline.
		masked := publishable(err.Error())
		current.attempt.Verdict, current.attempt.Error = domain.VerdictError, masked
		current.halt = fmt.Sprintf("#%d merged but could not be observed: %s", request.Number, masked)
		r.log.Warnf("#%d the cluster could not be observed after the merge, keeping the merge: %v", request.Number, err)
		r.outcome(outcomeBad, "Cluster unreadable after the merge; it was left in place", masked)
		asking, done := outliving(ctx)
		if annotation := r.annotateLeftInPlace(asking, request.Number, "the cluster could not be observed after the merge", err); annotation != nil {
			current.attempt.Error = unannounced(current.attempt.Error, annotation)
			current.halt = unannounced(current.halt, annotation)
		}
		done()
		return current.done()
	}
	current.attempt.Watch = outcome.Result
	watched := events.About(events.WatchResult, request, candidate.Update.Dependency)
	watched.Watch, watched.Objects = string(outcome.Result), events.Objects(failuresOf(outcome))
	r.emit(watched)
	if outcome.Result == domain.WatchPass {
		current.attempt.Verdict = domain.VerdictMerged
		r.outcome(outcomeGood, "Healthy", "")
		return current.done()
	}
	return r.repair(ctx, candidate, baseline, preMergeHead, outcome, current)
}

// notMerged separates a pull request that changed underneath the run from one
// the merge itself refused: nothing was written, and a later run assesses
// whatever is there now from scratch.
const notMerged = "was not merged"

// errHeadMoved is the pre-merge re-read finding a head other than the assessed
// one, which is the same event GitHub reports as github.ErrHeadModified.
var errHeadMoved = errors.New("the head moved after it was assessed")

// confirmAssessedHead re-reads the pull request immediately before the merge.
// The queue is captured once, so a rebase between discovery and here would
// otherwise be caught only by GitHub's own refusal of a stale SHA. Merging the
// head that is there now instead is not the remedy: it was never assessed, and
// pinning the merge to the assessed head is what stops a wrong version
// deploying. A head that cannot be read is not the assessed head either.
func (r *Runner) confirmAssessedHead(ctx context.Context, request domain.PullRequest) error {
	latest, err := r.forge.Get(ctx, request.Number)
	if err != nil {
		return fmt.Errorf("the head could not be re-read before merging: %w", err)
	}
	if latest.HeadSHA != request.HeadSHA {
		return fmt.Errorf("%w: it is now %s, not %s",
			errHeadMoved, shortSHA(latest.HeadSHA), shortSHA(request.HeadSHA))
	}
	return nil
}

func (r *Runner) branchHead(ctx context.Context) (string, error) {
	branch, err := r.forge.Branch(ctx)
	if err != nil {
		return "", err
	}
	return r.forge.BranchHead(ctx, branch)
}

// namesRepository reports whether an attribution spells a repository at all.
// Renovate writes owner/name, optionally behind a forge or registry path, and
// nothing else; a value that names no repository cannot be the "other
// repository" the discard label claims, so it reads as no attribution.
func namesRepository(attribution string) bool {
	fields := strings.Fields(attribution)
	if len(fields) != 1 {
		return false
	}
	owner, name, found := strings.Cut(fields[0], "/")
	return found && owner != "" && name != ""
}

// asked is the changelog as the agent is told about it. A range suppressed as
// incomplete or an upstream that could not be read carries no text, so both are
// put the same question as an upstream that published nothing: go and find one.
func asked(resolved domain.Changelog) domain.Changelog {
	if resolved.Source == changelog.SourceIncomplete || resolved.Source == changelog.SourceUnreadable {
		resolved.Source = domain.ChangelogNotFound
	}
	return resolved
}

// changelogLabel reports how the changelog was resolved. discarded is the case
// the resolver cannot express: the pull request carried release notes and they
// were thrown away because a configured override names another repository, so
// "nothing found" would be false in the direction that misleads.
func changelogLabel(resolved domain.Changelog, discarded bool) string {
	switch resolved.Source {
	case domain.ChangelogFromPullRequest:
		return "Changelog taken from the pull request body."
	case domain.ChangelogFromAnnotation:
		return "Changelog taken from " + resolved.Repository + ", found through the image annotation."
	case domain.ChangelogFromOverride:
		return "Changelog taken from " + resolved.Repository + ", named in configuration."
	case changelog.SourceIncomplete:
		return "The releases published by " + resolved.Repository + " do not account for this whole version " +
			"range, so the changelog was not used. This update needs manual approval."
	case changelog.SourceUnreadable:
		return "The releases of " + resolved.Repository + " could not be read, so it is not known whether " +
			"this range has a changelog. This update needs manual approval."
	case domain.ChangelogOverrideEmpty:
		if discarded {
			return "The pull request's release notes were discarded: they are attributed to a " +
				"repository other than the one named in configuration, which resolved no releases. " +
				"This update needs manual approval."
		}
		return "A configured changelog override resolved no releases, so the expected " +
			"breaking-change evidence is absent. This update needs manual approval."
	}
	return "No changelog found automatically; the agent will look for one."
}

// resetWaiting starts a fresh cadence so each watch window reports from zero.
func (r *Runner) resetWaiting() {
	if r.observer == nil {
		return
	}
	watch := &waiting{runner: r}
	r.observer.Observe(watch.observe)
}

// remember mutates the attempt being built. The pre-merge gate learns things
// worth recording well before the attempt is finished.
func (r *Runner) remember(apply func(*domain.Attempt)) {
	if r.building != nil {
		apply(r.building)
	}
}
