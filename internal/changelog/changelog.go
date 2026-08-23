// Package changelog resolves upstream release notes for a dependency bump.
//
// A configured override names the upstream repository outright and settles the
// answer on its own. Without one the chain is the Renovate pull request body,
// then the image's org.opencontainers.image.source annotation. When nothing
// resolves the assessment agent is expected to go looking, and records its
// result as ChangelogFromSearch.
package changelog

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/lkshrk/ops-pilot/internal/adapters/github"
	"github.com/lkshrk/ops-pilot/internal/adapters/renovate"
	"github.com/lkshrk/ops-pilot/internal/diagnostics"
	"github.com/lkshrk/ops-pilot/internal/domain"
)

// ImageSourceReader reads the source repository an image declares.
type ImageSourceReader interface {
	Resolve(ctx context.Context, reference string) (string, error)
}

// ReleaseReader lists the published releases of an upstream repository.
type ReleaseReader interface {
	Releases(ctx context.Context, repository string) (github.ReleaseHistory, error)
}

type Resolver struct {
	images    ImageSourceReader
	releases  ReleaseReader
	overrides map[string]string
	log       *diagnostics.Logger
}

func NewResolver(
	images ImageSourceReader,
	releases ReleaseReader,
	overrides []Override,
	log *diagnostics.Logger,
) *Resolver {
	index := make(map[string]string, len(overrides))
	for _, override := range overrides {
		index[override.Dependency] = override.Repository
	}
	return &Resolver{images: images, releases: releases, overrides: index, log: log}
}

type Override struct {
	Dependency string
	Repository string
}

// SourceIncomplete is an upstream whose releases were read and then suppressed
// because the range had a hole in it. Unlike domain.ChangelogNotFound the notes
// exist, so their absence from the assessment is a gap the caller must hold on
// rather than a dependency that publishes nothing.
const SourceIncomplete domain.ChangelogSource = "upstream_incomplete"

// SourceUnreadable is an upstream whose releases could not be read at all. It
// carries no evidence either way, so unlike domain.ChangelogNotFound it is not a
// finding the caller may merge on: the release notes may exist and say anything.
const SourceUnreadable domain.ChangelogSource = "upstream_unreadable"

// Resolve returns the highest-precedence changelog it can resolve. A result
// with source ChangelogNotFound is not an error: it means the caller must ask
// the agent to search, or escalate to manual approval.
func (r *Resolver) Resolve(ctx context.Context, update renovate.Update) (domain.Changelog, error) {
	dependency := update.Dependency
	notes := strings.TrimSpace(update.ReleaseNotes)
	body := domain.Changelog{
		Source:     domain.ChangelogFromPullRequest,
		Repository: update.Upstream,
		Text:       notes,
	}
	// The override is the only operator-authored source here; the inferred ones
	// exist to be corrected by it, never to be consulted ahead of it.
	if override := r.overrides[dependency.Name]; override != "" {
		if changelog, outcome := r.fromReleases(
			ctx, override, dependency, domain.ChangelogFromOverride,
		); outcome == lookupResolved {
			return changelog, nil
		}
		if notes != "" && sameRepository(update.Upstream, override) {
			r.log.Warnf("the changelog override %q resolved nothing for %q, so the pull request's own "+
				"release notes were served instead", override, dependency.Name)
			return body, nil
		}
		if notes != "" {
			// The attribution is untrusted pull request text, so it may go to the
			// redacted log and never into the result the prompt path renders.
			r.log.Warnf("release notes attributed to %q discarded: the configured override names %q",
				update.Upstream, override)
		}
		return domain.Changelog{Source: domain.ChangelogOverrideEmpty}, nil
	}
	if notes != "" {
		return body, nil
	}
	fallback := domain.Changelog{Source: domain.ChangelogNotFound}
	if repository := repositoryFromURL(update.Upstream); repository != "" {
		fallback.Repository = repository
		switch changelog, outcome := r.fromReleases(
			ctx, repository, dependency, domain.ChangelogFromPullRequest,
		); outcome {
		case lookupResolved:
			return changelog, nil
		case lookupIncomplete:
			fallback.Source = SourceIncomplete
		case lookupFailed:
			fallback.Source = SourceUnreadable
		}
	}
	if repository, sourceHint := r.annotatedRepository(ctx, dependency); sourceHint != "" {
		if repository == "" {
			if fallback.Repository == "" {
				fallback.Repository = sourceHint
			}
		} else if repository != fallback.Repository {
			switch changelog, outcome := r.fromReleases(
				ctx, repository, dependency, domain.ChangelogFromAnnotation,
			); outcome {
			case lookupResolved:
				return changelog, nil
			case lookupIncomplete:
				if fallback.Source != SourceUnreadable {
					fallback = domain.Changelog{Source: SourceIncomplete, Repository: repository}
				}
			case lookupFailed:
				fallback = domain.Changelog{Source: SourceUnreadable, Repository: repository}
			case lookupMissing:
				if fallback.Source == domain.ChangelogNotFound {
					fallback.Repository = repository
				}
			}
		}
	}
	return fallback, nil
}

