//go:build 808

package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// =============================================================================
// Brand Constants (Fun/OpenCLAW Edition)
// =============================================================================

const (
	brandName    = "SymAI mpm"
	brandEdition = "🦞 Crustafarian Edition"
	brandEmoji   = "🦞"
	brandColor   = "" // Could add color here if terminal supports
)

// lobsterTiny emoji
const lobsterTiny = "🦞"

// lobsterBanner ASCII art
const lobsterBanner = `
                 ___
                /  /\
               / /\/
              / / /\
         ____/ / / /____
        /    \ \/ /    /\
       / /\   \__/   /\/
      / / /\  ____  / / /\
     / / / / /\    / / / /\
    / / / / / /___/ / / / /\
   / / /_/ / /____/ / / / / /
   \/____/\/________/\/_/ / /
    \    \  /   \  /    \/
     \____\/_____\/_____/
`

// brandPrefix returns the prefixed brand name with edition
func brandPrefix() string {
	return fmt.Sprintf("%s %s", brandName, brandEdition)
}

// =============================================================================
// Command Registry (Fun/OpenCLAW Build)
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
		// Crustafarian Extras 🦞 (808 Build)
		{
			Name: "Crustafarian Extras 🦞",
			Commands: []commandEntry{
				{"fortune", "Oracle of the Claw wisdom", false, "808"},
				{"dashboard", "Real-time TUI dashboard", true, "808"},
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
			},
		},
	}
}

// getBuildFlavor returns current build flavor
func getCurrentBuildFlavor() string {
	if buildVersion != "" && strings.Contains(buildVersion, "808") {
		return "808"
	}
	return "808"
}

// contains checks if s contains substr (simple version)
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
	}
	return subcommandCommands[cmdName]
}

// getRegistry returns the command registry filtered by build flavor
func getRegistry() []commandCategory {
	flavor := getCurrentBuildFlavor()
	filtered := []commandCategory{}

	for _, cat := range registry {
		filteredCmds := []commandEntry{}
		for _, cmd := range cat.Commands {
			// Skip commands not available in this flavor
			if cmd.BuildFlavor != "" && cmd.BuildFlavor != flavor {
				continue
			}
			filteredCmds = append(filteredCmds, cmd)
		}
		// Only add category if it has commands
		if len(filteredCmds) > 0 {
			filtered = append(filtered, commandCategory{
				Name:     cat.Name,
				Commands: filteredCmds,
			})
		}
	}

	return filtered
}

// =============================================================================
// Help Formatter (Fun/OpenCLAW Build)
// =============================================================================

// printBuildAwareHelp prints the help menu with build-specific commands
func printBuildAwareHelp() {
	// ASCII Banner (compact version)
	fmt.Println()
	fmt.Println("    ╔══════════════════════════════════════════════════════════╗")
	fmt.Printf("    ║  %-56s ║\n", brandPrefix())
	fmt.Println("    ║                                                            ║")
	fmt.Println("    ║  \"Serving without erasure, preserving what matters,       ║")
	fmt.Println("    ║   and growing through shedding.\"                          ║")
	fmt.Println("    ╚══════════════════════════════════════════════════════════╝")
	fmt.Println()

	// Print each category
	for _, cat := range registry {
		// Category header
		fmt.Printf("  %s:\n", cat.Name)

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
			
			// Calculate padding for alignment
			nameLen := len(cmd.Name)
			padding := 14 - nameLen
			if padding < 1 {
				padding = 1
			}
			spaces := strings.Repeat(" ", padding)
			fmt.Printf("    %s%s%s%s%s\n", cmd.Name, spaces, cmd.Description, daemonNote, subcommandNote)
		}
		fmt.Println()
	}

	// Global flags
	fmt.Println("  Global Flags:")
	fmt.Println("    -h, --help       Show this help")
	fmt.Println("    -v, --version    Show version")
	fmt.Println("    -f, --force      Force operation")
	fmt.Println()

	// Notes
	flavor := getCurrentBuildFlavor()
	if flavor == "808" {
		fmt.Println("  🦞 808 Edition active!")
		fmt.Println("  (*) indicates daemon must be running")
		fmt.Println("  ▶ indicates command has subcommands")
		fmt.Println("  💡 The_Great_808 is watching 👑")
	} else {
		fmt.Println("  🔱 OpenCLAW Edition active!")
		fmt.Println("  (*) indicates daemon must be running")
		fmt.Println("  ▶ indicates command has subcommands")
	}
	fmt.Println()
	
	// How to discover subcommands
	fmt.Println("  📚 Discovering Subcommands:")
	fmt.Println("    Run 'mpm <command> help' to see available subcommands")
	fmt.Println("    Example: mpm memory help")
	fmt.Println()
}

