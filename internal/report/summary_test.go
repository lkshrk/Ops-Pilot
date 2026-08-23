package report_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/lkshrk/ops-pilot/internal/display"
	"github.com/lkshrk/ops-pilot/internal/domain"
	"github.com/lkshrk/ops-pilot/internal/report"
)

var zeroWidthSpace = string(rune(0x200b))

func attempt(number int, name string, verdict domain.Verdict, reason string) domain.Attempt {
	return domain.Attempt{
		PullRequest: number,
		Dependency:  domain.Dependency{Name: name, FromVersion: "1.0.0", ToVersion: "1.0.1"},
		Verdict:     verdict,
		Reason:      reason,
		Duration:    time.Minute,
	}
}

func render(t *testing.T, run domain.Run) string {
	t.Helper()
	var out bytes.Buffer
	if err := report.Summary(&out, run, false); err != nil {
		t.Fatalf("summary: %v", err)
	}
	return out.String()
}

// A dry run that would merge cleanly needs nobody. Reporting it as "needs you"
// told the operator the opposite of the truth on their very first run.
func TestDryRunIsNotReportedAsNeedingAHuman(t *testing.T) {
	got := render(t, domain.Run{Attempts: []domain.Attempt{
		attempt(1, "a", domain.VerdictWouldMerge, "safe"),
		attempt(2, "b", domain.VerdictWouldMerge, "safe"),
	}})
	if !strings.Contains(got, "2 would merge") {
		t.Fatalf("want a would-merge tally:\n%s", got)
	}
	if strings.Contains(got, "need you") {
		t.Fatalf("nothing needs a human here:\n%s", got)
	}
}

// Rescanning forty rows to find what still needs attention is the operator's
// job the summary exists to remove.
func TestTallyNamesWhatNeedsAttention(t *testing.T) {
	got := render(t, domain.Run{Attempts: []domain.Attempt{
		attempt(11, "a", domain.VerdictMerged, ""),
		attempt(22, "b", domain.VerdictSkipped, "major"),
		attempt(33, "c", domain.VerdictReverted, "broke"),
	}})
	for _, want := range []string{"1 merged", "1 needs you (#22)", "1 reverted (#33)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("want %q in:\n%s", want, got)
		}
	}
}

// A halt means the queue was abandoned and the cluster is in an unknown state.
// SPEC promises the summary says so.
func TestHaltedRunSaysSoInTheSummary(t *testing.T) {
	got := render(t, domain.Run{
		Attempts: []domain.Attempt{attempt(1, "a", domain.VerdictReverted, "broke")},
		Halted:   "#1 was reverted but the cluster did not recover",
	})
	if !strings.Contains(got, "Run halted: #1 was reverted but the cluster did not recover") {
		t.Fatalf("the halt must be unmissable:\n%s", got)
	}
}

// Every halt path today records its attempt first, but the queue can be
// abandoned before a pull request is reached and "Nothing to do" would then
// claim the tool looked and found no work, when it started work and stopped.
func TestAHaltedRunWithNoAttemptsIsNotReportedAsNothingToDo(t *testing.T) {
	for _, test := range []struct {
		name   string
		halted string
	}{
		{
			name: "the merge never reached the cluster",
			halted: "#42 merged but could not be observed: Flux did not fetch " +
				"3f2a1c9d8b7e6f5a4b3c2d1e0f9a8b7c6d5e4f3a within 10m0s, " +
				"so this merge was never applied",
		},
		{
			name: "the source stalled and reported its own error",
			halted: "#7 is merged and could not be observed: Flux source flux-system/gitops: " +
				"Flux source has stopped retrying: failed to checkout and determine revision: " +
				"unable to clone 'https://example.com/acme/gitops': authentication required",
		},
		{
			name: "the pod list failed",
			halted: "#3 is merged and could not be observed: list pods: " +
				"Get \"https://10.0.0.1:6443/api/v1/pods\": context deadline exceeded",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := render(t, domain.Run{Halted: test.halted})
			if strings.Contains(got, "Nothing to do") {
				t.Fatalf("a halted run did not find nothing to do:\n%s", got)
			}
			if !strings.Contains(got, "halted") {
				t.Fatalf("the halt must be unmissable:\n%s", got)
			}
			if !strings.Contains(display.Collapse(got), display.Collapse(test.halted)) {
				t.Fatalf("want the whole reason in:\n%s", got)
			}
		})
	}
}

