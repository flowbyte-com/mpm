package main

import (
	"database/sql"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"mpm/internal/config"

	mpminternal "mpm/internal"
)

// ============================================================================
// Global Daemon State
// ============================================================================

// Thread limiter - prevent runaway thread consumption
// This MUST be set before any goroutines are launched
func init() {
	// Cap threads at 32 to prevent resource exhaustion while staying responsive.
	// On a machine with many cores, this prevents a runaway goroutine storm
	// from eating all available threads. The default (unlimited) can cause the
	// Go scheduler to spawn hundreds of OS threads under heavy load, which on
	// a shared or memory-constrained system can lead to OOM kills.
	runtime.GOMAXPROCS(32)

	// Initialize global mode manager
	// Will be used to set default 808 mode on daemon startup
	configPath := os.ExpandEnv("$HOME/.openclaw/workspace/projects/mpm")
	modeManager = mpminternal.NewModeManager(configPath)
}

// buildVersion is set at compile time via -ldflags
var buildVersion = "dev"

// modeManager is the global mode manager instance (initialized at startup)
var modeManager any

// printVersion outputs version info
func printVersion() {
	fmt.Printf("SymAI mpm %s\n", buildVersion)
}

// printError formats and prints an error message mpm-style
func printError(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "[!] Error: "+format+"\n", args...)
}

// printSuccess prints a success message mpm-style
func printSuccess(format string, args ...interface{}) {
	fmt.Printf("✓ "+format+"\n", args...)
}

// printWarning prints a warning message mpm-style
func printWarning(format string, args ...interface{}) {
	fmt.Printf("⚠ "+format+"\n", args...)
}

// Global state kept for compatibility
var sockPath = socketPath()

// Message is the socket protocol struct (kept for other file references)
type Message struct {
	Args     []string `json:"args"`
	Output   string   `json:"output"`
	Error    string   `json:"error"`
	ExitCode int      `json:"exit_code"`
	Done     bool     `json:"done"`
}

