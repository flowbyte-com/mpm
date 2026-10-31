// cli_options_registry.go — the router's option vocabulary.
//
// WHY THIS EXISTS
// ---------------
// The CLI's contract is that an option the parser does not understand
// is a MISTAKE, and a mistake must never be treated as data.
//
// The dangerous cases were the commands that read their arguments
// POSITIONALLY:
//
//	mpm decide --anything     recorded a decision whose choice was "--anything"
//	mpm theorize --anything   proposed a theory named "--anything"
//	mpm gc --anything         ran the decay sweep
//	mpm backup --anything     wrote a SQL dump to a file named "--anything"
//	mpm synthesize --anything synthesised over the operator's memories
//
// Go's flag package already rejects an unknown flag for every command
// that uses a FlagSet. The gap was the commands that scan for known
// spellings by hand — and for those, the ROUTER is the only place
// that sees the raw argv before a handler can mistake an option for
// data.
//
// So the router consults this registry before dispatch.
//
// THE DIRECTION OF THE ERROR
// --------------------------
// An entry that lists an option the command does NOT support is the
// SAFE direction: the router passes the token on, and the command's
// own handler rejects it precisely — or ignores it, exactly as it
// did before this registry existed.
//
// An entry that MISSES an option the command DOES support is the
// dangerous direction: a valid invocation is rejected.
//
// The registry is therefore deliberately generous, and it is pinned by
// TestCLIOptionRegistryCoversEverySourceVocabulary, which derives the
// vocabulary from the source and fails when the registry drifts.
package main

import (
	"fmt"
	"sort"
	"strings"

	"github.com/flowbyte-com/mpm-core/usererror"
)

// cliOptionRegistry maps a command to the option names it accepts.
//
// The names carry NO dashes: "json" covers "--json" and "-json",
// because Go's flag package accepts either spelling and the router's
// scan must too.
//
// A command that is ABSENT from the map accepts no options at all: an
// option-looking token is then always a mistake. A command that is
// PRESENT with an empty list likewise accepts none.
//
// The map is built in init() rather than declared as a literal, because
// the FAMILIES' entries are the union of every other entry: a family
// dispatches its leaves below the router, so the router cannot know
// which leaf a token is meant for, and it must not reject a token the
// leaf accepts. Deriving the union instead of hand-maintaining it
// removes a whole class of drift — the hand-maintained version broke
// `mpm ops review --stale`, because "stale" belonged to the review
// leaf's vocabulary and not to the family's.
var cliOptionRegistry map[string][]string

// cliOptionFamilies are the commands whose leaves are dispatched BELOW
// the router.
//
// The router cannot tell which leaf an option is meant for, so a family
// accepts the union of the whole CLI's vocabulary and the LEAF rejects
// what it does not support. The leaves that carry a precise guard are
// doctor, review, decide/record_decision, theorize/propose_theory, gc,
// backup and synthesize.
var cliOptionFamilies = []string{
	"ops", "memory", "kb", "lesson", "work", "config", "topic", "session",
	"capability", "debug", "provenance", "drills", "compact", "tasks",
	"evidence", "mode", "persona", "switch", "directives", "self-heal",
	"skill",
}

