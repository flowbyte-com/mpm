// parse.go — argument-parsing helpers for the JSON payload boundary.
//
// All three surfaces — `mpm call <tool> --payload '{...}'`, the MCP
// server, and tests — deserialize into `map[string]interface{}` before
// calling DM methods. These helpers bridge that weakly-typed payload to
// strongly-typed Go values without scattering `if s, ok := v.(string)`
// guards through every tool handler.
//
// The exported `Parse*Or` variants are what cmd/mpm/call.go and
// cmd/mpm-mcp/tools.go use (those packages can't see the unexported
// canonical implementations). The lowercase versions are used by
// internal DM methods directly. Behavior is identical.
package internal

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// parseFloatDefault coerces v to float64, falling back to def on type
// mismatch. JSON unmarshaling gives us float64 for numbers, so the
// float64 case is the common one; the int and string cases are for
// callers that pass those shapes (some MCP clients do).
func parseFloatDefault(v interface{}, def float64) float64 {
	if v == nil {
		return def
	}
	switch n := v.(type) {
	case float64:
		return n
	case int:
		return float64(n)
	case string:
		if f, err := strconv.ParseFloat(n, 64); err == nil {
			return f
		}
	}
	return def
}

// parseStringDefault coerces v to string, falling back to def. Only
// string payloads return as-is; anything else returns def.
func parseStringDefault(v interface{}, def string) string {
	if v == nil {
		return def
	}
	if s, ok := v.(string); ok {
		return s
	}
	return def
}

