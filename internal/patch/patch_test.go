package patch_test

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/lkshrk/ops-pilot/internal/patch"
)

const twoReleases = `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: sonarr
spec:
  values:
    replicas: 1
---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: radarr
spec:
  values:
    replicas: 1
`

const threeReleases = twoReleases + `---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: lidarr
spec:
  values:
    replicas: 1
`

const helmRelease = `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: sonarr
spec:
  values:
    env:
      TZ: Europe/Berlin
`

func pathsOf(files []patch.File) []string {
	paths := make([]string, 0, len(files))
	for _, file := range files {
		path := file.NewPath
		if path == "" {
			path = file.OldPath
		}
		if path != "" {
			paths = append(paths, path)
		}
	}
	return paths
}

func TestApplyRenamesAKey(t *testing.T) {
	diff := `--- a/kubernetes/apps/media/sonarr/helmrelease.yaml
+++ b/kubernetes/apps/media/sonarr/helmrelease.yaml
@@ -5,5 +5,5 @@ spec:
 spec:
   values:
-    env:
+    envs:
       TZ: Europe/Berlin
`
	files, err := patch.Parse(diff)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(files) != 1 || files[0].NewPath != "kubernetes/apps/media/sonarr/helmrelease.yaml" {
		t.Fatalf("unexpected file section: %+v", files)
	}

	got, err := patch.Apply([]byte(helmRelease), files[0])
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !strings.Contains(string(got), "    envs:") || strings.Contains(string(got), "    env:\n") {
		t.Fatalf("key was not renamed:\n%s", got)
	}
	if !strings.HasSuffix(string(got), "\n") {
		t.Fatal("trailing newline was lost")
	}
}

func TestApplyFindsContextThatMovedFromTheStatedLine(t *testing.T) {
	shifted := "# a comment\n# another\n" + helmRelease
	diff := `--- a/x.yaml
+++ b/x.yaml
@@ -5,3 +5,3 @@
   values:
-    env:
+    envs:
`
	files, err := patch.Parse(diff)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := patch.Apply([]byte(shifted), files[0])
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !strings.Contains(string(got), "    envs:") {
		t.Fatalf("shifted context was not located:\n%s", got)
	}
}

func TestApplyRefusesAHunkWhoseContextIsAbsent(t *testing.T) {
	diff := `--- a/x.yaml
+++ b/x.yaml
@@ -1,1 +1,1 @@
-nothing like this exists
+replacement
`
	files, err := patch.Parse(diff)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, err := patch.Apply([]byte(helmRelease), files[0]); err == nil {
		t.Fatal("want a refusal rather than a guessed application")
	}
}

func TestApplyRefusesAHunkWhoseContextMatchesTwice(t *testing.T) {
	diff := `--- a/releases.yaml
+++ b/releases.yaml
@@ -1,3 +1,3 @@
 spec:
   values:
-    replicas: 1
+    replicas: 2
`
	files, err := patch.Parse(diff)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := patch.Apply([]byte(twoReleases), files[0])
	if err == nil {
		t.Fatalf("weak context matching two objects must be refused, not resolved to the nearest one:\n%s", got)
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("the refusal must name the ambiguity so the agent can add context: %v", err)
	}
}

// Nothing asks the agent for accurate line numbers and the repository files it
// reads carry none, so a header that happens to land on one of several matches
// is a coincidence, not a choice between them.
func TestARepeatingContextIsRefusedEvenWhenTheHeaderLandsOnOneOfTheMatches(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header string
	}{
		{name: "the header lands on the first block", header: "@@ -5,3 +5,3 @@"},
		{name: "the header lands on the second block", header: "@@ -13,3 +13,3 @@"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			diff := "--- a/releases.yaml\n+++ b/releases.yaml\n" + tc.header + `
 spec:
   values:
-    replicas: 1
+    replicas: 2
`
			files, err := patch.Parse(diff)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			got, err := patch.Apply([]byte(twoReleases), files[0])
			if err == nil {
				t.Fatalf("an off-by-one-block miscount lands on a real match, so the position cannot pick the object:\n%s", got)
			}
			if !strings.Contains(err.Error(), "ambiguous") {
				t.Fatalf("the refusal must name the ambiguity so the agent can add context: %v", err)
			}
		})
	}
}

func TestARepeatingContextAppliesOnceTheAgentAddsALineThatOccursOnce(t *testing.T) {
	diff := `--- a/releases.yaml
+++ b/releases.yaml
@@ -12,4 +12,4 @@
   name: radarr
 spec:
   values:
-    replicas: 1
+    replicas: 2
`
	files, err := patch.Parse(diff)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := patch.Apply([]byte(twoReleases), files[0])
	if err != nil {
		t.Fatalf("the context names the object, so there is nothing to guess: %v", err)
	}
	sonarr, radarr, found := strings.Cut(string(got), "---\n")
	if !found {
		t.Fatalf("both documents must survive:\n%s", got)
	}
	if !strings.Contains(sonarr, "    replicas: 1") {
		t.Fatalf("the object the operator did not approve a change to was modified:\n%s", sonarr)
	}
	if !strings.Contains(radarr, "    replicas: 2") {
		t.Fatalf("the approved object was not changed:\n%s", radarr)
	}
}

func TestAUniqueContextAppliesHoweverFarFromTheStatedLine(t *testing.T) {
	diff := `--- a/values.yaml
+++ b/values.yaml
@@ -1,1 +1,1 @@
-    image: repo/app:1.0.0
+    image: repo/app:1.1.0
`
	files, err := patch.Parse(diff)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, tc := range []struct {
		name   string
		before int
		after  int
	}{
		{name: "a few lines of drift", before: 50, after: 50},
		{name: "a placeholder header on a file too long to count by hand", before: 800, after: 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var file strings.Builder
			for i := range tc.before {
				fmt.Fprintf(&file, "# filler %d\n", i)
			}
			file.WriteString("    image: repo/app:1.0.0\n")
			for i := range tc.after {
				fmt.Fprintf(&file, "# tail %d\n", i)
			}

			got, err := patch.Apply([]byte(file.String()), files[0])
			if err != nil {
				t.Fatalf("the context occurs once in the file, so there is no other object it could mean: %v", err)
			}
			if !strings.Contains(string(got), "    image: repo/app:1.1.0") {
				t.Fatalf("the fix was not applied:\n%s", got)
			}
		})
	}
}

func TestIdenticalBlocksAreRefusedEvenWhenEveryBlockIsBeingFixed(t *testing.T) {
	diff := `--- a/releases.yaml
+++ b/releases.yaml
@@ -3,3 +3,3 @@
 spec:
   values:
-    replicas: 1
+    replicas: 2
@@ -11,3 +11,3 @@
 spec:
   values:
-    replicas: 1
+    replicas: 2
`
	files, err := patch.Parse(diff)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := patch.Apply([]byte(twoReleases), files[0])
	if err == nil {
		t.Fatalf("the two hunks are identical, so which block each one means is a guess:\n%s", got)
	}
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("the refusal must name the ambiguity so the agent can add context: %v", err)
	}
}

// The refusal is the whole of the agent's next turn: it comes back as
// RejectedFix and there is one retry left before the merge is reverted. A
// message that lets it retry by correcting the line number spends that retry on
// something that can no longer work.
func TestTheAmbiguityRefusalNamesEveryMatchAndRulesOutCorrectingThePosition(t *testing.T) {
	diff := `--- a/releases.yaml
+++ b/releases.yaml
@@ -5,3 +5,3 @@
 spec:
   values:
-    replicas: 1
+    replicas: 2
`
	files, err := patch.Parse(diff)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := patch.Apply([]byte(threeReleases), files[0])
	if err == nil {
		t.Fatalf("one hunk against three identical blocks names no object:\n%s", got)
	}
	if !strings.Contains(err.Error(), "lines 5, 13 and 21") {
		t.Fatalf("every block the context matches must be named, or the agent cannot see the structure repeats: %v", err)
	}
	if !strings.Contains(err.Error(), "the stated position does not choose between them") {
		t.Fatalf("a retry that only corrects the header cannot succeed, so the refusal must say so: %v", err)
	}
	if !strings.Contains(err.Error(), "appears once") {
		t.Fatalf("the refusal must say what would make the hunk applicable: %v", err)
	}
}

