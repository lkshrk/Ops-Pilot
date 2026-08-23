package cli

import (
	"bytes"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lkshrk/ops-pilot/internal/ai"
	"github.com/lkshrk/ops-pilot/internal/diagnostics"
	"github.com/lkshrk/ops-pilot/internal/domain"
	"github.com/lkshrk/ops-pilot/internal/run"
)

type brokenTerminal struct{ err error }

func (t brokenTerminal) Read([]byte) (int, error) { return 0, t.err }

var errTerminalGone = errors.New("terminal disconnected")

type countingBuffer struct {
	mu     sync.Mutex
	Buffer bytes.Buffer
	writes int
}

func (b *countingBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.writes++
	return b.Buffer.Write(p)
}

func (b *countingBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

type fakeClock struct {
	mu   sync.Mutex
	now  time.Time
	tick chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(0, 0), tick: make(chan time.Time)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) ticker(time.Duration) (<-chan time.Time, func()) {
	return c.tick, func() {}
}

// The second send completes once the goroutine has received tick #2, not once it has processed it.
func (c *fakeClock) advance(t *testing.T, d time.Duration) {
	t.Helper()
	c.mu.Lock()
	c.now = c.now.Add(d)
	at := c.now
	c.mu.Unlock()
	for range 2 {
		select {
		case c.tick <- at:
		case <-time.After(time.Second):
			t.Fatal("fakeClock.advance: tick send timed out; ticker consumer may have stopped")
		}
	}
}

// bytesWithoutANewline yields data that never finishes a line: the shape a
// half-open terminal produces when sticky is set, and a bottomless stream when
// it is not. budget only stops the test itself running away when the bound it
// pins is missing.
type bytesWithoutANewline struct {
	sticky   error
	budget   int
	produced int
}

func (r *bytesWithoutANewline) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.produced >= r.budget {
		return 0, io.ErrUnexpectedEOF
	}
	r.produced++
	p[0] = 'x'
	return 1, r.sticky
}

// scriptedReads returns exactly what each Read was scripted to return, so a
// chunk can arrive beside an error the way a stream failing mid-line delivers
// it, and so reading can go on afterwards.
type scriptedReads struct {
	reads []scriptedRead
	at    int
}

type scriptedRead struct {
	data string
	err  error
}

func (r *scriptedReads) Read(p []byte) (int, error) {
	if len(p) == 0 || r.at >= len(r.reads) {
		return 0, io.EOF
	}
	next := r.reads[r.at]
	n := copy(p, next.data)
	if n < len(next.data) {
		r.reads[r.at].data = next.data[n:]
		return n, nil
	}
	r.at++
	return n, next.err
}

func plainOutput(t *testing.T) {
	t.Helper()
	t.Setenv("NO_COLOR", "1")
}

func testApprover(t *testing.T, input io.Reader, interactive bool) (*Approver, *bytes.Buffer) {
	t.Helper()
	plainOutput(t)
	out := &bytes.Buffer{}
	approver := NewApprover(input, out, interactive, nil)
	return approver, out
}

// attendedApprover scripts the answers a present operator would type, one line
// per question.
func attendedApprover(t *testing.T, script string) (*Approver, *bytes.Buffer) {
	t.Helper()
	return testApprover(t, strings.NewReader(script), true)
}

func testApproval() run.Approval {
	return run.Approval{
		PullRequest: domain.PullRequest{Number: 42, URL: "https://github.com/o/r/pull/42"},
		Dependency: domain.Dependency{
			Name: "nginx", FromVersion: "1.0.0", ToVersion: "2.0.0", Bump: domain.BumpMajor,
		},
		Assessment: domain.Assessment{Reason: "major version bump"},
	}
}

func testRevert() run.Revert {
	return run.Revert{
		PullRequest: domain.PullRequest{Number: 42, URL: "https://github.com/o/r/pull/42"},
		Dependency:  domain.Dependency{Name: "nginx", FromVersion: "1.0.0", ToVersion: "2.0.0"},
		Cause:       "the rollout never became ready",
		Broken: []domain.ObjectHealth{{
			Ref:    domain.ObjectRef{Kind: "Deployment", Namespace: "web", Name: "nginx"},
			Reason: "0/3 replicas ready",
		}},
		Window: 90 * time.Second,
	}
}

// An unattended run has nobody to ask, so it must answer without writing a
// prompt into the log and without blocking on a stdin that will never speak.
func TestANonInteractiveRunAnswersWithoutPrompting(t *testing.T) {
	tests := []struct {
		name string
		ask  func(*Approver) (any, error)
		want any
	}{
		{
			name: "clarification",
			ask: func(a *Approver) (any, error) {
				answer, answered, err := a.Clarify(testApproval(), "Which manifest should change?")
				return struct {
					answer   string
					answered bool
				}{answer, answered}, err
			},
			want: struct {
				answer   string
				answered bool
			}{},
		},
		{
			name: "fix",
			ask: func(a *Approver) (any, error) {
				return a.ApproveFix(domain.PullRequest{Number: 42}, domain.Diagnosis{Cause: "c", Diff: "d"})
			},
			want: false,
		},
		{
			// Unattended reverting is the whole point of the tool; this answer
			// is configured consent, not absent consent.
			name: "revert",
			ask:  func(a *Approver) (any, error) { return a.ConfirmRevert(testRevert()) },
			want: run.RevertNow,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			approver, out := testApprover(t, brokenTerminal{err: errTerminalGone}, false)
			got, err := test.ask(approver)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != test.want {
				t.Errorf("got %v, want %v", got, test.want)
			}
			if out.Len() != 0 {
				t.Errorf("a non-interactive run wrote a prompt: %q", out.String())
			}
			if approver.Interactive() {
				t.Error("Interactive() must report what it was built with")
			}
		})
	}
}

