// internal/telemetry/frame.go — wire-format Frame struct + validation.
//
// Mirrors the v1 schema in docs/archive/2026-08-21-telemetry-binary-design.md §3.

package telemetry

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Frame is the decoded NDJSON payload sent by adapters per LLM invocation.
// Pointer fields are nullable in the wire format; nil means the framework
// / provider did not report the field. 0 means it reported zero. These
// two cases are analytically distinct and must remain so on disk.
type Frame struct {
	SchemaVersion      string  `json:"schema_version"`
	EventType          string  `json:"event_type"`
	InvocationID       string  `json:"invocation_id"`
	ParentInvocationID *string `json:"parent_invocation_id"`
	SessionID          *string `json:"session_id"`
	Framework          string  `json:"framework"`
	FrameworkVersion   *string `json:"framework_version"`
	Provider           string  `json:"provider"`
	Model              string  `json:"model"`
	ModelRevision      *string `json:"model_revision"`
	StartedAt          int64   `json:"started_at"`
	CompletedAt        int64   `json:"completed_at"`
	Status             string  `json:"status"`
	StopReason         *string `json:"stop_reason"`

	InputTokens       *int64 `json:"input_tokens"`
	OutputTokens      *int64 `json:"output_tokens"`
	CacheReadTokens   *int64 `json:"cache_read_tokens"`
	CacheWriteTokens  *int64 `json:"cache_write_tokens"`
	ReasoningTokens   *int64 `json:"reasoning_tokens"`
	DurationMS        *int64 `json:"duration_ms"`

	ProviderMetadata json.RawMessage `json:"provider_metadata"`
}

// ValidationError is returned by ParseFrame on any rejected frame.
// Reason is a short string suitable for an ACCEPTED/DROPPED/REJECTED
// response body and for log lines.
type ValidationError struct {
	Reason string
}

func (e *ValidationError) Error() string { return e.Reason }

var allowedStatus = map[string]bool{
	"completed":  true,
	"failed":     true,
	"cancelled":  true,
	"timed_out":  true,
}

func ParseFrame(raw []byte) (Frame, error) {
	// Ping frames carry only {"event_type":"ping"} and skip all schema validation.
	// Extract event_type from raw JSON without full unmarshal to decide early.
	var pingCheck struct {
		EventType string `json:"event_type"`
	}
	if err := json.Unmarshal(raw, &pingCheck); err != nil {
		return Frame{}, &ValidationError{Reason: fmt.Sprintf("malformed_json: %v", err)}
	}
	if pingCheck.EventType == "ping" {
		var f Frame
		_ = json.Unmarshal(raw, &f) // populate whatever fields are present
		return f, nil
	}

	var f Frame
	if err := json.Unmarshal(raw, &f); err != nil {
		return Frame{}, &ValidationError{Reason: fmt.Sprintf("malformed_json: %v", err)}
	}
	if f.SchemaVersion != SchemaVersion {
		return Frame{}, &ValidationError{Reason: fmt.Sprintf("unknown_schema_version: %q", f.SchemaVersion)}
	}
	if f.EventType != "invocation_completed" {
		return Frame{}, &ValidationError{Reason: fmt.Sprintf("unsupported_event_type: %q", f.EventType)}
	}
	if f.InvocationID == "" {
		return Frame{}, &ValidationError{Reason: "invocation_id_required"}
	}
	if f.Framework == "" {
		return Frame{}, &ValidationError{Reason: "framework_required"}
	}
	if f.Provider == "" {
		return Frame{}, &ValidationError{Reason: "provider_required"}
	}
	if f.Model == "" {
		return Frame{}, &ValidationError{Reason: "model_required"}
	}
	if !allowedStatus[f.Status] {
		return Frame{}, &ValidationError{Reason: fmt.Sprintf("invalid_status: %q", f.Status)}
	}
	if f.CompletedAt < f.StartedAt {
		return Frame{}, &ValidationError{Reason: "completed_before_started"}
	}
	if len(f.ProviderMetadata) > 0 && !json.Valid(f.ProviderMetadata) {
		return Frame{}, &ValidationError{Reason: "provider_metadata_invalid_json"}
	}
	if len(f.ProviderMetadata) == 0 {
		// Accept missing or null provider_metadata; default to {} on persist.
		f.ProviderMetadata = json.RawMessage(`{}`)
	}
	return f, nil
}

// AsValidationError extracts *ValidationError from err, if any. Returns
// ("", false) for nil errors and non-validation errors.
func AsValidationError(err error) (*ValidationError, bool) {
	if err == nil {
		return nil, false
	}
	var ve *ValidationError
	if errors.As(err, &ve) {
		return ve, true
	}
	return nil, false
}
