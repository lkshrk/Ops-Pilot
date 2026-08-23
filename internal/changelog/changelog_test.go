package changelog_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/lkshrk/ops-pilot/internal/adapters/github"
	"github.com/lkshrk/ops-pilot/internal/adapters/renovate"
	"github.com/lkshrk/ops-pilot/internal/changelog"
	"github.com/lkshrk/ops-pilot/internal/diagnostics"
	"github.com/lkshrk/ops-pilot/internal/domain"
)

type fakeImages struct {
	source string
	err    error
}

func (f fakeImages) Resolve(context.Context, string) (string, error) { return f.source, f.err }

type fakeReleases struct {
	byRepository map[string][]github.Release
	truncated    bool
	err          error
	asked        []string
}

func (f *fakeReleases) Releases(_ context.Context, repository string) (github.ReleaseHistory, error) {
	f.asked = append(f.asked, repository)
	if f.err != nil {
		return github.ReleaseHistory{}, f.err
	}
	releases, found := f.byRepository[repository]
	if !found {
		return github.ReleaseHistory{}, errors.New("no such repository")
	}
	return github.ReleaseHistory{Releases: releases, Truncated: f.truncated}, nil
}

func sonarrUpdate() renovate.Update {
	return renovate.Update{Dependency: domain.Dependency{
		Name:        "ghcr.io/home-operations/sonarr",
		Kind:        domain.DependencyKindDocker,
		FromVersion: "4.0.14",
		ToVersion:   "4.0.19",
	}}
}

func TestPullRequestBodyWinsWithoutAnyNetworkCall(t *testing.T) {
	releases := &fakeReleases{}
	update := sonarrUpdate()
	update.ReleaseNotes = "Fixed a crash on import."
	update.Upstream = "Sonarr/Sonarr"

	got, err := changelog.NewResolver(fakeImages{}, releases, nil, nil).Resolve(context.Background(), update)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Source != domain.ChangelogFromPullRequest || got.Text != "Fixed a crash on import." {
		t.Fatalf("want the body verbatim, got %+v", got)
	}
	if len(releases.asked) != 0 {
		t.Fatalf("want no upstream lookup, got %v", releases.asked)
	}
}

func TestPullRequestRepositoryIsTriedWhenItsNotesAreEmpty(t *testing.T) {
	releases := &fakeReleases{byRepository: map[string][]github.Release{
		"Sonarr/Sonarr": {{TagName: "v4.0.19", Body: "Import fix", HTMLURL: "https://x"}},
	}}
	update := sonarrUpdate()
	update.Upstream = "Sonarr/Sonarr"

	got, err := changelog.NewResolver(fakeImages{}, releases, nil, nil).Resolve(context.Background(), update)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Source != domain.ChangelogFromPullRequest || got.Repository != "Sonarr/Sonarr" {
		t.Fatalf("want the pull request's upstream repository, got %+v", got)
	}
}

func TestMissingReleaseKeepsTheRepositoryHintForTheAgent(t *testing.T) {
	releases := &fakeReleases{byRepository: map[string][]github.Release{"Sonarr/Sonarr": {}}}
	update := sonarrUpdate()
	update.Upstream = "Sonarr/Sonarr"

	got, err := changelog.NewResolver(fakeImages{}, releases, nil, nil).Resolve(context.Background(), update)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Source != domain.ChangelogNotFound || got.Repository != "Sonarr/Sonarr" {
		t.Fatalf("want a not-found result that keeps the repository hint, got %+v", got)
	}
}

func TestAnnotationResolvesTheUpstreamRepository(t *testing.T) {
	releases := &fakeReleases{byRepository: map[string][]github.Release{
		"Sonarr/Sonarr": {{TagName: "v4.0.19", Name: "4.0.19", Body: "Import fix", HTMLURL: "https://x"}},
	}}
	resolver := changelog.NewResolver(
		fakeImages{source: "https://github.com/Sonarr/Sonarr"},
		releases,
		nil,
		nil,
	)

	got, err := resolver.Resolve(context.Background(), sonarrUpdate())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Source != domain.ChangelogFromAnnotation || got.Repository != "Sonarr/Sonarr" {
		t.Fatalf("want the annotated repository, got %+v", got)
	}
	if !strings.Contains(got.Text, "Import fix") {
		t.Fatalf("release body missing: %q", got.Text)
	}
}

func TestNonGitHubAnnotationIsKeptAsAnOfficialSourceHint(t *testing.T) {
	resolver := changelog.NewResolver(
		fakeImages{source: "https://gitlab.com/glitchtip/glitchtip-backend"},
		&fakeReleases{}, nil, nil,
	)

	got, err := resolver.Resolve(context.Background(), sonarrUpdate())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Source != domain.ChangelogNotFound || got.Repository != "https://gitlab.com/glitchtip/glitchtip-backend" {
		t.Fatalf("the non-GitHub source hint was discarded: %+v", got)
	}
}

func TestIncompletePullRequestAttributionDoesNotHideAValidImageAnnotation(t *testing.T) {
	releases := &fakeReleases{byRepository: map[string][]github.Release{
		"Wrong/Wrong":   {{TagName: "v9.0.0", Body: "Unrelated"}},
		"Sonarr/Sonarr": {{TagName: "v4.0.19", Body: "Import fix", HTMLURL: "https://x"}},
	}}
	update := sonarrUpdate()
	update.Upstream = "Wrong/Wrong"
	resolver := changelog.NewResolver(
		fakeImages{source: "https://github.com/Sonarr/Sonarr"}, releases, nil, nil,
	)

	got, err := resolver.Resolve(context.Background(), update)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Source != domain.ChangelogFromAnnotation || got.Repository != "Sonarr/Sonarr" {
		t.Fatalf("the trusted image annotation did not repair the PR attribution: %+v", got)
	}
}

// The home-operations images repackage upstream and publish no source
// annotation, which is exactly what the override exists for.
func TestOverrideCoversAnImageWithNoSourceAnnotation(t *testing.T) {
	releases := &fakeReleases{byRepository: map[string][]github.Release{
		"Sonarr/Sonarr": {{TagName: "v4.0.19", Body: "Import fix"}},
	}}
	resolver := changelog.NewResolver(
		fakeImages{err: errors.New("no annotation")},
		releases,
		[]changelog.Override{{Dependency: "ghcr.io/home-operations/sonarr", Repository: "Sonarr/Sonarr"}},
		nil,
	)

	got, err := resolver.Resolve(context.Background(), sonarrUpdate())
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Source != domain.ChangelogFromOverride {
		t.Fatalf("want the configured override, got %+v", got)
	}
}

func TestUnresolvableDependencyReportsNotFoundRatherThanFailing(t *testing.T) {
	resolver := changelog.NewResolver(fakeImages{err: errors.New("unreachable")}, &fakeReleases{}, nil, nil)

	got, err := resolver.Resolve(context.Background(), sonarrUpdate())
	if err != nil {
		t.Fatalf("resolve must not fail: %v", err)
	}
	if got.Source != domain.ChangelogNotFound {
		t.Fatalf("want not-found, got %+v", got)
	}
}

// The precedence rule: a configured override is the only operator-authored
// source in the chain, so it decides the upstream repository outright. The
// pull request body is Renovate's inference, the annotation is the image
// publisher's, and both exist to be corrected by an override rather than
// consulted ahead of it.
func TestExplicitOverrideOutranksEveryInferredChangelogSource(t *testing.T) {
	const (
		overrideRepository = "Sonarr/Sonarr"
		repackager         = "home-operations/containers"
	)
	release := github.Release{TagName: "v4.0.19", Body: "Import fix"}

	for _, testCase := range []struct {
		name              string
		overrides         []changelog.Override
		notes             string
		upstream          string
		annotation        string
		wantSource        domain.ChangelogSource
		wantRepository    string
		wantRegistryAsked bool
	}{
		{
			name:           "override outranks the body and the annotation together",
			overrides:      []changelog.Override{{Dependency: "ghcr.io/home-operations/sonarr", Repository: overrideRepository}},
			notes:          "chore: update sonarr",
			upstream:       repackager,
			annotation:     "https://github.com/" + repackager,
			wantSource:     domain.ChangelogFromOverride,
			wantRepository: overrideRepository,
		},
		{
			name:           "override outranks the pull request body",
			overrides:      []changelog.Override{{Dependency: "ghcr.io/home-operations/sonarr", Repository: overrideRepository}},
			notes:          "chore: update sonarr",
			upstream:       repackager,
			wantSource:     domain.ChangelogFromOverride,
			wantRepository: overrideRepository,
		},
		{
			name:           "override outranks the image annotation",
			overrides:      []changelog.Override{{Dependency: "ghcr.io/home-operations/sonarr", Repository: overrideRepository}},
			annotation:     "https://github.com/" + repackager,
			wantSource:     domain.ChangelogFromOverride,
			wantRepository: overrideRepository,
		},
		{
			name:              "the body outranks the annotation when no override is configured",
			notes:             "Fixed a crash on import.",
			upstream:          overrideRepository,
			annotation:        "https://github.com/" + repackager,
			wantSource:        domain.ChangelogFromPullRequest,
			wantRepository:    overrideRepository,
			wantRegistryAsked: false,
		},
		{
			name:              "the annotation is used only when nothing above it resolves",
			annotation:        "https://github.com/" + overrideRepository,
			wantSource:        domain.ChangelogFromAnnotation,
			wantRepository:    overrideRepository,
			wantRegistryAsked: true,
		},
		{
			name:       "an override that resolves nothing does not fall through to a source it contradicts",
			overrides:  []changelog.Override{{Dependency: "ghcr.io/home-operations/sonarr", Repository: "Sonarr/Empty"}},
			notes:      "chore: update sonarr",
			upstream:   repackager,
			annotation: "https://github.com/" + overrideRepository,
			wantSource: domain.ChangelogOverrideEmpty,
		},
		{
			name:           "an override that resolves nothing keeps notes attributed to the repository it names",
			overrides:      []changelog.Override{{Dependency: "ghcr.io/home-operations/sonarr", Repository: "Sonarr/Empty"}},
			notes:          "Fixed a crash on import.",
			upstream:       "sonarr/empty",
			annotation:     "https://github.com/" + repackager,
			wantSource:     domain.ChangelogFromPullRequest,
			wantRepository: "sonarr/empty",
		},
		{
			name:              "nothing resolves at all",
			wantSource:        domain.ChangelogNotFound,
			wantRegistryAsked: true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			images := &recordingImages{source: testCase.annotation}
			releases := &fakeReleases{byRepository: map[string][]github.Release{
				overrideRepository: {release},
				repackager:         {release},
				"Sonarr/Empty":     nil,
			}}
			update := sonarrUpdate()
			update.ReleaseNotes = testCase.notes
			update.Upstream = testCase.upstream

			got, err := changelog.NewResolver(images, releases, testCase.overrides, nil).Resolve(context.Background(), update)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got.Source != testCase.wantSource {
				t.Fatalf("want source %q, got %+v", testCase.wantSource, got)
			}
			if got.Repository != testCase.wantRepository {
				t.Fatalf("want repository %q, got %q", testCase.wantRepository, got.Repository)
			}
			if asked := len(images.asked) > 0; asked != testCase.wantRegistryAsked {
				t.Fatalf("want registry asked %v, got %v", testCase.wantRegistryAsked, images.asked)
			}
		})
	}
}