// Each compare is wider than the one above it and none may narrow, so an
// attribution that matched before still matches.
func sameRepository(a, b string) bool {
	if a == "" {
		return false
	}
	if strings.EqualFold(a, b) {
		return true
	}
	left, right := undecorate(a), undecorate(b)
	if left != "" && strings.EqualFold(left, right) {
		return true
	}
	leftHost, leftRepository := repositoryOf(left)
	rightHost, rightRepository := repositoryOf(right)
	if leftRepository == "" || !strings.EqualFold(leftRepository, rightRepository) {
		return false
	}
	// Two forges carry the same owner/name without being the same project, so a
	// host present on both sides has to agree.
	return leftHost == "" || rightHost == "" || strings.EqualFold(leftHost, rightHost)
}

// undecorate removes what may differ between two spellings of one repository
// without changing which repository it is.
func undecorate(value string) string {
	trimmed := strings.TrimSpace(value)
	if _, after, found := strings.Cut(trimmed, "://"); found {
		trimmed = after
	}
	if _, after, found := strings.Cut(trimmed, "@"); found {
		trimmed = after
	}
	return strings.TrimSuffix(strings.TrimSuffix(trimmed, "/"), ".git")
}

// repositoryOf reports the forge host and the repository an undecorated value
// names, and nothing at all unless it names exactly one. Truncating a longer
// path to its first two segments would make every project in a group compare
// equal to its group and to its siblings, so an unrecognised shape has to
// compare unequal instead.
func repositoryOf(value string) (host, repository string) {
	consumedGitHub := false
	for _, prefix := range []string{"github.com/", "github.com:"} {
		if after, found := strings.CutPrefix(value, prefix); found {
			value, host, consumedGitHub = after, "github.com", true
			break
		}
	}
	if host == "" {
		// A group name may itself carry a dot, so a leading host is only dropped
		// when an owner and a name still survive it.
		if candidate, after, found := strings.Cut(value, "/"); found && isHost(candidate) && strings.Contains(after, "/") {
			value, host = after, candidate
		}
	}
	owner, name, found := strings.Cut(value, "/")
	if !found || owner == "" {
		return "", ""
	}
	if strings.Contains(name, "/") {
		// GitHub nests no groups, so past a consumed github.com prefix a third
		// segment is a page path; anywhere else it may be a subgroup.
		if !consumedGitHub {
			return "", ""
		}
		name, _, _ = strings.Cut(name, "/")
	}
	if name == "" {
		return "", ""
	}
	return host, owner + "/" + name
}

func isHost(segment string) bool {
	return segment == "localhost" || strings.ContainsAny(segment, ".:")
}

