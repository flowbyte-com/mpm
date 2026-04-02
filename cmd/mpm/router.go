package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

// Build flavors
const (
	BuildStandard = "standard"
	BuildFun      = "fun"
	BuildOpenCLAW = "openclaw"
)

// Command describes a single command
type Command struct {
	Name        string
	Description string
	Aliases     []string
	MinArgs     int
	MaxArgs     int
	NeedsDaemon bool
	BuildFlavor string // "", "fun", "openclaw"
}

// CommandRouter routes commands to handlers
type CommandRouter struct {
	Commands    map[string]*Command
	socketPath  string
	buildFlavor string
	buildInfo   string
}

// NewRouter creates a new command router
func NewRouter() *CommandRouter {
	r := &CommandRouter{
		socketPath:  socketPath(),
		buildFlavor: getBuildFlavor(),
		buildInfo:   buildVersion,
	}

	// Register all commands
	r.Commands = map[string]*Command{
		// Lifecycle commands (need daemon)
		"start":    {Name: "start", Description: "Start daemon"},
		"status":   {Name: "status", Description: "Show daemon status", NeedsDaemon: true},
		"shutdown": {Name: "shutdown", Description: "Gracefully stop daemon", NeedsDaemon: true},
		"stop":     {Name: "stop", Description: "Alias for shutdown", Aliases: []string{"shutdown"}, NeedsDaemon: true},
		"reboot":   {Name: "reboot", Description: "Restart daemon", NeedsDaemon: true},
		"restart":  {Name: "restart", Description: "Alias for reboot", Aliases: []string{"reboot"}, NeedsDaemon: true},
		"logs":     {Name: "logs", Description: "Tail daemon logs", NeedsDaemon: true},

		// Standalone commands
		"version":   {Name: "version", Description: "Show version info", MinArgs: 0, MaxArgs: 0},
		"help":      {Name: "help", Description: "Show this help", MinArgs: 0, MaxArgs: 0},
		"doctor":    {Name: "doctor", Description: "Run diagnostics", MinArgs: 0},
		"synthesize": {Name: "synthesize", Description: "Synthesize session facts via LLM", MinArgs: 1},
		"recall":    {Name: "recall", Description: "Search memories for context", MinArgs: 1, Aliases: []string{"s"}},
		"topics":   {Name: "topics", Description: "List all topic names", MinArgs: 0},
		"watch":     {Name: "watch", Description: "Watch daemon for memory ingestion"},
		"menu":      {Name: "menu", Description: "Interactive control menu", NeedsDaemon: true},

		// Fun/OpenCLAW commands
		"fortune": {
			Name:        "fortune",
			Description: "Crustafarian wisdom oracle",
			MinArgs:     0,
			MaxArgs:     0,
			BuildFlavor: "fun",
		},
		"dashboard": {
			Name:        "dashboard",
			Description: "Real-time TUI dashboard",
			NeedsDaemon: true,
			BuildFlavor: "openclaw",
		},

		// Daemon subcommands
		"llm":      {Name: "llm", Description: "LLM operations", NeedsDaemon: true},
		"compile":  {Name: "compile", Description: "Compile project", NeedsDaemon: true},
		"shred":    {Name: "shred", Description: "Secure memory wipe", NeedsDaemon: true},
		"memory":   {Name: "memory", Description: "Memory operations", NeedsDaemon: true},
		"mode":     {Name: "mode", Description: "Mode operations", NeedsDaemon: true},
		"persona":  {Name: "persona", Description: "Persona operations", NeedsDaemon: true},
		"topic":    {Name: "topic", Description: "Topic management", MinArgs: 1},
		"session":  {Name: "session", Description: "Session operations", NeedsDaemon: true},
	}

	return r
}