// An override that resolves no releases still has to decide whether the body's
// notes belong to the repository it names. Renovate writes that attribution in
// whatever form the manifest carried, so the comparison has to survive the same
// URL prefixes and suffixes the annotation path already normalises away.
func TestAnOverrideKeepsTheBodyWhenTheAttributionNamesTheSameRepositoryInAnyForm(t *testing.T) {
	const notes = "BREAKING: the database schema was rewritten; downgrade is impossible."

	for _, testCase := range []struct {
		name     string
		override string
		upstream string
		wantKept bool
	}{
		{name: "identical", override: "Sonarr/Sonarr", upstream: "Sonarr/Sonarr", wantKept: true},
		{name: "different case", override: "Sonarr/Sonarr", upstream: "sonarr/sonarr", wantKept: true},
		{name: "git suffix", override: "Sonarr/Sonarr", upstream: "Sonarr/Sonarr.git", wantKept: true},
		{name: "trailing slash", override: "Sonarr/Sonarr", upstream: "Sonarr/Sonarr/", wantKept: true},
		{name: "https url", override: "Sonarr/Sonarr", upstream: "https://github.com/Sonarr/Sonarr", wantKept: true},
		{name: "http url", override: "Sonarr/Sonarr", upstream: "http://github.com/Sonarr/Sonarr", wantKept: true},
		{name: "host only", override: "Sonarr/Sonarr", upstream: "github.com/Sonarr/Sonarr", wantKept: true},
		{name: "ssh url", override: "Sonarr/Sonarr", upstream: "git@github.com:Sonarr/Sonarr.git", wantKept: true},
		{name: "url override against a bare attribution", override: "https://github.com/Sonarr/Sonarr", upstream: "Sonarr/Sonarr", wantKept: true},
		{name: "both sides urls", override: "github.com/Sonarr/Sonarr.git", upstream: "https://github.com/sonarr/sonarr/", wantKept: true},

		{name: "no attribution", override: "Sonarr/Sonarr", upstream: "", wantKept: false},
		{name: "a different repository", override: "Sonarr/Sonarr", upstream: "home-operations/containers", wantKept: false},
		{name: "a different name under the same owner", override: "Sonarr/Sonarr", upstream: "https://github.com/Sonarr/Radarr", wantKept: false},
		{name: "an attribution with no owner", override: "Sonarr/Sonarr", upstream: "sonarr", wantKept: false},

		// A registry namespace, a GitLab group and a self-hosted group are all
		// prefixes of the repository the override names. Truncating either side to
		// its first path segment would collide them into a match and serve the
		// repackager's or a sibling project's notes under the pull_request_body
		// label, which is the misinformation the override exists to suppress.
		{name: "a registry namespace is not the image under it", override: "ghcr.io/home-operations/sonarr", upstream: "ghcr.io/home-operations", wantKept: false},
		{name: "a gitlab group is not a project in it", override: "gitlab.com/mygroup/project-b", upstream: "gitlab.com/mygroup", wantKept: false},
		{name: "a self-hosted group is not a project in it", override: "git.example.com/team/project-b", upstream: "git.example.com/team", wantKept: false},
		{name: "two projects in the same gitlab group", override: "gitlab.com/mygroup/project-b", upstream: "gitlab.com/mygroup/project-a", wantKept: false},
		{name: "a repository path is not the repository", override: "Sonarr/Sonarr", upstream: "Sonarr/Sonarr/tree/main", wantKept: false},

		// Reachable: config accepts the override "github.com/Sonarr", and a
		// "github.com/Sonarr/Sonarr (dep)" summary yields exactly that upstream.
		// Neither side reduces to owner/name, so only the literal compare decides.
		{name: "an override that reduces to no repository still matches itself", override: "github.com/Sonarr", upstream: "github.com/Sonarr", wantKept: true},
		{name: "a host-qualified project matches across schemes", override: "gitlab.com/mygroup/project-b", upstream: "https://gitlab.com/mygroup/project-b", wantKept: true},

		// Renovate writes the attribution in whatever form the manifest carried,
		// so a bare override and a host-qualified attribution name one repository.
		// A host on both sides still has to agree: two forges carry the same
		// owner/name without being the same project.
		{name: "a bare override against a host-qualified attribution", override: "mygroup/project-b", upstream: "gitlab.com/mygroup/project-b", wantKept: true},
		{name: "a host-qualified override against a bare attribution", override: "gitlab.com/mygroup/project-b", upstream: "mygroup/project-b", wantKept: true},
		{name: "a bare override against a self-hosted attribution", override: "team/project-b", upstream: "https://git.example.com/team/project-b.git", wantKept: true},
		{name: "an owner whose name carries a dot is not a host", override: "my.group/project", upstream: "https://gitlab.com/my.group/project", wantKept: true},
		{name: "the same owner and name on two different forges", override: "gitlab.com/mygroup/project-b", upstream: "github.com/mygroup/project-b", wantKept: false},
		{name: "a github override against a gitlab attribution", override: "https://github.com/mygroup/project-b", upstream: "https://gitlab.com/mygroup/project-b", wantKept: false},

		// Past a consumed github.com prefix the segment roles are known: GitHub
		// nests no groups, so the third segment on is a page path. Without the
		// prefix, and on any other host, a third segment may be a subgroup and the
		// value names no single repository.
		{name: "a github page path names the project it is under", override: "Sonarr/Sonarr", upstream: "https://github.com/Sonarr/Sonarr/tree/main", wantKept: true},
		{name: "a github releases path names the project it is under", override: "Sonarr/Sonarr", upstream: "github.com/Sonarr/Sonarr/releases/tag/v4.0.19", wantKept: true},
		{name: "a gitlab subgroup path is not the project of the same name", override: "gitlab.com/group/project", upstream: "gitlab.com/group/subgroup/project", wantKept: false},
		{name: "a github owner is not a repository under it", override: "github.com/Sonarr/Sonarr", upstream: "github.com/Sonarr", wantKept: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			update := sonarrUpdate()
			update.ReleaseNotes = notes
			update.Upstream = testCase.upstream

			resolver := changelog.NewResolver(
				fakeImages{err: errors.New("no annotation")},
				&fakeReleases{},
				[]changelog.Override{{Dependency: update.Dependency.Name, Repository: testCase.override}},
				nil,
			)

			got, err := resolver.Resolve(context.Background(), update)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			wantSource := domain.ChangelogOverrideEmpty
			if testCase.wantKept {
				wantSource = domain.ChangelogFromPullRequest
			}
			if got.Source != wantSource {
				t.Fatalf("want source %q, got %+v", wantSource, got)
			}
			if testCase.wantKept && got.Text != notes {
				t.Fatalf("want the notes kept verbatim, got %q", got.Text)
			}
		})
	}
}

// Discarding attributed notes leaves the operator reading "No changelog found
// automatically", which is false: notes were found, attributed, and thrown away
// because the override names something else. The discard has to leave a trace
// where an operator looks after a surprising verdict, and the attribution itself
// is untrusted pull request text, so it may reach the redacted log and must not
// reach anything that renders it back as a source.
func TestADiscardedAttributionIsLoggedAndNeverReturned(t *testing.T) {
	const notes = "BREAKING: the database schema was rewritten; downgrade is impossible."

	for _, testCase := range []struct {
		name       string
		notes      string
		upstream   string
		wantLogged bool
	}{
		{name: "notes attributed to a repository the override contradicts", notes: notes, upstream: "home-operations/containers", wantLogged: true},
		{name: "notes the body carried no attribution for", notes: notes, upstream: "", wantLogged: true},
		{name: "no notes to discard", notes: "", upstream: "home-operations/containers", wantLogged: false},
		{name: "notes the override agrees with", notes: notes, upstream: "Sonarr/Sonarr", wantLogged: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var written strings.Builder
			update := sonarrUpdate()
			update.ReleaseNotes = testCase.notes
			update.Upstream = testCase.upstream

			got, err := changelog.NewResolver(
				fakeImages{err: errors.New("no annotation")},
				&fakeReleases{},
				[]changelog.Override{{Dependency: update.Dependency.Name, Repository: "Sonarr/Sonarr"}},
				diagnostics.NewLogger(&written, nil),
			).Resolve(context.Background(), update)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}

			logged := strings.Contains(written.String(), "discarded")
			if logged != testCase.wantLogged {
				t.Fatalf("want the discard logged %v, got log %q", testCase.wantLogged, written.String())
			}
			if !testCase.wantLogged {
				return
			}
			if !strings.Contains(written.String(), "Sonarr/Sonarr") {
				t.Fatalf("want the override named in the log, got %q", written.String())
			}
			if strings.Contains(written.String(), testCase.notes) {
				t.Fatalf("the discarded body must not be copied into the log, got %q", written.String())
			}
			// The prompt path interpolates a Changelog with no redactor, so an
			// attribution the resolver refused to trust may not ride out on the
			// result. cli/approver.go guards the same thing on its own side.
			if got.Source != domain.ChangelogOverrideEmpty || got.Repository != "" || got.Text != "" {
				t.Fatalf("a discarded attribution must not survive into the result, got %+v", got)
			}
		})
	}
}

