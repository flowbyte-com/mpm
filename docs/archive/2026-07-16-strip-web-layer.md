# Strip Web Layer from MPM Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Remove the HTTP server, static SPA, auth middleware, and `web_token` config from MPM so it becomes an HTTP-less data store that talks to clients (humans and agents) through `mpm-mcp` instead.

**Architecture:** Delete `cmd/mpm/web.go`, `cmd/mpm/web_handlers.go`, `cmd/mpm/web_auth_test.go`, and `cmd/mpm/web/`. Drop `web_token` from the config schema, drop the auth-token check from `mpm doctor`, drop `web` from the command router and help text. Update `README.md` and `CLAUDE.md` to reflect the new surface. The mpm binary becomes CLI + watch + call only; future human-facing UI lives in a separate binary (`mpm-telemetry`, roadmap item, not part of this plan) that talks to MPM via `mpm-mcp`.

**Tech Stack:** Go 1.26.x, `mattn/go-sqlite3` with FTS5, `mpm-core` (vendored at `internal/core/`). No new dependencies.

---

## File Structure

### Files to delete
| Path | Purpose | Lines |
|---|---|---|
| `cmd/mpm/web.go` | HTTP server, CSRF, auth middleware, `handleWeb` CLI entry | 488 |
| `cmd/mpm/web_handlers.go` | REST handlers for memories/topics/lessons/references/search | 552 |
| `cmd/mpm/web_auth_test.go` | Auth/CSRF middleware tests | (whole file) |
| `cmd/mpm/web/` | Static SPA: `app.js`, `index.html`, `style.css` | 3 files |

### Files to modify
| Path | Change |
|---|---|
| `internal/core/config/config.go` | Remove `WebToken` field (line 19) |
| `cmd/mpm/router.go` | Remove `"web"` from registry (line 58), remove `case "web"` dispatches (lines 183-184, 411-415), remove `"web"` from `opsSubcommandDescs` (line 507), drop the `// — Watcher & Web —` group label |
| `cmd/mpm/main.go` | Remove `runDoctorSecurityChecks` auth-token block (lines 1145-1164), update its doc comment (line 1098), remove `"web"` from help-text lists (lines 1327, 1348) |
| `README.md` | Remove `mpm web` command docs (line 738), remove auth policy section (line 1071) |
| `CLAUDE.md` | Remove web_token and web/HTTP references (lines 131, 151) |

### Files NOT to modify (audit findings out of scope)
- `docs/audit-2026-07-16.md` — historical record, do not edit
- `cmd/mpm/handlers_backup.go` — C-2 `restore-db` SQL parser is a separate audit item
- `cmd/mpm/handlers_status.go` — `countMemories` (C-3) and `getRecentWatchdogEvents` (H-12) are CLI issues, not web
- `cmd/mpm/handlers_memory.go` — H-5 fire-and-forget goroutine is CLI-side, unrelated

---

## Tasks

### Task 1: Drop `web_token` field from config schema

**Files:**
- Create: `internal/core/config/config_no_web_token_test.go`
- Modify: `internal/core/config/config.go:19`

The config schema's `WebToken` field exists only to support `mpm web`. Removing it severs the data side of the strip before any code touches the HTTP server.

- [ ] **Step 1: Write the failing test**

Create `internal/core/config/config_no_web_token_test.go`:

```go
package config

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestConfigHasNoWebToken ensures the web_token field is gone from the
// config schema. The field existed only to authenticate `mpm web`; after
// the HTTP server is stripped, no caller should be reading it.
func TestConfigHasNoWebToken(t *testing.T) {
	cfg := Config{}
	b, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	if strings.Contains(string(b), "web_token") {
		t.Fatalf("config still has web_token field: %s", string(b))
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test -tags fts5 -v ./internal/core/config/ -run TestConfigHasNoWebToken`

Expected: FAIL — "config still has web_token field"

- [ ] **Step 3: Remove the field**

Edit `internal/core/config/config.go`. Delete line 19:

```go
	WebToken       string            `json:"web_token,omitempty"`        // Optional bearer token for web UI auth
```

Also delete the surrounding alignment whitespace so the struct stays gofmt-clean. The struct should look like this after the edit:

```go
// Config holds the application configuration
type Config struct {
	Workspace      string            `json:"workspace,omitempty"`
	MemoryDir      string            `json:"memory_dir,omitempty"` // Legacy singular — use MemoryDirs
	MemoryDirs     []string          `json:"memory_dirs,omitempty"`
	SessionsDir    string            `json:"sessions_dir,omitempty"` // Legacy singular — use SessionsDirs
	SessionsDirs   []string          `json:"sessions_dirs,omitempty"`
	ExternalDbs    []ExternalDB      `json:"external_dbs,omitempty"`
	OpenClawDBPath string            `json:"openclaw_db_path,omitempty"` // Source DB for ingest (default: ~/.openclaw/memory/main.sqlite)
	Synth          *SynthConfig      `json:"synth,omitempty"`
	Aliases        map[string]string `json:"aliases,omitempty"` // CLI command aliases: "mem" → "recall --collection memories"
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test -tags fts5 -v ./internal/core/config/ -run TestConfigHasNoWebToken`

Expected: PASS

- [ ] **Step 5: Confirm no other internal/core code reads WebToken**

Run: `grep -rn 'WebToken' internal/`

Expected: no matches. If anything still references it, fix the consumer (do not leave a stub field).

- [ ] **Step 6: Run the full internal/core test suite**

Run: `cd internal/core && go test -tags fts5 -count=1 ./...`

Expected: PASS (all existing tests green)

- [ ] **Step 7: Commit**

```bash
git add internal/core/config/config.go internal/core/config/config_no_web_token_test.go
git commit -m "config: drop web_token field (HTTP server is being removed)"
```

---

### Task 2: Drop auth-token check from `mpm doctor`

**Files:**
- Modify: `cmd/mpm/main.go:1096-1164`

The doctor security check exists to warn operators that `web_token` is unset (which would prevent `mpm web` from starting). Once the web server is gone, the check is dead weight.

- [ ] **Step 1: Confirm the exact block to remove**

Read `cmd/mpm/main.go` around lines 1096-1164. The block to remove is:

```go
//   - auth configuration: is web_token set? is the server fail-open?
```

(inside the `runDoctorSecurityChecks` doc comment, line 1098)

and the entire auth-token check (lines 1145-1164):

```go
	// Auth config — check that web_token is set so `mpm web` doesn't
	// refuse to start. (Fail-closed is the safe default; warn when unset
	// so the operator knows the server will require --allow-anonymous.)
	cfg, _ := config.LoadConfig()
	if cfg == nil || cfg.WebToken == "" {
		report.Checks = append(report.Checks, DoctorCheck{
			Name: "Auth Token", Status: "WARN",
			Message:  "web_token is empty — `mpm web` will require --allow-anonymous flag (this is safe, not fail-open)",
			Duration: "0ms",
		})
		report.Warnings++
		fmt.Printf("    [%s] Auth Token: web_token empty (fail-closed default active)\n\n", colorYellow("WARN"))
	} else {
		report.Checks = append(report.Checks, DoctorCheck{
			Name: "Auth Token", Status: "PASS",
			Message: "web_token is configured", Duration: "0ms",
		})
		fmt.Printf("    [%s] Auth Token: web_token set\n\n", colorizeStatus("PASS"))
	}
	report.TotalChecks++

```

- [ ] **Step 2: Edit the doc comment**

Change `cmd/mpm/main.go:1098` from:

```go
//   - auth configuration: is web_token set? is the server fail-open?
```

to:

```go
//   - scanner coverage: a sanity ping of the static audit so the doctor
```

Wait — that line already exists at 1099-1100. Delete line 1098 entirely. After the edit the comment block should read:

```go
// runDoctorSecurityChecks covers security-relevant runtime state:
//   - synthesis telemetry: are LLM synth attempts succeeding or failing?
//   - scanner coverage: a sanity ping of the static audit so the doctor
//     report itself can flag if a new write path bypasses the scanner.
```

(One bullet removed, leaving synthesis telemetry + scanner coverage.)

- [ ] **Step 3: Delete the auth-token check block**

Delete lines 1145-1164 in `cmd/mpm/main.go` (the entire `// Auth config — ...` block including its trailing blank line). The next block to remain starts at the `// Scanner coverage is enforced by the test suite` comment (was line 1166, will become line 1145 after deletion).

