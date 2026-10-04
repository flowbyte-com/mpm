// mpm ops logs — manual log rotation for watchdog.jsonl + mirror.jsonl.
//
// Auto-rotation (5 MiB threshold, configurable via MPM_LOG_ROTATE_BYTES)
// handles slow-motion growth. This command exists for operators who need
// an explicit, immediate rotation regardless of threshold — typically
// before running a hostile test suite or creating a clean boundary for
// post-mortem analysis.
//
// Subcommand shape mirrors mpm ops shared / mpm ops gc:
//
//   mpm ops logs rotate             Force-rotate both logs immediately
//   mpm ops logs rotate <name>      Rotate only the named log
//                                    (watchdog | mirror)
//   mpm ops logs status             Show sizes, ages + retention policy
//   mpm ops logs purge              Remove HITL log history for one scope
//   mpm ops logs help               Print this help

package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/flowbyte-com/mpm-core/config"
	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/usererror"
)

// logFile describes one of the rotatable JSONL logs in the MPM data dir.
type logFile struct {
	Name string // "watchdog" | "mirror"
	Path string // absolute path
}

// resolveLogPaths returns the absolute paths for the two JSONL logs
// managed by the auto-rotation policy. Centralised so the CLI command
// and the production write sites agree on which files are in scope.
func resolveLogPaths() ([]logFile, error) {
	mpmdir := config.GetMPMDir()
	if mpmdir == "" {
		return nil, fmt.Errorf("MPM data dir is empty (set MPM_WORKSPACE or HOME)")
	}
	dbDir := filepath.Join(mpmdir, "src", "db")
	return []logFile{
		{Name: "watchdog", Path: filepath.Join(dbDir, "watchdog.jsonl")},
		{Name: "mirror", Path: filepath.Join(dbDir, "mirror.jsonl")},
	}, nil
}

// handleOpsLogs dispatches `mpm ops logs <subcommand>`.
func handleOpsLogs(args []string) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "-h" || args[0] == "--help" {
		fmt.Println("Usage: mpm ops logs rotate [name] | status | purge --scope <name> --force")
		fmt.Println()
		fmt.Println("Subcommands:")
		fmt.Println("  rotate              Force-rotate watchdog.jsonl + mirror.jsonl")
		fmt.Println("                      (regardless of MPM_LOG_ROTATE_BYTES threshold)")
		fmt.Println("  rotate <name>       Rotate only the named log (watchdog | mirror)")
		fmt.Println("  status              Show sizes, ages and the retention policy")
		fmt.Println("  purge --scope NAME  Empty one log and delete its rotations")
		fmt.Println("                      (watchdog | mirror). Requires --force.")
		fmt.Println("  help                Print this help")
		fmt.Println()
		fmt.Println("Retention is automatic: each log rotates when it passes its size")
		fmt.Println("threshold OR its 30-day active age, and old rotations expire by age")
		fmt.Println("with a count cap as a burst bound. Rotate and purge are for")
		fmt.Println("explicit, operator-driven boundaries.")
		fmt.Println()
		fmt.Println("purge is LOG-scope only. It does not touch mpm.db, the audit log,")
		fmt.Println("backups, or any memory. To remove a memory, use:")
		fmt.Println("  mpm memory shred <id>")
		return 0
	}

	switch args[0] {
	case "rotate":
		return handleOpsLogsRotate(args[1:])
	case "status":
		return handleOpsLogsStatus(args[1:])
	case "purge":
		return handleOpsLogsPurge(args[1:])
	default:
		usererror.Error("unknown subcommand: %q (want rotate | status)", args[0])
		return 1
	}
}

// handleOpsLogsRotate force-rotates the named log (or both). Force-rotate
// means: ignore the MPM_LOG_ROTATE_BYTES threshold and rotate now. The
// intent is operator-driven cleanup, not threshold-driven.
//
// `--threshold N` is an optional override that rotates only if the file
// is at least N bytes. Use this to avoid creating a tiny gz archive of
// a fresh log (the auto-rotate path already handles threshold checks).
func handleOpsLogsRotate(args []string) int {
	fs := flag.NewFlagSet("ops-logs-rotate", flag.ContinueOnError)
	threshold := fs.Int64("threshold", 0, "rotate only if file is at least this many bytes (0 = always rotate)")
	if err := fs.Parse(args); err != nil {
		return 1
	}

	logs, err := resolveLogPaths()
	if err != nil {
		usererror.Error("%v", err)
		return 1
	}

	// Filter to a single log if requested.
	target := ""
	if positional := fs.Args(); len(positional) > 0 {
		target = positional[0]
		if target != "watchdog" && target != "mirror" {
			usererror.Error("unknown log name: %q (want watchdog | mirror)", target)
			return 1
		}
	}

	rotated := 0
	skipped := 0
	failed := 0
	for _, lf := range logs {
		if target != "" && lf.Name != target {
			continue
		}
		// Verify the file exists before attempting rotation. An empty
		// or missing log is not an error — auto-rotation just hasn't
		// fired yet.
		info, statErr := os.Stat(lf.Path)
		if os.IsNotExist(statErr) {
			fmt.Printf("  %-7s  (no file at %s) — skipped\n", lf.Name, lf.Path)
			skipped++
			continue
		}
		if statErr != nil {
			usererror.Error("stat %s: %v", lf.Path, statErr)
			failed++
			continue
		}
		// Honour --threshold when set.
		if *threshold > 0 && info.Size() < *threshold {
			fmt.Printf("  %-7s  %d bytes < threshold %d — skipped\n",
				lf.Name, info.Size(), *threshold)
			skipped++
			continue
		}

		// Use a threshold of 1 byte to force the rotation, then let
		// the existing rotateLogIfNeeded do the work. This keeps the
		// rotation path single-source-of-truth between CLI and runtime.
		if err := mpminternal.RotateLogIfNeededForCLI(lf.Path, 1); err != nil {
			usererror.Error("rotate %s: %v", lf.Name, err)
			failed++
			continue
		}
		fmt.Printf("  %-7s  rotated %d bytes\n", lf.Name, info.Size())
		rotated++
	}

	fmt.Println()
	fmt.Printf("Rotated: %d  Skipped: %d  Failed: %d\n", rotated, skipped, failed)
	if failed > 0 {
		return 1
	}
	return 0
}

