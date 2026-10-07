package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sutantodadang/luncur/internal/ai"
	"github.com/sutantodadang/luncur/internal/aitools"
	"github.com/sutantodadang/luncur/internal/store"
)

// AI settings (see docs/ai/assistant.md). ai_api_key is sealed (sealedKeys).
const (
	settingAIProvider    = "ai_provider"           // claude | openai | off
	settingAIModel       = "ai_model"              // model id
	settingAIBaseURL     = "ai_base_url"           // provider endpoint, or app:<project>/<app>
	settingAIKey         = "ai_api_key"            // sealed
	settingAIEffort      = "ai_effort"             // default effort for chat/generate
	settingAINotify      = "ai_notify"             // on | off
	settingAIDailyBudget = "ai_daily_token_budget" // tokens/day, 0 = unlimited
	settingAIMaxSteps    = "ai_max_steps"          // tool-loop cap per request

	defaultAIDailyBudget = 2_000_000
	defaultAIMaxSteps    = 20
)

var aiEfforts = map[string]bool{"low": true, "medium": true, "high": true, "xhigh": true, "max": true}

// errAIDisabled is returned when no provider is configured.
var errAIDisabled = errors.New("the AI assistant is not configured — set ai_provider (luncur config set ai_provider claude) and ai_api_key")

// errAIBudget is returned when today's token budget is spent.
var errAIBudget = errors.New("today's AI token budget (ai_daily_token_budget) is used up")

// aiConfig is the resolved AI configuration for one request.
type aiConfig struct {
	provider ai.Provider
	effort   string
	maxSteps int
	budget   int64 // tokens/day; 0 = unlimited
}

func (s *server) aiSetting(key string) string {
	v, err := s.st.GetSetting(key)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(v)
}

// aiSetup builds the configured provider. s.aiProviderFn (tests) wins.
func (s *server) aiSetup(ctx context.Context) (aiConfig, error) {
	cfg := aiConfig{effort: "medium", maxSteps: defaultAIMaxSteps, budget: defaultAIDailyBudget}
	if e := s.aiSetting(settingAIEffort); aiEfforts[e] {
		cfg.effort = e
	}
	if n, err := strconv.Atoi(s.aiSetting(settingAIMaxSteps)); err == nil && n > 0 {
		cfg.maxSteps = n
	}
	if n, err := strconv.ParseInt(s.aiSetting(settingAIDailyBudget), 10, 64); err == nil && n >= 0 {
		cfg.budget = n
	}
	if s.aiProviderFn != nil {
		p, err := s.aiProviderFn()
		if err != nil {
			return aiConfig{}, err
		}
		cfg.provider = p
		return cfg, nil
	}

	key, _ := s.sealedSetting(settingAIKey)
	model := s.aiSetting(settingAIModel)
	baseURL := s.aiSetting(settingAIBaseURL)
	switch s.aiSetting(settingAIProvider) {
	case "claude":
		if key == "" {
			return aiConfig{}, fmt.Errorf("%w (ai_api_key is not set)", errAIDisabled)
		}
		cfg.provider = ai.NewClaude(ai.ClaudeConfig{APIKey: key, Model: model, BaseURL: baseURL})
	case "openai":
		resolved, err := s.aiResolveBaseURL(baseURL)
		if err != nil {
			return aiConfig{}, err
		}
		if resolved == "" || model == "" {
			return aiConfig{}, fmt.Errorf("%w (ai_base_url and ai_model are required for ai_provider=openai)", errAIDisabled)
		}
		cfg.provider = &ai.OpenAICompat{BaseURL: resolved, APIKey: key, Model: model}
	default:
		return aiConfig{}, errAIDisabled
	}
	return cfg, nil
}

