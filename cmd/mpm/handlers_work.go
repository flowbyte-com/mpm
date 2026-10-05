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
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/usererror"

	"github.com/flowbyte-com/mpm/cmd/mpm/render"
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
	// R3: --help / -h / "help" short-circuit. See handleMemoryAdd for
	// the rationale; same defect, same fix. Without this, `mpm work
	// status --help` would render the working-context status block
	// instead of printing work help.
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "help" {
			printWorkHelp()
			return 0
		}
	}
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
	// Defect 1 (2026-09-13 acceptance): mpm work clear --help used to
	// actually clear the working context because the router's
	// isSubcommandHelp fall-through routed "help" to this handler
	// without a short-circuit. Without this guard, `--help` after the
	// subcommand name is a destructive mutation vector.
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "help" {
			printWorkHelp()
			return 0
		}
	}
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
	// Defect 2 (2026-09-13 acceptance): mpm work promote --help used
	// to enter the promotion path because the router's
	// isSubcommandHelp fall-through routed "help" to this handler
	// without a short-circuit. Without this guard, `--help` after the
	// subcommand name is a destructive mutation vector.
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "help" {
			printWorkHelp()
			return 0
		}
	}
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
		"--status":      true,
		"--limit":       true,
		"--note":        true,
		"--content":     true,
		"--title":       true,
		"--reason-code": true,
		"--backup":      true,
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
	// Round 9 T57: validate --status against the canonical work
	// lifecycle (open|done|cancelled). The substrate silently coerces
	// any unknown value to "open" via AddWork's hardcoded INSERT,
	// which masks operator error — `mpm work item create --status
	// typo` produces an `open` work without any signal. Reject
	// non-canonical values at the CLI boundary. The canonical set is
	// the persisted-state vocabulary (internal/core/work.go); we do
	// NOT add `in_progress` as a stored lifecycle value because it
	// does not appear in isValidWorkTransition's switch.
	//
	// Rough-edge closure 2026-09-12 (item 9): `all` is a valid LIST
	// filter (no status constraint) — advertised by `work item list
	// --help`, accepted by the substrate (validWorkStatuses), and
	// expected by the grammar contract test. It is meaningless for
	// create (AddWork hardcodes open), so it stays rejected there.
	// The pre-fix gate rejected `all` everywhere, failing the
	// documented `list --status all` form.
	if s, ok := params["status"].(string); ok && s != "" {
		allowed := s == "open" || s == "done" || s == "cancelled" ||
			(s == "all" && sub == "list")
		if !allowed {
			if sub == "list" {
				usererror.Error("mpm work item: --status %q is not a valid list filter (use open|done|cancelled|all)", s)
			} else {
				usererror.Error("mpm work item: --status %q is not a canonical lifecycle state (use open|done|cancelled)", s)
			}
			return 1
		}
	}
	// --visibility is the second, independent axis (2026-09-30). It is
	// valid only for `list` — there is no "visibility of a single
	// work item" operation. Rejecting it elsewhere keeps a typo from
	// being silently ignored on archive/unarchive.
	if v, ok := params["visibility"].(string); ok && v != "" {
		valid := v == "active" || v == "archived" || v == "all"
		if !valid {
			usererror.Error("mpm work item: --visibility %q is not a valid list filter (use active|archived|all)", v)
			return 1
		}
		if sub != "list" {
			usererror.Error("mpm work item: --visibility applies only to `list`")
			return 1
		}
	}

	var action string
	switch sub {
	case "create":
		// Round 9 T54b: --title is the ergonomic flag form,
		// positional is the muscle-memory form; both are supported.
		// Pre-fix this branch required a positional title AND
		// ignored the parsed --title flag, so `mpm work item
		// create --title "X"` (and `mpm work item --title X create`)
		// got "requires a title positional arg". Honor --title
		// when set; fall through to the positional on the
		// canonical `create <title>` form.
		switch {
		case len(positional) >= 1:
			params["title"] = positional[0]
		case params["title"] != nil:
			// already parsed by parseWorkItemArgs; honor it.
		default:
			usererror.Error("mpm work item create requires a title (positional <title> or --title <text>)")
			return 1
		}
		// Conflict: both positional and --title supplied.
		// The positional form wins on the canonical `create
		// <title>` shape; if BOTH are present, the positional
		// value overrode --title above (and --title is
		// silently shadowed). Surface a notice on stderr but
		// do not error — `create --title "x" "y"` is
		// sometimes used by tooling that bulk-applies a title
		// shell-escape pattern.
		if len(positional) >= 1 && cmdLineHasFlag(rest, "--title") {
			usererror.Notice("--title flag supplied alongside positional title; positional value wins.")
		}
		if len(positional) > 1 {
			params["content"] = strings.Join(positional[1:], " ")
		}
		action = "create"
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
		// 2026-09-10 regression repair (T56): the natural
		// positional form `mpm work item note <id> "note text"`
		// must work alongside `--note "note text"`. Pre-fix the
		// positional form silently dropped the note text. The
		// flag form continues to work via parseWorkItemArgs.
		if len(positional) > 1 {
			params["note"] = strings.Join(positional[1:], " ")
		}
	case "reopen":
		if len(positional) < 1 {
			usererror.Error("mpm work item reopen requires a work_id positional arg")
			return 1
		}
		action = "reopen"
		params["work_id"] = positional[0]
	case "archive", "unarchive":
		// Archive lifecycle (2026-09-30). Archive is TERMINAL-ONLY:
		// an open item is refused with no writes. Unarchive restores
		// visibility only and never reopens work.
		if len(positional) < 1 {
			usererror.Error("mpm work item %s requires a work_id positional arg", sub)
			return 1
		}
		action = sub
		params["work_id"] = positional[0]
	case "purge":
		// Logical purge (2026-09-30). CLI-only by design: no `purge`
		// action exists on mpm_work or mpm_system, because both are
		// agent-reachable and an agent-reachable irreversible delete is
		// a different tool with a different risk profile.
		if len(positional) < 1 {
			usererror.Error("mpm work item purge requires a work_id positional arg")
			return 1
		}
		action = sub
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
		usererror.Error("mpm work item: unknown subcommand %q\navailable subcommands: create, list, show, complete, cancel, history, note, reopen, archive, unarchive, purge, resolve-contradiction, update", sub)
		return 1
	}

	// 2026-09-14 release-pass: `mpm work item list` shares the
	// structured row data between human mode and JSON mode. The
	// CLI handler calls the canonical ListWorkRows helper directly
	// (the same helper the substrate's mpm_work JSON path uses),
	// renders the canonical `MPM · Work item list` heading in
	// human mode, and preserves the JSON envelope when --json is
	// passed. The handler does NOT call another CLI surface and
	// parse its JSON output.
	if action == "list" {
		return handleWorkItemList(params)
	}

	// Purge bypasses the mpm_work RPC path entirely. It has no MCP
	// action and must never acquire one by accident, so it is
	// dispatched here, before the payload is built and before
	// handleCall — the substrate method is reached directly.
	if action == "purge" {
		return handleWorkItemPurge(params)
	}

	// 2026-09-14 release-pass: normalise work-id inputs through
	// NormalizeWorkID so every `mpm work item <sub>` command
	// accepts the canonical `mpm://work/<id>` pointer form in
	// addition to the bare id. The bare id is unchanged; the
	// pointer form has its prefix stripped. This is the single
	// chokepoint — every work subcommand that accepts a work_id
	// goes through here before the substrate sees it.
	if rawID, ok := params["work_id"].(string); ok && rawID != "" {
		params["work_id"] = NormalizeWorkID(rawID)
	}

	payload := map[string]interface{}{"action": action, "params": params}
	enc, _ := json.Marshal(payload)
	return handleCall([]string{"mpm_work", "--payload", string(enc)})
}

