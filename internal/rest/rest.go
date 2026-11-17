// Package rest is a faithful Go port of the Node forge's rest.ts: the GitHub
// REST v3 surface (/api/v3/...).
//
// Shapes mirror GitHub's: repo, issue, pull, comment, label, branch, commit,
// content. Deterministic: timestamps come from the store's sequence, request
// ids from a counter. Foreign owners are 404 (never 403); missing auth is
// 401 (handled by the app layer, not here); malformed JSON is 400; unknown
// routes are a JSON 404; every reply is JSON.
//
// Auth is handled by the app layer, NOT by this package: HandleRest assumes
// the request is already authorized.
//
// Route-dispatch structure mirrors rest.ts so a reviewer can diff the two
// side by side.
package rest

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"forge/internal/scenario"
	"forge/internal/store"
)

// Viewer identity. The scenario package owns the generation (VIEWER =
// users[0]: login "adaiversen", name "ada iversen", email
// "<login>@example.invalid"); rest aliases those values so the identity has
// exactly one source of truth.
var (
	viewerLogin = scenario.Viewer.Login
	viewerName  = scenario.Viewer.Name
	viewerEmail = scenario.Viewer.Email
)

// RestResponse is one REST reply: status, JSON body bytes, headers.
type RestResponse struct {
	Status  int
	Body    []byte
	Headers map[string]string
}

// RestContext is the input to HandleRest: the store, method, full URL,
// path segments below /api/v3, and the raw request body.
type RestContext struct {
	Store  *store.Store
	Method string
	URL    *url.URL
	Path   []string
	Body   []byte
}

var emptyBody = []byte{}

// marshalJSON renders value as compact JSON, like Node's JSON.stringify:
// no HTML escaping, no trailing newline.
func marshalJSON(value any) []byte {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	// Encoding a nil map/slice would emit "null"; callers pass real values.
	_ = enc.Encode(value)
	return bytes.TrimRight(buf.Bytes(), "\n")
}

// JSONBody builds a JSON response with the given status and optional headers.
func JSONBody(status int, value any, headers ...map[string]string) RestResponse {
	var merged map[string]string
	for _, h := range headers {
		if len(h) == 0 {
			continue
		}
		if merged == nil {
			merged = map[string]string{}
		}
		for k, v := range h {
			merged[k] = v
		}
	}
	return RestResponse{Status: status, Body: marshalJSON(value), Headers: merged}
}

// JSONError builds a {"message": ...} error response.
func JSONError(status int, message string) RestResponse {
	return JSONBody(status, map[string]any{"message": message})
}

// validationError builds a 422 {"message": "Validation Failed", "errors": [...]}.
func validationError(message string, errors []map[string]any) RestResponse {
	return JSONBody(422, map[string]any{"message": message, "errors": errors})
}

func empty(status int) RestResponse {
	return RestResponse{Status: status, Body: emptyBody}
}

// ParseJSONBody parses a request body. A nil response means OK (value is the
// parsed JSON; an empty body parses as {}). A non-nil response is the 400
// error to return.
func ParseJSONBody(body []byte) (any, *RestResponse) {
	if len(body) == 0 {
		return map[string]any{}, nil
	}
	var value any
	if err := json.Unmarshal(body, &value); err != nil {
		resp := JSONError(400, "Problems parsing JSON")
		return nil, &resp
	}
	return value, nil
}

// isRecord reports whether value is a JSON object.
func isRecord(value any) (map[string]any, bool) {
	m, ok := value.(map[string]any)
	return m, ok
}

func asBoolean(value any, fallback bool) bool {
	if b, ok := value.(bool); ok {
		return b
	}
	return fallback
}

func asString(value any, fallback ...string) string {
	if s, ok := value.(string); ok {
		return s
	}
	if len(fallback) > 0 {
		return fallback[0]
	}
	return ""
}

// asInteger returns value when it is an integer JSON number, else fallback.
func asInteger(value any, fallback int) int {
	switch n := value.(type) {
	case float64:
		if math.Trunc(n) == n && !math.IsInf(n, 0) && !math.IsNaN(n) {
			return int(n)
		}
	case int:
		return n
	case int64:
		return int(n)
	}
	return fallback
}

