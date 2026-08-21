package telemetry

import (
	"context"
	"path/filepath"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "telemetry.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func sampleFrame() Frame {
	id := "inv_01"
	return Frame{
		SchemaVersion: SchemaVersion,
		EventType:     "invocation_completed",
		InvocationID:  id,
		Framework:     "claude-code",
		Provider:      "anthropic",
		Model:         "claude-fable-5",
		StartedAt:     1756000000,
		CompletedAt:   1756000012,
		Status:        "completed",
		InputTokens:   ptrInt64(100),
		OutputTokens:  ptrInt64(50),
		ProviderMetadata: jsonRaw(`{}`),
	}
}

func ptrInt64(v int64) *int64 { return &v }
func jsonRaw(s string) []byte { return []byte(s) }

func TestInsertFrame_InsertsNewRow(t *testing.T) {
	s := newStore(t)
	res, err := s.InsertFrame(context.Background(), sampleFrame())
	if err != nil {
		t.Fatalf("InsertFrame: %v", err)
	}
	if !res.Inserted {
		t.Fatalf("expected Inserted=true, got %+v", res)
	}
}

func TestInsertFrame_IdempotentSamePayload(t *testing.T) {
	s := newStore(t)
	f := sampleFrame()
	if _, err := s.InsertFrame(context.Background(), f); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	res, err := s.InsertFrame(context.Background(), f)
	if err != nil {
		t.Fatalf("second insert: %v", err)
	}
	if res.Inserted {
		t.Errorf("expected Inserted=false on retry, got %+v", res)
	}
	if res.Conflict {
		t.Errorf("expected Conflict=false on identical retry, got %+v", res)
	}
}

func TestInsertFrame_ConflictOnDifferentPayload(t *testing.T) {
	s := newStore(t)
	if _, err := s.InsertFrame(context.Background(), sampleFrame()); err != nil {
		t.Fatalf("first insert: %v", err)
	}
	different := sampleFrame()
	different.InputTokens = ptrInt64(999) // different value, same id
	res, err := s.InsertFrame(context.Background(), different)
	if err != nil {
		t.Fatalf("expected conflict error, got %v", err)
	}
	if !res.Conflict {
		t.Errorf("expected Conflict=true, got %+v", res)
	}
}

func TestInsertFrame_NullTokensStayNull(t *testing.T) {
	s := newStore(t)
	f := sampleFrame()
	f.InputTokens = nil // framework didn't report
	f.CacheReadTokens = nil
	if _, err := s.InsertFrame(context.Background(), f); err != nil {
		t.Fatalf("InsertFrame: %v", err)
	}

	var got *int64
	row := s.DB().QueryRow(`SELECT input_tokens FROM telemetry_invocation WHERE invocation_id=?`, f.InvocationID)
	if err := row.Scan(&got); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got != nil {
		t.Errorf("expected NULL input_tokens, got %d", *got)
	}
}
