package e2e_test

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/lkshrk/ops-pilot/internal/ai"
	"github.com/lkshrk/ops-pilot/internal/cluster"
	"github.com/lkshrk/ops-pilot/internal/domain"
	"github.com/lkshrk/ops-pilot/internal/run"
)

const revertedLabel = "ops-pilot/reverted"

type harness struct {
	forge      *forge
	observer   *observer
	agent      *agent
	changelogs changelogs
	approver   *approver
	recorder   *recorder
	workspace  *workspace
	watchFails *failingObserver
	out        *bytes.Buffer
	options    run.Options
}

func newHarness() *harness {
	return &harness{
		forge:      newForge(),
		observer:   &observer{},
		agent:      &agent{},
		changelogs: fromBody(),
		approver:   &approver{},
		recorder:   &recorder{},
		workspace:  &workspace{},
		out:        &bytes.Buffer{},
		options: run.Options{
			Repository:     domain.RepositoryRef{Owner: "lkshrk", Name: "h-cloud", Branch: "main"},
			RevertedLabel:  revertedLabel,
			DeclinedLabel:  declinedLabel,
			MergeMethod:    "squash",
			MaxFixAttempts: 2,
			// Empty would refuse every fix, so the harness has to declare a path.
			FixAllowedPaths: []string{"apps/**"},
			Verbosity:       run.VerbosityNormal,
		},
	}
}

func (h *harness) run(t *testing.T) domain.Run {
	t.Helper()
	clock := time.Date(2026, 8, 2, 14, 0, 0, 0, time.UTC)
	var watcher run.Observer = h.observer
	if h.watchFails != nil {
		watcher = h.watchFails
	}
	runner := run.New(run.Dependencies{
		Forge:      h.forge,
		Observer:   watcher,
		Agent:      h.agent,
		Changelogs: h.changelogs,
		Recorder:   h.recorder,
		Approver:   h.approver,
		Workspace:  h.workspace,
		Out:        h.out,
		Now: func() time.Time {
			clock = clock.Add(time.Second)
			return clock
		},
		NewID: func() string { return "run-1" },
	}, h.options)

	result, err := runner.Run(context.Background())
	switch {
	case err != nil && result.Halted == "":
		t.Fatalf("run: %v\n%s", err, h.out)
	case err == nil && result.Halted != "":
		t.Fatalf("a halted run reported success: %q\n%s", result.Halted, h.out)
	}
	return result
}

func sonarr(number int, update, from, to, notes string) domain.PullRequest {
	return domain.PullRequest{
		Number:  number,
		Title:   "chore(container): update sonarr",
		Author:  "renovate[bot]",
		HeadSHA: "head00" + string(rune('0'+number%10)),
		BaseSHA: "base000",
		BaseRef: "main",
		Body: renovateBody(
			"ghcr.io/home-operations/sonarr", "image", update, from, to, notes,
		),
	}
}

// gatus is a second, unrelated dependency, so a queue of two pull requests is
// genuinely two candidates rather than one superseding the other.
func gatus(number int, update, from, to string) domain.PullRequest {
	request := sonarr(number, update, from, to, "Routine.")
	request.Body = renovateBody("ghcr.io/twin/gatus", "image", update, from, to, "Routine.")
	return request
}

func only(t *testing.T, result domain.Run) domain.Attempt {
	t.Helper()
	if len(result.Attempts) != 1 {
		t.Fatalf("want exactly one attempt, got %d: %+v", len(result.Attempts), result.Attempts)
	}
	return result.Attempts[0]
}

// 1. A safe bump merges and the cluster comes up healthy.
func TestCleanMergeReachesVerified(t *testing.T) {
	h := newHarness()
	h.forge.add(sonarr(1204, "patch", "4.0.14", "4.0.19", "Fixed a crash."), []string{"apps/sonarr.yaml"})
	h.agent.assessments = []domain.Assessment{safe("patch release, no configuration change")}
	h.observer.outcomes = []cluster.Outcome{pass()}

	attempt := only(t, h.run(t))

	if attempt.Verdict != domain.VerdictMerged {
		t.Fatalf("want merged, got %q (%s)", attempt.Verdict, attempt.Reason)
	}
	if len(h.forge.Merges) != 1 || h.forge.Merges[0] != 1204 {
		t.Fatalf("want exactly one merge of #1204, got %v", h.forge.Merges)
	}
	if h.observer.Reconciles != 1 {
		t.Fatalf("want exactly one reconcile trigger, got %d", h.observer.Reconciles)
	}
	if len(h.forge.Commits) != 0 {
		t.Fatalf("want no commits on a clean merge, got %+v", h.forge.Commits)
	}
	if attempt.MergeSHA == "" {
		t.Fatal("merge SHA was not recorded")
	}
}

// 2. A major bump is assessed like anything else. When every breaking change
// is proven irrelevant to this deployment, the version number alone is no hold.
func TestIrrelevantMajorBreakingChangeMergesWithoutApproval(t *testing.T) {
	h := newHarness()
	h.forge.add(sonarr(1207, "major", "3.9.0", "4.0.0", "Breaking."), []string{"apps/sonarr.yaml"})
	h.agent.assessments = []domain.Assessment{safe("no manifest touches the removed API")}
	h.observer.outcomes = []cluster.Outcome{pass()}

	attempt := only(t, h.run(t))

	if attempt.Verdict != domain.VerdictMerged || attempt.Decision != domain.DecideMerge {
		t.Fatalf("want a deployment-safe major bump merged, got %+v", attempt)
	}
	if attempt.Reason != "no manifest touches the removed API" {
		t.Fatalf("reason: %q", attempt.Reason)
	}
	if h.agent.Assessed != 1 {
		t.Fatal("a major bump must still be assessed, for the changelog and the evidence")
	}
	if len(h.forge.Merges) != 1 {
		t.Fatalf("want one merge, got %v", h.forge.Merges)
	}
}

// 3. A breaking change the deployment actually uses stops before merging.
func TestBreakingChangeTouchingOurConfigNeedsApproval(t *testing.T) {
	h := newHarness()
	h.forge.add(sonarr(1209, "minor", "4.0.14", "4.1.0", "env is now envs."), []string{"apps/sonarr.yaml"})
	h.agent.assessments = []domain.Assessment{needsApproval("renames values.env, which this HelmRelease sets")}

	attempt := only(t, h.run(t))

	if attempt.Verdict != domain.VerdictSkipped {
		t.Fatalf("want skipped, got %+v", attempt)
	}
	if !strings.Contains(attempt.Reason, "values.env") {
		t.Fatalf("want the agent's reason preserved, got %q", attempt.Reason)
	}
	if len(h.forge.Merges) != 0 {
		t.Fatal("a breaking change must not merge")
	}
}

