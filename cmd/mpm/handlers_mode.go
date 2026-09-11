package main

import (
	"fmt"
	"strings"

	"github.com/flowbyte-com/mpm-core"
)

// ============================================================================
// Handler: mode
// ============================================================================

func handleMode(args []string) int {
	if len(args) < 1 {
		return handleModeSelect()
	}

	subCmd := args[0]
	switch subCmd {
	case "help":
		return handleModeHelp()
	case "list":
		return handleModeList()
	case "show":
		return handleModeShow(args[1:])
	case "active":
		return handleModeActive()
	case "add":
		return handleModeAdd(args[1:])
	case "remove":
		return handleModeRemove(args[1:])
	case "clear":
		return handleModeClear()
	case "select":
		// Interactive mode selection via fzf
		return handleModeSelect()
	default:
		return handleModeHelp()
	}
}

func handleModeHelp() int {
	output := `mpm mode - Mode operations

Usage:
  mpm mode                   Interactive multi-mode selection (TUI, auto-compiles)
  mpm mode list              List available modes
  mpm mode show <name>       Inspect a mode definition (frontmatter + body)
  mpm mode active            Show active modes (plural)
  mpm mode add <name>        Add a mode to active list
  mpm mode remove <name>     Remove a mode from active list
  mpm mode clear             Clear all active modes (explicit, not fallback)

Examples:
  mpm mode                   # Pick multiple modes, auto-compiles
  mpm mode add developer
  mpm mode remove developer
  mpm mode show debugging    # Print frontmatter + body of mode/debugging.md
  mpm mode clear              # After this, wake shows active_modes=[]

Notes:
  • 'add' is idempotent — adding an already-active mode is a no-op.
  • Multiple modes can be active simultaneously (e.g. add debugging
    then add forensic — both are preserved in active.modes array).
  • 'clear' writes [] to active.json — wake context distinguishes
    explicit-clear (source=empty) from bootstrap (source=fallback).
  • 'show' reads the actual .md on disk; README.md is never selectable.
`
	return respond(output, "", 0)
}

func handleModeList() int {
	mm := internal.NewModeManager("")
	modes, err := mm.List()
	if err != nil {
		return respond("", fmt.Sprintf("Failed to list modes: %v", err), 1)
	}

	var output strings.Builder
	output.WriteString(fmt.Sprintf("Available modes (%d):\n\n", len(modes)))

	for _, m := range modes {
		name := m.Name
		if name == "" {
			name = m.Title // fallback to title for modes that use title instead of name
		}
		output.WriteString(fmt.Sprintf("  %s\n", name))
		if m.Description != "" {
			output.WriteString(fmt.Sprintf("      %s\n", m.Description))
		}
	}

	return respond(output.String(), "", 0)
}

func handleModeActive() int {
	mm := internal.NewModeManager("")
	modes, err := mm.GetActive()
	if err != nil {
		return respond("", fmt.Sprintf("Failed to get active modes: %v", err), 1)
	}

	if len(modes) == 0 {
		return respond("No active modes.\n", "", 0)
	}

	var output strings.Builder
	output.WriteString("Active modes:\n\n")
	for _, m := range modes {
		output.WriteString(fmt.Sprintf("  %s\n", m))
	}

	return respond(output.String(), "", 0)
}

func handleModeAdd(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm mode add <name>", 1)
	}

	name := args[0]
	mm := internal.NewModeManager("")

	// Validate the mode exists on disk before adding to the active list.
	if !mm.Validate(name) {
		return respond("", fmt.Sprintf("Unknown mode: %s\n", name), 1)
	}

	// Read current active list, append, write back. SetActive() also
	// mirrors the first real mode to config/current_mode for memory injection.
	active, _ := mm.GetActive()
	for _, m := range active {
		if m == name {
			return respond(fmt.Sprintf("Mode already active: %s\n", name), "", 0)
		}
	}
	active = append(active, name)
	if err := mm.SetActive(active); err != nil {
		return respond("", fmt.Sprintf("Failed to add mode: %v", err), 1)
	}
	return respond(fmt.Sprintf("Mode added: %s\n", name), "", 0)
}