// run.go reads a discard off the source alone: it tells the operator their notes
// were thrown away exactly when the result is ChangelogOverrideEmpty and the
// update carried notes and an attribution. That label is only true while
// ChangelogOverrideEmpty is the one source this resolver can return empty-handed
// after being handed notes. A new path returning any other source with the notes
// dropped would leave run.go silently claiming a changelog it does not have.
func TestOverrideEmptyIsTheOnlySourceReturnedEmptyAfterNotesWereOffered(t *testing.T) {
	const notes = "BREAKING: the database schema was rewritten; downgrade is impossible."
	const dependency = "ghcr.io/home-operations/sonarr"
	release := github.Release{TagName: "v4.0.19", Body: "Import fix"}

	for _, testCase := range []struct {
		name       string
		overrides  []changelog.Override
		upstream   string
		annotation string
		images     changelog.ImageSourceReader
		noReleases bool
	}{
		{
			name:     "no override, so the body is the changelog",
			upstream: "Sonarr/Sonarr",
		},
		{
			name:       "no override and an annotation that would resolve",
			upstream:   "Sonarr/Sonarr",
			annotation: "https://github.com/Sonarr/Sonarr",
		},
		{
			name:      "an override that resolves releases of its own",
			overrides: []changelog.Override{{Dependency: dependency, Repository: "Sonarr/Sonarr"}},
			upstream:  "home-operations/containers",
		},
		{
			name:      "an override the attribution agrees with",
			overrides: []changelog.Override{{Dependency: dependency, Repository: "Sonarr/Empty"}},
			upstream:  "sonarr/empty",
		},
		{
			name:      "an override the attribution contradicts",
			overrides: []changelog.Override{{Dependency: dependency, Repository: "Sonarr/Empty"}},
			upstream:  "home-operations/containers",
		},
		{
			name:      "an override and notes carrying no attribution at all",
			overrides: []changelog.Override{{Dependency: dependency, Repository: "Sonarr/Empty"}},
		},
		{
			name:      "an override naming a repository the forge does not have",
			overrides: []changelog.Override{{Dependency: dependency, Repository: "Sonarr/Missing"}},
			upstream:  "home-operations/containers",
		},
		{
			name:      "an override whose releases have a hole in the range",
			overrides: []changelog.Override{{Dependency: dependency, Repository: "Sonarr/Holed"}},
			upstream:  "home-operations/containers",
		},
		{
			name:       "an override with no release reader configured at all",
			overrides:  []changelog.Override{{Dependency: dependency, Repository: "Sonarr/Sonarr"}},
			upstream:   "home-operations/containers",
			noReleases: true,
		},
		{
			name:      "an override and an unreachable registry",
			overrides: []changelog.Override{{Dependency: dependency, Repository: "Sonarr/Empty"}},
			upstream:  "home-operations/containers",
			images:    fakeImages{err: errors.New("unreachable")},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			images := testCase.images
			if images == nil {
				images = fakeImages{source: testCase.annotation}
			}
			var releases changelog.ReleaseReader
			if !testCase.noReleases {
				releases = &fakeReleases{byRepository: map[string][]github.Release{
					"Sonarr/Sonarr": {release},
					"Sonarr/Empty":  nil,
					"Sonarr/Holed":  {{TagName: "sonarr-4.0.19", Body: "BREAKING"}, {TagName: "v4.0.15"}},
				}}
			}
			update := sonarrUpdate()
			update.ReleaseNotes = notes
			update.Upstream = testCase.upstream

			got, err := changelog.NewResolver(images, releases, testCase.overrides, nil).
				Resolve(context.Background(), update)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got.Text == "" && got.Source != domain.ChangelogOverrideEmpty {
				t.Fatalf("notes were offered and dropped under source %q, "+
					"which run.go does not report as a discard: %+v", got.Source, got)
			}
		})
	}
}

// A bare override carries no forge, so it matches the same owner/name on any
// host. That admits a cross-forge collision — a github-intended override matching
// a gitlab attribution of the same path — but the alternative, defaulting a bare
// override to github, would discard the changelog for every non-github bare
// override, which is the only source those dependencies can ever resolve. The
// collision requires two forges to publish the identical owner/name; the
// regression would hit a whole common class. The bare match is retained by
// design; this pins that decision.
func TestABareOverrideMatchesTheSameOwnerNameOnAnyForgeByDesign(t *testing.T) {
	const notes = "Fixed a crash on import."

	for _, upstream := range []string{
		"github.com/mygroup/project-b",
		"gitlab.com/mygroup/project-b",
		"https://git.example.com/mygroup/project-b",
		"mygroup/project-b",
	} {
		t.Run(upstream, func(t *testing.T) {
			update := sonarrUpdate()
			update.ReleaseNotes = notes
			update.Upstream = upstream

			got, err := changelog.NewResolver(
				fakeImages{err: errors.New("no annotation")},
				&fakeReleases{},
				[]changelog.Override{{Dependency: update.Dependency.Name, Repository: "mygroup/project-b"}},
				nil,
			).Resolve(context.Background(), update)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got.Source != domain.ChangelogFromPullRequest || got.Text != notes {
				t.Fatalf("want the bare override to keep the body, got %+v", got)
			}
		})
	}
}

type recordingImages struct {
	source string
	asked  []string
}

func (r *recordingImages) Resolve(_ context.Context, reference string) (string, error) {
	r.asked = append(r.asked, reference)
	return r.source, nil
}

func TestDigestBumpsAskTheRegistryForAValidReference(t *testing.T) {
	const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	for name, version := range map[string]string{
		"bare digest":  digest,
		"tag a digest": "4.0.19@" + digest,
		"tag":          "4.0.19",
	} {
		t.Run(name, func(t *testing.T) {
			images := &recordingImages{source: "https://github.com/Sonarr/Sonarr"}
			update := sonarrUpdate()
			update.Dependency.ToVersion = version
			releases := &fakeReleases{byRepository: map[string][]github.Release{
				"Sonarr/Sonarr": {{TagName: "v4.0.19", Body: "Import fix"}},
			}}

			if _, err := changelog.NewResolver(images, releases, nil, nil).Resolve(context.Background(), update); err != nil {
				t.Fatalf("resolve: %v", err)
			}

			want := "ghcr.io/home-operations/sonarr:" + version
			if strings.HasPrefix(version, "sha256:") {
				want = "ghcr.io/home-operations/sonarr@" + version
			}
			if len(images.asked) != 1 || images.asked[0] != want {
				t.Fatalf("want %q, got %v", want, images.asked)
			}
		})
	}
}

func TestAPaddedDigestStillAsksTheRegistryForAnUnpaddedReference(t *testing.T) {
	const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	cases := []struct {
		name    string
		version string
		want    string
	}{
		{"leading space", " " + digest, "ghcr.io/home-operations/sonarr@" + digest},
		{"trailing space", digest + " ", "ghcr.io/home-operations/sonarr@" + digest},
		{"leading tab", "\t" + digest, "ghcr.io/home-operations/sonarr@" + digest},
		{"trailing newline", digest + "\n", "ghcr.io/home-operations/sonarr@" + digest},
		{"padded on both sides", " " + digest + " ", "ghcr.io/home-operations/sonarr@" + digest},
		{"padded tag", " 4.0.19 ", "ghcr.io/home-operations/sonarr:4.0.19"},
		{"padded tag pinned to a digest", " 4.0.19@" + digest + " ", "ghcr.io/home-operations/sonarr:4.0.19@" + digest},
		{"whitespace only", "   ", "ghcr.io/home-operations/sonarr"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			images := &recordingImages{source: "https://github.com/Sonarr/Sonarr"}
			update := sonarrUpdate()
			update.Dependency.ToVersion = test.version
			releases := &fakeReleases{byRepository: map[string][]github.Release{
				"Sonarr/Sonarr": {{TagName: "v4.0.19", Body: "Import fix"}},
			}}

			if _, err := changelog.NewResolver(images, releases, nil, nil).Resolve(context.Background(), update); err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if len(images.asked) != 1 || images.asked[0] != test.want {
				t.Fatalf("want %q, got %v", test.want, images.asked)
			}
		})
	}
}

func TestBetweenSelectsOnlyTheReleasesInTheRange(t *testing.T) {
	releases := []github.Release{
		{TagName: "v4.0.20"},
		{TagName: "v4.0.19"},
		{TagName: "v4.0.15"},
		{TagName: "v4.0.14"},
	}
	selected, complete := changelog.Between(releases, "4.0.14", "4.0.19")

	var tags []string
	for _, release := range selected {
		tags = append(tags, release.TagName)
	}
	if strings.Join(tags, ",") != "v4.0.19,v4.0.15" {
		t.Fatalf("want the two releases inside the range, got %v", tags)
	}
	if !complete {
		t.Fatal("want the range reported complete")
	}
}

