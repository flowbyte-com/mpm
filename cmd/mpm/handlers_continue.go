// ==================================================================
//
// cmd/mpm/handlers_continue.go — mpm continue handler.
//
// ------------------------------------------------------------------
//
// ARCHITECTURE INVARIANT
//
// continue presents state.
//
// It MUST NOT:
//   - recommend actions
//   - rank priorities
//   - become a planner
//   - own any data source directly
//   - render any section that belongs to a subsystem
//   - invoke other commands (no exec.Command, no os/exec)
//   - own SQL / persistence logic
//   - own rendering logic
//
// It is intentionally analogous to:
//   git status
// not:
//   project manager
//
// Composition discipline:
//
//   Every section of continue's dashboard must have a Service owner.
//   continue owns ONLY the layout — the structure that turns
//   delegated sections into one dashboard. Everything inside a
//   section is rendered by the section's renderer.
//
//   continue commands services:
//     workingctx.Service.Load()
//     wake.Service.Load()
//     decision.Service.Recent()
//     skill.Service.Loaded()
//     theory.Service.Active()
//     status.Service.Snapshot()
//
//   and chooses its Formatter + DashboardRenderer for its
//   presentation context. mpm work show commands the same
//   WorkingContextService and chooses its own Formatter + Renderer.
//   Neither calls the other.
//
// The lines to defend:
//
//   continue is a view over multiple models.
//   It owns nothing except layout.
//
//   Commands do not compose commands.
//   Commands compose services.
//
// Breaking this invariant changes the cognitive architecture.
//
// ------------------------------------------------------------------
//
// This file is the cognitive-interface RFC's Wave 1 commit 3. Future
// contributors editing this file MUST understand the invariant above
// before making changes. The day a recommendation or priority-rank
// lands inside this handler, the cognitive architecture is broken.
//
// ==================================================================

package main

import (
	"flag"
	"os"

	"github.com/flowbyte-com/mpm-core/usererror"
)

// handleContinue assembles and prints the mpm continue dashboard.
// Tiny adapter: composes ContinueService, hands DashboardModel to
// DashboardRenderer, exits. No behaviour in this handler beyond
// flag parsing and the orchestration call.
func handleContinue(args []string) int {
	fs := flag.NewFlagSet("continue", flag.ContinueOnError)
	sessionID := fs.String("session-id", "", "Override session id (default: getOrMakeSessionID)")
	if err := fs.Parse(args); err != nil {
		// flag.ContinueOnError already wrote the error; just exit.
		return 2
	}

	if *sessionID == "" {
		*sessionID = getOrMakeSessionID()
	}

	dm := getDBConcrete()
	if dm == nil {
		usererror.Error("database unavailable")
		return 1
	}

	workingCtxSvc := NewWorkingContextService(
		NewWorkingContextStore(dm),
		NewDatabaseManagerMemoryWriter(dm),
	)
	continueSvc := NewContinueService(workingCtxSvc, dm)
	if continueSvc == nil {
		usererror.Error("continue: failed to wire services")
		return 1
	}

	model, err := continueSvc.Compose(*sessionID)
	if err != nil {
		// Compose returns an error only when session_id is missing —
		// already validated above. Treat any other error as fatal.
		usererror.Error("continue: %v", err)
		return 1
	}

	renderer := NewDashboardRenderer(os.Stdout)
	if err := renderer.Render(model); err != nil {
		usererror.Error("dashboard render: %v", err)
		return 1
	}
	return 0
}
