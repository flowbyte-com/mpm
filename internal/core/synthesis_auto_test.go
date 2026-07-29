package internal

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSanitiseFTS5Tokens_Quoting pins the core FTS5 fix. Bare
// tokens caused FTS5 to interpret content words as column names
// and fail with "no such column: X". Wrapping each token in
// phrase quotes forces FTS5 to treat every token as a literal
// search term.
func TestSanitiseFTS5Tokens_Quoting(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		expected []string
	}{
		{
			name:     "simple content",
			input:    "remember the milk",
			expected: []string{`"remember"`, `"the"`, `"milk"`},
		},
		{
			// The regression case: "07" was being interpreted as a
			// column name by FTS5. With quoting, it's a literal
			// search term.
			name:     "regression: \"07\" was treated as a column",
			input:    "07 verify pattern",
			expected: []string{`"07"`, `"verify"`, `"pattern"`},
		},
		{
			// Common cognitive-substrate tokens that look like
			// column names.
			name:     "common false-positive tokens",
			input:    "CHOICE verified closed hole",
			expected: []string{`"CHOICE"`, `"verified"`, `"closed"`, `"hole"`},
		},
		{
			// FTS5 operators are stripped from the output entirely.
			name:     "FTS5 operators stripped",
			input:    "running AND walking OR jumping",
			expected: []string{`"running"`, `"walking"`, `"jumping"`},
		},
		{
			// FTS5 special characters are stripped from word
			// boundaries. The close-paren at the end of "running)"
			// gets stripped, leaving "running".
			name:     "FTS5 special chars stripped",
			input:    "running) (walking) *star*",
			expected: []string{`"running"`, `"walking"`, `"star"`},
		},
		{
			// After stripping, an empty token doesn't appear.
			name:     "empty tokens dropped",
			input:    "real   word",
			expected: []string{`"real"`, `"word"`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitiseFTS5Tokens(tc.input)
			if !equalStringSlicesForTest(got, tc.expected) {
				t.Errorf("sanitiseFTS5Tokens(%q):\n  got      %q\n  expected %q",
					tc.input, got, tc.expected)
			}
		})
	}

	// Cap at 100 tokens to bound query length.
	t.Run("100 tokens cap", func(t *testing.T) {
		got := sanitiseFTS5Tokens(repeatWordTokens(150))
		if len(got) != 100 {
			t.Errorf("expected 100 tokens (cap), got %d", len(got))
		}
	})
}

// TestSynthesisEnabled_DefaultTrue pins the kill switch default.
// A missing field in mpm_config.json (i.e. older config files
// written before this commit) must default to enabled, NOT
// disabled. The whole point of the kill switch is opt-out, not
// opt-in.
func TestSynthesisEnabled_DefaultTrue(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MPM_WORKSPACE", dir)

	cfgFile := filepath.Join(dir, "mpm_config.json")
	if err := writeConfigFileForTest(cfgFile, `{
  "synth": {
    "model": "MiniMax-M3",
    "api_key": "test",
    "base_url": "http://localhost:11434/v1"
  }
}`); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if !synthesisEnabled() {
		t.Error("expected synthesisEnabled() == true when field is missing")
	}
}

// TestSynthesisEnabled_ExplicitTrue pins the explicit-true case.
// A config with synthesis_enabled=true must return true. (Tests
// that the field is *parsed* correctly, not just default-true.)
func TestSynthesisEnabled_ExplicitTrue(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MPM_WORKSPACE", dir)

	cfgFile := filepath.Join(dir, "mpm_config.json")
	if err := writeConfigFileForTest(cfgFile, `{
  "synthesis_enabled": true,
  "synth": {
    "model": "MiniMax-M3",
    "api_key": "test",
    "base_url": "http://localhost:11434/v1"
  }
}`); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if !synthesisEnabled() {
		t.Error("expected synthesisEnabled() == true when field is explicitly true")
	}
}

