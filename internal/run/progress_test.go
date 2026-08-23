package run

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/lkshrk/ops-pilot/internal/cluster"
	"github.com/lkshrk/ops-pilot/internal/diagnostics"
	"github.com/lkshrk/ops-pilot/internal/display"
	"github.com/lkshrk/ops-pilot/internal/domain"
)

// Folded first, the outer key claims the inner label as its value and the secret behind it stands.
func TestTheNarrativeScrubsBeforeItFoldsTheNewlinesTheKeyRulesRead(t *testing.T) {
	tests := []struct {
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

	runner := New(Dependencies{Redactor: diagnostics.NewRedactor([]string{"unrelated"})}, Options{})
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := runner.safe(test.text)
			if strings.Contains(got, test.secret) {
				t.Fatalf("the nested secret survived: %q", got)
			}
			if !strings.Contains(got, "[REDACTED]") {
				t.Fatalf("nothing was redacted at all: %q", got)
			}
		})
	}
}

// The guard is now the only thing separating the two bullet writers. Evidence
// for a verdict a human has to judge is shown whatever the verbosity, because it
// is the justification for an autonomous production change; evidence for the
// routine ones would bury that under every merge the run waves through.
func TestRoutineEvidenceIsWithheldUntilTheRunIsAskedToExplain(t *testing.T) {
	const point = "the release notes describe a dependency bump and no API change"
	tests := []struct {
		name      string
		verbosity Verbosity
		needed    bool
		printed   bool
	}{
		{name: "a routine verdict at normal verbosity", verbosity: VerbosityNormal, needed: false},
		{name: "a verdict a human must judge, at normal verbosity", verbosity: VerbosityNormal, needed: true, printed: true},
		{name: "a routine verdict when explaining", verbosity: VerbosityVerbose, needed: false, printed: true},
		{name: "a verdict a human must judge, when explaining", verbosity: VerbosityVerbose, needed: true, printed: true},
		{name: "a verdict a human must judge, in a quiet run", verbosity: VerbosityQuiet, needed: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			out := &bytes.Buffer{}
			runner := New(Dependencies{Out: out}, Options{Verbosity: test.verbosity})

			runner.evidence(test.needed, []string{point})

			if printed := strings.Contains(out.String(), point); printed != test.printed {
				t.Fatalf("evidence printed=%v, want %v:\n%s", printed, test.printed, out)
			}
		})
	}
}

// The cluster's own reasons and the agent's supporting points are printed one
// after the other under a single failure, so a bullet that indents, wraps or
// marks differently in one list than the other reads as two grades of fact.
func TestABrokenObjectAndAnEvidencePointRenderTheSameBullet(t *testing.T) {
	object := domain.ObjectHealth{
		Ref:    domain.ObjectRef{Namespace: "flux-system", Kind: "HelmRelease", Name: "podinfo"},
		Reason: strings.TrimSpace(strings.Repeat("the chart rejected the value it was given ", 4)),
	}
	point := object.Ref.String() + " — " + object.Reason

	for _, width := range []int{20, 37, 60, 100} {
		t.Run(fmt.Sprintf("a terminal %d columns wide", width), func(t *testing.T) {
			fromObject, fromPoint := &bytes.Buffer{}, &bytes.Buffer{}
			narrator := New(Dependencies{Out: fromObject}, Options{Verbosity: VerbosityNormal})
			narrator.style.Width = width
			narrator.broken([]domain.ObjectHealth{object})

			narrator = New(Dependencies{Out: fromPoint}, Options{Verbosity: VerbosityNormal})
			narrator.style.Width = width
			narrator.evidence(true, []string{point})

			if fromObject.Len() == 0 {
				t.Fatal("the broken object rendered nothing at all")
			}
			if fromObject.String() != fromPoint.String() {
				t.Fatalf("the two bullet lists disagree:\nbroken:\n%s\nevidence:\n%s",
					fromObject, fromPoint)
			}
		})
	}
}