func handleModeRemove(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm mode remove <name>", 1)
	}

	name := args[0]
	mm := internal.NewModeManager("")

	// Read current active list, drop the named one, write back. Does NOT
	// delete the .md file — manage those on the file system. SetActive()
	// also mirrors the next real mode to config/current_mode.
	active, _ := mm.GetActive()
	out := active[:0]
	found := false
	for _, m := range active {
		if m == name {
			found = true
			continue
		}
		out = append(out, m)
	}
	if !found {
		return respond("", fmt.Sprintf("Mode not active: %s\n", name), 1)
	}
	if err := mm.SetActive(out); err != nil {
		return respond("", fmt.Sprintf("Failed to remove mode: %v", err), 1)
	}
	return respond(fmt.Sprintf("Mode removed: %s\n", name), "", 0)
}

func handleModeClear() int {
	mm := internal.NewModeManager("")

	err := mm.ClearModes()
	if err != nil {
		return respond("", fmt.Sprintf("Failed to clear modes: %v", err), 1)
	}

	return respond("All modes cleared.\n", "", 0)
}

// handleModeShow reads the mode <name>.md file from the canonical
// mode/ directory and prints its frontmatter (key=value) plus body.
// Mirrors handleModeList's "list is filesystem-backed" invariant —
// there is no hard-coded mode vocabulary. README.md is rejected
// unconditionally by the manager's IsDefinitionFile gate before this
// function is reached.
func handleModeShow(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm mode show <name>\n", 1)
	}
	name := args[0]

	// Reject README-style names at the CLI layer too — defense in depth.
	if internal.IsDocumentationFile(name) || internal.IsDocumentationFile(name+".md") {
		return respond("", fmt.Sprintf("Not a selectable mode: %q (documentation file)\n", name), 1)
	}

	mm := internal.NewModeManager("")
	m, err := mm.Get(name)
	if err != nil {
		return respond("", fmt.Sprintf("Mode not found: %s\n", name), 1)
	}
	var b strings.Builder
	displayName := m.Name
	if displayName == "" {
		displayName = m.Title
	}
	b.WriteString(fmt.Sprintf("# Mode: %s\n\n", displayName))
	if m.Title != "" && m.Title != displayName {
		b.WriteString(fmt.Sprintf("Title:    %s\n", m.Title))
	}
	if m.Version != "" {
		b.WriteString(fmt.Sprintf("Version:  %s\n", m.Version))
	}
	if m.Status != "" {
		b.WriteString(fmt.Sprintf("Status:   %s\n", m.Status))
	}
	if m.Description != "" {
		b.WriteString(fmt.Sprintf("Description: %s\n", m.Description))
	}
	if m.Purpose != "" {
		b.WriteString(fmt.Sprintf("Purpose:  %s\n", m.Purpose))
	}
	if m.Checklist != "" {
		b.WriteString(fmt.Sprintf("Checklist: %s\n", m.Checklist))
	}
	if m.AntiPatterns != "" {
		b.WriteString(fmt.Sprintf("Anti-patterns: %s\n", m.AntiPatterns))
	}
	if m.Tools != "" {
		b.WriteString(fmt.Sprintf("Tools:    %s\n", m.Tools))
	}
	if m.Content != "" {
		b.WriteString("\n---\n\n")
		b.WriteString(m.Content)
		if !strings.HasSuffix(m.Content, "\n") {
			b.WriteString("\n")
		}
	}
	return respond(b.String(), "", 0)
}

func handleModeSelect() int {
	mm := internal.NewModeManager("")
	modes, err := mm.List()
	if err != nil {
		return respond("", fmt.Sprintf("Failed to list modes: %v", err), 1)
	}

	if len(modes) == 0 {
		return respond("No modes available.\n", "", 0)
	}

	// Get currently active modes
	activeModes, _ := mm.GetActive()
	activeSet := make(map[string]bool)
	for _, m := range activeModes {
		activeSet[m] = true
	}

	// Build selector items
	items := make([]selectorItem, 0, len(modes))
	for _, m := range modes {
		subtitle := ""
		if activeSet[m.Name] {
			subtitle = "[active]"
		}
		items = append(items, selectorItem{name: m.Name, subtitle: subtitle})
	}

	// Run PTY selector (multi-select)
	selected, err := runSelectorPTY(items, true, activeSet)
	if err != nil {
		return respond("", fmt.Sprintf("Selector error: %v", err), 1)
	}

	if len(selected) == 0 {
		return respond("No modes selected.\n", "", 0)
	}

	// Set new active modes
	err = mm.SetActive(selected)
	if err != nil {
		return respond("", fmt.Sprintf("Failed to set modes: %v", err), 1)
	}
	return respond(fmt.Sprintf("Active modes updated: %s\n", strings.Join(selected, ", ")), "", 0)

}
