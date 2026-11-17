// Package app is a faithful Go port of the Node forge's app.ts: the HTTP server —
// routing, auth, body reading, and the JSON 404 fallback.
//
// Routing order: /healthz (no auth) → /api/graphql → /mcp → /api/v3* → git smart
// HTTP (/<owner>/<repo>.git/info/refs, /git-upload-pack, /git-receive-pack) → JSON 404.
// Handlers never crash the process: a panic becomes a 500 with a JSON body.
// Auth: when MAKEFUN_SHOP_TOKEN is set, the Bearer token must equal it; when unset
// (local development), any non-empty Bearer token is accepted; a missing token is 401.
// Bodies are capped at 64 MiB.
//
// Concurrency: Go's net/http serves requests concurrently, but the Node entry is
// single-threaded. The dispatcher therefore holds Store.Mu for the whole request,
// which reproduces Node's run-to-completion semantics exactly.
package app

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"forge/internal/git"
	"forge/internal/graphql"
	"forge/internal/mcp"
	"forge/internal/rest"
	"forge/internal/store"
)

const maxBodyBytes = 64 * 1024 * 1024

// AuthToken returns the expected bearer token ("" when unset).
func AuthToken() string {
	return os.Getenv("MAKEFUN_SHOP_TOKEN")
}

func bearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	if header == "" || !strings.HasPrefix(header, "Bearer ") {
		return ""
	}
	token := strings.TrimSpace(strings.TrimPrefix(header, "Bearer "))
	return token
}

func authorized(r *http.Request) bool {
	expected := AuthToken()
	presented := bearerToken(r)
	if presented == "" {
		return false
	}
	if expected == "" {
		return true
	}
	return presented == expected
}

func sendJSON(w http.ResponseWriter, status int, body []byte, extraHeaders map[string]string, requestID string) {
	header := w.Header()
	header.Set("content-type", "application/json; charset=utf-8")
	header.Set("x-github-request-id", requestID)
	for k, v := range extraHeaders {
		header.Set(k, v)
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func sendGit(w http.ResponseWriter, response git.GitResponse, requestID string) {
	header := w.Header()
	header.Set("content-type", response.ContentType)
	header.Set("x-github-request-id", requestID)
	w.WriteHeader(response.Status)
	_, _ = w.Write(response.Body)
}

func jsonMessage(message string) []byte {
	body, _ := json.Marshal(map[string]string{"message": message})
	return body
}

// Forge is one forge instance: a store plus its HTTP handler.
type Forge struct {
	Store *store.Store
}

// NewForge creates a forge with a fresh store.
func NewForge() *Forge {
	return &Forge{Store: store.NewStore()}
}

// Handler returns the HTTP handler (route + auth + body cap + panic recovery).
// The store mutex is held for the whole request.
func (f *Forge) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.Store.Mu.Lock()
		defer f.Store.Mu.Unlock()
		requestID := f.Store.NextRequestID()
		defer func() {
			if rec := recover(); rec != nil {
				detail := fmt.Sprintf("%v", rec)
				if !headerSent(w) {
					sendJSON(w, 500, jsonMessage(detail), nil, requestID)
				}
			}
		}()
		f.handleRequest(w, r, requestID)
	})
}

// headerSent reports whether the response headers have already been written.
// Go's net/http does not expose this directly on the ResponseWriter; we track it
// by wrapping — but the dispatcher writes headers exactly once per request, and
// the only panic path is inside handlers before any write, so a plain 500 write
// is safe. This helper exists to mirror the Node code's headersSent guard.
func headerSent(w http.ResponseWriter) bool {
	// The dispatcher never writes a partial response before a handler returns,
	// so by construction headers are unsent when a panic unwinds here.
	return false
}

