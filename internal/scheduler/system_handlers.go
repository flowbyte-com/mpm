// system_handlers.go — the production system-wake handler registry.
//
// The scheduler dispatches a wake whose kind has a registered handler;
// every other kind belongs to the agent's notification surface and is
// delivered by the opportunistic fold.
//
// The SET of kinds the daemon dispatches lives here, in the library,
// rather than in cmd/mpm-scheduler/main.go, so that the ownership
// invariant can be stated and tested against the thing that actually
// runs:
//
//	no registered handler kind may be an AGENT-OWNED kind.
//
// A kind is either the agent's or the scheduler's, never both. The
// agent's kinds are delivered and retired by the fold; a handler for
// one of them would execute the same wake twice — once by the fold,
// once by the scheduler.
package scheduler

import (
	"log/slog"
)

// The wake kinds the production daemon dispatches through a handler.
//
// They are named constants because two places need them: the
// registration itself, and SystemHandlerKinds, which is what the
// ownership invariant is tested against. One list, two uses, no drift.
const (
	// SystemHandlerSnapshot takes an atomic SQLite backup.
	SystemHandlerSnapshot = "snapshot"
	// SystemHandlerCriticAudit runs one critic audit cycle.
	SystemHandlerCriticAudit = "critic_audit"
	// SystemHandlerGC runs the garbage-collection sweep.
	SystemHandlerGC = "gc"
	// SystemHandlerBroadcast fans a memory out to other agents.
	SystemHandlerBroadcast = "broadcast"
	// SystemHandlerCascadeSummary writes the per-drain forensic line.
	SystemHandlerCascadeSummary = "cascade_summary"
	// SystemHandlerDrill executes a behavioural drill.
	SystemHandlerDrill = "drill"
)

// SystemHandlerKinds returns every wake kind the production daemon
// dispatches through a registered handler.
//
// The list is the scheduler's half of the ownership boundary. The
// agent's half is core.AgentOwnedWakeKinds, and the two must not
// overlap: a kind in both would be executed by the fold AND by its
// handler.
func SystemHandlerKinds() []string {
	return []string{
		SystemHandlerBroadcast,
		SystemHandlerCascadeSummary,
		SystemHandlerCriticAudit,
		SystemHandlerDrill,
		SystemHandlerGC,
		SystemHandlerSnapshot,
	}
}

// SystemHandlerDeps are the dependencies the production handlers need.
type SystemHandlerDeps struct {
	// Workspace is the workspace the critic audits.
	Workspace string
	// CriticDBPath is the database the critic audits against. It is
	// resolved by the caller so the critic audits the same substrate
	// the scheduler dispatches against — even when --db selected a
	// workspace other than the environment's.
	CriticDBPath string
	// Logger is the daemon's logger.
	Logger *slog.Logger
}

// RegisterSystemHandlers registers every production system-wake handler.
//
// It lives here rather than in cmd/mpm-scheduler/main.go so that the
// registration and the kind list are one thing. Adding a new system
// kind is still a one-line operation; central dispatch never needs
// editing.
func RegisterSystemHandlers(s *Scheduler, deps SystemHandlerDeps) {
	s.Register(SystemHandlerSnapshot, SnapshotHandler)
	s.Register(SystemHandlerCriticAudit, NewCriticAuditHandler(deps.Workspace, deps.CriticDBPath))
	s.Register(SystemHandlerGC, GCHandler)
	s.Register(SystemHandlerBroadcast, BroadcastHandler)
	s.Register(SystemHandlerCascadeSummary, NewCascadeSummaryHandler(deps.Logger))
	s.Register(SystemHandlerDrill, DrillHandler)
}
