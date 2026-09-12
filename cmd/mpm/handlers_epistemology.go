package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/config"
	"github.com/flowbyte-com/mpm-core/usererror"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

func handleProposeTheory(args []string) int {
	// W-001: --json flag for machine-readable output.
	jsonOutput := false
	filteredArgs := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--json" {
			jsonOutput = true
			continue
		}
		filteredArgs = append(filteredArgs, args[i])
	}
	args = filteredArgs

	if len(args) == 0 {
		if jsonOutput {
			return respond("", `{"success":false,"error":"Usage: mpm propose_theory [--json] <text>"}`+"\n", 1)
		}
		return respond("", "Usage: mpm propose_theory [--json] <text>", 1)
	}

	// D-001/002/003: parse named flags first (--hypothesis, --validation,
	// --tags, --status). The pre-fix implementation joined all args into a
	// single string and ran the legacy token-form extractor, which silently
	// dropped --validation and concatenated flag text into the hypothesis.
	// Flag form takes precedence; legacy HYPOTHESIS:/VALIDATION_CRITERIA:
	// token form is still supported for backwards compatibility.
	hypothesis, validationCriteria, status, tagsStr, leftoverArgs := parseTheoryArgs(args)

	if hypothesis == "" && len(leftoverArgs) > 0 {
		// Pipe-separated positional form (M3 audit D-001/002/003): the
		// documented "<hypothesis> | <validation>" shape with `|` as the
		// explicit separator. Both single-token (whole phrase in one arg)
		// and two-token (split across args) shapes are supported. Without
		// the separator, two bare tokens are too ambiguous to disambiguate
		// (which is hypothesis, which is validation?), so we refuse
		// rather than silently dropping validation. The single-token
		// form keeps the legacy HYPOTHESIS:/VALIDATION_CRITERIA:
		// extractor as a final fallback.
		if validationCriteria == "" && len(leftoverArgs) >= 1 {
			joined := strings.Join(leftoverArgs, " ")
			if strings.Contains(joined, "|") {
				parts := strings.SplitN(joined, "|", 2)
				hypothesis = strings.TrimSpace(parts[0])
				validationCriteria = strings.TrimSpace(parts[1])
			} else {
				input := joined
				hypothesis = extractField(input, "HYPOTHESIS:")
				if hypothesis == "" {
					hypothesis = strings.TrimSpace(input)
				}
				if validationCriteria == "" {
					validationCriteria = extractField(input, "VALIDATION_CRITERIA:")
				}
				if status == "" {
					status = extractField(input, "STATUS:")
				}
				if tagsStr == "" {
					tagsStr = extractField(input, "TAGS:")
				}
			}
		}
	}
	// Reject empty/whitespace-only hypotheses so a stray `mpm propose_theory
	// "   "` doesn't create an empty theories row.
	if strings.TrimSpace(hypothesis) == "" {
		if jsonOutput {
			return respond("", `{"success":false,"error":"propose_theory: hypothesis is required (non-empty)"}`+"\n", 1)
		}
		return respond("", "propose_theory: hypothesis is required (non-empty)\n", 1)
	}
	if status == "" {
		status = "pending"
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

	content := hypothesis
	if validationCriteria != "" {
		content += "\n\nVALIDATION_CRITERIA: " + validationCriteria
	}

	meta := map[string]interface{}{
		"status": status,
	}
	if validationCriteria != "" {
		meta["validation_criteria"] = validationCriteria
	}

	store := getMemoryStore()
	if store == nil {
		if jsonOutput {
			return respond("", `{"success":false,"error":"memory store not available"}`+"\n", 1)
		}
		return respond("", "Error: memory store not available\n", 1)
	}

	mem, err := store.AddMemory(content, "theories", tags, meta, "", "cli")
	if err != nil {
		if jsonOutput {
			out, _ := json.Marshal(map[string]interface{}{
				"success": false,
				"error":   fmt.Sprintf("Failed to save theory: %v", err),
			})
			return respond("", string(out)+"\n", 1)
		}
		return respond("", fmt.Sprintf("Failed to save theory: %v\n", err), 1)
	}

	// Auto-link to theories topic (idempotent via INSERT OR IGNORE)
	if dm := getDBConcrete(); dm != nil {
		topicID, tErr := dm.GetOrCreateTopic("theories")
		if tErr == nil {
			dm.AddMemoryToTopic(mem.ID, topicID, "primary")
		}
	}

	if jsonOutput {
		out, _ := json.Marshal(map[string]interface{}{
			"success": true,
			"id":      mem.ID,
			"status":  status,
		})
		return respond("", string(out)+"\n", 0)
	}
	return respond("", fmt.Sprintf("✅ Theory proposed: %s (status: %s)\n", mem.ID, status), 0)
}

