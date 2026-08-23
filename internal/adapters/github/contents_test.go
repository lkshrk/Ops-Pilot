package github

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
)

func contentsReply(kind, encoding string, contents []byte) map[string]any {
	return map[string]any{
		"type":     kind,
		"encoding": encoding,
		"content":  base64.StdEncoding.EncodeToString(contents),
	}
}

// A revert has to delete the paths the pull request created, so a file that is
// absent at the base ref is the ordinary case, not a failure. Reading it as an
// error would abort the revert of a merge that is already deployed and broken.
func TestAFileMissingAtTheRefIsAbsentRatherThanAnError(t *testing.T) {
	handler, requests := recording(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		writeAPIError(w, http.StatusNotFound, "Not Found")
	})
	client := newTestClient(t, handler, "main")

	contents, found, err := client.FileAt(context.Background(), "clusters/prod/podinfo.yaml", "beforesha")
	if err != nil {
		t.Fatalf("FileAt: %v", err)
	}
	if found {
		t.Fatal("FileAt reported a 404 path as present")
	}
	if contents != nil {
		t.Errorf("FileAt returned %q for an absent path", contents)
	}
	if seen := requests(); len(seen) != 1 {
		t.Fatalf("GitHub saw %d requests, want the absence taken at its word", len(seen))
	}
}

// The opposite direction, and the dangerous one: a read that failed for any
// other reason is not an absence. Treating a 403 or a 500 as "the file is not
// there" makes a revert delete a file it was never able to look at.
func TestAFileReadThatFailedIsNotReportedAsAbsent(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		message string
	}{
		{name: "forbidden", status: http.StatusForbidden, message: "Resource not accessible by integration"},
		{name: "unauthorized", status: http.StatusUnauthorized, message: "Bad credentials"},
		{name: "rate limited", status: http.StatusTooManyRequests, message: "API rate limit exceeded"},
		{name: "server error", status: http.StatusInternalServerError, message: "Server Error"},
		{name: "gateway", status: http.StatusBadGateway, message: "Bad gateway"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, _ := recording(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				writeAPIError(w, test.status, test.message)
			})
			client := newTestClient(t, handler, "main")
			recordBackoff(client)

			contents, found, err := client.FileAt(context.Background(), "clusters/prod/podinfo.yaml", "beforesha")
			if err == nil {
				t.Fatalf("FileAt reported found=%v with no error for status %d", found, test.status)
			}
			if found {
				t.Errorf("FileAt reported the file present alongside an error")
			}
			if contents != nil {
				t.Errorf("FileAt returned %q alongside an error", contents)
			}
		})
	}
}

func TestAFilePresentAtTheRefIsDecodedFromBase64(t *testing.T) {
	want := []byte("apiVersion: kustomize.config.k8s.io/v1beta1\nkind: Kustomization\n")
	handler, _ := recording(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, contentsReply("file", "base64", want))
	})
	client := newTestClient(t, handler, "main")

	contents, found, err := client.FileAt(context.Background(), "clusters/prod/kustomization.yaml", "beforesha")
	if err != nil {
		t.Fatalf("FileAt: %v", err)
	}
	if !found {
		t.Fatal("FileAt reported a served file as absent")
	}
	if string(contents) != string(want) {
		t.Fatalf("FileAt returned %q, want %q", contents, want)
	}
}

// GitHub wraps the base64 payload at 60 columns, so every file larger than 45
// bytes arrives split across lines and has to come back whole. The sharp case
// is the opposite one: a payload the decoder rejects part-way yields the bytes
// it managed alongside the error, and handing those back as the file would
// restore a truncated manifest during a revert.
func TestAWrappedBase64PayloadIsDecodedWholeAndAnUndecodableOneIsNotTruncated(t *testing.T) {
	want := []byte(strings.Repeat("kind: Kustomization\n", 40))
	encoded := base64.StdEncoding.EncodeToString(want)
	wrapped := func(separator string) string {
		var out strings.Builder
		for rest := encoded; ; {
			if len(rest) <= 60 {
				out.WriteString(rest + separator)
				return out.String()
			}
			out.WriteString(rest[:60] + separator)
			rest = rest[60:]
		}
	}
	tests := []struct {
		name    string
		content string
		whole   bool
	}{
		{name: "unwrapped", content: encoded, whole: true},
		{name: "wrapped at 60 columns", content: wrapped("\n"), whole: true},
		{name: "wrapped with carriage returns", content: wrapped("\r\n"), whole: true},
		{name: "broken up by spaces", content: wrapped(" ")},
		{name: "truncated mid-payload", content: encoded[:len(encoded)-9] + "!!!"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, _ := recording(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, map[string]any{
					"type": "file", "encoding": "base64", "content": test.content,
				})
			})
			client := newTestClient(t, handler, "main")

			contents, found, err := client.FileAt(context.Background(), "clusters/prod/kustomization.yaml", "beforesha")
			if !test.whole {
				if err == nil {
					t.Fatalf("FileAt returned %d bytes and no error for an undecodable payload", len(contents))
				}
				if found || contents != nil {
					t.Fatalf("FileAt returned found=%v and %d bytes alongside an error", found, len(contents))
				}
				return
			}
			if err != nil {
				t.Fatalf("FileAt: %v", err)
			}
			if !found || string(contents) != string(want) {
				t.Fatalf("FileAt returned found=%v %d bytes, want the whole %d-byte file", found, len(contents), len(want))
			}
		})
	}
}