// handleWorkItemPurge implements `mpm work item purge <work_id>
// --reason-code <enum> [--note <text>] [--backup <path>] [--force]`.
//
// Dry run is the default. Without --force nothing is written, and the
// output reports exactly what a forced run would remove, every inbound
// structural reference that would block it, and the locations where the
// content can still survive even after a successful purge.
//
// A --note is operator-authored independent input. It is copied into a
// permanent audit record and survives the purge — purge does not remove
// information you manually place in it.
func handleWorkItemPurge(params map[string]interface{}) int {
	dm := getDBConcrete()
	if dm == nil {
		return 1
	}
	workID, _ := params["work_id"].(string)
	if workID == "" {
		usererror.Error("mpm work item purge requires a work_id positional arg")
		return 1
	}
	reasonCode, _ := params["reason_code"].(string)
	if reasonCode == "" {
		usererror.Error("mpm work item purge requires --reason-code (%s)",
			strings.Join(mpminternal.ValidWorkPurgeReasonCodes, " | "))
		return 1
	}
	if !mpminternal.WorkPurgeReasonCodeIsValid(reasonCode) {
		usererror.Error("mpm work item purge: --reason-code %q is not valid (use %s)",
			reasonCode, strings.Join(mpminternal.ValidWorkPurgeReasonCodes, " | "))
		return 1
	}
	note, _ := params["note"].(string)
	backupPath, _ := params["backup"].(string)
	force, _ := params["force"].(bool)

	// --backup is taken BEFORE the delete, and only when the operator
	// asked for it. Purge creates no backup on its own — an implicit
	// copy of a purge target would be a copy of the material the
	// operator is trying to remove.
	//
	// The dump is a full copy of that same material, so it is written
	// only for a purge that will actually run. Taking it and then
	// refusing would leave a copy of the content on disk as a side
	// effect of a command that changed nothing. The core re-checks
	// referrers inside the transaction; this preflight only fixes the
	// ORDER of the two side effects, and a lost race is caught there.
	backupTaken := false
	if force && backupPath != "" {
		pre, err := dm.PreflightWorkPurge(dm, workID)
		if err != nil {
			return respond("", fmt.Sprintf("mpm work item purge: %v", err), 1)
		}
		if len(pre.Referrers) == 0 {
			if err := writePurgeBackup(dm, backupPath); err != nil {
				return respond("", fmt.Sprintf("mpm work item purge: backup failed, nothing was removed: %v", err), 1)
			}
			backupTaken = true
		}
	}

	report, err := dm.PurgeWork(mpminternal.WorkPurgeRequest{
		WorkID:     workID,
		ReasonCode: reasonCode,
		Note:       note,
		Operator:   getOrMakeSessionID(),
		Force:      force,
	})
	if err != nil {
		// A refusal enumerates its referrers. The operator has to be
		// able to see WHICH records block the purge without turning
		// to the database, because resolving them is the next thing
		// they will do.
		if errors.Is(err, mpminternal.ErrWorkPurgeReferenced) && report != nil {
			return respond("", renderWorkPurgeRefusal(workID, report, backupPath, backupTaken), 1)
		}
		return respond("", fmt.Sprintf("mpm work item purge: %v", err), 1)
	}

	return renderWorkPurgeReport(report, backupPath)
}

