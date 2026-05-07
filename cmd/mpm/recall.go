package main

import (
	"database/sql"
	"flag"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"


	_ "github.com/mattn/go-sqlite3"
	mpminternal "mpm/internal"
)

// Pre-compiled regexes for stripMarkdown (avoid repeated recompilation)
var (
	stripMarkdownHeadings = regexp.MustCompile(`(?m)^#+\s*`)
	stripMarkdownBold     = regexp.MustCompile(`\*\*(.*?)\*\*`)
	stripMarkdownItalic   = regexp.MustCompile(`\*(.*?)\*`)
	stripMarkdownCode     = regexp.MustCompile("`([^`]+)`")
)

// =============================================================================
// mpm recall <query> — Search memories for relevant context
// =============================================================================

func handleRecall(args []string) int {
	fs := flag.NewFlagSet("recall", flag.ContinueOnError)
	since := fs.String("since", "", "Search memories since date (YYYY-MM-DD)")
	until := fs.String("until", "", "Search memories until date (YYYY-MM-DD)")
	limit := fs.Int("limit", 15, "Maximum results to return")
	fs.Usage = func() {
		fmt.Println("Usage: mpm recall [options] <query>")
		fmt.Println("\nRecall options:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args[1:]); err != nil {
		return 1
	}

	query := fs.Arg(0)
	if query == "" {
		fmt.Fprintf(os.Stderr, "Usage: mpm recall [options] <query>\n")
		return 1
	}
	query = strings.TrimSpace(query)
	if query == "" {
		fmt.Fprintf(os.Stderr, "Empty query.\n")
		return 1
	}

	dm, err := mpminternal.NewDatabaseManager("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ DB open failed: %v\n", err)
		return 1
	}
	defer dm.Close()

	db := dm.SQLDB()

	// Keyword search using LIKE + FTS5 fallback with time filters
	rows, err := keywordSearchWithTime(db, query, *since, *until, *limit)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Search failed: %v\n", err)
		return 1
	}
	defer rows.Close()

	type recallEntry struct {
		content     string
		sessionID   string
		createdAt   time.Time
		tags        string
		synthesized bool
	}
	var entries []recallEntry
	for rows.Next() {
		var id, content, createdAt string
		var nullableSessionID, nullableTags sql.NullString
		if err := rows.Scan(&id, &content, &nullableSessionID, &nullableTags, &createdAt); err != nil {
			continue
		}
		if content == "" {
			continue
		}
		sessionID := ""
		if nullableSessionID.Valid {
			sessionID = nullableSessionID.String
		}
		entry := recallEntry{
			content:   content,
			sessionID: sessionID,
			tags:      nullableTags.String,
		}
		if t, err := time.Parse(time.RFC3339, createdAt); err == nil {
			entry.createdAt = t
		}
		if strings.Contains(nullableTags.String, "synthesized") {
			entry.synthesized = true
		}
		entries = append(entries, entry)
	}

	if len(entries) == 0 {
		fmt.Printf("No memories found for: %s\n", query)
		return 0
	}

	// Sort by recency
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].createdAt.After(entries[j].createdAt)
	})

	cyan := "\033[36m"
	magenta := "\033[35m"
	reset := "\033[0m"
	bold := "\033[1m"

	fmt.Printf("%s%sRecall — %s%s\n\n", bold, cyan, query, reset)

	for i, e := range entries {
		age := ""
		if !e.createdAt.IsZero() {
			age = formatAge(e.createdAt)
		}

		content := e.content
		if len(content) > 250 {
			content = content[:250] + "..."
		}
		content = stripMarkdown(content)

		sessionTag := ""
		if e.sessionID != "" {
			sessionTagLen := 8
			if len(e.sessionID) < sessionTagLen {
				sessionTagLen = len(e.sessionID)
			}
			sessionTag = fmt.Sprintf(" %s[%s]%s", magenta, e.sessionID[:sessionTagLen], reset)
		}
		synthTag := ""
		if e.synthesized {
			synthTag = fmt.Sprintf(" %s[synth]%s", magenta, reset)
		}
		ageTag := ""
		if age != "" {
			ageTag = fmt.Sprintf(" %s%s%s", magenta, age, reset)
		}

		fmt.Printf("%s%d.%s %s%s%s\n    %s\n\n",
			cyan, i+1, reset,
			sessionTag, ageTag, synthTag,
			content)
	}

	fmt.Printf("%s%d results%s\n", cyan, len(entries), reset)
	return 0
}