// 4. No changelog anywhere still reaches the agent, which is expected to look
// for one itself and to ask when it cannot.
func TestNoChangelogAnywhereNeedsApproval(t *testing.T) {
	h := newHarness()
	h.changelogs = notFound()
	h.forge.add(sonarr(1211, "patch", "4.0.14", "4.0.15", ""), []string{"apps/sonarr.yaml"})
	h.agent.assessments = []domain.Assessment{needsApproval("no changelog found for this version")}

	attempt := only(t, h.run(t))

	if attempt.Verdict != domain.VerdictSkipped {
		t.Fatalf("want skipped, got %+v", attempt)
	}
	if attempt.ChangelogSource != domain.ChangelogNotFound {
		t.Fatalf("want the not-found source recorded, got %q", attempt.ChangelogSource)
	}
}

const helmRelease = `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
spec:
  values:
    env:
      TZ: Europe/Berlin
`

const fixDiff = `--- a/apps/sonarr.yaml
+++ b/apps/sonarr.yaml
@@ -4,2 +4,2 @@
   values:
-    env:
+    envs:
`

// 5. A failure the agent can fix is fixed, with the operator's approval, and
// the pull request stays merged.
func TestFailureFixedWithApprovalStaysMerged(t *testing.T) {
	h := newHarness()
	h.approver.interactive, h.approver.fixAnswer = true, true
	h.forge.add(sonarr(1209, "minor", "0.3.6", "0.4.0", "env renamed"), []string{"apps/sonarr.yaml"})
	h.forge.fileAt("merge1209", "apps/sonarr.yaml", []byte(helmRelease))
	h.agent.assessments = []domain.Assessment{safe("no configuration change expected")}
	h.agent.diagnoses = []domain.Diagnosis{{
		Action: domain.DiagnoseFix,
		Cause:  "values.env was renamed to values.envs in 0.4.0",
		Diff:   fixDiff,
	}}
	h.observer.outcomes = []cluster.Outcome{broke("sonarr"), pass()}

	attempt := only(t, h.run(t))

	if attempt.Verdict != domain.VerdictFixed {
		t.Fatalf("want fixed, got %q (%s)", attempt.Verdict, attempt.Reason)
	}
	if attempt.FixAttempts != 1 {
		t.Fatalf("want one fix attempt, got %d", attempt.FixAttempts)
	}
	if len(h.forge.Commits) != 1 {
		t.Fatalf("want exactly one commit, got %d", len(h.forge.Commits))
	}
	applied := string(h.forge.Commits[0].Changes[0].Contents)
	if !strings.Contains(applied, "    envs:") {
		t.Fatalf("the fix was not applied to the file:\n%s", applied)
	}
	if len(h.forge.Labels[1209]) != 0 {
		t.Fatalf("a fixed pull request must not be labelled reverted: %v", h.forge.Labels[1209])
	}
}

// 6. Declining the fix reverts, labels, and comments.
func TestDeclinedFixReverts(t *testing.T) {
	h := newHarness()
	h.approver.interactive, h.approver.fixAnswer = true, false
	h.forge.add(sonarr(1209, "minor", "0.3.6", "0.4.0", "env renamed"), []string{"apps/sonarr.yaml"})
	h.forge.fileAt("base000", "apps/sonarr.yaml", []byte(helmRelease))
	h.agent.assessments = []domain.Assessment{safe("looks routine")}
	h.agent.diagnoses = []domain.Diagnosis{{
		Action: domain.DiagnoseFix,
		Cause:  "values.env was renamed",
		Diff:   fixDiff,
	}}
	h.observer.outcomes = []cluster.Outcome{broke("sonarr")}

	attempt := only(t, h.run(t))

	if attempt.Verdict != domain.VerdictReverted {
		t.Fatalf("want reverted, got %q (%s)", attempt.Verdict, attempt.Reason)
	}
	if len(h.forge.Commits) != 1 {
		t.Fatalf("want exactly one revert commit, got %d", len(h.forge.Commits))
	}
	restored := h.forge.Commits[0].Changes[0]
	if restored.Path != "apps/sonarr.yaml" || string(restored.Contents) != helmRelease {
		t.Fatalf("the revert did not restore the pre-merge content: %+v", restored)
	}
	if h.observer.Restores != 1 {
		t.Fatalf("want the cluster checked for recovery once, got %d", h.observer.Restores)
	}
	assertReverted(t, h, 1209, "values.env was renamed")
}

// 7. An unfixable failure reverts without asking anyone.
func TestUnfixableFailureReverts(t *testing.T) {
	h := newHarness()
	h.forge.add(sonarr(1209, "minor", "0.3.6", "0.4.0", "rewrite"), []string{"apps/sonarr.yaml"})
	h.forge.fileAt("base000", "apps/sonarr.yaml", []byte(helmRelease))
	h.agent.assessments = []domain.Assessment{safe("looks routine")}
	h.agent.diagnoses = []domain.Diagnosis{{
		Action: domain.DiagnoseUnfixable,
		Cause:  "0.4.0 requires a database migration this chart cannot perform",
	}}
	h.observer.outcomes = []cluster.Outcome{broke("sonarr")}

	attempt := only(t, h.run(t))

	if attempt.Verdict != domain.VerdictReverted {
		t.Fatalf("want reverted, got %+v", attempt)
	}
	if h.approver.FixAsks != 0 {
		t.Fatal("an unfixable diagnosis must not prompt for a fix")
	}
	assertReverted(t, h, 1209, "database migration")
}

// 7b. An operator at the terminal is asked before a merge is discarded, and can
// keep it. The run continues to the next pull request.
func TestOperatorCanKeepAMergeInsteadOfRevertingIt(t *testing.T) {
	h := newHarness()
	h.approver.interactive = true
	h.approver.revertAnswer = run.RevertKeep
	h.forge.add(sonarr(1209, "minor", "0.3.6", "0.4.0", "rewrite"), []string{"apps/sonarr.yaml"})
	h.forge.fileAt("base000", "apps/sonarr.yaml", []byte(helmRelease))
	h.agent.assessments = []domain.Assessment{safe("looks routine")}
	h.agent.diagnoses = []domain.Diagnosis{{
		Action: domain.DiagnoseUnfixable,
		Cause:  "0.4.0 requires a database migration this chart cannot perform",
	}}
	h.observer.outcomes = []cluster.Outcome{broke("sonarr")}

	attempt := only(t, h.run(t))

	if attempt.Verdict != domain.VerdictKept {
		t.Fatalf("want the merge kept, got %+v", attempt)
	}
	if h.approver.RevertAsks != 1 {
		t.Fatalf("want one revert prompt, got %d", h.approver.RevertAsks)
	}
	if len(h.forge.Commits) != 0 {
		t.Fatalf("keeping a merge must write nothing, got %+v", h.forge.Commits)
	}
	if len(h.forge.Labels[1209]) != 0 {
		t.Fatalf("a kept merge must not be labelled reverted: %v", h.forge.Labels[1209])
	}
	if len(h.approver.LastRevert.Broken) == 0 {
		t.Fatal("the operator must be told what is still unhealthy")
	}
}