// formatUptime returns a human-readable uptime string
func formatUptime(d time.Duration) string {
	if d < 0 {
		return "0s"
	}
	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60
	if h > 0 {
		return fmt.Sprintf("%dh %dm %ds", h, m, s)
	}
	if m > 0 {
		return fmt.Sprintf("%dm %ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

// truncate truncates a string to maxLen, adding ellipsis if needed
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

// handleGatewayCommand routes gateway subcommands
func handleGatewayCommand(args []string) {
	printGatewayHelp()
}

// PreFlightResult holds the result of the pre-flight health check
type PreFlightResult struct {
	Status     string           // "Passed", "Repaired", or "Failed"
	DurationMs int64            // Time taken to complete checks
	Checks     []PreFlightCheck // Individual check results
	Timestamp  time.Time        // When checks were run
}

// PreFlightCheck represents a single diagnostic check
type PreFlightCheck struct {
	Name       string   // Check name (e.g., "Database Integrity")
	Status     string   // "OK", "Repaired", "Warning", "Error"
	Message    string   // Human-readable message
	Details    []string // Additional details (paths, sizes, etc.)
	DurationMs int64    // Time for this specific check
}

func main() {
	// If MPM_SELECT=1, we're in a PTY selector subprocess — run the selector TUI
	if os.Getenv("MPM_SELECT") == "1" {
		exitCode := 0
		if !RunSelectorStandalone() {
			exitCode = 1
		}
		os.Exit(exitCode)
	}

	// Create router
	router := NewRouter()
	args := os.Args[1:]

	// Handle "gateway" subcommand specially
	if len(args) > 0 && args[0] == "gateway" {
		handleGatewayCommand(args[1:])
		return
	}

	// Parse flags
	args = router.parseFlags(args)
	if len(args) == 0 || args[0] == "" {
		// No command - show help
		PrintHelp()
		return
	}

	// In the unified architecture, all commands execute in-process.
	// No daemon subprocess or socket IPC is needed.
	// Execute via router
	exitCode := router.Execute(args)
	os.Exit(exitCode)
}

// ============================================================================
// Doctor Command - System Diagnostic Utility
// ============================================================================

// DoctorCheck represents a single diagnostic check result
type DoctorCheck struct {
	Name     string
	Status   string // "PASS", "WARN", "FAIL"
	Message  string
	Details  []string
	Duration string
}

// DoctorReport is the full diagnostic report
type DoctorReport struct {
	TotalChecks int
	Passed      int
	Warnings    int
	Failed      int
	Checks      []DoctorCheck
}

// Colors for terminal output (ANSI)
const (
	ansiGreen  = "\033[92m"
	ansiYellow = "\033[93m"
	ansiRed    = "\033[91m"
	ansiBold   = "\033[1m"
	ansiReset  = "\033[0m"
)

// runDoctorCommand runs the diagnostic utility without daemon
func runDoctorCommand() {
	fix := len(os.Args) >= 3 && (os.Args[2] == "--fix" || os.Args[2] == "-f")

	fmt.Printf("\n%s[%s]%s %sRunning mpm Doctor...%s\n\n", ansiBold, colorCyan("●"), ansiReset, ansiBold, ansiReset)

	report := DoctorReport{Checks: []DoctorCheck{}}

	// System Checks
	runDoctorSystemChecks(&report)

	// Environment Checks
	runDoctorEnvironmentChecks(&report)

	// Workspace Checks
	runDoctorWorkspaceChecks(&report)

	// Database Checks
	runDoctorDatabaseChecks(&report)

	// Network Checks
	runDoctorNetworkChecks(&report)

	// Dependency Checks
	runDoctorDependencyChecks(&report)

	// Apply fixes if requested
	if fix {
		runDoctorApplyFixes(&report)
	}

	// Print summary
	printDoctorSummary(&report)

	// Exit with appropriate code
	if report.Failed > 0 {
		os.Exit(1)
	}
}

// Color helper functions (no external dependencies)
func colorCyan(s string) string {
	return "\033[36m" + s + "\033[0m"
}

func colorMagenta(s string) string {
	return "\033[35m" + s + "\033[0m"
}

func colorGreen(s string) string {
	return ansiGreen + s + ansiReset
}

func colorYellow(s string) string {
	return ansiYellow + s + ansiReset
}

func colorRed(s string) string {
	return ansiRed + s + ansiReset
}

func ansiBoldString(s string) string {
	return ansiBold + s + ansiReset
}

// Doctor check functions
func runDoctorSystemChecks(report *DoctorReport) {
	fmt.Printf("  %s%sSystem Information%s\n\n", ansiBold, colorCyan("▸"), ansiReset)

	// OS Check
	check := DoctorCheck{Name: "Operating System", Status: "PASS", Details: []string{}}
	check.Message = runtime.GOOS + "/" + runtime.GOARCH
	check.Details = append(check.Details, "Go Version: "+runtime.Version())
	check.Duration = "0ms"
	report.Checks = append(report.Checks, check)
	report.Passed++
	report.TotalChecks++
	fmt.Printf("    [%s] %s\n", colorGreen("PASS"), check.Name)
	fmt.Printf("          %s\n\n", check.Message)

	// User permissions
	uid := os.Getuid()
	check = DoctorCheck{Name: "User Permissions", Status: "PASS", Details: []string{}}
	check.Message = fmt.Sprintf("Running as UID %d", uid)
	if uid == 0 {
		check.Status = "WARN"
		check.Message = "Running as root (not recommended)"
		report.Warnings++
	} else {
		report.Passed++
	}
	check.Duration = "0ms"
	report.Checks = append(report.Checks, check)
	report.TotalChecks++
	statusStr := colorGreen("PASS")
	if check.Status == "WARN" {
		statusStr = colorYellow("WARN")
	}
	fmt.Printf("    [%s] %s\n", statusStr, check.Name)
	fmt.Printf("          %s\n\n", check.Message)
}

func runDoctorEnvironmentChecks(report *DoctorReport) {
	fmt.Printf("  %s%sEnvironment Variables%s\n\n", ansiBold, colorCyan("▸"), ansiReset)

	// Workspace
	workspace := config.GetWorkspace()
	if workspace == "" {
		report.Checks = append(report.Checks, DoctorCheck{
			Name:     "MPM_WORKSPACE",
			Status:   "WARN",
			Message:  "Not set (will use default)",
			Duration: "0ms",
		})
		report.Warnings++
		report.TotalChecks++
		fmt.Printf("    [%s] %s\n", colorYellow("WARN"), "MPM_WORKSPACE")
		fmt.Printf("          %s\n\n", "Not set - using default location")
	} else {
		report.Checks = append(report.Checks, DoctorCheck{
			Name:     "MPM_WORKSPACE",
			Status:   "PASS",
			Message:  workspace,
			Duration: "0ms",
		})
		report.Passed++
		report.TotalChecks++
		fmt.Printf("    [%s] %s\n", colorGreen("PASS"), "MPM_WORKSPACE")
		fmt.Printf("          %s\n\n", workspace)
	}

	// XDG_RUNTIME_DIR
	xdgRuntime := os.Getenv("XDG_RUNTIME_DIR")
	socketDir := sockPath
	if xdgRuntime != "" {
		socketDir = filepath.Join(xdgRuntime, "mpm.sock")
	}
	report.Checks = append(report.Checks, DoctorCheck{
		Name:     "Socket Path",
		Status:   "PASS",
		Message:  socketDir,
		Duration: "0ms",
	})
	report.Passed++
	report.TotalChecks++
	fmt.Printf("    [%s] %s\n", colorGreen("PASS"), "Socket Path")
	fmt.Printf("          %s\n\n", socketDir)

	// Webhook URL
	webhookURL := os.Getenv("MPM_WEBHOOK_URL")
	if webhookURL == "" {
		report.Checks = append(report.Checks, DoctorCheck{
			Name:     "Webhook URL",
			Status:   "WARN",
			Message:  "Not configured (webhooks disabled)",
			Duration: "0ms",
		})
		report.Warnings++
		report.TotalChecks++
		fmt.Printf("    [%s] %s\n", colorYellow("WARN"), "Webhook URL")
		fmt.Printf("          %s\n\n", "Not configured - heartbeats will be local only")
	} else {
		report.Checks = append(report.Checks, DoctorCheck{
			Name:     "Webhook URL",
			Status:   "PASS",
			Message:  webhookURL,
			Duration: "0ms",
		})
		report.Passed++
		report.TotalChecks++
		fmt.Printf("    [%s] %s\n", colorGreen("PASS"), "Webhook URL")
		fmt.Printf("          %s\n\n", webhookURL)
	}
}

func runDoctorWorkspaceChecks(report *DoctorReport) {
	fmt.Printf("  %s%sWorkspace Structure%s\n\n", ansiBold, colorCyan("▸"), ansiReset)

	workspace := config.GetWorkspace()
	if workspace == "" {
		report.Checks = append(report.Checks, DoctorCheck{
			Name:     "Workspace Directory",
			Status:   "FAIL",
			Message:  "Workspace not found",
			Duration: "0ms",
		})
		report.Failed++
		report.TotalChecks++
		fmt.Printf("    [%s] %s\n", colorRed("FAIL"), "Workspace Directory")
		fmt.Printf("          %s\n\n", "Cannot determine workspace path")
		return
	}

	// Check critical directories
	dirs := map[string]string{
		"mode":     filepath.Join(workspace, "mode"),
		"persona":  filepath.Join(workspace, "persona"),
		"src/db":   filepath.Join(workspace, "src", "db"),
		"sessions": filepath.Join(workspace, "sessions"),
	}

	for name, path := range dirs {
		info, err := os.Stat(path)
		if err != nil && !os.IsNotExist(err) {
			// Unexpected error — log it and skip
			fmt.Printf("    [%s] %s\n", colorYellow("WARN"), name+" Directory")
			fmt.Printf("          Stat error: %v\n\n", err)
			continue
		}
		if os.IsNotExist(err) || info == nil {
			report.Checks = append(report.Checks, DoctorCheck{
				Name:     name + " Directory",
				Status:   "FAIL",
				Message:  "Missing: " + path,
				Duration: "0ms",
			})
			report.Failed++
			report.TotalChecks++
			fmt.Printf("    [%s] %s\n", colorRed("FAIL"), name+" Directory")
			fmt.Printf("          Missing: %s\n\n", path)
		} else if !info.IsDir() {
			report.Checks = append(report.Checks, DoctorCheck{
				Name:     name + " Directory",
				Status:   "FAIL",
				Message:  "Not a directory: " + path,
				Duration: "0ms",
			})
			report.Failed++
			report.TotalChecks++
			fmt.Printf("    [%s] %s\n", colorRed("FAIL"), name+" Directory")
			fmt.Printf("          Not a directory: %s\n\n", path)
		} else {
			report.Checks = append(report.Checks, DoctorCheck{
				Name:     name + " Directory",
				Status:   "PASS",
				Message:  path,
				Duration: "0ms",
			})
			report.Passed++
			report.TotalChecks++
			fmt.Printf("    [%s] %s\n", colorGreen("PASS"), name+" Directory")
			fmt.Printf("          %s\n\n", path)
		}
	}

	// Check socket directory permissions
	socketDir := filepath.Dir(sockPath)
	if err := testWritable(socketDir); err != nil {
		report.Checks = append(report.Checks, DoctorCheck{
			Name:     "Socket Directory Writable",
			Status:   "FAIL",
			Message:  "Cannot write to: " + socketDir,
			Details:  []string{err.Error()},
			Duration: "0ms",
		})
		report.Failed++
		report.TotalChecks++
		fmt.Printf("    [%s] %s\n", colorRed("FAIL"), "Socket Directory Writable")
		fmt.Printf("          Cannot write to: %s\n", socketDir)
		fmt.Printf("          Error: %s\n\n", err.Error())
	} else {
		report.Checks = append(report.Checks, DoctorCheck{
			Name:     "Socket Directory Writable",
			Status:   "PASS",
			Message:  socketDir,
			Duration: "0ms",
		})
		report.Passed++
		report.TotalChecks++
		fmt.Printf("    [%s] %s\n", colorGreen("PASS"), "Socket Directory Writable")
		fmt.Printf("          %s\n\n", socketDir)
	}
}

func runDoctorDatabaseChecks(report *DoctorReport) {
	fmt.Printf("  %s%sDatabase Integrity%s\n\n", ansiBold, colorCyan("▸"), ansiReset)

	// The database is ALWAYS at mpm/src/db/mpm.db
	dbPath := filepath.Join(config.GetMPMDir(), "src", "db", "mpm.db")

	info, err := os.Stat(dbPath)
	if err != nil || info.IsDir() {
		report.Checks = append(report.Checks, DoctorCheck{
			Name:     "Database File",
			Status:   "WARN",
			Message:  "No database found (first run?)",
			Duration: "0ms",
		})
		report.Warnings++
		report.TotalChecks++
		fmt.Printf("    [%s] %s\n", colorYellow("WARN"), "Database File")
		fmt.Printf("          No database found - daemon will create on first run\n\n")
		return
	}
	dbSize := info.Size()

	// Check database
	check := DoctorCheck{
		Name:    "Database File",
		Status:  "PASS",
		Message: dbPath,
		Details: []string{},
	}
	check.Details = append(check.Details, fmt.Sprintf("Size: %s", formatBytes(dbSize)))

	sqlDB, err := sql.Open("sqlite", dbPath+"?mode=ro")
	if err != nil {
		check.Status = "FAIL"
		check.Details = append(check.Details, "Cannot open: "+err.Error())
		report.Failed++
		report.TotalChecks++
		fmt.Printf("    [%s] %s\n", colorRed("FAIL"), "Database File")
		fmt.Printf("          Cannot open: %s\n\n", err.Error())
		return
	}
	defer sqlDB.Close()

	// Run integrity check
	var integrityResult string
	if err := sqlDB.QueryRow("PRAGMA integrity_check").Scan(&integrityResult); err != nil {
		check.Status = "FAIL"
		check.Details = append(check.Details, "Integrity check failed: "+err.Error())
		report.Failed++
		report.TotalChecks++
		fmt.Printf("    [%s] %s\n", colorRed("FAIL"), "Database Integrity")
		fmt.Printf("          Integrity check failed: %s\n\n", err.Error())
		return
	}

	if integrityResult != "ok" {
		check.Status = "FAIL"
		check.Details = append(check.Details, "Integrity check result: "+integrityResult)
		report.Failed++
		report.TotalChecks++
		fmt.Printf("    [%s] %s\n", colorRed("FAIL"), "Database Integrity")
		fmt.Printf("          Corruption detected: %s\n\n", integrityResult)
		return
	}

	// Quick stats
	var tableCount int
	sqlDB.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table'").Scan(&tableCount)
	check.Details = append(check.Details, fmt.Sprintf("Tables: %d", tableCount))

	report.Passed++
	report.TotalChecks++
	check.Status = "PASS"
	check.Message = "Database valid"
	check.Duration = "0ms"
	report.Checks = append(report.Checks, check)
	fmt.Printf("    [%s] %s\n", colorGreen("PASS"), "Database File")
	fmt.Printf("          %s (%s)\n", dbPath, formatBytes(dbSize))
	fmt.Printf("          Tables: %d | Integrity: OK\n\n", tableCount)
}

func runDoctorNetworkChecks(report *DoctorReport) {
	fmt.Printf("  %s%sNetwork Connectivity%s\n\n", ansiBold, colorCyan("▸"), ansiReset)

	webhookURL := os.Getenv("MPM_WEBHOOK_URL")
	if webhookURL == "" {
		check := DoctorCheck{
			Name:     "Webhook Connectivity",
			Status:   "WARN",
			Message:  "No webhook configured",
			Duration: "0ms",
		}
		report.Checks = append(report.Checks, check)
		report.Warnings++
		report.TotalChecks++
		fmt.Printf("    [%s] %s\n", colorYellow("WARN"), "Webhook Connectivity")
		fmt.Printf("          Webhook not configured\n\n")
		return
	}

	// Perform HEAD request to webhook
	start := time.Now()
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Head(webhookURL)
	duration := time.Since(start)

	if err != nil {
		check := DoctorCheck{
			Name:     "Webhook Connectivity",
			Status:   "WARN",
			Message:  "Cannot reach webhook: " + err.Error(),
			Details:  []string{"Local logging still active"},
			Duration: duration.String(),
		}
		report.Checks = append(report.Checks, check)
		report.Warnings++
		report.TotalChecks++
		fmt.Printf("    [%s] %s\n", colorYellow("WARN"), "Webhook Connectivity")
		fmt.Printf("          Cannot reach: %s\n", webhookURL)
		fmt.Printf("          Error: %s\n", err.Error())
		fmt.Printf("          (Local logging still active)\n\n")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		check := DoctorCheck{
			Name:     "Webhook Connectivity",
			Status:   "PASS",
			Message:  fmt.Sprintf("HTTP %d - API reachable", resp.StatusCode),
			Duration: duration.String(),
		}
		report.Checks = append(report.Checks, check)
		report.Passed++
		report.TotalChecks++
		fmt.Printf("    [%s] %s\n", colorGreen("PASS"), "Webhook Connectivity")
		fmt.Printf("          HTTP %d | Latency: %s\n\n", resp.StatusCode, duration)
	} else {
		check := DoctorCheck{
			Name:     "Webhook Connectivity",
			Status:   "WARN",
			Message:  fmt.Sprintf("HTTP %d - check API key", resp.StatusCode),
			Duration: duration.String(),
		}
		report.Checks = append(report.Checks, check)
		report.Warnings++
		report.TotalChecks++
		fmt.Printf("    [%s] %s\n", colorYellow("WARN"), "Webhook Connectivity")
		fmt.Printf("          HTTP %d | Latency: %s\n", resp.StatusCode, duration)
		fmt.Printf("          (Check if API key is valid)\n\n")
	}
}

func runDoctorDependencyChecks(report *DoctorReport) {
	fmt.Printf("  %s%sExternal Dependencies%s\n\n", ansiBold, colorCyan("▸"), ansiReset)

	// Check for common optional tools
	tools := []struct {
		name     string
		command  string
		required bool
	}{
		{"fzf", "fzf --version", false},
		{"sqlite3", "sqlite3 --version", false},
		{"git", "git --version", false},
	}

	for _, tool := range tools {
		_, err := exec.LookPath(tool.command)
		if err != nil {
			status := "WARN"
			report.Warnings++
			fmt.Printf("    [%s] %s\n", colorYellow(status), tool.name)
			fmt.Printf("          Not found (optional)\n\n")
			check := DoctorCheck{
				Name:     tool.name,
				Status:   status,
				Message:  "Not found (optional)",
				Duration: "0ms",
			}
			report.Checks = append(report.Checks, check)
			report.TotalChecks++
		} else {
			// Get version
			out, _ := exec.Command(tool.name, "--version").Output()
			version := strings.TrimSpace(string(out))
			if len(version) > 50 {
				version = version[:50] + "..."
			}
			report.Passed++
			report.TotalChecks++
			fmt.Printf("    [%s] %s\n", colorGreen("PASS"), tool.name)
			fmt.Printf("          %s\n\n", version)
			check := DoctorCheck{
				Name:     tool.name,
				Status:   "PASS",
				Message:  version,
				Duration: "0ms",
			}
			report.Checks = append(report.Checks, check)
		}
	}
}

func runDoctorApplyFixes(report *DoctorReport) {
	fmt.Printf("  %s%sApplying Fixes%s\n\n", ansiBold, colorCyan("▸"), ansiReset)

	// Clean up stale socket file
	if _, err := os.Stat(sockPath); err == nil {
		// Socket exists - check if daemon is actually listening
		conn, dialErr := net.DialTimeout("unix", sockPath, 500*time.Millisecond)
		if dialErr != nil {
			// No daemon listening - stale socket
			os.Remove(sockPath)
			fmt.Printf("    [%s] %s\n", colorGreen("FIXED"), "Stale Socket Removed")
			fmt.Printf("          Removed: %s\n\n", sockPath)
		} else {
			conn.Close()
			fmt.Printf("    [%s] %s\n", colorGreen("SKIP"), "Socket In Use")
			fmt.Printf("          Daemon is active\n\n")
		}
	}

	// Fix socket directory permissions
	socketDir := filepath.Dir(sockPath)
	if err := os.Chmod(socketDir, 0755); err == nil {
		fmt.Printf("    [%s] %s\n", colorGreen("FIXED"), "Socket Directory Permissions")
		fmt.Printf("          chmod 755 %s\n\n", socketDir)
	} else {
		fmt.Printf("    [%s] %s\n", colorRed("FAIL"), "Socket Directory Permissions")
		fmt.Printf("          Cannot fix: %s\n\n", err.Error())
	}

	// Fix database directory permissions (always mpm/src/db/)
	dbDir := filepath.Join(config.GetMPMDir(), "src", "db")
	if err := os.Chmod(dbDir, 0755); err == nil {
		fmt.Printf("    [%s] %s\n", colorGreen("FIXED"), "Database Directory Permissions")
		fmt.Printf("          chmod 755 %s\n\n", dbDir)
	} else {
		fmt.Printf("    [%s] %s\n", colorRed("FAIL"), "Database Directory Permissions")
		fmt.Printf("          Cannot fix: %s\n\n", err.Error())
	}
}

func printDoctorSummary(report *DoctorReport) {
	total := report.TotalChecks
	fmt.Printf("%s%s─────────────────────────────────────────────────────────────%s\n", ansiBold, colorCyan("─"), ansiReset)
	fmt.Printf("\n  %s%sSummary%s\n\n", ansiBold, ansiBoldString("▸"), ansiReset)

	fmt.Printf("    Total Checks:  %d\n", total)
	fmt.Printf("    %s Passed:  %d%s\n", colorGreen("●"), report.Passed, ansiReset)
	if report.Warnings > 0 {
		fmt.Printf("    %s Warnings: %d%s\n", colorYellow("●"), report.Warnings, ansiReset)
	}
	if report.Failed > 0 {
		fmt.Printf("    %s Failed:  %d%s\n", colorRed("●"), report.Failed, ansiReset)
	}
	fmt.Printf("\n")

	if report.Failed > 0 {
		fmt.Printf("  %s  Some checks failed. Run 'mpm doctor --fix' to attempt repairs.%s\n\n", colorRed("!"), ansiReset)
	} else if report.Warnings > 0 {
		fmt.Printf("  %s  All critical checks passed. Review warnings above.%s\n\n", colorYellow("!"), ansiReset)
	} else {
		fmt.Printf("  %s  All systems operational.%s\n\n", colorGreen("✓"), ansiReset)
	}
}

// ============================================================================
// Pre-Flight Health Check System
// ============================================================================

// runPreFlightChecks executes the diagnostic suite and returns results
// Target: Complete within 1-2 seconds for fast startup
func runPreFlightChecks() *PreFlightResult {
	startTime := time.Now()
	result := &PreFlightResult{
		Timestamp: startTime,
		Checks:    []PreFlightCheck{},
	}

	// Run all checks (order: fastest first, critical last)
	runLockfileCheck(result)
	runDirectoryCheck(result)
	runDatabaseCheck(result)
	runPersonaCheck(result)
	runPermissionsCheck(result)

	result.DurationMs = time.Since(startTime).Milliseconds()

	// Determine overall status
	hasError := false
	hasRepair := false
	for _, check := range result.Checks {
		if check.Status == "Error" {
			hasError = true
			break
		}
		if check.Status == "Repaired" {
			hasRepair = true
		}
	}
	if hasError {
		result.Status = "Failed"
	} else if hasRepair {
		result.Status = "Repaired"
	} else {
		result.Status = "Passed"
	}

	return result
}

// runLockfileCheck removes stale lockfiles from previous crash
func runLockfileCheck(result *PreFlightResult) {
	start := time.Now()
	details := []string{}

	// Check for common lockfile patterns
	lockPatterns := []string{
		filepath.Join(os.TempDir(), "mpm.lock"),
		filepath.Join(os.TempDir(), "mpm-daemon.lock"),
	}

	for _, lockPath := range lockPatterns {
		info, err := os.Stat(lockPath)
		if err == nil && !info.IsDir() {
			// Lockfile exists - check if it's stale (>24h old)
			if time.Since(info.ModTime()) > 24*time.Hour {
				if err := os.Remove(lockPath); err == nil {
					details = append(details, fmt.Sprintf("Removed stale lockfile: %s", lockPath))
				}
			}
		}
	}

	// Also check socket directory for orphaned sockets without daemon
	socketInfo, err := os.Stat(sockPath)
	if err == nil && socketInfo.Mode()&os.ModeSocket != 0 {
		// Socket exists but no one is listening - try to connect
		conn, err := net.DialTimeout("unix", sockPath, 100*time.Millisecond)
		if err != nil {
			// No one listening - orphaned socket
			os.Remove(sockPath)
			details = append(details, fmt.Sprintf("Removed orphaned socket: %s", sockPath))
		} else {
			conn.Close()
		}
	}

	status := "OK"
	message := "No stale lockfiles found"
	if len(details) > 0 {
		status = "Repaired"
		message = fmt.Sprintf("Cleaned %d stale lockfile(s)", len(details))
	}

	result.Checks = append(result.Checks, PreFlightCheck{
		Name:       "Lockfile Cleanup",
		Status:     status,
		Message:    message,
		Details:    details,
		DurationMs: time.Since(start).Milliseconds(),
	})
}

// runDirectoryCheck ensures all required directories exist
func runDirectoryCheck(result *PreFlightResult) {
	start := time.Now()
	details := []string{}

	// Determine workspace and data directories
	workspace := config.GetWorkspace()
	dirs := []string{
		filepath.Dir(sockPath),                // Socket directory (XDG_RUNTIME_DIR or ~/.mpm)
		filepath.Join(workspace, "mode"),      // Mode configurations
		config.GetPersonaPath(),               // Persona configurations (correct path: projects/mpm/persona)
		filepath.Join(workspace, "src", "db"), // Database directory
	}

	// Also check sessions directory (may not exist yet)
	sessionsPath := getSessionsDir()
	if sessionsPath != "" {
		dirs = append(dirs, sessionsPath)
	}

	for _, dir := range dirs {
		info, err := os.Stat(dir)
		if os.IsNotExist(err) {
			// Create missing directory
			if err := os.MkdirAll(dir, 0755); err != nil {
				result.Checks = append(result.Checks, PreFlightCheck{
					Name:    "Directory Check",
					Status:  "Error",
					Message: fmt.Sprintf("Failed to create directory: %s", dir),
					Details: []string{err.Error()},
				})
				return
			}
			details = append(details, fmt.Sprintf("Created: %s", dir))
		} else if err != nil || !info.IsDir() {
			result.Checks = append(result.Checks, PreFlightCheck{
				Name:    "Directory Check",
				Status:  "Error",
				Message: fmt.Sprintf("Path exists but is not a directory: %s", dir),
				Details: []string{},
			})
			return
		}
	}

	status := "OK"
	message := "All required directories exist"
	if len(details) > 0 {
		status = "Repaired"
		message = fmt.Sprintf("Created %d missing directory(ies)", len(details))
	}

	result.Checks = append(result.Checks, PreFlightCheck{
		Name:       "Directory Check",
		Status:     status,
		Message:    message,
		Details:    details,
		DurationMs: time.Since(start).Milliseconds(),
	})
}

// runDatabaseCheck validates database integrity (pre-flight)
func runDatabaseCheck(result *PreFlightResult) {
	start := time.Now()

	// The database is ALWAYS at mpm/src/db/mpm.db
	dbPath := filepath.Join(config.GetMPMDir(), "src", "db", "mpm.db")

	info, err := os.Stat(dbPath)
	if err != nil || info.IsDir() {
		// No database found - this is OK for first run
		result.Checks = append(result.Checks, PreFlightCheck{
			Name:       "Database Integrity",
			Status:     "OK",
			Message:    "No database file found (first run expected)",
			Details:    []string{},
			DurationMs: time.Since(start).Milliseconds(),
		})
		return
	}
	dbSize := info.Size()

	// Database exists - check if it's readable and valid
	details := []string{fmt.Sprintf("Database: %s (%s)", dbPath, formatBytes(dbSize))}

	// Try to open the database with SQLite
	sqlDB, err := openDatabase(dbPath)
	if err != nil {
		result.Checks = append(result.Checks, PreFlightCheck{
			Name:       "Database Integrity",
			Status:     "Error",
			Message:    fmt.Sprintf("Cannot open database: %v", err),
			Details:    details,
			DurationMs: time.Since(start).Milliseconds(),
		})
		return
	}
	defer sqlDB.Close()

	// Run PRAGMA integrity_check
	var integrityResult string
	row := sqlDB.QueryRow("PRAGMA integrity_check")
	if err := row.Scan(&integrityResult); err != nil {
		details = append(details, fmt.Sprintf("Integrity check failed to run: %v", err))
		result.Checks = append(result.Checks, PreFlightCheck{
			Name:       "Database Integrity",
			Status:     "Warning",
			Message:    "Integrity check query failed",
			Details:    details,
			DurationMs: time.Since(start).Milliseconds(),
		})
		return
	}

	if integrityResult != "ok" {
		details = append(details, fmt.Sprintf("Integrity check result: %s", integrityResult))
		result.Checks = append(result.Checks, PreFlightCheck{
			Name:       "Database Integrity",
			Status:     "Error",
			Message:    "Database corruption detected",
			Details:    details,
			DurationMs: time.Since(start).Milliseconds(),
		})
		return
	}

	details = append(details, "Integrity check: ok")

	// Quick table count check (non-blocking)
	var tableCount int
	sqlDB.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table'").Scan(&tableCount)
	details = append(details, fmt.Sprintf("Tables: %d", tableCount))

	result.Checks = append(result.Checks, PreFlightCheck{
		Name:       "Database Integrity",
		Status:     "OK",
		Message:    "Database valid",
		Details:    details,
		DurationMs: time.Since(start).Milliseconds(),
	})
}

// openDatabase opens a SQLite database and returns the connection
// Uses modernc.org/sqlite (pure Go, no cgo)
func openDatabase(path string) (*sql.DB, error) {
	return sql.Open("sqlite", path+"?mode=ro") // Read-only mode for checks
}

// runPersonaCheck validates active persona exists
func runPersonaCheck(result *PreFlightResult) {
	start := time.Now()

	personaPath := config.GetPersonaPath()
	details := []string{}

	// Check if persona directory exists
	if _, err := os.Stat(personaPath); os.IsNotExist(err) {
		// Persona directory missing - CRITICAL
		result.Checks = append(result.Checks, PreFlightCheck{
			Name:       "Persona Validation",
			Status:     "Error",
			Message:    "Persona directory missing: " + personaPath,
			Details:    []string{"Daemon cannot start without persona directory"},
			DurationMs: time.Since(start).Milliseconds(),
		})
		return
	}
	details = append(details, fmt.Sprintf("Persona dir: %s", personaPath))

	// Check for active persona marker or default persona
	defaultPersonaPath := filepath.Join(personaPath, "default.json")
	activePersonaPath := filepath.Join(personaPath, "active.json")

	defaultExists := fileExists(defaultPersonaPath)
	activeExists := fileExists(activePersonaPath)

	if !defaultExists && !activeExists {
		// No persona found - CRITICAL (daemon needs at least a default)
		result.Checks = append(result.Checks, PreFlightCheck{
			Name:       "Persona Validation",
			Status:     "Error",
			Message:    "No default or active persona found",
			Details:    details,
			DurationMs: time.Since(start).Milliseconds(),
		})
		return
	}

	if defaultExists {
		details = append(details, fmt.Sprintf("Default persona: %s", defaultPersonaPath))
	}
	if activeExists {
		details = append(details, fmt.Sprintf("Active persona: %s", activePersonaPath))
	}

	// Count available personas
	entries, err := os.ReadDir(personaPath)
	if err == nil {
		personaCount := 0
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
				personaCount++
			}
		}
		details = append(details, fmt.Sprintf("Total personas: %d", personaCount))
	}

	result.Checks = append(result.Checks, PreFlightCheck{
		Name:       "Persona Validation",
		Status:     "OK",
		Message:    "Persona configuration valid",
		Details:    details,
		DurationMs: time.Since(start).Milliseconds(),
	})
}

// runPermissionsCheck verifies R/W permissions on critical paths
func runPermissionsCheck(result *PreFlightResult) {
	start := time.Now()
	details := []string{}

	// Critical paths that must be writable
	writablePaths := []string{
		filepath.Dir(sockPath),                            // Socket directory
		filepath.Join(config.GetWorkspace(), "src", "db"), // DB directory
	}

	// Check read/write access
	for _, path := range writablePaths {
		if err := testWritable(path); err != nil {
			details = append(details, fmt.Sprintf("NOT WRITABLE: %s", path))
		} else {
			details = append(details, fmt.Sprintf("WRITABLE: %s", path))
		}
	}

	status := "OK"
	message := "All critical paths have correct permissions"
	if len(details) > 0 {
		hasError := false
		for _, d := range details {
			if strings.HasPrefix(d, "NOT") {
				hasError = true
				break
			}
		}
		if hasError {
			status = "Error"
			message = "Some critical paths are not writable"
		}
	}

	result.Checks = append(result.Checks, PreFlightCheck{
		Name:       "Permissions Check",
		Status:     status,
		Message:    message,
		Details:    details,
		DurationMs: time.Since(start).Milliseconds(),
	})
}

// testWritable checks if a directory is writable by attempting to create a temp file
func testWritable(dir string) error {
	testFile := filepath.Join(dir, ".mpm-perm-test-"+fmt.Sprintf("%d", os.Getpid()))
	defer os.Remove(testFile)
	f, err := os.Create(testFile)
	if err != nil {
		return err
	}
	f.Close()
	return nil
}

// getSessionsDir returns the sessions directory path
func getSessionsDir() string {
	// Try config first
	if config, err := config.LoadConfig(); err == nil && config.SessionsDir != "" {
		return config.SessionsDir
	}
	// Fallback to workspace (workspace IS the mpm directory, so sessions is a sibling)
	workspace := config.GetWorkspace()
	return filepath.Join(workspace, "sessions")
}

// fileExists checks if a file exists
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// formatBytes returns a human-readable byte count
func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

// socketPath returns the daemon socket path, preferring user-specific locations
func socketPath() string {
	// Try XDG_RUNTIME_DIR first (Linux/BSD standard)
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		sockPath := filepath.Join(xdg, "mpm.sock")
		os.MkdirAll(xdg, 0700)
		return sockPath
	}
	// Fallback to ~/.mpm/mpm.sock for portability
	if home, err := os.UserHomeDir(); err == nil {
		mpmDir := filepath.Join(home, ".mpm")
		os.MkdirAll(mpmDir, 0700)
		return filepath.Join(mpmDir, "mpm.sock")
	}
	// Last resort - /tmp (note: shared in multi-user systems!)
	return "/tmp/mpm.sock"
}

