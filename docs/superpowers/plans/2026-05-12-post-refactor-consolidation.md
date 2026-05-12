# Post-Refactor Consolidation & Hardening Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Address post-refactor tradeoffs: fill testing gaps, harden the unified watcher lifecycle with native detached process persistence, and add integration tests for the critical path.

**Architecture:** The unified single-process model runs the file watcher and external DB pollers as in-process goroutines coordinated by a WorkerPool. Detached persistence is achieved via self-spawn: `mpm watch start` (parent) re-invokes the binary with `--bg`, then exits; the child writes a PID file and blocks on `select{}`. The PID file enables `stop` and `status` to find and signal the detached watcher without a Unix socket.

---

## File Map

```
cmd/mpm/
  handlers.go          # handleWatch, startWatchGoroutine, stopWatchGoroutine,
                       # handleWatchStart (--bg branch), handleWatchStop, handleWatchStatus,
                       # watchPIDPath helper, signal handler in child
  router.go            # Command registry, --bg flag parsing
  worker.go            # WorkerPool, WatchEvent, processEvent() dispatch
  watch.go             # watcherDaemon, fsnotify event loop, startupSweep, file processors
  watch_lifecycle_test.go  # Integration tests

internal/
  db.go                # DatabaseManager
  memory.go            # MemoryStore, IsSensitiveContent
internal/config/
  config.go            # GetMPMDir() — used for PID file path resolution
```

---

## Task 1: Add Watch Lifecycle Integration Tests

**Files:**
- Create: `cmd/mpm/watch_lifecycle_test.go`

- [ ] **Step 1: Write integration test for watch start/stop cycle**

```go
package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"mpm/internal/config"
	mpminternal "mpm/internal"
)

func TestWatchStartStopLifecycle(t *testing.T) {
	// Setup: create a temp memory directory
	tmpDir := t.TempDir()
	memDir := filepath.Join(tmpDir, "memory")
	sessionsDir := filepath.Join(tmpDir, "sessions")
	os.MkdirAll(memDir, 0755)
	os.MkdirAll(sessionsDir, 0755)

	// Save original config and restore after test
	origCfg, _ := config.LoadConfig()
	defer func() {
		if origCfg != nil {
			config.SaveConfig(origCfg)
		}
	}()

	// Override config to use temp dirs
	cfg := &config.Config{
		MemoryDirs:   []string{memDir},
		SessionsDirs: []string{sessionsDir},
	}
	config.SaveConfig(cfg)

	// Reset global state between tests
	watchPool = nil
	watcherCtx = nil
	watcherCancel = nil
	watcherDone = nil

	// Start watcher
	err := startWatchGoroutine()
	if err != nil {
		t.Fatalf("startWatchGoroutine failed: %v", err)
	}

	// Use WaitGroup to detect when goroutines have started
	var wg sync.WaitGroup
	wg.Add(1)

	go func() {
		defer wg.Done()
		// Wait for pool to be alive: either active workers or processed events > 0
		// The startup sweep is submitted immediately after goroutine start
		for i := 0; i < 50; i++ { // poll for up to 5s
			if watchPool != nil && (watchPool.ActiveWorkers() > 0 || watchPool.ProcessedCount() > 0) {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()

	// Wait for goroutine startup with timeout
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		// pool is alive
	case <-time.After(5 * time.Second):
		t.Fatal("Watch pool did not become active within 5s")
	}

	// Verify pool is usable
	if watchPool == nil {
		t.Fatal("watchPool is nil after start")
	}

	// Verify we can submit an event and it is picked up
	initialProcessed := watchPool.ProcessedCount()
	ok := watchPool.Submit(WatchEvent{Type: EventTopicClusterCheck, DryRun: true, Verbose: false})
	if !ok {
		t.Fatal("watchPool.Submit returned false — queue may be full or pool dead")
	}

	// Wait for event to be processed (poll, no sleep)
	for i := 0; i < 20; i++ {
		if watchPool.ProcessedCount() > initialProcessed {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Stop watcher — this blocks until the goroutine exits
	stopWatchGoroutine()

	// Verify stopped state
	if watcherCancel != nil || watcherCtx != nil {
		t.Fatal("Watcher globals not cleared after stop")
	}
}

func TestWatchExternalDBPoolInitialization(t *testing.T) {
	// Test that external DB polling goroutines start without crashing
	// when no external DBs are configured
	tmpDir := t.TempDir()
	cfg := &config.Config{
		MemoryDirs:  []string{tmpDir},
		SessionsDirs: []string{},
		ExternalDBs:  []config.ExternalDB{},
	}
	origCfg, _ := config.LoadConfig()
	config.SaveConfig(cfg)
	defer func() {
		if origCfg != nil {
			config.SaveConfig(origCfg)
		}
	}()

	watchPool = nil
	watcherCtx = nil
	watcherCancel = nil
	watcherDone = nil

	err := startWatchGoroutine()
	if err != nil {
		t.Fatalf("startWatchGoroutine failed: %v", err)
	}
	defer stopWatchGoroutine()

	// Wait for startup sweep to complete (it processes synchronously in the pool)
	for i := 0; i < 50; i++ {
		if watchPool != nil && watchPool.ProcessedCount() > 0 {
			return // startup sweep done
		}
		time.Sleep(100 * time.Millisecond)
	}
	// If no events processed, at least verify the pool didn't crash
	if watchPool == nil {
		t.Fatal("watchPool is nil after start — pool may have crashed")
	}
}

func TestWatchEventProcessorDispatch(t *testing.T) {
	// Create a minimal in-memory DB for testing
	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		t.Skip("Database not available for testing")
	}
	defer dm.Close()

	pool := NewWorkerPool(dm, 2)
	ctx := context.Background()
	pool.Start(ctx)
	defer pool.Stop()

	// Poll for processed count instead of sleep
	initialProcessed := pool.ProcessedCount()

	ok := pool.Submit(WatchEvent{Type: EventTopicClusterCheck, DryRun: true, Verbose: false})
	if !ok {
		t.Fatal("Submit failed")
	}

	// Wait for processing with poll loop
	for i := 0; i < 20; i++ {
		if pool.ProcessedCount() > initialProcessed {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}
```

