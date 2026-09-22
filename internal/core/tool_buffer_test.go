// tool_buffer_test.go — pins the buffer's contract: N=1 per session,
// singleton lifecycle, cognitive-verb exclusion semantics.

package internal

import (
	"sync"
	"testing"
	"time"
)

// freshBuffer returns a clean ToolBuffer for tests that need isolation
// from the package singleton.
func freshBuffer() *ToolBuffer {
	ResetForTest()
	return GlobalToolBuffer()
}

func TestToolBuffer_RecordAndHead(t *testing.T) {
	b := freshBuffer()
	rec := &ToolCallRecord{ToolName: "read_file", URI: "/tmp/x"}
	b.Record("session-1", rec)

	got := b.Head("session-1")
	if got == nil {
		t.Fatal("Head returned nil after Record")
	}
	if got.ToolName != "read_file" {
		t.Errorf("ToolName: want read_file, got %q", got.ToolName)
	}
	if got.CallID == "" {
		t.Error("Record should auto-fill empty CallID with uuid")
	}
	if got.CalledAt.IsZero() {
		t.Error("Record should auto-fill zero CalledAt with time.Now")
	}
}

func TestToolBuffer_N1Invariant(t *testing.T) {
	b := freshBuffer()
	b.Record("s1", &ToolCallRecord{ToolName: "read_file"})
	b.Record("s1", &ToolCallRecord{ToolName: "web_search"}) // overwrites
	b.Record("s1", &ToolCallRecord{ToolName: "exec"})        // overwrites again

	got := b.Head("s1")
	if got == nil {
		t.Fatal("Head returned nil")
	}
	if got.ToolName != "exec" {
		t.Errorf("expected most recent (exec), got %q", got.ToolName)
	}
	if b.Len() != 1 {
		t.Errorf("Len: want 1 (N=1 invariant), got %d", b.Len())
	}
}

func TestToolBuffer_HeadUnknownSession(t *testing.T) {
	b := freshBuffer()
	if got := b.Head("never-recorded"); got != nil {
		t.Errorf("Head on unknown session: want nil, got %+v", got)
	}
}

func TestToolBuffer_Forget(t *testing.T) {
	b := freshBuffer()
	b.Record("s1", &ToolCallRecord{ToolName: "read_file"})
	b.Forget("s1")
	if b.Head("s1") != nil {
		t.Error("Head after Forget: want nil")
	}
	// Forget unknown session is no-op (idempotent)
	b.Forget("never-existed")
}

func TestToolBuffer_NilRecordIgnored(t *testing.T) {
	b := freshBuffer()
	b.Record("s1", nil)
	if b.Head("s1") != nil {
		t.Error("Record(nil) should not create an entry")
	}
}

func TestToolBuffer_EmptySessionIDIgnored(t *testing.T) {
	b := freshBuffer()
	b.Record("", &ToolCallRecord{ToolName: "read_file"})
	if b.Len() != 0 {
		t.Errorf("empty session_id must be ignored, got Len=%d", b.Len())
	}
	if b.Head("") != nil {
		t.Error("Head(\"\") must return nil")
	}
}

func TestToolBuffer_ConcurrentRecordHead(t *testing.T) {
	// Hammer the buffer from multiple goroutines — the RWMutex should
	// keep reads safe and not deadlock. No assertions on final state
	// beyond "no panic, no data race".
	b := freshBuffer()
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func(id int) {
			defer wg.Done()
			b.Record("s", &ToolCallRecord{ToolName: "x", CalledAt: time.Now()})
		}(i)
		go func() {
			defer wg.Done()
			_ = b.Head("s")
		}()
	}
	wg.Wait()
}

// TestIsCognitiveVerbExhaustive pins the cognitive-verb list. Adding a
// new save/write tool to the registry requires updating this list —
// the test enforces that the well-known names are present.
func TestIsCognitiveVerbExhaustive(t *testing.T) {
	knownWrites := []string{
		"save_to_memory", "record_decision", "propose_theory",
		"resolve_theory", "save_lesson", "save_skill",
		"commit_milestone", "create_topic", "link_topic",
		"add_evidence", "challenge_memory", "shred_memory",
		"add_reference", "save_reference",
		"flush_scratchpad", "promote_scratchpad", "discard_scratchpad",
		"session_end", "session_handoff", "mpm_log_to_changelog",
		"promote_memory", "promote_to_global", "promote_skill_to_global",
		"record_global_rule",
	}
	for _, name := range knownWrites {
		if !IsCognitiveVerb(name) {
			t.Errorf("known write tool %q is NOT in cognitiveVerbs — provenance will break", name)
		}
	}

	// And the inverse — observation/action tools must NOT be cognitive.
	observationTools := []string{
		"read_file", "web_search", "web_fetch", "exec", "playwright_*",
		"image_generate", "music_generate", "video_generate",
	}
	for _, name := range observationTools {
		if IsCognitiveVerb(name) {
			t.Errorf("observation tool %q wrongly classified as cognitive", name)
		}
	}
}

func TestGlobalToolBufferSingleton(t *testing.T) {
	// Two calls should return the same pointer.
	if GlobalToolBuffer() != GlobalToolBuffer() {
		t.Error("GlobalToolBuffer() not returning the same pointer")
	}
}

func TestResetForTestClearsState(t *testing.T) {
	b := freshBuffer()
	b.Record("s1", &ToolCallRecord{ToolName: "read_file"})
	if b.Len() != 1 {
		t.Fatalf("setup: Len=%d, want 1", b.Len())
	}
	ResetForTest()
	if GlobalToolBuffer().Len() != 0 {
		t.Error("ResetForTest did not clear the singleton")
	}
}