// cmd/mpm/handlers_cognitive_verbs.go — Wave 3 cognitive-verb aliases.
//
// The two-audience principle (RFC §'two-audience') applies here:
//
//   HUMANS want noun-verb commands that read as cognition:
//     mpm remember     create a memory
//     mpm learn        create a lesson
//     mpm decide       record a decision
//     mpm theorize     propose a theory
//
//   AGENTS use the consolidated mpm_* aggregator tool contracts:
//     mpm call mpm_memory '{"action":"save",...}'
//     mpm call mpm_lessons '{"action":"save",...}'
//     mpm call mpm_decisions '{"action":"save","params":{"context":...}}'
//     mpm call mpm_theories '{"action":"propose","params":{"hypothesis":...}}'
//     (the substrate aggregator names are stable; the actions inside
//      each aggregator are the dispatch surface, kept narrow on purpose)
//
// Wave 3 ships the human-facing cognitive verbs as THIN ALIASES —
// every cognitive verb routes to an existing handler. No behaviour
// changes. No new storage. No new service / store / renderer needed.
//
// Each handler is intentionally tiny (1-3 lines of dispatch).
// Composition discipline: a cognitive-verb alias is a command,
// it composes an existing command. It is NOT a service. It does
// NOT duplicate logic; it does NOT query SQLite. The "two audience"
// principle holds.

package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/usererror"
)

// handleRemember is the cognitive-verb alias for mpm add.
// Memory CRUD, just routed under a verb an operator can guess.
// `mpm remember <content>` ≡ `mpm add <content>`.
func handleRemember(args []string) int {
	return handleAdd(args)
}

// handleLearn is the cognitive-verb alias for mpm kb lesson add
// (the lesson entity is the "curated insight" substrate; learning
// is the operator-facing verb for that substrate).
//
// `mpm learn <content>` ≡ `mpm lesson add <content>`.
// Passes "add" as the subcommand so handleLesson's switch dispatches
// to handleLessonAdd with the remaining args.
func handleLearn(args []string) int {
	allArgs := append([]string{"add"}, args...)
	return handleLesson(allArgs)
}

// handleDecide is the cognitive-verb alias for record_decision.
// `mpm decide <flags>` ≡ `mpm record_decision <flags>`.
//
// The substrate tool is record_decision per the MCP contract
// stability principle (RFC §1); the cognitive verb here is just the
// operator-facing entry point.
func handleDecide(args []string) int {
	return handleRecordDecision(args)
}

// handleTheorize is the cognitive-verb alias for propose_theory.
// `mpm theorize <hypothesis>` ≡ `mpm propose_theory <hypothesis>`.
func handleTheorize(args []string) int {
	return handleProposeTheory(args)
}