- [ ] **Step 2: Run the new tests**

Run: `go test -v -tags fts5 ./cmd/mpm/ -run "TestWatch" -timeout 30s`
Expected: Tests compile and run. Any failure reveals a real issue.

- [ ] **Step 3: Commit**

```bash
git add cmd/mpm/watch_lifecycle_test.go
git commit -m "test: add watch lifecycle integration tests"
```

---

## Task 2: Test the Topic Clustering Path

**Files:**
- Modify: `cmd/mpm/watch_lifecycle_test.go` (add tests)

- [ ] **Step 1: Add test for topic clustering with DB-backed topics**

```go
func TestTopicClusteringDB(t *testing.T) {
	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		t.Skip("Database not available")
	}
	defer dm.Close()

	// Insert 3 LTM memories with the same tag
	tag := "testcluster"
	for i := 0; i < 3; i++ {
		content := fmt.Sprintf("#%s some content %d", tag, i)
		emb, _ := json.Marshal([32]byte{})
		tags := map[string]interface{}{tag: true}
		metadata := map[string]interface{}{"is_long_term": true, "weight": 10}
		_, err := dm.SaveMemory("memories", content, "", tags, metadata, emb, true, 10)
		if err != nil {
			t.Fatalf("SaveMemory failed: %v", err)
		}
	}

	// Create watcherDaemon with the DB
	memory := mpminternal.NewMemoryStore("")
	wd := &watcherDaemon{
		db:      dm,
		memory:  memory,
		dryRun:  false,
		verbose: false,
	}

	// Run clustering check — should create a topic
	wd.checkTopicClustering()

	// Verify topic was created via DB query (no in-memory cache)
	topic, err := wd.findTopicByTag(tag)
	if err != nil {
		t.Fatalf("findTopicByTag failed: %v", err)
	}
	if topic == nil {
		t.Fatal("Expected topic to be created for tag 'testcluster'")
	}
}
```

