package run

import (
	"fmt"
	"strings"
	"time"

	"github.com/lkshrk/ops-pilot/internal/cluster"
	"github.com/lkshrk/ops-pilot/internal/diagnostics"
	"github.com/lkshrk/ops-pilot/internal/display"
	"github.com/lkshrk/ops-pilot/internal/domain"
)

// The narrative is the product and goes to stdout unadorned; warnings and
// errors go to the log on stderr. Three levels: --quiet leaves only the
// summary, the default reports each step as it happens, and --verbose adds the
// agent's reasoning, its evidence, and the tools it reached for.
const (
	statusCadence  = 30 * time.Second
	namedObjects   = 3
	toolCallIndent = "        "
	waitIndent     = "      "
)

// safe prepares untrusted text for the terminal. Almost everything printed
// originates outside ops-pilot - pull request titles, model prose, registry
// annotations, Kubernetes status - so it is sanitised and redacted here rather
// than trusted at each call site. The configured values ops-pilot holds are only
// half of it: a credential belonging to the workload has nothing but its shape.
func (r *Runner) safe(value string) string {
	// Scrubbed before display.Safe, which folds the newlines the key-name rules
	// use to tell a nested key from the value beside it.
	return r.redactor.Redact(display.Safe(diagnostics.ScrubSecrets(value)))
}

func (r *Runner) narrating() bool { return r.verbosity >= VerbosityNormal }

func (r *Runner) explaining() bool { return r.verbosity >= VerbosityVerbose }

// header makes a log file self-describing: which run, which repository, and
// under what settings. Without it a piped run cannot be tied to its own history
// row, and there is no way to tell that --all re-armed set-aside pull requests.
func (r *Runner) header(run domain.Run) {
	if !r.narrating() {
		return
	}
	settings := []string{run.Mode}
	if r.options.All {
		settings = append(settings, "reconsidering set-aside")
	}
	if r.options.OnlyPullRequest > 0 {
		settings = append(settings, fmt.Sprintf("#%d only", r.options.OnlyPullRequest))
	}
	fmt.Fprintf(r.out, "%s  %s\n  %s\n",
		r.style.Title("ops-pilot"),
		r.style.Heading(run.Repository.String()),
		r.style.Detail(fmt.Sprintf("run %s  %s  %s",
			run.ID, strings.Join(settings, ", "), run.StartedAt.UTC().Format("2006-01-02 15:04:05Z"))))
}

// announce reports the shape of the queue. It distinguishes pull requests a
// previous decision set aside from ones that could not be read at all, because
// calling the second kind "resolved" claims they were handled when they need a
// human.
func (r *Runner) announce(candidates []Candidate) {
	if !r.narrating() || len(candidates) == 0 {
		return
	}
	var settled, unreadable int
	var bullets []string
	for _, candidate := range candidates {
		switch candidate.Skip {
		case "":
		case domain.DecideNeedsApproval:
			unreadable++
			reason := candidate.Reason
			if reason == "" {
				reason = "no reason was recorded"
			}
			bullets = append(bullets, fmt.Sprintf("#%d %s", candidate.PullRequest.Number, reason))
		default:
			settled++
		}
	}
	parts := []string{fmt.Sprintf("Processing %s", count(len(candidates)-settled-unreadable, "pull request"))}
	if settled+unreadable > 0 {
		parts[0] += fmt.Sprintf(" of %d", len(candidates))
	}
	var notes []string
	if settled > 0 {
		notes = append(notes, fmt.Sprintf("%d set aside earlier", settled))
	}
	if unreadable > 0 {
		notes = append(notes, fmt.Sprintf("%d could not be read", unreadable))
	}
	line := r.style.Heading(parts[0] + ".")
	if len(notes) > 0 {
		line += " " + r.style.Detail(strings.Join(notes, ", ")+".")
	}
	fmt.Fprintln(r.out, line)
	r.bullets(bullets)
}

