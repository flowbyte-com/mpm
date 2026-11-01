// Command mpm-update is the standalone MPM update manager.
//
// It lives OUTSIDE the versioned release bundle it manages: after every
// successful activation the updater copies itself to
// <data root>/mpm-update (a real file, not a symlink into any release),
// so the next recovery never executes the release it is recovering.
// `mpm update` delegates here; when the active runtime is broken the
// operator runs ~/.mpm/mpm-update directly.
//
// It links only internal/update and internal/buildinfo — never the
// substrate packages. Recovering MPM must never require starting the
// broken MPM runtime: no database opens, no config loads, no scheduler
// contact except the explicit, authorised service restarts.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/flowbyte-com/mpm/internal/buildinfo"
	"github.com/flowbyte-com/mpm/internal/update"
)

const usage = `MPM · Update

Usage:
  mpm-update --check [--to <tag>] [--json]
      Resolve the candidate (default: latest stable release) and report.
      Changes nothing. Needs the network unless the source is local.

  mpm-update --status [--json]
      Report local state only: active release, previous release, last
      outcome, service health, stale processes. No network.

  mpm-update [--to <tag> | --branch <name> | --commit <sha> | --from <dir>]
             [--dry-run] [--allow-dirty] [--restart-services] [--json]
      Run the full safety workflow: resolve, lock, stage, backup,
      preflight, validate, activate, smoke, record. The default source
      is the latest stable GitHub release; --branch/--commit/--from
      are explicit development modes.

  mpm-update --rollback [--with-db-restore --force] [--restart-services] [--json]
      Repoint the active release at the previous release. The database
      is untouched unless --with-db-restore --force is given (which
      overwrites newer user data and is therefore never automatic).
      Needs no network.

Options:
  --to <tag>          install a specific published release (e.g. --to v0.2.0)
  --branch <name>     build the tip of an upstream branch (development)
  --commit <sha>      build an exact upstream commit (development)
  --from <dir>        build a local checkout outside the installed runtime
  --check             resolve the candidate and report; change nothing
  --status            report local state only; change nothing, no network
  --dry-run           validate the candidate without changing or keeping anything (no backup, no staging in the data root, nothing recorded)
  --rollback          return to the previous release (runtime only)
  --with-db-restore   with --rollback: also restore the pre-update backup
  --force             confirm the destructive half of --with-db-restore
  --allow-dirty       build a dirty local checkout (development override)
  --restart-services  authorise stopping/restarting mpm-scheduler and
                      mpm-telemetry where the workflow requires quiescence
  --json              machine-readable output

Examples:
  mpm-update --check
  mpm-update --status
  mpm-update --dry-run
  mpm-update
  mpm-update --to v0.2.0
  mpm-update --rollback
`

