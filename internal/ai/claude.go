package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
	"github.com/anthropics/anthropic-sdk-go/packages/param"
	"github.com/anthropics/anthropic-sdk-go/shared/constant"
)

// DefaultClaudeModel is the model used when ai_model is unset.
const DefaultClaudeModel = "claude-opus-5-5"

// Claude talks to the Anthropic Messages API through the official SDK.
type Claude struct {
	client anthropic.Client
	model  string
}

// ClaudeConfig configures NewClaude. BaseURL and HTTPClient are optional
// (tests point BaseURL at an httptest server).
type ClaudeConfig struct {
	APIKey     string
	Model      string
	BaseURL    string
	HTTPClient *http.Client
}

// NewClaude builds a Claude provider.
func NewClaude(cfg ClaudeConfig) *Claude {
	opts := []option.RequestOption{option.WithAPIKey(cfg.APIKey)}
	if cfg.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(cfg.BaseURL))
	}
	if cfg.HTTPClient != nil {
		opts = append(opts, option.WithHTTPClient(cfg.HTTPClient))
	}
	model := cfg.Model
	if model == "" {
		model = DefaultClaudeModel
	}
	return &Claude{client: anthropic.NewClient(opts...), model: model}
}

func (c *Claude) Name() string { return "claude/" + c.model }

// claudeSupportsEffort reports whether output_config.effort is accepted
// (Haiku and pre-4.5 models reject it).
func claudeSupportsEffort(model string) bool {
	for _, p := range []string{"claude-opus-", "claude-sonnet-5", "claude-sonnet-4-6", "claude-fable-", "claude-mythos-"} {
		if strings.HasPrefix(model, p) {
			return true
		}
	}
	return false
}

// claudeSupportsDefaultFallbacks reports whether the server-side refusal
// fallback ("fallbacks": "default") is available for model.
func claudeSupportsDefaultFallbacks(model string) bool {
	switch model {
	case "claude-fable-5-1", "claude-opus-5-5", "claude-opus-5", "claude-sonnet-5-5":
		return true
	}
	return false
}

func (c *Claude) Chat(ctx context.Context, req Request) (Response, error) {
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 16000
	}
	params := anthropic.BetaMessageNewParams{
		Model:     anthropic.Model(c.model),
		MaxTokens: int64(maxTokens),
		Messages:  claudeMessages(req.Messages),
	}
	if req.System != "" {
		// The system prompt (with the tool list rendered before it) is the
		// stable prefix of every request in a workflow: cache it.
		params.System = []anthropic.BetaTextBlockParam{{
			Text:         req.System,
			CacheControl: anthropic.NewBetaCacheControlEphemeralParam(),
		}}
	}
	for _, t := range req.Tools {
		params.Tools = append(params.Tools, claudeTool(t))
	}
	if req.Effort != "" && claudeSupportsEffort(c.model) {
		params.OutputConfig.Effort = anthropic.BetaOutputConfigEffort(req.Effort)
	}
	if req.JSONSchema != nil {
		params.OutputConfig.Format = anthropic.BetaJSONOutputFormatParam{Schema: req.JSONSchema}
	}
	if claudeSupportsDefaultFallbacks(c.model) {
		// A safety-classifier decline is re-served by a fallback model
		// inside the same call instead of failing the workflow.
		params.Fallbacks = anthropic.BetaFallbacksParamUnion{OfDefault: constant.ValueOf[constant.Default]()}
		params.Betas = append(params.Betas, anthropic.AnthropicBetaServerSideFallback2026_07_01)
	}

	resp, err := c.client.Beta.Messages.New(ctx, params)
	if err != nil {
		return Response{}, fmt.Errorf("claude: %w", err)
	}

	out := Response{
		Message: Message{Role: RoleAssistant, raw: resp.ToParam()},
		Usage:   Usage{InputTokens: resp.Usage.InputTokens + resp.Usage.CacheReadInputTokens + resp.Usage.CacheCreationInputTokens, OutputTokens: resp.Usage.OutputTokens},
	}
	var text []string
	for _, block := range resp.Content {
		switch b := block.AsAny().(type) {
		case anthropic.BetaTextBlock:
			text = append(text, b.Text)
		case anthropic.BetaToolUseBlock:
			out.Message.ToolCalls = append(out.Message.ToolCalls, ToolCall{
				ID: b.ID, Name: b.Name, Input: json.RawMessage(b.JSON.Input.Raw()),
			})
		}
	}
	out.Message.Text = strings.Join(text, "\n")

	switch resp.StopReason {
	case anthropic.BetaStopReasonToolUse:
		out.StopReason = StopToolUse
	case anthropic.BetaStopReasonMaxTokens, anthropic.BetaStopReasonModelContextWindowExceeded:
		out.StopReason = StopMaxTokens
	case anthropic.BetaStopReasonRefusal:
		out.StopReason = StopRefusal
		msg := resp.StopDetails.Explanation
		if msg == "" {
			msg = string(resp.StopDetails.Category)
		}
		return out, fmt.Errorf("%w: %s", ErrRefused, msg)
	default:
		out.StopReason = StopEnd
	}
	return out, nil
}

func claudeTool(t Tool) anthropic.BetaToolUnionParam {
	schema := anthropic.BetaToolInputSchemaParam{Properties: t.Schema["properties"]}
	if req, ok := t.Schema["required"].([]string); ok {
		schema.Required = req
	}
	tp := anthropic.BetaToolParam{
		Name:        t.Name,
		Description: param.NewOpt(t.Description),
		InputSchema: schema,
	}
	return anthropic.BetaToolUnionParam{OfTool: &tp}
}

func claudeMessages(msgs []Message) []anthropic.BetaMessageParam {
	out := make([]anthropic.BetaMessageParam, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == RoleAssistant {
			if raw, ok := m.raw.(anthropic.BetaMessageParam); ok {
				out = append(out, raw)
				continue
			}
			var blocks []anthropic.BetaContentBlockParamUnion
			if m.Text != "" {
				blocks = append(blocks, anthropic.NewBetaTextBlock(m.Text))
			}
			for _, tc := range m.ToolCalls {
				var input any = map[string]any{}
				if len(tc.Input) > 0 {
					input = tc.Input
				}
				blocks = append(blocks, anthropic.NewBetaToolUseBlock(tc.ID, input, tc.Name))
			}
			out = append(out, anthropic.BetaMessageParam{Role: anthropic.BetaMessageParamRoleAssistant, Content: blocks})
			continue
		}
		var blocks []anthropic.BetaContentBlockParamUnion
		for _, tr := range m.ToolResults {
			blocks = append(blocks, anthropic.NewBetaToolResultBlock(tr.CallID, tr.Content, tr.IsError))
		}
		if m.Text != "" {
			blocks = append(blocks, anthropic.NewBetaTextBlock(m.Text))
		}
		out = append(out, anthropic.NewBetaUserMessage(blocks...))
	}
	return out
}
