package run

import (
	"fmt"
	"strings"
	"testing"

	"github.com/lkshrk/ops-pilot/internal/adapters/renovate"
	"github.com/lkshrk/ops-pilot/internal/domain"
)

func renovateBody(name, from, to string) string {
	return "| Package | Type | Update | Change |\n" +
		"|---|---|---|---|\n" +
		fmt.Sprintf("| [%s](https://example.test/%s) | image | patch | `%s` -> `%s` |\n", name, name, from, to)
}

func pull(number int, name, from, to string, labels ...string) domain.PullRequest {
	return domain.PullRequest{
		Number: number,
		Title:  fmt.Sprintf("chore(deps): update %s to %s", name, to),
		Body:   renovateBody(name, from, to),
		Labels: labels,
	}
}

func target(number int, name, to string) Candidate {
	return Candidate{
		PullRequest: domain.PullRequest{Number: number},
		Update:      renovate.Update{Dependency: domain.Dependency{Name: name, ToVersion: to}},
	}
}

func setAside(number int, name, to string, decision domain.Decision) Candidate {
	candidate := target(number, name, to)
	candidate.Skip = decision
	candidate.Reason = "set aside before supersession ran"
	return candidate
}

func skips(t *testing.T, candidates []Candidate) map[int]domain.Decision {
	t.Helper()
	out := make(map[int]domain.Decision, len(candidates))
	for _, candidate := range candidates {
		if _, clash := out[candidate.PullRequest.Number]; clash {
			t.Fatalf("duplicate candidate for #%d", candidate.PullRequest.Number)
		}
		out[candidate.PullRequest.Number] = candidate.Skip
	}
	return out
}

func assertSkips(t *testing.T, candidates []Candidate, want map[int]domain.Decision) {
	t.Helper()
	got := skips(t, candidates)
	if len(got) != len(want) {
		t.Fatalf("candidates:\n got %v\nwant %v", got, want)
	}
	for number, decision := range want {
		if got[number] != decision {
			t.Fatalf("#%d: got %q, want %q (all: %v)", number, got[number], decision, got)
		}
	}
}

// oneDeployment is the ordinary case supersession exists for: every pull
// request rewrites the same declaration in the same file.
func oneDeployment(int) ([]string, bool) { return []string{"apps/gatus/helmrelease.yaml"}, true }

func sameFileForAll(candidates []Candidate) map[int][]string {
	paths := make(map[int][]string, len(candidates))
	for _, candidate := range candidates {
		paths[candidate.PullRequest.Number] = []string{"apps/gatus/helmrelease.yaml"}
	}
	return paths
}

// Two HelmReleases of one chart, held on different versions, are two offers a
// repository wants both of. Ranking them against each other closes the
// conservative one on the forge, and Renovate does not raise it again.
func TestTwoDeploymentsOfOneDependencyAreBothLeftOpen(t *testing.T) {
	tests := []struct {
		name  string
		paths map[int][]string
	}{
		{
			name: "one HelmRelease per file",
			paths: map[int][]string{
				1: {"apps/monitoring/gatus/helmrelease.yaml"},
				2: {"apps/tools/gatus/helmrelease.yaml"},
			},
		},
		{
			name: "the newer offer also rewrites its kustomization",
			paths: map[int][]string{
				1: {"apps/monitoring/gatus/helmrelease.yaml"},
				2: {"apps/tools/gatus/helmrelease.yaml", "apps/tools/gatus/kustomization.yaml"},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidates := []Candidate{target(1, "gatus", "1.3.1"), target(2, "gatus", "1.4.0")}

			markSuperseded(candidates, test.paths)

			assertSkips(t, candidates, map[int]domain.Decision{1: "", 2: ""})
		})
	}
}

// Two deployments of one chart, each with its own HelmRelease, commonly import a
// shared values file. That single overlap is not the two rewriting one
// declaration: each still carries a private file the other never touches, so
// closing one on the shared values file discards a genuine deployment's update.
func TestTwoDeploymentsSharingAValuesFileAreBothLeftOpen(t *testing.T) {
	candidates := []Candidate{target(1, "gatus", "1.3.1"), target(2, "gatus", "1.4.0")}

	markSuperseded(candidates, map[int][]string{
		1: {"apps/monitoring/gatus/helmrelease.yaml", "apps/common/values.yaml"},
		2: {"apps/tools/gatus/helmrelease.yaml", "apps/common/values.yaml"},
	})

	assertSkips(t, candidates, map[int]domain.Decision{1: "", 2: ""})
}

