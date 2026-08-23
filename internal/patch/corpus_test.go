package patch_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/lkshrk/ops-pilot/internal/patch"
)

// The manifests a Flux GitOps repository is actually made of, at the shapes
// where structure repeats: a bundle of Kustomizations that differ only in the
// app name, a release whose sidecar carries the same resource block as its main
// container, and a chart whose subcharts each declare replicas and limits.
const (
	fluxKustomizationBundle = `---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: sonarr
  namespace: flux-system
spec:
  interval: 30m
  path: ./kubernetes/apps/media/sonarr/app
  prune: true
  sourceRef:
    kind: GitRepository
    name: home-ops
  wait: false
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: radarr
  namespace: flux-system
spec:
  interval: 30m
  path: ./kubernetes/apps/media/radarr/app
  prune: true
  sourceRef:
    kind: GitRepository
    name: home-ops
  wait: false
---
apiVersion: kustomize.toolkit.fluxcd.io/v1
kind: Kustomization
metadata:
  name: bazarr
  namespace: flux-system
spec:
  interval: 30m
  path: ./kubernetes/apps/media/bazarr/app
  prune: true
  sourceRef:
    kind: GitRepository
    name: home-ops
  wait: false
`

	appTemplateRelease = `---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: sonarr
spec:
  interval: 30m
  chart:
    spec:
      chart: app-template
      version: 3.5.1
      sourceRef:
        kind: HelmRepository
        name: bjw-s
        namespace: flux-system
  values:
    controllers:
      sonarr:
        containers:
          app:
            image:
              repository: ghcr.io/home-operations/sonarr
              tag: 4.0.10.2544
            env:
              TZ: Europe/Berlin
            resources:
              requests:
                cpu: 100m
                memory: 512Mi
              limits:
                memory: 2Gi
    service:
      app:
        controller: sonarr
        ports:
          http:
            port: 8989
    ingress:
      app:
        className: internal
        hosts:
          - host: sonarr.example.com
            paths:
              - path: /
                service:
                  identifier: app
                  port: http
`

	releaseWithASidecar = `---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: atuin
spec:
  interval: 30m
  values:
    controllers:
      atuin:
        containers:
          app:
            image:
              repository: ghcr.io/atuinsh/atuin
              tag: 18.4.0
            resources:
              requests:
                cpu: 10m
                memory: 128Mi
              limits:
                memory: 256Mi
          exporter:
            image:
              repository: ghcr.io/prometheus/exporter
              tag: 0.15.0
            resources:
              requests:
                cpu: 10m
                memory: 128Mi
              limits:
                memory: 256Mi
`

	monitoringStackValues = `---
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: kube-prometheus-stack
  namespace: observability
spec:
  interval: 30m
  chart:
    spec:
      chart: kube-prometheus-stack
      version: 65.1.1
      sourceRef:
        kind: HelmRepository
        name: prometheus-community
        namespace: flux-system
  values:
    alertmanager:
      alertmanagerSpec:
        replicas: 1
        retention: 120h
        resources:
          requests:
            cpu: 10m
            memory: 128Mi
          limits:
            memory: 256Mi
    prometheus:
      prometheusSpec:
        replicas: 1
        retention: 14d
        resources:
          requests:
            cpu: 100m
            memory: 1Gi
          limits:
            memory: 4Gi
    grafana:
      replicas: 1
      resources:
        requests:
          cpu: 10m
          memory: 128Mi
        limits:
          memory: 256Mi
`

	kustomizationResourceList = `---
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - ./bazarr/ks.yaml
  - ./prowlarr/ks.yaml
  - ./radarr/ks.yaml
  - ./sonarr/ks.yaml
`
)

// Hand-written values files separate their repeated blocks with blank lines,
// which a model reproduces as a context line whose single leading space many
// editors and renderers then strip.
const blankSeparatedValues = `controllers:
  sonarr:
    containers:
      app:
        image:
          repository: ghcr.io/home-operations/sonarr
          tag: 4.0.10.2544
        resources:
          requests:
            cpu: 100m
            memory: 512Mi

  radarr:
    containers:
      app:
        image:
          repository: ghcr.io/home-operations/radarr
          tag: 5.14.0.9383
        resources:
          requests:
            cpu: 100m
            memory: 512Mi

  bazarr:
    containers:
      app:
        image:
          repository: ghcr.io/home-operations/bazarr
          tag: 1.4.5
        resources:
          requests:
            cpu: 100m
            memory: 512Mi
`