// renderWorkPurgeRefusal renders §6.2: the referring records, the fact
// that nothing changed, and the absence of a cascade path.
//
// A requested --backup that was not taken is stated rather than left
// silent. The operator asked for a copy of material they believe they
// are removing; where that copy is (or is not) is the whole subject of
// §5.7, and a refusal that quietly produced one would undercut it.
func renderWorkPurgeRefusal(workID string, report *mpminternal.WorkPurgeReport, backupPath string, backupTaken bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "mpm work item purge: %s is referenced by %d record(s):\n",
		workID, len(report.Referrers))
	for _, r := range report.Referrers {
		id := r.ID
		if id == "" {
			id = "-"
		}
		fmt.Fprintf(&b, "  %s: %s", r.Kind, id)
		if r.Detail != "" {
			fmt.Fprintf(&b, "  (%s)", r.Detail)
		}
		b.WriteString("\n")
	}
	b.WriteString("No changes were made. Resolve the references above and retry.\n")
	b.WriteString("MPM has no --cascade; purge never rewrites a referring record.\n")
	if backupPath != "" {
		if backupTaken {
			b.WriteString(fmt.Sprintf("NOTE: %s was written before the refusal and CONTAINS this work item. Purge does not delete backups.\n", backupPath))
		} else {
			b.WriteString("No backup was written: the purge was refused before any change.\n")
		}
	}
	return b.String()
}