// aiResolveBaseURL turns "app:<project>/<app>" (a luncur kind=model app) into
// its in-cluster OpenAI-compatible URL, so a fully self-hosted install can
// point the assistant at a model it serves itself. Anything else passes
// through unchanged.
func (s *server) aiResolveBaseURL(v string) (string, error) {
	ref, ok := strings.CutPrefix(v, "app:")
	if !ok {
		return v, nil
	}
	projectName, appName, ok := strings.Cut(ref, "/")
	if !ok || projectName == "" || appName == "" {
		return "", fmt.Errorf("ai_base_url %q: want app:<project>/<app>", v)
	}
	p, err := s.st.GetProject(projectName)
	if err != nil {
		return "", fmt.Errorf("ai_base_url: project %q: %w", projectName, err)
	}
	a, err := s.st.GetApp(p.ID, appName)
	if err != nil {
		return "", fmt.Errorf("ai_base_url: app %q: %w", appName, err)
	}
	ns, err := s.appNamespace(a)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("http://%s.%s:80/v1", a.Name, ns), nil
}

// aiBudgetCheck returns a loop budget function enforcing the install-wide
// daily token budget (today's recorded usage plus this request's spend).
func (s *server) aiBudgetCheck(cfg aiConfig) func(ai.Usage) error {
	return func(spent ai.Usage) error {
		if cfg.budget <= 0 {
			return nil
		}
		today, err := s.st.AITokensToday()
		if err != nil {
			return nil // a usage-table read failure must not take the assistant down
		}
		if today+spent.Total() >= cfg.budget {
			return errAIBudget
		}
		return nil
	}
}

// aiRecordUsage stores one request's usage (best effort).
func (s *server) aiRecordUsage(userID int64, workflow string, u ai.Usage) {
	if u.Total() == 0 {
		return
	}
	_ = s.st.AddAIUsage(userID, workflow, u.InputTokens, u.OutputTokens)
}

// --- in-process tool dispatch (act within role) ---------------------------

// aiActorKey marks an in-process request made by the assistant on behalf of
// a user. Only server code can plant a context value, so authed() can trust
// it the way it trusts a verified token.
type aiActorKey struct{}

type aiActor struct {
	user store.User
}

// aiSystemUser is the identity of system-initiated, read-only assistant
// work (notification summaries). It never reaches a mutating tool.
var aiSystemUser = store.User{ID: 0, Email: "ai-assistant", Role: "admin"}

// aiAPI is the server's own HTTP stack, built once, that tool calls are
// dispatched through: same routes, authorization, validation and audit as
// the public API.
func (s *server) aiAPI() http.Handler {
	s.aiAPIOnce.Do(func() { s.aiAPIHandler = s.handler() })
	return s.aiAPIHandler
}

// aiRecorder captures an in-process response (and satisfies http.Flusher,
// which the SSE log endpoints require).
type aiRecorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (r *aiRecorder) Header() http.Header { return r.header }
func (r *aiRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
}
func (r *aiRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	if r.body.Len() > 4<<20 { // a runaway log stream is cut, not buffered forever
		return len(b), nil
	}
	return r.body.Write(b)
}
func (r *aiRecorder) Flush() {}

// aiCallTool runs one registry tool as u through the in-process API.
func (s *server) aiCallTool(ctx context.Context, u store.User, tool aitools.Tool, args map[string]any) (string, bool) {
	req, err := tool.BuildRequest(args)
	if err != nil {
		return err.Error(), true
	}
	// Detach the outer request's audit record: the in-process call gets its
	// own (auditMiddleware plants one for mutating methods), and a nested
	// call must never overwrite the outer request's audit row.
	ctx = context.WithValue(ctx, auditCtxKey{}, (*auditInfo)(nil))
	hreq, err := http.NewRequestWithContext(context.WithValue(ctx, aiActorKey{}, &aiActor{user: u}),
		req.Method, "http://luncur.internal"+req.Path, bytes.NewReader(req.Body))
	if err != nil {
		return err.Error(), true
	}
	if req.Body != nil {
		hreq.Header.Set("Content-Type", "application/json")
	}
	rec := &aiRecorder{header: http.Header{}}
	s.aiAPI().ServeHTTP(rec, hreq)
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	return tool.ShapeResponse(rec.status, rec.body.Bytes())
}

// aiAction is one tool call the assistant made, as shown to the user.
type aiAction struct {
	Tool        string `json:"tool"`
	CLI         string `json:"cli,omitempty"`
	Mutating    bool   `json:"mutating"`
	Destructive bool   `json:"destructive,omitempty"`
	OK          bool   `json:"ok"`
	Error       string `json:"error,omitempty"`
}