// 7c. Answering "wait" buys another window, and a cluster that recovers in it
// keeps the merge without anyone reverting anything.
func TestOperatorCanWaitInsteadOfRevertingAndTheMergeSurvives(t *testing.T) {
	h := newHarness()
	h.approver.interactive = true
	h.approver.revertAnswer = run.RevertWait
	h.forge.add(sonarr(1209, "minor", "0.3.6", "0.4.0", "rewrite"), []string{"apps/sonarr.yaml"})
	h.forge.fileAt("base000", "apps/sonarr.yaml", []byte(helmRelease))
	h.agent.assessments = []domain.Assessment{safe("looks routine")}
	h.agent.diagnoses = []domain.Diagnosis{{
		Action: domain.DiagnoseUnfixable,
		Cause:  "the new image never becomes ready",
	}}
	h.observer.outcomes = []cluster.Outcome{broke("sonarr")}
	h.observer.restored = []cluster.Outcome{pass()}

	attempt := only(t, h.run(t))

	if attempt.Verdict != domain.VerdictMerged {
		t.Fatalf("want the merge kept after the extra window, got %+v", attempt)
	}
	if h.observer.Restores != 1 {
		t.Fatalf("want one extra window, got %d", h.observer.Restores)
	}
	if len(h.forge.Commits) != 0 {
		t.Fatalf("waiting must write nothing, got %+v", h.forge.Commits)
	}
}

// 7d. Diagnosing takes minutes and a rollout can finish inside them. A merge is
// never discarded over an object that has since recovered.
func TestRecoveryWhileDiagnosingKeepsTheMerge(t *testing.T) {
	h := newHarness()
	h.forge.add(sonarr(1209, "minor", "0.3.6", "0.4.0", "rewrite"), []string{"apps/sonarr.yaml"})
	h.forge.fileAt("base000", "apps/sonarr.yaml", []byte(helmRelease))
	h.agent.assessments = []domain.Assessment{safe("looks routine")}
	h.agent.diagnoses = []domain.Diagnosis{{
		Action: domain.DiagnoseUnfixable,
		Cause:  "no repository defect can be identified",
	}}
	h.observer.outcomes = []cluster.Outcome{broke("sonarr")}
	h.observer.Recovered = true

	attempt := only(t, h.run(t))

	if attempt.Verdict != domain.VerdictMerged {
		t.Fatalf("want the merge kept, got %+v", attempt)
	}
	if len(h.forge.Commits) != 0 {
		t.Fatalf("want no revert commit, got %+v", h.forge.Commits)
	}
	if len(h.forge.Labels[1209]) != 0 {
		t.Fatalf("a merge that recovered must not be labelled reverted: %v", h.forge.Labels[1209])
	}
}

// 8. A stall the agent calls benign is waited out, and the merge survives.
func TestStalledWatchTheAgentCallsBenignPasses(t *testing.T) {
	h := newHarness()
	h.forge.add(sonarr(1204, "patch", "4.0.14", "4.0.19", "Fixed a crash."), []string{"apps/sonarr.yaml"})
	h.agent.assessments = []domain.Assessment{safe("patch release")}
	h.agent.diagnoses = []domain.Diagnosis{{
		Action: domain.DiagnoseBenignWait,
		Cause:  "the image is 2 GiB and the node is still pulling it",
	}}
	h.observer.outcomes = []cluster.Outcome{stalled("sonarr"), pass()}

	attempt := only(t, h.run(t))

	if attempt.Verdict != domain.VerdictMerged {
		t.Fatalf("want merged after the extension, got %q (%s)", attempt.Verdict, attempt.Reason)
	}
	if !h.agent.LastDiagnosis.Stalled {
		t.Fatal("the agent must be told the window stalled rather than failed")
	}
	if len(h.forge.Commits) != 0 {
		t.Fatal("waiting must not write anything")
	}
}

// 8b. A stall that persists after its extension cannot ask to wait again: the
// agent is told so, and its next answer decides. This is OPEN-1.
func TestSecondStallForbidsAnotherWaitAndReverts(t *testing.T) {
	h := newHarness()
	h.forge.add(sonarr(1204, "patch", "4.0.14", "4.0.19", "Fixed a crash."), []string{"apps/sonarr.yaml"})
	h.forge.fileAt("base000", "apps/sonarr.yaml", []byte(helmRelease))
	h.agent.assessments = []domain.Assessment{safe("patch release")}
	h.agent.diagnoses = []domain.Diagnosis{
		{Action: domain.DiagnoseBenignWait, Cause: "still pulling"},
		{Action: domain.DiagnoseUnfixable, Cause: "the image never becomes ready"},
	}
	h.observer.outcomes = []cluster.Outcome{stalled("sonarr")}
	h.observer.restored = []cluster.Outcome{stalled("sonarr"), pass()}

	attempt := only(t, h.run(t))

	if attempt.Verdict != domain.VerdictReverted {
		t.Fatalf("want reverted, got %q (%s)", attempt.Verdict, attempt.Reason)
	}
	if !h.agent.LastDiagnosis.BenignWaitUsed {
		t.Fatal("the agent must be told its extension was already spent")
	}
}

// 8c. Fix attempts are bounded; the third failure reverts rather than looping.
func TestFixAttemptsAreBounded(t *testing.T) {
	h := newHarness()
	h.approver.interactive, h.approver.fixAnswer = true, true
	h.options.MaxFixAttempts = 1
	h.forge.add(sonarr(1209, "minor", "0.3.6", "0.4.0", "env renamed"), []string{"apps/sonarr.yaml"})
	h.forge.fileAt("base000", "apps/sonarr.yaml", []byte(helmRelease))
	h.forge.fileAt("merge1209", "apps/sonarr.yaml", []byte(helmRelease))
	h.agent.assessments = []domain.Assessment{safe("looks routine")}
	h.agent.diagnoses = []domain.Diagnosis{
		{Action: domain.DiagnoseFix, Cause: "rename env", Diff: fixDiff},
		{Action: domain.DiagnoseFix, Cause: "rename env again", Diff: fixDiff},
	}
	h.observer.outcomes = []cluster.Outcome{broke("sonarr"), broke("sonarr")}

	attempt := only(t, h.run(t))

	if attempt.Verdict != domain.VerdictReverted {
		t.Fatalf("want reverted once attempts are exhausted, got %+v", attempt)
	}
	if attempt.FixAttempts != 1 {
		t.Fatalf("want the configured single attempt, got %d", attempt.FixAttempts)
	}
}