// A tag Between cannot parse is a release it cannot exclude. One sitting between
// two releases it did select is inside the range by position, so the selection
// has a hole in it and is not the changelog for that range.
func TestBetweenReportsARangeStraddlingATagItCannotParseAsIncomplete(t *testing.T) {
	for _, testCase := range []struct {
		name         string
		releases     []github.Release
		from         string
		to           string
		want         string
		wantComplete bool
	}{
		{
			name:     "an unparseable tag between two selected releases",
			releases: []github.Release{{TagName: "v4.0.19"}, {TagName: "sonarr-4.0.17"}, {TagName: "v4.0.15"}},
			from:     "4.0.14", to: "4.0.19",
			want: "v4.0.19,v4.0.15", wantComplete: false,
		},
		{
			name:     "an unparseable tag newer than everything selected",
			releases: []github.Release{{TagName: "nightly"}, {TagName: "v4.0.19"}, {TagName: "v4.0.15"}},
			from:     "4.0.14", to: "4.0.19",
			want: "v4.0.19,v4.0.15", wantComplete: true,
		},
		{
			name:     "an unparseable tag older than everything selected",
			releases: []github.Release{{TagName: "v4.0.19"}, {TagName: "v4.0.15"}, {TagName: "sonarr-4.0.10"}},
			from:     "4.0.14", to: "4.0.19",
			want: "v4.0.19,v4.0.15", wantComplete: true,
		},
		{
			name:     "a single selected release cannot straddle anything",
			releases: []github.Release{{TagName: "sonarr-4.0.20"}, {TagName: "v4.0.19"}, {TagName: "sonarr-4.0.17"}},
			from:     "4.0.18", to: "4.0.19",
			want: "v4.0.19", wantComplete: true,
		},
		{
			name:     "an unparseable range endpoint is not a hole in the release list",
			releases: []github.Release{{TagName: "2024.11.1"}, {TagName: "2024.10.3"}},
			from:     "unparseable", to: "2024.11.1",
			want: "2024.11.1", wantComplete: true,
		},
		{
			name:     "an unparseable endpoint above the selection is a hole when nothing selected reaches to",
			releases: []github.Release{{TagName: "sonarr-4.0.19"}, {TagName: "v4.0.15"}},
			from:     "4.0.14", to: "4.0.19",
			want: "v4.0.15", wantComplete: false,
		},
		{
			name:     "an unparseable tag above a selection that reaches to stays complete",
			releases: []github.Release{{TagName: "nightly"}, {TagName: "v4.0.19"}, {TagName: "v4.0.15"}},
			from:     "4.0.14", to: "4.0.19",
			want: "v4.0.19,v4.0.15", wantComplete: true,
		},
		{
			name:     "an endpoint that was never published is not a hole absent any unplaceable tag",
			releases: []github.Release{{TagName: "v4.0.15"}},
			from:     "4.0.14", to: "4.0.19",
			want: "v4.0.15", wantComplete: true,
		},
		{
			name:     "a same-version parseable decoy does not mask an unplaceable endpoint at that version",
			releases: []github.Release{{TagName: "sonarr-4.0.19"}, {TagName: "v4.0.19"}, {TagName: "v4.0.15"}},
			from:     "4.0.14", to: "4.0.19",
			want: "v4.0.19,v4.0.15", wantComplete: false,
		},
		{
			name:     "a separatorless project prefix fused to the endpoint version is still a hole behind a same-version decoy",
			releases: []github.Release{{TagName: "sonarr4.0.19"}, {TagName: "v4.0.19"}, {TagName: "v4.0.15"}},
			from:     "4.0.14", to: "4.0.19",
			want: "v4.0.19,v4.0.15", wantComplete: false,
		},
		{
			name:     "a go-style prefix fused to the endpoint version is still a hole behind a same-version decoy",
			releases: []github.Release{{TagName: "go1.21.0"}, {TagName: "v1.21.0"}, {TagName: "v1.20.0"}},
			from:     "1.19.0", to: "1.21.0",
			want: "v1.21.0,v1.20.0", wantComplete: false,
		},
		{
			name:     "a dot-separated project prefix on the endpoint version is still a hole behind a same-version decoy",
			releases: []github.Release{{TagName: "foo.4.0.19"}, {TagName: "v4.0.19"}, {TagName: "v4.0.15"}},
			from:     "4.0.14", to: "4.0.19",
			want: "v4.0.19,v4.0.15", wantComplete: false,
		},
		{
			name:     "a non-ascii project prefix fused to the endpoint version is still a hole behind a same-version decoy",
			releases: []github.Release{{TagName: "сонарр4.0.19"}, {TagName: "v4.0.19"}, {TagName: "v4.0.15"}},
			from:     "4.0.14", to: "4.0.19",
			want: "v4.0.19,v4.0.15", wantComplete: false,
		},
		{
			name:     "a dot inside a version is not a prefix boundary, so a lower-major tag stays out of a same-shaped range",
			releases: []github.Release{{TagName: "foo-4.0.17"}, {TagName: "v0.19"}, {TagName: "v0.15"}},
			from:     "0.14", to: "0.19",
			want: "v0.19,v0.15", wantComplete: true,
		},
		{
			name:     "a dotted rolling tag carrying no in-range version does not suppress a complete selection",
			releases: []github.Release{{TagName: "nightly.2024.11.1"}, {TagName: "v4.0.19"}, {TagName: "v4.0.15"}},
			from:     "4.0.14", to: "4.0.19",
			want: "v4.0.19,v4.0.15", wantComplete: true,
		},
		{
			name:     "a prerelease fused onto the endpoint version is still a hole behind a same-version decoy",
			releases: []github.Release{{TagName: "app4.0.19rc1"}, {TagName: "v4.0.19"}, {TagName: "v4.0.15"}},
			from:     "4.0.14", to: "4.0.19",
			want: "v4.0.19,v4.0.15", wantComplete: false,
		},
		{
			name:     "a bare prerelease fused onto an in-range version is a hole",
			releases: []github.Release{{TagName: "4.0.17rc1"}, {TagName: "v4.0.19"}, {TagName: "v4.0.15"}},
			from:     "4.0.14", to: "4.0.19",
			want: "v4.0.19,v4.0.15", wantComplete: false,
		},
		{
			name:     "a fused prerelease below from is not a hole",
			releases: []github.Release{{TagName: "app4.0.19rc1"}, {TagName: "v4.0.20"}},
			from:     "4.0.19", to: "4.0.20",
			want: "v4.0.20", wantComplete: true,
		},
		{
			name:     "a hex-ish suffix on a bare number is not a fused prerelease and does not suppress a complete selection",
			releases: []github.Release{{TagName: "abc4a3f"}, {TagName: "v4.1.0"}, {TagName: "v4.0.0"}},
			from:     "3.9.0", to: "4.1.0",
			want: "v4.1.0,v4.0.0", wantComplete: true,
		},
		{
			name:     "a hash-ish tag carrying no in-range version does not suppress a complete selection",
			releases: []github.Release{{TagName: "abc123def"}, {TagName: "v4.0.19"}, {TagName: "v4.0.15"}},
			from:     "4.0.14", to: "4.0.19",
			want: "v4.0.19,v4.0.15", wantComplete: true,
		},
		{
			name:     "a bare-major suffix out of range does not suppress a complete selection",
			releases: []github.Release{{TagName: "nginx1"}, {TagName: "v4.0.19"}, {TagName: "v4.0.15"}},
			from:     "4.0.14", to: "4.0.19",
			want: "v4.0.19,v4.0.15", wantComplete: true,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			selected, complete := changelog.Between(testCase.releases, testCase.from, testCase.to)
			var tags []string
			for _, release := range selected {
				tags = append(tags, release.TagName)
			}
			if got := strings.Join(tags, ","); got != testCase.want {
				t.Fatalf("want %q, got %q", testCase.want, got)
			}
			if complete != testCase.wantComplete {
				t.Fatalf("want complete %v, got %v", testCase.wantComplete, complete)
			}
		})
	}
}

// The invariant: a resolved changelog accounts for every release in its range.
// Either the release whose tag could not be placed is in the text, or the result
// is not offered as the changelog for that bump at all.
func TestAResolvedChangelogNeverOmitsAReleaseItCouldNotPlace(t *testing.T) {
	const breaking = "BREAKING: the database schema was rewritten"

	for _, testCase := range []struct {
		name     string
		releases []github.Release
	}{
		{
			name: "a project-prefixed tag inside the range",
			releases: []github.Release{
				{TagName: "v4.0.19", Body: "an import fix"},
				{TagName: "sonarr-4.0.17", Body: breaking},
				{TagName: "v4.0.15", Body: "a rename"},
			},
		},
		{
			name: "the same range with every tag parseable",
			releases: []github.Release{
				{TagName: "v4.0.19", Body: "an import fix"},
				{TagName: "v4.0.17", Body: breaking},
				{TagName: "v4.0.15", Body: "a rename"},
			},
		},
		{
			name: "a project-prefixed endpoint above everything selected",
			releases: []github.Release{
				{TagName: "sonarr-4.0.19", Body: breaking},
				{TagName: "v4.0.15", Body: "a rename"},
			},
		},
		{
			name: "a project-prefixed endpoint masked by a same-version bare decoy",
			releases: []github.Release{
				{TagName: "sonarr-4.0.19", Body: breaking},
				{TagName: "v4.0.19", Body: "an import fix"},
				{TagName: "v4.0.15", Body: "a rename"},
			},
		},
		{
			name: "a separatorless project-prefixed endpoint masked by a same-version bare decoy",
			releases: []github.Release{
				{TagName: "sonarr4.0.19", Body: breaking},
				{TagName: "v4.0.19", Body: "an import fix"},
				{TagName: "v4.0.15", Body: "a rename"},
			},
		},
		{
			name: "a dot-separated project-prefixed endpoint masked by a same-version bare decoy",
			releases: []github.Release{
				{TagName: "foo.4.0.19", Body: breaking},
				{TagName: "v4.0.19", Body: "an import fix"},
				{TagName: "v4.0.15", Body: "a rename"},
			},
		},
		{
			name: "an endpoint carrying a fused prerelease masked by a same-version bare decoy",
			releases: []github.Release{
				{TagName: "app4.0.19rc1", Body: breaking},
				{TagName: "v4.0.19", Body: "an import fix"},
				{TagName: "v4.0.15", Body: "a rename"},
			},
		},
		{
			name: "a non-ascii project-prefixed endpoint masked by a same-version bare decoy",
			releases: []github.Release{
				{TagName: "сонарр4.0.19", Body: breaking},
				{TagName: "v4.0.19", Body: "an import fix"},
				{TagName: "v4.0.15", Body: "a rename"},
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			releases := &fakeReleases{byRepository: map[string][]github.Release{"Sonarr/Sonarr": testCase.releases}}
			got, err := changelog.NewResolver(
				fakeImages{err: errors.New("no annotation")},
				releases,
				[]changelog.Override{{Dependency: "ghcr.io/home-operations/sonarr", Repository: "Sonarr/Sonarr"}},
				nil,
			).Resolve(context.Background(), sonarrUpdate())
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			served := got.Source != domain.ChangelogNotFound && got.Source != domain.ChangelogOverrideEmpty
			if served && !strings.Contains(got.Text, breaking) {
				t.Fatalf("changelog offered as %q omits a release in its range: %q", got.Source, got.Text)
			}
		})
	}
}

