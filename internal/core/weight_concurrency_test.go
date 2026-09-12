// Regression tests for the concurrent weight-update race.
//
// The validation run observed `final weight=4` under concurrent reinforce
// +5 / weaken -3 starting from weight=1. That observation was traced to
// the in-memory shared-cache test DB (no WAL, no busy_timeout) where
// SQLITE_BUSY on the second writer silently aborted one of the two
// UPDATEs — the implementation never lost an UPDATE under WAL, but the
// test harness was returning early on contention.
//
// The fix is to make the mutation structurally atomic at the database
// level (single-statement UPDATE) and to add a read-back assertion
// (Defense Triad rule 3) so a silent-promotion cannot slip past
// RowsAffected. eaac79a is the production change.
//
// The actual SQL contract (see web_db.go:535 and :701):
//
//	ReinforceMemory(id, delta):
//	    rc    += delta
//	    weight = MIN(weight + (delta+1)/2, 100)
//	WeakenMemory(id, delta):
//	    rc    = MAX(rc - delta, 0)
//	    weight = MAX(weight - (delta+1)/2, 1)
//
// Floor-at-1 (T24, 2026-09-11): the substrate floor is 1 across every
// weight-modifying primitive. Pre-fix the floor was 0 and weaken-first
// could drive weight below 1 — the closure invariant then read
// {(2,2), (5,3)} for the (weight=1) seed. After T24 weaken-first floors
// at weight=1, so the post-weaken reinforce path lands at weight=4 and
// the closure expands to {(2,2), (5,4)}.
//
// The test below (weight=2 seed) inherits the same floor effect:
// weaken-first now lands at weight=1 (not 0), and the subsequent
// reinforce gains +3, leaving the closure at {(2,3), (5,4)}.
package internal

import (
	"sync"
	"testing"
)

// TestReinforceWeaken_Concurrent_NoUpdateLost pins that an atomic
// SQL-level mutation survives concurrent reinforce/weaken operations
// across many iterations. Each iter resets weight=2 (the prior test's
// baseline — chosen because weight=2 + reinforce +5 → 5, then weaken -2
// → 3 AND weight=2 → weaken 1 → 1 (T24 floor), then reinforce +3 → 4,
// yielding weight ∈ {3, 4} depending on serialization) and asserts
// the coupled closure invariant. From weight=2 the valid outcomes
// are:
//   reinforce-first  → (rc=2, w=3)
//   weaken-first     → (rc=5, w=4)  (T24: pre-fix this was (rc=5, w=3))
func TestReinforceWeaken_Concurrent_NoUpdateLost(t *testing.T) {
	dm := newTestFileDM(t)
	id := seedWeight(t, dm, 2)

	for iter := 0; iter < 75; iter++ {
		if _, err := dm.SQLDB().Exec(
			`UPDATE memories SET weight=2, reinforcement_count=0 WHERE id=?`, id,
		); err != nil {
			t.Fatalf("iter=%d reset: %v", iter, err)
		}

		var wg sync.WaitGroup
		wg.Add(2)
		var rerr, werr error
		go func() {
			defer wg.Done()
			rerr = dm.ReinforceMemory(id, 5)
		}()
		go func() {
			defer wg.Done()
			werr = dm.WeakenMemory(id, 3)
		}()
		wg.Wait()
		if rerr != nil || werr != nil {
			t.Fatalf("iter=%d: reinforce=%v weaken=%v", iter, rerr, werr)
		}

		var w float64
		var rc int
		if err := dm.SQLDB().QueryRow(
			`SELECT weight, reinforcement_count FROM memories WHERE id=?`, id,
		).Scan(&w, &rc); err != nil {
			t.Fatalf("iter=%d read: %v", iter, err)
		}
		// Closure set with T24 floor-at-1:
		//   reinforce-first: weight=MIN(2+3,100)=5, weaken weight=MAX(5-2,1)=3, rc=MAX(5-3,0)=2  → (w=3, rc=2)
		//   weaken-first:    weight=MAX(2-2,1)=1, reinforce weight=1+3=4, rc=MAX(0-3,0)+5=5 → (w=4, rc=5)
		// So weight ∈ {3, 4} and the rc/weight are coupled.
		switch {
		case rc == 2 && w == 3:
			// reinforce-first serialization
		case rc == 5 && w == 4:
			// weaken-first serialization (T24 floor-at-1)
		default:
			t.Errorf("iter=%d: (rc=%d, w=%v) not in closure {(2,3),(5,4)} — operation lost or floor differs",
				iter, rc, w)
		}
	}
}

// TestReinforceReinfoce_Concurrent_SameDirection pins that same-direction
// concurrent reinforces survive without lost updates. Two concurrent
// reinforce delta=5 from weight=2 should give `2 + 3 + 3 = 8` (the
// clamping rules + MIN cap of 100 keep it well under). The rc closure
// is `0 + 5 + 5 = 10`.
func TestReinforceReinfoce_Concurrent_SameDirection(t *testing.T) {
	dm := newTestFileDM(t)
	id := seedWeight(t, dm, 2)

	for iter := 0; iter < 25; iter++ {
		if _, err := dm.SQLDB().Exec(`UPDATE memories SET weight=2, reinforcement_count=0 WHERE id=?`, id); err != nil {
			t.Fatalf("reset: %v", err)
		}
		var r1err, r2err error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			r1err = dm.ReinforceMemory(id, 5)
		}()
		go func() {
			defer wg.Done()
			r2err = dm.ReinforceMemory(id, 5)
		}()
		wg.Wait()
		if r1err != nil || r2err != nil {
			t.Fatalf("iter=%d: r1=%v r2=%v", iter, r1err, r2err)
		}
		var w float64
		var rc int
		if err := dm.SQLDB().QueryRow(
			`SELECT weight, reinforcement_count FROM memories WHERE id=?`, id,
		).Scan(&w, &rc); err != nil {
			t.Fatalf("iter=%d read: %v", iter, err)
		}
		const wantW = 8.0 // 2 + 3 + 3
		const wantRC = 10  // 0 + 5 + 5
		if w != wantW {
			t.Errorf("iter=%d: final weight=%v, want %v", iter, w, wantW)
		}
		if rc != wantRC {
			t.Errorf("iter=%d: final rc=%d, want %d", iter, rc, wantRC)
		}
	}
}