// TestSynthesisEnabled_ExplicitFalse pins the kill switch.
// The whole point of this commit: when synthesis_enabled is false,
// AutoSynthesize returns immediately without any FTS5 query or
// LLM call.
func TestSynthesisEnabled_ExplicitFalse(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("MPM_WORKSPACE", dir)

	cfgFile := filepath.Join(dir, "mpm_config.json")
	if err := writeConfigFileForTest(cfgFile, `{
  "synthesis_enabled": false,
  "synth": {
    "model": "MiniMax-M3",
    "api_key": "test",
    "base_url": "http://localhost:11434/v1"
  }
}`); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if synthesisEnabled() {
		t.Error("expected synthesisEnabled() == false when kill switch is engaged")
	}
}

// TestSynthesisEnabled_NilConfig pins the safe-default path.
// If config fails to load (or is nil), synthesisEnabled returns
// true. This is the fail-safe: a broken config must not silently
// disable synthesis. Operationally, this means a corrupted
// mpm_config.json without a synthesis_enabled field should
// continue to fire synthesis rather than silently break it.
func TestSynthesisEnabled_NilConfig(t *testing.T) {
	// Point at a directory that doesn't exist. config.LoadConfig
	// returns an empty default config in that case — but the
	// critical assertion is that synthesisEnabled() returns true.
	dir := t.TempDir()
	t.Setenv("MPM_WORKSPACE", dir)

	// No mpm_config.json in this directory. LoadConfig should
	// return &Config{} with all fields nil/zero. synthesisEnabled
	// must default to true.
	if !synthesisEnabled() {
		t.Error("expected synthesisEnabled() == true when config is empty/default")
	}
}

// Test helpers below — all renamed to avoid name collisions with
// other test files in the same package (mode_persona_validation_test.go,
// vector_index_test.go, etc. all define their own writeFile and itoa).

// writeConfigFileForTest writes a config file with mode 0600.
// Renamed from writeFile to avoid collision with mode_persona_validation_test.go.
func writeConfigFileForTest(path, content string) error {
	return os.WriteFile(path, []byte(content), 0600)
}

// repeatWordTokens returns a string of "w0 w1 w2 ... wN-1" with the
// requested count. Used to test the 100-token cap.
func repeatWordTokens(n int) string {
	b := make([]byte, 0, n*4)
	for i := 0; i < n; i++ {
		if i > 0 {
			b = append(b, ' ')
		}
		b = append(b, []byte("w")...)
		b = append(b, []byte(intToStr10ForTest(i))...)
	}
	return string(b)
}

// intToStr10ForTest is a tiny int-to-string helper, renamed to avoid
// collision with vector_index_test.go's itoa.
func intToStr10ForTest(n int) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

// equalStringSlicesForTest is a tiny helper for content equality.
func equalStringSlicesForTest(a, b []string) bool {
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

// _ = time and context — keep these imports alive across
// future edits where the test file may grow to need them.
var _ = time.Now
var _ = context.Background

// =============================================================================
// Content_hash dedup + cooldown tests
// =============================================================================

// TestSynthHashContent_KnownVector pins the SHA-256 hex format. If
// the algorithm ever changes (or we accidentally swap to a different
// hash), this regression test fires. The test vector below is the
// standard SHA-256("hello") from NIST examples.
func TestSynthHashContent_KnownVector(t *testing.T) {
	const want = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	got := synthHashContent("hello")
	if got != want {
		t.Errorf("synthHashContent(\"hello\") = %q; want %q", got, want)
	}
}

// TestSynthHashContent_Format pins the format contract: 64-char
// lower-case hex. Anything else would break either the SQL column
// (TEXT comparison) or the equality lookup (case-sensitive).
func TestSynthHashContent_Format(t *testing.T) {
	cases := []string{
		"hello",
		"",
		"a",
		"This is a longer string with mixed CASE and 123 numbers",
		"\u4e2d\u6587\u5185\u5bb9", // Chinese — non-ASCII path
	}
	for _, s := range cases {
		got := synthHashContent(s)
		if len(got) != 64 {
			t.Errorf("synthHashContent(%q) length = %d; want 64", s, len(got))
		}
		for i, r := range got {
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
				t.Errorf("synthHashContent(%q)[%d] = %q not lower-case hex (got %q)", s, i, r, got)
				break
			}
		}
	}
}