// A rolling tag between two selected releases is suppressed (complete=false,
// source=none, the agent then searches) rather than served, and this is a
// deliberate cost, not a defect to relax. A version-less real release carrying a
// breaking body sits in the same position and reads identically from its tag, so
// relaxing the between-check to serve past a "nightly" would silently drop that
// release's notes — trading a safe degradation for the misinformation Between
// exists to prevent.
func TestARollingTagBetweenTwoReleasesIsSuppressedBecauseARealReleaseThereIsIndistinguishable(t *testing.T) {
	for _, tag := range []string{"nightly", "latest", "the-big-rewrite"} {
		t.Run(tag, func(t *testing.T) {
			releases := []github.Release{{TagName: "v4.0.19"}, {TagName: tag}, {TagName: "v4.0.15"}}
			selected, complete := changelog.Between(releases, "4.0.14", "4.0.19")

			var tags []string
			for _, release := range selected {
				tags = append(tags, release.TagName)
			}
			if got := strings.Join(tags, ","); got != "v4.0.19,v4.0.15" {
				t.Fatalf("want the two placeable releases, got %q", got)
			}
			if complete {
				t.Fatal("want the range reported incomplete so the agent searches rather than serving past the gap")
			}
		})
	}
}

func TestBetweenKeepsPrereleasesBelowTheirReleaseAndExcludesNewerOnes(t *testing.T) {
	releases := []github.Release{
		{TagName: "v1.2.4"},
		{TagName: "v1.2.4-rc.1"},
		{TagName: "v1.2.4-beta.1"},
		{TagName: "v1.2.3"},
	}
	selected, _ := changelog.Between(releases, "1.2.3", "1.2.4-beta.1")

	var tags []string
	for _, release := range selected {
		tags = append(tags, release.TagName)
	}
	if strings.Join(tags, ",") != "v1.2.4-beta.1" {
		t.Fatalf("want only the target prerelease, got %v", tags)
	}
}

// An omitted trailing segment is a zero, not a lower version: the tag v1.1.0 and
// the version 1.1 Renovate wrote are the same release. Ordering by segment count
// instead excluded the target as newer than itself.
func TestBetweenTreatsAnOmittedTrailingSegmentAsZeroRatherThanALowerVersion(t *testing.T) {
	releases := []github.Release{{TagName: "v1.1.0"}, {TagName: "v1.0.0"}, {TagName: "v1.0.0-alpha"}}

	for _, testCase := range []struct {
		name string
		from string
		to   string
		want string
	}{
		{name: "the target spells one segment fewer than its tag", from: "1.0", to: "1.1", want: "v1.1.0"},
		{name: "the origin spells one segment fewer than its tag", from: "0.9", to: "1.0", want: "v1.0.0,v1.0.0-alpha"},
		{name: "both ends spell one segment fewer than their tags", from: "0.9", to: "1.1", want: "v1.1.0,v1.0.0,v1.0.0-alpha"},
		{name: "an origin that spells the tag exactly excludes it", from: "1.0.0", to: "1.1", want: "v1.1.0"},
		{name: "a prerelease stays below the release the target spells short", from: "0.9", to: "1.0.0-alpha", want: "v1.0.0-alpha"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var tags []string
			selected, _ := changelog.Between(releases, testCase.from, testCase.to)
			for _, release := range selected {
				tags = append(tags, release.TagName)
			}
			if got := strings.Join(tags, ","); got != testCase.want {
				t.Fatalf("want %q, got %q", testCase.want, got)
			}
		})
	}
}

// The exclusion is not merely a missing changelog: with the target excluded the
// range still holds the release below it, and fromReleases reports that as the
// resolved changelog under the authoritative config_override label.
func TestAnOverrideNeverServesTheReleaseBelowTheTargetAsTheTargetsNotes(t *testing.T) {
	releases := &fakeReleases{byRepository: map[string][]github.Release{
		"Sonarr/Sonarr": {
			{TagName: "v1.1.0", Body: "1.1.0 removed the legacy database"},
			{TagName: "v1.0.0", Body: "1.0.0 fixed an import crash"},
		},
	}}
	update := sonarrUpdate()
	update.Dependency.FromVersion, update.Dependency.ToVersion = "1.0", "1.1"

	got, err := changelog.NewResolver(
		fakeImages{err: errors.New("no annotation")},
		releases,
		[]changelog.Override{{Dependency: update.Dependency.Name, Repository: "Sonarr/Sonarr"}},
		nil,
	).Resolve(context.Background(), update)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Source != domain.ChangelogFromOverride {
		t.Fatalf("want the override to resolve, got %+v", got)
	}
	if !strings.Contains(got.Text, "1.1.0 removed the legacy database") {
		t.Fatalf("target release notes missing: %q", got.Text)
	}
	if strings.Contains(got.Text, "1.0.0 fixed an import crash") {
		t.Fatalf("notes from below the range served as the target's: %q", got.Text)
	}
}

// Renovate writes the target version the manifest carried (4.0.20), but upstream
// tags the release with an extra build segment (v4.0.20.1234). Ordering by number
// puts the tag above to and excludes it; the release below it must never be served
// as the target's notes. A lone unconfirmable extra-segment tag fails safe instead
// of being guessed at as the target.
func TestBetweenRecoversATargetWhoseTagCarriesAnExtraBuildSegment(t *testing.T) {
	for _, testCase := range []struct {
		name         string
		releases     []github.Release
		from         string
		to           string
		want         string
		wantComplete bool
	}{
		{
			name:     "the target tagged with an extra segment above a selected release",
			releases: []github.Release{{TagName: "v4.0.20.1234"}, {TagName: "v4.0.19"}},
			from:     "4.0.18", to: "4.0.20",
			want: "v4.0.20.1234,v4.0.19", wantComplete: true,
		},
		{
			name:     "a lone extra-segment target cannot be confirmed and fails safe",
			releases: []github.Release{{TagName: "v4.0.20.1234"}},
			from:     "4.0.18", to: "4.0.20",
			want: "", wantComplete: true,
		},
		{
			name:     "the only in-range release is the excluded extra-segment target",
			releases: []github.Release{{TagName: "v4.0.20.1234"}, {TagName: "v1.1.0"}, {TagName: "v1.0.0"}},
			from:     "4.0.19", to: "4.0.20",
			want: "", wantComplete: true,
		},
		{
			name:     "two tags extend the target prefix so the endpoint is ambiguous",
			releases: []github.Release{{TagName: "v4.0.20.2"}, {TagName: "v4.0.20.1"}, {TagName: "v4.0.19"}},
			from:     "4.0.18", to: "4.0.20",
			want: "v4.0.19", wantComplete: false,
		},
		{
			name:     "an under-specified target does not recover a higher patch as its endpoint",
			releases: []github.Release{{TagName: "v4.0.5"}, {TagName: "v3.5"}},
			from:     "3.0", to: "4.0",
			want: "v3.5", wantComplete: false,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			selected, complete := changelog.Between(testCase.releases, testCase.from, testCase.to)
			var tags []string
			for _, release := range selected {
				tags = append(tags, release.TagName)
			}
			if got := strings.Join(tags, ","); got != testCase.want {
				t.Fatalf("want %q, got %q", testCase.want, got)
			}
			if complete != testCase.wantComplete {
				t.Fatalf("want complete %v, got %v", testCase.wantComplete, complete)
			}
		})
	}
}

// The extra-segment exclusion reaches the agent through the authoritative
// config_override label: with the target v4.0.20.1234 dropped, the range still
// holds the minor fix below it, and fromReleases would serve that alone as the
// complete changelog for a breaking bump.
func TestAnOverrideNeverServesTheMinorFixBelowAnExtraSegmentTargetAsItsNotes(t *testing.T) {
	releases := &fakeReleases{byRepository: map[string][]github.Release{
		"Sonarr/Sonarr": {
			{TagName: "v4.0.20.1234", Body: "TARGET rewrote the database schema"},
			{TagName: "v4.0.19", Body: "a minor import fix"},
		},
	}}
	update := sonarrUpdate()
	update.Dependency.FromVersion, update.Dependency.ToVersion = "4.0.18", "4.0.20"

	got, err := changelog.NewResolver(
		fakeImages{err: errors.New("no annotation")},
		releases,
		[]changelog.Override{{Dependency: update.Dependency.Name, Repository: "Sonarr/Sonarr"}},
		nil,
	).Resolve(context.Background(), update)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Source != domain.ChangelogFromOverride {
		t.Fatalf("want the override to resolve, got %+v", got)
	}
	if !strings.Contains(got.Text, "TARGET rewrote the database schema") {
		t.Fatalf("target release notes missing: %q", got.Text)
	}
}

func TestBetweenFallsBackToTheExactTargetTag(t *testing.T) {
	releases := []github.Release{{TagName: "2024.11.1"}, {TagName: "2024.10.3"}}
	selected, _ := changelog.Between(releases, "unparseable", "2024.11.1")

	if len(selected) != 1 || selected[0].TagName != "2024.11.1" {
		t.Fatalf("want the exact target tag, got %+v", selected)
	}
}

// An inverted range takes the releases above to out of production, and the
// target's own notes describe none of them. Served as the complete changelog
// they read as the routine patch the version arithmetic already called this,
// and the breaking release being removed is nowhere in the assessment.
func TestADowngradeIsNotServedAsTheChangelogForTheReleasesItRemoves(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		from, to string
		want     domain.ChangelogSource
		wantText string
	}{
		{
			name: "a downgrade across a breaking release",
			from: "4.0.25", to: "4.0.19",
			want: changelog.SourceIncomplete,
		},
		{
			name: "a downgrade of a single release",
			from: "4.0.20", to: "4.0.19",
			want: changelog.SourceIncomplete,
		},
		{
			name: "the same version served again",
			from: "4.0.19", to: "4.0.19",
			want: domain.ChangelogFromAnnotation, wantText: "a minor import fix",
		},
		{
			name: "an endpoint the range cannot place",
			from: "jammy-20240401", to: "4.0.19",
			want: domain.ChangelogFromAnnotation, wantText: "a minor import fix",
		},
		{
			name: "an ordinary forward range",
			from: "4.0.18", to: "4.0.22",
			want: domain.ChangelogFromAnnotation, wantText: "rewrote the database schema",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			releases := &fakeReleases{byRepository: map[string][]github.Release{
				"Sonarr/Sonarr": {
					{TagName: "v4.0.25", Body: "a logging tweak"},
					{TagName: "v4.0.22", Body: "rewrote the database schema"},
					{TagName: "v4.0.19", Body: "a minor import fix"},
				},
			}}
			update := sonarrUpdate()
			update.Dependency.FromVersion = testCase.from
			update.Dependency.ToVersion = testCase.to

			got, err := changelog.NewResolver(
				fakeImages{source: "https://github.com/Sonarr/Sonarr"}, releases, nil, nil,
			).Resolve(context.Background(), update)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got.Source != testCase.want {
				t.Fatalf("source = %v, want %v (text %q)", got.Source, testCase.want, got.Text)
			}
			if testCase.wantText != "" && !strings.Contains(got.Text, testCase.wantText) {
				t.Fatalf("want %q in the notes, got %q", testCase.wantText, got.Text)
			}
		})
	}
}

