// skill_package_export_loss_cli_test.go — the export loss marker at the
// CLI boundary, and the JSON export compatibility policy.
//
// Design of record: PR #222 §6.3 (R11) and §13 T44.
//
// ─────────────────────────────────────────────────────────────────────────────
// WHAT P7 CHANGED HERE
// ─────────────────────────────────────────────────────────────────────────────
//
// Before P7 the marker named every package the export did NOT carry —
// because the export carried none. The marker was the export's own
// admission that the skills export was a LOSS path.
//
// P7 makes the export COMPLETE: the archives are written beside the
// document, and the marker names only what could NOT be carried. So the
// marker's meaning inverted, and the tests that pinned the old meaning
// moved to the new file, `skill_package_export_cli_test.go`.
//
// What is left here is the part of the contract that did NOT change:
//
//   - the marker is ALWAYS present, with a stable shape;
//   - the DEPRECATED bare-array format is available only when the
//     export has nothing to lose;
//   - the structured document's key layout stays what `mpm migrate
//     --from` reads;
//   - a mistyped format is an error, not a silent empty export.
//
// The tests run against the package's disposable test database (the
// package's TestMain pins HOME to the temporary root), never against the
// operator's installation.
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

// TestP4_CLITheLegacyArrayFormatWorksWhenNothingIsOmitted pins the
// usable half of the compatibility policy.
//
// An export of a collection that cannot lose a package — anything but
// "skills" — has nothing to omit, so the deprecated bare array is
// available and produces the legacy shape.
func TestP4_CLITheLegacyArrayFormatWorksWhenNothingIsOmitted(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "export.json")
	var rc int
	out := captureUserError(t, func() int {
		rc = handleExport([]string{"export", "--format", "json-array", "--collection", "directives", "--output", outPath})
		return rc
	})
	if rc != 0 {
		t.Fatalf("the legacy array format was refused for a collection that omits nothing: %s", out)
	}

	body, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read the export: %v", err)
	}
	// The legacy shape is a BARE ARRAY.
	var rows []map[string]interface{}
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatalf("the legacy export is not a bare JSON array: %v\n%s", err, body)
	}
	t.Logf("the legacy export carried %d row(s)", len(rows))

	// And the deprecation is announced, on standard error, so the
	// document on standard output stays parseable.
	if !strings.Contains(out, "DEPRECATED") {
		t.Errorf("the legacy format did not announce its deprecation:\n%s", out)
	}
}

// TestP4_CLIAnUnknownExportFormatIsRefused pins that a mistyped format
// is an error rather than a silent empty export.
func TestP4_CLIAnUnknownExportFormatIsRefused(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "export.json")
	var rc int
	out := captureUserError(t, func() int {
		rc = handleExport([]string{"export", "--format", "jsonl", "--output", outPath})
		return rc
	})
	if rc == 0 {
		t.Fatalf("an unknown export format was accepted")
	}
	if !strings.Contains(out, "jsonl") {
		t.Errorf("the refusal does not name the bad format:\n%s", out)
	}
}