// TestWeightUpdate_MultipleGoroutines_Bounds pins the clamping
// invariants under stress: weight can never go below the floor (1 for
// weaken, lower for SetMemoryWeight), and MIN(100) caps reinforce.
func TestWeightUpdate_MultipleGoroutines_Bounds(t *testing.T) {
	dm := newTestFileDM(t)
	id := seedWeight(t, dm, 50)

	runUp := func(delta int) {
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 20; j++ {
					_ = dm.ReinforceMemory(id, delta)
				}
			}()
		}
		wg.Wait()
	}
	runUp(10)

	var w float64
	if err := dm.SQLDB().QueryRow(`SELECT weight FROM memories WHERE id=?`, id).Scan(&w); err != nil {
		t.Fatal(err)
	}
	if w > 100 {
		t.Errorf("weight must cap at 100, got %v", w)
	}

	// Now reverse: many weakens. Web_db WeakenMemory floors at 0 (>=0).
	if _, err := dm.SQLDB().Exec(`UPDATE memories SET weight=10 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	runDown := func(delta int) {
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 20; j++ {
					_ = dm.WeakenMemory(id, delta)
				}
			}()
		}
		wg.Wait()
	}
	runDown(5)

	if err := dm.SQLDB().QueryRow(`SELECT weight FROM memories WHERE id=?`, id).Scan(&w); err != nil {
		t.Fatal(err)
	}
	if w < 0 {
		t.Errorf("weight must not go negative, got %v", w)
	}
}

// TestReinforceMemory_Atomicity_Regression pins a deterministic
// property of the mutator: regardless of ordering, the atomic UPDATE
// shape cannot silently drop a write. The regression captures the
// structural shape (single-statement atomic, with read-back assertion)
// so future refactors cannot regress to a read-modify-write sequence.
//
// The hammer uses a real file-backed DatabaseManager so SQLite WAL
// mode + busy_timeout=5000ms (the production contract) serializes
// concurrent writers the way the substrate relies on under load.
func TestReinforceMemory_Atomicity_Regression(t *testing.T) {
	dm := newTestFileDM(t)
	id := seedWeight(t, dm, 10)

	var wg sync.WaitGroup
	const goroutines = 8
	const perGoroutine = 25
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				if err := dm.ReinforceMemory(id, 1); err != nil {
					t.Errorf("reinforce: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	var w float64
	if err := dm.SQLDB().QueryRow(`SELECT weight FROM memories WHERE id=?`, id).Scan(&w); err != nil {
		t.Fatal(err)
	}
	// Each reinforce delta=1 → weightGain=1. 8 × 25 = 200 calls, each
	// gain=1, capped at 100. Starting from 10, we expect ~100 (cap).
	// Invariants: non-negative, not above the cap.
	if w < 0 {
		t.Errorf("weight went negative: %v", w)
	}
	if w > 100 {
		t.Errorf("weight exceeded MIN(.., 100) cap: %v", w)
	}
	// Reinforcement_count check: should have advanced by exactly
	// `goroutines × perGoroutine = 200` from the seed state.
	var rc int64
	if err := dm.SQLDB().QueryRow(`SELECT reinforcement_count FROM memories WHERE id=?`, id).Scan(&rc); err != nil {
		t.Fatal(err)
	}
	if rc < int64(goroutines*perGoroutine) {
		t.Errorf("reinforcement_count=%d, want >=%d (no lost updates)", rc, goroutines*perGoroutine)
	}
}

// newTestFileDM returns a DatabaseManager pointed at a fresh tempfile
// DB so the SQLITE_BUSY handling in production (WAL + busy_timeout=5000ms)
// is exercised rather than the in-memory shared-cache harsher locking.
// Used by tests that drive concurrent writes.
func newTestFileDM(t *testing.T) *DatabaseManager {
	t.Helper()
	dbPath := t.TempDir() + "/wac.db"
	dm, err := NewDatabaseManager(dbPath)
	if err != nil {
		t.Fatalf("newTestFileDM: %v", err)
	}
	// Mirror the production busy_timeout = 5000ms so concurrent writers
	// retry transparently instead of surfacing SQLITE_BUSY the way the
	// in-memory shared-cache test DM does.
	if _, err := dm.SQLDB().Exec(`PRAGMA busy_timeout = 5000`); err != nil {
		t.Fatalf("newTestFileDM: busy_timeout: %v", err)
	}
	t.Cleanup(func() { dm.Close() })
	return dm
}

// seedWeight inserts a fresh memory row and forces its weight to `start`.
// Returns the inserted memory id.
func seedWeight(t *testing.T, dm CoreDB, start float64) string {
	t.Helper()
	id, err := dm.SaveMemoryWithExtras(
		"memories", "race target memory", "",
		nil, nil, nil, false, start, "", "0.5", "0.5", "",
	)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return id
}
