package rest

import (
	"bytes"
	"encoding/base64"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"

	"forge/internal/store"
)

// handleContents serves /repos/:owner/:repo/contents/:path... — GET, PUT,
// DELETE. rest[0] is "contents"; the file path is rest[1:] joined with "/".
func handleContents(ctx RestContext, repo *store.Repo, owner, repoName string, rest []string) RestResponse {
	s := ctx.Store
	method := ctx.Method
	query := ctx.URL.Query()
	filePath := strings.Join(rest[1:], "/")

	if method == "GET" {
		branch := queryDefault(query, "ref", repo.DefaultBranch)
		tip, ok := repo.Refs["refs/heads/"+branch]
		if !ok {
			return JSONError(404, "Not Found")
		}
		head, ok := repo.Objects[tip]
		if !ok || head.Type != store.ObjectCommit {
			return JSONError(404, "Not Found")
		}
		flat := s.FlattenTree(repo, store.ParseCommit(head.Data).Tree)
		if blobOID, ok := flat[filePath]; ok {
			blob, ok := repo.Objects[blobOID]
			if !ok || blob.Type != store.ObjectBlob {
				return JSONError(404, "Not Found")
			}
			name := filePath
			if i := strings.LastIndexByte(filePath, '/'); i >= 0 {
				name = filePath[i+1:]
			}
			return JSONBody(200, map[string]any{
				"type":         "file",
				"encoding":     "base64",
				"size":         len(blob.Data),
				"name":         name,
				"path":         filePath,
				"content":      base64.StdEncoding.EncodeToString(blob.Data),
				"sha":          blobOID,
				"url":          "https://github.local/api/v3/repos/" + owner + "/" + repoName + "/contents/" + filePath,
				"html_url":     "https://github.local/" + owner + "/" + repoName + "/blob/" + branch + "/" + filePath,
				"git_url":      "https://github.local/api/v3/repos/" + owner + "/" + repoName + "/git/blobs/" + blobOID,
				"download_url": "https://github.local/" + owner + "/" + repoName + "/raw/" + branch + "/" + filePath,
			})
		}
		// Directory listing: immediate children of the path ("" = root).
		prefix := ""
		if filePath != "" {
			prefix = filePath + "/"
		}
		type child struct {
			typ  string
			sha  string
			size int
		}
		children := map[string]child{}
		for candidate, oid := range flat {
			if !strings.HasPrefix(candidate, prefix) {
				continue
			}
			remainder := candidate[len(prefix):]
			if remainder == "" {
				continue
			}
			headSegment := strings.Split(remainder, "/")[0]
			if strings.Contains(remainder, "/") {
				if _, has := children[headSegment]; !has {
					// The subtree's tree oid is found by walking the head tree.
					dirPath := headSegment
					if filePath != "" {
						dirPath = filePath + "/" + headSegment
					}
					if dirOID := findTreeOID(repo, tip, dirPath); dirOID != "" {
						children[headSegment] = child{typ: "dir", sha: dirOID, size: 0}
					}
				}
			} else {
				size := 0
				if obj, ok := repo.Objects[oid]; ok {
					size = len(obj.Data)
				}
				children[headSegment] = child{typ: "file", sha: oid, size: size}
			}
		}
		if len(children) == 0 {
			return JSONError(404, "Not Found")
		}
		listing := []map[string]any{}
		for name, value := range children {
			var downloadURL any
			if value.typ == "file" {
				downloadURL = "https://github.local/" + owner + "/" + repoName + "/raw/" + branch + "/" + prefix + name
			}
			gitKind := "blobs"
			if value.typ == "dir" {
				gitKind = "trees"
			}
			listing = append(listing, map[string]any{
				"type":         value.typ,
				"size":         value.size,
				"name":         name,
				"path":         prefix + name,
				"sha":          value.sha,
				"url":          "https://github.local/api/v3/repos/" + owner + "/" + repoName + "/contents/" + prefix + name,
				"html_url":     "https://github.local/" + owner + "/" + repoName + "/tree/" + branch + "/" + prefix + name,
				"git_url":      "https://github.local/api/v3/repos/" + owner + "/" + repoName + "/git/" + gitKind + "/" + value.sha,
				"download_url": downloadURL,
			})
		}
		sort.Slice(listing, func(i, j int) bool { return listing[i]["name"].(string) < listing[j]["name"].(string) })
		return JSONBody(200, listing)
	}

	if method == "PUT" || method == "DELETE" {
		parsed, parseResp := ParseJSONBody(ctx.Body)
		if parseResp != nil {
			return *parseResp
		}
		value, _ := isRecord(parsed)
		branch := asString(value["branch"], repo.DefaultBranch)
		var flat map[string]string
		if tip, ok := repo.Refs["refs/heads/"+branch]; ok {
			var data []byte
			if obj, ok := repo.Objects[tip]; ok {
				data = obj.Data
			}
			flat = s.FlattenTree(repo, store.ParseCommit(data).Tree)
		} else {
			flat = map[string]string{}
		}
		existing, hasExisting := flat[filePath]
		if method == "PUT" {
			contentB64 := asString(value["content"])
			if contentB64 == "" {
				return validationError("Validation Failed", []map[string]any{
					{"resource": "Content", "field": "content", "code": "missing_field"},
				})
			}
			content := decodeBase64Lenient(contentB64)
			if hasExisting {
				if sha, ok := value["sha"].(string); ok && sha != existing {
					return JSONBody(409, map[string]any{"message": "sha does not match the current blob"})
				}
			} else {
				if sha, ok := value["sha"].(string); ok && sha != "" {
					return JSONBody(409, map[string]any{"message": "sha does not match the current blob"})
				}
			}
			message := asString(value["message"], "Update "+filePath)
			sha := s.CreateCommitOnBranch(repo, branch, filePath, content, message)
			blobOID := store.HashObject(store.ObjectBlob, content)
			status := 200
			if !hasExisting {
				status = 201
			}
			name := filePath
			if i := strings.LastIndexByte(filePath, '/'); i >= 0 {
				name = filePath[i+1:]
			}
			return JSONBody(status, map[string]any{
				"content": map[string]any{
					"name": name, "path": filePath, "sha": blobOID, "size": len(content),
					"url": "", "html_url": "", "git_url": "", "download_url": "",
					"type": "file", "content": contentB64, "encoding": "base64",
				},
				"commit": commitIdentity(repo, sha, message),
			})
		}
		// DELETE
		if !hasExisting {
			return JSONError(404, "Not Found")
		}
		if sha, ok := value["sha"].(string); ok && sha != existing {
			return JSONBody(409, map[string]any{"message": "sha does not match the current blob"})
		}
		message := asString(value["message"], "Delete "+filePath)
		remaining := make(map[string]string, len(flat))
		for k, v := range flat {
			remaining[k] = v
		}
		delete(remaining, filePath)
		// One commit whose tree is the flat map without the file.
		rebuilt := rebuildBranchWithout(repo, branch, remaining, message)
		return JSONBody(200, map[string]any{
			"content": nil,
			"commit":  commitIdentity(repo, rebuilt, message),
		})
	}
	return JSONError(404, "Not Found")
}