// handleResolveTheory marks a theory as resolved, updating metadata and bumping weight.
//
// Arc 1 closure: if --winner=<memory_id> is provided, the theory is
// treated as an arbitration theory (created by the close-call path
// of `mpm ops resolve-contradictions`). The system verifies the
// winner is in the theory's dependencies, slashes the OTHER one,
// writes a resolution memory, attaches evidence to the winner, and
// marks the original contradiction queue row as resolved — all in
// one transaction. If --winner is not provided, the legacy path
// runs (just mark the theory resolved; no slash).
func handleResolveTheory(args []string) int {
	if len(args) < 2 {
		return respond("", "Usage: mpm resolve_theory <id> <conclusion> [--winner=<memory_id>]", 1)
	}

	id := args[0]
	conclusion := strings.Join(args[1:], " ")
	// Strip --winner=<memory_id> from conclusion (it might be at the
	// end if the operator pasted it there, or interspersed). The
	// cleanest way is to scan args for the flag and remove it.
	winnerID := ""
	filtered := make([]string, 0, len(args))
	for _, a := range args {
		if strings.HasPrefix(a, "--winner=") {
			winnerID = strings.TrimPrefix(a, "--winner=")
			continue
		}
		filtered = append(filtered, a)
	}
	if len(filtered) >= 2 {
		id = filtered[0]
		conclusion = strings.Join(filtered[1:], " ")
	} else {
		id = filtered[0]
		conclusion = ""
	}
	// Reject empty conclusion so a stray `mpm resolve_theory <id>` doesn't
	// resolve the theory without recording what was learned.
	if strings.TrimSpace(conclusion) == "" {
		return respond("", "resolve_theory: conclusion is required (non-empty)\n", 1)
	}

	dm := getDB()
	if dm == nil {
		return 1
	}

	// Arc 1 closure: if --winner is provided, route through the
	// arbitration auto-slash path. This atomically: (1) slashes
	// the loser, (2) writes a resolution memory, (3) attaches
	// evidence to the winner, (4) marks the queue row resolved,
	// (5) resolves the theory. All in one transaction.
	if winnerID != "" {
		result, err := dm.ResolveArbitrationTheory(id, winnerID, conclusion)
		if err != nil {
			return respond("", fmt.Sprintf("Failed to resolve arbitration theory: %v\n", err), 1)
		}
		// Human-readable confirmation.
		out := fmt.Sprintf("✅ Arbitration resolved: %s\n", id)
		out += fmt.Sprintf("   winner: %s (kept)\n", result["winner_id"])
		out += fmt.Sprintf("   loser:  %s (slashed by %v weight units)\n", result["loser_id"], result["slash_amount"])
		out += fmt.Sprintf("   resolution memory: %s\n", result["resolution_memory_id"])
		out += fmt.Sprintf("   evidence for winner: %s\n", result["evidence_id"])
		out += fmt.Sprintf("   queue row %v marked resolved\n", result["queue_id"])
		out += fmt.Sprintf("   conclusion: %s\n", conclusion)
		return respond("", out, 0)
	}

	// Legacy path: plain theory resolution (no slash). The
	// metadata patch + weight bump are unchanged from the
	// pre-Arc-1-closure behavior.
	mem, err := dm.GetMemory(id)
	if err != nil {
		return respond("", fmt.Sprintf("Theory not found: %s\n", id), 1)
	}

	coll, _ := mem["collection"].(string)
	if coll != "theories" {
		return respond("", fmt.Sprintf("Memory %s is not a theory (collection: %s)\n", id, coll), 1)
	}

	// Build metadata patch (upserts into existing metadata via json_patch)
	now := time.Now().UTC().Format(time.RFC3339)
	// D-010: align the internal status vocabulary with the filter
	// vocabulary. The MCP/arbitration paths write "proven"/"disproven";
	// the legacy CLI wrote "resolved" for both outcomes, which made
	// `mpm call mpm_theories list status=proven` return zero rows for
	// theories resolved through the CLI. Map the conclusion keyword to
	// the matching status. For an unknown conclusion keyword, refuse
	// rather than silently writing the legacy "resolved" value — the
	// audit's D-010 finding was that an operator running
	// `mpm resolve_theory <id> some random text` got a row that
	// `list status=proven` couldn't find. The legacy "resolved" value
	// is still honored as a stored value on rows written before the
	// fix, but never written by this handler.
	status := ""
	switch strings.ToLower(strings.TrimSpace(conclusion)) {
	case "confirmed", "proven":
		status = "proven"
	case "disproven", "refuted", "invalidated":
		status = "disproven"
	}
	if status == "" {
		return respond("", "resolve_theory: conclusion must be one of: confirmed, proven, disproven, refuted, invalidated\n", 1)
	}
	patch := map[string]interface{}{
		"status":      status,
		"conclusion":  conclusion,
		"resolved_at": now,
	}
	patchJSON, _ := json.Marshal(patch)

	if err := dm.UpdateMemoryMetadata(id, string(patchJSON)); err != nil {
		return respond("", fmt.Sprintf("Failed to resolve theory: %v\n", err), 1)
	}

	// Bump weight — reinforces the resolved theory. Defense Triad rule 3:
	// surface errors so a missing/deleted row doesn't print "resolved"
	// after the metadata patch succeeded.
	if err := dm.ReinforceMemory(id, 1); err != nil {
		return respond("", fmt.Sprintf("Failed to reinforce resolved theory: %v\n", err), 1)
	}

	return respond("", fmt.Sprintf("✅ Theory resolved: %s — %s\n", id, conclusion), 0)
}

