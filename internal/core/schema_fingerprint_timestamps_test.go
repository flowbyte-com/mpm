package internal

import (
	"fmt"
	"strings"
	"testing"
)

// TestSchemaFingerprint_TimestampColumnsAreInteger verifies every column
// in allTimestampsToMigrate resolves to INTEGER (not TEXT) on a freshly
// built DB after BaseTables + ReferenceTables + SafeMigrations +
// CommonIndexes run. If a future DDL change re-introduces a TEXT column
// for any of these fields, this test fails loudly.
//
// SQLite's manifest typing silently accepts strings stored in INTEGER
// columns, which is the C1 risk. This test is the structural safety net
// at the test layer — it catches the drift at build time rather than at
// production runtime, where the verifyTimestampsMigration post-UPDATE
// check would surface it (and refuse to write the sentinel).
//
// Mirrors verifyTimestampsMigration's runtime check at the test layer:
// the migration function verifies the storage class after running the
// UPDATE on real data; this test verifies the underlying schema
// declaration BEFORE any data is touched. They are complementary, not
// redundant.
func TestSchemaFingerprint_TimestampColumnsAreInteger(t *testing.T) {
	dm := NewTestDM(t)

	if len(allTimestampsToMigrate) == 0 {
		t.Fatal("allTimestampsToMigrate is empty; test would pass vacuously")
	}

	for _, tc := range allTimestampsToMigrate {
		t.Run(tc.table+"."+tc.col, func(t *testing.T) {
			var colType string
			// pragma_table_info is a table-valued function. The two-arg
			// form is (schema, table), so the column filter has to be a
			// WHERE clause; passing the column name as the second arg
			// would be parsed as a database name (and error out with
			// "unknown database 'created_at'").
			q := fmt.Sprintf(
				`SELECT type FROM pragma_table_info(%q) WHERE name = %q`,
				tc.table, tc.col,
			)
			err := dm.SQLDB().QueryRow(q).Scan(&colType)
			if err != nil {
				t.Fatalf("pragma_table_info(%s, %s): %v", tc.table, tc.col, err)
			}
			// pragma_table_info returns the declared type name. For INTEGER
			// affinity, this is "INTEGER" (case-insensitive). NUMERIC
			// affinity — the legacy DATETIME declaration — is not
			// acceptable here because the C1 root cause is that NUMERIC
			// affinity columns accept (then coerce back) strings.
			upper := strings.ToUpper(strings.TrimSpace(colType))
			if upper == "INTEGER" {
				return
			}
			t.Errorf("%s.%s: expected INTEGER column, got %q", tc.table, tc.col, colType)
		})
	}
}