func counted(n int) *int { return &n }

func TestARunThatFoundNoUpdatesAndDidNotHaltSaysNothingToDo(t *testing.T) {
	got := render(t, domain.Run{Repository: domain.RepositoryRef{Branch: "main"}})
	if !strings.Contains(got, "Nothing to do") {
		t.Fatalf("want the quiet-run line in:\n%s", got)
	}
}

// An empty queue and a queue emptied by the configured filters read identically
// to an operator, so a repository with twelve waiting Renovate pull requests and
// a mistyped author is reported as idle. The count is what tells them apart.
// The base branch narrowing empties a queue as thoroughly as a mistyped author
// does, and it is invisible to Discovered: both listings drop a pull request
// aimed elsewhere, so twelve waiting updates on master read as an idle main.
func TestAQuietRunSaysHowManyPullRequestsAreAimedAtAnotherBranch(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		run        domain.Run
		want       []string
		unexpected []string
	}{
		{
			name: "every open pull request targets another branch",
			run: domain.Run{
				Repository:    domain.RepositoryRef{Branch: "main"},
				Discovered:    counted(0),
				OtherBranches: counted(12),
			},
			want: []string{"no open pull requests on main", "12 open pull requests target other branches"},
		},
		{
			name: "one pull request targets another branch",
			run: domain.Run{
				Repository:    domain.RepositoryRef{Branch: "main"},
				Discovered:    counted(0),
				OtherBranches: counted(1),
			},
			want: []string{"1 open pull request targets another branch"},
		},
		{
			name: "the branch this run merges into was never configured",
			run: domain.Run{
				Discovered:    counted(0),
				OtherBranches: counted(12),
			},
			want: []string{"no open pull requests", "12 open pull requests target other branches"},
		},
		{
			name: "a filtered queue and pull requests elsewhere",
			run: domain.Run{
				Repository:    domain.RepositoryRef{Branch: "main"},
				Discovered:    counted(3),
				OtherBranches: counted(2),
			},
			want: []string{"3 open pull requests", "authors", "2 open pull requests target other branches"},
		},
		{
			name: "this branch could not be counted but the others could",
			run: domain.Run{
				Repository:    domain.RepositoryRef{Branch: "main"},
				OtherBranches: counted(4),
			},
			want: []string{"could not be read", "4 open pull requests target other branches"},
		},
		{
			name: "nothing is waiting on another branch",
			run: domain.Run{
				Repository:    domain.RepositoryRef{Branch: "main"},
				Discovered:    counted(0),
				OtherBranches: counted(0),
			},
			unexpected: []string{"other branches", "another branch"},
		},
		{
			// Nil is not zero, and claiming no pull request is aimed elsewhere is
			// exactly the false reassurance this line exists to remove.
			name: "the other branches were not counted",
			run: domain.Run{
				Repository: domain.RepositoryRef{Branch: "main"},
				Discovered: counted(0),
			},
			want:       []string{"could not be read"},
			unexpected: []string{"other branches", "another branch"},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got := display.Collapse(render(t, testCase.run))
			for _, want := range testCase.want {
				if !strings.Contains(got, want) {
					t.Fatalf("want %q in:\n%s", want, got)
				}
			}
			for _, unexpected := range testCase.unexpected {
				if strings.Contains(got, unexpected) {
					t.Fatalf("the operator is misled by %q in:\n%s", unexpected, got)
				}
			}
		})
	}
}

