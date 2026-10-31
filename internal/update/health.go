package update

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ManagedServices are the units the updater may restart, and ONLY when
// the operator explicitly authorised it (--restart-services). Agent
// sessions are never in this list.
var ManagedServices = []string{"mpm-scheduler", "mpm-telemetry"}

// ServiceControl probes and restarts managed services. The production
// implementation shells out to systemctl; tests inject a fake so no
// test ever touches the operator's live services.
//
// The interface is per-unit, because the updater must preserve
// previously inactive services as inactive: it stops only the units
// that were active, and starts only those back.
type ServiceControl interface {
	// ActiveUnits returns the managed units currently active.
	ActiveUnits() []string
	// Stop stops one unit.
	Stop(unit string) error
	// Start starts one unit.
	Start(unit string) error
	// MainPID returns the unit's main PID, or 0 when unknown.
	MainPID(unit string) (int, error)
}

// SystemdServices is the production ServiceControl.
type SystemdServices struct{}

func (SystemdServices) ActiveUnits() []string {
	var out []string
	for _, s := range ManagedServices {
		if systemctlActive(s) {
			out = append(out, s)
		}
	}
	return out
}

func (SystemdServices) Stop(unit string) error {
	if out, err := exec.Command("systemctl", "--user", "stop", unit).CombinedOutput(); err != nil { //nolint:gosec
		return fmt.Errorf("could not stop %s: %v\n%s", unit, err, out)
	}
	return nil
}

func (SystemdServices) Start(unit string) error {
	if out, err := exec.Command("systemctl", "--user", "start", unit).CombinedOutput(); err != nil { //nolint:gosec
		return fmt.Errorf("could not start %s: %v\n%s", unit, err, out)
	}
	return nil
}

func (SystemdServices) MainPID(unit string) (int, error) {
	out, err := exec.Command("systemctl", "--user", "show", "-p", "MainPID", "--value", unit).CombinedOutput() //nolint:gosec
	if err != nil {
		return 0, fmt.Errorf("could not query %s: %v", unit, err)
	}
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &pid); err != nil {
		return 0, fmt.Errorf("could not parse MainPID for %s: %v", unit, err)
	}
	return pid, nil
}

// QuietServices reports no running services. Tests use it to prove
// paths without touching live services.
type QuietServices struct{}

func (QuietServices) ActiveUnits() []string            { return nil }
func (QuietServices) Stop(unit string) error           { return nil }
func (QuietServices) Start(unit string) error          { return nil }
func (QuietServices) MainPID(unit string) (int, error) { return 0, nil }

// ServiceHealth is the read-only state of one managed service.
type ServiceHealth struct {
	Name   string
	Active bool
}

// CheckServices probes the managed services without changing anything.
// systemctl is the same read-only probe handlers_info uses.
func CheckServices() []ServiceHealth {
	var out []ServiceHealth
	for _, s := range ManagedServices {
		out = append(out, ServiceHealth{Name: s, Active: systemctlActive(s)})
	}
	return out
}

func systemctlActive(unit string) bool {
	cmd := exec.Command("systemctl", "--user", "is-active", "--quiet", unit) //nolint:gosec
	return cmd.Run() == nil
}

// StopServices stops the managed services. It is called only on paths
// the operator authorised (promotion with --restart-services, or a
// database restore that cannot proceed under writers).
func StopServices() error {
	for _, s := range ManagedServices {
		if !systemctlActive(s) {
			continue
		}
		if out, err := exec.Command("systemctl", "--user", "stop", s).CombinedOutput(); err != nil { //nolint:gosec
			return fmt.Errorf("could not stop %s: %v\n%s", s, err, out)
		}
	}
	return nil
}

// StartServices starts the managed services that were active before.
func StartServices() error {
	for _, s := range ManagedServices {
		if out, err := exec.Command("systemctl", "--user", "restart", s).CombinedOutput(); err != nil { //nolint:gosec
			return fmt.Errorf("could not restart %s: %v\n%s", s, err, out)
		}
	}
	return nil
}