// TestP7_CLITheStructuredExportStaysConsumableByMigrate pins the
// coupling between the export writer and the migrate parser.
//
// The export's top-level key is "memories", and `mpm migrate --from`
// reads a "memories" array. That coupling is REAL and it is what keeps
// an export file a valid input for the migrate command — but it was
// incidental, and nothing pinned it.
//
// This test pins it for the export that carries NO package. The
// package-bearing case is a REFUSAL, and it is pinned in
// `skill_package_export_cli_test.go`: the migration consumer reads only
// the projected rows, so importing a package-bearing document would
// silently drop every package's content.
func TestP7_CLITheStructuredExportStaysConsumableByMigrate(t *testing.T) {
	dm := getDBConcrete()
	if dm == nil {
		t.Skip("DB unavailable")
	}

	outPath := filepath.Join(t.TempDir(), "export.json")
	if rc := handleExport([]string{"export", "--format", "json", "--collection", "directives", "--output", outPath}); rc != 0 {
		t.Fatalf("handleExport returned %d", rc)
	}
	body, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read the export: %v", err)
	}

	// The document is an OBJECT, and the object carries the keys a
	// consumer reads.
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(body, &probe); err != nil {
		t.Fatalf("the export is not a JSON object: %v\n%s", err, body)
	}
	if _, ok := probe["memories"]; !ok {
		t.Fatalf("the export document has no \"memories\" key: %s", body)
	}
	if _, ok := probe["packages_omitted"]; !ok {
		t.Fatalf("the export document has no \"packages_omitted\" key: %s", body)
	}
	if _, ok := probe["package_archives"]; !ok {
		t.Fatalf("the export document has no \"package_archives\" key: %s", body)
	}

	// And the migrate parser accepts it.
	facts, err := mpminternal.ParseJsonFacts(string(body), outPath)
	if err != nil {
		t.Fatalf("the export document is NOT consumable by mpm migrate --from: %v", err)
	}
	t.Logf("mpm migrate --from reads %d facts from the export", len(facts))
}

// TestP7_CLIAnEmptyExportIsStillConsumableByMigrate pins the EMPTY
// case of the same coupling.
//
// A nil slice encodes as `null`, and `null` is not an array — so an
// export of a collection with no rows wrote a document whose
// `memories` key the migration's parser refused. The export's own
// claim that its document stays a valid `mpm migrate --from` input
// was therefore false for every empty collection.
//
// The empty collection is produced by a filter, not by a fixture: the
// export's own filters decide what is in scope, and a collection that
// holds no rows is the case in question.
func TestP7_CLIAnEmptyExportIsStillConsumableByMigrate(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "export.json")
	if rc := handleExport([]string{
		"export", "--format", "json", "--collection", "p7-no-such-collection",
		"--output", outPath,
	}); rc != 0 {
		t.Fatalf("handleExport returned %d", rc)
	}
	body, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read the export: %v", err)
	}

	// The document's `memories` is an ARRAY, and it is EMPTY.
	var probe struct {
		Memories []map[string]interface{} `json:"memories"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		t.Fatalf("the export is not a JSON object with a memories array: %v\n%s", err, body)
	}
	if probe.Memories == nil {
		t.Fatalf("the export wrote a null memories array; null is not an array\n%s", body)
	}
	if len(probe.Memories) != 0 {
		t.Fatalf("the export carried %d row(s); the filter selects none", len(probe.Memories))
	}

	// And the migration accepts it.
	source := filepath.Join(t.TempDir(), "import.json")
	if err := os.WriteFile(source, body, 0o600); err != nil {
		t.Fatalf("write the source: %v", err)
	}
	if rc := handleMigrate([]string{"migrate", "--from", source}); rc != 0 {
		t.Fatalf("the migration refused an empty export document")
	}
}

// TestP7_CLITheEmptyLegacyArrayIsAnArray pins the same thing for the
// DEPRECATED bare-array shape.
//
// The legacy shape is a bare JSON array, and a nil slice encodes as
// `null` — which is not an array at all.
func TestP7_CLITheEmptyLegacyArrayIsAnArray(t *testing.T) {
	outPath := filepath.Join(t.TempDir(), "export.json")
	var rc int
	out := captureUserError(t, func() int {
		rc = handleExport([]string{
			"export", "--format", "json-array", "--collection", "p7-no-such-collection",
			"--output", outPath,
		})
		return rc
	})
	if rc != 0 {
		t.Fatalf("the legacy array format was refused for an empty collection: %s", out)
	}
	body, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read the export: %v", err)
	}
	var rows []map[string]interface{}
	if err := json.Unmarshal(body, &rows); err != nil {
		t.Fatalf("the legacy export is not a bare JSON array: %v\n%s", err, body)
	}
	if rows == nil {
		t.Fatalf("the legacy export wrote null; null is not an array\n%s", body)
	}
}