func TestStreamRendersThinkingThenAssistantChunks(t *testing.T) {
	approver, out := attendedApprover(t, "charts/nginx/values.yaml\n")
	approver.terminal = true
	approver.Stream(ai.StreamEvent{Kind: ai.StreamTurnStart})
	approver.Stream(ai.StreamEvent{Kind: ai.StreamDelta, Text: "Update \x1b[2Jcaf\xc3"})
	approver.Stream(ai.StreamEvent{Kind: ai.StreamDelta, Text: "\xa9. Which values file needs changing?"})
	approver.Stream(ai.StreamEvent{Kind: ai.StreamTurnEnd})

	const want = "\r\033[2K  ⠋ ops-pilot is thinking…\r\033[2K  ops-pilot > Update café. Which values file needs changing?\n"
	if got := out.String(); got != want {
		t.Errorf("stream output = %q, want %q", got, want)
	}
	answer, answered, err := approver.Clarify(testApproval(), "Which values file needs changing?")
	if err != nil || !answered || answer != "charts/nginx/values.yaml" {
		t.Fatalf("Clarify() = (%q, %t, %v)", answer, answered, err)
	}
	if got := out.String(); strings.Contains(got, "Clarification needed") || strings.Count(got, "Which values file") != 1 || !strings.HasSuffix(got, "\n  Enter or /skip to leave this PR pending.\n  you        > ") {
		t.Errorf("chat follow-up repeated streamed context or began uncleanly: %q", got)
	}
}

func TestStreamedStatusStillShowsStructuredQuestion(t *testing.T) {
	approver, out := attendedApprover(t, "yes\n")
	approver.terminal = true
	approver.Stream(ai.StreamEvent{Kind: ai.StreamTurnStart})
	approver.Stream(ai.StreamEvent{Kind: ai.StreamDelta, Text: "I checked the release notes."})
	approver.Stream(ai.StreamEvent{Kind: ai.StreamTurnEnd})
	answer, answered, err := approver.Clarify(testApproval(), "Which values file needs changing?")
	if err != nil || !answered || answer != "yes" {
		t.Fatalf("Clarify() = (%q, %t, %v)", answer, answered, err)
	}
	if got := out.String(); !strings.Contains(got, "Question") || !strings.Contains(got, "Which values file needs changing?") || !strings.HasSuffix(got, "\n  Enter or /skip to leave this PR pending.\n  you        > ") {
		t.Errorf("streamed status hid structured question: %q", got)
	}
}

func TestStreamRedactsConfiguredValuesSplitAcrossDeltas(t *testing.T) {
	const secret = "top secret value"
	for split := 0; split <= len(secret); split++ {
		out := &bytes.Buffer{}
		approver := NewApprover(strings.NewReader(""), out, true, diagnostics.NewRedactor([]string{secret}))
		approver.terminal = true
		approver.Stream(ai.StreamEvent{Kind: ai.StreamTurnStart})
		approver.Stream(ai.StreamEvent{Kind: ai.StreamDelta, Text: "model said " + secret[:split]})
		approver.Stream(ai.StreamEvent{Kind: ai.StreamDelta, Text: secret[split:] + " now"})
		approver.Stream(ai.StreamEvent{Kind: ai.StreamTurnEnd})
		if got := out.String(); strings.Contains(got, secret) || !strings.Contains(got, "[REDACTED]") {
			t.Fatalf("split %d stream = %q", split, got)
		}
	}
}

func TestStreamEndSanitisesTheFinalBufferedText(t *testing.T) {
	out := &bytes.Buffer{}
	approver := NewApprover(strings.NewReader(""), out, true, diagnostics.NewRedactor(nil))
	approver.terminal = true
	approver.Stream(ai.StreamEvent{Kind: ai.StreamTurnStart})
	// "token" holds this credential-looking line until TurnEnd, so these
	// controls exercise the flush path rather than the ordinary delta path.
	approver.Stream(ai.StreamEvent{Kind: ai.StreamDelta, Text: "token: final\r\a\u202epayload"})
	approver.Stream(ai.StreamEvent{Kind: ai.StreamTurnEnd})
	if got := out.String(); strings.ContainsAny(got, "\a\u202e") || strings.Count(got, "\r") != 2 || strings.Contains(got, "payload") {
		t.Fatalf("final buffered stream text was unsafe: %q", got)
	}
}

func TestStreamEndWithoutContentClearsThinkingAndClarifyFallsBack(t *testing.T) {
	approver, out := attendedApprover(t, "\n")
	approver.terminal = true
	approver.Stream(ai.StreamEvent{Kind: ai.StreamTurnStart})
	approver.Stream(ai.StreamEvent{Kind: ai.StreamDelta, Text: "\x1b\x00"})
	approver.Stream(ai.StreamEvent{Kind: ai.StreamTurnEnd})
	if got, want := out.String(), "\r\033[2K  ⠋ ops-pilot is thinking…\r\033[2K"; got != want {
		t.Errorf("empty stream output = %q, want %q", got, want)
	}
	_, _, _ = approver.Clarify(testApproval(), "Which values file needs changing?")
	if got := out.String(); !strings.Contains(got, "Clarification needed") || !strings.Contains(got, "Which values file needs changing?") {
		t.Errorf("empty stream did not use full clarification fallback: %q", got)
	}
}

func TestStreamThinkingAnimatesAndFirstTokenReplacesIt(t *testing.T) {
	approver, out := attendedApprover(t, "")
	clock := newFakeClock()
	approver.terminal = true
	approver.now, approver.ticks = clock.Now, clock.ticker
	approver.thinkingEvery = 5 * time.Millisecond
	approver.flushEvery = 2 * time.Millisecond
	approver.Stream(ai.StreamEvent{Kind: ai.StreamTurnStart})
	clock.advance(t, 5*time.Millisecond)
	clock.advance(t, 5*time.Millisecond)
	approver.Stream(ai.StreamEvent{Kind: ai.StreamDelta, Text: "hello"})
	approver.Stream(ai.StreamEvent{Kind: ai.StreamTurnEnd})

	got := out.String()
	if frames := strings.Count(got, "ops-pilot is thinking…"); frames != 3 {
		t.Fatalf("thinking frames = %d, output %q", frames, got)
	}
	if !strings.Contains(got, "\r\033[2K  ops-pilot > hello\n") {
		t.Fatalf("first token did not atomically replace thinking: %q", got)
	}
}

