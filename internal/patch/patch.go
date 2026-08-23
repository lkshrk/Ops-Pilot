// Package patch parses and applies unified diffs.
//
// Fixes are committed through GitHub's commit mutation rather than pushed from
// a working tree, so a diff has to be applied to file contents in memory.
package patch

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
)

type Hunk struct {
	OldStart, OldCount int
	NewStart, NewCount int
	Lines              []string
	// NoNewlineBefore and NoNewlineAfter record a "\ No newline at end of file"
	// marker for the original and the patched side respectively.
	NoNewlineBefore bool
	NoNewlineAfter  bool
}

type File struct {
	OldPath string
	NewPath string
	Hunks   []Hunk
	// Deleted marks a diff that removes the file outright.
	Deleted bool
	// Created marks a diff that adds a file that did not exist.
	Created bool
}

// Parse reads a unified diff. It accepts git-style headers and plain ---/+++
// pairs, and ignores everything it does not recognise between file sections.
func Parse(diff string) ([]File, error) {
	var (
		files   []File
		current *File
		orphan  string
		dropped string
	)
	lines := strings.Split(strings.ReplaceAll(diff, "\r\n", "\n"), "\n")
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		switch {
		case strings.HasPrefix(line, "--- "):
			if i+1 >= len(lines) || !strings.HasPrefix(lines[i+1], "+++ ") {
				current, orphan = nil, line
				continue
			}
			files = append(files, File{
				OldPath: cleanPath(strings.TrimPrefix(line, "--- ")),
				NewPath: cleanPath(strings.TrimPrefix(lines[i+1], "+++ ")),
			})
			current = &files[len(files)-1]
			current.Created = current.OldPath == ""
			current.Deleted = current.NewPath == ""
			orphan = ""
			i++
		case strings.HasPrefix(line, "+++ "), strings.HasPrefix(line, "diff --git "):
			current, orphan = nil, line
		case strings.HasPrefix(line, "@@"):
			if current == nil {
				if orphan != "" {
					return nil, fmt.Errorf(
						"hunk header %q follows %q, which does not complete a file header: every hunk must follow a --- line and a +++ line naming the file it applies to",
						line, orphan)
				}
				return nil, fmt.Errorf("hunk header before any file header")
			}
			hunk, err := parseHunkHeader(line)
			if err != nil {
				return nil, err
			}
			for i+1 < len(lines) {
				next := lines[i+1]
				if sectionStart(lines, i+1) {
					break
				}
				// Leave an orphaned header for the outer loop, so the file it
				// fails to name cannot be inherited by the next hunk.
				if orphanedFileHeader(lines, i+1) && !bodyContinues(lines, i+2) {
					dropped = lines[i+1]
					break
				}
				i++
				if next == `\ No newline at end of file` {
					markNoNewline(&hunk)
					continue
				}
				if next == "" {
					if !blankIsContext(lines, hunk, i) {
						break
					}
					if marked, ok := blankSplitsAChange(hunk, lines, i); ok {
						return nil, blankAmbiguity(hunk.Lines[len(hunk.Lines)-1], marked)
					}
					hunk.Lines = append(hunk.Lines, " ")
					continue
				}
				if orphanedFileHeader(lines, i) || !strings.ContainsAny(next[:1], " +-") {
					if !bodyContinues(lines, i+1) {
						break
					}
					if orphanedFileHeader(lines, i) {
						return nil, fmt.Errorf(
							"%q is not a complete file header, so the hunk after it would have been applied to the previous file: a file section is a --- line immediately followed by a +++ line",
							next)
					}
					return nil, fmt.Errorf(
						"hunk %q is interrupted by %q, and body lines follow it: every line of a hunk must start with a space, + or -",
						line, next)
				}
				// Ending the body, a dashes-only line reads equally as a removal
				// and as a markdown rule before closing prose; guessing wrong
				// either deletes a document separator or drops the removal.
				if dashRule(next) && !bodyContinues(lines, i+1) {
					return nil, fmt.Errorf(
						"%q ends the hunk body and reads both as a removal of %q and as a horizontal rule before prose: add a context line after it if the removal is intended",
						next, next[1:])
				}
				hunk.Lines = append(hunk.Lines, next)
			}
			if err := checkHunkCounts(hunk, line); err != nil {
				return nil, err
			}
			current.Hunks = append(current.Hunks, hunk)
		}
	}
	if dropped != "" {
		return nil, fmt.Errorf(
			"%q ends the hunk body and reads both as a change to %q and as a file header that names no file: nothing after it distinguishes the two, so delete it if it is prose about the diff, or write the change as a --- line immediately followed by the +++ line that replaces it",
			dropped, dropped[1:])
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("diff contains no file sections")
	}
	// A hunkless deletion or creation names the whole change; a hunkless
	// modification would be applied as a byte-identical rewrite and reported as a
	// fix that landed.
	for _, file := range files {
		if len(file.Hunks) == 0 && !file.Deleted && !file.Created {
			return nil, fmt.Errorf(
				"the file section %q / %q carries no hunk, so it changes nothing: add the @@ hunk it was meant to carry, or drop the section",
				"--- "+file.OldPath, "+++ "+file.NewPath)
		}
	}
	return files, nil
}