func TestTheAmbiguityRefusalCountsMatchesItDoesNotList(t *testing.T) {
	var file strings.Builder
	for range 40 {
		file.WriteString("    replicas: 1\n")
	}
	files, err := patch.Parse(`--- a/releases.yaml
+++ b/releases.yaml
@@ -1,1 +1,1 @@
-    replicas: 1
+    replicas: 2
`)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, err = patch.Apply([]byte(file.String()), files[0])
	if err == nil {
		t.Fatal("forty matches is not a unique context")
	}
	if !strings.Contains(err.Error(), "40 places") {
		t.Fatalf("the total is what tells the agent the context is hopeless, so it must survive truncation: %v", err)
	}
	if len(err.Error()) > 400 {
		t.Fatalf("this error is fed back into a prompt; it must not carry the whole file: %v", err)
	}
}

func TestApplyRefusesAContextFreeInsertionIntoAnExistingFile(t *testing.T) {
	diff := `--- a/x.yaml
+++ b/x.yaml
@@ -4,0 +5,1 @@
+  namespace: media
`
	files, err := patch.Parse(diff)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := patch.Apply([]byte(helmRelease), files[0])
	if err == nil {
		t.Fatalf("a hunk with nothing to match against places its lines by arithmetic alone:\n%s", got)
	}
}

func TestParseHandlesADeletion(t *testing.T) {
	diff := `--- a/gone.yaml
+++ /dev/null
@@ -1,1 +0,0 @@
-content
`
	files, err := patch.Parse(diff)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !files[0].Deleted {
		t.Fatalf("want a deletion, got %+v", files[0])
	}
	got, err := patch.Apply([]byte("content\n"), files[0])
	if err != nil || got != nil {
		t.Fatalf("want empty contents for a deletion, got %q %v", got, err)
	}
}

func TestApplyRefusesADeletionWhoseHunkDoesNotMatchTheFile(t *testing.T) {
	diff := `--- a/kubernetes/apps/media/sonarr/helmrelease.yaml
+++ /dev/null
@@ -1,2 +0,0 @@
-apiVersion: something.else/v1
-kind: SomethingElse
`
	files, err := patch.Parse(diff)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !files[0].Deleted {
		t.Fatalf("want a deletion, got %+v", files[0])
	}
	if _, err := patch.Apply([]byte(helmRelease), files[0]); err == nil {
		t.Fatal("want a refusal: a deletion whose hunk describes other contents must not remove the file")
	}
}

func TestApplyHonoursADeletionWithNoHunks(t *testing.T) {
	diff := `--- a/gone.yaml
+++ /dev/null
`
	files, err := patch.Parse(diff)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got, err := patch.Apply([]byte(helmRelease), files[0])
	if err != nil || got != nil {
		t.Fatalf("a hunkless deletion is legitimate shorthand, got %q %v", got, err)
	}
}

// A model writing a diff by hand routinely miscounts the header while the
// content is correct. The body is the truth, as it is for git apply and patch;
// context matching is what makes a wrong hunk impossible to apply.
func TestApplyIgnoresMiscountedHunkHeaders(t *testing.T) {
	diff := `--- a/x.yaml
+++ b/x.yaml
@@ -4,1 +4,1 @@
   values:
-    env:
+    envs:
       TZ: Europe/Berlin
`
	files, err := patch.Parse(diff)
	if err != nil {
		t.Fatalf("a miscounted header must not fail the parse: %v", err)
	}
	got, err := patch.Apply([]byte(helmRelease), files[0])
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !strings.Contains(string(got), "    envs:") {
		t.Fatalf("the fix was not applied:\n%s", got)
	}
}

func TestParseRejectsAHunkWithNoBody(t *testing.T) {
	if _, err := patch.Parse("--- a/x\n+++ b/x\n@@ -1,1 +1,1 @@\n"); err == nil {
		t.Fatal("want an error for a hunk with nothing in it")
	}
}

func TestParseAcceptsAHunkHeaderThatOmitsTheCount(t *testing.T) {
	diff := `--- a/x.yaml
+++ b/x.yaml
@@ -7 +7 @@
-    env:
+    envs:
`
	files, err := patch.Parse(diff)
	if err != nil {
		t.Fatalf("an omitted count means one line on each side: %v", err)
	}
	got, err := patch.Apply([]byte(helmRelease), files[0])
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !strings.Contains(string(got), "    envs:") {
		t.Fatalf("key was not renamed:\n%s", got)
	}
}

func TestApplyHonoursTheNoNewlineMarker(t *testing.T) {
	t.Run("patch removes the terminal newline", func(t *testing.T) {
		diff := `--- a/x.yaml
+++ b/x.yaml
@@ -8,1 +8,1 @@
-      TZ: Europe/Berlin
+      TZ: UTC
\ No newline at end of file
`
		files, err := patch.Parse(diff)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		got, err := patch.Apply([]byte(helmRelease), files[0])
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		if strings.HasSuffix(string(got), "\n") {
			t.Fatalf("marker on the added line must strip the terminal newline:\n%q", got)
		}
		if !strings.HasSuffix(string(got), "      TZ: UTC") {
			t.Fatalf("unexpected contents:\n%q", got)
		}
	})

	t.Run("patch adds the terminal newline", func(t *testing.T) {
		original := strings.TrimSuffix(helmRelease, "\n")
		diff := `--- a/x.yaml
+++ b/x.yaml
@@ -8,1 +8,1 @@
-      TZ: Europe/Berlin
\ No newline at end of file
+      TZ: UTC
`
		files, err := patch.Parse(diff)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		got, err := patch.Apply([]byte(original), files[0])
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		if !strings.HasSuffix(string(got), "      TZ: UTC\n") {
			t.Fatalf("marker on the removed line only: the result must end with a newline:\n%q", got)
		}
	})

	t.Run("created file without a terminal newline", func(t *testing.T) {
		diff := `--- /dev/null
+++ b/new.yaml
@@ -0,0 +1,1 @@
+created
\ No newline at end of file
`
		files, err := patch.Parse(diff)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		got, err := patch.Apply(nil, files[0])
		if err != nil {
			t.Fatalf("apply: %v", err)
		}
		if string(got) != "created" {
			t.Fatalf("a created file must not be forced to end with a newline: %q", got)
		}
	})
}

func TestParseHandlesACreationAndMultipleFiles(t *testing.T) {
	diff := `diff --git a/one.yaml b/one.yaml
--- a/one.yaml
+++ b/one.yaml
@@ -1,1 +1,1 @@
-old
+new
diff --git a/two.yaml b/two.yaml
--- /dev/null
+++ b/two.yaml
@@ -0,0 +1,1 @@
+created
`
	files, err := patch.Parse(diff)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("want two file sections, got %d", len(files))
	}
	if !files[1].Created {
		t.Fatalf("want the second file marked created: %+v", files[1])
	}
	created, err := patch.Apply(nil, files[1])
	if err != nil {
		t.Fatalf("apply creation: %v", err)
	}
	if string(created) != "created\n" {
		t.Fatalf("created contents: %q", created)
	}
	if paths := pathsOf(files); strings.Join(paths, ",") != "one.yaml,two.yaml" {
		t.Fatalf("paths: %v", paths)
	}
}

func TestParseRefusesAHunkWhoseBodyIsInterruptedByANonDiffLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		diff string
	}{
		{
			name: "narrative between a removal and its replacement",
			diff: `--- a/x.yaml
+++ b/x.yaml
@@ -6,3 +6,3 @@
   values:
-    env:
(rename the key here)
+    envs:
`,
		},
		{
			name: "narrative before the second half of the fix",
			diff: `--- a/x.yaml
+++ b/x.yaml
@@ -5,5 +5,5 @@
 spec:
   values:
-    env:
+    envs:
and keep the value
       TZ: Europe/Berlin
`,
		},
		{
			name: "interrupted hunk followed by a second file section",
			diff: `--- a/x.yaml
+++ b/x.yaml
@@ -6,3 +6,3 @@
   values:
...
-    env:
+    envs:
--- a/y.yaml
+++ b/y.yaml
@@ -1,1 +1,1 @@
-old
+new
`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := patch.Parse(tc.diff); err == nil {
				t.Fatal("a hunk body the parser cannot read to its end must be refused, not silently truncated")
			}
		})
	}
}

// The parser has to tolerate whatever the model wraps its diff in, or a fenced
// answer is refused twice and a merge is reverted over punctuation.
func TestParseReadsADiffThatEndsInSomethingOtherThanABodyLine(t *testing.T) {
	for _, tc := range []struct {
		name string
		diff string
	}{
		{
			name: "code fence",
			diff: "```diff\n--- a/x.yaml\n+++ b/x.yaml\n@@ -6,3 +6,3 @@\n   values:\n-    env:\n+    envs:\n```\n",
		},
		{
			name: "code fence and a closing sentence",
			diff: "```diff\n--- a/x.yaml\n+++ b/x.yaml\n@@ -6,3 +6,3 @@\n   values:\n-    env:\n+    envs:\n```\n\nThe chart renamed the key in v4.\n",
		},
		{
			name: "trailing prose alone",
			diff: "--- a/x.yaml\n+++ b/x.yaml\n@@ -6,3 +6,3 @@\n   values:\n-    env:\n+    envs:\nThe chart renamed the key in v4.\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files, err := patch.Parse(tc.diff)
			if err != nil {
				t.Fatalf("nothing follows the body, so nothing was swallowed: %v", err)
			}
			got, err := patch.Apply([]byte(helmRelease), files[0])
			if err != nil {
				t.Fatalf("apply: %v", err)
			}
			if !strings.Contains(string(got), "    envs:") {
				t.Fatalf("the fix was not applied:\n%s", got)
			}
		})
	}
}

