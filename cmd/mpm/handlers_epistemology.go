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
	if len(args) == 0 {
		return respond("", "Usage: mpm propose_theory <text>", 1)
	}

	input := strings.Join(args, " ")

	hypothesis := extractField(input, "HYPOTHESIS:")
	validationCriteria := extractField(input, "VALIDATION_CRITERIA:")
	status := extractField(input, "STATUS:")
	tagsStr := extractField(input, "TAGS:")

	if hypothesis == "" {
		hypothesis = strings.TrimSpace(input)
	}
	// Reject empty/whitespace-only hypotheses so a stray `mpm propose_theory
	// "   "` doesn't create an empty theories row.
	if strings.TrimSpace(hypothesis) == "" {
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
		return respond("", "Error: memory store not available\n", 1)
	}

	mem, err := store.AddMemory(content, "theories", tags, meta, "", "cli")
	if err != nil {
		return respond("", fmt.Sprintf("Failed to save theory: %v\n", err), 1)
	}

	// Auto-link to theories topic (idempotent via INSERT OR IGNORE)
	if dm := getDBConcrete(); dm != nil {
		topicID, tErr := dm.GetOrCreateTopic("theories")
		if tErr == nil {
			dm.AddMemoryToTopic(mem.ID, topicID, "primary")
		}
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
	patch := map[string]interface{}{
		"status":      "resolved",
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
	if len(args) == 0 {
		return respond("", "Usage: mpm record_decision [--context <text>] [--choice <text>] [--rationale <text>] [--tags <csv>] [--supersedes <decision-id>]\n"+
			"   or: mpm record_decision <text with CONTEXT:/CHOICE:/RATIONALE:/TAGS: tokens>\n"+
			"   or: mpm decide --context <text> --choice <text> --rationale <text> [--tags <csv>]", 1)
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
	if hasDecisionFlags(args) && choice == "" {
		return respond("", "Error: --choice is required when using flag form\n", 1)
	}

	if choice == "" {
		return respond("", "Error: decision requires a CHOICE (flag --choice or CHOICE: token)\n", 1)
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
		return respond("", fmt.Sprintf("✅ Decision superseded: %s (was %s)\n", newID, supersedes), 0)
	}

	mem, err := store.AddMemory(content, "decisions", tags, meta, "", "cli")
	if err != nil {
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
	case "help", "-h", "--help":
		return respond("", "Usage: mpm theories [list|all|pending|resolved|proven|disproven]\n", 0)
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

// handleDecisions displays the decision ledger with context, choice, and rationale for each entry.
func handleDecisions(args []string) int {
	dm := getDB()
	if dm == nil {
		return 1
	}

	// Alpha-4 D-005 subcommands: show / list / query.
	// `mpm decisions` (no args) keeps the legacy listing behavior for
	// backward compatibility with operators' muscle memory.
	if len(args) > 0 {
		switch args[0] {
		case "show":
			return handleDecisionsShow(dm, args[1:])
		case "list":
			return handleDecisionsList(dm, args[1:])
		case "query":
			return handleDecisionsQuery(dm, args[1:])
		}
	}

	memories, err := dm.GetMemoriesForExport("decisions", "", "")
	if err != nil {
		return respond("", fmt.Sprintf("Error: %v\n", err), 1)
	}

	if len(memories) == 0 {
		return respond("", "No decisions recorded yet. Run `mpm record_decision` to log your first decision.\n", 0)
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
		for _, name := range active.Modes {
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
	limit := 0
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
	limit := 0
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
