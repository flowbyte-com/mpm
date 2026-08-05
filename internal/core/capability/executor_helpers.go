package capability

// =============================================================================
// executor_helpers.go — Store method hooks the Executor depends on
//
// FractureCapability is invoked by Executor.Invoke when a
// source_hash mismatch is detected at runtime. EX-3 keeps this
// as a no-op (the source-hash mismatch path still returns
// ErrSourceHashMismatch to the caller, but doesn't transition
// the row to 'fractured' yet); EX-7 fills in the real
// transition + §2.4.1 wake emission.
//
// The EX-1 stub `storeInsertInvocation` was removed in EX-3 —
// the Executor now calls Store.RecordInvocation directly, which
// lives in store_invocation.go. Keeping a no-op stub here would
// be misleading (a future maintainer reading this file would
// wonder which write path the Executor actually uses).
// =============================================================================

// FractureCapability is the EX-3 stub for the EX-7 fracture
// transition. Returns nil so the Executor's source_hash
// mismatch handler can call it without an error path. EX-7
// replaces this with the real transition (state → 'fractured',
// insert EventFracture row, emit wake).
//
// The method is on *Store (rather than a free function) so the
// EX-7 implementation can call back into the Store's
// transaction helpers without an import cycle. The nil-store
// guard preserves the EX-1 test convenience of being able to
// pass nil for non-telemetry code paths.
func (s *Store) FractureCapability(id, reason string) error {
	if s == nil {
		return nil
	}
	// EX-7: BEGIN; SELECT current state; refuse if not in a
	// callable state; UPDATE capabilities SET state='fractured';
	// INSERT INTO capability_events (...EventFracture...); emit
	// wake; COMMIT.
	_ = id
	_ = reason
	return nil
}