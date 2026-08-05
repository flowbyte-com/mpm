package capability

import (
	"encoding/json"
	"testing"
)

func TestCapabilityMetadata_ScanValue_RoundTrip(t *testing.T) {
	src := CapabilityMetadata{
		"state_history": []interface{}{},
		"policy_snapshot": map[string]interface{}{
			"probation_required_success_count": float64(5),
			"probation_max_failure_rate":       0.10,
		},
		"rationale": "test rationale",
	}

	val, err := src.Value()
	if err != nil {
		t.Fatalf("Value() error: %v", err)
	}
	if val == nil {
		t.Fatal("Value() returned nil for non-empty metadata")
	}
	raw, ok := val.([]byte)
	if !ok {
		t.Fatalf("Value() returned %T, want []byte", val)
	}

	var dst CapabilityMetadata
	if err := dst.Scan(raw); err != nil {
		t.Fatalf("Scan([]byte) error: %v", err)
	}
	if len(dst) != len(src) {
		t.Errorf("round-trip lost keys: got %d, want %d", len(dst), len(src))
	}
	if dst["rationale"] != "test rationale" {
		t.Errorf("round-trip lost string value: got %v, want %q", dst["rationale"], "test rationale")
	}
}

func TestCapabilityMetadata_ScanAcceptsString(t *testing.T) {
	// mattn/go-sqlite3 returns []byte from Scan, but manual scan paths
	// and tests sometimes pass strings. Scanner must accept both.
	raw, _ := json.Marshal(CapabilityMetadata{"k": "v"})
	var m CapabilityMetadata
	if err := m.Scan(string(raw)); err != nil {
		t.Errorf("Scan(string) error: %v", err)
	}
	if m["k"] != "v" {
		t.Errorf("Scan(string) lost data: got %v", m)
	}
}

func TestCapabilityMetadata_ScanNil(t *testing.T) {
	var m CapabilityMetadata
	if err := m.Scan(nil); err != nil {
		t.Errorf("Scan(nil) returned error: %v", err)
	}
	if m != nil {
		t.Errorf("Scan(nil) set metadata to %v, want nil", m)
	}
}

func TestCapabilityMetadata_ScanEmpty(t *testing.T) {
	var m CapabilityMetadata
	// Empty bytes — common when the DB column is an empty TEXT.
	if err := m.Scan([]byte{}); err != nil {
		t.Errorf("Scan([]byte{}) error: %v", err)
	}
	if m != nil {
		t.Errorf("Scan([]byte{}) set metadata to %v, want nil", m)
	}
	// Empty string — same.
	if err := m.Scan(""); err != nil {
		t.Errorf("Scan(\"\") error: %v", err)
	}
	if m != nil {
		t.Errorf("Scan(\"\") set metadata to %v, want nil", m)
	}
}

func TestCapabilityMetadata_ScanRejectsUnsupportedType(t *testing.T) {
	var m CapabilityMetadata
	if err := m.Scan(42); err == nil {
		t.Error("Scan(int) accepted unsupported type; expected error")
	}
	if err := m.Scan(struct{}{}); err == nil {
		t.Error("Scan(struct) accepted unsupported type; expected error")
	}
}

func TestCapabilityMetadata_ValueEmptyReturnsBraces(t *testing.T) {
	// Empty metadata marshals to `{}` (not NULL) so the column's
	// NOT NULL DEFAULT '{}' constraint is satisfied.
	m := CapabilityMetadata{}
	val, err := m.Value()
	if err != nil {
		t.Fatalf("Value() error: %v", err)
	}
	raw, ok := val.([]byte)
	if !ok {
		t.Fatalf("Value() returned %T, want []byte", val)
	}
	if string(raw) != "{}" {
		t.Errorf("empty metadata Value() = %q, want %q", raw, "{}")
	}
}

func TestStringSlice_ScanValue_RoundTrip(t *testing.T) {
	src := StringSlice{"git", "vcs", "status"}

	val, err := src.Value()
	if err != nil {
		t.Fatalf("Value() error: %v", err)
	}
	raw, ok := val.([]byte)
	if !ok {
		t.Fatalf("Value() returned %T, want []byte", val)
	}

	var dst StringSlice
	if err := dst.Scan(raw); err != nil {
		t.Fatalf("Scan([]byte) error: %v", err)
	}
	if len(dst) != len(src) {
		t.Errorf("round-trip lost items: got %d, want %d", len(dst), len(src))
	}
	for i, v := range src {
		if dst[i] != v {
			t.Errorf("round-trip item %d: got %q, want %q", i, dst[i], v)
		}
	}
}

func TestStringSlice_ScanAcceptsString(t *testing.T) {
	raw, _ := json.Marshal(StringSlice{"a", "b"})
	var s StringSlice
	if err := s.Scan(string(raw)); err != nil {
		t.Errorf("Scan(string) error: %v", err)
	}
	if len(s) != 2 || s[0] != "a" || s[1] != "b" {
		t.Errorf("Scan(string) lost data: got %v", s)
	}
}

func TestStringSlice_ScanNil(t *testing.T) {
	var s StringSlice
	if err := s.Scan(nil); err != nil {
		t.Errorf("Scan(nil) returned error: %v", err)
	}
	if s != nil {
		t.Errorf("Scan(nil) set slice to %v, want nil", s)
	}
}

func TestStringSlice_ScanEmpty(t *testing.T) {
	var s StringSlice
	if err := s.Scan([]byte{}); err != nil {
		t.Errorf("Scan([]byte{}) error: %v", err)
	}
	if s != nil {
		t.Errorf("Scan([]byte{}) set slice to %v, want nil", s)
	}
}

func TestStringSlice_ValueEmptyReturnsJSONArray(t *testing.T) {
	// Empty slice marshals to `[]` (not NULL) so the column's
	// NOT NULL DEFAULT '[]' constraint is satisfied.
	s := StringSlice{}
	val, err := s.Value()
	if err != nil {
		t.Fatalf("Value() error: %v", err)
	}
	raw, ok := val.([]byte)
	if !ok {
		t.Fatalf("Value() returned %T, want []byte", val)
	}
	if string(raw) != "[]" {
		t.Errorf("empty slice Value() = %q, want %q", raw, "[]")
	}
}

func TestEventMetadata_ScanValue_RoundTrip(t *testing.T) {
	src := EventMetadata{
		"tolerance_breached": "success_rate_delta",
		"predecessor_id":     "cap_orig_v1",
		"cascade_depth":      float64(1),
	}

	val, err := src.Value()
	if err != nil {
		t.Fatalf("Value() error: %v", err)
	}
	if val == nil {
		t.Fatal("Value() returned nil for non-empty event metadata")
	}

	var dst EventMetadata
	if err := dst.Scan(val); err != nil {
		t.Fatalf("Scan() error: %v", err)
	}
	if len(dst) != len(src) {
		t.Errorf("round-trip lost keys: got %d, want %d", len(dst), len(src))
	}
	if dst["tolerance_breached"] != "success_rate_delta" {
		t.Errorf("round-trip lost string: got %v", dst["tolerance_breached"])
	}
}

func TestEventMetadata_ScanNil(t *testing.T) {
	var m EventMetadata
	if err := m.Scan(nil); err != nil {
		t.Errorf("Scan(nil) returned error: %v", err)
	}
	if m != nil {
		t.Errorf("Scan(nil) set metadata to %v, want nil", m)
	}
}
