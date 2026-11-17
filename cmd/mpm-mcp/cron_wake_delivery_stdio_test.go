// cron_wake_delivery_stdio_test.go — the cron wake's delivery over the
// REAL MCP stdio transport.
//
// WHY THIS FILE EXISTS.
//
// The fold's unit tests prove that CheckPendingWakes returns a cron
// wake. They do not prove that an MCP HOST receives it. This file
// drives the actual mpm-mcp binary over the actual stdio JSON-RPC
// protocol, the way a host (OpenCode, Claude Code) does, and asserts
// on the bytes the host would see.
//
// The delivery affordance under test is the <system_wake_notification>
// block: the MCP adapter prepends it to the tool response when the
// fold finds due wakes. For a cron wake it must carry the
// directive_id — the field the cron injection writes "so the agent can
// discover the directive_id at wake-time without an extra lookup"
// (internal/core/scheduled_tasks.go). Before the default fold was
// widened, no cron wake could ever reach that block, so the
// directive_id rendering was unreachable code.
//
// Every test uses a disposable temp workspace and never touches the
// operator's.
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
)

// mcpStdioClient is a JSON-RPC client over a child mpm-mcp process's
// stdin/stdout.
type mcpStdioClient struct {
	t    *testing.T
	cmd  *exec.Cmd
	stdin io.WriteCloser

	mu        sync.Mutex
	responses []map[string]interface{}
	nextID    int
}

// newMCPStdioClient starts mpm-mcp against a disposable workspace and
// completes the MCP handshake.
func newMCPStdioClient(t *testing.T) (*mcpStdioClient, string) {
	t.Helper()

	bin := mcpCmd(t)
	ws := t.TempDir()
	dbPath := filepath.Join(ws, "src", "db", "mpm.db")

	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(),
		"MPM_WORKSPACE="+ws,
		"MPM_DB_PATH="+dbPath,
		// The cron wake is armed through the mpm_wakes tool, which is
		// a specialist outside the default 3-tool surface.
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
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start mpm-mcp: %v", err)
	}

	c := &mcpStdioClient{t: t, cmd: cmd, stdin: stdin}

	// Drain stdout on a goroutine: the server writes newline-delimited
	// JSON frames and never closes stdout while it is serving. The
	// WaitGroup binds the goroutine's lifecycle to the test — the
	// cleanup kills the server, the pipe reaches EOF, and the reader
	// returns before the test ends.
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
		// Kill first: the child's stdout write end closes, the reader
		// sees EOF and returns. Waiting on the reader BEFORE cmd.Wait
		// keeps the documented "do not Wait before the reads complete"
		// hazard away.
		_ = cmd.Process.Kill()
		readerWG.Wait()
		_ = cmd.Wait()
	})

	c.call("initialize", map[string]interface{}{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]interface{}{},
		"clientInfo":      map[string]interface{}{"name": "cron-delivery-probe", "version": "1.0"},
	})
	c.notify("notifications/initialized")

	return c, dbPath
}

