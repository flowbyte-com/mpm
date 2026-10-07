// doctor_flag_truthfulness_test.go — §19 TEST MATRIX.
//
// Pins the post-fix contract for every audited flag. The pre-fix
// evidence lives in doctor_flag_truthfulness_repro_test.go.
//
// Every test is hermetic: HOME and MPM_WORKSPACE point into t.TempDir(),
// so nothing here can read or mutate the real ~/.mpm.
package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// B — baseline doctor still runs the standard report.
func TestDoctorFlag_BaselineRunsStandardReport(t *testing.T) {
	bin, ws := doctorFixture(t)
	out, exit := runDoctorFixture(t, bin, ws, "doctor")

	if strings.Contains(out, "unknown flag") {
		t.Fatalf("baseline doctor rejected its own arguments:\n%s", out)
	}
	// The standard report is not the deep-scan report and not the
	// explain report — those are distinct modes.
	if strings.Contains(out, "Deep-Scan Integrity Audit") {
		t.Errorf("baseline doctor ran the deep-scan report:\n%s", out)
	}
	if strings.Contains(out, "FTS5 Query Plan") {
		t.Errorf("baseline doctor ran the explain report:\n%s", out)
	}
	// Exit is 0/1/2 by severity, never a usage error.
	if exit > 2 {
		t.Errorf("baseline doctor exited %d; expected severity code 0/1/2", exit)
	}
}

// C — --deep-scan must materially change behaviour: it runs the
// FTS/integrity audit, which the standard report does not.
func TestDoctorFlag_DeepScanHasBehaviouralDelta(t *testing.T) {
	bin, ws := doctorFixture(t)
	warmDoctorFixture(t, bin, ws)

	base, _ := runDoctorFixture(t, bin, ws, "doctor")
	out, exit := runDoctorFixture(t, bin, ws, "doctor", "--deep-scan")

	if strings.Contains(out, "unknown flag") {
		t.Fatalf("--deep-scan was rejected:\n%s", out)
	}
	// Pinned by check identity, not by "output got longer".
	for _, want := range []string{
		"Deep-Scan Integrity Audit",
		"FTS Orphan Scan",
		"Soft-Delete Ghost Scan",
		"Dangling Topic Membership Scan",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("--deep-scan did not run the %q check:\n%s", want, out)
		}
	}
	if out == base {
		t.Error("--deep-scan output is byte-identical to baseline; the flag is inert")
	}
	// Deep-scan owns its exit contract: 0 clean/warnings, 1 hard failure.
	if exit != 0 && exit != 1 {
		t.Errorf("--deep-scan exited %d; expected 0 or 1", exit)
	}
}

// D — --explain must produce the EXPLAIN QUERY PLAN tree, which is a
// real, deterministic difference from the standard report.
func TestDoctorFlag_ExplainHasBehaviouralDelta(t *testing.T) {
	bin, ws := doctorFixture(t)
	warmDoctorFixture(t, bin, ws)

	base, _ := runDoctorFixture(t, bin, ws, "doctor")
	out, exit := runDoctorFixture(t, bin, ws, "doctor", "--explain")

	if strings.Contains(out, "unknown flag") {
		t.Fatalf("--explain was rejected:\n%s", out)
	}
	if !strings.Contains(out, "FTS5 Query Plan") {
		t.Errorf("--explain did not print the query plan:\n%s", out)
	}
	if out == base {
		t.Error("--explain output is byte-identical to baseline; the flag is inert")
	}
	if exit != 0 {
		t.Errorf("--explain exited %d; expected 0", exit)
	}
}

// B' — --all is rejected truthfully, with a message that names the
// alternative. Rejection is the correct outcome here: there is no
// extended check set to widen into.
func TestDoctorFlag_AllIsExplicitlyRejected(t *testing.T) {
	bin, ws := doctorFixture(t)
	warmDoctorFixture(t, bin, ws)

	out, exit := runDoctorFixture(t, bin, ws, "doctor", "--all")

	if exit == 0 {
		t.Errorf("--all exited 0; an unsupported flag must fail:\n%s", out)
	}
	if !strings.Contains(out, "--all is not supported") {
		t.Errorf("--all rejection must say it is unsupported; got:\n%s", out)
	}
	// The message must point at the truthful alternative rather than
	// leaving the user with nothing to do.
	if !strings.Contains(out, "--deep-scan") {
		t.Errorf("--all rejection should name the supported alternative; got:\n%s", out)
	}
}

// E' — a bare --fix is rejected, because the standard report has no
// remediation path. Only --deep-scan --fix is supported.
func TestDoctorFlag_BareFixIsExplicitlyRejected(t *testing.T) {
	bin, ws := doctorFixture(t)
	warmDoctorFixture(t, bin, ws)

	out, exit := runDoctorFixture(t, bin, ws, "doctor", "--fix")

	if exit == 0 {
		t.Errorf("bare --fix exited 0; it must not imply remediation occurred:\n%s", out)
	}
	if !strings.Contains(out, "--fix requires --deep-scan") {
		t.Errorf("--fix rejection must explain the pairing requirement; got:\n%s", out)
	}
}

