package telemetry

import (
	"strings"
	"testing"
)

const goodFrame = `{
  "schema_version": "v1",
  "event_type": "invocation_completed",
  "invocation_id": "inv_01",
  "parent_invocation_id": null,
  "session_id": "sess_01",
  "framework": "claude-code",
  "framework_version": "1.2.3",
  "provider": "anthropic",
  "model": "claude-fable-5",
  "model_revision": null,
  "started_at": 1756000000,
  "completed_at": 1756000012,
  "status": "completed",
  "stop_reason": "end_turn",
  "input_tokens": 18234,
  "output_tokens": 4123,
  "cache_read_tokens": null,
  "cache_write_tokens": 1200,
  "reasoning_tokens": 3200,
  "duration_ms": 12000,
  "provider_metadata": {"request_id": "req_1"}
}`

func TestParseFrame_AcceptsGoodFrame(t *testing.T) {
	f, err := ParseFrame([]byte(goodFrame))
	if err != nil {
		t.Fatalf("good frame rejected: %v", err)
	}
	if f.InvocationID != "inv_01" {
		t.Errorf("InvocationID = %q, want %q", f.InvocationID, "inv_01")
	}
	if f.CacheReadTokens != nil {
		t.Errorf("CacheReadTokens = %v, want nil", *f.CacheReadTokens)
	}
}

func TestParseFrame_RejectsUnknownSchemaVersion(t *testing.T) {
	raw := strings.Replace(goodFrame, `"schema_version": "v1"`, `"schema_version": "v999"`, 1)
	_, err := ParseFrame([]byte(raw))
	if err == nil {
		t.Fatalf("expected schema_version rejection")
	}
	if !strings.Contains(err.Error(), "schema_version") {
		t.Errorf("error does not mention schema_version: %v", err)
	}
}

func TestParseFrame_RejectsMissingInvocationID(t *testing.T) {
	raw := strings.Replace(goodFrame, `"invocation_id": "inv_01"`, `"invocation_id": ""`, 1)
	_, err := ParseFrame([]byte(raw))
	if err == nil {
		t.Fatalf("expected empty invocation_id rejection")
	}
}

func TestParseFrame_RejectsCompletedBeforeStarted(t *testing.T) {
	raw := strings.Replace(goodFrame, `"completed_at": 1756000012`, `"completed_at": 1755999999`, 1)
	_, err := ParseFrame([]byte(raw))
	if err == nil {
		t.Fatalf("expected completed_at < started_at rejection")
	}
}

func TestParseFrame_RejectsInvalidStatus(t *testing.T) {
	raw := strings.Replace(goodFrame, `"status": "completed"`, `"status": "weird_status"`, 1)
	_, err := ParseFrame([]byte(raw))
	if err == nil {
		t.Fatalf("expected invalid status rejection")
	}
}

func TestParseFrame_NullTokensStayNull(t *testing.T) {
	f, err := ParseFrame([]byte(goodFrame))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if f.CacheReadTokens != nil {
		t.Errorf("expected nil CacheReadTokens, got %d", *f.CacheReadTokens)
	}
	if f.CacheWriteTokens == nil || *f.CacheWriteTokens != 1200 {
		t.Errorf("expected CacheWriteTokens=1200, got %v", f.CacheWriteTokens)
	}
}