// Both counts are read separately and either can come back unread, so the line
// is nine sentences rather than one. Pinning them whole is what stops a later
// edit to one clause from stranding another mid-sentence, and what stops the
// unread cases from drifting back into the wording of a measured zero.
func TestTheQuietLineReadsWholeForEveryCombinationOfCounts(t *testing.T) {
	const unread = " Whether open pull requests are waiting on a different branch could not be read."
	for _, testCase := range []struct {
		name string
		run  domain.Run
		want string
	}{
		{
			name: "neither count could be read",
			run:  domain.Run{Repository: domain.RepositoryRef{Branch: "main"}},
			want: "Nothing to do: no pull request matched, and the number open could not be read." + unread,
		},
		{
			name: "this branch could not be read and nothing waits elsewhere",
			run: domain.Run{
				Repository:    domain.RepositoryRef{Branch: "main"},
				OtherBranches: counted(0),
			},
			want: "Nothing to do: no pull request matched, and the number open could not be read.",
		},
		{
			name: "this branch could not be read and two wait elsewhere",
			run: domain.Run{
				Repository:    domain.RepositoryRef{Branch: "main"},
				OtherBranches: counted(2),
			},
			want: "Nothing to do: no pull request matched, and the number open could not be read." +
				" 2 open pull requests target other branches.",
		},
		{
			name: "an idle branch and the others could not be read",
			run: domain.Run{
				Repository: domain.RepositoryRef{Branch: "main"},
				Discovered: counted(0),
			},
			want: "Nothing to do: no open pull requests on main." + unread,
		},
		{
			name: "an idle repository stays a single sentence",
			run: domain.Run{
				Repository:    domain.RepositoryRef{Branch: "main"},
				Discovered:    counted(0),
				OtherBranches: counted(0),
			},
			want: "Nothing to do: no open pull requests on main.",
		},
		{
			name: "an idle branch and two waiting elsewhere",
			run: domain.Run{
				Repository:    domain.RepositoryRef{Branch: "main"},
				Discovered:    counted(0),
				OtherBranches: counted(2),
			},
			want: "Nothing to do: no open pull requests on main. 2 open pull requests target other branches.",
		},
		{
			name: "a filtered queue and the others could not be read",
			run: domain.Run{
				Repository: domain.RepositoryRef{Branch: "main"},
				Discovered: counted(3),
			},
			want: "Nothing to do: 3 open pull requests, none matched the configured " +
				"pullRequests.authors or .labels." + unread,
		},
		{
			name: "a filtered queue and nothing waiting elsewhere",
			run: domain.Run{
				Repository:    domain.RepositoryRef{Branch: "main"},
				Discovered:    counted(3),
				OtherBranches: counted(0),
			},
			want: "Nothing to do: 3 open pull requests, none matched the configured " +
				"pullRequests.authors or .labels.",
		},
		{
			name: "a filtered queue and two waiting elsewhere",
			run: domain.Run{
				Repository:    domain.RepositoryRef{Branch: "main"},
				Discovered:    counted(3),
				OtherBranches: counted(2),
			},
			want: "Nothing to do: 3 open pull requests, none matched the configured " +
				"pullRequests.authors or .labels. 2 open pull requests target other branches.",
		},
		{
			name: "one waiting elsewhere keeps its singular",
			run: domain.Run{
				Repository:    domain.RepositoryRef{Branch: "main"},
				Discovered:    counted(0),
				OtherBranches: counted(1),
			},
			want: "Nothing to do: no open pull requests on main. 1 open pull request targets another branch.",
		},
		{
			name: "no branch was configured and neither count could be read",
			run:  domain.Run{},
			want: "Nothing to do: no pull request matched, and the number open could not be read." + unread,
		},
		{
			name: "no branch was configured and the others could not be read",
			run:  domain.Run{Discovered: counted(0)},
			want: "Nothing to do: no open pull requests." + unread,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			if got := render(t, testCase.run); got != testCase.want+"\n" {
				t.Fatalf("want %q, got %q", testCase.want+"\n", got)
			}
		})
	}
}

