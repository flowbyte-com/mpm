// shred_session_soft_delete_test.go — pins the soft-delete contract for
// `mpm shred session <id>` and its alias `mpm session shred <id>`.
//
// # What this test exists to prevent
//
// `handleShredSession` is documented (help text, SPEC lifecycle table)
// as a SOFT delete: the row stays in the substrate with full content +
// history preserved, and is recoverable via `mpm memory restore <id>`.
// Internally it just calls store.DeleteMemory, which sets `deleted_at`.
//
// Before the 2026-10-04 contract fix, the success message rendered
// the operation as "Session shredded: <id>". That is the same class
// of lie `mpm shred sessions` used to print at the bulk layer — it
// told the operator the row was destroyed when in fact the
// `deleted_at` tombstone makes it restorable. The only place that
// needed to change was the response string, but a string regression
// is easy to reintroduce in a follow-up edit, so both layers of the
// contract are pinned here:
//
//   1. Source-pinning: the success template in handlers_shred.go
//      must use the verb "soft-deleted" and must not describe the
//      operation as a shred. Fast, no fixtures, catches editorial
//      drift in the same turn.
//
//   2. Behavioural: the row is still in `memories` after the call
//      (with `deleted_at` set, content preserved), and
//      `mpm memory restore <id>` clears the tombstone so the
//      memory becomes visible again. End-to-end through the real
//      router. Catches drift in store.DeleteMemory or
//      handleShredSession's collection choice.

package main

