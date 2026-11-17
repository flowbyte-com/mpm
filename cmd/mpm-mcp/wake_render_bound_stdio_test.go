// wake_render_bound_stdio_test.go — the byte bound on the agent-visible
// response, measured over the REAL MCP stdio transport.
//
// WHY THIS FILE EXISTS.
//
// The core unit tests (internal/core/wake_render_bound_test.go) prove
// that the fold bounds what it delivers. They do not prove that the
// bytes an MCP HOST receives are bounded.
//
// The transport has TWO surfaces and they are bounded SEPARATELY:
//
//   - the RENDERED <system_wake_notification> block, which the MCP
//     adapter prepends to every tool response AFTER the output-policy
//     spill decision — so the policy's 20,480-byte threshold never
//     applied to it; and
//   - the STRUCTURED payload (WakesPending / wakes_pending), which a
//     tool whose handler consumes due wakes itself returns in its own
//     envelope.
//
// Both surfaces carry the delivered wake rows, so both had to be
// bounded. Pre-fix, one wake with a 204,800-byte directive_id produced
// a 204,957-byte rendered block and a 205,020-byte structured row.
//
// This file drives the actual mpm-mcp binary over actual JSON-RPC
// frames on stdin/stdout and measures the bytes the HOST receives.
//
// Every test uses a disposable temp workspace and never touches the
// operator's.
package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// mcpOutputPolicyThresholdBytes is the MCP output policy's own
// threshold. The policy applies to the tool's PAYLOAD alone; the wake
// block is prepended after the spill decision, so it never
// participated in it.
const mcpOutputPolicyThresholdBytes = 20480

// plantOversizedWake plants one due cron wake whose directive_id is
// oversized, exactly as a legacy row or a row written by another
// producer would look.
func plantOversizedWake(t *testing.T, dbPath string, id string, directiveBytes int) {
	t.Helper()
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	oversized := strings.Repeat("A", directiveBytes)
	if _, err := db.Exec(
		`INSERT INTO scheduled_wakes (id, target_time, reason, fired, created_by, metadata)
		 VALUES (?, ?, 'probe', 0, 'mpm-scheduler', ?)`,
		id, time.Now().Add(-time.Hour).Unix(),
		fmt.Sprintf(`{"kind":"cron","source":"cron","directive_id":%q}`, oversized),
	); err != nil {
		t.Fatalf("plant oversized wake: %v", err)
	}
}

// plantMaximalWakes plants n due wakes of EVERY agent-owned kind whose
// directive_id is at the render bound, so each kind's contribution
// approaches its own byte cap.
func plantMaximalWakes(t *testing.T, dbPath string, n int) {
	t.Helper()
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	directive := strings.Repeat("d", 256)
	for _, kind := range []string{"notification", "cron", "cascade"} {
		for i := 0; i < n; i++ {
			if _, err := db.Exec(
				`INSERT INTO scheduled_wakes (id, target_time, reason, fired, created_by, metadata)
				 VALUES (?, ?, 'probe', 0, 'mpm-scheduler', ?)`,
				fmt.Sprintf("wk-stdio-max-%s-%02d", kind, i),
				time.Now().Add(-time.Hour).Unix(),
				fmt.Sprintf(`{"kind":%q,"source":"probe","directive_id":%q}`, kind, directive),
			); err != nil {
				t.Fatalf("plant maximal %s wake: %v", kind, err)
			}
		}
	}
}

// TestMCPNotificationBlockIsBoundedWithAnOversizedPersistedWake is the
// transport reproduction of the rendered block.
//
// Pre-fix: the host received a 204,957-byte response for ONE wake,
// against a 4,096-byte per-kind cap.
func TestMCPNotificationBlockIsBoundedWithAnOversizedPersistedWake(t *testing.T) {
	c, dbPath := newMCPStdioClient(t)
	waitForDB(t, dbPath)

	plantOversizedWake(t, dbPath, "wk-stdio-oversized", 200*1024)

	// mpm_memory query does not consume due wakes, so the adapter's
	// fold produces the notification block.
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

	// The block itself must respect the per-kind byte cap. The
	// oversized wake is the only one, so its kind's whole contribution
	// is the block.
	block := wakeBlockOf(text)
	if block == "" {
		t.Fatalf("could not find the wake block in the response:\n%s", text)
	}
	const perKindCap = 4096
	if len(block) > perKindCap {
		t.Errorf("the rendered wake block must respect the per-kind byte cap: %d bytes > %d",
			len(block), perKindCap)
	}

	// And the whole response must stay small.
	if len(text) > 64*1024 {
		t.Errorf("the agent-visible response must stay bounded: %d bytes", len(text))
	}

	// The cut is VISIBLE: the agent must be able to tell that the
	// directive_id it was handed is not the whole one.
	if !strings.Contains(block, "truncated") {
		t.Errorf("a directive_id that was cut must say so; block was:\n%s", block)
	}
}

