package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/flowbyte-com/mpm-core/usererror"
	"github.com/flowbyte-com/mpm/cmd/mpm/render"
	mpmupdate "github.com/flowbyte-com/mpm/internal/update"
)

// printUpdateHelp renders the update surface help.
//
// Help is inert: it prints the contract and exits 0 without touching
// the network, the database, the lock or any service.
func printUpdateHelp() int {
	render.Heading(os.Stdout, "Update")
	render.BlankLine(os.Stdout)
	fmt.Println("Keep the runtime on a newer release without touching operator state.")
	render.BlankLine(os.Stdout)
	fmt.Println("Usage:")
	fmt.Println("  mpm update --check [--to <tag>] [--json]")
	fmt.Println("      Resolve the candidate (default: latest stable release) and")
	fmt.Println("      report. Changes nothing.")
	fmt.Println("  mpm update --status [--json]")
	fmt.Println("      Report local state only: active release, previous release,")
	fmt.Println("      last outcome, service health, stale processes. No network.")
	fmt.Println("  mpm update [--to <tag> | --branch <name> | --commit <sha> | --from <dir>]")
	fmt.Println("             [--dry-run] [--allow-dirty] [--restart-services] [--json]")
	fmt.Println("      Run the safety workflow: resolve, lock, stage, backup,")
	fmt.Println("      preflight, validate, activate, smoke, record. The default")
	fmt.Println("      source is the latest stable published release.")
	fmt.Println("  mpm update --rollback [--with-db-restore --force] [--restart-services] [--json]")
	fmt.Println("      Return to the previous release. The database is untouched")
	fmt.Println("      unless --with-db-restore --force is given. No network.")
	render.BlankLine(os.Stdout)
	fmt.Println("Options:")
	fmt.Println("  --to <tag>          install a specific published release (e.g. --to v0.2.0)")
	fmt.Println("  --branch <name>     build the tip of an upstream branch (development)")
	fmt.Println("  --commit <sha>      build an exact upstream commit (development)")
	fmt.Println("  --from <dir>        build a local checkout outside the installed runtime")
	fmt.Println("  --check             resolve the candidate and report; change nothing")
	fmt.Println("  --status            report local state only; change nothing, no network")
	fmt.Println("  --dry-run           validate the candidate without changing or keeping anything (no backup, no staging in the data root, nothing recorded)")
	fmt.Println("  --rollback          return to the previous release (runtime only)")
	fmt.Println("  --with-db-restore   with --rollback: also restore the pre-update backup")
	fmt.Println("  --allow-dirty       build a dirty local checkout (development override)")
	fmt.Println("  --restart-services  authorise stopping/restarting mpm-scheduler and")
	fmt.Println("                      mpm-telemetry where the workflow requires it")
	fmt.Println("  --json, -j          machine-readable output")
	render.BlankLine(os.Stdout)
	fmt.Println("Notes:")
	fmt.Println("  --to names a release tag. (--version cannot take a value here: the")
	fmt.Println("  router reserves --version for build-identity reporting.)")
	fmt.Println("  --with-db-restore overwrites newer user data: it needs --force")
	fmt.Println("  (the global -f/--force flag), and it is never automatic.")
	fmt.Println("  The updater never terminates agent sessions; restart OpenCode")
	fmt.Println("  by hand after an update.")
	render.BlankLine(os.Stdout)
	fmt.Println("Examples:")
	fmt.Println("  mpm update --check")
	fmt.Println("  mpm update --status")
	fmt.Println("  mpm update --dry-run")
	fmt.Println("  mpm update")
	fmt.Println("  mpm update --to v0.2.0")
	fmt.Println("  mpm update --rollback")
	return 0
}

// updateFlagSet is the parsed form of one `mpm update` invocation.
type updateFlagSet struct {
	to, branch, commit, from string
	check, status, dryRun    bool
	rollback, withRestore    bool
	allowDirty, restart      bool
	jsonOut                  bool
}

