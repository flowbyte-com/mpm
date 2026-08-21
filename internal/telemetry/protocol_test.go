package telemetry

import (
	"bytes"
	"strings"
	"testing"
)

func TestEncodeResponse_Accepted(t *testing.T) {
	var buf bytes.Buffer
	if err := EncodeResponse(&buf, Response{Status: StatusAccepted, Inserted: true}); err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	line := strings.TrimSpace(buf.String())
	if !strings.Contains(line, `"status":"ACCEPTED"`) || !strings.Contains(line, `"inserted":true`) {
		t.Errorf("got %q", line)
	}
}

func TestEncodeResponse_Dropped(t *testing.T) {
	var buf bytes.Buffer
	if err := EncodeResponse(&buf, Response{Status: StatusDropped, Reason: "persistence_failed"}); err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	line := strings.TrimSpace(buf.String())
	if !strings.Contains(line, `"status":"DROPPED"`) || !strings.Contains(line, `"persistence_failed"`) {
		t.Errorf("got %q", line)
	}
}

func TestEncodeResponse_Rejected(t *testing.T) {
	var buf bytes.Buffer
	if err := EncodeResponse(&buf, Response{Status: StatusRejected, Reason: "unknown_schema_version"}); err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	line := strings.TrimSpace(buf.String())
	if !strings.Contains(line, `"status":"REJECTED"`) || !strings.Contains(line, `"unknown_schema_version"`) {
		t.Errorf("got %q", line)
	}
}

func TestEncodeResponse_AlwaysEndsWithNewline(t *testing.T) {
	var buf bytes.Buffer
	if err := EncodeResponse(&buf, Response{Status: StatusAccepted}); err != nil {
		t.Fatalf("EncodeResponse: %v", err)
	}
	if !bytes.HasSuffix(buf.Bytes(), []byte("\n")) {
		t.Errorf("response must end with newline, got %q", buf.String())
	}
}

func TestDecodeFrame_GoodLine(t *testing.T) {
	input := "{\"schema_version\":\"v1\",\"event_type\":\"invocation_completed\",\"invocation_id\":\"inv_x\",\"framework\":\"x\",\"provider\":\"y\",\"model\":\"z\",\"started_at\":1,\"completed_at\":2,\"status\":\"completed\"}\n"
	f, err := DecodeFrame(strings.NewReader(input))
	if err != nil {
		t.Fatalf("DecodeFrame: %v", err)
	}
	if f.InvocationID != "inv_x" {
		t.Errorf("InvocationID = %q", f.InvocationID)
	}
}

func TestDecodeFrame_BadJSONReturnsValidationError(t *testing.T) {
	_, err := DecodeFrame(strings.NewReader("not json\n"))
	if err == nil {
		t.Fatalf("expected error")
	}
	if _, ok := AsValidationError(err); !ok {
		t.Errorf("expected *ValidationError, got %T", err)
	}
}

func TestDecodeFrame_BlankLineReturnsEOF(t *testing.T) {
	_, err := DecodeFrame(strings.NewReader("\n\n"))
	if err == nil {
		t.Errorf("expected EOF on blank lines, got nil")
	}
}
