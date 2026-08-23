package smoke_test

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

var supportedTargets = []target{
	{goos: "darwin", goarch: "amd64"},
	{goos: "darwin", goarch: "arm64"},
	{goos: "linux", goarch: "amd64"},
	{goos: "linux", goarch: "arm64"},
}

var expectedToolVersions = map[string]string{
	"GO_VERSION":                      "1.26.6",
	"GORELEASER_VERSION":              "v2.17.0",
	"GORELEASER_LINUX_AMD64_SHA256":   "dde10e2d5a13cef969c0eec00c74f359c0ac306d702b1bd291ad9337b4e54c1d",
	"GORELEASER_LINUX_ARM64_SHA256":   "75f93fc0e25d10d8535ffd0e4abcf39d6784a2467ba453d479ae513729a9ebbf",
	"SYFT_VERSION":                    "1.44.0",
	"SYFT_LINUX_AMD64_SHA256":         "0e91737aee2b5baf1d255b959630194a302335d848ff97bb07921eb6205b5f5a",
	"SYFT_LINUX_ARM64_SHA256":         "6f6cdcdc695721d91ce756e3b5bc3e3416599c464101f5e32e9c3f33054ee6d9",
	"CRANE_VERSION":                   "v0.21.7",
	"CRANE_LINUX_AMD64_SHA256":        "1a57bc98207fa1c0d04bf760699099e26f8383499bfd55b99c1b919a928a7230",
	"CRANE_LINUX_ARM64_SHA256":        "b6ee979d9411dfb05ce35ab9e156fe5de7def11a230764a7856ffa2eb971fa88",
	"REGISTRY_IMAGE":                  "docker.io/library/registry:2@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373",
	"GO_BUILDER_IMAGE":                "docker.io/library/golang:1.26.6-bookworm@sha256:116d58cbd88c1297624acc6e967a060012422bacf9930927e23fb719189c6f36",
	"RUNTIME_IMAGE":                   "docker.io/library/debian:bookworm-20260623-slim@sha256:60eac759739651111db372c07be67863818726f754804b8707c90979bda511df",
	"ACTIONS_CHECKOUT_SHA":            "3d3c42e5aac5ba805825da76410c181273ba90b1",
	"ACTIONS_SETUP_GO_SHA":            "b7ad1dad31e06c5925ef5d2fc7ad053ef454303e",
	"ACTIONS_UPLOAD_ARTIFACT_SHA":     "043fb46d1a93c77aae656e7c1c64a875d1fc6a0a",
	"ACTIONS_DOWNLOAD_ARTIFACT_SHA":   "3e5f45b2cfb9172054b4087a40e8e0b5a5461e7c",
	"GITHUB_CODEQL_ACTION_SHA":        "e4fba868fa4b1b91e1fdab776edc8cfbe6e9fb81",
	"DOCKER_LOGIN_ACTION_SHA":         "abd2ef45e78c5afb21d64d4ca52ee8550d9572c7",
	"DOCKER_METADATA_ACTION_SHA":      "dc802804100637a589fabce1cb79ff13a1411302",
	"SETUP_QEMU_ACTION_VERSION":       "v4.2.0",
	"SETUP_QEMU_ACTION_SHA":           "96fe6ef7f33517b61c61be40b68a1882f3264fb8",
	"QEMU_BINFMT_IMAGE":               "docker.io/tonistiigi/binfmt@sha256:400a4873b838d1b89194d982c45e5fb3cda4593fbfd7e08a02e76b03b21166f0",
	"SETUP_BUILDX_ACTION_VERSION":     "v4.2.0",
	"SETUP_BUILDX_ACTION_SHA":         "bb05f3f5519dd87d3ba754cc423b652a5edd6d2c",
	"DOCKER_BUILD_PUSH_ACTION_SHA":    "53b7df96c91f9c12dcc8a07bcb9ccacbed38856a",
	"SOFTPROPS_ACTION_GH_RELEASE_SHA": "3d0d9888cb7fd7b750713d6e236d1fcb99157228",
	"BUILDX_VERSION":                  "v0.34.1",
	"BUILDKIT_IMAGE":                  "docker.io/moby/buildkit:v0.30.0@sha256:0168606be2315b7c807a03b3d8aa79beefdb31c98740cebdffdfeebf31190c9f",
}

