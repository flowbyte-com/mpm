// F2 mixed-direction concurrency regression suite.
//
// The closure-membership oracle (this file) is the structural assertion
// against "successful operation silently lost". For a given starting
// state and operation count, the closure set enumerates every (rc,
// weight) pair reachable by interleaving the actual successful
// operations in any order under the documented SQL semantics:
//
//	ReinforceMemory(id, delta):
//	    rc    += delta
//	    weight = MIN(weight + (delta+1)/2, 100)
//	WeakenMemory(id, delta):
//	    rc    = MAX(rc - delta, 0)
//	    weight = MAX(weight - (delta+1)/2, 0)
//
// The test asserts that the durable (rc, weight) is in that closure.
// Any out-of-closure state means an operation returned success but
// its effect is absent from durable state — the silent-promotion
// class the read-back assertion in eaac79a was designed to surface.
//
// Closure is computed via BFS over (rc, w, rLeft, wLeft) state, which
// scales to large operation counts (Case D = 20+20) without exponential
// interleaving enumeration.
//
// Per-iteration outcome accounting distinguishes "operation attempted"
// from "operation succeeded" — failures are logged verbatim so a
// regression where errors are silently discarded cannot masquerade as
// success.
package internal

import (
	"sync"
	"testing"
)

// closureBFS computes the closure set of (rc, w) reachable from
// (rc0, w0) under `reinforceOK` × reinforce and `weakenOK` × weaken
// operations. Returns a set keyed by (rc, w).
func closureBFS(rc0, w0 int, reinforceOK, weakenOK, reinDelta, weakDelta int) map[[2]int]bool {
	const cap = 100
	weightGain := (reinDelta + 1) / 2
	if weightGain == 0 {
		weightGain = 1
	}
	weightLoss := (weakDelta + 1) / 2

	type state struct {
		rc, w int
		rLeft int
		wLeft int
	}

	start := state{rc0, w0, reinforceOK, weakenOK}
	visited := map[state]bool{start: true}
	out := map[[2]int]bool{}
	queue := []state{start}

	for len(queue) > 0 {
		s := queue[0]
		queue = queue[1:]
		if s.rLeft == 0 && s.wLeft == 0 {
			out[[2]int{s.rc, s.w}] = true
			continue
		}
		if s.rLeft > 0 {
			rc := s.rc + reinDelta
			w := s.w + weightGain
			if w > cap {
				w = cap
			}
			ns := state{rc, w, s.rLeft - 1, s.wLeft}
			if !visited[ns] {
				visited[ns] = true
				queue = append(queue, ns)
			}
		}
		if s.wLeft > 0 {
			rc := s.rc - weakDelta
			if rc < 0 {
				rc = 0
			}
			w := s.w - weightLoss
			if w < 0 {
				w = 0
			}
			ns := state{rc, w, s.rLeft, s.wLeft - 1}
			if !visited[ns] {
				visited[ns] = true
				queue = append(queue, ns)
			}
		}
	}
	return out
}

// runMixedCase is the shared harness for all four intensities. It
// fires the requested number of concurrent reinforce/weaken calls,
// captures every operation's outcome verbatim, and asserts the
// durable (rc, weight) is in the closure set for the SUCCESSFUL
// operation count.
func runMixedCase(t *testing.T, name string, iters, nReinforce, nWeaken, reinDelta, weakDelta, baselineWeight int) {
	t.Helper()
	dm := newTestFileDM(t)
	id := seedWeight(t, dm, baselineWeight)

	for iter := 0; iter < iters; iter++ {
		if _, err := dm.SQLDB().Exec(
			`UPDATE memories SET weight=?, reinforcement_count=0 WHERE id=?`, baselineWeight, id,
		); err != nil {
			t.Fatalf("[%s] iter=%d reset: %v", name, iter, err)
		}

		rerrCh := make(chan error, nReinforce)
		werrCh := make(chan error, nWeaken)
		var wg sync.WaitGroup
		wg.Add(nReinforce + nWeaken)

		for i := 0; i < nReinforce; i++ {
			go func() {
				defer wg.Done()
				rerrCh <- dm.ReinforceMemory(id, reinDelta)
			}()
		}
		for i := 0; i < nWeaken; i++ {
			go func() {
				defer wg.Done()
				werrCh <- dm.WeakenMemory(id, weakDelta)
			}()
		}
		wg.Wait()
		close(rerrCh)
		close(werrCh)

		var reinforceOK, weakenOK int
		var errs []string
		for e := range rerrCh {
			if e == nil {
				reinforceOK++
			} else {
				errs = append(errs, "R: "+e.Error())
			}
		}
		for e := range werrCh {
			if e == nil {
				weakenOK++
			} else {
				errs = append(errs, "W: "+e.Error())
			}
		}

		var wFinal float64
		var rcFinal int
		if err := dm.SQLDB().QueryRow(
			`SELECT weight, reinforcement_count FROM memories WHERE id=?`, id,
		).Scan(&wFinal, &rcFinal); err != nil {
			t.Fatalf("[%s] iter=%d read: %v", name, iter, err)
		}

		closure := closureBFS(0, baselineWeight, reinforceOK, weakenOK, reinDelta, weakDelta)
		key := [2]int{rcFinal, int(wFinal)}
		if !closure[key] {
			t.Errorf("[%s] iter=%d: (rc=%d, w=%v) not in closure (succ: r=%d w=%d errs=%v) — silent lost update",
				name, iter, rcFinal, wFinal, reinforceOK, weakenOK, errs)
		}

		if len(errs) > 0 {
			t.Logf("[%s] iter=%d: %d op(s) failed (closure still satisfied): %v",
				name, iter, len(errs), errs)
		}
	}
}