func TestParseRejectsTextThatIsNotADiff(t *testing.T) {
	if _, err := patch.Parse("I think you should rename the key."); err == nil {
		t.Fatal("want an error for prose")
	}
}

// The hunk body ends at the next hunk or file section, and a line of narration
// is neither however it begins. Breaking on the "diff " or "--- " prefix alone
// let a sentence end the body early: the outer loop then ignored every body
// line after it, and the shortened hunk applied.
func TestNarrationThatBeginsLikeASectionDoesNotEndTheHunkBody(t *testing.T) {
	for _, tc := range []struct {
		name string
		diff string
	}{
		{
			name: "narration beginning \"diff \"",
			diff: "--- a/x.yaml\n+++ b/x.yaml\n@@ -1,8 +1,8 @@\n   values:\n-    env:\n+    envs:\ndiff continues below\n-      TZ: Europe/Berlin\n+      TZ: UTC\n",
		},
		{
			name: "narration beginning \"--- \"",
			diff: "--- a/x.yaml\n+++ b/x.yaml\n@@ -1,8 +1,8 @@\n   values:\n-    env:\n+    envs:\n--- and now the timezone\n-      TZ: Europe/Berlin\n+      TZ: UTC\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files, err := patch.Parse(tc.diff)
			if err != nil {
				return
			}
			got, err := patch.Apply([]byte(helmRelease), files[0])
			if err != nil {
				return
			}
			if !strings.Contains(string(got), "TZ: UTC") {
				t.Fatalf("the second half of the approved fix was swallowed and the first half committed anyway:\n%s", got)
			}
		})
	}
}

// A file header the model got slightly wrong used to be skipped in silence,
// leaving the previous file current: the next hunk was attributed to it,
// applied to it, and the file the hunk actually named never reached the path
// gate at all. Nothing downstream can notice, because the parse it is handed
// says the diff only ever touched one file.
func TestAHunkAfterAnIncompleteFileHeaderIsRefusedRatherThanAppliedToThePreviousFile(t *testing.T) {
	for _, tc := range []struct {
		name string
		diff string
	}{
		{
			name: "the +++ line lost the space after its marker",
			diff: "--- a/x.yaml\n+++ b/x.yaml\n@@ -6,3 +6,3 @@\n   values:\n-    env:\n+    envs:\n--- a/y.yaml\n+++b/y.yaml\n@@ -1,2 +1,2 @@\n-      TZ: Europe/Berlin\n+      TZ: UTC\n",
		},
		{
			name: "a git header with no ---/+++ pair under it",
			diff: "--- a/x.yaml\n+++ b/x.yaml\n@@ -6,3 +6,3 @@\n   values:\n-    env:\n+    envs:\ndiff --git a/y.yaml b/y.yaml\n@@ -1,2 +1,2 @@\n-      TZ: Europe/Berlin\n+      TZ: UTC\n",
		},
		{
			name: "the --- line is missing entirely",
			diff: "--- a/x.yaml\n+++ b/x.yaml\n@@ -6,3 +6,3 @@\n   values:\n-    env:\n+    envs:\n+++ b/y.yaml\n@@ -1,2 +1,2 @@\n-      TZ: Europe/Berlin\n+      TZ: UTC\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files, err := patch.Parse(tc.diff)
			if err != nil {
				return
			}
			for _, file := range files {
				if len(file.Hunks) > 1 {
					t.Fatalf("%s was given a hunk belonging to another file: %+v", file.NewPath, file.Hunks)
				}
			}
			if paths := pathsOf(files); !slices.Contains(paths, "y.yaml") {
				t.Fatalf("y.yaml is written but never named, so no path gate can refuse it: %v", paths)
			}
		})
	}
}

// The same prefixes still have to work when they really do start a section, or
// a legitimate multi-file diff is refused.
func TestASectionStillEndsTheHunkBody(t *testing.T) {
	diff := "diff --git a/one.yaml b/one.yaml\nindex 1234567..89abcde 100644\n--- a/one.yaml\n+++ b/one.yaml\n@@ -1,1 +1,1 @@\n-old\n+new\ndiff --git a/two.yaml b/two.yaml\nindex 0000000..1111111 100644\n--- a/two.yaml\n+++ b/two.yaml\n@@ -1,1 +1,1 @@\n-old\n+new\n"
	files, err := patch.Parse(diff)
	if err != nil {
		t.Fatalf("git's own output must parse: %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("want two file sections, got %d: %+v", len(files), files)
	}
	for _, file := range files {
		if len(file.Hunks) != 1 {
			t.Fatalf("%s: each file owns exactly one hunk here, got %d", file.NewPath, len(file.Hunks))
		}
	}
	if paths := pathsOf(files); strings.Join(paths, ",") != "one.yaml,two.yaml" {
		t.Fatalf("both paths must survive for the repair gate to see them: %v", paths)
	}
}

// git never writes anything between a section's +++ line and its @@, but a
// model hand-writing a multi-file diff drops in a stray extended-header line
// (index, mode, rename), and the section must still start rather than be
// swallowed into the previous hunk's body. A body line can never carry those
// markerless prefixes, so stepping over them cannot misread an in-body pair.
// A stray line between a file-header pair and its @@ is NOT stepped over: any
// line class the parser skips there (blank, "index ", "mode ", "rename from ")
// can be forged by a context line whose leading space the model dropped and
// whose content begins with that marker, splitting an in-body pair into a
// phantom file that silently swallows the next hunk (C-L72/C-M63). Since a real
// second section and that forged bridge are byte-indistinguishable, the section
// must fail closed here — a recoverable refusal, never a silent wrong write.
func TestAStrayLineBetweenAHeaderAndItsHunkFailsClosedNotAPhantomSplit(t *testing.T) {
	for _, tc := range []struct{ name, diff string }{
		{
			name: "a stray index line",
			diff: "--- a/one.yaml\n+++ b/one.yaml\n@@ -1,1 +1,1 @@\n-old\n+new\n--- a/two.yaml\n+++ b/two.yaml\nindex 1234567..89abcde 100644\n@@ -1,1 +1,1 @@\n-old\n+new\n",
		},
		{
			name: "a stray mode line",
			diff: "--- a/one.yaml\n+++ b/one.yaml\n@@ -1,1 +1,1 @@\n-old\n+new\n--- a/two.yaml\n+++ b/two.yaml\nnew mode 100644\n@@ -1,1 +1,1 @@\n-old\n+new\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files, _ := patch.Parse(tc.diff)
			// The second section must not become an independent applied file: the
			// stray line keeps it inside the first section, so the diff carries at
			// most the one section that started cleanly and the rest fails closed.
			if paths := pathsOf(files); strings.Join(paths, ",") == "one.yaml,two.yaml" {
				t.Fatalf("pathsOf() = %v: the stray line was stepped over and the "+
					"second section started — the unsafe relaxation is back", paths)
			}
		})
	}
}

// git puts a context line between an in-body double-marker pair and the next
// hunk, and `git diff -U1` on a hunk that changes a "-- x" line and ends on a
// blank trailing-context line emits "--- x"/"+++ y"/" "/@@ verbatim. Once a
// model drops that context line's leading space it is a bare blank between the
// pair and a @@ — which must stay a change, not split into a phantom file that
// swallows the next hunk's insertions.
func TestABlankAfterAnInBodyHeaderPairDoesNotSplitItIntoAPhantomFile(t *testing.T) {
	const original = "alpha\nmid\n-- note\n\ng5\ng6\ng7\ng8\ng9\ng10\ng11\ng12\n"
	diff := "--- a/db.sql\n+++ b/db.sql\n@@ -1,4 +1,4 @@\n-alpha\n+ALPHA\n mid\n--- note\n+++ note\n\n@@ -12 +12,3 @@ g11\n g12\n+Z1\n+Z2\n"

	files, err := patch.Parse(diff)
	if err != nil {
		return
	}
	if len(files) != 1 {
		t.Fatalf("Parse() = %d files %v, want 1: the in-body pair was split into a phantom that swallows the next hunk", len(files), pathsOf(files))
	}
	out, err := patch.Apply([]byte(original), files[0])
	if err != nil {
		return
	}
	if !strings.Contains(string(out), "++ note") {
		t.Fatalf("the -- note change was dropped and reported applied: %q", out)
	}
	if !strings.Contains(string(out), "Z1") || !strings.Contains(string(out), "Z2") {
		t.Fatalf("the second hunk's insertions were dropped: %q", out)
	}
}