// annotatedRepository reads org.opencontainers.image.source from the candidate
// image. Chart dependencies and unreachable registries simply yield nothing: a
// registry that cannot be read hides whether the image declares a source at all,
// and the dependencies that declare none are answered by sending the agent
// searching, so a failure here is not evidence that anything was missed.
func (r *Resolver) annotatedRepository(ctx context.Context, dependency domain.Dependency) (string, string) {
	// Real Renovate bodies often carry no Type column, leaving the kind unset;
	// an unset kind must still reach the registry rather than silently skipping.
	if r.images == nil || dependency.Kind == domain.DependencyKindHelm || dependency.Name == "" {
		return "", ""
	}
	reference := imageReference(dependency.Name, dependency.ToVersion)
	source, err := r.images.Resolve(ctx, reference)
	if err != nil {
		r.log.Warnf("the source annotation of %q could not be read: %v", reference, err)
		return "", ""
	}
	if source == "" {
		return "", ""
	}
	return repositoryFromURL(source), strings.TrimSpace(source)
}

// imageReference joins the name and the target version the way a registry
// expects: @ for a bare digest, : for a tag or the combined tag@digest form.
func imageReference(name, version string) string {
	version = strings.TrimSpace(version)
	if version == "" {
		return name
	}
	if renovate.IsDigest(version) {
		return name + "@" + version
	}
	return name + ":" + version
}

// repositoryFromURL reduces a GitHub project URL to owner/name.
func repositoryFromURL(value string) string {
	trimmed := strings.TrimSpace(value)
	for _, prefix := range []string{"https://github.com/", "http://github.com/", "git@github.com:", "github.com/"} {
		if after, found := strings.CutPrefix(trimmed, prefix); found {
			trimmed = after
			break
		}
	}
	trimmed = strings.TrimSuffix(strings.TrimSuffix(trimmed, "/"), ".git")
	owner, rest, found := strings.Cut(trimmed, "/")
	if !found || owner == "" {
		return ""
	}
	name, _, _ := strings.Cut(rest, "/")
	if name == "" {
		return ""
	}
	return owner + "/" + name
}

// lookup separates an upstream that offered nothing for this range from one
// whose releases were read and then declined and one that could not be read at
// all, which the caller answers for differently.
type lookup int

const (
	lookupMissing lookup = iota
	lookupFailed
	lookupIncomplete
	lookupResolved
)

func (r *Resolver) fromReleases(
	ctx context.Context,
	repository string,
	dependency domain.Dependency,
	source domain.ChangelogSource,
) (domain.Changelog, lookup) {
	if r.releases == nil {
		return domain.Changelog{}, lookupMissing
	}
	history, err := r.releases.Releases(ctx, repository)
	if err != nil {
		r.log.Warnf("releases of %q could not be read: %v", repository, err)
		return domain.Changelog{}, lookupFailed
	}
	releases := history.Releases
	if history.Truncated && !accountsForRange(releases, dependency.FromVersion, dependency.ToVersion) {
		r.log.Warnf("releases of %q were read only as far back as the page budget allows, "+
			"which does not reach %s", repository, dependency.FromVersion)
		return domain.Changelog{}, lookupFailed
	}
	if len(releases) == 0 {
		return domain.Changelog{}, lookupMissing
	}
	selected, complete := Between(releases, dependency.FromVersion, dependency.ToVersion)
	if !complete {
		return domain.Changelog{}, lookupIncomplete
	}
	if len(selected) == 0 {
		return domain.Changelog{}, lookupMissing
	}
	var text strings.Builder
	for _, release := range selected {
		title := release.Name
		if title == "" {
			title = release.TagName
		}
		fmt.Fprintf(&text, "## %s\n\n%s\n\n", title, strings.TrimSpace(release.Body))
	}
	return domain.Changelog{
		Source:     source,
		Repository: repository,
		URL:        selected[0].HTMLURL,
		Text:       strings.TrimSpace(text.String()),
	}, lookupResolved
}