// parseUpdateArgs parses `mpm update` flags. It accepts --flag value
// and --flag=value. A leading bare "version" token is the router's
// rewrite artifact for --version (parseFlags maps --version to the
// literal "version" before dispatch): it is read as --to's value, and
// a bare "version" with no value is an error that names --to.
func parseUpdateArgs(args []string) (updateFlagSet, error) {
	var f updateFlagSet
	i := 0
	takeValue := func(flag, cur string) (string, error) {
		if cur != "" {
			return cur, nil
		}
		if i+1 >= len(args) {
			return "", fmt.Errorf("%s requires a value", flag)
		}
		i++
		return args[i], nil
	}
	for i < len(args) {
		a := args[i]
		name, inline, _ := splitUpdateFlag(a)
		var err error
		switch name {
		case "--check":
			f.check = true
		case "--status":
			f.status = true
		case "--dry-run":
			f.dryRun = true
		case "--rollback":
			f.rollback = true
		case "--with-db-restore":
			f.withRestore = true
		case "--allow-dirty":
			f.allowDirty = true
		case "--restart-services":
			f.restart = true
		case "--json", "-j":
			f.jsonOut = true
		case "--to":
			if f.to, err = takeValue("--to", inline); err != nil {
				return f, err
			}
		case "--branch":
			if f.branch, err = takeValue("--branch", inline); err != nil {
				return f, err
			}
		case "--commit":
			if f.commit, err = takeValue("--commit", inline); err != nil {
				return f, err
			}
		case "--from":
			if f.from, err = takeValue("--from", inline); err != nil {
				return f, err
			}
		case "version":
			// The router's --version rewrite artifact. Read it as --to.
			if inline != "" {
				f.to = inline
			} else if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				f.to = args[i]
			} else {
				return f, fmt.Errorf("--version takes no value here; pin a release with --to <tag> (e.g. mpm update --to v0.2.0)")
			}
		default:
			return f, fmt.Errorf("update does not accept %q", a)
		}
		i++
	}
	return f, nil
}

func splitUpdateFlag(a string) (name, value string, hasValue bool) {
	if !strings.HasPrefix(a, "-") {
		return a, "", false
	}
	if eq := strings.IndexByte(a, '='); eq >= 0 {
		return a[:eq], a[eq+1:], true
	}
	return a, "", false
}

// handleUpdate implements `mpm update`.
//
// The normal path DELEGATES to the standalone mpm-update binary: the
// updater that recovers a broken installation cannot itself depend on
// the installation working. When no standalone copy exists yet (a
// developer checkout that was never installed), it runs the same
// workflow in-process so `mpm update` works before the first install.
func handleUpdate(args []string) int {
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "help" {
			return printUpdateHelp()
		}
	}
	flags, err := parseUpdateArgs(args)
	if err != nil {
		usererror.Error("%v", err)
		usererror.Usage("mpm update [--check|--status|--rollback] [--to <tag>|--branch <name>|--commit <sha>|--from <dir>] [--dry-run] [--json]")
		return 1
	}
	nSources := countSources(flags)
	if nSources > 1 {
		usererror.Error("pick one source: --to, --branch, --commit or --from")
		return 1
	}
	if flags.status && (flags.check || flags.dryRun || flags.rollback || nSources > 0) {
		usererror.Error("--status takes no other action flag")
		return 1
	}
	if flags.rollback && (flags.check || flags.dryRun || nSources > 0) {
		usererror.Error("--rollback takes no source flag")
		return 1
	}
	if flags.withRestore && !flags.rollback {
		usererror.Error("--with-db-restore needs --rollback")
		return 1
	}

	if bin := updateBinary(); bin != "" {
		return execUpdateBinary(bin, args)
	}
	return runUpdateInProcess(flags)
}

func countSources(f updateFlagSet) int {
	n := 0
	for _, s := range []string{f.to, f.branch, f.commit, f.from} {
		if s != "" {
			n++
		}
	}
	return n
}

// updateBinary locates the standalone updater: $MPM_UPDATE_BIN (tests
// and overrides), then the stable copy at the data-root top level,
// then the active bundle's copy. Empty means "not installed yet".
func updateBinary() string {
	if env := os.Getenv("MPM_UPDATE_BIN"); env != "" {
		if fi, err := os.Stat(env); err == nil && !fi.IsDir() && fi.Mode().Perm()&0111 != 0 {
			return env
		}
		return ""
	}
	return mpmupdate.LocateStableUpdater(mpmupdate.DataRoot())
}