// notify sends a JSON-RPC notification (no response expected).
func (c *mcpStdioClient) notify(method string) {
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

// call sends a JSON-RPC request and returns the result.
func (c *mcpStdioClient) call(method string, params map[string]interface{}) map[string]interface{} {
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
	if _, err := c.stdin.Write(append(b, '\n')); err != nil {
		c.t.Fatalf("write request: %v", err)
	}

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		for _, resp := range c.responses {
			if gotID, ok := resp["id"].(float64); ok && int(gotID) == id {
				c.mu.Unlock()
				if errObj, ok := resp["error"]; ok {
					c.t.Fatalf("jsonrpc error for %s: %v", method, errObj)
				}
				result, _ := resp["result"].(map[string]interface{})
				return result
			}
		}
		c.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	c.t.Fatalf("timed out waiting for a response to %s", method)
	return nil
}

// textContent concatenates the text content blocks of a tool result.
func (c *mcpStdioClient) textContent(result map[string]interface{}) string {
	c.t.Helper()
	content, _ := result["content"].([]interface{})
	var b strings.Builder
	for _, item := range content {
		m, _ := item.(map[string]interface{})
		if m["type"] == "text" {
			if s, ok := m["text"].(string); ok {
				b.WriteString(s)
				b.WriteString("\n")
			}
		}
	}
	return b.String()
}

// jsonResult parses the tool's own JSON payload out of a tools/call
// result. The MCP adapter may prepend a <system_wake_notification>
// text block, so the JSON is the LAST text block that parses.
func (c *mcpStdioClient) jsonResult(result map[string]interface{}) map[string]interface{} {
	c.t.Helper()
	content, _ := result["content"].([]interface{})
	for i := len(content) - 1; i >= 0; i-- {
		m, _ := content[i].(map[string]interface{})
		if m["type"] != "text" {
			continue
		}
		s, _ := m["text"].(string)
		var parsed map[string]interface{}
		if err := json.Unmarshal([]byte(s), &parsed); err == nil {
			return parsed
		}
	}
	c.t.Fatalf("no JSON content block in the tool result: %v", result)
	return nil
}

// waitForDB waits for the server to create its database file.
func waitForDB(t *testing.T, dbPath string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(dbPath); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("mpm-mcp did not create %s", dbPath)
}

// TestMCPCronWakeIsDeliveredOverStdio is the headline transport test.
//
// The wake is armed THROUGH the MCP surface (the mpm_wakes tool), so
// the whole chain is exercised: the tool arms the row, the fold finds
// it due, and the wake reaches the host.
//
// The delivery CHANNEL differs by tool, and that is by design: a tool
// whose handler consumes due wakes itself (mpm_wakes schedule,
// mpm_context read_wake_context) reports them in its own envelope
// (WakesPending / wakes_pending), while a tool that does not
// (mpm_memory query) gets the <system_wake_notification> block
// prepended by the adapter. What matters for an agent is the same in
// both cases: the wake arrives, once, carrying its directive_id.
func TestMCPCronWakeIsDeliveredOverStdio(t *testing.T) {
	c, dbPath := newMCPStdioClient(t)
	waitForDB(t, dbPath)

	// Arm a cron wake that is ALREADY due (target_time in the past).
	due := time.Now().Add(-time.Second).Unix()
	res := c.call("tools/call", map[string]interface{}{
		"name": "mpm_wakes",
		"arguments": map[string]interface{}{
			"action": "schedule",
			"params": map[string]interface{}{
				"reason":      "cron:epistemic-compaction",
				"target_time": fmt.Sprintf("%d", due),
				"metadata": map[string]interface{}{
					"kind":         "cron",
					"source":       "cron",
					"task_id":      "epistemic-compaction",
					"directive_id": "mpm-seed-epistemic-compaction",
				},
			},
		},
	})
	if isErr, ok := res["isError"].(bool); ok && isErr {
		t.Fatalf("mpm_wakes schedule failed: %s", c.textContent(res))
	}

	text := c.textContent(res)

	// The delivered wake must be present, and must carry the cron
	// wake's directive_id — the field the cron injection writes "so
	// the agent can discover the directive_id at wake-time without an
	// extra lookup".
	payload := c.jsonResult(res)
	pending, _ := payload["WakesPending"].([]interface{})
	if len(pending) != 1 {
		t.Fatalf("the mpm_wakes response must carry exactly one delivered wake, got %d: %s",
			len(pending), text)
	}
	wake, _ := pending[0].(map[string]interface{})
	meta, _ := wake["metadata"].(map[string]interface{})
	if meta["kind"] != "cron" {
		t.Errorf("the delivered wake must be the cron wake; got kind=%v", meta["kind"])
	}
	if meta["directive_id"] != "mpm-seed-epistemic-compaction" {
		t.Errorf("the delivered cron wake must carry its directive_id; got %v", meta["directive_id"])
	}
	if wake["reason"] != "cron:epistemic-compaction" {
		t.Errorf("the delivered cron wake must carry its reason; got %v", wake["reason"])
	}
	wakeID, _ := wake["id"].(string)
	if wakeID == "" {
		t.Fatal("the delivered wake must carry an id")
	}

	// The row is retired by the fold.
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	var retired int
	if err := db.QueryRow(`SELECT COUNT(*) FROM scheduled_wakes WHERE fired = 1 AND json_extract(metadata,'$.kind')='cron'`).Scan(&retired); err != nil {
		t.Fatalf("read the delivered cron wake: %v", err)
	}
	if retired != 1 {
		t.Errorf("the delivered cron wake must be retired (fired=1), got %d retired rows", retired)
	}

	// Exactly once: a second tool call must not re-deliver it.
	second := c.call("tools/call", map[string]interface{}{
		"name":      "mpm_context",
		"arguments": map[string]interface{}{"action": "read_wake_context"},
	})
	txt := c.textContent(second)
	if strings.Contains(txt, "<system_wake_notification>") {
		t.Errorf("the cron wake was delivered twice (notification block); second response was:\n%s", txt)
	}
	if strings.Contains(txt, wakeID) {
		t.Errorf("the cron wake %s was delivered twice (wake payload); second response was:\n%s", wakeID, txt)
	}
	secondPayload := c.jsonResult(second)
	if count, ok := secondPayload["wakes_pending_count"].(float64); ok && count != 0 {
		t.Errorf("the second call reported %v pending wake(s); the cron wake must not be re-delivered", count)
	}
}

// TestMCPCronWakeReachesTheNotificationBlockOverStdio pins the
// affordance the agent's seeded triage directive describes.
//
// The seeded directive `mpm-seed-wake-triage-policy` tells every agent:
//
//	"Each entry has id, reason, overdue_secs, and (for cron-style
//	 wakes) an optional directive_id."
//
// and
//
//	"The <system_wake_notification> is delivered on EVERY tool call."
//
// A cron wake reaches that block only when the tool's handler does not
// consume due wakes itself — mpm_memory query is such a tool.
func TestMCPCronWakeReachesTheNotificationBlockOverStdio(t *testing.T) {
	c, dbPath := newMCPStdioClient(t)
	waitForDB(t, dbPath)

	// Plant a due cron wake with the production metadata shape.
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	if _, err := db.Exec(
		`INSERT INTO scheduled_wakes (id, target_time, reason, fired, created_by, metadata)
		 VALUES ('wk-stdio-block-cron', ?, 'cron:epistemic-compaction', 0, 'mpm-scheduler', ?)`,
		time.Now().Add(-5*time.Second).Unix(),
		`{"kind":"cron","source":"cron","task_id":"epistemic-compaction",`+
			`"directive_id":"mpm-seed-epistemic-compaction-policy"}`,
	); err != nil {
		t.Fatalf("seed cron wake: %v", err)
	}

	// mpm_memory query does not consume wakes, so the adapter's fold
	// produces the notification block.
	res := c.call("tools/call", map[string]interface{}{
		"name": "mpm_memory",
		"arguments": map[string]interface{}{
			"action": "query",
			"params": map[string]interface{}{"query": "anything"},
		},
	})
	text := c.textContent(res)

	if !strings.Contains(text, "<system_wake_notification>") {
		t.Fatalf("the MCP host did not receive a wake notification; response was:\n%s", text)
	}
	if !strings.Contains(text, `directive_id="mpm-seed-epistemic-compaction-policy"`) {
		t.Errorf("the notification block must carry the cron wake's directive_id; got:\n%s", text)
	}
	// The block renders the wake id TRUNCATED to 8 runes (the
	// formatter's maxRunes bound), so the assertion is on the
	// truncated prefix, not the full id. The truncation is a
	// pre-existing limitation of the notification formatter, not of
	// cron delivery; the full id is available in the tool payload.
	if !strings.Contains(text, "id=wk-stdio") {
		t.Errorf("the notification block must carry the cron wake; got:\n%s", text)
	}
	if !strings.Contains(text, `reason="cron:epistemic-compaction"`) {
		t.Errorf("the notification block must carry the cron wake's reason; got:\n%s", text)
	}

	// Exactly once.
	second := c.call("tools/call", map[string]interface{}{
		"name": "mpm_memory",
		"arguments": map[string]interface{}{
			"action": "query",
			"params": map[string]interface{}{"query": "anything"},
		},
	})
	if txt := c.textContent(second); strings.Contains(txt, "<system_wake_notification>") {
		t.Errorf("the cron wake was delivered twice; second response was:\n%s", txt)
	}
}

// TestMCPCronAndNotificationWakesCoexistOverStdio pins coexistence on
// the real transport.
//
// A cron wake and a notification wake that are both due must arrive
// together — the cron wake must not displace the notification, and
// vice versa.
func TestMCPCronAndNotificationWakesCoexistOverStdio(t *testing.T) {
	c, dbPath := newMCPStdioClient(t)
	waitForDB(t, dbPath)

	// Plant both rows directly so they are due together and land in one
	// fold. The metadata shapes are the production ones.
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	now := time.Now().Unix()
	rows := []struct {
		id       string
		reason   string
		metadata string
	}{
		{
			id:       "wk-stdio-notification",
			reason:   "VERIFICATION: a notification wake",
			metadata: `{"kind":"notification"}`,
		},
		{
			id:     "wk-stdio-cron",
			reason: "cron:epistemic-compaction",
			metadata: `{"kind":"cron","source":"cron","task_id":"epistemic-compaction",` +
				`"directive_id":"mpm-seed-epistemic-compaction"}`,
		},
	}
	for _, r := range rows {
		if _, err := db.Exec(
			`INSERT INTO scheduled_wakes (id, target_time, reason, fired, created_by, metadata)
			 VALUES (?, ?, ?, 0, 'test', ?)`,
			r.id, now-5, r.reason, r.metadata,
		); err != nil {
			t.Fatalf("seed %s: %v", r.id, err)
		}
	}

	res := c.call("tools/call", map[string]interface{}{
		"name":      "mpm_context",
		"arguments": map[string]interface{}{"action": "read_wake_context"},
	})
	text := c.textContent(res)
	if !strings.Contains(text, "wk-stdio-cron") {
		t.Errorf("the wake context must carry the cron wake; got:\n%s", text)
	}
	if !strings.Contains(text, "wk-stdio-notification") {
		t.Errorf("the wake context must carry the notification wake; got:\n%s", text)
	}
	if !strings.Contains(text, `"directive_id":"mpm-seed-epistemic-compaction"`) {
		t.Errorf("the delivered cron wake must carry its directive_id; got:\n%s", text)
	}
	// Both wakes are retired — the fold retired what it delivered.
	var fired int
	if err := db.QueryRow(`SELECT COUNT(*) FROM scheduled_wakes WHERE fired = 1`).Scan(&fired); err != nil {
		t.Fatalf("count retired wakes: %v", err)
	}
	if fired != 2 {
		t.Errorf("retired %d wake(s), want 2: the fold must retire both the cron and the notification wake", fired)
	}
}

// TestMCPNoWakeNotificationWhenNothingIsDue is the negative control.
//
// The notification block must not appear when there is nothing due —
// otherwise the test above would pass for the wrong reason.
func TestMCPNoWakeNotificationWhenNothingIsDue(t *testing.T) {
	c, dbPath := newMCPStdioClient(t)
	waitForDB(t, dbPath)

	res := c.call("tools/call", map[string]interface{}{
		"name":      "mpm_context",
		"arguments": map[string]interface{}{"action": "read_wake_context"},
	})
	if text := c.textContent(res); strings.Contains(text, "<system_wake_notification>") {
		t.Errorf("a wake notification was emitted with nothing due:\n%s", text)
	}
}
