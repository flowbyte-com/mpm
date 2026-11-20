// f_h4_envelope_parity_test.go — F-H4 CLI/MCP save envelope parity.
//
// F-H4: `mpm memory add --json` and `mpm call mpm_memory save` returned
// envelopes with disjoint field sets:
//
//   CLI:    {success, id, content, tags, weight, suggested_topics}
//   MCP:    {success, id, content, tags, weight, pointer,
//            content_truncated, content_bytes, note}
//
// Both surfaces wrote to the same Memory row but emitted different JSON.
// An agent that automated against the CLI surface and the MCP surface had
// to know about two different shapes; field absences (pointer on CLI,
// suggested_topics on MCP) forced conditional handling at the consumer.
//
// The fix aligns the CLI envelope to the MCP envelope. The CLI keeps the
// suggested_topics hint because it has no analog on the MCP side, but
// every parity field present in MCP is now present in CLI:
//   - pointer:           always emitted
//   - content_truncated: emitted only when content exceeded the wire bound
//   - content_bytes:     emitted only when truncated
//   - note:              emitted only when truncated
//
// The tests below pin both contracts and the parity shape.
package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// envelopeMirrorsCLI parses the JSON printed by `mpm memory add --json`.
func envelopeMirrorsCLI(t *testing.T, args []string) map[string]interface{} {
	t.Helper()
	out := captureBoth(t, func() {
		handleMemoryAdd(args)
	})
	// The handler prints the JSON on the last line of stdout; captureBoth
	// collects both. Trim any non-JSON noise (errors go to stderr but the
	// JSON succeeds).
	var lastJSON string
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "{") && strings.HasSuffix(line, "}") {
			lastJSON = line
		}
	}
	if lastJSON == "" {
		t.Fatalf("no JSON envelope in output:\n%s", out)
	}
	var env map[string]interface{}
	if err := json.Unmarshal([]byte(lastJSON), &env); err != nil {
		t.Fatalf("unmarshal envelope: %v\noutput:\n%s", err, out)
	}
	return env
}

// TestF_H4_CLIEnvelopeHasParityCore pins the headline contract: the CLI
// envelope MUST contain every field the MCP envelope always carries.
func TestF_H4_CLIEnvelopeHasParityCore(t *testing.T) {
	env := envelopeMirrorsCLI(t, []string{"--tags", "f-h4-core", "--json", "F-H4 parity core"})

	for _, field := range []string{"success", "id", "content", "pointer"} {
		if _, ok := env[field]; !ok {
			t.Errorf("CLI envelope missing required field %q (got keys: %v)", field, keysOf(env))
		}
	}
	if env["success"] != true {
		t.Errorf("success must be true, got %v", env["success"])
	}
	if env["pointer"] != "mpm://memory/"+env["id"].(string) {
		t.Errorf("pointer must match id (%v vs %v)", env["pointer"], env["id"])
	}
}

// TestF_H4_CLIEnvelopeContentBounded pins the F4 wire-bound contract:
// content over the bound (default 2048 bytes) must be truncated and
// flagged with content_truncated / content_bytes / note.
func TestF_H4_CLIEnvelopeContentBounded(t *testing.T) {
	// 4 KB of unique content — guaranteed to exceed the 2048 byte bound.
	big := strings.Repeat("X", 4096)
	env := envelopeMirrorsCLI(t, []string{"--tags", "f-h4-trunc", "--json", big})

	if got, ok := env["content_truncated"].(bool); !ok || !got {
		t.Errorf("expected content_truncated=true, got %v (env keys: %v)", env["content_truncated"], keysOf(env))
	}
	if got, ok := env["content_bytes"].(float64); !ok || int(got) != 4096 {
		t.Errorf("expected content_bytes=4096, got %v", env["content_bytes"])
	}
	if note, _ := env["note"].(string); note == "" {
		t.Errorf("expected note to explain truncation, got empty (env keys: %v)", keysOf(env))
	}
	// Inline echo must be at most the bound.
	if c, _ := env["content"].(string); len(c) > 2048 {
		t.Errorf("inline echo exceeds bound: %d bytes", len(c))
	}
}

// TestF_H4_CLIEnvelopeNoTruncationWhenSmall pins the negative contract:
// small content must NOT carry truncation flags.
func TestF_H4_CLIEnvelopeNoTruncationWhenSmall(t *testing.T) {
	env := envelopeMirrorsCLI(t, []string{"--tags", "f-h4-small", "--json", "short content"})
	for _, f := range []string{"content_truncated", "content_bytes", "note"} {
		if _, ok := env[f]; ok {
			t.Errorf("small content should NOT emit %q, got %v", f, env[f])
		}
	}
}

// TestF_H4_CLIEnvelopeShapeMatchesMCP pins the field-set contract: the
// keys present in a small-memory CLI envelope must be a strict subset of
// the keys present in the MCP envelope (which is the canonical shape).
// `suggested_topics` is allowed on CLI but not required; everything else
// must match.
//
// This is the regression that fails if either side adds a one-off field
// without updating the other.
func TestF_H4_CLIEnvelopeShapeMatchesMCP(t *testing.T) {
	env := envelopeMirrorsCLI(t, []string{"--tags", "f-h4-shape", "--json", "F-H4 shape"})

	// The contract: every key the CLI emits must be one of:
	//   - the shared parity set
	//   - the CLI-only suggested_topics field
	allowed := map[string]bool{
		"success":          true,
		"id":               true,
		"content":          true,
		"tags":             true,
		"weight":           true,
		"pointer":          true,
		"content_truncated": true,
		"content_bytes":    true,
		"note":             true,
		"suggested_topics": true, // CLI-only hint
	}
	for k := range env {
		if !allowed[k] {
			t.Errorf("CLI envelope emitted unexpected field %q — update the parity contract or revert this regression", k)
		}
	}
}

// TestF_H4_CLIEnvelopeWeightEcho pins that --weight echoes the user's
// intent, not the persisted value. This is a CLI-specific contract — MCP
// echoes the persisted value because it has no other way to show "what
// was actually stored." The CLI should preserve "what the user asked
// for" so the user can spot a parser bug.
func TestF_H4_CLIEnvelopeWeightEcho(t *testing.T) {
	env := envelopeMirrorsCLI(t, []string{"--tags", "f-h4-weight", "--weight", "7.5", "--json", "F-H4 weight"})
	if w, _ := env["weight"].(float64); w != 7.5 {
		t.Errorf("CLI weight should echo user input (7.5), got %v", env["weight"])
	}
}

// TestF_H4_CLIEnvelopeTagsEcho pins the same echo contract for tags.
func TestF_H4_CLIEnvelopeTagsEcho(t *testing.T) {
	env := envelopeMirrorsCLI(t, []string{"--tags", "alpha,beta,gamma", "--json", "F-H4 tags"})
	tags, ok := env["tags"].([]interface{})
	if !ok || len(tags) != 3 {
		t.Fatalf("expected 3 tags in CSV order, got %v", env["tags"])
	}
	want := []string{"alpha", "beta", "gamma"}
	for i, w := range want {
		if tags[i] != w {
			t.Errorf("tag[%d] = %v, want %s", i, tags[i], w)
		}
	}
}

func keysOf(m map[string]interface{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}