// execUpdateBinary delegates to the standalone updater, passing stdio
// through and preserving its exit code. The argv after "update" is
// translated: the router's "version" rewrite artifact becomes --to.
func execUpdateBinary(bin string, args []string) int {
	translated := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "version" {
			translated = append(translated, "--to")
			continue
		}
		translated = append(translated, args[i])
	}
	cmd := exec.Command(bin, translated...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			return exit.ExitCode()
		}
		usererror.Error("could not run the standalone updater: %v", err)
		return 1
	}
	return 0
}

// runUpdateInProcess runs the workflow inside this process. Used only
// when no standalone updater is installed yet (a checkout that was
// never installed). The rendering matches the standalone binary's
// contract: same headings, same JSON keys.
func runUpdateInProcess(f updateFlagSet) int {
	o := mpmupdate.Options{
		DataRoot:        mpmupdate.DataRoot(),
		CheckOnly:       f.check,
		DryRun:          f.dryRun,
		Rollback:        f.rollback,
		WithDBRestore:   f.withRestore,
		Force:           forceRequested(),
		RestartServices: f.restart,
		AllowDirty:      f.allowDirty,
		WantVersion:     f.to,
		WantBranch:      f.branch,
		WantCommit:      f.commit,
		WantLocal:       f.from,
	}
	ctx := context.Background()
	// Bare `mpm update` runs the default update (same contract as the
	// standalone binary). Read-only views need their explicit flag.
	if f.status {
		return renderUpdateStatus(mpmupdate.GetStatus(o.DataRoot), f.jsonOut)
	}
	if f.rollback {
		out, err := mpmupdate.Rollback(o)
		if err != nil {
			return updateError("rollback", err, f.jsonOut)
		}
		return renderUpdateOutcome("rollback", out, f.jsonOut)
	}
	if f.check {
		out, err := mpmupdate.Check(ctx, o)
		if err != nil {
			return updateError("check", err, f.jsonOut)
		}
		return renderUpdateOutcome("check", out, f.jsonOut)
	}
	out, err := mpmupdate.Update(ctx, o)
	if err != nil {
		return updateError("update", err, f.jsonOut)
	}
	return renderUpdateOutcome("update", out, f.jsonOut)
}

func renderUpdateStatus(st mpmupdate.Status, jsonOut bool) int {
	if jsonOut {
		services := make([]map[string]interface{}, 0, len(st.Services))
		for _, s := range st.Services {
			services = append(services, map[string]interface{}{"name": s.Name, "active": s.Active})
		}
		raw, _ := json.MarshalIndent(map[string]interface{}{
			"active_rev":     st.ActiveRev,
			"active_commit":  st.ActiveCommit,
			"active_version": st.ActiveVersion,
			"previous_rev":   st.PreviousRev,
			"stable_updater": st.StableUpdater,
			"services":       services,
		}, "", "  ")
		fmt.Println(string(raw))
		return 0
	}
	render.Heading(os.Stdout, "Update · Status")
	render.BlankLine(os.Stdout)
	fmt.Printf("Active release:   %s\n", updateOrDash(st.ActiveRev))
	fmt.Printf("Previous release: %s\n", updateOrDash(st.PreviousRev))
	return 0
}

func renderUpdateOutcome(action string, out mpmupdate.Outcome, jsonOut bool) int {
	if jsonOut {
		raw, _ := json.MarshalIndent(map[string]interface{}{
			"action":          action,
			"already_current": out.AlreadyCurrent,
			"from_rev":        out.FromRev,
			"to_rev":          out.ToRev,
			"commit":          out.Commit,
			"backup_dir":      out.BackupDir,
		}, "", "  ")
		fmt.Println(string(raw))
		return 0
	}
	fmt.Print(out.Report)
	return 0
}

func updateError(action string, err error, jsonOut bool) int {
	if jsonOut {
		raw, _ := json.MarshalIndent(map[string]interface{}{
			"action":  action,
			"success": false,
			"error":   err.Error(),
		}, "", "  ")
		fmt.Println(string(raw))
		return 1
	}
	usererror.Error("%v", err)
	return 1
}

func updateOrDash(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}