// 9. A pull request already labelled reverted is never processed again.
func TestRevertedLabelSkipsThePullRequest(t *testing.T) {
	h := newHarness()
	request := sonarr(1209, "minor", "0.3.6", "0.4.0", "env renamed")
	request.Labels = []string{revertedLabel}
	h.forge.add(request, []string{"apps/sonarr.yaml"})

	attempt := only(t, h.run(t))

	if attempt.Decision != domain.DecideSkipReverted || attempt.Verdict != domain.VerdictSkipped {
		t.Fatalf("want a reverted skip, got %+v", attempt)
	}
	if h.agent.Assessed != 0 || len(h.forge.Merges) != 0 {
		t.Fatal("a reverted pull request must not be assessed or merged")
	}
}

// 10. Two pull requests for the same dependency: the older one is closed.
func TestSupersededPullRequestIsClosed(t *testing.T) {
	h := newHarness()
	h.forge.add(sonarr(1198, "patch", "4.0.14", "4.0.17", "older"), []string{"apps/sonarr.yaml"})
	h.forge.add(sonarr(1204, "patch", "4.0.14", "4.0.19", "newer"), []string{"apps/sonarr.yaml"})
	h.agent.assessments = []domain.Assessment{safe("patch release")}
	h.observer.outcomes = []cluster.Outcome{pass()}

	result := h.run(t)

	if len(result.Attempts) != 2 {
		t.Fatalf("want both pull requests accounted for, got %+v", result.Attempts)
	}
	if len(h.forge.Closed) != 1 || h.forge.Closed[0] != 1198 {
		t.Fatalf("want #1198 closed, got %v", h.forge.Closed)
	}
	if len(h.forge.Merges) != 1 || h.forge.Merges[0] != 1204 {
		t.Fatalf("want only #1204 merged, got %v", h.forge.Merges)
	}
}

// A pull request whose body declares several dependencies cannot be attributed
// to one changelog, so it goes to a human rather than being merged blind.
func TestMultiDependencyPullRequestNeedsApproval(t *testing.T) {
	h := newHarness()
	request := sonarr(1220, "patch", "4.0.14", "4.0.19", "")
	request.Body = renovateBody("a/one", "image", "patch", "1.0.0", "1.0.1", "") +
		"| [b/two](https://x) | image | patch | `2.0.0` -> `2.0.1` |\n"
	h.forge.add(request, []string{"apps/one.yaml"})

	attempt := only(t, h.run(t))

	if attempt.Verdict != domain.VerdictSkipped {
		t.Fatalf("want skipped, got %+v", attempt)
	}
	if !strings.Contains(attempt.Reason, "2 dependencies") {
		t.Fatalf("want the multi-dependency reason, got %q", attempt.Reason)
	}
}

// A merge that fails is recorded as an error and never watched or reverted.
func TestMergeFailureIsRecordedWithoutWatching(t *testing.T) {
	h := newHarness()
	h.forge.add(sonarr(1204, "patch", "4.0.14", "4.0.19", "Fixed a crash."), []string{"apps/sonarr.yaml"})
	h.forge.MergeErr = errMergeConflict
	h.agent.assessments = []domain.Assessment{safe("patch release")}

	attempt := only(t, h.run(t))

	if attempt.Verdict != domain.VerdictError {
		t.Fatalf("want an error verdict, got %+v", attempt)
	}
	if h.observer.Watches != 0 {
		t.Fatal("a failed merge must not be watched")
	}
	if len(h.forge.Commits) != 0 {
		t.Fatal("a failed merge must not be reverted")
	}
}

// A dry run decides everything and writes nothing.
func TestDryRunMakesNoExternalChange(t *testing.T) {
	h := newHarness()
	h.options.DryRun = true
	h.forge.add(sonarr(1204, "patch", "4.0.14", "4.0.19", "Fixed a crash."), []string{"apps/sonarr.yaml"})
	h.agent.assessments = []domain.Assessment{safe("patch release")}

	attempt := only(t, h.run(t))

	if attempt.Verdict != domain.VerdictWouldMerge || attempt.Decision != domain.DecideMerge {
		t.Fatalf("want a decided-but-unmerged attempt, got %+v", attempt)
	}
	if len(h.forge.Merges) != 0 || len(h.forge.Commits) != 0 || h.observer.Watches != 0 {
		t.Fatal("a dry run must not touch anything")
	}
}

// Every attempt reaches the history log, whatever the verdict.
func TestEveryAttemptIsRecorded(t *testing.T) {
	h := newHarness()
	h.forge.add(sonarr(1204, "patch", "4.0.14", "4.0.19", "Fixed a crash."), []string{"apps/sonarr.yaml"})
	h.forge.add(sonarr(1207, "major", "3.9.0", "4.0.0", "Breaking."), []string{"apps/other.yaml"})
	h.agent.assessments = []domain.Assessment{safe("patch release")}
	h.observer.outcomes = []cluster.Outcome{pass()}

	result := h.run(t)

	if len(h.recorder.Attempts) != len(result.Attempts) {
		t.Fatalf("want %d recorded attempts, got %d", len(result.Attempts), len(h.recorder.Attempts))
	}
	if len(h.recorder.Runs) != 1 || len(h.recorder.Finished) != 1 {
		t.Fatalf("want one started and one finished run, got %d/%d",
			len(h.recorder.Runs), len(h.recorder.Finished))
	}
}

func assertReverted(t *testing.T, h *harness, number int, wantCause string) {
	t.Helper()
	if len(h.forge.Labels[number]) != 1 || h.forge.Labels[number][0] != revertedLabel {
		t.Fatalf("want the reverted label on #%d, got %v", number, h.forge.Labels[number])
	}
	comments := h.forge.Comments[number]
	if len(comments) != 1 {
		t.Fatalf("want exactly one comment on #%d, got %d", number, len(comments))
	}
	if !strings.Contains(comments[0], wantCause) {
		t.Fatalf("comment does not carry the cause %q:\n%s", wantCause, comments[0])
	}
	if !strings.Contains(comments[0], revertedLabel) {
		t.Fatalf("comment does not say how to retry:\n%s", comments[0])
	}
}