func TestAQuietRunSaysWhetherTheQueueWasEmptyOrEmptied(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		run        domain.Run
		want       []string
		unexpected []string
	}{
		{
			name: "no open pull requests at all",
			run: domain.Run{
				Repository: domain.RepositoryRef{Branch: "main"},
				Discovered: counted(0),
			},
			want: []string{"no open pull requests", "main"},
		},
		{
			name: "every open pull request filtered out by the configured authors",
			run: domain.Run{
				Repository: domain.RepositoryRef{Branch: "main"},
				Discovered: counted(12),
			},
			want:       []string{"12 open pull requests", "authors"},
			unexpected: []string{"no open dependency updates"},
		},
		{
			name: "one open pull request filtered out by the configured authors",
			run: domain.Run{
				Repository: domain.RepositoryRef{Branch: "main"},
				Discovered: counted(1),
			},
			want: []string{"1 open pull request", "authors"},
		},
		{
			// The count is the whole content of this line, so a run that could not
			// read it may not fall back to asserting the repository is idle.
			name: "the pull requests could not be counted",
			run:  domain.Run{Repository: domain.RepositoryRef{Branch: "main"}},
			want: []string{"could not be read"},
			unexpected: []string{
				"no open pull requests",
				"no open dependency updates",
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			got := display.Collapse(render(t, testCase.run))
			for _, want := range testCase.want {
				if !strings.Contains(got, want) {
					t.Fatalf("want %q in:\n%s", want, got)
				}
			}
			for _, unexpected := range testCase.unexpected {
				if strings.Contains(got, unexpected) {
					t.Fatalf("the operator is misled by %q in:\n%s", unexpected, got)
				}
			}
		})
	}
}

// A controller writes the halt reason, so it arrives long and multi-line.
// Truncating it would hide the cause of the state nobody is watching.
func TestALongHaltReasonIsWrappedRatherThanTruncated(t *testing.T) {
	reason := "#42 is merged and could not be observed:\nFlux source flux-system/gitops:\n" +
		strings.Repeat("the source controller said something at considerable length. ", 6)
	for _, run := range []domain.Run{
		{Halted: reason},
		{Attempts: []domain.Attempt{attempt(42, "a", domain.VerdictMerged, "ok")}, Halted: reason},
	} {
		got := render(t, run)
		if strings.Contains(got, "…") {
			t.Fatalf("the halt reason must not be abbreviated:\n%s", got)
		}
		if !strings.Contains(display.Collapse(got), display.Collapse(reason)) {
			t.Fatalf("want the whole reason in:\n%s", got)
		}
		for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
			if display.Width(line) > display.DefaultWidth {
				t.Fatalf("line wider than the terminal: %q\n%s", line, got)
			}
		}
	}
}

// The identifier in a halt reason is what the operator pastes into `flux get`
// or `git show` next, so it overflows the line rather than being split: a
// digest read across two lines is mis-copied, and the wrong revision is a worse
// answer than a wide line. The prose around it still has to wrap.
func TestAnUnbreakableIdentifierInAHaltReasonIsKeptWholeRatherThanSplit(t *testing.T) {
	for _, test := range []struct {
		name, token string
	}{
		{"a digest", "sha256:" + strings.Repeat("a1b2c3d4", 27)},
		{"a source url", "https://example.com/acme/" + strings.Repeat("deeply-nested-", 15) + "gitops.git"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := render(t, domain.Run{Halted: "#42 is merged and could not be observed: " +
				"the source controller never fetched " + test.token + " within the settle window"})
			if !strings.Contains(display.Collapse(got), test.token) {
				t.Fatalf("the identifier must survive whole and copyable:\n%s", got)
			}
			if strings.Contains(got, "…") {
				t.Fatalf("the identifier must not be abbreviated:\n%s", got)
			}
			for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
				if strings.TrimSpace(line) == test.token {
					continue
				}
				if display.Width(line) > display.DefaultWidth {
					t.Fatalf("only the identifier's own line may overflow: %q\n%s", line, got)
				}
			}
		})
	}
}