// SmokeResult is the outcome of reading state back through a runtime.
type SmokeResult struct {
	OK     bool
	Detail string
}

// SmokeLive runs health_check through the ACTIVE runtime against the
// LIVE database. It runs only AFTER promotion — running a new binary
// against the live database before the pointer moves is how the
// rehearsal mutated the database it was backing up.
func SmokeLive(dataRoot string) SmokeResult {
	l := NewLayout(dataRoot)
	mpm := LocateActiveMPM(dataRoot)
	if mpm == "" {
		return SmokeResult{Detail: "no active mpm binary"}
	}
	cmd := exec.Command(mpm, "call", "mpm_system", "--payload", `{"action":"health_check"}`) //nolint:gosec
	cmd.Env = append(os.Environ(), "MPM_WORKSPACE="+l.Root, "MPM_DB_PATH="+l.LiveDB())
	out, err := cmd.CombinedOutput()
	if err != nil {
		return SmokeResult{Detail: fmt.Sprintf("health_check failed: %v\n%s", err, out)}
	}
	if !strings.Contains(string(out), `"ok":true`) {
		return SmokeResult{Detail: "health_check did not report ok"}
	}
	return SmokeResult{OK: true, Detail: "health_check through the active runtime"}
}

// SmokeScratch runs health_check through a CANDIDATE binary against a
// SCRATCH copy of state. The scratch is a canonical workspace (the
// database sits at <scratch>/src/db/mpm.db and MPM_DB_PATH restates
// exactly that path), because the substrate refuses a divergent
// MPM_DB_PATH. The live database is never opened by the candidate.
func SmokeScratch(candidateMPM, backupDB string) SmokeResult {
	scratch, err := os.MkdirTemp("", "mpm-update-smoke.")
	if err != nil {
		return SmokeResult{Detail: fmt.Sprintf("could not create scratch: %v", err)}
	}
	defer os.RemoveAll(scratch)
	dbDir := filepath.Join(scratch, "src", "db")
	if err := os.MkdirAll(dbDir, 0700); err != nil {
		return SmokeResult{Detail: fmt.Sprintf("could not create scratch: %v", err)}
	}
	dbPath := filepath.Join(dbDir, "mpm.db")
	raw, err := os.ReadFile(backupDB)
	if err != nil {
		return SmokeResult{Detail: fmt.Sprintf("could not read the backup copy: %v", err)}
	}
	if err := os.WriteFile(dbPath, raw, 0600); err != nil {
		return SmokeResult{Detail: fmt.Sprintf("could not stage the scratch copy: %v", err)}
	}
	cmd := exec.Command(candidateMPM, "call", "mpm_system", "--payload", `{"action":"health_check"}`) //nolint:gosec
	cmd.Env = append(os.Environ(), "MPM_WORKSPACE="+scratch, "MPM_DB_PATH="+dbPath)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return SmokeResult{Detail: fmt.Sprintf("health_check failed against the scratch copy: %v\n%s", err, out)}
	}
	if !strings.Contains(string(out), `"ok":true`) {
		return SmokeResult{Detail: "health_check did not report ok against the scratch copy"}
	}
	return SmokeResult{OK: true, Detail: "health_check against a scratch copy of the state"}
}

// LocateActiveMPM resolves the active mpm binary through the pointer.
func LocateActiveMPM(dataRoot string) string {
	_, rev, err := CurrentReleaseTarget(dataRoot)
	if err != nil || rev == "" {
		return ""
	}
	p := filepath.Join(NewLayout(dataRoot).ReleaseDir(rev), "bin", "mpm")
	if fi, err := os.Stat(p); err == nil && !fi.IsDir() && fi.Mode().Perm()&0111 != 0 {
		return p
	}
	return ""
}