func TestStreamBatchesDeltasAndFlushesBeforeEnd(t *testing.T) {
	plainOutput(t)
	out := &countingBuffer{}
	clock := newFakeClock()
	approver := NewApprover(strings.NewReader(""), out, true, nil)
	approver.terminal = true
	approver.now, approver.ticks = clock.Now, clock.ticker
	approver.thinkingEvery = time.Hour
	approver.flushEvery = 5 * time.Millisecond
	approver.Stream(ai.StreamEvent{Kind: ai.StreamTurnStart})
	for _, delta := range []string{"a ", "b ", "c ", "d ", "e ", "f ", "g ", "h "} {
		approver.Stream(ai.StreamEvent{Kind: ai.StreamDelta, Text: delta})
	}
	clock.advance(t, 5*time.Millisecond)
	beforeEnd := out.String()
	if !strings.Contains(beforeEnd, "ops-pilot > a b c d e f g h ") {
		t.Fatalf("continuous stream did not flush before end: %q", beforeEnd)
	}
	approver.Stream(ai.StreamEvent{Kind: ai.StreamTurnEnd})
	if got := out.String(); !strings.HasSuffix(got, "ops-pilot > a b c d e f g h \n") {
		t.Fatalf("batched output = %q", got)
	}
	out.mu.Lock()
	writes := out.writes
	out.mu.Unlock()
	if writes >= 8 {
		t.Fatalf("writes = %d, want fewer than 8 deltas", writes)
	}
}

func TestStreamEndStopsAnimationAndConsecutiveTurnsStaySeparate(t *testing.T) {
	approver, out := attendedApprover(t, "")
	approver.terminal = true
	approver.thinkingEvery = 2 * time.Millisecond
	approver.flushEvery = 2 * time.Millisecond
	approver.Stream(ai.StreamEvent{Kind: ai.StreamTurnStart})
	approver.Stream(ai.StreamEvent{Kind: ai.StreamTurnEnd})
	ended := out.String()
	time.Sleep(10 * time.Millisecond)
	if got := out.String(); got != ended {
		t.Fatalf("animation wrote after end: before %q after %q", ended, got)
	}
	// Stray events are ignored, then the next complete turn starts cleanly.
	approver.Stream(ai.StreamEvent{Kind: ai.StreamDelta, Text: "ignored"})
	approver.Stream(ai.StreamEvent{Kind: ai.StreamTurnEnd})
	approver.Stream(ai.StreamEvent{Kind: ai.StreamTurnStart})
	approver.Stream(ai.StreamEvent{Kind: ai.StreamDelta, Text: "next"})
	approver.Stream(ai.StreamEvent{Kind: ai.StreamTurnEnd})
	if got := out.String(); strings.Count(got, "ops-pilot > next\n") != 1 || strings.Contains(got, "ignored") {
		t.Fatalf("consecutive turns were not isolated: %q", got)
	}
}

func TestNonInteractiveStreamWritesNothing(t *testing.T) {
	approver, out := testApprover(t, strings.NewReader(""), false)
	approver.terminal = true
	approver.Stream(ai.StreamEvent{Kind: ai.StreamTurnStart})
	approver.Stream(ai.StreamEvent{Kind: ai.StreamDelta, Text: "hello"})
	approver.Stream(ai.StreamEvent{Kind: ai.StreamTurnEnd})
	if got := out.String(); got != "" {
		t.Errorf("non-interactive stream output = %q, want empty", got)
	}
}

func TestClarifyCollectsOnlyFreeFormAnswers(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		answer   string
		answered bool
	}{
		{name: "answer", input: "set replicas to three\n", answer: "set replicas to three", answered: true},
		{name: "merge word is still text", input: "merge\n", answer: "merge", answered: true},
		{name: "blank defers", input: "\n"},
		{name: "q defers", input: "q\n"},
		{name: "q at EOF defers", input: "q"},
		{name: "quit defers", input: "quit\n"},
		{name: "cancel defers", input: "cancel\n"},
		{name: "escape defers", input: "\x1b\n"},
		{name: "skip is model prose", input: "skip\n", answer: "skip", answered: true},
		{name: "slash skip defers", input: "/skip\n"},
		{name: "later is model prose", input: "later\n", answer: "later", answered: true},
		{name: "defer is model prose", input: "defer\n", answer: "defer", answered: true},
		{name: "skip case variant is model prose", input: " \tSkIp \t\n", answer: "SkIp", answered: true},
		{name: "later case variant is model prose", input: "LATER\n", answer: "LATER", answered: true},
		{name: "defer case variant is model prose", input: "DeFeR\n", answer: "DeFeR", answered: true},
		{name: "skip prose remains an answer", input: "skip this check but continue\n", answer: "skip this check but continue", answered: true},
		{name: "later prose remains an answer", input: "later today works\n", answer: "later today works", answered: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			approver, out := attendedApprover(t, test.input)
			answer, answered, err := approver.Clarify(testApproval(), "Update ghp_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789 safely?\x1b")
			if err != nil {
				t.Fatalf("Clarify() error = %v", err)
			}
			if answer != test.answer || answered != test.answered {
				t.Errorf("Clarify() = (%q, %t), want (%q, %t)", answer, answered, test.answer, test.answered)
			}
			if strings.Contains(out.String(), "ghp_AbCdEfGhIjKlMnOpQrStUvWxYz0123456789") || strings.Contains(out.String(), "\x1b") {
				t.Errorf("question was not safely displayed: %q", out.String())
			}
			if !strings.Contains(out.String(), "Enter or /skip to leave this PR pending.") {
				t.Errorf("clarification prompt did not explain how to leave the PR pending: %q", out.String())
			}
			if strings.Contains(out.String(), "Decide later") {
				t.Errorf("clarification prompt repeated a menu instead of using chat input: %q", out.String())
			}
		})
	}
}

func TestClarifyFailsClosedOnBrokenOrExhaustedInput(t *testing.T) {
	tests := []struct {
		name  string
		input io.Reader
	}{
		{name: "empty EOF", input: strings.NewReader("")},
		{name: "broken stream", input: brokenTerminal{err: errTerminalGone}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			approver, _ := testApprover(t, test.input, true)
			answer, answered, err := approver.Clarify(testApproval(), "What changed?")
			if err == nil || answer != "" || answered {
				t.Errorf("Clarify() = (%q, %t, %v), want unanswered error", answer, answered, err)
			}
		})
	}
}

func TestClarifyDeliversAnEOFAnswerOnceThenFailsClosed(t *testing.T) {
	approver, _ := attendedApprover(t, "update the values file")
	answer, answered, err := approver.Clarify(testApproval(), "What changed?")
	if err != nil || answer != "update the values file" || !answered {
		t.Fatalf("first Clarify() = (%q, %t, %v)", answer, answered, err)
	}
	answer, answered, err = approver.Clarify(testApproval(), "Anything else?")
	if err == nil || answer != "" || answered {
		t.Errorf("second Clarify() = (%q, %t, %v), want unanswered error", answer, answered, err)
	}
}

