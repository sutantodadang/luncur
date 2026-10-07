package client

import (
	"bytes"
	"io"
	"net/http"
	"net/url"
	"strconv"
)

// AIDiagnosis is the assistant's 3-line error contract for a failure.
type AIDiagnosis struct {
	Broke       string `json:"broke"`
	Why         string `json:"why"`
	NextCommand string `json:"next_command"`
	Confidence  string `json:"confidence"`
}

// AIAction is one luncur API call the assistant made.
type AIAction struct {
	Tool        string `json:"tool"`
	CLI         string `json:"cli"`
	Mutating    bool   `json:"mutating"`
	Destructive bool   `json:"destructive"`
	OK          bool   `json:"ok"`
	Error       string `json:"error"`
}

// AIChatResult is one assistant turn.
type AIChatResult struct {
	ConversationID string     `json:"conversation_id"`
	Reply          string     `json:"reply"`
	Actions        []AIAction `json:"actions"`
	Error          string     `json:"error"`
}

// AIGenerateRequest asks for a validated config file.
type AIGenerateRequest struct {
	Kind         string `json:"kind"`
	Description  string `json:"description"`
	Current      string `json:"current,omitempty"`
	Project      string `json:"project,omitempty"`
	Env          string `json:"env,omitempty"`
	App          string `json:"app,omitempty"`
	OverrideKind string `json:"override_kind,omitempty"`
}

// AIUsage is the usage report.
type AIUsage struct {
	Rows []struct {
		Day          string `json:"day"`
		User         string `json:"user"`
		Workflow     string `json:"workflow"`
		InputTokens  int64  `json:"input_tokens"`
		OutputTokens int64  `json:"output_tokens"`
		Requests     int64  `json:"requests"`
	} `json:"rows"`
	TodayTokens int64 `json:"today_tokens"`
	DailyBudget int64 `json:"daily_budget"`
}

// AIStatus reports whether the assistant is configured.
func (c *Client) AIStatus() (map[string]any, error) {
	var out map[string]any
	err := c.do("GET", "/v1/ai/status", nil, &out)
	return out, err
}

// AIExplain diagnoses an app's latest (or the given) failed deploy or run.
func (c *Client) AIExplain(project, app, deployID, runID string) (AIDiagnosis, error) {
	var out AIDiagnosis
	err := c.doWith(c.long, "POST", "/v1/ai/explain", map[string]string{
		"project": project, "env": c.env, "app": app, "deploy_id": deployID, "run_id": runID,
	}, &out)
	return out, err
}

// AIChat sends one message to the ops assistant.
func (c *Client) AIChat(conversationID, project, message string) (AIChatResult, error) {
	var out AIChatResult
	err := c.doWith(c.long, "POST", "/v1/ai/chat", map[string]string{
		"conversation_id": conversationID, "project": project, "env": c.env, "message": message,
	}, &out)
	return out, err
}

// AIGenerate returns a validated config file.
func (c *Client) AIGenerate(req AIGenerateRequest) (string, int, error) {
	if req.Env == "" {
		req.Env = c.env
	}
	var out struct {
		Content  string `json:"content"`
		Attempts int    `json:"attempts"`
	}
	err := c.doWith(c.long, "POST", "/v1/ai/generate", req, &out)
	return out.Content, out.Attempts, err
}

// AIUsage returns the last `days` days of assistant token usage.
func (c *Client) AIUsage(days int) (AIUsage, error) {
	var out AIUsage
	err := c.do("GET", "/v1/ai/usage?days="+url.QueryEscape(strconv.Itoa(days)), nil, &out)
	return out, err
}

// Call sends a raw API request and returns the status and body — the
// `luncur mcp` executor (tool responses, errors included, go back to the
// agent verbatim, so no error-envelope decoding here).
func (c *Client) Call(method, path string, body []byte) (int, []byte, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, c.base+path, rd)
	if err != nil {
		return 0, nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.transfer.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, b, err
}
