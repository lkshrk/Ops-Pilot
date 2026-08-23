// Package renovate reads the dependency table and release notes that Renovate
// writes into a pull request body.
package renovate

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/lkshrk/ops-pilot/internal/domain"
)

var (
	markdownLink  = regexp.MustCompile(`\[([^\]]+)\]\([^)]*\)`)
	versionChange = regexp.MustCompile("`([^`]+)`\\s*(?:->|→)\\s*`([^`]+)`")
	summaryLine   = regexp.MustCompile(`(?i)<summary>\s*(.+?)\s*</summary>`)
	summaryParts  = regexp.MustCompile(`^(.*?)\s*\(([^()]+)\)$`)
	repositoryRef = regexp.MustCompile(`^[A-Za-z0-9._-]+(?:/[A-Za-z0-9._-]+)+$`)
	numeric       = regexp.MustCompile(`^\d+$`)
)

// Update is one row of the Renovate dependency table.
type Update struct {
	Dependency domain.Dependency
	// Upstream is the owner/name repository Renovate attributed release notes
	// to, when the body carried any. It is a hint, not a guarantee.
	Upstream string
	// ReleaseNotes is the section of the body describing this dependency.
	ReleaseNotes string
}

// Parse reads every dependency update declared in a Renovate pull request body.
func Parse(body string) ([]Update, error) {
	rows, err := parseTable(body)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("pull request body declares no dependency updates")
	}
	updates := make([]Update, 0, len(rows))
	for _, row := range rows {
		updates = append(updates, Update{Dependency: row})
	}
	bindNotes(updates, parseReleaseNotes(body))
	return updates, nil
}

// Single returns the only update in the body. A pull request touching several
// dependencies at once cannot be attributed to one changelog, so it is refused.
func Single(body string) (Update, error) {
	updates, err := Parse(body)
	if err != nil {
		return Update{}, err
	}
	if len(updates) != 1 {
		return Update{}, fmt.Errorf("pull request updates %d dependencies at once", len(updates))
	}
	return updates[0], nil
}

func parseTable(body string) ([]domain.Dependency, error) {
	lines := strings.Split(body, "\n")
	header, start := -1, -1
	for i, line := range lines {
		if !strings.Contains(line, "|") {
			continue
		}
		columns := tableCells(line)
		if indexOf(columns, "package") >= 0 && indexOf(columns, "change") >= 0 {
			header, start = i, i+2
			break
		}
	}
	if header < 0 || start >= len(lines) {
		return nil, nil
	}
	columns := tableCells(lines[header])
	packageAt, changeAt := indexOf(columns, "package"), indexOf(columns, "change")
	typeAt, updateAt := indexOf(columns, "type"), indexOf(columns, "update")

	var result []domain.Dependency
	for _, line := range lines[start:] {
		if !strings.Contains(line, "|") {
			break
		}
		cells := tableCells(line)
		if len(cells) <= packageAt || len(cells) <= changeAt {
			continue
		}
		name := dependencyName(cells[packageAt])
		from, to, found := parseVersionChange(cells[changeAt])
		if name == "" || !found {
			continue
		}
		dependency := domain.Dependency{
			Name:        name,
			Kind:        dependencyKind(cellAt(cells, typeAt)),
			FromVersion: from,
			ToVersion:   to,
			Bump:        BumpClass(from, to, plainText(cellAt(cells, updateAt))),
		}
		result = append(result, dependency)
	}
	return result, nil
}

func tableCells(line string) []string {
	trimmed := strings.TrimSpace(line)
	trimmed = strings.TrimPrefix(trimmed, "|")
	trimmed = strings.TrimSuffix(trimmed, "|")
	cells := strings.Split(trimmed, "|")
	for i := range cells {
		cells[i] = strings.TrimSpace(cells[i])
	}
	return cells
}

func cellAt(cells []string, index int) string {
	if index < 0 || index >= len(cells) {
		return ""
	}
	return cells[index]
}

func indexOf(cells []string, want string) int {
	for i, cell := range cells {
		if strings.EqualFold(plainText(cell), want) {
			return i
		}
	}
	return -1
}

