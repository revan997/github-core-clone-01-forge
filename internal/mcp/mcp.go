// Package mcp is a faithful Go port of the Node forge's mcp.ts: the Model
// Context Protocol endpoint (POST /mcp), modern era (2026-07-28).
//
// The suite's client probes `server/discover` first: a result makes the era
// modern (no handshake, no session); it still answers `initialize` for any
// client that asks. Transport: plain JSON over one POST per request.
//
// Tool failures are isError:true, never an HTTP 500. An unknown tool is
// JSON-RPC -32602; a body that is not JSON is HTTP 400 with -32700.
// Auth is handled by the app layer; this handler assumes authorization.
//
// Viewer login comes from the scenario package (scenario.Viewer), the same
// source mcp.ts imports VIEWER from.
package mcp

import (
	"bytes"
	"encoding/json"
	"math"
	"strconv"
	"strings"

	"forge/internal/scenario"
	"forge/internal/store"
)

const (
	serverName    = "makefun-forge"
	serverVersion = "1.0.0"
)

var supportedVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18"}

// McpResponse is the HTTP response for POST /mcp.
type McpResponse struct {
	Status int
	Body   []byte
}

type schemaProp struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}

type inputSchema struct {
	Type       string                `json:"type"`
	Properties map[string]schemaProp `json:"properties"`
	Required   []string              `json:"required"`
}

type toolDef struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	InputSchema inputSchema `json:"inputSchema"`
}

var tools = []toolDef{
	{
		Name:        "create_repo",
		Description: "Create a repository owned by the authenticated user.",
		InputSchema: inputSchema{Type: "object", Properties: map[string]schemaProp{
			"name":        {Type: "string", Description: "Repository name"},
			"description": {Type: "string", Description: "Repository description"},
			"private":     {Type: "boolean", Description: "Whether the repository is private"},
		}, Required: []string{"name"}},
	},
	{
		Name:        "get_repo",
		Description: "Get a repository by owner and name.",
		InputSchema: inputSchema{Type: "object", Properties: map[string]schemaProp{
			"owner": {Type: "string", Description: "Repository owner login"},
			"repo":  {Type: "string", Description: "Repository name"},
		}, Required: []string{"owner", "repo"}},
	},
	{
		Name:        "create_issue",
		Description: "Create an issue in a repository.",
		InputSchema: inputSchema{Type: "object", Properties: map[string]schemaProp{
			"owner": {Type: "string", Description: "Repository owner login"},
			"repo":  {Type: "string", Description: "Repository name"},
			"title": {Type: "string", Description: "Issue title"},
			"body":  {Type: "string", Description: "Issue body"},
		}, Required: []string{"owner", "repo", "title"}},
	},
	{
		Name:        "list_issues",
		Description: "List issues in a repository, optionally filtered by state.",
		InputSchema: inputSchema{Type: "object", Properties: map[string]schemaProp{
			"owner": {Type: "string", Description: "Repository owner login"},
			"repo":  {Type: "string", Description: "Repository name"},
			"state": {Type: "string", Description: "Issue state: open, closed, or all"},
		}, Required: []string{"owner", "repo"}},
	},
	{
		Name:        "create_pull_request",
		Description: "Create a pull request from a head branch into a base branch.",
		InputSchema: inputSchema{Type: "object", Properties: map[string]schemaProp{
			"owner": {Type: "string", Description: "Repository owner login"},
			"repo":  {Type: "string", Description: "Repository name"},
			"title": {Type: "string", Description: "Pull request title"},
			"head":  {Type: "string", Description: "Head branch name"},
			"base":  {Type: "string", Description: "Base branch name"},
		}, Required: []string{"owner", "repo", "title", "head", "base"}},
	},
	{
		Name:        "merge_pull_request",
		Description: "Squash-merge a pull request.",
		InputSchema: inputSchema{Type: "object", Properties: map[string]schemaProp{
			"owner":       {Type: "string", Description: "Repository owner login"},
			"repo":        {Type: "string", Description: "Repository name"},
			"pull_number": {Type: "number", Description: "Pull request number"},
		}, Required: []string{"owner", "repo", "pull_number"}},
	},
}

