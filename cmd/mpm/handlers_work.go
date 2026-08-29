// cmd/mpm/handlers_work.go — `mpm work` family: thin adapter handlers
// composing the layered architecture.
//
// Architectural invariants (cognitive-interface RFC §7):
//   Commands compose services. They do NOT own SQL. They do NOT
//   own rendering. They do NOT own behaviour.
//
// Each subcommand here is a 5-15 line adapter:
//
//   mpm work status   → WorkingContextService.GetCurrent + StatusRenderer
//   mpm work show     → WorkingContextService.GetCurrent + TerminalRenderer
//   mpm work clear    → WorkingContextService.Clear
//   mpm work promote  → WorkingContextService.Promote + JSONEncoder (--json)
//
// The handlers are deliberately tiny. Anything else (validation,
// aggregation, formatting) belongs in the service/formatter/renderer
// layers — never in this file.
//
// Default session_id: getOrMakeSessionID() from session_runtime.go.
// Every CLI invocation naturally gets a fresh session_id unless the
// operator exported MPM_SESSION_ID. The `--session-id` flag overrides
// for operator use.

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/usererror"
)

// workJSONEncoder is the shape of JSON output for `mpm work --json`.
// Handlers emit one of these for machine consumers. The shape matches
// RFC §7's three-concern split: model + presentation choice separate
// from rendering.
type workJSONEncoder struct {
	SessionID  string    `json:"session_id"`
	Thesis     string    `json:"thesis,omitempty"`
	Supporting string    `json:"supporting,omitempty"`
	UpdatedAt  time.Time `json:"updated_at,omitempty"`
	ExpiresAt  time.Time `json:"expires_at,omitempty"`
}

// handleWork is the entry point for `mpm work <subcommand>`.
// Dispatches subcommands; shared flag parsing for --session-id and
// --json lives below.
func handleWork(args []string) int {
	if len(args) == 0 {
		// mpm work with no subcommand → show usage (similar to mpm ops
		// help behaviour).
		printWorkHelp()
		return 0
	}
	sub := args[0]
	rest := args[1:]

	switch sub {
	case "status":
		return handleWorkStatus(rest)
	case "show":
		return handleWorkShow(rest)
	case "clear":
		return handleWorkClear(rest)
	case "promote":
		return handleWorkPromote(rest)
	case "item":
		// RECOMMENDED 8: `mpm work item <sub>` is a thin discoverable
		// facade over `mpm call mpm_work --payload '{"action":...}'`
		// for the durable work-item family (create/list/show/complete/
		// cancel/history/note/reopen/update). The `item` namespace
		// avoids collision with the scratchpad subcommands above.
		return handleWorkItem(rest)
	case "help", "-h", "--help":
		printWorkHelp()
		return 0
	default:
		usererror.Error("mpm work: unknown subcommand %q\n"+
			"available subcommands: status, show, clear, promote, item", sub)
		return 1
	}
}

// workCommonFlags parses --session-id and --json from any work subcommand's
// args. Returns the parsed values plus the args slice with flags removed.
func workCommonFlags(rest []string, hasJSON bool) (sessionID string, jsonOutput bool) {
	sessionID = getOrMakeSessionID()
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch {
		case a == "--session-id" && i+1 < len(rest):
			sessionID = rest[i+1]
			rest = append(rest[:i], rest[i+2:]...)
			i--
		case strings.HasPrefix(a, "--session-id="):
			sessionID = strings.TrimPrefix(a, "--session-id=")
			rest = append(rest[:i], rest[i+1:]...)
			i--
		case hasJSON && (a == "--json" || a == "-j"):
			jsonOutput = true
			rest = append(rest[:i], rest[i+1:]...)
			i--
		}
	}
	return sessionID, jsonOutput
}

