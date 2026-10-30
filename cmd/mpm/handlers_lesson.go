package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/flowbyte-com/mpm-core"

	"github.com/flowbyte-com/mpm/cmd/mpm/render"
)

func handleLesson(args []string) int {
	if len(args) < 1 {
		return handleLessonHelp()
	}

	subCmd := args[0]
	switch subCmd {
	case "help":
		return handleLessonHelp()
	case "add":
		return handleLessonAdd(args[1:])
	case "list":
		return handleLessonList(args[1:])
	case "search":
		return handleLessonSearch(args[1:])
	case "get":
		return handleLessonGet(args[1:])
	case "delete":
		// 2026-09-10 lifecycle fix: `delete` is now a reversible soft
		// delete (sets deleted_at tombstone; row + history retained).
		// `shred` remains the irreversible hard delete.
		return handleLessonDelete(args[1:])
	case "restore":
		// Undoes a prior soft-delete. Idempotent on an
		// already-visible lesson (returns a clean error).
		return handleLessonRestore(args[1:])
	case "shred":
		return handleLessonShred(args[1:])
	case "proposals":
		// The review queue: lesson proposals an agent has submitted and
		// an operator has not yet acted on.
		return handleLessonProposals(args[1:])
	case "promote":
		// The operator's acceptance of a pending proposal. This is the
		// ONLY path from a proposal to a trusted lesson.
		return handleLessonPromote(args[1:])
	case "reject":
		// The operator's decline of a pending proposal.
		return handleLessonReject(args[1:])
	case "stats":
		return handleLessonStats()
	default:
		return handleLessonHelp()
	}
}

func handleLessonHelp() int {
	output := `mpm lesson - Lesson operations

Usage:
  mpm lesson add <content> [--type warning|practice|insight] [--tags tags]
  mpm lesson list [--type warning|practice|insight]
  mpm lesson search <query>
  mpm lesson get <id>
  mpm lesson delete <id>     Soft-delete (reversible via restore)
  mpm lesson restore <id>    Restore a soft-deleted lesson
  mpm lesson shred <id>      Permanent hard delete (irreversible)
  mpm lesson proposals [--status pending|promoted|rejected|all]
  mpm lesson promote <id>    Accept a pending proposal → becomes a trusted lesson
  mpm lesson reject <id>     Decline a pending proposal
  mpm lesson stats

Lifecycle:
  delete  → soft/reversible — row + history retained, hidden from
            list/search/get. Reversible via 'restore'.
  restore → clear the deleted_at tombstone; lesson reappears in
            normal queries. Idempotent on an already-visible lesson.
  shred   → permanent/irreversible — row removed from lessons_base
            + lessons_fts; no restore path.

Trust boundary:
  A lesson is TRUSTED ON ARRIVAL — the lesson surface has no pending
  state, so a lesson row is immediately inside the trusted retrieval set
  (search, list, the wake context). Creating one is therefore an
  OPERATOR action: 'mpm lesson add' from a terminal.

  Agents cannot write a trusted lesson. An agent records the experience
  as a PROPOSAL ('mpm_lessons action=propose' over MCP or 'mpm call'),
  which is pending until an operator acts on it:

    mpm lesson proposals          list what is waiting for review
    mpm lesson promote <id>       accept it — it becomes a trusted lesson
    mpm lesson reject <id>        decline it (the record is kept)

Examples:
  mpm lesson add "Check file extensions before executing" --type warning --tags safety,files
  mpm lesson add "Use gofmt for Go code formatting" --type practice --tags go,style
  mpm lesson list
  mpm lesson list --type warning
  mpm lesson search "safety"
  mpm lesson get abc123
  mpm lesson delete abc123
  mpm lesson restore abc123
  mpm lesson shred abc123
  mpm lesson stats

Lesson types:
  warning  - "don't do X" (negative lessons)
  practice - "do Y" (positive lessons, best practices)
  insight  - "X leads to Y" (causal knowledge, default)
`
	return respond(output, "", 0)
}