func main() {
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) == 0 {
		fmt.Print(usage)
		return 0
	}
	for _, a := range args {
		if a == "--version" || a == "-v" {
			printBinaryVersion()
			return 0
		}
	}
	var o update.Options
	var check, status, dryRun, rollback, withRestore, force, allowDirty, restart, jsonOut bool
	var to, branch, commit, from string
	seen := map[string]bool{}
	i := 0
	for i < len(args) {
		a := args[i]
		if a == "--" {
			i++
			break
		}
		if a == "-h" || a == "--help" || a == "help" {
			fmt.Print(usage)
			return 0
		}
		name, value, hasValue := splitFlag(a)
		takeValue := func(flag string) (string, bool) {
			if hasValue {
				return value, true
			}
			if i+1 >= len(args) {
				fmt.Fprintf(os.Stderr, "mpm-update: %s requires a value\n", flag)
				return "", false
			}
			i++
			return args[i], true
		}
		switch name {
		case "--check":
			check = true
		case "--status":
			status = true
		case "--dry-run":
			dryRun = true
		case "--rollback":
			rollback = true
		case "--with-db-restore":
			withRestore = true
		case "--force", "-f":
			force = true
		case "--allow-dirty":
			allowDirty = true
		case "--restart-services":
			restart = true
		case "--json", "-j":
			jsonOut = true
		case "--to":
			v, ok := takeValue("--to")
			if !ok {
				return 1
			}
			to = v
		case "--branch":
			v, ok := takeValue("--branch")
			if !ok {
				return 1
			}
			branch = v
		case "--commit":
			v, ok := takeValue("--commit")
			if !ok {
				return 1
			}
			commit = v
		case "--from":
			v, ok := takeValue("--from")
			if !ok {
				return 1
			}
			from = v
		default:
			fmt.Fprintf(os.Stderr, "mpm-update: unknown flag %q\n\n%s", a, usage)
			return 1
		}
		if seen[name] && name != "--json" && name != "-j" && name != "--force" && name != "-f" {
			fmt.Fprintf(os.Stderr, "mpm-update: %s given twice\n", name)
			return 1
		}
		seen[name] = true
		i++
	}
	if i < len(args) {
		fmt.Fprintf(os.Stderr, "mpm-update: unexpected argument %q\n\n%s", args[i], usage)
		return 1
	}

	o = update.Options{
		DataRoot:  update.DataRoot(),
		CheckOnly: check,

		DryRun:          dryRun,
		Rollback:        rollback,
		WithDBRestore:   withRestore,
		Force:           force || os.Getenv("MPM_FORCE") == "1",
		RestartServices: restart,
		AllowDirty:      allowDirty,
		WantVersion:     to,
		WantBranch:      branch,
		WantCommit:      commit,
		WantLocal:       from,
	}
	nSources := 0
	for _, s := range []string{to, branch, commit, from} {
		if s != "" {
			nSources++
		}
	}
	if nSources > 1 {
		fmt.Fprintln(os.Stderr, "mpm-update: pick one source: --to, --branch, --commit or --from")
		return 1
	}
	if status && (check || dryRun || rollback || nSources > 0) {
		fmt.Fprintln(os.Stderr, "mpm-update: --status takes no other action flag")
		return 1
	}
	if rollback && (check || dryRun || nSources > 0) {
		fmt.Fprintln(os.Stderr, "mpm-update: --rollback takes no source flag")
		return 1
	}
	if withRestore && !rollback {
		fmt.Fprintln(os.Stderr, "mpm-update: --with-db-restore needs --rollback")
		return 1
	}

	ctx := context.Background()
	// Bare `mpm-update` (no flags at all) runs the default update: the
	// latest stable release through the full safety workflow. Every
	// read-only view has an explicit flag (--status, --check,
	// --dry-run), so a bare invocation is never ambiguous about
	// whether it changes anything: it does, and it says so in the
	// usage above.
	if status {
		return runStatus(o, jsonOut)
	}
	if rollback {
		return runRollback(o, jsonOut)
	}
	if check {
		return runCheck(ctx, o, jsonOut)
	}
	return runUpdate(ctx, o, jsonOut)
}

func splitFlag(a string) (name, value string, hasValue bool) {
	if !strings.HasPrefix(a, "-") {
		return a, "", false
	}
	if eq := strings.IndexByte(a, '='); eq >= 0 {
		return a[:eq], a[eq+1:], true
	}
	return a, "", false
}

func runStatus(o update.Options, jsonOut bool) int {
	st := update.GetStatus(o.DataRoot)
	if jsonOut {
		raw, _ := json.MarshalIndent(toStatusJSON(st), "", "  ")
		fmt.Println(string(raw))
		return 0
	}
	fmt.Println("MPM · Update · Status")
	fmt.Println()
	fmt.Printf("Active release:   %s\n", displayOr(st.ActiveRev, "(none)"))
	if st.ActiveCommit != "" {
		fmt.Printf("  commit:         %s\n", st.ActiveCommit)
	}
	if st.ActiveVersion != "" {
		fmt.Printf("  version:        %s\n", st.ActiveVersion)
	}
	fmt.Printf("Previous release: %s\n", displayOr(st.PreviousRev, "(none recorded)"))
	fmt.Printf("Stable updater:   %s\n", yesNo(st.StableUpdater))
	fmt.Println()
	fmt.Println("Services:")
	for _, s := range st.Services {
		fmt.Printf("  %s  %s\n", mark(s.Active), s.Name)
	}
	fmt.Println()
	fmt.Print(update.FormatStaleReport(st.Stale))
	if len(st.History) > 0 {
		last := st.History[len(st.History)-1]
		fmt.Printf("\nLast outcome: %s %s -> %s (%s)\n", last.Action, displayOr(last.FromRev, "(none)"), displayOr(last.ToRev, "(none)"), last.Outcome)
		if last.BackupDir != "" {
			fmt.Printf("  recovery point: %s\n", last.BackupDir)
		}
	}
	return 0
}