// handleWorkStatus shows a one-line status: session_id, thesis preview,
// age, expiry. Uses StatusRenderer.
func handleWorkStatus(args []string) int {
	sessionID, _ := workCommonFlags(args, false)
	dm := getDBConcrete()
	if dm == nil {
		usererror.Error("database unavailable")
		return 1
	}
	wc, err := buildWorkingContextService(dm).GetCurrent(sessionID)
	if err != nil {
		usererror.Error("work status: %v", err)
		return 1
	}
	renderer := NewStatusRenderer(os.Stdout)
	if wc == nil {
		if err := renderer.RenderEmpty(sessionID); err != nil {
			usererror.Error("render: %v", err)
			return 1
		}
		return 0
	}
	status := WorkingContextStatus{
		SessionID:  wc.SessionID,
		ThesisHead: truncate(wc.Thesis, 80),
		AgeLabel:   formatAgeUnix(wc.UpdatedAt),
		ExpiresIn:  formatExpiresInFromNowUnix(wc.ExpiresAt),
	}
	if err := renderer.Render(status); err != nil {
		usererror.Error("render: %v", err)
		return 1
	}
	return 0
}

// handleWorkShow shows the raw Working Context exactly as stored.
// Uses TerminalRenderer. Per RFC §7 this command never interprets
// or summarises — operators trust the output matches storage.
func handleWorkShow(args []string) int {
	sessionID, _ := workCommonFlags(args, false)
	dm := getDBConcrete()
	if dm == nil {
		usererror.Error("database unavailable")
		return 1
	}
	wc, err := buildWorkingContextService(dm).GetCurrent(sessionID)
	if err != nil {
		usererror.Error("work show: %v", err)
		return 1
	}
	if wc == nil {
		fmt.Printf("Working Context\n  session_id : %s\n  state      : (no active working context)\n", sessionID)
		return 0
	}
	// Raw output per RFC: no rendering, no interpretation.
	// If the agent wrote JSON in supporting, the operator sees JSON.
	// If the agent wrote Markdown, the operator sees Markdown.
	body := "Working Context\n" +
		fmt.Sprintf("  session_id  : %s\n", wc.SessionID) +
		fmt.Sprintf("  thesis      : %s\n", wc.Thesis) +
		"  supporting  :\n"
	// Indent supporting for legibility without altering content.
	if wc.Supporting != "" {
		for _, line := range strings.Split(wc.Supporting, "\n") {
			body += fmt.Sprintf("    %s\n", line)
		}
	} else {
		body += "    (empty)\n"
	}
	body += fmt.Sprintf("  updated_at  : %s\n", mpminternal.FormatUnixSeconds(wc.UpdatedAt))
	body += fmt.Sprintf("  expires_at  : %s\n", mpminternal.FormatUnixSeconds(wc.ExpiresAt))
	fmt.Print(body)
	return 0
}

// handleWorkClear removes the current working context.
// Idempotent — no current context yields a no-op with a friendly message.
func handleWorkClear(args []string) int {
	sessionID, _ := workCommonFlags(args, false)
	dm := getDBConcrete()
	if dm == nil {
		usererror.Error("database unavailable")
		return 1
	}
	if err := buildWorkingContextService(dm).Clear(sessionID); err != nil {
		usererror.Error("work clear: %v", err)
		return 1
	}
	fmt.Printf("Working Context cleared for session %s\n", sessionID)
	return 0
}

// handleWorkPromote promotes the working context to a permanent memory.
// Prints the new memory id. Use --json for machine-readable output.
func handleWorkPromote(args []string) int {
	sessionID, jsonOutput := workCommonFlags(args, true)
	dm := getDBConcrete()
	if dm == nil {
		usererror.Error("database unavailable")
		return 1
	}
	res, err := buildWorkingContextService(dm).Promote(sessionID)
	if err != nil {
		usererror.Error("work promote: %v", err)
		return 1
	}
	if jsonOutput {
		out := struct {
			MemoryID  string `json:"memory_id"`
			SessionID string `json:"session_id"`
			Thesis    string `json:"thesis"`
		}{res.MemoryID, res.SessionID, res.Thesis}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(out); err != nil {
			usererror.Error("json encode failed: %v", err)
			return 1
		}
		return 0
	}
	fmt.Printf("Promoted working context for session %s\n", sessionID)
	fmt.Printf("  thesis    : %s\n", res.Thesis)
	fmt.Printf("  memory_id : %s\n", res.MemoryID)
	return 0
}

