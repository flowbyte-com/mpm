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
		"recall":       {Name: "recall", Description: "Search memories for context", MinArgs: 1, Aliases: []string{"s"}},
		"add":          {Name: "add", Description: "Add a new memory", MinArgs: 1},
		"ls":           {Name: "ls", Description: "List memories", MinArgs: 0},
		"show":         {Name: "show", Description: "Show memory details", MinArgs: 1},
		"rm":           {Name: "rm", Description: "Delete a memory", MinArgs: 1},
		"promote":      {Name: "promote", Description: "Make memory LTM (deprecated: use 'mpm kb memory promote')", MinArgs: 1},
		"patch-memory": {Name: "patch-memory", Description: "Patch metadata JSON in-place (deprecated: use 'mpm debug patch-memory')", MinArgs: 2},
		"reinforce":    {Name: "reinforce", Description: "Reinforce a memory (deprecated: use 'mpm kb memory reinforce')", MinArgs: 1},
		"weaken":       {Name: "weaken", Description: "Weaken a memory (deprecated: use 'mpm kb memory weaken')", MinArgs: 1},
		"snooze":       {Name: "snooze", Description: "Bump memory relevance (deprecated: use 'mpm kb memory snooze')", MinArgs: 1},
		"set-weight":   {Name: "set-weight", Description: "Set memory weight (deprecated: use 'mpm kb memory set-weight')", MinArgs: 2, MaxArgs: 2},
		"synthesize":   {Name: "synthesize", Description: "Merge near-duplicate memories via LLM synthesis", MinArgs: 0},
		"shred":        {Name: "shred", Description: "Secure delete memory", MinArgs: 1},
		"stats":        {Name: "stats", Description: "Show memory statistics", MinArgs: 0},
		"prune":        {Name: "prune", Description: "Prune old/expired memories", MinArgs: 0},
		"export":       {Name: "export", Description: "Export memories to JSON", MinArgs: 0},
		"maintain":     {Name: "maintain", Description: "Run self-maintenance (decay, consolidate, prune)", MinArgs: 0},

		// Review commands
		"review": {Name: "review", Description: "Spaced reinforcement review", MinArgs: 0},

		// Feature commands
		"watch":     {Name: "watch", Description: "File watcher for memory ingestion"},
		"web":       {Name: "web", Description: "Start web UI server", MinArgs: 0},
		"switch":    {Name: "switch", Description: "Interactive UI to change persona/mode", MinArgs: 0},
		"reference": {Name: "reference", Description: "Reference library (deprecated: use 'mpm kb reference')", MinArgs: 1},
		"topic":     {Name: "topic", Description: "Topic management (deprecated: use 'mpm kb topic')", MinArgs: 1},
		"session":   {Name: "session", Description: "Session operations (deprecated: use 'mpm kb session')", MinArgs: 1},
		"lesson":    {Name: "lesson", Description: "Lesson operations (deprecated: use 'mpm kb lesson')", MinArgs: 1},
		"memory":    {Name: "memory", Description: "Memory operations (deprecated: use 'mpm kb memory')", MinArgs: 1},

		"ingest": {Name: "ingest", Description: "Import memories from external SQLite sources"},

		"mode":                {Name: "mode", Description: "Mode operations"},
		"wake":                {Name: "wake", Description: "Show last session context (--json, --strict)", MinArgs: 0},
		"gc":                  {Name: "gc", Description: "Run memory decay sweep (--dry-run, --review, --purge)"},
		"backfill-embeddings": {Name: "backfill-embeddings", Description: "Backfill embeddings for existing memories (--batch-size, --collection, --dry-run)", MinArgs: 0},
		"dlq:review":          {Name: "dlq:review", Description: "Inspect DLQ (review/clear/retry) — synth failures awaiting retry", MinArgs: 0},
		"backup":              {Name: "backup", Description: "Export database to timestamped .sql dump (optional path arg)"},
		"restore":             {Name: "restore", Description: "Restore a soft-deleted memory", MinArgs: 1},
		"restore-db":          {Name: "restore-db", Description: "Import a .sql dump to restore full database state", MinArgs: 1},
		"directives":          {Name: "directives", Description: "Show behavioral directives"},
		"persona":             {Name: "persona", Description: "Persona operations"},
		"ops":                 {Name: "ops", Description: "Maintenance, diagnostics, and engine-room tools"},

		// Proactive Recall Hint
		"hint": {Name: "hint", Description: "Check conversation context for relevant decisions/theories", MinArgs: 1},
		"route": {Name: "route", Description: "Render mode+persona for a prompt (Claude Code hook input)", MinArgs: 0, MaxArgs: 1},

		// Epistemology Engine
		"propose_theory":  {Name: "propose_theory", Description: "Record a hypothesis with validation criteria", MinArgs: 1},
		"resolve_theory":  {Name: "resolve_theory", Description: "Mark a theory as resolved", MinArgs: 2},
		"record_decision": {Name: "record_decision", Description: "Record a decision with context, choice, and rationale", MinArgs: 1},
		"theories":        {Name: "theories", Description: "List theories [pending|resolved|all]", MinArgs: 0},
		"decisions":       {Name: "decisions", Description: "Show decision ledger", MinArgs: 0},
		"call":            {Name: "call", Description: "Universal machine interface: mpm call <tool> [--payload <json>]", MinArgs: 1},
		"evidence":        {Name: "evidence", Description: "Evidence operations (add|list) — confidence/evidence foundation", MinArgs: 0},

		// Knowledge Base — entity-centric namespace (reads + writes)
		"kb":   {Name: "kb", Description: "Knowledge base: memory, topic, lesson, session, reference", MinArgs: 0},
		"debug": {Name: "debug", Description: "Low-level inspection tools for human troubleshooting", MinArgs: 0},
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

	// Intercept bare +/- feedback: +<id> or -<id>
	// Requires len > 2 to avoid colliding with single-dash flags like -v, -h
	if len(cmdName) > 1 {
		if cmdName[0] == '+' {
			return handleFeedback([]string{"feedback", cmdName[1:], "1"})
		}
		if cmdName[0] == '-' && cmdName[1] != '-' && len(cmdName) > 2 {
			return handleFeedback([]string{"feedback", cmdName[1:], "-1"})
		}
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
		return handleMutationDeprecation("promote", args)
	case "patch-memory":
		return handleMutationDeprecation("patch-memory", args)
	case "reinforce":
		return handleMutationDeprecation("reinforce", args)
	case "weaken":
		return handleMutationDeprecation("weaken", args)
	case "snooze":
		return handleMutationDeprecation("snooze", args)
	case "set-weight":
		return handleMutationDeprecation("set-weight", args)
	case "synthesize":
		return handleSynthesize(args)
	case "shred":
		return handleShredMem(args)
	case "reference":
		return handleEntityDeprecation("reference", args)
	case "directives":
		return handlePrimeDirectives()
	case "memory":
		return handleEntityDeprecation("memory", args)
	case "mode":
		return handleMode(args[1:])
	case "persona":
		return handlePersona(args[1:])
	case "topic":
		return handleEntityDeprecation("topic", args)
	case "session":
		return handleEntityDeprecation("session", args)
	case "lesson":
		return handleEntityDeprecation("lesson", args)
	case "hint":
		return handleHint(args[1:])
	case "route":
		return r.handleRoute(args[1:])
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
	case "history":
		return handleHistory(args)
	case "diff":
		return handleDiff(args)
	case "diff-lines":
		return handleDiffLines(args)
	case "challenge":
		sub := args[1]
		if sub == "restore" && len(args) >= 3 {
			return handleChallengeRestore(args[1:])
		}
		return handleChallenge(args[1:])
	case "ops":
		return handleOps(args)
	case "kb":
		return handleKB(args)
	case "debug":
		return handleDebug(args)
	case "call":
		return handleCall(args[1:])
	case "evidence":
		return handleEvidence(args[1:])

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

// handleEntityDeprecation prints a deprecation notice then routes to the kb namespace.
func handleEntityDeprecation(entity string, args []string) int {
	fmt.Fprintf(os.Stderr, "[!] Warning: 'mpm %s' is deprecated.\n", entity)
	fmt.Fprintf(os.Stderr, "    Use 'mpm kb %s' instead.\n\n", entity)
	switch entity {
	case "memory":
		return handleKBMemory(args[1:])
	case "topic":
		return handleKBTopic(args[1:])
	case "lesson":
		return handleKBLesson(args[1:])
	case "session":
		return handleKBSession(args[1:])
	case "reference":
		return handleKBReference(args[1:])
	default:
		return 1
	}
}

// handleMutationDeprecation prints a deprecation notice then routes to the kb namespace.
// cmd is the root command name (e.g. "promote", "reinforce").
// args[0] is the command name at root level; kb expects entity/subcommand at [1]/[2].
func handleMutationDeprecation(cmd string, args []string) int {
	fmt.Fprintf(os.Stderr, "[!] Warning: 'mpm %s' is deprecated.\n", cmd)
	fmt.Fprintf(os.Stderr, "    Use 'mpm kb memory %s' instead.\n\n", cmd)
	// Route to kb memory <cmd> <remaining args>
	return handleKBMemory(append([]string{cmd}, args[1:]...))
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
	case "backfill-embeddings":
		return handleBackfillEmbeddings(subArgs)
	case "dlq:review":
		return handleDLQReview(subArgs)

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
	{"backfill-embeddings [--batch-size/--collection/--dry-run]", "Backfill embeddings for existing memories"},
	{"dlq:review [review/clear/retry]", "Dead letter queue — failed synth events"},
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
// KB Namespace — Knowledge Base: memory, topic, lesson, session, reference
// ============================================================================

// handleKB routes to the appropriate kb subcommand.
func handleKB(args []string) int {
	if len(args) < 2 {
		printKBHelp()
		return 0
	}

	subCmd := args[1]
	subArgs := args[2:]

	switch subCmd {
	// — Entity subcommands —
	case "memory":
		return handleKBMemory(subArgs)
	case "topic":
		return handleKBTopic(subArgs)
	case "lesson":
		return handleKBLesson(subArgs)
	case "session":
		return handleKBSession(subArgs)
	case "reference":
		return handleKBReference(subArgs)

	// — Epistemology engine —
	case "theories":
		return handleTheories(subArgs)
	case "decisions":
		return handleDecisions(subArgs)
	case "propose_theory":
		return handleProposeTheory(subArgs)
	case "resolve_theory":
		return handleResolveTheory(subArgs)
	case "record_decision":
		return handleRecordDecision(subArgs)
	case "hint":
		return handleHint(subArgs)

	// — Help —
	case "help":
		printKBHelp()
		return 0

	default:
		printKBHelp()
		return 0
	}
}

// handleKBMemory routes memory subcommands. Thin wrapper around handleMemory.
func handleKBMemory(args []string) int {
	return handleMemory(append([]string{"memory"}, args...))
}

func handleKBTopic(args []string) int {
	return handleTopic(append([]string{"topic"}, args...))
}

func handleKBLesson(args []string) int {
	return handleLesson(append([]string{"lesson"}, args...))
}

func handleKBSession(args []string) int {
	return handleSession(append([]string{"session"}, args...))
}

func handleKBReference(args []string) int {
	return handleRef(append([]string{"reference"}, args...))
}

// kbSubcommandDescs is the canonical list of all kb subcommands.
var kbSubcommandDescs = []struct {
	name string
	desc string
}{
	{"memory list|show|search|shred", "Memory operations"},
	{"memory add|reinforce|weaken|snooze|set-weight|promote", "Memory mutations"},
	{"topic list|show|search|shred|add|link", "Topic operations"},
	{"lesson list|show|search|shred|add", "Lesson operations"},
	{"session list|get|search|shred|stats", "Session operations"},
	{"reference list|show|search|shred|add", "Reference library"},
	{"theories [pending|resolved|all]", "List theories"},
	{"decisions", "Show decision ledger"},
	{"propose_theory <theoryId>", "Record a hypothesis with validation criteria"},
	{"resolve_theory <theoryId> <status>", "Mark a theory as resolved"},
	{"record_decision <decisionId>", "Record a decision with context/choice/rationale"},
	{"hint <topic>", "Check conversation for relevant decisions/theories"},
	{"help", "Show this help"},
}

func printKBHelp() {
	fmt.Println()
	fmt.Println("mpm kb — Knowledge Base: entity-centric memory interface")
	fmt.Println()
	fmt.Println("Usage: mpm kb <entity> <subcommand> [arguments]")
	fmt.Println()
	fmt.Println("Entities and subcommands:")
	for _, sc := range kbSubcommandDescs {
		fmt.Printf("  %-22s %s\n", sc.name, sc.desc)
	}
	fmt.Println()
	fmt.Println("Examples:")
	fmt.Println("  mpm kb memory list")
	fmt.Println("  mpm kb memory search <query>")
	fmt.Println("  mpm kb memory shred <id>")
	fmt.Println("  mpm kb topic link <topicId> <linkTopicId>")
	fmt.Println("  mpm kb theories pending")
	fmt.Println()
}

// ============================================================================
// Debug Namespace — Low-level inspection for human troubleshooting
// ============================================================================

// handleDebug routes to the appropriate debug subcommand.
func handleDebug(args []string) int {
	if len(args) < 2 {
		printDebugHelp()
		return 0
	}

	subCmd := args[1]
	subArgs := args[2:]

	switch subCmd {
	// — Version history —
	case "history":
		return handleHistory(append([]string{"history"}, subArgs...))
	case "diff":
		return handleDiff(append([]string{"diff"}, subArgs...))
	case "diff-lines":
		return handleDiffLines(append([]string{"diff-lines"}, subArgs...))

	// — Memory patching —
	case "patch-memory":
		return handlePatchMemory(append([]string{"patch-memory"}, subArgs...))
	case "shred":
		return handleShredMem(append([]string{"shred"}, subArgs...))
	case "show":
		return handleShow(append([]string{"show"}, subArgs...))

	// — Inspection —
	case "gc":
		return handleGC(append([]string{"gc"}, subArgs...))

	// — Help —
	case "help":
		printDebugHelp()
		return 0

	default:
		printDebugHelp()
		return 0
	}
}

// debugSubcommandDescs is the canonical list of all debug subcommands.
var debugSubcommandDescs = []struct {
	name string
	desc string
}{
	{"history <memoryId>", "Show version history for a memory"},
	{"diff <memoryId> <v1> <v2>", "Unified diff between two versions"},
	{"diff-lines <text1> <text2>", "Compute unified diff of two text blocks"},
	{"patch-memory <id> <json>", "Patch metadata JSON in-place"},
	{"shred <id>", "Secure delete memory"},
	{"show <id>", "Show memory details"},
	{"gc [--dry-run]", "Memory decay sweep (dry-run for inspection)"},
	{"help", "Show this help"},
}

func printDebugHelp() {
	fmt.Println()
	fmt.Println("mpm debug — Low-level inspection tools for human troubleshooting")
	fmt.Println()
	fmt.Println("Usage: mpm debug <subcommand> [arguments]")
	fmt.Println()
	fmt.Println("Subcommands:")
	for _, sc := range debugSubcommandDescs {
		fmt.Printf("  %-22s %s\n", sc.name, sc.desc)
	}
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

// handleRoute reads prompt from positional arg or stdin, evaluates against
// the workspace's mode+persona files, and prints a <system-reminder> block
// to stdout. Designed for the Claude Code UserPromptSubmit hook — never
// blocks the user on errors (any failure → exit 0, no output).
//
// Usage:
//   mpm route "review this code"        # positional arg
//   echo "review this" | mpm route      # stdin literal
//   mpm route < hook-stdin.json         # stdin JSON (Claude Code format)
func (r *CommandRouter) handleRoute(args []string) int {
	prompt := extractRoutePrompt(args, os.Stdin)
	skip, _ := shouldSkipRoute(prompt, os.Getenv)
	if skip {
		return 0
	}

	workspace := resolveRouteWorkspace()
	rendered, err := renderRoute(workspace, prompt)
	if err != nil {
		// Programmer-level error. Only surface on TTY (interactive) — never
		// when invoked from a hook (would corrupt hook output).
		if isatty(os.Stdout) {
			fmt.Fprintf(os.Stderr, "mpm route: %v\n", err)
		}
		return 0
	}

	if rendered != "" {
		fmt.Println(rendered)
	}
	return 0
}

// isatty returns true if f is a terminal. Used to gate stderr noise.
func isatty(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) != 0
}