// Execute routes and runs the command
func (r *CommandRouter) Execute(args []string) int {
	// Handle empty command - start daemon
	if len(args) == 0 {
		becomeDaemonAndExecute()
		return 0
	}

	// Parse global flags first
	args = r.parseFlags(args)

	if len(args) < 1 {
		PrintHelp()
		return 0
	}

	cmdName := args[0]

	// Handle empty command after flag parsing
	if cmdName == "" {
		becomeDaemonAndExecute()
		return 0
	}

	cmd := r.resolveCommand(cmdName)

	if cmd == nil {
		r.unknownCommand(cmdName)
		return 1
	}

	// Check build flavor compatibility
	if cmd.BuildFlavor != "" && r.buildFlavor != cmd.BuildFlavor && r.buildFlavor != "openclaw" {
		r.incompatibleBuild(cmd.Name, cmd.BuildFlavor)
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
	case "help":
		return r.handleHelp()
	case "doctor", "fortune", "logs", "start":
		// Standalone commands - don't need daemon
		return r.handleStandalone(cmd.Name, args)
	case "synthesize":
		return handleSynthesize(args)
	case "recall":
		return handleRecall(args)
	case "topics":
		return topicList(args)
	case "topic":
		return topicCmd(args)
	case "watch":
		// watch - if no args, start watch daemon standalone
		// if args provided, route to daemon for status/start/stop/restart
		if len(args) < 2 {
			// No subcommand - start watch daemon as standalone process
			if cmdWatch(args[1:]) {
				return 0
			}
			return 1
		}
		// Has subcommand - route to daemon (status/start/stop/restart)
		return r.handleDaemonCommand(cmd.Name, args)
	case "status", "shutdown", "stop", "reboot", "restart":
		return r.handleDaemonCommand(cmd.Name, args)
	case "dashboard":
		return r.handleDashboard()
	case "menu":
		return r.handleMenu()
	default:
		// Daemon commands (llm, compile, shred, ss, etc.)
		if cmd.NeedsDaemon {
			return r.handleDaemonCommand(cmd.Name, args)
		}
		r.unknownCommand(cmdName)
		return 1
	}
}

// handleStandalone runs commands that don't need the daemon
func (r *CommandRouter) handleStandalone(cmdName string, args []string) int {
	switch cmdName {
	case "help":
		PrintHelp()
		return 0
	case "doctor":
		runDoctorCommand()
		return 0
	case "fortune":
		if r.buildFlavor == BuildStandard {
			r.errorf("[!] Error: fortune requires 'fun' or 'openclaw' build\n")
			return 1
		}
		handleFortuneDirect()
		return 0
	case "logs":
		handleLogsCommand()
		return 0
	case "start":
		handleStartCommand()
		return 0
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

// commandNeedsDaemon checks if a command requires the daemon
func (r *CommandRouter) commandNeedsDaemon(name string) bool {
	cmd := r.resolveCommand(name)
	if cmd == nil {
		return false
	}
	return cmd.NeedsDaemon
}

// parseFlags parses global flags, returns remaining args
func (r *CommandRouter) parseFlags(args []string) []string {
	result := make([]string, 0, len(args))
	i := 0
	for i < len(args) {
		arg := args[i]
		switch arg {
		case "-h", "--help":
			// Only intercept help if it's standalone or followed by nothing
			if i == 0 || (i == 1 && (args[0] == "watch" || args[0] == "status")) {
				result = append(result, "help")
				return result
			}
			result = append(result, arg)
			i++
		case "-v", "--version":
			// Only intercept version flag if it's the ONLY command-like argument
			// This allows "mpm watch -v" to pass -v to the watch command
			if i == 0 || (i == 1 && args[0] == "watch") {
				result = append(result, "version")
				return result
			}
			result = append(result, arg)
			i++
		case "-f", "--force":
			os.Setenv("MPM_FORCE", "1")
			i++
		case "-i", "--interactive":
			os.Setenv("MPM_INTERACTIVE", "1")
			i++
		default:
			result = append(result, arg)
			i++
		}
	}
	return result
}

// ============================================================================
// Command Handlers
// ============================================================================

func (r *CommandRouter) handleVersion() int {
	flavor := getBuildFlavor()
	fmt.Printf("SymAI mpm %s (%s)\n", buildVersion, flavor)
	return 0
}

func (r *CommandRouter) handleHelp() int {
	PrintHelp()
	return 0
}

func (r *CommandRouter) handleDashboard() int {
	// Dashboard as CLI command: print status summary (no TUI)
	// mpm dashboard → show quick status
	conn, err := r.dialDaemonWithRetry(2, 500*time.Millisecond)
	if err != nil {
		fmt.Println("Daemon not running. Start with: mpm")
		return 1
	}
	defer conn.Close()

	msg := Message{Args: []string{"status"}}
	if err := json.NewEncoder(conn).Encode(msg); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		return 1
	}

	var resp Message
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		fmt.Fprintf(os.Stderr, "Error reading response: %v\n", err)
		return 1
	}

	if resp.Output != "" {
		fmt.Print(resp.Output)
	}
	if resp.Error != "" {
		fmt.Fprintf(os.Stderr, "Error: %s\n", resp.Error)
		return resp.ExitCode
	}

	// Quick links
	fmt.Println()
	fmt.Println("  Quick Links:")
	fmt.Println("  ─────────────────────────────────────")
	fmt.Println("  🌐 Dashboard:  mpm menu               (interactive TUI)")
	fmt.Println("  📊 Status:     mpm status             (full status)")
	fmt.Println("  📜 Logs:      mpm logs --json        (tail logs)")
	fmt.Println("  💾 Memory:    mpm memory search       (query memory)")
	fmt.Println("  🎭 Modes:     mpm mode list           (available modes)")
	fmt.Println()
	return 0
}