// marshalJSON is JSON.stringify: compact, no HTML escaping (Go's default
// escapes <, >, &; Node does not), no trailing newline.
func marshalJSON(v any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return bytes.TrimRight(buf.Bytes(), "\n")
}

func isRecord(v any) bool {
	_, ok := v.(map[string]any)
	return ok
}

// jsString mirrors JS String(v) for the JSON values tool args can hold.
func jsString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case json.Number:
		return t.String()
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	case bool:
		if t {
			return "true"
		}
		return "false"
	default:
		return ""
	}
}

// jsNumber mirrors JS Number(v): (value, ok). ok=false for NaN-like inputs
// ("" handled as 0, like JS; non-numeric strings fail).
func jsNumber(v any) (float64, bool) {
	switch t := v.(type) {
	case json.Number:
		f, err := t.Float64()
		return f, err == nil
	case float64:
		return t, true
	case string:
		s := strings.TrimSpace(t)
		if s == "" {
			return 0, true
		}
		f, err := strconv.ParseFloat(s, 64)
		return f, err == nil
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	case nil:
		return 0, true
	}
	return 0, false
}

// jsInt is jsNumber restricted to integer values (a TS Map.get(NaN) misses).
func jsInt(v any) (int, bool) {
	f, ok := jsNumber(v)
	if !ok || math.Trunc(f) != f || f > math.MaxInt32 || f < math.MinInt32 {
		return 0, false
	}
	return int(f), true
}

// Tool result payloads are structs (not maps) so their JSON key order matches
// Node's JSON.stringify insertion order exactly — including inside the
// content[0].text string, which the suite JSON-parses.
type ownerData struct {
	Login string `json:"login"`
}

type repoData struct {
	ID            int       `json:"id"`
	Name          string    `json:"name"`
	FullName      string    `json:"full_name"`
	Owner         ownerData `json:"owner"`
	Private       bool      `json:"private"`
	Description   string    `json:"description"`
	DefaultBranch string    `json:"default_branch"`
}

type issueData struct {
	ID     string `json:"id"`
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	State  string `json:"state"`
	URL    string `json:"url"`
}

type issueListItem struct {
	ID     string `json:"id"`
	Number int    `json:"number"`
	Title  string `json:"title"`
	Body   string `json:"body"`
	State  string `json:"state"`
}

type listIssuesData struct {
	Issues []issueListItem `json:"issues"`
}

type pullData struct {
	ID                string `json:"id"`
	Number            int    `json:"number"`
	PullNumber        int    `json:"pull_number"`
	PullRequestNumber int    `json:"pull_request_number"`
	Title             string `json:"title"`
	State             string `json:"state"`
	Head              string `json:"head"`
	Base              string `json:"base"`
	HeadRefName       string `json:"headRefName"`
	BaseRefName       string `json:"baseRefName"`
}

type mergeData struct {
	Number     int    `json:"number"`
	PullNumber int    `json:"pull_number"`
	Merged     bool   `json:"merged"`
	State      string `json:"state"`
	SHA        string `json:"sha"`
}

type errorData struct {
	Error string `json:"error"`
}

func toolResult(data any, isError bool) map[string]any {
	return map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": string(marshalJSON(data))}},
		"structuredContent": data,
		"isError":           isError,
	}
}

func toolError(message string) map[string]any {
	return toolResult(errorData{Error: message}, true)
}

func repoJSON(repo *store.Repo) repoData {
	return repoData{
		ID:            repo.DBID,
		Name:          repo.Name,
		FullName:      repo.Owner + "/" + repo.Name,
		Owner:         ownerData{Login: repo.Owner},
		Private:       repo.IsPrivate,
		Description:   repo.Description,
		DefaultBranch: repo.DefaultBranch,
	}
}

func findTool(name string) *toolDef {
	for i := range tools {
		if tools[i].Name == name {
			return &tools[i]
		}
	}
	return nil
}

