package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/flowbyte-com/mpm-core/config"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

func handleStance(args []string) int {
	if len(args) < 1 {
		return respond("", "Usage: mpm ops stance assume|synthesize ...\n", 1)
	}
	sub := args[0]
	switch sub {
	case "assume":
		return handleStanceAssume(args[1:])
	case "synthesize":
		return handleStanceSynthesize(args[1:])
	default:
		return respond("", "Usage: mpm ops stance assume|synthesize ...\n", 1)
	}
}

// handleStanceAssume hot-swaps an existing persona when auto is active.
// CLI: mpm ops stance assume <mode> <persona> <rationale>
func handleStanceAssume(args []string) int {
	if len(args) < 3 {
		return respond("", "Usage: mpm ops stance assume <mode> <persona> <rationale>\n", 1)
	}
	mode := args[0]
	persona := args[1]
	rationale := strings.Join(args[2:], " ")

	if status := mpminternal.CheckAutoActive(); !status.Active {
		return respond("", fmt.Sprintf("Error: cannot assume stance — %s\n", status.Reason), 1)
	}

	if getDB() == nil {
		return 1
	}
	// DeleteEphemeralPersona takes a concrete *DatabaseManager, not
	// the CoreDB interface. The singleton is always a *DatabaseManager,
	// so the type assertion is safe; the nil check above guards it.
	dm := getDB().(*mpminternal.DatabaseManager)

	// Collision rule: clear any existing ephemeral_persona
	mpminternal.DeleteEphemeralPersona(dm)

	// Read active.json
	active, err := mpminternal.LoadActiveJSON()
	if err != nil {
		return respond("", fmt.Sprintf("Error reading active.json: %v\n", err), 1)
	}

	// Update mode/persona; leave the unspecified one as-is
	if mode != "-" {
		active.Modes = []string{mode}
	}
	if persona != "-" {
		active.Persona = persona
	}
	active.Updated = time.Now().UTC().Format(time.RFC3339)
	if err := mpminternal.SaveActiveJSON(active); err != nil {
		return respond("", fmt.Sprintf("Error saving active.json: %v\n", err), 1)
	}

	// Audit trail: log to decisions table
	auditContent := fmt.Sprintf("CONTEXT: Stance hot-swap to %s/%s. RATIONALE: %s. CHOICE: assume_stance", mode, persona, rationale)
	dm.SaveMemory("decisions", auditContent, "", nil, nil, nil, false, 1)

	// Inject new directive into LLM context mid-session via stdout.
	// OpenClaw captures tool stdout and injects it into the session chat history,
	// so 808 reads this on the very next turn and hot-swaps without restart.
	directive := GetSystemPrompt()
	fmt.Print("\n[SYSTEM NOTIFICATION: STANCE HOT-SWAPPED]\n")
	fmt.Print("You must immediately adopt the following Mode and Persona directives for the remainder of this session:\n\n")
	fmt.Print(directive)
	fmt.Print("\n")

	return respond(fmt.Sprintf("Stance assumed: mode=%s persona=%s\n", mode, persona), "", 0)
}

