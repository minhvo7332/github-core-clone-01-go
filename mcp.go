// MCP over streamable HTTP (the 2026-07-28 revision), at POST /mcp.
//
// The suite's client probes `server/discover` first: a result makes the
// session modern, so this endpoint answers discover with the supported
// revisions, the tools capability and the server identity (both as a loose
// serverInfo and under _meta, as the client reads either). The legacy
// `initialize` handshake is answered too, for completeness.
//
// tools/list is one precomputed array, so two calls are byte-identical.
// tools/call maps the suite's semantic arguments onto the canonical property
// names and acts on the same store as REST, so a repository or issue created
// here reads back identically over REST.
//   - an unknown tool is JSON-RPC -32602 (never HTTP 500);
//   - a missing required argument is a tool result with isError:true;
//   - a body that is not JSON is HTTP 400 with JSON-RPC -32700.
package main

import (
	"encoding/json"
	"math/rand"
	"strconv"
	"strings"
	"time"
)

var mcpVersions = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

const mcpServerName = "github-core-clone"
const mcpServerVersion = "1.0.0"

type mcpToolDef struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema mcpInputSchema `json:"inputSchema"`
}

type mcpInputSchema struct {
	Type       string             `json:"type"`
	Properties map[string]mcpProp `json:"properties"`
	Required   []string           `json:"required"`
}

type mcpProp struct {
	Type        string `json:"type"`
	Description string `json:"description"`
}

func mcpTool(name, description string, props map[string]string, required []string) mcpToolDef {
	out := mcpToolDef{
		Name: name, Description: description,
		InputSchema: mcpInputSchema{Type: "object", Properties: map[string]mcpProp{}, Required: required},
	}
	for k, desc := range props {
		typ := "string"
		if k == "pull_number" {
			typ = "integer"
		}
		if k == "private" {
			typ = "boolean"
		}
		out.InputSchema.Properties[k] = mcpProp{Type: typ, Description: desc}
	}
	return out
}

var mcpTools = []mcpToolDef{
	mcpTool("create_repo", "Create a repository owned by the viewer.", map[string]string{"name": "The repository name.", "owner": "The owner login (the viewer).", "description": "A short description.", "private": "Whether the repository is private."}, []string{"name"}),
	mcpTool("get_repo", "Read one repository by owner and name.", map[string]string{"owner": "The owner login.", "repo": "The repository name."}, []string{"owner", "repo"}),
	mcpTool("list_repos", "List the viewer repositories.", map[string]string{"owner": "The owner login (the viewer)."}, []string{}),
	mcpTool("create_issue", "Create an issue in a repository.", map[string]string{"owner": "The owner login.", "repo": "The repository name.", "title": "The issue title.", "body": "The issue body."}, []string{"owner", "repo", "title"}),
	mcpTool("get_issue", "Read one issue by number.", map[string]string{"owner": "The owner login.", "repo": "The repository name.", "issue_number": "The issue number."}, []string{"owner", "repo", "issue_number"}),
	mcpTool("list_issues", "List the issues of a repository.", map[string]string{"owner": "The owner login.", "repo": "The repository name.", "state": "open, closed or all."}, []string{"owner", "repo"}),
	mcpTool("create_pull_request", "Open a pull request from a head branch into a base branch.", map[string]string{"owner": "The owner login.", "repo": "The repository name.", "title": "The pull request title.", "body": "The pull request body.", "head": "The head branch name.", "base": "The base branch name."}, []string{"owner", "repo", "title", "head", "base"}),
	mcpTool("merge_pull_request", "Squash-merge a pull request.", map[string]string{"owner": "The owner login.", "repo": "The repository name.", "pull_number": "The pull request number."}, []string{"owner", "repo", "pull_number"}),
}

var mcpToolsJSON []byte

func init() {
	mcpToolsJSON, _ = json.Marshal(mcpTools)
}

func toolResult(data any) map[string]any {
	var text string
	if s, ok := data.(string); ok {
		text = s
	} else {
		b, _ := json.Marshal(data)
		text = string(b)
	}
	return map[string]any{
		"content":           []any{map[string]string{"type": "text", "text": text}},
		"structuredContent": data,
	}
}

func toolError(message string) map[string]any {
	return map[string]any{
		"isError": true,
		"content": []any{map[string]string{"type": "text", "text": message}},
	}
}

func missingMCPArg(def mcpToolDef, args map[string]any) string {
	for _, req := range def.InputSchema.Required {
		v := args[req]
		if v == nil || v == "" {
			return req
		}
	}
	return ""
}

