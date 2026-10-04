// mirror_v2.go — the v2 mirror record: what MPM wrote, in a form a human
// can read from a terminal.
//
// v1 wrote the whole Memory object — id, content, metadata, tags, the
// embedding vector — on nearly every line. Measured over 88 days of
// history, ~98% of mirror bytes were full cognitive content or embeddings
// (design §5.3). That is ballast by the HITL test: nobody reading a
// 3 KB line with a 300-float array learns something a 200-byte line with
// a bounded preview and an id would not have told them, and every one of
// those ids is already re-queryable from mpm.db.
//
// v2 replaces the snapshot with an event:
//
//	{"v":2,"ts":"…","op":"memory_created","id":"mem-…",
//	 "collection":"memories","source":"claude_code",
//	 "preview":"Prefers Postgres advisory locks for singleton scheduler",
//	 "digest":"sha256:9f2c…"}
//
// What is deliberately absent is the point: no embedding, no full content,
// no metadata dump, no numeric churn in weight/importance/confidence. A
// field earns its place only if someone reading the line benefits.
package internal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// mirrorFormatVersion is the `v` field on every record this module writes.
// Records without a `v` are v1 history and are never rewritten.
const mirrorFormatVersion = 2

// The v2 mirror operation vocabulary. Bounded on purpose: a journal whose
// operation names are an open set is a log, not a journal, and every new
// name is a new thing a reader has to learn.
//
// Events that would exist only because the underlying rows exist are not
// here. Per-tick reinforcement and confidence transitions, every
// work_events row, and per-save revisions of an unchanged memory are
// telemetry candidates, not journal entries.
const (
	mirrorOpMemoryCreated         = "memory_created"
	mirrorOpMemoryRevised         = "memory_revised"
	mirrorOpMemorySuperseded      = "memory_superseded"
	mirrorOpMemoryReinforced      = "memory_reinforced"
	mirrorOpMemoryWeakened        = "memory_weakened"
	mirrorOpDecisionCreated       = "decision_created"
	mirrorOpDecisionChanged       = "decision_changed"
	mirrorOpTheoryCreated         = "theory_created"
	mirrorOpTheoryChanged         = "theory_changed"
	mirrorOpLessonCreated         = "lesson_created"
	mirrorOpContradictionFound    = "contradiction_found"
	mirrorOpContradictionResolved = "contradiction_resolved"
	mirrorOpWorkLifecycle         = "work_lifecycle"
	mirrorOpDestructiveOperation  = "destructive_operation"
	mirrorOpBlockedAttempt        = "blocked_attempt"
	mirrorOpEmbeddingFailure      = "embedding_failure"
	mirrorOpMemoryShredded        = "memory_shredded"
)

// mirrorOps is the allow-list of operations a v2 record may carry. Callers
// that build an event from a dynamic string must go through
// isKnownMirrorOp, so a new code path cannot smuggle an unbounded name into
// the journal.
var mirrorOps = map[string]bool{
	mirrorOpMemoryCreated:         true,
	mirrorOpMemoryRevised:         true,
	mirrorOpMemorySuperseded:      true,
	mirrorOpMemoryReinforced:      true,
	mirrorOpMemoryWeakened:        true,
	mirrorOpDecisionCreated:       true,
	mirrorOpDecisionChanged:       true,
	mirrorOpTheoryCreated:         true,
	mirrorOpTheoryChanged:         true,
	mirrorOpLessonCreated:         true,
	mirrorOpContradictionFound:    true,
	mirrorOpContradictionResolved: true,
	mirrorOpWorkLifecycle:         true,
	mirrorOpDestructiveOperation:  true,
	mirrorOpBlockedAttempt:        true,
	mirrorOpEmbeddingFailure:      true,
	mirrorOpMemoryShredded:        true,
}

func isKnownMirrorOp(op string) bool { return mirrorOps[op] }

// mirrorCollectionOps maps a mirrored collection to the event that records
// its creation. Collections with no special meaning — knowledge, directives,
// changelog, project notes — are all ordinary `memory_created` events; the
// collection field is what distinguishes them.
var mirrorCollectionOps = map[string]string{
	"decisions": mirrorOpDecisionCreated,
	"theories":  mirrorOpTheoryCreated,
	"lessons":   mirrorOpLessonCreated,
}

