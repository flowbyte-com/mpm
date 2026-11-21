// assignment_dispatch_cmds.go — the operator's door to the dispatcher.
//
// Two verbs live here:
//
//	mpm assignment dispatch         — ONE bounded dispatch pass
//	mpm assignment dispatch-status  — what is pending, blocked, recoverable
//
// # WHY THEY ARE CLI-ONLY
//
// `dispatch` starts a contained runtime process and spends real model
// budget, exactly as PR 3's `execute` does. It is not an MCP action and
// it is not in the tool registry: an agent must not be able to start
// inference on its own initiative. The operator's shell is the door.
//
// # THE DISPATCHER IS OFF UNLESS THE OPERATOR SAYS SO
//
// The configuration file is the switch. A MISSING file means the
// dispatcher is off, and the verb says so and exits without reading a
// row or building a runtime. A file that exists and cannot be
// understood is an ERROR: a broken configuration is never a reason to
// guess at the limits.
//
// # ONE PASS, NOT A LOOP
//
// `dispatch` performs ONE pass and exits. It does not poll, does not
// sleep between attempts, and does not run in the background. The
// scheduler integration is the thing that repeats — and it is inert
// unless the same configuration file enables it.
package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/flowbyte-com/mpm-core/assignment"
	"github.com/flowbyte-com/mpm-core/config"
	"github.com/flowbyte-com/mpm-core/dispatch"
)

// dispatchConfigFileName is the configuration file's name inside the MPM
// directory.
//
// It is a FILE and not a flag's default alone, because the scheduler
// integration has to find the same file without an operator typing
// anything: an installed binary must never dispatch on its own, and a
// named file is the explicit opt-in.
const dispatchConfigFileName = "dispatch.json"

// dispatchConfigPath is where the dispatcher reads its configuration.
//
// The operator may point it elsewhere with --dispatch-config; the
// default is the MPM directory's dispatch.json.
func dispatchConfigPath(f *assignmentFlags) string {
	if strings.TrimSpace(f.dispatchConfig) != "" {
		return f.dispatchConfig
	}
	return filepath.Join(config.GetMPMDir(), dispatchConfigFileName)
}

// loadDispatchConfig reads the dispatcher's configuration.
//
// A missing file is the disabled default, and no error. A file that
// exists and is unusable is an error, and the error says what is wrong
// with it.
func loadDispatchConfig(f *assignmentFlags) (dispatch.Config, error) {
	return dispatch.LoadConfig(dispatchConfigPath(f))
}

// assignmentVerbDispatch performs ONE bounded dispatch pass.
func assignmentVerbDispatch(store *assignment.Store, f *assignmentFlags) (interface{}, error) {
	cfg, err := loadDispatchConfig(f)
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		// The disabled path is explicit and cheap: it says the
		// dispatcher is off and it does nothing else. It does NOT read a
		// row and it does NOT build a runtime, so an operator can run it
		// to confirm the state of the switch without any risk.
		return &dispatch.Report{
			Enabled: false,
			Outcomes: []dispatch.Outcome{{
				Reason: dispatch.ReasonDisabled,
				Detail: "autonomous dispatch is not enabled; no work was discovered and no runtime was built",
			}},
		}, nil
	}

	factory, err := dispatch.NewOpenCodeFactory(cfg)
	if err != nil {
		return nil, err
	}
	defer factory.Close()

	engine, err := dispatch.NewEngine(store, cfg, factory.Factory())
	if err != nil {
		return nil, err
	}
	return engine.RunOnce(context.Background())
}

// assignmentVerbDispatchStatus reports what the dispatcher could do.
//
// It is a READ: no runtime is built, no session is created, nothing is
// written. It works when the dispatcher is disabled, which is exactly
// when an operator wants to see what enabling it would do.
func assignmentVerbDispatchStatus(store *assignment.Store, f *assignmentFlags) (interface{}, error) {
	cfg, err := loadDispatchConfig(f)
	if err != nil {
		return nil, err
	}
	return dispatch.Status(store, cfg)
}

// renderDispatchReport renders one dispatch pass for a human.
//
// The counts come first, because they are what the operator asked: how
// much was dispatched, completed, refused, deferred and ambiguous. The
// per-assignment reasons follow, because the reason is what says what
// to do next.
func renderDispatchReport(report *dispatch.Report) {
	fmt.Printf("dispatcher: %s\n", enabledWord(report.Enabled))
	fmt.Printf("dispatched %d   completed %d   refused %d   deferred %d   ambiguous %d\n",
		report.Dispatched, report.Completed, report.Refused, report.Deferred, report.Ambiguous)
	fmt.Println()
	for _, o := range report.Outcomes {
		if o.AssignmentID == "" {
			fmt.Printf("  %-24s %s\n", o.Reason, o.Detail)
			continue
		}
		fmt.Printf("  %s  %-24s %s\n", o.AssignmentID, o.Reason, o.Detail)
	}
}

// renderDispatchStatus renders the status report for a human.
func renderDispatchStatus(status *dispatch.StatusReport) {
	fmt.Printf("dispatcher: %s\n", enabledWord(status.Enabled))
	fmt.Printf("\npending (%d)\n", len(status.Pending))
	for _, item := range status.Pending {
		fmt.Printf("  %s  %-11s %-11s %s\n", item.AssignmentID, item.Role, item.State, item.Title)
	}
	fmt.Printf("\nblocked (%d)\n", len(status.Blocked))
	for _, item := range status.Blocked {
		fmt.Printf("  %s  %-11s %-20s %s\n", item.AssignmentID, item.Role, item.Reason, item.Detail)
	}
	fmt.Printf("\nrecoverable (%d)\n", len(status.Recoverable))
	for _, item := range status.Recoverable {
		fmt.Printf("  %s  %-11s %-20s %s\n", item.AssignmentID, item.Role, item.Reason, item.Detail)
	}
	fmt.Printf("\nbudgets (%d)\n", len(status.Budgets))
	for _, b := range status.Budgets {
		fmt.Printf("  %s  tasks %d/%d  units %d/%d  live claims %d/%d\n",
			b.RootID, b.Tasks, b.MaxTasks, b.Units, b.MaxUnits,
			b.LiveClaims, b.MaxConcurrent)
	}
}

// enabledWord renders the switch.
func enabledWord(enabled bool) string {
	if enabled {
		return "ENABLED"
	}
	return "disabled"
}
