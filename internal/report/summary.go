package report

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/lkshrk/ops-pilot/internal/diagnostics"
	"github.com/lkshrk/ops-pilot/internal/display"

	"github.com/lkshrk/ops-pilot/internal/domain"
)

// Label is exported so `ops-pilot history` and the summary cannot drift apart.
func Label(verdict domain.Verdict) string {
	switch verdict {
	case domain.VerdictMerged:
		return "MERGED"
	case domain.VerdictFixed:
		return "FIXED"
	case domain.VerdictReverted:
		return "REVERTED"
	case domain.VerdictKept:
		return "KEPT"
	case domain.VerdictWouldMerge:
		return "WOULD MERGE"
	case domain.VerdictSkipped:
		return "SKIPPED"
	case domain.VerdictError:
		return "ERROR"
	default:
		return strings.ToUpper(string(verdict))
	}
}

// quiet explains a run that attempted nothing. An empty queue and a queue the
// configured filters emptied are the same picture to an operator, and calling
// both of them idle is the one wording that hides a repository full of waiting
// pull requests, so the count carries the line - and a count that could not be
// read may not borrow the wording of a count that came back zero.
func quiet(run domain.Run) string {
	return onThisBranch(run) + elsewhere(run)
}

func onThisBranch(run domain.Run) string {
	switch {
	case run.Discovered == nil:
		return "Nothing to do: no pull request matched, and the number open could not be read."
	case *run.Discovered > 0:
		return "Nothing to do: " + openPullRequests(*run.Discovered) +
			", none matched the configured pullRequests.authors or .labels."
	case run.Repository.Branch != "":
		return "Nothing to do: no open pull requests on " + run.Repository.Branch + "."
	}
	return "Nothing to do: no open pull requests."
}

// The base branch narrowing empties a queue as silently as a mistyped author
// does, and no count on this branch can see what it removed. Silence is how a
// measured zero reads, so a count that could not be read may not borrow it.
func elsewhere(run domain.Run) string {
	switch {
	case run.OtherBranches == nil:
		return " Whether open pull requests are waiting on a different branch could not be read."
	case *run.OtherBranches == 0:
		return ""
	case *run.OtherBranches == 1:
		return " 1 open pull request targets another branch."
	}
	return fmt.Sprintf(" %d open pull requests target other branches.", *run.OtherBranches)
}

func openPullRequests(n int) string {
	if n == 1 {
		return "1 open pull request"
	}
	return fmt.Sprintf("%d open pull requests", n)
}

// paint colours a verdict so a failure is visible in a table of forty rows.
func paint(style display.Style, verdict domain.Verdict, value string) string {
	switch verdict {
	case domain.VerdictMerged, domain.VerdictFixed, domain.VerdictWouldMerge:
		return style.Good(value)
	case domain.VerdictReverted, domain.VerdictError:
		return style.Bad(value)
	default:
		return style.Warn(value)
	}
}

// Summary writes the end-of-run table and its tally. narrated says whether
// per-pull-request output preceded it, so a quiet run does not open with a
// stray blank line.
func Summary(out io.Writer, run domain.Run, narrated bool) error {
	if narrated {
		if _, err := fmt.Fprintln(out); err != nil {
			return err
		}
	}
	style := display.NewStyle(out)
	if narrated {
		if _, err := fmt.Fprintln(out, style.Title("Run summary")); err != nil {
			return err
		}
	}
	if len(run.Attempts) == 0 {
		if run.Halted != "" {
			return halted(out, style, "before any pull request was attempted", run.Halted)
		}
		_, err := fmt.Fprintln(out, quiet(run))
		return err
	}
	table := NewTable("PR", "DEP", "BUMP", "RESULT", "TOOK")
	for _, attempt := range run.Attempts {
		table.AddStyled(
			[]int{3},
			func(value string) string { return paint(style, attempt.Verdict, value) },
			"#"+strconv.Itoa(attempt.PullRequest),
			attempt.Dependency.Name,
			bump(attempt.Dependency),
			result(attempt),
			took(attempt.Duration),
		)
	}
	if err := table.Render(out); err != nil {
		return err
	}
	line := tally(run)
	if !run.StartedAt.IsZero() && !run.FinishedAt.IsZero() {
		line += style.Detail(fmt.Sprintf("  in %s", took(run.FinishedAt.Sub(run.StartedAt))))
	}
	if _, err := fmt.Fprintln(out, "\n"+line); err != nil {
		return err
	}
	// A halt means the cluster is in a state later attempts cannot be judged
	// against, and that the queue was abandoned. It is the most important thing
	// the run can say, so it goes last and it is unmissable.
	if run.Halted != "" {
		if _, err := fmt.Fprintln(out); err != nil {
			return err
		}
		return halted(out, style, "", run.Halted)
	}
	return nil
}

