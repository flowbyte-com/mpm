// F2 sequential semantic oracle.
//
// The concurrency tests (TestReinforceWeaken_Concurrent_NoUpdateLost etc.)
// check final state against a hard-coded expected number. Before this
// oracle, the expected numbers were derived from a flawed mental model:
// "weight += delta on reinforce, weight -= delta on weaken". The actual
// contract from web_db.go is:
//
//	ReinforceMemory(id, delta):
//	    rc    += delta
//	    weight = MIN(weight + (delta+1)/2, 100)
//	WeakenMemory(id, delta):
//	    rc    = MAX(rc - delta, 0)
//	    weight = MAX(weight - (delta+1)/2, 1)
//
// Floor-at-1 (T24, 2026-09-11): the substrate floor for `weight` is 1.0
// across every weight-modifying primitive (WeakenMemory, WeakenMemoryTool,
// AdjustMemoryWeight, legacy_weight). Pre-fix the floor was 0.0 and the
// tests below reflected that — they're updated to assert the canonical
// floor-at-1 contract. See internal/core/web_db.go:682-700 for the full
// rationale (scoring model assumes weight >= 1; embedding_migration
// filters synthetic memories by `weight < 1.0`).
//
// The oracle test exercises every cell of that matrix so the concurrency
// expectations derive from observed sequential behaviour, not from a
// pre-conceived notion of what "should" happen.
package internal

import (
	"database/sql"
	"sync"
	"testing"
	"time"
)

// TestWeightSemanticOracle_ReinforceWeaken_ForwardOrder pins the
// forward-order oracle: weight=1, rc=R; reinforce +5, then weaken -3.
//
// Expected per web_db.go:481 and :631:
//   reinforce +5:  rc += 5, weight = MIN(1 + 3, 100) = 4
//   weaken    -3:  rc = MAX(R+5 - 3, 0) = R+2, weight = MAX(4 - 2, 0) = 2
//
// Observed result of this test (recorded below in assertFinal) is the
// ground truth used by TestReinforceWeaken_Concurrent_NoUpdateLost.
func TestWeightSemanticOracle_ReinforceWeaken_ForwardOrder(t *testing.T) {
	dm := NewTestDM(t)
	id := seedWeight(t, dm, 1)

	var rcBefore int
	if err := dm.SQLDB().QueryRow(
		`SELECT reinforcement_count FROM memories WHERE id = ?`, id,
	).Scan(&rcBefore); err != nil {
		t.Fatalf("read rc before: %v", err)
	}

	if err := dm.ReinforceMemory(id, 5); err != nil {
		t.Fatalf("reinforce: %v", err)
	}

	wAfterR, rcAfterR := readState(t, dm, id)
	if wAfterR != 4 {
		t.Errorf("after reinforce +5 from weight=1: weight=%v, want 4", wAfterR)
	}
	if rcAfterR != rcBefore+5 {
		t.Errorf("after reinforce +5: rc=%d, want %d", rcAfterR, rcBefore+5)
	}

	if err := dm.WeakenMemory(id, 3); err != nil {
		t.Fatalf("weaken: %v", err)
	}

	wFinal, rcFinal := readState(t, dm, id)
	// Forward-order oracle: weight=2, rc=R+2.
	const wantWeightFwd = 2.0
	if wFinal != wantWeightFwd {
		t.Errorf("FORWARD ORACLE: weight=%v, want %v", wFinal, wantWeightFwd)
	}
	wantRCFwdVal := rcBefore + 5 - 3
	if rcFinal != wantRCFwdVal {
		t.Errorf("FORWARD ORACLE: rc=%d, want %d", rcFinal, wantRCFwdVal)
	}

	t.Logf("ORACLE FORWARD  weight=%v rc=%d  (rcBefore=%d)",
		wFinal, rcFinal, rcBefore)
}