// headline opens one pull request's section. The position counts only pull
// requests actually being processed, so the numbers run consecutively instead of
// skipping over the ones a label set aside.
func (r *Runner) headline(position, total int, request domain.PullRequest, dependency domain.Dependency) {
	if !r.narrating() {
		return
	}
	name := dependency.Name
	if name == "" {
		name = request.Title
	}
	change := display.ShortVersion(dependency.FromVersion) + " to " + display.ShortVersion(dependency.ToVersion)
	if dependency.FromVersion == "" && dependency.ToVersion == "" {
		change = "no parsed version change"
	}
	bump := ""
	if dependency.Bump != "" && dependency.Bump != domain.BumpUnknown {
		bump = fmt.Sprintf(" (%s)", dependency.Bump)
	}
	fmt.Fprintf(r.out, "\n%s %s  %s%s\n",
		r.style.Accent(fmt.Sprintf("[%d/%d]", position, total)),
		r.style.Heading(fmt.Sprintf("#%d %s", request.Number, r.safe(name))),
		r.safe(change), r.style.Detail(bump))
}

// step reports what is happening now, so a minute-long assessment does not look
// like a hang.
func (r *Runner) step(format string, values ...any) {
	if !r.narrating() {
		return
	}
	fmt.Fprintf(r.out, "  %s\n", r.style.Detail(r.safe(fmt.Sprintf(format, values...))))
}

// outcome reports a conclusion and its reason, trimmed to one scannable line
// unless the operator asked for everything.
func (r *Runner) outcome(kind outcomeKind, label, reason string) {
	if !r.narrating() {
		return
	}
	marked := r.mark(kind, label)
	if reason == "" {
		fmt.Fprintf(r.out, "  %s\n", marked)
		return
	}
	text := trimRestatement(label, display.Collapse(r.safe(reason)))
	// A failure's cause and the reasons a pull request is held are what an
	// operator acts on, and a held pull request may carry several, each prepended
	// ahead of the last - so they are wrapped rather than abbreviated, which would
	// drop every hold but the outermost. Routine conclusions stay to one line.
	if kind != outcomeGood || r.explaining() {
		lines := display.Wrap(text, r.reasonColumn(label))
		if len(lines) == 0 {
			fmt.Fprintf(r.out, "  %s\n", marked)
			return
		}
		for i, line := range lines {
			if i == 0 {
				fmt.Fprintf(r.out, "  %s %s\n", marked, line)
				continue
			}
			fmt.Fprintf(r.out, "    %s\n", line)
		}
		return
	}
	fmt.Fprintf(r.out, "  %s %s\n", marked, display.Clip(text, r.style.Width))
}

// reasonColumn charges the label only while it leaves the first line something,
// because the continuation lines are indented by four columns and not by it.
func (r *Runner) reasonColumn(label string) int {
	if display.Width(label)+6 >= r.style.Width {
		return r.style.Width - 4
	}
	return r.style.Width - display.Width(label) - 4
}

// outcomeKind selects the marker and colour for a conclusion.
type outcomeKind int

const (
	outcomeGood outcomeKind = iota
	outcomeAsk
	outcomeBad
)

func (r *Runner) mark(kind outcomeKind, label string) string {
	switch kind {
	case outcomeGood:
		return r.style.Good("+ " + label + ":")
	case outcomeBad:
		return r.style.Bad("! " + label + ":")
	default:
		return r.style.Warn("? " + label + ":")
	}
}

// broken names each object attributed to the merge with the cluster's own
// reason. The agent's diagnosis is an interpretation of this; when it is wrong,
// this is all the operator has.
func (r *Runner) broken(objects []domain.ObjectHealth) {
	if !r.narrating() {
		return
	}
	lines := make([]string, len(objects))
	for i, object := range objects {
		lines[i] = object.Ref.String()
		if object.Reason != "" {
			lines[i] += " — " + object.Reason
		}
	}
	r.bullets(lines)
}

// evidence prints the agent's supporting points. They are the justification for
// an autonomous production change, so they are shown by default whenever a human
// has to judge the outcome, and held back for the routine ones.
func (r *Runner) evidence(needed bool, points []string) {
	if !r.narrating() || (!needed && !r.explaining()) {
		return
	}
	r.bullets(points)
}

func (r *Runner) bullets(lines []string) {
	for _, line := range lines {
		for i, wrapped := range display.Wrap(r.safe(line), r.style.Width-8) {
			marker := "      - "
			if i > 0 {
				marker = "        "
			}
			fmt.Fprintln(r.out, r.style.Detail(marker+wrapped))
		}
	}
}

