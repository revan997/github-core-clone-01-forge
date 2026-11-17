package rest

import (
	"net/url"
	"sort"
	"strings"

	"forge/internal/store"
)

// handleIssues serves /repos/:owner/:repo/issues[/:number[/comments|/labels]].
func handleIssues(ctx RestContext, repo *store.Repo, owner, repoName string, rest []string, u *url.URL, page, perPage int) RestResponse {
	s := ctx.Store
	method := ctx.Method
	query := u.Query()

	if len(rest) == 1 {
		if method == "GET" {
			issues := make([]*store.Issue, 0, len(repo.Issues))
			for _, issue := range repo.Issues {
				issues = append(issues, issue)
			}
			state := queryDefault(query, "state", "open")
			if state != "all" {
				filtered := issues[:0]
				for _, issue := range issues {
					if issue.State == state {
						filtered = append(filtered, issue)
					}
				}
				issues = filtered
			}
			if labelsParam := query.Get("labels"); labelsParam != "" {
				var wanted []string
				for _, item := range strings.Split(labelsParam, ",") {
					if item = strings.TrimSpace(item); item != "" {
						wanted = append(wanted, item)
					}
				}
				filtered := issues[:0]
				for _, issue := range issues {
					ok := true
					for _, name := range wanted {
						found := false
						for _, label := range issue.Labels {
							if label.Name == name {
								found = true
								break
							}
						}
						if !found {
							ok = false
							break
						}
					}
					if ok {
						filtered = append(filtered, issue)
					}
				}
				issues = filtered
			}
			// sort=created&direction=asc orders by creation (number asc);
			// the default is newest first.
			direction := queryDefault(query, "direction", "desc")
			sort.Slice(issues, func(i, j int) bool {
				if direction == "asc" {
					return issues[i].Number < issues[j].Number
				}
				return issues[i].Number > issues[j].Number
			})
			pageItems, headers := paginate(u, issues, page, perPage)
			return JSONBody(200, issueListJSON(repo, pageItems), headers)
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
					{"resource": "Issue", "field": "title", "code": "missing_field"},
				})
			}
			var labels []string
			if arr, ok := value["labels"].([]any); ok {
				for _, item := range arr {
					if str, ok := item.(string); ok {
						labels = append(labels, str)
					}
				}
			}
			// An unknown label is a 422 (GitHub rejects labels that do not
			// exist on the repo).
			for _, name := range labels {
				if _, ok := repo.Labels[name]; !ok {
					return validationError("Validation Failed", []map[string]any{
						{"resource": "Issue", "code": "custom", "field": "labels", "message": "label " + name + " does not exist"},
					})
				}
			}
			issue := s.CreateIssue(repo, title, asString(value["body"]), labels)
			return issueResponse(repo, issue, 201)
		}
		return JSONError(404, "Not Found")
	}

	number, ok := parseIssueNumber(rest[1])
	if !ok {
		return JSONError(404, "Not Found")
	}
	issue, ok := repo.Issues[number]
	if !ok {
		return JSONError(404, "Not Found")
	}

	if len(rest) == 2 {
		if method == "GET" {
			return issueResponse(repo, issue, 200)
		}
		if method == "PATCH" {
			parsed, parseResp := ParseJSONBody(ctx.Body)
			if parseResp != nil {
				return *parseResp
			}
			value, _ := isRecord(parsed)
			if title, ok := value["title"].(string); ok {
				issue.Title = title
			}
			if body, ok := value["body"].(string); ok {
				issue.Body = body
			}
			if state, ok := value["state"].(string); ok && (state == "open" || state == "closed") {
				issue.State = state
				if state == "closed" {
					closedAt := s.SeqTimestamp()
					issue.ClosedAt = &closedAt
				} else {
					issue.ClosedAt = nil
				}
			}
			issue.UpdatedAt = s.SeqTimestamp()
			s.TouchIssue(issue)
			return issueResponse(repo, issue, 200)
		}
		return JSONError(404, "Not Found")
	}

	if len(rest) == 3 && rest[2] == "comments" {
		if method == "GET" {
			pageItems, headers := paginate(u, issue.Comments, page, perPage)
			shapes := make([]map[string]any, 0, len(pageItems))
			for _, comment := range pageItems {
				shapes = append(shapes, commentShape(repo, issue, comment))
			}
			return JSONBody(200, shapes, headers)
		}
		if method == "POST" {
			parsed, parseResp := ParseJSONBody(ctx.Body)
			if parseResp != nil {
				return *parseResp
			}
			value, _ := isRecord(parsed)
			bodyText := asString(value["body"])
			if bodyText == "" {
				return validationError("Validation Failed", []map[string]any{
					{"resource": "IssueComment", "field": "body", "code": "missing_field"},
				})
			}
			comment := s.AddComment(repo, issue, bodyText)
			return JSONBody(201, commentShape(repo, issue, comment))
		}
		return JSONError(404, "Not Found")
	}

	if len(rest) == 3 && rest[2] == "labels" {
		if method == "GET" {
			return JSONBody(200, labelShapes(issue.Labels))
		}
		if method == "POST" || method == "PUT" {
			parsed, parseResp := ParseJSONBody(ctx.Body)
			if parseResp != nil {
				return *parseResp
			}
			var names []string
			if arr, ok := parsed.([]any); ok {
				for _, item := range arr {
					if str, ok := item.(string); ok {
						names = append(names, str)
					}
				}
			} else if value, ok := isRecord(parsed); ok {
				if arr, ok := value["labels"].([]any); ok {
					for _, item := range arr {
						if str, ok := item.(string); ok {
							names = append(names, str)
						}
					}
				}
			}
			// Unknown names are silently dropped (unlike issue creation).
			newLabels := make([]store.Label, 0, len(names))
			for _, name := range names {
				if label, ok := repo.Labels[name]; ok {
					newLabels = append(newLabels, label)
				}
			}
			issue.Labels = newLabels
			issue.UpdatedAt = s.SeqTimestamp()
			s.TouchIssue(issue)
			return JSONBody(200, labelShapes(issue.Labels))
		}
		return JSONError(404, "Not Found")
	}

	return JSONError(404, "Not Found")
}

// handleRepoLabels serves /repos/:owner/:repo/labels — list + create
// (idempotent: an existing name returns 201).
func handleRepoLabels(ctx RestContext, repo *store.Repo, u *url.URL, page, perPage int) RestResponse {
	if ctx.Method == "GET" {
		labels := orderedLabels(repo)
		pageItems, headers := paginate(u, labels, page, perPage)
		return JSONBody(200, labelShapes(pageItems), headers)
	}
	if ctx.Method == "POST" {
		parsed, parseResp := ParseJSONBody(ctx.Body)
		if parseResp != nil {
			return *parseResp
		}
		value, _ := isRecord(parsed)
		name := asString(value["name"])
		if name == "" {
			return validationError("Validation Failed", []map[string]any{
				{"resource": "Label", "field": "name", "code": "missing_field"},
			})
		}
		if existing, ok := repo.Labels[name]; ok {
			return JSONBody(201, labelShape(existing))
		}
		label := store.Label{Name: name, Color: asString(value["color"], "ededed"), Description: asString(value["description"])}
		repo.Labels[name] = label
		return JSONBody(201, labelShape(label))
	}
	return JSONError(404, "Not Found")
}
