package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/flowbyte-com/mpm-core/config"
	"github.com/flowbyte-com/mpm-core/usererror"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

func handleSession(args []string) int {
	if len(args) < 1 {
		return handleSessionHelp()
	}

	subCmd := args[0]
	switch subCmd {
	case "help":
		return handleSessionHelp()
	case "add":
		return handleSessionAdd(args[1:])
	case "search":
		return handleSessionSearch(args[1:])
	case "show":
		return handleSessionShow(args[1:])
	case "shred":
		if len(args) < 2 {
			return handleSessionHelp()
		}
		return handleShredSession(args[1])
	case "list":
		return handleSessionList(args[1:])
	default:
		return handleSessionHelp()
	}
}

func handleSessionHelp() int {
	output := `mpm session - Session operations

Usage:
  mpm session add <content>      Add a new session entry (returns ID)
  mpm session search <query>     Search sessions
  mpm session show <id>          Show session by ID
  mpm session shred <id>         Secure delete session
  mpm session list               List recent sessions

Examples:
  mpm session add "Session notes for project discussion"
  mpm session search "golang"
  mpm session show abc123
  mpm session shred abc123
`
	return respond(output, "", 0)
}

func handleSessionAdd(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm session add <content>", 1)
	}

	content := strings.Join(args, " ")
	store := getMemoryStore()

	// Inject active mode/persona from config files
	injectActiveContext()
	defer clearActiveContext()

	memMetadata := map[string]interface{}{
		"provenance": map[string]interface{}{
			"source":  "human",
			"model":   "direct",
			"compute": "absolute",
			"agent":   "mpm_cli",
			"persona": "operator",
		},
	}
	if activeMode != "" {
		memMetadata["active_mode"] = activeMode
	}
	if activePersona != "" {
		memMetadata["active_persona"] = activePersona
	}

	// Add to session collection
	mem, err := store.AddMemory(content, "session", nil, memMetadata, "", "cli")
	if err != nil {
		return respond("", fmt.Sprintf("Failed to add session: %v", err), 1)
	}

	return respond(fmt.Sprintf("Session added with ID: %s\n", mem.ID), "", 0)
}