func keywordSearch(db *sql.DB, query string, limit int) (*sql.Rows, error) {
	// Try FTS5 first — use table-level MATCH with JOIN pattern (same as QueryMemory)
	ftsQuery := `
		SELECT m.id, m.content, m.session_id, m.tags, m.created_at
		FROM memories m
		JOIN memories_fts fts ON m.rowid = fts.rowid
		WHERE memories_fts MATCH ? AND m.deleted_at IS NULL
		ORDER BY fts.rank
		LIMIT ?`

	rows, err := db.Query(ftsQuery, query, limit)
	if err == nil {
		return rows, nil
	}

	// FTS5 failed (malformed query or unavailable) — fallback to LIKE search
	fmt.Fprintf(os.Stderr, "⚠️ FTS5 query failed (query=%q): %v — falling back to LIKE\n", query, err)
	likePattern := "%" + query + "%"
	likeQuery := `
		SELECT id, content, session_id, tags, created_at
		FROM memories
		WHERE deleted_at IS NULL
		  AND (content LIKE ? OR tags LIKE ?)
		ORDER BY created_at DESC
		LIMIT ?`
	return db.Query(likeQuery, likePattern, likePattern, limit)
}

func keywordSearchWithTime(db *sql.DB, query, since, until string, limit int) (*sql.Rows, error) {
	// Try FTS5 first
	ftsQuery := `
		SELECT m.id, m.content, m.session_id, m.tags, m.created_at
		FROM memories m
		JOIN memories_fts fts ON m.rowid = fts.rowid
		WHERE memories_fts MATCH ? AND m.deleted_at IS NULL`

	args := []interface{}{query}

	if since != "" {
		ftsQuery += " AND m.created_at >= ?"
		args = append(args, since)
	}
	if until != "" {
		ftsQuery += " AND m.created_at <= ?"
		args = append(args, until+" 23:59:59")
	}

	ftsQuery += " ORDER BY fts.rank LIMIT ?"
	args = append(args, limit)

	rows, err := db.Query(ftsQuery, args...)
	if err == nil {
		return rows, nil
	}

	// FTS5 failed — fallback to LIKE
	likePattern := "%" + query + "%"
	likeQuery := `
		SELECT id, content, session_id, tags, created_at
		FROM memories
		WHERE deleted_at IS NULL
		  AND (content LIKE ? OR tags LIKE ?)`

	args = []interface{}{likePattern, likePattern}

	if since != "" {
		likeQuery += " AND created_at >= ?"
		args = append(args, since)
	}
	if until != "" {
		likeQuery += " AND created_at <= ?"
		args = append(args, until+" 23:59:59")
	}

	likeQuery += " ORDER BY created_at DESC LIMIT ?"
	args = append(args, limit)

	return db.Query(likeQuery, args...)
}

func stripMarkdown(s string) string {
	s = stripMarkdownHeadings.ReplaceAllString(s, "")
	s = stripMarkdownBold.ReplaceAllString(s, "$1")
	s = stripMarkdownItalic.ReplaceAllString(s, "$1")
	s = stripMarkdownCode.ReplaceAllString(s, "$1")
	return strings.TrimSpace(s)
}

func formatAge(t time.Time) string {
	age := time.Since(t)
	switch {
	case age < time.Minute:
		return "just now"
	case age < time.Hour:
		return fmt.Sprintf("%dm ago", int(age.Minutes()))
	case age < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(age.Hours()))
	default:
		days := int(age.Hours() / 24)
		if days == 1 {
			return "yesterday"
		}
		return fmt.Sprintf("%dd ago", days)
	}
}
