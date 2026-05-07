package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"

	"mpm/internal/config"
)

var skillNameRegexp = regexp.MustCompile(`<name>([\w-]+)</name>`)

// DailyReviewReport holds all three sections of the daily review
// Note: ansiBold, ansiReset, colorCyan, colorGreen, colorYellow, colorRed
// are declared in main.go and shared within the main package.
type DailyReviewReport struct {
	Memories  []MemoryEntry  `json:"memories"`
	Skills    []SkillEntry   `json:"skills"`
	Runtime   RuntimeConfig  `json:"runtime"`
	Generated string         `json:"generated_at"`
}

// MemoryEntry represents a single memory row
type MemoryEntry struct {
	ID        string `json:"id"`
	CreatedAt string `json:"created_at"`
	Content   string `json:"content"`
	Collection string `json:"collection"`
	Tags      string `json:"tags"`
}

// SkillEntry represents an installed skill
type SkillEntry struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Version     string `json:"version"`
	InstalledAt string `json:"installed_at"`
	Slug        string `json:"slug"`
}

// RuntimeConfig holds the runtime config from sessions.json
type RuntimeConfig struct {
	SessionID      string   `json:"session_id"`
	UpdatedAt      string   `json:"updated_at"`
	StartedAt      string   `json:"started_at"`
	ChatType       string   `json:"chat_type"`
	Channel        string   `json:"channel"`
	ModelProvider  string   `json:"model_provider"`
	Model          string   `json:"model"`
	ReasoningLevel string   `json:"reasoning_level"`
	Status         string   `json:"status"`
	SkillsCount    int      `json:"skills_count"`
	Skills         []string `json:"skills"`
	TokensFresh    bool     `json:"tokens_fresh"`
	TokensTotal    int      `json:"tokens_total"`
	ContextTokens  int      `json:"context_tokens"`
	InputTokens    int      `json:"input_tokens"`
	OutputTokens   int      `json:"output_tokens"`
	CacheRead      int      `json:"cache_read"`
	CacheWrite     int      `json:"cache_write"`
	EstCostUSD     int      `json:"est_cost_usd"`
}


func runDailyReviewCmd() {
	hours := 72
	limit := 30
	store := false

	args := os.Args[2:]
	for _, arg := range args {
		switch arg {
		case "--store", "-s":
			store = true
		case "-h", "--help":
			fmt.Println("Usage: mpm daily-review [flags]")
			fmt.Println("  --store, -s   Save snapshots to MPM database")
			fmt.Println("  -h, --help    Show this help")
			return
		}
	}

	report := DailyReviewReport{
		Generated: time.Now().Format(time.RFC3339),
	}

	// 1. Recent memories
	report.Memories = fetchRecentMemories(hours, limit)

	// 2. Skills inventory
	report.Skills = fetchSkillsInventory()

	// 3. Runtime config
	report.Runtime = fetchRuntimeConfig()

	// Print report
	printDailyReview(&report)

	// Store to MPM if requested
	if store {
		storeDailyReviewToMPM(&report)
	}
}

func fetchRecentMemories(hours, limit int) []MemoryEntry {
	dbPath := filepath.Join(config.GetMPMDir(), "src", "db", "mpm.db")
	db, err := sql.Open("sqlite3", dbPath+"?mode=ro")
	if err != nil {
		fmt.Printf("    [%s] Cannot open DB: %v\n", colorRed("ERR"), err)
		return nil
	}
	defer db.Close()

	since := time.Now().Add(-time.Duration(hours) * time.Hour).Format("2006-01-02 15:04:05")
	rows, err := db.Query(
		`SELECT id, created_at, content, collection, tags FROM memories
		 WHERE created_at >= ? ORDER BY created_at DESC LIMIT ?`,
		since, limit,
	)
	if err != nil {
		fmt.Printf("    [%s] Query failed: %v\n", colorRed("ERR"), err)
		return nil
	}
	defer rows.Close()

	var entries []MemoryEntry
	for rows.Next() {
		var e MemoryEntry
		if err := rows.Scan(&e.ID, &e.CreatedAt, &e.Content, &e.Collection, &e.Tags); err == nil {
			entries = append(entries, e)
		}
	}
	return entries
}

