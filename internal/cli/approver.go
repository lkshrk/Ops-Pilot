package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/term"

	"github.com/lkshrk/ops-pilot/internal/ai"
	"github.com/lkshrk/ops-pilot/internal/diagnostics"
	"github.com/lkshrk/ops-pilot/internal/display"
	"github.com/lkshrk/ops-pilot/internal/domain"
	"github.com/lkshrk/ops-pilot/internal/run"
)

const (
	maxAnswer             = 4096
	thinkingFrameInterval = 80 * time.Millisecond
	streamFlushInterval   = 30 * time.Millisecond
)

var thinkingFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

var errAnswerTooLong = errors.New("the answer never ended")

// Approver prompts the operator. A non-interactive run answers no without
// asking, so the pull request is recorded as needing a human and the run
// continues to the next one.
type Approver struct {
	in              *bufio.Reader
	out             io.Writer
	style           display.Style
	interactive     bool
	discard         func()
	dropRest        func() bool
	desynced        bool
	unfinished      bool
	terminal        bool
	streaming       bool
	streamed        bool
	streamText      strings.Builder
	streamPending   strings.Builder
	streamTail      []byte
	streamANSI      ansiState
	redactor        *diagnostics.Redactor
	streamRedactor  *diagnostics.StreamRedactor
	streamMu        sync.Mutex
	streamEventsMu  sync.Mutex
	streamStop      chan struct{}
	streamDone      chan struct{}
	streamFrame     int
	streamLastSpin  time.Time
	streamLastFlush time.Time
	thinkingEvery   time.Duration
	flushEvery      time.Duration
	now             func() time.Time
	// ticks is the animation's only clock: a test drives the loop a tick at a time.
	ticks func(time.Duration) (<-chan time.Time, func())
}

type ansiState uint8

const (
	ansiNone ansiState = iota
	ansiEscape
	ansiCSI
	ansiOSC
	ansiOSCEscape
)

func NewApprover(in io.Reader, out io.Writer, interactive bool, redactor *diagnostics.Redactor) *Approver {
	a := &Approver{
		in:            bufio.NewReader(in),
		out:           out,
		style:         display.NewStyle(out),
		interactive:   interactive,
		redactor:      redactor,
		discard:       func() {},
		thinkingEvery: thinkingFrameInterval,
		flushEvery:    streamFlushInterval,
		now:           time.Now,
		ticks: func(interval time.Duration) (<-chan time.Time, func()) {
			ticker := time.NewTicker(interval)
			return ticker.C, ticker.Stop
		},
	}
	a.dropRest = func() bool { return dropLine(a.in, maxAnswer) }
	if f, ok := out.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		a.terminal = true
	}
	// On a terminal, a line typed before a question is on screen was aimed at an
	// earlier question and must not answer this one; piped input is a deliberate
	// script and is consumed in order.
	if f, ok := in.(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		a.discard = func() {
			a.in.Discard(a.in.Buffered())
			flushInput(int(f.Fd()))
		}
		// The flush already drops the rest of an over-long line, and reading past
		// it instead would block until the operator ended a line nobody asked for.
		a.dropRest = func() bool { return true }
	}
	return a
}

func (a *Approver) Interactive() bool { return a.interactive }

