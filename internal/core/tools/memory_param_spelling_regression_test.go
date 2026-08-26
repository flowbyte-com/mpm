package tools

import (
	"testing"

	"github.com/flowbyte-com/mpm-core"
)

// TestMpmMemory_ParamSpellingAliases is the D6 regression test (2026-08-25).
//
// Bug: the registry schema documents BOTH `memory_id` and `memoryId` as
// mpm_memory params, but each id-taking action accepted exactly one spelling;
// a schema-guided caller picking the wrong variant got a hard error.
//
// Contract after fix: canonical wire format is snake_case `memory_id`
// (wire-format block above handleShredMemory). The camelCase alias is
// accepted on every id-taking action; `challenge` remains canonically
// camelCase (`memoryId`) with snake_case aliased onto it. Both spellings
// must succeed for every action below.
func TestMpmMemory_ParamSpellingAliases(t *testing.T) {
	dm := newTestSharedDM(t)

	save := func(content string) string {
		res, err := handleMpmMemory(dm, internal.ActiveContext{}, map[string]interface{}{
			"action": "save",
			"params": map[string]interface{}{"fact": content},
		})
		if err != nil {
			t.Fatalf("save %q: %v", content, err)
		}
		id, _ := res.(map[string]interface{})["id"].(string)
		if id == "" {
			t.Fatalf("save %q returned empty id: %v", content, res)
		}
		return id
	}

	probe := save("d6 spelling probe")

	cases := []struct {
		action string
		extra  map[string]interface{}
	}{
		{"reinforce", map[string]interface{}{"delta": float64(1)}},
		{"weaken", map[string]interface{}{"delta": float64(1)}},
		{"snooze", map[string]interface{}{"days": float64(1)}},
		{"set_weight", map[string]interface{}{"weight": float64(3)}},
	}

	for _, tc := range cases {
		for _, key := range []string{"memory_id", "memoryId"} {
			params := map[string]interface{}{key: probe}
			for k, v := range tc.extra {
				params[k] = v
			}
			if _, err := handleMpmMemory(dm, internal.ActiveContext{}, map[string]interface{}{
				"action": tc.action, "params": params,
			}); err != nil {
				t.Errorf("%s with %q failed: %v", tc.action, key, err)
			}
		}
	}

	// patch + promote + shred with both spellings.
	for _, key := range []string{"memory_id", "memoryId"} {
		if _, err := handleMpmMemory(dm, internal.ActiveContext{}, map[string]interface{}{
			"action": "patch",
			"params": map[string]interface{}{key: probe, "patch": map[string]interface{}{"k": "v"}},
		}); err != nil {
			t.Errorf("patch with %q failed: %v", key, err)
		}
	}
	for _, key := range []string{"memory_id", "memoryId"} {
		if _, err := handleMpmMemory(dm, internal.ActiveContext{}, map[string]interface{}{
			"action": "promote",
			"params": map[string]interface{}{key: probe},
		}); err != nil {
			t.Errorf("promote with %q failed: %v", key, err)
		}
	}

	// challenge accepts both spellings (canonical memoryId, aliased memory_id).
	for _, key := range []string{"memoryId", "memory_id"} {
		if _, err := handleMpmMemory(dm, internal.ActiveContext{}, map[string]interface{}{
			"action": "challenge",
			"params": map[string]interface{}{key: probe, "evidence": "d6 alias probe"},
		}); err != nil {
			t.Errorf("challenge with %q failed: %v", key, err)
		}
	}

	// shred last (irrecoverable): a FRESH memory per spelling, since the
	// first successful shred removes the only live row.
	for _, key := range []string{"memory_id", "memoryId"} {
		victim := save("d6 shred victim " + key)
		if _, err := handleMpmMemory(dm, internal.ActiveContext{}, map[string]interface{}{
			"action": "shred",
			"params": map[string]interface{}{key: victim},
		}); err != nil {
			t.Errorf("shred with %q failed: %v", key, err)
		}
	}
}