// TestMCPStructuredPayloadIsBoundedWithAnOversizedPersistedWake is the
// transport reproduction of the STRUCTURED surface.
//
// This is a different surface from the rendered block, with a
// different bound, and it is the one `mpm call` returns. Pre-fix the
// same wake produced a 205,020-byte structured row.
func TestMCPStructuredPayloadIsBoundedWithAnOversizedPersistedWake(t *testing.T) {
	c, dbPath := newMCPStdioClient(t)
	waitForDB(t, dbPath)

	plantOversizedWake(t, dbPath, "wk-stdio-oversized-structured", 200*1024)

	// mpm_context read_wake_context consumes due wakes itself and
	// returns them in its own envelope.
	res := c.call("tools/call", map[string]interface{}{
		"name":      "mpm_context",
		"arguments": map[string]interface{}{"action": "read_wake_context"},
	})
	text := c.textContent(res)

	if len(text) > 64*1024 {
		t.Errorf("the agent-visible response must stay bounded: %d bytes", len(text))
	}

	payload := c.jsonResult(res)
	pending, _ := payload["wakes_pending"].([]interface{})
	if len(pending) == 0 {
		t.Fatalf("the returning agent must receive the pending wake; response was:\n%s", text)
	}
	wake, _ := pending[0].(map[string]interface{})
	meta, _ := wake["metadata"].(map[string]interface{})
	directiveID, _ := meta["directive_id"].(string)
	if len(directiveID) > 256 {
		t.Errorf("the structured directive_id must be bounded: %d bytes", len(directiveID))
	}
	if !strings.Contains(directiveID, "truncated") {
		t.Errorf("a directive_id that was cut must say so; got %q", directiveID)
	}
}