// Stream writes the assistant's response as it arrives, but only to the
// terminal an operator is actively using. A stream is deliberately not a log:
// redirected and unattended runs remain quiet and deterministic.
func (a *Approver) Stream(event ai.StreamEvent) {
	if !a.interactive || !a.terminal {
		return
	}
	a.streamEventsMu.Lock()
	defer a.streamEventsMu.Unlock()
	switch event.Kind {
	case ai.StreamTurnStart:
		a.endStream()
		a.streamMu.Lock()
		a.streaming = true
		a.streamed = false
		a.streamText.Reset()
		a.streamPending.Reset()
		a.streamTail = nil
		a.streamANSI = ansiNone
		a.streamRedactor = a.redactor.Stream()
		a.streamFrame = 0
		a.streamLastSpin = a.now()
		a.streamLastFlush = a.streamLastSpin
		a.writeThinkingLocked()
		a.streamStop, a.streamDone = make(chan struct{}), make(chan struct{})
		stop, done := a.streamStop, a.streamDone
		a.streamMu.Unlock()
		go a.animateStream(stop, done)
	case ai.StreamDelta:
		a.streamMu.Lock()
		if !a.streaming {
			a.streamMu.Unlock()
			return
		}
		text := a.safeStreamTextLocked(event.Text)
		if text == "" {
			a.streamMu.Unlock()
			return
		}
		a.streamText.WriteString(text)
		a.streamPending.WriteString(text)
		if strings.Contains(text, "\n") {
			a.flushStreamLocked()
		}
		a.streamMu.Unlock()
	case ai.StreamTurnEnd:
		a.streamMu.Lock()
		if !a.streaming {
			a.streamMu.Unlock()
			return
		}
		// A trailing partial UTF-8 sequence is untrusted incomplete input; drop it
		// instead of manufacturing a replacement glyph in the terminal.
		a.streamTail = nil
		a.streamANSI = ansiNone
		if text := display.Safe(a.streamRedactor.Flush()); text != "" {
			a.streamText.WriteString(text)
			a.streamPending.WriteString(text)
		}
		a.streamMu.Unlock()
		a.endStream()
	}
}

func (a *Approver) animateStream(stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	interval := a.flushEvery
	if interval <= 0 || (a.thinkingEvery > 0 && a.thinkingEvery < interval) {
		interval = a.thinkingEvery
	}
	if interval <= 0 {
		interval = streamFlushInterval
	}
	ticks, stopTicking := a.ticks(interval)
	defer stopTicking()
	for {
		select {
		case <-stop:
			return
		case now := <-ticks:
			a.streamMu.Lock()
			if !a.streaming {
				a.streamMu.Unlock()
				return
			}
			if now.Sub(a.streamLastSpin) >= a.thinkingEvery {
				a.streamFrame = (a.streamFrame + 1) % len(thinkingFrames)
				a.streamLastSpin = now
				if !a.streamed {
					a.writeThinkingLocked()
				}
			}
			if now.Sub(a.streamLastFlush) >= a.flushEvery {
				a.flushStreamLocked()
			}
			a.streamMu.Unlock()
		}
	}
}

func (a *Approver) endStream() {
	a.streamMu.Lock()
	stop, done := a.streamStop, a.streamDone
	a.streamStop, a.streamDone = nil, nil
	a.streamMu.Unlock()
	if stop != nil {
		close(stop)
		<-done
	}
	a.streamMu.Lock()
	defer a.streamMu.Unlock()
	if !a.streaming {
		return
	}
	a.flushStreamLocked()
	if a.streamed {
		fmt.Fprintln(a.out)
	} else {
		fmt.Fprint(a.out, "\r\033[2K")
	}
	a.streaming = false
}

func (a *Approver) writeThinkingLocked() {
	fmt.Fprint(a.out, "\r\033[2K  "+a.style.Detail(thinkingFrames[a.streamFrame]+" ops-pilot is thinking…"))
}

// safeStreamText holds an incomplete rune until its next chunk, strips terminal
// controls, then gives complete text to the per-turn configured and shape
// redactor.
func (a *Approver) safeStreamTextLocked(text string) string {
	bytes := append(a.streamTail, a.stripStreamControls(text)...)
	a.streamTail = nil
	var clean strings.Builder
	for len(bytes) > 0 {
		r, size := utf8.DecodeRune(bytes)
		if r == utf8.RuneError && size == 1 && !utf8.FullRune(bytes) {
			a.streamTail = append(a.streamTail, bytes...)
			break
		}
		clean.WriteRune(r)
		bytes = bytes[size:]
	}
	return display.Safe(a.streamRedactor.Write(clean.String()))
}

func (a *Approver) flushStreamLocked() {
	if a.streamPending.Len() == 0 {
		return
	}
	if !a.streamed {
		fmt.Fprint(a.out, "\r\033[2K  "+a.style.Accent("ops-pilot")+" > ")
		a.streamed = true
	}
	fmt.Fprint(a.out, a.streamPending.String())
	a.streamPending.Reset()
	a.streamLastFlush = a.now()
}