// An empty-but-known file list is not evidence two pull requests are one
// deployment; the forge returns it transiently during a force-push or
// mergeability race. Closing on it is closing on a guess, the one thing
// supersession may never do - and an empty newest would otherwise take every
// sibling on the line down with it.
func TestAnEmptyChangedFileListSupersedesNothing(t *testing.T) {
	tests := []struct {
		name  string
		paths map[int][]string
	}{
		{name: "the older reports no files", paths: map[int][]string{1: {}, 2: {"apps/gatus/hr.yaml"}}},
		{name: "the newest reports no files", paths: map[int][]string{1: {"apps/gatus/hr.yaml"}, 2: {}}},
		{name: "both report no files", paths: map[int][]string{1: {}, 2: {}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidates := []Candidate{target(1, "gatus", "1.3.1"), target(2, "gatus", "1.4.0")}

			markSuperseded(candidates, test.paths)

			assertSkips(t, candidates, map[int]domain.Decision{1: "", 2: ""})
		})
	}
}

// The older pull request being a strict superset - it carries a private file the
// newest never touches - is still one deployment edited across a bump: the newest
// rewrites a subset of what the older did, and a stale merge would conflict. It is
// closed, unlike the mutual-private-file case where each side owns a file the
// other lacks.
func TestASupersetOlderPullRequestIsStillSuperseded(t *testing.T) {
	candidates := []Candidate{target(1, "gatus", "1.3.1"), target(2, "gatus", "1.4.0")}

	markSuperseded(candidates, map[int][]string{
		1: {"apps/gatus/helmrelease.yaml", "apps/gatus/extra.yaml"},
		2: {"apps/gatus/helmrelease.yaml"},
	})

	assertSkips(t, candidates, map[int]domain.Decision{1: domain.DecideSkipSuperseded, 2: ""})
}

// A digest refresh on one instance says nothing about another instance.
func TestADigestRefreshDoesNotCloseAnotherInstanceOfTheSameImage(t *testing.T) {
	candidates := []Candidate{
		target(1, "gatus", "1.3.0@sha256:aaa"),
		target(2, "gatus", "1.3.1"),
	}

	markSuperseded(candidates, map[int][]string{
		1: {"apps/monitoring/gatus/helmrelease.yaml"},
		2: {"apps/tools/gatus/helmrelease.yaml"},
	})

	assertSkips(t, candidates, map[int]domain.Decision{1: "", 2: ""})
}

// Two pull requests rewriting the same declaration are the case supersession
// exists for, and a stale one merged after the newer would conflict anyway.
func TestPullRequestsRewritingTheSameFileStillSupersede(t *testing.T) {
	tests := []struct {
		name  string
		paths map[int][]string
		want  map[int]domain.Decision
	}{
		{
			name: "the same file",
			paths: map[int][]string{
				1: {"apps/gatus/helmrelease.yaml"},
				2: {"apps/gatus/helmrelease.yaml"},
			},
			want: map[int]domain.Decision{1: domain.DecideSkipSuperseded, 2: ""},
		},
		{
			name: "overlapping sets",
			paths: map[int][]string{
				1: {"apps/gatus/helmrelease.yaml"},
				2: {"apps/gatus/helmrelease.yaml", "apps/gatus/kustomization.yaml"},
			},
			want: map[int]domain.Decision{1: domain.DecideSkipSuperseded, 2: ""},
		},
		{
			name: "the newer pull request renamed the file the older one edits",
			paths: map[int][]string{
				1: {"apps/gatus/helmrelease.yaml"},
				2: {"apps/monitoring/gatus/helmrelease.yaml", "apps/gatus/helmrelease.yaml"},
			},
			want: map[int]domain.Decision{1: domain.DecideSkipSuperseded, 2: ""},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidates := []Candidate{target(1, "gatus", "1.3.1"), target(2, "gatus", "1.4.0")}

			markSuperseded(candidates, test.paths)

			assertSkips(t, candidates, test.want)
		})
	}
}