func callTool(s *store.Store, name string, args map[string]any) map[string]any {
	tool := findTool(name)
	if tool == nil {
		return toolError("unknown tool " + name)
	}
	for _, required := range tool.InputSchema.Required {
		v, present := args[required]
		if !present || v == nil {
			return toolError("missing required argument: " + required)
		}
		if str, ok := v.(string); ok && str == "" {
			return toolError("missing required argument: " + required)
		}
	}
	owner := scenario.Viewer.Login
	if str, ok := args["owner"].(string); ok {
		owner = str
	}
	repoName := ""
	if str, ok := args["repo"].(string); ok {
		repoName = str
	}
	switch name {
	case "create_repo":
		{
			repo := s.CreateRepo(jsString(args["name"]), args["private"] == true, jsStringOr(args["description"], ""))
			if repo == nil {
				return toolError("Repository " + jsString(args["name"]) + " already exists")
			}
			return toolResult(repoJSON(repo), false)
		}
	case "get_repo":
		{
			repo := s.GetRepo(owner, repoName)
			if repo == nil {
				return toolError("Repository " + owner + "/" + repoName + " not found")
			}
			return toolResult(repoJSON(repo), false)
		}
	case "create_issue":
		{
			repo := s.GetRepo(owner, repoName)
			if repo == nil {
				return toolError("Repository " + owner + "/" + repoName + " not found")
			}
			issue := s.CreateIssue(repo, jsString(args["title"]), jsStringOr(args["body"], ""), []string{})
			return toolResult(issueData{
				ID: issue.GlobalID, Number: issue.Number, Title: issue.Title,
				Body: issue.Body, State: issue.State,
				URL: "/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/issues/" + strconv.Itoa(issue.Number),
			}, false)
		}
	case "list_issues":
		{
			repo := s.GetRepo(owner, repoName)
			if repo == nil {
				return toolError("Repository " + owner + "/" + repoName + " not found")
			}
			state := "open"
			if str, ok := args["state"].(string); ok {
				state = str
			}
			issues := make([]*store.Issue, 0, len(repo.Issues))
			for _, issue := range repo.Issues {
				issues = append(issues, issue)
			}
			// Sort by number ascending (deterministic; TS sorts the same way).
			for i := 1; i < len(issues); i++ {
				for j := i; j > 0 && issues[j].Number < issues[j-1].Number; j-- {
					issues[j], issues[j-1] = issues[j-1], issues[j]
				}
			}
			list := make([]issueListItem, 0, len(issues))
			for _, issue := range issues {
				if state != "all" && issue.State != state {
					continue
				}
				list = append(list, issueListItem{
					ID: issue.GlobalID, Number: issue.Number, Title: issue.Title,
					Body: issue.Body, State: issue.State,
				})
			}
			return toolResult(listIssuesData{Issues: list}, false)
		}
	case "create_pull_request":
		{
			repo := s.GetRepo(owner, repoName)
			if repo == nil {
				return toolError("Repository " + owner + "/" + repoName + " not found")
			}
			head := jsString(args["head"])
			if idx := strings.LastIndex(head, ":"); idx >= 0 {
				head = head[idx+1:]
			}
			base := jsString(args["base"])
			if idx := strings.LastIndex(base, ":"); idx >= 0 {
				base = base[idx+1:]
			}
			pull, message, ok := s.CreatePull(repo, jsString(args["title"]), "", head, base)
			if !ok {
				return toolError(message)
			}
			return toolResult(pullData{
				ID: pull.GlobalID, Number: pull.Number, PullNumber: pull.Number,
				PullRequestNumber: pull.Number, Title: pull.Title, State: pull.State,
				Head: pull.HeadRef, Base: pull.BaseRef,
				HeadRefName: pull.HeadRef, BaseRefName: pull.BaseRef,
			}, false)
		}
	case "merge_pull_request":
		{
			repo := s.GetRepo(owner, repoName)
			if repo == nil {
				return toolError("Repository " + owner + "/" + repoName + " not found")
			}
			number, ok := jsInt(args["pull_number"])
			var pull *store.PullRequest
			if ok {
				pull = repo.Pulls[number]
			}
			if pull == nil {
				return toolError("Pull request " + jsString(args["pull_number"]) + " not found")
			}
			sha, message, ok := s.MergePull(repo, pull)
			if !ok {
				return toolError(message)
			}
			return toolResult(mergeData{
				Number: pull.Number, PullNumber: pull.Number,
				Merged: true, State: "closed", SHA: sha,
			}, false)
		}
	default:
		return toolError("unknown tool " + name)
	}
}

