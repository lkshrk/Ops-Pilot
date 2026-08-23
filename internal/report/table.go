// Package report renders the run summary and the history listing.
package report

import (
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/lkshrk/ops-pilot/internal/display"
)

type Table struct {
	header []string
	rows   [][]string
	paint  map[[2]int]func(string) string
	fixed  map[int]bool
}

func NewTable(header ...string) *Table { return &Table{header: header} }

// Fixed marks a column that must always render at its full natural width,
// because something downstream needs to read it whole rather than clipped -
// the run id a `history --run <id>` lookup depends on, say.
func (t *Table) Fixed(col int) {
	if t.fixed == nil {
		t.fixed = map[int]bool{}
	}
	t.fixed[col] = true
}

// Add records a row. Every cell is sanitised here rather than at each call
// site, because table content is dependency names, versions and AI prose.
func (t *Table) Add(cells ...string) {
	safe := make([]string, len(cells))
	for i, cell := range cells {
		safe[i] = display.Safe(cell)
	}
	t.rows = append(t.rows, safe)
}

// AddStyled records a row where some cells are coloured. The colour is applied
// after the padding is measured, because an escape sequence has no width and
// would otherwise shift every column after it.
func (t *Table) AddStyled(styled []int, paint func(string) string, cells ...string) {
	t.Add(cells...)
	row := len(t.rows) - 1
	if t.paint == nil {
		t.paint = map[[2]int]func(string) string{}
	}
	for _, column := range styled {
		t.paint[[2]int{row, column}] = paint
	}
}

const minColumn = 8

// fit shrinks widths to fit budget, leaving any column named in fixed at its
// natural width - a copy-pasteable id is useless truncated, so it is reserved
// before the rest water-fill what is left.
func fit(widths []int, fixed map[int]bool, budget int) {
	if len(widths) == 0 {
		return
	}
	gaps := 2 * (len(widths) - 1)
	total := gaps
	for _, width := range widths {
		total += width
	}
	// Must be <=: a row landing on exactly the budget fits and may not shrink.
	if total <= budget {
		return
	}
	shrinkable := make([]int, 0, len(widths))
	for i := range widths {
		if !fixed[i] {
			shrinkable = append(shrinkable, i)
		}
	}
	reserved := gaps
	for i, width := range widths {
		if !fixed[i] {
			continue
		}
		// Once the rest can no longer reach the floor, this column shrinks too.
		if reserved+width+2*len(shrinkable) <= budget {
			reserved += width
			continue
		}
		shrinkable = append(shrinkable, i)
	}
	if len(shrinkable) == 0 {
		return
	}
	remaining := budget - reserved
	// Floor stays in [2, minColumn] and within remaining's fair share, so Clip is never a no-op and no column overspends.
	floor := max(2, min(minColumn, remaining/len(shrinkable)))
	order := append([]int(nil), shrinkable...)
	slices.SortStableFunc(order, func(a, b int) int { return widths[a] - widths[b] })
	for rank, column := range order {
		widths[column] = min(widths[column], max(remaining/(len(order)-rank), floor))
		remaining -= widths[column]
	}
}

// Render writes the table left-aligned, two spaces between columns, sized to the destination. A table with no rows still writes its header so an empty result is legible.
func (t *Table) Render(out io.Writer) error {
	widths := make([]int, len(t.header))
	for i, cell := range t.header {
		widths[i] = display.Width(cell)
	}
	for _, row := range t.rows {
		for i, cell := range row {
			if i < len(widths) && display.Width(cell) > widths[i] {
				widths[i] = display.Width(cell)
			}
		}
	}
	fit(widths, t.fixed, display.NewStyle(out).Width)
	// Row -1 is the header; AddStyled only ever keys paint by a real row index.
	if err := t.writeStyledRow(out, -1, t.header, widths); err != nil {
		return err
	}
	for i, row := range t.rows {
		if err := t.writeStyledRow(out, i, row, widths); err != nil {
			return err
		}
	}
	return nil
}

// writeStyledRow pads every cell to its column width first, then paints, so the
// invisible escape codes never enter the width arithmetic.
func (t *Table) writeStyledRow(out io.Writer, row int, cells []string, widths []int) error {
	parts := make([]string, 0, len(cells))
	for i, cell := range cells {
		text := cell
		if i < len(widths) {
			if display.Width(text) > widths[i] {
				text = display.Clip(text, widths[i])
			}
			if i != len(cells)-1 {
				text = display.Pad(text, widths[i])
			}
		}
		if paint, styled := t.paint[[2]int{row, i}]; styled {
			text = paint(text)
		}
		parts = append(parts, text)
	}
	_, err := fmt.Fprintln(out, strings.TrimRight(strings.Join(parts, "  "), " "))
	return err
}
