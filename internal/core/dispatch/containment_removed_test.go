package dispatch

import "testing"

// TestContainmentRemovalGate proves the migration gate: obsolete
// containment configuration refuses execution, and the transitional
// acknowledgement is the only way past it.
func TestContainmentRemovalGate(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	if err := CheckContainmentRemoval(env(nil)); err != nil {
		t.Fatalf("an empty environment was refused: %v", err)
	}
	if err := CheckContainmentRemoval(env(map[string]string{"MPM_OPENCODE_RELAY_SOCKET": "x"})); err == nil {
		t.Fatal("the relay socket alone did not trigger the gate")
	}
	for _, name := range RemovedContainmentEnv {
		err := CheckContainmentRemoval(env(map[string]string{name: "x"}))
		if err == nil {
			t.Fatalf("%s did not trigger the gate", name)
		}
		if !contains(err.Error(), name) {
			t.Fatalf("the refusal does not name %s: %v", name, err)
		}
		if !contains(err.Error(), "REMOVED") {
			t.Fatalf("the refusal does not say the subsystem was removed: %v", err)
		}
	}
	// Whitespace is not configuration.
	if err := CheckContainmentRemoval(env(map[string]string{RemovedContainmentEnv[0]: "   "})); err != nil {
		t.Fatalf("a blank value triggered the gate: %v", err)
	}
	// The acknowledgement unblocks it.
	if err := CheckContainmentRemoval(env(map[string]string{
		RemovedContainmentEnv[0]: "x",
		ContainmentRemovedAck:    "1",
	})); err != nil {
		t.Fatalf("the acknowledgement did not unblock: %v", err)
	}
	// Any other acknowledgement value does not.
	if err := CheckContainmentRemoval(env(map[string]string{
		RemovedContainmentEnv[0]: "x",
		ContainmentRemovedAck:    "true",
	})); err == nil {
		t.Fatal("an acknowledgement other than 1 was accepted")
	}
}

// contains is a tiny local helper so the test reads without importing
// strings for one call.
func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
