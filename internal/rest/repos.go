package rest

import (
	"encoding/base64"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"forge/internal/store"
)

// handleRepoRoute dispatches /api/v3/repos/:owner/:repo[/...]. It mirrors the
// rest.ts block order: repo itself, single branch, git refs, branch list,
// commits, trees, blobs, contents, issues, labels, pulls.
func handleRepoRoute(ctx RestContext, u *url.URL, owner, repoName string, rest []string, page, perPage int) RestResponse {
	s := ctx.Store
	method := ctx.Method
	query := queryOf(u)

	// GET/DELETE/PATCH /repos/:owner/:repo
	if len(rest) == 0 {
		if method == "GET" {
			repo, resp := requireViewerRepo(ctx, owner, repoName)
			if resp != nil {
				return *resp
			}
			return JSONBody(200, repoShape(repo))
		}
		if method == "DELETE" {
			if owner != viewerLogin {
				return JSONError(404, "Not Found")
			}
			if !s.DeleteRepo(owner, repoName) {
				return JSONError(404, "Not Found")
			}
			return empty(204)
		}
		if method == "PATCH" {
			repo, resp := requireViewerRepo(ctx, owner, repoName)
			if resp != nil {
				return *resp
			}
			parsed, parseResp := ParseJSONBody(ctx.Body)
			if parseResp != nil {
				return *parseResp
			}
			if value, ok := isRecord(parsed); ok {
				if desc, ok := value["description"].(string); ok {
					repo.Description = desc
				}
				if priv, ok := value["private"].(bool); ok {
					repo.IsPrivate = priv
				}
			}
			return JSONBody(200, repoShape(repo))
		}
		return JSONError(404, "Not Found")
	}

	repo, resp := requireViewerRepo(ctx, owner, repoName)
	if resp != nil {
		return *resp
	}

	// GET /repos/:owner/:repo/branches/:branch — a single branch.
	if len(rest) == 2 && rest[0] == "branches" {
		if method != "GET" {
			return JSONError(404, "Not Found")
		}
		sha, ok := repo.Refs["refs/heads/"+rest[1]]
		if !ok {
			return JSONError(404, "Not Found")
		}
		return JSONBody(200, map[string]any{
			"name": rest[1],
			"commit": map[string]any{
				"sha": sha,
				"url": "https://github.local/api/v3/repos/" + owner + "/" + repoName + "/commits/" + sha,
			},
			"protected": false,
		})
	}

	// Git Data refs: POST /git/refs (create), GET /git/refs (list),
	// GET /git/refs/:ref...
	if len(rest) >= 2 && rest[0] == "git" && rest[1] == "refs" {
		return handleGitRefs(ctx, repo, owner, repoName, rest, u, page, perPage)
	}

	// GET /repos/:owner/:repo/branches
	if len(rest) == 1 && rest[0] == "branches" {
		if method != "GET" {
			return JSONError(404, "Not Found")
		}
		branches := []map[string]any{}
		for name, sha := range repo.Refs {
			if !strings.HasPrefix(name, "refs/heads/") {
				continue
			}
			branches = append(branches, map[string]any{
				"name": name[len("refs/heads/"):],
				"commit": map[string]any{
					"sha": sha,
					"url": "https://github.local/api/v3/repos/" + owner + "/" + repoName + "/commits/" + sha,
				},
				"protected": false,
			})
		}
		sort.Slice(branches, func(i, j int) bool { return branches[i]["name"].(string) < branches[j]["name"].(string) })
		pageItems, headers := paginate(u, branches, page, perPage)
		return JSONBody(200, pageItems, headers)
	}

	// GET /repos/:owner/:repo/commits[/:sha]
	if len(rest) >= 1 && rest[0] == "commits" {
		return handleCommits(ctx, repo, owner, repoName, rest, u, page, perPage)
	}

	// GET /repos/:owner/:repo/git/trees/:sha
	if len(rest) == 3 && rest[0] == "git" && rest[1] == "trees" {
		if method != "GET" {
			return JSONError(404, "Not Found")
		}
		sha := rest[2]
		object, ok := repo.Objects[sha]
		if !ok || object.Type != store.ObjectTree {
			return JSONError(404, "Not Found")
		}
		recursive := query.Get("recursive") == "1"
		entries := []map[string]any{}
		var walk func(treeOID, prefix string)
		walk = func(treeOID, prefix string) {
			treeObject, ok := repo.Objects[treeOID]
			if !ok || treeObject.Type != store.ObjectTree {
				return
			}
			for _, entry := range store.ParseTree(treeObject.Data) {
				pathOut := entry.Name
				if prefix != "" {
					pathOut = prefix + "/" + entry.Name
				}
				isTree := entry.Mode == "40000"
				item := map[string]any{
					"path": pathOut,
					"mode": entry.Mode,
					"type": map[bool]string{true: "tree", false: "blob"}[isTree],
					"sha":  entry.OID,
					"url":  "https://github.local/api/v3/repos/" + owner + "/" + repoName + "/git/" + map[bool]string{true: "trees", false: "blobs"}[isTree] + "/" + entry.OID,
				}
				if !isTree {
					size := 0
					if blob, ok := repo.Objects[entry.OID]; ok {
						size = len(blob.Data)
					}
					item["size"] = size
				}
				entries = append(entries, item)
				if recursive && isTree {
					walk(entry.OID, pathOut)
				}
			}
		}
		walk(sha, "")
		return JSONBody(200, map[string]any{
			"sha":       sha,
			"url":       "https://github.local/api/v3/repos/" + owner + "/" + repoName + "/git/trees/" + sha,
			"tree":      entries,
			"truncated": false,
		})
	}

	// GET /repos/:owner/:repo/git/blobs/:sha
	if len(rest) == 3 && rest[0] == "git" && rest[1] == "blobs" {
		if method != "GET" {
			return JSONError(404, "Not Found")
		}
		sha := rest[2]
		object, ok := repo.Objects[sha]
		if !ok || object.Type != store.ObjectBlob {
			return JSONError(404, "Not Found")
		}
		return JSONBody(200, map[string]any{
			"sha":      sha,
			"node_id":  store.GlobalID("Blob", repo.DBID, sha),
			"size":     len(object.Data),
			"url":      "https://github.local/api/v3/repos/" + owner + "/" + repoName + "/git/blobs/" + sha,
			"content":  base64.StdEncoding.EncodeToString(object.Data),
			"encoding": "base64",
		})
	}

	// /repos/:owner/:repo/contents/:path... — GET, PUT, DELETE
	if len(rest) >= 1 && rest[0] == "contents" {
		return handleContents(ctx, repo, owner, repoName, rest)
	}

	// /repos/:owner/:repo/issues[/:number[/comments|/labels]] and /labels
	if len(rest) >= 1 && rest[0] == "issues" {
		return handleIssues(ctx, repo, owner, repoName, rest, u, page, perPage)
	}

	// /repos/:owner/:repo/labels — list + create (idempotent: an existing
	// name returns 201).
	if len(rest) == 1 && rest[0] == "labels" {
		return handleRepoLabels(ctx, repo, u, page, perPage)
	}

	// /repos/:owner/:repo/pulls[/:number[/merge|/files|/commits]]
	if len(rest) >= 1 && rest[0] == "pulls" {
		return handlePulls(ctx, repo, owner, repoName, rest, u, page, perPage)
	}

	return JSONError(404, "Not Found")
}