// The agent must read the pull request's own head, not whatever was last
// checked out, or "does this affect my deployment" is answered against the
// wrong manifests.
func TestTheAgentReadsThePullRequestHead(t *testing.T) {
	h := newHarness()
	h.forge.add(sonarr(1204, "patch", "4.0.14", "4.0.19", "Fixed a crash."), []string{"apps/sonarr.yaml"})
	h.agent.assessments = []domain.Assessment{safe("patch release")}
	h.observer.outcomes = []cluster.Outcome{pass()}

	h.run(t)

	if len(h.workspace.Synced) != 1 || h.workspace.Synced[0] != 1204 {
		t.Fatalf("want the checkout synced to #1204, got %v", h.workspace.Synced)
	}
}

// A checkout that fails degrades the assessment rather than failing the run.
func TestCheckoutFailureStillReachesTheAgent(t *testing.T) {
	h := newHarness()
	h.workspace.Err = errCheckout
	h.forge.add(sonarr(1204, "patch", "4.0.14", "4.0.19", "Fixed a crash."), []string{"apps/sonarr.yaml"})
	h.agent.assessments = []domain.Assessment{needsApproval("cannot see the manifests")}

	attempt := only(t, h.run(t))

	if h.agent.Assessed != 1 {
		t.Fatal("the agent must still be consulted")
	}
	if attempt.Verdict != domain.VerdictSkipped {
		t.Fatalf("want skipped, got %+v", attempt)
	}
}

// A revert must undo only this pull request. Restoring the merge base would
// take the file back past every merge that landed in between.
func TestRevertRestoresTheBranchAsItWasBeforeThisMerge(t *testing.T) {
	h := newHarness()
	h.forge.add(sonarr(1209, "minor", "0.3.6", "0.4.0", "env renamed"), []string{"apps/sonarr.yaml"})
	// The pull request's own base is stale: another merge has since rewritten
	// the same file, and that content is what the branch head carries.
	h.forge.fileAt("base000", "apps/sonarr.yaml", []byte("stale: from-before-an-earlier-merge\n"))
	h.forge.fileAt("base000", "apps/other.yaml", []byte("unrelated\n"))
	h.forge.head = "afterA"
	h.forge.fileAt("afterA", "apps/sonarr.yaml", []byte(helmRelease))

	h.agent.assessments = []domain.Assessment{safe("looks routine")}
	h.agent.diagnoses = []domain.Diagnosis{{
		Action: domain.DiagnoseUnfixable,
		Cause:  "0.4.0 needs a migration",
	}}
	h.observer.outcomes = []cluster.Outcome{broke("sonarr")}

	attempt := only(t, h.run(t))

	if attempt.Verdict != domain.VerdictReverted {
		t.Fatalf("want reverted, got %+v", attempt)
	}
	restored := h.forge.Commits[0].Changes[0]
	if string(restored.Contents) != helmRelease {
		t.Fatalf("revert restored the wrong revision:\n%s", restored.Contents)
	}
}

// A rename has two ends. Restoring only the new path deletes it and leaves the
// original missing, so the file is simply gone.
func TestRevertRestoresBothEndsOfARename(t *testing.T) {
	h := newHarness()
	h.forge.add(sonarr(1209, "minor", "0.3.6", "0.4.0", "moved"), nil)
	h.forge.renamed(1209, "apps/old.yaml", "apps/new.yaml")
	h.forge.fileAt("base000", "apps/old.yaml", []byte(helmRelease))
	h.agent.assessments = []domain.Assessment{safe("looks routine")}
	h.agent.diagnoses = []domain.Diagnosis{{Action: domain.DiagnoseUnfixable, Cause: "broke"}}
	h.observer.outcomes = []cluster.Outcome{broke("sonarr")}

	h.run(t)

	changes := map[string]bool{}
	for _, change := range h.forge.Commits[0].Changes {
		changes[change.Path] = change.Delete
	}
	if deleted, present := changes["apps/new.yaml"]; !present || !deleted {
		t.Fatalf("want the renamed-to path deleted, got %+v", changes)
	}
	if deleted, present := changes["apps/old.yaml"]; !present || deleted {
		t.Fatalf("want the original path restored, got %+v", changes)
	}
}

// A revert that cannot land leaves the cluster broken. Continuing would judge
// every later pull request against a baseline that is no longer true.
func TestFailedRevertHaltsTheRun(t *testing.T) {
	h := newHarness()
	h.forge.add(sonarr(1209, "minor", "0.3.6", "0.4.0", "broke"), []string{"apps/sonarr.yaml"})
	h.forge.add(gatus(1211, "patch", "1.0.0", "1.0.1"), []string{"apps/other.yaml"})
	h.forge.CommitErr = errCommitRejected
	h.agent.assessments = []domain.Assessment{safe("looks routine"), safe("also routine")}
	h.agent.diagnoses = []domain.Diagnosis{{Action: domain.DiagnoseUnfixable, Cause: "broke"}}
	h.observer.outcomes = []cluster.Outcome{broke("sonarr")}

	result := h.run(t)

	if result.Halted == "" {
		t.Fatal("want the run halted after a failed revert")
	}
	if len(result.Attempts) != 1 {
		t.Fatalf("want the queue abandoned after the failure, got %+v", result.Attempts)
	}
	if len(h.forge.Merges) != 1 {
		t.Fatalf("want no further merge, got %v", h.forge.Merges)
	}
}

// A revert that lands but does not restore health is the same situation.
func TestRevertThatDoesNotRestoreHealthHaltsTheRun(t *testing.T) {
	h := newHarness()
	h.forge.add(sonarr(1209, "minor", "0.3.6", "0.4.0", "broke"), []string{"apps/sonarr.yaml"})
	h.forge.add(gatus(1211, "patch", "1.0.0", "1.0.1"), []string{"apps/other.yaml"})
	h.forge.fileAt("base000", "apps/sonarr.yaml", []byte(helmRelease))
	h.observer.RestoreErr = errClusterUnreachable
	h.agent.assessments = []domain.Assessment{safe("looks routine"), safe("also routine")}
	h.agent.diagnoses = []domain.Diagnosis{{Action: domain.DiagnoseUnfixable, Cause: "broke"}}
	h.observer.outcomes = []cluster.Outcome{broke("sonarr")}

	result := h.run(t)

	if result.Halted == "" {
		t.Fatal("want the run halted when the cluster did not recover")
	}
	if len(h.forge.Merges) != 1 {
		t.Fatalf("want no further merge, got %v", h.forge.Merges)
	}
}

