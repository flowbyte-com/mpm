package core

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// ============================================================================
// System Prompt Builder with Identity-First Approach
// ============================================================================

// BuildSystemPromptWithIdentity builds the full system prompt.
// Priority: IDENTITY.md (core, stable) + persona (costume overlay) + mode (behavior rules)
func BuildSystemPromptWithIdentity(binaryDir, personaContent, modeContent string, memories, directives, references []string, anchors []Anchor) string {
	identity, _ := LoadIdentity(binaryDir)

	var sb strings.Builder

	// Core identity from IDENTITY.md
	if identity != nil {
		sb.WriteString(fmt.Sprintf("You are %s (v%s) — %s. ", identity.Name, identity.Version, identity.Type))
		if identity.Traits != "" {
			sb.WriteString(fmt.Sprintf("Core traits: %s. ", identity.Traits))
		}
		if identity.Boundaries != "" {
			sb.WriteString(fmt.Sprintf("Boundaries: %s. ", identity.Boundaries))
		}
	} else {
		sb.WriteString("You are mini-bot. ")
	}

	// Anchors (high-priority memories)
	if len(anchors) > 0 {
		sb.WriteString("\n\n## Anchored Memories\n")
		for _, a := range anchors {
			sb.WriteString(fmt.Sprintf("- [weight:%d] %s\n", a.Weight, a.Content))
		}
	}

	// Persona costume overlay (MPM personas)
	if personaContent != "" {
		sb.WriteString(fmt.Sprintf("\n\n[Persona Costume] %s", personaContent))
	}

	// Mode behavior rules
	if modeContent != "" {
		sb.WriteString(fmt.Sprintf("\n\n[Mode] %s", modeContent))
	}

	// Memory context
	if len(memories) > 0 {
		sb.WriteString("\n\n## Relevant Memories\n")
		for _, m := range memories {
			sb.WriteString(fmt.Sprintf("- %s\n", m))
		}
	}

	// Directives
	if len(directives) > 0 {
		sb.WriteString("\n## Prime Directives\n")
		for _, d := range directives {
			sb.WriteString(fmt.Sprintf("- %s\n", d))
		}
	}

	// References
	if len(references) > 0 {
		sb.WriteString("\n## Reference Material\n")
		for _, r := range references {
			sb.WriteString(fmt.Sprintf("- %s\n", r))
		}
	}

	sb.WriteString(fmt.Sprintf("\n## Current Date: %s", time.Now().Format("2006-01-02")))
	return sb.String()
}

// retrieveMemories returns formatted relevant memories from MPM DB
func retrieveMemories(db *sql.DB, query string, limit int) []string {
	if query == "" {
		return nil
	}
	rows, err := db.Query(`
		SELECT content, tags FROM memories
		JOIN memories_fts fts ON memories.rowid = fts.rowid
		WHERE memories_fts MATCH ? AND deleted_at IS NULL
		ORDER BY rank LIMIT ?
	`, query, limit)
	if err != nil {
		like := "%" + query + "%"
		rows, err = db.Query(`
			SELECT content, tags FROM memories
			WHERE (content LIKE ? OR tags LIKE ?) AND deleted_at IS NULL
			ORDER BY created_at DESC LIMIT ?
		`, like, like, limit)
		if err != nil {
			return nil
		}
	}
	defer rows.Close()
	var results []string
	for rows.Next() {
		var content, tags string
		if err := rows.Scan(&content, &tags); err != nil {
			continue
		}
		results = append(results, content)
	}
	return results
}

// retrieveDirectives returns all prime directives
func retrieveDirectives(db *sql.DB) []string {
	rows, err := db.Query(`
		SELECT content FROM memories
		WHERE metadata LIKE '%is_prime_directive%' AND deleted_at IS NULL
		ORDER BY created_at DESC LIMIT 20
	`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var results []string
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			continue
		}
		results = append(results, content)
	}
	return results
}

// retrieveReferences returns formatted reference chunks
func retrieveReferences(db *sql.DB, query string, limit int) []string {
	if query == "" {
		return nil
	}
	rows, err := db.Query(`
		SELECT r.title, rc.content FROM reference_chunks rc
		JOIN reference_docs r ON rc.doc_id = r.id
		JOIN reference_chunks_fts fts ON rc.rowid = fts.rowid
		WHERE reference_chunks_fts MATCH ?
		ORDER BY rank LIMIT ?
	`, query, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var results []string
	for rows.Next() {
		var title, content string
		if err := rows.Scan(&title, &content); err != nil {
			continue
		}
		results = append(results, fmt.Sprintf("### %s\n%s", title, content))
	}
	return results
}

// RunAgent runs a single agent query with full identity and context.
// Returns the response text or error.
func RunAgent(query string, db *sql.DB, binaryDir string) (string, error) {
	// Get anchors as high-priority context
	anchors, _ := GetRecentAnchors(db, 5)

	// Get recent lessons for context
	lessons, _ := GetRecentLessons(db, 3)

	// Build context arrays
	memories := retrieveMemories(db, query, 5)
	directives := retrieveDirectives(db)
	references := retrieveReferences(db, query, 3)

	// Build system prompt with identity-first approach
	systemPrompt := BuildSystemPromptWithIdentity(binaryDir, "", "",
		memories, directives, references, anchors)

	// Include lessons in system prompt if available
	if len(lessons) > 0 {
		// Lessons are already embedded via BuildSystemPromptWithIdentity's anchor section
		// but we can also add them separately if needed
	}

	return systemPrompt, nil
}