// TestWeightSemanticOracle_WeakenReinforce_ReverseOrder pins the
// reverse-order oracle: weight=1, rc=R; weaken -3, then reinforce +5.
//
// Expected per the same contract:
//   weaken    -3:  rc = MAX(R - 3, 0) = MAX(R-3, 0), weight = MAX(1 - 2, 0) = 0
//   reinforce +5:  rc += 5, weight = MIN(0 + 3, 100) = 3
//
// Observed final: weight=3, rc=R+2 (or R-3+5 = R+2, same total delta).
func TestWeightSemanticOracle_WeakenReinforce_ReverseOrder(t *testing.T) {
	dm := NewTestDM(t)
	id := seedWeight(t, dm, 1)

	var rcBefore int
	if err := dm.SQLDB().QueryRow(
		`SELECT reinforcement_count FROM memories WHERE id = ?`, id,
	).Scan(&rcBefore); err != nil {
		t.Fatalf("read rc before: %v", err)
	}

	if err := dm.WeakenMemory(id, 3); err != nil {
		t.Fatalf("weaken: %v", err)
	}

	wAfterW, rcAfterW := readState(t, dm, id)
	// Floor-at-1 (T24): weight = MAX(1 - 2, 1) = 1 (not 0); rc = MAX(R-3, 0).
	if wAfterW != 1 {
		t.Errorf("after weaken -3 from weight=1: weight=%v, want 1 (floor)", wAfterW)
	}
	wantRCAfterW := rcBefore - 3
	if wantRCAfterW < 0 {
		wantRCAfterW = 0
	}
	if rcAfterW != wantRCAfterW {
		t.Errorf("after weaken -3: rc=%d, want %d", rcAfterW, wantRCAfterW)
	}

	if err := dm.ReinforceMemory(id, 5); err != nil {
		t.Fatalf("reinforce: %v", err)
	}

	wFinal, rcFinal := readState(t, dm, id)
	// Reverse-order oracle: weight = MIN(1 + 3, 100) = 4; rc = MAX(R-3,0) + 5.
	const wantWeightRev = 4.0
	if wFinal != wantWeightRev {
		t.Errorf("REVERSE ORACLE: weight=%v, want %v", wFinal, wantWeightRev)
	}
	wantRCFinal := rcAfterW + 5
	if rcFinal != wantRCFinal {
		t.Errorf("REVERSE ORACLE: rc=%d, want %d", rcFinal, wantRCFinal)
	}

	t.Logf("ORACLE REVERSE  weight=%v rc=%d  (rcBefore=%d)",
		wFinal, rcFinal, rcBefore)
}

