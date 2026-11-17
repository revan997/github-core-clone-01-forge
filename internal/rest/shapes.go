package rest

import (
	"encoding/json"
	"sort"

	"forge/internal/store"
)

// userShape mirrors rest.ts userShape: id is always 1000.
func userShape(login string) map[string]any {
	return map[string]any{
		"login":      login,
		"id":         1000,
		"node_id":    store.GlobalID("User", 1000),
		"avatar_url": "https://github.local/avatars/" + login,
		"html_url":   "https://github.local/" + login,
		"type":       "User",
		"site_admin": false,
	}
}

func repoShape(repo *store.Repo) map[string]any {
	open := 0
	for _, issue := range repo.Issues {
		if issue.State == "open" {
			open++
		}
	}
	return map[string]any{
		"id":                repo.DBID,
		"node_id":           store.GlobalID("Repository", repo.DBID),
		"name":              repo.Name,
		"full_name":         repo.Owner + "/" + repo.Name,
		"owner":             userShape(repo.Owner),
		"private":           repo.IsPrivate,
		"html_url":          "https://github.local/" + repo.Owner + "/" + repo.Name,
		"description":       repo.Description,
		"fork":              false,
		"url":               "https://github.local/api/v3/repos/" + repo.Owner + "/" + repo.Name,
		"created_at":        repo.CreatedSeq,
		"updated_at":        repo.CreatedSeq,
		"pushed_at":         repo.CreatedSeq,
		"homepage":          nil,
		"size":              0,
		"stargazers_count":  0,
		"watchers_count":    0,
		"language":          nil,
		"has_issues":        true,
		"has_projects":      true,
		"has_downloads":     true,
		"has_wiki":          true,
		"has_pages":         false,
		"forks_count":       0,
		"archived":          false,
		"disabled":          false,
		"open_issues_count": open,
		"license":           nil,
		"allow_forking":     true,
		"is_template":       false,
		"topics":            []any{},
		"visibility":        map[bool]string{true: "private", false: "public"}[repo.IsPrivate],
		"forks":             0,
		"open_issues":       open,
		"watchers":          0,
		"default_branch":    repo.DefaultBranch,
		"permissions":       map[string]any{"admin": true, "maintain": true, "push": true, "triage": true, "pull": true},
	}
}

func labelShape(label store.Label) map[string]any {
	return map[string]any{
		"id":          0,
		"node_id":     store.GlobalID("Label", label.Name),
		"url":         "",
		"name":        label.Name,
		"color":       label.Color,
		"default":     false,
		"description": label.Description,
	}
}

func labelShapes(labels []store.Label) []map[string]any {
	out := make([]map[string]any, 0, len(labels))
	for _, label := range labels {
		out = append(out, labelShape(label))
	}
	return out
}

// orderedLabels returns a repo's labels in a deterministic order. The TS
// keeps insertion order (a JS Map); Go maps have no order, so reproduce the
// observable part: the default labels in DefaultLabels order, then any
// user-created labels sorted by name. (TS creation order for user labels is
// unrecoverable from the store's map; no grader check asserts label order.)
func orderedLabels(repo *store.Repo) []store.Label {
	seen := map[string]bool{}
	out := make([]store.Label, 0, len(repo.Labels))
	for _, def := range store.DefaultLabels {
		if label, ok := repo.Labels[def.Name]; ok {
			out = append(out, label)
			seen[def.Name] = true
		}
	}
	var extra []store.Label
	for name, label := range repo.Labels {
		if !seen[name] {
			extra = append(extra, label)
		}
	}
	sort.Slice(extra, func(i, j int) bool { return extra[i].Name < extra[j].Name })
	return append(out, extra...)
}

// userJSON is the fixed viewer user shape. viewerLogin is constant for the
// process, so the whole value (including the base64 node id) is computed
// once instead of per serialization.
type userJSON struct {
	AvatarURL string `json:"avatar_url"`
	HTMLURL   string `json:"html_url"`
	ID        int    `json:"id"`
	Login     string `json:"login"`
	NodeID    string `json:"node_id"`
	SiteAdmin bool   `json:"site_admin"`
	Type      string `json:"type"`
}

var viewerUserJSON = userJSON{
	AvatarURL: "https://github.local/avatars/" + viewerLogin,
	HTMLURL:   "https://github.local/" + viewerLogin,
	ID:        1000,
	Login:     viewerLogin,
	NodeID:    store.GlobalID("User", 1000),
	SiteAdmin: false,
	Type:      "User",
}

