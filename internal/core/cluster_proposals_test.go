// cluster_proposals_test.go — table-driven tests for the cluster proposal
// helpers (HashMessage, ClusterKey). These functions are load-bearing:
// both the write-side UPSERT (audit.go::upsertClusterCounter) and the
// read-side surfacing (wake_context.go::AuditSummary) derive cluster_key
// from them. A regression here would silently split or merge clusters
// across the entire codebase. The tests pin the invariants:
//
//   1. HashMessage collapses leading/trailing whitespace (log noise
//      like "timeout\n" matches "timeout") but preserves internal
//      whitespace (different stack traces stay distinct).
//   2. HashMessage is deterministic and stable for fixed inputs.
//   3. ClusterKey includes component, so two subsystems throwing the
//      same message become distinct clusters keyed by component.
//   4. ClusterKey is identical for identical (component, hash) pairs.
//
// Pure helpers, zero external deps, no DB needed.
package internal

import "testing"

func TestHashMessage_Invariants(t *testing.T) {
	cases := []struct {
		name string
		a, b string
		same bool
	}{
		// --- same-hash cases ---
		{"trailing newline collapses", "connection timeout", "connection timeout\n", true},
		{"leading whitespace collapses", "connection timeout", "  connection timeout", true},
		{"both-end whitespace collapses", "connection timeout", "  connection timeout\n\t", true},
		{"identical input", "foo bar baz", "foo bar baz", true},

		// --- different-hash cases ---
		{"different message", "connection timeout", "connection refused", false},
		{"internal whitespace preserved (different stack traces stay distinct)", "foo  bar", "foo bar", false},
		{"empty vs non-empty", "", "x", false},
		{"case-sensitive", "Error", "error", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotSame := HashMessage(tc.a) == HashMessage(tc.b)
			if gotSame != tc.same {
				t.Errorf("HashMessage(%q) == HashMessage(%q): got same=%v, want same=%v\n  a=%s\n  b=%s",
					tc.a, tc.b, gotSame, tc.same,
					HashMessage(tc.a), HashMessage(tc.b))
			}
		})
	}
}

func TestHashMessage_Deterministic(t *testing.T) {
	// Same input twice must yield the same hash. The md5 package is
	// deterministic by construction, but pinning this here means a
	// future "let's add a salt" or "let's randomize" PR would have
	// to update the test explicitly.
	a := HashMessage("connection timeout")
	b := HashMessage("connection timeout")
	if a != b {
		t.Errorf("HashMessage not deterministic: %q vs %q", a, b)
	}
	// And the hash should be the right shape: 32 hex chars (md5 is 128 bits).
	if len(a) != 32 {
		t.Errorf("HashMessage output length: got %d, want 32 hex chars\n  hash=%s", len(a), a)
	}
}

func TestClusterKey_IsolatesByComponent(t *testing.T) {
	// Same message hash, different components → distinct keys.
	// This is the load-bearing invariant: two subsystems throwing the
	// same generic message ("connection timeout" from relay vs sse)
	// must produce distinct clusters so they can be tracked separately.
	hash := HashMessage("connection timeout")
	keyRelay := ClusterKey("relay", hash)
	keySSE := ClusterKey("sse", hash)
	keyMCP := ClusterKey("mpm-mcp", hash)

	if keyRelay == keySSE {
		t.Errorf("relay and sse should have distinct keys; both got %q", keyRelay)
	}
	if keyRelay == keyMCP {
		t.Errorf("relay and mpm-mcp should have distinct keys; both got %q", keyRelay)
	}
	if keySSE == keyMCP {
		t.Errorf("sse and mpm-mcp should have distinct keys; both got %q", keySSE)
	}

	// Each key should contain the component name as a substring so
	// cluster_key is human-readable when surfaced in wake_context.
	// (Not a correctness invariant, but a debugging affordance.)
	for _, pair := range []struct{ component, key string }{
		{"relay", keyRelay},
		{"sse", keySSE},
		{"mpm-mcp", keyMCP},
	} {
		if !contains(pair.key, pair.component) {
			t.Errorf("ClusterKey(%q) = %q does not contain component name", pair.component, pair.key)
		}
	}
}

func TestClusterKey_DeterministicForSameInputs(t *testing.T) {
	a := ClusterKey("relay", "abc123")
	b := ClusterKey("relay", "abc123")
	if a != b {
		t.Errorf("ClusterKey not deterministic: %q vs %q", a, b)
	}
	// Format: "component:hash". Pin the exact shape so a refactor
	// that changes the separator (e.g. to "_" or "/") breaks this
	// test visibly rather than silently breaking wake_context lookups.
	want := "relay:abc123"
	if a != want {
		t.Errorf("ClusterKey(\"relay\", \"abc123\") = %q, want %q", a, want)
	}
}

func TestClusterKey_SameInputsSameKey(t *testing.T) {
	// Identical (component, hash) inputs must produce identical keys.
	// This is the dedup primitive: same error from same component
	// always lands in the same cluster row.
	hash := HashMessage("disk full")
	k1 := ClusterKey("storage", hash)
	k2 := ClusterKey("storage", hash)
	if k1 != k2 {
		t.Errorf("ClusterKey must be deterministic for same inputs: %q vs %q", k1, k2)
	}
}

// contains is provided by shared_write_test.go in the same package.