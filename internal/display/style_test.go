package display

import (
	"strings"
	"testing"
)

func TestStylePaintsOnlyWhenEnabled(t *testing.T) {
	plain := Style{colour: false}
	if got := plain.Good("ok"); got != "ok" {
		t.Fatalf("piped output must stay plain, got %q", got)
	}
	coloured := Style{colour: true}
	got := coloured.Good("ok")
	if !strings.HasPrefix(got, green) || !strings.HasSuffix(got, reset) {
		t.Fatalf("want the value wrapped in colour, got %q", got)
	}
}

func TestStyleLeavesEmptyValuesAlone(t *testing.T) {
	if got := (Style{colour: true}).Detail(""); got != "" {
		t.Fatalf("want no escape codes around nothing, got %q", got)
	}
}

func TestDiffColoursEveryKindOfLine(t *testing.T) {
	const diff = "--- a/apps/sonarr.yaml\n" +
		"+++ b/apps/sonarr.yaml\n" +
		"@@ -1,2 +1,2 @@\n" +
		"-image: sonarr:1.0\n" +
		"+image: sonarr:1.1\n" +
		" replicas: 1\n"
	want := dim + "--- a/apps/sonarr.yaml" + reset + "\n" +
		dim + "+++ b/apps/sonarr.yaml" + reset + "\n" +
		bold + "@@ -1,2 +1,2 @@" + reset + "\n" +
		red + "-image: sonarr:1.0" + reset + "\n" +
		green + "+image: sonarr:1.1" + reset + "\n" +
		" replicas: 1\n"

	if got := (Style{colour: true}).Diff(diff); got != want {
		t.Fatalf("diff colours differ\n got: %q\nwant: %q", got, want)
	}
}

func TestDiffLeavesRedirectedOutputByteIdentical(t *testing.T) {
	const diff = "--- a/apps/sonarr.yaml\n+++ b/apps/sonarr.yaml\n" +
		"@@ -1 +1 @@\n-image: sonarr:1.0\n+image: sonarr:1.1\n"

	if got := (Style{colour: false}).Diff(diff); got != diff {
		t.Fatalf("want the diff unchanged, got %q", got)
	}
}

func TestDiffEndsOnOneNewlineWhateverItWasGiven(t *testing.T) {
	for _, given := range []string{"-old\n+new", "-old\n+new\n", "-old\n+new\n\n\n"} {
		if got := (Style{colour: false}).Diff(given); got != "-old\n+new\n" {
			t.Fatalf("want one trailing newline for %q, got %q", given, got)
		}
	}
}
