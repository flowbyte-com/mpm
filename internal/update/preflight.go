package update

import (
	"database/sql"
	"fmt"
	"os"

	_ "github.com/mattn/go-sqlite3"
)

// snapshotDB copies a database file to a temp scratch file through the
// online backup path. The live file is opened read-only; the copy is
// what the preflight inspects.
func snapshotDB(live string) (string, error) {
	tmp, err := os.CreateTemp("", "mpm-preflight.*.db")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	tmp.Close()
	os.Remove(tmpName)
	// VACUUM INTO runs on the SOURCE connection and writes only the
	// destination: the live file is opened read-only and never
	// modified. (Verified empirically: a mode=ro source vacuums into
	// a new file without touching the source, WAL included.)
	src, err := openSQLiteRO(live)
	if err != nil {
		return "", err
	}
	defer src.Close()
	src.SetMaxOpenConns(1)
	if _, err := src.Exec(fmt.Sprintf("VACUUM INTO '%s'", escapeSingleQuote(tmpName))); err != nil {
		os.Remove(tmpName)
		return "", fmt.Errorf("snapshot failed: %w", err)
	}
	return tmpName, nil
}

// checkTableShape compares the live table's shape against the
// candidate's reference DDL. Absent table: fine (the substrate creates
// it). Matching shape: fine. Anything else is a COLLISION and the
// update is refused before any new binary can reach the live database.
func checkTableShape(snapPath, table, ddl string) error {
	refDB, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		return err
	}
	defer refDB.Close()
	refDB.SetMaxOpenConns(1)
	if _, err := refDB.Exec(ddl); err != nil {
		return fmt.Errorf("the candidate's reference DDL does not apply: %w", err)
	}
	refShape, err := tableShapeOf(refDB, table)
	if err != nil {
		return err
	}
	if refShape == "" {
		return fmt.Errorf("the candidate's reference DDL does not create %s", table)
	}
	liveDB, err := openSQLiteRO(snapPath)
	if err != nil {
		return err
	}
	defer liveDB.Close()
	liveDB.SetMaxOpenConns(1)
	liveShape, err := tableShapeOf(liveDB, table)
	if err != nil {
		return err
	}
	if liveShape == "" {
		return nil // absent: the substrate will create it
	}
	if liveShape != refShape {
		return fmt.Errorf("schema collision on table %q: the live table's shape is not the candidate's shape; rename or migrate the unrelated table before updating", table)
	}
	return nil
}

func tableShapeOf(db *sql.DB, table string) (string, error) {
	var name string
	err := db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name=?", table).Scan(&name)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	rows, err := db.Query("SELECT sql FROM sqlite_master WHERE type='table' AND name=?", table)
	if err != nil {
		return "", err
	}
	var ddl string
	for rows.Next() {
		var s sql.NullString
		if err := rows.Scan(&s); err != nil {
			rows.Close()
			return "", err
		}
		ddl += s.String
	}
	rows.Close()
	return ddl, nil
}
