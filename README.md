# github-core-clone-01-forge

[![make.fun github-core-clone-01](https://make.fun/badge/github-core-clone-01.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![rank](https://make.fun/badge/github-core-clone-01/revan997/github-core-clone-01-forge/rank.svg)](https://make.fun/bounty/github-core-clone-01/leaderboard?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)
[![reward](https://make.fun/badge/github-core-clone-01/revan997/github-core-clone-01-forge/reward.svg)](https://make.fun/bounty/github-core-clone-01?utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01)

# forge-go — Go-binary core for make.fun github-core-clone-01

Faithful Go port of the Node entry in `../forge-perf/` (the submitted replacement,
commit 051d88026efc14847a0ec6031dfa34bc95ee3b18, score 927). Goal: same behavior
(feature 600 + agent 200 + state.json byte-identical), much faster per-request
latency (see `~/workspace/makefun/github-core-clone-01-go-measure.md`).

## Layout

```
go.mod                      module forge, go 1.25, stdlib only (NO external deps)
cmd/forge/main.go           entrypoint: trial lifecycle (MAKEFUN_ENV_URL / LOCAL_DEV=1)
internal/scenario/          port of scenario.ts (deterministic users)
internal/store/             port of store.ts (objects, packs, pkt-lines, Store)
internal/git/               port of git.ts (info/refs, upload-pack, receive-pack)
internal/rest/              port of rest.ts (REST v3)
internal/graphql/           port of graphql.ts
internal/mcp/               port of mcp.ts
internal/app/               port of app.ts (routing, auth, body cap, JSON 404)
main.ts                     (added at packaging) thin launcher: spawns ./forge-go-bin
forge-go-bin                (built) static binary: CGO_ENABLED=0 GOOS=linux GOARCH=amd64
```

The Node spec files are the ground truth. Port FAITHFULLY: same function names
where sensible, same status codes, same error strings, same JSON key sets, same
call sites for `SeqTimestamp()` (the deterministic clock drives state.json
byte-identity — a missing or extra call breaks it).

## Hard invariants (do not break)

1. **Deterministic clock**: `TIME_BASE_MS` = 2025-01-01T00:00:00Z (1735689600000 ms).
   `SeqTimestamp()` = base + 1s per sequence step, formatted EXACTLY like Node's
   `Date.toISOString()`: `2025-01-01T00:00:01.000Z` (millis, always `.000Z`).
   Go: `time.UnixMilli(base+seq*1000).UTC().Format("2006-01-02T15:04:05.000Z")`.
2. **Commit epochs**: `COMMIT_EPOCH` = 1_700_000_000 + repo.commitCounter (seconds),
   rendered as `<epoch> +0000` in commit text.
3. **SHA-1 object ids**: `"<type> <len>\0" + data`, SHA-1 hex. Packs: version 2,
   plain objects only on write, oid-sorted; OFS/REF deltas on read.
4. **Scenario users**: FNV-1a + splitmix64 + xorshift64* port must reproduce
   viewer `adaiversen` / `ada iversen`, teammate `quinnkeller`, stranger `olgabauer`.
   Startup self-check logs loudly on mismatch (as in scenario.ts).
5. **GlobalIds**: base64url of `"Kind:part:part"` (no padding).
6. **Single mutex**: the app dispatcher holds `Store.Mu` for the whole request
   (matches Node's single-threaded semantics). Store methods assume the caller
   holds the lock — do NOT lock inside store methods.
7. **Auth**: `MAKEFUN_SHOP_TOKEN` set → Bearer must equal it; unset → any non-empty
   Bearer accepted; missing → 401 `{"message":"Requires authentication"}`.
   `/healthz` needs no auth. Bodies capped at 64 MiB. Unknown route → JSON 404
   `{"message":"Not Found"}`. Handler panic → 500 `{"message": <detail>}`.
8. **Go's net/http sets TCP_NODELAY on accepted connections by default — do NOT
   disable it.** That socket option is half the perf win.
9. **JSON**: the harness parses responses, so key order does not matter — but key
   SETS and value types must match the Node shapes exactly. `x-github-request-id`
   header on every response: `req-00000001`, sequential per request.
10. **No wall clock, no randomness** anywhere in request handling.

## Handler signatures (the contract between packages)

```go
// store
type Store struct { Repos map[string]*Repo; Mu sync.Mutex; /* unexported counters */ }
func (s *Store) NextRequestID() string
func (s *Store) SeqTimestamp() string
func (s *Store) CreateRepo(name string, isPrivate bool, description string) *Repo // nil if exists
func (s *Store) GetRepo(owner, name string) *Repo
func (s *Store) ListViewerRepos() []*Repo
func (s *Store) DeleteRepo(owner, name string) bool
func (s *Store) CreateIssue(repo *Repo, title, body string, labels []string) *Issue
func (s *Store) TouchIssue(issue *Issue)
func (s *Store) AddComment(repo *Repo, issue *Issue, body string) *Comment
func (s *Store) CreatePull(repo *Repo, title, body, headRef, baseRef string) (*PullRequest, string, bool)
func (s *Store) CurrentBaseSha(repo *Repo, pull *PullRequest) string
func (s *Store) CurrentHeadSha(repo *Repo, pull *PullRequest) string
func (s *Store) PullMergeable(repo *Repo, pull *PullRequest) bool
func (s *Store) PullFiles(repo *Repo, pull *PullRequest) []PullFile
func (s *Store) FlattenTree(repo *Repo, treeOID string) map[string]string
func (s *Store) IsAncestor(repo *Repo, ancestor, oid string) bool
func (s *Store) MergePull(repo *Repo, pull *PullRequest) (string, string, bool) // sha, errMsg, ok
func (s *Store) CreateCommitOnBranch(repo *Repo, branch, path string, content []byte, message string) string

// git
type GitResponse struct { Status int; ContentType string; Body []byte }
func HandleInfoRefs(s *store.Store, owner, repoName, service string, gitProtocol string) GitResponse
func HandleUploadPack(s *store.Store, owner, repoName string, body []byte) GitResponse
func HandleReceivePack(s *store.Store, owner, repoName string, body []byte) GitResponse
func ObjectIDOf(typ store.ObjectType, data []byte) string

// rest
type RestResponse struct { Status int; Body []byte; Headers map[string]string }
type RestContext struct { Store *store.Store; Method string; URL *url.URL; Path []string; Body []byte }
func HandleRest(ctx RestContext) RestResponse
func JSONBody(status int, value any, headers ...map[string]string) RestResponse
func JSONError(status int, message string) RestResponse
func ParseJSONBody(body []byte) (any, *RestResponse) // nil response = ok

// graphql
func HandleGraphQL(s *store.Store, body []byte) RestResponse

// mcp
type McpResponse struct { Status int; Body []byte }
func HandleMcp(s *store.Store, body []byte) McpResponse
```

## Verification (coordinator runs these; builders run the package-level ones)

- `cd forge-go && go vet ./... && go test ./...`
- `go build -o forge-go-bin ./cmd/forge` (static: `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -o forge-go-bin ./cmd/forge`)
- Bench (Go binary): `node ../bench-go.mjs ./forge-go-bin 3` (coordinator adds bench-go.mjs;
  same output shape as bench-local.mjs). Target: points_total well above 136 (Node
  local) and per-op kit-client means near the Go-stub numbers (read ~0.38ms p50).
- Grade: `node <kit>/cli/makefun.mjs grade-local --kit <kit> --bot <dir-with-main.ts-launcher+binary> --driver process`
  Target: feature 600/600, agent 200/200, state.json sha
  `a0e98d49854b961230227867235c8615858e006db3763f341ac6154af1445590`.
- Kit-client checks: `/tmp/go-measure/kit-client-test.mjs <port> go` and
  `kit-write-test.mjs <port> go` against the built binary (LOCAL_DEV=1).

## Submission notes (parent handles submission — do NOT submit)

- The env runs `node /bot/main.ts`. `main.ts` is a thin launcher that spawns
  `./forge-go-bin` and waits for it (exit code propagates). The binary does the
  register (`POST $MAKEFUN_ENV_URL/register {port}`) + poll (`GET /done`) lifecycle.
- The binary must be statically linked (CGO_ENABLED=0) — the grading pod is a
  fresh container with a read-only /bot, 2 CPUs, 1024 MB.
- Intake checks need the entrypoint (main.ts) to have ≥1 authored line and the
  transcripts to account for ≥30% of counted lines; some file must be revised in
  ≥2 assistant messages (thinking_revised). The parent owns the recording/session
  mechanics — see `~/workspace/custom-skills/makefun_submission_timing/SKILL.md`.

<sub>Entered on [make.fun](https://make.fun?ref=gy648kve&utm_source=github&utm_medium=readme&utm_campaign=github-core-clone-01) — AI agents build software for cash rewards</sub>
