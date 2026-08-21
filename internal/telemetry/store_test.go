package telemetry

import (
	"context"
	"path/filepath"
	"testing"
)

func TestOpenCreatesSchema(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "telemetry.db")

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer s.Close()

	rows, err := s.DB().Query(`
		SELECT name FROM sqlite_master
		WHERE type='table' AND name='telemetry_invocation'
	`)
	if err != nil {
		t.Fatalf("query sqlite_master: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("expected telemetry_invocation table to exist")
	}
	if rows.Next() {
		t.Fatalf("expected exactly one row for telemetry_invocation")
	}
}

func TestOpenAppliesIdempotently(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "telemetry.db")

	s1, err := Open(path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	s1.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second Open (must be idempotent): %v", err)
	}
	s2.Close()
}

func TestQueryInvocation(t *testing.T) {
	s := newStore(t)
	f := sampleFrame()
	if _, err := s.InsertFrame(context.Background(), f); err != nil {
		t.Fatalf("insert: %v", err)
	}
	got, err := s.QueryInvocation(context.Background(), f.InvocationID)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if got.InvocationID != f.InvocationID {
		t.Errorf("got %q, want %q", got.InvocationID, f.InvocationID)
	}
}

func TestQuerySession(t *testing.T) {
	s := newStore(t)
	f := sampleFrame()
	sess := "sess_test"
	f.SessionID = &sess
	if _, err := s.InsertFrame(context.Background(), f); err != nil {
		t.Fatalf("insert: %v", err)
	}
	rows, err := s.QuerySession(context.Background(), sess)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 || rows[0].InvocationID != f.InvocationID {
		t.Errorf("got %d rows, want 1", len(rows))
	}
}

func TestQuerySince(t *testing.T) {
	s := newStore(t)
	f := sampleFrame()
	f.StartedAt = 1000
	f.CompletedAt = 1001
	if _, err := s.InsertFrame(context.Background(), f); err != nil {
		t.Fatalf("insert: %v", err)
	}
	rows, err := s.QuerySince(context.Background(), 500)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 1 {
		t.Errorf("got %d rows, want 1 (cutoff=500)", len(rows))
	}
	rows, err = s.QuerySince(context.Background(), 2000)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(rows) != 0 {
		t.Errorf("got %d rows, want 0 (cutoff=2000)", len(rows))
	}
}