// buildWorkingContextService constructs the service graph for `mpm work`.
// Wired at call-time (not package init) so each command invocation gets
// a fresh service with the current DatabaseManager. Stays in this
// file because it is the handler-tier wiring, not a shared utility.
func buildWorkingContextService(dm *mpminternal.DatabaseManager) *WorkingContextService {
	return NewWorkingContextService(
		NewWorkingContextStore(dm),
		NewDatabaseManagerMemoryWriter(dm),
	)
}



// handleWorkItem dispatches the durable work-item sub-namespace under
// `mpm work item <sub>`. Each subcommand is a thin facade that builds
// the canonical mpm_work payload and delegates to handleCall so audit
// recording, error envelopes, and ActiveContext propagation stay in
// one chokepoint. RECOMMENDED 8 surfaces the durable work-item family
// (previously only reachable via `mpm call mpm_work`) without
// duplicating the handler logic.
func handleWorkItem(args []string) int {
	if len(args) == 0 {
		printWorkItemHelp()
		return 0
	}
	// Help-flag short-circuit: --help / -h (and the parseFlags rewrite
	// "help") at any position triggers help. Without this, the
	// leading-flag pre-scan below picks --help as a flag and routes
	// to parseWorkItemArgs, which rejects it as "unknown flag --help"
	// because help is not in the item subcommand's flag set. The
	// "help" form is also matched because parseFlags rewrites --help
	// to the literal token "help" BEFORE this handler runs; without
	// matching "help", `mpm work item --help` would fall through to
	// the pre-scan and error. Help is the universal convention; treat
	// it that way uniformly here so callers do not have to learn
	// which subcommands accept which flags before they can ask for
	// documentation.
	for _, a := range args {
		if a == "--help" || a == "-h" || a == "help" {
			printWorkItemHelp()
			return 0
		}
	}
	// Identify subcommand before extracting flags so that
	// `mpm work item --limit 5` is treated as `mpm work item list --limit 5`
	// rather than an unknown subcommand "--limit". The grammar is:
	//
	//   mpm work item [<flags...>] <sub> [<flags...>] [<positional...>]
	//
	// — flags are accepted before the subcommand (the common ergonomic
	// pattern for `mpm work item --limit 5` against the default `list`
	// subcommand) and after it (the canonical `mpm work item list --limit 5`).
	// If no positional subcommand token appears, default to "list".
	//
	// The scan must skip flag-value pairs (e.g. `--limit 5`) so the
	// value token (5) is not picked as the subcommand. Any token that
	// starts with `--` is a flag — the next token is its value unless
	// it was supplied as `--key=value`.
	valueTakingFlags := map[string]bool{
		"--status":  true,
		"--limit":   true,
		"--note":    true,
		"--content": true,
		"--title":   true,
	}
	var subIdx int = -1
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "--") {
			if valueTakingFlags[a] && i+1 < len(args) {
				i++ // skip the flag's value
			}
			// --key=value form is self-contained, no skip
			continue
		}
		subIdx = i
		break
	}
	var sub string
	var rest []string
	if subIdx < 0 {
		// `mpm work item --limit 5` → default to list with all args as flags.
		sub = "list"
		rest = append([]string{}, args...)
	} else {
		sub = args[subIdx]
		// rest = (everything before subIdx) + (everything after subIdx)
		// so per-subcommand flag extraction still sees the leading flags.
		rest = make([]string, 0, len(args)-1)
		rest = append(rest, args[:subIdx]...)
		rest = append(rest, args[subIdx+1:]...)
	}

	// Parse --status / --limit / --note flags as needed. Returns the
	// remaining positional args (work_id, title, content) and a payload
	// map of extracted key=value pairs plus flag values.
	params, positional, err := parseWorkItemArgs(rest)
	if err != nil {
		usererror.Error("mpm work item %s: %v", sub, err)
		return 1
	}

	var action string
	switch sub {
	case "create":
		if len(positional) < 1 {
			usererror.Error("mpm work item create requires a title positional arg")
			return 1
		}
		action = "create"
		params["title"] = positional[0]
		if len(positional) > 1 {
			params["content"] = strings.Join(positional[1:], " ")
		}
	case "list":
		action = "list"
	case "show":
		if len(positional) < 1 {
			usererror.Error("mpm work item show requires a work_id positional arg")
			return 1
		}
		action = "show"
		params["work_id"] = positional[0]
	case "complete":
		if len(positional) < 1 {
			usererror.Error("mpm work item complete requires a work_id positional arg")
			return 1
		}
		action = "complete"
		params["work_id"] = positional[0]
	case "cancel":
		if len(positional) < 1 {
			usererror.Error("mpm work item cancel requires a work_id positional arg")
			return 1
		}
		action = "cancel"
		params["work_id"] = positional[0]
	case "history":
		if len(positional) < 1 {
			usererror.Error("mpm work item history requires a work_id positional arg")
			return 1
		}
		action = "history"
		params["work_id"] = positional[0]
	case "note":
		if len(positional) < 1 {
			usererror.Error("mpm work item note requires a work_id positional arg")
			return 1
		}
		action = "note"
		params["work_id"] = positional[0]
	case "reopen":
		if len(positional) < 1 {
			usererror.Error("mpm work item reopen requires a work_id positional arg")
			return 1
		}
		action = "reopen"
		params["work_id"] = positional[0]
	case "resolve-contradiction":
		// F6-1 / T20-1: agent-facing first-class contradiction resolution.
		// The recovery path for unsubstantiated disputes against work
		// items. Required: work_id positional, --reason flag (audit trail).
		if len(positional) < 1 {
			usererror.Error("mpm work item resolve-contradiction requires a work_id positional arg")
			return 1
		}
		action = "resolve_contradiction"
		params["work_id"] = positional[0]
		// --reason is required; the handler validates and refuses
		// empty/missing reasons to keep the audit trail intact.
	case "update":
		if len(positional) < 1 {
			usererror.Error("mpm work item update requires a work_id positional arg")
			return 1
		}
		action = "update"
		params["work_id"] = positional[0]
	case "help", "-h", "--help":
		printWorkItemHelp()
		return 0
	default:
		usererror.Error("mpm work item: unknown subcommand %q\navailable subcommands: create, list, show, complete, cancel, history, note, reopen, resolve-contradiction, update", sub)
		return 1
	}

	payload := map[string]interface{}{"action": action, "params": params}
	enc, _ := json.Marshal(payload)
	return handleCall([]string{"mpm_work", "--payload", string(enc)})
}