// A git refname cannot contain a colon, so a reference that carries one can
// never be spelled as a release tag.
func spellableAsATag(reference string) bool {
	return reference != "" && !strings.ContainsAny(reference, ":~^?*[\\ ")
}

// The fallback still stands in for a range whose endpoints cannot be placed, and
// there it serves one release for a change that may span many. Two separate
// facts keep that from merging unattended, and every endpoint pair needs one of
// them: either the parser refuses to classify the bump, so the run holds, or the
// target cannot be spelled as a tag, so the fallback never finds it to begin
// with. The digest-pinned references rest on the second alone.
func TestARangeServedByTheFallbackEitherHoldsTheBumpOrCannotBeSpelledAsATag(t *testing.T) {
	unplaceable := []string{
		"jammy-20240401", "latest", "stable", "bookworm", "release-2024.11.1",
		"REL_16_2", "sonarr-4.0.19", "20240401-alpine", "edge", "3.11-slim",
		"latest@sha256:" + strings.Repeat("ab", 32),
		"latest@sha256:" + strings.Repeat("cd", 32),
		"sha256:" + strings.Repeat("ef", 32),
	}
	declared := []string{"", "major", "minor", "patch", "digest", "rollback", "replacement", "pin"}
	endpoints := append([]string{"4.0.19", "v4.0.19", "2024.11.1"}, unplaceable...)
	served, unspellable := 0, 0
	for _, endpoint := range unplaceable {
		for _, other := range endpoints {
			for _, pair := range [][2]string{{endpoint, other}, {other, endpoint}} {
				from, to := pair[0], pair[1]
				releases := []github.Release{
					{TagName: to, Body: "the target"},
					{TagName: "an-intermediate-nothing-can-place", Body: "BREAKING"},
					{TagName: from, Body: "the base"},
				}
				selected, complete := changelog.Between(releases, from, to)
				if !complete || len(selected) != 1 {
					continue
				}
				if !spellableAsATag(to) {
					unspellable++
					continue
				}
				served++
				for _, update := range declared {
					bump := renovate.BumpClass(from, to, update)
					if bump == domain.BumpMajor || bump == domain.BumpUnknown {
						continue
					}
					t.Errorf("from=%q to=%q update=%q served one release as complete "+
						"while the parser classified the bump as %v, so nothing holds the merge",
						from, to, update, bump)
				}
			}
		}
	}
	if served == 0 || unspellable == 0 {
		t.Fatalf("both legs must be exercised, got served=%d unspellable=%d", served, unspellable)
	}
}

// The parser's rebuild branch reads two differing digests on one tag as a
// routine digest bump, so a digest-pinned endpoint is the one shape it can
// classify while the changelog cannot place it. Nothing in the changelog holds
// that back: what does is that a digest reference carries a colon and a release
// tag cannot, so the fallback never matches one. Drop the colon and the same
// reference is a legal tag the fallback would serve, which is why the parser
// must keep refusing to classify that shape.
func TestADigestReferenceIsKeptFromTheFallbackByTheTagSpellingNotTheParser(t *testing.T) {
	const (
		before = "sha256:" + "abababababababababababababababababababababababababababababababab"
		after  = "sha256:" + "cdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcdcd"
	)
	if bump := renovate.BumpClass("latest@"+before, "latest@"+after, ""); bump != domain.BumpDigest {
		t.Fatalf("the premise moved: a digest rebuild now classifies as %v, so this shape "+
			"is no longer one the parser lets through", bump)
	}
	if spellableAsATag("latest@" + after) {
		t.Fatal("a digest reference became spellable as a release tag, so the fallback can " +
			"now match one and a digest rebuild serves a single release as complete")
	}

	colonless := []string{
		"latest@" + strings.TrimPrefix(before, "sha256:"),
		"latest@" + strings.TrimPrefix(after, "sha256:"),
	}
	if !spellableAsATag(colonless[1]) {
		t.Fatalf("%q must be a legal tag for this test to say anything", colonless[1])
	}
	selected, complete := changelog.Between([]github.Release{
		{TagName: colonless[1], Body: "the target"},
		{TagName: "an-intermediate-nothing-can-place", Body: "BREAKING"},
		{TagName: colonless[0], Body: "the base"},
	}, colonless[0], colonless[1])
	if !complete || len(selected) != 1 {
		t.Fatalf("want the fallback to serve one release as complete, got %d complete=%v",
			len(selected), complete)
	}
	for _, update := range []string{"", "major", "minor", "patch", "digest", "rollback"} {
		bump := renovate.BumpClass(colonless[0], colonless[1], update)
		if bump == domain.BumpMajor || bump == domain.BumpUnknown {
			continue
		}
		t.Errorf("update=%q classified a colonless digest pair as %v, and the fallback "+
			"serves one release for it, so the merge proceeds on notes for a range nobody read",
			update, bump)
	}
}

// A range Between could not account for is not the same answer as an upstream
// with nothing to serve: the first hides release notes ops-pilot did resolve and
// then declined to trust, and the caller may not merge on a verdict formed
// without them.
func TestAnUpstreamSuppressedAsIncompleteIsNotReportedAsNothingFound(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		releases []github.Release
		want     domain.ChangelogSource
	}{
		{
			name: "a rolling tag between two selected releases",
			releases: []github.Release{
				{TagName: "v4.0.19", Body: "an import fix"},
				{TagName: "nightly.4.0.16"},
				{TagName: "v4.0.15", Body: "a rename"},
			},
			want: changelog.SourceIncomplete,
		},
		{
			name: "a fused build suffix between two selected releases",
			releases: []github.Release{
				{TagName: "v4.0.19", Body: "an import fix"},
				{TagName: "4.0.16f2a1c9"},
				{TagName: "v4.0.15", Body: "a rename"},
			},
			want: changelog.SourceIncomplete,
		},
		{
			name: "an upstream with no release in the range",
			releases: []github.Release{
				{TagName: "v3.0.0", Body: "an older line"},
			},
			want: domain.ChangelogNotFound,
		},
		{
			name: "an upstream that accounts for the whole range",
			releases: []github.Release{
				{TagName: "v4.0.19", Body: "an import fix"},
				{TagName: "v4.0.15", Body: "a rename"},
			},
			want: domain.ChangelogFromAnnotation,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			releases := &fakeReleases{byRepository: map[string][]github.Release{"Sonarr/Sonarr": testCase.releases}}
			got, err := changelog.NewResolver(
				fakeImages{source: "https://github.com/Sonarr/Sonarr"},
				releases,
				nil,
				nil,
			).Resolve(context.Background(), sonarrUpdate())
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got.Source != testCase.want {
				t.Fatalf("want source %q, got %q (%+v)", testCase.want, got.Source, got)
			}
			if testCase.want == changelog.SourceIncomplete && got.Text != "" {
				t.Fatalf("a suppressed changelog served text anyway: %q", got.Text)
			}
		})
	}
}

// A lookup that failed carries no evidence at all, so it is not the same answer
// as an upstream that answered and published nothing in the range: the second is
// a positive fact the assessment may be formed on, the first only means ops-pilot
// never read the release notes a breaking change would have been announced in.
func TestAnUpstreamLookupThatFailedIsNotReportedAsNothingFound(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		err       error
		releases  []github.Release
		truncated bool
		want      domain.ChangelogSource
	}{
		{
			name: "a release history longer than the page budget whose range start was paged away",
			releases: []github.Release{
				{TagName: "v4.0.19", Body: "an import fix"},
				{TagName: "v4.0.18", Body: "a rename"},
			},
			truncated: true,
			want:      changelog.SourceUnreadable,
		},
		{
			name: "a transport failure that outlived the retry schedule",
			err:  errors.New("GET /repos/Sonarr/Sonarr/releases: connection reset by peer"),
			want: changelog.SourceUnreadable,
		},
		{
			name: "an annotated repository that is not readable",
			err:  errors.New("GET /repos/Sonarr/Sonarr/releases: 404 Not Found"),
			want: changelog.SourceUnreadable,
		},
		{
			name:     "an upstream that answered with no releases at all",
			releases: []github.Release{},
			want:     domain.ChangelogNotFound,
		},
		{
			name:     "an upstream that answered with no release in the range",
			releases: []github.Release{{TagName: "v3.0.0", Body: "an older line"}},
			want:     domain.ChangelogNotFound,
		},
		{
			name: "an upstream that accounts for the whole range",
			releases: []github.Release{
				{TagName: "v4.0.19", Body: "an import fix"},
				{TagName: "v4.0.15", Body: "a rename"},
			},
			want: domain.ChangelogFromAnnotation,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			releases := &fakeReleases{
				byRepository: map[string][]github.Release{"Sonarr/Sonarr": testCase.releases},
				truncated:    testCase.truncated,
				err:          testCase.err,
			}
			got, err := changelog.NewResolver(
				fakeImages{source: "https://github.com/Sonarr/Sonarr"},
				releases,
				nil,
				nil,
			).Resolve(context.Background(), sonarrUpdate())
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got.Source != testCase.want {
				t.Fatalf("want source %q, got %q (%+v)", testCase.want, got.Source, got)
			}
			if testCase.want == changelog.SourceUnreadable {
				if got.Repository != "Sonarr/Sonarr" {
					t.Fatalf("the operator is not told which upstream could not be read: %+v", got)
				}
				if got.Text != "" {
					t.Fatalf("a failed lookup served text anyway: %q", got.Text)
				}
			}
		})
	}
}