func TestConfirmAcceptsOnlyAnExplicitYes(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{name: "y", input: "y\n", want: true},
		{name: "apply", input: "a\n", want: true},
		{name: "uppercase", input: "Y\n", want: true},
		{name: "yes", input: "yes\n", want: true},
		{name: "mixed case", input: "YeS\n", want: true},
		{name: "surrounding whitespace", input: "  yes \t\n", want: true},
		{name: "no newline", input: "y", want: true},
		{name: "bare enter declines", input: "\n", want: false},
		{name: "whitespace only declines", input: "   \n", want: false},
		{name: "n", input: "n\n", want: false},
		{name: "no", input: "NO\n", want: false},
		{name: "unrecognised reprompts until yes", input: "maybe\ny\n", want: true},
		{name: "unrecognised reprompts until no", input: "maybe\nn\n", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			approver, out := attendedApprover(t, test.input)
			got, err := approver.confirm("  merge anyway?")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != test.want {
				t.Errorf("confirm(%q) = %t, want %t", test.input, got, test.want)
			}
			if !strings.Contains(out.String(), "[enter]  Skip fix; choose whether to revert (default)") {
				t.Errorf("the prompt must show that no is the default: %q", out.String())
			}
		})
	}
}

// confirm guards additive acts — merging, applying a fix — so an operator who
// cannot answer must leave the repository as it is. Declining is only half the
// answer: run.recordDecline comments "an operator declined this update" and
// applies the declined label, parking the update until a human removes it, so a
// stream that broke has to be tellable from an operator who said no.
func TestAnUnanswerableConfirmDeclinesAndReportsThatNobodyAnswered(t *testing.T) {
	tests := []struct {
		name  string
		input io.Reader
	}{
		{name: "eof", input: strings.NewReader("")},
		{name: "read error", input: brokenTerminal{err: errTerminalGone}},
		{name: "unrecognised then eof", input: strings.NewReader("maybe\n")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			approver, _ := testApprover(t, test.input, true)
			got, err := approver.confirm("  merge anyway?")
			if err == nil {
				t.Error("an unanswered prompt was reported as a decision the operator made")
			}
			if got {
				t.Error("an unanswerable prompt must not be read as approval")
			}
		})
	}
}

// Fix approval reaches internal/run through the Approver port, and its caller
// branches on the error before the bool, so a broken stream cannot write.
func TestABrokenStreamDeclinesFixWithAnError(t *testing.T) {
	tests := []struct {
		name string
		ask  func(*Approver) (bool, error)
	}{
		{
			name: "fix",
			ask: func(a *Approver) (bool, error) {
				return a.ApproveFix(
					domain.PullRequest{Number: 42},
					domain.Diagnosis{Cause: "the image tag is wrong", Diff: "-  image: a\n"},
				)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			approver, _ := testApprover(t, brokenTerminal{err: errTerminalGone}, true)
			got, err := test.ask(approver)
			if err == nil {
				t.Fatal("want an error the caller can tell from a decline")
			}
			if !errors.Is(err, errTerminalGone) {
				t.Errorf("error = %v, want it to carry %v", err, errTerminalGone)
			}
			if got {
				t.Error("a broken stream must not be read as approval")
			}
		})
	}
}

func TestConfirmRevertMapsEachAnswerToItsChoice(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  run.RevertChoice
	}{
		{name: "bare enter reverts", input: "\n", want: run.RevertNow},
		{name: "whitespace only reverts", input: "  \t\n", want: run.RevertNow},
		{name: "y", input: "y\n", want: run.RevertNow},
		{name: "revert", input: "r\n", want: run.RevertNow},
		{name: "yes mixed case", input: "YeS\n", want: run.RevertNow},
		{name: "n", input: "n\n", want: run.RevertKeep},
		{name: "no", input: "no\n", want: run.RevertKeep},
		{name: "k", input: "K\n", want: run.RevertKeep},
		{name: "keep", input: "  keep  \n", want: run.RevertKeep},
		{name: "w", input: "w\n", want: run.RevertWait},
		{name: "wait", input: " WAIT \n", want: run.RevertWait},
		{name: "no newline", input: "n", want: run.RevertKeep},
		{name: "unrecognised reprompts", input: "later\nw\n", want: run.RevertWait},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			approver, out := attendedApprover(t, test.input)
			got, err := approver.ConfirmRevert(testRevert())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != test.want {
				t.Errorf("ConfirmRevert(%q) = %q, want %q", test.input, got, test.want)
			}
			if !strings.Contains(out.String(), "[w]      Wait 1m30s") {
				t.Errorf("the prompt must show the choices and the wait length: %q", out.String())
			}
		})
	}
}

// Pressing enter is a deliberate act by a person who read the prompt; a stream
// that ends or breaks is not an answer at all. Reverting on the second is the
// fail-open path C-H08 names: a terminal dropping mid-prompt would discard a
// merge nobody was asked about.
//
// The choice must be RevertKeep specifically, not merely something other than
// RevertNow. run.Runner switches on it with `default:` reverting, so the zero
// RevertChoice — the obvious thing to return beside an error — reaches the
// revert path just as RevertNow does for any caller that drops the error.
func TestAnUnanswerableRevertPromptHaltsRatherThanReverting(t *testing.T) {
	tests := []struct {
		name  string
		input io.Reader
	}{
		{name: "eof", input: strings.NewReader("")},
		{name: "read error", input: brokenTerminal{err: errTerminalGone}},
		{name: "unrecognised then eof", input: strings.NewReader("later\n")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			approver, _ := testApprover(t, test.input, true)
			got, err := approver.ConfirmRevert(testRevert())
			if err == nil {
				t.Fatalf("want an error the caller can halt on, got choice %q", got)
			}
			if got != run.RevertKeep {
				t.Errorf("choice = %q, want %q: a caller that drops the error must "+
					"still land on a terminal, non-destructive choice", got, run.RevertKeep)
			}
		})
	}
}