type target struct {
	goos   string
	goarch string
}

func (t target) String() string { return t.goos + "/" + t.goarch }

type releaseArtifact struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Goos   string `json:"goos"`
	Goarch string `json:"goarch"`
	Type   string `json:"type"`
}

type archiveEntry struct {
	mode   int64
	size   int64
	digest string
}

type commandResult struct {
	ExitCode int
	Stdout   string
	Stderr   string
}

func TestToolVersionManifestIsExact(t *testing.T) {
	got := toolVersions(t)
	if !reflect.DeepEqual(got, expectedToolVersions) {
		t.Fatalf("tool versions differ\n got: %#v\nwant: %#v", got, expectedToolVersions)
	}
}

func TestPackagingToolchainMatchesManifest(t *testing.T) {
	if _, err := exec.LookPath("goreleaser"); err != nil {
		t.Skip("goreleaser is not installed")
	}
	if _, err := exec.LookPath("syft"); err != nil {
		t.Skip("syft is not installed")
	}
	versions := toolVersions(t)
	if got := strings.TrimSpace(runOutput(t, projectRoot(t), "go", "env", "GOVERSION")); got != "go"+versions["GO_VERSION"] {
		t.Fatalf("Go version = %q, want %q", got, "go"+versions["GO_VERSION"])
	}
	assertCommandMentionsVersion(t, "goreleaser", []string{"--version"}, versions["GORELEASER_VERSION"])
	assertCommandMentionsVersion(t, "syft", []string{"version"}, versions["SYFT_VERSION"])
}

func TestArchivePrerequisiteFailuresAreActionable(t *testing.T) {
	tests := []struct {
		name  string
		tools prerequisiteTools
		want  []string
	}{
		{name: "missing git", want: []string{"Git 2.38", "install"}},
		{
			name:  "old git",
			tools: prerequisiteTools{gitOutput: "git version 2.37.9"},
			want:  []string{"Git 2.38", "2.37.9", "upgrade"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := runArchive(t, test.tools)
			if got.ExitCode != 1 || got.Stdout != "" {
				t.Fatalf("result=%+v", got)
			}
			for _, want := range test.want {
				if !strings.Contains(got.Stderr, want) {
					t.Fatalf("stderr %q does not contain %q", got.Stderr, want)
				}
			}
		})
	}
}

// The archive is what an operator actually installs, so the released binary
// itself has to start on a machine that only has git.
func TestTheReleasedBinaryStartsWithGitAsItsOnlyInstalledTool(t *testing.T) {
	got := runArchive(t, prerequisiteTools{git: realGit(t)})
	if strings.Contains(got.Stderr, "codebase-memory-mcp") {
		t.Fatalf("released binary still demands the repository indexer: %+v", got)
	}
	if strings.Contains(got.Stderr, "check prerequisites") {
		t.Fatalf("released binary failed a prerequisite it should not have: %+v", got)
	}
}

func TestArchivesContainExactlyFourSupportedTargets(t *testing.T) {
	archives := archiveArtifacts(t)
	if len(archives) != len(supportedTargets) {
		t.Fatalf("archive count = %d, want %d: %#v", len(archives), len(supportedTargets), archives)
	}

	remaining := make(map[string]bool, len(supportedTargets))
	for _, target := range supportedTargets {
		remaining[target.String()] = true
	}
	for _, artifact := range archives {
		key := target{goos: artifact.Goos, goarch: artifact.Goarch}.String()
		if !remaining[key] {
			t.Fatalf("unexpected or duplicate archive target %q", key)
		}
		delete(remaining, key)
	}
	if len(remaining) != 0 {
		t.Fatalf("missing archive targets: %#v", remaining)
	}
}

