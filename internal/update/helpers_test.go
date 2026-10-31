package update

import (
	"database/sql"
	"os/exec"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// fakeServices is a scripted ServiceControl. Tests use it to prove
// the stop/start scoping and identity verification without touching
// live services.
type fakeServices struct {
	active  map[string]bool
	stopped []string
	started []string
	pids    map[string]int
}

func (f *fakeServices) ActiveUnits() []string {
	var out []string
	for _, u := range ManagedServices {
		if f.active[u] {
			out = append(out, u)
		}
	}
	return out
}

func (f *fakeServices) Stop(unit string) error {
	f.stopped = append(f.stopped, unit)
	return nil
}

func (f *fakeServices) Start(unit string) error {
	f.started = append(f.started, unit)
	return nil
}

func (f *fakeServices) MainPID(unit string) (int, error) {
	if p, ok := f.pids[unit]; ok {
		return p, nil
	}
	return 0, nil
}

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