// renderWorkPurgeReport renders the dry-run and forced-purge output
// through one path, so the two can never drift in what they disclose.
func renderWorkPurgeReport(report *mpminternal.WorkPurgeReport, backupPath string) int {
	var b strings.Builder
	if report.DryRun {
		render.Heading(&b, "Work item purge (dry run — nothing was written)")
	} else {
		render.Heading(&b, "Work item purged")
	}
	render.BlankLine(&b)
	render.Label(&b, "  "+report.WorkID, report.Title)
	render.Hint(&b, fmt.Sprintf("status: %s · reason: %s", report.Status, report.ReasonCode))
	render.BlankLine(&b)

	if report.DryRun {
		render.Section(&b, "Would remove")
	} else {
		render.Section(&b, "Removed")
	}
	render.Label(&b, "  work_events", fmt.Sprintf("%d", report.Counts.WorkEvents))
	render.Label(&b, "  evidence", fmt.Sprintf("%d", report.Counts.Evidence))
	render.Label(&b, "  artifact_provenance", fmt.Sprintf("%d", report.Counts.ArtifactProvenance))
	render.Label(&b, "  epistemic_provenance", fmt.Sprintf("%d", report.Counts.EpistemicProvenance))
	render.BlankLine(&b)

	if report.DryRun {
		render.Section(&b, "To actually purge, re-run with --force")
		render.BlankLine(&b)
	} else {
		render.Section(&b, "Audit")
		render.Label(&b, "  work_purge_audit", report.AuditID)
		render.BlankLine(&b)
	}

	// Mandatory disclosure on both paths. Purge's guarantee stops at
	// the SQLite logical layer, and an operator who believes otherwise
	// is worse off than one who was never offered the command.
	render.Section(&b, "Residual exposure — purge is NOT erasure")
	render.Plain(&b, "  MPM has no secure-erasure capability in v1. After this purge the content may still exist in:")
	for _, exposure := range report.ResidualExposure {
		render.Plain(&b, "    - "+exposure)
	}
	if backupPath != "" {
		render.BlankLine(&b)
		render.Section(&b, "Backup")
		render.Plain(&b, "  Wrote "+backupPath)
		render.Plain(&b, "  NOTE: that dump contains the purged material. Purge does not delete backups.")
	}
	return respond(b.String(), "", 0)
}

// writePurgeBackup dumps the database to outPath, before any delete.
// H-4: routed through the DatabaseManager singleton so the shared
// maintenance lease (LOCK_SH) covers the pre-delete flushWal +
// sqlite3 .dump.
//
// Refuses rather than silently skipping when the sqlite3 CLI is absent:
// a requested backup that quietly did not happen would leave the
// operator believing they have a copy they do not have.
func writePurgeBackup(dm *mpminternal.DatabaseManager, outPath string) error {
	if dm == nil {
		return fmt.Errorf("writePurgeBackup: nil DatabaseManager")
	}
	dbPath := dm.DBPath()
	if err := os.MkdirAll(filepath.Dir(outPath), 0o700); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	// H-4: flushWal runs against the singleton's *sql.DB so the
	// maintenance lease covers the checkpoint; a restore-db /
	// shred-database racing this flushWal will refuse (EWOULDBLOCK)
	// on the same lock inode.
	if err := flushWal(dm); err != nil {
		return fmt.Errorf("wal flush: %w", err)
	}
	sqlitePath, err := exec.LookPath("sqlite3")
	if err != nil {
		return fmt.Errorf("`sqlite3` CLI not found on PATH (see `mpm doctor`)")
	}
	dump, err := exec.Command(sqlitePath, dbPath, ".dump").Output()
	if err != nil {
		return fmt.Errorf("sqlite3 .dump: %w", err)
	}
	return os.WriteFile(outPath, dump, 0o600)
}

