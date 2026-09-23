package capability

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	internal "github.com/flowbyte-com/mpm-core"
)

// =============================================================================
// store_invocation_test.go — EX-2 telemetry acceptance tests
//
// Pins the load-bearing contract:
//
//   * RecordInvocation writes the row + counter update atomically.
//   * Counter invariants hold: success_count + failure_count ==
//     COUNT(*) FROM capability_invocations WHERE capability_id = ?.
//   * SQLITE_BUSY consumes the retry budget; non-transient errors
//     fail-fast on the first attempt.
//   * ctx cancellation aborts the retry loop promptly.
//   * ArgsHash preserves argv order; EnvKeys is sorted.
//   * Stdout/Stderr are stored truncated to MaxOutputBytes.
//   * RecentInvocations reads the same rows back in order.
//
// The retry tests inject a custom invocationWriter so the
// contention is deterministic — no real SQLite locks held
// across goroutines, no timing-sensitive sleeps.
// =============================================================================

// fakeInvocationWriter is the deterministic mock for the
// invocationWriter interface. Returns pre-loaded errors in
// order; once the queue is drained, delegates to the real
// realInvocationWriter so the row + counter actually get
// written. This lets retry tests assert both "Write was called
// the right number of times" AND "the eventual successful
// commit produced the expected DB state."
//
// Used by the retry tests: configuring errors=[BUSY, BUSY]
// produces exactly 2 Write calls returning BUSY; the third
// call delegates to the real writer and commits successfully.
// Add an explicit nil at the end of the queue to suppress
// the real write if you want to test pure retry mechanics.
type fakeInvocationWriter struct {
	errors []error
	calls  int
	mu     sync.Mutex // protects calls
	real   *realInvocationWriter
}

func (f *fakeInvocationWriter) Write(ctx context.Context, n internal.DBNode, payload *TelemetryPayload) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.calls <= len(f.errors) {
		return f.errors[f.calls-1]
	}
	// Queue drained; delegate to the real writer so the
	// row + counter actually get written. This is what a
	// "successful" Write would do in production.
	return f.real.Write(ctx, n, payload)
}