// An archive member is a binary an operator ends up trusting and keeping on
// PATH, so the set is pinned exactly rather than merely checked for what it
// must contain.
func TestArchivesShipOpsPilotAndItsDocumentationAndNothingElse(t *testing.T) {
	archives := archiveArtifacts(t)
	byTarget := make(map[string]releaseArtifact, len(archives))
	for _, artifact := range archives {
		byTarget[target{goos: artifact.Goos, goarch: artifact.Goarch}.String()] = artifact
	}

	for _, target := range supportedTargets {
		t.Run(target.String(), func(t *testing.T) {
			artifact := byTarget[target.String()]
			got := archiveEntries(t, artifactFile(t, artifact))
			want := []string{
				"ops-pilot", "README.md",
				"docs/install.md", "docs/configuration.md", "docs/cli.md",
			}
			if len(got) != len(want) {
				t.Fatalf("archive members = %v, want exactly %v", sortedKeys(got), want)
			}
			for _, name := range want {
				entry, ok := got[name]
				if !ok || entry.size == 0 {
					t.Fatalf("archive entry %q = %+v, present=%t", name, entry, ok)
				}
			}
			if got["ops-pilot"].mode&0o111 == 0 {
				t.Fatalf("ops-pilot mode = %#o, want executable", got["ops-pilot"].mode)
			}
		})
	}
}

func TestArchivesAreChecksummedAndHaveOneSBOMEach(t *testing.T) {
	artifacts := releaseArtifacts(t)
	archives := artifactsOfType(artifacts, "Archive")
	checksums := artifactsOfType(artifacts, "Checksum")
	sboms := artifactsOfType(artifacts, "SBOM")
	if len(checksums) != 1 {
		t.Fatalf("checksum artifacts = %d, want 1", len(checksums))
	}
	if len(sboms) != len(archives) {
		t.Fatalf("SBOM artifacts = %d, want %d", len(sboms), len(archives))
	}

	checksumMembers := parseChecksums(t, artifactFile(t, checksums[0]))
	wantMembers := make(map[string]string, len(archives))
	for _, artifact := range archives {
		data, err := os.ReadFile(artifactFile(t, artifact))
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(data)
		wantMembers[filepath.Base(artifactFile(t, artifact))] = hex.EncodeToString(sum[:])
	}
	if !reflect.DeepEqual(checksumMembers, wantMembers) {
		t.Fatalf("checksum members = %#v, want %#v", checksumMembers, wantMembers)
	}

	sbomNames := make(map[string]bool, len(sboms))
	for _, artifact := range sboms {
		sbomNames[artifact.Name] = true
		var document struct {
			SPDXVersion string `json:"spdxVersion"`
			Packages    []any  `json:"packages"`
		}
		data, err := os.ReadFile(artifactFile(t, artifact))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(data, &document); err != nil {
			t.Fatalf("decode SBOM %q: %v", artifact.Name, err)
		}
		if !strings.HasPrefix(document.SPDXVersion, "SPDX-") || len(document.Packages) == 0 {
			t.Fatalf("SBOM %q is incomplete", artifact.Name)
		}
	}
	for _, archive := range archives {
		if !sbomNames[archive.Name+".sbom.json"] {
			t.Fatalf("archive %q has no matching SBOM", archive.Name)
		}
	}
}