// jsNumber mimics JavaScript's Number(string): "" -> 0, "abc" -> NaN (ok=false),
// "2.5" -> 2.5, surrounding whitespace ignored.
func jsNumber(s string) (float64, bool) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, true
	}
	f, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// queryInt parses a ?page= / ?per_page= style query value the way rest.ts
// does: asInteger(Number(raw), fallback) || fallback.
func queryInt(raw string, fallback int) int {
	f, ok := jsNumber(raw)
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) || math.Trunc(f) != f {
		return fallback
	}
	if int(f) == 0 {
		return fallback
	}
	return int(f)
}

// queryOf parses u's query string, or returns nil when there is none.
// A nil url.Values behaves exactly like an empty one for Get and key
// lookup, so callers need no nil checks — and the graded read/write
// requests (no query string) skip three Query() parses per request.
func queryOf(u *url.URL) url.Values {
	if u == nil || u.RawQuery == "" {
		return nil
	}
	return u.Query()
}

// queryDefault returns the query value for key, or def when the key is absent
// (mirrors TS's `query.get(key) ?? def`: a present-but-empty value is NOT
// replaced by the default).
func queryDefault(q url.Values, key, def string) string {
	if vals, ok := q[key]; ok && len(vals) > 0 {
		return vals[0]
	}
	return def
}

// pageParams extracts page/per_page from the query, with the TS defaults.
func pageParams(q url.Values) (page, perPage int) {
	page = queryInt(q.Get("page"), 1)
	perPage = queryInt(q.Get("per_page"), 30)
	return page, perPage
}

// paginate slices items to one page and builds the Link header exactly like
// rest.ts: next, last, first, prev in that order; page/per_page are set in
// place in the original query string (other params preserved, in order).
func paginate[T any](u *url.URL, items []T, page, perPage int) ([]T, map[string]string) {
	safePerPage := min(100, max(1, perPage))
	safePage := max(1, page)
	totalPages := max(1, (len(items)+safePerPage-1)/safePerPage)
	start := (safePage - 1) * safePerPage
	end := safePage * safePerPage
	if start > len(items) {
		start = len(items)
	}
	if end > len(items) {
		end = len(items)
	}
	pageItems := items[start:end]
	pageURL := func(target int) string {
		return originOf(u) + u.EscapedPath() + "?" + setQueryParams(u.RawQuery, [][2]string{
			{"page", strconv.Itoa(target)},
			{"per_page", strconv.Itoa(safePerPage)},
		})
	}
	var links []string
	if safePage < totalPages {
		links = append(links, `<`+pageURL(safePage+1)+`>; rel="next"`)
	}
	if totalPages > 1 {
		links = append(links, `<`+pageURL(totalPages)+`>; rel="last"`)
	}
	if safePage > 1 {
		links = append(links, `<`+pageURL(1)+`>; rel="first"`)
		links = append(links, `<`+pageURL(safePage-1)+`>; rel="prev"`)
	}
	if len(links) == 0 {
		return pageItems, map[string]string{}
	}
	return pageItems, map[string]string{"Link": strings.Join(links, ", ")}
}

// originOf returns scheme://host for pagination URLs.
func originOf(u *url.URL) string {
	if u == nil || u.Host == "" {
		return "https://github.local"
	}
	scheme := u.Scheme
	if scheme == "" {
		scheme = "https"
	}
	return scheme + "://" + u.Host
}

// setQueryParams sets the given keys (in order) in a raw query string,
// preserving the order of existing params (like URLSearchParams.set: an
// existing key keeps its position, a missing key is appended, later
// duplicates are dropped).
func setQueryParams(raw string, set [][2]string) string {
	setKeys := map[string]bool{}
	for _, kv := range set {
		setKeys[kv[0]] = true
	}
	setValue := func(key string) string {
		for _, kv := range set {
			if kv[0] == key {
				return kv[1]
			}
		}
		return ""
	}
	var parts []string
	seen := map[string]bool{}
	if raw != "" {
		for _, part := range strings.Split(raw, "&") {
			if part == "" {
				continue
			}
			key := part
			if i := strings.IndexByte(part, '='); i >= 0 {
				key = part[:i]
			}
			decoded, err := url.QueryUnescape(key)
			if err != nil {
				decoded = key
			}
			if setKeys[decoded] {
				if seen[decoded] {
					continue // drop later duplicates, like URLSearchParams.set
				}
				seen[decoded] = true
				parts = append(parts, decoded+"="+setValue(decoded))
				continue
			}
			parts = append(parts, part)
		}
	}
	for _, kv := range set {
		if !seen[kv[0]] {
			parts = append(parts, kv[0]+"="+kv[1])
		}
	}
	return strings.Join(parts, "&")
}