// stripStreamControls consumes complete and chunk-split ANSI escape sequences
// before display.Safe handles the remaining terminal controls. Safe alone would
// remove ESC but leave its printable CSI tail (for example, "[2J") behind.
func (a *Approver) stripStreamControls(text string) string {
	var clean []byte
	for _, b := range []byte(text) {
		switch a.streamANSI {
		case ansiNone:
			if b == '\x1b' {
				a.streamANSI = ansiEscape
				continue
			}
			clean = append(clean, b)
		case ansiEscape:
			switch b {
			case '[':
				a.streamANSI = ansiCSI
			case ']':
				a.streamANSI = ansiOSC
			case '\x1b':
				// Still waiting for the sequence selector.
			default:
				a.streamANSI = ansiNone
				clean = append(clean, b)
			}
		case ansiCSI:
			if b >= 0x40 && b <= 0x7e {
				a.streamANSI = ansiNone
			}
		case ansiOSC:
			switch b {
			case '\a':
				a.streamANSI = ansiNone
			case '\x1b':
				a.streamANSI = ansiOSCEscape
			}
		case ansiOSCEscape:
			switch b {
			case '\\':
				a.streamANSI = ansiNone
			case '\x1b':
				// Consecutive escapes remain inside the OSC payload.
			default:
				a.streamANSI = ansiOSC
			}
		}
	}
	return string(clean)
}

// Clarify collects context for a fresh assessment. Its free-form answer is
// deliberately separate from merge and fix confirmation, so no text here can
// authorize either action.
func (a *Approver) Clarify(approval run.Approval, question string) (string, bool, error) {
	if !a.interactive {
		return "", false, nil
	}
	prompt := "\n  Enter or /skip to leave this PR pending.\n  " + a.style.Accent("you") + "        > "
	a.streamMu.Lock()
	streamed, turn := a.streamed, a.streamText.String()
	a.streamMu.Unlock()
	if streamed {
		// The streamed assistant turn already gave the context and question; keep
		// this an actual chat turn instead of echoing the whole assessment.
		if !containsQuestion(turn, question) {
			a.paragraph("Question", question)
		}
	} else {
		a.heading("Clarification needed")
		a.field("PR", fmt.Sprintf("#%d", approval.PullRequest.Number))
		a.field("Dependency", approval.Dependency.Name)
		a.field("Version", fmt.Sprintf("%s -> %s (%s)",
			approval.Dependency.FromVersion, approval.Dependency.ToVersion, approval.Dependency.Bump))
		a.paragraph("Why", approval.Assessment.Reason)
		a.paragraph("Question", question)
	}

	line, err := a.ask(prompt)
	answer := strings.TrimSpace(line)
	switch strings.ToLower(answer) {
	case "", "q", "quit", "cancel", "\x1b", "/skip":
		// EOF completes a non-empty terminal line; only a stream with no bytes
		// at all failed to provide a leave-pending choice.
		if err != nil && (!errors.Is(err, io.EOF) || line == "") {
			return "", false, noAnswer("clarification", err)
		}
		return "", false, nil
	}
	// EOF after a complete answer is safe to use once; the following ask fails
	// closed because the stream is exhausted.
	if err != nil && !errors.Is(err, io.EOF) {
		return "", false, noAnswer("clarification", err)
	}
	return answer, true, nil
}

func containsQuestion(turn, question string) bool {
	turn = strings.ToLower(display.Collapse(turn))
	question = strings.ToLower(display.Collapse(display.Safe(diagnostics.ScrubSecrets(question))))
	return question != "" && strings.Contains(turn, question)
}

