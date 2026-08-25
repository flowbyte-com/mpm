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
	case "help", "-h", "--help":
		printWorkHelp()
		return 0
	default:
		usererror.Error("mpm work: unknown subcommand %q\n"+
			"available subcommands: status, show, clear, promote", sub)
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

Flags:
  --session-id <id>   Override the per-process session id (default: getOrMakeSessionID)
  --json              Emit machine-readable JSON (where applicable)

The 'work' command never exposes the implementation name 'scratchpad';
internally the substrate's existing scratchpad APIs are used.

Note: additional work actions (create, complete, cancel, reopen, history,
note) are available via the machine interface: mpm call mpm_work.
`)
}

// flag was unused in the Wave 1 minimum; kept stubbed so future
// subcommands that need real flag parsing have an obvious hook.
// (Today's uses use workCommonFlags which scans rest string-by-string.)
var _ = flag.NewFlagSet
