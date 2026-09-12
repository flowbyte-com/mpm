package internal

import "testing"

// TestGetMemoryStats_LTMUsesCanonicalDefinition pins the 2026-09-12 CLI
// acceptance fix: GetMemoryStats["ltm"] must use the canonical
// IsLTMMemory definition (flag OR weight>=10), matching `mpm status`.
// Pre-fix it counted only is_long_term=1 and disagreed with status on
// every high-weight non-promoted row.
func TestGetMemoryStats_LTMUsesCanonicalDefinition(t *testing.T) {
	dm := NewTestDM(t)

	mustExec := func(q string, args ...interface{}) {
		t.Helper()
		if _, err := dm.db.Exec(q, args...); err != nil {
			t.Fatalf("exec %q: %v", q, err)
		}
	}
	// Flag-only LTM (low weight), weight-only LTM (no flag), plain row.
	mustExec(`INSERT INTO memories (id, collection, content, created_at, is_long_term, weight) VALUES ('ltm-flag', 'memories', 'flag ltm', 1, 1, 1)`)
	mustExec(`INSERT INTO memories (id, collection, content, created_at, is_long_term, weight) VALUES ('ltm-weight', 'memories', 'weight ltm', 1, 0, 10)`)
	mustExec(`INSERT INTO memories (id, collection, content, created_at, is_long_term, weight) VALUES ('plain', 'memories', 'plain', 1, 0, 5)`)

	stats, err := dm.GetMemoryStats()
	if err != nil {
		t.Fatalf("GetMemoryStats: %v", err)
	}
	got, ok := stats["ltm"]
	if !ok {
		t.Fatalf("GetMemoryStats missing ltm key (keys: %v)", stats)
	}
	var ltm int
	switch v := got.(type) {
	case int:
		ltm = v
	case int64:
		ltm = int(v)
	default:
		t.Fatalf("ltm has unexpected type %T (%v)", got, got)
	}
	if ltm != 2 {
		t.Errorf("ltm = %d, want 2 (flag-only + weight-only rows)", ltm)
	}
}
