// cmd/mpm/handlers_help.go — Progressive disclosure help system
// (cognitive-interface Wave 1 commit 4).
//
// Per RFC §4:
//
//   Default help should expose approximately 8 primary commands.
//   Everything else should remain accessible via:
//     mpm help <section>
//     mpm help --all
//
// Implementation:
//
//   mpm help             → printCognitiveHelp (8 primary cognitive verbs)
//   mpm help --all       → printHelpAll (full Commands catalogue dump)
//   mpm help <section>   → printSectionHelp(section) —
//                            knowledge | runtime | maintenance |
//                            reflection | work | explain
//   mpm help <command>   → existing per-command help from router.go
//
// The default `mpm help` deliberately shows fewer commands than the
// previous implementation. Operators who want the long-form catalogue
// opt in via `--all`. The cognitive-interface principle: discoverability
// first; the long form is the escape hatch, not the default.

package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// cognitiveHelpSections is the default cognitive-interface help.
// Each section is small; the whole output fits in a terminal.
// Sections in display order. Future waves add commands here as
// their primary cognitive verb aliases land (commit 5 of Wave 1).
var cognitiveHelpSections = []cogHelpSection{
	{
		title: "Daily",
		cmds: []helpCmd{
			{"continue", "Session resumption dashboard (composes Working Context + Wake Context + Memory Stats)", true},
			{"work", "Working Context — ephemeral execution state", true},
			{"call", "Universal MCP-tool boundary (use for everything not yet aliased as a cognitive verb)", false},
			{"status", "System status dashboard (--json for machine output)", false},
			{"version", "Show mpm version + build info", false},
		},
	},
	{
		title: "Knowledge",
		cmds: []helpCmd{
			{"kb memory", "Memory CRUD (list|show|search|shred|add|reinforce|weaken|snooze|set-weight|promote|patch-memory)", true},
			{"kb lesson", "Lesson CRUD (curated insights from memories)", true},
			{"kb skill", "Skill CRUD (procedural memory with --file frontmatter)", true},
			{"kb topic", "Topic CRUD", true},
			{"kb reference", "Reference library CRUD", true},
			{"kb theory", "Theories (pending|resolved|all)", false},
			{"kb decision", "Decision ledger (record|recall)", false},
		},
	},
	{
		title: "Engine Room",
		cmds: []helpCmd{
			{"ops", "Maintenance, diagnostics, synthesis, and power tools", true},
		},
	},
	{
		title: "Debug",
		cmds: []helpCmd{
			{"debug", "Low-level inspection tools", true},
		},
	},
	{
		title: "Need more?",
		cmds: []helpCmd{
			{"mpm help knowledge", "Expanded view of knowledge surface (memory|lesson|skill|topic|reference)", false},
			{"mpm help runtime", "Wave 2 — wake, doctor, why", false},
			{"mpm help maintenance", "Wave 2+ — backup, restore, review", false},
			{"mpm help reflection", "Wave 2+ — synthesize, confidence, evidence, milestones", false},
			{"mpm help work", "Working Context subcommands (status|show|clear|promote)", false},
			{"mpm help --all", "Full catalogue dump (operator interface + aliases)", false},
		},
	},
}

// cogHelpSection is the new progressive-disclosure section type.
// Renamed from helpSection (which collides with main.go's lipgloss.Style
// of the same name) so this file is self-contained without touching
// the existing printHelp() implementation in main.go.
type cogHelpSection struct {
	title string
	cmds  []helpCmd
}

// printCognitiveHelp prints the default cognitive-interface help —
// ~22 commands across 5 sections, all visible in one terminal screen
// for ~80 columns wide. The "Need more?" section is the progressive-
// disclosure pointer (RFC §4): operators naturally discover the long
// form when they need it.
func printCognitiveHelp() {
	fmt.Println(renderCognitiveHelp())
}

// renderCognitiveHelp returns the help text as a string (used by both
// the print and the test paths).
func renderCognitiveHelp() string {
	titleStyle := lipgloss.NewStyle().Foreground(helpGold).Bold(true).Align(lipgloss.Center)
	subtitleStyle := lipgloss.NewStyle().Foreground(helpCyan).Align(lipgloss.Center)
	border := lipgloss.NewStyle().BorderStyle(lipgloss.RoundedBorder()).BorderForeground(helpBorder).Padding(1, 2).Margin(1)

	var content string
	content = "\n" + titleStyle.Render(" mpm  ·  Cognitive Interface") + "\n"
	content += subtitleStyle.Render("Your long-term memory, always within reach") + "\n"
	for _, sec := range cognitiveHelpSections {
		content += "\n" + helpSection.Render(sec.title) + "\n"
		for _, c := range sec.cmds {
			if c.hasSubs {
				content += fmt.Sprintf("  ▸  %s    %s\n",
					helpCommand.Render(fmt.Sprintf("%-22s", c.name)),
					helpDesc.Render(c.desc))
			} else {
				content += fmt.Sprintf("      %s    %s\n",
					helpCommand.Render(fmt.Sprintf("%-22s", c.name)),
					helpDesc.Render(c.desc))
			}
		}
	}
	return border.Render(content)
}