// sectionStart reports whether lines[i] begins a new hunk or file section. A
// model narrates around its diff, and a sentence opening "diff " or "--- " is
// prose however much it looks like a marker: treating it as one ends the hunk
// body early and the rest of the body is then swallowed silently.
func sectionStart(lines []string, i int) bool {
	switch line := lines[i]; {
	case strings.HasPrefix(line, "@@"):
		return true
	case strings.HasPrefix(line, "--- "):
		// Only a pair whose hunk header follows immediately: any line stepped
		// over here can be forged by a dropped-space context line whose content
		// begins with that line class's marker, splitting an in-body pair into a
		// phantom file (C-L72/C-M63). A stray line between +++ and @@ fails closed.
		return headerPair(lines, i) && i+2 < len(lines) && strings.HasPrefix(lines[i+2], "@@")
	case strings.HasPrefix(line, "diff --git "):
		return true
	}
	return false
}

// headerPair reports whether lines[i] and lines[i+1] carry the two file-header
// markers. Inside a hunk body the same two lines are also how git spells a
// change to a line beginning "-- ": it groups removals ahead of additions, so
// the last removal and the first addition of a group land next to each other.
// Only a hunk header under the pair tells the two apart, because a file section
// is always followed by one and a body line never is.
func headerPair(lines []string, i int) bool {
	return strings.HasPrefix(lines[i], "--- ") &&
		i+1 < len(lines) && strings.HasPrefix(lines[i+1], "+++ ")
}

// orphanedFileHeader reports whether lines[i] is half of a file header whose
// other half is missing. Both halves also parse as an ordinary body line — a
// removal of "-- x", an addition of "++ x" — so they have to be judged before
// the body test rather than after it. Neither half of a pair is orphaned,
// whichever of the pair's two readings holds.
func orphanedFileHeader(lines []string, i int) bool {
	if strings.HasPrefix(lines[i], "+++ ") {
		return i == 0 || !headerPair(lines, i-1)
	}
	return strings.HasPrefix(lines[i], "--- ") && !headerPair(lines, i)
}

// blankIsContext reports whether the empty line at i is a context line whose
// leading space the model dropped, rather than the gap before the answer's
// closing prose or the trailing newline that Split always leaves behind.
//
// A hunk's last context line may legitimately be blank, and dropping it can
// shorten the context enough to make it ambiguous, so a blank still counts when
// nothing but blanks follow. What never counts is the final element of lines,
// which is Split's own artifact and never part of the diff.
func blankIsContext(lines []string, hunk Hunk, i int) bool {
	if bodyContinues(lines, i+1) {
		return true
	}
	if len(hunk.Lines) == 0 || i+1 >= len(lines) {
		return false
	}
	for _, line := range lines[i+1:] {
		if line != "" {
			return false
		}
	}
	return true
}

// blankSplitsAChange returns the marked line the unmarked blank at i separates
// from the removal above it. git puts a context line there whenever the removal
// and what follows are separated in the file, so the blank is as likely to be
// context as it is to be a model's cosmetic gap before an addition, or a removal
// whose marker went the way of its leading space. Further blanks are stepped
// over: a longer gap hides the same ambiguity, not a different one.
func blankSplitsAChange(hunk Hunk, lines []string, i int) (string, bool) {
	if len(hunk.Lines) == 0 || hunk.Lines[len(hunk.Lines)-1][0] != '-' {
		return "", false
	}
	for j := i + 1; j < len(lines); j++ {
		switch line := lines[j]; {
		case line == "":
			continue
		case sectionStart(lines, j), orphanedFileHeader(lines, j):
			return "", false
		case strings.ContainsAny(line[:1], "+-"):
			return line, true
		default:
			return "", false
		}
	}
	return "", false
}

