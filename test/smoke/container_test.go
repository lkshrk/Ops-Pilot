package smoke_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

type containerRuntime struct {
	Target           string
	UID              int
	GitVersion       string
	HasGoToolchain   bool
	StateWritable    bool
	CheckoutWritable bool
	CacheWritable    bool
}

func TestContainerRuntimeIsMinimal(t *testing.T) {
	dockerfile := filepath.Join(projectRoot(t), "Dockerfile")
	if _, err := os.Stat(dockerfile); err != nil {
		t.Fatalf("packaging artifact %q is missing or unreadable: %v", dockerfile, err)
	}

	for _, target := range []string{"amd64", "arm64"} {
		t.Run(target, func(t *testing.T) {
			got := inspectBuiltContainer(t, target)
			if got.UID == 0 ||
				versionLessThan(got.GitVersion, "2.38.0") ||
				got.HasGoToolchain ||
				!got.StateWritable ||
				!got.CheckoutWritable ||
				!got.CacheWritable {
				t.Fatalf("runtime=%+v", got)
			}
		})
	}
}

func TestDockerfileUsesPinnedBaseImages(t *testing.T) {
	versions := toolVersions(t)
	filename := filepath.Join(projectRoot(t), "Dockerfile")
	data, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	var images []string
	scanner := regexp.MustCompile(`(?m)^FROM[ \t]+([^ \t]+)`).FindAllSubmatch(data, -1)
	for _, match := range scanner {
		images = append(images, string(match[1]))
	}
	want := []string{versions["GO_BUILDER_IMAGE"], versions["RUNTIME_IMAGE"]}
	if len(images) != len(want) || images[0] != want[0] || images[1] != want[1] {
		t.Fatalf("Dockerfile FROM images = %#v, want %#v", images, want)
	}
}

func TestContainerBuildRejectsUnknownArchitecture(t *testing.T) {
	if os.Getenv("OPS_PILOT_SMOKE_BUILD") != "1" {
		t.Skip("set OPS_PILOT_SMOKE_BUILD=1 to run destructive container builds")
	}
	requireDockerfile(t)
	output, exitCode := runDockerBuild(
		t,
		"linux/amd64",
		"--build-arg",
		"TARGETARCH=386",
	)
	if exitCode == 0 || !strings.Contains(output, "unsupported TARGETARCH") {
		t.Fatalf("unknown-architecture build exit=%d output=%q", exitCode, output)
	}
}

func inspectBuiltContainer(t *testing.T, architecture string) containerRuntime {
	t.Helper()
	target := "linux/" + architecture
	image := smokeImage(t, architecture)

	uidText := strings.TrimSpace(dockerRun(t, target, image, nil, "/usr/bin/id", "-u"))
	uid, err := strconv.Atoi(uidText)
	if err != nil {
		t.Fatalf("container UID %q: %v", uidText, err)
	}
	gitOutput := strings.TrimSpace(dockerRun(t, target, image, nil, "/usr/bin/git", "--version"))
	gitVersion := strings.TrimPrefix(gitOutput, "git version ")

	hasGo, err := dockerCommandSucceeds(
		target,
		image,
		nil,
		"/bin/sh",
		"-ceu",
		"command -v go >/dev/null || test -e /usr/local/go/bin/go",
	)
	if err != nil {
		t.Fatal(err)
	}

	state, checkout, cache := t.TempDir(), t.TempDir(), t.TempDir()
	for _, dir := range []string{state, checkout, cache} {
		if err := os.Chmod(dir, 0o777); err != nil {
			t.Fatal(err)
		}
	}
	mounts := []string{
		state + ":/state",
		checkout + ":/checkout",
		cache + ":/cache",
	}
	dockerRun(
		t,
		target,
		image,
		mounts,
		"/bin/sh",
		"-ceu",
		`test -s /usr/share/doc/git/copyright
touch /state/smoke /checkout/smoke /cache/smoke`,
	)

	return containerRuntime{
		Target:           target,
		UID:              uid,
		GitVersion:       gitVersion,
		HasGoToolchain:   hasGo,
		StateWritable:    regularFile(t, filepath.Join(state, "smoke")),
		CheckoutWritable: regularFile(t, filepath.Join(checkout, "smoke")),
		CacheWritable:    regularFile(t, filepath.Join(cache, "smoke")),
	}
}