// Closing a pull request on the forge is not reversible by ops-pilot and
// Renovate does not raise it again, so an unreadable file list has to leave the
// pull request open rather than guess that it was replaced.
func TestAPullRequestWhoseFilesCannotBeReadIsNeitherSupersededNorSupersedes(t *testing.T) {
	tests := []struct {
		name  string
		paths map[int][]string
	}{
		{name: "the older one could not be read", paths: map[int][]string{2: {"apps/gatus/hr.yaml"}}},
		{name: "the newer one could not be read", paths: map[int][]string{1: {"apps/gatus/hr.yaml"}}},
		{name: "neither could be read", paths: map[int][]string{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidates := []Candidate{target(1, "gatus", "1.3.1"), target(2, "gatus", "1.4.0")}

			markSuperseded(candidates, test.paths)

			assertSkips(t, candidates, map[int]domain.Decision{1: "", 2: ""})
		})
	}
}

// Measured, and left open on the row: a path set names files, not deployments,
// so two HelmReleases declared in one file are one identity and the closure
// C-M40 reports is still live for that layout. This pins the boundary rather
// than the wish.
func TestTwoDeploymentsDeclaredInOneFileAreStillIndistinguishable(t *testing.T) {
	candidates := []Candidate{target(1, "gatus", "1.3.1"), target(2, "gatus", "1.4.0")}

	markSuperseded(candidates, map[int][]string{
		1: {"apps/monitoring/releases.yaml"},
		2: {"apps/monitoring/releases.yaml"},
	})

	assertSkips(t, candidates, map[int]domain.Decision{
		1: domain.DecideSkipSuperseded,
		2: "",
	})
}

func TestOnlyTheNewestOfThreePullRequestsForOneDependencySurvives(t *testing.T) {
	candidates := Queue([]domain.PullRequest{
		pull(1, "gatus", "1.0.0", "1.0.1"),
		pull(2, "gatus", "1.0.0", "1.0.3"),
		pull(3, "gatus", "1.0.0", "1.0.2"),
	}, Labels{}, oneDeployment)

	assertSkips(t, candidates, map[int]domain.Decision{
		1: domain.DecideSkipSuperseded,
		2: "",
		3: domain.DecideSkipSuperseded,
	})
	for _, candidate := range candidates {
		if candidate.Skip == domain.DecideSkipSuperseded && !strings.Contains(candidate.Reason, "gatus") {
			t.Fatalf("#%d: reason %q does not name the dependency", candidate.PullRequest.Number, candidate.Reason)
		}
	}
}

// A superseded pull request is commented on and closed on the forge, so a
// candidate with no dependency to compare must never reach that path.
// The targets share a major, so only the name guard keeps these apart: an update
// nothing could be attributed to must not rank against anything.
func TestAnUpdateWithNoDependencyNameIsNeitherSupersededNorSupersedes(t *testing.T) {
	candidates := []Candidate{
		target(1, "", "1.0.0"),
		target(2, "", "1.9.9"),
		target(3, "gatus", "1.0.0"),
	}

	markSuperseded(candidates, sameFileForAll(candidates))

	assertSkips(t, candidates, map[int]domain.Decision{1: "", 2: "", 3: ""})
}

// run.go closes whatever carries DecideSkipSuperseded, so a candidate an earlier
// stage set aside may neither acquire that decision nor hand it to a live pull
// request. Every target here shares a major line, leaving the skip guard as the
// only thing standing between these decisions and the forge.
func TestACandidateAlreadySetAsideNeitherSupersedesNorIsSuperseded(t *testing.T) {
	tests := []struct {
		name       string
		candidates []Candidate
		want       map[int]domain.Decision
	}{
		{
			name: "a body nobody could parse does not close the pull request it outranks",
			candidates: []Candidate{
				target(1, "gatus", "1.0.1"),
				setAside(2, "gatus", "1.9.9", domain.DecideNeedsApproval),
			},
			want: map[int]domain.Decision{1: "", 2: domain.DecideNeedsApproval},
		},
		{
			name: "a body nobody could parse is not relabelled superseded",
			candidates: []Candidate{
				target(1, "gatus", "1.9.9"),
				setAside(2, "gatus", "1.0.1", domain.DecideNeedsApproval),
			},
			want: map[int]domain.Decision{1: "", 2: domain.DecideNeedsApproval},
		},
		{
			name: "an already reverted update does not close its live replacement",
			candidates: []Candidate{
				target(1, "gatus", "1.0.1"),
				setAside(2, "gatus", "1.9.9", domain.DecideSkipReverted),
			},
			want: map[int]domain.Decision{1: "", 2: domain.DecideSkipReverted},
		},
		{
			name: "an update the operator declined is not closed behind their back",
			candidates: []Candidate{
				target(1, "gatus", "1.9.9"),
				setAside(2, "gatus", "1.0.1", domain.DecideSkipDeclined),
			},
			want: map[int]domain.Decision{1: "", 2: domain.DecideSkipDeclined},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			markSuperseded(test.candidates, sameFileForAll(test.candidates))
			assertSkips(t, test.candidates, test.want)
		})
	}
}