// commitIdentity is the {sha, node_id, message, author, committer} object
// the contents endpoints return. The date is the repo's createdSeq, as in
// rest.ts.
func commitIdentity(repo *store.Repo, sha, message string) map[string]any {
	identity := map[string]any{"name": viewerName, "email": viewerEmail, "date": repo.CreatedSeq}
	return map[string]any{
		"sha":       sha,
		"node_id":   store.GlobalID("Commit", repo.DBID, sha),
		"message":   message,
		"author":    identity,
		"committer": identity,
	}
}

// findTreeOID walks the head tree to find a subtree's oid (used for
// directory listings).
func findTreeOID(repo *store.Repo, tip, dirPath string) string {
	head, ok := repo.Objects[tip]
	if !ok || head.Type != store.ObjectCommit {
		return ""
	}
	current := store.ParseCommit(head.Data).Tree
	for _, segment := range strings.Split(dirPath, "/") {
		if segment == "" {
			continue
		}
		object, ok := repo.Objects[current]
		if !ok || object.Type != store.ObjectTree {
			return ""
		}
		found := ""
		for _, entry := range store.ParseTree(object.Data) {
			if entry.Name == segment && entry.Mode == "40000" {
				found = entry.OID
				break
			}
		}
		if found == "" {
			return ""
		}
		current = found
	}
	return current
}