// handleWorkItemList renders the `mpm work item list` surface.
func handleWorkItemList(params map[string]interface{}) int {
	dm := getDBConcrete()
	if dm == nil {
		return 1
	}
	jsonOutput := false
	if v, ok := params["json"].(bool); ok {
		jsonOutput = v
	}
	status, _ := params["status"].(string)
	if status == "" {
		status = "open"
	}
	visibility, _ := params["visibility"].(string)
	if visibility == "" {
		visibility = string(mpminternal.WorkVisibilityActive)
	}
	limit := 0
	if v, ok := params["limit"]; ok {
		switch x := v.(type) {
		case float64:
			limit = int(x)
		case int:
			limit = x
		case int64:
			limit = int(x)
		}
	}
	rows, err := mpminternal.ListWorkRows(dm, status, visibility, limit)
	if err != nil {
		return respond("", fmt.Sprintf("work item list: %v", err), 1)
	}
	if jsonOutput {
		// JSON mode: preserve the canonical envelope shape
		// (success/works/count) and echo both axes so a scripted
		// caller can tell an empty result from a filtered one.
		out := map[string]interface{}{
			"success":    true,
			"works":      rows,
			"count":      len(rows),
			"status":     status,
			"visibility": visibility,
		}
		enc, _ := json.Marshal(out)
		fmt.Println(string(enc))
		return 0
	}
	// Human mode: canonical visual grammar.
	heading := "Work item list"
	if visibility == string(mpminternal.WorkVisibilityArchived) {
		heading = "Work item list (archived)"
	} else if visibility == string(mpminternal.WorkVisibilityAll) {
		heading = "Work item list (active + archived)"
	}
	render.Heading(os.Stdout, heading)
	render.BlankLine(os.Stdout)
	if len(rows) == 0 {
		scope := status
		if visibility != string(mpminternal.WorkVisibilityActive) {
			scope = fmt.Sprintf("%s, %s", status, visibility)
		}
		render.Plain(os.Stdout, fmt.Sprintf("No %s work items.", scope))
		return 0
	}
	for _, row := range rows {
		id, _ := row["id"].(string)
		title, _ := row["title"].(string)
		st, _ := row["status"].(string)
		verification, _ := row["verification"].(string)
		render.Label(os.Stdout, "  "+id, title)
		var hintParts []string
		if st != "" {
			hintParts = append(hintParts, "status: "+st)
		}
		if verification != "" {
			hintParts = append(hintParts, "verification: "+verification)
		}
		hintParts = append(hintParts, "created: "+mpminternal.FormatUnixSeconds(toInt64(row["created_at"])))
		render.Hint(os.Stdout, strings.Join(hintParts, " · "))
	}
	return 0
}

// toInt64 converts an int-shaped interface{} (JSON wire: float64)
// to int64. Returns 0 on type-mismatch / nil.
func toInt64(v interface{}) int64 {
	switch x := v.(type) {
	case float64:
		return int64(x)
	case int:
		return int64(x)
	case int64:
		return x
	case int32:
		return int64(x)
	}
	return 0
}

// parseWorkItemArgs extracts --status / --limit / --note / --content flags
// from rest and returns the remaining positional args. --note and --content
// can be supplied as --key=value or --key value.
// cmdLineHasFlag reports whether a flag token appears verbatim or
// in `--key=value` form anywhere in args. Round 9 T54b uses this to
// detect a positional + --title coexistence so the conflict notice
// can surface without rejecting the call.
func cmdLineHasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name || strings.HasPrefix(a, name+"=") {
			return true
		}
	}
	return false
}

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
		case a == "--visibility" && i+1 < len(rest):
			// Second, independent query axis (2026-09-30). Not derived
			// from --status: `done` + `archived` is a coherent query.
			params["visibility"] = rest[i+1]
			i += 2
		case strings.HasPrefix(a, "--visibility="):
			params["visibility"] = strings.TrimPrefix(a, "--visibility=")
			i++
		case a == "--reason-code" && i+1 < len(rest):
			// Purge justification. An enum, never free text — there is
			// deliberately no `--reason` on this path, because
			// resolve_contradiction already owns that name for a
			// different, non-destructive meaning.
			params["reason_code"] = rest[i+1]
			i += 2
		case strings.HasPrefix(a, "--reason-code="):
			params["reason_code"] = strings.TrimPrefix(a, "--reason-code=")
			i++
		case a == "--backup" && i+1 < len(rest):
			params["backup"] = rest[i+1]
			i += 2
		case strings.HasPrefix(a, "--backup="):
			params["backup"] = strings.TrimPrefix(a, "--backup=")
			i++
		case a == "--force":
			// Boolean. No value, so it is NOT in valueTakingFlags —
			// a `--force` at the end of the line has no argument to
			// swallow, and `--force=false` is not offered.
			params["force"] = true
			i++
		case a == "--json" || a == "-j":
			// 2026-09-14 release-pass: --json flag is honoured by
			// handleWorkItemList (list subcommand) for machine-readable
			// output. Other item subcommands ignore it (their JSON
			// path is via `mpm call mpm_work`).
			params["json"] = true
			i++
		case strings.HasPrefix(a, "--"):
			return nil, nil, fmt.Errorf("unknown flag %q (supported: --status, --visibility, --limit, --note, --content, --title, --reason, --json)", a)
		default:
			positional = append(positional, a)
			i++
		}
	}
	return params, positional, nil
}