// ============================================================================
// Help System (Lipgloss-styled)
// ============================================================================

// helpCmd represents a command in the help menu
type helpCmd struct {
	name    string
	desc    string
	hasSubs bool // Shows ▸ indicator
}

var (
	helpGold    = lipgloss.Color("220")
	helpCyan    = lipgloss.Color("87")
	helpMagenta = lipgloss.Color("213")
	helpDim     = lipgloss.Color("245")
	helpGreen   = lipgloss.Color("84")
	helpBorder  = lipgloss.Color("99")

	helpTitle = lipgloss.NewStyle().
			Foreground(helpGold).
			Bold(true).
			Align(lipgloss.Center)

	helpSection = lipgloss.NewStyle().
			Foreground(helpMagenta).
			Bold(true).
			Padding(1, 0, 0, 0)

	helpCommand = lipgloss.NewStyle().
			Foreground(helpCyan)

	helpDesc = lipgloss.NewStyle().
			Foreground(helpDim)

	helpBorderStyle = lipgloss.NewStyle().
			BorderStyle(lipgloss.RoundedBorder()).
			BorderForeground(helpBorder).
			Padding(1, 2)

	helpTip = lipgloss.NewStyle().
		Foreground(helpDim).
		Italic(true)
)

// printHelp displays the mpm help text with lipgloss styling
func printHelp() {
	// Build sections
	coreSection := buildHelpSection("Core Commands", []helpCmd{
		{"persona", "Persona management", true},
		{"mode", "Mode management", true},
		{"memory", "Memory management", true},
		{"session", "Session management", true},
		{"topics", "Topic operations", true},
		{"reference", "Reference library", true},
		{"lesson", "Lesson operations", true},
		{"recall <query>", "Semantic memory search", false},
		{"compile", "Compile JSON to database", false},
		{"synthesize [uuid]", "Generate memory summaries", false},
	})

	sysSection := buildHelpSection("System", []helpCmd{
		{"watch", "File watcher (start/stop/status)", true},
		{"doctor", "Run diagnostics", false},
	})

	infoSection := buildHelpSection("Info", []helpCmd{
		{"help", "Show this help", false},
		{"version", "Show version info", false},
		{"prime-directives", "Show 808 directives", false},
		{"gateway", "Gateway control", true},
	})

	// Assemble with border
	mainStyle := lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(helpBorder).
		Padding(1, 2).
		Margin(1)

	// Title
	titleStyle := lipgloss.NewStyle().
		Foreground(helpGold).
		Bold(true).
		Align(lipgloss.Center).
		Render("⟨ mpm ⟩  Memory-Persona-Mode Manager")

	subtitleStyle := lipgloss.NewStyle().
		Foreground(helpCyan).
		Align(lipgloss.Center).
		Render("Your long-term memory and persona system")

	content := "\n" + titleStyle + "\n" + subtitleStyle + "\n\n" +
		coreSection + "\n" +
		sysSection + "\n" +
		infoSection + "\n"

	fmt.Println(mainStyle.Render(content))
}

