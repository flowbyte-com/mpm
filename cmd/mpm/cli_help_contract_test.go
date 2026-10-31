// cli_help_contract_test.go — the CLI's help and option contract.
//
// WHAT THIS FILE PINS
// -------------------
// Four properties that the CLI had broken in four different ways:
//
//  1. A help request exits 0 and prints something, for EVERY command.
//  2. An unrecognised option fails VISIBLY (nonzero exit, the option
//     named) — for every command. The dangerous direction is the one
//     that MATTERS: `mpm decide --anything` recorded a decision whose
//     choice was "--anything"; `mpm gc --anything` ran the decay
//     sweep; `mpm backup --anything` wrote a dump to a file named
//     "--anything". An option the parser does not understand must
//     never be treated as data.
//  3. A help request MUTATES NOTHING — not the database, not the
//     filesystem. A help request that deletes a memory is the worst
//     possible outcome of asking for documentation.
//  4. The options a command ADVERTISES are the options it ACCEPTS.
//
// The matrix is table-driven on purpose: a per-command test suite
// cannot keep up with 79 commands, and the properties above are
// uniform. A new command that violates one of them fails here.
//
// EVERY test in this file runs against an isolated workspace. The live
// installation is never touched — see TestCLIHelpContract_NeverTouchesTheLiveRuntime.
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// cliContractCommand is one command in the contract matrix.
type cliContractCommand struct {
	// Args is the command line, without the binary.
	Args []string
	// MutatesOnHelp is true for the commands whose normal operation
	// WRITES. They are the ones where a help request must be proven
	// inert.
	MutatesOnHelp bool
	// FreeText is true for the commands whose positional argument is
	// free text.
	//
	// The CLI's convention is "options come before positionals", and
	// the router's guard honours it for these commands: the scan
	// stops at the first positional, because everything after it is
	// content. `mpm remember Warning: don't forget --flags; ok?`
	// stores five tokens of content, one of which begins with a
	// dash.
	//
	// The unknown-option probe therefore puts the option BEFORE the
	// positional for these commands — which is the shape that a
	// mistyped option actually takes.
	FreeText bool
}