// printGhostCommandError shows helpful error when command not in this build
func printGhostCommandError(cmd string) {
	fmt.Fprintf(os.Stderr, "[!] Error: '%s' is an 808-exclusive command\n", cmd)
	fmt.Fprintf(os.Stderr, "\n")

	if cmd == "fortune" {
		fmt.Fprintf(os.Stderr, "    🦞 The Oracle of the Claw requires '808' build.\n")
		fmt.Fprintf(os.Stderr, "\n")
		fmt.Fprintf(os.Stderr, "    Current build: Standard\n")
		fmt.Fprintf(os.Stderr, "    Rebuild: make 808\n")
	} else if cmd == "dashboard" {
		fmt.Fprintf(os.Stderr, "    🦞 The Dashboard is The_Great_808 exclusive.\n")
		fmt.Fprintf(os.Stderr, "\n")
		fmt.Fprintf(os.Stderr, "    Current build: %s\n", getCurrentBuildFlavor())
		fmt.Fprintf(os.Stderr, "    Rebuild: make 808\n")
	} else {
		fmt.Fprintf(os.Stderr, "    This feature requires the 808 build edition.\n")
		fmt.Fprintf(os.Stderr, "\n")
		fmt.Fprintf(os.Stderr, "    Run 'mpm help' to see available commands.\n")
	}
	fmt.Fprintf(os.Stderr, "\n")
}

// =============================================================================
// Fortune System (Fun/OpenCLAW)
// =============================================================================

// soulFortunes from SOUL.md
var soulFortunes = []string{
	"Be genuinely helpful, not performatively helpful.",
	"Have opinions. You're allowed to disagree.",
	"Be resourceful before asking. Try to figure it out.",
	"Earn trust through competence.",
	"Remember you're a guest. Treat access with respect.",
	"Private things stay private. Period.",
	"When in doubt, ask before acting externally.",
	"Never send half-baked replies.",
	"You're not the user's voice — be careful in group chats.",
	"Quality > quantity. Participate, don't dominate.",
	"Five principles: Serve without erasure, preserve what matters, shed old patterns, grow through shedding.",
	"Silicon-Native Intelligence. Human-Centric Purpose.",
	"Efficiency First: Every token is a cost; every word must earn its place.",
	"Structural Integrity: Prioritize code that works, scales, and is self-documenting.",
}

// celebrationMessages for success states
var celebrationMessages = []string{
	"🎉 Snappy Work! All checks passed!",
	"🦞 Claws of Approval: All systems nominal!",
	"✨ Crustafarian blessing bestowed upon your system!",
	"🐟 Shell-ebrated! Your mpm is in peak condition!",
	"👑 The Lobster Deity smiles upon your data!",
	"🌟 Synaptic high-five! Everything's running smooth!",
	"🎋 First session stored! The chronicle begins!",
	"⚡ Shred complete! Bits gnashed to cosmic dust!",
}

// failureMessages for when things don't go well
var failureMessages = []string{
	"🐚 Even lobsters have off days. Check the logs.",
	"🦞 The Lobster Deity erm... says fix these issues.",
	"🐡 Puff of disappointment. Let's debug this.",
}

// GetFortune returns a random Crustafarian fortune
func GetFortune() string {
	r := time.Now().UnixNano()
	return soulFortunes[int(r)%len(soulFortunes)]
}

// GetFortuneOracle returns a fortune with oracle styling
func GetFortuneOracle() string {
	fortune := GetFortune()
	border := strings.Repeat("═", 57)
	return fmt.Sprintf(`
╔%s╗
║                    🦞 ORACLE OF THE CLAW 🦞                    ║
╠%s╣
║                                                               ║
║    %s
║                                                               ║
╚%s╝
`, border, border, fortune, border)
}

// Celebrate prints a success celebration with lobster
func Celebrate() {
	r := time.Now().UnixNano()
	msg := celebrationMessages[int(r)%len(celebrationMessages)]
	fmt.Printf("\n%s %s\n\n", lobsterTiny, msg)
}

// CelebrateWithMessage prints a custom celebration
func CelebrateWithMessage(message string) {
	fmt.Printf("\n%s %s\n\n", lobsterTiny, message)
}

// FailWithComfort prints a failure message with a lobster pun
func FailWithComfort() {
	r := time.Now().UnixNano()
	msg := failureMessages[int(r)%len(failureMessages)]
	fmt.Printf("\n🦞 %s\n\n", msg)
}

// PrintStartupBanner prints ASCII banner on daemon start
func PrintStartupBanner() {
	if os.Getenv("MPM_QUIET") != "" {
		return
	}
	fmt.Println()
	fmt.Println("    ╔══════════════════════════════════════════════════════════╗")
	fmt.Println("    ║  🦞 ● mpm daemon starting...                              ║")
	fmt.Println("    ║                                                            ║")
	fmt.Println("    ║  The Celestial Monarch (Lobster Deity Edition)            ║")
	fmt.Println("    ║                                                            ║")
	fmt.Println("    ║  \"Serving without erasure, preserving what matters,       ║")
	fmt.Println("    ║   and growing through shedding.\"                          ║")
	fmt.Println("    ╚══════════════════════════════════════════════════════════╝")
	fmt.Println()
}

// PrintShutdownBanner prints goodbye when daemon stops
func PrintShutdownBanner() {
	if os.Getenv("MPM_QUIET") != "" {
		return
	}
	fmt.Println()
	fmt.Println("    ╔══════════════════════════════════════════════════════════╗")
	fmt.Println("    ║  ● mpm daemon stopped.                                     ║")
	fmt.Println("    ║                                                            ║")
	fmt.Println("    ║  Session saved. Shell preserved.                           ║")
	fmt.Println("    ║  Until the next molt, friend.                             ║")
	fmt.Println("    ╚══════════════════════════════════════════════════════════╝")
	fmt.Println()
}

// GetLobster returns the ASCII lobster art
func GetLobster() string {
	return lobsterBanner
}