// buildHelpSection creates a styled section with commands, left-aligned
func buildHelpSection(title string, cmds []helpCmd) string {
	subStyle := lipgloss.NewStyle().Foreground(helpMagenta).Render("▸")

	var b strings.Builder
	b.WriteString("\n")
	b.WriteString(helpSection.Render(title))
	b.WriteString("\n")

	for _, c := range cmds {
		// Build indicators
		indicators := ""
		if c.hasSubs {
			indicators += subStyle
		}
		if indicators == "" {
			indicators = "  "
		} else {
			indicators += " "
		}

		cmdStr := helpCommand.Render(c.name)
		descStr := helpDesc.Render(c.desc)
		// Left-aligned: indicator + command + description
		line := fmt.Sprintf("  %s%s  %s\n", indicators, cmdStr, descStr)
		b.WriteString(line)
	}
	return b.String()
}

// printGatewayHelp outputs gateway-specific help with lipgloss styling
func printGatewayHelp() {
	gatewayCmds := []helpCmd{
		{"help", "Show this help", false},
		{"start", "Start or connect to gateway", false},
		{"stop", "Stop the gateway", false},
		{"restart", "Restart the gateway", false},
		{"status", "Show gateway status", false},
	}

	var b strings.Builder
	b.WriteString("\n")
for _, c := range gatewayCmds {
		cmdStr := helpCommand.Render(c.name)
		descStr := helpDesc.Render(c.desc)
		b.WriteString(fmt.Sprintf("  %s  %s\n", cmdStr, descStr))
	}

	b.WriteString("\n")
	b.WriteString(helpTip.Render("  mpm gateway start  # Start/restart gateway"))

	fmt.Print(b.String() + "\n\n")
}

// parseWorkspaceFlag extracts --workspace from args (doesn't mutate global state)
func parseWorkspaceFlag(args []string) string {
	for i, arg := range args {
		if arg == "--workspace" && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(arg, "--workspace=") {
			return strings.TrimPrefix(arg, "--workspace=")
		}
	}
	return ""
}
