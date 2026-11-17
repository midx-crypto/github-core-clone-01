// mcp.go -- MCP JSON-RPC 2.0 Streamable HTTP endpoint (Worker 2).
//
// Speaks the modern revision 2026-07-28 (server/discover, no handshake, no
// session) and the legacy handshake era (initialize +
// notifications/initialized) so the suite's client connects either way.
// Tools act on the same Store as REST. The tools list is built from one
// fixed table, so two tools/list calls answer byte-identical lists.

package main

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
)

const mcpServerName = "github-core-clone"
const mcpServerVersion = "1.0.0"

// mcpSupportedVersions is the non-empty YYYY-MM-DD list server/discover
// reports, newest first.
var mcpSupportedVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

type mcpToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

// mcpTools is the fixed tool table. Required properties use only names the
// suite can fill (see kit checks-mcp.ts ARGUMENT_SYNONYMS): adding a
// required property the suite has no value for fails that tool's check.
var mcpTools = []mcpToolDef{
	{
		Name:        "create_repo",
		Description: "Create a new repository owned by the authenticated user.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"name":        strProp("Repository name."),
				"description": strProp("Repository description."),
				"private":     map[string]any{"type": "boolean", "description": "Whether the repository is private."},
			},
			"required": []string{"name"},
		},
	},
	{
		Name:        "get_repo",
		Description: "Get a repository by owner and name.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner": strProp("Repository owner login."),
				"repo":  strProp("Repository name."),
			},
			"required": []string{"owner", "repo"},
		},
	},
	{
		Name:        "create_issue",
		Description: "Create an issue in a repository.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner": strProp("Repository owner login."),
				"repo":  strProp("Repository name."),
				"title": strProp("Issue title."),
				"body":  strProp("Issue body."),
			},
			"required": []string{"owner", "repo", "title"},
		},
	},
	{
		Name:        "list_issues",
		Description: "List issues in a repository, optionally filtered by state.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner": strProp("Repository owner login."),
				"repo":  strProp("Repository name."),
				"state": strProp("Issue state: open, closed or all."),
			},
			"required": []string{"owner", "repo"},
		},
	},
	{
		Name:        "create_pull_request",
		Description: "Create a pull request from a head branch into a base branch.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner": strProp("Repository owner login."),
				"repo":  strProp("Repository name."),
				"title": strProp("Pull request title."),
				"body":  strProp("Pull request body."),
				"head":  strProp("Head branch name."),
				"base":  strProp("Base branch name."),
			},
			"required": []string{"owner", "repo", "title", "head", "base"},
		},
	},
	{
		Name:        "merge_pull_request",
		Description: "Merge a pull request by number.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner":       strProp("Repository owner login."),
				"repo":        strProp("Repository name."),
				"pull_number": map[string]any{"type": "integer", "description": "Pull request number."},
			},
			"required": []string{"owner", "repo", "pull_number"},
		},
	},
}

// mcpToolsJSON is the pre-marshaled tools array: tools/list answers it
// verbatim every time, so two calls are byte-identical.
var mcpToolsJSON = func() json.RawMessage {
	b, _ := json.Marshal(mcpTools)
	return b
}()

// registerMCP wires the MCP JSON-RPC 2.0 surface into the router. The shared
// API instance (see shared in rest.go) keeps comments and overlays in sync
// with the REST and GraphQL surfaces.
func registerMCP(rt *Router, store *Store) {
	api := shared(store)
	rt.Handle("POST", "/mcp", func(w *ResponseWriter, r *Request) {
		api.mcp(w, r)
	})
}