func handleLessonAdd(args []string) int {
	// R3: --help / -h / "help" short-circuit. See handleMemoryAdd for
	// the rationale; same defect, same fix.
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "help" {
			return handleLessonHelp()
		}
	}
	if len(args) == 0 {
		return respond("", "Usage: mpm lesson add <content> [--type warning|practice|insight] [--tags tags] [--json]", 1)
	}

	content := ""
	lessonType := "insight"
	var tags []string
	jsonOutput, filteredArgs := ExtractJSONFlag(args)
	args = filteredArgs

	// Defect F (2026-09-13 acceptance): pre-fix this loop had a
	// "first positional wins, swallow the rest" anti-pattern. After
	// capturing content from the first non-flag token, it appended
	// any subsequent non-flag tokens to content AND then skipped
	// every remaining flag. Result: `mpm lesson add "x" --type
	// warning --tags a,b` stored content "x" with the default
	// lessonType "insight" and no tags — silently ignored. The fix
	// walks the full arg list linearly, hoisting --type/--tags out
	// of the positional stream regardless of position.
	var contentParts []string
	i := 0
	for i < len(args) {
		switch args[i] {
		case "--type":
			if i+1 < len(args) {
				lessonType = args[i+1]
				i += 2
			} else {
				return respond("", "--type requires a value (warning|practice|insight)", 1)
			}
		case "--tags":
			if i+1 < len(args) {
				tags = strings.Split(args[i+1], ",")
				i += 2
			} else {
				return respond("", "--tags requires a value", 1)
			}
		default:
			if strings.HasPrefix(args[i], "--") {
				return respond("", fmt.Sprintf("unknown flag %q (supported: --type, --tags)", args[i]), 1)
			}
			contentParts = append(contentParts, args[i])
			i++
		}
	}
	content = strings.Join(contentParts, " ")

	if content == "" {
		return respond("", "Usage: mpm lesson add <content>", 1)
	}
	if err := internal.ValidateLessonType(lessonType); err != nil {
		return respond("", err.Error(), 1)
	}

	lessonStore := internal.NewLessonStoreForDM(getDBConcrete())
	if lessonStore == nil {
		if jsonOutput {
			data, _ := json.Marshal(map[string]interface{}{"success": false, "error": "Failed to initialize lesson store: database not available"})
			fmt.Println(string(data))
		} else {
			respond("", "Failed to initialize lesson store: database not available", 1)
		}
		return 1
	}
	lesson, err := lessonStore.AddLesson(content, internal.LessonType(lessonType), tags, "")
	if err != nil {
		if jsonOutput {
			data, _ := json.Marshal(map[string]interface{}{"success": false, "error": fmt.Sprintf("Failed to add lesson: %v", err)})
			fmt.Println(string(data))
		} else {
			respond("", fmt.Sprintf("Failed to add lesson: %v", err), 1)
		}
		return 1
	}

	if jsonOutput {
		data, _ := json.Marshal(map[string]interface{}{
			"success":       true,
			"id":            lesson.ID,
			"type":          string(lesson.Type),
			"reinforcement": lesson.ReinforcementCount,
		})
		fmt.Println(string(data))
	} else {
		respond(fmt.Sprintf("Lesson added with ID: %s (reinforcement: %d)\n", lesson.ID, lesson.ReinforcementCount), "", 0)
	}
	return 0
}

func handleLessonList(args []string) int {
	lessonType := ""
	jsonOutput, args := ExtractJSONFlag(args)
	for _, arg := range args {
		if strings.HasPrefix(arg, "--type=") {
			lessonType = strings.TrimPrefix(arg, "--type=")
		}
	}
	if lessonType != "" {
		if err := internal.ValidateLessonType(lessonType); err != nil {
			return respond("", err.Error(), 1)
		}
	}

	lessonStore := internal.NewLessonStoreForDM(getDBConcrete())
	if lessonStore == nil {
		return respond("", "Failed to initialize lesson store: database not available", 1)
	}
	lessons, listErr := lessonStore.ListLessons(internal.LessonType(lessonType))
	if listErr != nil {
		return respond("", fmt.Sprintf("Failed to list lessons: %v", listErr), 1)
	}

	if len(lessons) == 0 {
		if jsonOutput {
			fmt.Println(`{"lessons": [], "message": "No lessons stored"}`)
		} else {
			render.Heading(os.Stdout, "Lesson list")
			render.Plain(os.Stdout, "No lessons stored.")
		}
		return 0
	}

	if jsonOutput {
		type lessonEntry struct {
			ID        string   `json:"id"`
			Type      string   `json:"type"`
			Content   string   `json:"content"`
			Tags      []string `json:"tags"`
			CreatedAt string   `json:"created_at"`
		}
		result := make([]lessonEntry, 0, len(lessons))
		for _, l := range lessons {
			result = append(result, lessonEntry{
				ID:        l.ID,
				Type:      string(l.Type),
				Content:   l.Content,
				Tags:      l.Tags,
				CreatedAt: l.Created,
			})
		}
		data, _ := json.Marshal(map[string]interface{}{"lessons": result})
		fmt.Println(string(data))
		return 0
	}

	render.Heading(os.Stdout, "Lesson list")
	render.Plain(os.Stdout, fmt.Sprintf("%d lessons\n", len(lessons)))
	for _, l := range lessons {
		typeLabel := string(l.Type)
		render.Plain(os.Stdout, fmt.Sprintf("[%s] [%s] %s", l.ID, typeLabel, l.Created[:10]))
		snippet := l.Content
		if len(snippet) > 100 {
			snippet = snippet[:100] + "..."
		}
		render.Plain(os.Stdout, fmt.Sprintf("    %s", snippet))
	}
	return 0
}

