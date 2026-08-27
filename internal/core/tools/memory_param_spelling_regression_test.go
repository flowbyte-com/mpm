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
// Contract after D6 fix: canonical wire format is snake_case `memory_id`
// (wire-format block above handleShredMemory). The camelCase alias is
// accepted on every id-taking action for backward compat.
//
// F4 convergence (2026-08-27): every action now uses the canonical
// snake_case `memory_id` as the SOLE wire param. Legacy `memoryId` is
// still accepted at runtime via the dispatcher-level normalize, but the
// schema no longer advertises it. This test asserts the canonical wire
// form succeeds for every action; the backward-compat alias path is
// covered by TestMpmMemory_LegacyCamelCaseAlias below.
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

	probe := save("f4 canonical probe")

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
		params := map[string]interface{}{"memory_id": probe}
		for k, v := range tc.extra {
			params[k] = v
		}
		if _, err := handleMpmMemory(dm, internal.ActiveContext{}, map[string]interface{}{
			"action": tc.action, "params": params,
		}); err != nil {
			t.Errorf("%s with canonical memory_id failed: %v", tc.action, err)
		}
	}

	// patch + promote with canonical snake_case.
	if _, err := handleMpmMemory(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": "patch",
		"params": map[string]interface{}{"memory_id": probe, "patch": map[string]interface{}{"k": "v"}},
	}); err != nil {
		t.Errorf("patch with canonical memory_id failed: %v", err)
	}
	if _, err := handleMpmMemory(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": "promote",
		"params": map[string]interface{}{"memory_id": probe},
	}); err != nil {
		t.Errorf("promote with canonical memory_id failed: %v", err)
	}

	// challenge (F4 convergence): canonical is now snake_case `memory_id`,
	// not camelCase `memoryId`. The handler reads memory_id directly.
	if _, err := handleMpmMemory(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": "challenge",
		"params": map[string]interface{}{"memory_id": probe, "evidence": "f4 canonical probe"},
	}); err != nil {
		t.Errorf("challenge with canonical memory_id failed: %v", err)
	}

	// shred last (irrecoverable) with canonical memory_id.
	victim := save("f4 shred victim")
	if _, err := handleMpmMemory(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": "shred",
		"params": map[string]interface{}{"memory_id": victim},
	}); err != nil {
		t.Errorf("shred with canonical memory_id failed: %v", err)
	}
}

// TestMpmMemory_LegacyCamelCaseAlias asserts the backward-compat contract
// introduced by the 2026-08-25 D6 fix and preserved by the 2026-08-27 F4
// convergence: legacy `memoryId` callers continue to succeed for every
// id-taking action. The dispatcher normalizes the camelCase form onto
// `memory_id` before the handler runs. Removing this normalization would
// silently break every existing camelCase caller.
func TestMpmMemory_LegacyCamelCaseAlias(t *testing.T) {
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

	probe := save("d6 backward-compat probe")

	cases := []struct {
		action string
		extra  map[string]interface{}
	}{
		{"reinforce", map[string]interface{}{"delta": float64(1)}},
		{"weaken", map[string]interface{}{"delta": float64(1)}},
		{"snooze", map[string]interface{}{"days": float64(1)}},
		{"set_weight", map[string]interface{}{"weight": float64(3)}},
		{"patch", map[string]interface{}{"patch": map[string]interface{}{"k": "v"}}},
		{"promote", nil},
		// challenge: legacy camelCase `memoryId` was canonical pre-F4;
		// the dispatcher still accepts it via the `memoryId → memory_id`
		// normalize. After F4, the canonical form for challenge is also
		// `memory_id`, so the legacy path must continue to work to avoid
		// silently breaking existing callers.
		{"challenge", map[string]interface{}{"evidence": "d6 backward-compat challenge"}},
	}

	for _, tc := range cases {
		params := map[string]interface{}{"memoryId": probe}
		for k, v := range tc.extra {
			params[k] = v
		}
		if _, err := handleMpmMemory(dm, internal.ActiveContext{}, map[string]interface{}{
			"action": tc.action, "params": params,
		}); err != nil {
			t.Errorf("%s with legacy memoryId alias failed: %v", tc.action, err)
		}
	}

	// shred last (irrecoverable) with legacy camelCase alias.
	victim := save("d6 backward-compat shred victim")
	if _, err := handleMpmMemory(dm, internal.ActiveContext{}, map[string]interface{}{
		"action": "shred",
		"params": map[string]interface{}{"memoryId": victim},
	}); err != nil {
		t.Errorf("shred with legacy memoryId alias failed: %v", err)
	}
}
