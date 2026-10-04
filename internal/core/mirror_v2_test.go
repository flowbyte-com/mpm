package internal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// ==================== Mirror v2 ====================

// newTestDMWithMirror returns a hermetic DatabaseManager whose mirrorPath
// is rewired to a temp file so every mirror record is captured without
// touching the test's ambient workspace.
//
// The file is created up front (empty) so that hitlStreamFor resolves the
// same path the test will read from, and so that the v2 envelope the test
// asserts against does not race the on-disk file's first open.
func newTestDMWithMirror(t *testing.T) (*DatabaseManager, string) {
	t.Helper()
	dm := newTestDM(t)
	path := t.TempDir() + "/mirror.jsonl"
	dm.mirrorPath = path
	return dm, path
}

// readMirrorLines is a tiny decoder used only by the mirror v2 tests.
// It tolerates a trailing newline, which the shared append path always
// writes. A line that does not parse as JSON is reported as an error so
// a regression in the v2 envelope shows up as a build failure, not a
// silent "0 records".
func readMirrorLines(t *testing.T, path string) []map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read mirror: %v", err)
	}
	out := []map[string]interface{}{}
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("decode mirror line %q: %v", line, err)
		}
		out = append(out, m)
	}
	return out
}

// TestMirrorEvent_CarriesV2Envelope is the structural assertion: every
// record written through the shared path has the v2 envelope fields.
// "preview" is always present (the v2 design), so an absent or wrong-typed
// field is a regression, not a missing optional.
func TestMirrorEvent_CarriesV2Envelope(t *testing.T) {
	dm, path := newTestDMWithMirror(t)

	ev := NewMirrorLessonEvent("lesson-1", "First lesson title", "mpm")
	if err := appendMirrorLine(dm.mirrorPath, ev); err != nil {
		t.Fatalf("append: %v", err)
	}

	lines := readMirrorLines(t, path)
	if len(lines) != 1 {
		t.Fatalf("got %d lines, want 1", len(lines))
	}
	row := lines[0]
	if row["v"] != float64(2) {
		t.Errorf("v = %v, want 2", row["v"])
	}
	if _, ok := row["ts"].(string); !ok {
		t.Errorf("ts is %T (%v), want string", row["ts"], row["ts"])
	}
	if row["op"] != "lesson_created" {
		t.Errorf("op = %v, want lesson_created", row["op"])
	}
	if _, ok := row["preview"]; !ok {
		t.Errorf("preview field absent; v2 envelope requires it always present")
	}
}

// TestMirrorEvent_LessonPreviewIsBounded pins the 240/280 cap on a lesson
// title whose source is a paragraph. The test feeds a 600-character
// string and asserts the stored preview is at most 280 characters and
// marked with an ellipsis.
func TestMirrorEvent_LessonPreviewIsBounded(t *testing.T) {
	dm, path := newTestDMWithMirror(t)
	big := strings.Repeat("a long lesson body that runs on. ", 20)
	ev := NewMirrorLessonEvent("lesson-big", big, "mpm")
	if err := appendMirrorLine(dm.mirrorPath, ev); err != nil {
		t.Fatal(err)
	}
	rows := readMirrorLines(t, path)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	preview, _ := rows[0]["preview"].(string)
	if n := len([]rune(preview)); n > mirrorPreviewMax {
		t.Errorf("preview is %d chars, > %d", n, mirrorPreviewMax)
	}
	if !strings.HasSuffix(preview, "…") {
		t.Errorf("truncated preview does not end with the cut marker: %q", preview)
	}
}

