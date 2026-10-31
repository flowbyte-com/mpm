// namespace_cmds.go — the runtime namespaces: `mpm telemetry`,
// `mpm critic`, `mpm scheduler`.
//
// WHY THIS FILE EXISTS
// --------------------
// MPM ships three operator-facing executables beside the main CLI:
//
//	mpm-telemetry   the telemetry sidecar (ping|query|cost|observe|serve)
//	mpm-critic      one audit cycle, run by the scheduler as a payload
//	mpm-scheduler   the wake daemon, long-running, singleton-locked
//
// Their DIRECT interfaces do not change. systemd units name them by
// path, the scheduler invokes the critic as a payload handler, and the
// daemon contract is not this file's business. What this file adds is a
// discoverable FRONT door: an operator who knows `mpm` should not have
// to know three other names.
//
// THE TWO MISTAKES THIS DESIGN PREVENTS
// -------------------------------------
// The dangerous verbs in this area are the DEFAULT ones:
//
//   - `mpm-critic` with no arguments runs an audit. So `mpm critic`,
//     typed to ask what the critic does, must NOT reach it.
//   - `mpm-scheduler` is a daemon with a singleton lock. So
//     `mpm scheduler`, typed the same way, must NOT reach it — and no
//     path here may start a second, unmanaged scheduler process.
//
// Both are answered locally: a bare namespace prints help, and the
// executable is reached only through an explicit verb (`run`, `start`,
// `stop`, `restart`) or a read-only one (`version`, `status`, `logs`,
// and telemetry's own read subcommands).
//
// HELP IS NOT DELEGATED BLINDLY
// -----------------------------
// The router rewrites -h/--help into the literal token "help" before
// any handler sees argv (see parseFlags). A namespace that simply
// forwarded its arguments would send "help" to a program that never
// agreed to interpret it. Each handler therefore decides what a help
// request means for ITS executable, and `printTelemetryHelp` /
// `printCriticHelp` / `printSchedulerHelp` are registered as the
// Command's Help hook so that `mpm <ns> --help`, `mpm <ns> -h` and
// `mpm help <ns>` all render the same page.
//
// SAME-BUNDLE RESOLUTION
// ----------------------
// A delegated executable is resolved from the DIRECTORY OF THE RUNNING
// mpm, never from PATH. The installed layout is
//
//	~/.mpm/releases/<rev>/bin/{mpm,mpm-telemetry,mpm-critic,mpm-scheduler}
//	~/.mpm/current -> releases/<rev>          (the active pointer)
//	~/.mpm/bin/*                              (symlinks into current)
//
// so `filepath.Dir(os.Executable())` is the active runtime bundle
// whatever the operator typed to get there (~/.mpm/bin/mpm,
// ~/.local/bin/mpm, or the release path itself — /proc/self/exe
// resolves the symlinks). Taking the child from PATH instead would let
// an old shim, a stale dev build, or an unrelated program answer for
// the runtime the operator is actually running.
//
// systemctl and journalctl are the opposite case and DO come from PATH:
// they are the host's tools, not MPM's, and the operator's PATH is the
// correct authority for them.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"github.com/flowbyte-com/mpm-core/usererror"
	"github.com/flowbyte-com/mpm/cmd/mpm/render"
)

const (
	telemetryExecutable = "mpm-telemetry"
	criticExecutable    = "mpm-critic"
	schedulerExecutable = "mpm-scheduler"

	// schedulerUnit is the user unit both the installer and this
	// namespace address. It is the unit name install.sh writes and the
	// one install.sh restarts; there is no second alias to keep in sync.
	schedulerUnit = "mpm-scheduler.service"
)

// ============================================================================
// Same-bundle resolution
// ============================================================================

// siblingError reports that a delegated executable was not found beside
// the running mpm. It names the path that was checked, because the
// likely cause is an incomplete bundle rather than a typo, and the
// operator's next step depends on knowing which path was expected.
type siblingError struct {
	Name   string
	Path   string
	Reason string
}