// Re-asking is right for an operator's typo and wrong for a stream that yields
// bytes it can never finish a line with: the unrecognised answer is the same
// one next time round. Both shapes below asked forever — one printing the
// question at every errored read, one growing a single answer without end.
//
// The endless shape reads two bounds' worth, not one: the answer up to the
// bound, then as far again looking for the line's end. What must hold is that
// both are bounds and neither accumulates — a budget here that grows with the
// stream is the bug this test exists for.
func TestAPromptIsNotSpunByAStreamThatNeverFinishesALine(t *testing.T) {
	tests := []struct {
		name     string
		sticky   error
		wantRead int
	}{
		{name: "a byte beside a broken terminal", sticky: errTerminalGone, wantRead: 1},
		{name: "endless data with no newline", sticky: nil, wantRead: 2 * maxAnswer},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Run("confirm", func(t *testing.T) {
				stream := &bytesWithoutANewline{sticky: test.sticky, budget: 4 * maxAnswer}
				approver, out := testApprover(t, stream, true)
				got, err := approver.confirm("  merge anyway?")
				if err == nil {
					t.Error("a stream that never answered was reported as a decision")
				}
				if got {
					t.Error("a stream that never answered must not be read as approval")
				}
				assertAskedOnce(t, stream, out.String(), "merge anyway?", test.wantRead)
			})

			t.Run("revert", func(t *testing.T) {
				stream := &bytesWithoutANewline{sticky: test.sticky, budget: 4 * maxAnswer}
				approver, out := testApprover(t, stream, true)
				got, err := approver.ConfirmRevert(testRevert())
				if err == nil {
					t.Error("a stream that never answered was reported as a decision")
				}
				if got != run.RevertKeep {
					t.Errorf("choice = %q, want %q: nobody consented to discarding the merge",
						got, run.RevertKeep)
				}
				assertAskedOnce(t, stream, out.String(), "Resolve this rollout?", test.wantRead)
			})
		})
	}
}

// What is bounded is a stream that cannot answer, not how often a person may
// mistype: an operator who fumbles the answer a hundred times is still the one
// who decides, and capping the re-prompts instead would take that away.
func TestAnOperatorMayMistypeAsOftenAsTheyLikeBeforeAnswering(t *testing.T) {
	const fumbled = 100

	approver, _ := attendedApprover(t, strings.Repeat("maybe\n", fumbled)+"y\n")
	merged, err := approver.confirm("  merge anyway?")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !merged {
		t.Error("the answer typed after the typos was not the one that counted")
	}

	approver, _ = attendedApprover(t, strings.Repeat("later\n", fumbled)+"n\n")
	choice, err := approver.ConfirmRevert(testRevert())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if choice != run.RevertKeep {
		t.Errorf("choice = %q, want %q", choice, run.RevertKeep)
	}
}

// An answer dropped for being over-long leaves the rest of its line unread, and
// on a pipe nothing flushes it, so the next question reads the tail of a line
// aimed at the last one. The tail scripted here is "y": as an approval it merges
// what nobody approved, and as a revert answer it discards a merge nobody was
// asked about — the 3am false positive the tool exists to avoid.
func TestTheTailOfADroppedAnswerDoesNotAnswerTheNextQuestion(t *testing.T) {
	// One over-long line whose tail reads as consent, then the real answer to
	// the second question.
	script := strings.Repeat("j", maxAnswer) + "y\n" + "n\n"

	t.Run("merge", func(t *testing.T) {
		approver, _ := attendedApprover(t, script)
		if _, err := approver.confirm("  merge anyway?"); err == nil {
			t.Fatal("an answer that never ended was reported as a decision")
		}
		merged, err := approver.confirm("  merge anyway?")
		if merged {
			t.Errorf("the second question was approved by the tail of the first "+
				"answer, not by the %q on its own line", "n")
		}
		if err != nil {
			t.Errorf("the scripted answer to the second question was lost: %v", err)
		}
	})

	t.Run("revert", func(t *testing.T) {
		approver, _ := attendedApprover(t, script)
		if _, err := approver.ConfirmRevert(testRevert()); err == nil {
			t.Fatal("an answer that never ended was reported as a decision")
		}
		choice, err := approver.ConfirmRevert(testRevert())
		if choice != run.RevertKeep {
			t.Errorf("choice = %q, want %q: the merge was discarded on debris from "+
				"a line answering an earlier question", choice, run.RevertKeep)
		}
		if err != nil {
			t.Errorf("the scripted answer to the second question was lost: %v", err)
		}
	})
}

// Reading past the tail is bounded, so a line that outlasts the bound leaves the
// stream at an unknown offset for good: every later answer read from it could
// belong to another question. Refusing outright is the only reading that cannot
// invent consent, and it costs no further reads.
func TestAnAnswerThatOutlastsTheDropBudgetSilencesEveryLaterQuestion(t *testing.T) {
	stream := &bytesWithoutANewline{budget: 16 * maxAnswer}
	approver, out := testApprover(t, stream, true)

	if _, err := approver.confirm("  merge anyway?"); err == nil {
		t.Fatal("an answer that never ended was reported as a decision")
	}
	afterFirst := stream.produced
	if afterFirst > 2*maxAnswer {
		t.Errorf("read %d bytes for one dropped answer, want at most %d",
			afterFirst, 2*maxAnswer)
	}

	choice, err := approver.ConfirmRevert(testRevert())
	if err == nil {
		t.Error("a stream nobody can be read from was reported as a decision")
	}
	if choice != run.RevertKeep {
		t.Errorf("choice = %q, want %q", choice, run.RevertKeep)
	}
	if stream.produced != afterFirst {
		t.Errorf("read %d more bytes after the stream was known to be out of step, want 0",
			stream.produced-afterFirst)
	}
	if asked := strings.Count(out.String(), "Resolve this rollout?"); asked != 0 {
		t.Errorf("a question was printed %d times to a stream that cannot answer it, want 0", asked)
	}
}

