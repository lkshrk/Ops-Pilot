package diagnostics

import (
	"regexp"
	"strings"
)

// scrubMark stands in for a redacted value while the rules run, and becomes
// redactedValue once they have all finished. It is a single control byte that
// every value class excludes, so no rule can match what another rule wrote:
// the rules are not disjoint, and neither ordering of them is safe on its own.
const scrubMark = "\x00"

// credentialNames is shared by the same-line and next-line key rules, which must
// recognise the same set or a name added to one leaks past the other.
const credentialNames = `pass(?:word|wd|phrase)|secret[_-]?key|client[_-]?secret|api[_-]?key|` +
	`api[_-]?secret|access[_-]?key|access[_-]?token|refresh[_-]?token|auth[_-]?token|` +
	`session[_-]?key|private[_-]?key|credentials?|token`

// A ${...} or $(...) placeholder opens its own delimiter, so its matching close
// is carried into the redaction; otherwise the value stops before a closing
// "/'/)/} it did not open, leaving that delimiter to close its own surrounding.
const credentialValue = `(?:\$\{[^\s"',;}\x00]{2,}\}|\$\([^\s"',;)\x00]{2,}\)|[^\s"',;)}\x00]{4,})`

// The path-qualified "=" rule must not read the second "=" of a "=="/"!="/">="
// comparison as the start of a value, so its value may not open with "=".
const credentialValueNoEquals = `(?:\$\{[^\s"',;}\x00]{2,}\}|\$\([^\s"',;)\x00]{2,}\)|[^\s"',;)}=\x00][^\s"',;)}\x00]{3,})`

// Bare words may not close a next-line value: prose after the token means the
// token is an object name, not a secret. A "#" comment closes a value too, but a
// value carrying a "/" is an object reference (Namespace/Kind/name), so the
// comment terminator is denied to it while a quote, comma, or end-of-line is not.
const nextLineValueAndEnd = `(?:([^\s"',;)}:=\x00]{4,})(["']?,?[ \t]*(?:\r?\n|$))` +
	`|([^\s"',;)}:=/\x00]{4,})(["']?,?[ \t]*#[^\n]*(?:\r?\n|$)))`