// accountsForRange reports whether a release listing that was cut short still
// holds every release in the range. The listing is ordered by publication and
// the range is a version interval, and the two are independent: a repository
// maintaining an older line alongside its newest one publishes low versions late
// and high versions early, so a release older than the range start proves
// nothing about what lies below the cut. Three facts together do.
//
// The release the range starts at must be in the listing, because then every
// release published after it is in the listing too and only a release published
// before it can be missing. Nothing the range could reach may sit below it,
// which is that inversion showing up where it can still be seen. And the run
// down to it must fall in version order, which is the only evidence available
// here that the pages below the cut hold lower versions rather than more of a
// line that was being published out of version order all along.
func accountsForRange(releases []github.Release, from, to string) bool {
	anchor := -1
	for index, release := range releases {
		order, comparable := compare(release.TagName, from)
		if !comparable {
			return false
		}
		if index > 0 {
			step, ordered := compare(release.TagName, releases[index-1].TagName)
			if !ordered || step > 0 {
				return false
			}
		}
		if order == 0 {
			anchor = index
			break
		}
	}
	if anchor < 0 {
		return false
	}
	for _, release := range releases[anchor+1:] {
		if reachableByRange(release.TagName, from, to) {
			return false
		}
	}
	return true
}

// reachableByRange reports whether Between could select the tag or count it as a
// hole: inside the range outright, the endpoint carrying an extra build segment,
// or a version inside the range embedded in a tag Between cannot parse.
func reachableByRange(tag, from, to string) bool {
	if newerThanFrom, comparable := compare(tag, from); comparable && newerThanFrom > 0 {
		if newerThanTo, comparable := compare(tag, to); comparable && newerThanTo <= 0 {
			return true
		}
	}
	return extendsToPrefix(tag, to) || embeddedVersionInRange(tag, from, to)
}

// Between selects the releases strictly newer than from and no newer than to,
// newest first. Tags are matched with a tolerant version comparison so a v
// prefix or a build segment does not hide a release, while prereleases stay
// ordered below their own release.
//
// It also reports whether the selection accounts for every release in the range.
// A tag it cannot parse is a release it cannot exclude, and one lying between two
// releases it did select is inside the range by position, so the selection has a
// hole in it and is not the changelog for that range.
func Between(releases []github.Release, from, to string) ([]github.Release, bool) {
	var selected []github.Release
	var unplaceable []int
	var excludedTargets []int
	first, last := -1, -1
	for index, release := range releases {
		tag := release.TagName
		if tag == "" {
			continue
		}
		if _, _, parseable := splitVersion(tag); !parseable {
			unplaceable = append(unplaceable, index)
			continue
		}
		newerThanFrom, comparable := compare(tag, from)
		if !comparable || newerThanFrom <= 0 {
			continue
		}
		newerThanTo, comparable := compare(tag, to)
		if !comparable || newerThanTo > 0 {
			if extendsToPrefix(tag, to) {
				excludedTargets = append(excludedTargets, index)
			}
			continue
		}
		if first < 0 {
			first = index
		}
		last = index
		selected = append(selected, release)
	}
	if len(selected) == 0 {
		// Nothing matched the range: fall back to the exact target tag, which
		// covers projects whose tags are not comparable to the image version.
		order, ordered := compare(to, from)
		for _, release := range releases {
			if !sameVersion(release.TagName, to) {
				continue
			}
			// An inverted range takes the releases above to out of production and
			// the target is not one of them, so it cannot stand in for their notes.
			if ordered && order < 0 {
				return nil, false
			}
			return []github.Release{release}, true
		}
		return nil, true
	}
	reachesTo := selectionReachesTo(selected, to)
	for _, index := range unplaceable {
		if index > first && index < last {
			return selected, false
		}
		// A selected tag equal to reachesTo cannot vouch for a same-version decoy: a
		// monorepo stamps the component onto the endpoint (sonarr-4.0.19), so an
		// unplaceable tag whose embedded version is inside the range is a hole even
		// when a bare release already reaches to.
		if embeddedVersionInRange(releases[index].TagName, from, to) {
			return selected, false
		}
		// An unplaceable tag above the selection is the endpoint itself only when
		// nothing selected already equals to; otherwise it is a rolling tag past
		// the range (a nightly) and gating on it would silence the dependency.
		if index < first && !reachesTo {
			return selected, false
		}
	}
	if !reachesTo && len(excludedTargets) > 0 {
		// The endpoint was tagged with an extra build segment and ordered above to.
		// With one candidate it is unambiguously the target and is recovered; more
		// than one means the endpoint cannot be identified, so fail safe. An
		// under-specified to cannot be extended by a build segment either: its extra
		// segment is a plausible newer patch, so recovering it would serve notes past
		// the range — leave the endpoint unresolved and let the agent search.
		if len(excludedTargets) > 1 || !fullVersion(to) {
			return selected, false
		}
		selected = append([]github.Release{releases[excludedTargets[0]]}, selected...)
	}
	return selected, true
}

