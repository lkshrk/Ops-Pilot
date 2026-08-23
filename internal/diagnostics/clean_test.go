package diagnostics_test

import (
	"strings"
	"testing"

	"github.com/lkshrk/ops-pilot/internal/diagnostics"
)

// The carriers that heal a split once the text is normalised: zero-width spaces
// and joiners, the BOM, a soft hyphen, a bidi override, a private-use rune, and a
// bare carriage return. Held as code points because a literal one is invisible to
// review and staticcheck rejects several outright.
var healingCarriers = []struct {
	name    string
	carrier rune
}{
	{"zero width space", 0x200B},
	{"zero width non joiner", 0x200C},
	{"zero width joiner", 0x200D},
	{"word joiner", 0x2060},
	{"byte order mark", 0xFEFF},
	{"soft hyphen", 0x00AD},
	{"right to left override", 0x202E},
	{"private use", 0xE000},
	{"carriage return", 0x000D},
}

func splitBy(carrier rune, head, tail string) string {
	return head + string(carrier) + tail
}

// The base order the at-rest sinks shipped: the needles run on the un-normalised
// text and miss a split, then the scrub normalises and heals it whole.
func redactThenScrub(text string, r *diagnostics.Redactor) string {
	return diagnostics.Storable(diagnostics.ScrubSecrets(r.Redact(text)))
}

// The order 8beed14 tried: normalise, then the needles, then the shapes. It heals
// the split but a configured value that is a key token eats the key and blinds
// the shape rules to the value beside it.
func normaliseThenRedactThenScrub(text string, r *diagnostics.Redactor) string {
	return diagnostics.ScrubSecrets(r.Redact(diagnostics.Storable(text)))
}

// The order the terminal and commit sinks shipped: scrub the shapes, then the
// needles. A configured value whose head is credential-shaped loses its head to a
// shape rule, and the replacer can no longer match the whole to remove the tail.
func scrubThenRedact(text string, r *diagnostics.Redactor) string {
	return r.Redact(diagnostics.ScrubSecrets(text))
}

// Class 1 — heal. A configured value split by an invisible rune leaks at the
// redact-then-scrub sink because the needle misses the split and the scrub then
// heals it; Clean normalises before the needle and closes it, for every carrier.
func TestCleanClosesTheSplitHealOfAConfiguredValue(t *testing.T) {
	const configured = "correcthorsebatterystaple"

	for _, c := range healingCarriers {
		t.Run(c.name, func(t *testing.T) {
			text := "the pod printed " + splitBy(c.carrier, "correcthorse", "batterystaple")
			r := diagnostics.NewRedactor([]string{configured})

			if base := redactThenScrub(text, r); !strings.Contains(base, configured) {
				t.Fatalf("the base order did not heal the split; the fixture no longer reproduces the leak: %q", base)
			}
			if tip := diagnostics.Clean(text, r); strings.Contains(tip, configured) {
				t.Errorf("Clean healed the split whole: %q", tip)
			}
		})
	}
}

// Class 2 — key blinding. A configured value that is a key token eats its own key
// once the text is normalised, after which the shape rules no longer see a
// key-value pair and write the workload's credential out whole; Clean collects
// both span sets over one normalisation so neither pass destroys the other's
// evidence, for every carrier that splits the key.
func TestCleanDoesNotLetAKeyTokenBlindTheShapeRules(t *testing.T) {
	const workload = "correcthorsebatterystaple"

	for _, c := range healingCarriers {
		t.Run(c.name, func(t *testing.T) {
			text := splitBy(c.carrier, "pass", "word") + ": |\n  " + workload
			r := diagnostics.NewRedactor([]string{"password"})

			if base := normaliseThenRedactThenScrub(text, r); !strings.Contains(base, workload) {
				t.Fatalf("the normalise-first order did not blind the shape rules; the fixture no longer reproduces the leak: %q", base)
			}
			if tip := diagnostics.Clean(text, r); strings.Contains(tip, workload) {
				t.Errorf("Clean let the key token blind the shape rules: %q", tip)
			}
		})
	}
}

// Class 3 — tail stranding. A configured value whose head is credential-shaped
// loses its head to a shape rule at a scrub-first sink, stranding its tail; Clean
// unions the value's whole span with the shape span and redacts both.
func TestCleanRedactsAShapeHeadedConfiguredValueWhole(t *testing.T) {
	const (
		secret = "AKIAIOSFODNN7EXAMPLEtailofthesecret"
		tail   = "tailofthesecret"
	)
	text := "the pod printed " + secret + " and died"
	r := diagnostics.NewRedactor([]string{secret})

	if base := scrubThenRedact(text, r); !strings.Contains(base, tail) {
		t.Fatalf("the scrub-first order did not strand the tail; the fixture no longer reproduces the leak: %q", base)
	}
	tip := diagnostics.Clean(text, r)
	if strings.Contains(tip, tail) {
		t.Errorf("Clean stranded the tail: %q", tip)
	}
	if strings.Contains(tip, secret) {
		t.Errorf("Clean left the configured value whole: %q", tip)
	}
}