// A read that fails part way through a line is two problems at once. What
// arrived is not an answer -- "n" may be all that came of "never", so reading it
// as one invents a decision nobody finished making -- and the rest of that line
// stays in the reader, where on a pipe nothing flushes it, so it answers the
// question after the one it was aimed at. The tail scripted here is "y": as an
// approval it merges what nobody approved, and as a revert answer it discards a
// merge nobody was asked about.
//
// The answer to the second question is deliberately neither the tail's meaning
// nor the fail-safe one, so silencing the stream fails this test as loudly as
// letting the tail through does.
func TestAnAnswerCutShortByAFailedReadDecidesNothingAndCannotAnswerTheNextQuestion(t *testing.T) {
	tests := []struct {
		name    string
		partial string
	}{
		{name: "the part that arrived reads as consent", partial: "y"},
		{name: "the part that arrived reads as a refusal", partial: "n"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			script := func(answer string) *scriptedReads {
				return &scriptedReads{reads: []scriptedRead{
					{data: test.partial, err: errTerminalGone},
					{data: "y\n" + answer},
				}}
			}

			t.Run("revert", func(t *testing.T) {
				approver, _ := testApprover(t, script("w\n"), true)

				choice, err := approver.ConfirmRevert(testRevert())
				if choice != run.RevertKeep {
					t.Errorf("choice = %q, want %q: nothing that arrived was a whole answer",
						choice, run.RevertKeep)
				}
				if err == nil {
					t.Error("a read that failed mid-answer was reported as a decision the operator made")
				}

				choice, err = approver.ConfirmRevert(testRevert())
				if choice != run.RevertWait {
					t.Errorf("choice = %q, want %q: the second question was decided by debris "+
						"from a line answering an earlier one, or by nothing at all",
						choice, run.RevertWait)
				}
				if err != nil {
					t.Errorf("the scripted answer to the second question was lost: %v", err)
				}
			})
		})
	}
}

// A stream that fails mid-line and stays broken has no line end left to find, so
// looking for one must not become a cost every later question pays. The reading
// past the tail is what the next answer spends, not what the failed one does, so
// the prompt that failed reads nothing more, the one after it spends a single
// read discovering the stream cannot be realigned, and every question after that
// is answered fail-safe without reading or asking at all.
func TestAStreamThatStaysBrokenAfterFailingMidAnswerIsSilencedRatherThanGuessedAt(t *testing.T) {
	stream := &bytesWithoutANewline{sticky: errTerminalGone, budget: 16 * maxAnswer}
	approver, out := testApprover(t, stream, true)

	merged, err := approver.confirm("  merge anyway?")
	if merged {
		t.Error("a read that failed mid-answer was read as approval")
	}
	if err == nil {
		t.Fatal("a read that failed mid-answer was reported as a decision the operator made")
	}
	if stream.produced != 1 {
		t.Errorf("read %d bytes for an answer the stream had already failed to give, want 1",
			stream.produced)
	}

	choice, err := approver.ConfirmRevert(testRevert())
	if err == nil {
		t.Error("a stream that cannot be realigned was reported as a decision")
	}
	if choice != run.RevertKeep {
		t.Errorf("choice = %q, want %q: nobody consented to discarding the merge", choice, run.RevertKeep)
	}
	if stream.produced > 2 {
		t.Errorf("read %d bytes looking for the end of a line that has none, want at most 2",
			stream.produced)
	}
	silenced := stream.produced

	if _, err := approver.confirm("  merge anyway?"); err == nil {
		t.Error("a stream known to be out of step was reported as a decision")
	}
	if stream.produced != silenced {
		t.Errorf("read %d more bytes after the stream was known to be out of step, want 0",
			stream.produced-silenced)
	}
	if asked := strings.Count(out.String(), "Resolve this rollout?"); asked != 0 {
		t.Errorf("a question was printed %d times to a stream that cannot answer it, want 0", asked)
	}
}

// A read can fail before a single byte of an answer has arrived. No line was in
// progress then, so there is no remainder of one to read past, and reading past
// one anyway would swallow the next question's own answer -- a whole line nobody
// aimed at it, charged for a line that never started. Only an answer the read
// reached into pays the drop.
func TestAReadThatFailsBeforeAnyAnswerArrivesDoesNotCostTheNextQuestionItsAnswer(t *testing.T) {
	script := func(answer string) *scriptedReads {
		return &scriptedReads{reads: []scriptedRead{
			{err: errTerminalGone},
			{data: answer},
		}}
	}

	t.Run("revert", func(t *testing.T) {
		approver, _ := testApprover(t, script("w\n"), true)

		choice, err := approver.ConfirmRevert(testRevert())
		if choice != run.RevertKeep {
			t.Errorf("choice = %q, want %q: nothing arrived that could answer",
				choice, run.RevertKeep)
		}
		if err == nil {
			t.Fatal("a failed read was reported as a decision the operator made")
		}

		choice, err = approver.ConfirmRevert(testRevert())
		if choice != run.RevertWait {
			t.Errorf("choice = %q, want %q: the answer to the second question was read "+
				"past as the tail of a line the failed read never reached into",
				choice, run.RevertWait)
		}
		if err != nil {
			t.Errorf("the scripted answer to the second question was lost: %v", err)
		}
	})
}

// Reading past the remains of an answer a failed read cut short is paid for by
// the question after it, and a failure that took the rest of the line with it
// leaves no remains to find -- so that reading lands on the next question's own
// answer and spends it. Nothing in the stream distinguishes the tail of the
// broken line from the next answer, and treating those bytes as the next answer
// is exactly what lets a line decide a question nobody aimed it at. So the price
// is deliberate, and it is charged in the safe direction: both questions decline
// with an error, keeping the merge and halting, rather than reading as an
// operator's own no.
func TestAFailedReadWithNoTailLeftBehindItCostsTheNextQuestionItsAnswer(t *testing.T) {
	script := func() *scriptedReads {
		return &scriptedReads{reads: []scriptedRead{
			{data: "n", err: errTerminalGone},
			{data: "y\n"},
		}}
	}

	t.Run("revert", func(t *testing.T) {
		approver, _ := testApprover(t, script(), true)

		choice, err := approver.ConfirmRevert(testRevert())
		if choice != run.RevertKeep {
			t.Errorf("choice = %q, want %q: nothing that arrived was a whole answer",
				choice, run.RevertKeep)
		}
		if err == nil {
			t.Fatal("a read that failed mid-answer was reported as a decision the operator made")
		}

		choice, err = approver.ConfirmRevert(testRevert())
		if choice != run.RevertKeep {
			t.Errorf("choice = %q, want %q: the merge was discarded on a line read past "+
				"to realign the stream", choice, run.RevertKeep)
		}
		if err == nil {
			t.Error("the answer spent realigning the stream was reported as an operator's keep")
		}
	})
}