// GitHub answers the contents endpoint for directories and submodules too, and
// serves encoding "none" for a file above its inline size limit. None of those
// is the file's bytes, and none of them is an absence either.
func TestAContentsReplyThatIsNotAnInlineFileIsAnErrorRatherThanEmptyContents(t *testing.T) {
	tests := []struct {
		name  string
		reply any
		says  string
	}{
		{name: "directory", reply: contentsReply("dir", "base64", nil), says: "is not a file"},
		{name: "submodule", reply: contentsReply("submodule", "base64", nil), says: "is not a file"},
		{name: "symlink", reply: contentsReply("symlink", "base64", nil), says: "is not a file"},
		{name: "missing type", reply: map[string]any{"encoding": "base64", "content": ""}, says: "is not a file"},
		{
			name:  "too large to inline",
			reply: map[string]any{"type": "file", "encoding": "none", "content": ""},
			says:  "encoding",
		},
		{
			name:  "not base64 at all",
			reply: map[string]any{"type": "file", "encoding": "base64", "content": "not base64!!"},
			says:  "decode",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, _ := recording(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, test.reply)
			})
			client := newTestClient(t, handler, "main")

			contents, found, err := client.FileAt(context.Background(), "clusters/prod", "beforesha")
			if err == nil {
				t.Fatalf("FileAt returned found=%v contents=%q, want an error", found, contents)
			}
			if found {
				t.Errorf("FileAt reported the path present alongside an error")
			}
			if !strings.Contains(err.Error(), test.says) {
				t.Errorf("error %q does not mention %q", err, test.says)
			}
		})
	}
}

// The path is escaped per segment so the directory structure survives, and the
// ref goes in the query. A ref escaped as a path segment, or a path whose
// slashes were escaped away, reads some other object entirely.
func TestAFileReadAddressesTheExactPathAndRef(t *testing.T) {
	tests := []struct {
		name string
		path string
		ref  string
		want string
	}{
		{
			name: "nested path",
			path: "clusters/prod/apps/podinfo.yaml",
			ref:  "beforesha",
			want: "/repos/acme/cluster/contents/clusters/prod/apps/podinfo.yaml",
		},
		{
			name: "space in a segment",
			path: "clusters/prod/app v2.yaml",
			ref:  "beforesha",
			want: "/repos/acme/cluster/contents/clusters/prod/app%20v2.yaml",
		},
		{
			name: "hash and plus in a segment",
			path: "charts/podinfo+1.2.3#rc.tgz",
			ref:  "beforesha",
			want: "/repos/acme/cluster/contents/charts/podinfo+1.2.3%23rc.tgz",
		},
		{
			name: "ref is a branch path",
			path: "clusters/prod.yaml",
			ref:  "refs/heads/release/1.0",
			want: "/repos/acme/cluster/contents/clusters/prod.yaml",
		},
		{
			name: "plus in the ref",
			path: "clusters/prod.yaml",
			ref:  "release+1.0",
			want: "/repos/acme/cluster/contents/clusters/prod.yaml",
		},
		{
			name: "ampersand in the ref",
			path: "clusters/prod.yaml",
			ref:  "feature/a&ref=main",
			want: "/repos/acme/cluster/contents/clusters/prod.yaml",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, requests := recording(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, contentsReply("file", "base64", []byte("kind: A\n")))
			})
			client := newTestClient(t, handler, "main")

			if _, _, err := client.FileAt(context.Background(), test.path, test.ref); err != nil {
				t.Fatalf("FileAt: %v", err)
			}
			seen := requests()
			if len(seen) != 1 {
				t.Fatalf("GitHub saw %d requests, want one", len(seen))
			}
			if seen[0].method != http.MethodGet {
				t.Errorf("FileAt used %s, want GET", seen[0].method)
			}
			if seen[0].path != test.want {
				t.Errorf("FileAt read %q, want %q", seen[0].path, test.want)
			}
			if got := seen[0].query.Get("ref"); got != test.ref {
				t.Errorf("FileAt asked for ref %q, want %q", got, test.ref)
			}
		})
	}
}

// endpoint() reparses the path and keeps only the decoded form, so Go re-encodes
// it and a %2F collapses back to a separator: escaping the whole path instead of
// each segment is invisible on the wire and can only be pinned on the function.
func TestEscapePathEscapesEachSegmentAndKeepsTheSeparators(t *testing.T) {
	tests := []struct{ name, path, want string }{
		{name: "plain", path: "clusters/prod/podinfo.yaml", want: "clusters/prod/podinfo.yaml"},
		{name: "space", path: "clusters/app v2.yaml", want: "clusters/app%20v2.yaml"},
		{name: "hash", path: "charts/podinfo#rc.tgz", want: "charts/podinfo%23rc.tgz"},
		{name: "question mark", path: "charts/what?.tgz", want: "charts/what%3F.tgz"},
		{name: "percent", path: "charts/100%.tgz", want: "charts/100%25.tgz"},
		{name: "single segment", path: "prod.yaml", want: "prod.yaml"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := escapePath(test.path); got != test.want {
				t.Fatalf("escapePath(%q) = %q, want %q", test.path, got, test.want)
			}
		})
	}
}

func TestAFileReadWithoutAPathOrRefNeverReachesGitHub(t *testing.T) {
	tests := []struct{ name, path, ref string }{
		{name: "no path", ref: "beforesha"},
		{name: "no ref", path: "clusters/prod.yaml"},
		{name: "neither"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler, requests := recording(t, func(_ int, w http.ResponseWriter, _ *http.Request) {
				writeJSON(w, http.StatusOK, contentsReply("file", "base64", []byte("kind: A\n")))
			})
			client := newTestClient(t, handler, "main")

			_, found, err := client.FileAt(context.Background(), test.path, test.ref)
			if err == nil {
				t.Fatal("expected a refusal")
			}
			if found {
				t.Error("FileAt reported a file present")
			}
			if seen := requests(); len(seen) != 0 {
				t.Fatalf("GitHub saw %d requests, want none", len(seen))
			}
		})
	}
}