var gitOpsCorpus = []struct{ name, content string }{
	{"a bundle of flux Kustomizations", fluxKustomizationBundle},
	{"an app-template HelmRelease", appTemplateRelease},
	{"a release with a sidecar", releaseWithASidecar},
	{"a monitoring stack's values", monitoringStackValues},
	{"a kustomization resource list", kustomizationResourceList},
}

// oneLineFix writes the diff an agent produces to change a single line: above
// lines of leading context, below lines of trailing context, and a @@ header
// that states the true position. Nothing about it is wrong, so every refusal it
// draws is a false positive.
func oneLineFix(lines []string, index, above, below int) string {
	start, end := max(index-above, 0), min(index+below+1, len(lines))
	var diff strings.Builder
	diff.WriteString("--- a/f.yaml\n+++ b/f.yaml\n")
	fmt.Fprintf(&diff, "@@ -%d,%d +%d,%d @@\n", start+1, end-start, start+1, end-start)
	for i := start; i < end; i++ {
		if i == index {
			diff.WriteString("-" + lines[i] + "\n+" + lines[i] + " # CHANGED\n")
			continue
		}
		diff.WriteString(" " + lines[i] + "\n")
	}
	return diff.String()
}

// applyOneLineFix reports whether the fix landed exactly where it was aimed.
func applyOneLineFix(content string, lines []string, index, above, below int) (landed bool, err error) {
	return applyDiff(oneLineFix(lines, index, above, below), content, lines, index)
}

// isRenovateFixSite reports whether a line is one an ops-pilot fix plausibly
// rewrites: the version, image and resource knobs a failed upgrade is repaired
// with. The all-lines rate below is the honest denominator; this one is the
// rate on the lines that matter.
func isRenovateFixSite(line string) bool {
	trimmed := strings.TrimSpace(line)
	for _, key := range []string{"tag:", "version:", "replicas:", "memory:", "cpu:", "interval:", "retention:", "port:", "repository:"} {
		if strings.HasPrefix(trimmed, key) {
			return true
		}
	}
	return false
}

// Whether a blank line is a context line or the gap before an answer's closing
// prose is not decidable from the line itself, so the rule that decides it must
// at least be invisible: stripping the single leading space off a blank context
// line, which editors and renderers do routinely, must not change whether a
// correct fix applies.
//
// This measures that, and it also bounds how much the rule can matter. Only the
// hunks that end on a blank are exposed to it at all, and in this corpus the
// context above such a blank already carries a unique key — an image repository
// or an app name — so the blank is never the line that disambiguates. The shape
// where it is decisive needs two blocks identical above the blank and differing
// only below it, which the unit table covers and no manifest here produces.
func TestStrippingABlankContextLineDoesNotChangeTheVerdict(t *testing.T) {
	lines := strings.Split(strings.TrimSuffix(blankSeparatedValues, "\n"), "\n")
	var carried, ending, changed int
	for index, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		diff := oneLineFix(lines, index, 3, 3)
		if !strings.Contains(diff, "\n \n") {
			continue
		}
		carried++
		if strings.HasSuffix(diff, "\n \n") {
			ending++
		}
		intact, intactErr := applyDiff(diff, blankSeparatedValues, lines, index)
		stripped, strippedErr := applyDiff(
			strings.ReplaceAll(diff, "\n \n", "\n\n"), blankSeparatedValues, lines, index)
		if intact != stripped || (intactErr == nil) != (strippedErr == nil) {
			changed++
			t.Errorf("line %d %q: stripping the space from a blank context line changed the verdict: intact=%v stripped=%v",
				index+1, strings.TrimSpace(line), intactErr, strippedErr)
		}
	}
	if ending == 0 {
		t.Fatal("no hunk here ends on a blank context line, so the rule this measures is not exercised at all")
	}
	t.Logf("%d hunks carry a blank context line and %d end on one; %d changed verdict when it was stripped",
		carried, ending, changed)
}

func applyDiff(diff, content string, lines []string, index int) (landed bool, err error) {
	files, err := patch.Parse(diff)
	if err != nil {
		return false, err
	}
	got, err := patch.Apply([]byte(content), files[0])
	if err != nil {
		return false, err
	}
	want := append(append([]string{}, lines[:index]...), lines[index]+" # CHANGED")
	want = append(want, lines[index+1:]...)
	return string(got) == strings.Join(want, "\n")+"\n", nil
}