// The exemption that keeps a digest copyable is justified at digest and URL
// scale and silent above it. The reason is untrusted controller text, so a
// controller emitting a large unspaced blob had nothing capping the line at all.
func TestAnImplausiblyLongIdentifierInAHaltReasonIsSplitToTheTerminalWidth(t *testing.T) {
	for _, test := range []struct {
		name  string
		token string
		whole bool
	}{
		{"a digest", "sha256:" + strings.Repeat("a1b2c3d4", 27), true},
		{"a long source url", "https://example.com/acme/" + strings.Repeat("deeply-nested-", 15) + "gitops.git", true},
		{"a token at the ceiling", strings.Repeat("z", 512), true},
		{"a token past the ceiling", strings.Repeat("y", 513), false},
		{"a hundred kilobyte blob", strings.Repeat("x", 100_000), false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := render(t, domain.Run{Halted: "#42 is merged and could not be observed: " +
				"the source controller never fetched " + test.token + " within the settle window"})
			if strings.Contains(got, "…") {
				t.Fatalf("the reason must not be abbreviated")
			}
			if !strings.Contains(strings.Join(strings.Fields(got), ""), test.token) {
				t.Fatalf("the identifier did not survive whole")
			}
			onOneLine, widest := false, 0
			for _, line := range strings.Split(strings.TrimSpace(got), "\n") {
				if strings.TrimSpace(line) == test.token {
					onOneLine = true
					// Only an identifier under the ceiling may overflow its line.
					if test.whole {
						continue
					}
				}
				if display.Width(line) > widest {
					widest = display.Width(line)
				}
			}
			if test.whole && !onOneLine {
				t.Fatalf("an identifier at this length must stay copyable on one line:\n%s", got)
			}
			if !test.whole && widest > display.DefaultWidth {
				t.Fatalf("no line may exceed the terminal, widest = %d columns", widest)
			}
		})
	}
}

// The reason is untrusted controller text and may carry no printable characters
// at all; the halt itself is still the most important thing the run can say.
func TestAHaltReasonWithNothingPrintableStillReportsTheHalt(t *testing.T) {
	for _, test := range []struct {
		name, halted, want string
	}{
		{"printable residue", "\x1b[2K\rIMPOSTOR", "IMPOSTOR"},
		{"nothing printable", "\x00" + zeroWidthSpace + "\r\n\t", "no reason was recorded"},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := render(t, domain.Run{Halted: test.halted})
			if strings.ContainsAny(got, "\x1b\r\x00") {
				t.Fatalf("control characters reached stdout:\n%q", got)
			}
			if strings.Contains(got, "Nothing to do") || !strings.Contains(got, "Run halted") {
				t.Fatalf("want an unmissable halt in:\n%s", got)
			}
			if !strings.Contains(got, test.want) {
				t.Fatalf("want %q in:\n%s", test.want, got)
			}
		})
	}
}

// The halt reason is controller-authored and the result column is model prose,
// so both quote credentials belonging to the workload. The redactor upstream
// holds only ops-pilot's own configured values and cannot match one.
func TestSummaryScrubsACredentialItWasNeverConfiguredWith(t *testing.T) {
	const (
		jwt   = "eyJhbGciOiJSUzI1NiIsImtpZCI6ImFiYyJ9.eyJzdWIiOiJzeXN0ZW06c2VydmljZWFjY291bnQifQ.c2lnbmF0dXJlLWJ5dGVzMDAw"
		named = "api_key=Ai8fkq2LmZx0Rt7Yb3Nc"
		dbURL = "postgres://app:hunter2correcthorse@db:5432/app"
	)
	tests := []struct {
		name   string
		run    domain.Run
		secret string
	}{
		{
			name:   "a halt reason quoting a pod log",
			run:    domain.Run{Halted: "the source controller said " + jwt},
			secret: "eyJzdWIiOiJzeXN0ZW06c2VydmljZWFjY291bnQifQ",
		},
		{
			name: "a halt reason after attempts",
			run: domain.Run{
				Attempts: []domain.Attempt{attempt(42, "a", domain.VerdictMerged, "ok")},
				Halted:   "watch abandoned: " + dbURL,
			},
			secret: "hunter2correcthorse",
		},
		{
			name: "a model reason in the result column",
			run: domain.Run{Attempts: []domain.Attempt{
				attempt(7, "app", domain.VerdictReverted, "the pod printed "+named),
			}},
			secret: "Ai8fkq2LmZx0Rt7Yb3Nc",
		},
		{
			name: "an error in the result column",
			run: domain.Run{Attempts: []domain.Attempt{{
				PullRequest: 8,
				Dependency:  domain.Dependency{Name: "app"},
				Verdict:     domain.VerdictError,
				Error:       "clone failed: " + dbURL,
			}}},
			secret: "hunter2correcthorse",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := render(t, test.run); strings.Contains(got, test.secret) {
				t.Fatalf("the summary kept %q:\n%s", test.secret, got)
			}
		})
	}
}