func (f *Forge) handleRequest(w http.ResponseWriter, r *http.Request, requestID string) {
	rawURL := r.URL.RequestURI()
	if rawURL == "" {
		rawURL = "/"
	}
	host := r.Host
	if host == "" {
		host = "127.0.0.1"
	}
	parsed, err := url.Parse("http://" + host + rawURL)
	if err != nil {
		sendJSON(w, 500, jsonMessage("internal error"), nil, requestID)
		return
	}
	method := strings.ToUpper(r.Method)
	segments := splitSegments(parsed.Path)
	decoded := make([]string, len(segments))
	for i, seg := range segments {
		if unescaped, err := url.PathUnescape(seg); err == nil {
			decoded[i] = unescaped
		} else {
			decoded[i] = seg
		}
	}

	// GET /healthz — the boot probe, no auth.
	if method == "GET" && parsed.Path == "/healthz" {
		sendJSON(w, 200, jsonMessageOK(), nil, requestID)
		return
	}

	// Everything else needs a valid bearer token.
	if !authorized(r) {
		sendJSON(w, 401, jsonMessage("Requires authentication"), nil, requestID)
		return
	}

	var body []byte
	if method != "GET" && method != "HEAD" {
		var err error
		body, err = readBody(r)
		if err != nil {
			// Mirrors the Node code: a body read failure (too large / I/O error)
			// propagates to the 500 handler.
			panic(err)
		}
	}

	// POST /api/graphql
	if parsed.Path == "/api/graphql" {
		if method != "POST" {
			sendJSON(w, 404, jsonMessage("Not Found"), nil, requestID)
			return
		}
		response := graphql.HandleGraphQL(f.Store, body)
		sendJSON(w, response.Status, response.Body, response.Headers, requestID)
		return
	}

	// POST /mcp
	if parsed.Path == "/mcp" {
		if method != "POST" {
			sendJSON(w, 404, jsonMessage("Not Found"), nil, requestID)
			return
		}
		response := mcp.HandleMcp(f.Store, body)
		sendJSON(w, response.Status, response.Body, nil, requestID)
		return
	}

	// /api/v3/*
	if parsed.Path == "/api/v3" || strings.HasPrefix(parsed.Path, "/api/v3/") {
		var restPath []string
		if parsed.Path != "/api/v3" {
			restPath = decoded[2:]
		} else {
			restPath = []string{}
		}
		response := rest.HandleRest(rest.RestContext{Store: f.Store, Method: method, URL: parsed, Path: restPath, Body: body})
		sendJSON(w, response.Status, response.Body, response.Headers, requestID)
		return
	}

	// Git smart HTTP: /<owner>/<repo>.git/info/refs, /git-upload-pack, /git-receive-pack.
	if len(decoded) == 3 && strings.HasSuffix(decoded[1], ".git") {
		owner := decoded[0]
		repoName := strings.TrimSuffix(decoded[1], ".git")
		if decoded[2] == "git-upload-pack" && method == "POST" {
			sendGit(w, git.HandleUploadPack(f.Store, owner, repoName, body), requestID)
			return
		}
		if decoded[2] == "git-receive-pack" && method == "POST" {
			sendGit(w, git.HandleReceivePack(f.Store, owner, repoName, body), requestID)
			return
		}
	}
	if len(decoded) == 4 && strings.HasSuffix(decoded[1], ".git") && decoded[2] == "info" && decoded[3] == "refs" && method == "GET" {
		owner := decoded[0]
		repoName := strings.TrimSuffix(decoded[1], ".git")
		service := parsed.Query().Get("service")
		if service != "git-upload-pack" && service != "git-receive-pack" {
			sendJSON(w, 400, jsonMessage("Not Found"), nil, requestID)
			return
		}
		sendGit(w, git.HandleInfoRefs(f.Store, owner, repoName, service, r.Header.Get("git-protocol")), requestID)
		return
	}

	// Anything else: a JSON 404.
	sendJSON(w, 404, jsonMessage("Not Found"), nil, requestID)
}

func jsonMessageOK() []byte {
	body, _ := json.Marshal(map[string]bool{"ok": true})
	return body
}

func splitSegments(path string) []string {
	parts := strings.Split(path, "/")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func readBody(r *http.Request) ([]byte, error) {
	// The Node code caps at 64 MiB and rejects when exceeded.
	limited := io.LimitReader(r.Body, maxBodyBytes+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > maxBodyBytes {
		return nil, fmt.Errorf("request body too large")
	}
	return body, nil
}
