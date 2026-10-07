// Package ai is luncur's provider-neutral LLM layer: a small message/tool
// model, a Provider interface with two implementations (Claude via the
// official Anthropic SDK, and any OpenAI-compatible endpoint such as a luncur
// model app, Ollama or vLLM), and the tool-use loop the server's assistant
// workflows run on. It has no server/store/kube dependency.
package ai

import (
	"context"
	"encoding/json"
	"errors"
)

// Role is a message author.
type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
)

// ToolCall is one tool invocation the model asked for.
type ToolCall struct {
	ID    string
	Name  string
	Input json.RawMessage
}

// ToolResult answers one ToolCall.
type ToolResult struct {
	CallID  string
	Content string
	IsError bool
}

// Message is one conversation turn. A user turn carries Text and/or
// ToolResults; an assistant turn carries Text and/or ToolCalls.
type Message struct {
	Role        Role
	Text        string
	ToolCalls   []ToolCall
	ToolResults []ToolResult

	// raw is the provider's own encoding of an assistant turn it produced
	// (e.g. Claude's content blocks, thinking included). Providers replay it
	// unchanged when it's theirs — Claude's thinking blocks must round-trip
	// byte-for-byte — and rebuild from the neutral fields otherwise.
	raw any
}

// Tool is a function the model may call. Schema is a JSON Schema object
// ({"type":"object","properties":...,"required":[...]}).
type Tool struct {
	Name        string
	Description string
	Schema      map[string]any
}

// Request is one model call.
type Request struct {
	System    string
	Messages  []Message
	Tools     []Tool
	MaxTokens int
	// Effort is a reasoning-effort hint ("low", "medium", "high"); providers
	// that don't support one ignore it.
	Effort string
	// JSONSchema, when set, asks for a final answer that is a JSON object
	// matching it (structured output where the provider supports it).
	JSONSchema map[string]any
}

// Stop reasons, normalized across providers.
const (
	StopEnd       = "end_turn"
	StopToolUse   = "tool_use"
	StopMaxTokens = "max_tokens"
	StopRefusal   = "refusal"
)

// Usage counts tokens for one or more calls.
type Usage struct {
	InputTokens  int64
	OutputTokens int64
}

// Add accumulates u2 into u.
func (u *Usage) Add(u2 Usage) {
	u.InputTokens += u2.InputTokens
	u.OutputTokens += u2.OutputTokens
}

// Total is input plus output tokens.
func (u Usage) Total() int64 { return u.InputTokens + u.OutputTokens }

// Response is one model reply.
type Response struct {
	Message    Message // RoleAssistant
	StopReason string
	Usage      Usage
}

// Provider is an LLM backend.
type Provider interface {
	Name() string
	Chat(ctx context.Context, req Request) (Response, error)
}

// ErrRefused is returned when the model declines the request (Claude's
// "refusal" stop reason); the error text carries the provider's explanation.
var ErrRefused = errors.New("the model declined this request")