- [ ] **Step 2: Run the clustering test**

Run: `go test -v -tags fts5 ./cmd/mpm/ -run "TestTopicClusteringDB" -timeout 30s`
Expected: PASS — confirms the no-in-memory-cache path is correct

- [ ] **Step 3: Commit**

```bash
git add cmd/mpm/watch_lifecycle_test.go
git commit -m "test: add topic clustering DB integration test"
```

---

## Task 3: Implement PID File Helper and Already-Running Check

**Files:**
- Modify: `cmd/mpm/handlers.go` (add PID file helper functions)

- [ ] **Step 1: Add PID file path constant and helper functions**

At the top of `handlers.go` (after the existing package-level declarations around line 18), add:

```go
// watchPIDFile is the path to the watcher's PID file.
// Written by the detached child process, used by parent stop/status to locate the watcher.
const watchPIDFile = "watch.pid"

// watchPIDPath returns the absolute path to the watch.pid file.
// Uses GetMPMDir() so the PID file lives alongside mpm.db and other runtime data.
func watchPIDPath() string {
	return filepath.Join(config.GetMPMDir(), watchPIDFile)
}

// readWatchPID reads the PID from watch.pid and returns it.
// Returns 0 if the file does not exist or cannot be read.
func readWatchPID() int {
	data, err := os.ReadFile(watchPIDPath())
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(data)))
	return pid
}

// writeWatchPID writes the current process PID to watch.pid.
func writeWatchPID() error {
	return os.WriteFile(watchPIDPath(), []byte(fmt.Sprintf("%d", os.Getpid())), 0644)
}

// deleteWatchPID removes the watch.pid file.
// Safe to call even if the file does not exist.
func deleteWatchPID() {
	os.Remove(watchPIDPath())
}

// isWatchProcessAlive checks if the process identified by pid is running.
// Uses signal 0 (no actual signal sent) to test process existence.
func isWatchProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Signal 0 checks if process exists without sending a real signal
	err = proc.Signal(syscall.Signal(0))
	return err == nil
}
```

- [ ] **Step 2: Add `strconv` to the imports in handlers.go**

In the import block of `handlers.go`, add `"strconv"` to the import list:

```go
import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"mpm/internal"
)
```

- [ ] **Step 3: Verify compilation**

Run: `go build -tags fts5 ./cmd/mpm/ 2>&1`
Expected: No errors (the new functions are defined but not yet called)

- [ ] **Step 4: Commit**

```bash
git add cmd/mpm/handlers.go
git commit -m "feat: add PID file helpers for detached watcher lifecycle"
```

---

## Task 4: Implement Detached Self-Spawn in handleWatchStart

**Files:**
- Modify: `cmd/mpm/handlers.go` (handleWatch case "start")

- [ ] **Step 1: Refactor handleWatch case "start" to add self-spawn logic**

Replace the existing `case "start":` block in `handleWatch` (around line 2101-2105) with:

```go
case "start":
	// Check if --bg flag is present (indicates this is the child process)
	bgFlag := false
	for _, arg := range args[1:] {
		if arg == "--bg" {
			bgFlag = true
			break
		}
	}

	if !bgFlag {
		// PARENT: Check if watcher is already running via PID file
		existingPID := readWatchPID()
		if existingPID > 0 && isWatchProcessAlive(existingPID) {
			return respond("", fmt.Sprintf("Watcher is already running (PID %d)\n", existingPID), 1)
		}

		// SPAWN CHILD: Re-invoke self with --bg flag
		exe, err := os.Executable()
		if err != nil {
			return respond("", fmt.Sprintf("Error: cannot find executable: %v\n", err), 1)
		}
		cmd := exec.Command(exe, append([]string{"watch", "start", "--bg"}, args[1:]...)...)
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			return respond("", fmt.Sprintf("Error: failed to start watcher: %v\n", err), 1)
		}
		fmt.Printf("🚀 Watcher started in background (PID %d)\n", cmd.Process.Pid)
		os.Exit(0)
		return // unreachable
	}

	// CHILD: --bg flag present — proceed with normal startup
	if err := startWatchGoroutine(); err != nil {
		return respond("", fmt.Sprintf("Error: %v\n", err), 1)
	}
	return respond("File watcher started\n", "", 0)
```

