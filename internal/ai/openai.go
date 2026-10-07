package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// OpenAICompat talks to any OpenAI-compatible /v1/chat/completions endpoint:
// a luncur kind=model app (llama.cpp / vLLM), Ollama, or a hosted service.
// It's the self-hosted path, so it uses only the standard library.
type OpenAICompat struct {
	BaseURL string // e.g. http://chat.luncur-ml:80/v1
	APIKey  string // optional
	Model   string
	HTTP    *http.Client
}

func (o *OpenAICompat) Name() string { return "openai/" + o.Model }

func (o *OpenAICompat) client() *http.Client {
	if o.HTTP != nil {
		return o.HTTP
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

type oaiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaiMessage struct {
	Role       string        `json:"role"`
	Content    *string       `json:"content"`
	ToolCalls  []oaiToolCall `json:"tool_calls,omitempty"`
	ToolCallID string        `json:"tool_call_id,omitempty"`
}

type oaiTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description,omitempty"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

func strPtr(s string) *string { return &s }

func (o *OpenAICompat) Chat(ctx context.Context, req Request) (Response, error) {
	var msgs []oaiMessage
	system := req.System
	if req.JSONSchema != nil {
		// Structured output support varies across self-hosted servers, so ask
		// for the shape in plain words; callers parse leniently.
		b, _ := json.Marshal(req.JSONSchema)
		system += "\n\nReply with only a JSON object matching this JSON Schema, no prose:\n" + string(b)
	}
	if system != "" {
		msgs = append(msgs, oaiMessage{Role: "system", Content: strPtr(system)})
	}
	for _, m := range req.Messages {
		if m.Role == RoleAssistant {
			am := oaiMessage{Role: "assistant", Content: strPtr(m.Text)}
			for _, tc := range m.ToolCalls {
				c := oaiToolCall{ID: tc.ID, Type: "function"}
				c.Function.Name = tc.Name
				c.Function.Arguments = string(tc.Input)
				am.ToolCalls = append(am.ToolCalls, c)
			}
			msgs = append(msgs, am)
			continue
		}
		for _, tr := range m.ToolResults {
			content := tr.Content
			if tr.IsError {
				content = "ERROR: " + content
			}
			msgs = append(msgs, oaiMessage{Role: "tool", Content: strPtr(content), ToolCallID: tr.CallID})
		}
		if m.Text != "" {
			msgs = append(msgs, oaiMessage{Role: "user", Content: strPtr(m.Text)})
		}
	}

	body := map[string]any{"model": o.Model, "messages": msgs}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	if len(req.Tools) > 0 {
		var tools []oaiTool
		for _, t := range req.Tools {
			ot := oaiTool{Type: "function"}
			ot.Function.Name = t.Name
			ot.Function.Description = t.Description
			ot.Function.Parameters = t.Schema
			tools = append(tools, ot)
		}
		body["tools"] = tools
	}
	b, err := json.Marshal(body)
	if err != nil {
		return Response{}, err
	}
	url := strings.TrimRight(o.BaseURL, "/") + "/chat/completions"
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
	if err != nil {
		return Response{}, err
	}
	hreq.Header.Set("Content-Type", "application/json")
	if o.APIKey != "" {
		hreq.Header.Set("Authorization", "Bearer "+o.APIKey)
	}
	resp, err := o.client().Do(hreq)
	if err != nil {
		return Response{}, fmt.Errorf("openai-compatible: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return Response{}, fmt.Errorf("openai-compatible: read response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := string(raw)
		if len(snippet) > 300 {
			snippet = snippet[:300]
		}
		return Response{}, fmt.Errorf("openai-compatible: %s: %s", resp.Status, snippet)
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content   *string       `json:"content"`
				ToolCalls []oaiToolCall `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage struct {
			PromptTokens     int64 `json:"prompt_tokens"`
			CompletionTokens int64 `json:"completion_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return Response{}, fmt.Errorf("openai-compatible: decode response: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return Response{}, fmt.Errorf("openai-compatible: response has no choices")
	}
	ch := parsed.Choices[0]
	out := Response{
		Message: Message{Role: RoleAssistant},
		Usage:   Usage{InputTokens: parsed.Usage.PromptTokens, OutputTokens: parsed.Usage.CompletionTokens},
	}
	if ch.Message.Content != nil {
		out.Message.Text = *ch.Message.Content
	}
	for i, tc := range ch.Message.ToolCalls {
		id := tc.ID
		if id == "" {
			id = fmt.Sprintf("call_%d", i)
		}
		args := tc.Function.Arguments
		if strings.TrimSpace(args) == "" {
			args = "{}"
		}
		out.Message.ToolCalls = append(out.Message.ToolCalls, ToolCall{ID: id, Name: tc.Function.Name, Input: json.RawMessage(args)})
	}
	switch {
	case len(out.Message.ToolCalls) > 0:
		out.StopReason = StopToolUse
	case ch.FinishReason == "length":
		out.StopReason = StopMaxTokens
	default:
		out.StopReason = StopEnd
	}
	return out, nil
}