func init() {
	registry := map[string][]string{
		// ── Information and diagnostics ────────────────────────────────
		"version":     {"check", "json"},
		"help":        {"all"},
		"doctor":      {"json", "j", "explain", "deep-scan", "fix", "all"},
		"status":      {},
		"info":        {"user", "value"},
		"wake":        {"compact", "strict"},
		"why":         {"kind"},
		"lint":        {"dir", "show-clean", "strict"},
		"integration": {},

		// ── Knowledge ──────────────────────────────────────────────────
		//
		// These commands live in simple_cmds.go, whose vocabulary is
		// shared across them.
		"add":          simpleCommandOptions,
		"remember":     simpleCommandOptions,
		"ls":           simpleCommandOptions,
		"show":         simpleCommandOptions,
		"rm":           simpleCommandOptions,
		"promote":      simpleCommandOptions,
		"patch-memory": simpleCommandOptions,
		"reinforce":    simpleCommandOptions,
		"weaken":       simpleCommandOptions,
		"snooze":       simpleCommandOptions,
		"set-weight":   simpleCommandOptions,
		"reference":    simpleCommandOptions,
		"stats":        {},
		"prune":        maintenanceOptionNames,
		"export":       maintenanceOptionNames,
		"maintain":     maintenanceOptionNames,
		"synthesize":   {"dry-run", "dryrun"},
		"shred":        {},
		"audit":        auditOptionNames,

		// ── The review workflow ────────────────────────────────────────
		"review": {"stale", "promoted", "days", "limit", "ack", "ack-all", "decision", "note", "json", "j"},

		// ── Cognitive verbs ────────────────────────────────────────────
		"decide":   cognitiveVerbOptions,
		"theorize": cognitiveVerbOptions,
		"decision": cognitiveVerbOptions,
		"theory":   cognitiveVerbOptions,
		"skill":    cognitiveVerbOptions,
		"learn":    cognitiveVerbOptions,

		// ── The commands that carry their own vocabulary ──────────────
		//
		// Every entry below was ABSENT from this map when the router
		// guard was first switched on, which meant the guard rejected
		// EVERY option for it: `mpm gc --max-age=24` and
		// `mpm backup --output` stopped working the moment the guard
		// was introduced.
		//
		// The AST-derived coverage test
		// (TestCLIOptionRegistryCoversEverySourceVocabulary) is what
		// found it, and it is what keeps it fixed.
		"backup":          {},
		"blob":            {"dry-run", "n"},
		"cascade":         {"max-iterations", "poll-interval", "once"},
		"challenge":       {"json", "j", "limit"},
		"continue":        {"session-id"},
		"decisions":       cognitiveVerbOptions,
		"gc":              {"json", "j", "dry-run", "aggressive", "review", "purge", "shred-negative", "max-age"},
		"handoff":         {"json", "j", "session-id", "summary", "tag", "unread"},
		"hint":            {"json", "max"},
		"ingest":          ingestOptionNames,
		"lifecycle":       {},
		"list-skills":     skillOptionNames,
		"migrate":         migrateOptionNames,
		"onboard":         onboardOptionNames,
		"propose_theory":  cognitiveVerbOptions,
		"read-skill":      skillOptionNames,
		"recall":          recallOptionNames,
		"record_decision": cognitiveVerbOptions,
		"resolve_theory":  cognitiveVerbOptions,
		"restore":         {},
		"restore-db":      {},
		"route":           {"apply"},
		"save-skill":      skillOptionNames,
		"theories":        cognitiveVerbOptions,
		"tour":            {"demo", "step"},
		"update":          {"to", "branch", "commit", "from", "check", "status", "dry-run", "rollback", "with-db-restore", "allow-dirty", "restart-services", "json", "j"},

		// ── The families ───────────────────────────────────────────────
		//
		// A family's entry is the union of its leaves' vocabularies. The
		// router cannot tell which leaf a token is meant for — the
		// dispatch happens below it — so it accepts the union and the
		// LEAF rejects what it does not support.
		"call": {"payload", "payload-file", "json", "j"},
	}

	// The families accept the union of every other command's
	// vocabulary. Anything the CLI knows is passed through to the
	// leaf; anything it does not know is rejected here, before a
	// handler can mistake it for data.
	union := map[string]bool{}
	for _, names := range registry {
		for _, n := range names {
			union[n] = true
		}
	}
	// The vocabulary of the family LEAVES that has no entry of its
	// own — the ops subcommands, the evidence leaves and the
	// self-heal leaf.
	for _, n := range familyLeafOptionNames {
		union[n] = true
	}
	all := make([]string, 0, len(union))
	for n := range union {
		all = append(all, n)
	}
	sort.Strings(all)
	for _, fam := range cliOptionFamilies {
		registry[fam] = all
	}
	cliOptionRegistry = registry
}

// simpleCommandOptions is the option vocabulary of the commands that
// live in simple_cmds.go.
//
// It is the union of that file's hand-rolled option spellings. It is
// deliberately generous: several of those commands accept only a
// subset, and the router passing a token on to a command that ignores
// it is exactly the behaviour that existed before this registry.
var simpleCommandOptions = []string{
	"chunk-size", "collection", "days", "doc", "dry-run", "duration", "json",
	"limit", "reason", "session", "since", "tag", "tags", "title", "ttl",
	"until", "url", "vector-weight", "weight",
}

// maintenanceOptionNames is the vocabulary of the maintenance commands
// (maint_cmds.go): prune, export and maintain.
var maintenanceOptionNames = []string{
	"collection", "days", "format", "never-accessed", "older-than", "output",
	"review", "since", "until",
}

// auditOptionNames is the vocabulary of `mpm audit`
// (handlers_audit.go).
var auditOptionNames = []string{
	"artifact-id", "component", "days", "include-stack", "json", "level",
	"limit",
}