// ask discards anything typed before the question appears, then reads one
// bounded line. A failed read is the stream's last word: nothing arrives after
// it that could answer the question. A line the read never reached the end of is
// not an answer either, whether it ran too long or was cut short by a failure,
// because where the operator meant it to end is unknown. What remains of such a
// line is read past before the next question rather than after this one, so a
// stream that has already failed is not read again for the answer it failed to
// give. A stream whose line boundaries have been lost is never asked again,
// because any answer read from it could be one somebody aimed at an earlier
// question.
func (a *Approver) ask(question string) (string, error) {
	if a.desynced {
		return "", errAnswerTooLong
	}
	if a.unfinished {
		a.unfinished = false
		if !a.dropRest() {
			a.desynced = true
			return "", errAnswerTooLong
		}
	}
	a.discard()
	fmt.Fprint(a.out, question)
	answer, err := readAnswer(a.in, maxAnswer)
	switch {
	case errors.Is(err, errAnswerTooLong):
		if !a.dropRest() {
			a.desynced = true
		}
	// An end of stream ends the line it stops in; any other failure does not.
	case err != nil && answer != "" && !errors.Is(err, io.EOF):
		a.unfinished = true
		return "", err
	}
	return answer, err
}

// readAnswer drops an over-long line rather than truncating it, because where
// the operator meant their answer to end is no longer knowable.
func readAnswer(in *bufio.Reader, limit int) (string, error) {
	var answer strings.Builder
	for answer.Len() < limit {
		b, err := in.ReadByte()
		if err != nil {
			return answer.String(), err
		}
		answer.WriteByte(b)
		if b == '\n' {
			return answer.String(), nil
		}
	}
	return "", errAnswerTooLong
}

// dropLine reads past the remains of an over-long answer so its tail cannot be
// read as the answer to the next question, and reports whether the line ended
// within limit. Nothing it reads is retained, so the bound on what an answer may
// cost holds however long the line turns out to be.
func dropLine(in *bufio.Reader, limit int) bool {
	for dropped := 0; dropped < limit; {
		chunk, err := in.ReadSlice('\n')
		dropped += len(chunk)
		if err == nil {
			return true
		}
		if !errors.Is(err, bufio.ErrBufferFull) {
			return false
		}
	}
	return false
}

func noAnswer(prompt string, err error) error {
	return fmt.Errorf("no answer to the %s prompt: %w", prompt, err)
}

func changelogLink(changelog domain.Changelog) string {
	if changelog.URL != "" {
		return changelog.URL
	}
	// A repository beside ChangelogNotFound is an attribution the resolver
	// declined, so it may not be turned into a link the operator would read.
	if changelog.Source != domain.ChangelogNotFound && changelog.Repository != "" {
		return "https://github.com/" + changelog.Repository + "/releases"
	}
	return ""
}

func (a *Approver) ApproveFix(request domain.PullRequest, diagnosis domain.Diagnosis) (bool, error) {
	if !a.interactive {
		return false, nil
	}
	a.heading("Proposed repair")
	a.field("PR", fmt.Sprintf("#%d", request.Number))
	a.paragraph("Cause", diagnosis.Cause)
	fmt.Fprint(a.out, "\n"+indent(a.style.Diff(safeLines(diagnostics.ScrubSecrets(diagnosis.Diff)))))
	return a.confirm("Apply this repair?")
}

// ConfirmRevert asks before a merge is discarded. Pressing enter reverts,
// because that is what leaves the cluster as it was; waiting and keeping both
// need a deliberate answer. A stream that ends or breaks is not an answer at
// all and must not be read as one.
func (a *Approver) ConfirmRevert(revert run.Revert) (run.RevertChoice, error) {
	if !a.interactive {
		return run.RevertNow, nil
	}
	a.heading("Failed rollout")
	a.field("PR", fmt.Sprintf("#%d", revert.PullRequest.Number))
	a.field("Dependency", revert.Dependency.Name)
	a.field("Version", fmt.Sprintf("%s -> %s", revert.Dependency.FromVersion, revert.Dependency.ToVersion))
	a.paragraph("Cause", revert.Cause)
	for _, object := range revert.Broken {
		a.paragraph("", fmt.Sprintf("%s — %s", object.Ref, object.Reason))
	}
	a.field("Pull request", revert.PullRequest.URL)
	prompt := a.menu("Resolve this rollout?",
		choice{"r", "Revert merge (default)"},
		choice{"k", "Keep merge"},
		choice{"w", "Wait " + window(revert.Window)},
	)
	for {
		line, err := a.ask(prompt)
		if err != nil && line == "" {
			// RevertKeep rather than the zero choice: a caller that drops the
			// error must still not discard the merge.
			return run.RevertKeep, noAnswer("revert", err)
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "", "r", "revert", "y", "yes":
			return run.RevertNow, nil
		case "n", "no", "k", "keep", "q", "quit", "cancel", "\x1b":
			return run.RevertKeep, nil
		case "w", "wait":
			return run.RevertWait, nil
		}
		if err != nil {
			return run.RevertKeep, noAnswer("revert", err)
		}
		a.invalid("Choose r to revert, k to keep, or w to wait.")
	}
}