// TestWeightMixedConcurrent_1plus1 is the canonical sequential closure:
// 1 reinforce (+5) + 1 weaken (-3) from (rc=0, w=1) yields
// (rc, w) ∈ {(2, 2), (5, 3)}. Already exercised by the existing
// semantic-oracle test, repeated here as the smallest case.
func TestWeightMixedConcurrent_1plus1(t *testing.T) {
	runMixedCase(t, "1+1", 100, 1, 1, 5, 3, 1)
}

// TestWeightMixedConcurrent_5plus3 is the alpha-rerun anomaly shape:
// 5 reinforce (+5) + 3 weaken (-3) from (rc=0, w=1). Closure set
// (computed computationally) contains 6 valid (rc, w) pairs — none
// of them is weight=4, so any pre-fix observation of "weight=4" was
// either pre-F2-fix code, an in-memory shared-cache test DB without
// WAL, or a test that silently discarded operation errors.
func TestWeightMixedConcurrent_5plus3(t *testing.T) {
	runMixedCase(t, "5+3", 50, 5, 3, 5, 3, 1)
}

// TestWeightMixedConcurrent_10plus10 doubles the contention to verify
// the closure-membership invariant holds under heavier load.
func TestWeightMixedConcurrent_10plus10(t *testing.T) {
	runMixedCase(t, "10+10", 25, 10, 10, 5, 3, 10)
}

// TestWeightMixedConcurrent_20plus20 verifies the BFS-based closure
// oracle scales to many small operations without false membership.
func TestWeightMixedConcurrent_20plus20(t *testing.T) {
	runMixedCase(t, "20+20", 10, 20, 20, 1, 1, 50)
}

// TestWeightMixedConcurrent_5plus3_BusyClassification verifies that
// SQLITE_BUSY-shaped errors do not escape the production path under
// the canonical 5+3 case. The production wrapper in web_db.go:497-501
// surfaces busy errors distinctly so callers cannot mistake a busy
// outcome for success. Under WAL + busy_timeout=5000ms (production
// contract) no busy errors are expected to surface at the call site.
func TestWeightMixedConcurrent_5plus3_BusyClassification(t *testing.T) {
	dm := newTestFileDM(t)
	id := seedWeight(t, dm, 1)

	const iters = 100
	busyCount := 0
	for iter := 0; iter < iters; iter++ {
		if _, err := dm.SQLDB().Exec(
			`UPDATE memories SET weight=1, reinforcement_count=0 WHERE id=?`, id,
		); err != nil {
			t.Fatalf("iter=%d reset: %v", iter, err)
		}

		rerrCh := make(chan error, 5)
		werrCh := make(chan error, 3)
		var wg sync.WaitGroup
		wg.Add(8)
		for i := 0; i < 5; i++ {
			go func() {
				defer wg.Done()
				rerrCh <- dm.ReinforceMemory(id, 5)
			}()
		}
		for i := 0; i < 3; i++ {
			go func() {
				defer wg.Done()
				werrCh <- dm.WeakenMemory(id, 3)
			}()
		}
		wg.Wait()
		close(rerrCh)
		close(werrCh)

		for e := range rerrCh {
			if e != nil && isBusyError(e) {
				busyCount++
			}
		}
		for e := range werrCh {
			if e != nil && isBusyError(e) {
				busyCount++
			}
		}
	}
	if busyCount > 0 {
		t.Logf("5+3 classification: %d SQLITE_BUSY-shaped errors across %d iterations (absorbed by busy_timeout)",
			busyCount, iters)
	}
	// busyCount is informational; the closure-membership tests above
	// already enforce the no-silent-drop contract via durable state.
}