package run

import (
	"context"
	"fmt"
	"strings"

	"github.com/lkshrk/ops-pilot/internal/config"
	"github.com/lkshrk/ops-pilot/internal/patch"
	"github.com/lkshrk/ops-pilot/internal/repopath"
)

func patchPath(file patch.File) string {
	if file.NewPath != "" {
		return file.NewPath
	}
	return file.OldPath
}

// diffPaths names every path one section of a diff can reach. applyFix writes
// only patchPath, but revertCommit restores both halves of the ---/+++ pair, so
// the gate and fixPaths have to harvest the same set or the revert writes a path
// nothing checked. Its input has been through parseFix, so an absent half is
// empty rather than a /dev/null marker.
func diffPaths(file patch.File) []string {
	paths := make([]string, 0, 2)
	if file.OldPath != "" {
		paths = append(paths, file.OldPath)
	}
	if file.NewPath != "" && file.NewPath != file.OldPath {
		paths = append(paths, file.NewPath)
	}
	return paths
}

// parseFix reads a fix diff and settles the /dev/null halves cleanPath leaves
// whole. A marker carrying a timestamp is still the absence of a file, and read
// as a path it names one no allowlist can cover and no repository holds.
func parseFix(diff string) ([]patch.File, error) {
	files, err := patch.Parse(diff)
	if err != nil {
		return nil, err
	}
	for i := range files {
		if devNull(files[i].OldPath) {
			files[i].OldPath, files[i].Created = "", true
		}
		if devNull(files[i].NewPath) {
			files[i].NewPath, files[i].Deleted = "", true
		}
	}
	return files, nil
}

// notARename refuses a section whose halves name two different files. Nothing
// in a diff tells a deliberate move from a mistyped +++ line, and the two
// readings disagree over whether the old path survives.
func notARename(file patch.File) error {
	if file.OldPath == "" || file.NewPath == "" || file.OldPath == file.NewPath {
		return nil
	}
	if devNull(file.OldPath) || devNull(file.NewPath) {
		return nil
	}
	return fmt.Errorf(
		"refusing to move %q to %q: a fix may not rename a file, because nothing in the diff says whether the move is meant or the +++ line is mistyped; write it as one section deleting %q and one section creating %q",
		file.OldPath, file.NewPath, file.OldPath, file.NewPath)
}

// devNull matches the creation marker some producers write with a trailing
// timestamp (`/dev/null 1970-01-01 ...`) that cleanPath leaves whole.
func devNull(path string) bool {
	rest, ok := strings.CutPrefix(path, "/dev/null")
	return ok && (rest == "" || rest[0] == ' ' || rest[0] == '\t')
}

// allowedFixPath is the outer gate. A path carries no sign of the kind the
// manifest inside it declares, so a refusal set can only enumerate the dangers
// somebody already thought of; what nobody listed is what has to be refused.
func (r *Runner) allowedFixPath(path string) error {
	if len(r.options.FixAllowedPaths) == 0 {
		return fmt.Errorf("refusing %q: no fixes.allowedPaths are configured, so no fix may write anything", path)
	}
	if !config.AllowsFixPath(r.options.FixAllowedPaths, path) {
		return fmt.Errorf("refusing %q: no fixes.allowedPaths pattern covers it", path)
	}
	return nil
}

// unboundedFixPaths returns the configured pattern that covers the whole
// repository, if one is configured. Accepting it is deliberate: an allowlist
// that cannot express "anywhere" is hardcoded policy rather than a declaration.
// The test is measured against config.AllowsFixPath, not the pattern's syntax:
// "**/*" and "*/**" match every path just as "**" does, and a syntactic
// all-"**" check misses them.
func unboundedFixPaths(patterns []string) string {
	for _, pattern := range patterns {
		if pattern == "" {
			continue
		}
		if matchesEveryPath(pattern) {
			return pattern
		}
	}
	return ""
}

// everyPathProbe spans depths, names and extensions so that only a pattern
// admitting every repository path matches all of them; the single root-level
// segment is what "*/*/**" fails on.
var everyPathProbe = [...]string{
	"a",
	"z/y",
	"m/n/o/p/q",
	"README",
	"dir.d/File_1-2.YAML",
}

