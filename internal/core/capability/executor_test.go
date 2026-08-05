package capability

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// =============================================================================
// executor_test.go — EX-1 acceptance tests
//
// These tests pin the Executor.Invoke contract before any real
// driver is wired in (EX-3 adds BwrapDriver; EX-6 adds
// DirectDriver). Every test uses FakeDriver so:
//
//   * No subprocess is spawned — tests are fast and hermetic.
//   * The Executor's policy code is exercised end-to-end without
//     any SQLite dependency.
//   * Source-hash and operator-approval gates are verified
//     against an in-memory Capability struct with a real
//     SHA-256 SourceHash.
//
// The eight tests cover the EX-1 ticket's acceptance list:
//
//   1. Liveness — non-callable states refuse Invoke.
//   2. Source-hash mismatch fractures the capability.
//   3. Operator domain requires metadata.operator_approved_at.
//   4. Operator domain with approval passes through.
//   5. FakeDriver queue exhaustion surfaces as an error.
//   6. SourceLanguage validation rejects unknown languages.
//   7. Limits resolution: request > metadata > default.
//   8. Nil-capability / nil-driver / nil-store guards.
//
// Decoupling note: tests that don't reach the telemetry write
// (1, 2, 3, 5, 6, 8) use newTestStore to construct a Store but
// never seed a Capability row — they short-circuit at an
// earlier policy gate, before the EX-2 telemetry FK fires.
// Tests that DO reach telemetry (4, 7) seed a Capability row
// via seedCapabilityWithHash so the FK succeeds. EX-3 wires
// the Dispatcher; the tests use NewExecutor (single-Driver
// mode) for back-compat, which still routes through the
// SingleDriverDispatcher internally.
// =============================================================================

// buildTestCapability constructs an in-memory Capability row in
// state='active' with execution_domain='sandbox' and a
// correctly-computed SourceHash for the given source string.
// Pure function — no SQLite, no clock, no globals. This keeps
// the Executor unit tests fast and decoupled from the Store
// layer, matching the same stateless discipline used for the
// cascade materializer.
//
// Override any field by mutating the returned struct after the
// call (e.g., cap.State = StateDraft).
//
// Note: this builds ONLY the in-memory struct. Tests that need
// to invoke through Executor.Invoke (and thus trigger the
// telemetry write's FK on capabilities.id) must ALSO seed the
// row in the database via seedCapabilityWithHash. Tests that
// fail at an earlier policy gate (liveness, hash, operator
// approval, language) don't need to seed because the
// short-circuit happens before the telemetry path runs.
func buildTestCapability(id, source string) *Capability {
	sum := sha256.Sum256([]byte(source))
	return &Capability{
		ID:              id,
		Name:            id,
		Purpose:         "test capability",
		SourceCode:      source,
		SourceLanguage:  string(LangBash),
		SourceHash:      hex.EncodeToString(sum[:]),
		State:           StateActive,
		ExecutionDomain: DomainSandbox,
		Metadata:        CapabilityMetadata{},
	}
}

// hashOf returns the SHA-256 hex of s as the Executor would
// compute it. Used by the source-hash-mismatch test to build a
// Capability whose hash does NOT match the source passed to
// Invoke.
func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// TestExecutor_LivenessRequiresActive confirms that Invoke
// refuses a capability in a non-callable state (draft,
// validated, fractured, retired, etc.) with ErrNotLive.
func TestExecutor_LivenessRequiresActive(t *testing.T) {
	store, _, _ := newTestStore(t)
	driver := &FakeDriver{
		Results: []*DriverResult{{ExitCode: 0, Stdout: []byte("ok")}},
	}
	ex := NewExecutor(store, driver, nil)

	cap := buildTestCapability("cap_draft", "echo hi")
	cap.State = StateDraft // non-callable

	_, err := ex.Invoke(context.Background(), InvokeRequest{
		Capability: cap,
		Language:   LangBash,
		SourceCode: "echo hi",
	})
	if !errors.Is(err, ErrNotLive) {
		t.Errorf("expected ErrNotLive for draft state, got: %v", err)
	}
	if len(driver.Calls) != 0 {
		t.Errorf("driver should not be called when liveness fails, got %d calls", len(driver.Calls))
	}
}

// TestExecutor_SourceHashMismatchFractures confirms that a
// hash mismatch returns ErrSourceHashMismatch. The
// FractureCapability side effect is a stub in EX-1 (returns
// nil); the test verifies the error path and that the driver
// is not invoked.
func TestExecutor_SourceHashMismatchFractures(t *testing.T) {
	store, _, _ := newTestStore(t)
	driver := &FakeDriver{
		Results: []*DriverResult{{ExitCode: 0, Stdout: []byte("should not run")}},
	}
	ex := NewExecutor(store, driver, nil)

	// Capability row was built for one source ("original")
	// but Invoke is called with a different source ("tampered").
	// Their SHA-256 hashes differ → tamper detected.
	cap := buildTestCapability("cap_tamper", "original")

	_, err := ex.Invoke(context.Background(), InvokeRequest{
		Capability: cap,
		Language:   LangBash,
		SourceCode: "tampered",
	})
	if !errors.Is(err, ErrSourceHashMismatch) {
		t.Errorf("expected ErrSourceHashMismatch, got: %v", err)
	}
	// Defensive: the SourceHash field really should NOT equal
	// hashOf("tampered") after buildTestCapability("cap_tamper", "original").
	if cap.SourceHash == hashOf("tampered") {
		t.Fatalf("test setup error: hashes should differ but matched: %s", cap.SourceHash)
	}
	if len(driver.Calls) != 0 {
		t.Errorf("driver should not be called when hash mismatches, got %d calls", len(driver.Calls))
	}
}