// handleRecordDecision parses structured decision text and saves to the decisions collection.
func handleRecordDecision(args []string) int {
	// W-001: --json flag for machine-readable output (parity with `mpm call`).
	jsonOutput := false
	filteredArgs := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		if args[i] == "--json" {
			jsonOutput = true
			continue
		}
		filteredArgs = append(filteredArgs, args[i])
	}
	args = filteredArgs

	if len(args) == 0 {
		if jsonOutput {
			return respond("", `{"success":false,"error":"decision requires --choice or token form"}`+"\n", 1)
		}
		return respond("", "Usage: mpm record_decision [--json] [--context <text>] [--choice <text>] [--rationale <text>] [--tags <csv>] [--supersedes <decision-id>]\n"+
			"   or: mpm record_decision [--json] <text with CONTEXT:/CHOICE:/RATIONALE:/TAGS: tokens>\n"+
			"   or: mpm decide --json --context <text> --choice <text> --rationale <text> [--tags <csv>]", 1)
	}

	// F-C1/C2: support both flag-style and legacy token-style inputs.
	// Flag form takes precedence; if no flags are present, fall back
	// to the colon-token parser for backward compatibility.
	contextText, choice, rationale, tagsStr, freeText, leftoverArgs := parseDecisionArgs(args)
	if choice == "" {
		choice = freeText
	}

	// F-H1: --supersedes <decision-id> routes through the
	// SupersedeDecision substrate call. The CLI flag uses the
	// user-facing "decision-id" vocabulary; the substrate's internal
	// parameter is "original_id". This shim reconciles the naming.
	supersedes := extractFlagValue(args, "--supersedes")

	// If the caller passed flags but no choice, refuse — silent-empty
	// decisions were the bug F-C1 surfaced (the operator thought they
	// had recorded a choice; the DB got an empty record).
	if hasDecisionFlags(args) && strings.TrimSpace(choice) == "" {
		if jsonOutput {
			return respond("", `{"success":false,"error":"--choice is required when using flag form"}`+"\n", 1)
		}
		return respond("", "Error: --choice is required when using flag form\n", 1)
	}

	// F-2 (2026-09-04 residual inventory, P1): reject whitespace-only
	// choices at the CLI boundary. Pre-fix, `--choice "   "` passed the
	// `choice == ""` emptiness check (whitespace is non-empty in Go)
	// and the row landed in the decisions table with choice="   " — a
	// low-quality artifact future retrieval would surface verbatim.
	// Match the pattern already in use at handlers_epistemology.go:81
	// for theory hypothesis and :94 for tags.
	if strings.TrimSpace(choice) == "" {
		if jsonOutput {
			return respond("", `{"success":false,"error":"decision requires a CHOICE (flag --choice or CHOICE: token)"}`+"\n", 1)
		}
		return respond("", "Error: decision requires a CHOICE (flag --choice or CHOICE: token)\n", 1)
	}
	// Persist the trimmed form so retrieval doesn't surface leading/trailing
	// whitespace as part of the choice.
	choice = strings.TrimSpace(choice)

	var tags []string
	if tagsStr != "" {
		for _, t := range strings.Split(tagsStr, ",") {
			t = strings.TrimSpace(t)
			if t != "" {
				tags = append(tags, t)
			}
		}
	}

	// D-007 (alpha-4.1.1) cross-surface fix: the CLI previously prefixed
	// each structured field with a label ("CHOICE: ", "CONTEXT: ",
	// "RATIONALE: ") and used single "\n" separators — the same
	// duplication the MCP path's RecordDecision was carrying. The MCP
	// path was updated to strings.Join(parts, "\n\n") with no labels;
	// the CLI must match so a decision recorded via the CLI surface
	// shows up identically when read back through `mpm decisions show`
	// (or the MCP equivalent). Metadata still carries the structured
	// fields; content is the FTS-searchable body, with no labels.
	var contentParts []string
	if choice != "" {
		contentParts = append(contentParts, choice)
	}
	if contextText != "" {
		contentParts = append(contentParts, contextText)
	}
	if rationale != "" {
		contentParts = append(contentParts, rationale)
	}
	content := strings.Join(contentParts, "\n\n")

	meta := map[string]interface{}{}
	if contextText != "" {
		meta["context"] = contextText
	}
	if rationale != "" {
		meta["rationale"] = rationale
	}

	store := getMemoryStore()
	if store == nil {
		return respond("", "Error: memory store not available\n", 1)
	}

	// F-H1: --supersedes routes through SupersedeDecision when
	// present; otherwise fall through to the plain AddMemory path.
	if supersedes != "" {
		dm := getDBConcrete()
		if dm == nil {
			return respond("", "Error: database not available\n", 1)
		}
		res, err := dm.SupersedeDecision(supersedes, contextText, choice, rationale, "", tags, nil, internal.ActiveContext{})
		if err != nil {
			return respond("", fmt.Sprintf("Failed to supersede decision: %v\n", err), 1)
		}
		newID, _ := res["id"].(string)
		if newID == "" {
			return respond("", fmt.Sprintf("Failed to supersede decision: no id in result %v\n", res), 1)
		}
		if jsonOutput {
		out, _ := json.Marshal(map[string]interface{}{
			"success":     true,
			"id":          newID,
			"supersedes":  supersedes,
			"action":      "supersede",
		})
		return respond("", string(out)+"\n", 0)
	}
	return respond("", fmt.Sprintf("✅ Decision superseded: %s (was %s)\n", newID, supersedes), 0)
	}

	mem, err := store.AddMemory(content, "decisions", tags, meta, "", "cli")
	if err != nil {
		if jsonOutput {
			out, _ := json.Marshal(map[string]interface{}{
				"success": false,
				"error":   fmt.Sprintf("Failed to record decision: %v", err),
			})
			return respond("", string(out)+"\n", 1)
		}
		return respond("", fmt.Sprintf("Failed to record decision: %v\n", err), 1)
	}

	// Auto-link to decisions topic
	if dm := getDBConcrete(); dm != nil {
		topicID, tErr := dm.GetOrCreateTopic("decisions")
		if tErr == nil {
			dm.AddMemoryToTopic(mem.ID, topicID, "primary")
		}
	}

	_ = leftoverArgs // currently unused; reserved for future positional content
	if jsonOutput {
		out, _ := json.Marshal(map[string]interface{}{
			"success": true,
			"id":      mem.ID,
			"action":  "record",
		})
		return respond("", string(out)+"\n", 0)
	}
	return respond("", fmt.Sprintf("✅ Decision recorded: %s\n", mem.ID), 0)
}