func TestArchiveBinariesAreStaticAndCarryCommitDerivedVersionData(t *testing.T) {
	root := projectRoot(t)
	commit := strings.TrimSpace(runOutput(t, root, "git", "rev-parse", "HEAD"))
	epochText := strings.TrimSpace(runOutput(t, root, "git", "show", "-s", "--format=%ct", "HEAD"))
	epoch, err := strconv.ParseInt(epochText, 10, 64)
	if err != nil {
		t.Fatalf("parse commit epoch %q: %v", epochText, err)
	}
	commitDate := time.Unix(epoch, 0).UTC().Format(time.RFC3339)

	archives := archiveArtifacts(t)
	native := nativeArchive(t, archives)
	nativeBinary := extractArchiveEntry(t, artifactFile(t, native), "ops-pilot")
	versionOutput := strings.TrimSpace(runOutput(t, t.TempDir(), nativeBinary, "version"))
	version, ok := strings.CutPrefix(versionOutput, "ops-pilot ")
	if !ok || version == "" || version == "dev" {
		t.Fatalf("version output = %q", versionOutput)
	}

	for _, artifact := range archives {
		t.Run(target{goos: artifact.Goos, goarch: artifact.Goarch}.String(), func(t *testing.T) {
			binary := readArchiveEntry(t, artifactFile(t, artifact), "ops-pilot")
			for _, value := range []string{version, commit, commitDate} {
				if !bytes.Contains(binary, []byte(value)) {
					t.Fatalf("binary does not contain injected value %q", value)
				}
			}
			if artifact.Goos == "linux" {
				path := extractArchiveEntry(t, artifactFile(t, artifact), "ops-pilot")
				file, err := elf.Open(path)
				if err != nil {
					t.Fatalf("open Linux ELF: %v", err)
				}
				defer file.Close()
				if file.Section(".dynamic") != nil {
					t.Fatal("Linux binary has a dynamic section")
				}
				for _, program := range file.Progs {
					if program.Type == elf.PT_INTERP {
						t.Fatal("Linux binary has a dynamic interpreter")
					}
				}
			}
		})
	}
}

