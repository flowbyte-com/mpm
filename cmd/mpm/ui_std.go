//go:build !808

package main

import (
	"fmt"
	"os"
	"strings"
)

// =============================================================================
// Brand Constants (Standard Edition)
// =============================================================================

const (
	brandName    = "SymAI mpm"
	brandEdition = "(Standard Edition)"
	brandEmoji   = ""
	brandColor   = "" // No color in standard
)

// brandPrefix returns the prefixed brand name with edition
func brandPrefix() string {
	return fmt.Sprintf("%s %s", brandName, brandEdition)
}

// =============================================================================
// Command Registry (Standard Build)
// =============================================================================

// commandCategory represents a help section
type commandCategory struct {
	Name     string
	Commands []commandEntry
}

// commandEntry is a single command in the registry
type commandEntry struct {
	Name        string
	Description string
	NeedsDaemon bool
	BuildFlavor string // "fun", "openclaw", or "" for both
}

// registry is the global command registry
var registry []commandCategory

func init() {
	registry = []commandCategory{
		// Core Commands
		{
			Name: "Core Commands",
			Commands: []commandEntry{
				{"help", "Show this help", false, ""},
				{"version", "Show version info", false, ""},
				{"doctor", "Run diagnostics", false, ""},
			},
		},
		// System Management
		{
			Name: "System Management",
			Commands: []commandEntry{
				{"status", "Show daemon status", true, ""},
				{"shutdown", "Gracefully stop daemon", true, ""},
				{"stop", "Alias for shutdown", true, ""},
				{"reboot", "Restart daemon", true, ""},
				{"restart", "Alias for reboot", true, ""},
				{"logs", "Tail daemon logs", true, ""},
			},
		},
		// Daemon Operations
		{
			Name: "Daemon Operations",
			Commands: []commandEntry{
				{"llm", "LLM operations", true, ""},
				{"compile", "Compile project", true, ""},
				{"shred", "Secure memory wipe", true, ""},
				{"memory", "Memory operations", true, ""},
				{"mode", "Mode operations", true, ""},
				{"persona", "Persona operations", true, ""},
				{"watch", "Watch daemon control", true, ""},
				{"session", "Session operations", true, ""},
			},
		},
	}
}

// getRegistry returns the command registry for this build
func getRegistry() []commandCategory {
	return registry
}

// getBuildFlavor returns current build flavor
func getCurrentBuildFlavor() string {
	return "standard"
}

// contains checks if s contains substr
func contains(s, substr string) bool {
	return len(s) >= len(substr) && findSubstring(s, substr)
}

func findSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// hasSubcommands checks if a command has subcommands by checking the registry
func hasSubcommands(cmdName string) bool {
	// Commands that are known to have subcommands
	subcommandCommands := map[string]bool{
		"llm":      true,
		"memory":   true,
		"mode":     true,
		"persona":  true,
		"compile":  true,
		"shred":    true,
		"dashboard": true,
		"watch":    true,
		"session":  true,
	}
	return subcommandCommands[cmdName]
}

// =============================================================================
// Help Formatter (Standard Build)
// =============================================================================

// printBuildAwareHelp prints the help menu with build-specific commands
func printBuildAwareHelp() {
	// Header
	width := 58
	line := strings.Repeat("─", width)
	border := "├" + line + "┤"

	fmt.Println()
	fmt.Printf("  %s\n", brandPrefix())
	fmt.Println()
	fmt.Println("  Commands:")
	fmt.Printf("  %s\n", border)

	// Print each category
	for _, cat := range registry {
		// Category header
		fmt.Printf("  │\n")
		fmt.Printf("  │  %s:\n", cat.Name)
		fmt.Printf("  │\n")

		// Print commands aligned
		for _, cmd := range cat.Commands {
			daemonNote := ""
			if cmd.NeedsDaemon {
				daemonNote = " *"
			}
			
			// Check if this command has subcommands
			subcommandNote := ""
			if hasSubcommands(cmd.Name) {
				subcommandNote = " ▶"
			}
			
			// Format: "  │    <cmd>............<desc>"
			padding := 14 - len(cmd.Name)
			if padding < 1 {
				padding = 1
			}
			spaces := strings.Repeat(" ", padding)
			fmt.Printf("  │    %s%s%s%s%s\n", cmd.Name, spaces, cmd.Description, daemonNote, subcommandNote)
		}
	}

	// Footer
	fmt.Printf("  %s\n", border)
	fmt.Println()
	fmt.Println("  Global Flags:")
	fmt.Println("    -h, --help       Show this help")
	fmt.Println("    -v, --version    Show version")
	fmt.Println("    -f, --force      Force operation")
	fmt.Println()

	// Note about daemon commands
	fmt.Println("  (* indicates daemon must be running)")
	fmt.Println("  ▶ indicates command has subcommands")
	fmt.Println()

	// How to discover subcommands
	fmt.Println("  📚 Discovering Subcommands:")
	fmt.Println("    Run 'mpm <command> help' to see available subcommands")
	fmt.Println("    Example: mpm memory help")
	fmt.Println()
}

// printGhostCommandError shows helpful error when command not in this build
func printGhostCommandError(cmd string) {
	fmt.Fprintf(os.Stderr, "[!] Error: '%s' is not available in this build\n", cmd)
	fmt.Fprintf(os.Stderr, "\n")
	fmt.Fprintf(os.Stderr, "    This command requires features from another edition.\n")
	fmt.Fprintf(os.Stderr, "\n")
	fmt.Fprintf(os.Stderr, "    Run 'mpm help' to see available commands.\n")
	fmt.Fprintf(os.Stderr, "\n")
}

// GetFortuneOracle returns a generic fortune in standard build
func GetFortuneOracle() string {
	return "MPM: Memory-Persona-Mode Manager\n"
}