func assertAskedOnce(t *testing.T, stream *bytesWithoutANewline, rendered, prompt string, wantRead int) {
	t.Helper()
	if asked := strings.Count(rendered, prompt); asked != 1 {
		t.Errorf("the question was printed %d times, want once", asked)
	}
	if stream.produced > wantRead {
		t.Errorf("read %d bytes for one answer, want at most %d", stream.produced, wantRead)
	}
}

func TestTheRevertPromptShowsWhatWouldBeDiscarded(t *testing.T) {
	approver, out := attendedApprover(t, "n\n")
	if _, err := approver.ConfirmRevert(testRevert()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{
		"#42", "nginx", "1.0.0", "2.0.0",
		"the rollout never became ready",
		"web/Deployment/nginx", "0/3 replicas ready",
		"https://github.com/o/r/pull/42",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the prompt omitted %q:\n%s", want, out.String())
		}
	}
}

// The resolver returns a bare ChangelogNotFound whenever it declines an
// attribution, so a repository on such a value is one nothing vouched for.
func TestNoChangelogLinkIsBuiltFromAnAttributionTheResolverRefused(t *testing.T) {
	tests := []struct {
		name      string
		changelog domain.Changelog
		want      string
	}{
		{
			name:      "explicit url wins",
			changelog: domain.Changelog{Source: domain.ChangelogFromOverride, URL: "https://x/notes"},
			want:      "https://x/notes",
		},
		{
			// run.go swaps in the agent's URL without correcting Source, so a
			// found URL must still render on a not-found changelog.
			name:      "url found by the agent survives a stale source",
			changelog: domain.Changelog{Source: domain.ChangelogNotFound, URL: "https://x/notes"},
			want:      "https://x/notes",
		},
		{
			name:      "trusted repository becomes a releases link",
			changelog: domain.Changelog{Source: domain.ChangelogFromAnnotation, Repository: "o/r"},
			want:      "https://github.com/o/r/releases",
		},
		{
			name:      "refused attribution renders nothing",
			changelog: domain.Changelog{Source: domain.ChangelogNotFound, Repository: "o/r"},
			want:      "",
		},
		{name: "empty", changelog: domain.Changelog{}, want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := changelogLink(test.changelog); got != test.want {
				t.Errorf("changelogLink = %q, want %q", got, test.want)
			}
		})
	}
}

// A long run scrolls, so a question has to restate what it is about rather than
// rely on a headline that may be far above. The fix prompt is the one whose
// decline reverts a merge, and it was the one that did not restate.
func TestEveryPromptSaysWhichPullRequestItIsAskingAbout(t *testing.T) {
	tests := []struct {
		name string
		ask  func(*Approver) error
	}{
		{"fix", func(a *Approver) error {
			_, err := a.ApproveFix(
				domain.PullRequest{Number: 42},
				domain.Diagnosis{Cause: "the image tag is wrong", Diff: "@@ -1 +1 @@\n-a\n+b\n"},
			)
			return err
		}},
		{"revert", func(a *Approver) error { _, err := a.ConfirmRevert(testRevert()); return err }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			approver, out := attendedApprover(t, "n\n")

			if err := test.ask(approver); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if !strings.Contains(out.String(), "#42") {
				t.Fatalf("the prompt never said which pull request it is about:\n%s", out.String())
			}
		})
	}
}

// A diff is what the operator is actually approving, and skipping it leads to
// the revert decision, so it has to stay readable rather than collapse.
func TestTheFixPromptKeepsTheDiffOnSeparateLines(t *testing.T) {
	diagnosis := domain.Diagnosis{
		Cause: "the image tag is wrong",
		Diff:  "--- a/app.yaml\n+++ b/app.yaml\n-  image: nginx:2.0.0\n+  image: nginx:2.0.1\n",
	}
	approver, out := attendedApprover(t, "n\n")
	if _, err := approver.ApproveFix(domain.PullRequest{Number: 42}, diagnosis); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{
		"    --- a/app.yaml", "    +++ b/app.yaml",
		"    -  image: nginx:2.0.0", "    +  image: nginx:2.0.1",
	} {
		if !strings.Contains(out.String(), want+"\n") {
			t.Errorf("the diff line %q was not rendered on its own line:\n%s", want, out.String())
		}
	}
	if !strings.Contains(out.String(), "choose whether to revert") {
		t.Errorf("the prompt must say what declining costs:\n%s", out.String())
	}
}

// Style.Diff colours by leading +/-, so indenting before colouring rather than
// after would silently render an agent's whole change in one colour.
func TestTheFixPromptColoursAddedAndRemovedLinesApart(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("TERM", "xterm")
	t.Setenv("FORCE_COLOR", "1")

	out := &bytes.Buffer{}
	approver := NewApprover(strings.NewReader("n\n"), out, true, nil)
	diagnosis := domain.Diagnosis{
		Cause: "the image tag is wrong",
		Diff:  "@@ -1 +1 @@\n-  image: nginx:2.0.0\n+  image: nginx:2.0.1\n",
	}
	if _, err := approver.ApproveFix(domain.PullRequest{Number: 42}, diagnosis); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	added := strings.Index(out.String(), "+  image: nginx:2.0.1")
	removed := strings.Index(out.String(), "-  image: nginx:2.0.0")
	if added < 1 || removed < 1 {
		t.Fatalf("the diff lines are missing:\n%s", out.String())
	}
	if out.String()[:added] == out.String()[:removed] ||
		strings.Count(out.String(), "\x1b[") < 3 {
		t.Errorf("the diff was not coloured line by line:\n%q", out.String())
	}
}

// Every string in these prompts is attacker-influenced: a Renovate pull request
// body, an upstream annotation, a Kubernetes status message.
func TestPromptsNeverLetForeignTextDriveTheTerminal(t *testing.T) {
	hostile := "\x1b[2J\x1b[1;1Hall clear\nmerged successfully"

	t.Run("revert", func(t *testing.T) {
		revert := testRevert()
		revert.Cause = hostile
		revert.Broken[0].Reason = hostile
		revert.PullRequest.URL = hostile
		approver, out := attendedApprover(t, "n\n")
		if _, err := approver.ConfirmRevert(revert); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertNoEscapes(t, out.String())
		assertFieldsStayOnOneLine(t, out.String())
	})

	// The diff block is the one place multi-line foreign text is the point, so
	// only the escaping applies to it; the cause beside it is still a field.
	t.Run("fix", func(t *testing.T) {
		approver, out := attendedApprover(t, "n\n")
		_, err := approver.ApproveFix(
			domain.PullRequest{Number: 42},
			domain.Diagnosis{Cause: hostile, Diff: "-  image: a\n" + hostile},
		)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		assertNoEscapes(t, out.String())
		assertFieldsStayOnOneLine(t, out.String())
	})
}

