package internal

import (
	"fmt"
)

type SearchOptions struct {
	Query string
	Table string // Validated against WipeTableNames
	Limit int
	ID    string
}

func (dm *DatabaseManager) SearchWithSnippet(opts SearchOptions) ([]map[string]interface{}, error) {
	if WipeTableNames[opts.Table] == "" {
		return nil, fmt.Errorf("invalid table: %s", opts.Table)
	}

	// Matches the schema defined in db.go (e.g., sessions_fts)
	ftsTable := fmt.Sprintf("%s_fts", opts.Table)
	query := fmt.Sprintf(`
	SELECT rowid, snippet(%s, 0, '[', ']', '...', 10) as snippet, session_id 
	FROM %s 
	WHERE %s MATCH ? 
	ORDER BY rank 
	LIMIT ?`, ftsTable, ftsTable, ftsTable)

	rows, err := dm.db.Query(query, opts.Query, opts.Limit)
	if err != nil {
		return nil, fmt.Errorf("FTS5 search failed: %w", err)
	}
	defer rows.Close()

	var results []map[string]interface{}
	for rows.Next() {
		var rowid int64
		var snippet, sessionID string
		if err := rows.Scan(&rowid, &snippet, &sessionID); err != nil {
			continue
		}
		results = append(results, map[string]interface{}{
			"rowid":     rowid,
			"snippet":   snippet,
			"session_id": sessionID,
		})
	}
	return results, nil
}

// Shred now correctly verifies the deletion on the primary table.
func (dm *DatabaseManager) Shred(opts SearchOptions) error {
	if WipeTableNames[opts.Table] == "" {
		return fmt.Errorf("invalid tier for shredding: %s", opts.Table)
	}

	// 1. Execute the hard delete + vacuum from db.go
	if err := dm.WipeRecord(opts.Table, opts.ID); err != nil {
		return fmt.Errorf("shred operation failed: %w", err)
	}

	// 2. Corrected Verification: Check the actual content table
	var count int
	verifyQuery := fmt.Sprintf("SELECT COUNT(*) FROM %s WHERE id = ?", opts.Table)
	err := dm.db.QueryRow(verifyQuery, opts.ID).Scan(&count)

	if err != nil {
		return fmt.Errorf("shred verification failed: %w", err)
	}
	if count > 0 {
		return fmt.Errorf("security breach: record %s still exists in %s after shred", opts.ID, opts.Table)
	}

	return nil
}