type statusJSON struct {
	ActiveRev     string `json:"active_rev"`
	ActiveCommit  string `json:"active_commit,omitempty"`
	ActiveVersion string `json:"active_version,omitempty"`
	PreviousRev   string `json:"previous_rev,omitempty"`
	StableUpdater bool   `json:"stable_updater"`
	Services      []svc  `json:"services"`
}

type svc struct {
	Name   string `json:"name"`
	Active bool   `json:"active"`
}

func toStatusJSON(st update.Status) statusJSON {
	out := statusJSON{ActiveRev: st.ActiveRev, ActiveCommit: st.ActiveCommit, ActiveVersion: st.ActiveVersion, PreviousRev: st.PreviousRev, StableUpdater: st.StableUpdater}
	for _, s := range st.Services {
		out.Services = append(out.Services, svc{Name: s.Name, Active: s.Active})
	}
	return out
}

func runCheck(ctx context.Context, o update.Options, jsonOut bool) int {
	out, err := update.Check(ctx, o)
	if err != nil {
		return fail("check", err, jsonOut)
	}
	if jsonOut {
		raw, _ := json.MarshalIndent(map[string]interface{}{
			"action":          "check",
			"already_current": out.AlreadyCurrent,
			"from_rev":        out.FromRev,
			"to_rev":          out.ToRev,
			"commit":          out.Commit,
		}, "", "  ")
		fmt.Println(string(raw))
		return 0
	}
	fmt.Println("MPM · Update · Check")
	fmt.Println()
	fmt.Printf("  %s\n", out.Report)
	return 0
}

func runUpdate(ctx context.Context, o update.Options, jsonOut bool) int {
	out, err := update.Update(ctx, o)
	if err != nil {
		return fail("update", err, jsonOut)
	}
	if jsonOut {
		raw, _ := json.MarshalIndent(map[string]interface{}{
			"action":          "update",
			"already_current": out.AlreadyCurrent,
			"from_rev":        out.FromRev,
			"to_rev":          out.ToRev,
			"commit":          out.Commit,
			"backup_dir":      out.BackupDir,
		}, "", "  ")
		fmt.Println(string(raw))
		return 0
	}
	fmt.Println("MPM · Update")
	fmt.Println()
	fmt.Printf("  %s", out.Report)
	return 0
}

func runRollback(o update.Options, jsonOut bool) int {
	out, err := update.Rollback(o)
	if err != nil {
		return fail("rollback", err, jsonOut)
	}
	if jsonOut {
		raw, _ := json.MarshalIndent(map[string]interface{}{
			"action":     "rollback",
			"from_rev":   out.FromRev,
			"to_rev":     out.ToRev,
			"backup_dir": out.BackupDir,
		}, "", "  ")
		fmt.Println(string(raw))
		return 0
	}
	fmt.Println("MPM · Update · Rollback")
	fmt.Println()
	fmt.Printf("  %s", out.Report)
	return 0
}

func fail(action string, err error, jsonOut bool) int {
	if jsonOut {
		raw, _ := json.MarshalIndent(map[string]interface{}{
			"action":  action,
			"success": false,
			"error":   err.Error(),
		}, "", "  ")
		fmt.Println(string(raw))
		return 1
	}
	fmt.Fprintf(os.Stderr, "mpm-update: %v\n", err)
	return 1
}

func displayOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func yesNo(b bool) string {
	if b {
		return "installed"
	}
	return "missing"
}

func mark(active bool) string {
	if active {
		return "✓"
	}
	return "○"
}

func printBinaryVersion() {
	info := buildinfo.Current("mpm-update")
	fmt.Print(buildinfo.FormatHuman(info))
}
