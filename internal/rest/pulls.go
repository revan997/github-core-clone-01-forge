package rest

import (
	"net/url"
	"sort"
	"strings"

	"forge/internal/store"
)

// handlePulls serves /repos/:owner/:repo/pulls[/:number[/merge|/files|/commits]].
func handlePulls(ctx RestContext, repo *store.Repo, owner, repoName string, rest []string, u *url.URL, page, perPage int) RestResponse {
	s := ctx.Store
	method := ctx.Method
	query := queryOf(u)

	if len(rest) == 1 {
		if method == "GET" {
			pulls := make([]*store.PullRequest, 0, len(repo.Pulls))
			for _, pull := range repo.Pulls {
				pulls = append(pulls, pull)
			}
			state := queryDefault(query, "state", "open")
			if state != "all" {
				filtered := pulls[:0]
				for _, pull := range pulls {
					if pull.State == state {
						filtered = append(filtered, pull)
					}
				}
				pulls = filtered
			}
			sort.Slice(pulls, func(i, j int) bool { return pulls[i].Number > pulls[j].Number })
			pageItems, headers := paginate(u, pulls, page, perPage)
			shapes := make([]map[string]any, 0, len(pageItems))
			for _, pull := range pageItems {
				shapes = append(shapes, pullShape(repo, pull))
			}
			return JSONBody(200, shapes, headers)
		}
		if method == "POST" {
			parsed, parseResp := ParseJSONBody(ctx.Body)
			if parseResp != nil {
				return *parseResp
			}
			value, _ := isRecord(parsed)
			title := asString(value["title"])
			if title == "" {
				return validationError("Validation Failed", []map[string]any{
					{"resource": "PullRequest", "field": "title", "code": "missing_field"},
				})
			}
			head := lastColonPart(asString(value["head"]))
			base := lastColonPart(asString(value["base"]))
			created, message, ok := s.CreatePull(repo, title, asString(value["body"]), head, base)
			if !ok {
				return validationError("Validation Failed", []map[string]any{
					{"resource": "PullRequest", "field": "head", "code": "custom", "message": message},
				})
			}
			return JSONBody(201, pullShape(repo, created))
		}
		return JSONError(404, "Not Found")
	}

	number, ok := parseIssueNumber(rest[1])
	if !ok {
		return JSONError(404, "Not Found")
	}
	pull, ok := repo.Pulls[number]
	if !ok {
		return JSONError(404, "Not Found")
	}

	if len(rest) == 2 {
		if method == "GET" {
			return JSONBody(200, pullShape(repo, pull))
		}
		if method == "PATCH" {
			parsed, parseResp := ParseJSONBody(ctx.Body)
			if parseResp != nil {
				return *parseResp
			}
			value, _ := isRecord(parsed)
			if title, ok := value["title"].(string); ok {
				pull.Title = title
			}
			if body, ok := value["body"].(string); ok {
				pull.Body = body
			}
			if state, ok := value["state"].(string); ok && (state == "open" || state == "closed") {
				pull.State = state
			}
			pull.UpdatedAt = s.SeqTimestamp()
			return JSONBody(200, pullShape(repo, pull))
		}
		return JSONError(404, "Not Found")
	}

	if len(rest) == 3 && rest[2] == "merge" {
		if method != "PUT" && method != "POST" {
			return JSONError(404, "Not Found")
		}
		sha, message, ok := s.MergePull(repo, pull)
		if !ok {
			return JSONBody(405, map[string]any{"message": message})
		}
		return JSONBody(200, map[string]any{
			"merged":  true,
			"sha":     sha,
			"message": "Pull Request successfully merged",
		})
	}

	if len(rest) == 3 && rest[2] == "files" {
		if method != "GET" {
			return JSONError(404, "Not Found")
		}
		files := []map[string]any{}
		for _, file := range s.PullFiles(repo, pull) {
			diff := fileLineDiff(s, repo, pull, file.Filename, file.Status)
			files = append(files, map[string]any{
				"sha":          pull.HeadSHA,
				"filename":     file.Filename,
				"status":       file.Status,
				"additions":    diff.additions,
				"deletions":    diff.deletions,
				"changes":      diff.additions + diff.deletions,
				"blob_url":     "https://github.local/" + owner + "/" + repoName + "/blob/" + pull.HeadSHA + "/" + file.Filename,
				"raw_url":      "https://github.local/" + owner + "/" + repoName + "/raw/" + pull.HeadSHA + "/" + file.Filename,
				"contents_url": "https://github.local/api/v3/repos/" + owner + "/" + repoName + "/contents/" + file.Filename + "?ref=" + pull.HeadSHA,
			})
		}
		pageItems, headers := paginate(u, files, page, perPage)
		return JSONBody(200, pageItems, headers)
	}

	if len(rest) == 3 && rest[2] == "commits" {
		if method != "GET" {
			return JSONError(404, "Not Found")
		}
		commits := []map[string]any{}
		seen := map[string]bool{}
		queue := []string{pull.HeadSHA}
		for len(queue) > 0 {
			oid := queue[0]
			queue = queue[1:]
			if seen[oid] || oid == pull.BaseSHA {
				continue
			}
			seen[oid] = true
			object, ok := repo.Objects[oid]
			if !ok || object.Type != store.ObjectCommit {
				continue
			}
			commit := store.ParseCommit(object.Data)
			parents := make([]map[string]any, 0, len(commit.Parents))
			for _, parent := range commit.Parents {
				parents = append(parents, map[string]any{"sha": parent})
			}
			commits = append(commits, map[string]any{
				"sha": oid,
				"commit": map[string]any{
					"message": commit.Message,
					"tree":    map[string]any{"sha": commit.Tree},
				},
				"parents": parents,
			})
			queue = append(queue, commit.Parents...)
		}
		return JSONBody(200, commits)
	}

	return JSONError(404, "Not Found")
}

