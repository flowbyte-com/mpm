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
		return respond("", "Usage: mpm record_decision <text>", 1)
	}

	input := strings.Join(args, " ")

	contextText := extractField(input, "CONTEXT:")
	choice := extractField(input, "CHOICE:")
	rationale := extractField(input, "RATIONALE:")
	tagsStr := extractField(input, "TAGS:")

	if choice == "" {
		choice = strings.TrimSpace(input)
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

	content := "CHOICE: " + choice
	if contextText != "" {
		content += "\nCONTEXT: " + contextText
	}
	if rationale != "" {
		content += "\nRATIONALE: " + rationale
	}

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

	return respond("", fmt.Sprintf("✅ Decision recorded: %s\n", mem.ID), 0)
}

// handleTheories lists theories with status chips. Supports filter: all, pending, resolved.
func handleTheories(args []string) int {
	filter := "all"
	if len(args) > 0 {
		filter = args[0]
	}

	dm := getDB()
	if dm == nil {
		return 1
	}

	memories, err := dm.GetMemoriesForExport("theories", "", "")
	if err != nil {
		return respond("", fmt.Sprintf("Error: %v\n", err), 1)
	}

	if len(memories) == 0 {
		return respond("", "No theories yet. Run `mpm propose_theory` to propose your first theory.\n", 0)
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

		if filter != "all" && status != filter {
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
