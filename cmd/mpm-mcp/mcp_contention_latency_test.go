// mcp_contention_latency_test.go — the measurement harness for the
// observational-write latency mission (issue #178, F2).
//
// WHY THIS FILE EXISTS.
//
// A single MCP wake request was measured at 124.47 s while a competing
// writer held the SQLite write lock. The number is a WALL-CLOCK number
// for a whole request, not a sum of statement timings: it is what an
// agent actually waits for. This file exists to reproduce that number
// from the outside — over the real MCP stdio transport, against a
// disposable workspace — so that any claim of improvement is measured
// the same way the baseline was.
//
// The harness deliberately does NOT instrument the substrate. It holds a
// real lock, issues a real JSON-RPC request, and reads the clock. The
// substrate's own watchdog (src/db/watchdog.jsonl) supplies the
// per-statement attribution afterwards, so the breakdown is observed
// rather than asserted.
//
// Every run uses a disposable workspace and never touches the operator's.

package main

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"

	core "github.com/flowbyte-com/mpm-core"
)

// latencyClient is a JSON-RPC client over a child mpm-mcp process.
//
// It is deliberately separate from the cron-delivery stdio client: that
// client's `call` has a hard 30 s response deadline, and this harness
// has to survive a request that takes minutes before the fix lands.
type latencyClient struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stderr *latencyStderr

	mu        sync.Mutex
	responses []map[string]interface{}
	nextID    int
}

// latencyStderr is a concurrency-safe buffer for the child's stderr. The
// MCP server reports audit-insert failures on stderr with the audit
// level and component, which is how the failure-path writes are
// attributed.
type latencyStderr struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (s *latencyStderr) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *latencyStderr) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// newLatencyClient starts mpm-mcp against the given workspace and
// completes the MCP handshake.
func newLatencyClient(t *testing.T, ws string) *latencyClient {
	t.Helper()

	bin := mcpCmd(t)
	dbPath := filepath.Join(ws, "src", "db", "mpm.db")

	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"MPM_WORKSPACE="+ws,
		"MPM_DB_PATH="+dbPath,
		// The handoff write is reached through mpm_handoff, which is
		// registered but outside the default 3-tool MCP surface.
		"MPM_EXPOSE_ALL_TOOLS=1",
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	stderr := &latencyStderr{}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start mpm-mcp: %v", err)
	}

	c := &latencyClient{t: t, cmd: cmd, stdin: stdin, stderr: stderr}

	var readerWG sync.WaitGroup
	readerWG.Add(1)
	go func() {
		defer readerWG.Done()
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var msg map[string]interface{}
			if err := json.Unmarshal([]byte(line), &msg); err != nil {
				continue
			}
			c.mu.Lock()
			c.responses = append(c.responses, msg)
			c.mu.Unlock()
		}
	}()

	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		readerWG.Wait()
		_ = cmd.Wait()
	})

	c.call("initialize", map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]interface{}{},
		"clientInfo":      map[string]interface{}{"name": "contention-latency-probe", "version": "1.0"},
	}, 60*time.Second)
	c.notify("notifications/initialized")
	return c
}

// notify sends a JSON-RPC notification (no response expected).
func (c *latencyClient) notify(method string) {
	c.t.Helper()
	c.mu.Lock()
	c.nextID++
	c.mu.Unlock()
	payload := map[string]interface{}{"jsonrpc": "2.0", "method": method}
	b, err := json.Marshal(payload)
	if err != nil {
		c.t.Fatalf("marshal notification: %v", err)
	}
	if _, err := c.stdin.Write(append(b, '\n')); err != nil {
		c.t.Fatalf("write notification: %v", err)
	}
}

