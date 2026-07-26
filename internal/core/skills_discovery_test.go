// skills_discovery_test.go — Task 9 of the skills-layer plan.
//
// ProactiveRecallHint must surface skills whose when_to_use field
// overlaps any keyword from the conversation text. Skills are a fourth
// hint source alongside the existing decision/theory matches from
// FindEpistemologyOverlaps.
//
// The minScore argument is -3.0 in every test because the underlying
// FindEpistemologyOverlaps treats `score >= threshold` as SKIP — BM25
// scores are negative numbers (closer to 0 = better match), so -3.0 is
// permissive enough to let every candidate through. The MCP handler
// (handleProactiveRecallHint) already defaults min_score to -3.0 when
// the caller doesn't supply one, confirming this is the floor.

package internal

import (
	"strings"
	"testing"
)

func TestProactiveRecallHint_SkillOverlaps(t *testing.T) {
	const skillFrontmatter = "---\nname: agentshell\nversion: 1.0.0\nwhen_to_use: agentshell, wordpress, theme\n---\nbody"

	cases := []struct {
		name         string
		conversation string
		wantSkill    bool
	}{
		{
			name:         "overlap surfaces skill hint",
			conversation: "I'm working with the agentshell theme",
			wantSkill:    true,
		},
		{
			name:         "no overlap yields no skill hint",
			conversation: "the weather is sunny today",
			wantSkill:    false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dm := NewTestDM(t)
			insertRawSkill(t, dm, "skill:agentshell-v1.0.0", "agentshell", "1.0.0", skillFrontmatter)

			hints, err := dm.ProactiveRecallHint(tc.conversation, 3, -3.0)
			if err != nil {
				t.Fatalf("ProactiveRecallHint: %v", err)
			}

			found := false
			for _, h := range hints {
				content, ok := h["content"].(string)
				if !ok {
					continue
				}
				if strings.Contains(content, "agentshell") {
					found = true
					break
				}
			}
			if found != tc.wantSkill {
				t.Fatalf("skill hint surfaced=%v, want %v (hints=%+v)", found, tc.wantSkill, hints)
			}
		})
	}
}

// TestProactiveRecallHint_ListSkillsFailureIsNonFatal documents the
// best-effort contract for the skill scan. The implementation guards
// the skill loop on `if skillErr == nil`; a controlled fault
// injection against the production code requires either a method-level
// seam or a test-only hook that does not exist in this codebase.
// Rather than contort the production code to permit fault injection,
// we leave the contract verifiable via code review (directive_tools.go
// line `if skillErr == nil { ... }`) and surface the intent here.