func (e *siblingError) Error() string {
	return fmt.Sprintf(
		"%s is not available beside this mpm (%s)\n"+
			"     looked in: %s\n"+
			"     This namespace runs the executable from the SAME runtime bundle as the\n"+
			"     mpm you invoked; it never falls back to PATH, because an mpm from one\n"+
			"     release must not be answered by a binary from another.\n"+
			"     Reinstall the runtime (./install.sh) or run %s directly.",
		e.Name, e.Reason, e.Path, e.Name)
}

// siblingBinary resolves name from the directory holding the running
// mpm executable.
func siblingBinary(name string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("cannot determine the mpm executable path: %w", err)
	}
	return siblingBinaryFrom(exe, name)
}

// siblingBinaryFrom is the testable core of siblingBinary: it resolves
// name relative to an explicit executable path, so a test can describe
// a bundle without rearranging its own process.
func siblingBinaryFrom(exe, name string) (string, error) {
	if exe == "" {
		return "", fmt.Errorf("cannot resolve %s: the mpm executable path is empty", name)
	}
	path := filepath.Join(filepath.Dir(exe), name)
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "", &siblingError{Name: name, Path: path, Reason: "not found"}
	case err != nil:
		return "", &siblingError{Name: name, Path: path, Reason: err.Error()}
	case info.IsDir():
		return "", &siblingError{Name: name, Path: path, Reason: "is a directory"}
	case info.Mode()&0o111 == 0:
		return "", &siblingError{Name: name, Path: path, Reason: "is not executable"}
	}
	return path, nil
}

// ============================================================================
// Delegation
// ============================================================================

// runSibling runs path with args, wired straight through to this
// process's standard streams, and returns the exit status the operator
// would have seen running the child directly.
//
// INTERRUPT BEHAVIOUR. The child shares our process group, so a
// terminal Ctrl-C reaches it without our help. The forwarding below
// covers the other case — a signal aimed at THIS pid (`kill -INT <mpm
// pid>`, or a supervisor stopping the wrapper) — so the child is not
// orphaned while its parent dies.
//
// When the child dies from a signal, we re-raise that same signal on
// ourselves instead of reporting a synthetic number: a caller that
// inspects wait status (a shell reporting "Interrupt", a supervisor
// distinguishing SIGTERM from a clean failure) must see what it would
// have seen without the namespace in the way.
func runSibling(path string, args []string) int {
	cmd := exec.Command(path, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	sigCh := make(chan os.Signal, 4)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	if err := cmd.Start(); err != nil {
		usererror.Error("cannot start %s: %v", filepath.Base(path), err)
		return 1
	}

	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		// Bound to this frame by the WaitGroup: the forwarder cannot
		// outlive runSibling, and runSibling does not return until it
		// has stopped. (mpm-lint classifies an unbound goroutine as
		// go-detached and fails the gate — correctly: a signal
		// forwarder that survives its command would signal whatever
		// process took its place.)
		defer wg.Done()
		for {
			select {
			case s := <-sigCh:
				_ = cmd.Process.Signal(s)
			case <-done:
				return
			}
		}
	}()

	err := cmd.Wait()

	// Stop the handler and JOIN the forwarder BEFORE classifying the
	// status: classifying may re-raise a signal, and a still-installed
	// handler would swallow it and turn an interrupt into an ordinary
	// exit code.
	signal.Stop(sigCh)
	close(done)
	wg.Wait()
	return exitStatusOf(err)
}

// exitStatusOf converts a cmd.Wait error into the exit status the
// operator should observe.
func exitStatusOf(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		// The process was started but never produced a wait status, or
		// something else went wrong at the os/exec level.
		usererror.Error("waiting for the delegated process failed: %v", err)
		return 1
	}
	if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		sig := ws.Signal()
		signal.Reset(sig)
		// If the re-raise does not terminate us (the signal is blocked
		// or handled elsewhere), fall back to the shell's encoding.
		_ = syscall.Kill(os.Getpid(), sig)
		return 128 + int(sig)
	}
	return exitErr.ExitCode()
}

// ============================================================================
// mpm telemetry
// ============================================================================