func TestAPullRequestSetAsideByALabelNeitherSupersedesNorIsSuperseded(t *testing.T) {
	labels := Labels{Reverted: "ops-pilot/reverted", Declined: "ops-pilot/declined"}
	requests := []domain.PullRequest{
		pull(1, "gatus", "1.0.0", "1.0.1"),
		pull(2, "gatus", "1.0.0", "1.0.9", labels.Reverted),
		pull(3, "gatus", "1.0.0", "1.0.8", labels.Declined),
	}

	assertSkips(t, Queue(requests, labels, oneDeployment), map[int]domain.Decision{
		1: "",
		2: domain.DecideSkipReverted,
		3: domain.DecideSkipDeclined,
	})

	labels.All = true
	assertSkips(t, Queue(requests, labels, oneDeployment), map[int]domain.Decision{
		1: domain.DecideSkipSuperseded,
		2: "",
		3: domain.DecideSkipSuperseded,
	})
}

func TestAnUnparseableBodyIsHeldForApprovalRatherThanSuperseded(t *testing.T) {
	candidates := Queue([]domain.PullRequest{
		pull(1, "gatus", "1.0.0", "1.0.1"),
		{Number: 2, Title: "chore(deps): update gatus", Body: "no renovate table here"},
	}, Labels{}, oneDeployment)

	assertSkips(t, candidates, map[int]domain.Decision{
		1: "",
		2: domain.DecideNeedsApproval,
	})
}

// Omitted trailing segments are zeroes, so 1.0 and 1.0.0 name the same release
// and closing either as superseded would discard a live update.
func TestTargetsThatDifferOnlyBySegmentCountDoNotSupersedeEachOther(t *testing.T) {
	assertSkips(t, Queue([]domain.PullRequest{
		pull(1, "gatus", "0.9.0", "1.0"),
		pull(2, "gatus", "0.9.0", "1.0.0"),
	}, Labels{}, oneDeployment), map[int]domain.Decision{1: "", 2: ""})

	assertSkips(t, Queue([]domain.PullRequest{
		pull(1, "gatus", "0.9.0", "1.0"),
		pull(2, "gatus", "0.9.0", "1.0.1"),
	}, Labels{}, oneDeployment), map[int]domain.Decision{
		1: domain.DecideSkipSuperseded,
		2: "",
	})
}

// Renovate opens one branch per major of the target version, so a major bump
// and a patch on the line still in production are parallel offers, not a stale
// pull request and its replacement.
func TestAMajorBumpDoesNotSupersedeAPatchOnTheOlderLine(t *testing.T) {
	assertSkips(t, Queue([]domain.PullRequest{
		pull(1, "app-template", "3.1.0", "3.1.1"),
		pull(2, "app-template", "3.1.0", "4.0.0"),
	}, Labels{}, oneDeployment), map[int]domain.Decision{1: "", 2: ""})
}

func TestTwoMajorsRunningInParallelAreProcessedIndependently(t *testing.T) {
	assertSkips(t, Queue([]domain.PullRequest{
		pull(1, "app-template", "3.1.0", "3.1.1"),
		pull(2, "app-template", "4.0.0", "4.0.1"),
	}, Labels{}, oneDeployment), map[int]domain.Decision{1: "", 2: ""})
}

// A Renovate pull request closed by anything but a merge is not raised again.
func TestTwoMajorOffersLeavingOneVersionBehindAreBothLeftOpen(t *testing.T) {
	assertSkips(t, Queue([]domain.PullRequest{
		pull(1, "app-template", "1.5.0", "2.4.0"),
		pull(2, "app-template", "1.5.0", "3.0.0"),
	}, Labels{}, oneDeployment), map[int]domain.Decision{1: "", 2: ""})
}

func TestSupersessionStillAppliesWithinOneMajorLine(t *testing.T) {
	assertSkips(t, Queue([]domain.PullRequest{
		pull(1, "app-template", "3.1.0", "3.1.1"),
		pull(2, "app-template", "3.1.0", "3.1.2"),
		pull(3, "app-template", "3.1.0", "4.0.0"),
		pull(4, "app-template", "3.1.0", "4.1.0"),
	}, Labels{}, oneDeployment), map[int]domain.Decision{
		1: domain.DecideSkipSuperseded,
		2: "",
		3: domain.DecideSkipSuperseded,
		4: "",
	})
}