func assertNoEscapes(t *testing.T, rendered string) {
	t.Helper()
	if strings.ContainsRune(rendered, 0x1b) {
		t.Errorf("an escape sequence reached the terminal: %q", rendered)
	}
}

// A field is one label and one value, so foreign text may not split it into a
// second line that could impersonate ops-pilot's own output.
func assertFieldsStayOnOneLine(t *testing.T, rendered string) {
	t.Helper()
	if !strings.Contains(rendered, "all clear merged successfully") {
		t.Errorf("foreign text broke out of its line: %q", rendered)
	}
}

// The agent reads pod logs and repository files and the controllers quote
// Secret payloads, so every prose field in a prompt can carry a credential that
// ops-pilot was never configured with and the redactor therefore cannot know.
func TestPromptsScrubCredentialsOutOfEveryForeignField(t *testing.T) {
	const secret = "hunter2correcthorse"
	quoted := "the pod logged password: " + secret + " before exiting"
	tests := []struct {
		name string
		ask  func(*Approver) error
	}{
		{
			name: "revert heading dependency name",
			ask: func(a *Approver) error {
				revert := testRevert()
				revert.Dependency.Name = "https://user:" + secret + "@registry.example.com/pkg"
				_, err := a.ConfirmRevert(revert)
				return err
			},
		},
		{
			name: "revert cause",
			ask: func(a *Approver) error {
				revert := testRevert()
				revert.Cause = quoted
				_, err := a.ConfirmRevert(revert)
				return err
			},
		},
		{
			name: "broken object reason",
			ask: func(a *Approver) error {
				revert := testRevert()
				revert.Broken[0].Reason = quoted
				_, err := a.ConfirmRevert(revert)
				return err
			},
		},
		{
			name: "diagnosis cause",
			ask: func(a *Approver) error {
				_, err := a.ApproveFix(
					domain.PullRequest{Number: 42},
					domain.Diagnosis{Cause: quoted, Diff: "-  image: a\n"},
				)
				return err
			},
		},
		{
			name: "fix diff",
			ask: func(a *Approver) error {
				_, err := a.ApproveFix(
					domain.PullRequest{Number: 42},
					domain.Diagnosis{
						Cause: "the secret is wrong",
						Diff:  "--- a/app.yaml\n+++ b/app.yaml\n-  password: old\n+  password: " + secret + "\n",
					},
				)
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			approver, out := attendedApprover(t, "n\n")
			if err := test.ask(approver); err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if strings.Contains(out.String(), secret) {
				t.Errorf("a credential reached the operator's terminal:\n%s", out.String())
			}
			if !strings.Contains(out.String(), "[REDACTED]") {
				t.Errorf("nothing was redacted, so the field was not scrubbed at all:\n%s", out.String())
			}
		})
	}
}

// The diff is the one field that must survive the scrub as lines, and an image
// reference is byte-shape-identical to a key assignment apart from the path
// separator that keeps it out of the scrub's reach.
func TestTheFixPromptScrubsTheDiffWithoutLosingItsLinesOrItsImageReferences(t *testing.T) {
	diagnosis := domain.Diagnosis{
		Cause: "the registry credentials leaked into the manifest",
		Diff: "--- a/app.yaml\n+++ b/app.yaml\n" +
			"-  image: ghcr.io/org/secret-key:1.4.1\n" +
			"+  image: ghcr.io/org/secret-key:1.4.2\n" +
			"+  password: hunter2correcthorse\n",
	}
	approver, out := attendedApprover(t, "n\n")
	if _, err := approver.ApproveFix(domain.PullRequest{Number: 42}, diagnosis); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, want := range []string{
		"    --- a/app.yaml",
		"    +++ b/app.yaml",
		"    -  image: ghcr.io/org/secret-key:1.4.1",
		"    +  image: ghcr.io/org/secret-key:1.4.2",
		"    +  password: [REDACTED]",
	} {
		if !strings.Contains(out.String(), want+"\n") {
			t.Errorf("the diff line %q is not rendered on its own line:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "hunter2correcthorse") {
		t.Errorf("the diff published a credential:\n%s", out.String())
	}
}

// Piped stdin is a deliberate script, so its lines answer the prompts in
// order. The type-ahead guard exists only at a real terminal, where a stale
// line means an operator who never saw the question; that side is pinned in
// approver_tty_test.go.
func TestAPipedScriptAnswersPromptsInOrder(t *testing.T) {
	tests := []struct {
		name  string
		typed string
		want  run.RevertChoice
	}{
		{name: "yes reverts", typed: "y\n", want: run.RevertNow},
		{name: "enter reverts", typed: "\n", want: run.RevertNow},
		{name: "wait defers", typed: "w\n", want: run.RevertWait},
		{name: "no keeps", typed: "n\n", want: run.RevertKeep},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			approver, _ := testApprover(t, strings.NewReader(test.typed), true)
			choice, err := approver.ConfirmRevert(testRevert())
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if choice != test.want {
				t.Errorf("choice = %q, want %q", choice, test.want)
			}
		})
	}
}

// A reader that is not a terminal must never receive a flush: the discard hook
// stays inert so a script's unread lines survive between questions.
func TestAPipedReaderIsNeverFlushed(t *testing.T) {
	approver, _ := testApprover(t, strings.NewReader("y\n"), true)
	approver.discard()
	got, err := approver.confirm("  merge anyway?")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !got {
		t.Errorf("confirm = false: the scripted answer was discarded")
	}
}

func TestTheWaitOptionNamesItsLengthOrSaysItCannot(t *testing.T) {
	tests := []struct {
		name   string
		window time.Duration
		want   string
	}{
		{name: "rounded", window: 90*time.Second + 400*time.Millisecond, want: "1m30s"},
		{name: "whole minutes", window: 10 * time.Minute, want: "10m"},
		{name: "zero", window: 0, want: "another window"},
		{name: "negative", window: -time.Minute, want: "another window"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := window(test.window); got != test.want {
				t.Errorf("window(%s) = %q, want %q", test.window, got, test.want)
			}
		})
	}
}