// A held or failed pull request is one an operator has to act on, so its reason
// is wrapped whole; a routine success is scanned, not read, and stays on the one
// line the width allows.
func TestOnlyARoutineConclusionIsAbbreviatedToOneLine(t *testing.T) {
	const reason = "#1204 could not be checked out, so any manifest the agent read may be from another " +
		"commit: a changelog override is configured for this dependency but resolved no releases: " +
		"major version bump: the agent's evidence quoted a forged data fence"
	tests := []struct {
		name     string
		kind     outcomeKind
		label    string
		abbrevia bool
	}{
		{name: "a pull request held for approval", kind: outcomeAsk, label: "Needs your approval"},
		{name: "a failure", kind: outcomeBad, label: "Failed"},
		{name: "a routine success", kind: outcomeGood, label: "Would merge", abbrevia: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			out := &bytes.Buffer{}
			runner := New(Dependencies{Out: out}, Options{Verbosity: VerbosityNormal})

			runner.outcome(test.kind, test.label, reason)

			whole := strings.Contains(display.Collapse(out.String()), "forged data fence")
			if whole == test.abbrevia {
				t.Fatalf("reason kept whole=%v, want %v:\n%s", whole, !test.abbrevia, out)
			}
			if lines := strings.Count(strings.TrimSpace(out.String()), "\n") > 0; lines == test.abbrevia {
				t.Fatalf("wrapped=%v, want %v:\n%s", lines, !test.abbrevia, out)
			}
		})
	}
}

// Continuation lines are indented by four columns, not by the label, so charging
// the label's width to them costs nothing where the label fits and everything
// where it does not: the widest label in the tree on a narrow terminal drove the
// wrap column into Wrap's clamp and shredded a held reason into fifty lines of
// eight columns. The first line is already past the width in that case, so there
// is nothing left for the narrow column to protect.
func TestALabelWiderThanTheTerminalDoesNotShredTheReasonIntoAClampedColumn(t *testing.T) {
	const label = "Kept on your instruction; the cluster is still unhealthy"
	reason := strings.TrimSpace(strings.Repeat(
		"the helmrelease upgrade failed because the chart rejected the value ", 5))
	out := &bytes.Buffer{}
	runner := New(Dependencies{Out: out}, Options{Verbosity: VerbosityNormal})
	runner.style.Width = 37

	runner.outcome(outcomeAsk, label, reason)

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	widest := 0
	for _, line := range lines[1:] {
		if width := display.Width(strings.TrimSpace(line)); width > widest {
			widest = width
		}
	}
	if widest <= runner.style.Width/2 {
		t.Errorf("the reason was wrapped into %d columns of %d, over %d lines:\n%s",
			widest, runner.style.Width, len(lines), out)
	}
	for _, word := range strings.Fields(reason) {
		if !strings.Contains(display.Collapse(out.String()), word) {
			t.Fatalf("wrapping dropped %q:\n%s", word, out)
		}
	}
}

// The narrow case may not be bought by widening the ordinary one past the width
// it was sized for.
func TestALabelThatFitsStillSizesTheReasonToTheRemainingWidth(t *testing.T) {
	const reason = "the helmrelease upgrade failed because the chart rejected the value it was given"
	for _, label := range []string{"Failed", "Needs your approval"} {
		t.Run(label, func(t *testing.T) {
			out := &bytes.Buffer{}
			runner := New(Dependencies{Out: out}, Options{Verbosity: VerbosityNormal})
			runner.style.Width = 60

			runner.outcome(outcomeBad, label, reason)

			for _, line := range strings.Split(strings.TrimRight(out.String(), "\n"), "\n") {
				if width := display.Width(line); width > runner.style.Width {
					t.Errorf("a line ran %d columns past the width of %d:\n%s",
						width-runner.style.Width, runner.style.Width, out)
				}
			}
		})
	}
}

