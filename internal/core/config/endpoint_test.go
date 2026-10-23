// endpoint_test.go — endpoint URL validation.
//
// 2026-10-23 (HAL-9001): a clean install saved an embedding endpoint
// typed as "http:://localhost:11434". Nothing rejected it at the
// moment it was entered, so the configuration file carried a value
// that could never produce a request.
package config

import (
	"strings"
	"testing"
)

// TestValidateEndpointURL_AcceptsUsableEndpoints pins the values an
// operator legitimately types.
func TestValidateEndpointURL_AcceptsUsableEndpoints(t *testing.T) {
	for _, raw := range []string{
		"http://127.0.0.1:11434",
		"https://api.openai.com/v1",
		"https://api.minimax.io/anthropic/v1",
		"http://localhost:1234/v1",
		"https://example.com",
		"  http://127.0.0.1:11434  ",
		"HTTPS://example.com/v1",
	} {
		if err := ValidateEndpointURL(raw); err != nil {
			t.Errorf("ValidateEndpointURL(%q) = %v, want nil", raw, err)
		}
	}
}

// TestValidateEndpointURL_RejectsMalformedEndpoints pins the values
// that must never reach the configuration file.
func TestValidateEndpointURL_RejectsMalformedEndpoints(t *testing.T) {
	for _, raw := range []string{
		"",
		"   ",
		"http:://127.0.0.1:11434", // the doubled-colon typo
		"htt://127.0.0.1:11434",   // wrong scheme
		"://127.0.0.1:11434",      // no scheme
		"127.0.0.1:11434",         // no scheme; "127.0.0.1" parses as a scheme
		"localhost:11434",         // likewise
		"http://",                 // no host
		"ftp://example.com",       // not an HTTP endpoint
		"file:///etc/passwd",
	} {
		if err := ValidateEndpointURL(raw); err == nil {
			t.Errorf("ValidateEndpointURL(%q) = nil, want an error", raw)
		}
	}
}

// TestValidateEndpointURL_ErrorNamesTheValue pins that the message
// quotes the offending value, so the operator can see which of several
// endpoints was refused.
func TestValidateEndpointURL_ErrorNamesTheValue(t *testing.T) {
	err := ValidateEndpointURL("http:://127.0.0.1:11434")
	if err == nil {
		t.Fatal("ValidateEndpointURL returned nil for a malformed URL")
	}
	if !strings.Contains(err.Error(), "http:://127.0.0.1:11434") {
		t.Errorf("error %q does not name the offending value", err.Error())
	}
}