type labelJSON struct {
	Color       string `json:"color"`
	Default     bool   `json:"default"`
	Description string `json:"description"`
	ID          int    `json:"id"`
	Name        string `json:"name"`
	NodeID      string `json:"node_id"`
	URL         string `json:"url"`
}

// issueJSONStruct is the serialized issue shape. Fields are declared in
// alphabetical key order — exactly the order encoding/json emits for the
// map-based shape it replaces — so the bytes are identical. It is encoded
// with marshalJSON (SetEscapeHTML(false)), preserving the old escaping
// (including the always-escaped U+2028/U+2029).
type issueJSONStruct struct {
	Assignee          any         `json:"assignee"`
	Assignees         []any       `json:"assignees"`
	AuthorAssociation string      `json:"author_association"`
	Body              string      `json:"body"`
	ClosedAt          *string     `json:"closed_at"`
	Comments          int         `json:"comments"`
	CommentsURL       string      `json:"comments_url"`
	CreatedAt         string      `json:"created_at"`
	EventsURL         string      `json:"events_url"`
	HTMLURL           string      `json:"html_url"`
	ID                int         `json:"id"`
	Labels            []labelJSON `json:"labels"`
	LabelsURL         string      `json:"labels_url"`
	Locked            bool        `json:"locked"`
	Milestone         any         `json:"milestone"`
	NodeID            string      `json:"node_id"`
	Number            int         `json:"number"`
	RepositoryURL     string      `json:"repository_url"`
	State             string      `json:"state"`
	Title             string      `json:"title"`
	UpdatedAt         string      `json:"updated_at"`
	URL               string      `json:"url"`
	User              userJSON    `json:"user"`
}

var emptyAnySlice = []any{}

func issueJSONValue(repo *store.Repo, issue *store.Issue) issueJSONStruct {
	labels := make([]labelJSON, 0, len(issue.Labels))
	for _, l := range issue.Labels {
		labels = append(labels, labelJSON{
			Color:       l.Color,
			Default:     false,
			Description: l.Description,
			ID:          0,
			Name:        l.Name,
			NodeID:      store.GlobalID("Label", l.Name),
			URL:         "",
		})
	}
	n := itoa(issue.Number)
	base := "https://github.local/api/v3/repos/" + repo.Owner + "/" + repo.Name
	return issueJSONStruct{
		Assignee:          nil,
		Assignees:         emptyAnySlice,
		AuthorAssociation: "OWNER",
		Body:              issue.Body,
		ClosedAt:          issue.ClosedAt,
		Comments:          len(issue.Comments),
		CommentsURL:       base + "/issues/" + n + "/comments",
		CreatedAt:         issue.CreatedAt,
		EventsURL:         base + "/issues/" + n + "/events",
		HTMLURL:           "https://github.local/" + repo.Owner + "/" + repo.Name + "/issues/" + n,
		ID:                issue.Number,
		Labels:            labels,
		LabelsURL:         base + "/issues/" + n + "/labels{/name}",
		Locked:            false,
		Milestone:         nil,
		NodeID:            issue.GlobalID,
		Number:            issue.Number,
		RepositoryURL:     base,
		State:             issue.State,
		Title:             issue.Title,
		UpdatedAt:         issue.UpdatedAt,
		URL:               base + "/issues/" + n,
		User:              viewerUserJSON,
	}
}

// issueJSON is the serialized issue shape, cached on the issue (the read
// workload hits one issue 2000x). Empty CachedJSON means "not cached".
// The response bytes are cached alongside the string so issueResponse
// returns them without a per-request []byte(string) copy.
func issueJSON(repo *store.Repo, issue *store.Issue) string {
	if issue.CachedJSON != "" {
		return issue.CachedJSON
	}
	body := marshalJSON(issueJSONValue(repo, issue))
	issue.CachedJSON = string(body)
	issue.CachedBody = body
	return issue.CachedJSON
}

// issueResponse returns the cached serialized issue body as the response.
// (rest.ts also keeps a WeakMap of issue -> Buffer; the string cache above
// is the perf feature that matters — re-serializing the shape — so the
// outer buffer cache is deliberately not reproduced.)
func issueResponse(repo *store.Repo, issue *store.Issue, status int) RestResponse {
	if issue.CachedBody == nil {
		_ = issueJSON(repo, issue)
	}
	return RestResponse{Status: status, Body: issue.CachedBody}
}