func TestHomebrewCaskIsReleaseLocalAndInstallsOpsPilotAlone(t *testing.T) {
	casks := artifactsOfType(releaseArtifacts(t), "Homebrew Cask")
	if len(casks) != 1 || casks[0].Name != "ops-pilot.rb" {
		t.Fatalf("cask artifacts = %#v", casks)
	}
	data, err := os.ReadFile(artifactFile(t, casks[0]))
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{
		`binary "ops-pilot"`,
		`depends_on formula: "git"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("cask does not contain %q", want)
		}
	}
	if strings.Count(text, "binary \"") != 1 {
		t.Fatalf("cask installs more than the ops-pilot binary:\n%s", text)
	}
	for _, forbidden := range []string{"tap ", "HOMEBREW_GITHUB_API_TOKEN", "repository:"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("cask contains forbidden tap/upload surface %q", forbidden)
		}
	}
}

func TestHomebrewCaskConfigurationIsReleaseLocal(t *testing.T) {
	var config struct {
		HomebrewCasks []struct {
			Name       string `yaml:"name"`
			Repository struct {
				Owner string `yaml:"owner"`
				Name  string `yaml:"name"`
			} `yaml:"repository"`
			Binaries    []string `yaml:"binaries"`
			CustomBlock string   `yaml:"custom_block"`
			SkipUpload  bool     `yaml:"skip_upload"`
		} `yaml:"homebrew_casks"`
	}

	data, err := os.ReadFile(filepath.Join(projectRoot(t), ".goreleaser.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := yaml.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if len(config.HomebrewCasks) != 1 {
		t.Fatalf("homebrew_casks = %#v", config.HomebrewCasks)
	}
	cask := config.HomebrewCasks[0]
	if cask.Name != "ops-pilot" || cask.Repository.Owner != "lkshrk" || cask.Repository.Name != "homebrew-tap" || !cask.SkipUpload {
		t.Fatalf("cask config = %#v", cask)
	}
	rawCasks, ok := raw["homebrew_casks"].([]any)
	if !ok || len(rawCasks) != 1 {
		t.Fatalf("raw homebrew_casks = %#v", raw["homebrew_casks"])
	}
	rawCask, ok := rawCasks[0].(map[string]any)
	if !ok {
		t.Fatalf("raw cask = %#v", rawCasks[0])
	}
	repository, ok := rawCask["repository"].(map[string]any)
	if !ok || !reflect.DeepEqual(repository, map[string]any{"owner": "lkshrk", "name": "homebrew-tap"}) {
		t.Fatalf("cask repository = %#v", rawCask["repository"])
	}
	for _, key := range []string{"tap", "branch", "token", "token_type", "git", "pull_request"} {
		if _, exists := rawCask[key]; exists {
			t.Fatalf("cask contains forbidden %q: %#v", key, rawCask)
		}
	}
	if !reflect.DeepEqual(cask.Binaries, []string{"ops-pilot"}) || cask.CustomBlock != "depends_on formula: \"git\"\n" {
		t.Fatalf("cask config = %#v", cask)
	}
}

func TestHomebrewCaskInstallsFromLocalRelease(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("requires macOS Homebrew")
	}
	if os.Getenv("OPS_PILOT_REQUIRE_CASK_SMOKE") != "1" {
		t.Skip("set OPS_PILOT_REQUIRE_CASK_SMOKE=1 to run the isolated Homebrew install smoke")
	}
	brew, err := exec.LookPath("brew")
	if err != nil {
		t.Fatal("Homebrew is required when OPS_PILOT_REQUIRE_CASK_SMOKE=1")
	}

	cask := artifactsOfType(releaseArtifacts(t), "Homebrew Cask")
	if len(cask) != 1 {
		t.Fatalf("cask artifacts = %#v", cask)
	}
	archive := nativeArchive(t, archiveArtifacts(t))
	archivePath := artifactFile(t, archive)
	server := httptest.NewServer(http.FileServer(http.Dir(filepath.Dir(archivePath))))
	defer server.Close()
	response, err := http.Get(server.URL + "/" + filepath.Base(archivePath))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("local archive status = %s", response.Status)
	}
	wantFile, err := os.Open(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer wantFile.Close()
	want, got := sha256.New(), sha256.New()
	if _, err := io.Copy(want, wantFile); err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(got, response.Body); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(want.Sum(nil), got.Sum(nil)) {
		t.Fatal("local archive checksum differs from release artifact")
	}

	original, err := os.ReadFile(artifactFile(t, cask[0]))
	if err != nil {
		t.Fatal(err)
	}
	localCask := regexp.MustCompile(`(?m)^(\s*url\s+)"[^"]+"`).ReplaceAllString(
		string(original),
		`${1}"`+server.URL+`/`+filepath.Base(archivePath)+`"`,
	)
	if localCask == string(original) {
		t.Fatal("generated cask has no URL to replace for local smoke")
	}

	prefix := filepath.Join(t.TempDir(), "homebrew")
	if err := os.MkdirAll(filepath.Join(prefix, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	linkExecutable(t, realGit(t), filepath.Join(prefix, "bin", "git"))
	brewCopy := filepath.Join(prefix, "bin", "brew")
	brewSource, err := os.ReadFile(brew)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(brewCopy, brewSource, 0o755); err != nil {
		t.Fatal(err)
	}
	repository := strings.TrimSpace(runOutput(t, t.TempDir(), brew, "--repository"))
	if err := os.MkdirAll(filepath.Join(prefix, "Library"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(repository, "Library", "Homebrew"), filepath.Join(prefix, "Library", "Homebrew")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(prefix, "Library", "Taps"), 0o755); err != nil {
		t.Fatal(err)
	}

	root := t.TempDir()
	for _, directory := range []string{"home", "cache", "logs", "temp", "apps"} {
		if err := os.MkdirAll(filepath.Join(root, directory), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	tapRoot := filepath.Join(prefix, "Library", "Taps", "lkshrk", "homebrew-ops-pilot")
	localCaskPath := filepath.Join(tapRoot, "Casks", "ops-pilot.rb")
	if err := os.MkdirAll(filepath.Dir(localCaskPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(localCaskPath, []byte(localCask), 0o600); err != nil {
		t.Fatal(err)
	}
	env := commandEnvWithout(os.Environ(),
		"HOME", "HOMEBREW_CACHE", "HOMEBREW_LOGS", "HOMEBREW_TEMP", "HOMEBREW_NO_AUTO_UPDATE",
		"HOMEBREW_NO_ANALYTICS", "HOMEBREW_NO_ENV_HINTS", "HOMEBREW_NO_ASK", "HOMEBREW_CASK_OPTS",
	)
	env = append(env,
		"HOME="+filepath.Join(root, "home"),
		"HOMEBREW_CACHE="+filepath.Join(root, "cache"),
		"HOMEBREW_LOGS="+filepath.Join(root, "logs"),
		"HOMEBREW_TEMP="+filepath.Join(root, "temp"),
		"HOMEBREW_NO_AUTO_UPDATE=1",
		"HOMEBREW_NO_ANALYTICS=1",
		"HOMEBREW_NO_ENV_HINTS=1",
		"HOMEBREW_NO_ASK=1",
		"HOMEBREW_CASK_OPTS=--appdir="+filepath.Join(root, "apps"),
	)
	if _, err := os.Stat(filepath.Join(tapRoot, ".git")); !os.IsNotExist(err) {
		t.Fatalf("temporary cask tap has git state: %v", err)
	}
	installContext, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	runOutputEnvContext(t, installContext, env, prefix, brewCopy, "install", "--cask", "--skip-cask-deps", "lkshrk/ops-pilot/ops-pilot")
	if entries, err := os.ReadDir(filepath.Join(prefix, "Library", "Taps")); err != nil || len(entries) != 1 || entries[0].Name() != "lkshrk" {
		t.Fatalf("temporary tap owners = %#v, err=%v", entries, err)
	}
	if entries, err := os.ReadDir(filepath.Join(prefix, "Library", "Taps", "lkshrk")); err != nil || len(entries) != 1 || entries[0].Name() != "homebrew-ops-pilot" {
		t.Fatalf("temporary taps = %#v, err=%v", entries, err)
	}
	after, err := os.ReadFile(artifactFile(t, cask[0]))
	if err != nil || !bytes.Equal(after, original) {
		t.Fatalf("generated cask changed during smoke: %v", err)
	}

	git := filepath.Join(prefix, "bin", "git")
	gitVersion := strings.TrimSpace(runOutput(t, root, git, "--version"))
	gitParts := regexp.MustCompile(`^git version ([0-9]+)\.([0-9]+)`).FindStringSubmatch(gitVersion)
	if len(gitParts) != 3 {
		t.Fatalf("git version = %q", gitVersion)
	}
	major, _ := strconv.Atoi(gitParts[1])
	minor, _ := strconv.Atoi(gitParts[2])
	if major < 2 || major == 2 && minor < 38 {
		t.Fatalf("git version = %q, want >= 2.38", gitVersion)
	}
	if got := strings.TrimSpace(runOutput(t, root, filepath.Join(prefix, "bin", "ops-pilot"), "version")); !strings.HasPrefix(got, "ops-pilot ") {
		t.Fatalf("ops-pilot version = %q", got)
	}
}

type prerequisiteTools struct {
	git       string
	gitOutput string
}

func runArchive(t *testing.T, tools prerequisiteTools) commandResult {
	t.Helper()
	archive := nativeArchive(t, archiveArtifacts(t))
	binary := extractArchiveEntry(t, artifactFile(t, archive), "ops-pilot")
	binDirectory := t.TempDir()
	if tools.git != "" {
		linkExecutable(t, tools.git, filepath.Join(binDirectory, "git"))
	} else if tools.gitOutput != "" {
		writeVersionExecutable(t, filepath.Join(binDirectory, "git"), tools.gitOutput)
	}

	configData, err := os.ReadFile(filepath.Join(projectRoot(t), "configs", "ops-pilot.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "ops-pilot.yaml")
	if err := os.WriteFile(configPath, configData, 0o600); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(binary, "analyze", "--config", configPath, "1")
	command.Dir = t.TempDir()
	command.Env = []string{
		"HOME=" + t.TempDir(),
		"PATH=" + binDirectory,
		"TMPDIR=" + t.TempDir(),
	}
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err = command.Run()
	if err == nil {
		return commandResult{Stdout: stdout.String(), Stderr: stderr.String()}
	}
	exit, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("run archive: %v", err)
	}
	return commandResult{
		ExitCode: exit.ExitCode(),
		Stdout:   stdout.String(),
		Stderr:   stderr.String(),
	}
}

func nativeArchive(t *testing.T, archives []releaseArtifact) releaseArtifact {
	t.Helper()
	for _, artifact := range archives {
		if artifact.Goos == runtime.GOOS && artifact.Goarch == runtime.GOARCH {
			return artifact
		}
	}
	t.Fatalf("no native archive for %s/%s", runtime.GOOS, runtime.GOARCH)
	return releaseArtifact{}
}

func archiveArtifacts(t *testing.T) []releaseArtifact {
	t.Helper()
	return artifactsOfType(releaseArtifacts(t), "Archive")
}

func artifactsOfType(artifacts []releaseArtifact, artifactType string) []releaseArtifact {
	var selected []releaseArtifact
	for _, artifact := range artifacts {
		if artifact.Type == artifactType {
			selected = append(selected, artifact)
		}
	}
	sort.Slice(selected, func(i, j int) bool { return selected[i].Name < selected[j].Name })
	return selected
}

func releaseArtifacts(t *testing.T) []releaseArtifact {
	t.Helper()
	path := filepath.Join(projectRoot(t), "dist", "artifacts.json")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		t.Skipf("release artifacts are not present: %s", path)
	}
	if err != nil {
		t.Fatalf("packaging artifact %q is missing or unreadable: %v", path, err)
	}
	var artifacts []releaseArtifact
	if err := json.Unmarshal(data, &artifacts); err != nil {
		t.Fatalf("decode %q: %v", path, err)
	}
	return artifacts
}

func artifactFile(t *testing.T, artifact releaseArtifact) string {
	t.Helper()
	if filepath.IsAbs(artifact.Path) {
		return artifact.Path
	}
	return filepath.Join(projectRoot(t), filepath.FromSlash(artifact.Path))
}

func archiveEntries(t *testing.T, filename string) map[string]archiveEntry {
	t.Helper()
	file, err := os.Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	return tarGzipEntries(t, file)
}

func tarGzipEntries(t *testing.T, input io.Reader) map[string]archiveEntry {
	t.Helper()
	gzipReader, err := gzip.NewReader(input)
	if err != nil {
		t.Fatal(err)
	}
	defer gzipReader.Close()
	reader := tar.NewReader(gzipReader)
	entries := map[string]archiveEntry{}
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return entries
		}
		if err != nil {
			t.Fatal(err)
		}
		name := path.Clean(header.Name)
		if name != header.Name || name == "." || path.IsAbs(name) || strings.HasPrefix(name, "../") {
			t.Fatalf("unsafe archive path %q", header.Name)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			continue
		case tar.TypeReg, tar.TypeRegA:
		default:
			t.Fatalf("unsupported archive entry %q type %d", name, header.Typeflag)
		}
		if _, exists := entries[name]; exists {
			t.Fatalf("duplicate archive entry %q", name)
		}
		hash := sha256.New()
		size, err := io.Copy(hash, reader)
		if err != nil {
			t.Fatal(err)
		}
		entries[name] = archiveEntry{
			mode:   header.Mode,
			size:   size,
			digest: hex.EncodeToString(hash.Sum(nil)),
		}
	}
}

func readArchiveEntry(t *testing.T, filename, entryName string) []byte {
	t.Helper()
	file, err := os.Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		t.Fatal(err)
	}
	defer gzipReader.Close()
	reader := tar.NewReader(gzipReader)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			t.Fatalf("archive entry %q not found", entryName)
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == entryName {
			const maxBinarySize = 512 << 20
			if header.Size < 0 || header.Size > maxBinarySize {
				t.Fatalf("archive entry %q size = %d", entryName, header.Size)
			}
			data, err := io.ReadAll(io.LimitReader(reader, maxBinarySize+1))
			if err != nil {
				t.Fatal(err)
			}
			if int64(len(data)) != header.Size {
				t.Fatalf("archive entry %q size = %d, read %d", entryName, header.Size, len(data))
			}
			return data
		}
	}
}

func extractArchiveEntry(t *testing.T, filename, entryName string) string {
	t.Helper()
	data := readArchiveEntry(t, filename, entryName)
	output := filepath.Join(t.TempDir(), filepath.Base(entryName))
	if err := os.WriteFile(output, data, 0o700); err != nil {
		t.Fatal(err)
	}
	return output
}

func sortedKeys(entries map[string]archiveEntry) []string {
	names := make([]string, 0, len(entries))
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func parseChecksums(t *testing.T, filename string) map[string]string {
	t.Helper()
	file, err := os.Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	got := map[string]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(fields[0]) {
			t.Fatalf("invalid checksum line %q", scanner.Text())
		}
		name := strings.TrimPrefix(fields[1], "*")
		if filepath.Base(name) != name {
			t.Fatalf("checksum member %q is not a basename", name)
		}
		if _, exists := got[name]; exists {
			t.Fatalf("duplicate checksum member %q", name)
		}
		got[name] = fields[0]
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return got
}

func toolVersions(t *testing.T) map[string]string {
	t.Helper()
	filename := filepath.Join(projectRoot(t), "build", "tool-versions.env")
	file, err := os.Open(filename)
	if err != nil {
		t.Fatalf("read tool manifest %q: %v", filename, err)
	}
	defer file.Close()
	linePattern := regexp.MustCompile(`^([A-Z][A-Z0-9_]*)=([A-Za-z0-9._:/@+-]+)$`)
	got := map[string]string{}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		match := linePattern.FindStringSubmatch(scanner.Text())
		if match == nil {
			t.Fatalf("invalid tool manifest line %q", scanner.Text())
		}
		if _, exists := got[match[1]]; exists {
			t.Fatalf("duplicate tool manifest key %q", match[1])
		}
		got[match[1]] = match[2]
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return got
}

func projectRoot(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(filepath.Join(directory, "..", ".."))
}

func runOutput(t *testing.T, directory, name string, args ...string) string {
	t.Helper()
	command := exec.Command(name, args...)
	command.Dir = directory
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, output)
	}
	return string(output)
}

func runOutputEnv(t *testing.T, environment []string, directory, name string, args ...string) string {
	return runOutputEnvContext(t, context.Background(), environment, directory, name, args...)
}

func runOutputEnvContext(t *testing.T, ctx context.Context, environment []string, directory, name string, args ...string) string {
	t.Helper()
	command := exec.CommandContext(ctx, name, args...)
	command.Dir = directory
	command.Env = environment
	command.WaitDelay = 10 * time.Second
	output, err := command.CombinedOutput()
	if err != nil {
		if ctx.Err() != nil {
			t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), ctx.Err(), output)
		}
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, output)
	}
	return string(output)
}

