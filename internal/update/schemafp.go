package update

import (
	"crypto/sha256"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	_ "github.com/mattn/go-sqlite3"
)

// SchemaFingerprint returns a stable hash of a database's SHAPE: the
// sorted table names plus each table's column names, types, not-null
// flags and defaults, plus its CHECK constraints and index/trigger
// names.
//
// The fingerprint is the rollback safety mechanism. It deliberately
// hashes the shape and not the DATA: row contents change on every run
// and must never block a rollback, but a shape change means a migration
// ran — and restoring old binaries onto a migrated shape is how an
// operator ends up with a runtime that cannot read its own database.
//
// The extraction mirrors what scripts/lib/schema_preflight.py compares
// (columns incl. type/notnull/default, CHECK exprs, indexes, triggers),
// hashed to a single string so the history file stays small.
func SchemaFingerprint(dbPath string) (string, error) {
	db, err := openSQLiteRO(dbPath)
	if err != nil {
		return "", err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)

	rows, err := db.Query("SELECT name, sql FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	if err != nil {
		return "", err
	}
	type table struct {
		name string
		sql  string
	}
	var tables []table
	for rows.Next() {
		var t table
		var sqlNull sql.NullString
		if err := rows.Scan(&t.name, &sqlNull); err != nil {
			rows.Close()
			return "", err
		}
		t.sql = sqlNull.String
		tables = append(tables, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", err
	}

	var b strings.Builder
	for _, t := range tables {
		fmt.Fprintf(&b, "table %s\n", t.name)
		cols, err := db.Query(fmt.Sprintf(`PRAGMA table_info("%s")`, t.name))
		if err != nil {
			return "", err
		}
		var lines []string
		for cols.Next() {
			var cid int
			var name, ctype string
			var notnull int
			var dflt sql.NullString
			var pk int
			if err := cols.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
				cols.Close()
				return "", err
			}
			d := ""
			if dflt.Valid {
				d = dflt.String
			}
			lines = append(lines, fmt.Sprintf("  col %s %s notnull=%d default=%q pk=%d", name, strings.ToUpper(ctype), notnull, d, pk))
		}
		cols.Close()
		if err := cols.Err(); err != nil {
			return "", err
		}
		sort.Strings(lines)
		for _, l := range lines {
			b.WriteString(l + "\n")
		}
		fmt.Fprintf(&b, "  checks %q\n", extractCheckExprs(t.sql))
		b.WriteString("  indexes " + strings.Join(indexNames(db, t.name), ",") + "\n")
		b.WriteString("  triggers " + strings.Join(triggerNames(db, t.name), ",") + "\n")
	}
	sum := sha256.Sum256([]byte(b.String()))
	return fmt.Sprintf("sha256:%x", sum), nil
}

func indexNames(db *sql.DB, table string) []string {
	return objectNames(db, "index", table)
}

func triggerNames(db *sql.DB, table string) []string {
	return objectNames(db, "trigger", table)
}

func objectNames(db *sql.DB, typ, table string) []string {
	rows, err := db.Query("SELECT name FROM sqlite_master WHERE type=? AND tbl_name=? AND sql IS NOT NULL ORDER BY name", typ, table)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if rows.Scan(&n) == nil {
			out = append(out, n)
		}
	}
	return out
}

// extractCheckExprs returns the parenthesis-balanced CHECK constraint
// expressions of a CREATE TABLE statement, normalised the way
// schema_preflight.py normalises them (comments stripped, quoting and
// whitespace unified), sorted.
func extractCheckExprs(sql string) []string {
	sql = stripSQLComments(sql)
	upper := strings.ToUpper(sql)
	var out []string
	i := 0
	for {
		j := strings.Index(upper[i:], "CHECK")
		if j < 0 {
			break
		}
		j += i
		k := j + len("CHECK")
		for k < len(sql) && (sql[k] == ' ' || sql[k] == '\t' || sql[k] == '\n' || sql[k] == '\r') {
			k++
		}
		if k >= len(sql) || sql[k] != '(' {
			i = j + len("CHECK")
			continue
		}
		depth := 0
		m := k
		var quote byte
		inQuote := false
		for m < len(sql) {
			c := sql[m]
			if inQuote {
				if c == quote {
					inQuote = false
				}
			} else if c == '\'' || c == '"' || c == '`' {
				inQuote = true
				quote = c
			} else if c == '(' {
				depth++
			} else if c == ')' {
				depth--
				if depth == 0 {
					break
				}
			}
			m++
		}
		if m >= len(sql) {
			out = append(out, normaliseSQL(sql[j:]))
			break
		}
		out = append(out, normaliseSQL(sql[j:m+1]))
		i = m + 1
	}
	sort.Strings(out)
	return out
}

func stripSQLComments(sql string) string {
	var b strings.Builder
	i := 0
	for i < len(sql) {
		if strings.HasPrefix(sql[i:], "/*") {
			if j := strings.Index(sql[i+2:], "*/"); j >= 0 {
				b.WriteString(" ")
				i += j + 4
				continue
			}
			break
		}
		if strings.HasPrefix(sql[i:], "--") {
			j := strings.IndexByte(sql[i:], '\n')
			if j < 0 {
				break
			}
			b.WriteString(" ")
			i += j + 1
			continue
		}
		b.WriteByte(sql[i])
		i++
	}
	return b.String()
}

func normaliseSQL(s string) string {
	s = strings.ReplaceAll(s, `"`, "'")
	s = strings.ReplaceAll(s, "`", "'")
	return strings.Join(strings.Fields(s), " ")
}