// parseWorkItemArgs extracts --status / --limit / --note / --content flags
// from rest and returns the remaining positional args. --note and --content
// can be supplied as --key=value or --key value.
func parseWorkItemArgs(rest []string) (map[string]interface{}, []string, error) {
	params := map[string]interface{}{}
	positional := []string{}
	i := 0
	for i < len(rest) {
		a := rest[i]
		switch {
		case a == "--status" && i+1 < len(rest):
			params["status"] = rest[i+1]
			i += 2
		case strings.HasPrefix(a, "--status="):
			params["status"] = strings.TrimPrefix(a, "--status=")
			i++
		case a == "--limit" && i+1 < len(rest):
			if n, err := strconv.Atoi(rest[i+1]); err == nil {
				params["limit"] = n
			}
			i += 2
		case strings.HasPrefix(a, "--limit="):
			if n, err := strconv.Atoi(strings.TrimPrefix(a, "--limit=")); err == nil {
				params["limit"] = n
			}
			i++
		case a == "--note" && i+1 < len(rest):
			params["note"] = rest[i+1]
			i += 2
		case strings.HasPrefix(a, "--note="):
			params["note"] = strings.TrimPrefix(a, "--note=")
			i++
		case a == "--content" && i+1 < len(rest):
			params["content"] = rest[i+1]
			i += 2
		case strings.HasPrefix(a, "--content="):
			params["content"] = strings.TrimPrefix(a, "--content=")
			i++
		case a == "--title" && i+1 < len(rest):
			params["title"] = rest[i+1]
			i += 2
		case strings.HasPrefix(a, "--title="):
			params["title"] = strings.TrimPrefix(a, "--title=")
			i++
		case a == "--reason" && i+1 < len(rest):
			// F6-1 / T20-1: --reason is required for resolve_contradiction
			// so the audit trail captures why a dispute was withdrawn.
			params["reason"] = rest[i+1]
			i += 2
		case strings.HasPrefix(a, "--reason="):
			params["reason"] = strings.TrimPrefix(a, "--reason=")
			i++
		case strings.HasPrefix(a, "--"):
			return nil, nil, fmt.Errorf("unknown flag %q (supported: --status, --limit, --note, --content, --title, --reason)", a)
		default:
			positional = append(positional, a)
			i++
		}
	}
	return params, positional, nil
}

