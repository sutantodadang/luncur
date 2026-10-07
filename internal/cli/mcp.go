package cli

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"github.com/sutantodadang/luncur/internal/aitools"
)

// mcpCmd serves luncur's AI tool registry over the Model Context Protocol
// (JSON-RPC 2.0 on stdio), so an external agent — Claude Code, Claude
// Desktop, any MCP client — can operate luncur. Every call goes to the
// server with the logged-in user's token: the agent can do exactly what the
// user can, and each mutating call lands in the audit log as that user.
func mcpCmd() *cobra.Command {
	var project, env string
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Serve luncur as MCP tools on stdio, for AI agents (Claude Code, Claude Desktop)",
		Long: `Serve luncur as MCP tools on stdio for AI agents.

Claude Code:     claude mcp add luncur -- luncur mcp --project myproj
Claude Desktop:  {"mcpServers": {"luncur": {"command": "luncur", "args": ["mcp"]}}}

The agent acts with your login (luncur login): your role decides what it may do.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := apiClient()
			if err != nil {
				return err
			}
			admin := false
			if me, err := c.Me(); err == nil {
				admin = me.Role == "admin"
			}
			tools := aitools.Filter(admin, true)
			exec := func(tool aitools.Tool, a map[string]any) (string, bool) {
				if _, ok := a["project"]; !ok && project != "" && hasParam(tool, "project") {
					a["project"] = project
				}
				if _, ok := a["env"]; !ok && env != "" && tool.EnvScoped {
					a["env"] = env
				}
				req, err := tool.BuildRequest(a)
				if err != nil {
					return err.Error(), true
				}
				status, body, err := c.Call(req.Method, req.Path, req.Body)
				if err != nil {
					return err.Error(), true
				}
				return tool.ShapeResponse(status, body)
			}
			return serveMCP(os.Stdin, os.Stdout, tools, exec)
		},
	}
	cmd.Flags().StringVar(&project, "project", "", "default project for tool calls that omit one")
	cmd.Flags().StringVar(&env, "env", "", "default environment for tool calls that omit one")
	return cmd
}

func hasParam(t aitools.Tool, name string) bool {
	for _, p := range t.Params {
		if p.Name == name {
			return true
		}
	}
	return false
}

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *mcpError       `json:"error,omitempty"`
}

const mcpProtocolVersion = "2025-06-18"

// serveMCP runs the MCP stdio loop until in closes. exec runs one tool call.
func serveMCP(in io.Reader, out io.Writer, tools []aitools.Tool, exec func(aitools.Tool, map[string]any) (string, bool)) error {
	byName := make(map[string]aitools.Tool, len(tools))
	for _, t := range tools {
		byName[t.Name] = t
	}
	enc := json.NewEncoder(out)
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var req mcpRequest
		if err := json.Unmarshal(line, &req); err != nil {
			_ = enc.Encode(mcpResponse{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &mcpError{Code: -32700, Message: "parse error"}})
			continue
		}
		if len(req.ID) == 0 { // a notification (e.g. notifications/initialized): no reply
			continue
		}
		resp := mcpResponse{JSONRPC: "2.0", ID: req.ID}
		switch req.Method {
		case "initialize":
			var p struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			_ = json.Unmarshal(req.Params, &p)
			v := p.ProtocolVersion
			if v == "" {
				v = mcpProtocolVersion
			}
			resp.Result = map[string]any{
				"protocolVersion": v,
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "luncur", "version": version},
				"instructions":    "Tools operate a luncur PaaS install as the logged-in user. Read state before changing it; destructive tools delete data.",
			}
		case "ping":
			resp.Result = map[string]any{}
		case "tools/list":
			list := make([]map[string]any, 0, len(tools))
			for _, t := range tools {
				list = append(list, map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"inputSchema": t.Schema(),
					"annotations": map[string]any{
						"readOnlyHint":    !t.Mutating,
						"destructiveHint": t.Destructive,
					},
				})
			}
			resp.Result = map[string]any{"tools": list}
		case "tools/call":
			var p struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			if err := json.Unmarshal(req.Params, &p); err != nil {
				resp.Error = &mcpError{Code: -32602, Message: "invalid params"}
				break
			}
			tool, ok := byName[p.Name]
			if !ok {
				resp.Error = &mcpError{Code: -32602, Message: fmt.Sprintf("unknown tool %q", p.Name)}
				break
			}
			if p.Arguments == nil {
				p.Arguments = map[string]any{}
			}
			text, isErr := exec(tool, p.Arguments)
			resp.Result = map[string]any{
				"content": []map[string]any{{"type": "text", "text": text}},
				"isError": isErr,
			}
		default:
			resp.Error = &mcpError{Code: -32601, Message: "method not found: " + req.Method}
		}
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return sc.Err()
}
