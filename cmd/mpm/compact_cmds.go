// compact_cmds.go — `mpm compact` : the operator surface of the
// deferral lifecycle.
//
//	docs/designs/2026-09-30-compact-refusal-lifecycle.md §3.3 R1
//
// Why this is a CLI command and not an mpm_system action. R1 requires
// that requeue be "an explicit operator action", and R5 gives the
// reason: a deferral is a machine decision, and a requeue is a *human
// overriding a machine decision*. An agent that could requeue could
// undo a refusal on its own judgement — and a refusal is a considered
// judgement, made because the model looked at the rows and declined.
// Undoing it automatically puts back the loop this design exists to
// close, just slower and harder to notice.
//
// So requeue is not a parameter on the compact action. The compact
// action's params schema is `additionalProperties: false`, which means
// an agent cannot even pass it — the refusal is structural, not a
// check somebody can forget to write.
//
// This mirrors `mpm capability grant-operator`, the established shape
// for an operator-only mutation in this CLI: its own noun group, its
// own help, no MCP counterpart.
//
// Subcommands:
//
//	mpm compact deferred           list what is deferred and why
//	mpm compact requeue-deferred   return the oldest N to the pool
//
// Both support --json. The human path renders through the shared
// render package; the machine path is a plain JSON envelope and never
// touches it.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm/cmd/mpm/render"
)

// compactSubcommandDescs is the canonical list of compact
// subcommands. Drives printCompactHelp.
var compactSubcommandDescs = []struct {
	name string
	desc string
}{
	{"deferred", "List rows a synthesis refusal deferred, with reason and batch"},
	{"requeue-deferred [limit]", "Return up to <limit> deferred rows to the compaction pool (default 50)"},
	{"help", "Show this help"},
}

// handleCompact dispatches the `mpm compact` noun group.
//
// args[0] is the noun itself, matching handleCapability and every other
// noun group in this router: the router passes the full argv, so the
// subcommand is args[1].
func handleCompact(args []string) int {
	if len(args) < 2 {
		printCompactHelp()
		return 0
	}

	subCmd := args[1]
	subArgs := args[2:]

	switch subCmd {
	case "deferred":
		return handleCompactDeferred(subArgs)
	case "requeue-deferred":
		return handleCompactRequeueDeferred(subArgs)
	case "help":
		printCompactHelp()
		return 0
	default:
		printCompactHelp()
		return 1
	}
}

// handleCompactDeferred lists the deferred rows. This is the
// inspectability half of R5: an operator should be able to see WHAT is
// deferred and WHY before deciding whether to requeue any of it, and
// both facts are stored on the row.
func handleCompactDeferred(args []string) int {
	jsonOutput, args := ExtractJSONFlag(args)

	limit := mpminternal.CompactBatchSize
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-h", "--help", "help":
			printCompactDeferredHelp()
			return 0
		case "--limit":
			if i+1 >= len(args) {
				printError("--limit requires a value")
				return 1
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil {
				printError("--limit: %q is not a number", args[i+1])
				return 1
			}
			limit = n
			i++
		default:
			if strings.HasPrefix(args[i], "--limit=") {
				n, err := strconv.Atoi(strings.TrimPrefix(args[i], "--limit="))
				if err != nil {
					printError("--limit: %q is not a number", strings.TrimPrefix(args[i], "--limit="))
					return 1
				}
				limit = n
				continue
			}
			printError("%s", fmt.Sprintf("unknown argument: %s", args[i]))
			printCompactDeferredHelp()
			return 1
		}
	}

	dm := getDB()
	if dm == nil {
		printError("database unavailable")
		return 1
	}

	rows, err := dm.ListDeferred(context.Background(), limit)
	if err != nil {
		printError("%s", fmt.Sprintf("list deferred: %v", err))
		return 1
	}

	if jsonOutput {
		out := map[string]interface{}{"success": true, "deferred": rows, "count": len(rows)}
		if rows == nil {
			// A nil slice marshals to null; an operator scripting
			// against this gets [] and can iterate without a nil
			// check that no other list in this CLI asks for.
			out["deferred"] = []mpminternal.DeferredRow{}
		}
		b, err := json.Marshal(out)
		if err != nil {
			printError("%s", fmt.Sprintf("encode: %v", err))
			return 1
		}
		fmt.Println(string(b))
		return 0
	}

	render.Heading(os.Stdout, "Deferred rows")
	render.BlankLine(os.Stdout)
	if len(rows) == 0 {
		render.Plain(os.Stdout, "No deferred rows. Nothing has been held back by a synthesis refusal.")
		return 0
	}
	render.Plainf(os.Stdout, "%d deferred (oldest first, showing up to %d)", len(rows), limit)
	render.BlankLine(os.Stdout)
	for _, r := range rows {
		render.Plainf(os.Stdout, "  %s  %s", r.ID, truncateForDisplay(r.Content, 60))
		render.Plainf(os.Stdout, "      batch %s · %s · deferred %s",
			orDash(r.Batch), orDash(r.Reason), orDash(r.DeferredAt))
	}
	render.BlankLine(os.Stdout)
	render.Hint(os.Stdout, "Return rows to the compaction pool with: mpm compact requeue-deferred [limit]")
	return 0
}