// blankAmbiguity names the two readings the blank has, which differ by what
// follows it: against an addition the readings disagree over the order, against
// a removal over whether the blank survives at all.
func blankAmbiguity(previous, next string) error {
	if strings.HasPrefix(next, "+") {
		return fmt.Errorf(
			"the blank line between %q and %q is both a context line whose leading space was dropped and a gap inside one change, and the two readings put it on opposite sides of the addition: restore the leading space if it is context, or delete the line",
			previous, next)
	}
	return fmt.Errorf(
		"the blank line between %q and %q is both a context line whose leading space was dropped and a removal that lost its marker along with it, and the two readings disagree over whether the blank survives: restore the leading space if it is context, or mark it with - if it is removed",
		previous, next)
}

// bodyContinues reports whether any hunk body remains after from and before the
// next section. A blank line counts as neither body nor terminator, so a diff
// ending in a code fence and a trailing newline is not read as truncated.
func bodyContinues(lines []string, from int) bool {
	for i := max(from, 0); i < len(lines); i++ {
		line := lines[i]
		switch {
		case sectionStart(lines, i):
			return false
		case line == "":
			continue
		case line == `\ No newline at end of file`:
			return true
		case strings.ContainsAny(line[:1], " +-"):
			return true
		}
	}
	return false
}

// markNoNewline attributes the marker to whichever sides the preceding line
// belongs to, since a context line lacks the newline on both sides.
func markNoNewline(hunk *Hunk) {
	if len(hunk.Lines) == 0 {
		return
	}
	switch hunk.Lines[len(hunk.Lines)-1][0] {
	case ' ':
		hunk.NoNewlineBefore = true
		hunk.NoNewlineAfter = true
	case '-':
		hunk.NoNewlineBefore = true
	case '+':
		hunk.NoNewlineAfter = true
	}
}

// checkHunkCounts rejects a hunk with no body at all. The declared line counts
// are deliberately not enforced: they are redundant metadata that git apply and
// GNU patch both ignore in favour of the body, and a model writing a diff by
// hand miscounts them routinely while the content is correct. Refusing on the
// arithmetic threw away a merge whose fix an operator had already approved.
//
// Correctness does not rest on the counts. locate() requires every context line
// to match the file before anything is written, so a wrong hunk cannot apply
// whatever its header claims.
func checkHunkCounts(hunk Hunk, header string) error {
	before, after := hunkSides(hunk)
	if len(before) == 0 && len(after) == 0 {
		return fmt.Errorf("hunk header %q has no body", header)
	}
	// A hunk that changes nothing can only mean a +/- line was lost to parsing;
	// applying it would report success for a change that never landed.
	if slices.Equal(before, after) {
		return fmt.Errorf("hunk header %q changes nothing: its body is all context, so a line it was meant to add or remove was lost", header)
	}
	return nil
}

// dashRule reports whether line is dashes only, three or more — both a removal
// of a shorter dash run and a markdown horizontal rule.
func dashRule(line string) bool {
	return len(line) >= 3 && strings.Count(line, "-") == len(line)
}

func cleanPath(value string) string {
	path := strings.TrimSpace(value)
	if tab := strings.IndexByte(path, '\t'); tab >= 0 {
		path = path[:tab]
	}
	if path == "/dev/null" {
		return ""
	}
	for _, prefix := range []string{"a/", "b/"} {
		if after, found := strings.CutPrefix(path, prefix); found {
			return after
		}
	}
	return path
}

func parseHunkHeader(line string) (Hunk, error) {
	// @@ -old,count +new,count @@ optional context
	body := line
	if index := strings.Index(body[2:], "@@"); index >= 0 {
		body = body[:index+4]
	}
	fields := strings.Fields(strings.Trim(body, "@ "))
	if len(fields) < 2 {
		return Hunk{}, fmt.Errorf("malformed hunk header %q", line)
	}
	oldStart, oldCount, err := parseRange(strings.TrimPrefix(fields[0], "-"))
	if err != nil {
		return Hunk{}, fmt.Errorf("malformed hunk header %q: %w", line, err)
	}
	newStart, newCount, err := parseRange(strings.TrimPrefix(fields[1], "+"))
	if err != nil {
		return Hunk{}, fmt.Errorf("malformed hunk header %q: %w", line, err)
	}
	return Hunk{
		OldStart: oldStart, OldCount: oldCount,
		NewStart: newStart, NewCount: newCount,
	}, nil
}