// printWorkItemHelp prints the `mpm work item` subcommand help via
// the canonical visual grammar. 2026-09-14 release-pass: removes
// Round/T-number archaeology and the "discoverable facade" /
// "internally the substrate" implementation language.
func printWorkItemHelp() {
	render.Heading(os.Stdout, "Work item")
	render.BlankLine(os.Stdout)
	render.Section(os.Stdout, "Durable work-item CRUD (multi-session commitments)")
	render.Plain(os.Stdout, "Usage:")
	render.Plain(os.Stdout, "  mpm work item <subcommand> [args]")
	render.BlankLine(os.Stdout)
	render.Section(os.Stdout, "Subcommands")
	render.Label(os.Stdout, "create <title> [content]", "create a new work item (--title and positional are both supported; positional wins on conflict)")
	render.Label(os.Stdout, "list [--status <s>] [--visibility <v>] [--limit <n>]", "list work items (status: open|done|cancelled|all, default open; visibility: active|archived|all, default active)")
	render.Label(os.Stdout, "show <work_id>", "show a single work item by id")
	render.Label(os.Stdout, "complete <work_id> [--note <text>]", "mark a work item complete (records a completion event)")
	render.Label(os.Stdout, "cancel <work_id> [--note <text>]", "cancel a work item (locks verification below verified)")
	render.Label(os.Stdout, "history <work_id>", "show the full event ledger for a work item")
	render.Label(os.Stdout, "note <work_id> --note <text>", "append a free-form note to a work item's event ledger")
	render.Label(os.Stdout, "reopen <work_id>", "reopen a cancelled work item")
	render.Label(os.Stdout, "archive <work_id> [--note <text>]", "take a FINISHED (done or cancelled) item out of every default view. Open items are refused.")
	render.Label(os.Stdout, "unarchive <work_id> [--note <text>]", "return an archived item to the default views. Never reopens it.")
	render.Label(os.Stdout, "resolve-contradiction <work_id> --reason <text>", "withdraw unsubstantiated dispute evidence and re-derive verification. Audit-trail reason required.")
	render.Label(os.Stdout, "update <work_id> [--title <t>] [--content <c>] [--status <s>]", "update a work item's title/content/status")
	render.Label(os.Stdout, "purge <work_id> --reason-code <enum> [--note <text>] [--backup <path>] [--force]", "LOGICALLY DELETE a work item and its event ledger (dry run without --force). Not erasure — see below.")
	render.BlankLine(os.Stdout)
	render.Section(os.Stdout, "purge is logical deletion, NOT erasure")
	render.Plain(os.Stdout, "  --reason-code is required and must be one of:")
	render.Plain(os.Stdout, "    "+strings.Join(mpminternal.ValidWorkPurgeReasonCodes, " | "))
	render.Plain(os.Stdout, "  There is no 'privacy' code, because v1 purge provides no privacy-grade or")
	render.Plain(os.Stdout, "  forensic erasure. MPM has NO secure-erasure capability: after a purge the content")
	render.Plain(os.Stdout, "  can still exist in the WAL and rollback journal, in backups/critic-pre/")
	render.Plain(os.Stdout, "  (automatic scheduler snapshots, rotating at 7 — any snapshot taken before the")
	render.Plain(os.Stdout, "  purge keeps the content until it rotates out), in `mpm backup` dumps, and in")
	render.Plain(os.Stdout, "  filesystem snapshots outside MPM's reach.")
	render.Plain(os.Stdout, "  A purge refuses if another record structurally references the work item, and it")
	render.Plain(os.Stdout, "  enumerates the referrers. There is no --cascade: purge never rewrites a")
	render.Plain(os.Stdout, "  referring record. A prose mention of the id is NOT a reference.")
	render.Plain(os.Stdout, "  --note is YOUR OWN text, copied into a permanent audit record. It is not")
	render.Plain(os.Stdout, "  derived from the work item and survives the purge: purge does not remove")
	render.Plain(os.Stdout, "  information you manually place in it.")
	render.Plain(os.Stdout, "  --backup <path> writes a dump that CONTAINS the material being purged.")
	render.BlankLine(os.Stdout)
	render.Section(os.Stdout, "Status and visibility are separate filters")
	render.Plain(os.Stdout, "  --status is the lifecycle (open|done|cancelled|all); --visibility is the operational view")
	render.Plain(os.Stdout, "  (active|archived|all). Neither is derived from the other: --status done --visibility archived")
	render.Plain(os.Stdout, "  asks a question the old single-filter form could not express.")
	render.BlankLine(os.Stdout)
	render.Section(os.Stdout, "Examples")
	render.Plain(os.Stdout, "  mpm work item create \"ship parser fix\" \"introduce new lexer\"")
	render.Plain(os.Stdout, "  mpm work item list --status open --limit 10")
	render.Plain(os.Stdout, "  mpm work item complete work-abc123 --note \"shipped in commit def456\"")
	render.Plain(os.Stdout, "  mpm work item history work-abc123")
	render.Plain(os.Stdout, "  mpm work item archive work-abc123 --note \"shipped in alpha-final\"")
	render.Plain(os.Stdout, "  mpm work item list --status all --visibility archived")
	render.Plain(os.Stdout, "  mpm work item purge work-abc123 --reason-code test_debris")
	render.BlankLine(os.Stdout)
}