// An annotation resolving to a repository that publishes nothing in the range
// merges, and it has to, whether that repository is the wrong one or simply an
// upstream with nothing to say. The two are the same observation: a base image
// on 3.x beside a dependency on 4.x is indistinguishable from a correct upstream
// that stopped cutting releases at 3.x, and a calendar-versioned builder repo is
// indistinguishable from a correct upstream that tags by date. Every pair below
// shares a release-list signature and needs opposite answers, so any rule that
// holds one of them holds the other, and holding a correct upstream here is
// permanent: the override that would fix it lands on ChangelogOverrideEmpty,
// which holds too. A version-line rule was tried and reverted for exactly this.
func TestAnAnnotationOffThisVersionLineStillMergesBecauseNothingTellsItFromAnIdleUpstream(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		tags     []string
		from, to string
	}{
		{name: "a base image on its own major line", tags: []string{"v3.19.0", "v3.18.4"}},
		{name: "a correct upstream that stopped cutting releases a major ago", tags: []string{"v3.0.0", "v2.9.0"}},
		{name: "a calendar-versioned builder repository", tags: []string{"2024.10.1", "2024.9.2"}},
		{name: "a correct upstream that tags releases by date", tags: []string{"2024-08-01", "2024-07-01"}},
		{name: "a fork publishing its own release line", tags: []string{"v1.2.0", "v1.1.0"}},
		{name: "a correct upstream that adopted releases only at the next major", tags: []string{"v5.0.0", "v5.1.0"}},
		{name: "a correct upstream using monorepo component tags", tags: []string{"sonarr-4.0.13", "sonarr-4.0.10"}},
		{name: "a correct upstream whose tags carry an underscore", tags: []string{"REL_16_4", "REL_16_3"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			releases := make([]github.Release, 0, len(testCase.tags))
			for _, tag := range testCase.tags {
				releases = append(releases, github.Release{TagName: tag, Body: "notes for " + tag})
			}
			update := sonarrUpdate()
			if testCase.from != "" {
				update.Dependency.FromVersion, update.Dependency.ToVersion = testCase.from, testCase.to
			}
			got, err := changelog.NewResolver(
				fakeImages{source: "https://github.com/Sonarr/Sonarr"},
				&fakeReleases{byRepository: map[string][]github.Release{"Sonarr/Sonarr": releases}},
				nil,
				nil,
			).Resolve(context.Background(), update)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got.Source != domain.ChangelogNotFound {
				t.Fatalf("want %q so the agent is sent searching, got %q (%+v)",
					domain.ChangelogNotFound, got.Source, got)
			}
		})
	}
}

// An override that could not be read falls back to notes the pull request
// carries for the same repository, and merges on them. That is the pull request
// body doing the job it does for every dependency with no override at all,
// where it is consulted before any upstream is read and no failed read can
// even arise. Holding here instead would make configuring an override the
// stricter setting - the operator's remedy for the holds C-M74 and C-M75 raise
// would itself raise new ones - and for an upstream this run can never read,
// a forge that is not GitHub, it would hold every bump forever with no
// remedy at all: there the body is the only changelog that can ever resolve.
// The attribution gate is what makes the fallback safe, so notes the override
// contradicts are still discarded and still hold.
func TestAnOverrideThatResolvedNothingServesNotesAttributedToTheSameRepository(t *testing.T) {
	const notes = "BREAKING: the database schema was rewritten; downgrade is impossible."
	for _, testCase := range []struct {
		name       string
		dependency string
		repository string
		truncated  bool
		upstream   string
		notes      string
		want       domain.ChangelogSource
		wantLogged bool
	}{
		{
			name:       "releases that could not be read at all",
			repository: "Sonarr/Missing", upstream: "Sonarr/Missing", notes: notes,
			want: domain.ChangelogFromPullRequest, wantLogged: true,
		},
		{
			// The name is manifest text, so it may carry anything.
			name:       "a dependency whose name spells a log line of its own",
			dependency: "ghcr.io/home-operations/sonarr\n2026-08-15 21:00:00Z WARN  forged",
			repository: "Sonarr/Missing", upstream: "Sonarr/Missing", notes: notes,
			want: domain.ChangelogFromPullRequest, wantLogged: true,
		},
		{
			name:       "a history too long to account for the range",
			repository: "Sonarr/Sonarr", truncated: true, upstream: "Sonarr/Sonarr", notes: notes,
			want: domain.ChangelogFromPullRequest, wantLogged: true,
		},
		{
			name:       "releases with a hole in the range",
			repository: "Sonarr/Holed", upstream: "Sonarr/Holed", notes: notes,
			want: domain.ChangelogFromPullRequest, wantLogged: true,
		},
		{
			name:       "an upstream that published nothing in the range",
			repository: "Sonarr/Empty", upstream: "Sonarr/Empty", notes: notes,
			want: domain.ChangelogFromPullRequest, wantLogged: true,
		},
		{
			name:       "notes attributed to another repository",
			repository: "Sonarr/Missing", upstream: "home-operations/containers", notes: notes,
			want: domain.ChangelogOverrideEmpty,
		},
		{
			name:       "no notes to fall back to",
			repository: "Sonarr/Missing", upstream: "Sonarr/Missing",
			want: domain.ChangelogOverrideEmpty,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var written strings.Builder
			update := sonarrUpdate()
			update.ReleaseNotes = testCase.notes
			update.Upstream = testCase.upstream
			if testCase.dependency != "" {
				update.Dependency.Name = testCase.dependency
			}
			releases := &fakeReleases{
				byRepository: map[string][]github.Release{
					"Sonarr/Sonarr": {{TagName: "v4.0.19", Body: "an import fix"}},
					"Sonarr/Empty":  nil,
					"Sonarr/Holed":  {{TagName: "sonarr-4.0.19", Body: "BREAKING"}, {TagName: "v4.0.15"}},
				},
				truncated: testCase.truncated,
			}

			got, err := changelog.NewResolver(
				fakeImages{},
				releases,
				[]changelog.Override{{Dependency: update.Dependency.Name, Repository: testCase.repository}},
				diagnostics.NewLogger(&written, nil),
			).Resolve(context.Background(), update)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got.Source != testCase.want {
				t.Fatalf("want source %q, got %q (%+v)", testCase.want, got.Source, got)
			}
			if testCase.want == domain.ChangelogFromPullRequest && got.Text != notes {
				t.Fatalf("want the body verbatim, got %q", got.Text)
			}
			logged := strings.Contains(written.String(), "were served instead")
			if logged != testCase.wantLogged {
				t.Fatalf("want the unresolved override logged %v, got log %q", testCase.wantLogged, written.String())
			}
			if testCase.wantLogged {
				if !strings.Contains(written.String(), testCase.repository) {
					t.Fatalf("want the override named in the log, got %q", written.String())
				}
				if strings.Contains(written.String(), notes) {
					t.Fatalf("the body must not be copied into the log, got %q", written.String())
				}
				for _, line := range strings.Split(written.String(), "\n") {
					if strings.HasPrefix(line, "2026-08-15 21:00:00Z") {
						t.Fatalf("the dependency name wrote a log line of its own: %q", written.String())
					}
				}
			}
		})
	}
}

// A registry ops-pilot cannot read hides whether the image declares an upstream
// at all, and that is the whole of what a hold here would be protecting. Every
// pair below shares one observation - the annotation lookup failed - and needs
// opposite answers: the image that would have named a breaking upstream wants a
// hold, the image that declares nothing wants the merge every unannotated image
// already gets, and no rule reading the failure can tell them apart, because
// what separates them is on the far side of it. Holding anyway would stall every
// image behind a registry ops-pilot has no credentials for, on every bump, and
// the population that declares nothing is the larger one. Unlike C-M74 the
// evidence is not known to exist here: there the repository had already been
// read off the image and only its releases were unreachable.
func TestARegistryFailureMergesBecauseNothingTellsAnUnreadAnnotationFromAnAbsentOne(t *testing.T) {
	authDenied := errors.New("OCI registry authorization denied")
	for _, testCase := range []struct {
		name   string
		images fakeImages
	}{
		{
			name:   "a refused registry holding an image that declares a breaking upstream",
			images: fakeImages{source: "https://github.com/Sonarr/Sonarr", err: authDenied},
		},
		{
			name:   "a refused registry holding an image that declares nothing",
			images: fakeImages{err: authDenied},
		},
		{
			name:   "a public registry answering a pull limit",
			images: fakeImages{source: "https://github.com/Sonarr/Sonarr", err: errors.New("registry status 429")},
		},
		{
			// Renovate leaves the Type column off, the dependency reaches the
			// registry as an image reference, and Docker Hub answers 401 for a
			// repository that was never one.
			name:   "a dependency that is not a container image at all",
			images: fakeImages{err: authDenied},
		},
		{
			name:   "an image whose tag the registry has never heard of",
			images: fakeImages{err: errors.New("registry status 404")},
		},
		{
			name:   "an annotation on a forge whose releases this run cannot read",
			images: fakeImages{source: "https://gitlab.com/gitlab-org/gitlab"},
		},
		{
			name:   "an annotation that names no repository",
			images: fakeImages{source: "https://example.invalid/"},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			releases := &fakeReleases{}
			got, err := changelog.NewResolver(
				testCase.images,
				releases,
				nil,
				nil,
			).Resolve(context.Background(), sonarrUpdate())
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got.Source != domain.ChangelogNotFound {
				t.Fatalf("want %q so the agent is sent searching, got %q (%+v)",
					domain.ChangelogNotFound, got.Source, got)
			}
			if len(releases.asked) != 0 {
				t.Fatalf("want no upstream lookup, got %v", releases.asked)
			}
		})
	}
}