func smokeImage(t *testing.T, architecture string) string {
	t.Helper()
	if image := os.Getenv("OPS_PILOT_SMOKE_IMAGE_" + strings.ToUpper(architecture)); image != "" {
		return image
	}
	if architecture != "amd64" {
		t.Skip("set OPS_PILOT_SMOKE_IMAGE_ARM64 to inspect an arm64 image")
	}
	image := "ops-pilot:test"
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("no prepared container image")
	}
	output, err := exec.Command("docker", "image", "inspect", image).CombinedOutput()
	if err != nil {
		message := strings.ToLower(string(output))
		if strings.Contains(message, "no such image") || strings.Contains(message, "no such object") {
			t.Skipf("no prepared container image %q", image)
		}
		t.Fatalf("inspect prepared container image %q: %v: %s", image, err, strings.TrimSpace(string(output)))
	}
	return image
}

func dockerRun(
	t *testing.T,
	platform, image string,
	mounts []string,
	entrypoint string,
	args ...string,
) string {
	t.Helper()
	commandArgs := []string{
		"run", "--rm", "--pull=never", "--platform", platform,
		"--entrypoint", entrypoint,
	}
	for _, mount := range mounts {
		commandArgs = append(commandArgs, "--volume", mount)
	}
	commandArgs = append(commandArgs, image)
	commandArgs = append(commandArgs, args...)
	return runOutput(t, projectRoot(t), "docker", commandArgs...)
}

func dockerCommandSucceeds(
	platform, image string,
	mounts []string,
	entrypoint string,
	args ...string,
) (bool, error) {
	commandArgs := []string{
		"run", "--rm", "--pull=never", "--platform", platform,
		"--entrypoint", entrypoint,
	}
	for _, mount := range mounts {
		commandArgs = append(commandArgs, "--volume", mount)
	}
	commandArgs = append(commandArgs, image)
	commandArgs = append(commandArgs, args...)
	command := exec.Command("docker", commandArgs...)
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	err := command.Run()
	if err == nil {
		return true, nil
	}
	exit, ok := err.(*exec.ExitError)
	if ok && exit.ExitCode() == 1 {
		return false, nil
	}
	return false, fmt.Errorf("docker %s: %w\n%s", strings.Join(commandArgs, " "), err, output.String())
}

func runDockerBuild(t *testing.T, platform string, extra ...string) (string, int) {
	t.Helper()
	args := []string{
		"buildx", "build",
		"--platform", platform,
		"--progress=plain",
		"--output", "type=cacheonly",
	}
	args = append(args, extra...)
	args = append(args, projectRoot(t))
	command := exec.Command("docker", args...)
	command.Dir = projectRoot(t)
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	err := command.Run()
	if err == nil {
		return output.String(), 0
	}
	exit, ok := err.(*exec.ExitError)
	if !ok {
		t.Fatalf("docker buildx: %v\n%s", err, output.String())
	}
	return output.String(), exit.ExitCode()
}

func requireDockerfile(t *testing.T) {
	t.Helper()
	filename := filepath.Join(projectRoot(t), "Dockerfile")
	if _, err := os.Stat(filename); err != nil {
		t.Fatalf("packaging artifact %q is missing or unreadable: %v", filename, err)
	}
}

func regularFile(t *testing.T, filename string) bool {
	t.Helper()
	info, err := os.Stat(filename)
	if err != nil {
		t.Logf("stat %q: %v", filename, err)
		return false
	}
	return info.Mode().IsRegular()
}

func versionLessThan(got, minimum string) bool {
	parse := func(value string) ([3]int, bool) {
		match := regexp.MustCompile(`^([0-9]+)\.([0-9]+)(?:\.([0-9]+))?`).FindStringSubmatch(value)
		if match == nil {
			return [3]int{}, false
		}
		var version [3]int
		for i := range version {
			if match[i+1] == "" {
				continue
			}
			number, err := strconv.Atoi(match[i+1])
			if err != nil {
				return [3]int{}, false
			}
			version[i] = number
		}
		return version, true
	}
	gotVersion, gotOK := parse(got)
	minimumVersion, minimumOK := parse(minimum)
	if !gotOK || !minimumOK {
		return true
	}
	for i := range gotVersion {
		if gotVersion[i] != minimumVersion[i] {
			return gotVersion[i] < minimumVersion[i]
		}
	}
	return false
}