func handleLessonSearch(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm lesson search <query> [--json]", 1)
	}

	query := args[0]
	jsonOutput, queryArgs := ExtractJSONFlag(args)
	if len(queryArgs) > 0 {
		query = strings.Join(queryArgs, " ")
	}

	lessonStore := internal.NewLessonStoreForDM(getDBConcrete())
	if lessonStore == nil {
		return respond("", "Failed to initialize lesson store: database not available", 1)
	}
	results, searchErr := lessonStore.SearchLessons(query, 20)
	if searchErr != nil {
		return respond("", fmt.Sprintf("Search failed: %v", searchErr), 1)
	}

	if len(results) == 0 {
		if jsonOutput {
			data, _ := json.Marshal(map[string]interface{}{"query": query, "results": []interface{}{}, "message": "No lessons found"})
			fmt.Println(string(data))
		} else {
			respond("No lessons found.\n", "", 0)
		}
		return 0
	}

	if jsonOutput {
		type lessonResult struct {
			ID        string   `json:"id"`
			Type      string   `json:"type"`
			Content   string   `json:"content"`
			Tags      []string `json:"tags"`
			CreatedAt string   `json:"created_at"`
		}
		result := make([]lessonResult, 0, len(results))
		for _, l := range results {
			result = append(result, lessonResult{
				ID:        l.ID,
				Type:      string(l.Type),
				Content:   l.Content,
				Tags:      l.Tags,
				CreatedAt: l.Created,
			})
		}
		data, _ := json.Marshal(map[string]interface{}{"query": query, "results": result})
		fmt.Println(string(data))
		return 0
	}

	var output strings.Builder
	output.WriteString(fmt.Sprintf("Found %d lessons:\n\n", len(results)))
	for _, l := range results {
		snippet := l.Content
		if len(snippet) > 100 {
			snippet = snippet[:100] + "..."
		}
		output.WriteString(fmt.Sprintf("[%s] [%s]\n    %s\n\n", l.ID, l.Type, snippet))
	}

	return respond(output.String(), "", 0)
}

func handleLessonGet(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm lesson get <id>", 1)
	}

	id := args[0]
	lessonStore := internal.NewLessonStoreForDM(getDBConcrete())
	if lessonStore == nil {
		return respond("", "Failed to initialize lesson store: database not available", 1)
	}
	lesson, getErr := lessonStore.GetLesson(id)
	if getErr != nil {
		return respond("", fmt.Sprintf("Lesson not found: %s\n", id), 1)
	}

	var output strings.Builder
	output.WriteString(fmt.Sprintf("ID:      %s\n", lesson.ID))
	output.WriteString(fmt.Sprintf("Type:    %s\n", lesson.Type))
	output.WriteString(fmt.Sprintf("Created: %s\n", lesson.Created))
	output.WriteString(fmt.Sprintf("Reinforcement: %d\n", lesson.ReinforcementCount))
	if len(lesson.Tags) > 0 {
		output.WriteString(fmt.Sprintf("Tags:    %s\n", strings.Join(lesson.Tags, ", ")))
	}
	output.WriteString("\n")
	output.WriteString(lesson.Content)
	output.WriteString("\n")

	return respond(output.String(), "", 0)
}

func handleLessonDelete(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm lesson delete <id>", 1)
	}

	id := args[0]
	lessonStore := internal.NewLessonStoreForDM(getDBConcrete())
	if lessonStore == nil {
		return respond("", "Failed to initialize lesson store: database not available", 1)
	}
	err := lessonStore.DeleteLesson(id)
	if err != nil {
		return respond("", fmt.Sprintf("Failed to delete lesson: %v", err), 1)
	}

	return respond(fmt.Sprintf("Lesson deleted (soft): %s — use 'mpm lesson restore %s' to undo.\n", id, id), "", 0)
}

func handleLessonRestore(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm lesson restore <id>", 1)
	}

	id := args[0]
	lessonStore := internal.NewLessonStoreForDM(getDBConcrete())
	if lessonStore == nil {
		return respond("", "Failed to initialize lesson store: database not available", 1)
	}
	err := lessonStore.RestoreLesson(id)
	if err != nil {
		return respond("", fmt.Sprintf("Failed to restore lesson: %v", err), 1)
	}

	return respond(fmt.Sprintf("Lesson restored: %s\n", id), "", 0)
}