// handleOpsLogsPurge removes HITL log history for one scope: the active
// file is emptied and every gzip rotation belonging to it is deleted.
//
// LOG SCOPE ONLY. This does not touch mpm.db, the audit log, backups, or
// any memory. It exists so an operator who wants the *log copies* gone can
// say so explicitly, without that request growing into — or being confused
// with — a content-erasure path. `mpm memory shred <id>` is the hard
// delete; this is not it, and the wording never implies it is.
//
// --force is required, matching `mpm memory wipe --force`: the command is
// irreversible and there is no interactive prompt to interrupt.
func handleOpsLogsPurge(args []string) int {
	fs := flag.NewFlagSet("ops-logs-purge", flag.ContinueOnError)
	scope := fs.String("scope", "", "log to purge: watchdog | mirror")
	force := fs.Bool("force", false, "required; confirmation that log history may be deleted")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	if *scope == "" {
		usererror.Error("purge requires --scope (watchdog | mirror)")
		return 1
	}
	if _, ok := mpminternal.RetentionForLogName(*scope); !ok {
		usererror.Error("unknown scope %q (want watchdog | mirror)", *scope)
		return 1
	}
	if !*force {
		usererror.Error("purge requires --force; it deletes the log and all of its rotations")
		return 1
	}

	res, err := mpminternal.PurgeHITLLogForCLI(*scope)
	if err != nil {
		usererror.Error("purge %s: %v", *scope, err)
		return 1
	}
	if !res.Truncated && res.Rotations == 0 {
		fmt.Printf("  %-7s  nothing to purge (no file, no rotations)\n", *scope)
		return 0
	}
	fmt.Printf("  %-7s  %d rotation(s) deleted, active file %s\n",
		*scope, res.Rotations, map[bool]string{true: "emptied", false: "left absent"}[res.Truncated])
	return 0
}

// handleOpsLogsStatus reports current sizes + rotation thresholds for
// the two managed logs. Used by operators to check whether auto-rotation
// will fire on the next write, and by humans reviewing system health.
func handleOpsLogsStatus(args []string) int {
	logs, err := resolveLogPaths()
	if err != nil {
		usererror.Error("%v", err)
		return 1
	}
	override, hasOverride := mpminternal.LogRotateThresholdOverrideForCLI()
	if hasOverride {
		fmt.Printf("  Rotation threshold override: %d bytes (%s) via MPM_LOG_ROTATE_BYTES\n",
			override, humanBytes(override))
	}

	fmt.Println("MPM Logs — status")
	fmt.Println("──────────────────")
	fmt.Println()
	for _, lf := range logs {
		pol, ok := mpminternal.RetentionForLogName(lf.Name)
		if !ok {
			continue
		}
		threshold := pol.SizeThreshold
		if hasOverride {
			threshold = override
		}
		// The retention contract per stream, stated so an operator can
		// predict what a file will do without reading the source.
		fmt.Printf("  %s — rotate at %s or %d days; keep %d days, newest %d\n",
			lf.Name, humanBytes(threshold), pol.MaxActiveAgeDays,
			pol.MaxAgeDays, pol.CountCap)

		info, statErr := os.Stat(lf.Path)
		if os.IsNotExist(statErr) {
			fmt.Printf("      (file does not exist yet — %s)\n", lf.Path)
			continue
		}
		if statErr != nil {
			fmt.Printf("      stat error: %v\n", statErr)
			continue
		}
		age := time.Since(info.ModTime())
		rot, err := mpminternal.ListRotationsForCLI(lf.Path)
		if err != nil {
			fmt.Printf("      %d bytes (%s), age %s — rotation list failed: %v\n",
				info.Size(), humanBytes(info.Size()), age.Round(time.Minute), err)
			continue
		}
		fmt.Printf("      active: %d bytes (%s), age %s, %d rotation(s) retained\n",
			info.Size(), humanBytes(info.Size()), age.Round(time.Minute), len(rot))
	}
	return 0
}

// humanBytes returns a short human-readable size ("11 MB", "650 KB").
func humanBytes(n int64) string {
	const (
		kb = 1024
		mb = 1024 * kb
		gb = 1024 * mb
	)
	switch {
	case n >= gb:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(gb))
	case n >= mb:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(mb))
	case n >= kb:
		return fmt.Sprintf("%.1f KB", float64(n)/float64(kb))
	default:
		return fmt.Sprintf("%d B", n)
	}
}