package internal

import (
	"strconv"
	"strings"
	"testing"
)

func TestGatherWakeContext_IncludesAvailableSkills(t *testing.T) {
	dm := NewTestDM(t)
	insertRawSkill(t, dm, "skill:agentshell-v1.0.0", "agentshell", "1.0.0", "---\nname: agentshell\nversion: 1.0.0\nwhen_to_use: agentshell, theme\n---\nbody")
	data, err := dm.GatherWakeContext()
	if err != nil {
		t.Fatalf("GatherWakeContext: %v", err)
	}
	if len(data.AvailableSkills) != 1 {
		t.Fatalf("got %d skills, want 1", len(data.AvailableSkills))
	}
	out := formatWakeContext(data)
	if !strings.Contains(out, "**Available Skills (count=1):**") || !strings.Contains(out, "agentshell v1.0.0: agentshell, theme") {
		t.Fatalf("missing skills block: %s", out)
	}
}

func TestFormatWakeContext_AvailableSkillsMarksShared(t *testing.T) {
	out := formatWakeContext(WakeContextData{AvailableSkills: []SkillSummary{{
		Name: "shared-skill", Version: "1.0.0", WhenToUse: "shared work", IsGlobal: true,
	}}})
	if !strings.Contains(out, "shared-skill v1.0.0: shared work [shared]") {
		t.Fatalf("missing shared marker: %s", out)
	}
}

func TestGatherWakeContext_AvailableSkillsBoundedToTop20(t *testing.T) {
	dm := NewTestDM(t)
	for i := 0; i < 25; i++ {
		name := "bulk-" + strconv.Itoa(i)
		insertRawSkill(t, dm, "skill:"+name+"-v1.0.0", name, "1.0.0", "---\nname: "+name+"\nversion: 1.0.0\n---\nbody")
	}
	if got := len(populateAvailableSkills(dm, "all")); got != 20 {
		t.Errorf("got %d, want 20", got)
	}
}