func handleLessonShred(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm lesson shred <id>", 1)
	}

	id := args[0]
	lessonStore := internal.NewLessonStoreForDM(getDBConcrete())
	if lessonStore == nil {
		return respond("", "Failed to initialize lesson store: database not available", 1)
	}
	// 2026-09-10 lifecycle fix: `mpm lesson shred` is now the
	// irreversible hard-delete path. Previously it called
	// DeleteLesson (which was already destructive); with the new
	// contract DeleteLesson is the soft delete, ShredLesson is the
	// hard delete. Without this split, `mpm lesson shred` would
	// silently become a soft delete.
	err := lessonStore.ShredLesson(id)
	if err != nil {
		return respond("", fmt.Sprintf("Failed to shred lesson: %v", err), 1)
	}

	return respond(fmt.Sprintf("Lesson shredded (permanent): %s\n", id), "", 0)
}

// handleLessonProposals lists the lesson proposals waiting for review.
//
// The default status is "pending": the operator's question is "what is
// waiting for me?", and a proposal they already acted on is not
// waiting. "--status all" shows the whole history.
//
// The proposals are the UNTRUSTED surface: a proposal is not a lesson,
// and it is not in the lesson search, the lesson list or the wake
// context. It becomes a lesson only through `mpm lesson promote`.
// proposalDisplayContent renders a proposal's content for the operator's
// review.
//
// A proposal made by the COMPACTION carries the synthesis as a JSON
// envelope {title, body} — the same shape a promoted lesson holds, so a
// promoted derivation is byte-identical to what the compaction wrote
// before the trust correction. Printed raw, that envelope is one long
// JSON line, and the operator is being asked to decide whether it
// becomes trusted knowledge.
//
// The envelope is therefore rendered as "title — body" for DISPLAY only.
// The stored content, and the `content` field of the JSON output, are
// unchanged: this is a rendering, never a transformation of the record.
//
// A proposal that is not an envelope — the ordinary agent-proposed fact
// — is printed exactly as it was submitted.
func proposalDisplayContent(content string) string {
	var env struct {
		Title string `json:"title"`
		Body  string `json:"body"`
	}
	if err := json.Unmarshal([]byte(content), &env); err != nil {
		return content
	}
	if env.Title == "" && env.Body == "" {
		return content
	}
	switch {
	case env.Title == "":
		return env.Body
	case env.Body == "":
		return env.Title
	default:
		return env.Title + " — " + env.Body
	}
}

func handleLessonProposals(args []string) int {
	jsonOutput, args := ExtractJSONFlag(args)
	status := "pending"
	limit := 0
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--status":
			if i+1 < len(args) {
				status = args[i+1]
				i++
			}
		case strings.HasPrefix(arg, "--status="):
			status = strings.TrimPrefix(arg, "--status=")
		case strings.HasPrefix(arg, "--limit="):
			if n, err := strconv.Atoi(strings.TrimPrefix(arg, "--limit=")); err == nil {
				limit = n
			}
		}
	}
	if status == "all" {
		status = ""
	}

	lessonStore := internal.NewLessonStoreForDM(getDBConcrete())
	if lessonStore == nil {
		return respond("", "Failed to initialize lesson store: database not available", 1)
	}
	proposals, err := lessonStore.ListLessonProposals(status, limit)
	if err != nil {
		return respond("", fmt.Sprintf("Failed to list lesson proposals: %v", err), 1)
	}

	if jsonOutput {
		type proposalEntry struct {
			ID           string   `json:"id"`
			Type         string   `json:"type"`
			Content      string   `json:"content"`
			Tags         []string `json:"tags"`
			Created      string   `json:"created"`
			Status       string   `json:"status"`
			ProposedBy   string   `json:"proposed_by_framework"`
			ProposedModel string  `json:"proposed_by_model"`
			SessionID    string   `json:"source_session_id"`
			PromotedID   string   `json:"promoted_lesson_id"`
		}
		entries := make([]proposalEntry, 0, len(proposals))
		for _, p := range proposals {
			entries = append(entries, proposalEntry{
				ID:            p.ID,
				Type:          p.Type,
				Content:       p.Content,
				Tags:          p.Tags,
				Created:       p.Created,
				Status:        p.Status,
				ProposedBy:    p.FrameworkName,
				ProposedModel: p.ModelName,
				SessionID:     p.SourceSessionID,
				PromotedID:    p.PromotedLessonID,
			})
		}
		data, _ := json.Marshal(map[string]interface{}{
			"success":   true,
			"count":     len(entries),
			"proposals": entries,
		})
		fmt.Println(string(data))
		return 0
	}

	if len(proposals) == 0 {
		render.Heading(os.Stdout, "Lesson proposals")
		render.Plain(os.Stdout, "No lesson proposals are waiting for review.")
		return 0
	}

	render.Heading(os.Stdout, "Lesson proposals")
	for _, p := range proposals {
		origin := p.FrameworkName
		if origin == "" {
			origin = "unknown"
		}
		render.Plain(os.Stdout, fmt.Sprintf(
			"  %s  [%s]  %s\n      %s\n      proposed by %s at %s",
			p.ID, p.Status, p.Type, proposalDisplayContent(p.Content), origin, p.Created))
	}
	render.Plain(os.Stdout, "")
	render.Plain(os.Stdout, "  Accept one with:  mpm lesson promote <id>")
	render.Plain(os.Stdout, "  Decline one with: mpm lesson reject <id>")
	return 0
}

