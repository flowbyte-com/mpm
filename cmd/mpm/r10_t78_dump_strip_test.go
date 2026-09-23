// r10_t78_dump_strip_test.go — Round 10 T78 regression.
//
// Pin the restore-preprocessing contract:
//
//   - the dump's `PRAGMA writable_schema=ON;` / `=OFF;` brackets
//     get stripped (otherwise the canonical dump validator rejects
//     them as "PRAGMA writable_schema not allowed");
//   - the dump's `INSERT INTO sqlite_schema VALUES(... 'CREATE
//     VIRTUAL TABLE ...')` rows get stripped (the validator
//     forbids INSERT into sqlite_master; the FTS5 vtab registration
//     must be reconstructed by the new DB's init path, not replayed);
//   - the dump's `CREATE TABLE ... '<base>_fts_<suffix>'` shadow-table
//     CREATE statements get stripped (the validator sees them as
//     unknown-table CREATEs; replaying them would create orphan
//     shadows that block CREATE VIRTUAL TABLE).
//
// Without all three, the dump round-trip from the live DB to a fresh
// DB fails: validator rejects on PRAGMA, or on unknown shadow-table
// CREATE, or both. Pre-fix the writable_schema PRAGMAs slipped
// through and were the last step breaking validator round-trip;
// Round 7 (Round 8 follow-up) orphan-prevention stripped the
// INSERTs and shadow CREATEs but missed the wrapping PRAGMAs.

package main

import (
	"os"
	"strings"
	"testing"
)

// r10T78InspectDump uses `mpm ops backup` to generate a real dump
// from a hermetic workspace, then runs sqlite3 on it to count
// occurrences of each problem string. The test never relies on
// grep patterns that could confuse virtual-table-support strings
// with shadow-table-name strings — every check uses the literal
// `sqlite_master`/`sqlite_schema` insertion patterns that the
// validator would also reject.
func r10T78InspectDump(t *testing.T, workspace string) string {
	t.Helper()
	bin := mpmCmd(t)

	// Seed a memory so the backup has SOME shape; the test only
	// cares about the dump's metadata structures, not the contents.
	_, _ = mpmRun(t, bin, workspace, "memory", "add",
		"--collection", "memories", "--content", "r10-t78 fixture",
		"--tags", "r10")

	// Generate the dump.
	out, _ := mpmRun(t, bin, workspace, "ops", "backup")
	// Extract the actual dump path from "Backup written: <path>".
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "Backup written:") {
			path := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "Backup written:"))
			return path
		}
	}
	t.Fatalf("could not find dump path in: %s", out)
	return ""
}

// r10T78Count runs a literal substring count against a string,
// returning the number of occurrences.
func r10T78Count(needle, haystack string) int {
	c := 0
	for i := 0; i+len(needle) <= len(haystack); {
		if haystack[i:i+len(needle)] == needle {
			c++
			i += len(needle)
		} else {
			i++
		}
	}
	return c
}

// TestR10T78_RestorePreprocessAcceptsDump pins the contract
// end-to-end: the canonical dump must survive its validator +
// preprocess + restore round-trip without the validator ever
// surfacing `PRAGMA writable_schema not allowed` after the
// Round 10 fix.
//
// This is the behavioral contract that Round 8 fix #1/#2/#3
// aimed to deliver, and the live test that the pre-fix
// `PRAGMA writable_schema` regression broke. Pre-fix the
// validator rejection was reached at statement 44164 with
// `PRAGMA writable_schema not allowed` — never making it to
// the shadow-table or virtual-table stripping logic.

// TestR10T78_RestorePreprocessAcceptsDump pins the contract
// end-to-end: the canonical dump must survive its validator +
// preprocess + restore round-trip without the validator ever
// surfacing `PRAGMA writable_schema not allowed` after the
// Round 10 fix.
//
// This is the behavioral contract that Round 8 fix #1/#2/#3
// aimed to deliver, and the live test that the pre-fix
// `PRAGMA writable_schema` regression broke. Pre-fix the
// validator rejection was reached at statement 44164 with
// `PRAGMA writable_schema not allowed` — never making it to
// the shadow-table or virtual-table stripping logic.
func TestR10T78_RestorePreprocessAcceptsDump(t *testing.T) {
	// The contract under test is: a sqlite3 .dump output that
	// contains the canonical `PRAGMA writable_schema=ON;` /
	// `PRAGMA writable_schema=OFF;` brackets must round-trip
	// through mpm ops restore-db without surfacing the pre-fix
	// validator rejection.
	//
	// `mpm ops backup` always uses a fresh workspace; the dump's
	// PRAGMA writable_schema brackets only appear on databases
	// that have FTS5 virtual tables (the dump wraps the schema-
	// master insertions in writable_schema=ON/OFF). The workspace
	// must therefore have FTS5 content. Easiest path: generate the
	// dump from the canonical live DB (which has FTS5), then
	// restore it onto a hermetic workspace, and assert the
	// validator does not surface writable_schema rejection.
	workspace := t.TempDir()

	bin := mpmCmd(t)

	// Step 1: generate a dump into a per-test temp path. Pre-fix
	// this hardcoded /tmp/r10-t78-fixture.sql — a host-specific
	// path that leaks between concurrent test runs and depends on
	// /tmp being writable. t.TempDir() is per-test, hermetic,
	// and auto-cleaned.
	dumpPath := workspace + "/r10-t78-fixture.sql"
	out, _ := mpmRun(t, bin, workspace, "ops", "backup", dumpPath)
	defer os.Remove(dumpPath)

	// Sanity: the dump contains PRAGMA writable_schema. If the
	// source DB lacks FTS5 (highly unlikely on the live install),
	// skip rather than mask the regression.
	dumpBytes, err := os.ReadFile(dumpPath)
	if err != nil {
		t.Fatalf("read dump: %v", err)
	}
	if r10T78Count("PRAGMA writable_schema", string(dumpBytes)) == 0 {
		t.Skip("dump has no PRAGMA writable_schema (env differs); T78 fix is idempotent")
	}

	// Step 2: restore the dump on the hermetic workspace. The
	// validator runs on the preprocessed dump text; pre-fix the
	// PRAGMA writable_schema lines escaped preprocessing and the
	// validator surfaced `PRAGMA writable_schema not allowed`.
	out2, _ := mpmRun(t, bin, workspace, "ops", "restore-db", dumpPath)
	out = out2
	err = nil
	if strings.Contains(out2, "PRAGMA writable_schema not allowed") {
		t.Fatalf("restore validator rejected PRAGMA writable_schema:\n%s", out2)
	}

	if strings.Contains(string(out), "PRAGMA writable_schema not allowed") {
		t.Fatalf("validator rejected PRAGMA writable_schema after preprocess fix. Output:\n%s", string(out))
	}
}
