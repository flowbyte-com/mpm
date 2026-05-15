package main

import (
	"fmt"
	"os"
	"strings"
)

// Command describes a single command
type Command struct {
	Name        string
	Description string
	Aliases     []string
	MinArgs     int
	MaxArgs     int
}

// CommandRouter routes commands to handlers
type CommandRouter struct {
	Commands map[string]*Command
}

// NewRouter creates a new command router
func NewRouter() *CommandRouter {
	r := &CommandRouter{}

	// Register all commands
	r.Commands = map[string]*Command{
		// Info commands
		"version": {Name: "version", Description: "Show version info", MinArgs: 0, MaxArgs: 0},
		"help":    {Name: "help", Description: "Show this help", MinArgs: 0, MaxArgs: 0},
		"doctor":  {Name: "doctor", Description: "Run diagnostics", MinArgs: 0},

		// Memory commands
		"recall":     {Name: "recall", Description: "Search memories for context", MinArgs: 1, Aliases: []string{"s"}},
		"add":        {Name: "add", Description: "Add a new memory", MinArgs: 1},
		"ls":         {Name: "ls", Description: "List memories", MinArgs: 0},
		"show":       {Name: "show", Description: "Show memory details", MinArgs: 1},
		"rm":         {Name: "rm", Description: "Delete a memory", MinArgs: 1},
		"promote":    {Name: "promote", Description: "Make memory LTM", MinArgs: 1},
		"patch-memory": {Name: "patch-memory", Description: "Patch metadata JSON in-place", MinArgs: 2},
		"reinforce":  {Name: "reinforce", Description: "Reinforce a memory", MinArgs: 1},
		"weaken":     {Name: "weaken", Description: "Weaken a memory", MinArgs: 1},
		"set-weight": {Name: "set-weight", Description: "Set memory weight", MinArgs: 2, MaxArgs: 2},
		"shred":      {Name: "shred", Description: "Secure delete memory", MinArgs: 1},
		"stats":      {Name: "stats", Description: "Show memory statistics", MinArgs: 0},
		"prune":      {Name: "prune", Description: "Prune old/expired memories", MinArgs: 0},
		"export":     {Name: "export", Description: "Export memories to JSON", MinArgs: 0},
		"maintain":   {Name: "maintain", Description: "Run self-maintenance (decay, consolidate, prune)", MinArgs: 0},

		// Review commands
		"review":         {Name: "review", Description: "Spaced reinforcement review", MinArgs: 0},

		// Feature commands
		"watch":           {Name: "watch", Description: "File watcher for memory ingestion"},
		"web":             {Name: "web", Description: "Start web UI server", MinArgs: 0},
		"switch":          {Name: "switch", Description: "Interactive UI to change persona/mode", MinArgs: 0},
		"reference":       {Name: "reference", Description: "Reference library", MinArgs: 1},
		"topic":           {Name: "topic", Description: "Topic management", MinArgs: 1},
		"session":         {Name: "session", Description: "Session operations"},
		"lesson":          {Name: "lesson", Description: "Lesson operations"},
		"memory":          {Name: "memory", Description: "Memory operations"},

		"ingest":          {Name: "ingest", Description: "Import memories from external SQLite sources"},

		"mode":            {Name: "mode", Description: "Mode operations"},
			"wake":            {Name: "wake", Description: "Show last session context (mode, persona, recent memories)", MinArgs: 0},
		"gc":              {Name: "gc", Description: "Run memory decay sweep (--dry-run, --review, --purge)"},
			"restore":         {Name: "restore", Description: "Restore a soft-deleted memory", MinArgs: 1},
			"directives":       {Name: "directives", Description: "Show behavioral directives"},
		"persona":         {Name: "persona", Description: "Persona operations"},
	}

	return r
}

// Execute routes and runs the command, returning an exit code.
func (r *CommandRouter) Execute(args []string) int {
	if len(args) == 0 {
		PrintHelp()
		return 0
	}

	// Parse global flags first
	args = r.parseFlags(args)

	if len(args) < 1 {
		PrintHelp()
		return 0
	}

	cmdName := args[0]
	if cmdName == "" {
		PrintHelp()
		return 0
	}

	cmd := r.resolveCommand(cmdName)
	if cmd == nil {
		r.unknownCommand(cmdName)
		return 1
	}

	// Validate arguments
	if len(args)-1 < cmd.MinArgs {
		r.errorf("[!] Error: %s requires %d argument(s)\n", cmd.Name, cmd.MinArgs)
		return 1
	}

	if cmd.MaxArgs > 0 && len(args)-1 > cmd.MaxArgs {
		r.errorf("[!] Error: %s takes at most %d argument(s)\n", cmd.Name, cmd.MaxArgs)
		return 1
	}

	// Route to appropriate handler
	switch cmd.Name {
	case "version":
		return r.handleVersion()
	case "wake":
		return handleWake(args)
	case "gc":
		return handleGC(args)
	case "restore":
		return handleRestore(args)
	case "help":
		return r.handleHelp(args[1:])
	case "doctor":
		runDoctorCommand()
		return 0
	case "recall":
		return handleRecall(args)
	case "ingest":
		return handleIngest(args)
	case "stats":
		return handleStats(args)
	case "prune":
		return handlePrune(args)
	case "export":
		return handleExport(args)
	case "maintain":
		return handleMaintain(args)
	case "review":
		return handleReview(args)
	case "web":
		return handleWeb(args)
	case "watch":
		return handleWatch(args[1:])
	case "switch":
		return r.handleSwitch()
	case "add":
		return handleAdd(args)
	case "ls":
		return handleLs(args)
	case "show":
		return handleShow(args)
	case "rm":
		return handleRm(args)
	case "promote":
		return handlePromote(args)
	case "patch-memory":
		return handlePatchMemory(args)
	case "reinforce":
		return handleReinforce(args)
	case "weaken":
		return handleWeaken(args)
	case "set-weight":
		return handleSetWeight(args)
	case "shred":
		return handleShredMem(args)
	case "reference":
		return handleRef(args)
	case "directives":
		return handlePrimeDirectives()
	case "memory":
		return handleMemory(args[1:])
	case "mode":
		return handleMode(args[1:])
	case "persona":
		return handlePersona(args[1:])
	case "topic":
		return handleTopic(args[1:])
	case "session":
		return handleSession(args[1:])
	case "lesson":
		return handleLesson(args[1:])
	default:
		r.unknownCommand(cmdName)
		return 1
	}
}