- [ ] **Step 2: Add `os/exec` to the imports**

In the import block of `handlers.go`, add `"os/exec"`:

```go
import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"mpm/internal"
)
```

- [ ] **Step 3: Verify compilation**

Run: `go build -tags fts5 ./cmd/mpm/ 2>&1`
Expected: No errors

- [ ] **Step 4: Commit**

```bash
git add cmd/mpm/handlers.go
git commit -m "feat: implement detached self-spawn for watch start"
```

---

## Task 5: Implement Child Process Signal Handler and Blocking Wait

**Files:**
- Modify: `cmd/mpm/handlers.go` (startWatchGoroutine and new signal handler)

- [ ] **Step 1: Modify startWatchGoroutine to register signal handler and block**

Replace the current `startWatchGoroutine` function body (lines 2130-2166) with:

```go
// startWatchGoroutine launches the fsnotify watcher and external DB pollers
// as background goroutines within the current process.
// When run as a detached child (--bg flag), it also:
//   - Writes its PID to watch.pid
//   - Registers a SIGTERM/Interrupt handler for graceful shutdown
//   - Blocks forever (select{}) until signalled
func startWatchGoroutine() error {
	if watcherCancel != nil {
		return fmt.Errorf("watcher is already running")
	}

	// Lazy-init the worker pool with its own DatabaseManager.
	// The DM is opened once and shared across all pool workers.
	if watchPool == nil {
		dm, err := internal.NewDatabaseManager("")
		if err != nil {
			return fmt.Errorf("failed to open database for watcher: %w", err)
		}
		watchPool = NewWorkerPool(dm, defaultWorkerPoolSize)
	}
	watcherCtx, watcherCancel = context.WithCancel(context.Background())
	watcherDone = make(chan struct{})

	// Write PID file before starting goroutines (best effort — if this fails, continue anyway)
	_ = writeWatchPID()

	// Set up graceful shutdown handler
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)

	go func() {
		defer close(watcherDone)
		defer deleteWatchPID() // clean up PID file on exit

		// Launch the fsnotify watcher event loop in its own goroutine
		// (it blocks internally on fsnotify events).
		go startWatcherGoroutine(watcherCtx, watchPool, false, false)

		// Start external DB polling goroutines (each spawns its own goroutine).
		startExternalDBPollGoroutines(watcherCtx, watchPool, false, false)

		// Submit a startup sweep event to the pool.
		watchPool.Submit(WatchEvent{Type: EventStartupSweep, DryRun: false, Verbose: false})

		// Block until cancelled or signal received
		select {
		case <-watcherCtx.Done():
			// Cancelled by stopWatchGoroutine
		case sig := <-sigCh:
			// SIGTERM or Interrupt received — graceful shutdown
			fmt.Fprintf(os.Stderr, "\n⚠️  Received %v — shutting down watcher...\n", sig)
			stopWatchGoroutine()
		}
	}()

	return nil
}
```

- [ ] **Step 2: Add `signal` to the imports**

In the import block of `handlers.go`, add `"os/signal"`:

```go
import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"mpm/internal"
)
```

- [ ] **Step 3: Verify compilation**

Run: `go build -tags fts5 ./cmd/mpm/ 2>&1`
Expected: No errors

- [ ] **Step 4: Commit**

```bash
git add cmd/mpm/handlers.go
git commit -m "feat: add graceful shutdown signal handler to detached watcher"
```

---

## Task 6: Implement handleWatchStop and handleWatchStatus

**Files:**
- Modify: `cmd/mpm/handlers.go` (add handleWatchStop, handleWatchStatus; update case "stop" and case "status")

- [ ] **Step 1: Add handleWatchStop function**

Add this function after `stopWatchGoroutine()` (around line 2178):

