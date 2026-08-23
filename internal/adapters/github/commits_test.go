package github

import (
	"context"
	"encoding/base64"
	"net/http"
	"reflect"
	"strings"
	"testing"
)

func commitReply(oid string, parents ...string) map[string]any {
	nodes := make([]any, 0, len(parents))
	for _, parent := range parents {
		nodes = append(nodes, map[string]any{"oid": parent})
	}
	return map[string]any{
		"data": map[string]any{
			"createCommitOnBranch": map[string]any{
				"commit": map[string]any{
					"oid":     oid,
					"parents": map[string]any{"nodes": nodes},
				},
			},
		},
	}
}

// A commit is written through GitHub's mutation, so the path is data GitHub
// resolves rather than a file the adapter opens. A path that walks out of the
// repository or hides a newline has to be refused before it is sent, and the
// request count is what proves the refusal came first.
func TestACommitPathThatCouldEscapeOrSplitTheMutationIsRefusedBeforeItIsSent(t *testing.T) {
	tests := []struct {
		name    string
		changes []FileChange
	}{
		{name: "empty", changes: []FileChange{{Path: ""}}},
		{name: "absolute", changes: []FileChange{{Path: "/etc/passwd"}}},
		{name: "trailing slash", changes: []FileChange{{Path: "clusters/prod/"}}},
		{name: "empty segment", changes: []FileChange{{Path: "clusters//prod.yaml"}}},
		{name: "parent traversal", changes: []FileChange{{Path: "clusters/../../secrets.yaml"}}},
		{name: "leading parent traversal", changes: []FileChange{{Path: "../secrets.yaml"}}},
		{name: "bare parent", changes: []FileChange{{Path: ".."}}},
		{name: "current directory segment", changes: []FileChange{{Path: "clusters/./prod.yaml"}}},
		{name: "bare current directory", changes: []FileChange{{Path: "."}}},
		{name: "nul byte", changes: []FileChange{{Path: "clusters/prod.yaml\x00.txt"}}},
		{name: "newline", changes: []FileChange{{Path: "clusters/prod.yaml\nother.yaml"}}},
		{name: "carriage return", changes: []FileChange{{Path: "clusters/prod.yaml\rother.yaml"}}},
		{name: "over the length bound", changes: []FileChange{{Path: strings.Repeat("a", 4097)}}},
		{
			name:    "the same path twice",
			changes: []FileChange{{Path: "clusters/prod.yaml", Contents: []byte("a")}, {Path: "clusters/prod.yaml", Contents: []byte("b")}},
		},
		{
			name:    "written and deleted in one commit",
			changes: []FileChange{{Path: "clusters/prod.yaml", Contents: []byte("a")}, {Path: "clusters/prod.yaml", Delete: true}},
		},
		{
			name:    "a valid path alongside an invalid one",
			changes: []FileChange{{Path: "clusters/prod.yaml", Contents: []byte("a")}, {Path: "../escape.yaml", Contents: []byte("b")}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, requests := recording(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, commitReply("newoid", "headoid"))
			})
			client := newTestClient(t, handler, "main")

			sha, err := client.CreateCommit(context.Background(), "main", "headoid", "revert: podinfo", test.changes)
			if err == nil {
				t.Fatalf("CreateCommit returned %q, want a refusal", sha)
			}
			if sha != "" {
				t.Errorf("CreateCommit returned %q alongside an error", sha)
			}
			if seen := requests(); len(seen) != 0 {
				t.Fatalf("GitHub saw %d requests, want the refusal before anything was sent", len(seen))
			}
		})
	}
}

func TestACommitPathTheRepositoryActuallyUsesIsAccepted(t *testing.T) {
	tests := []struct {
		name string
		path string
	}{
		{name: "at the repository root", path: "prod.yaml"},
		{name: "nested", path: "clusters/prod/apps/podinfo.yaml"},
		{name: "a kustomization", path: "clusters/prod/kustomization.yaml"},
		{name: "a version in the file name", path: "charts/podinfo-1.2.3.tgz"},
		{name: "a segment opening with two dots", path: "clusters/..prod/app.yaml"},
		{name: "a hidden file", path: "clusters/prod/.sops.yaml"},
		{name: "a space in the file name", path: "clusters/prod/app v2.yaml"},
		{name: "on the length bound", path: strings.Repeat("a", 4096)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, _ := recording(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, commitReply("newoid", "headoid"))
			})
			client := newTestClient(t, handler, "main")

			sha, err := client.CreateCommit(context.Background(), "main", "headoid",
				"revert: podinfo", []FileChange{{Path: test.path, Contents: []byte("kind: Kustomization\n")}})
			if err != nil {
				t.Fatalf("CreateCommit: %v", err)
			}
			if sha != "newoid" {
				t.Fatalf("CreateCommit returned %q, want the new commit oid", sha)
			}
		})
	}
}

