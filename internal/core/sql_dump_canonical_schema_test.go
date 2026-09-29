package internal

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// TestCanonicalSchemaSync is the static guard that keeps
// CanonicalMPMSchema in sync with the CREATE TABLE statements in
// internal/core/db.go (and other internal/core/*.go files that ship
// schema DDL).
//
// Pattern: every `CREATE TABLE IF NOT EXISTS <name>` or `CREATE TABLE <name>`
// in the schema source files must have its <name> listed in
// CanonicalMPMSchema. Tables in RuntimeCanonicalSchema are NOT checked
// against source (they're runtime-created, not declared in Go).
func TestCanonicalSchemaSync(t *testing.T) {
	schemaTables, err := scanSchemaSources(t)
	if err != nil {
		t.Fatalf("scan schema sources: %v", err)
	}
	if len(schemaTables) == 0 {
		t.Fatalf("scanned zero tables from schema sources — guard pattern broken?")
	}

	canonical := make(map[string]bool, len(CanonicalMPMSchema))
	for _, name := range CanonicalMPMSchema {
		canonical[name] = true
	}

	var missingFromCanonical []string
	for name := range schemaTables {
		if !canonical[name] {
			missingFromCanonical = append(missingFromCanonical, name)
		}
	}
	sort.Strings(missingFromCanonical)

	var extraInCanonical []string
	for _, name := range CanonicalMPMSchema {
		if !schemaTables[name] {
			extraInCanonical = append(extraInCanonical, name)
		}
	}
	sort.Strings(extraInCanonical)

	if len(missingFromCanonical) > 0 {
		t.Errorf("tables present in internal/core schema but missing from CanonicalMPMSchema:\n  %s\n"+
			"Add them to CanonicalMPMSchema in internal/core/sql_dump_validator.go to keep the restore-db allow-list in sync.",
			strings.Join(missingFromCanonical, "\n  "))
	}
	if len(extraInCanonical) > 0 {
		t.Errorf("tables in CanonicalMPMSchema but not in internal/core schema:\n  %s\n"+
			"These may belong in RuntimeCanonicalSchema (runtime-created) or be stale entries to remove.",
			strings.Join(extraInCanonical, "\n  "))
	}
}