// E — --deep-scan --fix is the supported remediation path. On a clean
// fixture it reports the audit, and a second run must be identical:
// idempotent by construction, with no state to corrupt.
func TestDoctorFlag_DeepScanFixIsBoundedAndIdempotent(t *testing.T) {
	bin, ws := doctorFixture(t)
	warmDoctorFixture(t, bin, ws)

	first, firstExit := runDoctorFixture(t, bin, ws, "doctor", "--deep-scan", "--fix")
	if strings.Contains(first, "unknown flag") || strings.Contains(first, "requires --deep-scan") {
		t.Fatalf("--deep-scan --fix was rejected:\n%s", first)
	}
	// --fix must announce itself when it is actually engaged, so a user
	// can tell remediation was attempted rather than silently skipped.
	if !strings.Contains(first, "--fix enabled") {
		t.Errorf("--deep-scan --fix did not announce the fix path:\n%s", first)
	}
	if firstExit != 0 && firstExit != 1 {
		t.Errorf("--deep-scan --fix exited %d; expected 0 or 1", firstExit)
	}

	second, _ := runDoctorFixture(t, bin, ws, "doctor", "--deep-scan", "--fix")
	if first != second {
		t.Errorf("a second --deep-scan --fix was not idempotent:\n--- first ---\n%s\n--- second ---\n%s",
			first, second)
	}
}

// F — §13 read-only default contract. A representative non-fix run
// must not change the fixture's stored data or its schema object set.
//
// Note on what is asserted, and why it is not a byte comparison.
//
// Every mpm invocation runs migrateLessonsToView, which drops and
// recreates the three lessons_instead_of_* triggers. The recreated
// triggers are IDENTICAL — verified by diffing a full schema+data dump
// across two consecutive baseline runs with the trigger block excluded,
// which is byte-for-byte equal — but SQLite emits them in a different
// order, and the rewrite churns the -wal file. Both effects appear on
// a plain `mpm doctor` with no flag at all, so they are boot-time
// migration behaviour, not doctor mutating state.
//
// Asserting on file bytes would therefore report a false positive on
// every run. "Read-only" means the stored data and the set of schema
// objects are unchanged, and that is what this asserts.
func TestDoctorFlag_NonFixRunsAreReadOnly(t *testing.T) {
	// Each mode gets its OWN workspace, warmed beforehand. Sharing one
	// workspace across modes means a schema change made during mode 1 is
	// already present in mode 2's "before" snapshot, so the second mode
	// cannot detect it. A fresh workspace per mode makes every mode's
	// baseline the product of the warm-up alone, which is exactly the
	// question being asked: does the flagged run itself mutate anything?
	for _, args := range [][]string{
		{"doctor"},
		{"doctor", "--deep-scan"},
		{"doctor", "--explain"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			bin, ws := doctorFixture(t)
			warmDoctorFixture(t, bin, ws)

			dbPath := filepath.Join(ws, "src", "db", "mpm.db")
			q := func(s string) string {
				out, err := exec.Command("sqlite3", dbPath, s).Output()
				if err != nil {
					t.Fatalf("sqlite3 %q: %v", s, err)
				}
				return strings.TrimSpace(string(out))
			}

			// Stored data, order-stable across every user table.
			dataOf := func() string {
				tables := q("SELECT name FROM sqlite_master WHERE type='table' " +
					"AND name NOT LIKE 'sqlite_%' AND name NOT LIKE '%_fts%' " +
					"ORDER BY name;")
				h := sha256.New()
				for _, tbl := range strings.Split(tables, "\n") {
					if tbl == "" {
						continue
					}
					h.Write([]byte(tbl))
					h.Write([]byte(q(`SELECT * FROM "` + tbl + `";`)))
				}
				return fmt.Sprintf("%x", h.Sum(nil))
			}

			objectsBefore := q("SELECT type||' '||name FROM sqlite_master ORDER BY type, name;")
			dataBefore := dataOf()

			runDoctorFixture(t, bin, ws, args...)

			objectsAfter := q("SELECT type||' '||name FROM sqlite_master ORDER BY type, name;")
			dataAfter := dataOf()

			if objectsBefore != objectsAfter {
				t.Errorf("%v changed the schema object set:\n--- before ---\n%s\n--- after ---\n%s",
					args, objectsBefore, objectsAfter)
			}
			if dataBefore != dataAfter {
				t.Errorf("%v mutated stored data (read-only contract violated)", args)
			}
		})
	}
}

// H — unknown flags fail clearly rather than being swallowed.
func TestDoctorFlag_UnknownFlagRejected(t *testing.T) {
	bin, ws := doctorFixture(t)

	out, exit := runDoctorFixture(t, bin, ws, "doctor", "--definitely-not-a-real-flag")

	if exit == 0 {
		t.Errorf("an unknown flag exited 0:\n%s", out)
	}
	if !strings.Contains(out, "unknown flag") {
		t.Errorf("unknown flag must be named in the error; got:\n%s", out)
	}
	if !strings.Contains(out, "--definitely-not-a-real-flag") {
		t.Errorf("error must echo the offending flag; got:\n%s", out)
	}
}