// printWorkHelp prints the `mpm work` subcommand help via the
// canonical visual grammar. 2026-09-14 release-pass: removes the
// "facade over mpm call mpm_work" / "internally the substrate's
// existing scratchpad APIs" implementation archaeology. The
// standalone-CLI session-identity section is preserved — it is
// genuinely useful operator guidance, not archaeology.
func printWorkHelp() {
	render.Heading(os.Stdout, "Work")
	render.BlankLine(os.Stdout)
	render.Section(os.Stdout, "Working Context (ephemeral execution state)")
	render.Plain(os.Stdout, "Usage:")
	render.Plain(os.Stdout, "  mpm work <subcommand> [args]")
	render.BlankLine(os.Stdout)
	render.Section(os.Stdout, "Subcommands")
	render.Label(os.Stdout, "status", "quick one-line status (session, age, expires)")
	render.Label(os.Stdout, "show", "show the raw working context, exactly as stored")
	render.Label(os.Stdout, "clear", "discard the current working context")
	render.Label(os.Stdout, "promote", "promote the working context to a permanent memory")
	render.Label(os.Stdout, "item", "durable work-item CRUD (create/list/show/complete/cancel/history/note/reopen/archive/unarchive/purge/update)")
	render.BlankLine(os.Stdout)
	render.Section(os.Stdout, "Flags")
	render.Label(os.Stdout, "--session-id <id>", "override the per-process session id (default: a fresh random id per invocation)")
	render.Label(os.Stdout, "--json", "emit machine-readable JSON (where applicable)")
	render.BlankLine(os.Stdout)
	render.Section(os.Stdout, "Session identity (standalone CLI)")
	render.Plain(os.Stdout, "  Every process mints a fresh random session id unless you pin one.")
	render.Plain(os.Stdout, "  Multi-command working-context workflows must reuse the same identity.")
	render.Plain(os.Stdout, "  Export MPM_SESSION_ID once per shell, or pass --session-id per invocation:")
	render.BlankLine(os.Stdout)
	render.Plain(os.Stdout, "    export MPM_SESSION_ID=my-task-1")
	render.Plain(os.Stdout, "    mpm work status     # same session every time now")
	render.BlankLine(os.Stdout)
	render.Hint(os.Stdout, "Without pinning, status/show in separate invocations see different (empty) sessions — standalone processes have no daemon to keep shell sessions alive.")
}

// flag was unused in the Wave 1 minimum; kept stubbed so future
// subcommands that need real flag parsing have an obvious hook.
// (Today's uses use workCommonFlags which scans rest string-by-string.)
var _ = flag.NewFlagSet