// parseDecisionArgs supports two input shapes:
//
//  1. flag form: --context <text> --choice <text> --rationale <text>
//     --tags <csv> [--supersedes <decision-id>] [--weight N]
//  2. legacy token form: "CHOICE: foo\nCONTEXT: bar\nRATIONALE: baz"
//
// Flag form takes precedence. If no flag is present, the args are
// joined into one string and the legacy extractor runs.
//
// Returns: contextText, choice, rationale, tagsStr, freeText, leftoverArgs.
func parseDecisionArgs(args []string) (string, string, string, string, string, []string) {
	var (
		contextText string
		choice      string
		rationale   string
		tagsStr     string
		leftovers   []string
	)
	flagMode := false
	i := 0
	for i < len(args) {
		switch args[i] {
		case "--context", "-c":
			if i+1 < len(args) {
				contextText = args[i+1]
				i += 2
				flagMode = true
				continue
			}
			i++
		case "--choice":
			if i+1 < len(args) {
				choice = args[i+1]
				i += 2
				flagMode = true
				continue
			}
			i++
		case "--rationale", "-r":
			if i+1 < len(args) {
				rationale = args[i+1]
				i += 2
				flagMode = true
				continue
			}
			i++
		case "--tags", "-t":
			if i+1 < len(args) {
				tagsStr = args[i+1]
				i += 2
				flagMode = true
				continue
			}
			i++
		default:
			leftovers = append(leftovers, args[i])
			i++
		}
	}

	if flagMode {
		return contextText, choice, rationale, tagsStr, "", leftovers
	}

	// CLI acceptance 2026-09-12: bare `key=value` form (`mpm decide
	// context="..." choice="..." rationale="..."`, as taught by `mpm tour`
	// step 4 and the decision help examples). Pre-fix these fell through
	// as leftovers and the whole string landed in CHOICE with empty
	// CONTEXT/RATIONALE — a silent malformation on the onboarding path.
	// Match a leading `key=` (case-insensitive) for the four known keys;
	// anything else stays a leftover so free text containing "=" is
	// unaffected. Surrounding quotes/backslash-quotes (from programmatic
	// callers like the tour demo) are stripped.
	var rest []string
	kvFound := false
	for _, tok := range leftovers {
		key, val, hasEq := strings.Cut(tok, "=")
		switch strings.ToLower(key) {
		case "context", "choice", "rationale", "tags":
			if hasEq {
				val = strings.TrimSpace(val)
				// Strip programmatic backslash-escapes then shell quotes.
				val = strings.ReplaceAll(val, `\"`, `"`)
				val = strings.ReplaceAll(val, `\'`, `'`)
				if len(val) >= 2 {
					if (val[0] == '"' && val[len(val)-1] == '"') ||
						(val[0] == '\'' && val[len(val)-1] == '\'') {
						val = val[1 : len(val)-1]
					}
				}
				switch strings.ToLower(key) {
				case "context":
					contextText = val
				case "choice":
					choice = val
				case "rationale":
					rationale = val
				case "tags":
					tagsStr = val
				}
				kvFound = true
				continue
			}
		}
		rest = append(rest, tok)
	}
	if kvFound {
		if choice == "" && len(rest) > 0 {
			choice = strings.TrimSpace(strings.Join(rest, " "))
			rest = nil
		}
		return contextText, choice, rationale, tagsStr, "", rest
	}

	// Legacy token form
	input := strings.Join(args, " ")
	contextText = extractField(input, "CONTEXT:")
	choice = extractField(input, "CHOICE:")
	rationale = extractField(input, "RATIONALE:")
	tagsStr = extractField(input, "TAGS:")

	if choice == "" {
		choice = strings.TrimSpace(input)
	}
	return contextText, choice, rationale, tagsStr, "", leftovers
}

