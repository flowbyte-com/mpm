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
	case "show":
		return handlePersonaShow(args[1:])
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
  mpm persona show <name>       Inspect a persona definition (frontmatter + body)
  mpm persona active            Show active persona
  mpm persona set <name>        Set active persona
  mpm persona clear             Clear active persona (explicit, not fallback)

Examples:
  mpm persona                   # Pick one persona, auto-compiles
  mpm persona set 808
  mpm persona show critic        # Print frontmatter + body of persona/critic.md
  mpm persona clear              # After this, wake shows active_persona=""

Notes:
  • 'clear' sets persona to "" in active.json — wake context distinguishes
    explicit-clear (source=empty) from bootstrap (source=fallback).
  • 'show' reads the actual .md on disk; README.md is never selectable.
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

// handlePersonaShow reads the persona <name>.md file from the canonical
// persona/ directory and prints its frontmatter (key=value) plus body.
// Mirrors handlePersonaList's "list is filesystem-backed" invariant —
// there is no hard-coded persona vocabulary. README.md is rejected
// unconditionally by the manager's IsDefinitionFile gate before this
// function is reached.
func handlePersonaShow(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm persona show <name>\n", 1)
	}
	name := args[0]

	// Reject README-style names at the CLI layer too — defense in depth
	// on top of the manager's IsDefinitionFile filter.
	if internal.IsDocumentationFile(name) || internal.IsDocumentationFile(name+".md") {
		return respond("", fmt.Sprintf("Not a selectable persona: %q (documentation file)\n", name), 1)
	}

	pm := internal.NewPersonaManager("")
	p2, err := pm.Get(name)
	if err != nil {
		return respond("", fmt.Sprintf("Persona not found: %s\n", name), 1)
	}
	var b strings.Builder
	b.WriteString(fmt.Sprintf("# Persona: %s\n\n", p2.Name))
	if p2.Title != "" {
		b.WriteString(fmt.Sprintf("Title:       %s\n", p2.Title))
	}
	if p2.Version != "" {
		b.WriteString(fmt.Sprintf("Version:     %s\n", p2.Version))
	}
	if p2.Status != "" {
		b.WriteString(fmt.Sprintf("Status:      %s\n", p2.Status))
	}
	if p2.Description != "" {
		b.WriteString(fmt.Sprintf("Description: %s\n", p2.Description))
	}
	if p2.Voice != "" {
		b.WriteString(fmt.Sprintf("Voice:       %s\n", p2.Voice))
	}
	if p2.Emoji != "" {
		b.WriteString(fmt.Sprintf("Emoji:       %s\n", p2.Emoji))
	}
	if p2.Content != "" {
		b.WriteString("\n---\n\n")
		b.WriteString(p2.Content)
		if !strings.HasSuffix(p2.Content, "\n") {
			b.WriteString("\n")
		}
	}
	return respond(b.String(), "", 0)
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