// expectedHeadOid is the whole concurrency control on the write path: the
// commit must sit on the head the caller read. A reply whose parent is some
// other commit means GitHub raced the caller, and reporting that oid as ours
// would leave the workflow watching and reverting the wrong commit.
func TestACommitWhoseParentIsNotTheHeadTheCallerExpectedIsRefused(t *testing.T) {
	tests := []struct {
		name  string
		reply map[string]any
	}{
		{name: "parent is another commit", reply: commitReply("newoid", "someone-elses-oid")},
		{name: "no parent at all", reply: commitReply("newoid")},
		{name: "two parents", reply: commitReply("newoid", "headoid", "otheroid")},
		{name: "two parents, ours second", reply: commitReply("newoid", "otheroid", "headoid")},
		{name: "no commit oid", reply: commitReply("", "headoid")},
		{
			name: "empty mutation payload",
			reply: map[string]any{"data": map[string]any{
				"createCommitOnBranch": map[string]any{},
			}},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, _ := recording(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, test.reply)
			})
			client := newTestClient(t, handler, "main")

			sha, err := client.CreateCommit(context.Background(), "main", "headoid",
				"revert: podinfo", []FileChange{{Path: "clusters/prod.yaml", Contents: []byte("a")}})
			if err == nil {
				t.Fatalf("CreateCommit returned %q, want a stale-reply refusal", sha)
			}
			if sha != "" {
				t.Errorf("CreateCommit returned %q alongside an error", sha)
			}
		})
	}
}

func TestACommitMutationCarriesSortedChangesBase64EncodedAndTheMessageSplit(t *testing.T) {
	handler, requests := recording(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, commitReply("newoid", "headoid"))
	})
	client := newTestClient(t, handler, "main")

	_, err := client.CreateCommit(context.Background(), "main", "headoid",
		"revert: update podinfo to 6.5.0\n\nreverts #42\ncluster did not recover", []FileChange{
			{Path: "clusters/prod/z.yaml", Contents: []byte("kind: Z\n")},
			{Path: "clusters/prod/gone.yaml", Delete: true},
			{Path: "clusters/prod/a.yaml", Contents: []byte("kind: A\n")},
			{Path: "clusters/prod/also-gone.yaml", Delete: true},
		})
	if err != nil {
		t.Fatalf("CreateCommit: %v", err)
	}

	seen := requests()
	if len(seen) != 1 {
		t.Fatalf("GitHub saw %d requests, want one", len(seen))
	}
	if seen[0].method != http.MethodPost || seen[0].path != "/graphql" {
		t.Fatalf("commit was %s %s, want POST /graphql", seen[0].method, seen[0].path)
	}
	variables, ok := seen[0].body["variables"].(map[string]any)
	if !ok {
		t.Fatalf("mutation carried no variables: %v", seen[0].body)
	}
	want := map[string]any{
		"branch": map[string]any{
			"repositoryNameWithOwner": "acme/cluster",
			"branchName":              "main",
		},
		"message": map[string]any{
			"headline": "revert: update podinfo to 6.5.0",
			"body":     "reverts #42\ncluster did not recover",
		},
		"expectedHeadOid": "headoid",
		"fileChanges": map[string]any{
			"additions": []any{
				map[string]any{"path": "clusters/prod/a.yaml", "contents": base64.StdEncoding.EncodeToString([]byte("kind: A\n"))},
				map[string]any{"path": "clusters/prod/z.yaml", "contents": base64.StdEncoding.EncodeToString([]byte("kind: Z\n"))},
			},
			"deletions": []any{
				map[string]any{"path": "clusters/prod/also-gone.yaml"},
				map[string]any{"path": "clusters/prod/gone.yaml"},
			},
		},
	}
	if got := variables["input"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("mutation input =\n%#v\nwant\n%#v", got, want)
	}
}

