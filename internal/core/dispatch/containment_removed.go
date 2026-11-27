package dispatch

// containment_removed.go — the migration gate for the withdrawn subsystem.
//
// MPM used to run its agents inside an MPM-managed operating-system
// containment: bubblewrap, an unshared network namespace, an inference
// sidecar and relay, and a dedicated reviewer credential. That subsystem is
// REMOVED. MPM now invokes the agent framework natively, and isolation, if
// any, belongs to the framework and the deployment environment.
//
// # WHY THIS GATE EXISTS
//
// An existing deployment may still export the containment's environment:
// a systemd unit, a shell profile or a CI runner that was configured for
// the old boundary. Silently ignoring those variables would remove a
// security boundary without saying so — the one thing an upgrade must not
// do. So execution REFUSES while they are present, and says exactly what
// changed and how to acknowledge it.
//
// # THE ACKNOWLEDGEMENT IS TRANSITIONAL
//
// `MPM_CONTAINMENT_REMOVED_ACK=1` exists so an operator can bring a service
// back before they can edit its unit. It is not a mode: nothing in MPM
// behaves differently when it is set, and it is intended to be removed in a
// later release once deployments have had one upgrade window to clear the
// obsolete variables.
import (
	"fmt"
	"strings"
)

// RemovedContainmentEnv names every variable the withdrawn containment
// read. Presence of ANY of them means the environment was configured for a
// boundary MPM no longer provides.
var RemovedContainmentEnv = []string{
	"MPM_OPENCODE_SIDECAR_KEY",
	"MPM_OPENCODE_SIDECAR_UPSTREAM",
	"MPM_OPENCODE_SIDECAR_MODEL",
	"MPM_OPENCODE_REVIEWER_KEY",
	"MPM_OPENCODE_PLUGIN",
}

// ContainmentRemovedAck is the transitional acknowledgement.
const ContainmentRemovedAck = "MPM_CONTAINMENT_REMOVED_ACK"

// CheckContainmentRemoval refuses execution while obsolete containment
// configuration is present and unacknowledged.
//
// getenv is injected so the check is testable without touching the process
// environment.
func CheckContainmentRemoval(getenv func(string) string) error {
	if getenv == nil {
		return nil
	}
	if strings.TrimSpace(getenv(ContainmentRemovedAck)) == "1" {
		return nil
	}
	var found []string
	for _, name := range RemovedContainmentEnv {
		if strings.TrimSpace(getenv(name)) != "" {
			found = append(found, name)
		}
	}
	if len(found) == 0 {
		return nil
	}
	return fmt.Errorf(
		"dispatch: MPM-managed operating-system containment has been REMOVED, but this "+
			"environment still configures it (%s). MPM now invokes the agent framework "+
			"natively: there is no bubblewrap, no network namespace, no sidecar relay and "+
			"no MPM-provided isolation. The framework's permissions and the deployment "+
			"environment are the boundary. Remove those variables (the containment was "+
			"not providing a boundary any more), or set %s=1 to acknowledge the removal "+
			"for this transitional release.",
		strings.Join(found, ", "), ContainmentRemovedAck)
}