func matchesEveryPath(pattern string) bool {
	for _, path := range everyPathProbe {
		if !config.AllowsFixPath([]string{pattern}, path) {
			return false
		}
	}
	return true
}

// writablePath decides whether a path taken from a model-authored diff may be
// used at all. The commit mutation validates paths too, but only on the write,
// so a traversal still reached an authenticated repository read on the way
// there; this runs before the first read instead.
func writablePath(path string) error {
	// No Escapes case: a ".git" segment ahead of a ".." must still refuse as git metadata.
	switch repopath.Check(path) {
	case repopath.Empty:
		return fmt.Errorf("the diff names no file")
	case repopath.TooLong:
		return fmt.Errorf("refusing an over-long path")
	case repopath.NotRelative:
		return fmt.Errorf("refusing %q: a diff path must be relative to the repository root", path)
	case repopath.NotPlain:
		return fmt.Errorf("refusing %q: it is not a plain repository path", path)
	}
	segments := strings.Split(path, "/")
	for _, segment := range segments {
		if segment == "." || segment == ".." {
			return fmt.Errorf("refusing %q: it does not stay inside the repository", path)
		}
		if strings.HasPrefix(strings.ToLower(segment), ".git") {
			return fmt.Errorf("refusing %q: a fix may not change repository automation or git metadata", path)
		}
	}
	if bootstrapArtefact(segments) {
		return fmt.Errorf("refusing %q: a fix may not change where the cluster reconciles from", path)
	}
	if bootstrapFiles[strings.ToLower(segments[len(segments)-1])] {
		return fmt.Errorf("refusing %q: a fix may not change where the cluster reconciles from", path)
	}
	if governingPaths[strings.ToLower(path)] {
		return fmt.Errorf("refusing %q: a fix may not change what may be changed", path)
	}
	return nil
}

// notABootstrapKustomization refuses a kustomization that governs a directory
// holding the manifests "flux bootstrap" wrote. bootstrapArtefact can only
// recognise such a directory by the name flux-system, and that name is the -n
// flag rather than a constant: the manifests go to <path>/<namespace>, so an
// installation bootstrapped under any other namespace has a directory no
// path-shaped rule knows. The gotk files beside the kustomization are the
// evidence the directory's name is not.
//
// A probe that cannot be answered refuses. The question it failed to settle is
// whether this write repoints the cluster, and a fix refused on it costs one
// attempt and at worst a revert, which is the outcome the whole gate already
// produces.
func (r *Runner) notABootstrapKustomization(ctx context.Context, path, ref string) error {
	cut := strings.LastIndexByte(path, '/')
	if !kustomizationNames[strings.ToLower(path[cut+1:])] {
		return nil
	}
	for _, name := range bootstrapProbes {
		probe := path[:cut+1] + name
		if !config.AllowsFixPath(r.options.FixAllowedPaths, probe) {
			return fmt.Errorf(
				"refusing %q: whether it governs a Flux bootstrap directory could not be established, because fixes.allowedPaths does not cover the sibling %q the probe would read; allowing its directory rather than the file itself is what settles that question", path, probe)
		}
		_, found, err := r.forge.FileAt(ctx, probe, ref)
		if err != nil {
			return fmt.Errorf(
				"refusing %q: whether it governs a Flux bootstrap directory could not be established: %w", path, err)
		}
		if found {
			return fmt.Errorf("refusing %q: a fix may not change where the cluster reconciles from", path)
		}
	}
	return nil
}

// unprobeableKustomization returns a configured pattern that admits a
// kustomization but not the bootstrap manifests beside it, and the first of
// those manifests. Such a pattern refuses every fix it admits, and the refusal
// arrives at the one moment nobody wants to be reading about configuration: a
// broken cluster, a repair that cannot land, and a revert.
func unprobeableKustomization(patterns []string) (string, string) {
	for _, pattern := range patterns {
		directory := pattern[:strings.LastIndexByte(pattern, '/')+1]
		if !admitsKustomization(pattern, directory) {
			continue
		}
		for _, name := range bootstrapProbes {
			if probe := directory + name; !config.AllowsFixPath(patterns, probe) {
				return pattern, probe
			}
		}
	}
	return "", ""
}

