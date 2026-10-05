package critic

// Durable-cycle tests for the Critic audit lifecycle.
//
// The production lifecycle is ONE Audit per mpm-critic PROCESS. The
// scheduler's CriticAuditHandler execs the mpm-critic binary, that
// binary constructs one Audit via critic.New, runs it once, and exits.
// So these tests model the process boundary by constructing a FRESH
// Audit for each invocation against a SHARED database — they
// deliberately do NOT reuse one Audit across Runs, because doing so
// would test an architecture production never uses and would mask the
// very defect these tests exist to pin.
//
// Historical context: critic.New seeded cycle to 0 and Run did
// a.cycle++, so every process observed cycle 1 and the every-fifth-cycle
// Poison Pill could never fire in production. The cycle is now claimed
// atomically from durable state so it survives process exit.

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

// newSharedCriticDB creates ONE temp database carrying the hermetic
// test schema (including system_config), to be shared by successive
// simulated process invocations. Nothing here creates schema outside
// the fixture: production code claims rows, it does not define the
// table they live in.
func newSharedCriticDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "critic.db")
	sharedDBPath = path + "?_busy_timeout=5000&_journal_mode=WAL&_synchronous=NORMAL"
	db, err := sql.Open("sqlite3", sharedDBPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if _, err := db.Exec(criticTestSchema); err != nil {
		t.Fatalf("install schema: %v", err)
	}
	return db
}

// simulateProcessInvocation models ONE mpm-critic process: construct a
// fresh Audit, run exactly one cycle, discard it. The Audit never
// outlives this call, which is precisely what happens in production.
func simulateProcessInvocation(t *testing.T, db *sql.DB) (*Audit, *fakeCLI) {
	t.Helper()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a, err := New(db, log)
	if err != nil {
		t.Fatalf("critic.New: %v", err)
	}
	a.cli = &fakeCLI{}
	if err := a.Run(context.Background()); err != nil {
		t.Fatalf("audit run: %v", err)
	}
	return a, a.cli.(*fakeCLI)
}

// countPoisonPills counts poison-pill emissions specifically (the
// mpm_theories/propose tool+action pair), rather than asserting on
// global call counts that ordinary hunts would perturb.
func countPoisonPills(calls []fakeCall) int {
	n := 0
	for _, c := range calls {
		if c.Tool == "mpm_theories" && c.Action == "propose" {
			n++
		}
	}
	return n
}

// TestCriticCycle_PoisonPillAcrossProcessInvocations is the primary
// acceptance proof. Six simulated process invocations against one
// shared database must produce poison pill exactly on the fifth.
func TestCriticCycle_PoisonPillAcrossProcessInvocations(t *testing.T) {
	db := newSharedCriticDB(t)

	// Seed an eligible poison-pill target (high-confidence memory).
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	seed, err := New(db, log)
	if err != nil {
		t.Fatalf("seed audit: %v", err)
	}
	seedMemory(t, seed, "target", "memories", "high conf", "", 0.95, "", time.Now())

	var cycles []int
	var pills []int
	for inv := 1; inv <= 6; inv++ {
		a, cli := simulateProcessInvocation(t, db)
		cycles = append(cycles, a.Cycle())
		pills = append(pills, countPoisonPills(cli.Calls()))
	}

	for i, c := range cycles {
		want := i + 1
		if c != want {
			t.Errorf("invocation %d: cycle=%d, want %d (cycles must advance across process boundary)", i+1, c, want)
		}
	}

	for i, p := range pills {
		inv := i + 1
		switch inv {
		case 5:
			if p != 1 {
				t.Errorf("invocation 5: poison pill count=%d, want exactly 1", p)
			}
		default:
			if p != 0 {
				t.Errorf("invocation %d: poison pill count=%d, want 0", inv, p)
			}
		}
	}
}

