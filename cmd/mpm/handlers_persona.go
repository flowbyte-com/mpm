package main

import (
	"fmt"
	"strings"

	"github.com/flowbyte-com/mpm-core"
)

func handlePersona(args []string) int {
	if len(args) < 1 {
		return handlePersonaSelect()
	}

	subCmd := args[0]
	switch subCmd {
	case "help":
		return handlePersonaHelp()
	case "list":
		return handlePersonaList()
	case "active":
		return handlePersonaActive()
	case "set":
		return handlePersonaSet(args[1:])
	case "clear":
		return handlePersonaClear()
	case "select":
		// Interactive persona selection via fzf
		return handlePersonaSelect()
	default:
		return handlePersonaHelp()
	}
}

func handlePersonaHelp() int {
	output := `mpm persona - Persona operations

Usage:
  mpm persona                   Interactive persona selection (TUI, auto-compiles)
  mpm persona list              List available personas
  mpm persona active            Show active persona
  mpm persona set <name>        Set active persona
  mpm persona clear             Clear active persona

Examples:
  mpm persona                   # Pick one persona, auto-compiles
  mpm persona set 808
`
	return respond(output, "", 0)
}

func handlePersonaList() int {
	pm := internal.NewPersonaManager("")
	personas, err := pm.List()
	if err != nil {
		return respond("", fmt.Sprintf("Failed to list personas: %v", err), 1)
	}

	var output strings.Builder
	output.WriteString(fmt.Sprintf("Available personas (%d):\n\n", len(personas)))

	for _, p := range personas {
		output.WriteString(fmt.Sprintf("  %s\n", p.Name))
		if p.Description != "" {
			output.WriteString(fmt.Sprintf("      %s\n", p.Description))
		}
	}

	return respond(output.String(), "", 0)
}

func handlePersonaActive() int {
	pm := internal.NewPersonaManager("")
	persona, err := pm.GetActive()
	if err != nil {
		return respond("", fmt.Sprintf("Failed to get active persona: %v", err), 1)
	}

	if persona == "" {
		return respond("No active persona.\n", "", 0)
	}

	return respond(fmt.Sprintf("Active persona: %s\n", persona), "", 0)
}

func handlePersonaSet(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm persona set <name>", 1)
	}

	name := args[0]
	pm := internal.NewPersonaManager("")

	// Verify persona exists
	_, err := pm.Get(name)
	if err != nil {
		return respond("", fmt.Sprintf("Persona not found: %s\n", name), 1)
	}

	err = pm.SetActive(name)
	if err != nil {
		return respond("", fmt.Sprintf("Failed to set persona: %v", err), 1)
	}
	return respond(fmt.Sprintf("Persona set: %s\n", name), "", 0)
}

func handlePersonaClear() int {
	pm := internal.NewPersonaManager("")

	err := pm.SetActive("")
	if err != nil {
		return respond("", fmt.Sprintf("Failed to clear persona: %v", err), 1)
	}

	return respond("Persona cleared.\n", "", 0)
}

func handlePersonaSelect() int {
	pm := internal.NewPersonaManager("")
	personas, err := pm.List()
	if err != nil {
		return respond("", fmt.Sprintf("Failed to list personas: %v", err), 1)
	}

	if len(personas) == 0 {
		return respond("No personas available.\n", "", 0)
	}

	// Get currently active persona
	activePersona, _ := pm.GetActive()
	activeSet := map[string]bool{activePersona: true}

	// Build selector items
	items := make([]selectorItem, 0, len(personas))
	for _, p := range personas {
		subtitle := ""
		if p.Name == activePersona {
			subtitle = "[active]"
		}
		items = append(items, selectorItem{name: p.Name, subtitle: subtitle})
	}

	// Run PTY selector (single-select for persona)
	selected, err := runSelectorPTY(items, false, activeSet)
	if err != nil {
		return respond("", fmt.Sprintf("Selector error: %v", err), 1)
	}

	if len(selected) == 0 {
		return respond("No persona selected.\n", "", 0)
	}

	// Set new active persona
	err = pm.SetActive(selected[0])
	if err != nil {
		return respond("", fmt.Sprintf("Failed to set persona: %v", err), 1)
	}
	return respond(fmt.Sprintf("Persona set: %s\n", selected[0]), "", 0)

}
