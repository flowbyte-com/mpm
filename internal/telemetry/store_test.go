package telemetry

import (
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