// A merge whose reaction could not be observed leaves an unknown cluster; the
// next pull request must not be merged on top of it.
func TestUnobservableWatchHaltsTheRun(t *testing.T) {
	h := newHarness()
	h.forge.add(sonarr(1204, "patch", "4.0.14", "4.0.19", "fine"), []string{"apps/sonarr.yaml"})
	h.forge.add(gatus(1211, "patch", "1.0.0", "1.0.1"), []string{"apps/other.yaml"})
	h.agent.assessments = []domain.Assessment{safe("patch release"), safe("also fine")}
	failing := &failingObserver{err: errClusterUnreachable}
	h.observer = &failing.observer
	h.watchFails = failing

	result := h.run(t)

	if result.Halted == "" {
		t.Fatal("want the run halted when the merge could not be observed")
	}
	if len(h.forge.Merges) != 1 {
		t.Fatalf("want only the first merge, got %v", h.forge.Merges)
	}
}

// A clarification can lead to an approved manifest update, after which the
// refreshed pull request is reassessed before it is merged.
func TestClarificationCanApplyAProposalThenMerge(t *testing.T) {
	h := newHarness()
	h.approver.interactive, h.approver.fixAnswer = true, true
	h.approver.clarify = []string{"The old env key is still enabled."}
	request := sonarr(1209, "minor", "0.3.6", "0.4.0", "env renamed")
	request.HeadRef = "renovate/sonarr"
	request.HeadRepository = h.options.Repository.String()
	request.HeadSHA = "base000"
	h.forge.add(request, []string{"apps/sonarr.yaml"})
	h.forge.fileAt("base000", "apps/sonarr.yaml", []byte(helmRelease))
	h.agent.assessments = []domain.Assessment{
		{Verdict: domain.AssessmentClarify, Question: "Is values.env still enabled?"},
		{Verdict: domain.AssessmentNeedsApproval, Reason: "rename the removed setting", Diff: fixDiff},
		safe("the updated manifest matches the new version"),
	}
	h.observer.outcomes = []cluster.Outcome{pass()}

	attempt := only(t, h.run(t))

	if h.approver.ClarifyAsks != 1 || h.approver.LastQuestion != "Is values.env still enabled?" {
		t.Fatalf("clarification prompts: %d (%q)", h.approver.ClarifyAsks, h.approver.LastQuestion)
	}
	if h.approver.FixAsks != 1 || len(h.forge.Commits) != 1 {
		t.Fatalf("want one approved proposal commit, prompts %d commits %+v", h.approver.FixAsks, h.forge.Commits)
	}
	if h.agent.Assessed != 3 || len(h.agent.AssessmentRequests[1].Clarifications) != 1 {
		t.Fatalf("want clarification-fed reassessment, got %+v", h.agent.AssessmentRequests)
	}
	if got := h.agent.AssessmentRequests[1].Clarifications[0].Answer; got != "The old env key is still enabled." {
		t.Fatalf("clarification answer = %q", got)
	}
	if attempt.Verdict != domain.VerdictMerged || len(h.forge.Merges) != 1 {
		t.Fatalf("want proposed, refreshed, then merged; attempt %+v merges %v", attempt, h.forge.Merges)
	}
}

// An interactive discussion carries every complete assistant turn forward,
// streams each turn in order, and may continue past the old three-turn limit.
func TestStreamingDiscussionContinuesUntilSafe(t *testing.T) {
	h := newHarness()
	h.approver.interactive = true
	h.approver.clarify = []string{"one", "two", "three", "four"}
	h.forge.add(sonarr(1210, "minor", "0.3.6", "0.4.0", "needs context"), []string{"apps/sonarr.yaml"})

	turns := []string{
		"The release changes the first setting.",
		"The chart also changes the second setting.",
		"I need one more deployment detail.",
		"That resolves the remaining configuration question.",
		"The manifest is compatible with the new release.",
	}
	questions := []string{"Is first enabled?", "Is second enabled?", "What is the deployment mode?", "Is the fallback configured?"}
	for _, turn := range turns {
		h.agent.stream = append(h.agent.stream, []ai.StreamEvent{
			{Kind: ai.StreamTurnStart},
			{Kind: ai.StreamDelta, Text: turn[:len(turn)/2]},
			{Kind: ai.StreamDelta, Text: turn[len(turn)/2:]},
			{Kind: ai.StreamTurnEnd},
		})
	}
	for i, question := range questions {
		h.agent.assessments = append(h.agent.assessments, domain.Assessment{
			Verdict:  domain.AssessmentClarify,
			Message:  turns[i],
			Question: question,
		})
	}
	h.agent.assessments = append(h.agent.assessments, safe("all four answers establish compatibility"))
	h.observer.outcomes = []cluster.Outcome{pass()}

	attempt := only(t, h.run(t))

	if attempt.Verdict != domain.VerdictMerged || len(h.forge.Merges) != 1 {
		t.Fatalf("want merge after discussion, attempt %+v merges %v", attempt, h.forge.Merges)
	}
	if h.approver.ClarifyAsks != 4 || h.agent.Assessed != 5 {
		t.Fatalf("turns: clarification asks %d assessments %d", h.approver.ClarifyAsks, h.agent.Assessed)
	}
	for i := range questions {
		got := h.agent.AssessmentRequests[i+1].Clarifications
		if len(got) != i+1 || got[i].Assistant != turns[i] || got[i].Answer != h.approver.clarify[i] {
			t.Fatalf("request %d transcript = %#v", i+2, got)
		}
	}
	var want []ai.StreamEvent
	for _, stream := range h.agent.stream {
		want = append(want, stream...)
	}
	if len(h.approver.Streamed) != len(want) {
		t.Fatalf("stream event count = %d, want %d: %#v", len(h.approver.Streamed), len(want), h.approver.Streamed)
	}
	for i := range want {
		if h.approver.Streamed[i] != want[i] {
			t.Fatalf("stream event %d = %#v, want %#v", i, h.approver.Streamed[i], want[i])
		}
	}
}