// handleStanceSynthesize generates a JIT (ephemeral) persona when auto is active.
// CLI: mpm ops stance synthesize <name> --title <t> --creature <c> --vibe <v> --voice <v> --anti-patterns <a>
func handleStanceSynthesize(args []string) int {
	if len(args) < 1 {
		return respond("", "Usage: mpm ops stance synthesize <name> [flags]\n", 1)
	}

	name := args[0]
	title := ""
	creature := ""
	vibe := ""
	voice := ""
	antiPatterns := ""

	i := 1
	for i < len(args) {
		switch args[i] {
		case "--title":
			if i+1 < len(args) {
				i++
				title = args[i]
			}
		case "--creature":
			if i+1 < len(args) {
				i++
				creature = args[i]
			}
		case "--vibe":
			if i+1 < len(args) {
				i++
				vibe = args[i]
			}
		case "--voice":
			if i+1 < len(args) {
				i++
				voice = args[i]
			}
		case "--anti-patterns":
			if i+1 < len(args) {
				i++
				antiPatterns = args[i]
			}
		}
		i++
	}

	if status := mpminternal.CheckAutoActive(); !status.Active {
		return respond("", fmt.Sprintf("Error: cannot synthesize stance — %s\n", status.Reason), 1)
	}

	dm := getDBConcrete()
	if dm == nil {
		return 1
	}

	ep := &mpminternal.EphemeralPersona{
		Name:         name,
		Title:        title,
		Creature:     creature,
		Vibe:         vibe,
		Voice:        voice,
		AntiPatterns: antiPatterns,
	}

	// Idempotent upsert into system_config
	if err := mpminternal.SaveEphemeralPersona(dm, ep); err != nil {
		return respond("", fmt.Sprintf("Error saving ephemeral persona: %v\n", err), 1)
	}

	// Update active.json to point to ephemeral
	active, err := mpminternal.LoadActiveJSON()
	if err != nil {
		return respond("", fmt.Sprintf("Error reading active.json: %v\n", err), 1)
	}
	active.Persona = "ephemeral"
	active.Updated = time.Now().UTC().Format(time.RFC3339)
	if err := mpminternal.SaveActiveJSON(active); err != nil {
		return respond("", fmt.Sprintf("Error saving active.json: %v\n", err), 1)
	}

	displayName := name
	if title != "" {
		displayName = title
	}

	// Inject new directive into LLM context mid-session via stdout.
	// OpenClaw captures tool stdout and injects it into the session chat history,
	// so 808 reads this on the very next turn and hot-swaps without restart.
	directive := GetSystemPrompt()
	fmt.Print("\n[SYSTEM NOTIFICATION: STANCE HOT-SWAPPED]\n")
	fmt.Print("You must immediately adopt the following Mode and Persona directives for the remainder of this session:\n\n")
	fmt.Print(directive)
	fmt.Print("\n")

	return respond(fmt.Sprintf("Synthesized ephemeral persona: %q (active: ephemeral)\n", displayName), "", 0)
}

// handleOpsPromote promotes an ephemeral persona to a permanent persona file.
// CLI: mpm ops promote
// Atomicity: 1) fetch ephemeral_persona 2) write file 3) clear sysconfig 4) update active.json
func handleOpsPromote() int {
	dm := getDBConcrete()
	if dm == nil {
		return 1
	}

	ep, err := mpminternal.GetEphemeralPersona(dm)
	if err != nil {
		return respond("", "No ephemeral persona found. Nothing to promote.\n", 0)
	}

	// Format as markdown and write to personas/<name>.md
	md := mpminternal.FormatEphemeralPersonaAsMarkdown(ep)
	personaDir := filepath.Join(config.GetMPMDir(), "persona")
	if err := os.MkdirAll(personaDir, 0755); err != nil {
		return respond("", fmt.Sprintf("Error creating persona directory: %v\n", err), 1)
	}
	path := filepath.Join(personaDir, ep.Name+".md")
	if err := os.WriteFile(path, []byte(md), 0644); err != nil {
		return respond("", fmt.Sprintf("Error writing persona file: %v\n", err), 1)
	}

	// Only clear ephemeral_persona if file write succeeded
	if err := mpminternal.DeleteEphemeralPersona(dm); err != nil {
		// Log warning but don't fail — the file was written successfully
		slog.Warn("failed to clear ephemeral persona after promote", "error", err.Error())
	}

	// Update active.json to point to the newly permanent persona
	active, err := mpminternal.LoadActiveJSON()
	if err != nil {
		return respond("", fmt.Sprintf("Error reading active.json: %v\n", err), 1)
	}
	active.Persona = ep.Name
	active.Updated = time.Now().UTC().Format(time.RFC3339)
	if err := mpminternal.SaveActiveJSON(active); err != nil {
		return respond("", fmt.Sprintf("Error saving active.json: %v\n", err), 1)
	}

	return respond(fmt.Sprintf("Promoted ephemeral persona %q → %s\n", ep.Name, path), "", 0)
}