func refShape(owner, repoName string, repo *store.Repo, ref, sha string) map[string]any {
	return map[string]any{
		"ref":     ref,
		"node_id": store.GlobalID("Ref", repo.DBID, ref),
		"url":     "https://github.local/api/v3/repos/" + owner + "/" + repoName + "/git/refs/" + ref,
		"object": map[string]any{
			"sha":  sha,
			"type": "commit",
			"url":  "https://github.local/api/v3/repos/" + owner + "/" + repoName + "/git/commits/" + sha,
		},
	}
}

func handleGitRefs(ctx RestContext, repo *store.Repo, owner, repoName string, rest []string, u *url.URL, page, perPage int) RestResponse {
	method := ctx.Method
	if method == "POST" && len(rest) == 2 {
		parsed, parseResp := ParseJSONBody(ctx.Body)
		if parseResp != nil {
			return *parseResp
		}
		value, _ := isRecord(parsed)
		ref := asString(value["ref"])
		sha := asString(value["sha"])
		if !strings.HasPrefix(ref, "refs/") || !store.IsObjectID(sha) {
			return validationError("Validation Failed", []map[string]any{
				{"resource": "Reference", "field": "ref", "code": "invalid"},
			})
		}
		object, ok := repo.Objects[sha]
		if !ok || object.Type != store.ObjectCommit {
			return validationError("Validation Failed", []map[string]any{
				{"resource": "Reference", "field": "sha", "code": "invalid"},
			})
		}
		if _, exists := repo.Refs[ref]; exists {
			return validationError("Validation Failed", []map[string]any{
				{"resource": "Reference", "field": "ref", "code": "custom", "message": "Reference already exists"},
			})
		}
		repo.Refs[ref] = sha
		return JSONBody(201, refShape(owner, repoName, repo, ref, sha))
	}
	if method == "GET" {
		if len(rest) == 2 {
			refs := []map[string]any{}
			for name, sha := range repo.Refs {
				refs = append(refs, refShape(owner, repoName, repo, name, sha))
			}
			sort.Slice(refs, func(i, j int) bool { return refs[i]["ref"].(string) < refs[j]["ref"].(string) })
			pageItems, headers := paginate(u, refs, page, perPage)
			return JSONBody(200, pageItems, headers)
		}
		refPath := "refs/" + strings.Join(rest[2:], "/")
		sha, ok := repo.Refs[refPath]
		if !ok {
			return JSONError(404, "Not Found")
		}
		return JSONBody(200, refShape(owner, repoName, repo, refPath, sha))
	}
	return JSONError(404, "Not Found")
}

