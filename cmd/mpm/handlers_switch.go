package main

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

func handleSwitch(args []string) int {
	stat, _ := os.Stdin.Stat()
	if (stat.Mode() & os.ModeCharDevice) == 0 {
		fmt.Println("[!] Error: Interactive switch requires a TTY. Cannot run in headless/MCP mode.")
		return 1
	}

	active, err := mpminternal.LoadActiveJSON()
	if err != nil {
		fmt.Printf("[!] Error loading active.json: %v\n", err)
		return 1
	}

	fmt.Println("⚡ MPM Context Switcher")
	fmt.Println("─────────────────────────────────────────")
	fmt.Printf("Active Persona: %s\n", active.PersonaString())
	fmt.Printf("Active Modes:  %s\n", strings.Join(active.ModesSlice(), ", "))
	fmt.Println("─────────────────────────────────────────")

	fmt.Println("\nWhat do you want to change?")
	fmt.Println("  [1] Switch Persona")
	fmt.Println("  [2] Toggle Modes")
	fmt.Println("  [3] Exit")
	fmt.Print("\n> ")

	reader := bufio.NewReader(os.Stdin)
	line, err := reader.ReadString('\n')
	if err != nil {
		fmt.Println("[!] Read error")
		return 1
	}
	line = strings.TrimSpace(line)

	switch line {
	case "1":
		switchPersona(reader, active)
	case "2":
		toggleModes(reader, active)
	case "3":
		fmt.Println("No changes made.")
		return 0
	default:
		fmt.Println("[!] Invalid option")
		return 1
	}

	active.Updated = time.Now().UTC().Format(time.RFC3339)
	if err := mpminternal.SaveActiveJSON(active); err != nil {
		fmt.Printf("[!] Error saving: %v\n", err)
		return 1
	}

	fmt.Printf("\n⚡ Context updated: [Persona: %s] | [Modes: %s]\n",
		active.PersonaString(), strings.Join(active.ModesSlice(), ", "))
	return 0
}

func switchPersona(reader *bufio.Reader, active *mpminternal.ActiveState) {
	personas := getPersonaFiles()
	if len(personas) == 0 {
		fmt.Println("[!] No persona files found in persona/")
		return
	}

	fmt.Println("\nAvailable Personas:")
	currentPersona := active.PersonaString()
	for i, p := range personas {
		marker := ""
		if p == currentPersona {
			marker = " (current)"
		}
		fmt.Printf("  [%d] %s%s\n", i+1, p, marker)
	}
	fmt.Print("\nSelect persona (number): ")

	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(line)
	idx, err := strconv.Atoi(line)
	if err != nil || idx < 1 || idx > len(personas) {
		fmt.Println("[!] Invalid selection — no change made.")
		return
	}
	// Pointer assignment: explicit selection (vs nil = absent).
	p := personas[idx-1]
	active.Persona = &p
}

func toggleModes(reader *bufio.Reader, active *mpminternal.ActiveState) {
	modes := getModeFiles()
	if len(modes) == 0 {
		fmt.Println("[!] No mode files found in mode/")
		return
	}

	fmt.Println("\nAvailable Modes (enter numbers separated by commas, e.g. 1,3):")
	activeMap := make(map[string]bool)
	for _, m := range active.ModesSlice() {
		activeMap[m] = true
	}

	for i, m := range modes {
		marker := ""
		if activeMap[m] {
			marker = " [*]"
		}
		fmt.Printf("  [%d] %s%s\n", i+1, m, marker)
	}
	fmt.Print("\nSelect modes: ")

	line, _ := reader.ReadString('\n')
	line = strings.TrimSpace(line)

	selected := parseModeSelection(line, modes)
	if selected == nil {
		fmt.Println("[!] Invalid selection — no change made.")
		return
	}
	// Pointer assignment: explicit selection (vs nil = absent). An empty
	// selection (selected==nil or len==0) becomes &[]string{} = explicit clear.
	if selected == nil {
		empty := []string{}
		active.Modes = &empty
	} else {
		s := selected
		active.Modes = &s
	}
}