// A blank line inside a hunk is a context line the model stripped the leading
// space from. A blank line after the body is not: it is the gap before the
// answer's closing prose, or the diff's own trailing newline. Reading the
// second as the first invents a context line the file does not contain and
// refuses a diff that was correct.
func TestABlankLineIsAContextLineOnlyWhileTheHunkBodyContinues(t *testing.T) {
	const withAnInternalBlank = "a: 1\nb: 2\n\nc: 3\n"
	for _, tc := range []struct {
		name     string
		diff     string
		original string
		want     string
	}{
		{
			name:     "a blank separating the body from a closing sentence",
			diff:     "--- a/x.yaml\n+++ b/x.yaml\n@@ -6,3 +6,3 @@\n   values:\n-    env:\n+    envs:\n\nThe chart renamed the key in v4.\n",
			original: helmRelease,
			want:     "apiVersion: helm.toolkit.fluxcd.io/v2\nkind: HelmRelease\nmetadata:\n  name: sonarr\nspec:\n  values:\n    envs:\n      TZ: Europe/Berlin\n",
		},
		{
			name:     "a blank between two hunks",
			diff:     "--- a/x.yaml\n+++ b/x.yaml\n@@ -7,2 +7,2 @@\n-    env:\n+    envs:\n\n@@ -8,1 +8,1 @@\n-      TZ: Europe/Berlin\n+      TZ: UTC\n",
			original: helmRelease,
			want:     "apiVersion: helm.toolkit.fluxcd.io/v2\nkind: HelmRelease\nmetadata:\n  name: sonarr\nspec:\n  values:\n    envs:\n      TZ: UTC\n",
		},
		{
			name:     "a blank that is the hunk's last context line",
			diff:     "--- a/x.yaml\n+++ b/x.yaml\n@@ -1,3 +1,3 @@\n-k: v\n+k: vv\n b: 2\n\n",
			original: "k: v\nb: 2\n\nk: v\nb: 2\nz: 9\n",
			want:     "k: vv\nb: 2\n\nk: v\nb: 2\nz: 9\n",
		},
		{
			name:     "a blank the model stripped the leading space from, with body after it",
			diff:     "--- a/x.yaml\n+++ b/x.yaml\n@@ -1,4 +1,4 @@\n a: 1\n-b: 2\n+b: 22\n\n-c: 3\n+c: 33\n",
			original: withAnInternalBlank,
			want:     "a: 1\nb: 22\n\nc: 33\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files, err := patch.Parse(tc.diff)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			got, err := patch.Apply([]byte(tc.original), files[0])
			if err != nil {
				t.Fatalf("the hunk describes the file exactly, so there is nothing to refuse: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("got:\n%q\nwant:\n%q", got, tc.want)
			}
		})
	}
}

// A stray line at the very end of a hunk body leaves the hunk short without
// swallowing any body, so nothing but the declared @@ counts could detect it —
// and this measurement is why they are not used for it. Every diff below is one
// this package asserts elsewhere must parse and apply, and a third of their
// hunks declare counts their own bodies contradict. Refusing on that mismatch
// would reject roughly one correct hunk in three, spending the agent's single
// retry on arithmetic; the truncation it would catch costs a short context,
// which locate() then has to find uniquely anyway.
func TestDeclaredHunkCountsContradictWellFormedBodiesTooOftenToRefuseOn(t *testing.T) {
	corpus := []struct{ name, diff string }{
		{"renames a key", "--- a/x\n+++ b/x\n@@ -5,5 +5,5 @@ spec:\n spec:\n   values:\n-    env:\n+    envs:\n       TZ: Europe/Berlin\n"},
		{"context that moved from the stated line", "--- a/x\n+++ b/x\n@@ -5,3 +5,3 @@\n   values:\n-    env:\n+    envs:\n"},
		{"a context that matches twice", "--- a/x\n+++ b/x\n@@ -1,3 +1,3 @@\n spec:\n   values:\n-    replicas: 1\n+    replicas: 2\n"},
		{"a context made unique by naming the object", "--- a/x\n+++ b/x\n@@ -12,4 +12,4 @@\n   name: radarr\n spec:\n   values:\n-    replicas: 1\n+    replicas: 2\n"},
		{"a placeholder header on a long file", "--- a/x\n+++ b/x\n@@ -1,1 +1,1 @@\n-    image: repo/app:1.0.0\n+    image: repo/app:1.1.0\n"},
		{"a deletion", "--- a/gone\n+++ /dev/null\n@@ -1,1 +0,0 @@\n-content\n"},
		{"a header the model miscounted", "--- a/x\n+++ b/x\n@@ -4,1 +4,1 @@\n   values:\n-    env:\n+    envs:\n       TZ: Europe/Berlin\n"},
		{"a header that omits the count", "--- a/x\n+++ b/x\n@@ -7 +7 @@\n-    env:\n+    envs:\n"},
		{"a no-newline marker", "--- a/x\n+++ b/x\n@@ -8,1 +8,1 @@\n-      TZ: Europe/Berlin\n+      TZ: UTC\n\\ No newline at end of file\n"},
		{"a creation", "--- /dev/null\n+++ b/two.yaml\n@@ -0,0 +1,1 @@\n+created\n"},
		{"a fenced answer", "```diff\n--- a/x\n+++ b/x\n@@ -6,3 +6,3 @@\n   values:\n-    env:\n+    envs:\n```\n"},
		{"a fenced answer with a closing sentence", "```diff\n--- a/x\n+++ b/x\n@@ -6,3 +6,3 @@\n   values:\n-    env:\n+    envs:\n```\n\nThe chart renamed the key in v4.\n"},
		{"trailing prose alone", "--- a/x\n+++ b/x\n@@ -6,3 +6,3 @@\n   values:\n-    env:\n+    envs:\nThe chart renamed the key in v4.\n"},
	}

	var total int
	var contradicted []string
	for _, tc := range corpus {
		files, err := patch.Parse(tc.diff)
		if err != nil {
			t.Fatalf("%s: this corpus is diffs the parser accepts: %v", tc.name, err)
		}
		for _, hunk := range files[0].Hunks {
			total++
			var before, after int
			for _, line := range hunk.Lines {
				switch line[0] {
				case ' ':
					before, after = before+1, after+1
				case '-':
					before++
				case '+':
					after++
				}
			}
			if before != hunk.OldCount || after != hunk.NewCount {
				contradicted = append(contradicted, fmt.Sprintf(
					"%s (declared -%d,+%d against a body of %d and %d)", tc.name, hunk.OldCount, hunk.NewCount, before, after))
			}
		}
	}

	if len(contradicted)*3 < total {
		t.Fatalf(
			"only %d of %d hunks contradict their declared counts; the measurement that rules out enforcing them no longer holds and the decision must be retaken:\n%s",
			len(contradicted), total, strings.Join(contradicted, "\n"))
	}
	for _, name := range []string{"a header the model miscounted", "a fenced answer", "renames a key"} {
		if !slices.ContainsFunc(contradicted, func(entry string) bool { return strings.HasPrefix(entry, name) }) {
			t.Fatalf("%q is asserted elsewhere to parse and apply, so a count check must be shown to refuse it: %v", name, contradicted)
		}
	}
	t.Logf("%d of %d hunks in the accepted corpus contradict their declared counts:\n%s",
		len(contradicted), total, strings.Join(contradicted, "\n"))
}

func TestAHunkWhoseBodyIsAllContextIsRefusedAsALostChange(t *testing.T) {
	tests := []struct {
		name string
		diff string
	}{
		{
			"an addition beginning ++ as the last body line",
			"--- a/x\n+++ b/x\n@@ -1,1 +1,2 @@\n a: 1\n+++ new\n",
		},
		{
			"a removal beginning -- as the last body line",
			"--- a/x\n+++ b/x\n@@ -1,2 +1,1 @@\n a: 1\n--- legacy\n",
		},
		{
			"a ---/+++ pair inside the body inventing a file path",
			"--- a/x\n+++ b/x\n@@ -1,2 +1,2 @@\n a: 1\n--- legacy\n+++ modern\n@@ -1,1 +1,1 @@\n-old\n+new\n",
		},
		{
			"a hunk that is honestly all context",
			"--- a/x\n+++ b/x\n@@ -1,2 +1,2 @@\n a: 1\n b: 2\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := patch.Parse(test.diff); err == nil || !strings.Contains(err.Error(), "changes nothing") {
				t.Fatalf("Parse() err = %v, want a refusal naming the lost change", err)
			}
		})
	}
}