// parseTheoryArgs extracts named flags from `mpm propose_theory` /
// `mpm theorize` args. Four forms are accepted:
//
//  1. long-flag form: --hypothesis "X" --validation "Y" [--tags a,b] [--status proven]
//  2. short-flag form: -h X --v Y
//  3. bare key=value form: hypothesis=X validation=Y  (D-001/002/003, M3 audit)
//  4. legacy token form: "HYPOTHESIS: X\nVALIDATION_CRITERIA: Y"
//
// The bare key=value form is matched only when the token does NOT start with
// `--` (otherwise it would collide with the long-flag form) and contains
// `=` (otherwise it's a positional arg). Values are unquoted by trimming a
// single matched leading/trailing `"`.
//
// Flag forms take precedence over positional/legacy forms. If no flag is
// detected, the leftover positional args are returned for the caller to feed
// into the legacy extractor (extractField on HYPOTHESIS: / VALIDATION_CRITERIA:
// / STATUS: / TAGS: prefixes), or for the two-token positional form
// "<hypothesis> | <validation>" where `|` is the explicit separator.
//
// Returns: hypothesis, validationCriteria, status, tagsStr, leftoverArgs.
func parseTheoryArgs(args []string) (string, string, string, string, []string) {
	var (
		hypothesis         string
		validationCriteria string
		status             string
		tagsStr            string
		leftovers          []string
	)
	flagMode := false
	i := 0
	for i < len(args) {
		a := args[i]
		switch a {
		case "--hypothesis", "--h", "-h":
			if i+1 < len(args) {
				hypothesis = args[i+1]
				i += 2
				flagMode = true
				continue
			}
			i++
		case "--validation", "--validation-criteria", "--v":
			if i+1 < len(args) {
				validationCriteria = args[i+1]
				i += 2
				flagMode = true
				continue
			}
			i++
		case "--status", "--s":
			if i+1 < len(args) {
				status = args[i+1]
				i += 2
				flagMode = true
				continue
			}
			i++
		case "--tags", "--t", "-t":
			if i+1 < len(args) {
				tagsStr = args[i+1]
				i += 2
				flagMode = true
				continue
			}
			i++
		default:
			// Bare key=value form (M3 audit D-001/002/003): tokens
			// without `--` prefix that contain `=` are interpreted as
			// `key=value`. The key namespace matches the long-flag
			// forms. Surrounding double-quotes are trimmed.
			if !strings.HasPrefix(a, "--") && !strings.HasPrefix(a, "-") {
				if eq := strings.IndexByte(a, '='); eq > 0 {
					key := strings.ToLower(strings.TrimSpace(a[:eq]))
					val := unquoteBareValue(a[eq+1:])
					switch key {
					case "hypothesis", "hypothesis_id", "h":
						hypothesis = val
						flagMode = true
						i++
						continue
					case "validation", "validation_criteria", "v":
						validationCriteria = val
						flagMode = true
						i++
						continue
					case "status", "s":
						status = val
						flagMode = true
						i++
						continue
					case "tags", "t":
						tagsStr = val
						flagMode = true
						i++
						continue
					}
				}
			}
			leftovers = append(leftovers, a)
			i++
		}
	}

	if flagMode {
		return hypothesis, validationCriteria, status, tagsStr, leftovers
	}

	// No flags detected — caller may still run the legacy token-form
	// extractor over the joined leftover args.
	return "", "", "", "", leftovers
}

// unquoteBareValue trims a single matched pair of leading/trailing double
// quotes. Used by the bare key=value parser so callers can write
// `validation="throughput on 4 readers"` without shell quoting headaches.
// Single quotes and asymmetric quotes are preserved verbatim.
func unquoteBareValue(s string) string {
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		return s[1 : len(s)-1]
	}
	return s
}

// hasDecisionFlags returns true if any decision-style flag is present
// in args. Used by the validation branch above to enforce "flag form
// requires --choice".
func hasDecisionFlags(args []string) bool {
	for _, a := range args {
		switch a {
		case "--context", "-c",
			"--choice",
			"--rationale", "-r",
			"--tags", "-t",
			"--supersedes":
			return true
		}
	}
	return false
}

// extractFlagValue scans args for `--flag value` and returns the value
// (the arg immediately following the flag). Returns "" if absent.
// Empty values are valid (caller decides whether to reject them).
func extractFlagValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// handleTheories lists theories with status chips.
//
// Usage: mpm theories [list|ls|all|pending|resolved|proven|disproven]
//
// The literal subcommand "list"/"ls" is consumed as "show everything"
// — previously it leaked into the status filter where it matched no
// row, so `mpm theories list` printed "No list theories found." even
// with 61+ pending theories (audit finding F10).
//
// Status vocabulary: rows resolved through the MCP/arbitration paths
// carry "proven"/"disproven" while the legacy CLI wrote "resolved".
// The filter treats them as one family: "resolved" matches both
// proven and disproven rows.
func handleTheories(args []string) int {
	dm := getDB()
	if dm == nil {
		return 1
	}
	return runTheories(dm, args)
}