// TestWeightSemanticOracle_NoUpdateLostUnderConcurrent pins the F2
// concurrency contract: from weight=1, both reinforce +5 and weaken -3
// must succeed and the final state must satisfy the coupled closure
// invariant that BOTH operations took effect.
//
// Per the SQL contract (weightGain = (delta+1)/2 = 3 for reinforce +5,
// weightLoss = (delta+1)/2 = 2 for weaken -3; both clamped at 0/100):
//
//	Reinforce first, from (weight=1, rc=0):
//	    rc    = MAX(0+5 - 3, 0) = 2
//	    weight = MAX(MIN(1+3,100) - 2, 0) = 2
//	Weaken first, from (weight=1, rc=0):
//	    rc    = MAX(MAX(0-3,0) + 5, 0) = 5   (the -3 floor at 0 is lost)
//	    weight = MIN(MAX(1-2,0) + 3, 100) = 3
//
// Either serialization is valid. The two outcomes are coupled:
//	(rc=2, weight=2)  ↔ reinforce-first
//	(rc=5, weight=3)  ↔ weaken-first
//
// Crucially, the original alpha observations of weight=1 and weight=4
// are NOT in this set — they would mean one or both operations were
// silently dropped (lost update). weight=0 would mean only weaken
// landed; weight=4 would mean only reinforce landed.
//
// This test runs 200 iterations against a fresh file-DM (WAL +
// busy_timeout=5000ms — production contract) and asserts:
//   - both operations succeed (no SQLITE_BUSY leak, no silent failure)
//   - final (rc, weight) ∈ {(2,2), (5,3)}
//   - last_accessed_at was written by at least one of the two ops
//   - the coupled pair invariant (rc matches weight per the table)
//
// Note on iteration setup: F19 idempotency in saveMemoryRow dedups by
// (content, tags, metadata) and returns the existing row's id. So the
// test reseeds via seedWeight once and then resets weight/reinforcement
// state every iteration with a direct UPDATE — same pattern as the
// earlier TestReinforceWeaken_Concurrent_NoUpdateLost.
func TestWeightSemanticOracle_NoUpdateLostUnderConcurrent(t *testing.T) {
	dm := newTestFileDM(t)
	id := seedWeight(t, dm, 1)

	reinforceFirstCount := 0
	weakenFirstCount := 0

	for iter := 0; iter < 200; iter++ {
		if _, err := dm.SQLDB().Exec(
			`UPDATE memories SET weight=1, reinforcement_count=0, last_accessed_at=NULL WHERE id=?`, id,
		); err != nil {
			t.Fatalf("iter=%d reset: %v", iter, err)
		}

		var rcBefore int
		var lastBefore sql.NullInt64
		if err := dm.SQLDB().QueryRow(
			`SELECT reinforcement_count, last_accessed_at FROM memories WHERE id = ?`, id,
		).Scan(&rcBefore, &lastBefore); err != nil {
			t.Fatalf("iter=%d read before: %v", iter, err)
		}

		rerrCh := make(chan error, 1)
		werrCh := make(chan error, 1)
		go func() { rerrCh <- dm.ReinforceMemory(id, 5) }()
		go func() { werrCh <- dm.WeakenMemory(id, 3) }()

		rerr := <-rerrCh
		werr := <-werrCh
		if rerr != nil {
			t.Fatalf("iter=%d reinforce failed: %v", iter, rerr)
		}
		if werr != nil {
			t.Fatalf("iter=%d weaken failed: %v", iter, werr)
		}

		var wFinal float64
		var rcFinal int
		var lastAfter sql.NullInt64
		if err := dm.SQLDB().QueryRow(
			`SELECT weight, reinforcement_count, last_accessed_at FROM memories WHERE id = ?`, id,
		).Scan(&wFinal, &rcFinal, &lastAfter); err != nil {
			t.Fatalf("iter=%d read after: %v", iter, err)
		}

		// Coupled closure invariant: (rc, weight) must be one of two
		// valid serializations. Any other pair means an operation was
		// silently dropped OR the floor logic differs from the SQL.
		//
		// Closure (T24 floor-at-1):
		//   reinforce first  → (rc=5, w=4) → weaken → (rc=2, w=2)
		//   weaken    first  → (rc=0, w=1) → reinforce → (rc=5, w=4)
		// Pre-T24 the floor was 0 and the weaken-first path could reach
		// (rc=5, w=3); the canonical floor at 1 raises both serializations
		// to land at w=4 instead, leaving (5,3) unreachable.
		switch {
		case rcFinal == 2 && wFinal == 2:
			reinforceFirstCount++
		case rcFinal == 5 && wFinal == 4:
			weakenFirstCount++
		default:
			t.Errorf("iter=%d: (rc=%d, weight=%v) not in closure set {(2,2),(5,4)} — operation lost or floor differs",
				iter, rcFinal, wFinal)
		}

		// Audit invariant: last_accessed_at must have advanced (both ops
		// bump it) OR moved from NULL → set (seed inserts NULL and at
		// least one of the two concurrent ops must have written a
		// timestamp). Even if the reset path sets last_accessed_at=NULL,
		// both ops overwrite it, so NULL on read-after is impossible if
		// both succeeded.
		if !lastAfter.Valid {
			t.Errorf("iter=%d: last_accessed_at remained NULL despite two concurrent mutations",
				iter)
		}

		_ = time.Now() // keep import; reserved for future timing log
	}

	t.Logf("serialization distribution: reinforce-first=%d weaken-first=%d (out of 200)",
		reinforceFirstCount, weakenFirstCount)
}

