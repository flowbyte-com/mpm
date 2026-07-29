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
//   AGENTS use stable verb-noun MCP contracts:
//     mpm call save_to_memory / save_lesson / record_decision / propose_theory
//     (the substrate tool names never change)
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
// The substrate has record_decision but no decision-resolve (the
// analog of theory-resolve). So:
//
//   mpm decision add       → record_decision
//   mpm decision resolve   → print a friendly message pointing at
//                            the (currently absent) resolution path;
//                            no decision-resolve exists yet, so this
//                            is currently a stub. Future substrate
//                            work would add the missing primitive.
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
	case "resolve":
		usererror.Warn("decision resolution is not yet a substrate primitive — for now, record the alternative as another `mpm decide` call rather than superseding")
		return 0
	case "help", "-h", "--help":
		printDecisionHelp()
		return 0
	default:
		usererror.Error("mpm decision: unknown subcommand %q\n  available subcommands: add, resolve (reserved)", sub)
		return 1
	}
}

// handleTheory routes the top-level `mpm theory <sub>` family.
// `mpm theory add` ≡ `mpm propose_theory`; `mpm theory resolve`
// ≡ `mpm resolve_theory`. Plural `mpm theories` (legacy command for
// listing) remains on its own dispatch.
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
	case "help", "-h", "--help":
		printTheoryHelp()
		return 0
	default:
		usererror.Error("mpm theory: unknown subcommand %q\n  available subcommands: add, resolve", sub)
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
	case "list":
		return handleListSkills(rest)
	case "show":
		return handleReadSkill(rest)
	case "search":
		// Best-effort: substrate doesn't have a dedicated skill
		// search. For Wave 3 we surface guidance so operators see
		// the right verb. A future commit can add mpm call
		// search_skills or wire one here.
		usererror.Notice("skill search by keyword not yet wired — use `mpm call search_references` (skill refs live alongside) or `mpm skill list` to enumerate")
		return 0
	case "help", "-h", "--help":
		printSkillHelp()
		return 0
	default:
		usererror.Error("mpm skill: unknown subcommand %q\n  available subcommands: add, list, show, search", sub)
		return 1
	}
}

// printDecisionHelp prints the mpm decision help block.
func printDecisionHelp() {
	usererror.Notice(`mpm decision — Decision ledger

Subcommands:
  add      Record a new decision (alias for record_decision / mpm decide)
  resolve  Reserved for future decision-resolution primitive

Examples:
  mpm decision add context="..." choice="..." rationale="..."
  mpm decide context="..." choice="..." rationale="..."
  mpm call record_decision --payload '{"context":"...","choice":"...","rationale":"..."}'`)
}

// printTheoryHelp prints the mpm theory help block.
func printTheoryHelp() {
	usererror.Notice(`mpm theory — Theory tracker

Subcommands:
  add      Propose a new theory (alias for propose_theory / mpm theorize)
  resolve  Resolve an existing theory (alias for resolve_theory)

Examples:
  mpm theory add hypothesis_id=... validation="..."
  mpm theory resolve <hypothesis_id> confirmed
  mpm theorize hypothesis_id=... validation="..."`)
}

// printSkillHelp prints the mpm skill help block.
func printSkillHelp() {
	usererror.Notice(`mpm skill — Skill library

Subcommands:
  add    Save a skill from a markdown file (alias for save-skill --file <path>)
  list   List skills (alias for list-skills)
  show   Read a skill by name (alias for read-skill)
  search Skill search (reserved — use mpm call search_references today)

Examples:
  mpm skill add --file path/to/SKILL.md
  mpm skill list
  mpm skill show agentshell`)
}