// lastColonPart is TS's `value.split(':').pop()`: "user:branch" -> "branch".
func lastColonPart(s string) string {
	if i := strings.LastIndexByte(s, ':'); i >= 0 {
		return s[i+1:]
	}
	return s
}

type lineDiff struct {
	additions int
	deletions int
}

// fileLineDiff computes line additions/deletions for one pull-request file
// (multiset line diff).
func fileLineDiff(s *store.Store, repo *store.Repo, pull *store.PullRequest, filename, status string) lineDiff {
	headSha := s.CurrentHeadSha(repo, pull)
	baseSha := s.CurrentBaseSha(repo, pull)
	headTree := map[string]string{}
	if headCommit, ok := repo.Objects[headSha]; ok && headCommit.Type == store.ObjectCommit {
		headTree = s.FlattenTree(repo, store.ParseCommit(headCommit.Data).Tree)
	}
	baseTree := map[string]string{}
	if baseCommit, ok := repo.Objects[baseSha]; ok && baseCommit.Type == store.ObjectCommit {
		baseTree = s.FlattenTree(repo, store.ParseCommit(baseCommit.Data).Tree)
	}
	blobLines := func(oid string) []string {
		if oid == "" {
			return []string{}
		}
		blob, ok := repo.Objects[oid]
		if !ok || blob.Type != store.ObjectBlob {
			return []string{}
		}
		lines := strings.Split(string(blob.Data), "\n")
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		return lines
	}
	headLines := blobLines(headTree[filename])
	baseLines := blobLines(baseTree[filename])
	if status == "added" {
		return lineDiff{additions: max(1, len(headLines)), deletions: 0}
	}
	if status == "removed" {
		return lineDiff{additions: 0, deletions: max(1, len(baseLines))}
	}
	count := func(lines []string) map[string]int {
		out := map[string]int{}
		for _, line := range lines {
			out[line]++
		}
		return out
	}
	headCount := count(headLines)
	baseCount := count(baseLines)
	additions := 0
	for line, n := range headCount {
		if d := n - baseCount[line]; d > 0 {
			additions += d
		}
	}
	deletions := 0
	for line, n := range baseCount {
		if d := n - headCount[line]; d > 0 {
			deletions += d
		}
	}
	// A changed file with identical line multisets (pure reorder) still changed.
	if additions == 0 && deletions == 0 {
		additions = 1
	}
	return lineDiff{additions: additions, deletions: deletions}
}