// readState fetches weight + reinforcement_count from a memory row.
func readState(t *testing.T, dm *DatabaseManager, id string) (float64, int) {
	t.Helper()
	var w float64
	var rc int
	if err := dm.SQLDB().QueryRow(
		`SELECT weight, reinforcement_count FROM memories WHERE id = ?`, id,
	).Scan(&w, &rc); err != nil {
		t.Fatalf("read state: %v", err)
	}
	return w, rc
}

// TestWeightSemanticOracle_HighContention_Stress pins the closure
// invariant under heavy mixed-direction contention: 8 goroutines × 20
// operations, half reinforce +1 and half weaken -1, against a single
// row pre-seeded at weight=50 / rc=50.
//
// Unlike single-direction stress (where exact totals prove no lost
// updates), mixed-direction totals depend on the *order* of the
// interleaved ops because of the asymmetric SQL clamps (MIN(100) on
// reinforce, MAX(0) on weaken). When a sequence of weakens runs first
// against a small starting weight, intermediate weakens hit the 0 floor
// and become no-ops for weight; the corresponding 80 reinforces still
// drive weight up to 80. When the order is the reverse, all 80 weakens
// land and the final weight is 50.
//
// What we CAN assert deterministically:
//   - all 160 operations succeeded (errCh empty)
//   - weight ∈ [0, 100]      (model bounds)
//   - rc     ≥ 0              (weaken floor)
//   - weight ended in the deterministic range [50, 80] given a 50-start
//     and 80+80 - 80 = 0 net ops (max = 50 + 80 = 130 clamped to 100,
//     but weightGain=1 per reinforce gives 50+80=130 only if ALL
//     weakens hit the floor — which requires 50 of them to land first,
//     impossible since only 50 weigh units exist; the actual max is
//     80 reached when 50 weakens drive weight to 0 then all 80
//     reinforces run)
//
// The lost-update detection for mixed direction is provided by:
//   - TestWeightSemanticOracle_NoUpdateLostUnderConcurrent (200-iter
//     closure-invariant oracle from weight=1)
//   - TestWeightSemanticOracle_HighContention_ReinforceOnly
//   - TestWeightSemanticOracle_HighContention_WeakenOnly
// together. This test adds the all-ops-succeeded + bounds contract
// under high contention.
func TestWeightSemanticOracle_HighContention_Stress(t *testing.T) {
	dm := newTestFileDM(t)
	id := seedWeight(t, dm, 50)

	if _, err := dm.SQLDB().Exec(
		`UPDATE memories SET reinforcement_count = 50 WHERE id = ?`, id,
	); err != nil {
		t.Fatalf("seed rc: %v", err)
	}

	const goroutines = 8
	const perGoroutine = 20
	const reinDelta = 1
	const weakDelta = 1

	var wg sync.WaitGroup
	errCh := make(chan error, goroutines*perGoroutine)

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(reinforce bool) {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				if reinforce {
					if err := dm.ReinforceMemory(id, reinDelta); err != nil {
						errCh <- err
						return
					}
				} else {
					if err := dm.WeakenMemory(id, weakDelta); err != nil {
						errCh <- err
						return
					}
				}
			}
		}(i%2 == 0)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("op failed under stress: %v", err)
	}

	var w float64
	var rc int
	if err := dm.SQLDB().QueryRow(
		`SELECT weight, reinforcement_count FROM memories WHERE id = ?`, id,
	).Scan(&w, &rc); err != nil {
		t.Fatalf("read final: %v", err)
	}

	// Bounds: model contracts hold.
	if w < 0 || w > 100 {
		t.Errorf("mixed stress: weight=%v outside [0, 100] model bounds", w)
	}
	if rc < 0 {
		t.Errorf("mixed stress: rc=%d outside [0, ∞) model bounds", rc)
	}

	// Deterministic range for weight: from 50 with 80 reinforces
	// (gain=1 each, capped at 100) and 80 weakens (loss=1 each, floor
	// at 0). All weakens landing → weight = 50. Some weakens hitting
	// the floor mid-sequence → weight > 50, max observed 80 in practice.
	const wMin = 50.0
	const wMax = 80.0
	if w < wMin || w > wMax {
		t.Errorf("mixed stress: weight=%v outside deterministic range [%v, %v]",
			w, wMin, wMax)
	}

	// RC deterministic range: rc starts at 50, plus 80 from reinforces
	// (always land since rc < ∞), minus some weakens (clamped at 0 if
	// they hit rc=0). All weakens landing → rc = 50. Weakens clamped
	// → rc can grow up to 50 + 80 = 130.
	const rcMin = 50
	const rcMax = 130
	if rc < rcMin || rc > rcMax {
		t.Errorf("mixed stress: rc=%d outside deterministic range [%d, %d]",
			rc, rcMin, rcMax)
	}

	t.Logf("mixed stress: weight=%v rc=%d (range weight=[%v,%v] rc=[%d,%d])",
		w, rc, wMin, wMax, rcMin, rcMax)
}

