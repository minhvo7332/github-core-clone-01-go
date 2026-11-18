// The HTTP server: routing, auth, body reading. One process, one store.
//
// Routes:
//   GET  /healthz                       200 {ok:true} (no auth)
//   POST /mcp                          MCP over streamable HTTP
//   POST /api/graphql                  GraphQL v4
//   /api/v3/*                          REST v3 (see rest.go)
//   /{owner}/{repo}.git/info/refs      git smart-HTTP advertisements
//   /{owner}/{repo}.git/git-upload-pack
//   /{owner}/{repo}.git/git-receive-pack
//   anything else                      404 {"message":"Not Found"}
//
// Auth: every surface but /healthz requires a non-empty Bearer token and
// answers 401 {"message": ...} without one. The token is opaque; the viewer
// is always adaiversen. Every JSON reply carries X-GitHub-Request-Id.
package main

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync/atomic"
)

var reqCounter int64

func nextRequestID() string {
	n := atomic.AddInt64(&reqCounter, 1)
	return "req_" + strconv.FormatInt(n, 36)
}

func hasAuth(req *Request) bool {
	h := req.Headers["authorization"]
	return strings.HasPrefix(h, "Bearer ") && strings.TrimSpace(h[7:]) != ""
}

// jsonReply writes a JSON body with the standard headers.
func jsonReply(res *Response, status int, body any, extra map[string]string) {
	var text string
	if status == 204 {
		text = ""
	} else {
		b, _ := json.Marshal(body)
		text = string(b) + "\n"
	}
	headers := map[string]string{
		"content-type":        "application/json; charset=utf-8",
		"x-github-request-id": nextRequestID(),
	}
	if status != 204 {
		headers["content-length"] = strconv.Itoa(len(text))
	}
	for k, v := range extra {
		headers[k] = v
	}
	res.WriteHead(status, headers)
	res.End([]byte(text))
}

// jsonReplyText replies with a pre-serialized JSON body.
func jsonReplyText(res *Response, status int, text string) {
	headers := map[string]string{
		"content-type":        "application/json; charset=utf-8",
		"x-github-request-id": nextRequestID(),
		"content-length":      strconv.Itoa(len(text)),
	}
	res.WriteHead(status, headers)
	res.End([]byte(text))
}

func errReply(res *Response, status int, message string) {
	jsonReply(res, status, map[string]string{"message": message}, nil)
}

type app struct {
	store *Store
}

func newApp() *app { return &app{store: NewStore()} }

var gitRouteRe = regexp.MustCompile(`^/([^/]+)/([^/]+)\.git/(.+)$`)

func (a *app) dispatch(req *Request, res *Response) {
	// The store is single-threaded by design (like the Node original):
	// one lock around the whole dispatch keeps every surface consistent.
	a.store.mu.Lock()
	defer a.store.mu.Unlock()

	method, path := req.Method, req.Path
	host := req.Headers["host"]
	if host == "" {
		host = "127.0.0.1"
	}

	if method == "GET" && path == "/healthz" {
		jsonReply(res, 200, map[string]bool{"ok": true}, nil)
		return
	}
	if !hasAuth(req) {
		errReply(res, 401, "Requires authentication")
		return
	}

	if path == "/mcp" {
		if method != "POST" {
			errReply(res, 405, "Method Not Allowed")
			return
		}
		status, body, headers := handleMcp(a.store, host, req.Body)
		headers["x-github-request-id"] = nextRequestID()
		res.WriteHead(status, headers)
		res.End([]byte(body))
		return
	}

	if path == "/api/graphql" {
		if method != "POST" {
			errReply(res, 405, "Method Not Allowed")
			return
		}
		var parsed any
		if err := json.Unmarshal(req.Body, &parsed); err != nil {
			jsonReply(res, 400, map[string]any{"errors": []any{map[string]string{"message": "request body is not valid JSON"}}}, nil)
			return
		}
		status, payload := handleGraphQL(a.store, parsed)
		jsonReply(res, status, payload, nil)
		return
	}

	if m := gitRouteRe.FindStringSubmatch(path); m != nil {
		owner, repoName, suffix := decodeParam(m[1]), decodeParam(m[2]), m[3]
		switch {
		case suffix == "info/refs" && method == "GET":
			handleInfoRefs(a.store, owner, repoName, req, res, req.Query.Get("service"))
		case suffix == "git-upload-pack" && method == "POST":
			handleUploadPack(a.store, owner, repoName, req, res, req.Body)
		case suffix == "git-receive-pack" && method == "POST":
			handleReceivePack(a.store, owner, repoName, req, res, req.Body)
		default:
			errReply(res, 404, "Not Found")
		}
		return
	}

	if path == "/api/v3" || strings.HasPrefix(path, "/api/v3/") {
		var parsed any
		if method == "POST" || method == "PUT" || method == "PATCH" || method == "DELETE" {
			if len(req.Body) > 0 {
				if err := json.Unmarshal(req.Body, &parsed); err != nil {
					errReply(res, 400, "request body is not valid JSON")
					return
				}
			}
		}
		ctx := &restCtx{store: a.store, req: req, res: res, body: parsed, host: host}
		if handleRest(ctx, method, path) {
			return
		}
		errReply(res, 404, "Not Found")
		return
	}

	errReply(res, 404, "Not Found")
}

// decodeParam decodes %XX, returning the input on error.
func decodeParam(s string) string {
	if d, err := url.PathUnescape(s); err == nil {
		return d
	}
	return s
}