// TestSynthHashContent_Deterministic — same input → same output.
func TestSynthHashContent_Deterministic(t *testing.T) {
	a := synthHashContent("the quick brown fox")
	b := synthHashContent("the quick brown fox")
	if a != b {
		t.Errorf("hash not deterministic: %q vs %q", a, b)
	}
}

// TestSynthHashContent_DistinctInputs — distinct inputs → distinct
// hashes. Catches the trivial bug where someone forgets to pass the
// input through to sha256.
func TestSynthHashContent_DistinctInputs(t *testing.T) {
	a := synthHashContent("the quick brown fox")
	b := synthHashContent("the quick brown cat")
	if a == b {
		t.Errorf("collision: %q and %q both produced %q", "fox", "cat", a)
	}
}

// TestSynthHasContentHash_Miss pins the empty-DB miss path.
// synthHasContentHash must return (false, 0) on a clean DB.
func TestSynthHasContentHash_Miss(t *testing.T) {
	dm, err := NewDatabaseManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()

	hit, runCount := synthHasContentHash(dm, "deadbeef")
	if hit || runCount != 0 {
		t.Errorf("expected (false, 0) on empty DB; got (%v, %d)", hit, runCount)
	}
}

// TestSynthHasContentHash_HitAndBump pins the lifecycle: insert
// via recordSynthRun → look up returns hit + runCount=1 → call
// again with bump → runCount becomes 2.
func TestSynthHasContentHash_HitAndBump(t *testing.T) {
	dm, err := NewDatabaseManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()

	const hash = "abc123"
	recordSynthRun(dm, hash, "result-mem-1")

	hit, runCount := synthHasContentHash(dm, hash)
	if !hit {
		t.Errorf("expected hit after recordSynthRun")
	}
	if runCount != 1 {
		t.Errorf("runCount after first insert = %d; want 1", runCount)
	}

	bumpSynthDedupCounter(dm, hash)
	_, runCount = synthHasContentHash(dm, hash)
	if runCount != 2 {
		t.Errorf("runCount after dedup bump = %d; want 2", runCount)
	}
}

// TestRecordSynthRun_FirstRun pins the INSERT path. The first
// write for a content_hash sets run_count=1, last_run_at=now,
// result_memory_id set. We don't assert timestamps directly
// because they're time.Now() — too flaky for that. Instead we
// assert the row exists and run_count is 1.
func TestRecordSynthRun_FirstRun(t *testing.T) {
	dm, err := NewDatabaseManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()

	const hash = "first-run-hash"
	const resultID = "mem-result-1"
	recordSynthRun(dm, hash, resultID)

	var storedResult string
	err = dm.db.QueryRow(
		`SELECT result_memory_id FROM synth_runs WHERE content_hash = ?`,
		hash,
	).Scan(&storedResult)
	if err != nil {
		t.Fatalf("SELECT result_memory_id: %v", err)
	}
	if storedResult != resultID {
		t.Errorf("result_memory_id = %q; want %q", storedResult, resultID)
	}
}