func mirrorOpForCollection(collection string) string {
	if op, ok := mirrorCollectionOps[collection]; ok {
		return op
	}
	return mirrorOpMemoryCreated
}

// mirrorEvent is one v2 mirror record.
//
// The field set is split in two. The first block is the common envelope
// every record carries. The second block exists only for the blocked
// representation, which is governed by the F-4 security invariant and must
// keep its exact shape: a digest, a pattern family, a reason and a length —
// and never a byte of the rejected content.
type mirrorEvent struct {
	// V is the format version. Always 2 for records written here; a
	// missing `v` on disk means v1 history.
	V int `json:"v"`
	// Ts is RFC3339 UTC.
	Ts string `json:"ts"`
	// Op is a value from the mirrorOps vocabulary.
	Op string `json:"op"`
	// ID is the artifact id. Absent for events with no object behind them
	// (a blocked attempt has no id by construction).
	ID string `json:"id,omitempty"`
	// Collection is the collection or table the event belongs to.
	Collection string `json:"collection,omitempty"`
	// Source is the framework or caller that produced the event.
	Source string `json:"source,omitempty"`
	// Preview is the deterministic, bounded, secret-scanned human hint.
	// Always present (empty string renders as "preview":"") so a reader
	// can distinguish "no preview" from "field not in this version".
	Preview string `json:"preview"`
	// Digest is a short sha256 prefix of the content, for correlation
	// between two events about the same object.
	Digest string `json:"digest,omitempty"`
	// Reason carries the explanation for blocked, contradiction and
	// resolved transitions. It is the human reason, not a duplicate of
	// Preview.
	Reason string `json:"reason,omitempty"`

	// ── blocked_attempt only (F-4) ──
	PatternFamily string `json:"pattern_family,omitempty"`
	ContentSHA256 string `json:"content_sha256,omitempty"`
	ContentLength int    `json:"content_length,omitempty"`
	Action        string `json:"action,omitempty"`
	AttemptType   string `json:"type,omitempty"`
}

// newMirrorEvent stamps the v2 envelope onto an event. Every mirror write
// goes through here, which is what makes "every v2 record has v and ts" a
// structural property rather than a convention each call site remembers.
func newMirrorEvent(op, id, collection, source, preview, digest, reason string) mirrorEvent {
	return mirrorEvent{
		V:          mirrorFormatVersion,
		Ts:         time.Now().UTC().Format(time.RFC3339),
		Op:         op,
		ID:         id,
		Collection: collection,
		Source:     source,
		Preview:    preview,
		Digest:     digest,
		Reason:     reason,
	}
}

// appendMirrorLine writes one mirror record through the shared path. This
// is the only function in the codebase permitted to open mirror.jsonl
// (TestHITLStream_AllWritersGoThroughTheSharedPath enforces it).
func appendMirrorLine(path string, ev mirrorEvent) error {
	data, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("marshal mirror event %q: %w", ev.Op, err)
	}
	return hitlStreamFor(path, mirrorPolicy).Append(data)
}

// ==================== Preview construction ====================
//
// The design's preview contract (§6) is deliberately thin: no LLM, no
// embedding model, no semantic rewrite, no second derived representation.
// Byte-deterministic given the same input, so two runs of the same workload
// produce byte-identical logs and a diff means something actually changed.

const (
	// mirrorPreviewCap is the default truncation point. The design's
	// considered range is 160–280; 240 sits in the middle and keeps a
	// terminal line comfortably inside one screen width once the envelope
	// is added.
	mirrorPreviewCap = 240
	// mirrorPreviewMax is the hard ceiling. No caller may raise the cap
	// above it: a preview that does not fit on a line is a content copy
	// with extra steps.
	mirrorPreviewMax = 280
	// mirrorDigestChars is how much of the sha256 is kept for
	// correlation. Enough to disambiguate, short enough to grep.
	mirrorDigestChars = 8
)

// buildMirrorPreview returns the human-readable preview for a record.
//
// label wins when the originating operation already carries a concise human
// string (a decision title, a lesson title, a work title); otherwise the
// stored content is excerpted. Either way the result is sanitised,
// truncated, and scanned — and if the scan trips, the caller is expected to
// fall back to a structural record with no preview at all.
func buildMirrorPreview(label, content string) (preview string, blockedReason string) {
	raw := label
	if strings.TrimSpace(raw) == "" {
		raw = content
	}
	if raw == "" {
		return "", ""
	}
	clean := sanitizePreviewText(raw)
	if clean == "" {
		return "", ""
	}
	if hit, reason := ScanContentForWrite(clean); hit {
		// The preview itself would carry rejected material. The caller
		// writes the structural record instead — never this text.
		return "", reason
	}
	return truncatePreview(clean, mirrorPreviewCap), ""
}