// The C-L130 closures, at the Clean entry point: a shape credential ops-pilot
// never held is scrubbed when a rune splits it, and a configured value carrying
// such a rune is matched in both the form configured and the form rendered.
func TestCleanHoldsTheInvisibleRuneClosures(t *testing.T) {
	carrying := splitBy(0x200B, "hunter2", "correcthorse")
	shapeSplit := splitBy(0x200B, "AKIA", "IOSFODNN7EXAMPLE")

	tests := []struct {
		name       string
		configured []string
		text       string
		forbidden  string
	}{
		{"a shape credential split by a rune", []string{"unrelated"}, "the pod printed " + shapeSplit, "AKIAIOSFODNN7EXAMPLE"},
		{"a configured value carrying a rune, quoted verbatim", []string{carrying}, "the pod printed " + carrying, "hunter2correcthorse"},
		{"a configured value carrying a rune, quoted rendered", []string{carrying}, "the pod printed hunter2correcthorse", "hunter2correcthorse"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			r := diagnostics.NewRedactor(test.configured)
			if got := diagnostics.Clean(test.text, r); strings.Contains(got, test.forbidden) {
				t.Errorf("Clean kept %q: %q", test.forbidden, got)
			}
		})
	}
}

// The union over-redacts a configured value that is an ordinary word beside a
// shaped one, which is the safe direction and the shape 4cf4f67 pinned for the
// scrub-first order.
func TestCleanOverRedactsAWordValueBesideAShapedOne(t *testing.T) {
	r := diagnostics.NewRedactor([]string{"password"})
	got := diagnostics.Clean("password: |\n  correcthorsebatterystaple", r)
	if want := "[REDACTED]: |\n  [REDACTED]"; got != want {
		t.Errorf("Clean = %q, want %q", got, want)
	}
}

// Text carrying no secret survives Clean unchanged but for the normalisation the
// sinks apply anyway, so the entry point cannot silently erase a diagnosis.
func TestCleanChangesNothingWhenNoSecretMatches(t *testing.T) {
	r := diagnostics.NewRedactor([]string{"a-configured-value"})
	clean := []string{
		"HelmRelease media/sonarr failed: upgrade retries exhausted",
		"the object namespace/Kind/name did not become ready in time",
		"waiting for 3 of 5 replicas; last transition 2m ago",
		"error: could not resolve reference refs/heads/main",
		"diff --git a/app.yaml b/app.yaml\n@@ -1,3 +1,3 @@\n-image: app:1.0\n+image: app:1.1",
	}
	for _, text := range clean {
		if got := diagnostics.Clean(text, r); got != diagnostics.Storable(text) {
			t.Errorf("Clean altered secret-free text\n in:  %q\n got: %q", text, got)
		}
	}
}

// mergeMarkers collapses a run of adjacent redaction markers to one. The
// length-preserving mask fuses two abutting spans that the single-mark scrub
// prints as two markers, so the two are compared with adjacent markers merged.
func mergeMarkers(s string) string {
	const marker = "[REDACTED]"
	for strings.Contains(s, marker+marker) {
		s = strings.ReplaceAll(s, marker+marker, marker)
	}
	return s
}

// Clean with no configured values must redact exactly the spans ScrubSecrets
// does: the length-preserving passes that build the mask must select the same
// bytes as the single-mark passes the string API runs.
func TestCleanShapeRedactionMatchesScrubSecrets(t *testing.T) {
	empty := diagnostics.NewRedactor(nil)
	corpus := []string{
		"api_key=Ai8fkq2LmZx0Rt7Yb3Nc in the log",
		"postgres://app:hunter2correcthorse@db:5432/app",
		"authorization: Bearer abcdefghijklmnop0123456789",
		"password: |\n  s3cretVALUEhere0000\nother: keep",
		"tokens:\n  - firsttoken1234\n  - secondtoken5678\nnext: value",
		"-----BEGIN PRIVATE KEY-----\nMIIBODYbytes0000\n-----END PRIVATE KEY-----",
		"nothing secret here at all, just prose about a HelmRelease",
		"cookie: session=abcd1234efgh5678; path=/",
	}
	for _, text := range corpus {
		want := mergeMarkers(diagnostics.ScrubSecrets(text))
		if got := diagnostics.Clean(text, empty); got != want {
			t.Errorf("Clean shape redaction diverged\n in:   %q\n got:  %q\n want: %q", text, got, want)
		}
	}
}

func FuzzCleanShapeRedactionMatchesScrubSecrets(f *testing.F) {
	seeds := []string{
		"api_key=Ai8fkq2LmZx0Rt7Yb3Nc",
		"password: |\n  s3cretVALUEhere0000",
		"postgres://app:hunter2correcthorse@db:5432/app",
		"tokens:\n  - firsttoken1234\n  - secondtoken5678",
		"plain prose with no secret",
		"authorization: Bearer abcdefghijklmnop0123456789",
		"a\x00b\u200bc key: value1234",
	}
	for _, s := range seeds {
		f.Add(s)
	}
	empty := diagnostics.NewRedactor(nil)
	f.Fuzz(func(t *testing.T, text string) {
		// The literal marker in the input defeats the merge-based comparison, not
		// Clean itself; that hazard is its own concern.
		if strings.Contains(text, "[REDACTED]") {
			t.Skip()
		}
		want := mergeMarkers(diagnostics.ScrubSecrets(text))
		if got := diagnostics.Clean(text, empty); got != want {
			t.Errorf("diverged\n in:   %q\n got:  %q\n want: %q", text, got, want)
		}
	})
}
