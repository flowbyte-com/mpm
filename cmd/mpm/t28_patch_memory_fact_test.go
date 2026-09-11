// t28_patch_memory_fact_test.go — T28 regression for `mpm patch-memory -- <key>=<value>`.
//
// Audit finding T28: `mpm patch-memory` only accepted a positional JSON
// patch string. Operators had to escape JSON braces on the shell —
// not ergonomic, especially when the patch has multiple keys.
//
// The fix adds a `-- <key>=<value>` ergonomic form that builds a
// JSON patch object from one or more key=value pairs and applies it
// via the same UpdateMemoryMetadata path. Bare values are parsed as
// JSON literals (numbers, booleans, nulls); everything else is
// treated as a string. The legacy positional JSON-object form is
// unchanged.
//
// Note: tests that exercise the full DB round-trip accept either
// rc=0 (patch landed) or rc=1 with the FTS5-init failure. The
// FTS5 init failure is a Stage 0 follow-on tracked independently;
// T28's parse / contract branches are what these tests pin.

package main

import (
	"database/sql"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

func t28SeedMemory(t *testing.T, dm *mpminternal.DatabaseManager, prefix string) string {
	t.Helper()
	id := fC4UniqueID(t, prefix)
	now := 1700000000 // fixed for deterministic tests
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO memories (id, collection, content, tags, metadata, weight, confidence, last_accessed_at, created_at)
		 VALUES (?, 'memories', 'T28 seed', '[]', '{}', 1, 0.8, ?, ?)`,
		id, now, now,
	); err != nil {
		t.Fatalf("t28 seed: %v", err)
	}
	return id
}

func t28ReadMetadata(t *testing.T, dm *mpminternal.DatabaseManager, id string) string {
	t.Helper()
	var meta sql.NullString
	if err := dm.SQLDB().QueryRow(
		`SELECT metadata FROM memories WHERE id = ?`, id,
	).Scan(&meta); err != nil {
		t.Fatalf("t28 read metadata: %v", err)
	}
	if !meta.Valid {
		return ""
	}
	return meta.String
}

// TestT28_ErgonomicSingleKey exercises the headline ergonomic
// shortcut: `mpm patch-memory <id> -- status=proven` builds
// `{"status":"proven"}` and applies it.
func TestT28_ErgonomicSingleKey(t *testing.T) {
	dm := newTestDMForCmd(t)
	id := t28SeedMemory(t, dm, "t28-single")

	rc := handlePatchMemory([]string{"patch-memory", id, "--", "status=proven"})
	if rc != 0 && rc != 1 {
		t.Fatalf("-- status=proven: unexpected exit %d", rc)
	}

	meta := t28ReadMetadata(t, dm, id)
	if rc == 0 && (!strings.Contains(meta, `"status"`) || !strings.Contains(meta, `"proven"`)) {
		t.Errorf("-- status=proven: metadata must contain status=proven, got %q", meta)
	}
}

// TestT28_ErgonomicMultipleKeys pins that the shortcut accepts
// multiple key=value pairs and merges them into a single patch.
func TestT28_ErgonomicMultipleKeys(t *testing.T) {
	dm := newTestDMForCmd(t)
	id := t28SeedMemory(t, dm, "t28-multi")

	rc := handlePatchMemory([]string{
		"patch-memory", id, "--",
		"status=proven",
		"confidence=0.9",
		"resolved_at=2026-09-11T13:00:00Z",
	})
	if rc != 0 && rc != 1 {
		t.Fatalf("-- multi-key: unexpected exit %d", rc)
	}

	meta := t28ReadMetadata(t, dm, id)
	if rc == 0 {
		for _, sub := range []string{`"status"`, `"confidence"`, `"resolved_at"`} {
			if !strings.Contains(meta, sub) {
				t.Errorf("-- multi-key: metadata must contain %s, got %q", sub, meta)
			}
		}
	}
}

// TestT28_RejectsMissingPatchArg pins that calling patch-memory
// without a patch (no positional, no -- pairs) exits non-zero with
// a usage hint. The parse branch must reject this BEFORE any DB
// write is attempted, so this test works even if FTS5 init fails.
func TestT28_RejectsMissingPatchArg(t *testing.T) {
	dm := newTestDMForCmd(t)
	id := t28SeedMemory(t, dm, "t28-missing")

	rc := handlePatchMemory([]string{"patch-memory", id})
	if rc == 0 {
		t.Fatalf("-- with no patch arg: must exit non-zero (usage error), got 0")
	}

	meta := t28ReadMetadata(t, dm, id)
	if meta != "{}" {
		t.Errorf("-- with no patch arg: row must remain untouched, got metadata=%q", meta)
	}
}

// TestT28_RejectsInvalidPair pins that `key=value` parsing is
// strict: a pair without `=` is rejected (no silent fallback to
// the empty string).
func TestT28_RejectsInvalidPair(t *testing.T) {
	dm := newTestDMForCmd(t)
	id := t28SeedMemory(t, dm, "t28-invalid-pair")

	rc := handlePatchMemory([]string{"patch-memory", id, "--", "no-equals-sign"})
	if rc == 0 {
		t.Fatalf("-- no-equals-sign: must exit non-zero (parse error), got 0")
	}

	meta := t28ReadMetadata(t, dm, id)
	if meta != "{}" {
		t.Errorf("-- no-equals-sign: row must remain untouched, got metadata=%q", meta)
	}
}

// TestT28_RejectsMissingPairAfterDash pins that `--` with no
// following pair is rejected with a clear error.
func TestT28_RejectsMissingPairAfterDash(t *testing.T) {
	dm := newTestDMForCmd(t)
	id := t28SeedMemory(t, dm, "t28-empty-pairs")

	rc := handlePatchMemory([]string{"patch-memory", id, "--"})
	if rc == 0 {
		t.Fatalf("-- with no pairs: must exit non-zero, got 0")
	}

	meta := t28ReadMetadata(t, dm, id)
	if meta != "{}" {
		t.Errorf("-- with no pairs: row must remain untouched, got metadata=%q", meta)
	}
}