// handleDecision routes the top-level `mpm decision <sub>` family.
// The substrate has record_decision, supersede, invalidate, show, list,
// query — the CLI front door mirrors that vocabulary:
//
//   mpm decision add         → record_decision
//   mpm decision supersede   → supersede an existing decision with a replacement
//   mpm decision invalidate  → retire a decision without a replacement
//   mpm decision show        → fetch a single decision by id (mirrors mpm call mpm_decisions show)
//   mpm decision resolve     → (deprecated 2026-09-10) use 'supersede' instead
//   mpm decision list        → list decisions (mirrors mpm decisions)
//   mpm decision search      → keyword search (F-A2)
//
// The `supersede` and `invalidate` subcommands route through the
// substrate's mpm_decisions tool, which is the canonical source of
// truth for the decision lifecycle — the CLI is a thin shim that
// builds the payload and delegates. Field vocabulary is canonical
// snake_case: `id` for the decision being acted upon, `choice` /
// `context` / `rationale` / `tags` for the replacement (supersede),
// `reason` for invalidate.
//
// Note: `mpm decisions` (plural) is the ledger-listing command and
// remains on its own dispatch — singular `decision` introduces the
// subcommand vocabulary without colliding.
func handleDecision(args []string) int {
	if len(args) == 0 {
		printDecisionHelp()
		return 0
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "add":
		return handleRecordDecision(rest)
	case "supersede":
		// 2026-09-10 cleanup: expose the substrate's mpm_decisions
		// supersede action as a first-class CLI subcommand. Routes
		// through `mpm call mpm_decisions` so the CLI and tool share
		// one implementation, one envelope, one field vocabulary.
		return handleDecisionSupersede(rest)
	case "invalidate":
		// 2026-09-10 cleanup: same pattern as supersede. The CLI
		// previously had no surface for invalidate; only the tool
		// did. Mirrors `mpm call mpm_decisions --payload
		// '{"action":"invalidate","params":{"id":"<id>","reason":"<text>"}}'`.
		return handleDecisionInvalidate(rest)
	case "show":
		// W-003: parity with `mpm call mpm_decisions show`. Routes through
		// handleDecisionsShow so the CLI and MCP surfaces share the same
		// row-construction code (and the same JSON envelope).
		dm := getDB()
		if dm == nil {
			return 1
		}
		return handleDecisionsShow(dm, rest)
	case "resolve":
		// 2026-09-10 cleanup: `resolve` was a guidance-only stub
		// (printed "decision resolution is not yet a substrate
		// primitive" and returned 0). The substrate actually has
		// TWO distinct resolution paths — supersede (with replacement)
		// and invalidate (retire). Point operators at the right one
		// instead of leaving the stub in place.
		usererror.Warn("`mpm decision resolve` is deprecated — use `mpm decision supersede <id>` (record a replacement) or `mpm decision invalidate <id>` (retire without replacement)")
		return 0
	case "list", "ls", "all":
		// Singular `mpm decision list` ≡ plural `mpm decisions`. The
		// list-style verbs route through the plural handler so the
		// ledger surface has one implementation and one help string.
		return handleDecisions(rest)
	case "search":
		// F-A2: discoverability. Search decisions by keyword via FTS5
		// against the decisions collection.
		return handleDecisionSearch(rest)
	case "help", "-h", "--help":
		printDecisionHelp()
		return 0
	default:
		usererror.Error("mpm decision: unknown subcommand %q\n  available subcommands: add, supersede, invalidate, show, list, search", sub)
		return 1
	}
}

// handleTheory routes the top-level `mpm theory <sub>` family.
// `mpm theory add` ≡ `mpm propose_theory`; `mpm theory resolve`
// ≡ `mpm resolve_theory`; `mpm theory show <id>` ≡ `mpm call mpm_theories show`.
// Plural `mpm theories` (legacy command for listing) remains on its own dispatch.
func handleTheory(args []string) int {
	if len(args) == 0 {
		printTheoryHelp()
		return 0
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "add":
		return handleProposeTheory(rest)
	case "resolve":
		return handleResolveTheory(rest)
	case "show":
		// W-003: parity with `mpm call mpm_theories show`. The MCP
		// surface gained `show` after the alpha-4 D-006 fix; the CLI
		// had no equivalent. Routes through dm.GetTheory directly so
		// the singular `mpm theory show <id>` and the MCP `mpm call
		// mpm_theories show` read from the same code path.
		dm := getDB()
		if dm == nil {
			return 1
		}
		if len(rest) == 0 {
			return respond("", "Usage: mpm theory show <id>\n", 1)
		}
		row, err := dm.GetTheory(rest[0])
		if err != nil {
			return respond("", fmt.Sprintf("Error: %v\n", err), 1)
		}
		out, _ := json.MarshalIndent(row, "", "  ")
		fmt.Println(string(out))
		return 0
	case "list", "ls", "all", "pending", "resolved", "proven", "disproven":
		// Singular `mpm theory list` ≡ plural `mpm theories [filter]`.
		// Routes through the plural handler so filter vocabulary is
		// shared and the listing surface has one implementation.
		return handleTheories(args)
	case "search":
		// F-A2 parity: theory search by keyword, matching the
		// decision/skill/lesson search surfaces.
		return handleTheorySearch(rest)
	case "help", "-h", "--help":
		printTheoryHelp()
		return 0
	default:
		usererror.Error("mpm theory: unknown subcommand %q\n  available subcommands: add, resolve, show, list, pending, resolved, proven, disproven, search", sub)
		return 1
	}
}