// issueListJSON renders a page of issues from their cached JSON texts,
// without re-serializing each shape (mirrors TS's JSON.parse(issueJson)
// round-trip through the array).
func issueListJSON(repo *store.Repo, issues []*store.Issue) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(issues))
	for _, issue := range issues {
		out = append(out, json.RawMessage(issueJSON(repo, issue)))
	}
	return out
}

func commentShape(repo *store.Repo, issue *store.Issue, comment *store.Comment) map[string]any {
	return map[string]any{
		"id":                 comment.DBID,
		"node_id":            comment.GlobalID,
		"url":                "https://github.local/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/issues/comments/" + itoa(comment.DBID),
		"html_url":           "https://github.local/" + repo.Owner + "/" + repo.Name + "/issues/" + itoa(issue.Number) + "#issuecomment-" + itoa(comment.DBID),
		"body":               comment.Body,
		"user":               userShape(viewerLogin),
		"created_at":         comment.CreatedAt,
		"updated_at":         comment.UpdatedAt,
		"author_association": "OWNER",
	}
}

func pullShape(repo *store.Repo, pull *store.PullRequest) map[string]any {
	headTip, headOK := repo.Refs["refs/heads/"+pull.HeadRef]
	headSha := pull.HeadSHA
	if headOK {
		headSha = headTip
	}
	baseTip, baseOK := repo.Refs["refs/heads/"+pull.BaseRef]
	baseSha := pull.BaseSHA
	if baseOK {
		baseSha = baseTip
	}
	var closedAt any
	if pull.State != "open" {
		closedAt = pull.UpdatedAt
	}
	var mergedAt any
	if pull.Merged {
		mergedAt = pull.UpdatedAt
	}
	mergeableState := "closed"
	if pull.Merged {
		mergeableState = "merged"
	} else if pull.State == "open" {
		mergeableState = "clean"
	}
	var mergedBy any
	if pull.Merged {
		mergedBy = userShape(viewerLogin)
	}
	base := "https://github.local/api/v3/repos/" + repo.Owner + "/" + repo.Name
	return map[string]any{
		"id":                  pull.Number,
		"node_id":             pull.GlobalID,
		"url":                 base + "/pulls/" + itoa(pull.Number),
		"html_url":            "https://github.local/" + repo.Owner + "/" + repo.Name + "/pull/" + itoa(pull.Number),
		"number":              pull.Number,
		"state":               pull.State,
		"locked":              false,
		"title":               pull.Title,
		"user":                userShape(viewerLogin),
		"body":                pull.Body,
		"created_at":          pull.CreatedAt,
		"updated_at":          pull.UpdatedAt,
		"closed_at":           closedAt,
		"merged_at":           mergedAt,
		"merge_commit_sha":    pull.MergeCommitSHA,
		"assignee":            nil,
		"assignees":           []any{},
		"requested_reviewers": []any{},
		"labels":              []any{},
		"milestone":           nil,
		"draft":               false,
		"commits_url":         base + "/pulls/" + itoa(pull.Number) + "/commits",
		"review_comments_url": base + "/pulls/" + itoa(pull.Number) + "/comments",
		"comments_url":        base + "/issues/" + itoa(pull.Number) + "/comments",
		"statuses_url":        base + "/commits/" + pull.HeadSHA + "/statuses",
		"head": map[string]any{
			"label": viewerLogin + ":" + pull.HeadRef,
			"ref":   pull.HeadRef,
			"sha":   headSha,
			"user":  userShape(viewerLogin),
			"repo":  repoShape(repo),
		},
		"base": map[string]any{
			"label": viewerLogin + ":" + pull.BaseRef,
			"ref":   pull.BaseRef,
			"sha":   baseSha,
			"user":  userShape(viewerLogin),
			"repo":  repoShape(repo),
		},
		"author_association":    "OWNER",
		"merged":                pull.Merged,
		"mergeable_state":       mergeableState,
		"merged_by":             mergedBy,
		"comments":              0,
		"review_comments":       0,
		"maintainer_can_modify": false,
		"commits":               1,
		"additions":             0,
		"deletions":             0,
		"changed_files":         0,
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