// TestCriticCycle_PoisonPillTagUsesDurableCycle pins that the fifth
// invocation tags the finding with the DURABLE cycle number, not a
// process-local one.
func TestCriticCycle_PoisonPillTagUsesDurableCycle(t *testing.T) {
	db := newSharedCriticDB(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	seed, _ := New(db, log)
	seedMemory(t, seed, "target", "memories", "high conf", "", 0.95, "", time.Now())

	for inv := 1; inv <= 5; inv++ {
		_, cli := simulateProcessInvocation(t, db)
		if inv != 5 {
			continue
		}
		found := false
		for _, c := range cli.Calls() {
			if c.Tool != "mpm_theories" || c.Action != "propose" {
				continue
			}
			raw, _ := json.Marshal(c.Payload)
			var payload struct {
				Tags []string `json:"tags"`
			}
			if err := json.Unmarshal(raw, &payload); err != nil {
				t.Fatalf("decode payload: %v", err)
			}
			for _, tag := range payload.Tags {
				if tag == "cycle_5" {
					found = true
				}
				if tag == "cycle_0" || tag == "cycle_1" {
					t.Errorf("poison pill tagged with process-local cycle %q; want cycle_5", tag)
				}
			}
		}
		if !found {
			t.Errorf("fifth invocation did not tag poison pill with cycle_5")
		}
	}
}

// TestCriticCycle_RestartContinuity proves the state is database-durable
// rather than pointer-lifetime durable, including across a genuine
// connection reopen that models a process boundary.
func TestCriticCycle_RestartContinuity(t *testing.T) {
	db := newSharedCriticDB(t)

	a1, _ := simulateProcessInvocation(t, db)
	if a1.Cycle() != 1 {
		t.Errorf("first audit cycle=%d, want 1", a1.Cycle())
	}
	a1 = nil

	a2, _ := simulateProcessInvocation(t, db)
	if a2.Cycle() != 2 {
		t.Errorf("second audit cycle=%d, want 2", a2.Cycle())
	}
	a2 = nil

	// Reopen the database file entirely: nothing but the file survives.
	if err := db.Close(); err != nil {
		t.Fatalf("close db: %v", err)
	}
	reopened, err := sql.Open("sqlite3", dbPathOf(t))
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer reopened.Close()

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a3, err := New(reopened, log)
	if err != nil {
		t.Fatalf("new audit after reopen: %v", err)
	}
	if err := a3.Run(context.Background()); err != nil {
		t.Fatalf("run after reopen: %v", err)
	}
	if a3.Cycle() != 3 {
		t.Errorf("cycle after full reopen=%d, want 3 (state must be file-durable)", a3.Cycle())
	}
}

// TestCriticCycle_ConcurrentClaimsAreDistinct runs fresh Audits
// concurrently against one database. They must claim distinct
// consecutive cycle numbers.
func TestCriticCycle_ConcurrentClaimsAreDistinct(t *testing.T) {
	db := newSharedCriticDB(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	seed, _ := New(db, log)
	seedMemory(t, seed, "target", "memories", "high conf", "", 0.95, "", time.Now())

	const n = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make([]int, n)
	errs := make([]error, n)

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			a, err := New(db, log)
			if err != nil {
				errs[i] = err
				return
			}
			<-start
			errs[i] = a.Run(context.Background())
			results[i] = a.Cycle()
		}(i)
	}
	close(start)
	wg.Wait()

	seen := map[int]bool{}
	min, max := 0, 0
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if seen[results[i]] {
			t.Errorf("cycle %d claimed more than once", results[i])
		}
		seen[results[i]] = true
		if min == 0 || results[i] < min {
			min = results[i]
		}
		if results[i] > max {
			max = results[i]
		}
	}
	if max-min != n-1 {
		t.Errorf("claimed range %d..%d, want %d consecutive values", min, max, n)
	}
}

