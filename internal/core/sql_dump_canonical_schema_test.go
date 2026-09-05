package internal

import (
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
		"system_audit_log_new":       true,  // rename target in migrateAuditLevelConstraint
		"artifact_provenance_work":   true,  // rename target in migrateArtifactProvenanceWorkType
		"work_events_new":            true,  // rename target in migrateWorkEventsCheck (domain-neutral migration)
		"evidence_new":                true,  // rename target in migrateEvidenceWorkType
		"new_session_handoffs":       true,  // rename target in MigrateSessionHandoffsOptionalSessionID (handoff-identity hardening)
		"scheduled_tasks_new":        true,  // rename target in scheduled_tasks_migration (table-recreate pattern)
		"confidence_history__new":    true,  // rename target in migration_confidence_history_check_widening (2026-09-05 audit P0 widening)
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