// A reason that fits stays on its own line whatever the outcome, so widening the
// held case costs nothing where there was nothing to drop.
func TestAReasonThatFitsIsStillRenderedOnOneLine(t *testing.T) {
	out := &bytes.Buffer{}
	runner := New(Dependencies{Out: out}, Options{Verbosity: VerbosityNormal})

	runner.outcome(outcomeAsk, "Needs your approval", "major version bump")

	if got := strings.TrimSpace(out.String()); strings.Contains(got, "\n") {
		t.Fatalf("a short reason was broken across lines:\n%s", got)
	}
}

// The verdict is the part the operator cannot lose. A reason carrying no words
// wraps to no lines at all, and printing the wrapped lines was the only thing
// that printed the marker and the label, so the pull request left no trace in
// the narrative it was held or failed in.
func TestAnOutcomeWithNothingToSayStillReportsItsVerdict(t *testing.T) {
	tests := []struct {
		name   string
		kind   outcomeKind
		label  string
		reason string
		want   string
	}{
		{
			name:   "a hold whose reason is only whitespace",
			kind:   outcomeAsk,
			label:  "Needs your approval",
			reason: "   ",
			want:   "? Needs your approval:",
		},
		{
			name:   "a failure whose reason is only whitespace",
			kind:   outcomeBad,
			label:  "Failed",
			reason: "\t\n ",
			want:   "! Failed:",
		},
		{
			name:   "a reason that is nothing but the label",
			kind:   outcomeAsk,
			label:  "Needs your approval",
			reason: "Needs your approval: ",
			want:   "? Needs your approval:",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			out := &bytes.Buffer{}
			runner := New(Dependencies{Out: out}, Options{Verbosity: VerbosityNormal})

			runner.outcome(test.kind, test.label, test.reason)

			if !strings.Contains(out.String(), test.want) {
				t.Fatalf("the verdict was not reported at all, got %q", out.String())
			}
		})
	}
}

// The label already states the verdict, so a model repeating it wastes the width
// the actual reason needs.
func TestTrimRestatementDropsAnExactRepeat(t *testing.T) {
	got := trimRestatement("Would merge", "Would merge: the chart only adds an unused optional setting.")
	if got != "the chart only adds an unused optional setting." {
		t.Fatalf("got %q", got)
	}
}

// A colon inside a sentence is not a restatement. Cutting there truncated the
// operator's reason mid-thought.
func TestTrimRestatementLeavesProseAlone(t *testing.T) {
	const reason = "Needs human review because no changelog for the alpine/kubectl 1.36.3 image could be located."
	if got := trimRestatement("Needs your approval", reason); got != reason {
		t.Fatalf("prose was mangled:\n got %q\nwant %q", got, reason)
	}
}

func TestTrimRestatementKeepsAShortReasonWhole(t *testing.T) {
	const reason = "Would merge: safe."
	if got := trimRestatement("Would merge", reason); got != reason {
		t.Fatalf("got %q", got)
	}
}

