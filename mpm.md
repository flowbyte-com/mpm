# MPM Architecture & Rules of Engagement (v2.0)

**CRITICAL DIRECTIVE FOR AI AGENTS:** Read this document fully before proposing architectural changes, writing IPC logic, or modifying the daemon lifecycle. MPM operates on a strict single-process, cross-platform model. 

---

## 1. Core Architecture (The Single-Process Model)
MPM (Memory Process Manager) is a unified, single-binary application written in Go. 
* **NO Unix Sockets:** We do not use `net.Conn`, Unix sockets, or POSIX-specific IPC to communicate between the CLI and the background watcher.
* **Cross-Platform:** The architecture must compile and run natively on Linux, macOS, and Windows. Avoid OS-specific syscalls (e.g., use `os.Interrupt` instead of `syscall.SIGTERM`).
* **Unified State:** Both short-lived CLI commands and the long-running background watcher share the exact same codebase and execute within the same process environment.

---

## 2. Background Persistence (The Detached Watcher)
MPM does not use external process managers (like `systemd` or `pm2`) by default to run in the background, nor does it use C-style `fork()`. It manages its own persistence via a **Detached Process Spawn**.

### The Lifecycle
1. **Start (`mpm watch start`):** The parent CLI process invokes `os.Executable()` to spawn a child process of itself with a hidden `--bg` flag, then the parent exits immediately (returning terminal control to the user).
2. **The Child (`--bg`):** The child process writes its Process ID to `{MPM_WORKSPACE}/watch.pid`, starts the `WorkerPool` and `fsnotify` loops as goroutines, and blocks indefinitely via `select{}`.
3. **Stop & Status (`mpm watch stop/status`):** The CLI reads `watch.pid`. 
    * `status` uses a Signal 0 check to verify the process is alive. 
    * `stop` uses `os.Interrupt` to trigger a graceful shutdown. 
    * **Rule:** Never attempt to contact the watcher over a network/socket.

---

## 3. Concurrency & State Management
Because the CLI and the Watcher share the same architecture, state management relies on thread-safe Go primitives and SQLite locking, not inter-process messaging.

* **The SQLite Connection Pool:** The `DatabaseManager` is the single source of truth. SQLite is configured in WAL (Write-Ahead Logging) mode. Both the CLI and the Watcher interact with the database concurrently. 
    * **Rule:** Never implement in-memory caches that duplicate database state (e.g., no topic caches). Read directly from the DB to ensure data is never stale.
* **The Worker Pool:** File ingestion and background tasks are pushed into a central `WorkerPool` via Go channels. 
    * **Rule:** Do not spawn unmanaged goroutines for ingestion tasks. Submit tasks to the `WorkerPool` so they can be drained gracefully on shutdown.

---

## 4. Coding Constraints for Agents
When writing code for MPM, adhere to the following rules:
1. **No "Big Bang" Refactors:** If modifying the core `router.go` or `worker.go`, propose changes in small, isolated steps.
2. **Graceful Shutdown:** Any long-running goroutine must accept a `context.Context` and listen for `ctx.Done()` to ensure it cleans up properly when the main process receives an `os.Interrupt`.
3. **Error Bubbling:** Functions should return standard `(Type, error)` signatures. Do not use custom error-wrapping wrappers unless explicitly defined in `internal/`.
4. **Idempotency:** Startup scripts, database migrations, and trigger creations must use `IF NOT EXISTS` to prevent benign errors on subsequent boots.
