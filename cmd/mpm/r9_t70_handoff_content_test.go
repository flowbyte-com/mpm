// r9_t70_handoff_content_test.go — Round 9 T70 regression.
//
// Pin the handoff write contract:
//
//   - canonical field is `summary` (internal/core/tools/handlers.go
//     handleHandoffWrite)
//   - the substrate REJECTS `content` with a clear error pointing
//     at `summary` — there is NO legacy alias in either direction,
//     because the field was named `summary` from the start. A
//     smoke test using `content` is a stale assumption, not a bug.
//   - the substrate ACCEPTs `summary` and writes through to the
//     handoff row.
//
// This file documents the contract and is intentionally narrow:
// no behavior change here. The fix in T70 is to verify the
// existing behavior holds across smoke runs and to surface a
// clear error when an old CLI caller passes `content`.

package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// r9T70Mpm runs mpm with the test workspace redirected via env.
// We use os.Environ + explicit MPM_WORKSPACE addition rather than
// t.Setenv because the child is a separate process.
func r9T70Mpm(t *testing.T, workspace string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("/home/v/.mpm/bin/mpm", args...)
	env := make([]string, 0, len(os.Environ())+1)
	for _, e := range os.Environ() {
		if !strings.HasPrefix(e, "MPM_WORKSPACE=") {
			env = append(env, e)
		}
	}
	env = append(env, "MPM_WORKSPACE="+workspace)
	cmd.Env = env
	var outB, errB bytes.Buffer
	cmd.Stdout = &outB
	cmd.Stderr = &errB
	err := cmd.Run()
	out := outB.String() + errB.String()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	return out, code
}

// TestR9T70_HandoffSummaryAccepted pins the canonical happy path:
// `summary` writes successfully.
func TestR9T70_HandoffSummaryAccepted(t *testing.T) {
	out, code := r9T70Mpm(t, t.TempDir(),
		"call", "mpm_handoff", "--payload",
		`{"action":"write","params":{"summary":"r9-t70-canonical"}}`)
	if code != 0 {
		t.Fatalf("summary accepted; expected success. Output:\n%s", out)
	}
	if !strings.Contains(out, "r9-t70-canonical") {
		t.Errorf("response should echo canonical summary. Output:\n%s", out)
	}
}

// TestR9T70_HandoffContentRejected pins the rejection contract:
// `content` is NOT a synonym for `summary` and the substrate
// surfaces a structured error pointing at the canonical field name.
func TestR9T70_HandoffContentRejected(t *testing.T) {
	out, code := r9T70Mpm(t, t.TempDir(),
		"call", "mpm_handoff", "--payload",
		`{"action":"write","params":{"content":"r9-t70-stale-content"}}`)
	if code == 0 {
		t.Fatalf("content passed; expected rejection. Output:\n%s", out)
	}
	if !strings.Contains(out, "summary") {
		t.Errorf("error must mention canonical field 'summary'. Output:\n%s", out)
	}
}

// TestR9T70_NoAliases_NoLeakOnRejectedContent pins the no-duplicate-
// fields guarantee: a rejected `content` must not surface anywhere
// in subsequent list/read operations. Pre-fix the substrate could
// have silently coerced the field (the audit-acceptable alternative
// to literal rejection), but the canonical contract is "explicit
// error" — never implicit storage.
func TestR9T70_NoAliases_NoLeakOnRejectedContent(t *testing.T) {
	workspace := t.TempDir()
	// Reject the content-shaped request.
	rejOut, rejCode := r9T70Mpm(t, workspace,
		"call", "mpm_handoff", "--payload",
		`{"action":"write","params":{"content":"r9-t70-no-alias"}}`)
	if rejCode == 0 {
		t.Fatalf("content should fail (no silent aliasing). Output:\n%s", rejOut)
	}

	// A follow-up list should NOT include the rejected content.
	listOut, listCode := r9T70Mpm(t, workspace,
		"call", "mpm_handoff", "--payload",
		`{"action":"list"}`)
	if listCode != 0 {
		t.Fatalf("list failed: %s", listOut)
	}
	if strings.Contains(listOut, "r9-t70-no-alias") {
		t.Errorf("rejected content leaked into persisted state. List:\n%s", listOut)
	}
}