func TestTheStartupBannerNamesEveryPullRequestItCouldNotRead(t *testing.T) {
	out := &bytes.Buffer{}
	runner := New(Dependencies{Out: out}, Options{Verbosity: VerbosityNormal})

	runner.announce([]Candidate{
		{PullRequest: domain.PullRequest{Number: 822}},
		{
			PullRequest: domain.PullRequest{Number: 738},
			Skip:        domain.DecideNeedsApproval,
			Reason:      "pull request updates 5 dependencies at once",
		},
		{
			PullRequest: domain.PullRequest{Number: 840},
			Skip:        domain.DecideNeedsApproval,
			Reason:      "pull request body declares no dependency updates",
		},
		{
			PullRequest: domain.PullRequest{Number: 999},
			Skip:        domain.DecideSkipDeclined,
			Reason:      "declined by an operator in an earlier run",
		},
	})

	got := out.String()
	for _, want := range []string{
		"Processing 1 pull request of 4.",
		"1 set aside earlier, 2 could not be read.",
		"- #738 pull request updates 5 dependencies at once",
		"- #840 pull request body declares no dependency updates",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("the banner does not say %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "#999") {
		t.Fatalf("a pull request an earlier run set aside was named as unreadable:\n%s", got)
	}
	if lines := strings.Count(got, "\n"); lines != 3 {
		t.Fatalf("the banner is %d lines, want 3:\n%s", lines, got)
	}
}

func TestTheStartupBannerStaysOneLineWhenEveryPullRequestWasRead(t *testing.T) {
	out := &bytes.Buffer{}
	runner := New(Dependencies{Out: out}, Options{Verbosity: VerbosityNormal})

	runner.announce([]Candidate{
		{PullRequest: domain.PullRequest{Number: 822}},
		{PullRequest: domain.PullRequest{Number: 823}},
	})

	if got := out.String(); got != "Processing 2 pull requests.\n" {
		t.Fatalf("banner = %q", got)
	}
}

func TestTheVerboseToolTraceKeepsItsIndent(t *testing.T) {
	out := &bytes.Buffer{}
	runner := New(Dependencies{Out: out}, Options{Verbosity: VerbosityVerbose})

	runner.Activity("read_repo_file", "clusters/prod/app.yaml")

	line := strings.TrimRight(out.String(), "\n")
	if !strings.HasPrefix(line, toolCallIndent) {
		t.Fatalf("tool trace printed %q, want it indented by %d spaces", line, len(toolCallIndent))
	}
	if !strings.Contains(line, "reading clusters/prod/app.yaml") {
		t.Fatalf("tool trace printed %q, want it to describe the tool call", line)
	}
}

func TestALongToolTraceIsIndentedAndStillFitsTheTerminal(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	out := &bytes.Buffer{}
	runner := New(Dependencies{Out: out}, Options{Verbosity: VerbosityVerbose})
	runner.style.Width = 40

	runner.Activity("read_repo_file", strings.Repeat("very-long-path/", 12)+"app.yaml")

	line := strings.TrimRight(out.String(), "\n")
	if !strings.HasPrefix(line, toolCallIndent) {
		t.Fatalf("tool trace printed %q, want it indented", line)
	}
	if got := display.Width(line); got > 40 {
		t.Fatalf("tool trace is %d columns wide, want at most 40: %q", got, line)
	}
}

func TestTheClusterWaitStatusKeepsItsIndentAndTheTwoSpaceGap(t *testing.T) {
	out := &bytes.Buffer{}
	runner := New(Dependencies{Out: out}, Options{Verbosity: VerbosityNormal})
	watch := &waiting{runner: runner}

	watch.observe(cluster.Status{
		Fetched: true,
		Reconciling: []domain.ObjectHealth{
			{Ref: domain.ObjectRef{Kind: "Kustomization", Namespace: "flux-system", Name: "app"}},
		},
	})

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	line := lines[len(lines)-1]
	if !strings.HasPrefix(line, "      ") {
		t.Fatalf("status line printed %q, want it indented by 6 spaces", line)
	}
	if !strings.Contains(line, "0:00  1 object reconciling") {
		t.Fatalf("status line printed %q, want a two-space gap before the state", line)
	}
}

func TestALongClusterWaitStatusIsIndentedAndStillFitsTheTerminal(t *testing.T) {
	t.Setenv("NO_COLOR", "1")
	out := &bytes.Buffer{}
	runner := New(Dependencies{Out: out}, Options{Verbosity: VerbosityNormal})
	runner.style.Width = 40
	watch := &waiting{runner: runner}

	reconciling := make([]domain.ObjectHealth, 0, 8)
	for i := 0; i < 8; i++ {
		reconciling = append(reconciling, domain.ObjectHealth{
			Ref: domain.ObjectRef{Kind: "Kustomization", Namespace: "flux-system", Name: fmt.Sprintf("very-long-app-name-%d", i)},
		})
	}
	watch.observe(cluster.Status{Fetched: true, Reconciling: reconciling, Elapsed: 90 * time.Second})

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	line := lines[len(lines)-1]
	if !strings.HasPrefix(line, "      ") {
		t.Fatalf("status line printed %q, want it indented", line)
	}
	if got := display.Width(line); got > 40 {
		t.Fatalf("status line is %d columns wide, want at most 40: %q", got, line)
	}
}