// handleCompactRequeueDeferred returns the oldest deferred rows to the
// compaction pool.
//
// The bound is enforced in the core (R2) and is not re-implemented
// here; this handler only decides what to ask for and how to report
// the answer. Duplicating the cap in the CLI would give two places to
// change and one of them would eventually be wrong.
func handleCompactRequeueDeferred(args []string) int {
	jsonOutput, args := ExtractJSONFlag(args)

	// 0 means "use the core default", which is compactBatchSize. It
	// is not "unbounded" and there is no value of this flag that
	// means unbounded (R2).
	limit := 0

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-h", "--help", "help":
			printCompactRequeueDeferredHelp()
			return 0
		case "--limit":
			if i+1 >= len(args) {
				printError("--limit requires a value")
				return 1
			}
			n, err := strconv.Atoi(args[i+1])
			if err != nil {
				printError("--limit: %q is not a number", args[i+1])
				return 1
			}
			limit = n
			i++
		default:
			if strings.HasPrefix(args[i], "--limit=") {
				n, err := strconv.Atoi(strings.TrimPrefix(args[i], "--limit="))
				if err != nil {
					printError("--limit: %q is not a number", strings.TrimPrefix(args[i], "--limit="))
					return 1
				}
				limit = n
				continue
			}
			// A bare positional is the limit, so
			// `mpm compact requeue-deferred 25` works.
			if n, err := strconv.Atoi(args[i]); err == nil {
				limit = n
				continue
			}
			printError("%s", fmt.Sprintf("unknown argument: %s", args[i]))
			printCompactRequeueDeferredHelp()
			return 1
		}
	}

	dm := getDB()
	if dm == nil {
		printError("database unavailable")
		return 1
	}

	result, err := dm.RequeueDeferred(context.Background(), limit)
	if err != nil {
		printError("%s", fmt.Sprintf("requeue deferred: %v", err))
		return 1
	}

	if jsonOutput {
		// audit_id is unconditional. The trail is written inside the
		// requeue transaction, so a successful call always has one;
		// omitting the key when empty would reintroduce exactly the
		// "did this get recorded?" ambiguity the guarantee removes.
		b, err := json.Marshal(map[string]interface{}{
			"success":            true,
			"requeued":           result.Requeued,
			"row_ids":            result.RowIDs,
			"limit":              result.Limit,
			"remaining_deferred": result.RemainingDeferred,
			"audit_id":           result.AuditID,
		})
		if err != nil {
			printError("%s", fmt.Sprintf("encode: %v", err))
			return 1
		}
		fmt.Println(string(b))
		return 0
	}

	render.Heading(os.Stdout, "Requeue deferred")
	render.BlankLine(os.Stdout)
	if result.Requeued == 0 {
		render.Plain(os.Stdout, "No deferred rows to requeue.")
		return 0
	}
	render.Success(os.Stdout, fmt.Sprintf("Requeued %d row(s) back into the compaction pool", result.Requeued))
	render.BlankLine(os.Stdout)
	render.KeyValue(os.Stdout, "  limit", fmt.Sprintf("%d", result.Limit))
	render.KeyValue(os.Stdout, "  remaining deferred", fmt.Sprintf("%d", result.RemainingDeferred))
	if result.AuditID != "" {
		render.KeyValue(os.Stdout, "  audit", result.AuditID)
	}
	render.BlankLine(os.Stdout)
	render.Plain(os.Stdout, "Requeued rows (oldest first):")
	for _, id := range result.RowIDs {
		render.Plainf(os.Stdout, "  %s", id)
	}
	render.BlankLine(os.Stdout)
	render.Hint(os.Stdout, "Requeue returns rows to the pool; it does not synthesize. The next compaction run offers them again.")
	return 0
}