func TestADashRuleEndingTheHunkBodyIsRefusedRatherThanGuessed(t *testing.T) {
	tests := []struct {
		name string
		diff string
	}{
		{
			"a four-dash markdown rule then prose deleted a document separator",
			"--- a/x\n+++ b/x\n@@ -2,2 +2,2 @@\n apiVersion: v1\n-kind: A\n+kind: AA\n----\nThat renames the kind.\n",
		},
		{
			"a three-dash markdown rule then prose",
			"--- a/x\n+++ b/x\n@@ -2,2 +2,2 @@\n apiVersion: v1\n-kind: A\n+kind: AA\n---\nThat renames the kind.\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := patch.Parse(test.diff); err == nil || !strings.Contains(err.Error(), "add a context line") {
				t.Fatalf("Parse() err = %v, want the dash-rule refusal", err)
			}
		})
	}
}

func TestASeparatorRemovalFollowedByContextStillApplies(t *testing.T) {
	file := "---\napiVersion: v1\nkind: A\n---\napiVersion: v1\nkind: B\n"
	diff := "--- a/x\n+++ b/x\n@@ -3,3 +3,2 @@\n kind: A\n----\n apiVersion: v1\n"
	files, err := patch.Parse(diff)
	if err != nil {
		t.Fatalf("Parse() err = %v", err)
	}
	out, err := patch.Apply([]byte(file), files[0])
	if err != nil {
		t.Fatalf("Apply() err = %v", err)
	}
	want := "---\napiVersion: v1\nkind: A\napiVersion: v1\nkind: B\n"
	if string(out) != want {
		t.Fatalf("Apply() = %q, want %q", out, want)
	}
}

// The ambiguity refusals must not be argued down to "a manifest cannot contain
// that line". Whether a fix path holds a manifest is operator configuration —
// config.AllowsFixPath matches globs and imposes no file type, so
// "kubernetes/**" admits .sql, .sh and .md — and a rule whose safety depends on
// configuration is not a rule. Parse never sees the path, and these pin that:
// the same body refuses under every extension, so no later YAML fast path can
// reintroduce the dependence.
func TestTheAmbiguityRefusalsDoNotDependOnTheFileType(t *testing.T) {
	bodies := map[string]string{
		"an orphaned header ending the body": "@@ -1,2 +1,2 @@\n a\n+-- new\n--- legacy\n",
		"a dash rule ending the body":        "@@ -1,2 +1,2 @@\n a\n-kind: A\n+kind: AA\n----\n",
		"a blank splitting a replacement":    "@@ -1,3 +1,3 @@\n-a\n\n+aa\n b\n",
	}
	for _, path := range []string{"kubernetes/app.yaml", "kubernetes/schema.sql", "kubernetes/bootstrap.sh", "kubernetes/README.md", "kubernetes/Makefile"} {
		for name, body := range bodies {
			t.Run(name+" in "+path, func(t *testing.T) {
				if _, err := patch.Parse(fmt.Sprintf("--- a/%s\n+++ b/%s\n%s", path, path, body)); err == nil {
					t.Fatalf("%s parsed cleanly, so the refusal depends on the file type", path)
				}
			})
		}
	}
}

// The blind spot this package cannot close, pinned so nothing downstream is
// built on the belief that it does.
//
// sonarr is the app being repaired and has no resources block; radarr has one.
// An agent that believes the block it is quoting is sonarr's writes a diff that
// is correct in every way this package can check: the context occurs exactly
// once, the header states its true line, and the bytes committed are precisely
// the bytes the operator approved. radarr's limit is raised and sonarr is left
// untouched, with no error.
//
// No rule inside patch can see it. Uniqueness cannot, because the context is
// unique. Position cannot, because the header is right — this is stronger than
// the case S-M04 adjudicated, where the remedy rejected was to distrust a
// distant match. The divergence is between what the agent meant and what it
// wrote, and both the diff and the file agree with what it wrote. Closing it
// needs the object named: either the approver renders the enclosing object
// beside each hunk, or the prompt makes the agent name its target per hunk and
// the caller cross-checks it. Both are outside this package.
func TestAUniquelyMatchedHunkCanStillBeTheObjectTheAgentDidNotMean(t *testing.T) {
	const twoApps = "apiVersion: helm.toolkit.fluxcd.io/v2\nkind: HelmRelease\nmetadata:\n  name: sonarr\nspec:\n  values:\n    image:\n      tag: 4.0.0\n---\napiVersion: helm.toolkit.fluxcd.io/v2\nkind: HelmRelease\nmetadata:\n  name: radarr\nspec:\n  values:\n    resources:\n      limits:\n        memory: 512Mi\n"
	diff := "--- a/apps.yaml\n+++ b/apps.yaml\n@@ -16,3 +16,3 @@\n     resources:\n       limits:\n-        memory: 512Mi\n+        memory: 1Gi\n"

	files, err := patch.Parse(diff)
	if err != nil {
		t.Fatalf("Parse() err = %v", err)
	}
	out, err := patch.Apply([]byte(twoApps), files[0])
	if err != nil {
		t.Fatalf("nothing here is detectable from the diff and the file alone, so this must still apply: %v", err)
	}
	if !strings.Contains(string(out), "memory: 1Gi") {
		t.Fatalf("the hunk names radarr's block and quotes it correctly, so it applies there:\n%s", out)
	}
	if strings.Contains(string(out), "  name: sonarr\nspec:\n  values:\n    resources:") {
		t.Fatalf("sonarr gained a resources block it never had:\n%s", out)
	}
}

// The refusal rate is measured at five context widths but its recoverability
// was measured only at git's default, where the rate is lowest — so the reason
// for tolerating the rate went unchecked exactly where the rate is highest.
//
// Measured at all five: no refusal in this corpus is unreachable at any width.
// What decides recovery is the width the retry lands on, not the width the
// refusal happened at. An agent that re-emits at git's default or wider recovers
// every refusal at every width. An agent that instead adds a line to each side
// of its own narrow diff does not: from the changed line alone, 12 of 60 stay
// ambiguous after three such rounds, which is more than the one retry
// MaxFixAttempts allows. That is a property of the retry, not of locate(), and
// it is the argument for the error asking for a line that appears once rather
// than for more context.
func TestEveryRefusalRecoversAtEveryWidthTheRefusalRateIsMeasuredAt(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		above, below         int
		stuckWideningInPlace int
	}{
		{name: "git's default of three lines each side", above: 3, below: 3},
		{name: "three lines above the change only", above: 3, below: 0, stuckWideningInPlace: 1},
		{name: "two lines above the change only", above: 2, below: 0, stuckWideningInPlace: 4},
		{name: "one line each side", above: 1, below: 1, stuckWideningInPlace: 5},
		{name: "the changed line alone", above: 0, below: 0, stuckWideningInPlace: 12},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var refused, stuckInPlace, stuckFromDefault int
			for _, file := range gitOpsCorpus {
				lines := strings.Split(strings.TrimSuffix(file.content, "\n"), "\n")
				for index, line := range lines {
					if landed, err := applyOneLineFix(file.content, lines, index, tc.above, tc.below); err == nil && landed {
						continue
					}
					refused++
					recovers := func(above, below int) bool {
						landed, err := applyOneLineFix(file.content, lines, index, above, below)
						return err == nil && landed
					}
					inPlace, fromDefault := false, false
					for widen := 1; widen <= 3 && !inPlace; widen++ {
						inPlace = recovers(tc.above+widen, tc.below+widen)
					}
					for widen := range 4 {
						if fromDefault = recovers(max(tc.above, 3)+widen, max(tc.below, 3)+widen); fromDefault {
							break
						}
					}
					if !inPlace {
						stuckInPlace++
					}
					if !fromDefault {
						stuckFromDefault++
						t.Errorf("%s line %d %q: no width recovers this refusal, so the fix is unreachable",
							file.name, index+1, strings.TrimSpace(line))
					}
				}
			}
			if stuckFromDefault != 0 {
				t.Fatalf("%d of %d refusals are unreachable, and the rate is only tolerable because none are", stuckFromDefault, refused)
			}
			if stuckInPlace != tc.stuckWideningInPlace {
				t.Fatalf("%d of %d refusals survive three rounds of widening the agent's own diff, was %d",
					stuckInPlace, refused, tc.stuckWideningInPlace)
			}
		})
	}
}