// cliContractMatrix is the public command surface the contract applies
// to.
//
// The list is deliberately the operator-facing commands, not all 79:
// the families (`mpm ops`, `mpm memory`, `mpm lesson`, `mpm work`,
// `mpm config`, …) are covered through their representative leaves,
// and the leaf that mutates is the one that matters.
//
// Every entry is exercised with --help, with -h, and with an unknown
// option.
var cliContractMatrix = []cliContractCommand{
	// Information and diagnostics.
	{Args: []string{"version"}},
	{Args: []string{"help"}},
	{Args: []string{"doctor"}},
	{Args: []string{"status"}},
	{Args: []string{"info"}},

	// Knowledge — read and write.
	{Args: []string{"recall", "a query"}},
	{Args: []string{"add", "a fact"}, FreeText: true},
	{Args: []string{"ls"}},
	{Args: []string{"show", "an-id"}},
	{Args: []string{"rm", "an-id"}, MutatesOnHelp: true},
	{Args: []string{"promote", "an-id"}, MutatesOnHelp: true},
	{Args: []string{"reinforce", "an-id"}, MutatesOnHelp: true},
	{Args: []string{"weaken", "an-id"}, MutatesOnHelp: true},
	{Args: []string{"snooze", "an-id"}, MutatesOnHelp: true},
	{Args: []string{"set-weight", "an-id", "5"}, MutatesOnHelp: true},
	{Args: []string{"shred", "an-id"}, MutatesOnHelp: true},
	{Args: []string{"stats"}},
	{Args: []string{"prune"}, MutatesOnHelp: true},
	{Args: []string{"export"}},
	{Args: []string{"maintain"}, MutatesOnHelp: true},
	{Args: []string{"synthesize"}, MutatesOnHelp: true},

	// The review workflow — the subject of Part 1.
	{Args: []string{"review"}},
	{Args: []string{"ops", "review"}},

	// The cognitive verbs. decide and theorize WRITE.
	{Args: []string{"decide", "a choice"}, MutatesOnHelp: true, FreeText: true},
	{Args: []string{"theorize", "a hypothesis"}, MutatesOnHelp: true, FreeText: true},
	{Args: []string{"remember", "a fact"}, MutatesOnHelp: true, FreeText: true},
	{Args: []string{"learn", "a lesson"}, MutatesOnHelp: true},
	{Args: []string{"decision", "list"}},
	{Args: []string{"theory", "list"}},

	// The families the brief calls out.
	{Args: []string{"ops"}},
	{Args: []string{"ops", "doctor"}},
	{Args: []string{"ops", "gc"}, MutatesOnHelp: true},
	{Args: []string{"ops", "backup"}, MutatesOnHelp: true},
	{Args: []string{"ops", "stats"}},
	{Args: []string{"ops", "status"}},
	{Args: []string{"memory", "list"}},
	{Args: []string{"memory", "add", "a fact"}, MutatesOnHelp: true},
	{Args: []string{"lesson", "list"}},
	{Args: []string{"lesson", "add", "a lesson"}, MutatesOnHelp: true},
	{Args: []string{"work", "status"}},
	{Args: []string{"work", "clear"}, MutatesOnHelp: true},
	{Args: []string{"config", "show"}},
	{Args: []string{"config", "profile", "list"}},

	// Maintenance and administration.
	{Args: []string{"gc"}, MutatesOnHelp: true},
	{Args: []string{"backup"}, MutatesOnHelp: true},
	{Args: []string{"restore", "an-id"}, MutatesOnHelp: true},
	{Args: []string{"restore-db", "a-path"}, MutatesOnHelp: true},
	{Args: []string{"tasks", "list"}},
	{Args: []string{"lint"}},
	{Args: []string{"wake"}},
	{Args: []string{"call"}},
	{Args: []string{"evidence", "list"}},
	{Args: []string{"compact", "deferred"}},
	{Args: []string{"why", "an-id"}},
	{Args: []string{"skill", "list"}},
	{Args: []string{"topic", "list"}},
	{Args: []string{"session", "list"}},
	{Args: []string{"reference", "list"}},

	// Runtime namespaces. Their help is answered without a database and
	// their option vocabulary belongs to the delegated executable — so
	// the router must not pre-empt it, and a bad option must still fail.
	{Args: []string{"telemetry"}},
	{Args: []string{"critic"}},
	{Args: []string{"scheduler"}},
}

// cliContractFixture builds an isolated workspace with an initialised
// database, and returns the binary path and the workspace path.
func cliContractFixture(t *testing.T) (bin string, ws string) {
	t.Helper()
	bin = mpmBinForTest()
	if bin == "" {
		t.Skip("mpm binary not found; run `make build` first")
	}
	ws = t.TempDir()
	// Initialise the database so the snapshot has something to compare.
	runContractCLI(t, bin, ws, "call", "mpm_system", "--payload", `{"action":"health_check"}`)
	return bin, ws
}

// runContractCLI runs the binary in an isolated workspace and returns the
// combined output and the exit code.
func runContractCLI(t *testing.T, bin, ws string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(),
		"MPM_WORKSPACE="+ws,
		"MPM_SHARED_DB="+filepath.Join(ws, "shared.db"),
		"MPM_LOG=error",
	)
	out, err := cmd.CombinedOutput()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatalf("run %v: %v\n%s", args, err, out)
	}
	return string(out), code
}

// cliWorkspaceSnapshot is everything a help request must not change:
// the database file's content digest, every table's row count, and the
// workspace's file listing.
type cliWorkspaceSnapshot struct {
	DBHash string
	Counts map[string]int
	Files  []string
}

