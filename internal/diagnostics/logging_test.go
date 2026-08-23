package diagnostics

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"
)

var hostileRunes = []struct {
	name string
	rune rune
}{
	{"a bidi right-to-left override", 0x202E},
	{"a bidi left-to-right override", 0x202D},
	{"a zero width space", 0x200B},
	{"a zero width joiner", 0x200D},
	{"a line separator", 0x2028},
	{"a paragraph separator", 0x2029},
	{"the replacement character", utf8.RuneError},
	{"a private use character", 0xE000},
}

// The log and the rendered error are the two lines an operator always reads, so
// nothing that hides or reorders text may reach them.
func TestTheOperatorsLogDropsRunesThatHideOrReorderText(t *testing.T) {
	t.Parallel()

	for _, test := range hostileRunes {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			text := "registry" + string(test.rune) + "said"

			var output bytes.Buffer
			NewLeveledLogger(&output, nil, LevelWarn, nil).Warnf("%s", text)
			logged := output.String()
			if strings.ContainsRune(logged, test.rune) {
				t.Fatalf("%s survived to the log: %q", test.name, logged)
			}
			if !strings.Contains(logged, "registry") || !strings.Contains(logged, "said") {
				t.Fatalf("the text itself must survive, only the hostile rune removed: %q", logged)
			}

			rendered := RenderError(errors.New(text), nil)
			if strings.ContainsRune(rendered, test.rune) {
				t.Fatalf("%s survived to the rendered error: %q", test.name, rendered)
			}
		})
	}
}

// The runes terminalSafe and Storable are allowed to disagree on, because one
// renders to a terminal and the other keeps a stored diff readable.
var renderOnlyFolds = map[rune]struct{}{
	'\n': {}, '\t': {}, 0x2028: {}, 0x2029: {},
}

// A line separator is line structure, and a stored reason keeps its structure
// for the same reason it keeps \n: it is often a diff. Dropping it at rest would
// also glue the words either side of it together in the record itself, where
// nothing later can tell that a break was there. Every path that renders the
// text folds it to a space instead, which is where the exposure actually ends.
func TestALineSeparatorIsKeptAtRestAndFoldedWhenRendered(t *testing.T) {
	t.Parallel()

	for _, r := range []rune{0x2028, 0x2029} {
		text := "one" + string(r) + "two"

		if got := Storable(text); got != text {
			t.Errorf("%U was not kept at rest: got %q, want %q", r, got, text)
		}
		if got := terminalSafe(text); got != "one two" {
			t.Errorf("%U was not folded to a space when rendered: got %q", r, got)
		}
	}
}

// A rune deleted on the way to the terminal closes the gap it left behind, so a
// credential the shape rules declined to match because something sat in the
// middle of it arrives whole, and greppable, in front of a human.
func TestASplitCredentialIsNotHealedIntoTheOperatorsLog(t *testing.T) {
	t.Parallel()

	splitters := []struct {
		name string
		rune rune
	}{
		{"a zero width space", 0x200B},
		{"a zero width non-joiner", 0x200C},
		{"a zero width joiner", 0x200D},
		{"a word joiner", 0x2060},
		{"a byte order mark", 0xFEFF},
		{"a soft hyphen", 0x00AD},
		{"a bidi right-to-left override", 0x202E},
		{"a private use character", 0xE000},
		{"a carriage return", '\r'},
		{"a newline", '\n'},
	}
	credentials := []struct {
		name  string
		left  string
		right string
		leak  string
	}{
		{"an aws access key", "AKIA", "IOSFODNN7EXAMPLE", "AKIAIOSFODNN7EXAMPLE"},
		{"a github token", "ghp_", "abcdefghijklmnop0123", "ghp_abcdefghijklmnop0123"},
		{"a gitlab token", "glpat-", "abcdefghijklmnop0123", "glpat-abcdefghijklmnop0123"},
		{"a named password", "password", ": hunter2secret", "hunter2secret"},
		{"a bearer authorization header", "authorization: bearer ", "abcdefghijklmnop0123", "abcdefghijklmnop0123"},
	}

	for _, credential := range credentials {
		for _, splitter := range splitters {
			t.Run(credential.name+" split by "+splitter.name, func(t *testing.T) {
				t.Parallel()

				text := credential.left + string(splitter.rune) + credential.right

				var output bytes.Buffer
				NewLeveledLogger(&output, nil, LevelWarn, nil).Warnf("%s", text)
				if logged := output.String(); strings.Contains(logged, credential.leak) {
					t.Errorf("the split healed into a greppable credential in the log: %q", logged)
				}
				if rendered := RenderError(errors.New(text), nil); strings.Contains(rendered, credential.leak) {
					t.Errorf("the split healed into a greppable credential in the rendered error: %q", rendered)
				}
			})
		}
	}
}