// runTheories is the injectable core of `mpm theories` so tests can drive
// it against a hermetic database.
func runTheories(dm mpminternal.CoreDB, args []string) int {
	filter := "all"
	if len(args) > 0 {
		filter = args[0]
	}
	switch filter {
	case "list", "ls":
		filter = "all"
	case "all", "pending", "resolved", "proven", "disproven":
		// Accepted status filters — fall through to the listing code
		// which uses statusMatches() to filter rows by their stored
		// status (resolved is treated as the family containing proven +
		// disproven rows per the help text).
	case "help", "-h", "--help":
		return respond("", "Usage: mpm theories [list|all|pending|resolved|proven|disproven]\n", 0)
	default:
		// Audit D-005: reject unknown filters explicitly. Previously an
		// unknown filter (e.g. "bogus") silently fell through to the
		// status-match branch, which then matched nothing and printed
		// "no theories" — misleading for operators/agents who typoed
		// a verb. List the valid options in the error.
		valid := []string{"all", "pending", "resolved", "proven", "disproven", "list", "ls"}
		return respond("", fmt.Sprintf("Unknown filter %q for `mpm theories`. Valid filters: %s.\n", filter, strings.Join(valid, ", ")), 1)
	}

	memories, err := dm.GetMemoriesForExport("theories", "", "")
	if err != nil {
		return respond("", fmt.Sprintf("Error: %v\n", err), 1)
	}

	if len(memories) == 0 {
		return respond("", "No theories yet. Run `mpm propose_theory` to propose your first theory.\n", 0)
	}

	statusMatches := func(status, filter string) bool {
		switch filter {
		case "all":
			return true
		case "resolved":
			return status == "resolved" || status == "proven" || status == "disproven"
		default:
			return status == filter
		}
	}

	count := 0
	for _, m := range memories {
		content, _ := m["content"].(string)
		id, _ := m["id"].(string)

		var meta map[string]interface{}
		metaStr, _ := m["metadata"].(string)
		json.Unmarshal([]byte(metaStr), &meta)

		status, _ := meta["status"].(string)
		if status == "" {
			status = "pending"
		}

		if !statusMatches(status, filter) {
			continue
		}

		display := strings.SplitN(content, "\n", 2)[0]
		if len(display) > 80 {
			display = display[:80] + "..."
		}

		fmt.Printf("[%s] %s  [status: %s]\n", id, display, status)
		count++

		if vc, ok := meta["validation_criteria"].(string); ok && vc != "" {
			vcDisplay := vc
			if len(vcDisplay) > 60 {
				vcDisplay = vcDisplay[:60] + "..."
			}
			fmt.Printf("      validation: %s\n", vcDisplay)
		}
	}

	if count == 0 {
		fmt.Printf("No %s theories found.\n", filter)
	}

	return 0
}

// backfillEpistemologyTopics links existing theories/decisions memories to their topics.
// Idempotent: AddMemoryToTopic uses INSERT OR IGNORE so it is safe to call repeatedly.
func backfillEpistemologyTopics() {
	dm := getDB()
	if dm == nil {
		return
	}

	rows, err := dm.SQLDB().Query(
		`SELECT id, collection FROM memories WHERE collection IN ('theories', 'decisions') AND deleted_at IS NULL`,
	)
	if err != nil {
		return
	}
	defer rows.Close()

	linked := 0
	now := time.Now().UTC().Format(time.RFC3339)
	scanErr := func() error {
		for rows.Next() {
			var id, collection string
			if err := rows.Scan(&id, &collection); err != nil {
				return fmt.Errorf("scanning memory row for epistemology topic backfill: %w", err)
			}
			topicID, tErr := dm.GetOrCreateTopic(collection)
			if tErr != nil {
				continue
			}
		res, execErr := dm.SQLDB().Exec(
			`INSERT OR IGNORE INTO topic_memberships (memory_id, topic_id, created_at, role) VALUES (?, ?, ?, ?)`,
			id, topicID, now, "primary",
		)
		if execErr == nil {
			if ra, _ := res.RowsAffected(); ra > 0 {
				linked++
			}
		}
		}
		return nil
	}()
	if scanErr != nil {
		usererror.Warn("backfillEpistemologyTopics: %v", scanErr)
	}

	if linked > 0 {
		fmt.Printf("📚 Epistemology topics initialized: %d memories linked\n", linked)
	}
}

// defaultDecisionsListLimit caps the rows printed by the no-args legacy
// `mpm decisions` listing. Mirrors `mpm ls`'s default of 20 and the
// `mpm decisions list/query` --limit=N parsing convention so the same
// CLI flag has the same meaning across all three paths. Set --limit=0
// to disable the cap (0 means "no cap" in decision_filter.Limit, the
// existing convention used by mpm_decisions list).
const defaultDecisionsListLimit = 20

