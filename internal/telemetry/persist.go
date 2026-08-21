// internal/telemetry/persist.go — InsertFrame with idempotency + conflict detection.
//
// Idempotent retry: same invocation_id + same payload bytes => no-op,
// Inserted=false, Conflict=false. Per spec invariant 4 + §3.
//
// Conflicting duplicate: same invocation_id + different payload bytes =>
// rejected, Conflict=true. Per spec change #6 — we do NOT silently
// overwrite; an adapter that emits the same id with different bytes
// is buggy or replaying.
//
// NULL vs 0: token fields use sql.NullInt64 so NULL (not reported)
// stays distinct from 0 (reported as zero) in the SQL row.

package telemetry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type PersistResult struct {
	Inserted bool
	Conflict bool
}

// InsertFrame persists f. Idempotent for identical retries; rejects
// conflicting duplicates. Returns nil error on idempotent no-op and
// on conflict — the result struct distinguishes the two cases.
func (s *Store) InsertFrame(ctx context.Context, f Frame) (PersistResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return PersistResult{}, fmt.Errorf("begin tx: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	existing, err := loadFrameByID(ctx, tx, f.InvocationID)
	if err == nil {
		if framesEqual(f, existing) {
			return PersistResult{Inserted: false, Conflict: false}, nil
		}
		return PersistResult{Inserted: false, Conflict: true}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return PersistResult{}, fmt.Errorf("existence check: %w", err)
	}

	if _, err := tx.ExecContext(ctx, insertSQL, frameArgs(f, receivedAtUnixFn())...); err != nil {
		return PersistResult{}, fmt.Errorf("insert frame: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return PersistResult{}, fmt.Errorf("commit: %w", err)
	}
	committed = true
	return PersistResult{Inserted: true}, nil
}

const insertSQL = `INSERT INTO telemetry_invocation (
  invocation_id, parent_invocation_id, session_id,
  framework, framework_version,
  provider, model, model_revision,
  started_at, completed_at, received_at,
  status, stop_reason,
  input_tokens, output_tokens,
  cache_read_tokens, cache_write_tokens, reasoning_tokens,
  duration_ms,
  provider_metadata, schema_version
) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`

func frameArgs(f Frame, receivedAt int64) []any {
	meta := string(f.ProviderMetadata)
	if meta == "" {
		meta = "{}"
	}
	return []any{
		f.InvocationID,
		nullableString(f.ParentInvocationID),
		nullableString(f.SessionID),
		f.Framework,
		nullableString(f.FrameworkVersion),
		f.Provider,
		f.Model,
		nullableString(f.ModelRevision),
		f.StartedAt,
		f.CompletedAt,
		receivedAt,
		f.Status,
		nullableString(f.StopReason),
		nullableInt64(f.InputTokens),
		nullableInt64(f.OutputTokens),
		nullableInt64(f.CacheReadTokens),
		nullableInt64(f.CacheWriteTokens),
		nullableInt64(f.ReasoningTokens),
		nullableInt64(f.DurationMS),
		meta,
		f.SchemaVersion,
	}
}

// receivedAtUnixFn is replaced in tests with a deterministic value.
var receivedAtUnixFn = func() int64 { return time.Now().Unix() }

func nullableString(s *string) any {
	if s == nil || *s == "" {
		return nil
	}
	return *s
}

func nullableInt64(n *int64) any {
	if n == nil {
		return nil
	}
	return *n
}

func loadFrameByID(ctx context.Context, tx *sql.Tx, id string) (Frame, error) {
	var f Frame
	var meta string
	err := tx.QueryRowContext(ctx, `SELECT
		invocation_id, parent_invocation_id, session_id,
		framework, framework_version,
		provider, model, model_revision,
		started_at, completed_at,
		status, stop_reason,
		input_tokens, output_tokens,
		cache_read_tokens, cache_write_tokens, reasoning_tokens,
		duration_ms,
		provider_metadata, schema_version
	FROM telemetry_invocation WHERE invocation_id = ?`, id,
	).Scan(
		&f.InvocationID,
		&f.ParentInvocationID, &f.SessionID,
		&f.Framework, &f.FrameworkVersion,
		&f.Provider, &f.Model, &f.ModelRevision,
		&f.StartedAt, &f.CompletedAt,
		&f.Status, &f.StopReason,
		&f.InputTokens, &f.OutputTokens,
		&f.CacheReadTokens, &f.CacheWriteTokens, &f.ReasoningTokens,
		&f.DurationMS,
		&meta, &f.SchemaVersion,
	)
	if err != nil {
		return Frame{}, err
	}
	f.ProviderMetadata = []byte(meta)
	f.EventType = "invocation_completed"
	return f, nil
}

func framesEqual(a, b Frame) bool {
	if a.InvocationID != b.InvocationID ||
		a.Framework != b.Framework || a.Provider != b.Provider || a.Model != b.Model ||
		a.StartedAt != b.StartedAt || a.CompletedAt != b.CompletedAt ||
		a.Status != b.Status {
		return false
	}
	if !stringPtrEq(a.ParentInvocationID, b.ParentInvocationID) ||
		!stringPtrEq(a.SessionID, b.SessionID) ||
		!stringPtrEq(a.FrameworkVersion, b.FrameworkVersion) ||
		!stringPtrEq(a.ModelRevision, b.ModelRevision) ||
		!stringPtrEq(a.StopReason, b.StopReason) {
		return false
	}
	if !int64PtrEq(a.InputTokens, b.InputTokens) ||
		!int64PtrEq(a.OutputTokens, b.OutputTokens) ||
		!int64PtrEq(a.CacheReadTokens, b.CacheReadTokens) ||
		!int64PtrEq(a.CacheWriteTokens, b.CacheWriteTokens) ||
		!int64PtrEq(a.ReasoningTokens, b.ReasoningTokens) ||
		!int64PtrEq(a.DurationMS, b.DurationMS) {
		return false
	}
	if string(a.ProviderMetadata) != string(b.ProviderMetadata) {
		return false
	}
	return true
}

func stringPtrEq(a, b *string) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func int64PtrEq(a, b *int64) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}