// handleSkill routes the top-level `mpm skill <sub>` family.
// The substrate has save-skill (--file <path> | --stdin), list-skills,
// and read-skill. Wave 3 adds cognitive-verb subcommands:
//
//   mpm skill add      ≡ mpm save-skill --file <path>
//   mpm skill list     ≡ mpm list-skills
//   mpm skill show     ≡ mpm read-skill <name>
//   mpm skill search   ≡ mpm search-references (best-effort; if no
//                        standalone skill search exists, the
//                        command prints a guidance message)
//
// The internal MCP tool names (save_skill, list_skills, read_skill)
// are stable per RFC §1. The cognitive-verb surface here is the
// operator-facing front door.
func handleSkill(args []string) int {
	if len(args) == 0 {
		printSkillHelp()
		return 0
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "add":
		return handleSaveSkill(rest)
	case "save":
		// `mpm skill save --file <path>` is the legacy alias for
		// `mpm skill add` (documented in handleSaveSkillHelp). The
		// R9-T52 regression test pins that `mpm skill save --help`
		// surfaces the save-skill help page; routing it through
		// handleSaveSkill preserves the contract and the
		// documented discoverability.
		return handleSaveSkill(rest)
	case "list":
		return handleListSkills(rest)
	case "show":
		return handleReadSkill(rest)
	case "search":
		// F-F1: skill search by keyword. Replaces the Wave-3
		// "not yet wired" guidance with a real QueryMemory path
		// against collection='skills'.
		return handleSkillSearch(rest)
	case "workshop":
		return handleSkillWorkshop(rest)
	case "delete":
		// Final release-pass: skill lifecycle completeness. Soft
		// delete — recoverable via `mpm skill add --force` (which
		// resurrects the soft-deleted row with the same id).
		return handleSkillDelete(rest)
	case "shred":
		// Final release-pass: permanent deletion. Unlike delete,
		// shred is not recoverable; the row is removed with cascade
		// cleanup of dependent topic_memberships and confidence_history.
		return handleSkillShred(rest)
	case "help", "-h", "--help":
		printSkillHelp()
		return 0
	default:
		usererror.Error("mpm skill: unknown subcommand %q\n  available subcommands: add, list, show, search, workshop", sub)
		return 1
	}
}

// printDecisionHelp prints the mpm decision help block.
func printDecisionHelp() {
	usererror.Notice(`mpm decision — Decision ledger

Subcommands:
  add         Record a new decision (alias for record_decision / mpm decide)
  supersede   Supersede an existing decision with a replacement (canonical field: id)
  invalidate  Retire a decision without a replacement (canonical field: id, reason)
  show        Show a single decision by id (mirrors mpm call mpm_decisions show)
  list        List all decisions (alias for "mpm decisions")
  search      Search decisions by keyword

Examples:
  mpm decision add context="..." choice="..." rationale="..."
  mpm decision supersede <decision-id> --choice "new choice" --context "new context" --rationale "why"
  mpm decision invalidate <decision-id> --reason "context changed"
  mpm decision show <decision-id>
  mpm decision list
  mpm decide context="..." choice="..." rationale="..."
  mpm call mpm_decisions --payload '{"action":"record","params":{"context":"...","choice":"...","rationale":"..."}}'
  mpm call mpm_decisions --payload '{"action":"supersede","params":{"id":"<decision-id>","choice":"new choice"}}'
  mpm call mpm_decisions --payload '{"action":"invalidate","params":{"id":"<decision-id>","reason":"context changed"}}'`)
}

