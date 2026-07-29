// skills_test.go — Task 14 of the skills-layer plan.

package seed

import (
	"strings"
	"testing"
)

func TestSeedSkills_RegistryNonEmpty(t *testing.T) {
	if len(SeedSkills) == 0 {
		t.Fatal("SeedSkills registry is empty")
	}
	for _, s := range SeedSkills {
		if s.StableID == "" {
			t.Error("SeedSkill has empty StableID")
		}
		if s.Name == "" {
			t.Errorf("SeedSkill %s has empty Name", s.StableID)
		}
		if s.Version == "" {
			t.Errorf("SeedSkill %s has empty Version", s.StableID)
		}
		if s.WhenToUse == "" {
			t.Errorf("SeedSkill %s has empty WhenToUse", s.StableID)
		}
		if !strings.Contains(s.Content, "name: "+s.Name) {
			t.Errorf("SeedSkill %s content doesn't declare name %q", s.StableID, s.Name)
		}
		if !strings.Contains(s.Content, "version:") {
			t.Errorf("SeedSkill %s content doesn't declare version", s.StableID)
		}
	}
}

func TestSeedSkills_StableIDsUnique(t *testing.T) {
	seen := make(map[string]bool)
	for _, s := range SeedSkills {
		if seen[s.StableID] {
			t.Errorf("duplicate StableID: %s", s.StableID)
		}
		seen[s.StableID] = true
	}
}
