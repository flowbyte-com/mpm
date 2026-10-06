package scheduler

import (
	"fmt"
	"os"
	"path/filepath"
)

// MPMBinEnv names the environment variable that pins the `mpm` binary the
// scheduler shells out to.
//
// Why this exists. The scheduler invoked a bare "mpm", which exec resolves
// through PATH. Under systemd the scheduler's PATH is whatever the manager
// hands a user unit — not the operator's interactive shell PATH — so the
// binary that actually ran was whichever `mpm` happened to come first, if
// any. That is the same "installed location decided by ambient state" problem
// the workspace default had, one process hop further out: a stale `mpm`
// earlier in PATH silently serves the GC and broadcast wakes while the
// installed binary sits unused, and a hostile or accidental earlier entry
// gets to act as MPM.
//
// MPM_BIN mirrors the existing MPM_CRITIC_BIN convention, so the unit
// declares where every subprocess binary lives in one place.
const MPMBinEnv = "MPM_BIN"

// resolveMPMBin returns the binary to exec for scheduler-initiated `mpm`
// invocations.
//
// Three cases, and the middle one is the whole point:
//
//   - MPM_BIN set and usable  -> that exact path, always. PATH is not
//     consulted at all, so an earlier `mpm` on PATH cannot be selected.
//   - MPM_BIN set but broken -> an error. A configured path that does not
//     resolve must NOT silently degrade to a PATH lookup: the operator has
//     told us where the binary is, and quietly running a different one
//     because that one is missing would make the misconfiguration
//     invisible in exactly the situation where it matters most.
//   - MPM_BIN unset          -> bare "mpm" for ad-hoc and test launches.
//     This is an explicit contract, not an installed-scheduler contract:
//     the unit template sets MPM_BIN, so an installed scheduler never
//     reaches this branch.
func resolveMPMBin() (string, error) {
	bin := os.Getenv(MPMBinEnv)
	if bin == "" {
		return "mpm", nil
	}
	if err := checkExecutable(bin); err != nil {
		return "", fmt.Errorf(
			"%s=%q is configured but unusable: %w. Refusing to fall back to a "+
				"PATH lookup — running a different `mpm` than the one configured "+
				"would make this misconfiguration invisible",
			MPMBinEnv, bin, err)
	}
	return bin, nil
}

// checkExecutable rejects a configured path that cannot be exec'd, so the
// failure names the real cause instead of surfacing as a generic exec error.
func checkExecutable(path string) error {
	if filepath.Base(path) == path {
		// A bare name, not a path. There is nothing to stat, and honouring
		// it here would reintroduce the PATH lookup this exists to remove.
		return fmt.Errorf("not an absolute or explicit path")
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("is a directory, not an executable")
	}
	if info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("not executable (mode %s)", info.Mode().Perm())
	}
	return nil
}