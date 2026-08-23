package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/lkshrk/ops-pilot/internal/ai"
)

type toolDefinition struct {
	Type     string       `json:"type"`
	Function functionSpec `json:"function"`
}

type functionSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

func object(properties map[string]any, required ...string) map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": properties,
		"required":   required,
	}
}

func text(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func tool(name, description string, parameters map[string]any) toolDefinition {
	return toolDefinition{Type: "function", Function: functionSpec{
		Name:        name,
		Description: description,
		Parameters:  parameters,
	}}
}

// readTools are available to both prompts.
func readTools() []toolDefinition {
	return []toolDefinition{
		tool("read_repo_file", "Read a file from the GitOps repository at the pull request head.",
			object(map[string]any{"path": text("Repository-relative path.")}, "path")),
		tool("list_repo_files", "List repository files matching a glob such as kubernetes/apps/**/*.yaml.",
			object(map[string]any{"pattern": text("Glob pattern, using ** at most once.")}, "pattern")),
		tool("read_upstream_file", "Read a public file such as CHANGELOG.md from an upstream GitHub repository without repository credentials. Use an empty ref for the default branch, or a release tag when known.",
			object(map[string]any{
				"repository": text("owner/name."),
				"path":       text("Repository-relative file path."),
				"ref":        text("Tag, branch, or commit; empty means the default branch."),
			}, "repository", "path")),
		tool("search_repositories", "Find an upstream GitHub repository by name when nothing links to it.",
			object(map[string]any{"query": text("Search terms.")}, "query")),
		tool("releases", "List an upstream repository's published releases.",
			object(map[string]any{"repository": text("owner/name.")}, "repository")),
		tool("issues", "Search an upstream repository's open issues, for regression reports about a version.",
			object(map[string]any{
				"repository": text("owner/name."),
				"query":      text("Search terms, usually the target version."),
			}, "repository", "query")),
		tool("fetch_url", "Fetch a URL, for changelogs and release pages.",
			object(map[string]any{"url": text("Absolute URL.")}, "url")),
	}
}

func diagnosisTools() []toolDefinition {
	return []toolDefinition{
		tool("kube_pods",
			"List every pod in a namespace with its image, readiness, restarts and age. "+
				"Pod names carry a generated suffix, so start here rather than guessing one.",
			object(map[string]any{"namespace": text("Namespace.")}, "namespace")),
		tool("kube_events", "Read Kubernetes events for a namespace, optionally filtered by object name.",
			object(map[string]any{
				"namespace": text("Namespace."),
				"name":      text("Object name, or empty for the whole namespace."),
			}, "namespace")),
		tool("kube_logs", "Read a container's recent logs.",
			object(map[string]any{
				"namespace": text("Namespace."),
				"pod":       text("Pod name."),
				"container": text("Container name, or empty for the first."),
			}, "namespace", "pod")),
		tool("flux_status", "Read the current status of a Flux object.",
			object(map[string]any{
				"kind":      text("Kustomization or HelmRelease."),
				"namespace": text("Namespace."),
				"name":      text("Object name."),
			}, "kind", "namespace", "name")),
		submitDiagnosisTool,
	}
}

var submitAssessmentTool = tool(
	"submit_assessment",
	"Submit the final risk assessment. Call this exactly once, last.",
	object(map[string]any{
		"verdict": map[string]any{
			"type":        "string",
			"enum":        []string{"safe", "clarify", "needs_approval", "defer"},
			"description": "safe for evidence-backed bumps; clarify for one focused operator question; needs_approval for a relevant risk or unresolved high-consequence uncertainty; defer when the operator intends this PR to remain pending without more questions or a patch.",
		},
		"reason":       text("One or two sentences the person running ops-pilot can act on, addressed to them as \"you\"; never describe them in the third person. Required for safe, needs_approval, and defer; omit for clarify."),
		"changelogUrl": text("If you located release notes yourself, the URL you read. Leave empty otherwise."),
		"evidence": map[string]any{
			"type":     "array",
			"minItems": 1,
			"items": map[string]any{
				"type":        "string",
				"minLength":   1,
				"description": "A source finding plus its applicability; deployment claims name the manifest path and value.",
			},
		},
		"question": text("For clarify only: one focused question for the operator. Never combine with diff or defer."),
		"diff":     text("For needs_approval only: optional unified diff against repository files. Never combine with question or defer."),
	}, "verdict"),
)

var submitDiagnosisTool = tool(
	"submit_diagnosis",
	"Submit the final diagnosis. Call this exactly once, last.",
	object(map[string]any{
		"action": map[string]any{
			"type": "string",
			"enum": []string{"benign_wait", "fix", "unfixable"},
		},
		"cause": text("What actually went wrong, in one or two sentences."),
		"diff":  text("For action=fix only: a unified diff against repository files."),
	}, "action", "cause"),
)

var errFenceMarkerInCall = errors.New("the tool call spelled a data-fence marker; reissue it without the marker")

// consumedArguments joins the arguments the named tool reads, and only those: a
// marker in a field no case below reads reaches no error message, so refusing on
// it costs a turn for nothing. The separator keeps two fields from splicing into
// a marker neither of them spells.
func consumedArguments(name string, arguments map[string]string) string {
	var consumed strings.Builder
	for _, key := range declaredArguments(name) {
		consumed.WriteString(arguments[key])
		consumed.WriteString("\n")
	}
	return consumed.String()
}

// declaredArguments reads the argument names out of the tool's own schema, which
// is the list the switch below reads.
func declaredArguments(name string) []string {
	for _, definition := range append(readTools(), diagnosisTools()...) {
		if definition.Function.Name != name {
			continue
		}
		properties, ok := definition.Function.Parameters["properties"].(map[string]any)
		if !ok {
			return nil
		}
		keys := make([]string, 0, len(properties))
		for key := range properties {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		return keys
	}
	return nil
}

func (a *Agent) invoke(ctx context.Context, call toolCall) (string, error) {
	if a.tools == nil {
		return "", fmt.Errorf("no tools are available")
	}
	// The name is quoted back whether or not the arguments decode, so it is checked
	// before them; no error text puts the name next to an argument.
	if ai.FenceMarkerWithoutIdentifier(call.Function.Name) {
		return "", errFenceMarkerInCall
	}
	var arguments map[string]string
	if strings.TrimSpace(call.Function.Arguments) != "" {
		if err := json.Unmarshal([]byte(call.Function.Arguments), &arguments); err != nil {
			return "", fmt.Errorf("decode arguments for %s: %w", call.Function.Name, err)
		}
	}
	// After decoding, because the tool quotes back the decoded value: a JSON escape
	// is one string to Unmarshal and a different one to a scan of the raw bytes.
	consumed := consumedArguments(call.Function.Name, arguments)
	if ai.FenceMarkerWithoutIdentifier(consumed) {
		return "", errFenceMarkerInCall
	}
	// A bare marker in the model's own call is refused and reissued; an issued
	// identifier there is forgery and latched. It is served, not refused - the
	// hold is what the repair loop consumes - but latched before dispatch, so a
	// call the injection chose that returns nothing cannot carry it past the fence.
	ai.LatchFenceIdentifier(consumed)
	if a.activity != nil {
		a.activity(call.Function.Name, primaryArgument(call.Function.Name, arguments))
	}
	switch call.Function.Name {
	case "read_repo_file":
		return a.tools.ReadRepoFile(ctx, arguments["path"])
	case "list_repo_files":
		files, err := a.tools.ListRepoFiles(ctx, arguments["pattern"])
		return strings.Join(files, "\n"), err
	case "read_upstream_file":
		return a.tools.ReadUpstreamFile(ctx, arguments["repository"], arguments["path"], arguments["ref"])
	case "search_repositories":
		names, err := a.tools.SearchRepositories(ctx, arguments["query"])
		return strings.Join(names, "\n"), err
	case "releases":
		return a.tools.Releases(ctx, arguments["repository"])
	case "issues":
		titles, err := a.tools.Issues(ctx, arguments["repository"], arguments["query"])
		if err != nil {
			return "", err
		}
		if len(titles) == 0 {
			return "no open issues match", nil
		}
		return strings.Join(titles, "\n"), nil
	case "fetch_url":
		return a.tools.FetchURL(ctx, arguments["url"])
	case "kube_pods":
		return a.tools.KubePods(ctx, arguments["namespace"])
	case "kube_events":
		return a.tools.KubeEvents(ctx, arguments["namespace"], arguments["name"])
	case "kube_logs":
		return a.tools.KubeLogs(ctx, arguments["namespace"], arguments["pod"], arguments["container"])
	case "flux_status":
		return a.tools.FluxStatus(ctx, arguments["kind"], arguments["namespace"], arguments["name"])
	default:
		return "", fmt.Errorf("unknown tool %q", call.Function.Name)
	}
}

// primaryArgument picks the one value worth showing for each tool.
func primaryArgument(tool string, arguments map[string]string) string {
	for _, key := range []string{"path", "pattern", "repository", "url", "query", "pod", "name"} {
		if value := arguments[key]; value != "" {
			if key == "repository" && arguments["query"] != "" {
				return value + " for " + arguments["query"]
			}
			return value
		}
	}
	return ""
}