// Interrupted replaces the summary when the operator stopped the run. narrated
// carries the same meaning as it does for Summary.
func Interrupted(out io.Writer, run domain.Run, narrated bool) error {
	if narrated {
		if _, err := fmt.Fprintln(out); err != nil {
			return err
		}
	}
	style := display.NewStyle(out)
	line := "Interrupted. Nothing further was attempted"
	if run.ID != "" {
		line += "; ops-pilot history --run " + run.ID + " shows what was decided first"
	}
	for i, wrapped := range display.Wrap("! "+line+".", style.Width-2) {
		if i > 0 {
			wrapped = "  " + wrapped
		}
		if _, err := fmt.Fprintln(out, style.Warn(wrapped)); err != nil {
			return err
		}
	}
	return nil
}

// The reason is controller-authored as often as not, so it is wrapped rather
// than clipped: the sentence naming the state nobody is watching is the one an
// operator acts on, and an abbreviated one hides the cause.
func halted(out io.Writer, style display.Style, when, reason string) error {
	lead := "! Run halted"
	if when != "" {
		lead += " " + when
	}
	text := display.Collapse(display.Safe(diagnostics.ScrubSecrets(reason)))
	if text == "" {
		text = "no reason was recorded"
	}
	lines := display.Wrap(lead+": "+breakImplausibleTokens(text, style.Width-2), style.Width-2)
	for i, line := range lines {
		if i > 0 {
			line = "  " + line
		}
		if _, err := fmt.Fprintln(out, style.Bad(line)); err != nil {
			return err
		}
	}
	return nil
}

// identifierCeiling is the length above which an unbroken run stops being a
// digest or a URL somebody will paste somewhere and starts being a blob. A
// sha512 digest is 135 columns and a deeply nested source URL fits inside 300.
const identifierCeiling = 512

// breakImplausibleTokens withdraws the copy-paste exemption above the ceiling.
// It splits rather than clips because the halt reason may not be abbreviated,
// and because a viewer that hard-truncates loses a long tail without saying so.
func breakImplausibleTokens(text string, width int) string {
	if width < 1 {
		width = 1
	}
	fields := strings.Fields(text)
	for i, field := range fields {
		if display.Width(field) <= identifierCeiling {
			continue
		}
		var chunks []string
		var chunk strings.Builder
		columns := 0
		for _, r := range field {
			if size := display.Width(string(r)); columns+size > width {
				chunks = append(chunks, chunk.String())
				chunk.Reset()
				columns = 0
			}
			chunk.WriteRune(r)
			columns += display.Width(string(r))
		}
		fields[i] = strings.Join(append(chunks, chunk.String()), " ")
	}
	return strings.Join(fields, " ")
}

func bump(dependency domain.Dependency) string {
	if dependency.FromVersion == "" && dependency.ToVersion == "" {
		return "-"
	}
	return display.ShortVersion(dependency.FromVersion) + " -> " +
		display.ShortVersion(dependency.ToVersion)
}

func result(attempt domain.Attempt) string {
	label := Label(attempt.Verdict)
	detail := attempt.Reason
	if attempt.Verdict == domain.VerdictError && attempt.Error != "" {
		detail = attempt.Error
	}
	if detail == "" {
		return label
	}
	return label + " " + display.Clip(diagnostics.ScrubSecrets(detail), 60)
}

func took(duration time.Duration) string {
	if duration <= 0 {
		return "-"
	}
	return duration.Round(time.Second).String()
}

// tally counts the run by verdict, naming only what actually happened.
func tally(run domain.Run) string {
	counts := map[domain.Verdict]int{}
	for _, attempt := range run.Attempts {
		counts[attempt.Verdict]++
	}
	var parts []string
	for _, entry := range []struct {
		verdict      domain.Verdict
		one, several string
	}{
		{domain.VerdictMerged, "merged", "merged"},
		{domain.VerdictWouldMerge, "would merge", "would merge"},
		{domain.VerdictFixed, "fixed", "fixed"},
		{domain.VerdictReverted, "reverted", "reverted"},
		{domain.VerdictKept, "kept unhealthy", "kept unhealthy"},
		{domain.VerdictSkipped, "needs you", "need you"},
		{domain.VerdictError, "errored", "errored"},
	} {
		total := counts[entry.verdict]
		if total == 0 {
			continue
		}
		label := entry.several
		if total == 1 {
			label = entry.one
		}
		part := fmt.Sprintf("%d %s", total, label)
		if named := numbers(run, entry.verdict); named != "" && entry.verdict != domain.VerdictMerged {
			part += " (" + named + ")"
		}
		parts = append(parts, part)
	}
	if len(parts) == 0 {
		return "nothing to report"
	}
	result := parts[0]
	for _, part := range parts[1:] {
		result += "  " + part
	}
	return result
}

// numbers lists the pull requests behind a count, so an operator does not have
// to rescan the table to find what still needs them.
func numbers(run domain.Run, verdict domain.Verdict) string {
	var listed []string
	for _, attempt := range run.Attempts {
		if attempt.Verdict != verdict {
			continue
		}
		if len(listed) == 6 {
			return strings.Join(listed, " ") + " …"
		}
		listed = append(listed, "#"+strconv.Itoa(attempt.PullRequest))
	}
	return strings.Join(listed, " ")
}
