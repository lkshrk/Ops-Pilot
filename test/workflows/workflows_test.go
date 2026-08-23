package workflows_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var goTestTarget = regexp.MustCompile(`(^|\s)\./[A-Za-z0-9._/-]+`)

func TestEveryWorkflowGoTestTargetHoldsGoFiles(t *testing.T) {
	root := filepath.Join("..", "..")
	workflows, err := filepath.Glob(filepath.Join(root, ".github", "workflows", "*.y*ml"))
	if err != nil {
		t.Fatalf("list the workflows: %v", err)
	}
	if len(workflows) == 0 {
		t.Fatal("want the workflows, found none")
	}
	for _, workflow := range workflows {
		contents, err := os.ReadFile(workflow)
		if err != nil {
			t.Fatalf("read %s: %v", workflow, err)
		}
		for _, msg := range findBadGoTestTargets(root, filepath.Base(workflow), string(contents)) {
			t.Error(msg)
		}
	}
}

func TestFindBadGoTestTargetsJoinsLineContinuations(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "no-go-files"), 0o755); err != nil {
		t.Fatalf("create empty dir: %v", err)
	}

	tests := []struct {
		name     string
		contents string
		wantHits int
	}{
		{
			name:     "a backslash-continued command still checks its wrapped target",
			contents: "run: |\n  go test -race \\\n    ./no-go-files -count=1\n",
			wantHits: 1,
		},
		{
			name:     "a single-line command still checks its target",
			contents: "run: go test -race ./no-go-files\n",
			wantHits: 1,
		},
		{
			name:     "an ellipsis target is never flagged",
			contents: "run: go test ./...\n",
			wantHits: 0,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := findBadGoTestTargets(root, "workflow.yml", test.contents); len(got) != test.wantHits {
				t.Fatalf("findBadGoTestTargets(...) = %v, want %d hits", got, test.wantHits)
			}
		})
	}
}

// findBadGoTestTargets reports every `go test` target in contents that holds
// no Go files, joining backslash line continuations first so a wrapped target
// is not skipped for lacking "go test" on its own line.
func findBadGoTestTargets(root, workflowName, contents string) []string {
	var bad []string
	joined := strings.ReplaceAll(contents, "\\\n", " ")
	for line := range strings.SplitSeq(joined, "\n") {
		if !strings.Contains(line, "go test") {
			continue
		}
		for _, match := range goTestTarget.FindAllString(line, -1) {
			target := strings.TrimSpace(match)
			if strings.Contains(target, "...") {
				continue
			}
			if !holdsGoFiles(filepath.Join(root, target)) {
				bad = append(bad, workflowName+" runs go test against "+target+", which holds no Go files")
			}
		}
	}
	return bad
}

func holdsGoFiles(dir string) bool {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".go") {
			return true
		}
	}
	return false
}