func commandEnvWithout(environment []string, names ...string) []string {
	excluded := make(map[string]bool, len(names))
	for _, name := range names {
		excluded[name] = true
	}
	result := make([]string, 0, len(environment))
	for _, value := range environment {
		name, _, _ := strings.Cut(value, "=")
		if !excluded[name] {
			result = append(result, value)
		}
	}
	return result
}

func assertCommandMentionsVersion(t *testing.T, name string, args []string, want string) {
	t.Helper()
	output := runOutput(t, projectRoot(t), name, args...)
	want = strings.TrimPrefix(want, "v")
	pattern := regexp.MustCompile(`(^|[^0-9])` + regexp.QuoteMeta(want) + `([^0-9]|$)`)
	if !pattern.MatchString(output) {
		t.Fatalf("%s version output %q does not contain exact version %q", name, output, want)
	}
}

func realGit(t *testing.T) string {
	t.Helper()
	executable, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	return executable
}

func linkExecutable(t *testing.T, source, destination string) {
	t.Helper()
	if err := os.Symlink(source, destination); err != nil {
		t.Fatal(err)
	}
}

func writeVersionExecutable(t *testing.T, filename, output string) {
	t.Helper()
	script := "#!/bin/sh\nprintf '%s\\n' " + shellQuote(output) + "\n"
	if err := os.WriteFile(filename, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
