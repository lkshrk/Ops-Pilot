package repopath

import (
	"fmt"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"
)

// The three predicates this package replaced, verbatim from config, run and github.
func configPlainRepositoryPath(path string) bool {
	if path == "" || len(path) > 4096 || strings.ContainsAny(path, "\x00\n\r") {
		return false
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func runWritablePathBase(path string) bool {
	switch {
	case path == "":
		return false
	case len(path) > 4096:
		return false
	case strings.HasPrefix(path, "/"), strings.HasSuffix(path, "/"):
		return false
	case strings.Contains(path, "//"), strings.ContainsAny(path, "\x00\n\r"):
		return false
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func githubValidPath(path string) bool {
	if path == "" || len(path) > 4096 || strings.HasPrefix(path, "/") || strings.HasSuffix(path, "/") {
		return false
	}
	if strings.Contains(path, "//") || strings.ContainsAny(path, "\x00\n\r") {
		return false
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func TestPlainAnswersAsAllThreePredicatesDid(t *testing.T) {
	corpus := pathProbe()
	if len(corpus) < 14000 {
		t.Fatalf("probe built only %d paths", len(corpus))
	}
	accepted := 0
	for _, path := range corpus {
		got := Plain(path)
		if want := configPlainRepositoryPath(path); got != want {
			t.Errorf("Plain(%s) = %v, config predicate said %v", label(path), got, want)
		}
		if want := runWritablePathBase(path); got != want {
			t.Errorf("Plain(%s) = %v, run predicate said %v", label(path), got, want)
		}
		if want := githubValidPath(path); got != want {
			t.Errorf("Plain(%s) = %v, github predicate said %v", label(path), got, want)
		}
		if got {
			accepted++
		}
	}
	if accepted == 0 || accepted == len(corpus) {
		t.Fatalf("probe is degenerate: %d of %d accepted", accepted, len(corpus))
	}
}

func TestCheckNamesTheFaultCallersReportOn(t *testing.T) {
	for _, c := range []struct {
		path string
		want Fault
	}{
		{"", Empty},
		{strings.Repeat("a", MaxLength), OK},
		{strings.Repeat("a", MaxLength+1), TooLong},
		{"/" + strings.Repeat("a", MaxLength+1), TooLong},
		{strings.Repeat("a", MaxLength+1) + "/", TooLong},
		{"a//" + strings.Repeat("a", MaxLength+1), TooLong},
		{strings.Repeat("é", 2049), TooLong},
		{strings.Repeat("é", 2048), OK},
		{"a/\xffb/c", OK},
		{"\xff", OK},
		{"/a", NotRelative},
		{"a/", NotRelative},
		{"/", NotRelative},
		{"a//b", NotPlain},
		{"a\x00b", NotPlain},
		{"a\nb", NotPlain},
		{"a\rb", NotPlain},
		{"a/../b", escapes},
		{"a/./b", escapes},
		{"..", escapes},
		{"a/b.yaml", OK},
		{".git/config", OK},
		{"a/...", OK},
	} {
		if got := Check(c.path); got != c.want {
			t.Errorf("Check(%s) = %v, want %v", label(c.path), got, c.want)
		}
	}
}

func label(path string) string {
	if len(path) <= 32 {
		return fmt.Sprintf("%q", path)
	}
	return fmt.Sprintf("%q...(%d bytes, %d runes)", path[:16], len(path), utf8.RuneCountInString(path))
}

func pathProbe() []string {
	alphabet := []string{"", "a", ".", "..", "...", "/", "\x00", "\t", "\n", " ", ".git", "b"}
	seen := map[string]bool{}
	var corpus []string
	add := func(p string) {
		if !seen[p] {
			seen[p] = true
			corpus = append(corpus, p)
		}
	}
	words := [][]string{{}}
	for depth := 0; depth < 4; depth++ {
		var next [][]string
		for _, word := range words {
			for _, token := range alphabet {
				grown := append(append([]string{}, word...), token)
				next = append(next, grown)
				add(strings.Join(grown, "/"))
			}
		}
		words = next
	}
	for _, n := range []int{MaxLength - 2, MaxLength - 1, MaxLength, MaxLength + 1} {
		add(strings.Repeat("a", n))
		add("a/" + strings.Repeat("b", n-2))
		add(strings.Repeat("a/", n/2))
	}
	for _, extra := range []string{
		"café/x", "a\u2028b", "a\u2029b", "a\u00a0b", "\uFEFFa", "a\\b", "C:/x", "a/./b", "a/../b",
		"a/.git/b", ".github/workflows/x.yml", "a/.GIT/b", "a/..b/c", "a/b..", "\v", "\f", "\x7f",
		"clusters/prod/apps/podinfo.yaml", "flux-system/gotk-sync.yaml", "kustomization.yaml",
		"/" + strings.Repeat("a", MaxLength+1), strings.Repeat("a", MaxLength+1) + "/",
		"a//" + strings.Repeat("a", MaxLength+1), strings.Repeat("é", 2048), strings.Repeat("é", 2049),
		"/" + strings.Repeat("é", 2049), "a/\xffb/c", "\xff", "\xff/..", "\xc3(/a",
	} {
		add(extra)
	}
	sort.Strings(corpus)
	return corpus
}
