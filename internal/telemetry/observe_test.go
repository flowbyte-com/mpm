package telemetry

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestHunt_FlagsHighTokenZeroArtifact(t *testing.T) {
	s := newStore(t)
	sess := "sess_high"
	for i := 0; i < 3; i++ {
		f := sampleFrame()
		f.InvocationID = "inv_" + string(rune('a'+i))
		f.SessionID = &sess
		f.InputTokens = ptrInt64(40000)
		f.OutputTokens = ptrInt64(10000)
		if _, err := s.InsertFrame(context.Background(), f); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	countFn := func(_ context.Context, id string) (int, error) {
		if id != sess {
			t.Errorf("unexpected session_id: %s", id)
		}
		return 0, nil // zero artifacts
	}

	findings, err := Hunt(context.Background(), s, HuntConfig{
		Since: 0, HighTokenThreshold: 100000, MinInvocations: 1,
	}, countFn)
	if err != nil {
		t.Fatalf("hunt: %v", err)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].SessionID != sess {
		t.Errorf("SessionID = %q", findings[0].SessionID)
	}
	if !strings.Contains(findings[0].Reason, "high-token-no-artifact") {
		t.Errorf("Reason = %q", findings[0].Reason)
	}
}

func TestHunt_SkipsSessionWithArtifacts(t *testing.T) {
	s := newStore(t)
	sess := "sess_low"
	f := sampleFrame()
	f.SessionID = &sess
	f.InputTokens = ptrInt64(200000)
	f.OutputTokens = ptrInt64(0)
	if _, err := s.InsertFrame(context.Background(), f); err != nil {
		t.Fatalf("insert: %v", err)
	}
	countFn := func(_ context.Context, id string) (int, error) { return 5, nil }
	findings, err := Hunt(context.Background(), s, HuntConfig{
		Since: 0, HighTokenThreshold: 100000, MinInvocations: 1,
	}, countFn)
	if err != nil {
		t.Fatalf("hunt: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("expected 0 findings (session has artifacts), got %d", len(findings))
	}
}

func TestHunt_SkipsSessionBelowThreshold(t *testing.T) {
	s := newStore(t)
	sess := "sess_small"
	f := sampleFrame()
	f.SessionID = &sess
	f.InputTokens = ptrInt64(1000)
	f.OutputTokens = ptrInt64(500)
	if _, err := s.InsertFrame(context.Background(), f); err != nil {
		t.Fatalf("insert: %v", err)
	}
	countFn := func(_ context.Context, id string) (int, error) { return 0, nil }
	findings, err := Hunt(context.Background(), s, HuntConfig{
		Since: 0, HighTokenThreshold: 100000, MinInvocations: 1,
	}, countFn)
	if err != nil {
		t.Fatalf("hunt: %v", err)
	}
	if len(findings) != 0 {
		t.Errorf("expected 0 findings (below threshold), got %d", len(findings))
	}
}

func TestHunt_PropagatesArtifactCountError(t *testing.T) {
	s := newStore(t)
	sess := "sess_err"
	f := sampleFrame()
	f.SessionID = &sess
	f.InputTokens = ptrInt64(200000)
	if _, err := s.InsertFrame(context.Background(), f); err != nil {
		t.Fatalf("insert: %v", err)
	}
	countFn := func(_ context.Context, id string) (int, error) {
		return 0, fmt.Errorf("mpm call failed")
	}
	_, err := Hunt(context.Background(), s, HuntConfig{
		Since: 0, HighTokenThreshold: 100000, MinInvocations: 1,
	}, countFn)
	if err == nil {
		t.Fatalf("expected error from countFn")
	}
}

// Verify newStore and sampleFrame are accessible (reused from persist_test.go).
var _ = filepath.Join
