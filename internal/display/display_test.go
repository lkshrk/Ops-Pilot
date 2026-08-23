package display_test

import (
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/lkshrk/ops-pilot/internal/diagnostics"
	"github.com/lkshrk/ops-pilot/internal/display"
)

// Text ops-pilot prints comes from pull request titles, language models,
// registries and Kubernetes. None of it may drive the terminal.
func TestSafeRemovesTerminalControl(t *testing.T) {
	cases := map[string]string{
		"erase and rewrite": "safe\x1b[2K\rIMPOSTOR",
		"cursor movement":   "a\x1b[1;1Hb",
		"carriage return":   "first\rsecond",
		"newline":           "one\ntwo",
		"bell":              "wake\aup",
		"bidi override":     "report\u202etxt.exe",
		"zero width joiner": "a\u200db",
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			got := display.Safe(input)
			for _, r := range got {
				if r != ' ' && (r < 0x20 || r == 0x7f) {
					t.Fatalf("control character survived: %q", got)
				}
			}
			if strings.ContainsRune(got, 0x1b) {
				t.Fatalf("escape survived: %q", got)
			}
		})
	}
}

// U+2028 and U+2029 break a line without being control characters, so neither
// the control test nor the formatting test sees them.
func TestSafeFoldsTheLineSeparatorsThatAreNotControlCharacters(t *testing.T) {
	cases := map[string]string{
		"line separator":      "one" + string(rune(0x2028)) + "two",
		"paragraph separator": "one" + string(rune(0x2029)) + "two",
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			got := display.Safe(input)
			if strings.ContainsAny(got, string(rune(0x2028))+string(rune(0x2029))) {
				t.Fatalf("a line separator survived: %q", got)
			}
			if got != "one two" {
				t.Fatalf("got %q, want the break folded to a space, not the words joined", got)
			}
		})
	}
}

// Collapse already reads a line separator as whitespace, so the wrapper and
// Safe must not disagree about where the word boundary is.
func TestWrapTreatsALineSeparatorAsAWordBoundary(t *testing.T) {
	lines := display.Wrap("alpha"+string(rune(0x2028))+"beta", 40)
	if len(lines) != 1 || lines[0] != "alpha beta" {
		t.Fatalf("got %v, want one line reading %q", lines, "alpha beta")
	}
}

// display keeps its own rule so the package every printed string passes through
// depends on nothing. That freedom is only safe while the two rules classify
// runes alike: the terminal may not show a rune the database refused to keep.
func TestSafeAndTheStoredRuleClassifyEveryRuneAlike(t *testing.T) {
	folded := map[rune]struct{}{
		'\n': {}, '\t': {}, '\r': {}, 0x2028: {}, 0x2029: {},
	}
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if _, skip := folded[r]; skip {
			continue
		}
		value := string(r)
		if strings.ContainsRune(display.Safe(value), r) != strings.ContainsRune(diagnostics.Storable(value), r) {
			t.Fatalf("the terminal and the stored value disagree about %U: shown %q, stored %q",
				r, display.Safe(value), diagnostics.Storable(value))
		}
	}
}

func TestSafeKeepsOrdinaryText(t *testing.T) {
	const text = "values.env was renamed to values.envs in 0.4.0 (see #1204)"
	if got := display.Safe(text); got != text {
		t.Fatalf("legitimate text was altered:\n got %q\nwant %q", got, text)
	}
}

func TestWidthCountsTerminalColumnsNotBytes(t *testing.T) {
	cases := map[string]int{
		"abc":  3,
		"日本語":  6, // three East Asian wide runes
		"café": 4,
		"":     0,
		"á":   1, // a plus a combining acute occupies one column
	}
	for input, want := range cases {
		if got := display.Width(input); got != want {
			t.Errorf("Width(%q) = %d, want %d", input, got, want)
		}
	}
}

// Slicing by byte offset splits a rune and emits invalid UTF-8.
func TestClipNeverSplitsARune(t *testing.T) {
	got := display.Clip(strings.Repeat("日", 20), 9)
	if !utf8.ValidString(got) {
		t.Fatalf("clip produced invalid UTF-8: %q", got)
	}
	if display.Width(got) > 9 {
		t.Fatalf("clip exceeded its width: %q is %d columns", got, display.Width(got))
	}
}

func TestClipLeavesShortTextAlone(t *testing.T) {
	const text = "already short"
	if got := display.Clip(text, 40); got != text {
		t.Fatalf("got %q, want it unchanged", got)
	}
}

func TestClipMarksThatSomethingWasRemoved(t *testing.T) {
	got := display.Clip("the quick brown fox jumps over the lazy dog", 20)
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("truncation must be visible, got %q", got)
	}
	if display.Width(got) > 20 {
		t.Fatalf("%q is %d columns, want at most 20", got, display.Width(got))
	}
}

func TestPadAlignsMultiByteText(t *testing.T) {
	wide, plain := display.Pad("日本語", 10), display.Pad("abc", 10)
	if display.Width(wide) != display.Width(plain) {
		t.Fatalf("padding disagrees: %q is %d columns, %q is %d",
			wide, display.Width(wide), plain, display.Width(plain))
	}
}

func TestWrapBreaksAtWordBoundaries(t *testing.T) {
	lines := display.Wrap("the quick brown fox jumps over the lazy dog", 15)
	for _, line := range lines {
		if display.Width(line) > 15 {
			t.Fatalf("line exceeds the width: %q", line)
		}
	}
	if strings.Join(lines, " ") != "the quick brown fox jumps over the lazy dog" {
		t.Fatalf("wrapping lost or reordered words: %v", lines)
	}
}

// Breaking an image reference across lines makes it uncopyable, which is worse
// than one long line.
func TestWrapKeepsAnOverlongWordWhole(t *testing.T) {
	const reference = "ghcr.io/home-operations/sonarr@sha256:0123456789abcdef"
	lines := display.Wrap("see "+reference, 20)
	if !strings.Contains(strings.Join(lines, "\n"), reference) {
		t.Fatalf("the reference was split: %v", lines)
	}
}

// Scripts without spaces are one token to a word-boundary wrapper, so prose in
// them would never wrap and would run off the terminal.
func TestWrapBreaksTextThatHasNoSpacesToBreakOn(t *testing.T) {
	lines := display.Wrap(strings.Repeat("日", 40), 20)
	if len(lines) < 2 {
		t.Fatalf("want the text broken across lines, got %d line(s)", len(lines))
	}
	for _, line := range lines {
		if display.Width(line) > 20 {
			t.Fatalf("line exceeds the width: %q is %d columns", line, display.Width(line))
		}
	}
	if strings.Join(lines, "") != strings.Repeat("日", 40) {
		t.Fatal("breaking must not lose or reorder characters")
	}
}

// A long identifier is meant to be copied; breaking it ruins it.
func TestWrapStillKeepsALongIdentifierWhole(t *testing.T) {
	const reference = "ghcr.io/home-operations/sonarr@sha256:0123456789abcdef0123456789abcdef"
	lines := display.Wrap(reference, 20)
	if len(lines) != 1 || lines[0] != reference {
		t.Fatalf("the reference was split: %v", lines)
	}
}