// The failure merges, but it may not vanish: an operator whose registry
// credentials expired otherwise sees a fleet quietly lose its changelogs with
// nothing anywhere saying why.
func TestARegistryThatCouldNotBeAskedIsLoggedRatherThanSwallowed(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		images     fakeImages
		kind       string
		wantLogged bool
	}{
		{
			name:       "a registry that refused the request",
			images:     fakeImages{err: errors.New("OCI registry authorization denied")},
			wantLogged: true,
		},
		{
			name:   "an image that carries no source annotation",
			images: fakeImages{},
		},
		{
			name:   "an image that carries one",
			images: fakeImages{source: "https://github.com/Sonarr/Sonarr"},
		},
		{
			name:   "a chart, which is never asked about",
			images: fakeImages{err: errors.New("OCI registry authorization denied")},
			kind:   domain.DependencyKindHelm,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var written strings.Builder
			update := sonarrUpdate()
			if testCase.kind != "" {
				update.Dependency.Kind = testCase.kind
			}
			_, err := changelog.NewResolver(
				testCase.images,
				&fakeReleases{byRepository: map[string][]github.Release{"Sonarr/Sonarr": {{TagName: "v4.0.19"}}}},
				nil,
				diagnostics.NewLogger(&written, nil),
			).Resolve(context.Background(), update)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			logged := strings.Contains(written.String(), "source annotation")
			if logged != testCase.wantLogged {
				t.Fatalf("want the registry failure logged %v, got log %q", testCase.wantLogged, written.String())
			}
			if testCase.wantLogged && !strings.Contains(written.String(), "ghcr.io/home-operations/sonarr") {
				t.Fatalf("want the image named in the log, got %q", written.String())
			}
		})
	}
}

// longHistory is the newest count releases of a repository that publishes far
// more than the page budget covers, newest first and one minor apart.
func longHistory(newest, count int) []github.Release {
	releases := make([]github.Release, 0, count)
	for minor := newest; minor > newest-count; minor-- {
		releases = append(releases, github.Release{
			TagName: fmt.Sprintf("v1.%d.0", minor),
			Body:    fmt.Sprintf("notes for 1.%d.0", minor),
		})
	}
	return releases
}

// longHistoryWithOlderLine is the same newest line with an LTS line maintained
// beside it, which is what a repository with more releases than the page budget
// usually looks like: the backports are published late and versioned low, so
// they sit above the cut while releases inside the range sit below it.
func longHistoryWithOlderLine() []github.Release {
	prefix := longHistory(999, 100)
	for patch := 50; patch >= 1; patch-- {
		prefix = append(prefix, github.Release{
			TagName: fmt.Sprintf("v0.9.%d", patch),
			Body:    "an LTS backport",
		})
	}
	return prefix
}

func longHistoryUpdate(from, to string) renovate.Update {
	update := sonarrUpdate()
	update.Dependency.FromVersion, update.Dependency.ToVersion = from, to
	return update
}

// A history too long to read whole still answers for a range that lies inside
// the releases that were read: the pages never fetched hold releases older than
// the one the range starts at, so none of them belongs to it. Refusing every
// such range lost the changelog of every long-lived dependency on every bump.
// The refusal has to stand wherever the read cannot show that break, because
// there the pages never fetched may hold a breaking release inside the range.
func TestATruncatedHistoryAnswersOnlyForARangeTheReleasesReadAccountFor(t *testing.T) {
	inversion := longHistory(999, 6)
	inversion = append(inversion, github.Release{TagName: "v1.996.5", Body: "a late backport inside the range"})
	monorepo := longHistory(999, 6)
	monorepo = append(monorepo, github.Release{TagName: "sonarr-1.996.5", Body: "a component tag inside the range"})

	for _, testCase := range []struct {
		name     string
		releases []github.Release
		from, to string
		want     domain.ChangelogSource
	}{
		{
			name:     "a range whose start is among the releases read",
			releases: longHistory(999, 100),
			from:     "1.995.0", to: "1.999.0",
			want: domain.ChangelogFromAnnotation,
		},
		{
			name:     "a range whose start was paged away",
			releases: longHistory(999, 100),
			from:     "1.100.0", to: "1.999.0",
			want: changelog.SourceUnreadable,
		},
		{
			name:     "a release inside the range published below the range start",
			releases: inversion,
			from:     "1.995.0", to: "1.999.0",
			want: changelog.SourceUnreadable,
		},
		{
			name:     "a component tag inside the range published below the range start",
			releases: monorepo,
			from:     "1.995.0", to: "1.999.0",
			want: changelog.SourceUnreadable,
		},
		{
			name:     "a range start no release can be compared against",
			releases: longHistory(999, 100),
			from:     "stable", to: "1.999.0",
			want: changelog.SourceUnreadable,
		},
		{
			name:     "an older line maintained beside the newest, range start paged away",
			releases: longHistoryWithOlderLine(),
			from:     "1.100.0", to: "1.999.0",
			want: changelog.SourceUnreadable,
		},
		{
			name:     "an older line maintained beside the newest, range start read",
			releases: longHistoryWithOlderLine(),
			from:     "1.900.0", to: "1.999.0",
			want: domain.ChangelogFromAnnotation,
		},
		{
			// The read accounts for the range and the range holds nothing, which
			// is a fact about the upstream rather than a gap in the read: the
			// answer has to be the one a whole history publishing nothing in
			// range gives, or the cut invents a hold the evidence does not carry.
			name:     "a range the read accounts for and the upstream published nothing in",
			releases: []github.Release{{TagName: "v1.2.3"}, {TagName: "v1.1.0"}},
			from:     "1.2.3", to: "1.5.0",
			want: domain.ChangelogNotFound,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			releases := &fakeReleases{
				byRepository: map[string][]github.Release{"Sonarr/Sonarr": testCase.releases},
				truncated:    true,
			}
			got, err := changelog.NewResolver(
				fakeImages{source: "https://github.com/Sonarr/Sonarr"},
				releases,
				nil,
				nil,
			).Resolve(context.Background(), longHistoryUpdate(testCase.from, testCase.to))
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got.Source != testCase.want {
				t.Fatalf("want source %q, got %q (%+v)", testCase.want, got.Source, got)
			}
			if testCase.want != domain.ChangelogFromAnnotation {
				if got.Text != "" {
					t.Fatalf("a range the read cannot account for served text anyway: %q", got.Text)
				}
				return
			}
			for _, want := range []string{"1.999.0", "1.996.0"} {
				if !strings.Contains(got.Text, want) {
					t.Fatalf("the changelog is missing the release notes of %s:\n%s", want, got.Text)
				}
			}
			if strings.Contains(got.Text, "notes for "+testCase.from) {
				t.Fatalf("the changelog carries the release the range starts at:\n%s", got.Text)
			}
		})
	}
}

// A read the resolver accepts outranks the pull request's own notes, because on
// the override path it is consulted first. So an accepted read that is not
// actually the changelog for the range does not merely lose evidence, it
// replaces evidence that was complete: Renovate's body covers the whole range
// from its own sources and would otherwise have been served.
func TestATruncatedReadNeverDisplacesTheBodyNotesUnlessItAccountsForTheRange(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		from     string
		want     domain.ChangelogSource
		wantText string
	}{
		{
			name:     "a range the read cannot account for",
			from:     "1.100.0",
			want:     domain.ChangelogFromPullRequest,
			wantText: "Renovate's notes for the whole range, including the BREAKING 1.500.0",
		},
		{
			name:     "a range the read accounts for",
			from:     "1.900.0",
			want:     domain.ChangelogFromOverride,
			wantText: "notes for 1.999.0",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			update := longHistoryUpdate(testCase.from, "1.999.0")
			update.Upstream = "Sonarr/Sonarr"
			update.ReleaseNotes = "Renovate's notes for the whole range, including the BREAKING 1.500.0"

			got, err := changelog.NewResolver(
				fakeImages{},
				&fakeReleases{
					byRepository: map[string][]github.Release{"Sonarr/Sonarr": longHistoryWithOlderLine()},
					truncated:    true,
				},
				[]changelog.Override{{Dependency: update.Dependency.Name, Repository: "Sonarr/Sonarr"}},
				nil,
			).Resolve(context.Background(), update)
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got.Source != testCase.want {
				t.Fatalf("want source %q, got %q (%+v)", testCase.want, got.Source, got)
			}
			if !strings.Contains(got.Text, testCase.wantText) {
				t.Fatalf("want the changelog to carry %q, got:\n%s", testCase.wantText, got.Text)
			}
		})
	}
}

// The coverage rule only applies to a read that was cut short. A history read
// whole accounts for its range by construction, and deciding it on the same
// rule would suppress the changelog of every dependency whose upstream simply
// publishes nothing below the range.
func TestAWholeHistoryIsNotJudgedByTheTruncationCoverageRule(t *testing.T) {
	got, err := changelog.NewResolver(
		fakeImages{source: "https://github.com/Sonarr/Sonarr"},
		&fakeReleases{byRepository: map[string][]github.Release{
			"Sonarr/Sonarr": {{TagName: "v1.999.0", Body: "an import fix"}},
		}},
		nil,
		nil,
	).Resolve(context.Background(), longHistoryUpdate("1.995.0", "1.999.0"))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Source != domain.ChangelogFromAnnotation {
		t.Fatalf("want the changelog of a history read whole, got %q (%+v)", got.Source, got)
	}
}

// With an override configured a refused read is not a search, it is a hold on
// every bump of that dependency forever: the override is already the operator's
// answer, so nothing the run can do clears it. A range the releases read account
// for therefore has to resolve here.
func TestAnOverriddenDependencyWithATruncatedHistoryIsNotHeldOnEveryBumpForever(t *testing.T) {
	got, err := changelog.NewResolver(
		fakeImages{},
		&fakeReleases{
			byRepository: map[string][]github.Release{"Sonarr/Sonarr": longHistory(999, 100)},
			truncated:    true,
		},
		[]changelog.Override{{Dependency: "ghcr.io/home-operations/sonarr", Repository: "Sonarr/Sonarr"}},
		nil,
	).Resolve(context.Background(), longHistoryUpdate("1.995.0", "1.999.0"))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Source != domain.ChangelogFromOverride {
		t.Fatalf("want the override's changelog, got %q (%+v)", got.Source, got)
	}
	if !strings.Contains(got.Text, "1.999.0") {
		t.Fatalf("the changelog is missing the target release:\n%s", got.Text)
	}
}