import (
	"database/sql"
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestShredSession_SuccessMessageAdvertisesSoftDelete pins the wording
// of the success template in handlers_shred.go.
//
// It is intentionally source-pinning rather than a behavioural check:
// the wording IS the user-facing contract. Help text already says
// "Soft-delete a session by ID" and the SPEC lifecycle table says
// soft-delete; the only drift point between the two is the response
// string, and that is the only thing this assertion guards.
func TestShredSession_SuccessMessageAdvertisesSoftDelete(t *testing.T) {
	data, err := os.ReadFile("handlers_shred.go")
	if err != nil {
		t.Fatalf("read handlers_shred.go: %v", err)
	}
	src := string(data)

	// Locate handleShredSession. The check is structural so a future
	// refactor that splits the file or renames the function shows up
	// immediately with a clear message.
	start := strings.Index(src, "func handleShredSession(")
	if start < 0 {
		t.Fatalf("could not locate handleShredSession in handlers_shred.go")
	}
	body := src[start:]

	// The success template must name the operation as a soft delete
	// and must tell the operator how to reverse it. Both halves are
	// part of the contract — a future edit that strips the recovery
	// hint regresses the help-text / SPEC alignment.
	if !strings.Contains(body, "Session soft-deleted:") {
		t.Errorf("handleShredSession success message must use the verb 'soft-deleted' "+
			"(the operation sets deleted_at and is recoverable); got body:\n%s", body)
	}
	if !strings.Contains(body, "mpm memory restore") {
		t.Errorf("handleShredSession success message must point operators at "+
			"'mpm memory restore <id>' so the recoverability is visible at the "+
			"response layer; got body:\n%s", body)
	}

	// The misleading wording from the pre-fix handler was "Session
	// shredded:". The recovery hint is allowed to mention the
	// historical name as the command form (e.g. in a parenthetical
	// like `(recoverable via 'mpm memory restore <id>')`) so we do
	// NOT assert that the substring "shred" is absent from the
	// entire body. We DO assert that no success template uses the
	// verb "shredded" — that is the word that lied about the
	// operation's effect.
	if strings.Contains(body, "Session shredded:") {
		t.Errorf("handleShredSession must not label the operation as 'shredded'; "+
			"the row stays in the substrate with deleted_at set. got body:\n%s", body)
	}

	// The failure path must also be truthful: a missing/invalid id
	// must not say the operation failed to "shred" — it failed to
	// soft-delete. Same lie class, smaller blast radius.
	if !strings.Contains(body, "Failed to soft-delete session") {
		t.Errorf("handleShredSession error path must use 'Failed to soft-delete', "+
			"matching the success message's verb; got body:\n%s", body)
	}
}

// TestShredSession_PreservesRowAndAcceptsRestore pins the actual
// behaviour end-to-end through the router.
//
// Pre-2026-10-04 the success message read "Session shredded: <id>".
// A reader who believed it would assume the row was gone. The
// store.DeleteMemory call underneath is the same soft delete the
// help text and SPEC describe, so the test is also a guard against
// a future refactor that swaps in a hard delete (DELETE FROM
// memories ...) without updating the success message and the SPEC
// row together.
func TestShredSession_PreservesRowAndAcceptsRestore(t *testing.T) {
	clearForce(t)
	workspace(t) // pin MPM_WORKSPACE to a fresh temp dir; the value itself is not used in this test

	// Seed a session via the same store path the existing
	// `mpm session add` handler uses. Going through the store
	// directly avoids the only-test-in-this-package wrinkle that
	// `mpm memory add` hardcodes collection="memories" — we need
	// a row whose collection column is "session" so
	// handleShredSession's store.DeleteMemory(id, "session") lands
	// on it.
	store := getMemoryStore()
	const seedContent = "soft-delete test session sentinel"
	mem, err := store.AddMemory(seedContent, "session", nil, map[string]interface{}{}, "", "test")
	if err != nil || mem == nil {
		t.Fatalf("seed session: err=%v mem=%v", err, mem)
	}
	id := mem.ID

	// Sanity: the row exists and is not yet tombstoned.
	dm := getDBConcrete()
	if dm == nil {
		t.Fatalf("getDBConcrete() returned nil after seed")
	}
	var deletedBefore sql.NullInt64
	var contentBefore string
	if err := dm.SQLDB().QueryRow(
		"SELECT deleted_at, content FROM memories WHERE id = ? AND collection = ?",
		id, "session",
	).Scan(&deletedBefore, &contentBefore); err != nil {
		t.Fatalf("pre-shred query: %v", err)
	}
	if deletedBefore.Valid {
		t.Fatalf("seed row already has deleted_at set; the test fixture is poisoned")
	}
	if contentBefore != seedContent {
		t.Fatalf("seed content mismatch: got %q want %q", contentBefore, seedContent)
	}

	// Run `mpm shred session <id>` through the real router. This
	// exercises parseFlags -> the shred dispatcher ->
	// handleShredSession -> store.DeleteMemory(id, "session"),
	// exactly the production path.
	code, out := runCLI(t, "shred", "session", id)
	if code != 0 {
		t.Fatalf("`mpm shred session %s` exit %d, want 0\n%s", id, code, out)
	}

	// Truthful response: the verb matches the operation.
	if !strings.Contains(out, "soft-deleted") {
		t.Errorf("success message must say 'soft-deleted'; got:\n%s", out)
	}
	if strings.Contains(out, "shredded:") {
		t.Errorf("success message must not say 'shredded:'; the row stays in the substrate. got:\n%s", out)
	}
	if !strings.Contains(out, "mpm memory restore") {
		t.Errorf("success message must point at the recovery path; got:\n%s", out)
	}
	if !strings.Contains(out, id) {
		t.Errorf("success message must name the id (%s); got:\n%s", id, out)
	}

	// The row must still exist with content preserved and
	// deleted_at set. A hard delete would have RowsAffected() == 0
	// here; a soft delete keeps the row but flips the tombstone.
	var deletedAfter sql.NullInt64
	var contentAfter string
	if err := dm.SQLDB().QueryRow(
		"SELECT deleted_at, content FROM memories WHERE id = ? AND collection = ?",
		id, "session",
	).Scan(&deletedAfter, &contentAfter); err != nil {
		t.Fatalf("post-shred query: %v", err)
	}
	if !deletedAfter.Valid {
		t.Errorf("row's deleted_at is still NULL after `mpm shred session`; " +
			"the soft-delete path is not actually setting the tombstone")
	}
	if contentAfter != seedContent {
		t.Errorf("row content changed across soft-delete: got %q want %q "+
			"(soft-delete must preserve content)", contentAfter, seedContent)
	}

	// The soft-deleted row must NOT appear in the default
	// recent-memories listing. GetRecent filters on deleted_at IS
	// NULL, so a row with a tombstone is correctly hidden from
	// recall — but recoverable on demand.
	recent, err := store.GetRecent(50)
	if err != nil {
		t.Fatalf("GetRecent: %v", err)
	}
	for _, m := range recent {
		if m.ID == id {
			t.Errorf("soft-deleted row %s is visible in GetRecent; the "+
				"deleted_at tombstone is not being honoured by recall", id)
		}
	}

	// Recover: `mpm memory restore <id>` must clear the tombstone
	// and bring the row back into the default listing.
	code, out = runCLI(t, "memory", "restore", id)
	if code != 0 {
		t.Fatalf("`mpm memory restore %s` exit %d, want 0\n%s", id, code, out)
	}
	var deletedAfterRestore sql.NullInt64
	if err := dm.SQLDB().QueryRow(
		"SELECT deleted_at FROM memories WHERE id = ? AND collection = ?",
		id, "session",
	).Scan(&deletedAfterRestore); err != nil {
		t.Fatalf("post-restore query: %v", err)
	}
	if deletedAfterRestore.Valid {
		t.Errorf("deleted_at is still set after `mpm memory restore`; the "+
			"restore path is not clearing the tombstone (got %v)", deletedAfterRestore.Int64)
	}

	// The row is visible again. Same default listing it was hidden
	// from a moment ago.
	recent, err = store.GetRecent(50)
	if err != nil {
		t.Fatalf("GetRecent after restore: %v", err)
	}
	found := false
	for _, m := range recent {
		if m.ID == id {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("restored row %s is not visible in GetRecent; the "+
			"restore path is not actually bringing the row back into recall", id)
	}
}

// TestShredSession_HelpAndSourceAgreeOnSemantics is a tiny
// belt-and-braces check that the help text and the source file
// agree on the soft-delete verb. The existing help-truthfulness
// test in shred_help_truthfulness_test.go already pins the help
// text; this test just makes the cross-file invariant a one-step
// read for a future reviewer.
func TestShredSession_HelpAndSourceAgreeOnSemantics(t *testing.T) {
	clearForce(t)
	workspace(t)

	helpData, err := os.ReadFile("handlers_shred.go")
	if err != nil {
		t.Fatalf("read handlers_shred.go: %v", err)
	}
	helpSrc := string(helpData)

	helpRe := regexp.MustCompile(`(?m)^  mpm shred session <id>\s+Soft-delete a session by ID \(recoverable, NOT a shred\)`)
	if !helpRe.MatchString(helpSrc) {
		t.Errorf("help text for `mpm shred session` must explicitly call out "+
			"the soft-delete / recoverable / not-a-shred semantics; not found in:\n%s", helpSrc)
	}
}