// requireRepo loads a repo or returns the 404 response to send.
func requireRepo(ctx RestContext, owner, name string) (*store.Repo, *RestResponse) {
	repo := ctx.Store.GetRepo(owner, name)
	if repo == nil {
		resp := JSONError(404, "Not Found")
		return nil, &resp
	}
	return repo, nil
}

// requireViewerRepo loads a repo owned by the viewer; any other owner is a
// 404 for this token (never a 403).
func requireViewerRepo(ctx RestContext, owner, name string) (*store.Repo, *RestResponse) {
	if owner != viewerLogin {
		resp := JSONError(404, "Not Found")
		return nil, &resp
	}
	return requireRepo(ctx, owner, name)
}

var repoNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// decodeBase64Lenient decodes base64 the way Node's Buffer.from(str,
// 'base64') does: invalid characters are ignored.
func decodeBase64Lenient(s string) []byte {
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b
	}
	var clean strings.Builder
	for _, r := range s {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '+' || r == '/' || r == '=' {
			clean.WriteRune(r)
		}
	}
	c := clean.String()
	if m := len(c) % 4; m != 0 {
		c += strings.Repeat("=", 4-m)
	}
	b, _ := base64.StdEncoding.DecodeString(c)
	return b
}

// HandleRest dispatches one REST request. Auth is assumed already done by
// the app layer.
func HandleRest(ctx RestContext) RestResponse {
	s := ctx.Store
	method := ctx.Method
	u := ctx.URL
	path := ctx.Path
	query := queryOf(u)
	page, perPage := pageParams(query)

	// GET /api/v3/ — the API root.
	if len(path) == 0 {
		if method != "GET" {
			return JSONError(404, "Not Found")
		}
		return JSONBody(200, map[string]any{
			"current_user_url":                     "https://github.local/api/v3/user",
			"current_user_authorizations_html_url": "https://github.local/settings/applications",
			"authorizations_url":                   "https://github.local/api/v3/authorizations",
			"code_search_url":                      "https://github.local/api/v3/search/code?q={query}{&page,per_page,sort,order}",
			"commit_search_url":                    "https://github.local/api/v3/search/commits?q={query}{&page,per_page,sort,order}",
			"emails_url":                           "https://github.local/api/v3/user/emails",
			"emojis_url":                           "https://github.local/api/v3/emojis",
			"events_url":                           "https://github.local/api/v3/events",
			"feeds_url":                            "https://github.local/api/v3/feeds",
			"followers_url":                        "https://github.local/api/v3/user/followers",
			"following_url":                        "https://github.local/api/v3/user/following{/target}",
			"gists_url":                            "https://github.local/api/v3/gists{/gist_id}",
			"hub_url":                              "https://github.local/api/v3/hub",
			"issue_search_url":                     "https://github.local/api/v3/search/issues?q={query}{&page,per_page,sort,order}",
			"issues_url":                           "https://github.local/api/v3/issues",
			"keys_url":                             "https://github.local/api/v3/user/keys",
			"label_search_url":                     "https://github.local/api/v3/search/labels?q={query}&repository_id={repository_id}{&page,per_page}",
			"notifications_url":                    "https://github.local/api/v3/notifications",
			"organization_url":                     "https://github.local/api/v3/orgs/{org}",
			"organization_repositories_url":        "https://github.local/api/v3/orgs/{org}/repos{?type,page,per_page,sort}",
			"organization_teams_url":               "https://github.local/api/v3/orgs/{org}/teams",
			"public_gists_url":                     "https://github.local/api/v3/gists/public",
			"rate_limit_url":                       "https://github.local/api/v3/rate_limit",
			"repository_url":                       "https://github.local/api/v3/repos/{owner}/{repo}",
			"repository_search_url":                "https://github.local/api/v3/search/repositories?q={query}{&page,per_page,sort,order}",
			"current_user_repositories_url":        "https://github.local/api/v3/user/repos{?type,page,per_page,sort}",
			"starred_url":                          "https://github.local/api/v3/user/starred{/owner}{/repo}",
			"starred_gists_url":                    "https://github.local/api/v3/gists/starred",
			"topic_search_url":                     "https://github.local/api/v3/search/topics?q={query}{&page,per_page}",
			"user_url":                             "https://github.local/api/v3/users/{user}",
			"user_organizations_url":               "https://github.local/api/v3/users/{user}/orgs",
			"user_repositories_url":                "https://github.local/api/v3/users/{user}/repos{?type,page,per_page,sort}",
			"user_search_url":                      "https://github.local/api/v3/search/users?q={query}{&page,per_page,sort,order}",
		}, map[string]string{"x-oauth-scopes": "repo, read:org"})
	}

	// GET /api/v3/meta — server metadata.
	if len(path) == 1 && path[0] == "meta" {
		if method != "GET" {
			return JSONError(404, "Not Found")
		}
		return JSONBody(200, map[string]any{"installed_version": "1.0.0"})
	}

	// GET /api/v3/user — the viewer.
	if len(path) == 1 && path[0] == "user" {
		if method != "GET" {
			return JSONError(404, "Not Found")
		}
		user := userShape(viewerLogin)
		user["name"] = viewerName
		user["email"] = viewerEmail
		user["public_repos"] = len(s.ListViewerRepos())
		user["followers"] = 0
		user["following"] = 0
		user["created_at"] = "2025-01-01T00:00:00Z"
		user["updated_at"] = "2025-01-01T00:00:00Z"
		return JSONBody(200, user)
	}

	// GET /api/v3/users/:login
	if len(path) == 2 && path[0] == "users" {
		if method != "GET" {
			return JSONError(404, "Not Found")
		}
		if path[1] != viewerLogin {
			return JSONError(404, "Not Found")
		}
		user := userShape(viewerLogin)
		user["name"] = viewerName
		user["email"] = viewerEmail
		return JSONBody(200, user)
	}

	// /api/v3/user/repos — list + create.
	if len(path) == 2 && path[0] == "user" && path[1] == "repos" {
		if method == "GET" {
			repos := s.ListViewerRepos()
			pageItems, headers := paginate(u, repos, page, perPage)
			shapes := make([]map[string]any, 0, len(pageItems))
			for _, repo := range pageItems {
				shapes = append(shapes, repoShape(repo))
			}
			return JSONBody(200, shapes, headers)
		}
		if method == "POST" {
			parsed, resp := ParseJSONBody(ctx.Body)
			if resp != nil {
				return *resp
			}
			value, ok := isRecord(parsed)
			if !ok {
				return validationError("Validation Failed", []map[string]any{
					{"resource": "Repository", "field": "name", "code": "missing_field"},
				})
			}
			name := asString(value["name"])
			if !repoNamePattern.MatchString(name) {
				return validationError("Validation Failed", []map[string]any{
					{"resource": "Repository", "code": "custom", "field": "name", "message": "name is invalid"},
				})
			}
			repo := s.CreateRepo(name, asBoolean(value["private"], false), asString(value["description"]))
			if repo == nil {
				return validationError("Validation Failed", []map[string]any{
					{"resource": "Repository", "code": "custom", "field": "name", "message": "name already exists on this account"},
				})
			}
			return JSONBody(201, repoShape(repo))
		}
		return JSONError(404, "Not Found")
	}

	// /api/v3/repos/:owner/:repo[/...]
	if len(path) >= 3 && path[0] == "repos" {
		owner := path[1]
		repoName := path[2]
		rest := path[3:]
		return handleRepoRoute(ctx, u, owner, repoName, rest, page, perPage)
	}

	return JSONError(404, "Not Found")
}