// printHelpAll dumps the full Commands catalogue as a flat list.
// Sorts alphabetically for stable output (operators can grep easily).
// This is the "operator interface" — every command the binary
// supports. Tools that want this should use `mpm help --json` (future)
// rather than parsing this text — text help is not an API (RFC §4).
func printHelpAll() {
	if r := getCmdRouter(); r != nil {
		cmds := make([]string, 0, len(r.Commands))
		for name, c := range r.Commands {
			cmds = append(cmds, fmt.Sprintf("%-22s %s", name, c.Description))
		}
		// sort.Strings is the boring option but works without imports.
		sortStrings(cmds)
		fmt.Println("Full command catalogue (operator interface):")
		fmt.Println(strings.Repeat("─", 60))
		for _, line := range cmds {
			fmt.Println("  " + line)
		}
		fmt.Println()
		fmt.Printf("Total: %d commands. Use 'mpm help <section>' for progressive disclosure,\n", len(cmds))
		fmt.Println("or 'mpm help' for the cognitive-verb default.")
	}
}

// sectionHelpContent returns the expanded section text. Each section
// is small enough to fit comfortably. Future waves add commands as
// their alias surfaces ship.
func sectionHelpContent(section string) (string, bool) {
	switch strings.ToLower(section) {
	case "knowledge":
		return `Knowledge — entity-centric memory interface

  All commands route through the kb command:

    mpm kb memory   list|show|search|shred
                    add|reinforce|weaken|snooze|set-weight|promote
                    patch-memory
    mpm kb lesson   list|show|search|shred|add
    mpm kb skill    list|show|read|save
    mpm kb topic    list|show|search|shred|add|link
    mpm kb reference list|show|search|shred|add
    mpm kb theory    [pending|resolved|all]
    mpm kb decision  record-decision
    mpm kb hint      <topic>

  The full module is 'mpm ops kb help' (the ops engine-room backend).
  This is the cognitive-verb front door.`, true

	case "runtime":
		return `Runtime — session and process state

  Wave 2 surface (this section expands when those commands ship):

    mpm wake              Last session context
    mpm doctor            Substrate health signals (Wave 2)
    mpm why <id>          Provenance + evidence + confidence (Wave 2)
    mpm continue          Session-resumption dashboard (Wave 1 commit 3)

  Today only ` + "`continue`" + ` and ` + "`wake`" + ` ship. The rest land
  in Wave 2 once Wave 1's composition discipline is validated.`, true

	case "maintenance":
		return `Maintenance — keep the substrate healthy

    mpm ops backup [path]          Database backup (.sql dump)
    mpm ops restore <path>          Restore from JSON export
    mpm ops restore-db <path>      Restore from .sql dump
    mpm ops review [--stale]       Spaced reinforcement review
    mpm ops gc [--dry-run]         Memory decay sweep
    mpm ops maintain               Self-maintenance: decay + consolidate + prune

  Maintenance commands live under ` + "`mpm ops`" + ` so the cognitive
  default help stays small. Future waves may promote the most-used
  commands (backup, review) to root-level aliases.`, true

	case "reflection":
		return `Reflection — observability and synthesis surface (Wave 2+)

    mpm ops synthesize [--dry-run] LLM synthesis on all memories
    mpm ops confidence             Confidence / evidence engine
    mpm ops evidence <add|list>    Attach observations / decisions
    mpm ops broadcast <id>         Fan-out epistemic events
    mpm ops milestones             List narrative milestones

  These land in Wave 2+ per the cognitive-interface RFC Wave plan.`, true

	case "work":
		return `Working Context — the agent's ephemeral execution state

  This is the cognitive-verb front door to the substrate's scratchpad
  mechanism. The internal MCP tool names (flush_scratchpad etc.)
  remain stable — operators who want them continue to use
  ` + "`mpm call <mcp-tool>`" + `.

  Subcommands:

    mpm work status        One-line status (session, age, expires)
    mpm work show          Raw context, exactly as stored (no interpretation)
    mpm work clear         Discard the current working context
    mpm work promote       Promote working context to a permanent memory

  Flags:

    --session-id <id>     Override the per-process session id
                          (default: getOrMakeSessionID() each invocation)
    --json                Emit machine-readable JSON (where applicable)

  Architecture: ` + "`mpm work`" + ` is a thin adapter composing the
  WorkingContextService + WorkingContextStore + Renderers. It owns
  no SQL, no rendering, no behaviour. See RFC §'mpm work' for the
  full layering contract.`, true

	case "explain":
		return `Explain — provenance + introspection (Wave 2+)

  Wave 2 surface (this section expands when those commands ship):

    mpm why <id>          Single-level provenance for any artifact
    mpm explain <kind>    Retrieval explanation per kind
    mpm doctor            Trust signals across substrate subsystems

  Today's ` + "`mpm why`" + ` is available via the MCP boundary as
  ` + "`mpm call explain_retrieval`" + ` and ` + "`mpm call list_evidence`" + `. The CLI wrapper ships in Wave 2.`, true

	default:
		return "", false
	}
}

// printSectionHelp prints the expanded view for a single section. If
// the section name doesn't match a known section, returns false so
// the caller can fall back to existing per-command help dispatch.
func printSectionHelp(section string) bool {
	body, ok := sectionHelpContent(section)
	if !ok {
		return false
	}
	fmt.Println(body)
	return true
}

// getCmdRouter is the accessor for the package-level Commands map.
// Defined in router.go's NewRouter(). This accessor exists so
// handlers_help.go can dump the catalogue without reaching into
// router internals.
func getCmdRouter() *CommandRouter {
	return NewRouter() // each call re-builds the map; trivial cost
}

// sortStrings is a tiny in-place alphabetical sort. Used by printHelpAll
// to keep --all output deterministic (so scripts can grep).
func sortStrings(s []string) {
	// Insertion sort — N is small (~57 commands), good enough.
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j-1] > s[j]; j-- {
			s[j-1], s[j] = s[j], s[j-1]
		}
	}
}

// touch os for build-safety; future Wave 2 may add --json output here.
var _ = os.Stderr