// What the dash rule costs, stated rather than assumed. Parse refuses a whole
// diff, so an ambiguous "----" ending the first of two hunks also throws away
// the second hunk's unrelated and unambiguous change — the operator loses the
// replicas bump because a document separator was removed elsewhere in the file.
// That is the trade the rule accepts: the alternative is guessing whether a
// dashes-only line is a removal or a markdown rule, and guessing wrong either
// deletes a separator or silently drops the removal. It is bounded by the
// recovery below costing exactly one line.
func TestASeparatorRemovalEndingTheFirstHunkRefusesTheSecondHunkToo(t *testing.T) {
	const secondHunk = "@@ -12,4 +11,4 @@\n   name: radarr\n spec:\n   values:\n-    replicas: 1\n+    replicas: 2\n"
	const bumped = "apiVersion: helm.toolkit.fluxcd.io/v2\nkind: HelmRelease\nmetadata:\n  name: sonarr\nspec:\n  values:\n    replicas: 1\n---\napiVersion: helm.toolkit.fluxcd.io/v2\nkind: HelmRelease\nmetadata:\n  name: radarr\nspec:\n  values:\n    replicas: 2\n"

	refused := "--- a/x\n+++ b/x\n@@ -4,5 +4,4 @@\n   name: sonarr\n spec:\n   values:\n     replicas: 1\n----\n" + secondHunk
	if _, err := patch.Parse(refused); err == nil || !strings.Contains(err.Error(), "add a context line") {
		t.Fatalf("Parse() err = %v, want the dash-rule refusal", err)
	}

	if files, err := patch.Parse("--- a/x\n+++ b/x\n" + secondHunk); err != nil {
		t.Fatalf("the second hunk alone is unambiguous: %v", err)
	} else if out, err := patch.Apply([]byte(twoReleases), files[0]); err != nil || string(out) != bumped {
		t.Fatalf("Apply() = %q, err = %v, want %q", out, err, bumped)
	}

	recovered := "--- a/x\n+++ b/x\n@@ -4,6 +4,5 @@\n   name: sonarr\n spec:\n   values:\n     replicas: 1\n----\n apiVersion: helm.toolkit.fluxcd.io/v2\n" + secondHunk
	files, err := patch.Parse(recovered)
	if err != nil {
		t.Fatalf("one trailing context line must be the whole cost of the refusal: %v", err)
	}
	if len(files[0].Hunks) != 2 {
		t.Fatalf("both hunks must survive the recovery, got %d", len(files[0].Hunks))
	}
	out, err := patch.Apply([]byte(twoReleases), files[0])
	if err != nil {
		t.Fatalf("Apply() err = %v", err)
	}
	want := strings.Replace(bumped, "---\n", "", 1)
	if string(out) != want {
		t.Fatalf("Apply() = %q, want the separator removed and the replicas bumped: %q", out, want)
	}
}

// A body line beginning "-- " or "++ " carries the same prefix as half a file
// header, and ending the body is exactly where the two readings stop being
// distinguishable. Dropping it as a stray header applied the rest of the hunk
// and reported success for a change that was never made whole: the SQL comment
// below survived its own removal, and the replacement line never landed.
func TestAnOrphanedFileHeaderEndingTheHunkBodyIsRefusedRatherThanDropped(t *testing.T) {
	const sql = "CREATE TABLE t (id int);\n-- legacy note\nSELECT 1;\n"
	for _, tc := range []struct {
		name, file, diff string
	}{
		{
			name: "a removed SQL comment reads as a --- header",
			file: sql,
			diff: "--- a/x.sql\n+++ b/x.sql\n@@ -1,2 +1,2 @@\n CREATE TABLE t (id int);\n+-- new note\n--- legacy note\n",
		},
		{
			name: "an added ++ line reads as a +++ header",
			file: "a\n++ plus\nb\n",
			diff: "--- a/x.txt\n+++ b/x.txt\n@@ -1,3 +1,3 @@\n a\n-++ plus\n+++ plus2\n",
		},
		{
			name: "a complete file section follows, so nothing downstream refuses",
			file: sql,
			diff: "--- a/x.sql\n+++ b/x.sql\n@@ -1,2 +1,2 @@\n CREATE TABLE t (id int);\n+-- new note\n--- legacy note\n--- a/y.sql\n+++ b/y.sql\n@@ -1,1 +1,1 @@\n-SELECT 1;\n+SELECT 2;\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files, err := patch.Parse(tc.diff)
			if err != nil {
				if !strings.Contains(err.Error(), "reads both as a change to") {
					t.Fatalf("Parse() err = %v, want the ambiguity refusal", err)
				}
				return
			}
			out, err := patch.Apply([]byte(tc.file), files[0])
			t.Fatalf("the last body line was dropped and the rest applied anyway: Apply() = %q, err = %v", out, err)
		})
	}
}

// What the refusal above costs, measured rather than argued. Every one of these
// positions is refused; the one spelling that expresses the change is the pair,
// covered by TestTheEndOfBodyRefusalNamesTheSpellingThatApplies, and the refusal
// message names it. If a later edit makes one of these apply too, that is a
// second recovery and the message should say so.
func TestNoPositionInAHunkBodyCanExpressAChangeToADoubleMarkerLine(t *testing.T) {
	const sql = "CREATE TABLE t (id int);\n-- legacy note\nSELECT 1;\n"
	for _, tc := range []struct {
		name, diff string
	}{
		{"a context line under it", "--- a/x.sql\n+++ b/x.sql\n@@ -1,3 +1,2 @@\n CREATE TABLE t (id int);\n--- legacy note\n SELECT 1;\n"},
		{"an addition under it", "--- a/x.sql\n+++ b/x.sql\n@@ -1,3 +1,2 @@\n CREATE TABLE t (id int);\n--- legacy note\n+-- new note\n"},
		{"first in the body", "--- a/x.sql\n+++ b/x.sql\n@@ -2,2 +2,1 @@\n--- legacy note\n SELECT 1;\n"},
		{"a blank line under it", "--- a/x.sql\n+++ b/x.sql\n@@ -1,3 +1,2 @@\n CREATE TABLE t (id int);\n--- legacy note\n\n"},
		{"a no-newline marker under it", "--- a/x.sql\n+++ b/x.sql\n@@ -1,3 +1,2 @@\n CREATE TABLE t (id int);\n--- legacy note\n\\ No newline at end of file\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files, err := patch.Parse(tc.diff)
			if err != nil {
				return
			}
			out, err := patch.Apply([]byte(sql), files[0])
			if err == nil && !strings.Contains(string(out), "-- legacy note") {
				t.Fatalf("this shape now applies, so the end-of-body refusal has a recovery its message does not name: %q", out)
			}
		})
	}
}

// git emits a context line between a removal and the insertion that replaces it
// whenever the two are separated in the file, and that context line can be
// blank — `git diff --no-index` on "X\n\nQ\n" against "\nZ\nQ\n" produces
// exactly "-X", " ", "+Z", " Q". Once a model strips the leading space, that is
// indistinguishable from the cosmetic gap a model puts between a removal and its
// replacement, and the two readings put the blank on opposite sides of the
// addition. Guessing wrote the wrong bytes and reported success.
func TestABlankBetweenARemovalAndItsAdditionIsRefusedRatherThanPlaced(t *testing.T) {
	files, err := patch.Parse("--- a/x.yaml\n+++ b/x.yaml\n@@ -1,3 +1,3 @@\n-a: 1\n\n+a: 11\n b: 2\n")
	if err != nil {
		if !strings.Contains(err.Error(), "leading space") {
			t.Fatalf("Parse() err = %v, want the blank-placement refusal", err)
		}
		return
	}
	got, err := patch.Apply([]byte("a: 1\n\nb: 2\n"), files[0])
	t.Fatalf("the blank was spliced ahead of the addition on a guess: Apply() = %q, err = %v", got, err)
}

// The same ambiguity one line over. A blank between two removals is a context
// line the file keeps or a removal whose marker went with its leading space,
// and the two readings differ only on the added side — the before side is
// identical either way, so locate() matches and the wrong bytes are committed
// with no error. Extra blanks between a removal and its addition hide the same
// thing behind a longer gap.
func TestABlankBetweenARemovalAndTheNextMarkedLineIsRefusedRatherThanGuessed(t *testing.T) {
	for _, tc := range []struct{ name, diff, original string }{
		{
			name:     "a blank between two removals",
			diff:     "--- a/x\n+++ b/x\n@@ -1,4 +1,1 @@\n-a\n\n-b\n keep\n",
			original: "a\n\nb\nkeep\n",
		},
		{
			name:     "two blanks between a removal and its addition",
			diff:     "--- a/x\n+++ b/x\n@@ -1,4 +1,4 @@\n-X\n\n\n+Z\n Q\n",
			original: "X\n\n\nQ\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files, err := patch.Parse(tc.diff)
			if err != nil {
				if !strings.Contains(err.Error(), "leading space") {
					t.Fatalf("Parse() err = %v, want the blank-placement refusal", err)
				}
				return
			}
			got, err := patch.Apply([]byte(tc.original), files[0])
			t.Fatalf("the blank was placed on a guess: Apply() = %q, err = %v", got, err)
		})
	}
}