// A plain-language "skip" remains part of the discussion. The model, rather
// than the terminal parser, decides that it defers this particular update.
func TestStreamingDiscussionCanConcludeWithModelDefer(t *testing.T) {
	h := newHarness()
	h.approver.interactive = true
	h.approver.clarify = []string{"skip"}
	request := sonarr(1212, "minor", "0.3.6", "0.4.0", "needs configuration context")
	h.forge.add(request, []string{"apps/sonarr.yaml"})
	h.agent.assessments = []domain.Assessment{
		{
			Verdict:  domain.AssessmentClarify,
			Message:  "This release may require a configuration change.",
			Question: "Should the current configuration be updated now?",
		},
		{Verdict: domain.AssessmentDefer, Reason: "the operator chose to defer this update"},
	}
	h.agent.stream = [][]ai.StreamEvent{{
		{Kind: ai.StreamTurnStart},
		{Kind: ai.StreamDelta, Text: "This release may require a configuration change."},
		{Kind: ai.StreamTurnEnd},
	}}

	attempt := only(t, h.run(t))

	if attempt.Decision != domain.DecideNeedsApproval || attempt.Verdict != domain.VerdictSkipped {
		t.Fatalf("want pending skipped attempt, got %+v", attempt)
	}
	if h.agent.Assessed != 2 || h.approver.ClarifyAsks != 1 {
		t.Fatalf("want two assessments and one prompt, got assessments %d prompts %d", h.agent.Assessed, h.approver.ClarifyAsks)
	}
	if h.approver.LastQuestion != "Should the current configuration be updated now?" {
		t.Fatalf("prompt = %q", h.approver.LastQuestion)
	}
	transcript := h.agent.AssessmentRequests[1].Clarifications
	if len(transcript) != 1 || transcript[0].Answer != "skip" {
		t.Fatalf("second assessment transcript = %#v", transcript)
	}
	if len(h.approver.Streamed) != 3 {
		t.Fatalf("want streamed question turn, got %#v", h.approver.Streamed)
	}
	if len(h.forge.Commits) != 0 || len(h.forge.Merges) != 0 || len(h.forge.Closed) != 0 || len(h.forge.Labels[request.Number]) != 0 || len(h.forge.Comments[request.Number]) != 0 {
		t.Fatalf("defer wrote commits=%+v merges=%v closed=%v labels=%v comments=%v", h.forge.Commits, h.forge.Merges, h.forge.Closed, h.forge.Labels[request.Number], h.forge.Comments[request.Number])
	}
}

// Without an interactive terminal, a clarification is held without prompting
// or modifying the pull request.
func TestClarificationWithoutTerminalDoesNotWrite(t *testing.T) {
	h := newHarness()
	h.forge.add(sonarr(1209, "minor", "0.3.6", "0.4.0", "env renamed"), []string{"apps/sonarr.yaml"})
	h.agent.assessments = []domain.Assessment{{
		Verdict:  domain.AssessmentClarify,
		Question: "Is values.env still enabled?",
	}}

	attempt := only(t, h.run(t))

	if attempt.Decision != domain.DecideNeedsApproval || attempt.Verdict != domain.VerdictSkipped {
		t.Fatalf("want held clarification, got %+v", attempt)
	}
	if h.approver.ClarifyAsks != 0 || len(h.forge.Commits) != 0 || len(h.forge.Merges) != 0 {
		t.Fatalf("non-interactive clarification wrote or prompted: asks %d commits %+v merges %v", h.approver.ClarifyAsks, h.forge.Commits, h.forge.Merges)
	}
}

// Two pull requests differing only by digest cannot be ordered, so neither may
// be closed as superseded.
func TestDigestOnlyPairIsNeverSuperseded(t *testing.T) {
	h := newHarness()
	first := sonarr(1301, "digest", "1.2.3@sha256:aaa", "1.2.3@sha256:bbb", "")
	second := sonarr(1302, "digest", "1.2.3@sha256:aaa", "1.2.3@sha256:ccc", "")
	h.forge.add(first, []string{"apps/a.yaml"})
	h.forge.add(second, []string{"apps/b.yaml"})
	h.agent.assessments = []domain.Assessment{safe("digest repin"), safe("digest repin")}
	h.observer.outcomes = []cluster.Outcome{pass(), pass()}

	result := h.run(t)

	if len(h.forge.Closed) != 0 {
		t.Fatalf("want nothing closed, got %v", h.forge.Closed)
	}
	for _, attempt := range result.Attempts {
		if attempt.Decision == domain.DecideSkipSuperseded {
			t.Fatalf("#%d was wrongly marked superseded", attempt.PullRequest)
		}
	}
}

// Closing a superseded pull request must not depend on the comment landing.
func TestSupersededPullRequestClosesEvenIfCommentingFails(t *testing.T) {
	h := newHarness()
	h.forge.add(sonarr(1198, "patch", "4.0.14", "4.0.17", "older"), []string{"apps/sonarr.yaml"})
	h.forge.add(sonarr(1204, "patch", "4.0.14", "4.0.19", "newer"), []string{"apps/sonarr.yaml"})
	h.forge.CommentErr = errCommentRejected
	h.agent.assessments = []domain.Assessment{safe("patch release")}
	h.observer.outcomes = []cluster.Outcome{pass()}

	h.run(t)

	if len(h.forge.Closed) != 1 || h.forge.Closed[0] != 1198 {
		t.Fatalf("want #1198 closed despite the comment failure, got %v", h.forge.Closed)
	}
}

// A changelog the agent found itself is recorded as such, not as none.
func TestChangelogFoundByTheAgentIsRecorded(t *testing.T) {
	h := newHarness()
	h.changelogs = notFound()
	h.forge.add(sonarr(1211, "patch", "4.0.14", "4.0.15", ""), []string{"apps/sonarr.yaml"})
	h.agent.assessments = []domain.Assessment{{
		Verdict:      domain.AssessmentSafe,
		Reason:       "found the release notes upstream; no configuration change",
		ChangelogURL: "https://github.com/Sonarr/Sonarr/releases/tag/v4.0.15",
	}}
	h.observer.outcomes = []cluster.Outcome{pass()}

	attempt := only(t, h.run(t))

	if attempt.ChangelogSource != domain.ChangelogFromSearch {
		t.Fatalf("want the agent's own search recorded, got %q", attempt.ChangelogSource)
	}
}

const declinedLabel = "ops-pilot/declined"

func TestDeclinedLabelSkipsThePullRequest(t *testing.T) {
	h := newHarness()
	request := sonarr(1207, "major", "3.9.0", "4.0.0", "Breaking.")
	request.Labels = []string{declinedLabel}
	h.forge.add(request, []string{"apps/sonarr.yaml"})

	attempt := only(t, h.run(t))

	if attempt.Decision != domain.DecideSkipDeclined {
		t.Fatalf("want a declined skip, got %+v", attempt)
	}
	if h.agent.Assessed != 0 {
		t.Fatal("a declined pull request must not be assessed again")
	}
}

// --all is how an operator reconsiders what an earlier run set aside.
func TestAllReconsidersLabelledPullRequests(t *testing.T) {
	h := newHarness()
	h.options.All = true
	declined := sonarr(1204, "patch", "4.0.14", "4.0.19", "Fixed a crash.")
	declined.Labels = []string{declinedLabel}
	reverted := gatus(1211, "patch", "1.0.0", "1.0.1")
	reverted.Labels = []string{revertedLabel}
	h.forge.add(declined, []string{"apps/sonarr.yaml"})
	h.forge.add(reverted, []string{"apps/other.yaml"})
	h.agent.assessments = []domain.Assessment{safe("patch release"), safe("patch release")}
	h.observer.outcomes = []cluster.Outcome{pass(), pass()}

	result := h.run(t)

	for _, attempt := range result.Attempts {
		if attempt.Verdict != domain.VerdictMerged {
			t.Fatalf("#%d was not reconsidered: %+v", attempt.PullRequest, attempt)
		}
	}
	if len(h.forge.Merges) != 2 {
		t.Fatalf("want both merged, got %v", h.forge.Merges)
	}
}