// Scrubbing must run on the text the run produced, not on what is left after
// display has folded it: collapsing whitespace destroys the line structure the
// block-scalar rule needs, and clipping cuts a token below the length its rule
// requires. Both leave the summary looking scrubbed while the secret ships.
func TestSummaryScrubsBeforeDisplayFoldsTheText(t *testing.T) {
	const (
		blockScalar = "password: |\n  hunter2correcthorse\n"
		jwt         = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9." +
			"eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkFkbWluIn0." +
			"SflKxwRJSMeKKF2QT4fwpMeJf36POk6yJV_adQssw5c"
	)
	tests := []struct {
		name   string
		run    domain.Run
		secret string
	}{
		{
			name:   "a block scalar in a halt reason survives collapsing",
			run:    domain.Run{Halted: blockScalar},
			secret: "hunter2correcthorse",
		},
		{
			name: "a block scalar in a halt reason after attempts",
			run: domain.Run{
				Attempts: []domain.Attempt{attempt(3, "a", domain.VerdictMerged, "ok")},
				Halted:   blockScalar,
			},
			secret: "hunter2correcthorse",
		},
		{
			name: "a jwt in the result column outlives the clip",
			run: domain.Run{Attempts: []domain.Attempt{
				attempt(9, "app", domain.VerdictReverted, "token rejected: "+jwt),
			}},
			secret: "eyJzdWIiOiIxMjM0NTY3ODkwIiwibmFtZSI6IkFkbWluIn0",
		},
		{
			name: "a jwt in the error column outlives the clip",
			run: domain.Run{Attempts: []domain.Attempt{{
				PullRequest: 10,
				Dependency:  domain.Dependency{Name: "app"},
				Verdict:     domain.VerdictError,
				Error:       "token rejected: " + jwt,
			}}},
			secret: "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := render(t, test.run)
			if strings.Contains(got, test.secret) {
				t.Fatalf("display folded the text before it was scrubbed, keeping %q:\n%s", test.secret, got)
			}
			if !strings.Contains(got, "[REDACTED]") {
				t.Fatalf("the summary reported no redaction at all:\n%s", got)
			}
		})
	}
}

func TestSummaryIsPlainWhenPiped(t *testing.T) {
	got := render(t, domain.Run{Attempts: []domain.Attempt{
		attempt(1, "a", domain.VerdictReverted, "broke"),
	}})
	if strings.ContainsRune(got, 0x1b) {
		t.Fatalf("piped output must carry no escape codes:\n%q", got)
	}
}

// Untrusted text reaches the table: dependency names and AI prose.
func TestSummaryStripsTerminalControl(t *testing.T) {
	got := render(t, domain.Run{Attempts: []domain.Attempt{
		attempt(1, "evil", domain.VerdictMerged, "safe\x1b[2K\rIMPOSTOR"),
	}})
	if strings.ContainsAny(got, "\x1b\r") {
		t.Fatalf("control characters reached stdout:\n%q", got)
	}
}

func TestColumnsAlignWithMultiByteNames(t *testing.T) {
	got := render(t, domain.Run{Attempts: []domain.Attempt{
		attempt(1, "日本語のイメージ", domain.VerdictMerged, "ok"),
		attempt(2, "plain-ascii-name", domain.VerdictMerged, "ok"),
	}})
	lines := strings.Split(strings.TrimSpace(got), "\n")
	first := strings.Index(lines[1], "1.0.0")
	if second := strings.Index(lines[2], "1.0.0"); first == second {
		t.Fatalf("byte-width padding would coincidentally match; check the fixture:\n%s", got)
	}
	// The BUMP column must start at the same display column on both rows.
	if displayColumn(lines[1], "1.0.0") != displayColumn(lines[2], "1.0.0") {
		t.Fatalf("columns misaligned with multi-byte text:\n%s", got)
	}
}

