package update

import (
	"fmt"
	"os"
	"path/filepath"
)

// stablePathNames are the data-root paths that must resolve into the
// ACTIVE release.
//
// THEY ARE SYMLINKS, NOT DIRECTORIES. Every registration that names
// ~/.mpm keeps naming the same place, and it resolves into whichever
// release is active. The host-asset path matters most: the installed
// managed block advertises
// ~/.mpm/agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md and
// ~/.mpm/agent_installation/mpm-agent-protocol.md, and the
// per-adapter installers import the shared scripts under
// agent_installation/scripts/ by path. A path that does not resolve
// turns the installation's own instructions into dangling pointers.
//
// install.sh establishes both. The updater did not, so an installation
// that was ever promoted by the updater alone could be missing them.
var stablePathNames = []string{"bin", "agent_installation"}

// ensureStablePaths establishes the stable paths into the active
// release.
//
// IT NEVER REPLACES ANYTHING. A path that already exists is left
// exactly as it is: it may be an operator's own arrangement, and the
// installer's own conversion (which moves a legacy directory aside)
// is a decision the updater must not make on the operator's behalf. A
// path that does not exist is created as the symlink install.sh would
// create.
//
// The returned warnings name every path that is NOT the expected
// symlink, so the report says what is odd instead of leaving the
// operator to discover it.
func ensureStablePaths(dataRoot string) ([]string, error) {
	var warnings []string
	for _, name := range stablePathNames {
		target := filepath.Join(dataRoot, name)
		want := filepath.Join("current", name)
		fi, err := os.Lstat(target)
		switch {
		case err == nil && fi.Mode()&os.ModeSymlink != 0:
			got, rerr := os.Readlink(target)
			if rerr != nil {
				warnings = append(warnings, fmt.Sprintf("%s is a symlink but could not be read: %v", target, rerr))
				continue
			}
			if got != want {
				warnings = append(warnings, fmt.Sprintf("%s points at %q, not %q; the installed instructions name %s", target, got, want, target))
			}
		case err == nil:
			warnings = append(warnings, fmt.Sprintf("%s exists and is not the symlink to %s; it was left untouched", target, want))
		case !os.IsNotExist(err):
			return warnings, fmt.Errorf("could not inspect %s: %w", target, err)
		default:
			if err := os.Symlink(want, target); err != nil {
				return warnings, fmt.Errorf("could not create the stable path %s -> %s: %w", target, want, err)
			}
		}
	}
	return warnings, nil
}