// aiToolDefs converts registry tools to model tool definitions.
func aiToolDefs(tools []aitools.Tool) []ai.Tool {
	out := make([]ai.Tool, 0, len(tools))
	for _, t := range tools {
		desc := t.Description
		if t.Destructive {
			desc += " DESTRUCTIVE: only when the user explicitly asked for exactly this."
		}
		out = append(out, ai.Tool{Name: t.Name, Description: desc, Schema: t.Schema()})
	}
	return out
}

// aiRegistryExecutor executes registry tool calls as u, filling in the
// conversation's default project/env when the model omits them, and
// records every call.
func (s *server) aiRegistryExecutor(u store.User, allowed []aitools.Tool, project, env string, actions *[]aiAction) ai.Executor {
	byName := make(map[string]aitools.Tool, len(allowed))
	for _, t := range allowed {
		byName[t.Name] = t
	}
	return func(ctx context.Context, call ai.ToolCall) (string, bool, bool) {
		tool, ok := byName[call.Name]
		if !ok {
			return fmt.Sprintf("unknown or unavailable tool %q", call.Name), true, false
		}
		args := map[string]any{}
		if len(call.Input) > 0 {
			if err := json.Unmarshal(call.Input, &args); err != nil {
				return "invalid arguments: " + err.Error(), true, false
			}
		}
		if _, set := args["project"]; !set && project != "" && toolHasParam(tool, "project") {
			args["project"] = project
		}
		if _, set := args["env"]; !set && env != "" && tool.EnvScoped {
			args["env"] = env
		}
		out, isErr := s.aiCallTool(ctx, u, tool, args)
		act := aiAction{Tool: tool.Name, CLI: tool.CLIEcho(args), Mutating: tool.Mutating, Destructive: tool.Destructive, OK: !isErr}
		if isErr {
			act.Error = truncate(out, 300)
		}
		*actions = append(*actions, act)
		return out, isErr, false
	}
}

func toolHasParam(t aitools.Tool, name string) bool {
	for _, p := range t.Params {
		if p.Name == name {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// aiCanWrite reports whether u may run mutating tools in project (empty
// project: decided per call by the API itself).
func (s *server) aiCanWrite(u store.User, project string) bool {
	if u.Role == "admin" || project == "" {
		return true
	}
	p, err := s.st.GetProject(project)
	if err != nil {
		return false
	}
	role, err := s.st.MemberRole(p.ID, u.ID)
	return err == nil && role == "member"
}

// --- conversations ----------------------------------------------------------

// aiConversation is one assistant chat, kept in memory (a restart forgets
// chats; each request rebuilds context from live state anyway).
type aiConversation struct {
	id       string
	userID   int64
	project  string
	env      string
	messages []ai.Message
	actions  []aiAction
	turns    []aiTurn
	updated  time.Time
	mu       sync.Mutex // one in-flight request per conversation
}

// aiTurn is one user message and the assistant's reply, for display.
type aiTurn struct {
	User    string     `json:"user"`
	Reply   string     `json:"reply"`
	Actions []aiAction `json:"actions"`
	Error   string     `json:"error,omitempty"`
}

const (
	aiConversationTTL  = 2 * time.Hour
	aiMaxConversations = 500
)

type aiConversations struct {
	mu   sync.Mutex
	byID map[string]*aiConversation
}

func newAIID() string {
	b := make([]byte, 9)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// get returns the user's conversation id, or a new one when id is empty or
// unknown/expired/foreign.
func (c *aiConversations) get(id string, userID int64, project, env string) *aiConversation {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.byID == nil {
		c.byID = map[string]*aiConversation{}
	}
	now := time.Now()
	for k, v := range c.byID {
		if now.Sub(v.updated) > aiConversationTTL {
			delete(c.byID, k)
		}
	}
	if conv, ok := c.byID[id]; ok && conv.userID == userID {
		return conv
	}
	if len(c.byID) >= aiMaxConversations {
		var oldest *aiConversation
		for _, v := range c.byID {
			if oldest == nil || v.updated.Before(oldest.updated) {
				oldest = v
			}
		}
		delete(c.byID, oldest.id)
	}
	conv := &aiConversation{id: newAIID(), userID: userID, project: project, env: env, updated: now}
	c.byID[conv.id] = conv
	return conv
}
