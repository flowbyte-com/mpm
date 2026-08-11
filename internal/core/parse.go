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

// parseStringSliceAny accepts the two payload shapes agents emit:
//   - `[]interface{}` of strings (the JSON-native array form)
//   - a single comma-separated string (a compact form some clients use)
//
// Returns nil on any other shape or empty input — callers that need a
// non-nil slice should default locally.
func parseStringSliceAny(v interface{}) []string {
	if v == nil {
		return nil
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