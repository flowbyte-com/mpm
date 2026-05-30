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
		"snooze":     {Name: "snooze", Description: "Bump memory relevance (no LTM promotion)", MinArgs: 1},
		"set-weight": {Name: "set-weight", Description: "Set memory weight", MinArgs: 2, MaxArgs: 2},
		"synthesize": {Name: "synthesize", Description: "Merge near-duplicate memories via LLM synthesis", MinArgs: 0},
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
			"wake":            {Name: "wake", Description: "Show last session context (--json, --strict)", MinArgs: 0},
		"gc":              {Name: "gc", Description: "Run memory decay sweep (--dry-run, --review, --purge)"},
		"backup":          {Name: "backup", Description: "Export database to timestamped .sql dump (optional path arg)"},
		"restore":         {Name: "restore", Description: "Restore a soft-deleted memory", MinArgs: 1},
		"restore-db":      {Name: "restore-db", Description: "Import a .sql dump to restore full database state", MinArgs: 1},
		"_suggest_tags":   {Name: "_suggest_tags", Description: "Tag autocomplete for shell completion", MinArgs: 0},
			"directives":       {Name: "directives", Description: "Show behavioral directives"},
		"persona":         {Name: "persona", Description: "Persona operations"},
		"ops":             {Name: "ops", Description: "Maintenance, diagnostics, and engine-room tools"},

		// Proactive Recall Hint
		"hint":           {Name: "hint", Description: "Check conversation context for relevant decisions/theories", MinArgs: 1},

		// Epistemology Engine
		"propose_theory":  {Name: "propose_theory", Description: "Record a hypothesis with validation criteria", MinArgs: 1},
		"resolve_theory":  {Name: "resolve_theory", Description: "Mark a theory as resolved", MinArgs: 2},
		"record_decision": {Name: "record_decision", Description: "Record a decision with context, choice, and rationale", MinArgs: 1},
		"theories":        {Name: "theories", Description: "List theories [pending|resolved|all]", MinArgs: 0},
		"decisions":       {Name: "decisions", Description: "Show decision ledger", MinArgs: 0},
		"challenge":       {Name: "challenge", Description: "Challenge a memory — weaken, create theory, log decision", MinArgs: 1},
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
	case "backup":
		return handleBackup(args)
	case "restore":
		return handleRestore(args)
	case "restore-db":
		return handleRestoreDB(args)
	case "_suggest_tags":
		return handleSuggestTags(args)
	case "help":
		return r.handleHelp(args[1:])
	case "doctor":
		runDoctorCommand(args[1:])
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
	case "snooze":
		return handleSnooze(args)
	case "set-weight":
		return handleSetWeight(args)
	case "synthesize":
		return handleSynthesize(args)
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
	case "hint":
		return handleHint(args[1:])
	case "propose_theory":
		return handleProposeTheory(args[1:])
	case "resolve_theory":
		return handleResolveTheory(args[1:])
	case "record_decision":
		return handleRecordDecision(args[1:])
	case "theories":
		return handleTheories(args[1:])
	case "decisions":
		return handleDecisions(args[1:])
	case "challenge":
		sub := args[1]
		if sub == "restore" && len(args) >= 3 {
			return handleChallengeRestore(args[1:])
		}
		return handleChallenge(args[1:])
	case "ops":
		return handleOps(args)

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
	return handleSwitch([]string{})
}

// ============================================================================
// Ops Subcommand — Maintenance, diagnostics, and engine-room tools
// ============================================================================