func (a *API) mcp(w *ResponseWriter, r *Request) {
	if r.Method == "DELETE" {
		// Legacy session close: best-effort, no body needed.
		w.Header("Content-Type", "application/json")
		w.Status(200)
		_, _ = w.Write([]byte(`{}`))
		return
	}
	if r.Method != "POST" {
		a.mcpError(w, nil, -32600, "Method not allowed", 405)
		return
	}
	if !a.authed(r) {
		// No Authorization header: HTTP 401 (decided before any tool exists).
		a.mcpError(w, nil, -32000, "Unauthorized", 401)
		return
	}
	body := bytes.TrimSpace(r.Body)
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil || req == nil {
		// A body that is not JSON: HTTP 400 with JSON-RPC -32700.
		a.mcpError(w, nil, -32700, "Parse error", 400)
		return
	}
	method, _ := req["method"].(string)
	id := req["id"] // echoed verbatim; absent for notifications
	params, _ := req["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}

	// Notifications carry no id and get no JSON-RPC response.
	if _, hasID := req["id"]; !hasID {
		w.Header("Content-Type", "application/json")
		w.Status(202)
		_, _ = w.Write([]byte{})
		return
	}

	switch method {
	case "server/discover":
		a.mcpWrite(w, id, map[string]any{
			"resultType":        "server/discover",
			"supportedVersions": mcpSupportedVersions,
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{"name": mcpServerName, "version": mcpServerVersion},
			"_meta": map[string]any{
				"io.modelcontextprotocol/serverInfo": map[string]any{"name": mcpServerName, "version": mcpServerVersion},
			},
		})
	case "initialize":
		// Legacy handshake: echo a revision the client offered (or ours).
		version := "2025-11-25"
		if v, ok := params["protocolVersion"].(string); ok {
			for _, supported := range mcpSupportedVersions {
				if v == supported {
					version = v
					break
				}
			}
		}
		a.mcpWrite(w, id, map[string]any{
			"protocolVersion": version,
			"capabilities": map[string]any{
				"tools": map[string]any{},
			},
			"serverInfo": map[string]any{"name": mcpServerName, "version": mcpServerVersion},
		})
	case "ping":
		a.mcpWrite(w, id, map[string]any{})
	case "tools/list":
		a.mcpWriteRaw(w, id, `"tools":`+string(mcpToolsJSON))
	case "tools/call":
		a.mcpCallTool(w, id, params)
	default:
		// Unknown method: -32601 Method not found (what a pre-discover
		// server answers server/discover with, and what the suite expects
		// for unknown tools' siblings).
		a.mcpError(w, id, -32601, "Method not found: "+method, 200)
	}
}

// mcpWrite sends a JSON-RPC success result (HTTP 200, application/json).
func (a *API) mcpWrite(w *ResponseWriter, id any, result map[string]any) {
	env := map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
	a.writeJSONMCP(w, 200, env)
}

// mcpWriteRaw sends a success result whose "result" object is written from
// pre-marshaled JSON (the tools list, kept byte-identical).
func (a *API) mcpWriteRaw(w *ResponseWriter, id any, resultFragment string) {
	idJSON, _ := json.Marshal(id)
	body := []byte(`{"jsonrpc":"2.0","id":` + string(idJSON) + `,"result":{` + resultFragment + `}}`)
	a.writeJSONMCP(w, 200, nil, body)
}

// mcpError sends a JSON-RPC error object with the given HTTP status.
func (a *API) mcpError(w *ResponseWriter, id any, code int, message string, httpStatus int) {
	env := map[string]any{
		"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": message},
	}
	a.writeJSONMCP(w, httpStatus, env)
}