// TestMCPArmingRefusesAnOversizedDirectiveID is the transport
// reproduction of the arming vector.
//
// Pre-fix: an ordinary `mpm_wakes schedule` call armed the oversized
// wake, and the next tool call returned a 102,786-byte response.
func TestMCPArmingRefusesAnOversizedDirectiveID(t *testing.T) {
	c, dbPath := newMCPStdioClient(t)
	waitForDB(t, dbPath)

	oversized := strings.Repeat("D", 100*1024)
	res := c.call("tools/call", map[string]interface{}{
		"name": "mpm_wakes",
		"arguments": map[string]interface{}{
			"action": "schedule",
			"params": map[string]interface{}{
				"reason":      "probe",
				"target_time": "24h",
				"metadata": map[string]interface{}{
					"kind":         "cron",
					"directive_id": oversized,
				},
			},
		},
	})
	text := c.textContent(res)
	if !strings.Contains(text, "directive_id") {
		t.Errorf("the refusal must name the field it refused; response was:\n%s", text)
	}

	// Nothing was armed.
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM scheduled_wakes`).Scan(&count); err != nil {
		t.Fatalf("count wakes: %v", err)
	}
	if count != 0 {
		t.Errorf("a refused wake must not be persisted: %d row(s)", count)
	}

	// And the next tool call is bounded, because nothing oversized was
	// armed.
	next := c.call("tools/call", map[string]interface{}{
		"name": "mpm_memory",
		"arguments": map[string]interface{}{
			"action": "query",
			"params": map[string]interface{}{"query": "anything"},
		},
	})
	if body := c.textContent(next); len(body) > 64*1024 {
		t.Errorf("the next response must stay bounded: %d bytes", len(body))
	}
}

// TestMCPWakeBlockFillsThePerKindCapsAndStaysBounded measures the
// block at its FULL size.
//
// A tool whose handler does NOT consume due wakes (mpm_memory query)
// gets the block prepended by the adapter, and the adapter's fold
// delivers every agent-owned kind in one go — so this is the block at
// its largest.
//
// It must be LARGE (it really did fill the per-kind caps, so the
// measurement is not vacuous) and BOUNDED (it is within the sum of
// those caps).
func TestMCPWakeBlockFillsThePerKindCapsAndStaysBounded(t *testing.T) {
	c, dbPath := newMCPStdioClient(t)
	waitForDB(t, dbPath)

	plantMaximalWakes(t, dbPath, 12)

	res := c.call("tools/call", map[string]interface{}{
		"name": "mpm_memory",
		"arguments": map[string]interface{}{
			"action": "query",
			"params": map[string]interface{}{"query": "anything"},
		},
	})
	text := c.textContent(res)

	block := wakeBlockOf(text)
	if block == "" {
		t.Fatalf("could not find the wake block in the response:\n%s", text)
	}

	const perKindCap = 4096
	if len(block) > 3*perKindCap {
		t.Errorf("the rendered block must respect the sum of the per-kind caps: %d bytes > %d",
			len(block), 3*perKindCap)
	}
	// The measurement must not be vacuous: the block really did fill
	// the per-kind caps.
	if len(block) < perKindCap {
		t.Errorf("the block must really fill the per-kind caps for this measurement to "+
			"mean anything: %d bytes < %d", len(block), perKindCap)
	}
	t.Logf("block at full size: %d bytes (cap %d x 3)", len(block), perKindCap)
}

// TestMCPEnvelopeAccountsForTheBlockAndThePayloadSeparately pins the
// ACCOUNTING, which is the claim this PR's normative documents used to
// get wrong.
//
// The truth, measured over the real transport:
//
//   - the wake block is bounded by the PER-KIND caps;
//   - the tool's own payload is bounded by the OUTPUT POLICY, which
//     spills it — and the policy's decision is taken on the payload
//     ALONE, before the block is prepended;
//   - the envelope is the SUM of the two.
//
// The two do NOT share one budget. This test measures all three
// quantities separately rather than asserting a single bound on their
// sum.
//
// The case chosen makes the separation visible: the tool's payload is
// large enough to be SPILLED by the output policy, while the wake
// block — which the policy never sees — is still delivered whole, and
// is within its own caps.
func TestMCPEnvelopeAccountsForTheBlockAndThePayloadSeparately(t *testing.T) {
	c, dbPath := newMCPStdioClient(t)
	waitForDB(t, dbPath)

	// A block near the per-kind caps: enough maximal rows of all three
	// kinds to fill them.
	plantMaximalWakes(t, dbPath, 12)

	// A tool payload larger than the output policy's threshold.
	bigReason := strings.Repeat("r", 40*1024)
	res := c.call("tools/call", map[string]interface{}{
		"name": "mpm_wakes",
		"arguments": map[string]interface{}{
			"action": "schedule",
			"params": map[string]interface{}{
				"reason":      bigReason,
				"target_time": "24h",
				"metadata": map[string]interface{}{
					"kind": "notification",
				},
			},
		},
	})
	text := c.textContent(res)

	// 1. The BLOCK is present and bounded by its OWN caps, even though
	//    the tool's payload was far larger than the policy's
	//    threshold.
	block := wakeBlockOf(text)
	if block == "" {
		t.Fatalf("could not find the wake block in the response:\n%s", text)
	}
	const perKindCap = 4096
	if len(block) > 3*perKindCap {
		t.Errorf("the rendered block must respect the sum of the per-kind caps: %d bytes", len(block))
	}

	// 2. The policy bounded the tool's PAYLOAD alone: the response
	//    carries a spill envelope that reports the payload's true
	//    size, and that size is over the policy's threshold.
	envelope := parseJSONObject(t, res)
	if envelope["status"] != "spilled" {
		t.Fatalf("this test needs a payload over the output policy's threshold so the "+
			"policy's independent accounting is visible; got %v", envelope["status"])
	}
	payloadSize, _ := envelope["size_bytes"].(float64)
	if int(payloadSize) <= mcpOutputPolicyThresholdBytes {
		t.Fatalf("this test needs a payload over the policy threshold; got %d bytes", int(payloadSize))
	}

	// 3. The ENVELOPE is the SUM: the block is prepended to the
	//    (spilled) payload, and the block's bytes are present even
	//    though the payload itself was replaced by a pointer.
	if !strings.Contains(text, "<system_wake_notification>") {
		t.Errorf("the wake block must be delivered even when the tool's payload was spilled")
	}
	if len(text) <= len(block) {
		t.Errorf("the envelope must be the block plus the payload's envelope: "+
			"envelope=%d block=%d", len(text), len(block))
	}
	t.Logf("accounting: block=%d bytes (cap %d x 3), tool payload=%d bytes "+
		"(policy threshold %d, spilled), envelope=%d bytes",
		len(block), perKindCap, int(payloadSize), mcpOutputPolicyThresholdBytes, len(text))
}

// wakeBlockOf extracts the <system_wake_notification> block from a
// response, markers included.
func wakeBlockOf(response string) string {
	start := strings.Index(response, "<system_wake_notification>")
	if start < 0 {
		return ""
	}
	end := strings.Index(response[start:], "</system_wake_notification>")
	if end < 0 {
		return ""
	}
	return response[start : start+end+len("</system_wake_notification>")]
}

// parseJSONObject returns the tool's own JSON payload: the LAST text
// content block that parses as a JSON object. The MCP adapter may
// prepend the wake block, so the payload is not necessarily the first
// block.
func parseJSONObject(t *testing.T, result map[string]interface{}) map[string]interface{} {
	t.Helper()
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
	t.Fatalf("no JSON content block in the tool result: %v", result)
	return nil
}