// admitsKustomization asks the matcher instead of reading the pattern's last
// segment: "kustomization.*" and "kustomization.y*ml" name one just as surely,
// and a pattern matched against its own directory prefix answers for any
// spelling of a glob without this having to know the glob syntax. Both sides are
// folded because notABootstrapKustomization recognises the name case-insensitively
// while the matcher is case-sensitive, and it is the refusal this warns about.
func admitsKustomization(pattern, directory string) bool {
	folded := []string{strings.ToLower(pattern)}
	for name := range kustomizationNames {
		if config.AllowsFixPath(folded, strings.ToLower(directory)+name) {
			return true
		}
	}
	return false
}

// missingSibling reads the probe as what it is. A pattern spanning directories
// names no file that anything sits beside, so telling an operator to look next
// to it sends them nowhere.
func missingSibling(probe string) string {
	if strings.Contains(probe, "*") {
		return fmt.Sprintf("not a %q beside the files it matches", probe)
	}
	return fmt.Sprintf("not %q beside it", probe)
}

var kustomizationNames = map[string]bool{
	"kustomization.yaml":   true,
	"kustomization.yml":    true,
	"kustomizeconfig.yaml": true,
	"kustomizeconfig.yml":  true,
}

// bootstrapProbes are the two file names "flux bootstrap" always writes into the
// directory it owns, under the .yaml extension its manifest generator hardcodes.
var bootstrapProbes = [...]string{"gotk-sync.yaml", "gotk-components.yaml"}

// bootstrapFiles are what "flux bootstrap" writes: gotk-sync.yaml names the
// repository and branch the cluster reconciles from, gotk-components.yaml is
// the controllers that do the reconciling, RBAC included. Rewriting either
// repoints or re-privileges the cluster instead of repairing a workload. The
// names are matched anywhere because a repository may bootstrap into a
// directory of its own choosing.
var bootstrapFiles = map[string]bool{
	"gotk-sync.yaml":       true,
	"gotk-sync.yml":        true,
	"gotk-components.yaml": true,
	"gotk-components.yml":  true,
}

func inFluxSystemDirectory(segments []string) bool {
	for _, segment := range segments[:len(segments)-1] {
		if strings.EqualFold(segment, "flux-system") {
			return true
		}
	}
	return false
}

// bootstrapArtefact names the files that decide what a flux-system directory
// reconciles. "flux-system" is a namespace as well as the bootstrap directory,
// so refusing the directory whole refuses every ordinary workload an
// apps/<namespace>/ layout keeps under that name, and each of those apps carries
// a kustomization of its own. "flux bootstrap" writes three flat files and no
// subdirectory, so only a kustomization directly inside the directory can be one
// of them and a deeper one is an app's; the one directly inside is refused
// whichever it is, because no path-shaped rule tells a namespace directory's
// kustomization from a bootstrap one. A gotk- name has no ordinary meaning
// anywhere under the directory.
func bootstrapArtefact(segments []string) bool {
	if len(segments) < 2 {
		return false
	}
	name := strings.ToLower(segments[len(segments)-1])
	if inFluxSystemDirectory(segments) && strings.HasPrefix(name, "gotk-") {
		return true
	}
	if !strings.EqualFold(segments[len(segments)-2], "flux-system") {
		return false
	}
	return kustomizationNames[name]
}

// governingPaths decide who may change the repository and which updates reach
// it at all. Flux applies none of them, so a cluster repair never needs one,
// and an approved diff that quietly includes one widens its own authority.
// They are whole paths, not names: every tool that reads one reads it at the
// repository root, so a manifest sharing the name deeper in the tree is an
// ordinary cluster file and refusing it would send a repairable merge to a
// revert. package.json is here because Renovate reads a "renovate" key out of
// it, not because of its dependencies. The other locations git and Renovate
// look in - .github, .gitlab - are already refused by writablePath's ".git"
// segment rule, along with the workflows.
var governingPaths = map[string]bool{
	"codeowners":        true,
	"docs/codeowners":   true,
	"renovate.json":     true,
	"renovate.json5":    true,
	"package.json":      true,
	".renovaterc":       true,
	".renovaterc.json":  true,
	".renovaterc.json5": true,
	"ops-pilot.yaml":    true,
	"ops-pilot.yml":     true,
}
