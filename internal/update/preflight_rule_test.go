package update

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------
// The schema-preflight statement rule
// ---------------------------------------------------------------------
//
// The schema-collision preflight COMPILES the candidate's reference DDL
// against an in-memory database (internal/update/preflight.go,
// checkTableShape). So the file is EXECUTED, and what it contains is a
// security boundary: a file that merely CONTAINS a
// "CREATE TABLE lifecycle_events (...)" can also carry a DROP, a
// DELETE, or an ATTACH that writes a file outside the database.
//
// These tests pin the rule that decides: the file must be EXACTLY ONE
// supported statement.

// canonicalPreflight returns the substrate's own lifecycle_events DDL,
// extracted by the CANONICAL extractor.
//
// It is the positive control: the exact output of
// scripts/lib/schema_preflight.py must be accepted by both validators,
// byte for byte. A rule that only happens to accept the substrate's
// current DDL is not a rule.
func canonicalPreflight(t *testing.T) string {
	t.Helper()
	repo := repoRoot(t)
	out := filepath.Join(t.TempDir(), "lifecycle_events.sql")
	cmd := exec.Command("python3", filepath.Join(repo, "scripts", "lib", "schema_preflight.py"), //nolint:gosec
		"extract",
		"--go", filepath.Join(repo, "internal", "core", "schema.go"),
		"--table", "lifecycle_events",
		"--out", out)
	if o, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("the canonical extractor failed: %v\n%s", err, o)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// preflightRuleCases is the matrix. Every case that is not the
// canonical output must be REFUSED, and the two validators must agree.
func preflightRuleCases(t *testing.T) []struct {
	name    string
	text    string
	refused bool
} {
	canonical := canonicalPreflight(t)
	return []struct {
		name    string
		text    string
		refused bool
	}{
		// THE POSITIVE CONTROL: the canonical extractor's exact output.
		{"the canonical extractor's output", canonical, false},
		{"a plain CREATE TABLE", "CREATE TABLE lifecycle_events (event_id TEXT PRIMARY KEY NOT NULL);\n", false},
		{"a quoted table name", "CREATE TABLE IF NOT EXISTS \"lifecycle_events\" (event_id TEXT PRIMARY KEY NOT NULL);\n", false},
		{"a semicolon inside a string literal", "CREATE TABLE lifecycle_events (event_id TEXT DEFAULT ';' NOT NULL);\n", false},

		// THE CASES THE RULE EXISTS FOR.
		{"a valid CREATE followed by a DROP", "CREATE TABLE lifecycle_events (event_id TEXT PRIMARY KEY NOT NULL);\nDROP TABLE lifecycle_events;\n", true},
		{"a valid CREATE followed by a DELETE", "CREATE TABLE lifecycle_events (event_id TEXT PRIMARY KEY NOT NULL);\nDELETE FROM memories;\n", true},
		{"a DROP before a valid CREATE", "DROP TABLE lifecycle_events;\nCREATE TABLE lifecycle_events (event_id TEXT PRIMARY KEY NOT NULL);\n", true},
		{"an unterminated CREATE", "CREATE TABLE lifecycle_events (event_id TEXT PRIMARY KEY", true},
		{"a comment containing the expected phrase", "-- 'CREATE TABLE lifecycle_events (x)'\n", true},
		{"unexpected SQL before the declaration", "SELECT 1;\nCREATE TABLE lifecycle_events (event_id TEXT PRIMARY KEY NOT NULL);\n", true},
		{"unexpected SQL after the declaration", "CREATE TABLE lifecycle_events (event_id TEXT PRIMARY KEY NOT NULL);\nSELECT 1;\n", true},
		{"an ATTACH that writes a file outside the database", "ATTACH DATABASE '/tmp/mpm-preflight-side-effect.db' AS side;\nCREATE TABLE side.payload(x);\n", true},
		{"a statement for a different table", "CREATE TABLE memories (id TEXT PRIMARY KEY NOT NULL);\n", true},
		{"unbalanced parentheses", "CREATE TABLE lifecycle_events (event_id TEXT PRIMARY KEY NOT NULL));\n", true},
		{"a destructive statement only", "DROP TABLE lifecycle_events;\n", true},
	}
}

// TestTheSchemaPreflightMustBeExactlyOneStatement pins the rule itself.
func TestTheSchemaPreflightMustBeExactlyOneStatement(t *testing.T) {
	for _, tc := range preflightRuleCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			problems := preflightStatementProblems(tc.text)
			if tc.refused && len(problems) == 0 {
				t.Error("the rule accepted a preflight it must refuse")
			}
			if !tc.refused && len(problems) > 0 {
				t.Errorf("the rule refused a legitimate preflight: %v", problems)
			}
		})
	}
}

// TestTheSchemaPreflightRuleIsTheSameInGoAndInTheShell is the parity
// test.
//
// The bundle that reaches an operator depends on which tool assembled
// it, so the two validators must reach the same verdict on every
// preflight. The matrix includes the canonical extractor's exact
// output, which both must accept.
func TestTheSchemaPreflightRuleIsTheSameInGoAndInTheShell(t *testing.T) {
	lib := shellLibraryPath(t)
	if lib == "" {
		t.Skip("the shell library is not available")
	}
	for _, tc := range preflightRuleCases(t) {
		t.Run(tc.name, func(t *testing.T) {
			dir := testBundleDir(t)
			if err := os.WriteFile(filepath.Join(dir, "schema", "lifecycle_events.sql"), []byte(tc.text), 0644); err != nil {
				t.Fatal(err)
			}
			goValid := ValidateBundle(dir, "").Valid
			shellValid := shellValidateBundle(t, lib, dir)
			if goValid != shellValid {
				t.Errorf("the validators disagree: Go says %v, the shell library says %v", goValid, shellValid)
			}
			if tc.refused {
				if goValid {
					t.Error("the Go validator accepted a preflight it must refuse")
				}
				if shellValid {
					t.Error("the shell validator accepted a preflight it must refuse")
				}
			} else {
				if !goValid {
					t.Error("the Go validator refused the canonical preflight")
				}
				if !shellValid {
					t.Error("the shell validator refused the canonical preflight")
				}
			}
		})
	}
}

// TestThePreflightCheckSaysOnlyWhatItChecked pins the attestation.
//
// The bundle ASSERTS that its preflight is the substrate's DDL. The
// release format carries no independent witness of that: the binary
// embeds the DDL but exposes no way to read it back, and the bundle
// carries no source. So a bundle that ships a DIFFERENT — but
// syntactically valid — CREATE TABLE for lifecycle_events passes every
// check.
//
// A validator whose success message claims an authenticity it does not
// have is worse than one that says less: the operator reads the
// message as a guarantee. So the message names what was checked, and
// says plainly that the provenance is asserted.
func TestThePreflightCheckSaysOnlyWhatItChecked(t *testing.T) {
	dir := testBundleDir(t)
	rep := ValidateBundle(dir, "")
	if !rep.Valid {
		t.Fatalf("the bundle does not validate: %v", rep.ValidationError())
	}
	var detail string
	for _, c := range rep.Checks {
		if c.Name == "schema-preflight" {
			detail = c.Detail
		}
	}
	if detail == "" {
		t.Fatal("the report carries no schema-preflight check")
	}
	if strings.Contains(detail, "the substrate's lifecycle_events DDL") {
		t.Errorf("the check still claims the file IS the substrate's DDL: %q", detail)
	}
	if !strings.Contains(detail, "cannot prove") {
		t.Errorf("the check does not say that the provenance is asserted rather than proven: %q", detail)
	}
}
