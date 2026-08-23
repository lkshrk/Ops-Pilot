// Package openai implements the ai.Agent contract against an OpenAI-compatible
// chat completions endpoint with tool calling.
package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/lkshrk/ops-pilot/internal/ai"
	"github.com/lkshrk/ops-pilot/internal/diagnostics"
	"github.com/lkshrk/ops-pilot/internal/domain"
)

const (
	maxTurns         = 24
	maxResponseBytes = 8 << 20
	maxToolCalls     = 64
)

type Options struct {
	HTTPClient *http.Client
	BaseURL    string
	APIKey     string
	Model      string
	Tools      ai.Tools
	// Activity, when set, is called before each tool the agent invokes.
	Activity ai.Activity
}

type Agent struct {
	http     *http.Client
	baseURL  string
	apiKey   string
	model    string
	tools    ai.Tools
	activity ai.Activity
}

var _ ai.Agent = (*Agent)(nil)

func New(options Options) (*Agent, error) {
	if options.APIKey == "" || options.Model == "" || options.BaseURL == "" {
		return nil, fmt.Errorf("AI base URL, API key, and model are required")
	}
	client := options.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	return &Agent{
		http:     client,
		baseURL:  strings.TrimSuffix(options.BaseURL, "/"),
		apiKey:   options.APIKey,
		model:    options.Model,
		tools:    options.Tools,
		activity: options.Activity,
	}, nil
}

type message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []toolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type toolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

func (a *Agent) Assess(ctx context.Context, request ai.AssessmentRequest) (domain.Assessment, error) {
	conversation := assessmentConversation(request)
	reply, err := a.converse(ctx, conversation, append(readTools(), submitAssessmentTool), "submit_assessment", request.Stream)
	if err != nil {
		return domain.Assessment{}, err
	}
	var assessment domain.Assessment
	if err := json.Unmarshal(reply.arguments, &assessment); err != nil {
		return domain.Assessment{}, fmt.Errorf("decode assessment: %w", err)
	}
	if !assessment.Valid() {
		return domain.Assessment{}, fmt.Errorf("agent returned an incomplete assessment")
	}
	assessment.Message = reply.content
	return assessment, nil
}

func assessmentConversation(request ai.AssessmentRequest) []message {
	conversation := []message{
		{Role: "system", Content: assessmentSystemPrompt},
		{Role: "user", Content: assessmentPrompt(request)},
	}
	for _, clarification := range request.Clarifications {
		assistant, question := strings.TrimSpace(clarification.Assistant), strings.TrimSpace(clarification.Question)
		if assistant == "" {
			assistant = question
		} else if question != "" && !strings.Contains(assistant, question) {
			assistant += "\n" + question
		}
		conversation = append(conversation,
			message{Role: "assistant", Content: assistant},
			message{Role: "user", Content: clarification.Answer},
		)
	}
	return conversation
}

func (a *Agent) Diagnose(ctx context.Context, request ai.DiagnosisRequest) (domain.Diagnosis, error) {
	conversation := []message{
		{Role: "system", Content: diagnosisSystemPrompt},
		{Role: "user", Content: diagnosisPrompt(request)},
	}
	reply, err := a.converse(ctx, conversation, append(readTools(), diagnosisTools()...), "submit_diagnosis", nil)
	if err != nil {
		return domain.Diagnosis{}, err
	}
	var diagnosis domain.Diagnosis
	if err := json.Unmarshal(reply.arguments, &diagnosis); err != nil {
		return domain.Diagnosis{}, fmt.Errorf("decode diagnosis: %w", err)
	}
	switch diagnosis.Action {
	case domain.DiagnoseBenignWait, domain.DiagnoseUnfixable:
	case domain.DiagnoseFix:
		if strings.TrimSpace(diagnosis.Diff) == "" {
			return domain.Diagnosis{}, fmt.Errorf("agent proposed a fix with no diff")
		}
	default:
		return domain.Diagnosis{}, fmt.Errorf("agent returned an unknown action %q", diagnosis.Action)
	}
	if request.BenignWaitUsed && diagnosis.Action == domain.DiagnoseBenignWait {
		return domain.Diagnosis{}, fmt.Errorf("agent asked to wait again after its extension expired")
	}
	if strings.TrimSpace(diagnosis.Cause) == "" {
		return domain.Diagnosis{}, fmt.Errorf("agent returned a diagnosis with no cause")
	}
	return diagnosis, nil
}

// converse runs the tool-calling loop until the agent calls the submit tool.
// Every other tool call is served and fed back.
func (a *Agent) converse(
	ctx context.Context,
	conversation []message,
	tools []toolDefinition,
	submit string,
	stream func(ai.StreamEvent),
) (conclusion, error) {
	var prose strings.Builder
	for turn := 0; turn < maxTurns; turn++ {
		reply, err := a.complete(ctx, conversation, tools, stream)
		if err != nil {
			return conclusion{}, err
		}
		if reply.Content != "" {
			if prose.Len() != 0 {
				prose.WriteByte('\n')
			}
			prose.WriteString(reply.Content)
		}
		if len(reply.ToolCalls) == 0 {
			conversation = append(conversation, reply, message{
				Role:    "user",
				Content: "Call " + submit + " now with your conclusion.",
			})
			continue
		}
		conversation = append(conversation, reply)
		for _, call := range reply.ToolCalls {
			if call.Function.Name == submit {
				return conclusion{arguments: json.RawMessage(call.Function.Arguments), content: prose.String()}, nil
			}
			result, err := a.invoke(ctx, call)
			if err != nil {
				result = ai.FenceData("tool error", diagnostics.ScrubSecrets("error: "+err.Error()))
			}
			conversation = append(conversation, message{
				Role:       "tool",
				ToolCallID: call.ID,
				Content:    result,
			})
		}
	}
	return conclusion{}, fmt.Errorf("agent did not reach a conclusion within %d turns", maxTurns)
}