func plainText(cell string) string {
	return strings.TrimSpace(strings.Trim(markdownLink.ReplaceAllString(cell, "$1"), "`*_ "))
}

// dependencyName reads the Package cell. Renovate links the dependency and then
// often appends its own "([source](url))" link, so only the first link's text
// names the dependency.
func dependencyName(cell string) string {
	if match := markdownLink.FindStringSubmatch(cell); match != nil {
		return strings.TrimSpace(strings.Trim(match[1], "`*_ "))
	}
	return plainText(cell)
}

func parseVersionChange(cell string) (string, string, bool) {
	match := versionChange.FindStringSubmatch(cell)
	if match == nil {
		return "", "", false
	}
	from, to := plainText(match[1]), plainText(match[2])
	if from == "" || to == "" {
		return "", "", false
	}
	return from, to, true
}

func dependencyKind(cell string) string {
	switch value := strings.ToLower(plainText(cell)); {
	case strings.Contains(value, "helm") || strings.Contains(value, "chart"):
		return domain.DependencyKindHelm
	case value == "":
		return ""
	default:
		return domain.DependencyKindDocker
	}
}

// BumpClass reads Renovate's declared update type but never lets it under-state
// the version arithmetic: the numbers are the ground truth the approval gate
// rests on, the column is prose a hand-edited body can lie in. A declared class
// is honoured when it is at least as alarming as the numbers imply, so a range
// or pinning rule can still raise a patch to minor, but a declared patch on a
// cross-major change reads as major, and a declared patch on versions that say
// nothing at all reads as unknown: a pair the arithmetic cannot classify may be
// hiding a major change, so the operator, not the column, decides it.
//
// A type it declared but this does not recognise — replacement, rollback, pin,
// bump, lockFileMaintenance — is not a version bump at all, and reading the
// numbers as one is how a rollback to an older release passes as a minor
// update. Such a type reads as unknown, except where the versions alone are
// already more alarming: downgrading a major bump to unknown would trade a
// merge that asks the operator for one that does not.
func BumpClass(from, to, declared string) domain.BumpClass {
	computed := CompareVersions(from, to)
	switch strings.ToLower(strings.TrimSpace(declared)) {
	case "major":
		return domain.BumpMajor
	case "minor":
		return declaredOrStricter(domain.BumpMinor, computed)
	case "patch":
		return declaredOrStricter(domain.BumpPatch, computed)
	case "digest":
		return declaredOrStricter(domain.BumpDigest, computed)
	case "":
		return computed
	}
	if computed == domain.BumpMajor {
		return domain.BumpMajor
	}
	return domain.BumpUnknown
}

// declaredOrStricter keeps the declared class only while the version arithmetic
// can check it. A computed class that is more alarming wins, and so does an
// uncomparable one, which holds: a low class the numbers cannot confirm is how
// a change nobody has bounded merges unattended.
func declaredOrStricter(declared, computed domain.BumpClass) domain.BumpClass {
	if computed == domain.BumpUnknown || bumpSeverity(computed) > bumpSeverity(declared) {
		return computed
	}
	return declared
}

func bumpSeverity(bump domain.BumpClass) int {
	switch bump {
	case domain.BumpMajor:
		return 4
	case domain.BumpMinor:
		return 3
	case domain.BumpPatch:
		return 2
	case domain.BumpDigest:
		return 1
	default:
		return 0
	}
}