// Redaction by value cannot work here: the secret belongs to the workload, not
// to ops-pilot, so its shape is the only thing there is to match on.
//
// Order still matters, but only in one direction: a rule that recognises a whole
// secret must run before one that would match a fragment of it. The private key
// block, bounded by boundPrivateKeyBlocks, is consumed before the key-name rule,
// which stops at whitespace and would otherwise leave the body behind.
var scrubbers = []struct {
	pattern     *regexp.Regexp
	replacement string
}{
	{
		// The base64 of "-----BEGIN", as a key lands in a Secret payload.
		regexp.MustCompile(`\bLS0tLS1CRUdJT[A-Za-z0-9+/=]{16,}`),
		scrubMark,
	},
	{
		// Only a real name=value cookie, or "cookie: invalid session" is eaten.
		regexp.MustCompile(`(?i)((?:^|[\s"',;{(\[])(?:set-)?cookie["']?\s*[:=]\s*)[^\s;=]+=[^\s;]{4,}(?:;\s*[^\s;=]+=[^\s;]*)*`),
		"${1}" + scrubMark,
	},
	{
		regexp.MustCompile(`(?i)(authorization["']?\s*[:=]\s*["']?)(?:bearer|basic|token)?\s*[A-Za-z0-9\-._~+/]{16,}={0,2}`),
		"${1}" + scrubMark,
	},
	{
		// tls.key counts only when the value is key material, or
		// "open /etc/ssl/tls.key: permission denied" loses its reason.
		regexp.MustCompile(`(?i)([A-Za-z0-9_.\-]*[A-Za-z0-9]+\.key["']?\s*[:=]\s*["']?)[A-Za-z0-9+/=]{16,}`),
		"${1}" + scrubMark,
	},
	{
		regexp.MustCompile(`(?i)((?:^|[^A-Za-z0-9_.\-/])[A-Za-z0-9_.\-]*(?:` + credentialNames + `)["']?\s*[:=][ \t]*["']?)` + credentialValue),
		"${1}" + scrubMark,
	},
	{
		// "pass" alone counts only after a separator that is not "-", or
		// bypass, compass and first-pass all match.
		regexp.MustCompile(`(?i)((?:^|[\s"',;{(\[_])pass["']?\s*[:=][ \t]*["']?)` + credentialValue),
		"${1}" + scrubMark,
	},
	{
		// A path-qualified name is a reference under ":" - an image tag, a mount
		// path - but under "=" it is an assignment, and none of those shapes use "=".
		regexp.MustCompile(`(?i)((?:^|[^A-Za-z0-9_.\-])[A-Za-z0-9_.\-/]*/[A-Za-z0-9_.\-]*(?:` + credentialNames + `)["']?[ \t]*=[ \t]*["']?)` + credentialValueNoEquals),
		"${1}" + scrubMark,
	},
	// A YAML plain scalar may begin on the line below its key, so the value is
	// still reached across a newline - but only when it ends the line, since a
	// token followed by its own separator is the next key and not a value. The
	// value line may carry a diff "+"/"-" prefix, but only when the key line is
	// itself a diff line or holds prose: a key at true column 0 is raw YAML, not
	// a hunk body, so a "- " below it is a block-sequence dash - a list
	// reference, not a removed value - and the tolerance is dropped for it.
	{
		regexp.MustCompile(`(?i)((?:` +
			`(?m:^)["']?(?:[A-Za-z0-9_][A-Za-z0-9_.\-]*)?(?:` + credentialNames + `)["']?\s*[:=][ \t]*\r?\n[ \t]*["']?` +
			`|(?m:^)[+\-][ \t]*(?:-[ \t]+)?["']?[A-Za-z0-9_.\-]*(?:` + credentialNames + `)["']?\s*[:=][ \t]*\r?\n[+\-]?[ \t]*["']?` +
			`|[^A-Za-z0-9_.\-/\n][A-Za-z0-9_.\-]*(?:` + credentialNames + `)["']?\s*[:=][ \t]*\r?\n[+\-]?[ \t]*["']?` +
			`))` + nextLineValueAndEnd),
		"${1}" + scrubMark + "${3}${5}",
	},
	{
		regexp.MustCompile(`(?i)((?:^|[\s"',;{(\[_])pass["']?\s*[:=][ \t]*\r?\n[+\-]?[ \t]*["']?)` + nextLineValueAndEnd),
		"${1}" + scrubMark + "${3}${5}",
	},
	{
		regexp.MustCompile(`(?i)(--(?:pass(?:word|wd|phrase)?|token|api-?key|secret)[ =])[^\s\x00]{4,}`),
		"${1}" + scrubMark,
	},
	{
		// The username is optional: redis://:pw@host is the canonical Redis URL,
		// and https://:token@host is what git prints when a clone fails. The
		// password half cannot cross a "/", which is what keeps every legitimate
		// "@" - image digests, commit trailers, mailto links - out of reach.
		regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://[^\s:/?#@]*):[^\s/?#@]+@`),
		"${1}:" + scrubMark + "@",
	},
	{
		regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`),
		scrubMark,
	},
	{
		regexp.MustCompile(`\b(?:gh[pousr]_[A-Za-z0-9]{16,}|github_pat_[A-Za-z0-9_]{20,}|glpat-[A-Za-z0-9_\-]{16,}|xox[abprs]-[A-Za-z0-9-]{10,}|A[KS]IA[0-9A-Z]{16}|AIza[A-Za-z0-9_\-]{35}|sk-[A-Za-z0-9_\-]{20,})`),
		scrubMark,
	},
}

// The private-key body spans newlines, so it is bounded in code: an unbalanced
// BEGIN whose END is only reachable across a hunk header or a file header is left
// alone, so the rule cannot collapse approved diff content between two unrelated
// blocks into a single redaction. A blank line is not a boundary because an
// RFC 1421 encrypted key has a mandatory one before its body, and leaking that
// key is worse than the diff a blank-line bound would have protected.
var (
	pemBegin = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)
	pemEnd   = regexp.MustCompile(`-----END [A-Z0-9 ]*PRIVATE KEY-----`)
)

func boundPrivateKeyBlocks(text string, mark func(int) string) string {
	if !strings.Contains(text, "PRIVATE KEY-----") {
		return text
	}
	var b strings.Builder
	rest := text
	for {
		begin := pemBegin.FindStringIndex(rest)
		if begin == nil {
			b.WriteString(rest)
			break
		}
		tail := rest[begin[1]:]
		end := pemEnd.FindStringIndex(tail)
		if end == nil || crossesDiffBoundary(tail[:end[0]]) {
			b.WriteString(rest[:begin[1]])
			rest = tail
			continue
		}
		b.WriteString(rest[:begin[0]])
		b.WriteString(mark(begin[1] + end[1] - begin[0]))
		rest = tail[end[1]:]
	}
	return b.String()
}

func crossesDiffBoundary(mid string) bool {
	parts := strings.Split(mid, "\n")
	for i := 1; i < len(parts)-1; i++ {
		line := strings.TrimRight(parts[i], "\r")
		if line == "---" || line == "+++" ||
			strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ ") {
			return true
		}
		body := strings.TrimLeft(line, "+- ")
		body = strings.TrimLeft(body, " \t")
		if strings.HasPrefix(body, "@@") {
			return true
		}
	}
	return false
}

// An indented block sequence under a credential key holds real nested scalars,
// and the key may map to several of them, so the sequence is walked to its end
// rather than matched as one fixed-width span - every item is a secret, not just
// the first. A dash at true column 0 is the reference structure C-L61/C-L85 leave
// alone, so leading whitespace before the dash is required here; a "/"-bearing
// item is an object reference and is left to the value-shape rules. The sequence
// ends at the first line that is not an item at its own indent, which is what
// keeps the walk off the next key's list. A comment is not such a line: YAML does
// not end a sequence at one, so the items below it are still the key's values.
// The two families are kept apart because they disagree about a column-0 dash:
// the named-key rule drops its diff tolerance at column 0, so a list there is the
// reference structure C-L61/C-L85 leave alone, while the "pass" rule tolerates a
// marker unconditionally and redacts one. A pass key therefore carries no
// column-0 exemption in either form, marked or raw.
//
// Neither key is allowed the prose tolerance the value rules carry: a sentence
// ending in a credential word is not structure, and treating one as a key eats
// the bullet list of failing objects a Flux error puts under it. The one place
// a key legitimately reaches the walk without being at the start of its line is
// a hunk header: git's default xfuncname puts the nearest preceding column-0
// line after the second "@@", so for YAML the heading IS the key the hunk's
// items belong to, and the same structural test is applied to it.
var (
	credentialSequenceKey0 = regexp.MustCompile(`(?i)^[ \t]*["']?(?:[A-Za-z0-9_][A-Za-z0-9_.\-]*)?(?:` + credentialNames + `)["']?[ \t]*:[ \t]*\r?$`)
	passSequenceKey        = regexp.MustCompile(`(?i)^[ \t]*(?:#[ \t]*)?(?:-[ \t]+)*["']?(?:(?:[A-Za-z0-9_][A-Za-z0-9_.\-]*)?_)?pass["']?[ \t]*:[ \t]*\r?$`)
	hunkHeading            = regexp.MustCompile(`^@@+[ \t][^@\n]*@@+[ \t]?(.*)$`)
)

// A rotated-out secret commented in place is still a secret, so a commented item
// of a credential key's own list is redacted like a live one. Only the item shape
// is eaten: a comment is where an operator records what broke, so prose, a
// mapping and a "/"-bearing object reference all keep their text. A commented
// item reads a column-0 dash the way its key family does, so it needs the same
// two patterns the live item does.
var (
	sequenceDash               = regexp.MustCompile(`^([ \t]+)-(?:[ \t]|\r?$)`)
	sequenceItem               = regexp.MustCompile(`^([ \t]+-[ \t]+["']?)([^\s"',;)}:=/\x00]{4,})(["']?,?(?:[ \t]*#[^\n]*)?[ \t]*\r?)$`)
	sequenceDashColumn0        = regexp.MustCompile(`^([ \t]*)-(?:[ \t]|\r?$)`)
	sequenceItemColumn0        = regexp.MustCompile(`^([ \t]*-[ \t]+["']?)([^\s"',;)}:=/\x00]{4,})(["']?,?(?:[ \t]*#[^\n]*)?[ \t]*\r?)$`)
	sequenceCommentItem        = regexp.MustCompile(`^([ \t]+#[ \t]*-[ \t]+["']?)([^\s"',;)}:=/\x00]{4,})(["']?,?(?:[ \t]*#[^\n]*)?[ \t]*\r?)$`)
	sequenceCommentItemColumn0 = regexp.MustCompile(`^([ \t]*#[ \t]*-[ \t]+["']?)([^\s"',;)}:=/\x00]{4,})(["']?,?(?:[ \t]*#[^\n]*)?[ \t]*\r?)$`)
)

// A key at true column 0 cannot be a hunk body, so a leading byte below it is
// never a diff marker - that is what C-L61/C-L85 adjudicated. A key carrying a
// marker is inside a hunk, so every line below it carries one too. An indented
// key is either: its items keep whichever reading makes them items at all.
type sequenceMode int

const (
	rawSequence sequenceMode = iota
	rawPassSequence
	hunkSequence
	hunkPassSequence
	indentedSequence
)

func (m sequenceMode) inHunk() bool { return m == hunkSequence || m == hunkPassSequence }

func (m sequenceMode) passFamily() bool {
	return m == rawPassSequence || m == hunkPassSequence
}

func scrubCredentialSequences(text string, mark func(int) string) string {
	if !strings.Contains(text, "-") {
		return text
	}
	lines := strings.Split(text, "\n")
	inSequence := false
	mode := rawSequence
	itemIndent := -1
	for i, line := range lines {
		if inSequence {
			body := sequenceItemBody(line, mode)
			if strings.TrimRight(body, " \t\r") == "" {
				continue
			}
			marker := line[:len(line)-len(body)]
			dashPattern, itemPattern := sequenceDash, sequenceItem
			commentPattern := sequenceCommentItem
			if mode.passFamily() {
				dashPattern, itemPattern = sequenceDashColumn0, sequenceItemColumn0
				commentPattern = sequenceCommentItemColumn0
			}
			dash := dashPattern.FindStringSubmatch(body)
			if dash != nil && (itemIndent < 0 || len(dash[1]) == itemIndent) {
				itemIndent = len(dash[1])
				lines[i] = marker + applyRule(itemPattern, "${1}"+scrubMark+"${3}", body, mark)
				continue
			}
			if isCommentLine(body) && !credentialSequenceKey(body) {
				lines[i] = marker + applyRule(commentPattern, "${1}"+scrubMark+"${3}", body, mark)
				continue
			}
			inSequence = false
		}
		if m, ok := credentialSequenceKeyLine(line); ok {
			inSequence = true
			mode = m
			itemIndent = -1
		}
	}
	return strings.Join(lines, "\n")
}

// The column the exemption is about is the column in the file, not the column in
// the hunk, so a diff marker is removed before the indent is read. Under an
// indented key the leading byte may be real indentation, so it is kept when
// removing it would leave a line that is not an item at all.
func sequenceItemBody(line string, mode sequenceMode) string {
	if mode == rawSequence || mode == rawPassSequence || line == "" {
		return line
	}
	if c := line[0]; c != '+' && c != '-' && c != ' ' {
		return line
	}
	body := line[1:]
	if mode.inHunk() || sequenceDash.MatchString(body) || !sequenceDash.MatchString(line) {
		return body
	}
	return line
}

func credentialSequenceKeyLine(line string) (sequenceMode, bool) {
	if m := hunkHeading.FindStringSubmatch(line); m != nil {
		if !credentialSequenceKey(m[1]) {
			return rawSequence, false
		}
		if passSequenceKey.MatchString(m[1]) {
			return hunkPassSequence, true
		}
		return hunkSequence, true
	}
	if len(line) > 0 && (line[0] == '+' || line[0] == '-') && credentialSequenceKey(line[1:]) {
		if passSequenceKey.MatchString(line[1:]) {
			return hunkPassSequence, true
		}
		return hunkSequence, true
	}
	if !credentialSequenceKey(line) {
		return rawSequence, false
	}
	if line[0] == ' ' || line[0] == '\t' {
		return indentedSequence, true
	}
	if passSequenceKey.MatchString(line) {
		return rawPassSequence, true
	}
	return rawSequence, true
}

func isCommentLine(line string) bool {
	return strings.HasPrefix(strings.TrimLeft(line, " \t"), "#")
}

func credentialSequenceKey(line string) bool {
	return credentialSequenceKey0.MatchString(line) || passSequenceKey.MatchString(line)
}

// A block scalar puts the value on the indented lines after a "|"/">" header, a
// span no fixed-width regex can bound, so it is walked line by line. RE2 has no
// backreference to tie the body's indent to the header's, which is why the
// comparison is done in code and not in the pattern.
var blockScalarHeaders = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^[+\-]?([ \t]*)(?:.*[^A-Za-z0-9_.\-/])?["']?[A-Za-z0-9_.\-]*(?:` + credentialNames + `)["']?[ \t]*:[ \t]*[|>][+\-0-9]*[ \t]*(?:#[^\n]*)?\r?$`),
	regexp.MustCompile(`(?i)^[+\-]?([ \t]*)(?:.*[^A-Za-z0-9_.\-/])?(?:[A-Za-z0-9]+_)?pass["']?[ \t]*:[ \t]*[|>][+\-0-9]*[ \t]*(?:#[^\n]*)?\r?$`),
}

func scrubBlockScalars(text string, mark func(int) string) string {
	if !strings.ContainsAny(text, "|>") {
		return text
	}
	lines := strings.Split(text, "\n")
	inBlock := false
	headerIndent := 0
	for i, line := range lines {
		if inBlock {
			indent, blank := leadingIndent(line)
			if blank {
				continue
			}
			if indent > headerIndent {
				lines[i] = redactLineBody(line, mark)
				continue
			}
			inBlock = false
		}
		if indent, ok := blockScalarHeader(line); ok {
			inBlock = true
			headerIndent = indent
		}
	}
	return strings.Join(lines, "\n")
}

func blockScalarHeader(line string) (int, bool) {
	for _, header := range blockScalarHeaders {
		if m := header.FindStringSubmatch(line); m != nil {
			return len(m[1]), true
		}
	}
	return 0, false
}

func diffPrefixLen(line string) int {
	if len(line) > 0 && (line[0] == '+' || line[0] == '-') {
		return 1
	}
	return 0
}

func leadingIndent(line string) (int, bool) {
	p := diffPrefixLen(line)
	n := 0
	for p+n < len(line) && (line[p+n] == ' ' || line[p+n] == '\t') {
		n++
	}
	rest := line[p+n:]
	return n, rest == "" || rest == "\r"
}

func redactLineBody(line string, mark func(int) string) string {
	start := diffPrefixLen(line)
	for start < len(line) && (line[start] == ' ' || line[start] == '\t') {
		start++
	}
	end := len(line)
	if end > start && line[end-1] == '\r' {
		end--
	}
	if end <= start {
		return line
	}
	return line[:start] + mark(end-start) + line[end:]
}

// ScrubSecrets removes credential-shaped spans from text ops-pilot did not
// author. It matches shapes and secret-named assignments only, and leaves a
// value too short to be a credential alone: "the variable is empty" is itself
// a diagnosis.
//
// A bare credential with no key name beside it is an accepted risk, not an
// oversight, and no fixed-width rule may be added for one. A Flux revision
// reaches the model as 40 bare hex digits because normalizeRevision has already
// stripped the "sha1:" prefix, and an object reference renders as
// namespace/Kind/name, so any rule wide enough to catch a bare token or a bare
// AWS key also redacts, at some length, the identity of the object that broke.
// The diagnosis forks on exactly those fields, and a redaction there is silent
// and permanent for that object. No channel split separates them either: pod
// listings carry digests and Flux events carry revisions.
//
// Gating such a rule on nearby context does not rescue it. Redacting a 40-byte
// run only when an AWS term sits within 200 bytes was modelled against 1800
// enumerated references: 23% of AWS-adjacent messages lost the reference, and
// Flux Bucket sources, S3-backed Loki and Thanos and velero all produce that
// adjacency in normal operation. Every key-named AWS form is already covered by
// the rules above, so the rule buys only the context-free bare key.
func ScrubSecrets(text string) string {
	if text == "" {
		return text
	}
	text = scrubPasses(normaliseForScrub(text), markOnce)
	return strings.ReplaceAll(text, scrubMark, redactedValue)
}

// Matched as every sink renders it: a rune dropped downstream would close the
// gap it left, rejoining a secret these rules declined because it was split. A
// scrubMark carried in by untrusted text is dropped first so it cannot forge one.
func normaliseForScrub(text string) string {
	return Storable(strings.ReplaceAll(text, scrubMark, ""))
}

// markOnce collapses each redacted span to one scrubMark, which is what the
// public string API renders; markRun preserves the span's byte length so a mask
// built over the same text maps back onto it position for position.
func markOnce(int) string { return scrubMark }

func markRun(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.Repeat(scrubMark, n)
}

func scrubPasses(text string, mark func(int) string) string {
	text = scrubBlockScalars(text, mark)
	text = scrubCredentialSequences(text, mark)
	text = boundPrivateKeyBlocks(text, mark)
	for _, scrubber := range scrubbers {
		text = applyRule(scrubber.pattern, scrubber.replacement, text, mark)
	}
	return text
}

// applyRule replaces the value span of every match of re with mark(spanLen),
// keeping the surrounding captured context. The value span is whatever the
// single scrubMark stands for in the replacement template, located by splitting
// the template rather than searching the expansion, so a scrubMark an earlier
// pass left inside a captured group cannot be mistaken for it. With markOnce this
// reproduces re.ReplaceAllString(template) byte for byte.
func applyRule(re *regexp.Regexp, template, text string, mark func(int) string) string {
	locs := re.FindAllStringSubmatchIndex(text, -1)
	if locs == nil {
		return text
	}
	pre, suf, _ := strings.Cut(template, scrubMark)
	tPre, tSuf := []byte(pre), []byte(suf)
	src := []byte(text)
	var b strings.Builder
	b.Grow(len(text))
	prev := 0
	for _, loc := range locs {
		b.WriteString(text[prev:loc[0]])
		ep := re.Expand(nil, tPre, src, loc)
		es := re.Expand(nil, tSuf, src, loc)
		b.Write(ep)
		b.WriteString(mark((loc[1] - loc[0]) - len(ep) - len(es)))
		b.Write(es)
		prev = loc[1]
	}
	b.WriteString(text[prev:])
	return b.String()
}

// scrubShapeMask marks every byte of the already-normalised text n that a
// credential-shape rule would redact. n must be Storable-normalised, so it holds
// no scrubMark of its own and the length-preserving passes map onto it exactly.
// A length disagreement can only be a bug in the passes, and it fails closed:
// the whole text is reported as a span rather than risk a misaligned mask.
func scrubShapeMask(n string) []bool {
	marked := scrubPasses(n, markRun)
	mask := make([]bool, len(n))
	if len(marked) != len(n) {
		for i := range mask {
			mask[i] = true
		}
		return mask
	}
	for i := 0; i < len(n); i++ {
		if marked[i] == 0 {
			mask[i] = true
		}
	}
	return mask
}
