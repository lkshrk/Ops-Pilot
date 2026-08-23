// Package display prepares untrusted text for a terminal.
//
// Almost everything ops-pilot prints comes from somewhere it does not control:
// pull request titles written by bots, prose written by a language model,
// annotations from container registries, status messages from Kubernetes. That
// text must not be able to move the operator's cursor, erase what they have
// already read, or break the alignment of a table.
package display

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/width"
)

// Safe removes anything that would let text control the terminal rather than
// appear in it. Tabs become spaces because their width is unknowable; newlines
// are dropped so one field cannot become two lines.
func Safe(value string) string {
	var out strings.Builder
	out.Grow(len(value))
	for _, r := range value {
		switch {
		case r == '\t' || r == '\n' || r == '\r' || isLineSeparator(r):
			out.WriteByte(' ')
		case r == utf8.RuneError, unicode.IsControl(r), isFormatting(r):
			continue
		default:
			out.WriteRune(r)
		}
	}
	return out.String()
}

// The line breaks that are neither control nor formatting, so both tests miss them.
func isLineSeparator(r rune) bool { return unicode.In(r, unicode.Zl, unicode.Zp) }

// isFormatting covers the invisible characters that reorder or hide text:
// bidirectional overrides, zero-width joiners, and the like.
func isFormatting(r rune) bool {
	return unicode.In(r, unicode.Cf, unicode.Co, unicode.Cs)
}

// Width is how many terminal columns a string occupies. East Asian wide and
// fullwidth characters take two; combining marks take none.
func Width(value string) int {
	columns := 0
	for _, r := range value {
		columns += runeWidth(r)
	}
	return columns
}

func runeWidth(r rune) int {
	if unicode.In(r, unicode.Mn, unicode.Me) {
		return 0
	}
	switch width.LookupRune(r).Kind() {
	case width.EastAsianWide, width.EastAsianFullwidth:
		return 2
	default:
		return 1
	}
}

// Clip shortens text to at most columns wide, at a word boundary where one is
// close enough, marking that something was removed. It never splits a rune.
func Clip(value string, columns int) string {
	value = Collapse(value)
	if columns <= 1 || Width(value) <= columns {
		return value
	}
	var (
		out   strings.Builder
		used  int
		space = -1
	)
	for _, r := range value {
		w := runeWidth(r)
		if used+w > columns-1 {
			break
		}
		if r == ' ' {
			space = out.Len()
		}
		out.WriteRune(r)
		used += w
	}
	text := out.String()
	if space > 0 && Width(text[:space])*2 > columns {
		text = text[:space]
	}
	return strings.TrimRight(text, " .,;:") + "…"
}

// Wrap breaks text into lines no wider than columns, at word boundaries. A word
// longer than the limit is left whole rather than split mid-identifier, because
// a broken image reference or URL is worse than a long line.
//
// Scripts that do not use spaces are the exception: without a break they would
// be one unbreakable token and never wrap at all, so those are split on
// character boundaries.
func Wrap(value string, columns int) []string {
	if columns < 8 {
		columns = 8
	}
	var (
		lines []string
		line  strings.Builder
		used  int
	)
	for _, word := range breakLongTokens(strings.Fields(Collapse(value)), columns) {
		w := Width(word)
		switch {
		case line.Len() == 0:
			line.WriteString(word)
			used = w
		case used+1+w <= columns:
			line.WriteByte(' ')
			line.WriteString(word)
			used += 1 + w
		default:
			lines = append(lines, line.String())
			line.Reset()
			line.WriteString(word)
			used = w
		}
	}
	if line.Len() > 0 {
		lines = append(lines, line.String())
	}
	return lines
}

// Pad right-pads to a column width, for table alignment.
func Pad(value string, columns int) string {
	if missing := columns - Width(value); missing > 0 {
		return value + strings.Repeat(" ", missing)
	}
	return value
}

// Collapse reduces any run of whitespace to a single space.
func Collapse(value string) string { return strings.Join(strings.Fields(value), " ") }

// ShortVersion renders a version compactly: a bare digest is unreadable in
// full, and a tag@digest pair only needs its tag.
func ShortVersion(version string) string {
	if version == "" {
		return "unknown"
	}
	if tag, _, found := strings.Cut(version, "@"); found && tag != "" {
		return tag
	}
	if _, encoded, found := strings.Cut(version, ":"); found && len(encoded) > 12 {
		return encoded[:12]
	}
	if len(version) > 40 {
		return version[:12]
	}
	return version
}

// breakLongTokens splits a token that exceeds the line and carries no spaces to
// break on, which is how CJK prose arrives. A long identifier stays whole: it
// is meant to be copied, and a break would ruin it.
func breakLongTokens(words []string, columns int) []string {
	var out []string
	for _, word := range words {
		if Width(word) <= columns || !unspaced(word) {
			out = append(out, word)
			continue
		}
		var piece strings.Builder
		used := 0
		for _, r := range word {
			w := runeWidth(r)
			if used+w > columns {
				out = append(out, piece.String())
				piece.Reset()
				used = 0
			}
			piece.WriteRune(r)
			used += w
		}
		if piece.Len() > 0 {
			out = append(out, piece.String())
		}
	}
	return out
}

// unspaced reports text written in a script that does not separate words, where
// every rune is a legitimate break point.
func unspaced(word string) bool {
	for _, r := range word {
		if runeWidth(r) < 2 {
			return false
		}
	}
	return true
}