// CompareVersions classifies a version change without Renovate's help. It
// tolerates a leading v and version strings with more than three segments,
// which repackaged images such as sonarr 4.0.19.2995 use.
//
// A pair that went backwards is no class of upgrade at all and reads as
// unknown. Classifying it by the position that moved calls a rollback from
// 4.0.25 to 4.0.19 a patch, and a patch merges unattended on the agent's
// verdict without the removed releases ever being read - the notes that would
// have named what the rollback takes away are the ones no longer in the range.
// Renovate labels its own rollbacks, so a downgrade nothing declared is a body
// nobody bounded, and the operator, not the arithmetic, decides it.
func CompareVersions(from, to string) domain.BumpClass {
	fromDigest, toDigest := digestOf(from), digestOf(to)
	fromParts, toParts := versionParts(from), versionParts(to)
	if len(fromParts) == 0 || len(toParts) == 0 {
		// Differing digests report a rebuild only while the tags around them agree.
		if fromDigest != "" && toDigest != "" && fromDigest != toDigest && tagOf(from) == tagOf(to) {
			return domain.BumpDigest
		}
		return domain.BumpUnknown
	}
	if Newer(to, from) < 0 {
		return domain.BumpUnknown
	}
	// Omitted trailing segments are zeroes: 1 -> 1.5.0 moved the minor position.
	for i := 0; i < len(fromParts) || i < len(toParts); i++ {
		if partAt(fromParts, i) == partAt(toParts, i) {
			continue
		}
		switch i {
		case 0:
			return domain.BumpMajor
		case 1:
			return domain.BumpMinor
		default:
			return domain.BumpPatch
		}
	}
	if len(fromParts) != len(toParts) {
		return domain.BumpPatch
	}
	// A prerelease suffix carries the whole difference between 1.2.3-alpha and
	// 1.2.3-beta, so reporting a digest rebuild here would say the code is the
	// same when only the layers were rebuilt.
	if prereleaseOf(from) != prereleaseOf(to) {
		return domain.BumpUnknown
	}
	// A rebuild is only readable when both digests verify; one alone leaves the
	// other side's image unknown, so it cannot show that only the layers moved.
	if fromDigest != "" && toDigest != "" && fromDigest != toDigest {
		return domain.BumpDigest
	}
	return domain.BumpUnknown
}

// Newer compares two version strings segment by segment. It returns 0 whenever
// either side is uncomparable, so an unparseable version never wins.
func Newer(a, b string) int {
	left, right := versionParts(a), versionParts(b)
	if len(left) == 0 || len(right) == 0 {
		return 0
	}
	// Omitted trailing segments are zeroes, not a lower version: 1.0 is 1.0.0,
	// so ordering it by segment count would rank 1.0.0-alpha above its release.
	for i := 0; i < len(left) || i < len(right); i++ {
		switch {
		case partAt(left, i) > partAt(right, i):
			return 1
		case partAt(left, i) < partAt(right, i):
			return -1
		}
	}
	if order := comparePrerelease(prereleaseOf(a), prereleaseOf(b)); order != 0 {
		return order
	}
	// Two digests of the same version have no order; neither may displace the other.
	return 0
}

// comparePrerelease orders semver prerelease suffixes: a release outranks any
// prerelease of itself, numeric identifiers compare numerically and rank below
// alphanumeric ones, and a shorter run of equal identifiers ranks lower.
func comparePrerelease(left, right string) int {
	switch {
	case left == right:
		return 0
	case left == "":
		return 1
	case right == "":
		return -1
	}
	leftIDs, rightIDs := strings.Split(left, "."), strings.Split(right, ".")
	for i := 0; i < len(leftIDs) && i < len(rightIDs); i++ {
		if order := compareIdentifier(leftIDs[i], rightIDs[i]); order != 0 {
			return order
		}
	}
	switch {
	case len(leftIDs) > len(rightIDs):
		return 1
	case len(leftIDs) < len(rightIDs):
		return -1
	}
	return 0
}

func compareIdentifier(left, right string) int {
	leftNumeric, rightNumeric := numeric.MatchString(left), numeric.MatchString(right)
	switch {
	case leftNumeric && rightNumeric:
		leftValue, leftErr := strconv.Atoi(left)
		rightValue, rightErr := strconv.Atoi(right)
		if leftErr != nil || rightErr != nil {
			return strings.Compare(left, right)
		}
		switch {
		case leftValue > rightValue:
			return 1
		case leftValue < rightValue:
			return -1
		}
		return 0
	case leftNumeric:
		return -1
	case rightNumeric:
		return 1
	}
	return strings.Compare(left, right)
}

func partAt(parts []int, index int) int {
	if index >= len(parts) {
		return 0
	}
	return parts[index]
}

