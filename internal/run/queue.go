// Package run is the ops-pilot loop: discover, decide, merge, watch, repair.
package run

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/lkshrk/ops-pilot/internal/adapters/renovate"
	"github.com/lkshrk/ops-pilot/internal/domain"
)

func joinedNames(updates []renovate.Update) string {
	names := make([]string, 0, len(updates))
	for _, update := range updates {
		names = append(names, update.Dependency.Name)
	}
	return strings.Join(names, ", ")
}

// Candidate is one pull request paired with the update it declares.
type Candidate struct {
	PullRequest domain.PullRequest
	Update      renovate.Update
	// Skip is set when the candidate is not processed at all; Reason says why.
	Skip   domain.Decision
	Reason string
}

// Labels are what ops-pilot durably remembers about a pull request: one for a
// merge it had to revert, one for an update the operator refused.
type Labels struct {
	Reverted string
	Declined string
	// All processes every pull request, including ones a label had set aside.
	All bool
}

// Touched reports which repository paths a pull request rewrites. Supersession
// needs it to tell one deployment of a dependency from another; a pull request
// whose files cannot be read answers false, and is then left out of every
// supersession decision in both directions.
type Touched func(number int) ([]string, bool)

// Queue classifies discovered pull requests. Bodies that do not parse, pull
// requests a previous run set aside, and superseded ones are marked to skip
// rather than dropped, so the summary still accounts for them. A nil touched
// supersedes nothing: closing a pull request is not reversible from here.
func Queue(requests []domain.PullRequest, labels Labels, touched Touched) []Candidate {
	candidates := make([]Candidate, 0, len(requests))
	for _, request := range requests {
		candidate := Candidate{PullRequest: request}
		// Parsed even for set-aside requests, so their summary rows keep the
		// dependency; supersession still ignores anything marked to skip.
		updates, err := renovate.Parse(request.Body)
		switch {
		case err == nil && len(updates) == 1:
			candidate.Update = updates[0]
		case err == nil:
			// Attributable to no single changelog, but the names are known.
			candidate.Update.Dependency.Name = joinedNames(updates)
			err = fmt.Errorf("pull request updates %d dependencies at once", len(updates))
		}
		switch {
		case !labels.All && request.HasLabel(labels.Reverted):
			candidate.Skip = domain.DecideSkipReverted
			candidate.Reason = "reverted by an earlier run"
		case !labels.All && request.HasLabel(labels.Declined):
			candidate.Skip = domain.DecideSkipDeclined
			candidate.Reason = "declined in an earlier run"
		case err != nil:
			candidate.Skip = domain.DecideNeedsApproval
			candidate.Reason = err.Error()
		}
		candidates = append(candidates, candidate)
	}
	markSuperseded(candidates, contestedPaths(candidates, touched))
	return candidates
}

// contestedPaths reads the files of the pull requests that could actually lose
// one to the other, which is the only place supersession can fire. A quiet
// repository with one pull request per dependency asks the forge nothing.
func contestedPaths(candidates []Candidate, touched Touched) map[int][]string {
	if touched == nil {
		return nil
	}
	sharing := map[versionLine]int{}
	for _, candidate := range candidates {
		if line, ok := supersessionLine(candidate); ok {
			sharing[line]++
		}
	}
	paths := map[int][]string{}
	for _, candidate := range candidates {
		line, ok := supersessionLine(candidate)
		if !ok || sharing[line] < 2 {
			continue
		}
		if touchedPaths, known := touched(candidate.PullRequest.Number); known {
			paths[candidate.PullRequest.Number] = touchedPaths
		}
	}
	return paths
}

