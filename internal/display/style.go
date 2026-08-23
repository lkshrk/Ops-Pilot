package display

import (
	"io"
	"os"
	"strings"

	"golang.org/x/term"
)

// Style renders text for a terminal. Colour is applied only when the
// destination is a real terminal and the environment has not asked for plain
// output, so piped or redirected output stays clean.
//
// It lives here rather than in one command so the narrative, the summary table
// and the approval prompt share one palette and one detection rule instead of
// each inventing their own.
type Style struct {
	colour bool
	// Width is the usable text width of the destination.
	Width int
}

const (
	reset  = "\033[0m"
	bold   = "\033[1m"
	dim    = "\033[2m"
	red    = "\033[31m"
	green  = "\033[32m"
	yellow = "\033[33m"
	cyan   = "\033[36m"
)

// DefaultWidth is used when the destination is not a terminal, so redirected
// output is byte-identical run to run.
const DefaultWidth = 100

func NewStyle(out io.Writer) Style {
	style := Style{colour: colourable(out), Width: DefaultWidth}
	if file, ok := out.(*os.File); ok {
		if columns, _, err := term.GetSize(int(file.Fd())); err == nil && columns > 40 {
			style.Width = columns - 4
		}
	}
	return style
}

// Interactive reports whether the destination is a terminal a person is
// watching, which is the only place transient output makes sense.
func (s Style) Interactive() bool { return s.colour }

// colourable follows the informal NO_COLOR convention and the usual TERM=dumb
// signal before asking whether the destination is a terminal at all. NO_COLOR
// is honoured only when non-empty, as the convention specifies, and FORCE_COLOR
// covers CI systems that render ANSI without being a terminal.
func colourable(out io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	if os.Getenv("FORCE_COLOR") != "" {
		return true
	}
	file, ok := out.(*os.File)
	return ok && term.IsTerminal(int(file.Fd()))
}

func (s Style) paint(code, value string) string {
	if !s.colour || value == "" {
		return value
	}
	return code + value + reset
}

func (s Style) Heading(value string) string { return s.paint(bold, value) }
func (s Style) Title(value string) string   { return s.paint(bold+cyan, value) }
func (s Style) Accent(value string) string  { return s.paint(cyan, value) }
func (s Style) Detail(value string) string  { return s.paint(dim, value) }
func (s Style) Good(value string) string    { return s.paint(green, value) }
func (s Style) Warn(value string) string    { return s.paint(yellow, value) }
func (s Style) Bad(value string) string     { return s.paint(red, value) }

// Diff colours a unified diff the way git does, because an agent-written change
// to a production repository is the highest-stakes thing an operator reads here.
func (s Style) Diff(value string) string {
	var out strings.Builder
	for line := range strings.SplitSeq(strings.TrimRight(value, "\n"), "\n") {
		switch {
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
			out.WriteString(s.Detail(line))
		case strings.HasPrefix(line, "+"):
			out.WriteString(s.Good(line))
		case strings.HasPrefix(line, "-"):
			out.WriteString(s.Bad(line))
		case strings.HasPrefix(line, "@@"):
			out.WriteString(s.paint(bold, line))
		default:
			out.WriteString(line)
		}
		out.WriteByte('\n')
	}
	return out.String()
}