// TestExecutor_OperatorRequiresApproval confirms that an
// operator-domain capability without metadata.operator_approved_at
// is refused with ErrOperatorNotApproved BEFORE any driver is
// invoked (no subprocess ever spawned).
func TestExecutor_OperatorRequiresApproval(t *testing.T) {
	store, _, _ := newTestStore(t)
	driver := &FakeDriver{
		Results: []*DriverResult{{ExitCode: 0, Stdout: []byte("should not run")}},
	}
	ex := NewExecutor(store, driver, nil)

	cap := buildTestCapability("cap_op", "echo privileged")
	cap.ExecutionDomain = DomainOperator
	// Metadata is empty — operator_approved_at absent.

	_, err := ex.Invoke(context.Background(), InvokeRequest{
		Capability: cap,
		Language:   LangBash,
		SourceCode: "echo privileged",
	})
	if !errors.Is(err, ErrOperatorNotApproved) {
		t.Errorf("expected ErrOperatorNotApproved, got: %v", err)
	}
	if len(driver.Calls) != 0 {
		t.Errorf("driver must not be called when operator approval is missing, got %d calls", len(driver.Calls))
	}
}

// TestExecutor_OperatorApprovedPasses confirms that an
// operator-domain capability WITH metadata.operator_approved_at
// set proceeds to driver dispatch and writes a telemetry row.
func TestExecutor_OperatorApprovedPasses(t *testing.T) {
	store, db, _ := newTestStore(t)
	driver := &FakeDriver{
		Results: []*DriverResult{{ExitCode: 0, Stdout: []byte("ok"), DurationMs: 5}},
	}
	ex := NewExecutor(store, driver, nil)

	src := "echo privileged"
	srcHash := hashOf(src)
	// Seed the row so the FK on capability_invocations succeeds.
	// The Capability struct passed to Invoke must agree on ID,
	// execution_domain, source_hash, and metadata.
	seedCapabilityWithHash(t, db, "cap_op_ok", "cap_op_ok", StateActive,
		src, string(LangBash), srcHash,
		`{"operator_approved_at": 1700000000}`)

	cap := &Capability{
		ID:              "cap_op_ok",
		Name:            "cap_op_ok",
		Purpose:         "test capability",
		SourceCode:      src,
		SourceLanguage:  string(LangBash),
		SourceHash:      srcHash,
		State:           StateActive,
		ExecutionDomain: DomainOperator,
		Metadata:        CapabilityMetadata{"operator_approved_at": int64(1700000000)},
	}

	res, err := ex.Invoke(context.Background(), InvokeRequest{
		Capability: cap,
		Language:   LangBash,
		SourceCode: src,
	})
	if err != nil {
		t.Fatalf("expected success, got: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
	if len(driver.Calls) != 1 {
		t.Errorf("expected 1 driver call, got %d", len(driver.Calls))
	}
}

// TestExecutor_FakeDriverExhausted confirms that a driver
// that runs out of fixtures returns ErrFakeExhausted. This is
// the loud-failure guard — a driver that silently returns zero
// values would mask test bugs.
func TestExecutor_FakeDriverExhausted(t *testing.T) {
	store, _, _ := newTestStore(t)
	driver := &FakeDriver{} // both queues empty
	ex := NewExecutor(store, driver, nil)

	cap := buildTestCapability("cap_exhaust", "echo hi")

	_, err := ex.Invoke(context.Background(), InvokeRequest{
		Capability: cap,
		Language:   LangBash,
		SourceCode: "echo hi",
	})
	if !errors.Is(err, ErrFakeExhausted) {
		t.Errorf("expected ErrFakeExhausted, got: %v", err)
	}
}

// TestExecutor_SourceLanguageValidation confirms that an
// unknown SourceLanguage is rejected at the Executor boundary
// before any policy check runs.
func TestExecutor_SourceLanguageValidation(t *testing.T) {
	store, _, _ := newTestStore(t)
	driver := &FakeDriver{
		Results: []*DriverResult{{ExitCode: 0}},
	}
	ex := NewExecutor(store, driver, nil)

	cap := buildTestCapability("cap_lang", "echo hi")

	_, err := ex.Invoke(context.Background(), InvokeRequest{
		Capability: cap,
		Language:   SourceLanguage("ruby"), // unknown
		SourceCode: "echo hi",
	})
	if err == nil {
		t.Fatal("expected error for unknown language")
	}
	if !strings.Contains(err.Error(), "unknown source language") {
		t.Errorf("expected language validation error, got: %v", err)
	}
	if len(driver.Calls) != 0 {
		t.Errorf("driver must not be called for invalid language, got %d calls", len(driver.Calls))
	}
}

// TestExecutor_LimitsResolutionOrder confirms the three-tier
// precedence: request.Limits > capability.metadata >
// DefaultResourceLimits. Three sub-cases, one assertion each.
//
// Each sub-case seeds its capability row so the EX-2 telemetry
// FK succeeds — the limits-resolution tests reach the
// telemetry write because no earlier gate fails.
func TestExecutor_LimitsResolutionOrder(t *testing.T) {
	t.Run("request overrides metadata", func(t *testing.T) {
		store, db, _ := newTestStore(t)
		driver := &FakeDriver{
			Results: []*DriverResult{{ExitCode: 0}},
		}
		ex := NewExecutor(store, driver, nil)

		cap := buildTestCapability("cap_req", "echo hi")
		cap.Metadata = CapabilityMetadata{"max_runtime_ms": int64(5000)}
		seedCapabilityWithHash(t, db, cap.ID, cap.Name, cap.State,
			cap.SourceCode, cap.SourceLanguage, cap.SourceHash,
			`{"max_runtime_ms": 5000}`)

		_, err := ex.Invoke(context.Background(), InvokeRequest{
			Capability: cap,
			Language:   LangBash,
			SourceCode: "echo hi",
			Limits:     ResourceLimits{MaxRuntimeMs: 1234},
		})
		if err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		if got := driver.Calls[0].Limits.MaxRuntimeMs; got != 1234 {
			t.Errorf("request limit = %d, want 1234 (request should win)", got)
		}
	})

	t.Run("metadata overrides default", func(t *testing.T) {
		store, db, _ := newTestStore(t)
		driver := &FakeDriver{
			Results: []*DriverResult{{ExitCode: 0}},
		}
		ex := NewExecutor(store, driver, nil)

		cap := buildTestCapability("cap_meta", "echo hi")
		cap.Metadata = CapabilityMetadata{"max_runtime_ms": int64(7777)}
		seedCapabilityWithHash(t, db, cap.ID, cap.Name, cap.State,
			cap.SourceCode, cap.SourceLanguage, cap.SourceHash,
			`{"max_runtime_ms": 7777}`)

		_, err := ex.Invoke(context.Background(), InvokeRequest{
			Capability: cap,
			Language:   LangBash,
			SourceCode: "echo hi",
			// no request limits
		})
		if err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		if got := driver.Calls[0].Limits.MaxRuntimeMs; got != 7777 {
			t.Errorf("metadata limit = %d, want 7777 (metadata should override default)", got)
		}
	})

	t.Run("default applied when both absent", func(t *testing.T) {
		store, db, _ := newTestStore(t)
		driver := &FakeDriver{
			Results: []*DriverResult{{ExitCode: 0}},
		}
		ex := NewExecutor(store, driver, nil)

		cap := buildTestCapability("cap_def", "echo hi")
		seedCapabilityWithHash(t, db, cap.ID, cap.Name, cap.State,
			cap.SourceCode, cap.SourceLanguage, cap.SourceHash, "{}")

		_, err := ex.Invoke(context.Background(), InvokeRequest{
			Capability: cap,
			Language:   LangBash,
			SourceCode: "echo hi",
		})
		if err != nil {
			t.Fatalf("Invoke: %v", err)
		}
		want := DefaultResourceLimits().MaxRuntimeMs
		if got := driver.Calls[0].Limits.MaxRuntimeMs; got != want {
			t.Errorf("default limit = %d, want %d", got, want)
		}
	})
}

// TestExecutor_NilCapabilityRejected is a defensive test that
// confirms Invoke returns a clear error when the Capability
// pointer is nil — this is the kind of bug a CLI handler could
// easily introduce if it forgets to look up the row.
func TestExecutor_NilCapabilityRejected(t *testing.T) {
	store, _, _ := newTestStore(t)
	driver := &FakeDriver{}
	ex := NewExecutor(store, driver, nil)

	_, err := ex.Invoke(context.Background(), InvokeRequest{
		Capability: nil,
		Language:   LangBash,
		SourceCode: "echo hi",
	})
	if err == nil {
		t.Fatal("expected error for nil Capability")
	}
	if !strings.Contains(err.Error(), "Capability is nil") {
		t.Errorf("expected nil-capability error, got: %v", err)
	}
}

// TestExecutor_NilDriverPanics documents the NewExecutor
// contract: a nil driver is a programmer error and panics
// rather than producing a silently-broken Executor.
func TestExecutor_NilDriverPanics(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for nil driver")
		}
	}()
	store, _, _ := newTestStore(t)
	NewExecutor(store, nil, nil)
}

// TestExecutor_NewExecutorPanicsOnNilStore confirms the same
// nil-store guard. NewExecutor is a constructor — failing fast
// at construction time beats failing mysteriously at invoke time.
func TestExecutor_NewExecutorPanicsOnNilStore(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Error("expected panic for nil store")
		}
	}()
	NewExecutor(nil, &FakeDriver{}, nil)
}