// And what that refusal costs: every reading it declines to choose between is
// still writable, so it spends one retry and never the fix.
func TestSpellingTheBlankBetweenTwoRemovalsEitherWayStillApplies(t *testing.T) {
	for _, tc := range []struct{ name, diff, original, want string }{
		{
			name:     "kept as an explicit context line, as git emits it",
			diff:     "--- a/x\n+++ b/x\n@@ -1,4 +1,2 @@\n-a\n \n-b\n keep\n",
			original: "a\n\nb\nkeep\n",
			want:     "\nkeep\n",
		},
		{
			name:     "marked as the removal it may have been",
			diff:     "--- a/x\n+++ b/x\n@@ -1,4 +1,1 @@\n-a\n-\n-b\n keep\n",
			original: "a\n\nb\nkeep\n",
			want:     "keep\n",
		},
		{
			name:     "the addition ordered ahead of the blanks, as git emits it",
			diff:     "--- a/x\n+++ b/x\n@@ -1,4 +1,4 @@\n-X\n+Z\n \n \n Q\n",
			original: "X\n\n\nQ\n",
			want:     "Z\n\n\nQ\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files, err := patch.Parse(tc.diff)
			if err != nil {
				t.Fatalf("Parse() err = %v", err)
			}
			got, err := patch.Apply([]byte(tc.original), files[0])
			if err != nil {
				t.Fatalf("Apply() err = %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("Apply() = %q, want %q", got, tc.want)
			}
		})
	}
}

// Both readings stay reachable, so the refusal costs one retry and never the
// fix: spelling the blank as a space applies git's reading, dropping it applies
// the model's.
func TestSpellingTheBlankEitherWayStillApplies(t *testing.T) {
	for _, tc := range []struct {
		name, diff, original, want string
	}{
		{
			name:     "kept as an explicit context line, as git emits it",
			diff:     "--- a/x\n+++ b/x\n@@ -1,3 +1,3 @@\n-X\n \n+Z\n Q\n",
			original: "X\n\nQ\n",
			want:     "\nZ\nQ\n",
		},
		{
			name:     "dropped as the cosmetic gap it may have been",
			diff:     "--- a/x\n+++ b/x\n@@ -1,3 +1,3 @@\n-X\n+Z\n \n Q\n",
			original: "X\n\nQ\n",
			want:     "Z\n\nQ\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files, err := patch.Parse(tc.diff)
			if err != nil {
				t.Fatalf("Parse() err = %v", err)
			}
			got, err := patch.Apply([]byte(tc.original), files[0])
			if err != nil {
				t.Fatalf("Apply() err = %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("Apply() = %q, want %q", got, tc.want)
			}
		})
	}
}

// git groups a hunk's removals ahead of its additions, so when the last removal
// and the first addition of a group are both double-marker lines the body
// carries a literal "--- x" immediately followed by "+++ y". `git diff
// --no-index -U3` on "CREATE TABLE t (id int);\n-- legacy note\nSELECT 1;\n"
// against "++ new note\nCREATE TABLE u (id int);\nSELECT 1;\n" emits exactly
// that. Read as a file header pair, both lines vanished, the rest of the hunk
// applied with err=nil, and a second File named by the diff's own prose was
// fabricated — one that names a real file rewrites it byte-identically, so the
// dropped change is reported as applied.
func TestADoubleMarkerReplacementInsideAHunkBodyIsAChangeNotAFileHeader(t *testing.T) {
	const sql = "CREATE TABLE t (id int);\n-- legacy note\nSELECT 1;\n"
	diff := "--- a/x.sql\n+++ b/x.sql\n@@ -1,3 +1,3 @@\n-CREATE TABLE t (id int);\n--- legacy note\n+++ new note\n+CREATE TABLE u (id int);\n SELECT 1;\n"

	files, err := patch.Parse(diff)
	if err != nil {
		t.Fatalf("Parse() err = %v, want git's own output to parse", err)
	}
	if len(files) != 1 {
		t.Fatalf("Parse() = %d files %v, want 1: the body's double-marker pair was read as a file header", len(files), pathsOf(files))
	}
	got, err := patch.Apply([]byte(sql), files[0])
	if err != nil {
		t.Fatalf("Apply() err = %v", err)
	}
	const want = "++ new note\nCREATE TABLE u (id int);\nSELECT 1;\n"
	if string(got) != want {
		t.Fatalf("Apply() = %q, want %q", got, want)
	}
}

// The pair is a file header only where a hunk header follows it, which is the
// one shape a body line cannot take. Both readings have to keep working or a
// multi-file diff is misread in the other direction.
func TestAFileHeaderPairStillEndsTheHunkBodyWhenAHunkFollowsIt(t *testing.T) {
	diff := "--- a/x.sql\n+++ b/x.sql\n@@ -1,1 +1,1 @@\n-SELECT 1;\n+SELECT 2;\n--- a/y.sql\n+++ b/y.sql\n@@ -1,1 +1,1 @@\n-SELECT 3;\n+SELECT 4;\n"
	files, err := patch.Parse(diff)
	if err != nil {
		t.Fatalf("Parse() err = %v", err)
	}
	if paths := pathsOf(files); strings.Join(paths, ",") != "x.sql,y.sql" {
		t.Fatalf("pathsOf() = %v, want both file sections", paths)
	}
	for _, file := range files {
		if len(file.Hunks) != 1 {
			t.Fatalf("%s: each file owns exactly one hunk here, got %d", file.NewPath, len(file.Hunks))
		}
	}
}

// This pins an OPEN finding (C-M63), not a fix. `git diff -U0` on one file emits
// this exact shape when a hunk changes a "-- x"/"++ x" double-marker line and a
// later hunk follows it: the body carries "--- x"/"+++ x" and then a @@. Read as
// a file header the pair splits into a phantom section, the real file silently
// keeps its double-marker line, and the phantom — naming an empty allowed file —
// receives the next hunk's insertions with err=nil.
//
// It cannot be closed in the parser: byte for byte it is the legitimate second
// section of TestAFileHeaderPairStillEndsTheHunkBodyWhenAHunkFollowsIt — same
// pair-then-@@, same OldPath==NewPath, no diff --git either side (real git omits
// it before the phantom; a hand-written multi-file diff omits it before a real
// section). Any rule that refuses one refuses the other. The closure belongs to
// caller-side path validation, not here.
func TestAnInBodyHeaderPairFollowedByAHunkIsIndistinguishableFromASecondFileSection(t *testing.T) {
	diff := "diff --git a/db.sql b/db.sql\nindex 9ce3ac6..4ffbc20 100644\n--- a/db.sql\n+++ b/db.sql\n@@ -1,2 +1 @@\n-DROP TABLE old;\n--- apps/empty.yaml\n+++ apps/empty.yaml\n@@ -11,0 +11,2 @@ l11\n+ins1\n+ins2\n"
	files, err := patch.Parse(diff)
	if err != nil {
		t.Fatalf("Parse() err = %v", err)
	}
	if len(files) != 2 {
		t.Fatalf("Parse() = %d files, the pair split db.sql into a phantom: want 2", len(files))
	}
	if files[0].NewPath != "db.sql" || len(files[0].Hunks) != 1 || strings.Join(files[0].Hunks[0].Lines, "|") != "-DROP TABLE old;" {
		t.Fatalf("db.sql lost its double-marker change to the split: %+v", files[0])
	}
	if files[1].OldPath != "apps/empty.yaml" || files[1].NewPath != "apps/empty.yaml" {
		t.Fatalf("phantom paths = %q/%q, want the double-marker line's own text", files[1].OldPath, files[1].NewPath)
	}
	out, err := patch.Apply(nil, files[1])
	if err != nil || string(out) != "ins1\nins2" {
		t.Fatalf("phantom write to the empty file = %q, err = %v, want the foreign insertions and no error", out, err)
	}
}