func getRepoOrNull(s *Store, args map[string]any) *Repo {
	owner := viewerLogin
	if v, ok := args["owner"].(string); ok && v != "" {
		owner = v
	}
	repo := strVal(args["repo"])
	if owner != viewerLogin || repo == "" {
		return nil
	}
	return s.getRepo(owner, repo)
}

type rpcErrorVal struct {
	Code    int
	Message string
}

func callMCPTool(s *Store, host, name string, args map[string]any) (any, *rpcErrorVal) {
	var def *mcpToolDef
	for i := range mcpTools {
		if mcpTools[i].Name == name {
			def = &mcpTools[i]
			break
		}
	}
	if def == nil {
		return nil, &rpcErrorVal{-32602, "Unknown tool: " + name}
	}
	if missing := missingMCPArg(*def, args); missing != "" {
		return toolError("missing required argument: " + missing), nil
	}
	switch name {
	case "create_repo":
		repoName := strVal(args["name"])
		if s.getRepo(viewerLogin, repoName) != nil {
			return toolError("Repository " + viewerLogin + "/" + repoName + " already exists"), nil
		}
		priv, _ := args["private"].(bool)
		repo := s.createRepo(viewerLogin, repoName, priv, strVal(args["description"]))
		return toolResult(repoJSON(repo, host)), nil
	case "get_repo":
		repo := getRepoOrNull(s, args)
		if repo == nil {
			return toolError("Repository not found"), nil
		}
		return toolResult(repoJSON(repo, host)), nil
	case "list_repos":
		repos := s.listRepos(viewerLogin)
		out := make([]repoOut, 0, len(repos))
		for _, r := range repos {
			out = append(out, repoJSON(r, host))
		}
		return toolResult(map[string]any{"repositories": out}), nil
	case "create_issue":
		repo := getRepoOrNull(s, args)
		if repo == nil {
			return toolError("Repository not found"), nil
		}
		number := repo.NextNumber
		repo.NextNumber++
		now := nowISO()
		issue := &Issue{
			Number: number, GlobalID: issueGlobalID(repo.Owner, repo.Name, number),
			Title: strVal(args["title"]), Body: strVal(args["body"]), State: "open",
			Labels: []*Label{}, Comments: []*Comment{},
			Seq: s.issueSeq, CreatedAt: now, UpdatedAt: now,
		}
		s.issueSeq++
		repo.Issues[number] = issue
		return toolResult(issueJSON(repo, issue, host)), nil
	case "get_issue":
		repo := getRepoOrNull(s, args)
		var issue *Issue
		if repo != nil {
			issue = repo.Issues[numArg(args["issue_number"])]
		}
		if issue == nil || repo == nil {
			return toolError("Issue not found"), nil
		}
		return toolResult(issueJSON(repo, issue, host)), nil
	case "list_issues":
		repo := getRepoOrNull(s, args)
		if repo == nil {
			return toolError("Repository not found"), nil
		}
		state := strVal(args["state"])
		if state == "" {
			state = "open"
		}
		list := make([]*Issue, 0, len(repo.Issues))
		for _, iss := range repo.Issues {
			if state != "all" && iss.State != state {
				continue
			}
			list = append(list, iss)
		}
		// sort seq desc
		for i := 0; i < len(list); i++ {
			for j := i + 1; j < len(list); j++ {
				if list[j].Seq > list[i].Seq {
					list[i], list[j] = list[j], list[i]
				}
			}
		}
		out := make([]issueOut, 0, len(list))
		for _, iss := range list {
			out = append(out, issueJSON(repo, iss, host))
		}
		return toolResult(map[string]any{"issues": out}), nil
	case "create_pull_request":
		repo := getRepoOrNull(s, args)
		if repo == nil {
			return toolError("Repository not found"), nil
		}
		head := strVal(args["head"])
		base := strVal(args["base"])
		headSHA, ok1 := repo.Refs["refs/heads/"+head]
		baseSHA, ok2 := repo.Refs["refs/heads/"+base]
		if !ok1 || !ok2 {
			return toolError("head or base branch does not exist"), nil
		}
		for _, existing := range repo.Pulls {
			if existing.Head == head && existing.Base == base && existing.State == "open" {
				return toolError("A pull request already exists for this head and base"), nil
			}
		}
		number := repo.NextNumber
		repo.NextNumber++
		now := nowISO()
		pr := &PullRequest{
			Number: number, GlobalID: pullGlobalID(repo.Owner, repo.Name, number),
			Title: strVal(args["title"]), Body: strVal(args["body"]), State: "open",
			Head: head, Base: base, HeadSHA: headSHA, BaseSHA: baseSHA,
			Seq: nowSeq(s), CreatedAt: now, UpdatedAt: now,
		}
		repo.Pulls[number] = pr
		return toolResult(pullJSON(repo, pr, host)), nil
	case "merge_pull_request":
		repo := getRepoOrNull(s, args)
		var pr *PullRequest
		if repo != nil {
			pr = repo.Pulls[numArg(args["pull_number"])]
		}
		if pr == nil || repo == nil {
			return toolError("Pull request not found"), nil
		}
		if pr.State == "closed" {
			return toolError("Pull request is already closed"), nil
		}
		conflict, sha := squashMerge(s, repo, pr)
		if conflict {
			return toolError("Merge conflict"), nil
		}
		if sha == "" {
			return toolError("Cannot merge: missing branch tip"), nil
		}
		pr.Merged = true
		pr.State = "closed"
		pr.MergedAt = nowISO()
		pr.MergeCommitSHA = sha
		pr.BaseSHA = sha
		pr.UpdatedAt = nowISO()
		return toolResult(map[string]any{
			"merged": true, "sha": sha,
			"message": "Merge pull request #" + itoa(pr.Number) + " from " + pr.Head,
			"number":  pr.Number,
		}), nil
	}
	return nil, &rpcErrorVal{-32602, "Unknown tool: " + name}
}