// handleTelemetry forwards the whole argument vector to the telemetry
// sidecar. There is deliberately no per-subcommand logic here: the
// sidecar's parser already treats a literal "help" token as a help
// request (cmd/mpm-telemetry/help.go), so the router's rewrite of
// -h/--help into "help" survives the hop unchanged, and every flag the
// sidecar accepts keeps working without this file knowing it exists.
//
// A bare `mpm telemetry` therefore reaches the sidecar with no
// arguments, which prints its own top-level catalogue and exits 0 —
// without opening telemetry.db, dialling the socket or starting the
// collector.
func handleTelemetry(args []string) int {
	path, err := siblingBinary(telemetryExecutable)
	if err != nil {
		usererror.Error("%v", err)
		return 1
	}
	return runSibling(path, args)
}

// printTelemetryHelp renders the namespace page and then the sidecar's
// own catalogue, so `mpm telemetry --help` and a bare `mpm telemetry`
// do not disagree about what the surface is.
func printTelemetryHelp() int {
	// No heading of our own: the page the sidecar prints starts with
	// "MPM · Telemetry", and two of them would read like a bug.
	fmt.Println("The telemetry sidecar owns telemetry.db and its Unix-socket collector.")
	fmt.Println("This namespace runs the mpm-telemetry executable from the same runtime")
	fmt.Println("bundle as this mpm. Its own catalogue follows.")
	_ = render.BlankLine(os.Stdout)

	path, err := siblingBinary(telemetryExecutable)
	if err != nil {
		usererror.Error("%v", err)
		return 1
	}
	return runSibling(path, []string{"--help"})
}

// ============================================================================
// mpm critic
// ============================================================================

// handleCritic exposes the audit runner behind an explicit `run`.
//
// The executable's own interface is unchanged: `mpm-critic` with no
// arguments still runs one audit cycle, which is what the scheduler
// invokes. The namespace simply refuses to be that invocation by
// accident — `mpm critic` and `mpm critic --help` answer locally, and
// only `mpm critic run` reaches the audit.
func handleCritic(args []string) int {
	if len(args) == 0 {
		return printCriticHelp()
	}
	switch args[0] {
	case "help", "-h", "--help":
		// "-h"/"--help" can only arrive here if a caller reached this
		// handler without the router's rewrite; treating them as help
		// keeps the property true on every path.
		return printCriticHelp()
	case "version":
		path, err := siblingBinary(criticExecutable)
		if err != nil {
			usererror.Error("%v", err)
			return 1
		}
		// The critic answers a version request before it parses flags,
		// opens a database or runs a hunt — see cmd/mpm-critic/main.go.
		return runSibling(path, []string{"version"})
	case "run":
		path, err := siblingBinary(criticExecutable)
		if err != nil {
			usererror.Error("%v", err)
			return 1
		}
		// The subcommand token is STRIPPED, not forwarded.
		//
		// mpm-critic parses its flags with the standard flag package,
		// which stops at the first non-flag argument. Forwarding
		// `run --telemetry-hunt` would leave --telemetry-hunt in
		// flag.Args(), where the critic never reads it: the hunt would
		// be silently skipped and the operator would be told nothing.
		// Dropping `run` keeps the critic's existing interface and its
		// existing flag semantics exactly as they are.
		return runSibling(path, args[1:])
	default:
		usererror.Error("unknown critic subcommand: %q", args[0])
		_ = render.BlankLine(os.Stderr)
		printCriticHelpTo(os.Stderr)
		return 1
	}
}

func printCriticHelp() int {
	printCriticHelpTo(os.Stdout)
	return 0
}