// G — §9 flag combinations. Flags must compose intentionally; invalid
// combinations are rejected explicitly rather than one silently
// overriding another.
func TestDoctorFlag_CombinationContract(t *testing.T) {
	bin, ws := doctorFixture(t)
	warmDoctorFixture(t, bin, ws)

	t.Run("deep-scan+explain rejected rather than silently ordered", func(t *testing.T) {
		out, exit := runDoctorFixture(t, bin, ws, "doctor", "--explain", "--deep-scan")
		// --explain is its own mode; running both is a user error.
		// Whichever way it resolves, one of them must not silently
		// win. Rejecting is the truthful outcome.
		if exit == 0 {
			t.Errorf("--explain --deep-scan exited 0 without error:\n%s", out)
		}
	})

	t.Run("json+deep-scan rejected", func(t *testing.T) {
		out, exit := runDoctorFixture(t, bin, ws, "doctor", "--json", "--deep-scan")
		if exit == 0 {
			t.Errorf("--json --deep-scan exited 0; must reject:\n%s", out)
		}
		if !strings.Contains(out, "--json cannot be combined") {
			t.Errorf("--json --deep-scan rejection unclear; got:\n%s", out)
		}
	})

	t.Run("json alone still works", func(t *testing.T) {
		out, exit := runDoctorFixture(t, bin, ws, "doctor", "--json")
		if exit > 2 {
			t.Errorf("--json exited %d; expected severity code", exit)
		}
		if !strings.Contains(out, "\"summary\"") {
			t.Errorf("--json did not emit the canonical envelope:\n%s", out)
		}
	})

	t.Run("all three unsupported flags still rejected", func(t *testing.T) {
		out, exit := runDoctorFixture(t, bin, ws,
			"doctor", "--all", "--deep-scan", "--explain", "--fix")
		if exit == 0 {
			t.Errorf("--all --deep-scan --explain --fix exited 0:\n%s", out)
		}
		if !strings.Contains(out, "--all is not supported") {
			t.Errorf("--all should still be rejected when combined; got:\n%s", out)
		}
	})
}

// I — §11 help text must describe exactly what is supported. A flag
// described as active must not be one we reject.
func TestDoctorFlag_HelpMatchesSupportedSurface(t *testing.T) {
	bin, ws := doctorFixture(t)

	out, exit := runDoctorFixture(t, bin, ws, "doctor", "--help")
	if exit != 0 {
		t.Fatalf("doctor --help exited %d:\n%s", exit, out)
	}

	// --all must NOT be advertised as a supported doctor flag. The
	// generic help footer legitimately mentions `mpm help --all` (a
	// different command), so assert on the usage block specifically
	// rather than the whole output.
	usage := out
	if i := strings.Index(out, "Usage: mpm doctor"); i >= 0 {
		usage = out[i:]
		if j := strings.Index(usage, "\n\n"); j > 0 {
			usage = usage[:j]
		}
	}
	if strings.Contains(usage, "--all") {
		t.Errorf("doctor --help advertises --all, which is rejected:\n%s", usage)
	}
	// --fix must appear only with its required --deep-scan pairing.
	if strings.Contains(usage, "--fix") && !strings.Contains(usage, "--deep-scan --fix") {
		t.Errorf("doctor --help advertises --fix without its required pairing:\n%s", usage)
	}
	// The supported flags should be discoverable.
	for _, want := range []string{"--deep-scan", "--explain"} {
		if !strings.Contains(usage, want) {
			t.Errorf("doctor --help does not advertise the supported %s:\n%s", want, usage)
		}
	}
}

// J — §14 the real runtime root is never touched. Assert the live
// binary path is not something these tests can have written to, and
// that the fixture workspace is where state actually landed.
func TestDoctorFlag_TestsNeverTouchLiveRuntime(t *testing.T) {
	bin, ws := doctorFixture(t)
	warmDoctorFixture(t, bin, ws)
	runDoctorFixture(t, bin, ws, "doctor", "--deep-scan")

	// All state must be inside the temp workspace.
	if _, err := os.Stat(filepath.Join(ws, "src", "db", "mpm.db")); err != nil {
		t.Fatalf("expected the fixture DB inside the temp workspace: %v", err)
	}
	// The hermetic binary must not be the installed one.
	if strings.Contains(bin, filepath.Join(".mpm", "bin")) {
		t.Errorf("test binary resolved inside the live runtime root: %s", bin)
	}
}

// I2 — the router's command description must not promise --all or a
// bare --fix, since both are rejected.
func TestDoctorFlag_RouterDescriptionIsTruthful(t *testing.T) {
	cmd := getCmdRouter().resolveCommand("doctor")
	if cmd == nil {
		t.Skip("doctor command not registered")
	}
	if strings.Contains(cmd.Description, "--all") {
		t.Errorf("doctor description advertises --all, which is rejected: %q", cmd.Description)
	}
	if strings.Contains(cmd.Description, "--fix") && !strings.Contains(cmd.Description, "--deep-scan --fix") {
		t.Errorf("doctor description advertises --fix without its required pairing: %q", cmd.Description)
	}
}