// parseIdent splits a "Name <email> ..." git ident the way rest.ts does:
// name is the part before " <"; email is the part after the first '<' with
// its first '>' removed. NOTE the TS quirk this reproduces: for a full
// author line "ada iversen <ada@example.invalid> 1700000001 +0000" the
// email comes out as "ada@example.invalid 1700000001 +0000" (only the
// first '>' is stripped, the trailing timestamp stays).
func parseIdent(s string) (name, email string) {
	name = strings.Split(s, " <")[0]
	parts := strings.Split(s, "<")
	if len(parts) < 2 {
		return name, ""
	}
	return name, strings.Replace(parts[1], ">", "", 1)
}

func commitAuthorShape(c store.ParsedCommit, repo *store.Repo, withEmail bool) map[string]any {
	authorName, authorEmail := parseIdent(c.Author)
	committerName, committerEmail := parseIdent(c.Committer)
	if !withEmail {
		authorEmail = ""
		committerEmail = ""
	}
	return map[string]any{
		"author": map[string]any{
			"name":  authorName,
			"email": authorEmail,
			"date":  repo.CreatedSeq,
		},
		"committer": map[string]any{
			"name":  committerName,
			"email": committerEmail,
			"date":  repo.CreatedSeq,
		},
		"message": c.Message,
		"tree": map[string]any{
			"sha": c.Tree,
			"url": "https://github.local/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/git/trees/" + c.Tree,
		},
	}
}

func commitParents(c store.ParsedCommit, repo *store.Repo) []map[string]any {
	parents := make([]map[string]any, 0, len(c.Parents))
	for _, parent := range c.Parents {
		parents = append(parents, map[string]any{
			"sha": parent,
			"url": "https://github.local/api/v3/repos/" + repo.Owner + "/" + repo.Name + "/commits/" + parent,
		})
	}
	return parents
}

func handleCommits(ctx RestContext, repo *store.Repo, owner, repoName string, rest []string, u *url.URL, page, perPage int) RestResponse {
	if ctx.Method != "GET" {
		return JSONError(404, "Not Found")
	}
	query := queryOf(u)
	if len(rest) == 2 {
		sha := rest[1]
		object, ok := repo.Objects[sha]
		if !ok || object.Type != store.ObjectCommit {
			return JSONError(404, "Not Found")
		}
		commit := store.ParseCommit(object.Data)
		return JSONBody(200, map[string]any{
			"sha":       sha,
			"node_id":   store.GlobalID("Commit", repo.DBID, sha),
			"commit":    commitAuthorShape(commit, repo, true),
			"author":    userShape(viewerLogin),
			"committer": userShape(viewerLogin),
			"parents":   commitParents(commit, repo),
		})
	}
	branch := queryDefault(query, "sha", repo.DefaultBranch)
	tip, ok := repo.Refs["refs/heads/"+branch]
	commits := []map[string]any{}
	if ok {
		seen := map[string]bool{}
		queue := []string{tip}
		for len(queue) > 0 && len(commits) < 100 {
			oid := queue[0]
			queue = queue[1:]
			if seen[oid] {
				continue
			}
			seen[oid] = true
			object, ok := repo.Objects[oid]
			if !ok || object.Type != store.ObjectCommit {
				continue
			}
			commit := store.ParseCommit(object.Data)
			commits = append(commits, map[string]any{
				"sha":       oid,
				"node_id":   store.GlobalID("Commit", repo.DBID, oid),
				"commit":    commitAuthorShape(commit, repo, false),
				"author":    userShape(viewerLogin),
				"committer": userShape(viewerLogin),
				"parents":   commitParents(commit, repo),
			})
			queue = append(queue, commit.Parents...)
		}
	}
	pageItems, headers := paginate(u, commits, page, perPage)
	return JSONBody(200, pageItems, headers)
}

// parseIssueNumber mimics Number(rest[1]) + Number.isInteger: "1" -> 1,
// "abc"/"1.5"/"Infinity" -> not a number, "" -> 0.
func parseIssueNumber(s string) (int, bool) {
	// Fast path: plain digits (the common case) skip the float parse.
	if s != "" && len(s) <= 9 {
		allDigits := true
		for i := 0; i < len(s); i++ {
			if s[i] < '0' || s[i] > '9' {
				allDigits = false
				break
			}
		}
		if allDigits {
			n, err := strconv.Atoi(s)
			if err == nil {
				return n, true
			}
		}
	}
	f, ok := jsNumber(s)
	if !ok || f != float64(int(f)) || f < -2147483648 || f > 2147483647 {
		return 0, false
	}
	// jsNumber("") == 0 is integer; the caller looks the issue up and 404s.
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		// Handles "1e2"-style numbers JS accepts; Atoi rejects them.
		return int(f), true
	}
	return n, true
}