// printWorkItemHelp prints the `mpm work item` subcommand help. RECOMMENDED 8.
func printWorkItemHelp() {
	fmt.Print(`mpm work item — Durable work-item CRUD (multi-session commitments)

Usage:
  mpm work item <subcommand> [args]

Subcommands:
  create <title> [content]   Create a new work item
  list [--status <s>] [--limit <n>]
                              List work items (status: open|done|cancelled|all; default open)
  show <work_id>              Show a single work item by id
  complete <work_id> [--note <text>]
                              Mark a work item complete (records a completion event)
  cancel <work_id> [--note <text>]
                              Cancel a work item (locks verification below verified)
  history <work_id>           Show the full event ledger for a work item
  note <work_id> --note <text>
                              Append a free-form note to a work item's event ledger
  reopen <work_id>            Reopen a cancelled work item
  resolve-contradiction <work_id> --reason <text>
                              F6-1 / T20-1: withdraw unsubstantiated dispute
                              evidence from a work item and re-derive
                              verification. Audit-trail reason required.
  update <work_id> [--title <t>] [--content <c>] [--status <s>]
                              Update a work item's title/content/status

Examples:
  mpm work item create "ship parser fix" "introduce new lexer"
  mpm work item list --status open --limit 10
  mpm work item complete work-abc123 --note "shipped in commit def456"
  mpm work item history work-abc123

This is a discoverable facade over "mpm call mpm_work --payload '{"action":...}'".
`)
}


// printWorkHelp prints the `mpm work` subcommand help.
func printWorkHelp() {
	fmt.Print(`mpm work — Expose the current Working Context (ephemeral execution state)

Usage:
  mpm work <subcommand> [args]

Subcommands:
  status     Quick one-line status (session, age, expires)
  show       Show the raw working context, exactly as stored
  clear      Discard the current working context
  promote    Promote the working context to a permanent memory
  item       Durable work-item CRUD (create/list/show/complete/cancel/history/note/reopen/update)

Flags:
  --session-id <id>   Override the per-process session id (default: getOrMakeSessionID)
  --json              Emit machine-readable JSON (where applicable)

The 'work' command never exposes the implementation name 'scratchpad';
internally the substrate's existing scratchpad APIs are used.

The 'item' subcommand is a discoverable facade over "mpm call mpm_work";
see "mpm work item help" for the durable work-item vocabulary.
`)
}

// flag was unused in the Wave 1 minimum; kept stubbed so future
// subcommands that need real flag parsing have an obvious hook.
// (Today's uses use workCommonFlags which scans rest string-by-string.)
var _ = flag.NewFlagSet