// A file section that carries no hunk changes nothing, so Apply rewrites the
// file byte-identically and the caller commits a fix that never happened. The
// hunkless deletion and creation shorthands say what they mean; a modification
// with no body does not.
func TestAFileSectionThatChangesNothingIsRefusedRatherThanRewrittenIdentically(t *testing.T) {
	for _, tc := range []struct {
		name, diff string
		wantErr    bool
	}{
		{name: "a modification with no hunk", diff: "--- a/x.yaml\n+++ b/x.yaml\n", wantErr: true},
		{name: "a hunkless deletion", diff: "--- a/gone.yaml\n+++ /dev/null\n"},
		{name: "a hunkless creation", diff: "--- /dev/null\n+++ b/new.yaml\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := patch.Parse(tc.diff)
			if tc.wantErr && (err == nil || !strings.Contains(err.Error(), "no hunk")) {
				t.Fatalf("Parse() err = %v, want a refusal naming the missing hunk", err)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("Parse() err = %v, want the shorthand to survive", err)
			}
		})
	}
}

// The end-of-body refusal is only worth its cost while the change it refuses to
// guess at can be written some other way, so the message has to name that way
// and the way has to work.
func TestTheEndOfBodyRefusalNamesTheSpellingThatApplies(t *testing.T) {
	const sql = "CREATE TABLE t (id int);\n-- legacy note\nSELECT 1;\n"

	_, err := patch.Parse("--- a/x.sql\n+++ b/x.sql\n@@ -1,2 +1,2 @@\n CREATE TABLE t (id int);\n+-- new note\n--- legacy note\n")
	if err == nil || !strings.Contains(err.Error(), "+++ line that replaces it") {
		t.Fatalf("Parse() err = %v, want the refusal to name the spelling that applies", err)
	}

	files, err := patch.Parse("--- a/x.sql\n+++ b/x.sql\n@@ -1,3 +1,3 @@\n CREATE TABLE t (id int);\n--- legacy note\n+++ new note\n SELECT 1;\n")
	if err != nil {
		t.Fatalf("the refusal names this spelling, so it must parse: %v", err)
	}
	got, err := patch.Apply([]byte(sql), files[0])
	if err != nil {
		t.Fatalf("Apply() err = %v", err)
	}
	const want = "CREATE TABLE t (id int);\n++ new note\nSELECT 1;\n"
	if string(got) != want {
		t.Fatalf("Apply() = %q, want %q", got, want)
	}
}

// A second file section that runs out before its hunk is now read as a change
// to a "-- " line, which is the reading that fails closed: the removal it names
// is not in the file the hunk belongs to. The header reading returned a second
// File nobody could apply anything to and let the first hunk land alone.
func TestASecondFileSectionTruncatedBeforeItsHunkIsRefusedRatherThanHalfApplied(t *testing.T) {
	diff := "--- a/x.yaml\n+++ b/x.yaml\n@@ -1,1 +1,1 @@\n-a\n+b\n--- a/y.yaml\n+++ b/y.yaml\n"
	files, err := patch.Parse(diff)
	if err != nil {
		return
	}
	if len(files) != 1 {
		t.Fatalf("Parse() = %v, want the truncated section not to become a file of its own", pathsOf(files))
	}
	if out, err := patch.Apply([]byte("a\n"), files[0]); err == nil {
		t.Fatalf("Apply() = %q with no error: half the diff landed and the truncation went unreported", out)
	}
}

// Trailing narration that happens to begin "--- " or "+++ " is the one spelling
// of closing prose the parser refuses, and it cannot do otherwise: Parse never
// sees the file, so it cannot tell a sentence from a change to a line beginning
// "-- ", and the body reading of a "+++ " line writes that sentence into the
// file with no error at all, because an addition is never matched against
// anything. What it can do is cost one deleted line instead of the whole fix,
// so the refusal has to name that and deleting the line has to be enough.
func TestTrailingNarrationBeginningLikeAHeaderIsRefusedWithTheRemedyNamed(t *testing.T) {
	const body = "--- a/x.yaml\n+++ b/x.yaml\n@@ -6,3 +6,3 @@\n   values:\n-    env:\n+    envs:\n"
	for _, tc := range []struct {
		name, narration string
	}{
		{"a closing sentence beginning ---", "--- end of patch"},
		{"a closing sentence beginning +++", "+++ note: restart flux after"},
		{"a markdown rule with a trailing space", "--- "},
		{"an attribution beginning +++", "+++ generated from the upstream changelog"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := patch.Parse(body + tc.narration + "\n")
			if err == nil {
				t.Fatalf("Parse() err = nil, want the ambiguity refusal: %q also reads as a change", tc.narration)
			}
			if !strings.Contains(err.Error(), "delete it if it is prose") {
				t.Fatalf("Parse() err = %v, want the refusal to name deleting the line", err)
			}

			files, err := patch.Parse(body)
			if err != nil {
				t.Fatalf("deleting the line is the named remedy, so it must parse: %v", err)
			}
			got, err := patch.Apply([]byte(helmRelease), files[0])
			if err != nil {
				t.Fatalf("Apply() err = %v", err)
			}
			if !strings.Contains(string(got), "    envs:") {
				t.Fatalf("the remedy must cost the narration and not the fix:\n%s", got)
			}
		})
	}
}

// Closing prose that carries no diff marker at all stays tolerated, so the
// refusal above is scoped to the two prefixes that are also markers rather than
// to trailing narration as such.
func TestNarrationCarryingNoDiffMarkerStillCostsNothing(t *testing.T) {
	const body = "--- a/x.yaml\n+++ b/x.yaml\n@@ -6,3 +6,3 @@\n   values:\n-    env:\n+    envs:\n"
	const want = "apiVersion: helm.toolkit.fluxcd.io/v2\nkind: HelmRelease\nmetadata:\n  name: sonarr\nspec:\n  values:\n    envs:\n      TZ: Europe/Berlin\n"
	for _, narration := range []string{
		"That is the whole change.",
		"```",
		"...and restart flux.",
		"# end of patch",
		"*** end of patch",
	} {
		t.Run(narration, func(t *testing.T) {
			files, err := patch.Parse(body + narration + "\n")
			if err != nil {
				t.Fatalf("Parse() err = %v, want trailing prose tolerated", err)
			}
			got, err := patch.Apply([]byte(helmRelease), files[0])
			if err != nil {
				t.Fatalf("Apply() err = %v", err)
			}
			if string(got) != want {
				t.Fatalf("Apply() = %q, want the narration ignored: %q", got, want)
			}
		})
	}
}

// The blind spot the refusal above sits next to, pinned so nothing is built on
// the belief that trailing narration is generally free. One marker character
// short of the refused spellings there is no ambiguity left to detect: "+ x" and
// "++ x" are ordinary additions and nothing distinguishes them from prose, so
// the sentence is written into the manifest with no error. The "-" forms fail
// closed only because locate() cannot find them in the file.
//
// Narrowing the refusal above towards these would move the failure from a
// refusal the agent can act on to a wrong write into a production cluster.
// Closing this instead needs the marker to be unambiguous, which is the caller's
// prompt, not this package.
func TestNarrationBeginningWithASingleMarkerIsReadAsBody(t *testing.T) {
	const body = "--- a/x.yaml\n+++ b/x.yaml\n@@ -6,3 +6,3 @@\n   values:\n-    env:\n+    envs:\n"
	for _, tc := range []struct {
		narration  string
		wantWrites string
	}{
		{narration: "++ note: restart flux after", wantWrites: "+ note: restart flux after"},
		{narration: "+ and restart flux", wantWrites: " and restart flux"},
		{narration: "-- end of patch"},
		{narration: "- and restart flux"},
	} {
		t.Run(tc.narration, func(t *testing.T) {
			files, err := patch.Parse(body + tc.narration + "\n")
			if err != nil {
				t.Fatalf("Parse() err = %v: a single marker leaves nothing to be ambiguous about", err)
			}
			got, err := patch.Apply([]byte(helmRelease), files[0])
			if tc.wantWrites == "" {
				if err == nil {
					t.Fatalf("Apply() = %q with no error, want the removal not to be found", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("Apply() err = %v", err)
			}
			if !strings.Contains(string(got), "\n"+tc.wantWrites+"\n") {
				t.Fatalf("Apply() = %q, want the narration written in as the addition it spells", got)
			}
		})
	}
}

// A hunk whose file header is incomplete is a different failure with a
// different remedy, and the outer loop's message names it. Refusing the dropped
// body line must not swallow that.
func TestAnIncompleteFileHeaderBeforeAHunkKeepsItsOwnRefusal(t *testing.T) {
	diff := "--- a/x.sql\n+++ b/x.sql\n@@ -1,2 +1,2 @@\n CREATE TABLE t (id int);\n+-- new note\n--- legacy note\n@@ -3,1 +3,1 @@\n-SELECT 1;\n+SELECT 2;\n"
	_, err := patch.Parse(diff)
	if err == nil || !strings.Contains(err.Error(), "does not complete a file header") {
		t.Fatalf("Parse() err = %v, want the incomplete-file-header refusal", err)
	}
}