func fetchSkillsInventory() []SkillEntry {
	skillsDir := os.ExpandEnv("$HOME/.openclaw/workspace/skills")

	entries, _ := os.ReadDir(skillsDir)
	var skills []SkillEntry

	for _, entry := range entries {
		if !entry.IsDir() || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		skillPath := filepath.Join(skillsDir, entry.Name())

		// Read _meta.json
		metaPath := filepath.Join(skillPath, "_meta.json")
		var version, installedAt string
		var slug string
		if data, err := os.ReadFile(metaPath); err == nil {
			var m map[string]interface{}
			if json.Unmarshal(data, &m) == nil {
				if v, ok := m["version"].(string); ok {
					version = v
				}
				if published, ok := m["publishedAt"].(float64); ok {
					t := time.Unix(int64(published)/1000, 0)
					installedAt = t.Format("2006-01-02")
				}
				if inst, ok := m["installedAt"].(float64); ok && installedAt == "" {
					t := time.Unix(int64(inst)/1000, 0)
					installedAt = t.Format("2006-01-02")
				}
				if s, ok := m["slug"].(string); ok {
					slug = s
				}
			}
		}

		// Read SKILL.md for name + description
		name := entry.Name()
		description := ""
		if mdPath := filepath.Join(skillPath, "SKILL.md"); true {
			if data, err := os.ReadFile(mdPath); err == nil {
				if n := regexp.MustCompile(`(?m)^name:\s*(.+)$`).FindSubmatch(data); len(n) > 0 {
					name = string(n[1])
				}
				if d := regexp.MustCompile(`(?m)^description:\s*(.+)$`).FindSubmatch(data); len(d) > 0 {
					description = string(d[1])
				}
			}
		}

		if slug == "" {
			slug = entry.Name()
		}
		skills = append(skills, SkillEntry{
			Name:        name,
			Description: description,
			Version:     version,
			InstalledAt: installedAt,
			Slug:        slug,
		})
	}

	sort.Slice(skills, func(i, j int) bool {
		return skills[i].Name < skills[j].Name
	})
	return skills
}

func fetchRuntimeConfig() RuntimeConfig {
	home := os.Getenv("HOME")
	sessionsPath := filepath.Join(home, ".openclaw", "agents", "main", "sessions", "sessions.json")

	data, err := os.ReadFile(sessionsPath)
	if err != nil {
		fmt.Printf("    [%s] Cannot read sessions.json: %v\n", colorRed("ERR"), err)
		return RuntimeConfig{}
	}

	var sessions map[string]map[string]interface{}
	if err := json.Unmarshal(data, &sessions); err != nil {
		fmt.Printf("    [%s] Cannot parse sessions.json: %v\n", colorRed("ERR"), err)
		return RuntimeConfig{}
	}

	// Find most recently updated session
	var latest map[string]interface{}
	var latestUpdated int
	for _, s := range sessions {
		if up, ok := s["updatedAt"].(float64); ok && int(up) > latestUpdated {
			latestUpdated = int(up)
			latest = s
		}
	}

	if latest == nil {
		return RuntimeConfig{}
	}

	// Helper to extract string or ""
	strOf := func(m map[string]interface{}, k string) string {
		if v, ok := m[k].(string); ok {
			return v
		}
		return ""
	}
	intOf := func(m map[string]interface{}, k string) int {
		if v, ok := m[k].(float64); ok {
			return int(v)
		}
		return 0
	}
	boolOf := func(m map[string]interface{}, k string) bool {
		if v, ok := m[k].(bool); ok {
			return v
		}
		return false
	}

	// Extract skill names from skillsSnapshot
	var skillNames []string
	if ss, ok := latest["skillsSnapshot"].(map[string]interface{}); ok {
		if prompt, ok := ss["prompt"].(string); ok {
			matches := skillNameRegexp.FindAllStringSubmatch(prompt, -1)
			for _, m := range matches {
				if len(m) > 1 {
					skillNames = append(skillNames, m[1])
				}
			}
		}
	}
	sort.Strings(skillNames)

	updatedAt := intOf(latest, "updatedAt")
	startedAt := intOf(latest, "startedAt")

	return RuntimeConfig{
		SessionID:      strOf(latest, "sessionId"),
		UpdatedAt:      time.Unix(int64(updatedAt)/1000, 0).Format(time.RFC3339),
		StartedAt:      time.Unix(int64(startedAt)/1000, 0).Format(time.RFC3339),
		ChatType:       strOf(latest, "chatType"),
		Channel:        strOf(latest, "lastChannel"),
		ModelProvider:  strOf(latest, "modelProvider"),
		Model:          strOf(latest, "model"),
		ReasoningLevel: strOf(latest, "reasoningLevel"),
		Status:         strOf(latest, "status"),
		SkillsCount:    len(skillNames),
		Skills:         skillNames,
		TokensFresh:    boolOf(latest, "totalTokensFresh"),
		TokensTotal:    intOf(latest, "totalTokens"),
		ContextTokens:  intOf(latest, "contextTokens"),
		InputTokens:    intOf(latest, "inputTokens"),
		OutputTokens:   intOf(latest, "outputTokens"),
		CacheRead:      intOf(latest, "cacheRead"),
		CacheWrite:     intOf(latest, "cacheWrite"),
		EstCostUSD:     intOf(latest, "estimatedCostUsd"),
	}
}