func parseRange(value string) (int, int, error) {
	start, count, found := strings.Cut(value, ",")
	first, err := strconv.Atoi(start)
	if err != nil {
		return 0, 0, err
	}
	if !found {
		return first, 1, nil
	}
	second, err := strconv.Atoi(count)
	if err != nil {
		return 0, 0, err
	}
	return first, second, nil
}

// Apply applies a file's hunks to original. Hunk positions are treated as hints:
// the context is searched for near the stated offset, so a diff written against
// a slightly different revision still applies or fails cleanly.
func Apply(original []byte, file File) ([]byte, error) {
	var lines []string
	if len(original) > 0 {
		lines = strings.Split(strings.TrimSuffix(string(original), "\n"), "\n")
	}
	if file.Deleted {
		for _, hunk := range file.Hunks {
			before, _ := hunkSides(hunk)
			if _, err := locate(lines, before, hunk.OldStart-1); err != nil {
				return nil, fmt.Errorf("%s: %w", file.OldPath, err)
			}
		}
		return nil, nil
	}
	trailingNewline := len(original) > 0 && original[len(original)-1] == '\n'
	if file.Created {
		trailingNewline = true
	}
	offset := 0
	for _, hunk := range file.Hunks {
		before, after := hunkSides(hunk)
		at, err := locate(lines, before, hunk.OldStart-1+offset)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", file.NewPath, err)
		}
		updated := make([]string, 0, len(lines)-len(before)+len(after))
		updated = append(updated, lines[:at]...)
		updated = append(updated, after...)
		updated = append(updated, lines[at+len(before):]...)
		lines = updated
		offset += len(after) - len(before)
		if hunk.NoNewlineBefore || hunk.NoNewlineAfter {
			trailingNewline = !hunk.NoNewlineAfter
		}
	}
	result := strings.Join(lines, "\n")
	if trailingNewline {
		result += "\n"
	}
	return []byte(result), nil
}

func hunkSides(hunk Hunk) (before, after []string) {
	for _, line := range hunk.Lines {
		if line == "" {
			continue
		}
		marker, content := line[0], line[1:]
		switch marker {
		case ' ':
			before = append(before, content)
			after = append(after, content)
		case '-':
			before = append(before, content)
		case '+':
			after = append(after, content)
		}
	}
	return before, after
}

// locate finds where before matches in lines. An ambiguous or missing context
// is an error, never a guess: a fix that does not apply exactly must not be
// committed.
//
// The whole file is searched and a unique match is accepted however far from
// the header it lies. Distance is not evidence: nothing asks the agent for
// accurate line numbers, the repository files it reads carry none, and on a
// file too long to count it emits a placeholder rather than a miscount. For the
// same reason the position cannot break a tie either — a header that lands on
// one of several identical blocks has agreed with the file by coincidence.
func locate(lines, before []string, hint int) (int, error) {
	if len(before) == 0 {
		if len(lines) > 0 {
			return 0, fmt.Errorf("hunk at line %d has no context or removed lines to match against", hint+1)
		}
		return 0, nil
	}
	var found []int
	for at := range len(lines) {
		if matches(lines, before, at) {
			found = append(found, at)
		}
	}
	switch len(found) {
	case 0:
		return 0, fmt.Errorf("hunk context not found near line %d", hint+1)
	case 1:
		return found[0], nil
	}
	return 0, fmt.Errorf(
		"hunk context stated at line %d is ambiguous: it matches at %s, and the stated position does not choose between them: add a line to the context that appears once",
		hint+1, describeMatches(found))
}

// describeMatches keeps the total exact and the list short: the whole error is
// handed back to the agent as its next prompt.
func describeMatches(found []int) string {
	const listed = 5
	shown, prefix := found, "lines "
	if len(shown) > listed {
		shown, prefix = shown[:listed], fmt.Sprintf("%d places, among them lines ", len(found))
	}
	numbers := make([]string, len(shown))
	for i, at := range shown {
		numbers[i] = strconv.Itoa(at + 1)
	}
	last := len(numbers) - 1
	return prefix + strings.Join(numbers[:last], ", ") + " and " + numbers[last]
}

func matches(lines, before []string, at int) bool {
	if at < 0 || at+len(before) > len(lines) {
		return false
	}
	for i, line := range before {
		if lines[at+i] != line {
			return false
		}
	}
	return true
}