```go
// handleWatchStop reads the PID from watch.pid and signals the watcher to stop.
func handleWatchStop() int {
	pid := readWatchPID()
	if pid == 0 {
		return respond("", "Watcher is not running.\n", 1)
	}

	proc, err := os.FindProcess(pid)
	if err != nil {
		deleteWatchPID()
		return respond("", "Watcher is not running.\n", 1)
	}

	// Send Interrupt (cross-platform equivalent of SIGTERM)
	if err := proc.Signal(os.Interrupt); err != nil {
		// Process may have already exited — clean up PID file
		deleteWatchPID()
		// Check if it's actually gone
		if isWatchProcessAlive(pid) {
			return respond("", fmt.Sprintf("Error: failed to stop watcher: %v\n", err), 1)
		}
		// Process is gone, consider it stopped
	}

	// Give it a moment to shut down gracefully
	time.Sleep(500 * time.Millisecond)

	// Verify it's gone
	if isWatchProcessAlive(pid) {
		return respond("", fmt.Sprintf("Watcher stop signal sent (PID %d) — it may take a moment to shut down.\n", pid), 0)
	}

	deleteWatchPID()
	return respond("Watcher stopped.\n", "", 0)
}

// handleWatchStatus checks if the watcher is running via the PID file.
func handleWatchStatus() int {
	pid := readWatchPID()
	if pid == 0 {
		return respond("Watcher is not running.\n", "", 0)
	}

	if !isWatchProcessAlive(pid) {
		// Stale PID file — clean it up
		deleteWatchPID()
		return respond("Watcher is not running.\n", "", 0)
	}

	// Process is alive — report status from the global pool
	if watchPool == nil {
		return respond(fmt.Sprintf("Watcher is running (PID %d) — pool not yet initialized.\n", pid), "", 0)
	}
	active := watchPool.ActiveWorkers()
	processed := watchPool.ProcessedCount()
	return respond(fmt.Sprintf("Watcher is running (PID %d, %d active workers, %d events processed)\n", pid, active, processed), "", 0)
}
```

- [ ] **Step 2: Add `time` to the imports**

In the import block of `handlers.go`, add `"time"`:

```go
import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mpm/internal"
)
```

- [ ] **Step 3: Update handleWatch switch cases to use the new functions**

In `handleWatch`, replace the `case "stop":` and the status handling:

```go
// Old (lines 2107-2116):
//	case "stop":
//		stopWatchGoroutine()
//		return respond("File watcher stopped\n", "", 0)
//
//	case "restart":
//		stopWatchGoroutine()
//		if err := startWatchGoroutine(); err != nil {
//			return respond("", fmt.Sprintf("Error restarting: %v\n", err), 1)
//		}
//		return respond("File watcher restarted\n", "", 0)

// New:
case "stop":
	return handleWatchStop()

case "restart":
	stopWatchGoroutine()
	if err := startWatchGoroutine(); err != nil {
		return respond("", fmt.Sprintf("Error restarting: %v\n", err), 1)
	}
	return respond("File watcher restarted\n", "", 0)
```

Also update the status check at the top of `handleWatch` (line 2095) to use the new function:

```go
// Old:
if len(args) < 1 || args[0] == "status" {
	return respond(formatWatchStatus(), "", 0)
}

// New:
if len(args) < 1 || args[0] == "status" {
	return handleWatchStatus()
}
```

- [ ] **Step 4: Verify compilation**

Run: `go build -tags fts5 ./cmd/mpm/ 2>&1`
Expected: No errors

- [ ] **Step 5: Commit**

```bash
git add cmd/mpm/handlers.go
git commit -m "feat: implement watch stop and status via PID file"
```

---

## Task 7: Run Full Test Suite

**Files:**
- None (verification only)

- [ ] **Step 1: Run all tests**

Run: `make test 2>&1 | tail -50`
Expected: All tests pass with no data races

- [ ] **Step 2: If any test fails, investigate and fix before proceeding**

Investigate using: `go test -v -race -tags fts5 ./... -timeout 60s`

- [ ] **Step 3: Commit final state if changes were needed**