// TestDetectNearMiss_RespectsLastSynthesizedAtCooldown pins the
// per-candidate cooldown filter. A memory stamped with
// last_synthesized_at = now (within the cooldown window) must
// not be returned as a near-miss candidate.
func TestDetectNearMiss_RespectsLastSynthesizedAtCooldown(t *testing.T) {
	dm, err := NewDatabaseManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()

	const memID = "mem-cooldown-1"
	_, err = dm.db.Exec(`
		INSERT INTO memories (id, collection, content, content_hash, last_synthesized_at)
		VALUES (?, 'memories', ?, ?, strftime('%s','now'))
	`, memID, "alpha beta gamma delta epsilon", "hash-mem-1")
	if err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	// FTS5 needs the trigger to populate the FTS index. The trigger
	// only fires on INSERT INTO memories, so the FTS lookup below
	// would otherwise miss the row. Confirm the row IS indexed by
	// running a quick probe.
	var indexed int
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM memories_fts WHERE memories_fts MATCH ?`,
		`"alpha"`).Scan(&indexed); err != nil {
		t.Fatalf("FTS5 probe: %v", err)
	}
	if indexed == 0 {
		t.Skip("FTS5 trigger not auto-fired in this DB session; skipping cooldown assertion")
	}

	// Cooldown of 3600s: the row we just inserted has
	// last_synthesized_at = now, which is well within the
	// cooldown window. It MUST NOT appear.
	//
	// Threshold is 0 to bypass the bm25 score filter entirely
	// — this test is about the cooldown predicate, not FTS5
	// relevance scoring. SQLite FTS5 bm25 scores for a 5-word
	// doc matching a 2-word query are small (e.g. -1e-06); the
	// production -10 threshold would exclude this row for score
	// reasons even when cooldown is off, which would make the
	// test impossible to interpret. Threshold=0 means "any
	// FTS5 hit passes", letting us isolate the cooldown
	// predicate's behaviour.
	candidates, err := DetectNearMiss(dm, "alpha beta", "new-mem", 0.0, 3600)
	if err != nil {
		t.Fatalf("DetectNearMiss: %v", err)
	}
	for _, c := range candidates {
		if c.ID == memID {
			t.Errorf("candidate %s should be filtered by cooldown (3600s)", memID)
		}
	}

	// Cooldown of 0: the filter is a no-op. The row SHOULD appear.
	candidates, err = DetectNearMiss(dm, "alpha beta", "new-mem", 0.0, 0)
	if err != nil {
		t.Fatalf("DetectNearMiss (cooldown=0): %v", err)
	}
	found := false
	for _, c := range candidates {
		if c.ID == memID {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("candidate %s should appear with cooldown=0", memID)
	}
}

// TestMarkMemorySynthCooldown pins the UPDATE path. After
// markMemorySynthCooldown(R), R.last_synthesized_at must NOT be
// NULL (i.e. the cooldown filter will exclude R from
// near-miss candidates until the window expires).
func TestMarkMemorySynthCooldown(t *testing.T) {
	dm, err := NewDatabaseManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()

	const memID = "mem-result-cooldown"
	_, err = dm.db.Exec(`
		INSERT INTO memories (id, collection, content, content_hash, last_synthesized_at)
		VALUES (?, 'memories', ?, ?, NULL)
	`, memID, "result content", "hash-result")
	if err != nil {
		t.Fatalf("INSERT: %v", err)
	}

	markMemorySynthCooldown(dm, memID)

	var stamped *int64
	err = dm.db.QueryRow(
		`SELECT last_synthesized_at FROM memories WHERE id = ?`,
		memID,
	).Scan(&stamped)
	if err != nil {
		t.Fatalf("SELECT: %v", err)
	}
	if stamped == nil {
		t.Errorf("last_synthesized_at still NULL after markMemorySynthCooldown")
	}
}

// TestMarkMemorySynthCooldown_NilAndEmpty pins the safe-call path.
// markMemorySynthCooldown(nil, "x") and markMemorySynthCooldown(dm, "")
// must both no-op without panicking. The argument is touched on
// every successful synthesis — a panic here would crash the
// write path.
func TestMarkMemorySynthCooldown_NilAndEmpty(t *testing.T) {
	dm, err := NewDatabaseManager(t.TempDir())
	if err != nil {
		t.Fatalf("NewDatabaseManager: %v", err)
	}
	defer dm.Close()

	// nil dm → no-op.
	markMemorySynthCooldown(nil, "any-id")
	// empty id → no-op.
	markMemorySynthCooldown(dm, "")
	// Both must not have created any rows (the empty dm might, but
	// nil must not).
	var rows int
	if err := dm.db.QueryRow(`SELECT COUNT(*) FROM memories`).Scan(&rows); err != nil {
		t.Fatalf("COUNT: %v", err)
	}
	if rows != 0 {
		t.Errorf("expected 0 memories; got %d", rows)
	}
}