type conclusion struct {
	arguments json.RawMessage
	content   string
}

func (a *Agent) complete(ctx context.Context, conversation []message, tools []toolDefinition, stream func(ai.StreamEvent)) (message, error) {
	if stream != nil {
		stream(ai.StreamEvent{Kind: ai.StreamTurnStart})
		defer stream(ai.StreamEvent{Kind: ai.StreamTurnEnd})
	}
	payload := map[string]any{
		"model":    a.model,
		"messages": conversation,
		"tools":    tools,
	}
	if stream != nil {
		payload["stream"] = true
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return message{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, a.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return message{}, err
	}
	request.Header.Set("Authorization", "Bearer "+a.apiKey)
	request.Header.Set("Content-Type", "application/json")

	response, err := a.http.Do(request)
	if err != nil {
		return message{}, fmt.Errorf("call the AI provider: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode > 299 {
		raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
		if err != nil {
			return message{}, err
		}
		// The body distinguishes a provider refusal from a gateway in front of
		// it giving up, which the status alone does not.
		return message{}, fmt.Errorf(
			"AI provider returned status %d: %s",
			response.StatusCode, summarize(raw),
		)
	}
	if stream != nil {
		return decodeStream(response.Body, stream)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return message{}, err
	}
	var decoded struct {
		Choices []struct {
			Message message `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return message{}, fmt.Errorf("decode AI response: %w", err)
	}
	if len(decoded.Choices) == 0 {
		return message{}, fmt.Errorf("AI provider returned no choices")
	}
	reply := decoded.Choices[0].Message
	reply.Role = "assistant"
	return reply, nil
}

// decodeStream reads OpenAI's Server-Sent Events response. Tool calls arrive
// piecemeal just like prose, so both are merged before the loop sees a reply.
func decodeStream(body io.Reader, emit func(ai.StreamEvent)) (reply message, err error) {
	reader := bufio.NewReader(io.LimitReader(body, maxResponseBytes+1))
	var data []string
	var consumed int
	for {
		line, readErr := reader.ReadString('\n')
		consumed += len(line)
		if consumed > maxResponseBytes {
			return message{}, fmt.Errorf("AI stream exceeded %d bytes", maxResponseBytes)
		}
		if readErr != nil && readErr != io.EOF {
			return message{}, fmt.Errorf("read AI stream: %w", readErr)
		}
		line = strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")
		if line == "" {
			if len(data) > 0 {
				eventData := strings.Join(data, "\n")
				if eventData == "[DONE]" {
					reply.Role = "assistant"
					return reply, nil
				}
				if err := consumeStreamEvent(eventData, &reply, emit); err != nil {
					return message{}, err
				}
				data = nil
			}
		} else if strings.HasPrefix(line, "data:") {
			data = append(data, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
		} else if strings.HasPrefix(line, ":") || strings.HasPrefix(line, "event:") || strings.HasPrefix(line, "id:") || strings.HasPrefix(line, "retry:") {
			// Standard SSE metadata and keepalives carry no completion data.
		} else {
			return message{}, fmt.Errorf("malformed AI stream event")
		}
		if readErr == io.EOF {
			if len(data) != 0 {
				return message{}, fmt.Errorf("truncated AI stream event")
			}
			return message{}, fmt.Errorf("AI stream ended without [DONE]")
		}
	}
}

func consumeStreamEvent(data string, reply *message, emit func(ai.StreamEvent)) error {
	if data == "[DONE]" {
		return nil
	}
	var chunk struct {
		Choices []struct {
			Delta struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					Index    int    `json:"index"`
					ID       string `json:"id"`
					Type     string `json:"type"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return fmt.Errorf("decode AI stream event: %w", err)
	}
	if len(chunk.Choices) == 0 {
		return fmt.Errorf("AI stream event has no choices")
	}
	delta := chunk.Choices[0].Delta
	if delta.Content != "" {
		reply.Content += delta.Content
		emit(ai.StreamEvent{Kind: ai.StreamDelta, Text: delta.Content})
	}
	for _, update := range delta.ToolCalls {
		if update.Index < 0 || update.Index >= maxToolCalls {
			return fmt.Errorf("decode AI stream tool call index %d outside [0,%d)", update.Index, maxToolCalls)
		}
		for len(reply.ToolCalls) <= update.Index {
			reply.ToolCalls = append(reply.ToolCalls, toolCall{})
		}
		call := &reply.ToolCalls[update.Index]
		if update.ID != "" {
			call.ID = update.ID
		}
		if update.Type != "" {
			call.Type = update.Type
		}
		if update.Function.Name != "" {
			call.Function.Name = update.Function.Name
		}
		call.Function.Arguments += update.Function.Arguments
	}
	return nil
}

// summarize renders an error body compactly enough to log.
func summarize(raw []byte) string {
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return "empty response body"
	}
	var structured struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &structured) == nil && structured.Error.Message != "" {
		text = structured.Error.Message
	}
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > 300 {
		return text[:300] + "..."
	}
	return text
}