// extendsToPrefix reports whether tag is to with extra trailing numeric segments,
// which is how upstream stamps a build revision onto the release the manifest
// spells as to.
func extendsToPrefix(tag, to string) bool {
	tagCore, _, tagOK := splitVersion(tag)
	toCore, _, toOK := splitVersion(to)
	if !tagOK || !toOK || len(tagCore) <= len(toCore) {
		return false
	}
	for i := range toCore {
		if tagCore[i] != toCore[i] {
			return false
		}
	}
	return true
}

// embeddedVersionInRange reports whether a tag Between could not parse still
// carries a version strictly newer than from and no newer than to, which is how
// a monorepo tag (sonarr-4.0.19) hides a real in-range release behind a prefix.
func embeddedVersionInRange(tag, from, to string) bool {
	for _, candidate := range versionCandidates(tag) {
		newerThanFrom, ok := compare(candidate, from)
		if !ok || newerThanFrom <= 0 {
			continue
		}
		newerThanTo, ok := compare(candidate, to)
		if !ok || newerThanTo > 0 {
			continue
		}
		return true
	}
	return false
}

func versionCandidates(tag string) []string {
	var candidates []string
	before, prev := rune(-1), rune(-1)
	for i, curr := range tag {
		if i == 0 || startsVersionCandidate(before, prev, curr) {
			if candidate, ok := versionAt(tag[i:]); ok {
				candidates = append(candidates, candidate)
			}
		}
		before, prev = prev, curr
	}
	return candidates
}

// versionAt reads the version a tag suffix opens with, restoring the separator
// of a prerelease fused onto the core (4.0.19rc1). Only a dotted core is split:
// a letter run fused onto a bare number is as plausibly a hex fragment (4a3f).
func versionAt(value string) (string, bool) {
	if _, _, ok := splitVersion(value); ok {
		return value, true
	}
	core, prerelease, found := cutFusedPrerelease(value)
	if !found || !strings.Contains(core, ".") {
		return "", false
	}
	unfused := core + "-" + prerelease
	if _, _, ok := splitVersion(unfused); !ok {
		return "", false
	}
	return unfused, true
}

func cutFusedPrerelease(value string) (string, string, bool) {
	prev := rune(-1)
	for i, curr := range value {
		if isDecimalDigit(prev) && unicode.IsLetter(curr) {
			return value[:i], value[i:], true
		}
		prev = curr
	}
	return "", "", false
}

func startsVersionCandidate(before, prev, curr rune) bool {
	if isTagSeparator(prev) {
		return true
	}
	if prev == '.' {
		// A dot between digits is version-internal, so it never begins a candidate.
		return !isDecimalDigit(before)
	}
	return fusedVersionStart(prev, curr)
}

func isTagSeparator(r rune) bool {
	return r == '-' || r == '/' || r == '_' || r == ' ' || r == ':'
}