// handleDecisionSupersede (2026-09-10 cleanup) is the CLI front door
// for the mpm_decisions supersede action. It accepts flag-style args
// (--choice / --context / --rationale / --tags) and the decision id
// as a positional, then delegates to the canonical substrate call.
//
// Routes through dm.SupersedeDecision (the same call `mpm decision add
// --supersedes <id>` makes) so the CLI surface and the tool surface
// share one implementation, one audit trail, one supersedes-link
// column. The CLI flag form is purely a syntactic convenience over the
// substrate payload — no second implementation path.
func handleDecisionSupersede(args []string) int {
	jsonOutput := false
	filtered := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--json" {
			jsonOutput = true
			continue
		}
		filtered = append(filtered, args[i])
	}
	args = filtered

	if len(args) == 0 {
		return respond("", "Usage: mpm decision supersede <decision-id> [--choice <text>] [--context <text>] [--rationale <text>] [--tags <csv>] [--json]\n", 1)
	}
	id := args[0]
	if strings.TrimSpace(id) == "" {
		return respond("", "Usage: mpm decision supersede <decision-id> ...\n", 1)
	}
	rest := args[1:]

	choice, _ := extractFlag(rest, "--choice")
	contextText, _ := extractFlag(rest, "--context")
	rationale, _ := extractFlag(rest, "--rationale")
	tagsStr, _ := extractFlag(rest, "--tags")
	if strings.TrimSpace(choice) == "" {
		return respond("", "Error: --choice is required (the replacement decision)\n", 1)
	}

	var tags []string
	if tagsStr != "" {
		for _, t := range strings.Split(tagsStr, ",") {
			t = strings.TrimSpace(t)
			if t != "" {
				tags = append(tags, t)
			}
		}
	}

	dm := getDBConcrete()
	if dm == nil {
		return respond("", "Error: database not available\n", 1)
	}

	res, err := dm.SupersedeDecision(id, contextText, choice, rationale, "", tags, nil, internal.ActiveContext{})
	if err != nil {
		if jsonOutput {
			out, _ := json.Marshal(map[string]interface{}{"success": false, "id": id, "error": err.Error()})
			return respond("", string(out)+"\n", 1)
		}
		return respond("", fmt.Sprintf("Failed to supersede decision: %v\n", err), 1)
	}
	newID, _ := res["id"].(string)
	if jsonOutput {
		out, _ := json.Marshal(map[string]interface{}{
			"success":    true,
			"id":         newID,
			"supersedes": id,
			"action":     "supersede",
		})
		return respond("", string(out)+"\n", 0)
	}
	return respond("", fmt.Sprintf("Superseded %s with %s\n", id, newID), 0)
}

// handleDecisionInvalidate (2026-09-10 cleanup) is the CLI front door
// for the mpm_decisions invalidate action. Accepts the decision id
// as a positional arg and an optional --reason flag, then delegates
// to dm.InvalidateDecision. Same substrate call as the tool, same
// audit row.
func handleDecisionInvalidate(args []string) int {
	jsonOutput := false
	filtered := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--json" {
			jsonOutput = true
			continue
		}
		filtered = append(filtered, args[i])
	}
	args = filtered

	if len(args) == 0 {
		return respond("", "Usage: mpm decision invalidate <decision-id> [--reason <text>] [--json]\n", 1)
	}
	id := args[0]
	if strings.TrimSpace(id) == "" {
		return respond("", "Usage: mpm decision invalidate <decision-id> ...\n", 1)
	}
	rest := args[1:]
	reason, _ := extractFlag(rest, "--reason")

	dm := getDBConcrete()
	if dm == nil {
		return respond("", "Error: database not available\n", 1)
	}

	if _, err := dm.InvalidateDecision(id, reason); err != nil {
		if jsonOutput {
			out, _ := json.Marshal(map[string]interface{}{"success": false, "id": id, "error": err.Error()})
			return respond("", string(out)+"\n", 1)
		}
		return respond("", fmt.Sprintf("Failed to invalidate decision: %v\n", err), 1)
	}
	if jsonOutput {
		out, _ := json.Marshal(map[string]interface{}{"success": true, "id": id, "action": "invalidate"})
		return respond("", string(out)+"\n", 0)
	}
	return respond("", fmt.Sprintf("Invalidated decision %s\n", id), 0)
}

