package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/lkshrk/ops-pilot/internal/repopath"
)

// FileChange is one path written or removed by a commit.
type FileChange struct {
	Path     string
	Contents []byte
	Delete   bool
}

// CreateCommit writes changes to branch through GitHub's commit mutation, so
// GitHub signs the commit. There is no local push path.
func (c *Client) CreateCommit(
	ctx context.Context,
	branch, expectedHeadSHA, message string,
	changes []FileChange,
) (string, error) {
	if branch == "" || expectedHeadSHA == "" || message == "" || len(changes) == 0 {
		return "", fmt.Errorf("branch, expected head, message, and at least one change are required")
	}
	ordered := append([]FileChange(nil), changes...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })

	additions := make([]map[string]any, 0, len(ordered))
	deletions := make([]map[string]any, 0, len(ordered))
	for i, change := range ordered {
		if !repopath.Plain(change.Path) || i > 0 && change.Path == ordered[i-1].Path {
			return "", fmt.Errorf("invalid or duplicate path %q", change.Path)
		}
		if change.Delete {
			if len(change.Contents) > 0 {
				return "", fmt.Errorf("deletion of %q carries contents", change.Path)
			}
			deletions = append(deletions, map[string]any{"path": change.Path})
			continue
		}
		additions = append(additions, map[string]any{
			"path":     change.Path,
			"contents": base64.StdEncoding.EncodeToString(change.Contents),
		})
	}

	headline, body, _ := strings.Cut(message, "\n\n")
	input := map[string]any{
		"branch": map[string]any{
			"repositoryNameWithOwner": c.repository.Owner + "/" + c.repository.Name,
			"branchName":              branch,
		},
		"message":         map[string]any{"headline": headline, "body": body},
		"expectedHeadOid": expectedHeadSHA,
		"fileChanges":     map[string]any{"additions": additions, "deletions": deletions},
	}
	var result struct {
		CreateCommitOnBranch struct {
			Commit struct {
				OID     string `json:"oid"`
				Parents struct {
					Nodes []struct {
						OID string `json:"oid"`
					}
				}
			}
		}
	}
	err := c.graphql(
		ctx,
		`mutation($input:CreateCommitOnBranchInput!){createCommitOnBranch(input:$input){commit{oid parents(first:1){nodes{oid}}}}}`,
		map[string]any{"input": input},
		&result,
	)
	if err != nil {
		return "", err
	}
	commit := result.CreateCommitOnBranch.Commit
	if commit.OID == "" || len(commit.Parents.Nodes) != 1 || commit.Parents.Nodes[0].OID != expectedHeadSHA {
		return "", fmt.Errorf("partial or stale GitHub commit response")
	}
	return commit.OID, nil
}

func (c *Client) graphql(ctx context.Context, query string, variables map[string]any, out any) error {
	res, err := c.request(ctx, http.MethodPost, "/graphql", map[string]any{"query": query, "variables": variables})
	if err != nil {
		return err
	}
	defer func() { _ = res.Body.Close() }()
	var body struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		return err
	}
	if len(body.Errors) > 0 {
		return fmt.Errorf("GitHub GraphQL error: %s", body.Errors[0].Message)
	}
	if len(body.Data) == 0 {
		return fmt.Errorf("empty GitHub GraphQL response")
	}
	return json.Unmarshal(body.Data, out)
}