func TestACommitMessageWithoutABlankLineIsAllHeadline(t *testing.T) {
	handler, requests := recording(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, commitReply("newoid", "headoid"))
	})
	client := newTestClient(t, handler, "main")

	if _, err := client.CreateCommit(context.Background(), "main", "headoid", "revert: podinfo",
		[]FileChange{{Path: "a.yaml", Contents: []byte("a")}}); err != nil {
		t.Fatalf("CreateCommit: %v", err)
	}
	input := requests()[0].body["variables"].(map[string]any)["input"].(map[string]any)
	want := map[string]any{"headline": "revert: podinfo", "body": ""}
	if got := input["message"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("message = %#v, want %#v", got, want)
	}
}

// A deletion carrying contents is a caller that meant to write and asked to
// delete; the mutation would drop the contents silently and remove the file.
func TestADeletionCarryingContentsIsRefused(t *testing.T) {
	handler, requests := recording(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, commitReply("newoid", "headoid"))
	})
	client := newTestClient(t, handler, "main")

	_, err := client.CreateCommit(context.Background(), "main", "headoid", "revert: podinfo",
		[]FileChange{{Path: "clusters/prod.yaml", Contents: []byte("kind: A\n"), Delete: true}})
	if err == nil {
		t.Fatal("expected a refusal")
	}
	if seen := requests(); len(seen) != 0 {
		t.Fatalf("GitHub saw %d requests, want none", len(seen))
	}
}

func TestACommitMissingItsBranchHeadOrMessageNeverReachesGitHub(t *testing.T) {
	change := []FileChange{{Path: "clusters/prod.yaml", Contents: []byte("a")}}
	tests := []struct {
		name    string
		branch  string
		head    string
		message string
		changes []FileChange
	}{
		{name: "no branch", head: "headoid", message: "revert", changes: change},
		{name: "no expected head", branch: "main", message: "revert", changes: change},
		{name: "no message", branch: "main", head: "headoid", changes: change},
		{name: "no changes", branch: "main", head: "headoid", message: "revert"},
		{name: "empty change slice", branch: "main", head: "headoid", message: "revert", changes: []FileChange{}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, requests := recording(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, commitReply("newoid", "headoid"))
			})
			client := newTestClient(t, handler, "main")

			if _, err := client.CreateCommit(context.Background(), test.branch, test.head, test.message, test.changes); err == nil {
				t.Fatal("expected a refusal")
			}
			if seen := requests(); len(seen) != 0 {
				t.Fatalf("GitHub saw %d requests, want none", len(seen))
			}
		})
	}
}

// GraphQL reports a failed mutation with HTTP 200 and an errors array, so the
// status alone would read a refused commit as a written one.
func TestAGraphQLErrorReplyIsAnErrorDespiteTheSuccessStatus(t *testing.T) {
	tests := []struct {
		name  string
		reply map[string]any
		says  string
	}{
		{
			name: "expected head oid rejected",
			reply: map[string]any{
				"data":   nil,
				"errors": []any{map[string]any{"message": "expectedHeadOid does not match the current head"}},
			},
			says: "expectedHeadOid does not match the current head",
		},
		{
			name: "the branch is protected",
			reply: map[string]any{
				"data":   map[string]any{"createCommitOnBranch": nil},
				"errors": []any{map[string]any{"message": "Changes must be made through a pull request."}},
			},
			says: "Changes must be made through a pull request.",
		},
		{name: "no data at all", reply: map[string]any{}},
		{name: "null data", reply: map[string]any{"data": nil}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, _ := recording(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, test.reply)
			})
			client := newTestClient(t, handler, "main")

			sha, err := client.CreateCommit(context.Background(), "main", "headoid", "revert: podinfo",
				[]FileChange{{Path: "clusters/prod.yaml", Contents: []byte("a")}})
			if err == nil {
				t.Fatalf("CreateCommit returned %q, want an error", sha)
			}
			if sha != "" {
				t.Errorf("CreateCommit returned %q alongside an error", sha)
			}
			if test.says != "" && !strings.Contains(err.Error(), test.says) {
				t.Errorf("error %q does not carry GitHub's own message %q", err, test.says)
			}
		})
	}
}