func printDailyReview(r *DailyReviewReport) {
	fmt.Printf("\n%s[%s]%s %sMPM Daily Review%s\n\n", ansiBold, colorCyan("●"), ansiReset, ansiBold, ansiReset)

	// --- Memories ---
	fmt.Printf("%s─── Recent Memories ───%s\n", colorYellow("~"), ansiReset)
	if len(r.Memories) == 0 {
		fmt.Printf("  No memories from the last 72h.\n")
	} else {
		for _, m := range r.Memories {
			preview := m.Content
			if len(preview) > 180 {
				preview = preview[:180]
			}
			preview = strings.ReplaceAll(preview, "\n", " ")
			tags := ""
			if m.Tags != "" && m.Tags != "null" && m.Tags != "[]" {
				tags = fmt.Sprintf(" %s", m.Tags)
			}
			fmt.Printf("%s[%s]%s (%s)%s\n", colorCyan("  "), m.CreatedAt, ansiReset, m.Collection, tags)
			fmt.Printf("  %s\n\n", preview)
		}
	}

	// --- Skills ---
	fmt.Printf("%s─── Installed Skills (%d) ───%s\n", colorYellow("~"), len(r.Skills), ansiReset)
	if len(r.Skills) == 0 {
		fmt.Printf("  No skills found.\n")
	} else {
		for _, s := range r.Skills {
			desc := s.Description
			if len(desc) > 55 {
				desc = desc[:55] + "..."
			}
			installed := s.InstalledAt
			if installed == "" {
				installed = "?"
			}
			nameStr := fmt.Sprintf("%-22s", s.Name)
			verStr := fmt.Sprintf("%-10s", s.Version)
			fmt.Printf("%s  %s %s %s\n", colorCyan(""), nameStr, colorMagenta(verStr), installed)
			fmt.Print(ansiReset)
			fmt.Printf("  %s\n", desc)
		}
	}

	// --- Runtime Config ---
	fmt.Printf("%s─── Runtime Config ───%s\n", colorYellow("~"), ansiReset)
	rc := r.Runtime
	if rc.SessionID == "" {
		fmt.Printf("  No active session.\n")
	} else {
		fmt.Printf("%s  Session:%s   %s %s(%s, %s)%s\n", colorCyan(""), ansiReset,
			rc.SessionID[:8]+"...", colorGreen("running"), rc.ChatType, rc.Channel, ansiReset)
		fmt.Printf("%s  Model:%s     %s/%s  %sreasoning=%s%s%s\n", colorCyan(""), ansiReset,
			rc.ModelProvider, rc.Model, colorYellow("["), rc.ReasoningLevel, colorYellow("]"), ansiReset)
		fmt.Printf("%s  Tokens:%s    fresh=%v  total=%s\n", colorCyan(""), ansiReset,
			rc.TokensFresh, formatInt(int64(rc.TokensTotal)))
		fmt.Printf("%s             context=%s  input=%s  output=%s\n", colorCyan(""),
			formatInt(int64(rc.ContextTokens)), formatInt(int64(rc.InputTokens)), formatInt(int64(rc.OutputTokens)))
		fmt.Printf("%s  Skills:%s    %d loaded - %s%s\n", colorCyan(""), ansiReset,
			rc.SkillsCount, colorMagenta(strings.Join(rc.Skills, ", ")), ansiReset)
	}
	fmt.Println()
}

func storeDailyReviewToMPM(r *DailyReviewReport) {
	dbPath := filepath.Join(config.GetMPMDir(), "src", "db", "mpm.db")
	db, err := sql.Open("sqlite3", dbPath)
	if err != nil {
		fmt.Printf("\n  [%s] Cannot open DB for writing: %v\n", colorRed("ERR"), err)
		return
	}
	defer db.Close()

	payload, _ := json.Marshal(r)

	// Check for duplicate
	var lastContent string
	err = db.QueryRow(`SELECT content FROM memories WHERE collection='system' AND tags='["daily-review"]' ORDER BY created_at DESC LIMIT 1`).Scan(&lastContent)
	if err == nil && lastContent == string(payload) {
		fmt.Printf("\n  [%s] Daily review unchanged — not written\n", colorYellow("NOTE"))
		return
	}

	_, err = db.Exec(
		`INSERT INTO memories (collection, content, tags, metadata) VALUES (?, ?, ?, ?)`,
		"system", string(payload), `["daily-review"]`, "{}",
	)
	if err != nil {
		fmt.Printf("\n  [%s] Failed to store: %v\n", colorRed("ERR"), err)
		return
	}
	fmt.Printf("\n  [%s] Daily review saved to MPM\n", colorGreen("OK"))
}

func formatInt(n int64) string {
	return strconv.FormatInt(n, 10)
}
