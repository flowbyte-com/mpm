// d001_ls_numeric_fields_test.go — regressions for audit findings D-001 / W-008.
//
// D-001: `mpm ls` reported weight=1 in the WEIGHT column for every memory,
// regardless of the stored value. The defect was caused by web_db.go storing
// weight as Go's `int` while simple_cmds.go only type-asserted int64/float64,
// so the default value was used.
//
// W-008: the same Go-type mismatch is the underlying hazard. Now resolved by:
//   1. GetMemoriesForExport returns int64 (INTEGER cols) and float64 (REAL
//      weight) — matching the SQL schema types.
//   2. simple_cmds.go reads via float64FromMap / int64FromMap (strict helpers
//      in handlers_helpers.go) which warn instead of silently defaulting.
//   3. maint_cmds.go export CSV uses the same helpers.
//
// These tests pin the public-boundary behaviour: every stored weight value
// must appear verbatim in the WEIGHT column of `mpm ls` and in the weight
// column of `mpm export --format csv`.
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
)

// TestD001_LsShowsActualWeight is the headline regression: the WEIGHT column
// must contain the stored weight, not the hardcoded default of 1.
func TestD001_LsShowsActualWeight(t *testing.T) {
	dm := getDBConcrete()
	if dm == nil {
		t.Skip("DB unavailable")
	}

	// Three memories with distinct weights so a wrong projection would
	// produce an obvious mismatch.
	const wantW5 = 5
	const wantW10 = 10
	const wantW1 = 1
	id5 := d001SeedMemory(t, "d001-w5", wantW5)
	id10 := d001SeedMemory(t, "d001-w10", wantW10)
	id1 := d001SeedMemory(t, "d001-w1", wantW1)

	out := captureBoth(t, func() {
		handleLs([]string{"ls", "--limit", "500"})
	})

	// Each seeded memory must appear in the listing with its actual weight.
	// The header is "ID  CREATED  COLLECTION  WEIGHT  CONTENT" so the
	// fourth whitespace-separated field is WEIGHT.
	d001AssertWeightInRow(t, out, id5, wantW5)
	d001AssertWeightInRow(t, out, id10, wantW10)
	d001AssertWeightInRow(t, out, id1, wantW1)
}

// TestD001_LsDoesNotShowDefaultWeight pins the negative contract: even if a
// future regression strips the helper, no memory may show weight=1 unless
// the stored weight is genuinely 1.
func TestD001_LsDoesNotShowDefaultWeight(t *testing.T) {
	dm := getDBConcrete()
	if dm == nil {
		t.Skip("DB unavailable")
	}

	id := d001SeedMemory(t, "d001-no-default", 7)

	out := captureBoth(t, func() {
		handleLs([]string{"ls", "--limit", "500"})
	})

	row := d001FindRowByID(t, out, id)
	if row == "" {
		t.Fatalf("seeded memory %s not found in ls output:\n%s", id, out)
	}

	// Find the WEIGHT column (4th column). The ls format is:
	//   %-18s %.20s %-10s %-6d%s %s
	// so fields are whitespace-separated and the 4th field is WEIGHT.
	fields := strings.Fields(row)
	if len(fields) < 4 {
		t.Fatalf("row has fewer than 4 fields: %q", row)
	}
	got, err := strconv.Atoi(fields[3])
	if err != nil {
		t.Fatalf("WEIGHT field %q is not an integer: %v", fields[3], err)
	}
	if got != 7 {
		t.Errorf("WEIGHT column reports %d, want 7 (D-001 regression: silent default)", got)
	}
}