// Redact matches a configured value exactly, so it too was blind to a value an
// author had split; running it after the rules is what now gives it the whole one.
func TestASplitConfiguredSecretIsStillRedacted(t *testing.T) {
	t.Parallel()

	const secret = "ghs_configuredtokenvalue0123"

	for _, splitter := range []rune{0x200B, 0x202E, 0xFEFF, '\r'} {
		text := "registry rejected " + secret[:8] + string(splitter) + secret[8:]

		var output bytes.Buffer
		NewLeveledLogger(&output, NewRedactor([]string{secret}), LevelWarn, nil).Warnf("%s", text)
		if logged := output.String(); strings.Contains(logged, secret) {
			t.Errorf("%U healed a configured secret into the log: %q", splitter, logged)
		}
	}
}

// An operator pastes a registry password out of rendered HTML and it carries a
// soft hyphen. The rules normalise the line before Redact sees it, so the value
// configured is no longer the value present, and an exact match alone finds
// nothing at the one sink a human reads.
func TestAConfiguredSecretCarryingAnInvisibleRuneIsStillRedacted(t *testing.T) {
	t.Parallel()

	const visible = "hunter2registrypass"

	for _, carrier := range []struct {
		name string
		rune rune
	}{
		{"a zero width space", 0x200B},
		{"a byte order mark", 0xFEFF},
		{"a soft hyphen", 0x00AD},
		{"a carriage return", '\r'},
		{"a bidi right-to-left override", 0x202E},
	} {
		t.Run(carrier.name, func(t *testing.T) {
			t.Parallel()

			configured := visible[:7] + string(carrier.rune) + visible[7:]
			text := "registry rejected the credential " + configured + " for ghcr.io"

			var output bytes.Buffer
			NewLeveledLogger(&output, NewRedactor([]string{configured}), LevelWarn, nil).Warnf("%s", text)

			logged := output.String()
			if strings.Contains(logged, visible) {
				t.Errorf("the configured secret reached the log whole: %q", logged)
			}
			if !strings.Contains(logged, redactedValue) {
				t.Errorf("the configured secret was not redacted at all: %q", logged)
			}
		})
	}
}

// The rendered form is known beside the configured one, never in place of it:
// the sinks that redact before they scrub still meet the value as configured.
func TestARedactorKnowsBothTheConfiguredAndTheRenderedFormOfASecret(t *testing.T) {
	t.Parallel()

	const visible = "hunter2registrypass"
	configured := visible[:7] + string(rune(0x200B)) + visible[7:]
	redactor := NewRedactor([]string{configured})

	if got := redactor.Redact("registry rejected " + configured); got != "registry rejected "+redactedValue {
		t.Errorf("the configured form was traded away for the rendered one: %q", got)
	}
	if got := redactor.Redact("registry rejected " + visible); got != "registry rejected "+redactedValue {
		t.Errorf("the rendered form is not known to the redactor: %q", got)
	}
}

// A value built only from runes no reader can see renders to nothing, and an
// empty pattern matches at every position: it would redact whole lines that
// hold no secret at all.
func TestASecretThatRendersToNothingContributesNoPattern(t *testing.T) {
	t.Parallel()

	const innocent = "reconciliation succeeded for podinfo"
	invisible := string(rune(0x200B)) + string(rune(0xFEFF))

	if got := NewRedactor([]string{invisible}).Redact(innocent); got != innocent {
		t.Errorf("an empty pattern reached the replacer: %q", got)
	}
}

// The credential rules must see the text the operator will see. Any rune that
// survives them only to be deleted further down reopens the healing above, so
// what the log drops has to be gone before the rules run, not after.
func TestEveryRuneTheLogDeletesIsAlreadyGoneBeforeTheCredentialRulesRun(t *testing.T) {
	t.Parallel()

	for r := rune(0); r <= unicode.MaxRune; r++ {
		value := string(r)
		if terminalSafe(value) != "" {
			continue
		}
		if got := ScrubSecrets(value); got != "" {
			t.Fatalf("%U survives the credential rules as %q but the log deletes it", r, got)
		}
	}
}

// One rule, two output policies: a rune either carries meaning a reader can see
// or it does not, and the log may not be laxer about that than the database.
func TestTerminalSafeAndStorableClassifyEveryRuneAlike(t *testing.T) {
	t.Parallel()

	for r := rune(0); r <= unicode.MaxRune; r++ {
		if _, folded := renderOnlyFolds[r]; folded {
			continue
		}
		value := string(r)
		if strings.ContainsRune(terminalSafe(value), r) != strings.ContainsRune(Storable(value), r) {
			t.Fatalf("the log and the stored value disagree about %U: log %q, stored %q",
				r, terminalSafe(value), Storable(value))
		}
	}
}