func printCompactHelp() {
	render.Heading(os.Stdout, "compact")
	render.BlankLine(os.Stdout)
	render.Plain(os.Stdout, "Operator commands for the epistemic compaction deferral lifecycle.")
	render.Plain(os.Stdout, "Requeueing is deliberately not available to agents — it is a human overriding")
	render.Plain(os.Stdout, "a model refusal, and the refusal was a considered judgement.")
	render.BlankLine(os.Stdout)
	render.SectionHeading(os.Stdout, "Subcommands")
	for _, sc := range compactSubcommandDescs {
		render.Label(os.Stdout, "  ▸  "+sc.name, sc.desc)
	}
	render.BlankLine(os.Stdout)
	render.SectionHeading(os.Stdout, "Examples")
	render.Plain(os.Stdout, "  mpm compact deferred")
	render.Plain(os.Stdout, "  mpm compact requeue-deferred 25")
	render.Plain(os.Stdout, "  mpm compact requeue-deferred --limit 25 --json")
	render.BlankLine(os.Stdout)
}

func printCompactDeferredHelp() {
	render.Heading(os.Stdout, "compact deferred")
	render.BlankLine(os.Stdout)
	render.Plain(os.Stdout, "List rows a synthesis refusal deferred, with their reason and batch id.")
	render.BlankLine(os.Stdout)
	render.Plain(os.Stdout, "Usage: mpm compact deferred [--limit N] [--json]")
	render.BlankLine(os.Stdout)
	render.Plain(os.Stdout, "Flags:")
	render.Label(os.Stdout, "  --limit N", fmt.Sprintf("Max rows to list (default %d)", mpminternal.CompactBatchSize))
	render.Label(os.Stdout, "  --json", "Machine-readable output")
	render.BlankLine(os.Stdout)
}

func printCompactRequeueDeferredHelp() {
	render.Heading(os.Stdout, "compact requeue-deferred")
	render.BlankLine(os.Stdout)
	render.Plain(os.Stdout, "Return the oldest deferred rows to the compaction pool so the next drain")
	render.Plain(os.Stdout, "offers them to the model again.")
	render.BlankLine(os.Stdout)
	render.Plain(os.Stdout, "Usage: mpm compact requeue-deferred [limit] [--limit N] [--json]")
	render.BlankLine(os.Stdout)
	render.Plain(os.Stdout, "Flags:")
	render.Label(os.Stdout, "  --limit N", fmt.Sprintf("Max rows to requeue. Default %d. There is no unbounded", mpminternal.CompactBatchSize))
	render.Plain(os.Stdout, "                mode — a limit above the hard cap is clamped, never unlimited.")
	render.Label(os.Stdout, "  --json", "Machine-readable output")
	render.BlankLine(os.Stdout)
	render.SectionHeading(os.Stdout, "What it does and does not do")
	render.Plain(os.Stdout, "  Clears the four compaction_deferred_* keys and returns the rows to the pool")
	render.Plain(os.Stdout, "  on their original created_at position. It does not synthesize anything itself,")
	render.Plain(os.Stdout, "  and it never touches compacted_into — a row whose content already has a")
	render.Plain(os.Stdout, "  lesson stays compacted.")
	render.BlankLine(os.Stdout)
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func truncateForDisplay(s string, max int) string {
	s = strings.ReplaceAll(strings.TrimSpace(s), "\n", " ")
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