// sanitizePreviewText collapses everything that would make a terminal line
// lie about its own contents.
//
// This is not decoration. A preview containing a newline claims to be two
// records in `tail`; one containing an ANSI escape can rewrite the screen;
// one containing a carriage return or backspace can show text that is not
// in the file. A journal whose lines can lie is worse than no journal, so
// every control and format character becomes a space and every run of
// whitespace becomes one space.
func sanitizePreviewText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, c := range s {
		if unicode.IsControl(c) || unicode.Is(unicode.Cf, c) || c == unicode.ReplacementChar {
			// Tabs and newlines become a single space via the run
			// collapse below rather than being dropped outright, so
			// "a\nb" reads as "a b" and not "ab".
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(c)
	}
	return strings.TrimSpace(b.String())
}

// truncatePreview caps s at cap characters (hard-capped at
// mirrorPreviewMax), cutting at a word boundary and marking the cut.
//
// The word-boundary rule is a readability one: `tail`ing a journal means
// reading it, and a cut mid-word is the difference between skimming and
// re-reading. When there is no space near the cut — a 400-character
// base64 blob, say — it cuts where it is, because padding out a run of
// unbreakable characters with spaces would misrepresent the text.
func truncatePreview(s string, cap int) string {
	if cap > mirrorPreviewMax {
		cap = mirrorPreviewMax
	}
	r := []rune(s)
	if len(r) <= cap {
		return s
	}
	cut := cap - 1 // leave room for the ellipsis
	if idx := strings.LastIndex(string(r[:cut]), " "); idx > cut*3/4 {
		cut = idx
	}
	return strings.TrimRight(string(r[:cut]), " ") + "…"
}

// shortDigest returns a correlation digest of content: "sha256:" followed
// by the first few hex characters of the full digest.
//
// The prefix, not the whole thing. The full digest exists on the blocked
// record (F-4) where forensic correlation by an operator holding both sides
// of the hash is the point; on an ordinary event it is a "are these two
// lines about the same object?" hint, and 8 hex characters serve that at a
// fraction of the bytes.
func shortDigest(content string) string {
	if content == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(content))
	return "sha256:" + hex.EncodeToString(sum[:])[:mirrorDigestChars]
}

// ==================== Event builders ====================

// newMemoryEvent builds the record for a saved memory.
//
// The preview is a sanitised excerpt of the content, capped at
// mirrorPreviewCap and secret-scanned. If the scan trips, the record
// degrades to a blocked_attempt with zero preview — never a "mostly safe"
// excerpt, because "mostly" is exactly how a secret family label ends up
// in an operator's terminal history.
func newMemoryEvent(mem *Memory) mirrorEvent {
	preview, blocked := buildMirrorPreview("", mem.Content)
	if blocked != "" {
		// The content is passed to the blocked-record builder so the
		// digest is a real sha256 of the rejected bytes, not of "".
		// F-4's invariant is that the content itself is never written
		// to the file; the digest is metadata, not a key, and the
		// family/pattern label is what an operator uses to recognise
		// the attempt.
		return newBlockedMirrorEvent(mem.Content, blocked, "sensitive_attempt")
	}
	return newMirrorEvent(
		mirrorOpForCollection(mem.Collection),
		mem.ID,
		mem.Collection,
		mem.Source,
		preview,
		shortDigest(mem.Content),
		"",
	)
}

// newBlockedMirrorEvent builds the F-4 structural record. The rejected
// content never appears in it, and neither does any prefix or suffix of it.
func newBlockedMirrorEvent(content, reason, attemptType string) mirrorEvent {
	sum := sha256.Sum256([]byte(content))
	family := reason
	if idx := strings.LastIndex(reason, ": "); idx >= 0 {
		family = strings.TrimSpace(reason[idx+2:])
	}
	return mirrorEvent{
		V:             mirrorFormatVersion,
		Ts:            time.Now().UTC().Format(time.RFC3339),
		Op:            mirrorOpBlockedAttempt,
		Reason:        reason,
		PatternFamily: family,
		ContentSHA256: hex.EncodeToString(sum[:]),
		ContentLength: len(content),
		Action:        "blocked",
		AttemptType:   attemptType,
		Preview:       "",
	}
}