// resolveCommand finds command by name or alias
func (r *CommandRouter) resolveCommand(name string) *Command {
	if cmd, ok := r.Commands[name]; ok {
		return cmd
	}
	// Check aliases
	for _, cmd := range r.Commands {
		for _, alias := range cmd.Aliases {
			if alias == name {
				return cmd
			}
		}
	}
	return nil
}

// parseFlags parses global flags, returns remaining args
func (r *CommandRouter) parseFlags(args []string) []string {
	result := make([]string, 0, len(args))
	for _, arg := range args {
		switch arg {
		case "-h", "--help":
			result = append(result, "help")
		case "-v", "--version":
			result = append(result, "version")
		case "-f", "--force":
			os.Setenv("MPM_FORCE", "1")
		case "-i", "--interactive":
			os.Setenv("MPM_INTERACTIVE", "1")
		default:
			result = append(result, arg)
		}
	}
	return result
}

// ============================================================================
// Command Handlers
// ============================================================================

func (r *CommandRouter) handleVersion() int {
	fmt.Printf("MPM mpm %s\n", buildVersion)
	return 0
}

// handleHelp routes help requests to specific help functions or prints general help
func (r *CommandRouter) handleHelp(args []string) int {
	if len(args) == 0 {
		PrintHelp()
		return 0
	}

	// Route to specific help based on command
	helpCmd := args[0]
	var helpFunc func() int

	switch helpCmd {
	case "watch":
		helpFunc = handleWatchHelp
	case "mode":
		helpFunc = handleModeHelp
	case "persona":
		helpFunc = handlePersonaHelp
	case "topic":
		helpFunc = handleTopicHelp
	case "session":
		helpFunc = handleSessionHelp
	case "lesson":
		helpFunc = handleLessonHelp
	case "reference":
		helpFunc = handleReferenceHelp
	case "memory":
		helpFunc = handleMemoryHelp
	case "gateway":
		helpFunc = printGatewayHelp
	default:
		// Fall back to general help
		r.errorf("[!] Error: no help available for '%s'\n", helpCmd)
		PrintHelp()
		return 1
	}

	if helpFunc != nil {
		helpFunc()
	}
	return 0
}

func (r *CommandRouter) handleSwitch() int {
	StartSwitch()
	return 0
}

// ============================================================================
// Output Helpers
// ============================================================================

// PrintHelp shows the lipgloss-styled help menu
func PrintHelp() {
	printHelp()
}

func (r *CommandRouter) unknownCommand(name string) {
	r.errorf("[!] Error: unknown command '%s'\n", name)
	fmt.Printf("    Run 'mpm help' for available commands.\n")
}

func (r *CommandRouter) errorf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, format, args...)
}

// ExtractJSONFlag scans args for --json or -j, removes it, returns (jsonOutput, cleanedArgs).
// Callers may place the flag anywhere in the arg list.
func ExtractJSONFlag(args []string) (bool, []string) {
	jsonOutput := false
	cleaned := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "--json" || arg == "-j" {
			jsonOutput = true
		} else {
			cleaned = append(cleaned, arg)
		}
	}
	return jsonOutput, cleaned
}

// ExtractFlags scans args for any flags in removeFlags map (key = flag name, value = consumes next arg),
// removes them, returns (found flags as map, cleaned args).
func ExtractFlags(args []string, removeFlags map[string]bool) (map[string]bool, []string) {
	found := make(map[string]bool)
	cleaned := make([]string, 0, len(args))
	i := 0
	for i < len(args) {
		arg := args[i]
		if removeFlags[arg] {
			found[arg] = true
			if removeFlags[arg] && i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				// consume next arg if it's not a flag
				i += 2
				continue
			}
			i++
			continue
		}
		cleaned = append(cleaned, arg)
		i++
	}
	return found, cleaned
}