// S-M08 refuses a hunk whose context matches more than once even when the
// header states the right position, on the ground that the position is a
// coincidence rather than a choice. This is the cost of that rule, measured
// rather than argued: every line of every manifest above, changed by a diff
// that is correct and exactly positioned, at the context widths a model
// actually emits.
//
// It is not zero. At git's own default of three lines each side, 12 of 173
// correct fixes are refused, and the rate roughly triples if the model quotes
// only the lines above the change. Refusal is not a wrong write — see
// TestNoCorrectlyPositionedFixInTheCorpusLandsOnTheWrongObject — but on the
// WatchStalled path a fix that is refused twice reverts a merge that was never
// broken, so this is the number that bounds that risk.
func TestSomeCorrectlyPositionedFixesAreRefusedInRepeatingGitOpsStructures(t *testing.T) {
	for _, tc := range []struct {
		name                  string
		above, below          int
		refused, refusedAtFix int
	}{
		{name: "git's default of three lines each side", above: 3, below: 3, refused: 12, refusedAtFix: 2},
		{name: "three lines above the change only", above: 3, below: 0, refused: 24, refusedAtFix: 4},
		{name: "two lines above the change only", above: 2, below: 0, refused: 32, refusedAtFix: 9},
		{name: "one line each side", above: 1, below: 1, refused: 33, refusedAtFix: 5},
		{name: "the changed line alone", above: 0, below: 0, refused: 60, refusedAtFix: 11},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var total, refused, totalAtFix, refusedAtFix int
			for _, file := range gitOpsCorpus {
				lines := strings.Split(strings.TrimSuffix(file.content, "\n"), "\n")
				for index, line := range lines {
					landed, err := applyOneLineFix(file.content, lines, index, tc.above, tc.below)
					total++
					fixSite := isRenovateFixSite(line)
					if fixSite {
						totalAtFix++
					}
					if err == nil && landed {
						continue
					}
					refused++
					if fixSite {
						refusedAtFix++
					}
				}
			}
			if refused != tc.refused || refusedAtFix != tc.refusedAtFix {
				t.Fatalf(
					"refusal rate moved: %d of %d correct fixes refused (%d of %d at a version or resource line), was %d and %d",
					refused, total, refusedAtFix, totalAtFix, tc.refused, tc.refusedAtFix)
			}
			t.Logf("%d of %d correct fixes refused (%d of %d at a version or resource line)",
				refused, total, refusedAtFix, totalAtFix)
		})
	}
}

// The rate above is only tolerable because a refusal is recoverable: the error
// says to add a line that appears once, and in every repeating structure in the
// corpus such a line is within three of the change — the container name, the
// app name, the subchart key. That fits the one retry MaxFixAttempts allows.
// What is not measured here, and cannot be from inside this package, is whether
// the agent takes that action rather than renumbering the header.
func TestEveryRefusalInTheCorpusRecoversWithinThreeMoreLinesOfContext(t *testing.T) {
	for _, file := range gitOpsCorpus {
		lines := strings.Split(strings.TrimSuffix(file.content, "\n"), "\n")
		for index, line := range lines {
			if landed, err := applyOneLineFix(file.content, lines, index, 3, 3); err == nil && landed {
				continue
			}
			recovered := false
			for widen := 1; widen <= 3 && !recovered; widen++ {
				landed, err := applyOneLineFix(file.content, lines, index, 3+widen, 3+widen)
				recovered = err == nil && landed
			}
			if !recovered {
				t.Errorf(
					"%s line %d %q: no amount of context the agent can add in one retry makes this hunk unique, so the fix is unreachable",
					file.name, index+1, strings.TrimSpace(line))
			}
		}
	}
}

// Refusing is the whole of S-M08's cost. Whatever the context width, a diff
// that quotes the file correctly and states the right position either lands on
// the line it named or is refused outright — it is never applied to the sibling
// object that happens to read the same. A failure here is a wrong write into a
// production cluster, not a retry.
func TestNoCorrectlyPositionedFixInTheCorpusLandsOnTheWrongObject(t *testing.T) {
	for _, width := range [][2]int{{3, 3}, {3, 0}, {2, 0}, {1, 1}, {0, 0}} {
		for _, file := range gitOpsCorpus {
			lines := strings.Split(strings.TrimSuffix(file.content, "\n"), "\n")
			for index := range lines {
				landed, err := applyOneLineFix(file.content, lines, index, width[0], width[1])
				if err == nil && !landed {
					t.Fatalf("%s line %d with %d/%d lines of context: applied somewhere other than the line the diff named",
						file.name, index+1, width[0], width[1])
				}
			}
		}
	}
}
