package update

import (
	"database/sql"
	"os/exec"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

func openTestDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite3", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

type testCmd struct {
	dir  string
	args []string
}

func (c *testCmd) CombinedOutput() ([]byte, error) {
	cmd := exec.Command("git", c.args...)
	cmd.Dir = c.dir
	return cmd.CombinedOutput()
}