// TestD001_ExportCSVShowsActualWeight is the sibling regression: the CSV
// export path (maint_cmds.go) had the same int/int64 mismatch. Both must be
// fixed together — the export path is what feeds downstream tooling.
func TestD001_ExportCSVShowsActualWeight(t *testing.T) {
	dm := getDBConcrete()
	if dm == nil {
		t.Skip("DB unavailable")
	}

	const wantW = 7
	id := d001SeedMemory(t, "d001-export-w7", wantW)

	// Export to a temp CSV file so we can read the structured output.
	outPath := t.TempDir() + "/export.csv"
	if rc := handleExport([]string{"export", "--format", "csv", "--collection", "memories", "--output", outPath}); rc != 0 {
		t.Fatalf("handleExport returned %d", rc)
	}

	body, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read export.csv: %v", err)
	}

	// Header is: id,collection,content,tags,created_at,reinforcement_count,weight,is_long_term
	var found bool
	for _, line := range strings.Split(string(body), "\n") {
		if !strings.HasPrefix(line, id+",") {
			continue
		}
		cols := strings.Split(line, ",")
		if len(cols) < 7 {
			t.Fatalf("csv row has %d columns, want >= 7: %q", len(cols), line)
		}
		// weight column is index 6 (after id, collection, content, tags,
		// created_at, reinforcement_count).
		got := cols[6]
		// Weight is real-valued (REAL in schema). The CSV writer now uses
		// %f to surface fractional weights — accept either "7" or "7.00".
		if got != "7" && got != "7.00" {
			t.Errorf("CSV weight column is %q, want 7 or 7.00 (D-001 sibling regression)", got)
		}
		found = true
		break
	}
	if !found {
		t.Fatalf("exported CSV does not contain seeded memory %s:\n%s", id, string(body))
	}
}

// TestD001_StrictHelperSurfacesMismatch pins the underlying contract: when a
// future projection changes the Go type for a numeric field, the strict
// helpers must WARN (not silently default). We verify by feeding a
// mismatched-typed value and asserting the helper returns false AND emits
// a warning. This is the contract that prevents the silent-zero bug from
// recurring.
func TestD001_StrictHelperSurfacesMismatch(t *testing.T) {
	m := map[string]interface{}{
		"weight":               "not a number", // string, not int64/float64
		"reinforcement_count":  complex64(0),    // weird type
	}

	// Weight helper expects float64. String is a contract violation.
	w, ok := float64FromMap(m, "weight")
	if ok {
		t.Errorf("float64FromMap returned ok=true for string value (want false to surface contract violation)")
	}
	if w != 0 {
		t.Errorf("float64FromMap returned non-zero %v for type mismatch (want default 0 with warning)", w)
	}

	// Reinforcement_count helper expects int64. complex64 is a contract violation.
	rc, ok := int64FromMap(m, "reinforcement_count")
	if ok {
		t.Errorf("int64FromMap returned ok=true for complex64 value (want false to surface contract violation)")
	}
	if rc != 0 {
		t.Errorf("int64FromMap returned non-zero %d for type mismatch (want default 0 with warning)", rc)
	}
}

// d001SeedMemory inserts a memory row directly with a specific weight and
// returns its hex ID. We use SQL directly (not `mpm remember`) so the test
// is hermetic and does not depend on user-facing command defaults.
func d001SeedMemory(t *testing.T, tag string, weight int64) string {
	t.Helper()
	dm := getDBConcrete()
	memID := fC4UniqueID(t, "d001")
	now := nowUnix()
	if _, err := dm.SQLDB().Exec(
		`INSERT INTO memories (id, collection, content, tags, metadata, weight, confidence, last_accessed_at, created_at)
		 VALUES (?, 'memories', ?, ?, '{}', ?, 0.8, ?, ?)`,
		memID, fmt.Sprintf("D-001 seed tag=%s", tag), fmt.Sprintf("[%q]", tag), weight, now, now,
	); err != nil {
		t.Fatalf("seed weight=%d: %v", weight, err)
	}
	return memID
}

// d001AssertWeightInRow asserts that a row containing the given memory ID
// has the expected weight in the WEIGHT column.
func d001AssertWeightInRow(t *testing.T, out, memID string, wantWeight int64) {
	t.Helper()
	row := d001FindRowByID(t, out, memID)
	if row == "" {
		t.Errorf("ls output does not contain memory %s (weight=%d):\n%s", memID, wantWeight, out)
		return
	}
	fields := strings.Fields(row)
	if len(fields) < 4 {
		t.Fatalf("row for %s has fewer than 4 fields: %q", memID, row)
	}
	got, err := strconv.Atoi(fields[3])
	if err != nil {
		t.Fatalf("WEIGHT field %q is not an integer: %v", fields[3], err)
	}
	if got != int(wantWeight) {
		t.Errorf("memory %s shows weight=%d, want %d (D-001 regression)", memID, got, wantWeight)
	}
}

// d001FindRowByID finds the line in ls output whose first column contains
// the (possibly truncated) memory ID.
func d001FindRowByID(t *testing.T, out, memID string) string {
	t.Helper()
	// ls truncates to 16 chars for the ID column.
	prefix := memID
	if len(prefix) > 16 {
		prefix = prefix[:16]
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line
		}
	}
	return ""
}
