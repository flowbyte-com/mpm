# MPM Architecture & Systems Guide (v6.0.0)
**Project:** DESP
**User / Workspace Owner:** v
**Last Updated:** 2026-03-29

---

## 1. Path Resolution & Portability
MPM uses a cascading priority system for path resolution to enable portable installations. This allows the binary to run without hardcoded paths, making it ideal for containerized environments like Docker.

**Priority Order:**
1. **CLI Flag** (`--workspace=/custom/path`)
2. **Environment Variable** (`export MPM_WORKSPACE=/custom/path`)
3. **Executable Relative** (`os.Executable()`)
4. **Current Working Directory** (`os.Getwd()`)

**Configuration:**
Persistent customizations can be saved in `mpm_config.json`, dictating workspace, memory, and session directories. Environment variables (e.g., `MPM_SESSIONS_DIR`) will override the settings found in this configuration file.

---

## 2. Document Ingestion
MPM supports native document parsing using a pure Go implementation — no Python, Calibre, or other external runtimes required.

**Supported Formats:**

| Format | Extension | Parser | Notes |
|--------|-----------|--------|-------|
| Plain Text | `.txt` | Built-in `os` | Raw text extraction |
| Markdown | `.md` | Built-in | Section header extraction |
| JSON | `.json` | Built-in `encoding/json` | Structure preserved |
| PDF | `.pdf` | `github.com/ledongthuc/pdf` | Text extraction only (no OCR) |
| EPUB | `.epub` | `archive/zip` + `golang.org/x/net/html` | XHTML content extraction |
| HTML | `.html` | `golang.org/x/net/html` | Basic parsing, scripts stripped |

**Performance:**
- 10MB PDF: ~1.5 seconds
- 5MB EPUB: ~0.8 seconds

**Pipeline Features:**
- Automatic chunking with configurable sizes (default: 2000 chars, 200 char overlap)
- Section header extraction (Markdown `##`, `###`)
- Source metadata preservation
- Sensitive content scanning before storage

---

## 3. Data Lifecycle: The Topic System
Data within MPM matures through a specific lifecycle, utilizing the **Topic System** as an intermediate layer between raw sessions and hardened memories.

*   **Distillation:** When a session hits its token limit or shifts concepts, it is summarized and converted into a Topic.
*   **Hardening:** Once a Topic is successfully referenced three or more times, it is officially promoted to the Memory store.
*   **Provenance:** The system tracks data origins using `source_type` and `source_id`, ensuring any topic can be traced directly back to its original session or reference material.

---

## 4. Hybrid Search Engine (FTS5)
Search functionality is built on SQLite's FTS5 module, providing high-speed text retrieval across memories, sessions, and topics.

*   **Performance:** FTS5 queries execute in ~1-5ms. The system falls back to a standard substring search (~5-10ms) if the virtual tables are unavailable.
*   **Ranking:** Results are ordered by relevance using a built-in BM25 scoring algorithm.
*   **Highlighting:** Utilizes the `snippet()` function to highlight matching context (using `<<` and `>>` as boundary tags).
*   **Synchronization:** Standard data tables and FTS5 virtual tables are automatically kept in sync via SQL triggers executed after an `INSERT`.

---

## 5. Security: The Shred Protocol
For API key sanitization, GDPR compliance, or permanent data removal, MPM uses the Shred Protocol to ensure data is mathematically unrecoverable.

*   **True Hard Delete:** Executes `DELETE FROM` commands, explicitly avoiding any soft-deletion flags.
*   **Cascading:** Deleting a topic triggers cascading deletes to clear associated data in the `topic_memberships` table.
*   **Erasure & Verification:** Following the deletion, the system runs an SQLite `VACUUM` command (~100-500ms) to physically rewrite the database file, concluding with a post-delete count query to verify the erasure was successful.

---

## 6. Persistent Daemon & Webhooks (v6.1.0)
MPM can run as a persistent Unix socket daemon that provides structured logging and external notifications.

**Daemon Features:**
*   **Structured JSON Logging** — Events written to `~/.mpm/daemon.json.log` with 10MB rotation
*   **Per-User Socket** — `$XDG_RUNTIME_DIR/mpm.sock` or `~/.mpm/mpm.sock` prevents cross-user collisions
*   **Process Isolation** — `Setpgid: true` so Ctrl+C only kills subprocess, not daemon
*   **Meta-Commands** — `status`, `stop`, `logs` handled internally without subprocess

**Webhook System:**
*   **Async Dispatch** — Non-blocking channel-based dispatch (1000 buffer)
*   **Rate Limiting** — >50 events/sec triggers batching mode
*   **Selective Filter** — Only WARN and ERROR sent by default
*   **Retry Logic** — 1 retry on 5xx errors (500ms delay)
*   **5-Second Timeout** — Prevents goroutine leaks from hanging endpoints

**Environment Variables:**
| Variable | Purpose |
|----------|---------|
| `MPM_WEBHOOK_URL` | Enable webhooks (Slack, Discord, custom API) |
| `XDG_RUNTIME_DIR` | Socket/log directory (Linux/BSD) |