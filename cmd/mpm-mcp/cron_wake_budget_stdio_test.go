// cron_wake_budget_stdio_test.go — the delivery budget over the REAL
// MCP stdio transport.
//
// WHY THIS FILE EXISTS.
//
// The fold's unit tests (internal/core/wake_delivery_budget_test.go)
// prove that CheckPendingWakes bounds what it delivers. They do not
// prove that the AGENT-VISIBLE RESPONSE is bounded: the MCP adapter
// prepends the rendered <system_wake_notification> block to every tool
// response AFTER the output-policy spill decision, so the spill
// mechanism does not bound the wake block.
//
// This file drives the actual mpm-mcp binary over actual JSON-RPC
// frames on stdin/stdout and measures the bytes the HOST receives.
//
// Every test uses a disposable temp workspace and never touches the
// operator's.
package main

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"
)

// plantCronBacklog inserts n due cron-kind wakes directly, in one
// transaction, and returns the DB handle so the caller can inspect it.
func plantCronBacklog(t *testing.T, dbPath string, n int, age time.Duration) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback()
	now := time.Now().Add(-age).Unix()
	stmt, err := tx.Prepare(`
		INSERT INTO scheduled_wakes (id, target_time, reason, fired, created_by, metadata)
		VALUES (?, ?, ?, 0, 'mpm-scheduler', ?)
	`)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer stmt.Close()
	for i := 0; i < n; i++ {
		if _, err := stmt.Exec(
			fmt.Sprintf("wk-stdio-budget-%06d", i),
			now+int64(i),
			"cron:epistemic-compaction",
			`{"kind":"cron","source":"cron","task_id":"epistemic-compaction",`+
				`"directive_id":"mpm-seed-epistemic-compaction"}`,
		); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return db
}

// callMemoryQuery issues one mpm_memory query — a tool whose handler
// does NOT consume due wakes, so the adapter's fold produces the
// notification block.
func callMemoryQuery(c *mcpStdioClient) string {
	c.t.Helper()
	res := c.call("tools/call", map[string]interface{}{
		"name": "mpm_memory",
		"arguments": map[string]interface{}{
			"action": "query",
			"params": map[string]interface{}{"query": "anything"},
		},
	})
	return c.textContent(res)
}

// TestMCPWakeBlockIsBoundedOnALargeBacklog is the HIGH-2 transport
// reproduction.
//
// Pre-fix: 10,080 pending cron wakes produced a ~1 MB
// <system_wake_notification> block in a single tool response.
func TestMCPWakeBlockIsBoundedOnALargeBacklog(t *testing.T) {
	c, dbPath := newMCPStdioClient(t)
	waitForDB(t, dbPath)

	const backlog = 10_080
	plantCronBacklog(t, dbPath, backlog, 7*24*time.Hour)

	text := callMemoryQuery(c)
	if !strings.Contains(text, "<system_wake_notification>") {
		t.Fatalf("the MCP host did not receive a wake notification; response was:\n%s", text)
	}

	// The agent-visible response must stay small. The MCP output
	// policy's own boundary is 20 KB, and the wake block is prepended
	// on top of the tool's payload, so the block itself must be well
	// inside that.
	if len(text) > 64*1024 {
		t.Errorf("the agent-visible response must be bounded by the delivery budget: "+
			"%d bytes for a %d-wake backlog", len(text), backlog)
	}

	// The block must carry a bounded number of entries.
	entries := strings.Count(text, "  • id=")
	if entries > 64 {
		t.Errorf("the wake block must carry a bounded number of entries: %d", entries)
	}
	if entries == 0 {
		t.Errorf("the wake block must carry at least one entry")
	}

	// Nothing was settled beyond what was delivered.
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	var fired int
	if err := db.QueryRow(`SELECT COUNT(*) FROM scheduled_wakes WHERE fired = 1`).Scan(&fired); err != nil {
		t.Fatalf("count settled wakes: %v", err)
	}
	if fired != entries {
		t.Errorf("settled %d wake(s) but the block carried %d entries: the fold must "+
			"settle exactly what it delivered", fired, entries)
	}
}

// TestMCPRempeatedCallsDrainTheBacklog pins that the budget
// THROTTLES rather than DROPS, over the real transport.
//
// The backlog drains across successive calls, and every response stays
// bounded.
func TestMCPRempeatedCallsDrainTheBacklog(t *testing.T) {
	c, dbPath := newMCPStdioClient(t)
	waitForDB(t, dbPath)

	const backlog = 200
	plantCronBacklog(t, dbPath, backlog, time.Hour)

	delivered := 0
	for call := 0; call < 50; call++ {
		text := callMemoryQuery(c)
		if len(text) > 64*1024 {
			t.Fatalf("call %d: the agent-visible response must stay bounded: %d bytes",
				call, len(text))
		}
		entries := strings.Count(text, "  • id=")
		delivered += entries
		if delivered >= backlog {
			break
		}
	}
	if delivered != backlog {
		t.Errorf("delivered %d of %d wakes across the calls: the budget must throttle, "+
			"not drop", delivered, backlog)
	}
}

// TestMCPWakeBlockIsBoundedAfterAgentInactivity pins the case an
// operator actually sees: the agent has been away and comes back.
//
// One one-minute cron task produces ~60 wakes per hour, so a returning
// agent faces that pool. The burst must be bounded, and the rest must
// remain deliverable.
func TestMCPWakeBlockIsBoundedAfterAgentInactivity(t *testing.T) {
	c, dbPath := newMCPStdioClient(t)
	waitForDB(t, dbPath)

	plantCronBacklog(t, dbPath, 60, time.Hour)

	text := callMemoryQuery(c)
	entries := strings.Count(text, "  • id=")
	if entries == 0 {
		t.Fatalf("the returning agent must receive the pending cron wakes; response was:\n%s", text)
	}
	if entries > 64 {
		t.Errorf("the returning agent must receive a bounded burst: %d entries", entries)
	}
	if len(text) > 64*1024 {
		t.Errorf("the returning agent's response must be bounded: %d bytes", len(text))
	}

	// The rest is still pending — not lost.
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	var pending int
	if err := db.QueryRow(`SELECT COUNT(*) FROM scheduled_wakes WHERE fired = 0`).Scan(&pending); err != nil {
		t.Fatalf("count pending wakes: %v", err)
	}
	if pending == 0 {
		t.Errorf("the wakes beyond the budget must remain pending, not be settled silently")
	}
}