// handleDecisions displays the decision ledger with context, choice, and rationale for each entry.
func handleDecisions(args []string) int {
	dm := getDB()
	if dm == nil {
		return 1
	}

	// Alpha-4 D-005 subcommands: show / list / query.
	// `mpm decisions` (no args) keeps the legacy listing behavior for
	// backward compatibility with operators' muscle memory.
	// Flags (e.g. --limit=N) are not subcommands, so they fall through
	// to the legacy path instead of triggering the
	// "Unknown subcommand" rejection.
	if len(args) > 0 && !strings.HasPrefix(args[0], "--") {
		switch args[0] {
		case "show":
			return handleDecisionsShow(dm, args[1:])
		case "list":
			return handleDecisionsList(dm, args[1:])
		case "query":
			return handleDecisionsQuery(dm, args[1:])
		default:
			// Audit D-005: an unknown subcommand used to silently fall
			// through to the legacy list. Agents running this in a
			// reasoning loop would treat the list output as confirmation
			// of their (typo'd) intent. Reject explicitly.
			return respond("", fmt.Sprintf("Unknown subcommand %q for `mpm decisions`. Valid subcommands: show, list, query (or no args for legacy listing).\n", args[0]), 1)
		}
	}

	memories, err := dm.GetMemoriesForExport("decisions", "", "")
	if err != nil {
		return respond("", fmt.Sprintf("Error: %v\n", err), 1)
	}

	if len(memories) == 0 {
		return respond("", "No decisions recorded yet. Run `mpm record_decision` to log your first decision.\n", 0)
	}

	// Pointer-indirection sweep F-S-2 (docs/pointer-indirection-sweep-2026-09-05.md):
	// cap the legacy no-args listing so output scales linearly with
	// displayed rows, not with the user's accumulated decisions. This
	// path was unbounded — today 139 rows × ~250 B = ~35 KB on the
	// live host, growing linearly without bound. Mirror the
	// --limit=N parsing used by handleDecisionsList / handleDecisionsQuery
	// so the flag behaves identically across the three decision paths.
	limit := defaultDecisionsListLimit
	for _, a := range args {
		if strings.HasPrefix(a, "--limit=") {
			if n, err := strconv.Atoi(strings.TrimPrefix(a, "--limit=")); err == nil && n >= 0 {
				limit = n
			}
		}
	}
	if limit > 0 && len(memories) > limit {
		memories = memories[:limit]
	}

	for _, m := range memories {
		content, _ := m["content"].(string)
		createdAt, _ := m["created_at"].(string)

		var meta map[string]interface{}
		metaStr, _ := m["metadata"].(string)
		json.Unmarshal([]byte(metaStr), &meta)

		contextText, _ := meta["context"].(string)
		rationale, _ := meta["rationale"].(string)

		choice := strings.SplitN(content, "\n", 2)[0]
		if strings.HasPrefix(strings.ToUpper(choice), "CHOICE: ") {
			choice = strings.TrimSpace(choice[7:])
		}

		dateStr := createdAt
		if len(dateStr) >= 10 {
			dateStr = dateStr[:10]
		}

		fmt.Println("─────────────────────")
		if contextText != "" {
			fmt.Printf("CONTEXT:  %s\n", contextText)
		} else {
			fmt.Println("CONTEXT:  —")
		}
		fmt.Printf("CHOICE:   %s\n", choice)
		if rationale != "" {
			fmt.Printf("RATIONALE: %s\n", rationale)
		} else {
			fmt.Println("RATIONALE: —")
		}
		fmt.Printf("[%s]\n", dateStr)
	}
	fmt.Println("─────────────────────")

	return 0
}

// handleHint checks recent conversation context for epistemologically relevant
// memories (theories and decisions). Supports --json and --max <n> flags.
func handleHint(args []string) int {
	// Parse --max and --json flags
	maxHints := 1
	jsonOutput := false
	cleanArgs := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--json":
			jsonOutput = true
		case "--max":
			if i+1 < len(args) {
				i++
				n, err := strconv.Atoi(args[i])
				if err == nil && n > 0 {
					maxHints = n
				}
			}
		default:
			cleanArgs = append(cleanArgs, args[i])
		}
	}

	if len(cleanArgs) == 0 {
		return respond("", "Usage: mpm hint [--json] [--max N] <conversation text>\n", 1)
	}

	conversationText := strings.Join(cleanArgs, " ")

	keywords := internal.ExtractConversationKeywords(conversationText, 50)

	if getDB() == nil {
		return 1
	}
	// FindEpistemologyOverlaps takes a concrete *DatabaseManager, not
	// the CoreDB interface. The singleton is always a *DatabaseManager,
	// so the type assertion is safe; the nil check above guards it.
	dm := getDB().(*mpminternal.DatabaseManager)

	retrievalLimit := maxHints
	retrievalThreshold := -3.0
	if active, err := mpminternal.LoadActiveJSON(); err == nil {
		mm := mpminternal.NewModeManager(config.GetMPMDir())
		for _, name := range active.ModesSlice() {
			if m, err := mm.Get(name); err == nil {
				retrievalLimit = m.RetrievalLimit
				retrievalThreshold = m.RetrievalThreshold
				break
			}
		}
	}
	if maxHints > 0 && maxHints < retrievalLimit {
		retrievalLimit = maxHints
	}

	overlaps, err := internal.FindEpistemologyOverlaps(dm, keywords, retrievalLimit, retrievalThreshold)
	if err != nil {
		return respond("", fmt.Sprintf("Error: %v\n", err), 1)
	}

	if len(overlaps) == 0 {
		if jsonOutput {
			return respond("", "[]\n", 0)
		}
		return respond("", "", 0)
	}

	if jsonOutput {
		out, _ := json.MarshalIndent(overlaps, "", "  ")
		return respond("", string(out)+"\n", 0)
	}

	// Regular mode: show first hint only, formatted
	hint := overlaps[0]
	collection, _ := hint["collection"].(string)
	content, _ := hint["content"].(string)
	createdAt, _ := hint["created_at"].(string)
	if len(createdAt) >= 10 {
		createdAt = createdAt[:10]
	}
	meta, _ := hint["metadata"].(map[string]interface{})

	var status string
	if meta != nil {
		if s, ok := meta["status"].(string); ok {
			status = s
		}
	}
	if status == "" {
		for _, line := range strings.Split(content, "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(strings.ToUpper(trimmed), "STATUS:") {
				status = strings.TrimSpace(trimmed[7:])
				break
			}
		}
	}

	switch collection {
	case "decisions":
		choice := strings.SplitN(content, "\n", 2)[0]
		if strings.HasPrefix(strings.ToUpper(choice), "CHOICE: ") {
			choice = strings.TrimSpace(choice[7:])
		}
		var rationale string
		for _, line := range strings.Split(content, "\n") {
			if strings.HasPrefix(strings.ToUpper(line), "RATIONALE:") {
				rationale = strings.TrimSpace(line[10:])
				break
			}
		}
		fmt.Printf("[Recall] You decided: %s\n", choice)
		if status != "" {
			fmt.Printf("  STATUS: %s | %s\n", status, createdAt)
		}
		if rationale != "" {
			fmt.Printf("  RATIONALE: %s\n", rationale)
		}
	case "theories":
		hypothesis := strings.SplitN(content, "\n", 2)[0]
		if strings.HasPrefix(strings.ToUpper(hypothesis), "HYPOTHESIS: ") {
			hypothesis = strings.TrimSpace(hypothesis[11:])
		}
		fmt.Printf("[Recall] Hypothesis: %s\n", hypothesis)
		if meta != nil {
			if conclusion, ok := meta["conclusion"].(string); ok && conclusion != "" {
				fmt.Printf("  STATUS: %s | %s\n", status, createdAt)
				fmt.Printf("  CONCLUSION: %s\n", conclusion)
			} else {
				fmt.Printf("  STATUS: %s | %s\n", status, createdAt)
			}
		} else {
			fmt.Printf("  STATUS: %s | %s\n", status, createdAt)
		}
	}

	return 0
}