// jsStringOr returns args value as a string, or fallback when it is not one
// (TS: typeof x === 'string' ? x : fallback).
func jsStringOr(v any, fallback string) string {
	if str, ok := v.(string); ok {
		return str
	}
	return fallback
}

func jsonRPCResponse(status int, value any) McpResponse {
	return McpResponse{Status: status, Body: marshalJSON(value)}
}

// HandleMcp handles POST /mcp: one JSON-RPC 2.0 request per POST.
func HandleMcp(s *store.Store, body []byte) McpResponse {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var request any
	if err := dec.Decode(&request); err != nil {
		return jsonRPCResponse(400, map[string]any{
			"jsonrpc": "2.0", "id": nil,
			"error": map[string]any{"code": -32700, "message": "Parse error"},
		})
	}
	req, ok := request.(map[string]any)
	if !ok {
		return jsonRPCResponse(400, map[string]any{
			"jsonrpc": "2.0", "id": nil,
			"error": map[string]any{"code": -32600, "message": "Invalid Request"},
		})
	}
	method, ok := req["method"].(string)
	if !ok {
		return jsonRPCResponse(400, map[string]any{
			"jsonrpc": "2.0", "id": nil,
			"error": map[string]any{"code": -32600, "message": "Invalid Request"},
		})
	}
	var id any
	if v, present := req["id"]; present {
		id = v
	} else {
		id = nil
	}
	params := map[string]any{}
	if p, ok := req["params"].(map[string]any); ok {
		params = p
	}
	// A notification (no id) gets no answer.
	_, hasID := req["id"]
	isNotification := !hasID || req["id"] == nil
	answer := func(result any) McpResponse {
		return jsonRPCResponse(200, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
	}
	fail := func(code int, message string) McpResponse {
		return jsonRPCResponse(200, map[string]any{
			"jsonrpc": "2.0", "id": id,
			"error": map[string]any{"code": code, "message": message},
		})
	}

	switch method {
	case "server/discover":
		versions := make([]any, 0, len(supportedVersions))
		for _, v := range supportedVersions {
			versions = append(versions, v)
		}
		return answer(map[string]any{
			"resultType":        "discover",
			"supportedVersions": versions,
			"capabilities":      map[string]any{"tools": map[string]any{"listChanged": false}},
			"instructions":      "A GitHub-compatible git forge: create repositories, issues and pull requests over MCP tools.",
			"serverInfo":        map[string]any{"name": serverName, "version": serverVersion},
			"_meta": map[string]any{"io.modelcontextprotocol/serverInfo": map[string]any{
				"name": serverName, "version": serverVersion,
			}},
		})
	case "initialize":
		{
			requested, _ := params["protocolVersion"].(string)
			negotiated := "2025-11-25"
			for _, v := range supportedVersions {
				if v == requested {
					negotiated = requested
					break
				}
			}
			return answer(map[string]any{
				"protocolVersion": negotiated,
				"capabilities":    map[string]any{"tools": map[string]any{"listChanged": false}},
				"serverInfo":      map[string]any{"name": serverName, "version": serverVersion},
			})
		}
	case "notifications/initialized":
		if isNotification {
			return McpResponse{Status: 202, Body: []byte{}}
		}
		return answer(map[string]any{})
	case "ping":
		return answer(map[string]any{})
	case "tools/list":
		return answer(map[string]any{"tools": tools})
	case "tools/call":
		{
			toolName, _ := params["name"].(string)
			args := map[string]any{}
			if a, ok := params["arguments"].(map[string]any); ok {
				args = a
			}
			if toolName == "" {
				return fail(-32602, "tools/call needs a tool name")
			}
			if findTool(toolName) == nil {
				return fail(-32602, "unknown tool "+toolName)
			}
			return answer(callTool(s, toolName, args))
		}
	default:
		return fail(-32601, "Method not found: "+method)
	}
}