// handleOps routes to the appropriate sub-command under the ops parent.
// All engine-room commands live here. Root-level aliases are kept for
// backwards compatibility.
func handleOps(args []string) int {
	if len(args) < 2 {
		printOpsHelp()
		return 0
	}

	subCmd := args[1]
	subArgs := args[2:]

	switch subCmd {
	// — Diagnostics & Maintenance —
	case "doctor":
		runDoctorCommand(subArgs)
		return 0
	case "maintain":
		return handleMaintain(append([]string{"maintain"}, subArgs...))
	case "synthesize":
		return handleSynthesize(append([]string{"synthesize"}, subArgs...))
	case "gc":
		return handleGC(append([]string{"gc"}, subArgs...))

	// — Watcher & Web —
	case "watch":
		return handleWatch(subArgs)
	case "web":
		return handleWeb(append([]string{"web"}, subArgs...))

	// — Review & Stats —
	case "review":
		return handleReview(append([]string{"review"}, subArgs...))
	case "stats":
		return handleStats(append([]string{"stats"}, subArgs...))
	case "prune":
		return handlePrune(append([]string{"prune"}, subArgs...))
	case "export":
		return handleExport(append([]string{"export"}, subArgs...))

	// — Backup & Restore & Ingest —
	case "backup":
		return handleBackup(append([]string{"backup"}, subArgs...))
	case "restore-db":
		return handleRestoreDB(append([]string{"restore-db"}, subArgs...))
	case "ingest":
		return handleIngest(append([]string{"ingest"}, subArgs...))

	// — Interactive & Identity —
	case "switch":
		return handleSwitch(subArgs)
	case "directives":
		return handlePrimeDirectives()
	case "mode":
		return handleMode(subArgs)
	case "persona":
		return handlePersona(subArgs)

	// — Memory, Topic, Lesson, Session —
	case "memory":
		return handleMemory(subArgs)
	case "topic":
		return handleTopic(subArgs)
	case "lesson":
		return handleLesson(subArgs)
	case "session":
		return handleSession(subArgs)
	case "reference":
		return handleRef(append([]string{"reference"}, subArgs...))
	case "wake":
		return handleWake(append([]string{"wake"}, subArgs...))

	// — Gateway —
	case "gateway":
		handleGatewayCommand(subArgs)
		return 0

	// — Status Dashboard —
		case "stance":
			return handleStance(subArgs)

		case "promote":
			return handleOpsPromote()

		case "status":
			return handleStatus()

		// — Help —
	case "help":
		printOpsHelp()
		return 0

	default:
		printOpsHelp()
		return 0
	}
}

// opsSubcommandDescs is the canonical list of all ops subcommands and their descriptions.
var opsSubcommandDescs = []struct {
	name string
	desc string
}{
	{"doctor [--explain]", "Run diagnostics (--explain for FTS5 query plan)"},
	{"maintain", "Self-maintenance: decay, consolidate, prune"},
	{"synthesize [--dry-run]", "LLM synthesis on all memories"},
	{"gc [--dry-run/--review/--purge/--shred-negative]", "Memory decay sweep"},
	{"watch", "Start/stop/status watcher daemon"},
	{"web", "Start web UI server"},
	{"review", "Spaced reinforcement review"},
	{"stats", "Memory statistics"},
	{"prune", "Prune expired memories"},
	{"export", "Export memories to JSON"},
	{"backup [path]", "Database backup (.sql dump)"},
	{"restore-db <path>", "Restore database from .sql dump"},
	{"ingest", "Import memories from external SQLite"},
	{"switch", "Interactive persona/mode switcher"},
	{"directives", "Show behavioral directives"},
	{"mode", "Mode operations"},
	{"persona", "Persona operations"},
	{"topic", "Topic operations"},
	{"lesson", "Lesson operations"},
	{"session", "Session operations"},
	{"memory", "Memory operations"},
	{"reference", "Reference library"},
	{"wake", "Show last session context"},
	{"gateway", "Gateway control"},
	{"status", "System status dashboard"},
		{"stance assume <mode> <persona> <rationale>", "XITL: hot-swap existing persona when auto active"},
		{"stance synthesize <name> [flags]", "XITL: generate JIT persona for novel edge cases"},
		{"promote", "XITL: promote ephemeral persona to permanent disk file"},
		{"help", "Show this help"},
	}

	// printOpsHelp displays the ops subcommand help text.
func printOpsHelp() {
	fmt.Println()
	fmt.Println("mpm ops — Engine Room: maintenance, diagnostics, and power tools")
	fmt.Println()
	fmt.Println("Usage: mpm ops <subcommand> [arguments]")
	fmt.Println()
	fmt.Println("Subcommands:")
	for _, sc := range opsSubcommandDescs {
		fmt.Printf("  %-22s %s\n", sc.name, sc.desc)
	}
	fmt.Println()
	fmt.Println("All ops subcommands also work at the root level for")
	fmt.Println("backwards compatibility (e.g. `mpm doctor` = `mpm ops doctor`).")
	fmt.Println()
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