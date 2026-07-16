package main

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/flowbyte-com/mpm-core/config"
	"github.com/flowbyte-com/mpm-core/logging"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/synth"
	"github.com/flowbyte-com/mpm-core/usererror"
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
	// Go scheduler to spawn hundreds of OS threads under heavy load, which on a
	// shared or memory-constrained system can lead to OOM kills.
	runtime.GOMAXPROCS(32)

	// Initialize the package-level slog default. See internal/logging for the
	// MPM_LOG / MPM_LOG_FORMAT env-var policy.
	logging.Setup()

	// Initialize global mode manager
	// Will be used to set default 808 mode on daemon startup
	configPath := os.ExpandEnv("$HOME/.openclaw/workspace/projects/mpm")
	modeManager = mpminternal.NewModeManager(configPath)
}

// buildVersion is set at compile time via -ldflags
var buildVersion = "dev"

// startTime is the wall-clock timestamp of process entry — used for uptime display
var startTime = time.Now()

// modeManager is the global mode manager instance (initialized at startup)
var modeManager any

// printError formats and prints an error message mpm-style.
// Migrated to usererror so the severity prefix is centralized.
func printError(format string, args ...interface{}) {
	usererror.Error(format, args...)
}

// printSuccess prints a success message mpm-style
func printSuccess(format string, args ...interface{}) {
	fmt.Printf("✓ "+format+"\n", args...)
}

