// wake_skills.go — Populate the <available_skills> block on wake.

package internal

// TopSkillsInWake is the cap for the wake-context skills catalogue.
const TopSkillsInWake = 20

// populateAvailableSkills fetches the top-N skill summaries for the
// wake-context block.
func populateAvailableSkills(dm *DatabaseManager, scope string) []SkillSummary {
	all, err := dm.ListSkills(scope)
	if err != nil {
		return nil
	}
	if len(all) > TopSkillsInWake {
		all = all[:TopSkillsInWake]
	}
	return all
}