// handleWake returns context from the last active session.
// Displays mode, persona, recent topics, and recent memories.
func handleWake(args []string) int {
	dm := getDB()
	if dm == nil {
		return 1
	}

	// Parse --strict flag
	strictMode := false
	cleanArgs := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--strict" {
			strictMode = true
		} else {
			cleanArgs = append(cleanArgs, a)
		}
	}

	var sessionID string

	// Try to get the most recent explicit session record
	session, err := dm.GetLastSession()
	if err == nil && session != nil {
		sessionID, _ = session["session_id"].(string)
	}

	// Pull the latest handoff (read or unread) so the agent can see
	// what the previous session was doing, what it committed to, and
	// what was still unresolved. Distinct from the (currently empty)
	// sessions table — handoffs are bootstrap data, sessions are
	// content logs. The mpm call read_wake_context path marks the
	// handoff as read automatically; mpm wake leaves the read state
	// alone so the agent can browse the history.
	handoff, herr := dm.GetLatestHandoff()
	_ = herr // ignore: no handoffs is fine, just skip the block below

	// Strict mode: bypass fallback queries, rely solely on explicit session
	if strictMode && sessionID == "" {
		fmt.Println("No previous session found.")
		return 1
	}

	// Query all recent memories (any collection, not just 'session').
	// RECOMMENDED 9: structural epistemology collections (decisions,
	// theories) are excluded here because the wake-context render
	// surfaces them in their own sections. Including them in the
	// general recent_memories stream would duplicate the same row
	// across two sections. See internal/core/wake_context.go
	// recentMemories for the matching filter.
	var memories []map[string]interface{}
	rows, err := dm.SQLDB().Query(`
		SELECT id, collection, content, tags, metadata, created_at
		FROM memories
		WHERE deleted_at IS NULL
		  AND collection NOT IN ('decisions', 'theories')
		ORDER BY created_at DESC
		LIMIT 10
	`)
	if err == nil {
		defer rows.Close()
		scanErr := func() error {
			for rows.Next() {
				var memID, collection, content, createdAt string
				var tagsJSON, metadataJSON sql.NullString
				if err := rows.Scan(&memID, &collection, &content, &tagsJSON, &metadataJSON, &createdAt); err != nil {
					return fmt.Errorf("scanning wake context memory row: %w", err)
				}
			var tags []string
			var memMeta map[string]interface{}
			if tagsJSON.Valid {
				json.Unmarshal([]byte(tagsJSON.String), &tags)
			}
			if metadataJSON.Valid {
				json.Unmarshal([]byte(metadataJSON.String), &memMeta)
			}
			memories = append(memories, map[string]interface{}{
				"id":         memID,
				"collection": collection,
				"content":    content,
				"tags":       tags,
				"metadata":   memMeta,
				"created_at": createdAt,
			})
		}
			return nil
		}()
		if scanErr != nil {
			usererror.Warn("handleWake: %v", scanErr)
			return 1
		}
	}

	// Query recent lessons for additional cold-start context
	var lessons []map[string]interface{}
	lrows, err := dm.SQLDB().Query(`
		SELECT id, type, content, tags, created
		FROM lessons
		ORDER BY created DESC
		LIMIT 5
	`)
	if err == nil {
		defer lrows.Close()
		scanErrL := func() error {
			for lrows.Next() {
				var lessonID, lessonType, content, tagsJSON, created string
				if err := lrows.Scan(&lessonID, &lessonType, &content, &tagsJSON, &created); err != nil {
					return fmt.Errorf("scanning wake context lesson row: %w", err)
				}
			var tagList []string
			if tagsJSON != "" {
				json.Unmarshal([]byte(tagsJSON), &tagList)
			}
			lessons = append(lessons, map[string]interface{}{
				"id":      lessonID,
				"type":    lessonType,
				"content": "[Lesson] " + content,
				"tags":    tagList,
				"created": created,
			})
		}
			return nil
		}()
		if scanErrL != nil {
			usererror.Warn("handleWake: %v", scanErrL)
			return 1
		}
	}

	// Identity fallback: read active.json for persona and modes
	var activeMode, activePersona string
	mpmDir := config.GetMPMDir()
	activePath := filepath.Join(mpmDir, "active.json")
	if data, err := os.ReadFile(activePath); err == nil {
		type activeState struct {
			Persona string   `json:"persona"`
			Modes   []string `json:"modes"`
		}
		var active activeState
		if json.Unmarshal(data, &active) == nil {
			activePersona = active.Persona
			if len(active.Modes) > 0 {
				activeMode = strings.Join(active.Modes, ", ")
			}
		}
	}

	// Pre-scan for --json
	jsonOutput, _ := ExtractJSONFlag(cleanArgs)

	// Shared struct definitions for output
	type memoryRef struct {
		ID        string `json:"id"`
		Content   string `json:"content"`
		CreatedAt string `json:"created_at"`
	}
	type workRef struct {
		ID           string `json:"id"`
		Title        string `json:"title"`
		Status       string `json:"status"`
		Verification string `json:"verification,omitempty"`
		Pointer      string `json:"pointer"`
	}
	type wakeResult struct {
		SessionID      string               `json:"session_id"`
		ActiveMode     string               `json:"active_mode"`
		ActivePersona  string               `json:"active_persona"`
		RecentTopics   []string             `json:"recent_topics"`
		RecentMemories []memoryRef          `json:"recent_memories"`
		CompletedWorks []workRef            `json:"completed_works"`
		LastHandoff    *mpminternal.Handoff `json:"last_handoff,omitempty"`
	}

	// Collect memory references; topics are pulled from the topics table
	// directly via the shared GetRecentUserTopics helper so the structural
	// filter (decisions, theories, any future auto-created anchors) is
	// applied identically to the read_wake_context path. Previously the
	// CLI derived topics from per-memory topic_memberships, which silently
	// surfaced structural topics whenever a recent memory was linked to
	// them — live DB had 27 memberships on `decisions` and 10 on `theories`.
	memRefs := make([]memoryRef, 0)

	for _, mem := range memories {
		memRefs = append(memRefs, memoryRef{
			ID:        mem["id"].(string),
			Content:   mem["content"].(string),
			CreatedAt: mem["created_at"].(string),
		})
	}

	for _, l := range lessons {
		memRefs = append(memRefs, memoryRef{
			ID:        l["id"].(string),
			Content:   l["content"].(string),
			CreatedAt: l["created"].(string),
		})
	}

	topics, err := dm.GetRecentUserTopics(5)
	if err != nil {
		usererror.Warn("handleWake: failed to get recent user topics: %v", err)
		return 1
	}

	// Query recently completed work — mirrors gatherCompletedWorks in
	// internal/core/wake_context.go so the CLI projection is consistent
	// with `mpm call mpm_context read_wake_context`. RECOMMENDED 10 fix:
	// the field had been populated on WakeContextData but no public
	// surface actually rendered it, leaving it as dead projection data.
	// The query is bounded to 5 and ordered completed_at DESC for
	// deterministic, stable output. Title truncated to 120 chars to match
	// the MCP path.
	var completedRefs []workRef
	crows, cErr := dm.SQLDB().Query(`
		SELECT id, title, status, COALESCE(verification, '')
		FROM works
		WHERE status = 'done'
		ORDER BY COALESCE(completed_at, updated_at) DESC, updated_at DESC
		LIMIT 5
	`)
	if cErr == nil {
		for crows.Next() {
			var w workRef
			if scanErr := crows.Scan(&w.ID, &w.Title, &w.Status, &w.Verification); scanErr == nil {
				if len(w.Title) > 120 {
					w.Title = w.Title[:120]
				}
				w.Pointer = "mpm://work/" + w.ID
				completedRefs = append(completedRefs, w)
			}
		}
		crows.Close()
	} else {
		usererror.Warn("handleWake: completed works query: %v", cErr)
	}
	// Always emit non-nil slice for predictable JSON shape.
	if completedRefs == nil {
		completedRefs = []workRef{}
	}

	result := wakeResult{
		SessionID:       sessionID,
		ActiveMode:      activeMode,
		ActivePersona:   activePersona,
		RecentTopics:    topics,
		RecentMemories:  memRefs,
		LastHandoff:     handoff,
		CompletedWorks:  completedRefs,
	}

	if jsonOutput {
		data, _ := json.Marshal(result)
		fmt.Println(string(data))
		return 0
	}

	// No context at all — human-readable empty state. Completed works