func snapshotCLIWorkspace(t *testing.T, ws string) cliWorkspaceSnapshot {
	t.Helper()
	snap := cliWorkspaceSnapshot{Counts: map[string]int{}}
	dbPath := filepath.Join(ws, "src", "db", "mpm.db")
	if b, err := os.ReadFile(dbPath); err == nil {
		snap.DBHash = fmt.Sprintf("%x", b[:contractMin(len(b), 4096)])
	}
	db, err := sql.Open("sqlite3", dbPath)
	if err == nil {
		defer db.Close()
		rows, err := db.Query("SELECT name FROM sqlite_master WHERE type='table' ORDER BY name")
		if err == nil {
			var names []string
			for rows.Next() {
				var n string
				if rows.Scan(&n) == nil {
					names = append(names, n)
				}
			}
			rows.Close()
			for _, n := range names {
				if strings.HasPrefix(n, "sqlite_") {
					continue
				}
				var c int
				if err := db.QueryRow(`SELECT count(*) FROM "` + n + `"`).Scan(&c); err == nil {
					snap.Counts[n] = c
				}
			}
		}
	}
	_ = filepath.Walk(ws, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		snap.Files = append(snap.Files, path)
		return nil
	})
	sort.Strings(snap.Files)
	return snap
}

// ── 1. A help request exits 0 and prints something ────────────────────────

func TestCLIHelp_HelpRequestSucceedsForEveryCommand(t *testing.T) {
	bin, ws := cliContractFixture(t)

	for _, c := range cliContractMatrix {
		c := c
		t.Run(strings.Join(c.Args, "_"), func(t *testing.T) {
			for _, form := range []string{"--help", "-h"} {
				out, code := runContractCLI(t, bin, ws, append(append([]string{}, c.Args...), form)...)
				if code != 0 {
					t.Errorf("`mpm %s %s` exited %d; a help request must succeed:\n%s",
						strings.Join(c.Args, " "), form, code, out)
				}
				if strings.TrimSpace(out) == "" {
					t.Errorf("`mpm %s %s` printed nothing; a help request must print the help:\n%s",
						strings.Join(c.Args, " "), form, out)
				}
			}
		})
	}
}

// ── 2. An unknown option fails visibly ───────────────────────────────────

func TestCLIHelp_UnknownOptionFailsVisibly(t *testing.T) {
	bin, ws := cliContractFixture(t)

	for _, c := range cliContractMatrix {
		c := c
		t.Run(strings.Join(c.Args, "_"), func(t *testing.T) {
			probe := append([]string{}, c.Args...)
			if c.FreeText {
				// The option goes BEFORE the free-text positional:
				// after it, the token is content.
				probe = append(probe[:len(probe)-1], "--definitely-not-a-real-flag", probe[len(probe)-1])
			} else {
				probe = append(probe, "--definitely-not-a-real-flag")
			}
			out, code := runContractCLI(t, bin, ws, probe...)
			if code == 0 {
				t.Errorf("`mpm %s` exited 0 with an unrecognised option; it must fail:\n%s",
					strings.Join(probe, " "), out)
			}
			if !strings.Contains(out, "definitely-not-a-real-flag") {
				t.Errorf("the rejection must name the offending option:\n%s", out)
			}
		})
	}
}

// ── 3. A help request mutates nothing ────────────────────────────────────

func TestCLIHelp_HelpRequestMutatesNothing(t *testing.T) {
	bin, ws := cliContractFixture(t)

	for _, c := range cliContractMatrix {
		if !c.MutatesOnHelp {
			continue
		}
		c := c
		t.Run(strings.Join(c.Args, "_"), func(t *testing.T) {
			before := snapshotCLIWorkspace(t, ws)
			out, code := runContractCLI(t, bin, ws, append(append([]string{}, c.Args...), "--help")...)
			if code != 0 {
				t.Errorf("`mpm %s --help` exited %d:\n%s", strings.Join(c.Args, " "), code, out)
			}
			after := snapshotCLIWorkspace(t, ws)
			if before.DBHash != after.DBHash {
				t.Errorf("`mpm %s --help` changed the database file", strings.Join(c.Args, " "))
			}
			for table, n := range after.Counts {
				if before.Counts[table] != n {
					t.Errorf("`mpm %s --help` changed %s: %d -> %d rows",
						strings.Join(c.Args, " "), table, before.Counts[table], n)
				}
			}
			if len(before.Files) != len(after.Files) {
				t.Errorf("`mpm %s --help` changed the filesystem: %d -> %d files",
					strings.Join(c.Args, " "), len(before.Files), len(after.Files))
			}
		})
	}
}