// A diff that does not apply has changed nothing, so there is nothing to undo.
// Discarding the merge over it threw away work the operator had just approved a
// fix for, because the model miscounted a hunk header.
func TestAFixThatDoesNotApplyIsRetriedRatherThanReverted(t *testing.T) {
	h := newHarness()
	h.approver.interactive, h.approver.fixAnswer = true, true
	h.forge.add(sonarr(1209, "minor", "0.3.6", "0.4.0", "env renamed"), []string{"apps/sonarr.yaml"})
	h.forge.fileAt("merge1209", "apps/sonarr.yaml", []byte(helmRelease))
	h.agent.assessments = []domain.Assessment{safe("looks routine")}
	h.agent.diagnoses = []domain.Diagnosis{
		// The first diff's context does not exist in the file.
		{Action: domain.DiagnoseFix, Cause: "rename env", Diff: `--- a/apps/sonarr.yaml
+++ b/apps/sonarr.yaml
@@ -1,1 +1,1 @@
-nothing like this is in the file
+replacement
`},
		{Action: domain.DiagnoseFix, Cause: "rename env properly", Diff: fixDiff},
	}
	h.observer.outcomes = []cluster.Outcome{broke("sonarr"), pass()}

	attempt := only(t, h.run(t))

	if attempt.Verdict != domain.VerdictFixed {
		t.Fatalf("want the corrected fix to land, got %q (%s)", attempt.Verdict, attempt.Reason)
	}
	if len(h.forge.Commits) != 1 {
		t.Fatalf("want only the applicable fix committed, got %d", len(h.forge.Commits))
	}
	// The agent must be told why, or it will repeat the same diff.
	if h.agent.LastDiagnosis.RejectedFix == "" {
		t.Fatal("the agent was not told its diff had been rejected")
	}
}

// Attempts stay bounded: a model that cannot produce an applicable diff must
// not loop forever against a broken cluster.
func TestUnapplicableFixesStillExhaustTheBudget(t *testing.T) {
	h := newHarness()
	h.approver.interactive, h.approver.fixAnswer = true, true
	h.options.MaxFixAttempts = 1
	h.forge.add(sonarr(1209, "minor", "0.3.6", "0.4.0", "env renamed"), []string{"apps/sonarr.yaml"})
	h.forge.fileAt("base000", "apps/sonarr.yaml", []byte(helmRelease))
	h.agent.assessments = []domain.Assessment{safe("looks routine")}
	h.agent.diagnoses = []domain.Diagnosis{{
		Action: domain.DiagnoseFix, Cause: "rename env", Diff: `--- a/apps/sonarr.yaml
+++ b/apps/sonarr.yaml
@@ -1,1 +1,1 @@
-absent
+replacement
`,
	}}
	h.observer.outcomes = []cluster.Outcome{broke("sonarr")}

	if attempt := only(t, h.run(t)); attempt.Verdict != domain.VerdictReverted {
		t.Fatalf("want a revert once the budget is spent, got %+v", attempt)
	}
}

func labelled(request domain.PullRequest, author string, labels ...string) domain.PullRequest {
	request.Author = author
	request.Labels = labels
	return request
}

func TestOnlyPullRequestsTheFilterAdmitsAreActedOn(t *testing.T) {
	h := newHarness()
	h.options.Filter = domain.PullRequestFilter{
		Authors: []string{"renovate[bot]"},
		Labels:  []string{"deps"},
	}
	h.forge.add(labelled(sonarr(1204, "patch", "4.0.14", "4.0.19", "Fixed a crash."),
		"renovate[bot]", "deps"), []string{"apps/sonarr.yaml"})
	h.forge.add(labelled(gatus(1205, "patch", "1.0.0", "1.0.1"),
		"dependabot[bot]", "deps"), []string{"apps/gatus.yaml"})
	h.forge.add(labelled(gatus(1206, "patch", "1.0.0", "1.0.1"),
		"renovate[bot]", "chore"), []string{"apps/other.yaml"})
	elsewhere := labelled(gatus(1207, "patch", "1.0.0", "1.0.1"), "renovate[bot]", "deps")
	elsewhere.BaseRef = "next"
	h.forge.add(elsewhere, []string{"apps/next.yaml"})
	h.agent.assessments = []domain.Assessment{safe("patch release, no configuration change")}
	h.observer.outcomes = []cluster.Outcome{pass()}

	attempt := only(t, h.run(t))

	if attempt.PullRequest != 1204 {
		t.Fatalf("want only #1204 attempted, got #%d", attempt.PullRequest)
	}
	if len(h.forge.Merges) != 1 || h.forge.Merges[0] != 1204 {
		t.Fatalf("want exactly one merge of #1204, got %v", h.forge.Merges)
	}
}

func TestAQuietRunCountsWhatEachNarrowingRemoved(t *testing.T) {
	h := newHarness()
	h.options.Filter = domain.PullRequestFilter{Authors: []string{"renovate[bot]"}}
	h.forge.add(labelled(sonarr(1205, "patch", "4.0.14", "4.0.19", "Fixed a crash."),
		"dependabot[bot]"), []string{"apps/sonarr.yaml"})
	elsewhere := labelled(gatus(1207, "patch", "1.0.0", "1.0.1"), "renovate[bot]")
	elsewhere.BaseRef = "next"
	h.forge.add(elsewhere, []string{"apps/next.yaml"})

	result := h.run(t)

	if len(result.Attempts) != 0 {
		t.Fatalf("want nothing attempted, got %+v", result.Attempts)
	}
	if result.Discovered == nil {
		t.Fatal("want the unfiltered listing counted, got no count")
	}
	if *result.Discovered != 1 {
		t.Fatalf("want one pull request on the base branch counted, got %d", *result.Discovered)
	}
	if result.OtherBranches == nil {
		t.Fatal("want the other-branch listing counted, got no count")
	}
	if *result.OtherBranches != 1 {
		t.Fatalf("want one pull request aimed elsewhere counted, got %d", *result.OtherBranches)
	}
	if len(h.forge.Merges) != 0 {
		t.Fatalf("a filtered-out pull request was merged: %v", h.forge.Merges)
	}
}