func prereleaseOf(value string) string {
	value = strings.TrimSpace(value)
	if index := strings.Index(value, "@"); index >= 0 {
		value = value[:index]
	}
	value = strings.TrimPrefix(value, "v")
	if index := strings.Index(value, "+"); index >= 0 {
		value = value[:index]
	}
	if _, prerelease, found := strings.Cut(value, "-"); found {
		return prerelease
	}
	return ""
}

func versionParts(value string) []int {
	value = strings.TrimSpace(value)
	if index := strings.Index(value, "@"); index >= 0 {
		value = value[:index]
	}
	value = strings.TrimPrefix(value, "v")
	if index := strings.IndexAny(value, "-+"); index >= 0 {
		value = value[:index]
	}
	if value == "" {
		return nil
	}
	segments := strings.Split(value, ".")
	parts := make([]int, 0, len(segments))
	for _, segment := range segments {
		if !numeric.MatchString(segment) {
			return nil
		}
		number, err := strconv.Atoi(segment)
		if err != nil {
			return nil
		}
		parts = append(parts, number)
	}
	return parts
}

// An unverifiable part after the "@" reports no digest, so the change reads as
// uncomparable rather than as a rebuild that may merge unattended.
func digestOf(value string) string {
	if _, digest, found := strings.Cut(value, "@"); found {
		value = digest
	}
	value = strings.TrimSpace(value)
	if IsDigest(value) {
		return value
	}
	return ""
}

// tagOf reads the tag a digest was pinned to. A reference carrying no "@" is a
// bare digest with no tag at all, so it reports empty, not the digest itself.
func tagOf(value string) string {
	tag, _, found := strings.Cut(strings.TrimSpace(value), "@")
	if !found {
		return ""
	}
	return tag
}