// extractFlag returns the value following the named flag, or "" if the
// flag is absent. Helper for the 2026-09-10 supersede/invalidate CLI
// surface so they don't duplicate extractFlagValue's parsing logic.
func extractFlag(args []string, name string) (string, bool) {
	for i := 0; i < len(args); i++ {
		if args[i] == name {
			if i+1 < len(args) {
				return args[i+1], true
			}
			return "", true
		}
	}
	return "", false
}

// printTheoryHelp prints the mpm theory help block.
func printTheoryHelp() {
	usererror.Notice(`mpm theory — Theory tracker

Subcommands:
  add      Propose a new theory (alias for propose_theory / mpm theorize)
  resolve  Resolve an existing theory (alias for resolve_theory)
  show     Show a single theory by id (mirrors mpm call mpm_theories show)
  list     List theories (alias for "mpm theories [filter]")
  pending  List pending theories only
  resolved List resolved theories (proven + disproven)
  proven   List proven theories only
  disproven List disproven theories only
  search   Search theories by keyword

Resolution vocabulary (canonical conclusion keywords):
  Proven outcome (status="proven"):     confirmed, proven
  Disproven outcome (status="disproven"): disproven, refuted, invalidated

Examples:
  mpm theory add hypothesis_id=... validation="..."
  mpm theory resolve <hypothesis_id> confirmed
  mpm theory resolve <hypothesis_id> refuted --note "see lesson #abc"
  mpm theory show <theory-id>
  mpm theory list
  mpm theory pending
  mpm theorize hypothesis_id=... validation="..."`)
}

// printSkillHelp prints the mpm skill help block.
func printSkillHelp() {
	usererror.Notice(`mpm skill — Skill library

Subcommands:
  add       Save a skill from a markdown file (alias for save-skill --file <path>)
  list      List skills (alias for list-skills)
  show      Read a skill by name (alias for read-skill)
  search    Skill search (reserved — use mpm call search_references today)
  workshop  Run the skill workshop (form | refine) — guided skill-formation workflow.
            Run "mpm skill workshop --help" for the full schema, boundary vocabulary,
            change_type options, and JSON examples.
  delete    Soft-delete a skill (recoverable via add --force)
  shred     Permanently delete a skill (not recoverable)

Examples:
  mpm skill add --file path/to/SKILL.md
  mpm skill list
  mpm skill show agentshell
  mpm skill delete skill:agentshell-v1.0.0
  mpm skill shred skill:agentshell-v1.0.0
  mpm skill workshop --file request.json`)
}

// printRememberHelp prints the mpm remember help block.
// W-002: previously `mpm help remember` returned "no help available".
func printRememberHelp() {
	usererror.Notice(`mpm remember — Save a memory

Cognitive-verb alias for the underlying ` + "`mpm add`" + ` command. Use this
when you want the verb to read as cognition rather than CRUD.

Usage:
  mpm remember <content>           [tags=...] [--json]
  mpm remember -   (read content from stdin)
  mpm remember <content> --weight N
  mpm remember <content> --tags tag1,tag2

Examples:
  mpm remember "SQLite uses btree pages by default"
  mpm remember "decision ratified" --tags meeting,ratified --weight 9
  mpm call mpm_memory --payload '{"action":"save","params":{"fact":"..."}}'`)
}

// printLearnHelp prints the mpm learn help block.
// W-002: previously `mpm help learn` returned "no help available".
func printLearnHelp() {
	usererror.Notice(`mpm learn — Record a lesson

Cognitive-verb alias for ` + "`mpm lesson add`" + `. Lessons are durable
insights derived from experience; use them for warnings and practices
that should surface in future relevant contexts.

Usage:
  mpm learn <content>              [--type=insight|warning|practice]
  mpm learn <content> --tags ...   [--json]

Examples:
  mpm learn "Always run ` + "`go vet -tags fts5`" + ` before committing" --type warning
  mpm call mpm_lessons --payload '{"action":"save","params":{"fact":"...","type":"warning"}}'`)
}