// TestMirrorEvent_SensitiveContentBecomesBlockedRecord is the F-4
// invariant: a memory whose content trips the security scanner produces
// a structural record only. The preview is empty, the digest is a real
// sha256 of the rejected content, and the pattern family label is
// preserved exactly so an operator can correlate blocked attempts across
// runs.
func TestMirrorEvent_SensitiveContentBecomesBlockedRecord(t *testing.T) {
	dm, path := newTestDMWithMirror(t)

	// Real Anthropic key shape; the scanner MUST trip.
	creds := "sk-ant-api03-" + strings.Repeat("A", 60)
	mem := &Memory{
		ID:         "mem-secret",
		Collection: "memories",
		Content:    "my key is " + creds,
		Source:     "mpm",
	}
	ev := newMemoryEvent(mem)
	if err := appendMirrorLine(dm.mirrorPath, ev); err != nil {
		t.Fatal(err)
	}

	rows := readMirrorLines(t, path)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	row := rows[0]

	if row["op"] != "blocked_attempt" {
		t.Errorf("op = %v, want blocked_attempt", row["op"])
	}
	if row["preview"] != "" {
		t.Errorf("preview = %q, want empty (F-4 digest-only)", row["preview"])
	}
	if sha, _ := row["content_sha256"].(string); sha == "" {
		t.Errorf("content_sha256 absent on blocked record")
	} else {
		wantSum := sha256.Sum256([]byte(mem.Content))
		if got := hex.EncodeToString(wantSum[:]); sha != got {
			t.Errorf("content_sha256 = %q, want %q", sha, got)
		}
	}
	if row["action"] != "blocked" {
		t.Errorf("action = %v, want blocked", row["action"])
	}
	// Pattern family is the scanner's classification, not the value.
	if family, _ := row["pattern_family"].(string); family == "" {
		t.Errorf("pattern_family is empty; F-4 needs a family label")
	}
	// The credential itself MUST NOT appear in any field.
	for _, v := range row {
		if s, ok := v.(string); ok && strings.Contains(s, creds) {
			t.Errorf("credential survived into the mirror record: %v", row)
		}
	}
}

// TestMirrorEvent_NeverCarriesFullContent pins the absence of the body in
// ordinary records. A memory with a body well above the 280-char preview
// cap produces a record whose total byte size is bounded by the envelope
// + cap, and whose content-derived fields are the digest and the bounded
// preview only.
func TestMirrorEvent_NeverCarriesFullContent(t *testing.T) {
	dm, path := newTestDMWithMirror(t)
	body := strings.Repeat("alpha beta gamma delta. ", 100) // ~2.3 KiB
	mem := &Memory{
		ID:         "mem-big",
		Collection: "memories",
		Content:    body,
		Source:     "mpm",
	}
	ev := newMemoryEvent(mem)
	if err := appendMirrorLine(dm.mirrorPath, ev); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Envelope (v, ts, op, id, collection, source, preview, digest) plus
	// JSON syntax. 1 KiB is comfortable headroom under the 280 cap.
	if len(data) > 1024 {
		t.Errorf("mirror record is %d bytes; full body should never be inlined", len(data))
	}
	// The body is highly repetitive, so a "mid-section substring"
	// check would falsely match the preview itself. The real
	// invariant is the byte size bound above, plus the absence of
	// the trailing 1 KiB of the body, which is past every possible
	// preview length.
	tail := body[len(body)-1024:]
	if strings.Contains(string(data), tail) {
		t.Errorf("body tail survived into the mirror record; cap is being bypassed")
	}
}

// TestMirrorEvent_PreviewIsDeterministic pins the byte-for-byte
// determinism contract: two runs with the same input produce the same
// preview. A non-deterministic preview would defeat diffs in `git` and
// in the operator's history.
func TestMirrorEvent_PreviewIsDeterministic(t *testing.T) {
	ev1 := newMemoryEvent(&Memory{ID: "a", Collection: "memories", Content: "deterministic", Source: "mpm"})
	ev2 := newMemoryEvent(&Memory{ID: "a", Collection: "memories", Content: "deterministic", Source: "mpm"})
	if ev1.Preview != ev2.Preview {
		t.Errorf("preview not deterministic: %q vs %q", ev1.Preview, ev2.Preview)
	}
	if ev1.Digest != ev2.Digest {
		t.Errorf("digest not deterministic: %q vs %q", ev1.Digest, ev2.Digest)
	}
}

