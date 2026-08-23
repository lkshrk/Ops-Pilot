package renovate_test

import (
	"strings"
	"testing"

	"github.com/lkshrk/ops-pilot/internal/adapters/renovate"
	"github.com/lkshrk/ops-pilot/internal/domain"
)

const imageBody = "This PR contains the following updates:\n\n" +
	"| Package | Type | Update | Change |\n" +
	"|---|---|---|---|\n" +
	"| [ghcr.io/home-operations/sonarr](https://ghcr.io/home-operations/sonarr) | image | minor | `4.0.14.2939` -> `4.0.19.2995` |\n" +
	"\n---\n\n### Release Notes\n\n" +
	"<details>\n<summary>Sonarr/Sonarr (ghcr.io/home-operations/sonarr)</summary>\n\n" +
	"### [`v4.0.19`](https://github.com/Sonarr/Sonarr/releases/tag/v4.0.19)\n\n" +
	"Fixed a crash on import.\n\n</details>\n\n---\n"

func TestSingleReadsImageBumpFromTheTable(t *testing.T) {
	update, err := renovate.Single(imageBody)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := domain.Dependency{
		Name:        "ghcr.io/home-operations/sonarr",
		Kind:        domain.DependencyKindDocker,
		FromVersion: "4.0.14.2939",
		ToVersion:   "4.0.19.2995",
		Bump:        domain.BumpMinor,
	}
	if update.Dependency != want {
		t.Fatalf("dependency:\n got %+v\nwant %+v", update.Dependency, want)
	}
	if update.Upstream != "Sonarr/Sonarr" {
		t.Fatalf("want the upstream repository from the notes summary, got %q", update.Upstream)
	}
	if !strings.Contains(update.ReleaseNotes, "Fixed a crash on import") {
		t.Fatalf("release notes did not carry the body: %q", update.ReleaseNotes)
	}
}