func TestTargetsThatCannotBeOrderedLeaveEveryPullRequestOpen(t *testing.T) {
	candidates := []Candidate{
		target(1, "gatus", "1.0.0@sha256:aaa"),
		target(2, "gatus", "1.0.0@sha256:bbb"),
	}

	markSuperseded(candidates, sameFileForAll(candidates))

	assertSkips(t, candidates, map[int]domain.Decision{1: "", 2: ""})
}

func TestAStrictlyNewerReleaseSupersedesEveryDigestOfTheOlderOne(t *testing.T) {
	candidates := []Candidate{
		target(1, "gatus", "1.0.0@sha256:aaa"),
		target(2, "gatus", "1.0.0@sha256:bbb"),
		target(3, "gatus", "1.0.1"),
	}

	markSuperseded(candidates, sameFileForAll(candidates))

	assertSkips(t, candidates, map[int]domain.Decision{
		1: domain.DecideSkipSuperseded,
		2: domain.DecideSkipSuperseded,
		3: "",
	})
}

func TestALabelSetAsideStillCarriesItsParsedUpdateForTheSummary(t *testing.T) {
	labels := Labels{Reverted: "ops-pilot/reverted", Declined: "ops-pilot/declined"}
	candidates := Queue([]domain.PullRequest{
		pull(2, "gatus", "1.0.0", "1.0.9", labels.Reverted),
		pull(3, "gatus", "1.0.0", "1.0.8", labels.Declined),
	}, labels, oneDeployment)

	for _, candidate := range candidates {
		if candidate.Update.Dependency.Name != "gatus" {
			t.Errorf("#%d skipped as %s lost its dependency: name %q",
				candidate.PullRequest.Number, candidate.Skip, candidate.Update.Dependency.Name)
		}
		if candidate.Update.Dependency.FromVersion == "" || candidate.Update.Dependency.ToVersion == "" {
			t.Errorf("#%d skipped as %s lost its versions: %q -> %q",
				candidate.PullRequest.Number, candidate.Skip,
				candidate.Update.Dependency.FromVersion, candidate.Update.Dependency.ToVersion)
		}
	}
}

// A pull request refused because it bumps several dependencies still names
// them, so its summary row identifies what it would have changed.
func TestAMultiDependencySkipStillNamesItsDependencies(t *testing.T) {
	body := "| Package | Type | Update | Change |\n" +
		"|---|---|---|---|\n" +
		"| [litellm-database](https://example.test/a) | image | minor | `1.95.0` -> `1.97.0` |\n" +
		"| [litellm-helm](https://example.test/b) | image | minor | `1.95.0` -> `1.97.0` |\n"
	candidates := Queue([]domain.PullRequest{{Number: 7, Body: body}}, Labels{}, oneDeployment)

	if len(candidates) != 1 || candidates[0].Skip != domain.DecideNeedsApproval {
		t.Fatalf("candidates = %+v, want one needs_approval skip", candidates)
	}
	if got := candidates[0].Update.Dependency.Name; got != "litellm-database, litellm-helm" {
		t.Errorf("dependency name = %q, want the joined names", got)
	}
	if candidates[0].Update.Dependency.FromVersion != "" || candidates[0].Update.Dependency.ToVersion != "" {
		t.Error("a multi-dependency skip must not pretend to a single version range")
	}
	if want := "pull request updates 2 dependencies at once"; candidates[0].Reason != want {
		t.Errorf("reason = %q, want %q", candidates[0].Reason, want)
	}
}

func TestASetAsideReasonAddressesTheReaderDirectly(t *testing.T) {
	labels := Labels{Reverted: "ops-pilot/reverted", Declined: "ops-pilot/declined"}

	candidates := Queue([]domain.PullRequest{
		pull(3, "gatus", "1.0.0", "1.0.8", labels.Declined),
	}, labels, oneDeployment)

	if len(candidates) != 1 {
		t.Fatalf("Queue returned %d candidates, want 1", len(candidates))
	}
	if strings.Contains(candidates[0].Reason, "operator") {
		t.Fatalf("reason = %q, want it to say when rather than who", candidates[0].Reason)
	}
	if !strings.Contains(candidates[0].Reason, "earlier run") {
		t.Fatalf("reason = %q, want it to say when the update was set aside", candidates[0].Reason)
	}
}