// markSuperseded finds pull requests bumping the same dependency onto the same
// major and keeps only the newest target version. Renovate leaves the older ones
// open when a newer release lands mid-flight.
//
// paths is what each pull request rewrites, keyed by number. Two pull requests
// that share no file are two deployments of one dependency - two HelmReleases
// held on different minors, or one instance's digest refresh - and both are
// wanted. A pull request with no entry is one whose files could not be read,
// and it is never closed on a guess: this filter may only ever spare a pull
// request, never close one the version comparison had left open.
func markSuperseded(candidates []Candidate, paths map[int][]string) {
	newest := map[versionLine]int{}
	for i, candidate := range candidates {
		line, ok := supersessionLine(candidate)
		if !ok {
			continue
		}
		best, seen := newest[line]
		if !seen || supersedes(candidate.Update, candidates[best].Update) {
			newest[line] = i
		}
	}
	unorderable := map[versionLine]bool{}
	for i, candidate := range candidates {
		line, ok := supersessionLine(candidate)
		if !ok {
			continue
		}
		if best, seen := newest[line]; seen && best != i && !comparable(candidate.Update, candidates[best].Update) {
			unorderable[line] = true
		}
	}
	for i := range candidates {
		line, ok := supersessionLine(candidates[i])
		if !ok {
			continue
		}
		best := newest[line]
		if best == i || unorderable[line] {
			continue
		}
		if !oneFileSetContainsTheOther(paths, candidates[i].PullRequest.Number, candidates[best].PullRequest.Number) {
			continue
		}
		candidates[i].Skip = domain.DecideSkipSuperseded
		candidates[i].Reason = "superseded by a newer pull request for " + line.name
	}
}

// oneFileSetContainsTheOther reports whether two pull requests rewrite one
// deployment. Two deployments of one dependency each carry a private file - their
// own HelmRelease - even when both import a shared values file, so a bare overlap
// is not enough: closing the older on a shared file discards a live deployment.
// Containment holds for the same declaration edited across a version bump, where
// the newer only ever adds files. An empty set names no shared file and is no
// evidence of shared identity - the forge returns it transiently during a race -
// so it supersedes nothing rather than closing on a guess.
func oneFileSetContainsTheOther(paths map[int][]string, older, newer int) bool {
	left, known := paths[older]
	if !known || len(left) == 0 {
		return false
	}
	right, known := paths[newer]
	if !known || len(right) == 0 {
		return false
	}
	return contains(right, left) || contains(left, right)
}

func contains(super, sub []string) bool {
	for _, path := range sub {
		if !slices.Contains(super, path) {
			return false
		}
	}
	return true
}

// versionLine is the update stream a pull request belongs to. Renovate opens one
// branch per major of the target version, so a repository running two majors of
// one chart in parallel gets a pull request per line and wants both; ranking
// them against each other would close the conservative one unread.
type versionLine struct {
	name  string
	major int
}

func supersessionLine(candidate Candidate) (versionLine, bool) {
	name := candidate.Update.Dependency.Name
	if candidate.Skip != "" || name == "" {
		return versionLine{}, false
	}
	return versionLine{name: name, major: majorOf(candidate.Update.Dependency.ToVersion)}, true
}

// majorOf mirrors the normalisation Newer applies, and answers -1 for a target
// with no numeric major so that every digest-only reference to one dependency
// stays on a single line where comparable can still hold them apart.
func majorOf(version string) int {
	value := strings.TrimSpace(version)
	if index := strings.Index(value, "@"); index >= 0 {
		value = value[:index]
	}
	value = strings.TrimPrefix(value, "v")
	if index := strings.IndexAny(value, "-+"); index >= 0 {
		value = value[:index]
	}
	major, _, _ := strings.Cut(value, ".")
	number, err := strconv.Atoi(major)
	if err != nil {
		return -1
	}
	return number
}

// supersedes reports whether candidate targets a strictly newer version than
// incumbent. Versions it cannot compare never displace an incumbent.
func supersedes(candidate, incumbent renovate.Update) bool {
	return renovate.Newer(candidate.Dependency.ToVersion, incumbent.Dependency.ToVersion) > 0
}

// comparable reports whether two candidates for the same dependency can be
// ordered at all. Two references that differ only by digest cannot, and closing
// either as superseded would discard a genuine update.
func comparable(a, b renovate.Update) bool {
	left, right := a.Dependency.ToVersion, b.Dependency.ToVersion
	return renovate.Newer(left, right) != 0 || renovate.Newer(right, left) != 0
}