// TestCriticCycle_MalformedStateFailsClosed pins the compatibility
// matrix. A corrupt owned record must NOT be silently reset to 0,
// because that would resurrect cycle numbers that already drove a
// poison pill.
func TestCriticCycle_MalformedStateFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{"non-numeric cycle", `{"cycle":"not-a-number"}`},
		{"numeric string cycle", `{"cycle":"5"}`},
		{"missing cycle field", `{"other":1}`},
		{"json null cycle", `{"cycle":null}`},
		{"real cycle", `{"cycle":5.5}`},
		{"negative cycle", `{"cycle":-1}`},
		{"large negative cycle", `{"cycle":-999}`},
		{"empty object", `{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newSharedCriticDB(t)
			if _, err := db.Exec(
				`INSERT INTO system_config (key, raw_json, content_hash) VALUES (?, ?, '')`,
				criticCycleKey, tc.raw); err != nil {
				t.Fatalf("seed malformed: %v", err)
			}

			log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
			a, err := New(db, log)
			if err != nil {
				t.Fatalf("new: %v", err)
			}
			runErr := a.Run(context.Background())
			if runErr == nil {
				t.Fatalf("expected malformed state to fail the run; got nil")
			}
			if a.Cycle() != 0 {
				t.Errorf("cycle=%d after failed claim, want 0", a.Cycle())
			}

			// The corrupt row must be left untouched, not rewritten.
			var got string
			if err := db.QueryRow(`SELECT raw_json FROM system_config WHERE key = ?`, criticCycleKey).Scan(&got); err != nil {
				t.Fatalf("reread: %v", err)
			}
			if got != tc.raw {
				t.Errorf("malformed state was rewritten: got %q want %q", got, tc.raw)
			}
		})
	}
}

// TestCriticCycle_ZeroBootstrap pins the one non-negative value that
// IS accepted: stored cycle 0 means "no cycles yet", so the next claim
// yields 1. This must not wedge a hand-seeded or partially
// initialized row.
func TestCriticCycle_ZeroBootstrap(t *testing.T) {
	db := newSharedCriticDB(t)
	if _, err := db.Exec(
		`INSERT INTO system_config (key, raw_json, content_hash) VALUES (?, ?, '')`,
		criticCycleKey, `{"cycle":0}`); err != nil {
		t.Fatalf("seed zero: %v", err)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a, _ := New(db, log)
	a.cli = &fakeCLI{}
	if err := a.Run(context.Background()); err != nil {
		t.Fatalf("cycle 0 bootstrap should be accepted, got: %v", err)
	}
	if a.Cycle() != 1 {
		t.Errorf("cycle=%d after zero bootstrap, want 1", a.Cycle())
	}
}

// TestCriticCycle_RejectedClaimLeavesStateUntouched pins that a
// fail-closed claim does not silently repair corrupt state as a side
// effect of detecting it. A negative value must not increment to 0.
func TestCriticCycle_RejectedClaimLeavesStateUntouched(t *testing.T) {
	for _, raw := range []string{`{"cycle":-1}`, `{"cycle":5.5}`, `{"cycle":"5"}`} {
		t.Run(raw, func(t *testing.T) {
			db := newSharedCriticDB(t)
			if _, err := db.Exec(
				`INSERT INTO system_config (key, raw_json, content_hash) VALUES (?, ?, '')`,
				criticCycleKey, raw); err != nil {
				t.Fatalf("seed: %v", err)
			}

			log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
			a, _ := New(db, log)
			if err := a.Run(context.Background()); err == nil {
				t.Fatalf("expected fail-closed for %s", raw)
			}

			var got string
			if err := db.QueryRow(`SELECT raw_json FROM system_config WHERE key = ?`, criticCycleKey).Scan(&got); err != nil {
				t.Fatalf("reread: %v", err)
			}
			if got != raw {
				t.Errorf("rejected claim mutated state: %s -> %s (must be untouched)", raw, got)
			}
		})
	}
}

// TestCriticCycle_UnrelatedSystemConfigUntouched pins that owning one
// key does not disturb any other configuration row.
func TestCriticCycle_UnrelatedSystemConfigUntouched(t *testing.T) {
	db := newSharedCriticDB(t)
	if _, err := db.Exec(
		`INSERT INTO system_config (key, raw_json, content_hash) VALUES ('last_gc_at', '{"updated_at":"x"}', '')`); err != nil {
		t.Fatalf("seed unrelated: %v", err)
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a, _ := New(db, log)
	a.cli = &fakeCLI{}
	if err := a.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	var raw string
	if err := db.QueryRow(`SELECT raw_json FROM system_config WHERE key = 'last_gc_at'`).Scan(&raw); err != nil {
		t.Fatalf("unrelated row vanished: %v", err)
	}
	if raw != `{"updated_at":"x"}` {
		t.Errorf("unrelated row modified: %q", raw)
	}
}

// TestCriticCycle_CycleStableWithinRun pins that Cycle() does not drift
// across hunts inside one run.
func TestCriticCycle_CycleStableWithinRun(t *testing.T) {
	db := newSharedCriticDB(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	seed, _ := New(db, log)
	seedMemory(t, seed, "target", "memories", "high conf", "", 0.95, "", time.Now())

	seen := map[int]bool{}
	a := &Audit{
		db:  db,
		log: log,
		cli: &fakeCLI{},
		hunts: []Hunt{
			cycleObserverHunt{seen: seen},
			cycleObserverHunt{seen: seen},
			cycleObserverHunt{seen: seen},
		},
	}
	if err := a.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(seen) != 1 {
		t.Errorf("cycle drifted within run: observed %v", seen)
	}
	for c := range seen {
		if c != a.Cycle() {
			t.Errorf("hunt saw cycle %d but Run reports %d", c, a.Cycle())
		}
	}
}

// TestCriticCycle_PersistedShape pins the stored representation so a
// future change cannot silently alter the durable format.
func TestCriticCycle_PersistedShape(t *testing.T) {
	db := newSharedCriticDB(t)
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	a, _ := New(db, log)
	a.cli = &fakeCLI{}
	if err := a.Run(context.Background()); err != nil {
		t.Fatalf("run: %v", err)
	}

	var raw string
	if err := db.QueryRow(`SELECT raw_json FROM system_config WHERE key = ?`, criticCycleKey).Scan(&raw); err != nil {
		t.Fatalf("read state: %v", err)
	}
	if want := `{"cycle":1}`; raw != want {
		t.Errorf("persisted state=%s, want %s", raw, want)
	}

	// A resumed audit must continue from the stored value.
	a2, _ := New(db, log)
	a2.cli = &fakeCLI{}
	if err := a2.Run(context.Background()); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	if err := db.QueryRow(`SELECT raw_json FROM system_config WHERE key = ?`, criticCycleKey).Scan(&raw); err != nil {
		t.Fatalf("read state 2: %v", err)
	}
	if want := `{"cycle":2}`; raw != want {
		t.Errorf("persisted state=%s, want %s", raw, want)
	}
}

// cycleObserverHunt records whatever cycle value it observes.
type cycleObserverHunt struct{ seen map[int]bool }

func (cycleObserverHunt) Name() string { return "cycle-observer" }
func (h cycleObserverHunt) Run(_ context.Context, a *Audit) ([]Finding, error) {
	h.seen[a.Cycle()] = true
	return nil, nil
}

// dbPathOf returns the DSN of the shared test database by inspecting an
// already-open handle's file list, so the reconnect test does not have
// to thread the path separately.
func dbPathOf(t *testing.T) string {
	t.Helper()
	return sharedDBPath
}

// sharedDBPath is set by newSharedCriticDB for tests that must reopen.
var sharedDBPath string