- [ ] **Step 4: Remove the now-unused `config` import if main.go no longer uses it**

Run: `grep -n 'config\.' cmd/mpm/main.go | head`

Expected: if every remaining `config.` reference is removed when web.go is deleted in Task 4, the `config` import in main.go may go unused. If `goimports`/`go vet` flags it, remove the import. Otherwise leave it for Task 4 to clean up.

- [ ] **Step 5: Verify the build still compiles**

Run: `go build -tags fts5 ./cmd/mpm/`

Expected: success. (The build will still succeed because `web.go` still references `config.WebToken` — that's OK, the field is gone but `web.go` will break at Task 4 when we delete it. The intermediate state is fine.)

- [ ] **Step 6: Commit**

```bash
git add cmd/mpm/main.go
git commit -m "doctor: drop web_token auth-token check (HTTP server being removed)"
```

---

### Task 3: Drop `mpm web` from router (registry, dispatch, help)

**Files:**
- Create: `cmd/mpm/router_no_web_test.go`
- Modify: `cmd/mpm/router.go:58, 183-184, 411-415, 507`
- Modify: `cmd/mpm/main.go:1327, 1348`

The `web` command is currently registered in four places: the registry map, two switch cases (one in the main router, one in the ops subcommand router), and the ops help list. Plus main.go's parallel help-text structures.

- [ ] **Step 1: Write the failing tests**

Create `cmd/mpm/router_no_web_test.go`:

```go
package main

import (
	"strings"
	"testing"
)

// TestRouterHasNoWebCommand ensures the "web" command is gone from the
// router. The command existed only to start the HTTP server, which has
// been removed.
func TestRouterHasNoWebCommand(t *testing.T) {
	r := NewRouter()
	if _, ok := r.Commands["web"]; ok {
		t.Fatalf("router still has 'web' command registered")
	}
}

// TestOpsHelpHasNoWeb ensures the ops help list does not advertise a
// removed "web" subcommand.
func TestOpsHelpHasNoWeb(t *testing.T) {
	for _, entry := range opsSubcommandDescs {
		if entry.name == "web" {
			t.Fatalf("opsSubcommandDescs still has 'web' entry: %+v", entry)
		}
		if strings.Contains(entry.name, "web") {
			t.Fatalf("opsSubcommandDescs entry %q still mentions web", entry.name)
		}
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test -tags fts5 -v ./cmd/mpm/ -run 'TestRouterHasNoWebCommand|TestOpsHelpHasNoWeb'`

Expected: FAIL on both tests.

- [ ] **Step 3: Remove the registry entry**

Edit `cmd/mpm/router.go:58`. Delete:

```go
		"web":       {Name: "web", Description: "Start web UI server", MinArgs: 0},
```

and re-align the surrounding entries so the map stays gofmt-clean.

- [ ] **Step 4: Remove the main-router switch case**

Edit `cmd/mpm/router.go:183-184`. Delete:

```go
	case "web":
		return handleWeb(args)
```

- [ ] **Step 5: Remove the ops-router switch case and clean up the group label**

Edit `cmd/mpm/router.go:411-415`. Delete the block:

```go
	// — Watcher & Web —
	case "watch":
		return handleWatch(subArgs)
	case "web":
		return handleWeb(append([]string{"web"}, subArgs...))
```

and replace the comment with:

```go
	// — Watcher —
	case "watch":
		return handleWatch(subArgs)
```

(Removing the `& Web` from the label since `web` is gone.)

- [ ] **Step 6: Remove the ops help entry**

Edit `cmd/mpm/router.go:507`. Delete the line:

```go
	{"web", "Start web UI server"},
```

- [ ] **Step 7: Remove main.go help-text entries**

Edit `cmd/mpm/main.go:1327`. Delete:

```go
		{"web", "Start web UI server", false},
```

Edit `cmd/mpm/main.go:1348`. Delete the line:

```go
		{"watch | web | review", "", false},
```

and replace it with:

```go
		{"watch | review", "", false},
```

- [ ] **Step 8: Run the router tests to verify they pass**

Run: `go test -tags fts5 -v ./cmd/mpm/ -run 'TestRouterHasNoWebCommand|TestOpsHelpHasNoWeb'`

Expected: PASS

- [ ] **Step 9: Run the full cmd/mpm test suite to catch any indirect breakage**

Run: `go test -tags fts5 -count=1 ./cmd/mpm/`

Expected: PASS — the router still has `handleWeb` calls dangling, but the dispatch site is gone so the build may fail. If so, that confirms nothing calls `handleWeb` from the router and Task 4's deletion is safe.

If `go build` fails with "undefined: handleWeb", that's expected — proceed to Task 4 immediately.

- [ ] **Step 10: Commit**

```bash
git add cmd/mpm/router.go cmd/mpm/router_no_web_test.go cmd/mpm/main.go
git commit -m "router: drop 'mpm web' command (HTTP server being removed)"
```

---

### Task 4: Delete web server Go files

**Files:**
- Delete: `cmd/mpm/web.go`
- Delete: `cmd/mpm/web_handlers.go`
- Delete: `cmd/mpm/web_auth_test.go`

These three files contain the entire HTTP surface: server setup, all REST handlers, CSRF logic, auth middleware, the `webTokenLookup` var, and the `handleWeb` CLI entry. Once Tasks 1-3 have removed all upstream references, deleting the files is safe.

- [ ] **Step 1: Verify nothing in the repo references these files**

Run:

```bash
grep -rn 'web\.WebServer\|NewWebServer\|webTokenLookup\|handleWeb\|handleMemories\|handleTopics\|handleLessons\|handleReferences\|handleSearch\|handleStatus\|handleHealth\|handleCSRFToken\|realWebTokenLookup\|currentWebToken\|isMutatingMethod\|writeJSON\|writeError\|serverError\|parseInt\b' --include='*.go' .
```

Expected: no matches in any file outside the three being deleted (and outside the router.go which already lost its dispatch).

If anything outside these three files still references them, fix the importer first.

- [ ] **Step 2: Delete the files**

Run:

```bash
git rm cmd/mpm/web.go cmd/mpm/web_handlers.go cmd/mpm/web_auth_test.go
```

- [ ] **Step 3: Verify the build compiles**

Run: `go build -tags fts5 ./cmd/mpm/`

Expected: success, no undefined-symbol errors.

- [ ] **Step 4: Run the full test suite**

Run: `make test`

Expected: PASS. (If any test referenced `WebServer` / `webTokenLookup` / etc., the previous grep would have caught it.)

- [ ] **Step 5: Commit**

```bash
git commit -m "cmd/mpm: delete HTTP server (web.go, web_handlers.go, web_auth_test.go)"
```

---

### Task 5: Delete static SPA directory

**Files:**
- Delete: `cmd/mpm/web/app.js`
- Delete: `cmd/mpm/web/index.html`
- Delete: `cmd/mpm/web/style.css`
- Delete: empty `cmd/mpm/web/` directory

The static SPA was the only consumer of the HTTP server. With the server gone, the directory exists only as dead code.

- [ ] **Step 1: Confirm the directory is referenced nowhere**

Run: `grep -rn 'cmd/mpm/web\|web/app\.js\|web/index\.html\|web/style\.css' --include='*.go' .`

Expected: no matches. (The `//go:embed web` directive in `web.go` is already gone with the file.)

- [ ] **Step 2: Delete the directory**

Run: `git rm -r cmd/mpm/web/`

- [ ] **Step 3: Verify the directory is gone**

Run: `ls cmd/mpm/web 2>&1 || echo "directory absent (good)"`

Expected: "No such file or directory" or the "directory absent" line.

- [ ] **Step 4: Verify the build still compiles**

Run: `go build -tags fts5 ./cmd/mpm/`

Expected: success.

- [ ] **Step 5: Commit**

```bash
git commit -m "cmd/mpm: delete static SPA directory"
```

---

### Task 6: Update README.md

**Files:**
- Modify: `README.md:738`
- Modify: `README.md:1071`

The README still documents `mpm web` and the auth policy. Both are now historical.

- [ ] **Step 1: Find and read the `mpm web` documentation**

Run: `grep -n 'mpm web\|web_token\|web UI\|fail-closed' README.md`

Expected output (approximate line numbers — adjust to actual results):

```
613:... mpm web ... (some context)
738:mpm web [--port <n>] [--allow-anonymous]  # Start web UI server (fail-closed; requires web_token)
1071:**Auth policy:** `mpm web` defaults to **fail-closed** ...
```

- [ ] **Step 2: Remove the `mpm web` command line**

Edit `README.md:738`. Delete the entire line.

- [ ] **Step 3: Remove the auth-policy section**

Edit `README.md:1071`. Read the surrounding paragraph, then delete the auth-policy bullet/section in its entirety. If it's part of a numbered or bulleted list, delete the bullet and ensure surrounding bullets renumber cleanly.

- [ ] **Step 4: Confirm no README references to web remain**

Run: `grep -n 'mpm web\|web_token\|web UI\|fail-closed\|/api/\|http://localhost:18792' README.md`

Expected: no matches. (Some docs may legitimately mention webhooks — those stay; "webhook" should not match the patterns above.)

- [ ] **Step 5: Commit**

```bash
git add README.md
git commit -m "README: drop mpm web and web auth policy docs (HTTP server removed)"
```

---

### Task 7: Update CLAUDE.md (workspace + mpm-level)

**Files:**
- Modify: `/home/v/workspace/CLAUDE.md` (workspace-level) — line ~131
- Modify: `/home/v/workspace/projects/CLAUDE.md` (projects-level) — may have references
- Modify: `/home/v/workspace/projects/mpm/CLAUDE.md` (mpm-level) — line ~151

- [ ] **Step 1: Find all web references in CLAUDE.md files**

Run:

```bash
grep -n 'web_token\|web UI\|mpm web\|/api/\|SSE' /home/v/workspace/CLAUDE.md /home/v/workspace/projects/CLAUDE.md /home/v/workspace/projects/mpm/CLAUDE.md
```

Expected output (approximate):

```
mpm/CLAUDE.md:131:... authValid() / withAuth() reject requests when web_token is empty ...
mpm/CLAUDE.md:151:... web_token — bearer token for the web/SSE API ...
```

- [ ] **Step 2: Edit `mpm/CLAUDE.md:131`**

Find the bullet about auth and replace it. The current text reads:

```
- **Auth is fail-closed by default.** `authValid()` / `withAuth()` reject requests when `web_token` is empty. The `--allow-anonymous` flag is the explicit opt-in for local dev. The pre-audit "fail-open when token empty" footgun is gone.
```

Replace with:

```
- **Auth surface:** mpm has no HTTP server. Authentication is not a concern of this binary; consumers (`mpm-mcp`, future UI binaries) implement their own auth on top of the MCP/CLI boundary.
```

(Adjust the wording to match the surrounding tone — the goal is to remove the false claim that auth is fail-closed when the whole auth surface is gone.)

- [ ] **Step 3: Edit `mpm/CLAUDE.md:151`**

Find the `web_token` config bullet:

```
- `web_token` — bearer token for the web/SSE API (optional in current code → unauthenticated default)
```

Delete it. Re-number or re-flow the surrounding bullet list if needed for markdown cleanliness.

- [ ] **Step 4: Check workspace-level and projects-level CLAUDE.md**

For each file matched in Step 1's grep that lives outside `mpm/CLAUDE.md`, edit it the same way: delete the `web_token` bullet, replace auth claims with "no HTTP server."

- [ ] **Step 5: Confirm no CLAUDE.md references remain**

Run:

```bash
grep -rn 'web_token\|withAuth\|allow-anonymous\|fail-closed' /home/v/workspace/CLAUDE.md /home/v/workspace/projects/CLAUDE.md /home/v/workspace/projects/mpm/CLAUDE.md
```

Expected: no matches.

- [ ] **Step 6: Commit**

```bash
git add /home/v/workspace/CLAUDE.md /home/v/workspace/projects/CLAUDE.md /home/v/workspace/projects/mpm/CLAUDE.md
git commit -m "CLAUDE: remove web_token and HTTP-auth references (web layer stripped)"
```

---

### Task 8: Final verification

**Files:** none (verification only)

- [ ] **Step 1: Clean rebuild**

Run: `make clean && make build`

Expected: `make build` produces `bin/mpm` with no errors.

- [ ] **Step 2: Full test suite**

Run: `make test`

Expected: PASS — all green. If anything fails, investigate before claiming the strip is complete.

- [ ] **Step 3: Verify the binary is HTTP-less**

Run:

```bash
strings bin/mpm | grep -E 'mpm web|web_token|/api/|allow-anonymous' | head
```

Expected: no matches. (Some "web" substrings may legitimately remain if they appear in unrelated strings — e.g. "webhook" — but the four patterns above should produce nothing.)

- [ ] **Step 4: Verify `mpm web` is rejected**

Run: `./bin/mpm web 2>&1; echo "exit: $?"`

Expected: `unknown command: web` (or whatever the router prints for unknown commands — verify the message is clear) and a non-zero exit code.

- [ ] **Step 5: Verify `mpm doctor` no longer mentions auth**

Run: `./bin/mpm doctor 2>&1 | grep -i 'auth\|web_token'; echo "exit: $?"`

Expected: no matches and exit 1.

- [ ] **Step 6: Verify `mpm --help` no longer lists web**

Run: `./bin/mpm --help 2>&1 | grep -i 'web UI\|web server'; echo "exit: $?"`

Expected: no matches and exit 1.

- [ ] **Step 7: Update the audit document (optional, but recommended)**

The audit `docs/audit-2026-07-16.md` is a historical record, so do NOT edit the findings. Instead, append a brief "Resolution" section to the end:

```markdown
## Post-audit Resolution — Web Layer Stripped

**Date:** 2026-07-16
**Scope:** Removed all `mpm web` surface per the plan at `docs/superpowers/plans/2026-07-16-strip-web-layer.md`.

Closed findings (no longer applicable):
- C-1 fail-open web auth — server deleted
- FE-2 wildcard CORS — server deleted
- FE-3 `prompt()`-sourced token — SPA deleted
- FE-4 static CSRF nonce — middleware deleted
- FE-XSS-2 through FE-XSS-6 — SPA deleted
- FE-M-1 through FE-M-5 — SPA deleted
- H-1 config re-read per request — server deleted
- H-7 list-endpoint pagination — endpoints deleted
- H-8 editMemory double-decode — endpoint deleted
- H-9 1 MB content cap — endpoint deleted
- H-10 CSP header — header deleted
- H-6 per-session CSRF — middleware deleted

Replaced surface: human-facing UI is now a separate binary (`mpm-telemetry`, roadmap item) that talks to MPM via `mpm-mcp`. MPM itself is HTTP-less.
```

Append this section to `docs/audit-2026-07-16.md` (do not rewrite the audit findings — they remain historically accurate).

- [ ] **Step 8: Commit the audit resolution section (if added)**

```bash
git add docs/audit-2026-07-16.md
git commit -m "audit: append post-audit resolution note (web layer stripped 2026-07-16)"
```

(Only run this step if you actually appended the section.)

- [ ] **Step 9: Final summary commit if anything is still dirty**

If `git status` shows any uncommitted changes, commit them with:

```bash
git add -A
git commit -m "strip-web: final cleanup"
```

Otherwise the strip is complete.

---

## Self-Review

**Spec coverage:**
- Goal: strip HTTP/SPA surface from mpm → Tasks 4 + 5 (delete files), Task 1 (drop config), Task 2 (drop doctor check), Task 3 (drop router registration), Task 6+7 (drop docs). ✅
- `mpm web` command removed → Task 3 + verification in Task 8.4. ✅
- `web_token` config removed → Task 1 + verification in Task 8.3. ✅
- Docs updated → Tasks 6 + 7 + 8.7. ✅
- Build + test green → Task 8.1+8.2. ✅

**Out of scope (intentionally not addressed):**
- C-2 restore-db SQL allow-list parser — separate audit item, requires security-engineer review
- H-2 shred singleton reset — separate refactor
- H-4 restore-db concurrent-daemon guard — depends on C-2 fix design
- FE-1 drift `web/` directory at repo root — already absent from current checkout (verified during reconnaissance; no action needed)
- Untracked files (`arbitration.go`, `resolve.go`, `mpm-mcp/pidfile*.go`) — investigation items, separate work
- mpm-telemetry binary itself — roadmap item, separate project

**Type/symbol consistency:** All references to `handleWeb`, `NewWebServer`, `webTokenLookup`, `WebToken`, and `opsSubcommandDescs["web"]` are removed in lockstep across Tasks 1-5. No task introduces a partial state where symbols are referenced but undefined.