func nowSeq(s *Store) int {
	n := s.issueSeq
	s.issueSeq++
	return n
}

// handleMcp handles one POST /mcp body. Returns {status, body, headers}.
func handleMcp(s *Store, host string, rawBody []byte) (int, string, map[string]string) {
	jsonHeaders := map[string]string{"content-type": "application/json; charset=utf-8"}
	var request map[string]any
	if err := json.Unmarshal(rawBody, &request); err != nil || request == nil {
		b, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": nil,
			"error": map[string]any{"code": -32700, "message": "Parse error"},
		})
		return 400, string(b), jsonHeaders
	}
	id := request["id"]
	method := strVal(request["method"])
	params, _ := request["params"].(map[string]any)
	if params == nil {
		params = map[string]any{}
	}
	serverInfo := map[string]string{"name": mcpServerName, "version": mcpServerVersion}

	ok := func(result any, extra map[string]string) (int, string, map[string]string) {
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
		h := map[string]string{}
		for k, v := range jsonHeaders {
			h[k] = v
		}
		for k, v := range extra {
			h[k] = v
		}
		return 200, string(b), h
	}
	rpcErr := func(code int, message string) (int, string, map[string]string) {
		b, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": id,
			"error": map[string]any{"code": code, "message": message},
		})
		return 200, string(b), jsonHeaders
	}

	// A notification has no id and expects no response body.
	if method == "notifications/initialized" {
		return 202, "", map[string]string{}
	}

	if method == "server/discover" {
		return ok(map[string]any{
			"supportedVersions": mcpVersions,
			"capabilities":      map[string]any{"tools": map[string]any{}},
			"serverInfo":        serverInfo,
			"_meta":             map[string]any{"io.modelcontextprotocol/serverInfo": serverInfo},
		}, nil)
	}

	if method == "initialize" {
		offered := strVal(params["protocolVersion"])
		chosen := "2025-11-25"
		for _, v := range mcpVersions {
			if v == offered {
				chosen = offered
				break
			}
		}
		sess := "sess_" + strconv.FormatInt(time.Now().UnixMilli(), 36) + "_" + strconv.FormatInt(rand.Int63n(1<<40), 36)
		return ok(map[string]any{
			"protocolVersion": chosen,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      serverInfo,
		}, map[string]string{"mcp-session-id": sess})
	}

	if method == "tools/list" {
		b, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": id,
			"result": map[string]any{"tools": json.RawMessage(mcpToolsJSON)},
		})
		return 200, string(b), jsonHeaders
	}

	if method == "tools/call" {
		toolName := strVal(params["name"])
		args, _ := params["arguments"].(map[string]any)
		if args == nil {
			args = map[string]any{}
		}
		outcome, rpcE := callMCPTool(s, host, toolName, args)
		if rpcE != nil {
			return rpcErr(rpcE.Code, rpcE.Message)
		}
		return ok(outcome, nil)
	}

	if method == "ping" {
		return ok(map[string]any{}, nil)
	}

	return rpcErr(-32601, "Method not found: "+method)
}

var _ = strings.TrimSpace
