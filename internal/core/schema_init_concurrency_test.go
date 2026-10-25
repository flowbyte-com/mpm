// schema_init_concurrency_test.go — concurrent processes opening the
// same fresh database must not fail schema initialisation.
//
// Every CLI/MCP invocation opens its own DatabaseManager against
// <workspace>/src/db/mpm.db. On a fresh (or just-upgraded) database the
// first opens run the migration transaction concurrently. The transaction
// reads schema state and then writes migration sentinels; in WAL mode a
// write after a read snapshot that another process has superseded fails
// with SQLITE_BUSY_SNAPSHOT, which busy_timeout does NOT retry.
//
// Observed in the fresh-install acceptance run: 2 of 12 parallel
// `mpm remember` processes failed at startup with
//
//	opening database: failed to initialize schema: epistemic_pressure
//	deferral migration failed: record epistemic_pressure_deferral_v1
//	sentinel: database is locked
//
// The fix makes the steady-state open read-only (probe the sentinel
// before INSERT OR IGNORE). This test pins the user-visible contract:
// every concurrent open succeeds.
package internal

import (
	"fmt"
	"sync"
	"testing"
)

func TestConcurrentDatabaseOpens_SchemaInitSucceeds(t *testing.T) {
	if testing.Short() {
		t.Skip("concurrency fixture")
	}

	// A few rounds on fresh directories raise the chance of overlap
	// without making the test slow or flaky on a single-core runner.
	const rounds = 3
	const workers = 12

	for round := 0; round < rounds; round++ {
		t.Run(fmt.Sprintf("round-%d", round), func(t *testing.T) {
			dir := t.TempDir()
			start := make(chan struct{})
			errs := make(chan error, workers)
			var wg sync.WaitGroup
			for i := 0; i < workers; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					dm, err := NewDatabaseManager(dir)
					if err != nil {
						errs <- err
						return
					}
					_ = dm.Close()
				}()
			}
			close(start)
			wg.Wait()
			close(errs)

			var failures []string
			for err := range errs {
				failures = append(failures, err.Error())
			}
			if len(failures) > 0 {
				t.Fatalf("%d/%d concurrent opens failed on a fresh database:\n%v",
					len(failures), workers, failures)
			}
		})
	}
}