func TestOnlyASummaryThatFollowsANarrativeOpensWithABlankLine(t *testing.T) {
	run := domain.Run{Attempts: []domain.Attempt{attempt(1, "app", domain.VerdictMerged, "")}}
	for _, narrated := range []bool{true, false} {
		var out bytes.Buffer
		if err := report.Summary(&out, run, narrated); err != nil {
			t.Fatalf("summary: %v", err)
		}
		if leading := strings.HasPrefix(out.String(), "\n"); leading != narrated {
			t.Fatalf("narrated=%v opened with a blank line=%v:\n%q", narrated, leading, out.String())
		}
	}
}

func TestSummaryHasAClearSessionOutro(t *testing.T) {
	run := domain.Run{Attempts: []domain.Attempt{attempt(1, "app", domain.VerdictMerged, "")}}
	var out bytes.Buffer

	if err := report.Summary(&out, run, true); err != nil {
		t.Fatalf("summary: %v", err)
	}

	if !strings.HasPrefix(out.String(), "\nRun summary\n") {
		t.Fatalf("summary has no clear heading:\n%s", out.String())
	}
}

func tookCells(t *testing.T, durations ...time.Duration) []string {
	t.Helper()
	run := domain.Run{}
	for i, duration := range durations {
		run.Attempts = append(run.Attempts, domain.Attempt{
			PullRequest: i + 1,
			Dependency:  domain.Dependency{Name: "app"},
			Verdict:     domain.VerdictMerged,
			Duration:    duration,
		})
	}
	lines := strings.Split(render(t, run), "\n")
	if len(lines) < len(durations)+1 {
		t.Fatalf("want %d rows and a header, got %d lines", len(durations), len(lines))
	}
	cells := make([]string, 0, len(durations))
	for _, line := range lines[1 : len(durations)+1] {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			t.Fatalf("want a row, got a blank line among:\n%s", strings.Join(lines, "\n"))
		}
		cells = append(cells, fields[len(fields)-1])
	}
	return cells
}

func TestTookRoundsToWholeSecondsOnBothSidesOfTheMinuteBoundary(t *testing.T) {
	tests := []struct {
		name     string
		duration time.Duration
		want     string
	}{
		{"a run that recorded no duration", 0, "-"},
		{"a clock that went backwards", -time.Second, "-"},
		{"less than half a second", 499 * time.Millisecond, "0s"},
		{"half a second", 500 * time.Millisecond, "1s"},
		{"just under the last second before a minute", 59499 * time.Millisecond, "59s"},
		{"rounding up over the boundary from below", 59500 * time.Millisecond, "1m0s"},
		{"one nanosecond below a minute", time.Minute - 1, "1m0s"},
		{"exactly a minute", time.Minute, "1m0s"},
		{"one nanosecond above a minute", time.Minute + 1, "1m0s"},
		{"rounding down to the boundary from above", 60499 * time.Millisecond, "1m0s"},
		{"rounding up past the boundary", 60500 * time.Millisecond, "1m1s"},
		{"a minute and a half", 90 * time.Second, "1m30s"},
		{"an hour rounding down", time.Hour + 2*time.Minute + 3400*time.Millisecond, "1h2m3s"},
		{"an hour rounding up", time.Hour + 2*time.Minute + 3600*time.Millisecond, "1h2m4s"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := tookCells(t, test.duration)[0]; got != test.want {
				t.Fatalf("took(%v) = %q, want %q", test.duration, got, test.want)
			}
		})
	}
}