func TestSingleReadsHelmChartBump(t *testing.T) {
	body := "| Package | Type | Update | Change |\n" +
		"|---|---|---|---|\n" +
		"| [app-template](https://bjw-s.github.io) | HelmChart | major | `3.7.3` -> `4.0.1` |\n"

	update, err := renovate.Single(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if update.Dependency.Kind != domain.DependencyKindHelm {
		t.Fatalf("want a helm dependency, got %q", update.Dependency.Kind)
	}
	if update.Dependency.Bump != domain.BumpMajor {
		t.Fatalf("want a major bump, got %q", update.Dependency.Bump)
	}
}

func TestParseToleratesExtraRenovateColumns(t *testing.T) {
	body := "| Package | Type | Update | Age | Adoption | Passing | Confidence | Change |\n" +
		"|---|---|---|---|---|---|---|---|\n" +
		"| [gatus](https://x) | image | patch | ![age] | ![adoption] | ![passing] | ![confidence] | `0.3.6` -> `0.3.7` |\n"

	update, err := renovate.Single(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if update.Dependency.FromVersion != "0.3.6" || update.Dependency.ToVersion != "0.3.7" {
		t.Fatalf("version columns misread: %+v", update.Dependency)
	}
}

func TestSingleRefusesAMultiDependencyPullRequest(t *testing.T) {
	body := "| Package | Type | Update | Change |\n" +
		"|---|---|---|---|\n" +
		"| [a](https://x) | image | patch | `1.0.0` -> `1.0.1` |\n" +
		"| [b](https://y) | image | patch | `2.0.0` -> `2.0.1` |\n"

	if _, err := renovate.Single(body); err == nil {
		t.Fatal("want a refusal for a pull request updating two dependencies")
	}
}

func TestParseReportsABodyWithoutATable(t *testing.T) {
	if _, err := renovate.Parse("Just a description with no table."); err == nil {
		t.Fatal("want an error when the body declares no updates")
	}
}

func TestBumpClassPrefersRenovatesOwnClassificationWhenItIsMoreAlarming(t *testing.T) {
	// The versions alone read as a patch; Renovate declared minor for how the
	// repository pinned the range, and the more alarming class wins.
	if got := renovate.BumpClass("1.2.3", "1.2.4", "minor"); got != domain.BumpMinor {
		t.Fatalf("want the declared class to win, got %q", got)
	}
}

func TestADeclaredClassCannotUnderstateTheVersionArithmetic(t *testing.T) {
	cases := []struct {
		from, to, declared string
		want               domain.BumpClass
	}{
		{"1.5.0", "3.0.0", "patch", domain.BumpMajor},
		{"1.5.0", "3.0.0", "minor", domain.BumpMajor},
		{"1.5.0", "3.0.0", "digest", domain.BumpMajor},
		{"1.0.0", "1.5.0", "patch", domain.BumpMinor},
		{"1.0.0", "1.5.0", "digest", domain.BumpMinor},
		{"1.2.3", "1.2.4", "patch", domain.BumpPatch},
		{"1.2.0", "1.3.0", "minor", domain.BumpMinor},
		{"1.2.3@" + digestA, "1.2.3@" + digestB, "digest", domain.BumpDigest},
	}
	for _, test := range cases {
		if got := renovate.BumpClass(test.from, test.to, test.declared); got != test.want {
			t.Errorf("BumpClass(%q, %q, %q) = %q, want %q", test.from, test.to, test.declared, got, test.want)
		}
	}
}

func TestAVersionPairTheArithmeticCannotReadIsNeverAutoMergeable(t *testing.T) {
	cases := []struct {
		from, to, declared string
		want               domain.BumpClass
	}{
		{"garbage", "1.5.0", "patch", domain.BumpUnknown},
		{"", "", "patch", domain.BumpUnknown},
		{"latest", "edge", "patch", domain.BumpUnknown},
		{"latest", "edge", "minor", domain.BumpUnknown},
		{"latest", "edge", "digest", domain.BumpUnknown},
		{"RELEASE.2025-04-22T22-12-26Z", "RELEASE.2025-05-01T00-00-00Z", "minor", domain.BumpUnknown},
		{"1.2.3-alpha", "1.2.3-beta", "patch", domain.BumpUnknown},
		{"1.2.3", "1.2.3", "digest", domain.BumpUnknown},
		// A declared major is already the most alarming answer, and an unreadable
		// pair may not talk it down either.
		{"garbage", "1.5.0", "major", domain.BumpMajor},
		// A pair the arithmetic does read keeps classifying as it always did.
		{"1.2.3", "1.2.4", "patch", domain.BumpPatch},
		{"1.0.0", "1.5.0", "patch", domain.BumpMinor},
		{"1.2.3@" + digestA, "1.2.3@" + digestB, "digest", domain.BumpDigest},
		{digestA, digestB, "digest", domain.BumpDigest},
	}
	for _, test := range cases {
		if got := renovate.BumpClass(test.from, test.to, test.declared); got != test.want {
			t.Errorf("BumpClass(%q, %q, %q) = %q, want %q", test.from, test.to, test.declared, got, test.want)
		}
	}
}

func TestAnUpdateTypeRenovateDidNotDeclareIsNotGuessedFromTheVersions(t *testing.T) {
	cases := []struct {
		from, to, declared string
		want               domain.BumpClass
	}{
		{"1.2.3", "1.2.4", "replacement", domain.BumpUnknown},
		{"1.3.0", "1.2.9", "rollback", domain.BumpUnknown},
		{"1.2.3", "1.3.0", "lockFileMaintenance", domain.BumpUnknown},
		{"1.2.3", "1.2.4", "pin", domain.BumpUnknown},
		{"1.2.3", "1.2.4", "bump", domain.BumpUnknown},
		// An unrecognised type never reads as less alarming than the versions
		// alone, or the fix would trade a merge that asks for one that does not.
		{"1.2.3", "2.0.0", "replacement", domain.BumpMajor},
		// Renovate's own classification, and a body with no Update column at all.
		{"1.0.0", "1.5.0", "minor", domain.BumpMinor},
		{"1.0.0", "2.0.0", "MAJOR", domain.BumpMajor},
		{"1.2.3", "1.2.4", "", domain.BumpPatch},
		{"1.2.3", "2.0.0", "  ", domain.BumpMajor},
	}
	for _, test := range cases {
		if got := renovate.BumpClass(test.from, test.to, test.declared); got != test.want {
			t.Errorf("BumpClass(%q, %q, %q) = %q, want %q", test.from, test.to, test.declared, got, test.want)
		}
	}
}

func TestSingleLeavesTheRenovateConfigurationBlockUnattributed(t *testing.T) {
	body := "This PR contains the following updates:\n\n" +
		"| Package | Type | Update | Change |\n" +
		"|---|---|---|---|\n" +
		"| [ghcr.io/home-operations/radarr](https://ghcr.io/home-operations/radarr) | image | patch | `5.14.0` -> `5.14.1` |\n" +
		"\n---\n\n### Configuration\n\n" +
		"<details>\n<summary>Configuration</summary>\n\n" +
		"📅 **Schedule**: Branch creation - every weekend.\n\n</details>\n\n---\n"

	update, err := renovate.Single(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if update.ReleaseNotes != "" {
		t.Fatalf("want no changelog when the body carries only Renovate's configuration block, got %q", update.ReleaseNotes)
	}
	if update.Upstream != "" {
		t.Fatalf("want no upstream attribution, got %q", update.Upstream)
	}
}

// Renovate labels a release-notes block "owner/repo (depName)". Two charts of
// one project share a name prefix, and the sibling's block is written first.
const siblingBody = "This PR contains the following updates:\n\n" +
	"| Package | Type | Update | Change |\n" +
	"|---|---|---|---|\n" +
	"| [cert-manager](https://charts.jetstack.io) | HelmChart | patch | `1.16.1` -> `1.16.2` |\n" +
	"\n---\n\n### Release Notes\n\n" +
	"<details>\n<summary>cert-manager/cert-manager-webhook-dns (cert-manager-webhook-dns)</summary>\n\n" +
	"The webhook now rejects unsigned requests.\n\n</details>\n\n" +
	"<details>\n<summary>cert-manager/cert-manager (cert-manager)</summary>\n\n" +
	"Fixed a leaked goroutine.\n\n</details>\n\n---\n"

func TestADependencyDoesNotClaimASiblingProjectsReleaseNotes(t *testing.T) {
	update, err := renovate.Single(siblingBody)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !strings.Contains(update.ReleaseNotes, "leaked goroutine") {
		t.Fatalf("want cert-manager's own notes, got %q", update.ReleaseNotes)
	}
	if update.Upstream != "cert-manager/cert-manager" {
		t.Fatalf("upstream: %q", update.Upstream)
	}
}

func TestTwoDependenciesNeverShareOneReleaseNotesBlock(t *testing.T) {
	body := "This PR contains the following updates:\n\n" +
		"| Package | Type | Update | Change |\n" +
		"|---|---|---|---|\n" +
		"| [cert-manager](https://charts.jetstack.io) | HelmChart | patch | `1.16.1` -> `1.16.2` |\n" +
		"| [cert-manager-webhook-dns](https://charts.jetstack.io) | HelmChart | patch | `0.4.0` -> `0.4.1` |\n" +
		"\n---\n\n### Release Notes\n\n" +
		"<details>\n<summary>cert-manager/cert-manager-webhook-dns (cert-manager-webhook-dns)</summary>\n\n" +
		"The webhook now rejects unsigned requests.\n\n</details>\n\n" +
		"<details>\n<summary>cert-manager/cert-manager (cert-manager)</summary>\n\n" +
		"Fixed a leaked goroutine.\n\n</details>\n\n---\n"

	updates, err := renovate.Parse(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(updates) != 2 {
		t.Fatalf("want two updates, got %d", len(updates))
	}
	want := map[string]string{
		"cert-manager":             "leaked goroutine",
		"cert-manager-webhook-dns": "unsigned requests",
	}
	for _, update := range updates {
		if !strings.Contains(update.ReleaseNotes, want[update.Dependency.Name]) {
			t.Errorf("%s bound to %q", update.Dependency.Name, update.ReleaseNotes)
		}
	}
}

func TestAnExactRepositoryOutranksALastSegmentMatchInEitherRowOrder(t *testing.T) {
	row := func(name string) string {
		return "| [" + name + "](https://x) | patch | `1.0.0` -> `1.0.1` |\n"
	}
	orders := []struct {
		name string
		rows string
	}{
		{"colliding row first", row("myorg/redis") + row("bitnami/redis")},
		{"named row first", row("bitnami/redis") + row("myorg/redis")},
	}
	summaries := []string{"bitnami/redis", "⬆ bitnami/redis", "- bitnami/redis"}
	for _, order := range orders {
		for _, summary := range summaries {
			body := "| Package | Update | Change |\n|---|---|---|\n" + order.rows +
				"\n<details>\n<summary>" + summary + "</summary>\n\n" +
				"BREAKING: the persistence layout changed.\n\n</details>\n"
			updates, err := renovate.Parse(body)
			if err != nil {
				t.Fatalf("%s / %q: parse: %v", order.name, summary, err)
			}
			if len(updates) != 2 {
				t.Fatalf("%s / %q: want two updates, got %d", order.name, summary, len(updates))
			}
			notes := map[string]string{}
			upstreams := map[string]string{}
			for _, update := range updates {
				notes[update.Dependency.Name] = update.ReleaseNotes
				upstreams[update.Dependency.Name] = update.Upstream
			}
			if !strings.Contains(notes["bitnami/redis"], "persistence layout") {
				t.Errorf("%s / %q: bitnami/redis did not claim its own block, got %q", order.name, summary, notes["bitnami/redis"])
			}
			if upstreams["bitnami/redis"] != "bitnami/redis" {
				t.Errorf("%s / %q: bitnami/redis upstream %q", order.name, summary, upstreams["bitnami/redis"])
			}
			if notes["myorg/redis"] != "" {
				t.Errorf("%s / %q: myorg/redis took bitnami's notes: %q", order.name, summary, notes["myorg/redis"])
			}
			if upstreams["myorg/redis"] != "" {
				t.Errorf("%s / %q: myorg/redis attributed to %q", order.name, summary, upstreams["myorg/redis"])
			}
		}
	}
}

func TestASoleBlockNamingADifferentOwnerDoesNotBindOnLastSegmentAlone(t *testing.T) {
	cases := []struct {
		name    string
		row     string
		summary string
		want    bool
	}{
		{"cross-owner collision refused", "bitnami/redis", "myorg/redis", false},
		{"repackaged image still binds", "ghcr.io/home-operations/sonarr", "Sonarr/Sonarr", true},
		{"exact repository still binds", "bitnami/redis", "bitnami/redis", true},
	}
	for _, test := range cases {
		body := "| Package | Update | Change |\n|---|---|---|\n" +
			"| [" + test.row + "](https://x) | patch | `1.0.0` -> `1.0.1` |\n" +
			"\n<details>\n<summary>" + test.summary + "</summary>\n\nnotes.\n\n</details>\n"
		update, err := renovate.Single(body)
		if err != nil {
			t.Fatalf("%s: parse: %v", test.name, err)
		}
		if bound := update.ReleaseNotes != ""; bound != test.want {
			t.Errorf("%s: row %q against %q bound=%v, want %v", test.name, test.row, test.summary, bound, test.want)
		}
		if !test.want && update.Upstream != "" {
			t.Errorf("%s: row %q attributed to %q", test.name, test.row, update.Upstream)
		}
	}
}

func TestASummaryWithoutAParenthesisedNameStillBindsItsRepository(t *testing.T) {
	cases := []struct {
		name    string
		summary string
		want    bool
	}{
		{"app-template", "bjw-s-labs/helm-charts (app-template)", true},
		{"ghcr.io/home-operations/sonarr", "Sonarr/Sonarr (ghcr.io/home-operations/sonarr)", true},
		{"ghcr.io/home-operations/sonarr", "Sonarr/Sonarr", true},
		{"Sonarr/Sonarr", "Sonarr/Sonarr", true},
		{"cert-manager", "cert-manager/cert-manager-webhook-dns", false},
		{"cert-manager", "cert-manager/cert-manager-webhook-dns (cert-manager-webhook-dns)", false},
		{"radarr", "Configuration", false},
	}
	for _, test := range cases {
		body := "| Package | Update | Change |\n|---|---|---|\n" +
			"| [" + test.name + "](https://x) | patch | `1.0.0` -> `1.0.1` |\n" +
			"\n<details>\n<summary>" + test.summary + "</summary>\n\nnotes.\n\n</details>\n"
		update, err := renovate.Single(body)
		if err != nil {
			t.Fatalf("%s / %s: parse: %v", test.name, test.summary, err)
		}
		if bound := update.ReleaseNotes != ""; bound != test.want {
			t.Errorf("%s against %q: bound=%v, want %v", test.name, test.summary, bound, test.want)
		}
	}
}

func TestUpstreamAttributionSurvivesDecorationAndIsNeverTruncated(t *testing.T) {
	cases := []struct {
		summary string
		want    string
	}{
		{"Sonarr/Sonarr (ghcr.io/home-operations/sonarr)", "Sonarr/Sonarr"},
		{"⬆ Sonarr/Sonarr (ghcr.io/home-operations/sonarr)", "Sonarr/Sonarr"},
		{"- Sonarr/Sonarr (ghcr.io/home-operations/sonarr)", "Sonarr/Sonarr"},
		{"github.com/Sonarr/Sonarr (ghcr.io/home-operations/sonarr)", "github.com/Sonarr/Sonarr"},
		{"gitlab.com/group/project (ghcr.io/home-operations/sonarr)", "gitlab.com/group/project"},
		// The changelog side proves a collision unreachable from Upstream never
		// carrying an "@"; a value it cannot read is safer than one that lies.
		{"git@github.com:Sonarr/Sonarr (ghcr.io/home-operations/sonarr)", ""},
		{"Configuration", ""},
	}
	for _, test := range cases {
		body := "| Package | Update | Change |\n|---|---|---|\n" +
			"| [ghcr.io/home-operations/sonarr](https://x) | patch | `1.0.0` -> `1.0.1` |\n" +
			"\n<details>\n<summary>" + test.summary + "</summary>\n\nnotes.\n\n</details>\n"
		update, err := renovate.Single(body)
		if err != nil {
			t.Fatalf("%q: parse: %v", test.summary, err)
		}
		if update.Upstream != test.want {
			t.Errorf("%q: upstream %q, want %q", test.summary, update.Upstream, test.want)
		}
		if strings.Contains(update.Upstream, "@") {
			t.Errorf("%q: upstream carries an @: %q", test.summary, update.Upstream)
		}
	}
}

func TestNewerOrdersPrereleasesAndRefusesToRankDigests(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.2.3-rc.2", "1.2.3-rc.1", 1},
		{"1.2.3-rc.1", "1.2.3-rc.2", -1},
		{"1.2.3-rc.1", "1.2.3-rc.1", 0},
		{"1.2.3", "1.2.3-rc.1", 1},
		{"1.2.3-rc.1", "1.2.3", -1},
		{"1.2.3-rc.1", "1.2.3-rc", 1},
		{"1.2.3-beta", "1.2.3-alpha", 1},
		{"1.2.4-rc.1", "1.2.3", 1},
		{"1.2.3@sha256:aaa", "1.2.3@sha256:bbb", 0},
		{"1.2.3", "1.2.3@sha256:bbb", 0},
		{"1.2.3@sha256:aaa", "1.2.3@sha256:aaa", 0},
		{"1.3.0@sha256:aaa", "1.2.3@sha256:bbb", 1},
		{"1.2.3-rc.1@sha256:aaa", "1.2.3@sha256:bbb", -1},
	}
	for _, test := range cases {
		if got := renovate.Newer(test.a, test.b); got != test.want {
			t.Errorf("Newer(%q, %q) = %d, want %d", test.a, test.b, got, test.want)
		}
	}
}

func TestCompareVersionsClassifiesFourSegmentVersions(t *testing.T) {
	cases := []struct {
		from, to string
		want     domain.BumpClass
	}{
		{"4.0.14.2939", "4.0.19.2995", domain.BumpPatch},
		{"4.0.14.2939", "4.0.14.2996", domain.BumpPatch},
		{"4.0.14.2939", "4.1.0.2996", domain.BumpMinor},
		{"v1.2.3", "v2.0.0", domain.BumpMajor},
		{"1.2.3", "1.2.3", domain.BumpUnknown},
		{"1.2.3@" + digestA, "1.2.3@" + digestB, domain.BumpDigest},
		{"latest", "edge", domain.BumpUnknown},
	}
	for _, test := range cases {
		if got := renovate.CompareVersions(test.from, test.to); got != test.want {
			t.Errorf("%s -> %s: got %q, want %q", test.from, test.to, got, test.want)
		}
	}
}

const (
	digestA = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	digestB = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

func TestAPrereleaseChangeIsNotADigestOnlyRebuild(t *testing.T) {
	cases := []struct {
		from, to string
		want     domain.BumpClass
	}{
		{"1.2.3-alpha@" + digestA, "1.2.3-beta@" + digestB, domain.BumpUnknown},
		{"1.2.3-alpha", "1.2.3-beta", domain.BumpUnknown},
		{"1.2.3-rc.1@" + digestA, "1.2.3@" + digestB, domain.BumpUnknown},
		{"1.2.3-rc.1@" + digestA, "1.2.3-rc.1@" + digestB, domain.BumpDigest},
		{"1.2.3@" + digestA, "1.2.3@" + digestB, domain.BumpDigest},
	}
	for _, test := range cases {
		if got := renovate.CompareVersions(test.from, test.to); got != test.want {
			t.Errorf("%s -> %s: got %q, want %q", test.from, test.to, got, test.want)
		}
	}
}

func TestABareDigestReferenceIsADigestChange(t *testing.T) {
	cases := []struct {
		from, to string
		want     domain.BumpClass
	}{
		{digestA, digestB, domain.BumpDigest},
		{digestA, digestA, domain.BumpUnknown},
		{"latest", digestB, domain.BumpUnknown},
		{"latest", "edge", domain.BumpUnknown},
		{"1.2.3", "1.2.4", domain.BumpPatch},
	}
	for _, test := range cases {
		if got := renovate.CompareVersions(test.from, test.to); got != test.want {
			t.Errorf("%s -> %s: got %q, want %q", test.from, test.to, got, test.want)
		}
	}
}

func TestAVersionWrittenWithFewerSegmentsIsClassifiedByThePositionThatMoved(t *testing.T) {
	cases := []struct {
		from, to string
		want     domain.BumpClass
	}{
		{"1", "1.5.0", domain.BumpMinor},
		{"1", "2.0.0", domain.BumpMajor},
		{"1.2", "1.3", domain.BumpMinor},
		{"1.2", "1.2.4", domain.BumpPatch},
		{"1.2.3", "1.2.3.4", domain.BumpPatch},
		{"1", "1.0.5", domain.BumpPatch},
		// Omitted trailing segments are zeroes, so nothing about the version moved.
		{"1.2", "1.2.0", domain.BumpPatch},
		{"1", "1.0.0", domain.BumpPatch},
	}
	for _, test := range cases {
		if got := renovate.CompareVersions(test.from, test.to); got != test.want {
			t.Errorf("CompareVersions(%q, %q) = %q, want %q", test.from, test.to, got, test.want)
		}
	}
}

func TestADeclaredPatchCannotUnderstateAMinorWrittenWithFewerSegments(t *testing.T) {
	if got := renovate.BumpClass("1", "1.5.0", "patch"); got != domain.BumpMinor {
		t.Fatalf("BumpClass(%q, %q, %q) = %q, want %q", "1", "1.5.0", "patch", got, domain.BumpMinor)
	}
}

func TestAPinnedDigestDoesNotTurnAnUnreadableTagChangeIntoADigestBump(t *testing.T) {
	cases := []struct {
		name     string
		from, to string
		want     domain.BumpClass
	}{
		{"calver tag repin", "RELEASE.2025-04-22T22-12-26Z@" + digestA, "RELEASE.2025-05-01T00-00-00Z@" + digestB, domain.BumpUnknown},
		{"floating tag repin", "latest@" + digestA, "edge@" + digestB, domain.BumpUnknown},
		{"tag replaced by a bare digest", "latest@" + digestA, digestB, domain.BumpUnknown},
		{"same tag rebuilt", "latest@" + digestA, "latest@" + digestB, domain.BumpDigest},
		{"same calver tag rebuilt", "RELEASE.2025-04-22T22-12-26Z@" + digestA, "RELEASE.2025-04-22T22-12-26Z@" + digestB, domain.BumpDigest},
		{"bare digest rebuilt", digestA, digestB, domain.BumpDigest},
	}
	for _, test := range cases {
		if got := renovate.CompareVersions(test.from, test.to); got != test.want {
			t.Errorf("%s: CompareVersions(%q, %q) = %q, want %q", test.name, test.from, test.to, got, test.want)
		}
	}
}

func TestSomethingOtherThanADigestAfterTheAtSignIsNotADigestRebuild(t *testing.T) {
	shortHex := strings.Repeat("a", 31)
	minimalHex, otherMinimalHex := strings.Repeat("a", 32), strings.Repeat("b", 32)
	cases := []struct {
		name     string
		from, to string
		want     domain.BumpClass
	}{
		{"garbage after @ on an unreadable tag", "foo@garbage1", "foo@garbage2", domain.BumpUnknown},
		{"garbage after @ on an equal version", "1.2.3@garbage1", "1.2.3@garbage2", domain.BumpUnknown},
		{"truncated digest on an unreadable tag", "latest@sha256:aa", "latest@sha256:bb", domain.BumpUnknown},
		{"truncated digest on an equal version", "1.2.3@sha256:aa", "1.2.3@sha256:bb", domain.BumpUnknown},
		{"hex one short of the minimum", "latest@sha256:" + shortHex, "latest@sha256:" + strings.Repeat("b", 31), domain.BumpUnknown},
		{"hex at the minimum", "latest@sha256:" + minimalHex, "latest@sha256:" + otherMinimalHex, domain.BumpDigest},
		{"non-hex encoding of the right length", "latest@sha256:" + strings.Repeat("g", 64), "latest@sha256:" + strings.Repeat("h", 64), domain.BumpUnknown},
		{"algorithm missing", "latest@" + strings.Repeat("a", 64), "latest@" + strings.Repeat("b", 64), domain.BumpUnknown},
		{"garbage opposite a real digest", "latest@garbage", "latest@" + digestA, domain.BumpUnknown},
		{"real digests on an unreadable tag", "latest@" + digestA, "latest@" + digestB, domain.BumpDigest},
		{"real digests on an equal version", "1.2.3@" + digestA, "1.2.3@" + digestB, domain.BumpDigest},
	}
	for _, test := range cases {
		if got := renovate.CompareVersions(test.from, test.to); got != test.want {
			t.Errorf("%s: CompareVersions(%q, %q) = %q, want %q", test.name, test.from, test.to, got, test.want)
		}
	}
}

func TestARebuildOnlyOneSideCanVerifyIsNotADigestChange(t *testing.T) {
	cases := []struct {
		name     string
		from, to string
		want     domain.BumpClass
	}{
		{"garbage opposite a real digest", "1.2.3@garbage1", "1.2.3@" + digestA, domain.BumpUnknown},
		{"real digest opposite garbage", "1.2.3@" + digestA, "1.2.3@garbage1", domain.BumpUnknown},
		{"truncated digest opposite a real digest", "1.2.3@sha256:aa", "1.2.3@" + digestA, domain.BumpUnknown},
		{"non-hex encoding opposite a real digest", "1.2.3@sha256:" + strings.Repeat("g", 64), "1.2.3@" + digestA, domain.BumpUnknown},
		{"digest pinned onto an unpinned version", "1.2.3", "1.2.3@" + digestA, domain.BumpUnknown},
		{"digest dropped from a pinned version", "1.2.3@" + digestA, "1.2.3", domain.BumpUnknown},
		{"equal prerelease, one side unverifiable", "1.2.3-rc.1@garbage", "1.2.3-rc.1@" + digestA, domain.BumpUnknown},
		{"both sides verify and differ", "1.2.3@" + digestA, "1.2.3@" + digestB, domain.BumpDigest},
		{"both sides verify and differ, prerelease", "1.2.3-rc.1@" + digestA, "1.2.3-rc.1@" + digestB, domain.BumpDigest},
	}
	for _, test := range cases {
		if got := renovate.CompareVersions(test.from, test.to); got != test.want {
			t.Errorf("%s: CompareVersions(%q, %q) = %q, want %q", test.name, test.from, test.to, got, test.want)
		}
	}
}

func TestADeclaredDigestCannotUnderstateARebuildOnlyOneSideCanVerify(t *testing.T) {
	if got := renovate.BumpClass("1.2.3@garbage1", "1.2.3@"+digestA, "digest"); got != domain.BumpUnknown {
		t.Fatalf("BumpClass(%q, %q, %q) = %q, want %q", "1.2.3@garbage1", "1.2.3@"+digestA, "digest", got, domain.BumpUnknown)
	}
}

func TestWhitespaceAroundADigestDoesNotMakeItDifferFromItself(t *testing.T) {
	cases := []struct {
		name     string
		from, to string
		want     domain.BumpClass
	}{
		{"leading space opposite the same digest on an unreadable tag", "latest@ " + digestA, "latest@" + digestA, domain.BumpUnknown},
		{"trailing space opposite the same digest on an unreadable tag", "latest@" + digestA + " ", "latest@" + digestA, domain.BumpUnknown},
		{"padded on both sides, same digest, unreadable tag", "latest@ " + digestA, "latest@" + digestA + "\t", domain.BumpUnknown},
		{"leading space opposite the same digest on an equal version", "1.2.3@ " + digestA, "1.2.3@" + digestA, domain.BumpUnknown},
		{"padded on both sides, same digest, equal version", "1.2.3@\t" + digestA, "1.2.3@" + digestA + " ", domain.BumpUnknown},
		{"padded bare digest opposite its unpadded self", " " + digestA, digestA + " ", domain.BumpUnknown},
		{"padded on both sides, differing digests, unreadable tag", "latest@ " + digestA, "latest@ " + digestB, domain.BumpDigest},
		{"padded on both sides, differing digests, equal version", "1.2.3@ " + digestA, "1.2.3@" + digestB + " ", domain.BumpDigest},
	}
	for _, test := range cases {
		if got := renovate.CompareVersions(test.from, test.to); got != test.want {
			t.Errorf("%s: CompareVersions(%q, %q) = %q, want %q", test.name, test.from, test.to, got, test.want)
		}
	}
}

func TestIsDigestReadsTheWholeValueSoPaddingIsNotADigest(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  bool
	}{
		{"unpadded", digestA, true},
		{"leading space", " " + digestA, false},
		{"trailing space", digestA + " ", false},
		{"leading tab", "\t" + digestA, false},
		{"trailing newline", digestA + "\n", false},
		{"padded on both sides", " " + digestA + " ", false},
		{"no colon", strings.TrimPrefix(digestA, "sha256:"), false},
		{"empty algorithm", ":1111111111111111111111111111111111111111111111111111111111111111", false},
		{"encoded too short", "sha256:1111", false},
		{"encoded not hex", "sha256:gggggggggggggggggggggggggggggggggggggggggggggggggggggggggggggggg", false},
	}
	for _, test := range cases {
		if got := renovate.IsDigest(test.value); got != test.want {
			t.Errorf("%s: IsDigest(%q) = %v, want %v", test.name, test.value, got, test.want)
		}
	}
}

func TestIsDigestReadsTheShapeAndNotTheAlgorithmsTrueLength(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  bool
	}{
		{"sha256 in upper case hex", "sha256:" + strings.Repeat("A", 64), true},
		{"sha256 in mixed case hex", "sha256:" + strings.Repeat("aB", 32), true},
		{"md5 at its own full length", "md5:" + strings.Repeat("a", 32), true},
		{"sha512 truncated to a third of its length", "sha512:" + strings.Repeat("a", 40), true},
		{"an algorithm nothing publishes", "crc:" + strings.Repeat("a", 32), true},
		{"an algorithm carrying a plus", "sha256+b64:" + strings.Repeat("a", 64), false},
		{"an algorithm in upper case", "SHA256:" + strings.Repeat("a", 64), false},
		{"one hex digit short of the minimum", "sha256:" + strings.Repeat("a", 31), false},
		{"a tag that merely carries a colon", "registry:5000", false},
	}
	for _, test := range cases {
		if got := renovate.IsDigest(test.value); got != test.want {
			t.Errorf("%s: IsDigest(%q) = %v, want %v", test.name, test.value, got, test.want)
		}
	}
}

func TestTwoDigestsDifferingOnlyInHexCaseReadAsARebuild(t *testing.T) {
	lower, upper := "sha256:"+strings.Repeat("ab", 32), "sha256:"+strings.Repeat("AB", 32)
	cases := []struct {
		name     string
		from, to string
		want     domain.BumpClass
	}{
		{"case flipped under an equal version", "1.2.3@" + lower, "1.2.3@" + upper, domain.BumpDigest},
		{"case flipped under an unreadable tag", "latest@" + lower, "latest@" + upper, domain.BumpDigest},
		{"case flipped on a bare digest", lower, upper, domain.BumpDigest},
		{"the same case on both sides", "1.2.3@" + lower, "1.2.3@" + lower, domain.BumpUnknown},
	}
	for _, test := range cases {
		if got := renovate.CompareVersions(test.from, test.to); got != test.want {
			t.Errorf("%s: CompareVersions(%q, %q) = %q, want %q", test.name, test.from, test.to, got, test.want)
		}
	}
}

func TestADeclaredDigestCannotUnderstateAnUnverifiableDigestChange(t *testing.T) {
	if got := renovate.BumpClass("foo@garbage1", "foo@garbage2", "digest"); got != domain.BumpUnknown {
		t.Fatalf("BumpClass(%q, %q, %q) = %q, want %q", "foo@garbage1", "foo@garbage2", "digest", got, domain.BumpUnknown)
	}
}

func TestADeclaredClassCannotUnderstateAnUnreadableTagChangeThatCarriesDigests(t *testing.T) {
	cases := []struct {
		from, to, declared string
		want               domain.BumpClass
	}{
		{"RELEASE.2025-04-22T22-12-26Z@" + digestA, "RELEASE.2025-05-01T00-00-00Z@" + digestB, "minor", domain.BumpUnknown},
		{"RELEASE.2025-04-22T22-12-26Z@" + digestA, "RELEASE.2025-05-01T00-00-00Z@" + digestB, "patch", domain.BumpUnknown},
		{"latest@" + digestA, "edge@" + digestB, "patch", domain.BumpUnknown},
		{"latest@" + digestA, "edge@" + digestB, "minor", domain.BumpUnknown},
		{"latest@" + digestA, "edge@" + digestB, "digest", domain.BumpUnknown},
		{"latest@" + digestA, "edge@" + digestB, "major", domain.BumpMajor},
		{"latest@" + digestA, "latest@" + digestB, "digest", domain.BumpDigest},
		{digestA, digestB, "digest", domain.BumpDigest},
	}
	for _, test := range cases {
		if got := renovate.BumpClass(test.from, test.to, test.declared); got != test.want {
			t.Errorf("BumpClass(%q, %q, %q) = %q, want %q", test.from, test.to, test.declared, got, test.want)
		}
	}
}

func TestAPinnedDigestOnACalverBodyStillReachesTheOperator(t *testing.T) {
	body := "This PR contains the following updates:\n\n" +
		"| Package | Type | Update | Change |\n" +
		"|---|---|---|---|\n" +
		"| [quay.io/minio/minio](https://quay.io/minio/minio) | image | minor | " +
		"`RELEASE.2025-04-22T22-12-26Z@" + digestA + "` -> `RELEASE.2025-05-01T00-00-00Z@" + digestB + "` |\n"

	update, err := renovate.Single(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if update.Dependency.Bump != domain.BumpUnknown {
		t.Fatalf("bump = %q, want %q", update.Dependency.Bump, domain.BumpUnknown)
	}
}

func TestAVersionThatWentBackwardsIsNoClassOfUpgrade(t *testing.T) {
	cases := []struct {
		name     string
		from, to string
		want     domain.BumpClass
	}{
		{"patch-shaped rollback", "4.0.25", "4.0.19", domain.BumpUnknown},
		{"minor-shaped rollback", "1.5.0", "1.4.0", domain.BumpUnknown},
		{"major-shaped rollback", "2.0.0", "1.9.9", domain.BumpUnknown},
		{"leading v on both sides", "v4.0.25", "v4.0.19", domain.BumpUnknown},
		{"four-segment rollback", "4.0.19.2995", "4.0.14.2939", domain.BumpUnknown},
		{"rollback written with fewer segments", "1.5.0", "1", domain.BumpUnknown},
		{"trailing segment dropped backwards", "1.0.1", "1.0", domain.BumpUnknown},
		{"rollback carrying digests on both sides", "1.2.3@" + digestA, "1.2.2@" + digestB, domain.BumpUnknown},
		{"release rolled back to its own prerelease", "1.2.3", "1.2.3-rc.1", domain.BumpUnknown},
		{"prerelease rolled back", "1.2.3-rc.2", "1.2.3-rc.1", domain.BumpUnknown},
		// Forward and standing-still pairs keep the class they always had.
		{"patch forward", "4.0.19", "4.0.25", domain.BumpPatch},
		{"minor forward", "1.4.0", "1.5.0", domain.BumpMinor},
		{"major forward", "1.9.9", "2.0.0", domain.BumpMajor},
		{"four-segment forward", "4.0.14.2939", "4.0.19.2995", domain.BumpPatch},
		{"forward written with fewer segments", "1", "1.5.0", domain.BumpMinor},
		{"equal versions", "1.2.3", "1.2.3", domain.BumpUnknown},
		{"equal version, digest repinned", "1.2.3@" + digestA, "1.2.3@" + digestB, domain.BumpDigest},
		{"same value spelled with fewer segments", "1.2", "1.2.0", domain.BumpPatch},
		{"same value, trailing zero added", "1", "1.0.0", domain.BumpPatch},
		{"prerelease promoted to its release", "1.2.3-alpha", "1.2.3", domain.BumpUnknown},
		{"build metadata replaced", "1.2.3+build1", "1.2.3+build2", domain.BumpUnknown},
		{"floating tags neither side can order", "latest", "edge", domain.BumpUnknown},
	}
	for _, test := range cases {
		if got := renovate.CompareVersions(test.from, test.to); got != test.want {
			t.Errorf("%s: CompareVersions(%q, %q) = %q, want %q", test.name, test.from, test.to, got, test.want)
		}
	}
}

func TestADeclaredClassCannotUnderstateADowngrade(t *testing.T) {
	cases := []struct {
		from, to, declared string
		want               domain.BumpClass
	}{
		{"4.0.25", "4.0.19", "", domain.BumpUnknown},
		{"4.0.25", "4.0.19", "patch", domain.BumpUnknown},
		{"4.0.25", "4.0.19", "minor", domain.BumpUnknown},
		{"4.0.25", "4.0.19", "digest", domain.BumpUnknown},
		{"1.2.3@" + digestA, "1.2.2@" + digestB, "digest", domain.BumpUnknown},
		// Renovate's own rollback type held before the arithmetic could see one,
		// and still holds.
		{"1.3.0", "1.2.9", "rollback", domain.BumpUnknown},
		{"1.3.0", "1.2.9", "replacement", domain.BumpUnknown},
		// A declared major is the most alarming answer there is; a rollback below
		// it would trade a merge that asks the operator for one that does not.
		{"4.0.25", "4.0.19", "major", domain.BumpMajor},
	}
	for _, test := range cases {
		if got := renovate.BumpClass(test.from, test.to, test.declared); got != test.want {
			t.Errorf("BumpClass(%q, %q, %q) = %q, want %q", test.from, test.to, test.declared, got, test.want)
		}
	}
}

// A rollback Renovate did not label: the body carries no Update column, so the
// version arithmetic is the only thing standing between it and an unattended
// merge that never reads the releases it removes.
const rollbackBody = "This PR contains the following updates:\n\n" +
	"| Package | Change |\n" +
	"|---|---|\n" +
	"| [ghcr.io/home-operations/sonarr](https://ghcr.io/home-operations/sonarr) " +
	"([source](https://redirect.github.com/Sonarr/Sonarr)) | `v4.0.25` -> `v4.0.19` |\n"

func TestARollbackBodyWithNoUpdateColumnDoesNotClassifyAsAPatch(t *testing.T) {
	update, err := renovate.Single(rollbackBody)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if update.Dependency.FromVersion != "v4.0.25" || update.Dependency.ToVersion != "v4.0.19" {
		t.Fatalf("versions misread: %+v", update.Dependency)
	}
	if update.Dependency.Bump != domain.BumpUnknown {
		t.Fatalf("bump = %q, want %q", update.Dependency.Bump, domain.BumpUnknown)
	}
}

func TestAPrereleaseNeverOutranksItsOwnReleaseWrittenWithFewerSegments(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0-alpha", "1.0", -1},
		{"1.0", "1.0.0-alpha", 1},
		{"1.0.0", "1.0", 0},
		{"1.0", "1.0.0", 0},
		{"1.0.1", "1.0", 1},
		{"1.0", "1.0.1", -1},
		{"4.0.14.2939", "4.0.14", 1},
		{"4.0.14", "4.0.14.2939", -1},
	}
	for _, test := range cases {
		if got := renovate.Newer(test.a, test.b); got != test.want {
			t.Errorf("Newer(%q, %q) = %d, want %d", test.a, test.b, got, test.want)
		}
	}
}

// A real Renovate body, copied verbatim from lkshrk/h-cloud#740: the Package
// cell carries a second "(source)" link and there is no Type column at all.
const realBody = "This PR contains the following updates:\n\n" +
	"| Package | Update | Change |\n" +
	"|---|---|---|\n" +
	"| [ghcr.io/home-operations/sonarr](https://ghcr.io/home-operations/sonarr) " +
	"([source](https://redirect.github.com/Sonarr/Sonarr)) | patch | `4.0.18.2978` → `4.0.19.2995` |\n" +
	"\n---\n\n### Release Notes\n\n" +
	"<details>\n<summary>Sonarr/Sonarr (ghcr.io/home-operations/sonarr)</summary>\n\n" +
	"Fixed an import crash.\n\n</details>\n"

func TestSingleReadsARealRenovateBody(t *testing.T) {
	update, err := renovate.Single(realBody)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// The trailing "(source)" link must not become part of the name, or the
	// configured changelog override for this image never matches.
	if update.Dependency.Name != "ghcr.io/home-operations/sonarr" {
		t.Fatalf("name carries Renovate's source link: %q", update.Dependency.Name)
	}
	if update.Dependency.FromVersion != "4.0.18.2978" || update.Dependency.ToVersion != "4.0.19.2995" {
		t.Fatalf("versions misread: %+v", update.Dependency)
	}
	if update.Dependency.Bump != domain.BumpPatch {
		t.Fatalf("want a patch bump, got %q", update.Dependency.Bump)
	}
	if update.Upstream != "Sonarr/Sonarr" {
		t.Fatalf("upstream: %q", update.Upstream)
	}
}