// printDecideHelp prints the mpm decide / mpm record_decision help block.
// W-002: previously `mpm help decide` returned "no help available".
func printDecideHelp() {
	usererror.Notice(`mpm decide — Record a decision

Cognitive-verb alias for the underlying record_decision primitive.
A decision is an architectural choice with context, choice, and rationale.

Usage (flag form):
  mpm decide --choice "<text>" --context "<text>" --rationale "<text>"
             [--tags csv] [--supersedes <id>] [--weight N] [--json]

Usage (legacy token form):
  mpm decide "CHOICE: <text>
              CONTEXT: <text>
              RATIONALE: <text>"

Examples:
  mpm decide --choice "Use SQLite WAL" --context "concurrent reads" --rationale "WAL > DELETE"
  mpm call mpm_decisions --payload '{"action":"record","params":{"choice":"...","context":"...","rationale":"..."}}'`)
}

// printTheorizeHelp prints the mpm theorize / mpm propose_theory help block.
// W-002: previously `mpm help theorize` returned "no help available".
func printTheorizeHelp() {
	usererror.Notice(`mpm theorize — Propose a theory

Cognitive-verb alias for the underlying propose_theory primitive.
A theory is a testable hypothesis with explicit validation criteria.

Usage (all forms produce the same canonical record):
  mpm theorize --hypothesis "<text>" --validation "<criteria>" [--tags csv] [--json]
  mpm theorize hypothesis="<text>" validation="<criteria>" [--json]
  mpm theorize --hypothesis "<text>" --validation "<criteria>" | <more args>
  mpm theorize "HYPOTHESIS: <text>
                VALIDATION_CRITERIA: <criteria>"

Examples:
  mpm theorize --hypothesis "wal is faster than delete-journal" \
               --validation "throughput on 4 concurrent readers"
  mpm theorize hypothesis="wal is faster" validation="throughput on 4 readers"
  mpm call mpm_theories --payload '{"action":"propose","params":{"hypothesis":"...","validation_criteria":"..."}}'`)
}

// printResolveTheoryHelp prints the mpm resolve_theory help block.
// W-002: previously `mpm help resolve_theory` returned "no help available".
//
// T43 round 9: align the help text with the canonical conclusion
// vocabulary. The parser at handlers_epistemology.go acceptConclusionEnum
// accepts five values that all map to one of two stored statuses
// (canonical list: `confirmed|proven` → status="proven";
// `disproven|refuted|invalidated` → status="disproven"). The pre-fix
// help only documented two of these, leaving the other three as
// hidden-aliases. Smoke tests that used the documented-shortcut list
// (or canonical-only) would silently alias the un-documented
// keywords; smoke tests that referenced the underlying state
// machine directly would see inconsistent vocabulary. The help now
// lists all five.
func printResolveTheoryHelp() {
	usererror.Notice(`mpm resolve_theory — Resolve a theory

Usage:
  mpm resolve_theory <hypothesis_id> <conclusion> [--winner=<memory_id>] [--json]

Conclusion (canonical vocabulary; aliases that map to the same
stored status are documented for muscle memory):

  Proven outcome (status="proven"):
    confirmed, proven

  Disproven outcome (status="disproven"):
    disproven, refuted, invalidated

The legacy stored value "resolved" (from pre-2026-09-10 writes) is
honored as a read-only value on existing rows but is no longer
written by this handler — see handlers_epistemology.go acceptConclusionEnum.

Examples:
  mpm resolve_theory wal-better confirmed
  mpm resolve_theory wal-better disproven
  mpm resolve_theory wal-better refuted --note "latency regression"
  mpm resolve_theory wal-better invalidated --note "supersedes #abc123"
  mpm resolve_theory wal-better proven --json
  mpm resolve_theory wal-better --winner=abc123 confirmed  (arbitration slash)`)
}
