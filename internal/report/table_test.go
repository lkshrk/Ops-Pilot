package report_test

import (
	"bytes"
	"strings"
	"testing"
	"unicode"

	"github.com/lkshrk/ops-pilot/internal/domain"
	"github.com/lkshrk/ops-pilot/internal/report"
)

var rightToLeftOverride = string(rune(0x202e))

func assertNothingDrivesTheTerminal(t *testing.T, rendered string) {
	t.Helper()
	for _, line := range strings.Split(rendered, "\n") {
		for _, r := range line {
			if r == 0x1b || unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
				t.Fatalf("terminal control survived: %q in %q", r, rendered)
			}
		}
	}
}

// Add is the only sanitation on the summary path: result() clips a reason and
// scrubs its secrets but strips no control bytes, so a cell that reaches the
// table raw reaches the operator raw.
func TestEveryTableCellIsStrippedOfTerminalControl(t *testing.T) {
	cases := map[string]string{
		"erase and rewrite": "safe\x1b[2K\rIMPOSTOR",
		"carriage return":   "first\rsecond",
		"newline":           "one\ntwo",
		"bell":              "wake\aup",
		"bidi override":     "report" + rightToLeftOverride + "txt.exe",
	}
	for name, hostile := range cases {
		t.Run(name, func(t *testing.T) {
			table := report.NewTable("DEP", "RESULT")
			table.Add("sonarr", hostile)
			var out bytes.Buffer
			if err := table.Render(&out); err != nil {
				t.Fatalf("render: %v", err)
			}
			assertNothingDrivesTheTerminal(t, out.String())
			if got := strings.Count(out.String(), "\n"); got != 2 {
				t.Fatalf("one cell became %d lines:\n%s", got, out.String())
			}
		})
	}
}

func TestAStyledTableCellIsStrippedOfTerminalControl(t *testing.T) {
	table := report.NewTable("DEP", "RESULT")
	table.AddStyled([]int{1}, func(value string) string { return value },
		"sonarr", "safe\x1b[2K\rIMPOSTOR"+rightToLeftOverride)
	var out bytes.Buffer
	if err := table.Render(&out); err != nil {
		t.Fatalf("render: %v", err)
	}
	assertNothingDrivesTheTerminal(t, out.String())
}

// The header is the column legend the operator reads every summary and history
// listing by, and it is the one line whose alignment nothing else in the suite
// asserts: the control-byte tests count newlines, not columns.
func TestTheHeaderKeepsItsColumnsAlignedWithTheRowsBeneathIt(t *testing.T) {
	tests := []struct {
		name   string
		header []string
		rows   [][]string
		want   string
	}{
		{
			name:   "the run summary header",
			header: []string{"PR", "DEP", "BUMP", "RESULT", "TOOK"},
			rows:   [][]string{{"#714", "sonarr", "minor", "merged", "2m"}},
			want:   "PR    DEP     BUMP   RESULT  TOOK\n#714  sonarr  minor  merged  2m\n",
		},
		{
			name:   "the history listing header",
			header: []string{"RUN", "PR", "DEP", "BUMP", "RESULT", "TOOK"},
			rows:   [][]string{{"3", "#714", "sonarr", "minor", "reverted", "11m"}},
			want:   "RUN  PR    DEP     BUMP   RESULT    TOOK\n3    #714  sonarr  minor  reverted  11m\n",
		},
		{
			name:   "an empty result still names its columns",
			header: []string{"PR", "DEP", "BUMP", "RESULT", "TOOK"},
			want:   "PR  DEP  BUMP  RESULT  TOOK\n",
		},
		{
			name:   "a row wider than the header widens the header",
			header: []string{"PR", "DEP"},
			rows:   [][]string{{"#1", "a-very-long-dependency-name"}, {"#22", "x"}},
			want:   "PR   DEP\n#1   a-very-long-dependency-name\n#22  x\n",
		},
		{
			name:   "a header measured in display columns, not bytes",
			header: []string{"依存", "結果"},
			rows:   [][]string{{"sonarr", "merged"}},
			want:   "依存    結果\nsonarr  merged\n",
		},
		{
			name:   "a row carrying more cells than the header has columns",
			header: []string{"A"},
			rows:   [][]string{{"row", "cell"}},
			want:   "A\nrow  cell\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			table := report.NewTable(test.header...)
			for _, row := range test.rows {
				table.Add(row...)
			}
			var out bytes.Buffer
			if err := table.Render(&out); err != nil {
				t.Fatalf("render: %v", err)
			}
			if out.String() != test.want {
				t.Fatalf("rendered\n%q\nwant\n%q", out.String(), test.want)
			}
		})
	}
}

// Padding past the last cell is invisible in a terminal but not in a redirected
// file or a paste, and a header that trails it while its rows do not is the
// signature of two writers drifting apart.
func TestNoRenderedTableLineTrailsPaddingPastItsLastCell(t *testing.T) {
	tests := []struct {
		name   string
		header []string
		rows   [][]string
	}{
		{name: "an unnamed last column", header: []string{"A", ""}, rows: [][]string{{"row", "cell"}}},
		{name: "an entirely unnamed header", header: []string{"", ""}, rows: [][]string{{"row", "cell"}}},
		{name: "header cells written with a trailing space", header: []string{"A ", "B "}, rows: [][]string{{"row", "cell"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			table := report.NewTable(test.header...)
			for _, row := range test.rows {
				table.Add(row...)
			}
			var out bytes.Buffer
			if err := table.Render(&out); err != nil {
				t.Fatalf("render: %v", err)
			}
			for i, line := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
				if line != strings.TrimRight(line, " ") {
					t.Errorf("line %d trails padding: %q", i+1, line)
				}
			}
		})
	}
}

// record() cleans a copy of the Attempt, so the caller's reason still carries
// whatever the registry or the agent wrote when the summary renders it.
func TestASummaryReasonCannotRepaintTheTerminal(t *testing.T) {
	got := render(t, domain.Run{Attempts: []domain.Attempt{
		attempt(714, "sonarr", domain.VerdictReverted,
			"safe\x1b[2K\rIMPOSTOR"+rightToLeftOverride+"gpj.exe"),
	}})
	assertNothingDrivesTheTerminal(t, got)
}

func TestASummaryErrorCannotRepaintTheTerminal(t *testing.T) {
	broken := attempt(715, "radarr", domain.VerdictError, "")
	broken.Error = "apply failed: \x1b[2K\rIMPOSTOR" + rightToLeftOverride
	assertNothingDrivesTheTerminal(t, render(t, domain.Run{Attempts: []domain.Attempt{broken}}))
}

func TestAColumnWiderThanTheDestinationIsClippedRatherThanWrapped(t *testing.T) {
	table := report.NewTable("PR", "DEP", "TOOK")
	table.Add("#1", strings.Repeat("x", 200), "2m")

	var out bytes.Buffer
	if err := table.Render(&out); err != nil {
		t.Fatalf("render: %v", err)
	}

	want := "PR  DEP" + strings.Repeat(" ", 87) + "  TOOK\n" +
		"#1  " + strings.Repeat("x", 89) + "…  2m\n"
	if out.String() != want {
		t.Fatalf("rendered\n%q\nwant\n%q", out.String(), want)
	}
}