// TestRuntimeCanonicalSchemaHasNoPhantomEntries is the static guard for
// the half of the restore-db allow-list that TestCanonicalSchemaSync
// cannot reach.
//
// TestCanonicalSchemaSync cross-checks CanonicalMPMSchema against the
// CREATE TABLE statements in source. RuntimeCanonicalSchema is
// explicitly excluded from that check ("they're runtime-created, not
// declared in Go"), which is correct for genuinely runtime-created
// tables but leaves the list entirely unguarded. Two entries had
// accumulated there with no table behind them at all:
//
//   - synthesis_dlq — claimed to be "created by synthesis isolation
//     runtime". No CREATE TABLE for it exists anywhere in the tree, the
//     `mpm dlq` command that read it is gone, and no current database
//     contains it. It is the residue of a removed feature.
//   - reference_docs_fts — the FTS virtual table was renamed to
//     references_fts (it indexes the reference_docs base table; the
//     triggers are references_ai/ad/au). The old name survived in this
//     list and in docs/CONTRIBUTING.md.
//
// An allow-list entry with no table behind it is not inert. This
// validator is the security boundary for `mpm restore-db`: it is the
// thing that rejects a dump declaring arbitrary tables. An entry for a
// table MPM cannot create is exactly the shape of an allow-list hole —
// a hostile dump can declare it and be accepted. The list is therefore
// a tightening surface, not a convenience list, and a phantom entry is
// a defect rather than a harmless leftover.
//
// The check is deliberately NOT a plain substring search over the tree.
// `synthesis_dlq` appears in this package — in a comment inside
// sql_dump_validator.go itself, describing the very list it is
// checking. A scan that counted the validator's own text would
// validate the list against itself and pass forever. This guard
// therefore excludes sql_dump_validator.go from the evidence it
// accepts.
//
// When this test fails: either restore the table's CREATE statement to
// source, or delete the entry. Do not add a justification comment
// instead — a comment is not a table, and the comment is precisely the
// mechanism that let this drift survive.
func TestRuntimeCanonicalSchemaHasNoPhantomEntries(t *testing.T) {
	// Non-test sources in this package, excluding the allow-list file.
	const self = "sql_dump_validator.go"
	var srcFiles []string
	paths, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob package sources: %v", err)
	}
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") || p == self {
			continue
		}
		srcFiles = append(srcFiles, p)
	}
	if len(srcFiles) == 0 {
		t.Fatalf("found zero non-test source files in %s; the guard's own scope is broken", mustGetwd(t))
	}

	// Index every identifier-looking token that appears in real source.
	inSource := make(map[string]bool)
	// Real FTS virtual tables, from their CREATE VIRTUAL TABLE statements.
	//
	// Matched only inside Go raw string literals whose content STARTS
	// with CREATE VIRTUAL TABLE, mirroring bodyTablePattern's treatment
	// of CREATE TABLE in scanSchemaSources below. A looser whole-file
	// match would accept a name mentioned in a comment — and a comment
	// is exactly how synthesis_dlq stayed in this list, so accepting
	// comment evidence would preserve the defect.
	inFTSDDL := make(map[string]bool)
	rawStringRe := regexp.MustCompile("`([^`]+)`")
	ftsBodyRe := regexp.MustCompile(`(?i)^\s*CREATE\s+VIRTUAL\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([A-Za-z_][A-Za-z0-9_]*)`)
	tokRe := regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*`)

	for _, p := range srcFiles {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		for _, tok := range tokRe.FindAllString(string(b), -1) {
			inSource[tok] = true
		}
		for _, m := range rawStringRe.FindAllStringSubmatch(string(b), -1) {
			if fm := ftsBodyRe.FindStringSubmatch(m[1]); fm != nil {
				name := strings.ToLower(fm[1])
				// The optional IF NOT EXISTS clause is not always consumed
				// (schema uses varied casing and spacing), so a SQL keyword
				// can land here. A keyword entry could mask a phantom
				// literally named after it; drop them.
				if sqlKeywords[name] {
					continue
				}
				inFTSDDL[name] = true
			}
		}
	}

	if len(inFTSDDL) == 0 {
		t.Fatalf("found zero CREATE VIRTUAL TABLE statements in %d source files; the FTS probe is "+
			"broken and would pass every FTS entry vacuously", len(srcFiles))
	}

	var phantom []string
	for _, name := range RuntimeCanonicalSchema {
		lower := strings.ToLower(name)
		if strings.HasSuffix(lower, "_fts") {
			// FTS bases must be declared by a real CREATE VIRTUAL TABLE.
			if !inFTSDDL[lower] {
				phantom = append(phantom, fmt.Sprintf(
					"%s: no CREATE VIRTUAL TABLE for it in %d non-test source files (declaring file(s): %s)",
					name, len(srcFiles), sortedStringKeys(inFTSDDL)))
			}
			continue
		}
		// Non-FTS runtime tables need some live source reference.
		if !inSource[name] {
			phantom = append(phantom, fmt.Sprintf(
				"%s: the name appears in no non-test source file other than %s", name, self))
		}
	}
	sort.Strings(phantom)

	if len(phantom) > 0 {
		t.Errorf("RuntimeCanonicalSchema contains %d entr(ies) with no table behind them:\n  - %s\n"+
			"These are holes in the `mpm restore-db` security allow-list: a dump declaring a table MPM "+
			"cannot create would be accepted. Delete each entry, or restore the CREATE statement that "+
			"produces the table. Adding a comment does not count — the comment is how synthesis_dlq "+
			"survived here.", len(phantom), strings.Join(phantom, "\n  - "))
	}
}

// sqlKeywords are fragments the CREATE VIRTUAL TABLE probe can capture
// when the IF NOT EXISTS clause is not consumed verbatim.
var sqlKeywords = map[string]bool{
	"if": true, "not": true, "exists": true, "temp": true, "temporary": true,
}

// sortedStringKeys returns a map's keys in order, for stable failure messages.
func sortedStringKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// mustGetwd is a small helper for guard failure messages.
func mustGetwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		return "<unknown>"
	}
	return wd
}

// scanSchemaSources walks internal/core/*.go and extracts every name
// from a `CREATE TABLE [IF NOT EXISTS] <name>` statement. Only matches
// Go raw string literals whose content STARTS with CREATE TABLE — this
// excludes Go comments, Go source code that happens to contain the
// words "CREATE TABLE" in a string, and SQL keywords that aren't real
// table names. Returns a set of unique table names.
func scanSchemaSources(t *testing.T) (map[string]bool, error) {
	// Pattern A: Go raw string literals whose content STARTS with CREATE TABLE.
	rawStringPattern := regexp.MustCompile("`([^`]+)`")
	// Pattern B: Bare SQL at the start of a line (used in *_schema_patch.go files
	// where CREATE TABLE statements are concatenated into strings at runtime).
	bareTablePattern := regexp.MustCompile(`^\s*CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([A-Za-z_][A-Za-z0-9_.]*)`)
	// Match CREATE TABLE [IF NOT EXISTS] <name> at the START of a backtick-string body.
	bodyTablePattern := regexp.MustCompile(`^\s*CREATE\s+TABLE\s+(?:IF\s+NOT\s+EXISTS\s+)?([A-Za-z_][A-Za-z0-9_.]*)`)
	tables := make(map[string]bool)

	// Migration-target tables that are intentionally transient (created then
	// renamed). These are real CREATE TABLE statements in source but should
	// NOT be in the canonical allow-list because they exist only during a
	// migration window.
	transientTables := map[string]bool{
		"system_audit_log_new":     true, // rename target in migrateAuditLevelConstraint
		"artifact_provenance_work": true, // rename target in migrateArtifactProvenanceWorkType
		"work_events_new":          true, // rename target in migrateWorkEventsCheck (domain-neutral migration)
		"evidence_new":             true, // rename target in migrateEvidenceWorkType
		"new_session_handoffs":     true, // rename target in MigrateSessionHandoffsOptionalSessionID (handoff-identity hardening)
		"scheduled_tasks_new":      true, // rename target in scheduled_tasks_migration (table-recreate pattern)
		"confidence_history__new":  true, // rename target in migration_confidence_history_check_widening (2026-09-05 audit P0 widening)
	}

	matches, err := filepath.Glob("*.go")
	if err != nil {
		return nil, err
	}

	for _, path := range matches {
		// Skip test files — they may define fixture schemas that aren't part of MPM.
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		// Pattern A: backtick-wrapped content starting with CREATE TABLE.
		for _, m := range rawStringPattern.FindAllStringSubmatch(string(content), -1) {
			body := m[1]
			tm := bodyTablePattern.FindStringSubmatch(body)
			if tm == nil {
				continue
			}
			if transientTables[tm[1]] {
				continue
			}
			tables[tm[1]] = true
		}
		// Pattern B: bare CREATE TABLE at line start (schema_patch files).
		for _, line := range strings.Split(string(content), "\n") {
			tm := bareTablePattern.FindStringSubmatch(line)
			if tm == nil {
				continue
			}
			if transientTables[tm[1]] {
				continue
			}
			tables[tm[1]] = true
		}
	}
	return tables, nil
}

// TestPhantomTableDumpIsRejected is the behavioural counterpart to
// TestRuntimeCanonicalSchemaHasNoPhantomEntries: it proves the closed
// hole actually refuses a dump, not merely that the name is gone from a
// list.
//
// The threat this pins: `mpm restore-db` validates a dump against the
// canonical allow-list before it touches the live database, and that
// allow-list is the only thing standing between an arbitrary dump and
// the user's MPM state. While synthesis_dlq and reference_docs_fts sat
// in RuntimeCanonicalSchema, a dump could declare either table and be
// accepted — an attacker would not need a vulnerability, just a name
// that happened to be allow-listed by accident. The static guard above
// keeps the list honest; this test keeps the consequence honest.
func TestPhantomTableDumpIsRejected(t *testing.T) {
	v := NewCanonicalDumpValidator()

	for _, phantom := range []string{
		"synthesis_dlq",      // removed 2026-09-29: residue of the deleted DLQ feature
		"reference_docs_fts", // removed 2026-09-29: pre-rename name for references_fts
	} {
		t.Run(phantom, func(t *testing.T) {
			dump := "CREATE TABLE " + phantom + " (id TEXT PRIMARY KEY, payload TEXT);\n" +
				"INSERT INTO " + phantom + " VALUES('x', 'y');\n"

			err := v.Validate(dump)
			if err == nil {
				t.Fatalf("a dump declaring %q was ACCEPTED. It is not a table MPM can create, so it "+
					"must be rejected as an unknown table — acceptance means the restore-db allow-list "+
					"has a hole a hostile dump can walk through.", phantom)
			}
			if !strings.Contains(strings.ToLower(err.Error()), "unknown table") {
				t.Errorf("dump declaring %q was rejected, but not as an unknown table: %v", phantom, err)
			}
		})
	}
}

// TestAllowListIsLoadBearing is the counterweight to the test above.
//
// Removing entries from the allow-list must make the validator STRICTER.
// If instead removing an entry made validation pass more dumps, the
// allow-list would not be doing any work and every other test in this
// area would be measuring nothing. This pins the direction of the
// effect: drop a table the fixtures depend on, and a previously-valid
// dump must start failing.
func TestAllowListIsLoadBearing(t *testing.T) {
	const fixture = "good_dump.sql"
	dump := loadFixture(t, fixture)

	// Baseline: the canonical allow-list accepts it.
	if err := NewCanonicalDumpValidator().Validate(dump); err != nil {
		t.Fatalf("baseline: canonical validator rejected %s: %v", fixture, err)
	}

	// Now build an allow-list that omits `memories` — the one table the
	// fixture declares — and require the same dump to be rejected.
	var trimmed []string
	for _, name := range CanonicalMPMSchema {
		if name != "memories" {
			trimmed = append(trimmed, name)
		}
	}
	trimmed = append(trimmed, RuntimeCanonicalSchema...)
	if len(trimmed) >= len(CanonicalMPMSchema)+len(RuntimeCanonicalSchema) {
		t.Fatalf("test setup is wrong: trimming 'memories' removed nothing")
	}

	err := NewDumpValidator(trimmed).Validate(dump)
	if err == nil {
		t.Fatalf("%s was still accepted after `memories` was removed from the allow-list. The "+
			"allow-list is not load-bearing, so the acceptance tests prove nothing.", fixture)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "unknown table") {
		t.Errorf("expected an unknown-table rejection, got: %v", err)
	}
}