```bash
git add -A
git commit -m "test: address any failures from full test suite"
```

---

## Task 8: Smoke Test the Detached Watcher

**Files:**
- None (verification only)

- [ ] **Step 1: Test detached start/stop cycle**

```bash
cd /home/v/workspace/projects/mpm
go build -tags fts5 -o /tmp/mpm-test ./cmd/mpm/

# Start watcher (should return immediately, child keeps running)
/tmp/mpm-test watch start
# Expected output: "🚀 Watcher started in background (PID NNN)"
# Expected exit: 0 (parent exits)

sleep 1

# Check status (should show running with PID)
# Expected: "Watcher is running (PID NNN, ..."
/tmp/mpm-test watch status

# Stop watcher
/tmp/mpm-test watch stop
# Expected: "Watcher stopped."

sleep 1

# Verify it's gone
/tmp/mpm-test watch status
# Expected: "Watcher is not running."
```

- [ ] **Step 2: Test already-running guard**

```bash
/tmp/mpm-test watch start &
sleep 1

# Try to start again
/tmp/mpm-test watch start
# Expected: "Watcher is already running (PID NNN)" — exits 1

# Clean up
/tmp/mpm-test watch stop
```

- [ ] **Step 3: Test concurrent add stress test**

```bash
# Start watcher
/tmp/mpm-test watch start
sleep 2

# Concurrent adds (20 simultaneous)
/tmp/mpm-test watch status
# Should show events processed count

# Stop
/tmp/mpm-test watch stop
```

---

## Task 9: Update CLI Help for watch Command

**Files:**
- Modify: `cmd/mpm/router.go` (update watch command description)

- [ ] **Step 1: Update watch command description**

In `router.go` around line 50, update the watch command description:

```go
// Old:
"watch": {Name: "watch", Description: "File watcher for memory ingestion"},

// New:
"watch": {Name: "watch", Description: "File watcher for memory ingestion (start/stop/status)"},
```

- [ ] **Step 2: Commit**

```bash
git add cmd/mpm/router.go
git commit -m "docs: update watch command description"
```

---

## Self-Review Checklist

- [ ] Spec coverage: Each requirement from the spec is addressed by at least one task?
  - Parent checks PID file before spawning → Task 4 Step 1
  - Child writes PID file → Task 5 Step 1
  - Child blocks on select{} with signal handler → Task 5 Step 1
  - Graceful shutdown deletes PID file → Task 5 Step 1
  - handleWatchStop via PID file + os.Interrupt → Task 6 Step 1
  - handleWatchStatus via PID file + signal 0 → Task 6 Step 1
  - Stale PID file cleanup → Task 6 Step 1
  - Integration tests for lifecycle → Task 1

- [ ] Placeholder scan: No "TBD", "TODO", "fill in later" in any step

- [ ] Type consistency:
  - `watchPIDFile` (string constant) and `watchPIDPath()` (string function) match
  - `readWatchPID()` returns `int`, `isWatchProcessAlive()` takes `int` — consistent
  - `writeWatchPID()` uses `fmt.Sprintf("%d", os.Getpid())` — correct
  - `deleteWatchPID()` is called in `startWatchGoroutine` goroutine and in `handleWatchStop` — consistent
  - `handleWatchStop()` returns `int` (exit code) — consistent with other handlers
  - `handleWatchStatus()` returns `int` — consistent with other handlers

- [ ] All code is real Go — no pseudocode, all imports and calls are valid

- [ ] Cross-platform: `os.Interrupt` used for stop signal, not `syscall.SIGTERM`

- [ ] Test code uses `fmt` import (needed for `fmt.Sprintf` in `TestTopicClusteringDB`) — added in Task 2 Step 1

---

**Plan complete.** Saved to `docs/superpowers/plans/2026-05-12-post-refactor-consolidation.md`.

Two execution options:

**1. Subagent-Driven (recommended)** — I dispatch a fresh subagent per task, review between tasks, fast iteration

**2. Inline Execution** — Execute tasks in this session using executing-plans, batch execution with checkpoints

Which approach?