// counts toward context so a fresh session that has just shipped
// something does not see "No previous session found".
	if len(memRefs) == 0 && len(completedRefs) == 0 && activeMode == "" && activePersona == "" {
		fmt.Println("No previous session found.")
		return 0
	}

	// Human-readable output
	fmt.Println("╭─ Last Session ──────────────────────────────────────────────╮")
	if activeMode != "" {
		fmt.Printf("│ Mode:     %-42s │\n", activeMode)
	}
	if activePersona != "" {
		fmt.Printf("│ Persona:  %-42s │\n", activePersona)
	}
	if len(topics) > 0 {
		shown := topics
		if len(shown) > 3 {
			shown = shown[:3]
		}
		topicStr := strings.Join(shown, ", ")
		if len(topics) > 3 {
			topicStr += fmt.Sprintf(" (+%d more)", len(topics)-3)
		}
		fmt.Printf("│ Topics:   %-42s │\n", topicStr)
	}

	if len(memRefs) > 0 {
		fmt.Println("│                                                             │")
		fmt.Println("│ Recent context:                                            │")
		for _, ref := range memRefs {
			content := ref.Content
			if len(content) > 54 {
				content = content[:54] + "…"
			}
			fmt.Printf("│   • %-53s │\n", content)
		}
	}
	if len(completedRefs) > 0 {
		fmt.Println("│                                                             │")
		fmt.Println("│ Recently completed work:                                    │")
		for _, w := range completedRefs {
			title := w.Title
			if len(title) > 54 {
				title = title[:54] + "…"
			}
			fmt.Printf("│   ✓ %-51s │\n", title)
		}
	}
	if handoff != nil {
		fmt.Println("│                                                             │")
		fmt.Println("│ Previous session handoff:                                  │")
		summary := handoff.Summary
		if len(summary) > 54 {
			summary = summary[:54] + "…"
		}
		fmt.Printf("│   [%s] %-49s │\n", handoff.EndedState, summary)
		if handoff.EndedAt > 0 {
			ts := mpminternal.FormatUnixSeconds(handoff.EndedAt)
			fmt.Printf("│   ended %s%-40s │\n", ts, "")
		}
		if len(handoff.Commitments) > 0 {
			fmt.Printf("│   %-54s │\n", "commitments:")
			for _, c := range handoff.Commitments {
				if len(c) > 52 {
					c = c[:52] + "…"
				}
				fmt.Printf("│     - %-50s │\n", c)
			}
		}
		if len(handoff.OpenQuestions) > 0 {
			fmt.Printf("│   %-54s │\n", "open:")
			for _, q := range handoff.OpenQuestions {
				if len(q) > 52 {
					q = q[:52] + "…"
				}
				fmt.Printf("│     ? %-50s │\n", q)
			}
		}
	}
	fmt.Println("╰─────────────────────────────────────────────────────────────╯")
	return 0
}
func handleSessionSearch(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm session search <query>", 1)
	}

	query := strings.Join(args, " ")
	store := getMemoryStore()

	memories, err := store.SearchSessions(query, 20)
	if err != nil {
		return respond("", fmt.Sprintf("Search failed: %v", err), 1)
	}

	if len(memories) == 0 {
		return respond("No sessions found.\n", "", 0)
	}

	var output strings.Builder
	output.WriteString(fmt.Sprintf("Found %d sessions:\n\n", len(memories)))

	for _, mem := range memories {
		snippet := mem.Content
		metaJSON, _ := json.Marshal(mem.Metadata)
		if preamble := mpminternal.ProvenancePreamble(string(metaJSON)); preamble != "" {
			snippet = preamble + "\n" + snippet
		}
		if len(snippet) > 500 {
			snippet = snippet[:500] + "..."
		}
		snippet = strings.ReplaceAll(snippet, "\n", " ")

		output.WriteString(fmt.Sprintf("[%s] %s\n", mem.ID, datePrefixSec(mem.CreatedAt)))
		output.WriteString(fmt.Sprintf("    %s\n\n", snippet))
	}

	return respond(output.String(), "", 0)
}