func printCriticHelpTo(w *os.File) {
	render.Heading(w, "Critic")
	_ = render.BlankLine(w)
	fmt.Fprintln(w, "One Critic audit cycle. The audit does NOT run unless you ask for it:")
	fmt.Fprintln(w, "a bare `mpm critic` and `mpm critic --help` only print this page.")
	_ = render.BlankLine(w)
	fmt.Fprintln(w, "Usage: mpm critic run [flags]")
	fmt.Fprintln(w, "       mpm critic version")
	_ = render.BlankLine(w)
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintln(w, "  run        Run one audit cycle (the executable's default action)")
	fmt.Fprintln(w, "  version    Print the audit runner's build identity")
	_ = render.BlankLine(w)
	fmt.Fprintln(w, "Flags for `mpm critic run` (passed through to mpm-critic unchanged):")
	fmt.Fprintln(w, "      --telemetry-hunt   Also run the HighTokenNoArtifactHunt")
	fmt.Fprintln(w, "                         (read-only: it reports, it writes nothing)")
	fmt.Fprintln(w, "      --db <path>        Database to audit (default: $MPM_WORKSPACE/src/db/mpm.db)")
	fmt.Fprintln(w, "      --log-level <lvl>  debug, info, warn, error (default: info)")
	_ = render.BlankLine(w)
	fmt.Fprintln(w, "The scheduler runs the same executable as a payload handler; this")
	fmt.Fprintln(w, "namespace never starts a second audit on its own.")
}

// ============================================================================
// mpm scheduler
// ============================================================================

// handleScheduler is a service-management namespace. It addresses the
// user systemd unit and NEVER executes the daemon itself.
//
// The one exception is `version`, and it is safe by construction: the
// scheduler answers a version request before the flag parse, the perm
// gate, the singleton lock and the database open (see
// cmd/mpm-scheduler/main.go). `start`, `stop` and `restart` go through
// systemd, which enforces the singleton — so there is no path from
// this file to a second, unmanaged scheduler process.
func handleScheduler(args []string) int {
	if len(args) == 0 {
		return printSchedulerHelp()
	}
	switch args[0] {
	case "help", "-h", "--help":
		return printSchedulerHelp()
	case "status":
		if rc := rejectSchedulerArgs("status", args[1:]); rc != 0 {
			return rc
		}
		return schedulerStatus()
	case "logs":
		if rc := rejectSchedulerArgs("logs", args[1:]); rc != 0 {
			return rc
		}
		return schedulerLogs()
	case "start", "stop", "restart":
		if rc := rejectSchedulerArgs(args[0], args[1:]); rc != 0 {
			return rc
		}
		return schedulerMutate(args[0])
	case "version":
		path, err := siblingBinary(schedulerExecutable)
		if err != nil {
			usererror.Error("%v", err)
			return 1
		}
		return runSibling(path, []string{"version"})
	default:
		usererror.Error("unknown scheduler subcommand: %q", args[0])
		_ = render.BlankLine(os.Stderr)
		printSchedulerHelpTo(os.Stderr)
		return 1
	}
}

// rejectSchedulerArgs refuses extra arguments rather than forwarding
// them. `systemctl --user status <unit>` accepts options of its own, and
// a forwarded token could turn a read-only inspection into something
// else — or into a management command aimed at a different unit.
func rejectSchedulerArgs(sub string, rest []string) int {
	if len(rest) == 0 {
		return 0
	}
	usererror.Error("`mpm scheduler %s` takes no arguments (got %q)", sub, strings.Join(rest, " "))
	_ = render.BlankLine(os.Stderr)
	usererror.Usage("     Use `mpm scheduler %s` exactly, or run systemctl/journalctl directly\n"+
		"     for options this namespace does not expose.", sub)
	return 1
}

// systemTool resolves a host tool from PATH.
//
// systemctl and journalctl are the HOST's tools, so the operator's PATH
// is the correct authority for them — the opposite of the sibling
// binaries above, which must come from this runtime bundle.
func systemTool(name string) (string, error) {
	path, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("%s is not on PATH: %v\n"+
			"     This command manages a systemd --user service; without systemd it cannot run.\n"+
			"     Run `mpm scheduler` for the surface, or manage the unit directly.", name, err)
	}
	return path, nil
}