// Activity reports each tool the agent reaches for, because an assessment is
// otherwise a minute of silence.
func (r *Runner) Activity(tool, argument string) {
	if !r.explaining() {
		return
	}
	description := describeTool(tool, argument)
	// The agent often repeats a lookup; printing it twice tells the operator
	// nothing the first line did not.
	if description == "" || description == r.lastActivity {
		return
	}
	r.lastActivity = description
	fmt.Fprintln(r.out, toolCallIndent+r.style.Detail(
		display.Clip(r.safe(description), r.style.Width-len(toolCallIndent)),
	))
}

func describeTool(tool, argument string) string {
	switch tool {
	case "read_repo_file":
		return "reading " + argument
	case "list_repo_files":
		return "listing " + argument
	case "search_repositories":
		return "looking for the upstream project: " + argument
	case "releases":
		return "reading releases from " + argument
	case "issues":
		return "checking open issues in " + argument
	case "fetch_url":
		return "fetching " + argument
	case "kube_pods":
		return "listing pods in " + argument
	case "kube_events":
		return "reading events in " + argument
	case "kube_logs":
		return "reading logs from " + argument
	case "flux_status":
		return "reading the status of " + argument
	default:
		return ""
	}
}

// waiting renders the watch's periodic status. A window can last ten minutes,
// so it reports only when the picture changes or the cadence elapses, and names
// what it is waiting on rather than only counting.
type waiting struct {
	runner *Runner
	last   time.Duration
	state  string
	opened bool
	wrote  bool
}

func (w *waiting) observe(status cluster.Status) {
	if w.runner == nil || !w.runner.narrating() {
		return
	}
	state := describeWait(status)
	if state == w.state && status.Elapsed-w.last < statusCadence {
		return
	}
	if !w.opened {
		w.runner.step("Waiting for the cluster to reconcile.")
		w.opened = true
	}
	changed := state != w.state
	w.state, w.last = state, status.Elapsed
	elapsedText := elapsed(status.Elapsed)
	budget := w.runner.style.Width - len(waitIndent) - display.Width(elapsedText) - len("  ")
	state = display.Clip(state, budget)
	line := w.runner.style.Detail(w.runner.safe(waitIndent + elapsedText + "  " + state))
	// On a terminal an unchanged picture rewrites its own line rather than
	// scrolling; piped output keeps every line so the file stays a record.
	if w.runner.style.Interactive() && !changed {
		fmt.Fprintf(w.runner.out, "\r\033[K%s", line)
		return
	}
	if w.runner.style.Interactive() && w.wrote {
		fmt.Fprint(w.runner.out, "\r\033[K")
	}
	w.wrote = true
	fmt.Fprintln(w.runner.out, line)
}

func describeWait(status cluster.Status) string {
	if !status.Fetched {
		return "Flux has not fetched the merge commit yet"
	}
	if len(status.Reconciling) == 0 {
		return "settled; holding to confirm"
	}
	return fmt.Sprintf("%s reconciling: %s",
		count(len(status.Reconciling), "object"), nameObjects(status.Reconciling))
}

// nameObjects lists the first few by name, which is what an operator actually
// wants to see, and says how many more there are.
func nameObjects(objects []domain.ObjectHealth) string {
	names := make([]string, 0, namedObjects)
	for i, object := range objects {
		if i == namedObjects {
			break
		}
		names = append(names, object.Ref.Namespace+"/"+object.Ref.Name)
	}
	listed := strings.Join(names, ", ")
	if extra := len(objects) - len(names); extra > 0 {
		listed += fmt.Sprintf(" and %d more", extra)
	}
	return listed
}

func shortSHA(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// count renders a quantity with its noun pluralised.
func count(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

func elapsed(d time.Duration) string {
	total := int(d.Round(time.Second).Seconds())
	return fmt.Sprintf("%2d:%02d", total/60, total%60)
}

// trimRestatement drops a leading restatement of the verdict, because the label
// has already said it. It matches the whole phrase and only a short one: a
// looser rule cut sentences in half at their first colon.
func trimRestatement(label, reason string) string {
	head, rest, found := strings.Cut(reason, ":")
	if !found || len(strings.Fields(head)) > 4 || len(strings.TrimSpace(rest)) < 20 {
		return reason
	}
	if strings.EqualFold(strings.TrimSpace(head), strings.TrimSpace(label)) {
		return strings.TrimSpace(rest)
	}
	return reason
}
