package internal

import (
	"database/sql"
	"fmt"
)

// newLiveSchemaDumpValidator opens the given SQLite database in read-only
// mode and builds a validator whose allow-list is derived from the
// existing tables in the DB. FTS virtual tables and sqlite_* internal
// tables are excluded.
//
// Most callers should use NewCanonicalDumpValidator instead — this
// function is for the special case where you want the allow-list
// restricted to the live target schema (e.g., a restore-into-existing-DB
// check that should reject any dump referencing tables that aren't in
// the target).
//
// Requires the sqlopen_owner_test.go whitelist entry for
// `live_schema_validator.go`.
func newLiveSchemaDumpValidator(dbPath string) (*DumpValidator, error) {
	db, err := sql.Open("sqlite3", dbPath+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open db read-only: %w", err)
	}
	defer db.Close()

	rows, err := db.Query(`
		SELECT name FROM sqlite_master
		WHERE type IN ('table', 'view')
		  AND name NOT LIKE 'sqlite_%'
		  AND name NOT LIKE '%_fts%'
		ORDER BY name
	`)
	if err != nil {
		return nil, fmt.Errorf("query schema: %w", err)
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		tables = append(tables, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return NewDumpValidator(tables), nil
}