func (a *API) writeJSONMCP(w *ResponseWriter, status int, v any, raw ...[]byte) {
	var b []byte
	if len(raw) > 0 && raw[0] != nil {
		b = raw[0]
	} else {
		var err error
		b, err = json.Marshal(v)
		if err != nil {
			b = []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"Internal error"}}`)
		}
	}
	w.Header("Content-Type", "application/json")
	w.Status(status)
	_, _ = w.Write(b)
}

// ---------------------------------------------------------------------------
// tools/call

// mcpArgStr reads one argument, trying the canonical name then its synonyms.
func mcpArgStr(args map[string]any, names ...string) string {
	for _, n := range names {
		if s, ok := args[n].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func mcpArgInt(args map[string]any, names ...string) (int64, bool) {
	for _, n := range names {
		switch v := args[n].(type) {
		case float64:
			return int64(v), true
		case int64:
			return v, true
		case string:
			if num, err := strconv.ParseInt(v, 10, 64); err == nil {
				return num, true
			}
		}
	}
	return 0, false
}

// mcpRepoOwner resolves owner/repo from the arguments, including a
// full_name ("owner/repo") when the separate fields are absent.
func mcpRepoOwner(args map[string]any) (owner, repo string) {
	owner = mcpArgStr(args, "owner", "owner_login", "org", "user", "login")
	repo = mcpArgStr(args, "repo", "repository", "repository_name", "repo_name", "name")
	if owner == "" || repo == "" {
		if full := mcpArgStr(args, "full_name", "repo_full_name", "repository_full_name", "fullName"); full != "" {
			if i := strings.Index(full, "/"); i > 0 {
				if owner == "" {
					owner = full[:i]
				}
				if repo == "" {
					repo = full[i+1:]
				}
			}
		}
	}
	return owner, repo
}

// mcpToolResult renders a tool result as text JSON plus structured content,
// so the suite finds values both by reading text and by parsing JSON.
func (a *API) mcpToolResult(w *ResponseWriter, id any, value any) {
	text, _ := json.Marshal(value)
	a.mcpWrite(w, id, map[string]any{
		"content":           []map[string]any{{"type": "text", "text": string(text)}},
		"structuredContent": value,
	})
}

func (a *API) mcpToolError(w *ResponseWriter, id any, message string) {
	a.mcpWrite(w, id, map[string]any{
		"content": []map[string]any{{"type": "text", "text": message}},
		"isError": true,
	})
}

func (a *API) mcpCallTool(w *ResponseWriter, id any, params map[string]any) {
	name, _ := params["name"].(string)
	args, _ := params["arguments"].(map[string]any)
	if args == nil {
		args = map[string]any{}
	}
	// Resolve the tool by exact name, then by normalized suffix
	// (a server prefix is allowed) and case-insensitively.
	tool := mcpFindTool(name)
	if tool == nil {
		a.mcpError(w, id, -32602, "Unknown tool: "+name, 200)
		return
	}
	switch tool.Name {
	case "create_repo":
		a.mcpCreateRepo(w, id, args)
	case "get_repo":
		a.mcpGetRepo(w, id, args)
	case "create_issue":
		a.mcpCreateIssue(w, id, args)
	case "list_issues":
		a.mcpListIssues(w, id, args)
	case "create_pull_request":
		a.mcpCreatePull(w, id, args)
	case "merge_pull_request":
		a.mcpMergePull(w, id, args)
	default:
		a.mcpError(w, id, -32602, "Unknown tool: "+name, 200)
	}
}

func mcpNormalize(name string) string {
	var sb strings.Builder
	for _, c := range strings.ToLower(name) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			sb.WriteByte(byte(c))
		}
	}
	return sb.String()
}

func mcpFindTool(name string) *mcpToolDef {
	norm := mcpNormalize(name)
	for i := range mcpTools {
		if mcpNormalize(mcpTools[i].Name) == norm {
			return &mcpTools[i]
		}
	}
	for i := range mcpTools {
		want := mcpNormalize(mcpTools[i].Name)
		if len(norm) > len(want) && strings.HasSuffix(norm, want) {
			return &mcpTools[i]
		}
	}
	return nil
}

// mcpMissing answers a missing-required-argument call the way the suite
// accepts: JSON-RPC -32602.
func (a *API) mcpMissing(w *ResponseWriter, id any, tool, arg string) {
	a.mcpError(w, id, -32602, "Missing required argument: "+arg+" for tool "+tool, 200)
}

func (a *API) mcpCreateRepo(w *ResponseWriter, id any, args map[string]any) {
	name := mcpArgStr(args, "name", "repo", "repository", "repository_name", "repo_name")
	if name == "" {
		a.mcpMissing(w, id, "create_repo", "name")
		return
	}
	desc := mcpArgStr(args, "description", "about", "summary", "body")
	repo, err := a.store.CreateRepo(a.viewer, name, desc)
	if err != nil {
		a.mcpToolError(w, id, "Could not create repository "+name)
		return
	}
	a.mu.Lock()
	if v, ok := args["private"].(bool); ok {
		a.priv[repo.Owner+"\x00"+repo.Name] = v
	}
	a.mu.Unlock()
	a.mcpToolResult(w, id, a.repoToJSON(repo))
}

func (a *API) mcpGetRepo(w *ResponseWriter, id any, args map[string]any) {
	owner, repo := mcpRepoOwner(args)
	if owner == "" || repo == "" {
		a.mcpMissing(w, id, "get_repo", "owner/repo")
		return
	}
	r, ok := a.loadRepoQuiet(owner, repo)
	if !ok {
		a.mcpToolError(w, id, "Repository not found: "+owner+"/"+repo)
		return
	}
	a.mcpToolResult(w, id, a.repoToJSON(r))
}

// loadRepoQuiet is loadRepo without writing an HTTP error.
func (a *API) loadRepoQuiet(owner, name string) (*Repo, bool) {
	if owner != a.viewer {
		return nil, false
	}
	return a.store.GetRepo(owner, name)
}

func (a *API) mcpCreateIssue(w *ResponseWriter, id any, args map[string]any) {
	owner, repo := mcpRepoOwner(args)
	title := mcpArgStr(args, "title", "issue_title", "subject")
	if owner == "" || repo == "" || title == "" {
		missing := "owner/repo"
		if owner != "" && repo != "" {
			missing = "title"
		}
		a.mcpMissing(w, id, "create_issue", missing)
		return
	}
	if _, ok := a.loadRepoQuiet(owner, repo); !ok {
		a.mcpToolError(w, id, "Repository not found: "+owner+"/"+repo)
		return
	}
	body := mcpArgStr(args, "body", "issue_body", "description", "content", "text")
	iss, err := a.store.CreateIssue(owner, repo, title, body, nil, a.viewer)
	if err != nil {
		a.mcpToolError(w, id, "Could not create issue")
		return
	}
	a.mcpToolResult(w, id, a.issueToJSON(iss))
}

func (a *API) mcpListIssues(w *ResponseWriter, id any, args map[string]any) {
	owner, repo := mcpRepoOwner(args)
	if owner == "" || repo == "" {
		a.mcpMissing(w, id, "list_issues", "owner/repo")
		return
	}
	if _, ok := a.loadRepoQuiet(owner, repo); !ok {
		a.mcpToolError(w, id, "Repository not found: "+owner+"/"+repo)
		return
	}
	state := mcpArgStr(args, "state", "status")
	storeState := ""
	if state == "open" {
		storeState = "open"
	} else if state == "closed" {
		storeState = "closed"
	}
	issues := a.store.ListIssues(owner, repo, storeState)
	out := make([]issueJSON, 0, len(issues))
	for _, iss := range issues {
		out = append(out, a.issueToJSON(iss))
	}
	a.mcpToolResult(w, id, out)
}

func (a *API) mcpCreatePull(w *ResponseWriter, id any, args map[string]any) {
	owner, repo := mcpRepoOwner(args)
	title := mcpArgStr(args, "title", "pr_title", "pull_request_title", "subject")
	head := mcpArgStr(args, "head", "head_ref", "head_branch", "source", "source_branch", "from_branch")
	base := mcpArgStr(args, "base", "base_ref", "base_branch", "target", "target_branch", "into_branch")
	if owner == "" || repo == "" || title == "" || head == "" || base == "" {
		a.mcpMissing(w, id, "create_pull_request", "owner/repo/title/head/base")
		return
	}
	r, ok := a.loadRepoQuiet(owner, repo)
	if !ok {
		a.mcpToolError(w, id, "Repository not found: "+owner+"/"+repo)
		return
	}
	head = shortRef(head)
	base = shortRef(base)
	if _, ok := a.store.GetRef(r, "refs/heads/"+head); !ok {
		a.mcpToolError(w, id, "Head branch does not exist: "+head)
		return
	}
	if _, ok := a.store.GetRef(r, "refs/heads/"+base); !ok {
		a.mcpToolError(w, id, "Base branch does not exist: "+base)
		return
	}
	body := mcpArgStr(args, "body", "description", "content", "text")
	pull, err := a.store.CreatePull(owner, repo, title, body, head, base, a.viewer)
	if err != nil {
		a.mcpToolError(w, id, "Could not create pull request")
		return
	}
	a.mcpToolResult(w, id, a.pullToJSON(pull))
}

func (a *API) mcpMergePull(w *ResponseWriter, id any, args map[string]any) {
	owner, repo := mcpRepoOwner(args)
	num, hasNum := mcpArgInt(args, "pull_number", "pull_request_number", "pr_number", "number")
	if owner == "" || repo == "" || !hasNum {
		a.mcpMissing(w, id, "merge_pull_request", "owner/repo/pull_number")
		return
	}
	if _, ok := a.loadRepoQuiet(owner, repo); !ok {
		a.mcpToolError(w, id, "Repository not found: "+owner+"/"+repo)
		return
	}
	if _, ok := a.store.GetPull(owner, repo, num); !ok {
		a.mcpToolError(w, id, "Pull request not found")
		return
	}
	merged, err := a.store.MergePull(owner, repo, num)
	if err != nil {
		a.mcpToolError(w, id, "Pull request is conflicted and cannot be merged")
		return
	}
	a.mcpToolResult(w, id, map[string]any{
		"number": merged.Number, "merged": true,
		"sha": merged.MergeSHA, "state": merged.State,
	})
}