// A monorepo tag may drop the separator (sonarr4.0.19, go1.21.0), so a digit
// fused onto a letter run also begins a version candidate.
func fusedVersionStart(prev, curr rune) bool {
	return unicode.IsLetter(prev) && isDecimalDigit(curr)
}

func isDecimalDigit(r rune) bool {
	return r >= '0' && r <= '9'
}

// fullVersion reports whether value spells at least a major.minor.patch, the
// precision below which an extra trailing segment is a patch rather than a build
// stamp.
func fullVersion(value string) bool {
	core, _, ok := splitVersion(value)
	return ok && len(core) >= 3
}

func selectionReachesTo(selected []github.Release, to string) bool {
	for _, release := range selected {
		if order, ok := compare(release.TagName, to); ok && order == 0 {
			return true
		}
		if sameVersion(release.TagName, to) {
			return true
		}
	}
	return false
}

func sameVersion(a, b string) bool {
	return strings.TrimPrefix(a, "v") == strings.TrimPrefix(b, "v")
}

// compare orders two versions, reporting false when either side is not a
// numeric version the other can be compared against.
func compare(a, b string) (int, bool) {
	leftCore, leftPre, leftOK := splitVersion(a)
	rightCore, rightPre, rightOK := splitVersion(b)
	if !leftOK || !rightOK {
		return 0, false
	}
	if order := compareNumbers(leftCore, rightCore); order != 0 {
		return order, true
	}
	return comparePrerelease(leftPre, rightPre), true
}

func splitVersion(value string) ([]int, []string, bool) {
	value = strings.TrimSpace(value)
	if index := strings.Index(value, "@"); index >= 0 {
		value = value[:index]
	}
	value = strings.TrimPrefix(value, "v")
	if index := strings.Index(value, "+"); index >= 0 {
		value = value[:index]
	}
	core, prerelease, _ := strings.Cut(value, "-")
	if core == "" {
		return nil, nil, false
	}
	segments := strings.Split(core, ".")
	numbers := make([]int, 0, len(segments))
	for _, segment := range segments {
		number, err := strconv.Atoi(segment)
		if err != nil || number < 0 {
			return nil, nil, false
		}
		numbers = append(numbers, number)
	}
	if prerelease == "" {
		return numbers, nil, true
	}
	return numbers, strings.Split(prerelease, "."), true
}

// Omitted trailing segments are zeroes, not a lower version: 1.1 is 1.1.0, so
// ordering by segment count would exclude the target release as newer than the
// version Renovate wrote for it.
func compareNumbers(left, right []int) int {
	for i := 0; i < len(left) || i < len(right); i++ {
		switch {
		case segmentAt(left, i) > segmentAt(right, i):
			return 1
		case segmentAt(left, i) < segmentAt(right, i):
			return -1
		}
	}
	return 0
}

func segmentAt(numbers []int, index int) int {
	if index < len(numbers) {
		return numbers[index]
	}
	return 0
}

func comparePrerelease(left, right []string) int {
	switch {
	case len(left) == 0 && len(right) == 0:
		return 0
	case len(left) == 0:
		return 1
	case len(right) == 0:
		return -1
	}
	for i := 0; i < len(left) && i < len(right); i++ {
		if order := compareIdentifier(left[i], right[i]); order != 0 {
			return order
		}
	}
	switch {
	case len(left) > len(right):
		return 1
	case len(left) < len(right):
		return -1
	}
	return 0
}

func compareIdentifier(left, right string) int {
	leftNumber, leftErr := strconv.Atoi(left)
	rightNumber, rightErr := strconv.Atoi(right)
	switch {
	case leftErr == nil && rightErr == nil:
		switch {
		case leftNumber > rightNumber:
			return 1
		case leftNumber < rightNumber:
			return -1
		}
		return 0
	case leftErr == nil:
		return -1
	case rightErr == nil:
		return 1
	}
	return strings.Compare(left, right)
}
