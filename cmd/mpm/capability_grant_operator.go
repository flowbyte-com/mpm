// capability_grant_operator.go — `mpm capability grant-operator` command.
//
// Stamps `metadata.operator_approved_at` on a capability row so the
// executor's operator-domain gate admits the capability at invoke-time
// (executor.go: int64FromMeta(req.Capability.Metadata, "operator_approved_at")).
//
// This is the audit-preserving counterpart to the gate. Both the
// metadata stamp AND a capability_events row with event_type=
// 'operator_approval' are written in the same transaction, so the
// metadata the gate reads can never disagree with the audit trail
// `mpm skill audit <id>` surfaces.
//
// Usage:
//
//	mpm capability grant-operator <capability-id>
//	mpm capability grant-operator <capability-id> --actor <name>
//	mpm capability grant-operator <capability-id> --reason "<free-form rationale>"
//
// Flags:
//
//	--actor <name>      Operator identity recorded in the audit row
//	                     and metadata.operator_approved_by. Defaults
//	                     to "operator" if omitted.
//	--reason <text>     Free-form rationale recorded in the event row.
//	                     Empty string is fine; this is metadata, not a
//	                     human-readable commit message.
//
// State guards: refuses to stamp capabilities in `retired` or
// `fractured` state (they should be reconciled first). Soft-deleted
// rows surface as ErrNotFound.
//
// Idempotency: re-running refreshes the timestamp + actor + writes
// a fresh event row. The operator might want to assert "I re-
// verified this just now" and the fresh stamp is the audit-preserving
// way to record that.
package main

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/flowbyte-com/mpm-core/capability"
)

// handleCapabilityGrantOperator is the entry point for
// `mpm capability grant-operator <id>`.
//
// Args layout (after the global parseFlags rewrite):
//
//	[actor-name-or-empty] [--actor <name>] [--reason <text>]
//	[--help | -h | help]
//
// The capability id is the first positional argument (required).
// Subsequent flags are parsed left-to-right.
func handleCapabilityGrantOperator(args []string) int {
	// 1. Parse flags. We need at least the capability id.
	if len(args) == 0 {
		printCapabilityGrantOperatorHelp()
		printError("missing capability id")
		return 1
	}

	var (
		capID string
		actor string
		reason string
	)

	// Walk the args. First positional becomes the cap id; the
	// rest are flags. We deliberately DON'T use the global
	// parseFlags here because the leading positional (the
	// capability id) is required before any flag handling.
	i := 0
	for i < len(args) {
		a := args[i]
		switch a {
		case "-h", "--help", "help":
			printCapabilityGrantOperatorHelp()
			return 0
		case "--actor":
			if i+1 >= len(args) {
				printError("--actor requires a name argument")
				return 1
			}
			actor = args[i+1]
			i += 2
		case "--reason":
			if i+1 >= len(args) {
				printError("--reason requires a text argument")
				return 1
			}
			reason = args[i+1]
			i += 2
		default:
			if strings.HasPrefix(a, "-") {
				printError("mpm capability grant-operator: unknown flag %q", a)
				fmt.Println("Try: mpm capability grant-operator --help")
				return 1
			}
			// First positional = capability id. Subsequent
			// positionals are rejected as ambiguous.
			if capID != "" {
				printError("mpm capability grant-operator: unexpected positional argument %q (capability id already given as %q)",
					a, capID)
				return 1
			}
			capID = a
			i++
		}
	}

	if strings.TrimSpace(capID) == "" {
		printCapabilityGrantOperatorHelp()
		printError("missing capability id")
		return 1
	}

	// 2. Open the DB and stamp.
	dm := getDBConcrete()
	if dm == nil {
		return 1
	}
	store := capability.NewStore(dm)

	updated, err := store.MarkOperatorApproved(capID, actor, time.Time{}, reason)
	if err != nil {
		// Distinguish not-found from refused so the operator
		// gets an actionable message.
		switch {
		case errors.Is(err, capability.ErrNotFound):
			printError("capability %q not found (or soft-deleted)", capID)
			fmt.Println("Tip: list live capabilities with: mpm help capability list")
			return 1
		case errors.Is(err, capability.ErrOperatorApprovalRefused):
			printError("%v", err)
			fmt.Println("Tip: resolve the state issue (retire is terminal; fracture needs needs_revision first), then retry.")
			return 1
		default:
			printError("grant-operator: %v", err)
			return 1
		}
	}

	// 3. Print confirmation.
	printGrantOperatorReport(capID, actor, reason, updated)
	return 0
}

// printGrantOperatorReport renders the audit stamp result. Mirrors
// the report-shape used by handleCapabilitySeed so the operator
// gets a consistent surface across capability subcommands.
func printGrantOperatorReport(capID, actor, reason string, c *capability.Capability) {
	fmt.Println("Operator Approval Granted")
	fmt.Println(strings.Repeat("─", 60))
	fmt.Printf("\n  Capability:    %s (%s)\n", c.Name, c.ID)
	fmt.Printf("  State:         %s\n", c.State)
	if actor != "" {
		fmt.Printf("  Approved by:   %s\n", actor)
	} else {
		fmt.Printf("  Approved by:   operator (default)\n")
	}

	// The gate's int64 unix epoch — the field that actually
	// unlocks operator-domain execution.
	got := capability.Int64FromMeta(c.Metadata, "operator_approved_at")
	if got != 0 {
		fmt.Printf("  Stamp (unix):  %d  (%s)\n", got, time.Unix(got, 0).UTC().Format(time.RFC3339))
	}
	if v, ok := c.Metadata["operator_approved_at_rfc3339"]; ok {
		if s, ok := v.(string); ok {
			fmt.Printf("  Stamp (RFC):   %s\n", s)
		}
	}
	if reason != "" {
		fmt.Printf("  Reason:        %s\n", reason)
	}

	fmt.Println("\nThis capability may now be invoked under execution_domain=operator.")
	fmt.Println("Inspect the audit trail with:")
	fmt.Printf("  mpm skill audit %s\n", c.ID)
}

// printCapabilityGrantOperatorHelp is the per-subcommand help.
func printCapabilityGrantOperatorHelp() {
	fmt.Println()
	fmt.Println("mpm capability grant-operator — Stamp metadata.operator_approved_at")
	fmt.Println("on a capability row so the executor's operator-domain gate admits it.")
	fmt.Println()
	fmt.Println("Usage: mpm capability grant-operator <capability-id> [--actor <name>] [--reason <text>]")
	fmt.Println()
	fmt.Println("Arguments:")
	fmt.Println("  <capability-id>       The capability to approve (e.g. cap.list_capabilities).")
	fmt.Println()
	fmt.Println("Flags:")
	fmt.Println("  --actor <name>        Operator identity recorded in the audit row.")
	fmt.Println("                         Defaults to \"operator\" if omitted.")
	fmt.Println("  --reason <text>       Free-form rationale recorded in the event row.")
	fmt.Println("                         Empty string is fine; this is metadata, not a commit message.")
	fmt.Println("  --help, -h            Show this help.")
	fmt.Println()
	fmt.Println("The stamp is idempotent — re-running refreshes the timestamp + actor")
	fmt.Println("and writes a fresh audit row. Refuses to stamp retired or fractured")
	fmt.Println("capabilities (reconcile those states first).")
	fmt.Println()
}