// handleLessonPromote accepts a pending proposal and turns it into a
// TRUSTED lesson.
//
// This is the operator's decision, and it is the only path from a
// proposal to a lesson. The lesson is written through the same store
// path `mpm lesson add` uses, so it gets the same scanning, dedup and
// provenance treatment as any other lesson.
func handleLessonPromote(args []string) int {
	jsonOutput, args := ExtractJSONFlag(args)
	if len(args) == 0 {
		return respond("", "Usage: mpm lesson promote <id>", 1)
	}
	id := args[0]

	lessonStore := internal.NewLessonStoreForDM(getDBConcrete())
	if lessonStore == nil {
		return respond("", "Failed to initialize lesson store: database not available", 1)
	}
	lesson, proposal, credited, err := lessonStore.PromoteLessonProposal(id)
	if err != nil {
		return respond("", fmt.Sprintf("Failed to promote lesson proposal: %v", err), 1)
	}

	if jsonOutput {
		data, _ := json.Marshal(map[string]interface{}{
			"success":          true,
			"proposal_id":      proposal.ID,
			"lesson_id":        lesson.ID,
			"type":             string(lesson.Type),
			"reinforcement":    lesson.ReinforcementCount,
			"status":           proposal.Status,
			"credited_sources": credited,
		})
		fmt.Println(string(data))
		return 0
	}
	return respond(fmt.Sprintf(
		"Lesson proposal %s promoted. Lesson ID: %s (type: %s, reinforcement: %d, credited sources: %d)\n",
		proposal.ID, lesson.ID, lesson.Type, lesson.ReinforcementCount, credited), "", 0)
}

// handleLessonReject declines a pending proposal.
//
// The proposal's content is kept — a declined proposal records what the
// agent thought — but it leaves the review queue.
func handleLessonReject(args []string) int {
	jsonOutput, args := ExtractJSONFlag(args)
	if len(args) == 0 {
		return respond("", "Usage: mpm lesson reject <id>", 1)
	}
	id := args[0]

	lessonStore := internal.NewLessonStoreForDM(getDBConcrete())
	if lessonStore == nil {
		return respond("", "Failed to initialize lesson store: database not available", 1)
	}
	proposal, err := lessonStore.RejectLessonProposal(id)
	if err != nil {
		return respond("", fmt.Sprintf("Failed to reject lesson proposal: %v", err), 1)
	}

	if jsonOutput {
		data, _ := json.Marshal(map[string]interface{}{
			"success":     true,
			"proposal_id": proposal.ID,
			"status":      proposal.Status,
		})
		fmt.Println(string(data))
		return 0
	}
	return respond(fmt.Sprintf("Lesson proposal %s rejected (the record is kept, and it is no longer pending).\n",
		proposal.ID), "", 0)
}

func handleLessonStats() int {
	lessonStore := internal.NewLessonStoreForDM(getDBConcrete())
	if lessonStore == nil {
		return respond("", "Failed to initialize lesson store: database not available", 1)
	}
	stats, statsErr := lessonStore.GetLessonStats()
	if statsErr != nil {
		return respond("", fmt.Sprintf("Failed to get stats: %v", statsErr), 1)
	}

	var output strings.Builder
	output.WriteString("Lesson Statistics:\n\n")
	output.WriteString(fmt.Sprintf("Total: %d\n", stats["total_lessons"]))
	output.WriteString("\nBy type:\n")
	if byType, ok := stats["by_type"].(map[string]int); ok {
		for t, count := range byType {
			output.WriteString(fmt.Sprintf("  %s: %d\n", t, count))
		}
	}
	return respond(output.String(), "", 0)
}