// cognitiveVerbOptions is the option vocabulary of the cognitive-verb
// commands (decide, theorize, decision, theory, skill, learn).
//
// The decision and theory commands have PRECISE guards of their own
// (see recordDecisionOptions and proposeTheoryOptions) — this entry is
// the router's backstop for the ones that do not.
var cognitiveVerbOptions = []string{
	"json", "context", "c", "choice", "rationale", "r", "tags", "t",
	"supersedes", "hypothesis", "h", "validation", "validation-criteria",
	"v", "status", "s", "reason",
}

// recallOptionNames is the vocabulary of `mpm recall`, which owns a
// real FlagSet (recall.go).
var recallOptionNames = []string{
	"as-of", "before", "collection", "j", "json", "limit", "projected",
	"semantic", "since", "stale-days", "token-budget", "until",
	"vector-weight", "weight-below", "why",
}

// ingestOptionNames is the vocabulary of `mpm ingest` (ingest.go).
var ingestOptionNames = []string{
	"batch", "batch-size", "cleanup", "dry-run", "force", "history",
	"import-id", "list-schemas", "review", "source", "status", "undo",
}

// migrateOptionNames is the vocabulary of `mpm migrate` (migrate.go).
var migrateOptionNames = []string{
	"commit", "commit-batch", "dry-run", "from", "label", "undo",
}

// onboardOptionNames is the vocabulary of `mpm onboard`
// (onboard_cmds.go).
var onboardOptionNames = []string{
	"check", "dry-run", "home", "hosts", "j", "json", "models", "n",
	"repair", "report", "y", "yes",
}

// skillOptionNames is the vocabulary of the skill commands
// (handlers_skill.go).
var skillOptionNames = []string{
	"f", "file", "force", "name", "version",
}

// familyLeafOptionNames is the vocabulary of the family LEAVES that
// have no entry of their own in the registry.
//
// These are the ops subcommands (changelog, backfill-embeddings,
// confidence, logs), the evidence leaves and the self-heal leaf. Their
// options belong to the family, because the router dispatches them
// below itself and cannot tell which leaf a token is meant for.
//
// The list is checked against the source by
// TestCLIOptionRegistryCoversEverySourceVocabulary.
var familyLeafOptionNames = []string{
	"actor", "anti-patterns", "api-key", "apikey", "artifact",
	"artifact-id", "artifact-type", "backup", "base-url", "by",
	"candidate-cap", "compare-budget", "component", "content", "creature",
	"date", "description", "doc", "endpoint", "expires-in", "fact",
	"flavor", "force", "force-reembed", "format", "framework", "help",
	"include", "include-stack", "independence", "interactive", "invocation",
	"legacy", "level", "min-age-hours", "model", "never-accessed",
	"no-compliant", "notes", "older-than", "output", "per-drill",
	"per-framework", "project", "propose", "propose-limit", "provider",
	"quiet", "reason-code", "reference-url", "release-notes",
	"release-version", "repo", "scope", "show-content", "sidecar", "stats",
	"stdin", "strength", "threshold", "timeout", "token", "type", "version",
	"vibe", "visibility", "voice", "window-days", "with-synthesis",
	"write-budget",
}

// cliOptionDeferred lists the paths whose handler rejects an
// unrecognised option ITSELF, with a message better than the router's
// generic one.
//
// The router must not preempt them. `mpm doctor --nonsense` is
// rejected by the doctor with the full option block and a typo
// suggestion; `mpm provenance --bogus` names the supported flags;
// `mpm onboard --install-everything` says "unknown argument". The
// router's generic message would replace all three with a shorter one
// that carries less.
//
// The keys are either a command name ("doctor") or a "command
// subcommand" path ("challenge list"), because a family's leaf owns the
// rejection for that leaf only.
//
// The set was established empirically: each command was invoked with an
// unrecognised option and the router's guard was DISABLED, and the
// commands that still rejected it — naming the offending flag — are the
// ones listed here. The rest are policed by the router.
var cliOptionDeferred = map[string]bool{
	// The shared option-contract helper: the rejection carries the
	// full option block and a typo suggestion.
	"doctor":          true,
	"review":          true,
	"decide":          true,
	"record_decision": true,
	"theorize":        true,
	"propose_theory":  true,
	"gc":              true,
	"backup":          true,
	"synthesize":      true,
	"update":          true,

	// Per-command rejections with a message the router cannot match.
	"info":           true,
	"learn":          true,
	"version":        true,
	"help":           true,
	"onboard":        true,
	"provenance":     true,
	"challenge list": true,
}

