package main

import (
	"fmt"
	"strings"

	"mpm/internal"
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
  mpm mode active            Show active modes
  mpm mode add <name>        Add a mode to active list
  mpm mode remove <name>     Remove a mode from active list
  mpm mode clear             Clear all active modes

Examples:
  mpm mode                   # Pick multiple modes, auto-compiles
  mpm mode add developer
  mpm mode remove developer
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
