// cmd/mpm/tool_help_test.go — regression coverage for `mpm help <tool>`.
//
// Pins the W-007 + W-009 contract: the action enum and the top-level
// required-parameter list are extracted from the same JSON Schema the
// runtime dispatcher uses, so they cannot drift.
package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestExtractActionEnum_ValidSchema proves the action enum is read
// from the `properties.action.enum` path of the schema.
func TestExtractActionEnum_ValidSchema(t *testing.T) {
	schema := json.RawMessage(`{
		"type": "object",
		"properties": {
			"action": {
				"type": "string",
				"enum": ["save", "query", "shred"]
			}
		}
	}`)
	got := extractActionEnum(schema)
	want := []string{"save", "query", "shred"}
	if !equalStrings(got, want) {
		t.Errorf("extractActionEnum()=%v, want %v", got, want)
	}
}

// TestExtractActionEnum_MissingAction covers the absence-of-action case
// (some tools might not have an action field at all). Must return nil
// — callers fall back to "no actions declared".
func TestExtractActionEnum_MissingAction(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}}}`)
	if got := extractActionEnum(schema); got != nil {
		t.Errorf("extractActionEnum(missing)=%v, want nil", got)
	}
}

// TestExtractActionEnum_Malformed covers the malformed-schema case.
// Must return nil rather than panicking.
func TestExtractActionEnum_Malformed(t *testing.T) {
	schema := json.RawMessage(`{not valid json`)
	if got := extractActionEnum(schema); got != nil {
		t.Errorf("extractActionEnum(malformed)=%v, want nil", got)
	}
}

// TestExtractRequired verifies the `required` top-level field is
// surfaced as a list of names.
func TestExtractRequired(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"required":["action","params"],
		"properties":{"action":{"type":"string"},"params":{"type":"object"}}
	}`)
	got := extractRequired(schema)
	want := []string{"action", "params"}
	if !equalStrings(got, want) {
		t.Errorf("extractRequired()=%v, want %v", got, want)
	}
}

// TestExtractProperties_MarksRequired proves the Required flag is set
// only on properties named in the top-level required array.
func TestExtractProperties_MarksRequired(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"required":["action"],
		"properties":{
			"action":{"type":"string","description":"verb to invoke"},
			"limit":{"type":"integer","default":10,"description":"max rows"}
		}
	}`)
	props := extractProperties(schema)
	if props == nil {
		t.Fatal("extractProperties() returned nil")
	}
	if !props["action"].Required {
		t.Error("action should be Required=true")
	}
	if props["limit"].Required {
		t.Error("limit should be Required=false")
	}
	if props["action"].Type != "string" {
		t.Errorf("action.Type=%q, want string", props["action"].Type)
	}
	if props["limit"].Type != "integer" {
		t.Errorf("limit.Type=%q, want integer", props["limit"].Type)
	}
	if props["limit"].Default != "10" {
		t.Errorf("limit.Default=%q, want 10", props["limit"].Default)
	}
	if !strings.Contains(props["limit"].Description, "max rows") {
		t.Errorf("limit.Description=%q, missing 'max rows'", props["limit"].Description)
	}
}

// TestExtractProperties_HandlesEnumConstraints proves enums are
// surfaced so `mpm help` can render them inline.
func TestExtractProperties_HandlesEnumConstraints(t *testing.T) {
	schema := json.RawMessage(`{
		"type":"object",
		"properties":{
			"scope":{"type":"string","enum":["local","all","shared"]}
		}
	}`)
	props := extractProperties(schema)
	if got := props["scope"].EnumValues; !equalStrings(got, []string{"local","all","shared"}) {
		t.Errorf("scope.EnumValues=%v, want [local all shared]", got)
	}
}

// TestSchemaType_StringAndArray exercises the JSON Schema allowance
// for `type` to be either a single string or a list. We collapse to
// the first non-null entry.
func TestSchemaType_StringAndArray(t *testing.T) {
	if got := schemaType("string"); got != "string" {
		t.Errorf("schemaType(string)=%q, want string", got)
	}
	if got := schemaType([]interface{}{"string", "null"}); got != "string" {
		t.Errorf("schemaType([string,null])=%q, want string", got)
	}
	if got := schemaType([]interface{}{"null", "integer"}); got != "integer" {
		t.Errorf("schemaType([null,integer])=%q, want integer", got)
	}
	if got := schemaType(nil); got != "any" {
		t.Errorf("schemaType(nil)=%q, want any", got)
	}
}

// TestFormatDefault_EmptyForAbsent proves the empty-string sentinel
// is returned so callers can suppress the field in output.
func TestFormatDefault_EmptyForAbsent(t *testing.T) {
	if got := formatDefault(nil); got != "" {
		t.Errorf("formatDefault(nil)=%q, want empty", got)
	}
	if got := formatDefault(""); got != "" {
		t.Errorf("formatDefault(empty string)=%q, want empty", got)
	}
	if got := formatDefault(float64(10)); got != "10" {
		t.Errorf("formatDefault(10)=%q, want 10", got)
	}
}

// equalStrings is a tiny helper so we don't pull in reflect.DeepEqual
// just for this.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}