// call sends a JSON-RPC request and waits up to deadline for the
// response. It returns the elapsed wall time so the caller can report
// the request's true cost.
func (c *latencyClient) call(method string, params map[string]interface{}, deadline time.Duration) (map[string]interface{}, time.Duration) {
	c.t.Helper()
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.mu.Unlock()

	payload := map[string]interface{}{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
	}
	if params != nil {
		payload["params"] = params
	}
	b, err := json.Marshal(payload)
	if err != nil {
		c.t.Fatalf("marshal request: %v", err)
	}

	start := time.Now()
	if _, err := c.stdin.Write(append(b, '\n')); err != nil {
		c.t.Fatalf("write request: %v", err)
	}

	limit := start.Add(deadline)
	for time.Now().Before(limit) {
		c.mu.Lock()
		for _, resp := range c.responses {
			if gotID, ok := resp["id"].(float64); ok && int(gotID) == id {
				c.mu.Unlock()
				if errObj, ok := resp["error"]; ok {
					c.t.Fatalf("jsonrpc error for %s: %v", method, errObj)
				}
				result, _ := resp["result"].(map[string]interface{})
				return result, time.Since(start)
			}
		}
		c.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	c.t.Fatalf("timed out after %s waiting for a response to %s", deadline, method)
	return nil, 0
}

// holdWriteLock opens a competing connection and takes the SQLite write
// lock, holding it until the returned release function is called.
//
// The lock is taken by an actual write inside a transaction, which is
// what a concurrent writer looks like to every other connection in WAL
// mode: readers proceed, writers block.
//
// When holdFor is positive the lock is released automatically after
// that duration, which is how a RECOVERABLE contention window is
// simulated. A holdFor of 0 means "hold until released".
func holdWriteLock(t *testing.T, dbPath string, holdFor time.Duration) (release func()) {
	t.Helper()
	competitor, err := sql.Open("sqlite3", core.SqliteWriteDSN(dbPath))
	if err != nil {
		t.Fatalf("competitor open: %v", err)
	}
	t.Cleanup(func() { _ = competitor.Close() })
	tx, err := competitor.Begin()
	if err != nil {
		t.Fatalf("holder begin: %v", err)
	}
	if _, err := tx.Exec(
		`INSERT INTO tool_invocations (id, session_id, tool_name, action, invocation_id, actor_kind, framework_name, payload_hash, result_status, started_at, completed_at, duration_ms)
		 VALUES ('latency-holder', 's', 't', 'a', 'i', 'agent', 'f', 'h', 'success', 0, 0, 0)`,
	); err != nil {
		t.Fatalf("holder insert: %v", err)
	}
	var once sync.Once
	rel := func() {
		once.Do(func() {
			if err := tx.Commit(); err != nil {
				t.Errorf("holder commit: %v", err)
			}
		})
	}
	if holdFor > 0 {
		time.AfterFunc(holdFor, rel)
	}
	return rel
}

// seedWorkspaceContent gives the disposable workspace the state a
// production workspace has, and the content a wake request needs to
// exercise its write path.
//
// The mode/persona files matter for the measurement, not for realism
// alone: their ABSENCE makes the active-state resolver emit a
// diagnostic AuditInfo row ("mode fallback not found on disk"), and
// that row is a write on the read path — it costs a full durable-write
// budget under contention. A production workspace has these files, so
// the harness must too, or the baseline it measures is a different
// (and worse) request than the one an agent actually makes.
func seedWorkspaceContent(t *testing.T, c *latencyClient, ws string) {
	t.Helper()

	// The active-state resolver reads these from the MPM dir.
	for _, rel := range []string{"mode/default.md", "persona/default.md"} {
		path := filepath.Join(ws, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte("# default\n"), 0o600); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}

	// An UNREAD handoff, so the wake context's mark-read write fires.
	//
	// The MCP tools take their action-specific fields in a `params`
	// envelope (see the mpm_handoff schema in registry_list.go) — the
	// top-level shape is {"action": ..., "params": {...}}.
	c.call("tools/call", map[string]interface{}{
		"name": "mpm_handoff",
		"arguments": map[string]interface{}{
			"action": "write",
			"params": map[string]interface{}{
				"summary":    "latency probe handoff",
				"session_id": "latency-probe-session",
			},
		},
	}, 60*time.Second)

	// Memories, so the wake context's retrieval-telemetry writes fire.
	for i := 0; i < 3; i++ {
		c.call("tools/call", map[string]interface{}{
			"name": "mpm_memory",
			"arguments": map[string]interface{}{
				"action": "save",
				"params": map[string]interface{}{
					"content": fmt.Sprintf("latency probe memory %d", i),
				},
			},
		}, 60*time.Second)
	}
}

// TestMeasure_MCPWakeRequestUnderSustainedContention is the BASELINE
// measurement: one MCP wake request, with a competing writer holding
// the write lock for the whole request.
//
// It reports the wall-clock cost and the substrate's own per-statement
// attribution from the watchdog file. It asserts nothing about the
// duration — the duration is the measurement, and the acceptance
// thresholds live in the dedicated acceptance test.
func TestMeasure_MCPWakeRequestUnderSustainedContention(t *testing.T) {
	if testing.Short() {
		t.Skip("contention latency measurement is not a short test")
	}
	ws := t.TempDir()
	c := newLatencyClient(t, ws)
	seedWorkspaceContent(t, c, ws)

	dbPath := filepath.Join(ws, "src", "db", "mpm.db")
	release := holdWriteLock(t, dbPath, 0)
	defer release()

	_, elapsed := c.call("tools/call", map[string]interface{}{
		"name":      "mpm_context",
		"arguments": map[string]interface{}{"action": "read_wake_context"},
	}, 15*time.Minute)

	t.Logf("WALL CLOCK: %.2fs for one MCP wake request under sustained contention", elapsed.Seconds())
	dumpWatchdogAttribution(t, filepath.Join(filepath.Dir(dbPath), "watchdog.jsonl"))
	for _, line := range strings.Split(c.stderr.String(), "\n") {
		if strings.Contains(line, "audit insert failed") || strings.Contains(line, "audit cluster upsert failed") {
			t.Logf("  server stderr: %s", strings.TrimSpace(line))
		}
	}
}

// dumpWatchdogAttribution prints the substrate's own per-statement
// accounting for the contended request, read from the watchdog file the
// DatabaseManager writes next to the database.
//
// It attributes only the statements the substrate tracks (ExecTracked /
// QueryTracked). A BARE pool write is not tracked, so its cost shows up
// as the remainder between the wall clock and the attributed total.
func dumpWatchdogAttribution(t *testing.T, path string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Logf("watchdog file unavailable (%v) — no per-statement attribution", err)
		return
	}
	type ev struct {
		Op         string `json:"op"`
		Reason     string `json:"reason"`
		Level      string `json:"level"`
		DurationMs int64  `json:"duration_ms"`
		Retries    int    `json:"retries"`
		SQLShape   string `json:"sql_shape"`
		Error      string `json:"error"`
	}
	var total int64
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e ev
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue
		}
		total += e.DurationMs
		t.Logf("  %-6s %-14s %7dms retries=%d  %s  %s",
			e.Level, e.Reason, e.DurationMs, e.Retries, e.SQLShape, e.Error)
	}
	t.Logf("  watchdog-attributed total: %dms", total)
}