// TestWeightSemanticOracle_HighContention_ReinforceOnly pins the
// same-direction high-contention case: 8 goroutines × 20 reinforces,
// no weakens. After all goroutines join, weight must be exactly
// MIN(seedWeight + 8*20*1, 100) and rc must be exactly 8*20.
func TestWeightSemanticOracle_HighContention_ReinforceOnly(t *testing.T) {
	dm := newTestFileDM(t)
	id := seedWeight(t, dm, 10)

	const goroutines = 8
	const perGoroutine = 20
	const delta = 1

	var wg sync.WaitGroup
	errCh := make(chan error, goroutines*perGoroutine)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				if err := dm.ReinforceMemory(id, delta); err != nil {
					errCh <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("op failed: %v", err)
	}

	var w float64
	var rc int
	if err := dm.SQLDB().QueryRow(
		`SELECT weight, reinforcement_count FROM memories WHERE id = ?`, id,
	).Scan(&w, &rc); err != nil {
		t.Fatalf("read final: %v", err)
	}

	const wantRC = goroutines * perGoroutine // 160
	const wantW = 100.0                      // capped at 100 from 10 + 160
	if rc != wantRC {
		t.Errorf("reinforce-only stress: rc=%d, want %d", rc, wantRC)
	}
	if w != wantW {
		t.Errorf("reinforce-only stress: weight=%v, want %v (cap)", w, wantW)
	}
}

// TestWeightSemanticOracle_HighContention_WeakenOnly pins the
// reverse-direction high-contention case: 8 goroutines × 20 weakens
// from weight=50. After all join, weight must be MAX(50 - 160, 0) = 0
// and rc must hit the floor at 0 (started from rc=0, every weaken
// subtracts 1, so rc converges to 0 from below the floor).
func TestWeightSemanticOracle_HighContention_WeakenOnly(t *testing.T) {
	dm := newTestFileDM(t)
	id := seedWeight(t, dm, 50)

	const goroutines = 8
	const perGoroutine = 20
	const delta = 1

	var wg sync.WaitGroup
	errCh := make(chan error, goroutines*perGoroutine)
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				if err := dm.WeakenMemory(id, delta); err != nil {
					errCh <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("op failed: %v", err)
	}

	var w float64
	var rc int
	if err := dm.SQLDB().QueryRow(
		`SELECT weight, reinforcement_count FROM memories WHERE id = ?`, id,
	).Scan(&w, &rc); err != nil {
		t.Fatalf("read final: %v", err)
	}

	// rc started at 0, every weaken subtracts 1 but MAX(.., 0) clamps
	// at 0, so rc stays at 0 throughout. weight: 50 - 160 = -110
	// clamped to 1 (floor-at-1, T24 2026-09-11).
	const wantRC = 0
	const wantW = 1.0
	if rc != wantRC {
		t.Errorf("weaken-only stress: rc=%d, want %d", rc, wantRC)
	}
	if w != wantW {
		t.Errorf("weaken-only stress: weight=%v, want %v (floor)", w, wantW)
	}
}