// rebuildBranchWithout rebuilds a branch's tree from a flat map and commits
// it (contents DELETE). Port of rest.ts rebuildBranchWithout.
func rebuildBranchWithout(repo *store.Repo, branch string, flat map[string]string, message string) string {
	ref := "refs/heads/" + branch
	var parents []string
	if current, ok := repo.Refs[ref]; ok {
		parents = []string{current}
	}
	// Rebuild the tree from the flat map by reusing createCommitOnBranch's
	// tree builder through a synthetic empty-then-fill: instead, build the
	// tree directly here (buildTreeFromFlatLocal).
	treeOID := buildTreeFromFlat(repo, flat)
	timestamp := store.CommitEpoch + int64(repo.CommitCounter) + 1
	repo.CommitCounter++
	var buf bytes.Buffer
	buf.WriteString("tree " + treeOID + "\n")
	for _, parent := range parents {
		buf.WriteString("parent " + parent + "\n")
	}
	ts := strconv.FormatInt(timestamp, 10)
	buf.WriteString("author " + viewerName + " <" + viewerEmail + "> " + ts + " +0000\n")
	buf.WriteString("committer " + viewerName + " <" + viewerEmail + "> " + ts + " +0000\n\n" + message)
	commitData := buf.Bytes()
	sha := store.HashObject(store.ObjectCommit, commitData)
	repo.Objects[sha] = store.GitObject{Type: store.ObjectCommit, Data: commitData}
	repo.Refs[ref] = sha
	return sha
}

type flatTreeEntry struct {
	mode string
	name string
	oid  string
}

// buildTreeFromFlat builds nested trees bottom-up from a flat path map.
func buildTreeFromFlat(repo *store.Repo, flat map[string]string) string {
	dirs := map[string]map[string]flatTreeEntry{}
	dirs[""] = map[string]flatTreeEntry{}
	for path, oid := range flat {
		parts := strings.Split(path, "/")
		at := ""
		for i := 0; i < len(parts)-1; i++ {
			part := parts[i]
			next := part
			if at != "" {
				next = at + "/" + part
			}
			if _, ok := dirs[next]; !ok {
				dirs[next] = map[string]flatTreeEntry{}
			}
			parent := dirs[at]
			if _, ok := parent[part]; !ok {
				parent[part] = flatTreeEntry{mode: "40000"}
			}
			at = next
		}
		dirs[at][parts[len(parts)-1]] = flatTreeEntry{mode: "100644", oid: oid}
	}
	var build func(dir string) string
	build = func(dir string) string {
		entries := make([]flatTreeEntry, 0, len(dirs[dir]))
		for name, value := range dirs[dir] {
			if value.mode == "40000" {
				child := name
				if dir != "" {
					child = dir + "/" + name
				}
				entries = append(entries, flatTreeEntry{mode: "40000", name: name, oid: build(child)})
			} else {
				entries = append(entries, flatTreeEntry{mode: value.mode, name: name, oid: value.oid})
			}
		}
		sort.Slice(entries, func(i, j int) bool {
			aKey := entries[i].name
			if entries[i].mode == "40000" {
				aKey += "/"
			}
			bKey := entries[j].name
			if entries[j].mode == "40000" {
				bKey += "/"
			}
			return aKey < bKey
		})
		var buf bytes.Buffer
		for _, entry := range entries {
			buf.WriteString(entry.mode + " " + entry.name + "\x00")
			raw, _ := hex.DecodeString(entry.oid)
			buf.Write(raw)
		}
		bs := buf.Bytes()
		oid := store.HashObject(store.ObjectTree, bs)
		repo.Objects[oid] = store.GitObject{Type: store.ObjectTree, Data: bs}
		return oid
	}
	return build("")
}
