// wake_tools_test.go — tests for the wake notification formatter and
// the directive_id extraction helper. Pins the contract:
//
//   1. Empty input → empty output (preserves the legacy "no wakes
//      due, no XML block" behavior).
//   2. Each wake entry includes id (truncated), reason (max 80
//      chars), and overdue_secs.
//   3. When the wake's metadata has a non-empty directive_id, the
//      rendered line includes it (2026-09-01 ergonomics
//      improvement).
//   4. When the wake's metadata has no directive_id (or no
//      metadata at all), the rendered line does NOT include the
//      field — preserves the legacy contract for non-cron wakes.
//   5. directive_id tolerates both the parsed-map shape (the
//      shape wakeRowToMap produces) and the raw-string shape
//      (for forward-compat with callers passing a raw row map).
package internal

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestFormatWakeNotification_EmptyReturnsEmpty(t *testing.T) {
	require.Equal(t, "", FormatWakeNotification(nil))
	require.Equal(t, "", FormatWakeNotification([]map[string]interface{}{}))
}

func TestFormatWakeNotification_IncludesBasicFields(t *testing.T) {
	wakes := []map[string]interface{}{
		{
			"id":           "wk-abcdef123456",
			"reason":       "cron:epistemic-compaction",
			"overdue_secs": int64(3600),
		},
	}
	out := FormatWakeNotification(wakes)
	require.Contains(t, out, "<system_wake_notification>")
	require.Contains(t, out, "id=wk-abcde") // truncated to 8
	require.Contains(t, out, `reason="cron:epistemic-compaction"`)
	require.Contains(t, out, "overdue_secs=3600")
	require.Contains(t, out, "</system_wake_notification>")
}

func TestFormatWakeNotification_IncludesDirectiveIDFromMap(t *testing.T) {
	wakes := []map[string]interface{}{
		{
			"id":           "wk-12345678",
			"reason":       "cron:epistemic-compaction",
			"overdue_secs": int64(0),
			"metadata": map[string]interface{}{
				"kind":         "cron",
				"source":       "cron",
				"task_id":      "epistemic-compaction",
				"directive_id": "mpm-seed-epistemic-compaction-policy",
			},
		},
	}
	out := FormatWakeNotification(wakes)
	require.Contains(t, out, `directive_id="mpm-seed-epistemic-compaction-policy"`,
		"wake with parsed-map metadata must surface directive_id, got %q", out)
}

func TestFormatWakeNotification_IncludesDirectiveIDFromString(t *testing.T) {
	// Forward-compat: a raw-string metadata payload (older
	// callers that hand a JSON-encoded string) must also work.
	wakes := []map[string]interface{}{
		{
			"id":           "wk-87654321",
			"reason":       "cron:epistemic-compaction",
			"overdue_secs": int64(0),
			"metadata":     `{"kind":"cron","directive_id":"mpm-seed-epistemic-compaction-policy"}`,
		},
	}
	out := FormatWakeNotification(wakes)
	require.Contains(t, out, `directive_id="mpm-seed-epistemic-compaction-policy"`,
		"wake with raw-string metadata must surface directive_id, got %q", out)
}

func TestFormatWakeNotification_OmitsDirectiveIDWhenAbsent(t *testing.T) {
	// Legacy shape: no metadata at all. The line must NOT include
	// a directive_id field — the wake-triage-policy directive
	// expects to parse the older id|reason|overdue_secs shape.
	wakes := []map[string]interface{}{
		{
			"id":           "wk-notif0001",
			"reason":       "span-wc-theory",
			"overdue_secs": int64(100),
		},
	}
	out := FormatWakeNotification(wakes)
	require.NotContains(t, out, "directive_id",
		"wake without metadata must not render directive_id, got %q", out)
	require.Contains(t, out, "id=wk-notif")
}

func TestFormatWakeNotification_OmitsDirectiveIDWhenEmptyString(t *testing.T) {
	// Edge case: metadata present but directive_id is empty
	// string. Same omit-when-empty contract — the LLM triage
	// path treats a literal "directive_id=" as confusing.
	wakes := []map[string]interface{}{
		{
			"id":           "wk-empty0001",
			"reason":       "custom",
			"overdue_secs": int64(0),
			"metadata": map[string]interface{}{
				"kind":         "notification",
				"directive_id": "",
			},
		},
	}
	out := FormatWakeNotification(wakes)
	require.NotContains(t, out, "directive_id=",
		"wake with empty-string directive_id must not render the field, got %q", out)
}

func TestFormatWakeNotification_MultipleWakes(t *testing.T) {
	wakes := []map[string]interface{}{
		{
			"id":           "wk-cron00001",
			"reason":       "cron:epistemic-compaction",
			"overdue_secs": int64(0),
			"metadata":     map[string]interface{}{"directive_id": "mpm-seed-epistemic-compaction-policy"},
		},
		{
			"id":           "wk-casc00001",
			"reason":       "cascade:foundation",
			"overdue_secs": int64(500),
		},
	}
	out := FormatWakeNotification(wakes)
	require.True(t, strings.Contains(out, "directive_id=\"mpm-seed-epistemic-compaction-policy\""),
		"cron wake must surface directive_id, got %q", out)
	require.True(t, strings.Contains(out, `reason="cascade:foundation"`),
		"cascade wake must surface reason, got %q", out)
	// Cascade wake has no metadata, so no directive_id field for
	// the second line — but the field appears once (on the cron
	// wake). Count occurrences to verify.
	require.Equal(t, 1, strings.Count(out, "directive_id="),
		"exactly one directive_id field expected (one per cron wake), got %d in %q",
		strings.Count(out, "directive_id="), out)
}