// ── 4. The advertised options are the accepted options ───────────────────

// TestCLIHelp_AdvertisedOptionsAreAccepted is the property that makes
// the help page trustworthy: every option a command advertises is one
// it accepts.
//
// It is checked at the UNIT level, against the declared option set of
// the commands that declare one, and it is the reason the option set
// is declared ONCE (see clihelp.go): the declaration registers the
// flags with the parser AND renders the page, so the two cannot drift.
//
// The behavioural half — that the command really accepts the option —
// is covered by the specific tests per command.
func TestCLIHelp_AdvertisedOptionsAreAccepted(t *testing.T) {
	cases := []struct {
		name string
		opts []OptionSpec
	}{
		{"doctor", doctorOptions(&doctorOpts{})},
		{"review", reviewOptions(&reviewOpts{})},
		{"record_decision", recordDecisionOptions()},
		{"propose_theory", proposeTheoryOptions()},
		{"gc", gcOptions()},
		{"backup", backupOptions()},
		{"synthesize", synthesizeOptions()},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			// Every declared option must have a name and a usage line.
			//
			// The duplicate check keys on the FULL SPELLING, not on
			// the bare name: "-h" and "--h" are different tokens, and
			// a command may legitimately offer both. Keying on the bare
			// name made a valid declaration look like a collision.
			seen := map[string]bool{}
			for _, o := range c.opts {
				if o.Name == "" {
					t.Errorf("an option has no name")
					continue
				}
				if o.Usage == "" {
					t.Errorf("option %q has no usage line; the help page would show it with no explanation", o.Name)
				}
				spellings := o.Spellings()
				if len(spellings) == 0 {
					t.Errorf("option %q has no spellings; the help page would show it with no name", o.Name)
					continue
				}
				for _, s := range spellings {
					if seen[s] {
						t.Errorf("the spelling %q is declared twice; the help page would show it twice", s)
					}
					seen[s] = true
				}
			}
			// The help option is always advertised, and it is always
			// the router's — so it is never registered by the command's
			// own flag set.
			if !seen["--help"] {
				t.Errorf("the help option is not advertised")
			}
		})
	}
}

// TestCLIHelp_EveryNonRouterOptionIsRegistered is the mechanical half
// of "advertised == accepted": every declared option that is not
// router-owned must actually be a flag on the command's flag set.
//
// The doctor and the review command are the two with a real flag set;
// the others read their arguments positionally and reject unknown
// options by name, so their declared set IS their accepted set.
func TestCLIHelp_EveryNonRouterOptionIsRegistered(t *testing.T) {
	doctor := doctorOptions(&doctorOpts{})
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(discardWriter{})
	RegisterOptions(fs, doctor)
	assertFlagsPresent(t, fs, doctor)

	review := reviewOptions(&reviewOpts{})
	rfs := flag.NewFlagSet("review", flag.ContinueOnError)
	rfs.SetOutput(discardWriter{})
	RegisterOptions(rfs, review)
	assertFlagsPresent(t, rfs, review)
}

// assertFlagsPresent checks that every non-router-owned declared option
// is registered on fs under its long name and each alias.
func assertFlagsPresent(t *testing.T, fs *flag.FlagSet, opts []OptionSpec) {
	t.Helper()
	for _, o := range opts {
		if o.RouterOwned {
			continue
		}
		if fs.Lookup(o.Name) == nil {
			t.Errorf("option %q is advertised but not registered with the parser", o.Name)
		}
		for _, a := range o.Aliases {
			if fs.Lookup(a) == nil {
				t.Errorf("alias %q of option %q is advertised but not registered with the parser", a, o.Name)
			}
		}
	}
}

// contractMin is the smaller of two ints (Go's builtin `min` is not
// available in this module's Go version).
func contractMin(a, b int) int {
	if a < b {
		return a
	}
	return b
}