// schedulerStatus inspects the unit. It is READ-ONLY: `systemctl
// status` changes nothing, and this handler never asks systemd to do
// anything.
//
// EXIT CONTRACT. `systemctl status` reports the unit's state through its
// exit status: 0 running, 3 not running, 4 no such unit. A status
// INSPECTION that successfully reports "inactive" is a successful
// inspection, so 3 is returned as 0 here — otherwise `mpm scheduler
// status` would look like a failure on a host where the scheduler is
// simply stopped. 4 is a real error: the unit is not installed.
func schedulerStatus() int {
	path, err := systemTool("systemctl")
	if err != nil {
		usererror.Error("%v", err)
		return 1
	}
	switch rc := runSibling(path, []string{"--user", "status", schedulerUnit}); rc {
	case 0, 3:
		return 0
	case 4:
		return reportUnitNotInstalled("status")
	default:
		return rc
	}
}

// schedulerLogs reads the unit's journal. Read-only, and deliberately
// not a follow: `mpm scheduler logs` must terminate.
func schedulerLogs() int {
	path, err := systemTool("journalctl")
	if err != nil {
		usererror.Error("%v", err)
		return 1
	}
	return runSibling(path, []string{"--user", "-u", schedulerUnit})
}

// schedulerMutate starts, stops or restarts the unit through systemd.
//
// `systemctl` exits 5 when the named unit does not exist, for all three
// verbs. That is the "not installed" case and it gets a specific
// message; anything else is passed through so systemd's own diagnosis
// reaches the operator unchanged.
func schedulerMutate(verb string) int {
	path, err := systemTool("systemctl")
	if err != nil {
		usererror.Error("%v", err)
		return 1
	}
	rc := runSibling(path, []string{"--user", verb, schedulerUnit})
	switch rc {
	case 0:
		printSuccess("mpm-scheduler %s", pastTense(verb))
		return 0
	case 5:
		return reportUnitNotInstalled(verb)
	default:
		if rc != 0 {
			usererror.Usage("     If systemd is not running for this user, `systemctl --user` cannot reach it.\n" +
				"     Check XDG_RUNTIME_DIR / DBUS_SESSION_BUS_ADDRESS, or run the unit from a login session.")
		}
		return rc
	}
}

func reportUnitNotInstalled(verb string) int {
	usererror.Error("the %s user service is not installed — cannot %s it", schedulerUnit, verb)
	usererror.Usage("     Expected unit file: %s/.config/systemd/user/%s\n"+
		"     Install it with ./install.sh (or see contrib/systemd/ for the unit).",
		os.Getenv("HOME"), schedulerUnit)
	return 1
}

// pastTense renders the confirmation for a completed systemd verb.
func pastTense(verb string) string {
	switch verb {
	case "start":
		return "started"
	case "stop":
		return "stopped"
	case "restart":
		return "restarted"
	}
	return verb
}

func printSchedulerHelp() int {
	printSchedulerHelpTo(os.Stdout)
	return 0
}

func printSchedulerHelpTo(w *os.File) {
	render.Heading(w, "Scheduler")
	_ = render.BlankLine(w)
	fmt.Fprintln(w, "Service management for the MPM wake executor (a systemd --user unit).")
	fmt.Fprintln(w, "This namespace manages the installed service; it never launches the")
	fmt.Fprintln(w, "daemon itself, so typing `mpm scheduler` cannot start one.")
	_ = render.BlankLine(w)
	fmt.Fprintln(w, "Usage: mpm scheduler [command]")
	_ = render.BlankLine(w)
	fmt.Fprintln(w, "Commands:")
	fmt.Fprintln(w, "  status     Show the unit's state (read-only)")
	fmt.Fprintln(w, "  logs       Show the unit's journal (read-only)")
	fmt.Fprintln(w, "  start      Start the unit")
	fmt.Fprintln(w, "  stop       Stop the unit")
	fmt.Fprintln(w, "  restart    Restart the unit")
	fmt.Fprintln(w, "  version    Print the daemon executable's build identity")
	_ = render.BlankLine(w)
	fmt.Fprintln(w, "With no command, this help is printed. `start`, `stop` and `restart`")
	fmt.Fprintln(w, "go through systemctl, which enforces the singleton lock.")
	fmt.Fprintf(w, "Unit: %s\n", schedulerUnit)
}
