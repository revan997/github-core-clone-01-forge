package git

import (
	"bytes"
	"strings"
	"unicode/utf8"

	"forge/internal/store"
)

// ---------------------------------------------------------------------------------------------
// receive-pack
// ---------------------------------------------------------------------------------------------

type pushCommand struct {
	oldOID string
	newOID string
	ref    string
}

// HandleReceivePack answers POST /<owner>/<repo>.git/git-receive-pack: a
// command list (first line carrying capabilities after NUL), then the pack
// raw. The reply is report-status pkt-lines; when side-band-64k was
// negotiated those lines travel inside band-1 sideband packets. A refused
// ref is `ng <ref> non-fast-forward` and the ref does not move; other refs
// in the same push still apply. Every object lands in the store even when a
// ref is refused (unpack happens first).
func HandleReceivePack(s *store.Store, owner, repoName string, body []byte) GitResponse {
	repo, errResp := requireRepo(s, owner, repoName)
	if errResp != nil {
		return *errResp
	}
	packets, offset := store.DecodePktLines(body)
	var commands []pushCommand
	sideband := false
	sawFlush := false
	for _, packet := range packets {
		if packet.Kind == "flush" {
			sawFlush = true
			break
		}
		if packet.Kind != "data" {
			continue
		}
		text := stripOneNewline(string(packet.Payload))
		nul := strings.Index(text, "\x00")
		commandText := text
		if nul >= 0 {
			commandText = text[:nul]
			caps := strings.Split(text[nul+1:], " ")
			if containsStr(caps, "side-band-64k") {
				sideband = true
			}
		}
		parts := strings.Split(commandText, " ")
		if len(parts) != 3 || !store.IsObjectID(parts[0]) || !store.IsObjectID(parts[1]) {
			return textResponse(400, "malformed push command")
		}
		commands = append(commands, pushCommand{oldOID: parts[0], newOID: parts[1], ref: parts[2]})
	}
	if !sawFlush || len(commands) == 0 {
		return textResponse(400, "malformed push command list")
	}
	packBytes := body[offset:]
	type refResult struct {
		ref    string
		ok     bool
		reason string
	}
	report := func(unpackOk bool, unpackDetail string, refs []refResult) GitResponse {
		var inner [][]byte
		if unpackOk {
			inner = append(inner, store.PktLineString("unpack ok\n"))
		} else {
			inner = append(inner, store.PktLineString("unpack "+unpackDetail+"\n"))
		}
		for _, r := range refs {
			if r.ok {
				inner = append(inner, store.PktLineString("ok "+r.ref+"\n"))
			} else {
				inner = append(inner, store.PktLineString("ng "+r.ref+" "+r.reason+"\n"))
			}
		}
		innerBytes := bytes.Join(inner, nil)
		var bodyOut []byte
		if sideband {
			bodyOut = bytes.Join([][]byte{sidebandPack(innerBytes), store.PktFlush()}, nil)
		} else {
			bodyOut = bytes.Join([][]byte{innerBytes, store.PktFlush()}, nil)
		}
		return GitResponse{Status: 200, ContentType: "application/x-git-receive-pack-result", Body: bodyOut}
	}
	ngAll := func() []refResult {
		out := make([]refResult, len(commands))
		for i, command := range commands {
			out[i] = refResult{ref: command.ref, ok: false, reason: "unpack error"}
		}
		return out
	}
	// Unpack first: every object lands in the store even when a ref is refused.
	if len(packBytes) > 0 {
		read, err := store.ReadPack(packBytes)
		if err != nil {
			return report(false, truncateRunes(err.Error(), 120), ngAll())
		}
		if !read.ChecksumOK {
			return report(false, "pack checksum mismatch", ngAll())
		}
		for oid, object := range read.Objects {
			if _, ok := repo.Objects[oid]; !ok {
				repo.Objects[oid] = object
			}
		}
	} else if hasNonZeroNew(commands) {
		return report(false, "unpacker error", ngAll())
	}
	results := make([]refResult, 0, len(commands))
	for _, command := range commands {
		current, hasCurrent := repo.Refs[command.ref]
		if !strings.HasPrefix(command.ref, "refs/") || invalidRefChars(command.ref) {
			results = append(results, refResult{ref: command.ref, ok: false, reason: "invalid ref"})
			continue
		}
		if command.newOID == store.ZeroOID {
			// Delete.
			if !hasCurrent {
				results = append(results, refResult{ref: command.ref, ok: false, reason: "non-fast-forward"})
			} else if command.oldOID != store.ZeroOID && command.oldOID != current {
				results = append(results, refResult{ref: command.ref, ok: false, reason: "non-fast-forward"})
			} else {
				delete(repo.Refs, command.ref)
				results = append(results, refResult{ref: command.ref, ok: true})
			}
			continue
		}
		if command.oldOID == store.ZeroOID {
			// Create.
			if hasCurrent {
				results = append(results, refResult{ref: command.ref, ok: false, reason: "non-fast-forward"})
				continue
			}
			if !hasObject(repo, command.newOID) {
				results = append(results, refResult{ref: command.ref, ok: false, reason: "non-fast-forward"})
				continue
			}
			repo.Refs[command.ref] = command.newOID
			results = append(results, refResult{ref: command.ref, ok: true})
			continue
		}
		// Update.
		if !hasCurrent || command.oldOID != current {
			results = append(results, refResult{ref: command.ref, ok: false, reason: "non-fast-forward"})
			continue
		}
		if command.newOID == command.oldOID {
			results = append(results, refResult{ref: command.ref, ok: true})
			continue
		}
		if !hasObject(repo, command.newOID) {
			results = append(results, refResult{ref: command.ref, ok: false, reason: "non-fast-forward"})
			continue
		}
		oldCommit := peelToCommit(repo, command.oldOID)
		newCommit := peelToCommit(repo, command.newOID)
		if oldCommit == "" || newCommit == "" || !s.IsAncestor(repo, oldCommit, newCommit) {
			results = append(results, refResult{ref: command.ref, ok: false, reason: "non-fast-forward"})
			continue
		}
		repo.Refs[command.ref] = command.newOID
		results = append(results, refResult{ref: command.ref, ok: true})
	}
	return report(true, "ok", results)
}

func hasNonZeroNew(commands []pushCommand) bool {
	for _, command := range commands {
		if command.newOID != store.ZeroOID {
			return true
		}
	}
	return false
}

// truncateRunes cuts s to at most n runes (the spec's detail.slice(0, 120)).
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}