func (r *CommandRouter) handleMenu() int {
	// Ensure daemon is running before launching TUI (TUI calls mpm mode/persona set)
	handleStartCommand()
	StartTUI()
	return 0
}

func (r *CommandRouter) handleDaemonCommand(cmd string, args []string) int {
	// Try with retry - daemon might be starting
	conn, err := r.dialDaemonWithRetry(3, 500*time.Millisecond)
	if err != nil {
		r.daemonNotRunning(cmd)
		return 1
	}
	defer conn.Close()

	// Send command
	msg := Message{Args: append([]string{cmd}, args[1:]...)}
	if err := json.NewEncoder(conn).Encode(msg); err != nil {
		r.errorf("[!] Error: failed to send command: %v\n", err)
		return 1
	}

	// Stream response
	r.streamResponse(conn)
	return 0
}

// ============================================================================
// Daemon Connection
// ============================================================================

// dialDaemon connects to the daemon with timeout and stale socket cleanup
func (r *CommandRouter) dialDaemon() (net.Conn, error) {
	return r.dialDaemonWithRetry(1, 0)
}

// dialDaemonWithRetry attempts to connect with retries
func (r *CommandRouter) dialDaemonWithRetry(attempts int, delay time.Duration) (net.Conn, error) {
	for i := 0; i < attempts; i++ {
		if i > 0 && delay > 0 {
			time.Sleep(delay)
		}

		// Check if socket exists
		if _, err := os.Stat(r.socketPath); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}

		// Dial with timeout
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()

		conn, err := (&net.Dialer{}).DialContext(ctx, "unix", r.socketPath)
		if err != nil {
			// Socket exists but daemon is dead - cleanup stale socket
			if strings.Contains(err.Error(), "connection refused") {
				os.Remove(r.socketPath)
				continue
			}
			continue
		}

		return conn, nil
	}
	return nil, fmt.Errorf("daemon not available after %d attempts", attempts)
}

// streamResponse reads and displays daemon response
func (r *CommandRouter) streamResponse(conn net.Conn) {
	dec := json.NewDecoder(conn)
	for {
		var resp Message
		if err := dec.Decode(&resp); err != nil {
			break
		}
		if resp.Output != "" {
			fmt.Print(resp.Output)
		}
		if resp.Error != "" {
			r.errorf("[!] Error: %s\n", resp.Error)
		}
		if resp.Done {
			if resp.ExitCode != 0 {
				os.Exit(resp.ExitCode)
			}
			return
		}
	}
}

// ============================================================================
// Output Helpers
// ============================================================================

// PrintHelp shows dynamic help based on build flavor
func PrintHelp() {
	printBuildAwareHelp()
}

func (r *CommandRouter) printCmd(name string, needsDaemon bool) {
	cmd := r.Commands[name]
	if cmd == nil {
		return
	}
	daemonNote := ""
	if needsDaemon {
		daemonNote = "*"
	}
	fmt.Printf("    %-12s %s%s\n", name, cmd.Description, daemonNote)
}

func (r *CommandRouter) unknownCommand(name string) {
	r.errorf("[!] Error: unknown command '%s'\n", name)
	fmt.Printf("    Run 'mpm help' for available commands.\n")
}

func (r *CommandRouter) incompatibleBuild(cmd, required string) {
	// Use the build-aware ghost command handler
	printGhostCommandError(cmd)
}

func (r *CommandRouter) daemonNotRunning(cmd string) {
	r.errorf("[!] Error: daemon is not running\n")
	fmt.Printf("    The '%s' command requires the daemon.\n", cmd)
	fmt.Printf("    Run 'mpm' without arguments to start the daemon.\n")
}

func (r *CommandRouter) errorf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, format, args...)
}

// ============================================================================
// Build Detection
// ============================================================================

func getBuildFlavor() string {
	// Detect based on build version string
	if strings.Contains(buildVersion, "openclaw") {
		return BuildOpenCLAW
	}
	if strings.Contains(buildVersion, "fun") {
		return BuildFun
	}
	if strings.Contains(buildVersion, "std") {
		return BuildStandard
	}
	return BuildStandard
}