func window(d time.Duration) string {
	if d <= 0 {
		return "another window"
	}
	rounded := d.Round(time.Second)
	if rounded%time.Minute == 0 {
		return fmt.Sprintf("%dm", int64(rounded/time.Minute))
	}
	return rounded.String()
}

func (a *Approver) heading(text string) {
	fmt.Fprintf(a.out, "\n%s\n", a.style.Title(display.Safe(diagnostics.ScrubSecrets(text))))
}

func (a *Approver) field(label, value string) {
	fmt.Fprintf(a.out, "  %s %s\n",
		a.style.Accent(fmt.Sprintf("%-12s", label)),
		display.Safe(diagnostics.ScrubSecrets(value)))
}

// paragraph wraps prose instead of emitting one unreadable line, labelling the
// first line and hanging the rest beneath it.
func (a *Approver) paragraph(label, text string) {
	// Scrubbed before Safe, so the rules that reach a value on the line below
	// its key still see the newline that Safe would have turned into a space.
	text = display.Collapse(display.Safe(diagnostics.ScrubSecrets(text)))
	if text == "" {
		return
	}
	head, hang := "  "+a.style.Accent(fmt.Sprintf("%-12s", label))+" ", "               "
	if label == "" {
		head, hang = hang+"- ", hang+"  "
	}
	for i, line := range display.Wrap(text, a.style.Width-len(hang)) {
		if i == 0 {
			fmt.Fprint(a.out, head+line+"\n")
			continue
		}
		fmt.Fprint(a.out, hang+line+"\n")
	}
}

// confirm declines what it cannot ask about, and the error beside that decline
// is the only thing telling a broken stream from an operator's own no.
func (a *Approver) confirm(question string) (bool, error) {
	prompt := a.menu(question,
		choice{"a", "Apply fix"},
		choice{"enter", "Skip fix; choose whether to revert (default)"},
	)
	for {
		line, err := a.ask(prompt)
		if err != nil && line == "" {
			return false, noAnswer("approval", err)
		}
		switch strings.ToLower(strings.TrimSpace(line)) {
		case "a", "apply", "y", "yes":
			return true, nil
		case "", "n", "no", "q", "quit", "cancel", "\x1b":
			return false, nil
		}
		if err != nil {
			return false, noAnswer("approval", err)
		}
		a.invalid("Choose a to apply the fix, or press Enter to skip it.")
	}
}

type choice struct{ key, label string }

func (a *Approver) menu(question string, choices ...choice) string {
	var out strings.Builder
	out.WriteString("\n  " + a.style.Accent("? "+question) + "\n")
	for _, choice := range choices {
		key := fmt.Sprintf("%-9s", "["+choice.key+"]")
		out.WriteString("    " + a.style.Heading(key) + choice.label + "\n")
	}
	out.WriteString("  " + a.style.Accent(">") + " ")
	return out.String()
}

func (a *Approver) invalid(message string) {
	fmt.Fprintln(a.out, "  "+a.style.Warn("! "+message))
}

// safeLines sanitises each line separately, because display.Safe turns a
// newline into a space and a diff is only readable as lines.
func safeLines(value string) string {
	lines := strings.Split(strings.TrimRight(value, "\n"), "\n")
	for i, line := range lines {
		lines[i] = display.Safe(line)
	}
	return strings.Join(lines, "\n")
}

func indent(value string) string {
	var out strings.Builder
	for _, line := range strings.Split(strings.TrimRight(value, "\n"), "\n") {
		out.WriteString("    " + line + "\n")
	}
	return out.String()
}