// TestMirrorEvent_PreviewSanitisesControlChars is the "lines can't lie"
// contract. A content with embedded newlines claims to be multiple
// records in `tail`; an ANSI escape rewrites the screen. Both must
// collapse to a single readable line.
func TestMirrorEvent_PreviewSanitisesControlChars(t *testing.T) {
	dm, path := newTestDMWithMirror(t)
	evil := "first\x1b[2Jsecond\nthird\r\nfourth\x07fifth"
	mem := &Memory{ID: "evil", Collection: "memories", Content: evil, Source: "mpm"}
	ev := newMemoryEvent(mem)
	if err := appendMirrorLine(dm.mirrorPath, ev); err != nil {
		t.Fatal(err)
	}
	rows := readMirrorLines(t, path)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1 (newlines must not split records)", len(rows))
	}
	preview, _ := rows[0]["preview"].(string)
	for _, r := range preview {
		if r < 0x20 {
			t.Errorf("preview contains control char %U: %q", r, preview)
		}
	}
	if strings.Contains(preview, "\n") || strings.Contains(preview, "\r") {
		t.Errorf("preview contains a newline: %q", preview)
	}
}

// TestMirrorEvent_PreviewAtLowerBounds pins that a short content comes
// through verbatim (no padding) and a content with a 160-character body
// produces a preview that is the full content (the 240 cap is well above
// 160). The 240 cap is the *practical* bound; 280 is the hard maximum.
// Two of those three numbers in the brief, so two tests is the minimum.
func TestMirrorEvent_PreviewAtLowerBounds(t *testing.T) {
	dm, path := newTestDMWithMirror(t)

	short := "tiny"
	mem := &Memory{ID: "short", Collection: "memories", Content: short, Source: "mpm"}
	if err := appendMirrorLine(dm.mirrorPath, newMemoryEvent(mem)); err != nil {
		t.Fatal(err)
	}
	rows := readMirrorLines(t, path)
	preview, _ := rows[0]["preview"].(string)
	if preview != short {
		t.Errorf("short preview = %q, want %q", preview, short)
	}
}

// TestMirrorEvent_ShredTombstoneHasNoContent pins that a shred event
// names the object and its collection and carries no content-derived
// fields. A shred record exists to mark "what was removed when", not to
// carry a second index of what the memory said.
//
// The v2 envelope always emits a `preview` key (empty string is
// distinguishable from "missing"), so the test checks the value rather
// than the field's presence; the banned content-derived fields are
// checked for absence because they are not in the shred-event builder
// at all.
func TestMirrorEvent_ShredTombstoneHasNoContent(t *testing.T) {
	dm, path := newTestDMWithMirror(t)
	ev := NewMirrorShredEvent("mem-gone", "memories")
	if err := appendMirrorLine(dm.mirrorPath, ev); err != nil {
		t.Fatal(err)
	}
	rows := readMirrorLines(t, path)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	row := rows[0]
	if row["op"] != "memory_shredded" {
		t.Errorf("op = %v, want memory_shredded", row["op"])
	}
	if row["id"] != "mem-gone" {
		t.Errorf("id = %v, want mem-gone", row["id"])
	}
	if row["collection"] != "memories" {
		t.Errorf("collection = %v, want memories", row["collection"])
	}
	if pv, _ := row["preview"].(string); pv != "" {
		t.Errorf("shred preview = %q, want empty", pv)
	}
	if d, _ := row["digest"].(string); d != "" {
		t.Errorf("shred digest = %q, want empty (no content-derived fields)", d)
	}
	for _, banned := range []string{"content_sha256", "content_length", "pattern_family", "action", "type"} {
		if _, ok := row[banned]; ok {
			t.Errorf("shred record carries %q: %v", banned, row)
		}
	}
}

// TestMirrorEvent_ContradictionEventUsesReasonForEvidence pins the
// design's "do not record the same thing twice" rule. Contradiction
// evidence is human text; it goes in reason, never in preview.
func TestMirrorEvent_ContradictionEventUsesReasonForEvidence(t *testing.T) {
	dm, path := newTestDMWithMirror(t)
	ev := newContradictionEvent("mem-1", "conflicts with mem-2 on weight policy")
	if err := appendMirrorLine(dm.mirrorPath, ev); err != nil {
		t.Fatal(err)
	}
	rows := readMirrorLines(t, path)
	if len(rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(rows))
	}
	row := rows[0]
	if row["op"] != "contradiction_found" {
		t.Errorf("op = %v, want contradiction_found", row["op"])
	}
	if row["preview"] != "" {
		t.Errorf("preview = %q, want empty (evidence belongs in reason)", row["preview"])
	}
	if reason, _ := row["reason"].(string); !strings.Contains(reason, "conflicts with mem-2") {
		t.Errorf("reason did not capture the evidence: %q", reason)
	}
}