// handleDecisionsShow (alpha-4 D-005) prints a single decision by id
// in human-readable form. Mirrors `mpm call mpm_decisions show`.
func handleDecisionsShow(dm mpminternal.CoreDB, args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm decisions show <id>\n", 1)
	}
	row, err := dm.GetDecision(args[0])
	if err != nil {
		return respond("", fmt.Sprintf("Error: %v\n", err), 1)
	}
	out, _ := json.MarshalIndent(row, "", "  ")
	fmt.Println(string(out))
	return 0
}

// handleDecisionsList (alpha-4 D-005) lists decisions matching an
// optional status filter. Mirrors `mpm call mpm_decisions list`.
func handleDecisionsList(dm mpminternal.CoreDB, args []string) int {
	status := ""
	// 2026-09-05 audit remediation pass 2: the default is 50 (was
	// previously 0 with a DM-side coercion to 50; the coercion is
	// removed so the public surface's `limit=0 → 0 results` contract
	// holds. The CLI default preserves the historical "no --limit
	// flag → 50 results" behaviour.
	limit := 50
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "--status="):
			status = strings.TrimPrefix(a, "--status=")
		case strings.HasPrefix(a, "--limit="):
			n, err := strconv.Atoi(strings.TrimPrefix(a, "--limit="))
			if err == nil {
				limit = n
			}
		}
	}
	rows, err := dm.ListDecisions(mpminternal.DecisionFilter{Status: status, Limit: limit})
	if err != nil {
		return respond("", fmt.Sprintf("Error: %v\n", err), 1)
	}
	out, _ := json.MarshalIndent(map[string]interface{}{
		"status":    statusOrDefaultAlpha4(status),
		"count":     len(rows),
		"decisions": rows,
	}, "", "  ")
	fmt.Println(string(out))
	return 0
}

// handleDecisionsQuery (alpha-4 D-005) FTS-searches decisions. Mirrors
// `mpm call mpm_decisions query`.
func handleDecisionsQuery(dm mpminternal.CoreDB, args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm decisions query <text> [--limit=N]\n", 1)
	}
	query := args[0]
	// 2026-09-05 audit remediation pass 2: see handleDecisionsList.
	limit := 50
	for _, a := range args[1:] {
		if strings.HasPrefix(a, "--limit=") {
			n, err := strconv.Atoi(strings.TrimPrefix(a, "--limit="))
			if err == nil {
				limit = n
			}
		}
	}
	rows, err := dm.QueryDecisions(query, limit)
	if err != nil {
		return respond("", fmt.Sprintf("Error: %v\n", err), 1)
	}
	out, _ := json.MarshalIndent(map[string]interface{}{
		"query":     query,
		"count":     len(rows),
		"decisions": rows,
	}, "", "  ")
	fmt.Println(string(out))
	return 0
}

// statusOrDefaultAlpha4 mirrors the tool-envelope helper so the CLI
// shape matches what `mpm call mpm_decisions list` returns.
func statusOrDefaultAlpha4(s string) string {
	if s == "" {
		return "active"
	}
	return s
}