// parseStringSliceAny accepts the payload shapes agents emit:
//   - `[]interface{}` of strings (the JSON-native array form)
//   - `[]string` (the Go-native form — an in-process caller, or a client
//     that built the payload in Go rather than from JSON)
//   - a single comma-separated string (a compact form some clients use)
//
// Returns nil on any other shape or empty input — callers that need a
// non-nil slice should default locally.
//
// D7: the `[]string` case was missing. A Go-native slice fell through to
// the nil return, so every identifier a caller supplied in that shape
// disappeared without an error. A proposal could be created without the
// citations its author declared. The native form is a COMPLETE
// representation of the parameter, exactly like the JSON-decoded array
// form, so it is accepted and copied — copied, because the caller keeps
// ownership of the slice it handed over.
//
// nil stays nil (an absent parameter and an explicitly nil slice mean the
// same thing) and an empty slice stays an empty slice (like the JSON `[]`
// form), so the two representations agree element for element.
func parseStringSliceAny(v interface{}) []string {
	if v == nil {
		return nil
	}
	if arr, ok := v.([]string); ok {
		if arr == nil {
			return nil
		}
		out := make([]string, len(arr))
		copy(out, arr)
		return out
	}
	if arr, ok := v.([]interface{}); ok {
		out := make([]string, 0, len(arr))
		for _, x := range arr {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	if s, ok := v.(string); ok && s != "" {
		// Defensive decode: if the MCP framework coerced an array
		// argument to a JSON-encoded string (because the tool schema
		// declared tags as string when the caller sent an array),
		// we get here with s like `["a","b","c"]`. Splitting that on
		// commas produces `["a"`, `"b"`, `"c"]` — three strings with
		// escaped quotes that get re-marshaled into the stored JSON.
		// Try to decode as a JSON array first; fall back to the
		// legacy comma-split if that fails.
		var arr []string
		if err := json.Unmarshal([]byte(s), &arr); err == nil && len(arr) > 0 {
			return arr
		}
		return strings.Split(s, ",")
	}
	return nil
}

// ParseStringSliceStrict parses a string-list parameter as a WHOLE.
//
// It accepts every shape ParseStringSliceOr accepts — a Go-native
// `[]string`, a JSON-decoded array, a JSON-encoded array string, a
// comma-separated string — and returns an error in exactly the cases where
// the lenient form would SILENTLY LOSE an element.
//
// The difference is the point. A list that is accepted in part is a list
// the caller did not ask for: the caller believes every identifier it
// supplied is recorded, and a partial parse records fewer. For a CITATION
// list that means a proposal resting on less than its author declared — a
// data-integrity failure, not a convenience issue.
//
// The error is RETURNED, not logged and not swallowed: the caller decides
// whether the field is optional. This is the explicitly designed stricter
// API; the lenient ParseStringSliceOr keeps its fallback semantics for the
// callers that want them.
//
// A malformed ELEMENT is an error, not a drop. A non-array, non-string
// value is an error too — the lenient form would read it as "no value at
// all", which is how a caller's citations vanish.
func ParseStringSliceStrict(field string, v interface{}) ([]string, error) {
	switch t := v.(type) {
	case nil:
		return []string{}, nil
	case []string:
		out := make([]string, len(t))
		copy(out, t)
		return out, nil
	case []interface{}:
		out := make([]string, 0, len(t))
		for i, x := range t {
			s, ok := x.(string)
			if !ok {
				return nil, NewPayloadShapeError(PayloadShapeArray, field,
					fmt.Sprintf("%s[%d] must be a string, got %T", field, i, x))
			}
			out = append(out, s)
		}
		return out, nil
	case string:
		if t == "" {
			return []string{}, nil
		}
		// A JSON-encoded array (the shape an MCP framework produces when
		// it coerces an array argument to a string) and the compact
		// comma-separated form are both WHOLE parses: no element is
		// lost, so both are accepted. An empty JSON array is an empty
		// list, not the literal one-element list ["[]"].
		var arr []string
		if err := json.Unmarshal([]byte(t), &arr); err == nil {
			return arr, nil
		}
		return strings.Split(t, ","), nil
	}
	return nil, NewPayloadShapeError(PayloadShapeArray, field,
		fmt.Sprintf("%s must be a string array (or a comma-separated string), got %T", field, v))
}

// ParseStringSliceArrayOnly parses a string-list parameter under the
// SCHEMA contract: the parameter is an array of strings, or absent.
//
// It differs from ParseStringSliceStrict in one respect — a scalar (a
// comma-separated string, a number) is REJECTED rather than interpreted.
// That is the right contract for a declared array field: a scalar is a
// caller mistake worth reporting, not a shape to guess at.
//
// The absent case is an empty list, and a non-string ELEMENT is an error
// rather than a drop. The error strings are the ones the boundary already
// used, unchanged.
func ParseStringSliceArrayOnly(field string, v interface{}) ([]string, error) {
	if v == nil {
		return []string{}, nil
	}
	switch t := v.(type) {
	case []string:
		out := make([]string, len(t))
		copy(out, t)
		return out, nil
	case []interface{}:
		out := make([]string, 0, len(t))
		for i, x := range t {
			s, ok := x.(string)
			if !ok {
				return nil, NewPayloadShapeError(PayloadShapeArray, field,
					fmt.Sprintf("%s[%d] must be a string, got %T", field, i, x))
			}
			out = append(out, s)
		}
		return out, nil
	}
	return nil, NewPayloadShapeError(PayloadShapeArray, field,
		fmt.Sprintf("%s must be an array of strings, got %T", field, v))
}

// ── Exported aliases (cmd/mpm and cmd/mpm-mcp packages can't see
// unexported helpers — these wrappers exist only to bridge). ──

func ParseStringOr(v interface{}, def string) string {
	return parseStringDefault(v, def)
}

func ParseStringSliceOr(v interface{}) []string {
	return parseStringSliceAny(v)
}

func ParseFloatOr(v interface{}, def float64) float64 {
	return parseFloatDefault(v, def)
}

// ParseBoolOr coerces a JSON-decoded payload value to bool. Accepts
// native bool, string ("true"/"false"/"1"/"0"/"yes"/"no" case-insensitive),
// and numeric (non-zero = true). Falls back to def when the value is
// absent or of an unrecognised type.
func ParseBoolOr(v interface{}, def bool) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "true", "1", "yes", "y", "on":
			return true
		case "false", "0", "no", "n", "off", "":
			return false
		}
	case float64:
		return x != 0
	case int:
		return x != 0
	case int64:
		return x != 0
	}
	return def
}

// ── Path utilities — duplicated from the standard library to keep
// this package's import set tight (no `path/filepath` dep). ──

// filepathExt returns the extension of p, including the leading dot,
// or "" if there is none. Matches filepath.Ext's behavior except for
// separator handling (we only handle "/").
func filepathExt(p string) string {
	for i := len(p) - 1; i >= 0 && p[i] != '/'; i-- {
		if p[i] == '.' {
			return p[i:]
		}
	}
	return ""
}

// filepathBase returns the last element of p. Matches filepath.Base's
// behavior except for separator handling (we only handle "/").
func filepathBase(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[i+1:]
		}
	}
	return p
}

// ── Duration parsing for TTL strings on memory writes. ──

// parseDurationString accepts "24h", "30m", "0", or Go duration syntax.
// "0" returns (0, nil) meaning "no expiry". Empty input is an error.
func parseDurationString(s string) (time.Duration, error) {
	if s == "" {
		return 0, errEmptyDuration
	}
	if s == "0" {
		return 0, nil
	}
	return time.ParseDuration(s)
}

var errEmptyDuration = &durationError{msg: "empty duration"}

type durationError struct{ msg string }

func (e *durationError) Error() string { return e.msg }