// newContradictionEvent records a contradiction collision.
//
// The evidence string IS the human explanation, so it goes in `reason` and
// the preview stays empty. Copying it into both fields would double every
// contradiction line in exchange for nothing — the design's own
// "do not record the same thing twice" rule.
func newContradictionEvent(memoryID, evidence string) mirrorEvent {
	reason := truncatePreview(sanitizePreviewText(evidence), mirrorPreviewCap)
	return newMirrorEvent(
		mirrorOpContradictionFound,
		memoryID,
		"memories",
		"mpm",
		"",
		"",
		reason,
	)
}

// NewMirrorShredEvent builds the tombstone a per-id shred appends.
//
// It names the object and its collection and nothing else: no content, no
// digest of the content, no preview. A tombstone exists so an operator
// reading the journal can see that mem-a1b2 was removed and when — not to
// carry a second index of what it said.
//
// It does NOT rewrite history. Rotations keep whatever the mirror said at
// the time, and the audit table and backups keep their own copies; see SPEC
// §4.5.2. That is a documented limitation, disclosed in the shred success
// message, not an oversight.
func NewMirrorShredEvent(memoryID, collection string) mirrorEvent {
	ev := newMirrorEvent(mirrorOpMemoryShredded, memoryID, collection, "mpm", "", "", "")
	return ev
}

// NewMirrorDestructiveEvent builds a wipe/purge tombstone: scope and
// counts, never content.
func NewMirrorDestructiveEvent(scope string, count int) mirrorEvent {
	reason := fmt.Sprintf("%s: cleared %d file(s)", scope, count)
	return newMirrorEvent(mirrorOpDestructiveOperation, "", "", "mpm", "", "", reason)
}

// NewMirrorEmbeddingFailureEvent records an embedding backfill failure. It
// is content-free by construction — it never had the content.
func NewMirrorEmbeddingFailureEvent(memoryID string, cause error) mirrorEvent {
	msg := ""
	if cause != nil {
		msg = cause.Error()
	}
	ev := newMirrorEvent(
		mirrorOpEmbeddingFailure,
		memoryID,
		"memories",
		"mpm",
		"",
		"",
		truncatePreview(sanitizePreviewText(msg), mirrorPreviewCap),
	)
	return ev
}

// NewMirrorLessonEvent is the lesson_created record. Lessons live in their
// own table and were never mirrored; the event line restores the
// visibility without the double-write that putting a whole lesson body in
// the journal would have cost.
func NewMirrorLessonEvent(lessonID, title, source string) mirrorEvent {
	preview, blocked := buildMirrorPreview(title, "")
	if blocked != "" {
		return newBlockedMirrorEvent(title, blocked, "sensitive_attempt")
	}
	return newMirrorEvent(mirrorOpLessonCreated, lessonID, "lessons", source, preview, shortDigest(title), "")
}

// LogEmbeddingFailureToMirror is the exported entry point for the
// `mpm backfill-embeddings` failure writer, which lives in cmd/mpm and
// therefore cannot reach the unexported append path.
//
// It lives here rather than in the command for the same reason every other
// writer does: the design's grep-gate requires that no code outside the
// shared path opens mirror.jsonl, and a command-local OpenFile is exactly
// the hole that gate exists to close.
func LogEmbeddingFailureToMirror(memoryID string, cause error) {
	if err := appendMirrorLine(DefaultLogPath("mirror"), NewMirrorEmbeddingFailureEvent(memoryID, cause)); err != nil {
		return
	}
}

// lessonTitleLabel returns the concise human label for a lesson.
//
// Lessons have no title column, and a lesson body is routinely a paragraph
// of reasoning whose first sentence is the claim. Taking the first
// sentence — rather than the first N characters — is what makes the
// `lesson_created` line readable at a glance instead of merely shorter.
func lessonTitleLabel(content string) string {
	clean := sanitizePreviewText(content)
	if idx := strings.Index(clean, ". "); idx > 0 {
		// A sentence fragment this short is more likely to be an
		// abbreviation ("e.g. ", "v1. ") than a claim; leave it alone.
		if idx+2 > 40 {
			return clean[:idx+1]
		}
	}
	return clean
}