// cliOptionSubcommandFamilies are the commands whose FIRST token is a
// subcommand name rather than an option.
//
// The router skips that first token, so `mpm ops --nonsense` reaches
// the ops dispatcher and is reported as an unknown SUBCOMMAND — which
// is what it is — rather than as an unknown option. Every token after
// the subcommand is still policed.
var cliOptionSubcommandFamilies = map[string]bool{
	// The families of the router's own dispatch table.
	"work": true, "session": true, "handoff": true, "lesson": true,
	"skill": true, "ops": true, "call": true, "kb": true, "topic": true,
	"reference": true, "memory": true, "mode": true, "persona": true,
	"integration": true, "self-heal": true, "why": true, "config": true,
	"onboard": true, "compact": true, "capability": true, "debug": true,
	"provenance": true, "drills": true, "tasks": true, "evidence": true,
	"switch": true, "directives": true,

	// The families that are not in the router's dispatch table but
	// dispatch their own subcommands.
	"cascade": true, "challenge": true, "ingest": true, "migrate": true,
}

// cliOptionFreeTextPositional lists the commands whose positional
// argument is FREE TEXT.
//
// The distinction matters because the router cannot tell an option from
// a value. `mpm remember Warning: don't forget --flags; ok?` stores
// five tokens of content, one of which begins with a dash. A router
// that policed every dash-prefixed token would reject the whole
// command — and the operator would lose the ability to remember
// anything that mentions a flag.
//
// So for these commands the scan stops at the first positional, which
// is the CLI's own convention: OPTIONS COME BEFORE POSITIONALS. The
// leading option run is still policed, so `mpm remember --bogus "a
// fact"` is rejected.
//
// Every other command keeps the strict scan, so `mpm rm <id> --bogus`
// is rejected rather than silently ignoring the flag.
var cliOptionFreeTextPositional = map[string]bool{
	"add":      true,
	"remember": true,
	"hint":     true,
}

// rejectUnknownCommandOption rejects an option-looking token the
// command does not accept.
//
// It returns 0 when every token is acceptable, and 1 (after reporting)
// when one is not.
//
// The scan is deliberately narrow, because the CLI's positionals may
// legitimately begin with a dash:
//
//   - a bare "-" is stdin;
//   - "--" terminates the options and everything after it is data;
//   - a token that does not begin with "-" is data;
//   - a negative number is a value, not an option.
//
// The router has already consumed -h/--help/-v/--version/-f/--force/
// -i/--interactive, so those never reach this scan.
func rejectUnknownCommandOption(cmdName string, args []string) int {
	// A path that owns its own rejection is left alone.
	if cliOptionDeferred[cmdName] {
		return 0
	}
	if len(args) > 0 && cliOptionDeferred[cmdName+" "+args[0]] {
		return 0
	}

	accepted, known := cliOptionRegistry[cmdName]
	if !known {
		// An unregistered command accepts no options. The router's
		// own "unknown command" report follows.
		accepted = nil
	}
	set := make(map[string]bool, len(accepted))
	for _, a := range accepted {
		set[a] = true
	}

	// A family's first token is a subcommand name, not an option.
	start := 0
	if cliOptionSubcommandFamilies[cmdName] && len(args) > 0 {
		start = 1
	}

	for _, a := range args[start:] {
		if a == "--" {
			return 0
		}
		if a == "-" || !strings.HasPrefix(a, "-") {
			// A positional. For a free-text command it ends the
			// scan: everything after it is content.
			if cliOptionFreeTextPositional[cmdName] {
				return 0
			}
			continue
		}
		name := strings.TrimLeft(a, "-")
		if name == "" {
			continue
		}
		if eq := strings.Index(name, "="); eq >= 0 {
			name = name[:eq]
		}
		if name == "" {
			continue
		}
		// A negative number is a value, not an option.
		if isNumericToken(name) {
			continue
		}
		if set[name] {
			continue
		}
		w := usererror.GetWriter()
		fmt.Fprintf(w, "❌ Unknown option: %s\n", a)
		fmt.Fprintln(w)
		fmt.Fprintf(w, "Usage: mpm %s [options]\n", cmdName)
		fmt.Fprintln(w)
		if s := SuggestOptionName(accepted, a); s != "" {
			fmt.Fprintf(w, "Tip: Did you mean %s?\n", s)
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "Run 'mpm %s --help' for the accepted options.\n", cmdName)
		return 1
	}
	return 0
}