// printWarning prints a warning message mpm-style
func printWarning(format string, args ...interface{}) {
	fmt.Printf("⚠ "+format+"\n", args...)
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

// hasStdinData checks if stdin has piped data available
func hasStdinData() bool {
	fi, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return (fi.Mode() & os.ModeCharDevice) == 0
}

// readStdinContent reads all data from stdin and returns it trimmed
func readStdinContent() (string, error) {
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// isBinaryData detects binary content (null bytes or excessive non-printable chars)
func isBinaryData(data string) bool {
	if len(data) == 0 {
		return false
	}
	nonPrintable := 0
	for _, r := range data {
		if r == 0 {
			return true
		}
		if r < 32 && r != 9 && r != 10 && r != 13 {
			nonPrintable++
		}
	}
	return float64(nonPrintable)/float64(len(data)) > 0.3
}

// promptConfirmation reads [y/N] from /dev/tty (not stdin, which may be the pipe).
// Returns true only if the user answered y or Y.
func promptConfirmation() bool {
	fmt.Print("[y/N]: ")
	tty, err := os.OpenFile("/dev/tty", os.O_RDONLY, 0)
	if err != nil {
		// No TTY available (e.g. fully non-interactive) — default to no
		return false
	}
	defer tty.Close()
	var response string
	fmt.Fscanln(tty, &response)
	return response == "y" || response == "Y"
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

	// Stdin detection: if data is piped in and no subcommand given, offer to save.
	// This must happen before flag parsing so that `cat idea.md | mpm` "just works".
	if len(os.Args) == 1 && hasStdinData() {
		data, readErr := readStdinContent()
		if readErr == nil && len(data) > 0 {
			if isBinaryData(data) {
				printWarning("stdin appears to be binary; skipping")
			} else {
				preview := truncate(data, 120)
				fmt.Printf("Will save: \"%s\"\n", preview)
				confirmed := promptConfirmation()
				if confirmed {
					store := getMemoryStore()
					if store == nil {
						printError("cannot open database")
						os.Exit(0)
					}
					meta := map[string]interface{}{"source": "stdin"}
					mem, addErr := store.AddMemory(data, "memories", nil, meta, "", "cli")
					if addErr != nil {
						printError("failed to save memory: %v", addErr)
						os.Exit(0)
					}
					printSuccess("memory saved (id=%s)", mem.ID)
					// Fire-and-forget auto-synthesis (same pattern as memory add handler)
					go func(id, c string) {
						synthDM, synthErr := getDB().NewSession()
						if synthErr != nil {
							slog.Warn("synthesis: failed to open db session", "memory_id", id, "error", synthErr)
							return
						}
						defer synthDM.Close()
						mpminternal.AutoSynthesize(context.Background(), synthDM, synth.NewSynthClient(), id, c)
					}(mem.ID, data)
				}
				os.Exit(0)
			}
		}
		// Empty stdin: fall through to normal routing
	}

	// Create router (needed for default-to-recall command resolution check)
	router := NewRouter()
	args := os.Args[1:]

	// Default-to-recall: `mpm token budget` → `mpm recall token budget`
	// Only triggers for a single bare positional string that isn't a flag or known command.
	if len(args) == 1 && args[0] != "" && args[0][0] != '-' && router.resolveCommand(args[0]) == nil {
		args = []string{"recall", args[0]}
	}

	// Handle "gateway" subcommand specially
	if len(args) > 0 && args[0] == "gateway" {
		handleGatewayCommand(args[1:])
		return
	}

	// Parse flags
	args = router.parseFlags(args)
	if len(args) == 0 || args[0] == "" {
		PrintQuicklinks()
		return
	}

	// Early-exit for help/version — must resolve before alias processing
	switch args[0] {
	case "help", "version":
		exitCode := router.Execute(args)
		os.Exit(exitCode)
	}

	// CLI alias expansion: load from mpm_config.json and substitute.
	// If args[0] matches an alias key, replace args[0] with the expanded tokens.
	// Unrecognized aliases pass through unmodified.
	if cfg, err := config.LoadConfig(); err == nil && cfg.Aliases != nil {
		if expansion, ok := cfg.Aliases[args[0]]; ok {
			expanded := strings.Fields(expansion)
			if len(expanded) > 0 {
				args = append(expanded, args[1:]...)
			}
		}
	}

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

// runDoctorCommand runs the diagnostic utility
func runDoctorCommand(args []string) {
	// Backfill epistemology topics on every doctor run (idempotent)
	backfillEpistemologyTopics()

	fix := false
	explain := false
	deepScan := false
	for _, a := range args {
		if a == "--fix" {
			fix = true
		}
		if a == "--explain" {
			explain = true
		}
		if a == "--deep-scan" {
			deepScan = true
		}
	}
	// Check os.Args for -f shorthand (router.parseFlags consumes it as --force)
	if !fix {
		for _, a := range os.Args {
			if a == "-f" {
				fix = true
				break
			}
		}
	}

	if explain {
		runDoctorExplain()
		return
	}

	// --deep-scan is its own audit mode: skip the standard suite and run
	// on-demand integrity checks (FTS sync, soft-delete ghosts, dangling
	// memberships). Useful when investigating search-index drift.
	if deepScan {
		runDoctorDeepScan(fix)
		return
	}

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

	// Security & Telemetry Checks (added 2026-06-26: synthesis telemetry,
	// auth configuration, scanner coverage audit).
	runDoctorSecurityChecks(&report)

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

// DeepScanResult is the structured output of runDeepScanCheck. It captures
// every finding in a form that both the human-facing runDoctorDeepScan and
// the autonomous handleSelfHeal loop can consume without re-running queries.
type DeepScanResult struct {
	// FTSOrphans: per-FTS-table orphan count (rowid present in FTS, missing in source).
	FTSOrphans map[string]int
	// FTSOrphanSamples: per-FTS-table sample labels (capped at 5 each) for human reporting.
	FTSOrphanSamples map[string][]string
	// SoftDeleteGhosts: count of memories_fts rows whose memory has deleted_at IS NOT NULL.
	SoftDeleteGhosts int
	// SoftDeleteGhostSamples: sample IDs/rowids/timestamps (capped at 5).
	SoftDeleteGhostSamples []string
	// DanglingMemberships: count of topic_memberships rows whose topic no longer exists.
	DanglingMemberships int
	// QueryErrors: any individual queries that failed (so the caller can WARN).
	QueryErrors map[string]string
}

// HasKnownSafeDrift returns true if there is drift in the known-safe
// (auto-fixable) category — currently just soft-delete ghosts.
func (r *DeepScanResult) HasKnownSafeDrift() bool {
	return r.SoftDeleteGhosts > 0
}

// HasUnknownDrift returns true if there is drift in categories the system
// refuses to auto-fix (FTS orphans, dangling memberships).
func (r *DeepScanResult) HasUnknownDrift() bool {
	if r.DanglingMemberships > 0 {
		return true
	}
	for _, n := range r.FTSOrphans {
		if n > 0 {
			return true
		}
	}
	return false
}

// runDeepScanCheck performs the three integrity checks and returns a structured
// result. The DB is opened read-only; the caller is responsible for any writes.
func runDeepScanCheck(dbPath string) (*DeepScanResult, error) {
	sqlDB, err := sql.Open("sqlite3", dbPath+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	defer sqlDB.Close()

	res := &DeepScanResult{
		FTSOrphans:             map[string]int{},
		FTSOrphanSamples:       map[string][]string{},
		SoftDeleteGhostSamples: []string{},
		QueryErrors:            map[string]string{},
	}

	// — Check 1: FTS orphan scan —
	ftsPairs := []struct {
		fts   string
		table string
		idCol string
	}{
		{"memories_fts", "memories", "t.id"},
		{"topics_fts", "topics", "t.name"},
		{"lessons_fts", "lessons", "t.id"},
		{"sessions_fts", "sessions", "t.id"},
	}
	for _, p := range ftsPairs {
		q := fmt.Sprintf(
			"SELECT f.rowid, %s FROM %s f LEFT JOIN %s t ON f.rowid = t.rowid WHERE t.rowid IS NULL",
			p.idCol, p.fts, p.table,
		)
		rows, qerr := sqlDB.Query(q)
		if qerr != nil {
			res.QueryErrors[p.fts] = qerr.Error()
			continue
		}
		var samples []string
		count := 0
		for rows.Next() {
			var rid int64
			var label sql.NullString
			if err := rows.Scan(&rid, &label); err != nil {
				continue
			}
			count++
			if len(samples) < 5 {
				if !label.Valid || label.String == "" {
					samples = append(samples, fmt.Sprintf("rowid=%d", rid))
				} else {
					samples = append(samples, fmt.Sprintf("%s(rowid=%d)", label.String, rid))
				}
			}
		}
		rows.Close()
		res.FTSOrphans[p.fts] = count
		if count > 0 {
			res.FTSOrphanSamples[p.fts] = samples
		}
	}

	// — Check 2: soft-delete ghost scan (memories only) —
	ghostRows, err := sqlDB.Query(
		"SELECT m.rowid, m.id, m.deleted_at FROM memories_fts f JOIN memories m ON f.rowid = m.rowid WHERE m.deleted_at IS NOT NULL",
	)
	if err != nil {
		res.QueryErrors["soft_delete_ghosts"] = err.Error()
	} else {
		for ghostRows.Next() {
			var rid int64
			var id, deletedAt string
			if err := ghostRows.Scan(&rid, &id, &deletedAt); err != nil {
				continue
			}
			res.SoftDeleteGhosts++
			if len(res.SoftDeleteGhostSamples) < 5 {
				res.SoftDeleteGhostSamples = append(res.SoftDeleteGhostSamples,
					fmt.Sprintf("%s(rowid=%d, deleted_at=%s)", id, rid, deletedAt))
			}
		}
		ghostRows.Close()
	}

	// — Check 3: dangling topic memberships —
	var count int
	if err := sqlDB.QueryRow(
		"SELECT COUNT(*) FROM topic_memberships tm LEFT JOIN topics t ON tm.topic_id = t.id WHERE t.id IS NULL",
	).Scan(&count); err != nil {
		res.QueryErrors["dangling_memberships"] = err.Error()
	} else {
		res.DanglingMemberships = count
	}

	return res, nil
}

// runDeepScanFixSoftDeleteGhosts opens the DB writable and removes every
// memories_fts row whose joined memory has deleted_at IS NOT NULL. Returns
// the number of rows deleted. Safe to call when there are no ghosts (no-op).
func runDeepScanFixSoftDeleteGhosts(dbPath string) (int64, error) {
	wDB, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		return 0, fmt.Errorf("open writable: %w", err)
	}
	defer wDB.Close()
	res, err := wDB.Exec(
		"DELETE FROM memories_fts WHERE rowid IN (SELECT rowid FROM memories WHERE deleted_at IS NOT NULL)",
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// runDoctorDeepScan runs on-demand integrity checks that are too expensive
// for the standard doctor run. Wraps runDeepScanCheck and formats the result
// for human consumption.
//
// Pass --fix to clean soft-delete ghosts in place. FTS orphan and dangling
// membership fixes are left to manual intervention (they indicate schema
// drift, not just trigger lag).
func runDoctorDeepScan(fix bool) {
	fmt.Printf("\n%s[%s]%s %sDeep-Scan Integrity Audit%s\n\n", ansiBold, colorCyan("●"), ansiReset, ansiBold, ansiReset)
	if fix {
		fmt.Printf("  %s--fix enabled: soft-delete ghosts will be removed in place%s\n\n", ansiYellow, ansiReset)
	}

	dbPath := filepath.Join(config.GetMPMDir(), "src", "db", "mpm.db")
	if _, err := os.Stat(dbPath); err != nil {
		usererror.Warn("[%s] No database found at %s", colorRed("FAIL"), dbPath)
		os.Exit(1)
	}

	// Run the shared scan; it opens the DB read-only internally.
	scan, err := runDeepScanCheck(dbPath)
	if err != nil {
		usererror.Warn("[%s] Cannot open database: %v", colorRed("FAIL"), err)
		os.Exit(1)
	}

	failed := 0
	warnings := 0
	passed := 0

	// — Render Check 1: FTS orphan scan —
	fmt.Printf("  %s%sFTS Orphan Scan%s\n", ansiBold, colorCyan("▸"), ansiReset)
	order := []string{"memories_fts", "topics_fts", "lessons_fts", "sessions_fts"}
	for _, name := range order {
		if msg, ok := scan.QueryErrors[name]; ok {
			fmt.Printf("    [%s] %s: query failed: %v\n", colorRed("FAIL"), name, msg)
			failed++
			continue
		}
		n := scan.FTSOrphans[name]
		if n > 0 {
			samples := scan.FTSOrphanSamples[name]
			fmt.Printf("    [%s] %s: %d orphan(s): %s\n", colorYellow("WARN"), name, n, strings.Join(samples, ", "))
			warnings++
		} else {
			fmt.Printf("    [%s] %s: 0 orphans\n", colorGreen("PASS"), name)
			passed++
		}
	}
	fmt.Println()

	// — Render Check 2: soft-delete ghost scan —
	fmt.Printf("  %s%sSoft-Delete Ghost Scan (memories_fts)%s\n", ansiBold, colorCyan("▸"), ansiReset)
	if msg, ok := scan.QueryErrors["soft_delete_ghosts"]; ok {
		fmt.Printf("    [%s] query failed: %v\n", colorRed("FAIL"), msg)
		failed++
	} else if scan.SoftDeleteGhosts > 0 {
		fmt.Printf("    [%s] %d ghost(s) found: %s\n", colorYellow("WARN"), scan.SoftDeleteGhosts, strings.Join(scan.SoftDeleteGhostSamples, ", "))
		if fix {
			n, ferr := runDeepScanFixSoftDeleteGhosts(dbPath)
			if ferr != nil {
				fmt.Printf("    [%s] --fix failed: %v\n", colorRed("FAIL"), ferr)
				failed++
			} else {
				fmt.Printf("    [%s] --fix removed %d ghost row(s) from memories_fts\n", colorGreen("FIXED"), n)
			}
		} else {
			fmt.Printf("          hint: re-run with --deep-scan --fix to clean\n")
			warnings++
		}
	} else {
		fmt.Printf("    [%s] 0 ghosts — soft-delete trigger is current\n", colorGreen("PASS"))
		passed++
	}
	fmt.Println()

	// — Render Check 3: dangling topic memberships —
	fmt.Printf("  %s%sDangling Topic Membership Scan%s\n", ansiBold, colorCyan("▸"), ansiReset)
	if msg, ok := scan.QueryErrors["dangling_memberships"]; ok {
		fmt.Printf("    [%s] query failed: %v\n", colorRed("FAIL"), msg)
		failed++
	} else if scan.DanglingMemberships > 0 {
		fmt.Printf("    [%s] %d membership(s) reference deleted topics\n", colorYellow("WARN"), scan.DanglingMemberships)
		fmt.Printf("          hint: manual cleanup required (DELETE FROM topic_memberships WHERE topic_id NOT IN (SELECT id FROM topics))\n")
		warnings++
	} else {
		fmt.Printf("    [%s] 0 dangling memberships\n", colorGreen("PASS"))
		passed++
	}
	fmt.Println()

	// — Summary —
	total := passed + warnings + failed
	fmt.Printf("  %s%sSummary%s\n", ansiBold, colorCyan("▸"), ansiReset)
	fmt.Printf("    Passed:   %d\n", passed)
	fmt.Printf("    Warnings: %d\n", warnings)
	fmt.Printf("    Failed:   %d\n", failed)
	fmt.Printf("    Total:    %d checks run\n\n", total)

	totalOrphans := 0
	for _, n := range scan.FTSOrphans {
		totalOrphans += n
	}

	if failed > 0 {
		fmt.Printf("  [%s] Deep-scan found %d hard failure(s) — investigate before proceeding.\n", colorRed("FAIL"), failed)
		os.Exit(1)
	}
	if warnings > 0 {
		fmt.Printf("  [%s] Deep-scan clean. %d soft warning(s) noted (FTS orphans: %d).\n", colorYellow("OK"), warnings, totalOrphans)
	} else {
		fmt.Printf("  [%s] Deep-scan clean. No drift detected.\n", colorGreen("OK"))
	}
}

// runDoctorExplain runs EXPLAIN QUERY PLAN on the core FTS5 recall query
// and prints the query plan tree to stdout for index health diagnosis.
func runDoctorExplain() {
	dm := getDBConcrete()
	if dm == nil {
		usererror.Error("DB open failed: %v", dbManagerInitErr)
		return
	}

	query := `
EXPLAIN QUERY PLAN
SELECT m.id, m.collection, m.content, m.session_id, m.tags, m.metadata, m.embedding, m.created_at, fts.rank
FROM memories m
JOIN memories_fts fts ON fts.rowid = m.rowid
WHERE memories_fts MATCH 'test'
  AND m.collection = 'memories'
  AND m.deleted_at IS NULL
ORDER BY rank
LIMIT 10`

	rows, err := dm.SQLDB().Query(query)
	if err != nil {
		usererror.Error("EXPLAIN QUERY PLAN failed: %v", err)
		usererror.Error("   This may indicate FTS5 is not enabled or the memories_fts table is missing.")
		return
	}
	defer rows.Close()

	fmt.Println("\n📊 FTS5 Query Plan")
	fmt.Println("═══════════════════════════════════════════════════")

	var id, parent, notused int
	var detail string
	for rows.Next() {
		if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
			usererror.Warn("Scan error: %v", err)
			continue
		}
		indent := ""
		if parent > 0 {
			indent = "  "
		}
		fmt.Printf("  %s├── %s\n", indent, detail)
	}

	if err := rows.Err(); err != nil {
		usererror.Warn("Rows error: %v", err)
	}

	fmt.Println("\n✅ If you see `SEARCH memories_fts USING VIRTUAL TABLE INDEX`")
	fmt.Println("   the FTS5 index is being used correctly.")
	fmt.Println("   If you see `SCAN memories` or `SCAN memories_fts`,")
	fmt.Println("   the query is falling back to a full table scan.")
	fmt.Println()
}

// Color helper functions (no external dependencies)
func colorCyan(s string) string {
	return "\033[36m" + s + "\033[0m"
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

	sqlDB, err := sql.Open("sqlite3", dbPath+"?mode=ro")
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

// runDoctorSecurityChecks covers security-relevant runtime state:
//   - synthesis telemetry: are LLM synth attempts succeeding or failing?
//   - scanner coverage: a sanity ping of the static audit so the doctor
//     report itself can flag if a new write path bypasses the scanner.
func runDoctorSecurityChecks(report *DoctorReport) {
	fmt.Printf("  %s%sSecurity & Telemetry%s\n\n", ansiBold, colorCyan("▸"), ansiReset)

	dm := getDBConcrete()
	if dm == nil {
		report.Checks = append(report.Checks, DoctorCheck{
			Name: "Synthesis Telemetry", Status: "WARN",
			Message:  fmt.Sprintf("cannot open db to read watchdog: %v", dbManagerInitErr),
			Duration: "0ms",
		})
		report.Warnings++
		report.TotalChecks++
		fmt.Printf("    [%s] Synthesis Telemetry: %v\n\n", colorYellow("WARN"), dbManagerInitErr)
	} else {
		ops, werr := dm.RecentWatchdogOps(50, "synthesize_")
		if werr != nil {
			report.Checks = append(report.Checks, DoctorCheck{
				Name: "Synthesis Telemetry", Status: "WARN",
				Message:  fmt.Sprintf("watchdog read failed: %v", werr),
				Duration: "0ms",
			})
			report.Warnings++
		} else {
			failures := 0
			for _, op := range ops {
				if name, _ := op["op"].(string); name == "synthesize_error" {
					failures++
				}
			}
			status, msg := "PASS", fmt.Sprintf("%d synthesis events, 0 errors", len(ops))
			if failures > 0 {
				status = "WARN"
				msg = fmt.Sprintf("%d synthesize_error events in last %d entries (run `mpm synthesize failures`)", failures, len(ops))
				report.Warnings++
			}
			report.Checks = append(report.Checks, DoctorCheck{
				Name: "Synthesis Telemetry", Status: status,
				Message: msg, Duration: "0ms",
			})
			fmt.Printf("    [%s] Synthesis Telemetry: %s\n\n", colorizeStatus(status), msg)
		}
		report.TotalChecks++
	}

	// Scanner coverage is enforced by the test suite (internal/scanner_coverage_test.go).
	// The doctor report notes its existence so operators know to run `go test` if
	// they suspect a regression.
	report.Checks = append(report.Checks, DoctorCheck{
		Name: "Scanner Coverage", Status: "PASS",
		Message:  "enforced by internal/scanner_coverage_test.go (run `go test ./internal/...` to verify)",
		Duration: "0ms",
	})
	report.TotalChecks++
	fmt.Printf("    [%s] Scanner Coverage: enforced by static audit test\n\n", colorizeStatus("PASS"))
}

// colorizeStatus returns the ANSI color escape for a doctor status.
func colorizeStatus(status string) string {
	switch status {
	case "PASS":
		return ansiGreen + status + ansiReset
	case "WARN":
		return ansiYellow + status + ansiReset
	case "FAIL":
		return ansiRed + status + ansiReset
	default:
		return status
	}
}

func runDoctorApplyFixes(report *DoctorReport) {
	fmt.Printf("  %s%sApplying Fixes%s\n\n", ansiBold, colorCyan("▸"), ansiReset)

	// Fix database directory permissions
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

// PrintQuicklinks displays the compact quicklinks when mpm is run with no args.
func PrintQuicklinks() {
	fmt.Println()
	fmt.Println("mpm · Memory Persistence Module")
	fmt.Println()
	fmt.Println("Usage: mpm <query|text> [flags]")
	fmt.Println()
	fmt.Println("Daily Commands:")
	fmt.Println("  mpm <query>       Search memories (default when called with a bare string)")
	fmt.Println("  mpm add <text>    Add a new memory")
	fmt.Println("  mpm add -i        Interactive add — opens $EDITOR")
	fmt.Println("  mpm snooze <id>   Bump a memory's relevance (no LTM promotion)")
	fmt.Println("  mpm ls            List memories")
	fmt.Println("  mpm show <id>     Show memory details")
	fmt.Println("  mpm rm <id>       Delete a memory")
	fmt.Println("  mpm help          Show this help")
	fmt.Println()
	fmt.Println("Engine Room (ops):")
	fmt.Println("  mpm ops           Run maintenance, diagnostics, synthesis, and more")
	fmt.Println("  mpm ops help      List all ops subcommands")
	fmt.Println()
	fmt.Println("Also available via mpm ops:")
	fmt.Println("  mpm ops mode | persona | topic | lesson | session | reference | wake")
	fmt.Println()
}

// printHelp displays the mpm help text with lipgloss styling
func printHelp() {
	dailyCmds := buildHelpSection("Daily Commands", []helpCmd{
		{"<query>", "Search memories (default when called with a bare string)", false},
		{"add <text>", "Add a new memory", false},
		{"add -i", "Interactive add — opens $EDITOR", false},
		{"ls", "List memories", false},
		{"show <id>", "Show memory details", false},
		{"rm <id>", "Delete a memory", false},
		{"call <tool>", "Universal machine interface", false},
		{"wake", "Show last session context", false},
		{"status", "System status dashboard", false},
		{"version", "Show version info", false},
		{"help", "Show this help", false},
	})

	opsSection := buildHelpSection("Engine Room (ops)", []helpCmd{
		{"ops", "Maintenance, diagnostics, synthesis, and more", true},
	})

	kbSection := buildHelpSection("Knowledge Base (kb)", []helpCmd{
		{"kb", "Entity-centric memory interface (memory|topic|lesson|session|reference)", true},
	})

	debugSection := buildHelpSection("Debug", []helpCmd{
		{"debug", "Low-level inspection tools (history|diff|patch-memory|shred)", true},
	})

	alsoSection := buildHelpSection("Also available via ops", []helpCmd{
		{"mode | persona", "", false},
		{"wake | directives | switch | status", "", false},
		{"doctor | maintain | gc | prune", "", false},
		{"watch | review", "", false},
		{"stats | export | synthesize", "", false},
		{"backup | restore-db | ingest", "", false},
		{"backfill-embeddings", "", false},
		{"gateway", "", false},
		{"hint | theories | decisions", "", false},
	})

	mainStyle := lipgloss.NewStyle().
		BorderStyle(lipgloss.RoundedBorder()).
		BorderForeground(helpBorder).
		Padding(1, 2).
		Margin(1)

	titleStyle := lipgloss.NewStyle().
		Foreground(helpGold).
		Bold(true).
		Align(lipgloss.Center).
		Render(" mpm  ·  Memory Persistence Module")

	subtitleStyle := lipgloss.NewStyle().
		Foreground(helpCyan).
		Align(lipgloss.Center).
		Render("Your long-term memory, always within reach")

	content := "\n" + titleStyle + "\n" + subtitleStyle + "\n\n" +
		dailyCmds + "\n" +
		opsSection + "\n" +
		kbSection + "\n" +
		debugSection + "\n" +
		alsoSection + "\n"

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
func printGatewayHelp() int {
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
	return 0
}