// IsDigest recognises the algorithm:hex form a reference pinned to a digest
// rather than a tag uses. A tag cannot contain a colon, so the shape is unique.
// The whole value must be the digest: callers append it to "name@" verbatim.
//
// The value must arrive trimmed, and this refuses a padded one rather than
// trimming it, because trimming here would let two spellings of one digest look
// like two digests. Both errors that follow from a refusal fall the safe way:
// the change reads as unknown and holds, and the reference is built as a tag
// the registry then fails to resolve.
//
// It reads the shape and never the algorithm's true digest length, so md5 and a
// truncated sha512 both pass, and hex in upper case passes although no registry
// publishes one. A true answer therefore says the value is spelled like a
// digest, not that any registry will accept it.
func IsDigest(value string) bool {
	algorithm, encoded, found := strings.Cut(value, ":")
	if !found || algorithm == "" || len(encoded) < 32 {
		return false
	}
	for _, r := range algorithm {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
			return false
		}
	}
	for _, r := range encoded {
		if !(r >= 'a' && r <= 'f' || r >= 'A' && r <= 'F' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

type releaseNote struct {
	upstream string
	subject  string
	// dependency is the parenthesised name Renovate appends to the summary; it
	// is empty when the summary names the repository alone.
	dependency string
	text       string
}

// parseReleaseNotes splits the Release Notes section into its per-dependency
// <details> blocks. Renovate labels each block "owner/repo (dependency)".
func parseReleaseNotes(body string) []releaseNote {
	var notes []releaseNote
	remainder := body
	for {
		open := strings.Index(remainder, "<details>")
		if open < 0 {
			return notes
		}
		remainder = remainder[open:]
		end := strings.Index(remainder, "</details>")
		if end < 0 {
			return notes
		}
		block := remainder[:end]
		remainder = remainder[end+len("</details>"):]

		match := summaryLine.FindStringSubmatch(block)
		if match == nil {
			continue
		}
		subject := plainText(match[1])
		note := releaseNote{subject: subject, text: strings.TrimSpace(afterSummary(block))}
		repository := subject
		if parts := summaryParts.FindStringSubmatch(subject); parts != nil {
			repository, note.dependency = parts[1], strings.TrimSpace(parts[2])
		}
		note.upstream = repositoryOf(repository)
		notes = append(notes, note)
	}
}

// repositoryOf reads the repository a summary attributes its notes to. It scans
// the fields rather than anchoring at the start, so a bullet or an arrow glyph
// does not cost the attribution, and it takes the whole path: the changelog side
// compares by exact owner/name, so a truncated one is indistinguishable from
// garbage and no attribution is the safer answer. The pattern admits no "@",
// which the changelog comparison relies on to rule out a collision.
func repositoryOf(summary string) string {
	for _, field := range strings.Fields(summary) {
		if repositoryRef.MatchString(field) {
			return field
		}
	}
	return ""
}

func afterSummary(block string) string {
	if _, rest, found := strings.Cut(block, "</summary>"); found {
		return rest
	}
	return block
}

// bindNotes attaches each release-notes block to the row it belongs to.
// Renovate writes the dependency name in parentheses after the upstream
// repository, and falls back to the repository alone when they are the same
// project. Names are compared whole, because a substring match lets
// cert-manager take cert-manager-webhook-dns's block and read a sibling
// project's breaking changes as its own. Each criterion is tried across every
// row before the next one, so a loosely matching row cannot take the block an
// exactly matching row names, and a claimed block is consumed so no two
// dependencies report one changelog. A block nothing claims stays unattributed:
// Renovate appends its own configuration block to every pull request, and
// binding it would report a changelog that does not exist.
func bindNotes(updates []Update, notes []releaseNote) {
	bound := make([]bool, len(updates))
	claimed := make([]bool, len(notes))
	criteria := []func(releaseNote, string) bool{
		namesDependency,
		namesShortDependency,
		namesRepositoryAlone,
		namesRepackagedRepository,
	}
	for _, names := range criteria {
		for i := range updates {
			if bound[i] {
				continue
			}
			for j := range notes {
				if claimed[j] || !names(notes[j], updates[i].Dependency.Name) {
					continue
				}
				updates[i].Upstream, updates[i].ReleaseNotes = notes[j].upstream, notes[j].text
				bound[i], claimed[j] = true, true
				break
			}
		}
	}
}

func namesDependency(note releaseNote, dependency string) bool {
	return note.dependency != "" && strings.EqualFold(note.dependency, dependency)
}

// namesShortDependency covers a repackaged image, whose table row carries the
// registry path while the summary names only the project.
func namesShortDependency(note releaseNote, dependency string) bool {
	short := shortName(dependency)
	return note.dependency != "" && short != "" && strings.EqualFold(note.dependency, short)
}

// namesRepositoryAlone is gated on namesShortRepositoryAlone so it can only ever
// reorder blocks that already bind, never bind one that did not: the attribution
// survives decoration the last-segment comparison does not see through.
func namesRepositoryAlone(note releaseNote, dependency string) bool {
	if !namesShortRepositoryAlone(note, dependency) {
		return false
	}
	return strings.EqualFold(note.subject, dependency) || strings.EqualFold(note.upstream, dependency)
}

// namesRepackagedRepository binds a repackaged image to the upstream a sole
// block names: the table row carries the registry path and the summary the
// source repository, so its last segment matches but its owner need not. A row
// no deeper than the block is a plain owner/name whose owner differing from the
// block's is a cross-owner collision, not a repackaging, so it is refused rather
// than served another project's notes.
func namesRepackagedRepository(note releaseNote, dependency string) bool {
	if !namesShortRepositoryAlone(note, dependency) {
		return false
	}
	return pathDepth(dependency) > pathDepth(note.subject)
}

func pathDepth(value string) int {
	return strings.Count(value, "/")
}

// namesShortRepositoryAlone is a tier of its own below namesRepositoryAlone:
// two owners can publish the same project name, so the row a summary spells out
// whole must claim it before a row that only shares its last segment.
func namesShortRepositoryAlone(note releaseNote, dependency string) bool {
	if !repositoryAlone(note, dependency) {
		return false
	}
	short := shortName(note.subject)
	return short != "" && strings.EqualFold(short, shortName(dependency))
}

func repositoryAlone(note releaseNote, dependency string) bool {
	return note.dependency == "" && note.subject != "" && dependency != ""
}

func shortName(value string) string {
	if index := strings.LastIndex(value, "/"); index >= 0 {
		return value[index+1:]
	}
	return value
}