// The TOOK column is a whole number of seconds, so forty rows stay readable.
// took only ever receives a live measured interval, which is why rounding alone
// is enough: Duration.Round stops returning a whole second only when it
// saturates, 292 years out.
func TestTookRendersNoFractionForAnyDurationARunCanMeasure(t *testing.T) {
	var durations []time.Duration
	for d := time.Duration(0); d <= 3*time.Minute; d += 50 * time.Millisecond {
		durations = append(durations, d)
	}
	for d := 3 * time.Minute; d <= 24*time.Hour; d += 7*time.Minute + 3*time.Millisecond {
		durations = append(durations, d)
	}
	for _, d := range []time.Duration{
		24 * time.Hour, 365 * 24 * time.Hour, 10 * 365 * 24 * time.Hour, 100 * 365 * 24 * time.Hour,
	} {
		durations = append(durations, d, d+499*time.Millisecond, d+500*time.Millisecond)
	}
	for i, cell := range tookCells(t, durations...) {
		if strings.Contains(cell, ".") {
			t.Fatalf("took(%v) = %q, want a whole number of seconds", durations[i], cell)
		}
	}
}

// Clipping runs after scrubbing (pinned separately), so the only thing the width
// decides is how much of a model-authored reason shares a line with the verdict.
func TestALongReasonIsClippedToSixtyColumnsInTheResultColumn(t *testing.T) {
	reason := strings.Repeat("verylongword", 20)
	got := render(t, domain.Run{Attempts: []domain.Attempt{
		attempt(1, "app", domain.VerdictReverted, reason),
	}})
	var cells []string
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "#1") {
			cells = strings.Split(line, "  ")
		}
	}
	if len(cells) != 5 {
		t.Fatalf("want a five-cell row for #1, got %d:\n%s", len(cells), got)
	}
	detail, found := strings.CutPrefix(cells[3], "REVERTED ")
	if !found {
		t.Fatalf("no verdict in the result cell %q:\n%s", cells[3], got)
	}
	if width := widthOf(detail); width != 60 {
		t.Fatalf("clipped detail is %d columns, want 60: %q", width, detail)
	}
	if !strings.HasSuffix(detail, "…") {
		t.Fatalf("a clipped detail must say so: %q", detail)
	}
}

func displayColumn(line, needle string) int {
	index := strings.Index(line, needle)
	if index < 0 {
		return -1
	}
	return widthOf(line[:index])
}

func widthOf(value string) int { return display.Width(value) }

// The named list caps at six pull requests, so the ellipsis promises more than
// it shows. It must appear only when a seventh was genuinely elided: exactly six
// listed with a trailing "…" tells the operator to go hunting for a pull request
// that does not exist.
func TestTheSkippedListEllipsisMeansMoreThanSixOnlyWhenSomethingWasElided(t *testing.T) {
	for _, test := range []struct {
		name        string
		skipped     int
		wantElision bool
	}{
		{"five listed whole", 5, false},
		{"six listed whole", 6, false},
		{"seven elided after six", 7, true},
		{"ten elided after six", 10, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var attempts []domain.Attempt
			for i := 1; i <= test.skipped; i++ {
				attempts = append(attempts, attempt(i, "a", domain.VerdictSkipped, "major"))
			}
			got := display.Collapse(render(t, domain.Run{Attempts: attempts}))
			if elided := strings.Contains(got, "…"); elided != test.wantElision {
				t.Fatalf("with %d skipped, ellipsis=%v want %v:\n%s", test.skipped, elided, test.wantElision, got)
			}
		})
	}
}

func TestAnInterruptedRunWithNoIdStillReportsTheInterrupt(t *testing.T) {
	var out bytes.Buffer
	if err := report.Interrupted(&out, domain.Run{}, false); err != nil {
		t.Fatalf("interrupted: %v", err)
	}
	if got := out.String(); got != "! Interrupted. Nothing further was attempted.\n" {
		t.Fatalf("interrupt line = %q", got)
	}
}

func TestOnlyAnInterruptThatFollowsANarrativeOpensWithABlankLine(t *testing.T) {
	for _, narrated := range []bool{true, false} {
		var out bytes.Buffer
		if err := report.Interrupted(&out, domain.Run{}, narrated); err != nil {
			t.Fatalf("interrupted: %v", err)
		}
		if leading := strings.HasPrefix(out.String(), "\n"); leading != narrated {
			t.Fatalf("narrated=%v opened with a blank line=%v:\n%q", narrated, leading, out.String())
		}
	}
}