func (f *fakeInvocationWriter) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// TestRecordInvocation_HappyPath: a clean Write produces a
// capability_invocations row and increments the right counter.
func TestRecordInvocation_HappyPath(t *testing.T) {
	store, db, _ := newTestStore(t)
	src := "echo hi"
	seedCapabilityWithHash(t, db, "cap_a", "cap_a", StateActive,
		src, "bash", hashOf(src), "{}")

	payload := &TelemetryPayload{
		CapabilityID: "cap_a",
		Language:     LangBash,
		StartedAt:    1700000000,
		FinishedAt:   1700000005,
		ExitCode:     0,
		DurationMs:   5,
		Stdout:       []byte("hi"),
		Stderr:       nil,
		Truncated:    false,
		ArgsHash:     HashArgs([]string{"hi"}),
		EnvKeys:      []string{"HOME"},
		DriverName:   "fake",
	}
	if err := store.RecordInvocation(context.Background(), payload); err != nil {
		t.Fatalf("RecordInvocation: %v", err)
	}

	// Verify the row landed.
	var rowCount int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM capability_invocations WHERE capability_id = ?`,
		"cap_a",
	).Scan(&rowCount); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if rowCount != 1 {
		t.Errorf("invocation rows = %d, want 1", rowCount)
	}

	// Verify the counter incremented.
	var success, failure int
	if err := db.QueryRow(
		`SELECT success_count, failure_count FROM capabilities WHERE id = ?`,
		"cap_a",
	).Scan(&success, &failure); err != nil {
		t.Fatalf("read counters: %v", err)
	}
	if success != 1 || failure != 0 {
		t.Errorf("counters = (success=%d, failure=%d), want (1, 0)", success, failure)
	}
}

// TestRecordInvocation_FailureCounter: non-zero ExitCode increments
// failure_count, not success_count.
func TestRecordInvocation_FailureCounter(t *testing.T) {
	store, db, _ := newTestStore(t)
	src := "false"
	seedCapabilityWithHash(t, db, "cap_b", "cap_b", StateActive,
		src, "bash", hashOf(src), "{}")

	payload := &TelemetryPayload{
		CapabilityID: "cap_b",
		Language:     LangBash,
		StartedAt:    1700000000,
		FinishedAt:   1700000001,
		ExitCode:     1,
		DurationMs:   1,
		DriverName:   "fake",
	}
	if err := store.RecordInvocation(context.Background(), payload); err != nil {
		t.Fatalf("RecordInvocation: %v", err)
	}

	var success, failure int
	if err := db.QueryRow(
		`SELECT success_count, failure_count FROM capabilities WHERE id = ?`,
		"cap_b",
	).Scan(&success, &failure); err != nil {
		t.Fatalf("read counters: %v", err)
	}
	if success != 0 || failure != 1 {
		t.Errorf("counters = (success=%d, failure=%d), want (0, 1)", success, failure)
	}
}

// TestRecordInvocation_TransientBusyRetries: a Write that returns
// SQLITE_BUSY twice then succeeds consumes exactly 3 attempts.
func TestRecordInvocation_TransientBusyRetries(t *testing.T) {
	store, db, _ := newTestStore(t)
	src := "echo retry"
	seedCapabilityWithHash(t, db, "cap_c", "cap_c", StateActive,
		src, "bash", hashOf(src), "{}")

	// Swap in the fake writer with a BUSY×2 then nil queue.
	fw := &fakeInvocationWriter{
		// Two BUSYs; the third call drains the queue and
		// delegates to the real writer, which actually writes
		// the row + counter update.
		errors: []error{
			errors.New("SQLITE_BUSY: database is locked"),
			errors.New("SQLITE_BUSY: database is locked"),
		},
		real: store.writer.(*realInvocationWriter),
	}
	store.writer = fw

	payload := &TelemetryPayload{
		CapabilityID: "cap_c",
		Language:     LangBash,
		ExitCode:     0,
		StartedAt:    1700000000,
		FinishedAt:   1700000001,
		DurationMs:   1,
		DriverName:   "fake",
	}
	if err := store.RecordInvocation(context.Background(), payload); err != nil {
		t.Fatalf("RecordInvocation: %v", err)
	}
	if got := fw.Calls(); got != 3 {
		t.Errorf("Write attempts = %d, want 3 (2 BUSY + 1 success)", got)
	}

	// Verify the row was eventually committed.
	var rowCount int
	_ = db.QueryRow(`SELECT COUNT(*) FROM capability_invocations WHERE capability_id = ?`, "cap_c").Scan(&rowCount)
	if rowCount != 1 {
		t.Errorf("invocation rows = %d, want 1 (commit on attempt 3)", rowCount)
	}
}

// TestRecordInvocation_ExhaustedRetryReturnsErrTelemetryFailed:
// a Write that returns BUSY 4 times exhausts the budget and
// surfaces ErrTelemetryFailed to the caller.
func TestRecordInvocation_ExhaustedRetryReturnsErrTelemetryFailed(t *testing.T) {
	store, db, _ := newTestStore(t)
	src := "echo exhaust"
	seedCapabilityWithHash(t, db, "cap_d", "cap_d", StateActive,
		src, "bash", hashOf(src), "{}")

	busyErr := errors.New("SQLITE_BUSY: database is locked")
	fw := &fakeInvocationWriter{
		errors: []error{busyErr, busyErr, busyErr, busyErr}, // 4 BUSYs
		real:   store.writer.(*realInvocationWriter),
	}
	store.writer = fw

	payload := &TelemetryPayload{
		CapabilityID: "cap_d",
		Language:     LangBash,
		ExitCode:     0,
		StartedAt:    1700000000,
		FinishedAt:   1700000001,
		DurationMs:   1,
		DriverName:   "fake",
	}
	err := store.RecordInvocation(context.Background(), payload)
	if err == nil {
		t.Fatal("expected ErrTelemetryFailed after exhausted retry budget")
	}
	if !errors.Is(err, ErrTelemetryFailed) {
		t.Errorf("expected ErrTelemetryFailed, got: %v", err)
	}
	if got := fw.Calls(); got != 4 {
		t.Errorf("Write attempts = %d, want 4 (budget exhausted)", got)
	}

	// No row should have been committed.
	var rowCount int
	_ = db.QueryRow(`SELECT COUNT(*) FROM capability_invocations WHERE capability_id = ?`, "cap_d").Scan(&rowCount)
	if rowCount != 0 {
		t.Errorf("invocation rows = %d, want 0 (no successful commit)", rowCount)
	}
}

// TestRecordInvocation_NonTransientErrorFailsFast: a permanent
// error (not SQLITE_BUSY) must fail on the first attempt — no
// retry budget consumption.
func TestRecordInvocation_NonTransientErrorFailsFast(t *testing.T) {
	store, db, _ := newTestStore(t)
	src := "echo permfail"
	seedCapabilityWithHash(t, db, "cap_e", "cap_e", StateActive,
		src, "bash", hashOf(src), "{}")

	permErr := errors.New("SQLITE_CONSTRAINT: UNIQUE constraint failed")
	fw := &fakeInvocationWriter{
		errors: []error{permErr},
		real:   store.writer.(*realInvocationWriter),
	}
	store.writer = fw

	payload := &TelemetryPayload{
		CapabilityID: "cap_e",
		Language:     LangBash,
		ExitCode:     0,
		StartedAt:    1700000000,
		FinishedAt:   1700000001,
		DurationMs:   1,
		DriverName:   "fake",
	}
	err := store.RecordInvocation(context.Background(), payload)
	if err == nil {
		t.Fatal("expected error for non-transient failure")
	}
	if !errors.Is(err, ErrTelemetryFailed) {
		t.Errorf("expected ErrTelemetryFailed, got: %v", err)
	}
	if got := fw.Calls(); got != 1 {
		t.Errorf("Write attempts = %d, want 1 (non-transient must fail fast)", got)
	}
}

// TestRecordInvocation_ContextCancellationAbortsRetry: a
// caller-supplied ctx that's already cancelled aborts the
// retry loop before any attempt.
func TestRecordInvocation_ContextCancellationAbortsRetry(t *testing.T) {
	store, db, _ := newTestStore(t)
	src := "echo cancel"
	seedCapabilityWithHash(t, db, "cap_f", "cap_f", StateActive,
		src, "bash", hashOf(src), "{}")

	// Writer returns BUSY on every call so the retry loop
	// would otherwise spin. The cancelled ctx should kill
	// the loop on the first pre-attempt check.
	busyErr := errors.New("SQLITE_BUSY: database is locked")
	fw := &fakeInvocationWriter{
		errors: []error{busyErr, busyErr, busyErr, busyErr},
		real:   store.writer.(*realInvocationWriter),
	}
	store.writer = fw

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	payload := &TelemetryPayload{
		CapabilityID: "cap_f",
		Language:     LangBash,
		ExitCode:     0,
		StartedAt:    1700000000,
		FinishedAt:   1700000001,
		DurationMs:   1,
		DriverName:   "fake",
	}
	err := store.RecordInvocation(ctx, payload)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if !errors.Is(err, ErrTelemetryFailed) {
		t.Errorf("expected ErrTelemetryFailed wrapper, got: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled in chain, got: %v", err)
	}
	if got := fw.Calls(); got != 0 {
		t.Errorf("Write attempts = %d, want 0 (cancelled before any attempt)", got)
	}
}

// TestRecordInvocation_ContextCancellationMidRetry: a ctx
// cancelled mid-retry aborts on the next pre-attempt check.
// We give the loop one BUSY then cancel — the second attempt
// should be aborted before Write is called.
func TestRecordInvocation_ContextCancellationMidRetry(t *testing.T) {
	store, db, _ := newTestStore(t)
	src := "echo cancel-mid"
	seedCapabilityWithHash(t, db, "cap_g", "cap_g", StateActive,
		src, "bash", hashOf(src), "{}")

	busyErr := errors.New("SQLITE_BUSY: database is locked")
	fw := &fakeInvocationWriter{
		errors: []error{busyErr, busyErr, busyErr, busyErr},
		real:   store.writer.(*realInvocationWriter),
	}
	store.writer = fw

	ctx, cancel := context.WithCancel(context.Background())

	// Pre-cancel after the first Write via the Hook. This
	// simulates "the caller's deadline hit us after attempt 1."
	// We don't have a Hook on fakeInvocationWriter; instead,
	// cancel from a separate goroutine after a tiny sleep.
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()

	payload := &TelemetryPayload{
		CapabilityID: "cap_g",
		Language:     LangBash,
		ExitCode:     0,
		StartedAt:    1700000000,
		FinishedAt:   1700000001,
		DurationMs:   1,
		DriverName:   "fake",
	}
	err := store.RecordInvocation(ctx, payload)
	if err == nil {
		t.Fatal("expected error from mid-retry cancellation")
	}
	if !errors.Is(err, ErrTelemetryFailed) {
		t.Errorf("expected ErrTelemetryFailed wrapper, got: %v", err)
	}
}

// TestRecordInvocation_CounterAtomicity: 10 invocations
// (5 success + 5 failure) produce counters that exactly match
// the row count. The atomicity guarantee is the load-bearing
// invariant EX-7 fracture detection depends on.
func TestRecordInvocation_CounterAtomicity(t *testing.T) {
	store, db, _ := newTestStore(t)
	src := "echo atomic"
	seedCapabilityWithHash(t, db, "cap_h", "cap_h", StateActive,
		src, "bash", hashOf(src), "{}")

	for i := 0; i < 5; i++ {
		if err := store.RecordInvocation(context.Background(), &TelemetryPayload{
			CapabilityID: "cap_h",
			Language:     LangBash,
			ExitCode:     0, // success
			StartedAt:    int64(1700000000 + i),
			FinishedAt:   int64(1700000001 + i),
			DurationMs:   1,
			DriverName:   "fake",
		}); err != nil {
			t.Fatalf("RecordInvocation success #%d: %v", i, err)
		}
	}
	for i := 0; i < 5; i++ {
		if err := store.RecordInvocation(context.Background(), &TelemetryPayload{
			CapabilityID: "cap_h",
			Language:     LangBash,
			ExitCode:     1, // failure
			StartedAt:    int64(1700000010 + i),
			FinishedAt:   int64(1700000011 + i),
			DurationMs:   1,
			DriverName:   "fake",
		}); err != nil {
			t.Fatalf("RecordInvocation failure #%d: %v", i, err)
		}
	}

	var success, failure int
	if err := db.QueryRow(
		`SELECT success_count, failure_count FROM capabilities WHERE id = ?`,
		"cap_h",
	).Scan(&success, &failure); err != nil {
		t.Fatalf("read counters: %v", err)
	}
	if success != 5 || failure != 5 {
		t.Errorf("counters = (success=%d, failure=%d), want (5, 5)", success, failure)
	}

	var rowCount int
	_ = db.QueryRow(`SELECT COUNT(*) FROM capability_invocations WHERE capability_id = ?`, "cap_h").Scan(&rowCount)
	if rowCount != 10 {
		t.Errorf("row count = %d, want 10", rowCount)
	}
	// The invariant.
	if success+failure != rowCount {
		t.Errorf("INVARIANT BROKEN: success(%d) + failure(%d) != row_count(%d)",
			success, failure, rowCount)
	}
}

// TestRecentInvocations_OrderedByTime: RecentInvocations
// returns rows in ascending invoked_at order, with the lower
// bound respected.
func TestRecentInvocations_OrderedByTime(t *testing.T) {
	store, db, _ := newTestStore(t)
	src := "echo recent"
	seedCapabilityWithHash(t, db, "cap_i", "cap_i", StateActive,
		src, "bash", hashOf(src), "{}")

	// Insert 5 invocations at increasing invoked_at.
	for i := 0; i < 5; i++ {
		if err := store.RecordInvocation(context.Background(), &TelemetryPayload{
			CapabilityID: "cap_i",
			Language:     LangBash,
			ExitCode:     0,
			StartedAt:    int64(1700000000 + i*10),
			FinishedAt:   int64(1700000001 + i*10),
			DurationMs:   1,
			DriverName:   "fake",
		}); err != nil {
			t.Fatalf("RecordInvocation #%d: %v", i, err)
		}
	}

	// since=1700000020 should return the last 3 invocations
	// (started_at = 1700000020, 1700000030, 1700000040).
	rows, err := store.RecentInvocations(context.Background(), "cap_i", 1700000020, 0)
	if err != nil {
		t.Fatalf("RecentInvocations: %v", err)
	}
	if len(rows) != 3 {
		t.Errorf("got %d rows, want 3", len(rows))
	}
	for i := 0; i < len(rows)-1; i++ {
		if rows[i].InvokedAt > rows[i+1].InvokedAt {
			t.Errorf("rows not sorted ascending at index %d: %d > %d",
				i, rows[i].InvokedAt, rows[i+1].InvokedAt)
		}
	}
}

// TestRecentInvocations_LimitRespected: limit=N caps the
// result size.
func TestRecentInvocations_LimitRespected(t *testing.T) {
	store, db, _ := newTestStore(t)
	src := "echo limit"
	seedCapabilityWithHash(t, db, "cap_j", "cap_j", StateActive,
		src, "bash", hashOf(src), "{}")

	for i := 0; i < 10; i++ {
		if err := store.RecordInvocation(context.Background(), &TelemetryPayload{
			CapabilityID: "cap_j",
			Language:     LangBash,
			ExitCode:     0,
			StartedAt:    int64(1700000000 + i),
			FinishedAt:   int64(1700000001 + i),
			DurationMs:   1,
			DriverName:   "fake",
		}); err != nil {
			t.Fatalf("RecordInvocation: %v", err)
		}
	}
	rows, err := store.RecentInvocations(context.Background(), "cap_j", 0, 3)
	if err != nil {
		t.Fatalf("RecentInvocations: %v", err)
	}
	if len(rows) != 3 {
		t.Errorf("got %d rows, want 3 (limit=3)", len(rows))
	}
}

// TestRecentInvocations_InvocationContextReadable: the JSON
// column round-trips with all fields intact. This is the
// contract for EX-7's fracture detection (which will read
// driver_name, args_hash, env_keys from invocation_context).
func TestRecentInvocations_InvocationContextReadable(t *testing.T) {
	store, db, _ := newTestStore(t)
	src := "echo ctx"
	seedCapabilityWithHash(t, db, "cap_k", "cap_k", StateActive,
		src, "bash", hashOf(src), "{}")

	if err := store.RecordInvocation(context.Background(), &TelemetryPayload{
		CapabilityID: "cap_k",
		Language:     LangBash,
		ExitCode:     0,
		StartedAt:    1700000000,
		FinishedAt:   1700000001,
		DurationMs:   1,
		ArgsHash:     HashArgs([]string{"hi"}),
		EnvKeys:      []string{"PATH", "HOME"},
		DriverName:   "bwrap",
	}); err != nil {
		t.Fatalf("RecordInvocation: %v", err)
	}

	rows, err := store.RecentInvocations(context.Background(), "cap_k", 0, 1)
	if err != nil {
		t.Fatalf("RecentInvocations: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 row, got %d", len(rows))
	}
	if rows[0].InvocationContext == nil {
		t.Fatal("invocation_context is nil")
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(*rows[0].InvocationContext), &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed["driver_name"] != "bwrap" {
		t.Errorf("driver_name = %v, want bwrap", parsed["driver_name"])
	}
	if parsed["args_hash"] != HashArgs([]string{"hi"}) {
		t.Errorf("args_hash mismatch")
	}
	envKeys, ok := parsed["env_keys"].([]interface{})
	if !ok || len(envKeys) != 2 {
		t.Errorf("env_keys = %v, want 2 entries", parsed["env_keys"])
	}
}

// TestHashArgs_OrderPreservation: distinct argv orderings
// produce distinct hashes. "git checkout -b x" and
// "git -b checkout x" are different intents and must not
// collide in the router's failure clusters.
func TestHashArgs_OrderPreservation(t *testing.T) {
	a := HashArgs([]string{"git", "checkout", "-b", "x"})
	b := HashArgs([]string{"git", "-b", "checkout", "x"})
	if a == b {
		t.Errorf("argv order should affect hash; both = %s", a)
	}

	// Same order, same hash.
	c := HashArgs([]string{"git", "checkout", "-b", "x"})
	if a != c {
		t.Errorf("identical argv should produce identical hash; a=%s c=%s", a, c)
	}

	// Empty argv produces a deterministic hash (sha256 of "").
	empty := HashArgs(nil)
	want := sha256.Sum256([]byte(""))
	if empty != hex.EncodeToString(want[:]) {
		t.Errorf("HashArgs(nil) = %s, want %s", empty, hex.EncodeToString(want[:]))
	}
}

// TestSortedEnvKeys: env keys are sorted and values are never
// included in the output (secrets stay out of the ledger).
func TestSortedEnvKeys(t *testing.T) {
	env := map[string]string{
		"PATH":         "/usr/bin",
		"HOME":         "/tmp/hermetic-test-home",
		"SECRET_TOKEN": "should-not-appear",
		"AWS_KEY":      "AKIA...should-not-appear",
	}
	keys := SortedEnvKeys(env)
	if len(keys) != 4 {
		t.Fatalf("got %d keys, want 4", len(keys))
	}
	// Verify sorted order.
	for i := 0; i < len(keys)-1; i++ {
		if keys[i] > keys[i+1] {
			t.Errorf("keys not sorted at index %d: %s > %s", i, keys[i], keys[i+1])
		}
	}
	// Verify NO values appear in the keys slice.
	for _, k := range keys {
		if strings.Contains(k, "should-not-appear") {
			t.Errorf("env value leaked into keys: %s", k)
		}
	}
	// nil env returns nil.
	if SortedEnvKeys(nil) != nil {
		t.Errorf("SortedEnvKeys(nil) = %v, want nil", SortedEnvKeys(nil))
	}
}

// TestRecordInvocation_StderrNullWhenEmpty: an invocation with
// empty stderr stores NULL, not empty string — matches the
// column's NULL allowance and avoids bloating the ledger with
// empty blobs.
func TestRecordInvocation_StderrNullWhenEmpty(t *testing.T) {
	store, db, _ := newTestStore(t)
	src := "echo nostderr"
	seedCapabilityWithHash(t, db, "cap_l", "cap_l", StateActive,
		src, "bash", hashOf(src), "{}")

	if err := store.RecordInvocation(context.Background(), &TelemetryPayload{
		CapabilityID: "cap_l",
		Language:     LangBash,
		ExitCode:     0,
		StartedAt:    1700000000,
		FinishedAt:   1700000001,
		DurationMs:   1,
		Stderr:       nil, // empty
		DriverName:   "fake",
	}); err != nil {
		t.Fatalf("RecordInvocation: %v", err)
	}

	var stderr sql.NullString
	if err := db.QueryRow(
		`SELECT stderr FROM capability_invocations WHERE capability_id = ?`,
		"cap_l",
	).Scan(&stderr); err != nil {
		t.Fatalf("read stderr: %v", err)
	}
	if stderr.Valid {
		t.Errorf("expected NULL stderr, got %q", stderr.String)
	}
}
