package diagnostics

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Storable prepares untrusted prose to be kept and printed back later: by the
// history database, by the summary table, by `jq -r` over the event stream. Its
// rule is the house rule — a rune that shows a reader nothing but can hide or
// reorder what stands beside it does not survive. Newlines and tabs do, because
// a stored reason is often a diff and is unreadable without them.
func Storable(value string) string {
	var out strings.Builder
	out.Grow(len(value))
	for _, r := range value {
		switch {
		case r == '\n' || r == '\t':
			out.WriteRune(r)
		case isUnsafe(r):
			continue
		default:
			out.WriteRune(r)
		}
	}
	return out.String()
}

func isUnsafe(r rune) bool {
	return r == utf8.RuneError || unicode.IsControl(r) ||
		unicode.In(r, unicode.Cf, unicode.Co, unicode.Cs)
}

// The line breaks that are neither control nor formatting, so both tests miss them.
func isLineSeparator(r rune) bool { return unicode.In(r, unicode.Zl, unicode.Zp) }