func handleSessionShow(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm session show <id>", 1)
	}

	id := args[0]
	store := getMemoryStore()

	mem, err := store.GetByID(id, "session")
	if err != nil || mem == nil {
		return respond("", fmt.Sprintf("Session not found: %s\n", id), 1)
	}

	var output strings.Builder
	output.WriteString(fmt.Sprintf("ID:      %s\n", mem.ID))
	output.WriteString(fmt.Sprintf("Created: %s\n\n", mpminternal.FormatUnixSeconds(mem.CreatedAt)))
	output.WriteString(mem.Content)
	output.WriteString("\n")

	return respond(output.String(), "", 0)
}

func handleSessionList(args []string) int {
	store := getMemoryStore()
	if store == nil {
		return respond("", "Failed to initialize memory store", 1)
	}

	rows, err := store.DB.Query(`
		SELECT id, content, created_at
		FROM memories
		WHERE collection = 'session' AND deleted_at IS NULL
		ORDER BY created_at DESC
		LIMIT 20
	`)
	if err != nil {
		return respond("", fmt.Sprintf("Failed to list sessions: %v", err), 1)
	}
	defer rows.Close()

	type sessionItem struct {
		ID      string
		Content string
		Created string
	}
	var sessions []sessionItem
	scanErr := func() error {
		for rows.Next() {
			var s sessionItem
			if err := rows.Scan(&s.ID, &s.Content, &s.Created); err != nil {
				return fmt.Errorf("scanning session list row: %w", err)
			}
			sessions = append(sessions, s)
		}
		return nil
	}()
	if scanErr != nil {
		usererror.Warn("handleSessionList: %v", scanErr)
		return 1
	}

	if len(sessions) == 0 {
		return respond("No sessions stored.\n", "", 0)
	}

	var output strings.Builder
	output.WriteString(fmt.Sprintf("Recent %d sessions:\n\n", len(sessions)))

	for _, sess := range sessions {
		snippet := sess.Content
		if len(snippet) > 500 {
			snippet = snippet[:500] + "..."
		}
		snippet = strings.ReplaceAll(snippet, "\n", " ")

		output.WriteString(fmt.Sprintf("[%s] %s\n", sess.ID, datePrefix(sess.Created)))
		output.WriteString(fmt.Sprintf("    %s\n\n", snippet))
	}

	return respond(output.String(), "", 0)
}
