package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Executor runs one tool call. stop=true ends the loop right after this
// call's result is recorded (e.g. a generate workflow's accepted submit).
type Executor func(ctx context.Context, call ToolCall) (content string, isError, stop bool)

// Event reports loop progress (for streaming UIs and audit trails).
type Event struct {
	Kind   string // "tool_call", "tool_result", "text"
	Call   ToolCall
	Result ToolResult
	Text   string
}

// LoopConfig bounds a tool-use loop.
type LoopConfig struct {
	MaxSteps int // model calls; default 20
	// Budget, when set, is consulted before every model call with the usage
	// so far; a non-nil error stops the loop with that error.
	Budget  func(spent Usage) error
	OnEvent func(Event)
	// MaxResultBytes caps each tool result fed back to the model (tail
	// kept); default 24 KiB.
	MaxResultBytes int
}

// LoopResult is a finished loop.
type LoopResult struct {
	Messages []Message // the full conversation, including req.Messages
	Final    string    // the last assistant text
	Usage    Usage
	Steps    int
	Stopped  bool // an Executor asked to stop
}

// ErrMaxSteps is returned when the loop hits LoopConfig.MaxSteps.
var ErrMaxSteps = errors.New("assistant step limit reached")

// Run drives req through p, executing tool calls with exec until the model
// stops asking for tools, an executor stops it, or a limit is hit. Partial
// results are returned alongside any error.
func Run(ctx context.Context, p Provider, req Request, exec Executor, cfg LoopConfig) (LoopResult, error) {
	maxSteps := cfg.MaxSteps
	if maxSteps <= 0 {
		maxSteps = 20
	}
	maxResult := cfg.MaxResultBytes
	if maxResult <= 0 {
		maxResult = 24 << 10
	}
	emit := func(e Event) {
		if cfg.OnEvent != nil {
			cfg.OnEvent(e)
		}
	}

	res := LoopResult{Messages: append([]Message(nil), req.Messages...)}
	for res.Steps < maxSteps {
		if cfg.Budget != nil {
			if err := cfg.Budget(res.Usage); err != nil {
				return res, err
			}
		}
		req.Messages = res.Messages
		resp, err := p.Chat(ctx, req)
		res.Steps++
		res.Usage.Add(resp.Usage)
		if err != nil {
			return res, err
		}
		res.Messages = append(res.Messages, resp.Message)
		if resp.Message.Text != "" {
			res.Final = resp.Message.Text
			emit(Event{Kind: "text", Text: resp.Message.Text})
		}
		if resp.StopReason == StopMaxTokens && len(resp.Message.ToolCalls) == 0 {
			return res, fmt.Errorf("the model's reply was cut off (max tokens)")
		}
		if len(resp.Message.ToolCalls) == 0 {
			return res, nil
		}

		results := Message{Role: RoleUser}
		stop := false
		for _, call := range resp.Message.ToolCalls {
			emit(Event{Kind: "tool_call", Call: call})
			var tr ToolResult
			if !json.Valid(call.Input) {
				tr = ToolResult{CallID: call.ID, Content: "invalid tool input: not valid JSON", IsError: true}
			} else {
				content, isErr, s := exec(ctx, call)
				if len(content) > maxResult {
					content = "…(truncated)…\n" + content[len(content)-maxResult:]
				}
				tr = ToolResult{CallID: call.ID, Content: content, IsError: isErr}
				stop = stop || s
			}
			results.ToolResults = append(results.ToolResults, tr)
			emit(Event{Kind: "tool_result", Call: call, Result: tr})
		}
		// Every result goes back in one user turn (parallel tool calls).
		res.Messages = append(res.Messages, results)
		if stop {
			res.Stopped = true
			return res, nil
		}
	}
	return res, ErrMaxSteps
}
