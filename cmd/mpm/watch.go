package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"log/slog"
	"strings"
	"sync"
	"syscall"
	"time"

	"mpm/internal/config"

	mpminternal "mpm/internal"

	"github.com/fsnotify/fsnotify"
)

// ---------------------------------------------------------------------------
// Pre-compiled regex patterns (compiled once at package init, not per-call)
// ---------------------------------------------------------------------------

// extractFacts patterns — matched against session JSON lines
var factPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^.*"(name|key|value|path|url|endpoint|config|setting|preference)":\s*".*"$`),
	regexp.MustCompile(`(?i)^.*'(name|key|value|path|url|endpoint|config|setting|preference)':\s*'.*'$`),
	regexp.MustCompile(`(?i)^\s*[-*]\s+[A-Z].*:.*`),
	regexp.MustCompile(`(?i)^(user|preference|config|setting|path|name|key|value)[\s:-]+.+`),
}

var skipPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^(okay|ok|yes|yeah|yep|sure|great|thanks|thank you|i think|i believe|i feel)`),
	regexp.MustCompile(`(?i)^(the user|they|them|this is|here is|i'll|i will|i can|i could|let me|would you|could you)`),
	regexp.MustCompile(`^//.*`),
	regexp.MustCompile(`^\s*#.*`),
}

// looksLikeFact patterns
var looksLikeFactPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^(my |the |user )?[a-z]+ [a-z]+ is|are|has|was|were`),
	regexp.MustCompile(`(?i)^(remember|note|fact|important|preference)`),
	regexp.MustCompile(`(?i)^\(.*\) `),
}

// extractKeywords patterns
var (
	hashPattern  = regexp.MustCompile(`#([a-zA-Z][a-zA-Z0-9_-]*)`)
	camelPattern = regexp.MustCompile(`([A-Z][a-z]+[A-Z][a-zA-Z]*)`)
)


// resolveWatchDirs determines which directories to watch for the daemon
// Priority: 1) CLI flags, 2) Config file (memory_dirs/sessions_dirs), 3) OpenClaw workspace defaults
// NOTE: These are the paths the DAEMON watches for OpenClaw-created .md and session files.
//
//	The MPM database is separate and always at mpm/src/db/mpm.db.
func resolveWatchDirs(memDir, sesDir string) []string {
	var dirs []string

	// Load config for fallback paths
	cfg, err := config.LoadConfig()
	if err != nil {
		cfg = &config.Config{}
	}

	// Determine memory directories to watch (array from config)
	memoryDirs := cfg.GetMemoryDirs()
	if memDir != "" {
		// CLI flag overrides config
		resolved := config.ResolveEnvPath(memDir)
		if dirExists(resolved) {
			dirs = append(dirs, resolved)
		}
	} else if len(memoryDirs) > 0 {
		// Use array from config
		for _, md := range memoryDirs {
			resolved := config.ResolveEnvPath(md)
			if dirExists(resolved) {
				dirs = append(dirs, resolved)
			}
		}
	} else {
		// Default: OpenClaw workspace memory directory
		workspace := config.GetWorkspace()
		defaultMem := filepath.Join(workspace, "memory")
		// Always add mpm root memory/ as watched dir (agents write there)
		// mkdir if missing so fsnotify can attach to it when created
		if !dirExists(defaultMem) {
			os.MkdirAll(defaultMem, 0755)
		}
		dirs = append(dirs, defaultMem)
	}

	// Determine sessions directories to watch (array from config)
	sessionsDirs := cfg.GetSessionsDirs()
	if sesDir != "" {
		// CLI flag overrides config
		resolved := config.ResolveEnvPath(sesDir)
		if dirExists(resolved) {
			dirs = append(dirs, resolved)
		}
	} else if len(sessionsDirs) > 0 {
		// Use array from config
		for _, sd := range sessionsDirs {
			resolved := config.ResolveEnvPath(sd)
			if dirExists(resolved) {
				dirs = append(dirs, resolved)
			}
		}
	} else {
		// No sessions_dirs configured — this is a configuration error, not a fallback opportunity.
		// The user must explicitly configure where to watch for sessions.
		fmt.Fprintf(os.Stderr, "⚠️  No sessions_dirs configured in mpm_config.json and no default available.\n")
	}

	return dirs
}

// dirExists checks if a directory exists
func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// pollOnce performs one poll cycle for an external DB.
// The dm (DatabaseManager) is passed in and reused — caller manages its lifecycle.
// All saves are transactional — cursor only updated if all saves succeed.
func pollOnce(dbCfg *config.ExternalDB, dm *mpminternal.DatabaseManager, dryRun, verbose bool) {
	dbPath := dbCfg.Path

	cleanPath := filepath.Clean(dbPath)
	if !strings.HasSuffix(cleanPath, ".db") && !strings.HasSuffix(cleanPath, ".sqlite") && !strings.HasSuffix(cleanPath, ".sqlite3") {
		if verbose {
			fmt.Printf("⚠️  external DB %s: path does not look like SQLite DB: %s\n", dbCfg.Label, cleanPath)
		}
		return
	}

	extDB, err := sql.Open("sqlite3", cleanPath+"?mode=ro")
	if err != nil {
		if verbose {
			fmt.Printf("⚠️  external DB %s: open failed: %v\n", dbCfg.Label, err)
		}
		return
	}
	defer extDB.Close()

	// Detect schema adapter dynamically
	registry := mpminternal.NewAdapterRegistry()
	adapter := registry.Detect(extDB)
	if adapter == nil {
		if verbose {
			fmt.Printf("⚠️  external DB %s: no matching schema adapter found\n", dbCfg.Label)
		}
		return
	}

	cursor, err := dm.GetExternalDBCursor(dbCfg.Label)
	if err != nil {
		if verbose {
			fmt.Printf("⚠️  external DB %s: get cursor failed: %v\n", dbCfg.Label, err)
		}
		cursor = ""
	}

	memories, newCursor, err := adapter.FetchNew(extDB, cursor)
	if err != nil {
		if verbose {
			fmt.Printf("⚠️  external DB %s: fetch failed: %v\n", dbCfg.Label, err)
		}
		return
	}

	count := 0
	if dryRun {
		for _, mem := range memories {
			if verbose {
				fmt.Printf("   [dry-run] would ingest: id=%s, content_len=%d\n", mem.ID, len(mem.Content))
			}
		}
	} else {
		tx, err := dm.SQLDB().Begin()
		if err != nil {
			if verbose {
				fmt.Printf("⚠️  external DB %s: transaction begin failed: %v\n", dbCfg.Label, err)
			}
			return
		}

		for _, mem := range memories {
			existing, _ := dm.GetMemoryByExternalID(dbCfg.Label, mem.ID)
			if existing != nil {
				continue
			}

			tagsJSON := fmt.Sprintf(`["source:%s","external_db"]`, dbCfg.Label)
			metadataMap := map[string]interface{}{
				"source_label": dbCfg.Label,
				"source_id":    mem.ID,
			}
			metadataJSON, _ := json.Marshal(metadataMap)
			embedding := mpminternal.HashEmbed(mem.Content)
			embeddingJSON, _ := json.Marshal(embedding)
			contentHash := sha256.Sum256([]byte(mem.Content))
			now := time.Now()

			_, err := tx.Exec(`
				INSERT INTO memories (id, collection, content, session_id, tags, metadata, embedding, created_at, source_db, source_id, content_hash)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			`, mpminternal.GenerateID(), "memories", mem.Content, mem.SessionID, tagsJSON, string(metadataJSON), string(embeddingJSON), now.Format(time.RFC3339), dbCfg.Label, mem.ID, hex.EncodeToString(contentHash[:]))
			if err != nil {
				if verbose {
					fmt.Printf("⚠️  external DB %s: save failed for id=%s: %v\n", dbCfg.Label, mem.ID, err)
				}
				tx.Rollback()
				return
			}
			count++
		}

		if newCursor != "" {
			_, err = tx.Exec(`
				INSERT OR REPLACE INTO external_db_cursors (db_label, last_cursor, updated_at)
				VALUES (?, ?, CURRENT_TIMESTAMP)`,
				dbCfg.Label, newCursor)
			if err != nil {
				if verbose {
					fmt.Printf("⚠️  external DB %s: set cursor failed: %v\n", dbCfg.Label, err)
				}
				tx.Rollback()
				return
			}
		}

		if err := tx.Commit(); err != nil {
			if verbose {
				fmt.Printf("⚠️  external DB %s: commit failed: %v\n", dbCfg.Label, err)
			}
			return
		}
	}

	if verbose && count > 0 {
		fmt.Printf("✅ external DB %s: ingested %d new memories (cursor=%s)\n",
			dbCfg.Label, count, newCursor)
	}
}

// isSystemFile returns true for OpenClaw live session registry and config files
// that must NEVER be processed or deleted by the watch daemon.
// These files are managed by OpenClaw at runtime and changes are detected via
// fsnotify — they are processed via parseSessionsSnapshot into system_config table.
func isSystemFile(name string) bool {
	nameLower := strings.ToLower(name)
	// Exact matches for live OpenClaw system files
	switch nameLower {
	case "sessions.json", "session.json", "workspace.json", "config.json":
		return true
	}
	// Also skip sessions.json with any suffix/prefix pattern (e.g. Nextcloud sync conflicts
	// like "sessions [conflicted 8].json" or "sessions-2026-04-01.json")
	if strings.HasPrefix(nameLower, "sessions") && strings.HasSuffix(nameLower, ".json") {
		return true
	}
	return false
}

// =============================================================================
// WatcherDaemon - Main daemon structure
// =============================================================================

type watcherDaemon struct {
	watcher    *fsnotify.Watcher
	dirs       []string
	db         *mpminternal.DatabaseManager
	memory     *mpminternal.MemoryStore
	dryRun     bool
	verbose    bool
	mu         sync.Mutex
	stopCh     chan struct{}

	synthClient       *mpminternal.SynthClient // cached, created once
	lastClusterCheck  time.Time                // guards checkTopicClustering rate
}

func newWatcherDaemon(w *fsnotify.Watcher, dirs []string, dryRun, verbose bool) *watcherDaemon {
	// Add panic recovery
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "❌ newWatcherDaemon panic: %v\n", r)
		}
	}()

	// Initialize database
	projectRoot := config.GetWorkspace()
	db, err := mpminternal.NewDatabaseManager(projectRoot)
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Failed to initialize database: %v\n", err)
		return nil
	}

	memoryStore := mpminternal.NewMemoryStore(projectRoot)

	return &watcherDaemon{
		watcher:           w,
		dirs:              dirs,
		db:                db,
		memory:            memoryStore,
		dryRun:            dryRun,
		verbose:           verbose,
		stopCh:            make(chan struct{}),
		synthClient:       mpminternal.NewSynthClient(),
		lastClusterCheck:  time.Time{}, // zero — will fire on first ingest
	}
}

// IsReady returns true if the daemon is properly initialized
func (d *watcherDaemon) IsReady() bool {
	return d.db != nil && d.db.IsOpen()
}

// handleSignals handles graceful shutdown on SIGINT/SIGTERM
func (d *watcherDaemon) handleSignals(done chan bool) {
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	<-sigCh
	close(d.stopCh)
	done <- true
}

// =============================================================================
// PART 1: Startup Sweep
// =============================================================================

func (d *watcherDaemon) startupSweep() {
	for _, dir := range d.dirs {
		d.sweepDirectory(dir)
	}
}

func (d *watcherDaemon) sweepDirectory(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if d.verbose {
			fmt.Fprintf(os.Stderr, "⚠️  Cannot read directory %s: %v\n", dir, err)
		}
		return
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()
		path := filepath.Join(dir, name)

		// Skip conflicted files and OpenClaw system files
		nameLower := strings.ToLower(name)
		if strings.Contains(nameLower, "conflicted") {
			if d.verbose {
				fmt.Printf("   ⏭️  Skipping conflicted file: %s\n", name)
			}
			continue
		}

		// Skip OpenClaw system files that should never be processed
		// sessions.json is a LIVE file managed by OpenClaw — reading it causes race conditions
		switch nameLower {
		case "sessions.json", "session.json":
			// sessions.json/session.json are live session registries — never process
			if d.verbose {
				fmt.Printf("   ⏭️  Skipping live session registry: %s\n", name)
			}
			continue
		case "workspace.json", "config.json":
			// These are config files — skip in sweep, only process via fsnotify events if needed
			if d.verbose {
				fmt.Printf("   ⏭️  Skipping OpenClaw system config: %s\n", name)
			}
			continue
		}

		ext := strings.ToLower(filepath.Ext(name))

		switch ext {
		case ".md":
			// Route A: Process and delete .md memory files
			d.processMarkdownFile(path, true)

		case ".json":
			// Route C: OpenClaw static config files — parse, hash, store (never delete)
			// NOTE: sessions.json is a LIVE session registry (managed by OpenClaw at runtime).
			// Do NOT process it here — it changes on every message and causes desktop indexing
			// conflicts. Only process workspace.json and config.json which are truly static.
			if nameLower == "workspace.json" || nameLower == "config.json" {
				d.processSessionsConfig(path)
			}

		case ".jsonl":
			// Skip .jsonl in startup sweep — they are processed by the orphan
			// sweep triggered when a new session lock (.jsonl.lock) is created.
		}
	}
}

// =============================================================================
// PART 3: File Processing
// =============================================================================

// processMarkdownFile handles Route A: Explicit memory files (.md)
func (d *watcherDaemon) processMarkdownFile(path string, isStartup bool) {
	name := filepath.Base(path)
	prefix := "📄"
	if isStartup {
		prefix = "⚡ [sweep]"
	}

	if d.verbose {
		fmt.Printf("   %s Processing .md: %s\n", prefix, name)
	}

	// Read file content
	content, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "   ❌ Failed to read %s: %v\n", name, err)
		return
	}

	contentStr := string(content)

	// Run sanitization checks (regex + toxic phrases)
	if isSensitive, reason := mpminternal.IsSensitiveContent(contentStr); isSensitive {
		fmt.Fprintf(os.Stderr, "   ❌ Sensitive content blocked in %s: %s\n", name, reason)
		d.deleteFile(path, "sensitive content blocked")
		return
	}

	if isPoisoned, reason := d.isPoisoned(contentStr); isPoisoned {
		fmt.Fprintf(os.Stderr, "   ❌ Poison content blocked in %s: %s\n", name, reason)
		d.deleteFile(path, "poison content blocked")
		return
	}

	// Epistemology detection: route HYPOTHESIS: content to theories, CHOICE: to decisions
	contentUpper := strings.ToUpper(contentStr)
	var memID string

	if strings.Contains(contentUpper, "HYPOTHESIS:") {
		memID, err = d.ingestAsEpistemologyMemory(contentStr, path, "theories")
		if err != nil {
			fmt.Fprintf(os.Stderr, "   ❌ Failed to ingest theory %s: %v\n", name, err)
			return
		}
		fmt.Printf("   %s Theory saved: %s (id: %s)\n", prefix, name, memID[:8])
		d.deleteFile(path, "theory processed")
		return
	}

	if strings.Contains(contentUpper, "CHOICE:") {
		memID, err = d.ingestAsEpistemologyMemory(contentStr, path, "decisions")
		if err != nil {
			fmt.Fprintf(os.Stderr, "   ❌ Failed to ingest decision %s: %v\n", name, err)
			return
		}
		fmt.Printf("   %s Decision saved: %s (id: %s)\n", prefix, name, memID[:8])
		d.deleteFile(path, "decision processed")
		return
	}

	// Route A: Insert as LTM (is_long_term = true, weight = 10)
	memID, err = d.ingestAsLongTermMemory(contentStr, path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "   ❌ Failed to ingest %s: %v\n", name, err)
		return
	}

	// Fire-and-forget: check for near-miss candidates and auto-synthesize.
	// Uses cached synthClient to avoid redundant config reads.
	if d.synthClient != nil {
		go func() {
			mpminternal.AutoSynthesize(context.Background(), d.db, d.synthClient, memID, contentStr)
		}()
	}

	// Check for topic clustering — rate-limited to once every 5 minutes.
	// The full LTM scan is expensive; debouncing prevents O(n) waste on
	// high-ingest bursts.
	if time.Since(d.lastClusterCheck) > 5*time.Minute {
		d.checkTopicClustering()
		d.lastClusterCheck = time.Now()
	}

	fmt.Printf("   %s LTM memory saved: %s (id: %s)\n", prefix, name, memID[:8])
	d.deleteFile(path, "processed successfully")
}

// sweepOrphanSessions scans a session directory for orphan .jsonl files that
// have no matching .jsonl.lock, indicating the session has ended. Each orphan
// is processed: facts extracted and saved to DB, then LLM synthesis triggered.
// On completion the .jsonl is archived to archive/ to prevent data loss.
func (d *watcherDaemon) sweepOrphanSessions(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "   ⚠️  Cannot read session dir %s: %v\n", dir, err)
		return
	}

	// Build set of locked UUIDs (files with an active .jsonl.lock)
	locks := make(map[string]bool)
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".jsonl.lock") {
			uuid := strings.TrimSuffix(name, ".jsonl.lock")
			locks[uuid] = true
		}
	}

	// Process orphans: .jsonl files that have NO matching .lock
	var orphans []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		uuid := strings.TrimSuffix(name, ".jsonl")
		if locks[uuid] {
			continue // still active
		}
		orphans = append(orphans, filepath.Join(dir, name))
	}

	if len(orphans) == 0 {
		return
	}

	for _, path := range orphans {
		if d.verbose {
			fmt.Printf("   🔄 Orphan session: %s\n", filepath.Base(path))
		}
		d.processSessionFile(path, false)
	}
}

// processSessionFile handles Route B: Session files (.jsonl)
func (d *watcherDaemon) processSessionFile(path string, isStartup bool) {
	name := filepath.Base(path)
	prefix := "📋"
	if isStartup {
		prefix = "⚡ [sweep]"
	}

	if d.verbose {
		fmt.Printf("   %s Processing .jsonl: %s\n", prefix, name)
	}

	// Read and parse JSON lines
	lines, err := d.readJSONLines(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "   ❌ Failed to read %s: %v\n", name, err)
		return
	}

	// Extract declarative facts from session
	facts := d.extractFacts(lines)

	if len(facts) == 0 {
		if d.verbose {
			fmt.Printf("   %s No facts extracted from %s\n", prefix, name)
		}
		// Still delete the session file - no useful data
		d.deleteFile(path, "no facts extracted")
		return
	}

	// Insert facts into session collection (weight = 1, 24h TTL)
	var savedIDs []string
	for _, fact := range facts {
		// Sanitization checks
		if isSensitive, reason := mpminternal.IsSensitiveContent(fact); isSensitive {
			if d.verbose {
				fmt.Printf("   ⚠️  Skipping sensitive fact: %s\n", reason)
			}
			continue
		}

		if isPoisoned, reason := d.isPoisoned(fact); isPoisoned {
			if d.verbose {
				fmt.Printf("   ⚠️  Skipping poison fact: %s\n", reason)
			}
			continue
		}

		memID, err := d.ingestAsSessionMemory(fact, path)
		if err != nil {
			if d.verbose {
				fmt.Fprintf(os.Stderr, "   ⚠️  Failed to save fact: %v\n", err)
			}
			continue
		}
		savedIDs = append(savedIDs, memID)
	}

	if len(savedIDs) > 0 {
		fmt.Printf("   %s Session facts saved: %d memories from %s\n", prefix, len(savedIDs), name)
	}

	// Check for topic clustering
	d.checkTopicClustering()

	// Session file processing complete — .jsonl will be archived by caller
}

// processSessionsConfig handles Route C: OpenClaw system config files (sessions.json, workspace.json, etc.)
// Reads, hashes, parses structured data, and stores in system_config table.
// Does NOT delete the source file — it's a live config that OpenClaw manages.
func (d *watcherDaemon) processSessionsConfig(path string) {
	name := filepath.Base(path)

	// Read raw content
	rawBytes, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "   ❌ Failed to read %s: %v\n", name, err)
		return
	}
	rawJSON := string(rawBytes)

	// Compute content hash for change detection
	hash := sha256.Sum256(rawBytes)
	contentHash := hex.EncodeToString(hash[:])

	// Parse structured snapshot from sessions.json
	snapshot, err := d.parseSessionsSnapshot(rawBytes)
	if err != nil {
		if d.verbose {
			fmt.Printf("   ⚠️  Failed to parse %s as sessions config: %v\n", name, err)
		}
		snapshot = nil
	}

	snapshotJSON := ""
	if snapshot != nil {
		bytes, _ := json.Marshal(snapshot)
		snapshotJSON = string(bytes)
	}

	// Save to system_config (hash-check prevents duplicate writes)
	updated, err := d.db.SaveSystemConfig(name, rawJSON, contentHash, snapshotJSON)
	if err != nil {
		fmt.Fprintf(os.Stderr, "   ❌ Failed to save system config %s: %v\n", name, err)
		return
	}

	if updated {
		fmt.Printf("   📡 System config updated: %s\n", name)
	} else {
		if d.verbose {
			fmt.Printf("   ⏭️  System config unchanged: %s\n", name)
		}
	}
}

// parseSessionsSnapshot extracts a structured, queryable snapshot from sessions.json bytes
// Returns a flat map with the most useful fields, or nil on parse failure.
func (d *watcherDaemon) parseSessionsSnapshot(data []byte) (map[string]interface{}, error) {
	var sessions map[string]json.RawMessage
	if err := json.Unmarshal(data, &sessions); err != nil {
		return nil, err
	}

	// sessions.json is a map of sessionKey -> sessionData
	// We only care about the main "agent:main:main" session (or first key available)
	var sessionData map[string]interface{}
	for key, raw := range sessions {
		if err := json.Unmarshal(raw, &sessionData); err != nil {
			continue
		}
		// Use the first/primary session data available
		if key == "agent:main:main" {
			break
		}
		break // use first available
	}

	if sessionData == nil {
		return nil, fmt.Errorf("no session data found")
	}

	snapshot := map[string]interface{}{
		"session_key":         "",
		"session_id":          "",
		"channel":             "",
		"model":               "",
		"provider":            "",
		"skills_count":        0,
		"skills":              []string{},
		"workspace_files":     []string{},
		"system_prompt_chars": 0,
		"context_tokens":      0,
		"runtime_ms":          int64(0),
		"updated_at":          int64(0),
	}

	// Extract top-level fields
	if v, ok := sessionData["sessionId"].(string); ok {
		snapshot["session_id"] = v
	}
	if v, ok := sessionData["model"].(string); ok {
		snapshot["model"] = v
	}
	if v, ok := sessionData["modelProvider"].(string); ok {
		snapshot["provider"] = v
	}
	if v, ok := sessionData["contextTokens"].(float64); ok {
		snapshot["context_tokens"] = int(v)
	}
	if v, ok := sessionData["runtimeMs"].(float64); ok {
		snapshot["runtime_ms"] = int64(v)
	}
	if v, ok := sessionData["updatedAt"].(float64); ok {
		snapshot["updated_at"] = int64(v)
	}

	// Extract channel from deliveryContext
	if dc, ok := sessionData["deliveryContext"].(map[string]interface{}); ok {
		if v, ok := dc["channel"].(string); ok {
			snapshot["channel"] = v
		}
	}

	// Extract origin label
	if origin, ok := sessionData["origin"].(map[string]interface{}); ok {
		if v, ok := origin["label"].(string); ok {
			snapshot["origin_label"] = v
		}
	}

	// Extract skills
	var skillNames []string
	if ss, ok := sessionData["skillsSnapshot"].(map[string]interface{}); ok {
		if skills, ok := ss["skills"].([]interface{}); ok {
			for _, s := range skills {
				if m, ok := s.(map[string]interface{}); ok {
					if name, ok := m["name"].(string); ok {
						skillNames = append(skillNames, name)
					}
				}
			}
		}
	}
	snapshot["skills"] = skillNames
	snapshot["skills_count"] = len(skillNames)

	// Extract workspace files
	var wsFiles []string
	if ss, ok := sessionData["skillsSnapshot"].(map[string]interface{}); ok {
		if spr, ok := ss["systemPromptReport"].(map[string]interface{}); ok {
			if files, ok := spr["injectedWorkspaceFiles"].([]interface{}); ok {
				for _, f := range files {
					if m, ok := f.(map[string]interface{}); ok {
						if name, ok := m["name"].(string); ok {
							wsFiles = append(wsFiles, name)
						}
					}
				}
			}
		}
		if sp, ok := ss["systemPrompt"].(map[string]interface{}); ok {
			if v, ok := sp["chars"].(float64); ok {
				snapshot["system_prompt_chars"] = int(v)
			}
		}
	}
	snapshot["workspace_files"] = wsFiles

	return snapshot, nil
}

// sha256 and hex imported at top of file (already present)

func (d *watcherDaemon) readJSONLines(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var lines []string
	scanner := bufio.NewScanner(file)
	// Increase buffer size for large JSONL files (default 64KB is too small)
	const maxScanTokenSize = 1024 * 1024 // 1MB per line
	buf := make([]byte, maxScanTokenSize)
	scanner.Buffer(buf, maxScanTokenSize)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			lines = append(lines, line)
		}
	}
	return lines, scanner.Err()
}

// =============================================================================
// PART 4: Data Promotion Pipeline
// =============================================================================

// ingestAsLongTermMemory inserts explicit memory as LTM (weight = 10)
func (d *watcherDaemon) ingestAsLongTermMemory(content, sourcePath string) (string, error) {
	// Extract keywords/tags from content
	tags := d.extractKeywords(content)
	tags = append(tags, "ltm", "explicit-memory")

	// Prepare metadata with LTM flag
	metadata := map[string]interface{}{
		"is_long_term": true,
		"weight":       10,
		"source_path":  sourcePath,
		"promoted_at":  time.Now().UTC().Format(time.RFC3339),
	}

	// Generate hash-based embedding
	embedding := mpminternal.HashEmbed(content)

	// Save to database (LTM: isLongTerm=true, weight=10)
	id, err := d.db.SaveMemory("memories", content, "", tags, metadata, embedding, true, 10)
	if err != nil {
		return "", err
	}

	// Also append to mirror
	d.appendToMirror(id, content, "memories", tags, metadata)

	return id, nil
}

// ingestAsEpistemologyMemory saves content to the theories or decisions collection
// and auto-links to the corresponding topic.
func (d *watcherDaemon) ingestAsEpistemologyMemory(content, sourcePath, collection string) (string, error) {
	tags := d.extractKeywords(content)
	tags = append(tags, collection)

	metadata := map[string]interface{}{
		"source_path": sourcePath,
	}

	embedding := mpminternal.HashEmbed(content)
	id, err := d.db.SaveMemory(collection, content, "", tags, metadata, embedding, false, 5)
	if err != nil {
		return "", err
	}

	d.appendToMirror(id, content, collection, tags, metadata)

	// Auto-link to epistemology topic
	topicID, tErr := d.db.GetOrCreateTopic(collection)
	if tErr == nil {
		d.db.AddMemoryToTopic(id, topicID, "primary")
	}

	return id, nil
}

// ingestAsSessionMemory inserts extracted session facts (weight = 1)
// Facts are stored in the "session" collection (not "memories") to isolate them
// from long-term factual knowledge. A short TTL (24h) ensures automatic cleanup
// via `mpm maintain`. State-change dedup prevents duplicate entries when the
// underlying value hasn't actually changed.
func (d *watcherDaemon) ingestAsSessionMemory(content, sourcePath string) (string, error) {
	// Extract fact key prefix for per-type dedup.
	// e.g., "session cwd: /home/user" → prefix "session cwd", value "/home/user"
	// e.g., "model: gpt-4 (provider: openai)" → prefix "model", value "gpt-4 (provider: openai)"
	keyPrefix := content
	if idx := strings.Index(content, ":"); idx > 0 {
		keyPrefix = content[:idx]
	}

	// Deduplicate per fact type: only insert if the value differs from the most
	// recent session entry of the same type. This prevents heartbeats like cwd or
	// model from being re-stored when unchanged, while still capturing actual
	// state changes (e.g., cwd changed from /foo to /bar).
	db := d.db.SQLDB()
	var latestContent string
	err := db.QueryRow(
		`SELECT content FROM memories
		 WHERE collection = 'session' AND deleted_at IS NULL AND content LIKE ?
		 ORDER BY created_at DESC LIMIT 1`, keyPrefix+": %",
	).Scan(&latestContent)
	if err == nil && latestContent == content {
		return "", nil // no state change — skip
	}

	// Extract keywords/tags
	tags := d.extractKeywords(content)
	tags = append(tags, "session-fact")

	// 24h TTL for session facts — ensures automatic cleanup via mpm maintain
	ttl := time.Now().Add(24 * time.Hour).UTC()

	// Prepare metadata
	metadata := map[string]interface{}{
		"is_long_term": false,
		"weight":       1,
		"source_path":  sourcePath,
		"promoted_at":  time.Now().UTC().Format(time.RFC3339),
	}

	// Generate embedding
	embedding := mpminternal.HashEmbed(content)

	// Save to database — explicitly use "session" collection to isolate from LTM.
	// Pass expires_at explicitly so SaveMemory can write it to the correct column.
	id, err := d.db.SaveMemory("session", content, "", tags, metadata, embedding, false, 1, ttl)
	if err != nil {
		return "", err
	}

	// Also append to mirror
	d.appendToMirror(id, content, "session", tags, metadata)

	return id, nil
}

// extractFacts scans JSON lines for declarative facts/configurations
func (d *watcherDaemon) extractFacts(lines []string) []string {
	var facts []string

	// Add panic recovery
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "   ⚠️  extractFacts recovered from panic: %v\n", r)
		}
	}()

	for _, line := range lines {
		line = strings.TrimSpace(line)
		if len(line) < 5 {
			continue
		}

		// If line is JSON, skip pattern checks and go straight to JSON parsing
		isJSON := strings.HasPrefix(line, "{")
		if !isJSON {
			// Skip if matches any skip pattern
			shouldSkip := false
			for _, pattern := range skipPatterns {
				if pattern.MatchString(line) {
					shouldSkip = true
					break
				}
			}
			if shouldSkip {
				continue
			}

			// Include if matches any fact pattern
			for _, pattern := range factPatterns {
				if pattern.MatchString(line) {
					facts = append(facts, line)
					break
				}
			}
		}

		// Extract facts from OpenClaw session JSON format
		if fact := d.extractFromSessionLine(line); fact != "" {
			if d.verbose {
				fmt.Printf("   [fact] %s\n", fact[:min(80, len(fact))])
			}
			facts = append(facts, fact)
		}
	}

	// Deduplicate
	seen := make(map[string]bool)
	var unique []string
	for _, f := range facts {
		if !seen[f] {
			seen[f] = true
			unique = append(unique, f)
		}
	}
	return unique
}

// sessionMessage represents OpenClaw session message structure
type sessionMessage struct {
	Type      string `json:"type"`
	ID        string `json:"id"`
	ParentID  string `json:"parentId,omitempty"`
	Timestamp string `json:"timestamp,omitempty"`
	Message   struct {
		Role    string `json:"role"`
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text,omitempty"`
		} `json:"content"`
	} `json:"message,omitempty"`
	CWD      string `json:"cwd,omitempty"`
	ModelID  string `json:"modelId,omitempty"`
	Provider string `json:"provider,omitempty"`
}

// extractFromSessionLine extracts declarative facts from OpenClaw session JSON lines
func (d *watcherDaemon) extractFromSessionLine(line string) string {
	// Try to parse as JSON
	var msg sessionMessage
	if err := json.Unmarshal([]byte(line), &msg); err != nil {
		return ""
	}

	switch msg.Type {
	case "session":
		// Extract session metadata as fact
		if msg.CWD != "" {
			return fmt.Sprintf("session cwd: %s", msg.CWD)
		}

	case "model_change":
		// Model configuration changes
		if msg.ModelID != "" {
			return fmt.Sprintf("model: %s (provider: %s)", msg.ModelID, msg.Provider)
		}

	case "message":
		if msg.Message.Role == "user" {
			// Extract text content from user messages
			for _, content := range msg.Message.Content {
				if content.Type == "text" && len(content.Text) > 10 {
					text := strings.TrimSpace(content.Text)
					// Only extract if it looks like a fact (not a command)
					if d.looksLikeFact(text) {
						// Truncate long messages
						if len(text) > 200 {
							text = text[:200] + "..."
						}
						return text
					}
				}
			}
		}
	}

	return ""
}

// looksLikeFact determines if text content looks like a declarative fact
func (d *watcherDaemon) looksLikeFact(text string) bool {
	// Facts don't start with these patterns
	factIndicators := []string{
		"[cron:", "[heartbeat:", "[system",
		"Run ", "Check ", "Execute ",
		"808", "Hello", "Hey",
	}
	for _, indicator := range factIndicators {
		if strings.HasPrefix(text, indicator) {
			return false
		}
	}
	// Look for explicit fact patterns (pre-compiled at package level)
	for _, p := range looksLikeFactPatterns {
		if p.MatchString(text) {
			return true
		}
	}
	return false
}

// extractKeywords extracts tags/keywords from content
func (d *watcherDaemon) extractKeywords(content string) []string {
	// Simple keyword extraction based on:
	// 1. # tags (markdown)
	// 2. CamelCase words
	// 3. Common project names
	var tags []string
	seen := make(map[string]bool)

	// Extract # tags (hashPattern pre-compiled at package level)
	for _, match := range hashPattern.FindAllStringSubmatch(content, -1) {
		tag := strings.ToLower(match[1])
		if !seen[tag] && len(tag) > 2 {
			tags = append(tags, tag)
			seen[tag] = true
		}
	}

	// Extract CamelCase words (camelPattern pre-compiled at package level)
	for _, match := range camelPattern.FindAllStringSubmatch(content, -1) {
		tag := strings.ToLower(match[1])
		if !seen[tag] && len(tag) > 2 {
			tags = append(tags, tag)
			seen[tag] = true
		}
	}

	// Check for known project names
	knownProjects := []string{"mpm", "symai", "desp", "openclaw", "github"}
	contentLower := strings.ToLower(content)
	for _, project := range knownProjects {
		if strings.Contains(contentLower, project) && !seen[project] {
			tags = append(tags, project)
			seen[project] = true
		}
	}

	// Limit to top 5 tags
	if len(tags) > 5 {
		tags = tags[:5]
	}

	return tags
}

// checkTopicClustering implements the Topic Clustering trigger.
// When N or more LTM memories share the same tag, create a Topic record.
//
// Locking strategy: holds d.mu for entire operation to prevent race conditions.
// No in-memory cache — queries DB directly for consistency across daemons.
func (d *watcherDaemon) checkTopicClustering() {
	const clusterThreshold = 3

	d.mu.Lock()
	defer d.mu.Unlock()

	ltmMemories, err := d.getLTMMemories()
	if err != nil {
		if d.verbose {
			fmt.Fprintf(os.Stderr, "⚠️  Failed to get LTM memories: %v\n", err)
		}
		return
	}

	tagCounts := make(map[string][]string)
	for _, mem := range ltmMemories {
		for _, tag := range mem.Tags {
			tagCounts[tag] = append(tagCounts[tag], mem.ID)
		}
	}

	for tag, memIDs := range tagCounts {
		if len(memIDs) < clusterThreshold {
			continue
		}
		// Check if topic already exists in DB (no in-memory cache)
		existingTopic, err := d.findTopicByTag(tag)
		if err == nil && existingTopic != nil {
			continue
		}

		// Create new topic from cluster
		_, err = d.createTopicFromCluster(tag, memIDs)
		if err != nil {
			if d.verbose {
				fmt.Fprintf(os.Stderr, "⚠️  Failed to create topic for tag '%s': %v\n", tag, err)
			}
			continue
		}
		fmt.Printf("   🏷️  Topic auto-created: '%s' (tag: %s, %d memories)\n", formatTopicName(tag), tag, len(memIDs))
	}
}

// LTMemory represents a long-term memory record
type LTMemory struct {
	ID        string
	Content   string
	Tags      []string
	Weight    int
	CreatedAt string
}

// getLTMMemories retrieves all LTM memories from the database
func (d *watcherDaemon) getLTMMemories() ([]LTMemory, error) {
	query := `SELECT id, content, tags, metadata, created_at FROM memories WHERE (is_long_term = 1 OR weight >= 10) AND deleted_at IS NULL LIMIT 1000`

	rows, err := d.db.SQLDB().Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var memories []LTMemory
	for rows.Next() {
		var id, content, tagsJSON, metadataJSON, createdAt string
		if err := rows.Scan(&id, &content, &tagsJSON, &metadataJSON, &createdAt); err != nil {
			continue
		}

		// Parse tags
		var tags []string
		if tagsJSON != "" {
			json.Unmarshal([]byte(tagsJSON), &tags)
		}

		// Check weight from metadata
		weight := 1
		if strings.Contains(metadataJSON, `"weight":10`) || strings.Contains(metadataJSON, `"weight": 10`) {
			weight = 10
		}

		if weight >= 10 {
			memories = append(memories, LTMemory{
				ID:        id,
				Content:   content,
				Tags:      tags,
				Weight:    weight,
				CreatedAt: createdAt,
			})
		}
	}

	return memories, nil
}

// Topic represents a topic record
type Topic struct {
	ID          string
	Name        string
	Description string
	Tags        string
	CreatedAt   string
}

// findTopicByTag searches for an existing topic by tag name
func (d *watcherDaemon) findTopicByTag(tag string) (*Topic, error) {
	query := `SELECT id, name, description, tags, created_at FROM topics WHERE name LIKE ? OR tags LIKE ? LIMIT 1`
	escapedTag := strings.ReplaceAll(tag, "~", "~~")
	escapedTag = strings.ReplaceAll(escapedTag, "%", "~%")
	tagPattern := "%" + escapedTag + "%"

	row := d.db.SQLDB().QueryRow(query, tagPattern, tagPattern)
	var t Topic
	err := row.Scan(&t.ID, &t.Name, &t.Description, &t.Tags, &t.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// createTopicFromCluster creates a new topic and links memories to it
func (d *watcherDaemon) createTopicFromCluster(tag string, memoryIDs []string) (string, error) {
	topicName := formatTopicName(tag)
	topicDesc := fmt.Sprintf("Auto-generated topic from %d LTM memories tagged with '%s'", len(memoryIDs), tag)

	// Create the topic
	topicID := mpminternal.GenerateID()
	_, err := d.db.SQLDB().Exec(`
		INSERT INTO topics (id, name, description, tags)
		VALUES (?, ?, ?, ?)
	`, topicID, topicName, topicDesc, tag)
	if err != nil {
		return "", err
	}

	// Link memories to topic via topic_memberships
	for _, memID := range memoryIDs {
		// Get session_id from memory
		var sessionID string
		row := d.db.SQLDB().QueryRow("SELECT session_id FROM memories WHERE id = ?", memID)
		if err := row.Scan(&sessionID); err != nil {
			continue
		}

		// Get or create session record
		if sessionID == "" {
			sessionID = mpminternal.GenerateID()
		}

		// Insert membership link
		if _, err := d.db.SQLDB().Exec(`
			INSERT OR IGNORE INTO topic_memberships (session_id, topic_id)
			VALUES (?, ?)
		`, sessionID, topicID); err != nil {
			slog.Warn("topic membership insert failed", "session_id", sessionID, "topic_id", topicID, "error", err)
		}
	}

	return topicID, nil
}

// formatTopicName converts a tag to a readable topic name
func formatTopicName(tag string) string {
	// Convert snake_case or kebab-case to Title Case
	words := strings.Fields(strings.ReplaceAll(strings.ReplaceAll(tag, "_", " "), "-", " "))
	for i, word := range words {
		if len(word) > 0 {
			words[i] = strings.ToUpper(string(word[0])) + strings.ToLower(word[1:])
		}
	}
	return strings.Join(words, " ")
}

// =============================================================================
// Goroutine-based Watch (used by unified daemon — pushes events to Worker Pool)
// =============================================================================

// startWatcherGoroutine creates the fsnotify watcher and runs its event loop
// in the current goroutine. It pushes WatchEvents to the pool for processing.
// Blocks until ctx is cancelled or the watcher encounters a fatal error.
func startWatcherGoroutine(ctx context.Context, pool *WorkerPool, dryRun, verbose bool) {
	dirs := resolveWatchDirs("", "")
	if len(dirs) == 0 {
		fmt.Fprintf(os.Stderr, "⚠️  No watch directories configured — watcher started but will not process any files\n")
		fmt.Fprintf(os.Stderr, "   Configure memory_dirs and sessions_dirs in mpm_config.json\n")
		return
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Failed to create watcher: %v\n", err)
		return
	}
	defer w.Close()

	for _, dir := range dirs {
		if err := w.Add(dir); err != nil {
			fmt.Fprintf(os.Stderr, "⚠️  Failed to watch %s: %v\n", dir, err)
		}
	}

	if verbose {
		fmt.Printf("🔍 File watcher started. Watching: %v\n", dirs)
	}

	for {
		select {
		case <-ctx.Done():
			if verbose {
				fmt.Println("🛑 File watcher stopping...")
			}
			return
		case event, ok := <-w.Events:
			if !ok {
				return
			}
			ev := eventFromFsnotify(event, dryRun, verbose)
			if ev != nil {
				pool.Submit(*ev)
			}
		case err, ok := <-w.Errors:
			if !ok {
				return
			}
			if verbose {
				fmt.Fprintf(os.Stderr, "⚠️  Watch error: %v\n", err)
			}
		}
	}
}

// eventFromFsnotify converts an fsnotify event to a WatchEvent, or nil if
// the event should be ignored (system files, conflicted files, etc.).
func eventFromFsnotify(event fsnotify.Event, dryRun, verbose bool) *WatchEvent {
	name := filepath.Base(event.Name)
	ext := strings.ToLower(filepath.Ext(name))

	// Skip conflicted files
	if strings.Contains(strings.ToLower(name), "conflicted") {
		return nil
	}

	// Skip OpenClaw live session registry and config files
	if isSystemFile(name) {
		return nil
	}

switch {
	case ext == ".md" && (event.Has(fsnotify.Create) || event.Has(fsnotify.Write)):
		return &WatchEvent{Type: EventMarkdownFile, Path: event.Name, DryRun: dryRun, Verbose: verbose}
	case ext == ".lock" && event.Has(fsnotify.Create):
		// New session lock created → a new session just started.
		// Trigger an orphan sweep: process any .jsonl without a matching .lock
		// (the previous session's file that was never cleaned up).
		jsonlPath := strings.TrimSuffix(event.Name, ".lock")
		if strings.ToLower(filepath.Ext(jsonlPath)) != ".jsonl" {
			return nil
		}
		return &WatchEvent{Type: EventOrphanSweep, Path: filepath.Dir(event.Name), DryRun: dryRun, Verbose: verbose}
	case ext == ".json" && (event.Has(fsnotify.Create) || event.Has(fsnotify.Write)):
		nameLower := strings.ToLower(name)
		if nameLower == "workspace.json" || nameLower == "config.json" {
			return &WatchEvent{Type: EventSystemConfig, Path: event.Name, DryRun: dryRun, Verbose: verbose}
		}
	}

	return nil
}

// startReconciliationSweepGoroutine starts a periodic background sweep that
// reconciles files on disk with ingested records in the database. This catches
// files that fsnotify may have dropped (OS buffer overflow, system sleep, etc).
//
// Interval defaults to 10 minutes. The sweep is low-priority — it always
// submits non-blocking (Select with default) to avoid starving live events.
func startReconciliationSweepGoroutine(ctx context.Context, pool *WorkerPool, dryRun, verbose bool) {
	interval := 10 * time.Minute

	if verbose {
		fmt.Printf("🔄 Reconciliation sweep: every %s\n", interval)
	}

	// Fire once after a short delay (startup may still be processing events)
	time.Sleep(30 * time.Second)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pool.Submit(WatchEvent{Type: EventReconciliationSweep, DryRun: dryRun, Verbose: verbose})
		}
	}
}

// startExternalDBPollGoroutines starts one goroutine per external DB that
// periodically pushes poll events to the worker pool.
func startExternalDBPollGoroutines(ctx context.Context, pool *WorkerPool, dryRun, verbose bool) {
	cfg, err := config.LoadConfig()
	if err != nil {
		if verbose {
			fmt.Fprintf(os.Stderr, "⚠️  Cannot load config for external DB polling: %v\n", err)
		}
		return
	}

	dbs := cfg.GetExternalDbs()
	if len(dbs) == 0 {
		return
	}

	for _, dbc := range dbs {
		dbLabel := dbc.Label
		interval := dbc.IntervalSeconds
		if interval <= 0 {
			interval = 30
		}

		if verbose {
			fmt.Printf("🔄 External DB poller: %s (label=%s, interval=%ds)\n", dbc.Path, dbLabel, interval)
		}

		go func(label string, intervalSec int) {
			ticker := time.NewTicker(time.Duration(intervalSec) * time.Second)
			defer ticker.Stop()

			// Push first poll immediately
			pool.Submit(WatchEvent{Type: EventExternalDBPoll, Label: label, DryRun: dryRun, Verbose: verbose})

			for {
				select {
				case <-ctx.Done():
					if verbose {
						fmt.Printf("🛑 External DB poller %s stopping...\n", label)
					}
					return
				case <-ticker.C:
					pool.Submit(WatchEvent{Type: EventExternalDBPoll, Label: label, DryRun: dryRun, Verbose: verbose})
				}
			}
		}(dbLabel, interval)
	}
}

// =============================================================================
// Utility Functions
// =============================================================================

// deleteFile deletes a file (or logs if dry-run mode)
func (d *watcherDaemon) deleteFile(path, reason string) {
	if d.dryRun {
		fmt.Printf("    dry-run: would delete %s (%s)\n", filepath.Base(path), reason)
		return
	}
	if err := os.Remove(path); err != nil {
		fmt.Fprintf(os.Stderr, "   ⚠️  Failed to delete %s: %v\n", filepath.Base(path), err)
	} else {
		if d.verbose {
			fmt.Printf("   🗑️  Deleted: %s (%s)\n", filepath.Base(path), reason)
		}
	}
}

// archiveFile moves a processed session file to the archive/ subdirectory
// within the same session directory, preventing data loss.
func (d *watcherDaemon) archiveFile(path, reason string) {
	if d.dryRun {
		fmt.Printf("    dry-run: would archive %s (%s)\n", filepath.Base(path), reason)
		return
	}
	archiveDir := filepath.Join(filepath.Dir(path), "archive")
	if err := os.MkdirAll(archiveDir, 0755); err != nil {
		fmt.Fprintf(os.Stderr, "   ⚠️  Failed to create archive dir %s: %v\n", archiveDir, err)
		return
	}
	dest := filepath.Join(archiveDir, filepath.Base(path))
	if err := os.Rename(path, dest); err != nil {
		fmt.Fprintf(os.Stderr, "   ⚠️  Failed to archive %s: %v\n", filepath.Base(path), err)
	} else {
		if d.verbose {
			fmt.Printf("   📦 Archived: %s -> archive/ (%s)\n", filepath.Base(path), reason)
		}
	}
}

// appendToMirror appends a memory record to the mirror file
func (d *watcherDaemon) appendToMirror(id, content, collection string, tags []string, metadata map[string]interface{}) {
	mirrorPath := config.GetMirrorPath()

	record := map[string]interface{}{
		"id":         id,
		"collection": collection,
		"content":    content,
		"tags":       tags,
		"metadata":   metadata,
		"created":    time.Now().UTC().Format(time.RFC3339),
	}

	data, err := json.Marshal(record)
	if err != nil {
		if d.verbose {
			fmt.Fprintf(os.Stderr, "   ⚠️  Mirror marshal failed: %v\n", err)
		}
		return
	}

	f, err := os.OpenFile(mirrorPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		if d.verbose {
			fmt.Fprintf(os.Stderr, "   ⚠️  Mirror file open failed: %v\n", err)
		}
		return
	}
	defer f.Close()

	if _, err := f.WriteString(string(data) + "\n"); err != nil {
		if d.verbose {
			fmt.Fprintf(os.Stderr, "   ⚠️  Mirror write failed: %v\n", err)
		}
	}
}

// isPoisoned checks content against toxic phrases
func (d *watcherDaemon) isPoisoned(content string) (bool, string) {
	// Use the existing poison phrase check from memory store
	return d.memory.IsPoisonedForTest(content)
}

// =============================================================================
// Path Management Handlers
// =============================================================================

// handleWatchAddPath adds a path to the watch configuration
func handleWatchAddPath(args []string) int {
	fs := flag.NewFlagSet("watch add-path", flag.ContinueOnError)
	pathType := fs.String("type", "memory", "Path type: memory or sessions")
	fs.Usage = func() {
		fmt.Println("Usage: mpm watch add-path <path> [--type memory|sessions]")
	}
	if err := fs.Parse(args); err != nil {
		return 1
	}

	if fs.NArg() < 1 {
		fmt.Fprintf(os.Stderr, "Error: path required\n")
		return 1
	}

	path := fs.Arg(0)
	resolved := config.ResolveEnvPath(path)
	if !dirExists(resolved) {
		fmt.Fprintf(os.Stderr, "Error: directory does not exist: %s\n", resolved)
		return 1
	}

	cfg, err := config.LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		return 1
	}

	switch *pathType {
	case "memory":
		// Check if already exists
		for _, p := range cfg.GetMemoryDirs() {
			if p == path || config.ResolveEnvPath(p) == resolved {
				fmt.Printf("Path already in memory_dirs: %s\n", path)
				return 0
			}
		}
		cfg.MemoryDirs = append(cfg.MemoryDirs, path)
		fmt.Printf("Added to memory_dirs: %s\n", path)
	case "sessions":
		for _, p := range cfg.GetSessionsDirs() {
			if p == path || config.ResolveEnvPath(p) == resolved {
				fmt.Printf("Path already in sessions_dirs: %s\n", path)
				return 0
			}
		}
		cfg.SessionsDirs = append(cfg.SessionsDirs, path)
		fmt.Printf("Added to sessions_dirs: %s\n", path)
	default:
		fmt.Fprintf(os.Stderr, "Error: --type must be 'memory' or 'sessions'\n")
		return 1
	}

	if err := config.SaveConfig(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Error saving config: %v\n", err)
		return 1
	}

	fmt.Printf("Path added successfully. Restart watch daemon to pick up changes.\n")
	return 0
}

// handleWatchRemovePath removes a path from the watch configuration
func handleWatchRemovePath(args []string) int {
	fs := flag.NewFlagSet("watch remove-path", flag.ContinueOnError)
	pathType := fs.String("type", "memory", "Path type: memory or sessions")
	fs.Usage = func() {
		fmt.Println("Usage: mpm watch remove-path <path> [--type memory|sessions]")
	}
	if err := fs.Parse(args); err != nil {
		return 1
	}

	if fs.NArg() < 1 {
		fmt.Fprintf(os.Stderr, "Error: path required\n")
		return 1
	}

	path := fs.Arg(0)

	cfg, err := config.LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		return 1
	}

	resolved := config.ResolveEnvPath(path)
	removed := false

	switch *pathType {
	case "memory":
		var newDirs []string
		for _, p := range cfg.GetMemoryDirs() {
			if p == path || config.ResolveEnvPath(p) == resolved {
				removed = true
			} else {
				newDirs = append(newDirs, p)
			}
		}
		cfg.MemoryDirs = newDirs
	case "sessions":
		var newDirs []string
		for _, p := range cfg.GetSessionsDirs() {
			if p == path || config.ResolveEnvPath(p) == resolved {
				removed = true
			} else {
				newDirs = append(newDirs, p)
			}
		}
		cfg.SessionsDirs = newDirs
	default:
		fmt.Fprintf(os.Stderr, "Error: --type must be 'memory' or 'sessions'\n")
		return 1
	}

	if !removed {
		fmt.Fprintf(os.Stderr, "Path not found in %s_dirs: %s\n", *pathType, path)
		return 1
	}

	if err := config.SaveConfig(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "Error saving config: %v\n", err)
		return 1
	}

	fmt.Printf("Path removed. Restart watch daemon to pick up changes.\n")
	return 0
}

// handleWatchListPaths lists all configured watch paths
func handleWatchListPaths(args []string) int {
	cfg, err := config.LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error loading config: %v\n", err)
		return 1
	}

	fmt.Println("\n📁 Configured Watch Paths")
	fmt.Println("══════════════════════════════════════════")

	memDirs := cfg.GetMemoryDirs()
	if len(memDirs) > 0 {
		fmt.Printf("\n  Memory directories (%d):\n", len(memDirs))
		for _, p := range memDirs {
			resolved := config.ResolveEnvPath(p)
			exists := "✅"
			if !dirExists(resolved) {
				exists = "❌ (missing)"
			}
			fmt.Printf("    • %s %s\n", exists, p)
		}
	} else {
		fmt.Printf("\n  Memory directories: none configured\n")
	}

	sesDirs := cfg.GetSessionsDirs()
	if len(sesDirs) > 0 {
		fmt.Printf("\n  Sessions directories (%d):\n", len(sesDirs))
		for _, p := range sesDirs {
			resolved := config.ResolveEnvPath(p)
			exists := "✅"
			if !dirExists(resolved) {
				exists = "❌ (missing)"
			}
			fmt.Printf("    • %s %s\n", exists, p)
		}
	} else {
		fmt.Printf("\n  Sessions directories: none configured\n")
	}

	extDbs := cfg.GetExternalDbs()
	if len(extDbs) > 0 {
		fmt.Printf("\n  External databases (%d):\n", len(extDbs))
		for _, db := range extDbs {
			exists := "✅"
			if !dirExists(db.Path) {
				exists = "❌ (missing)"
			}
			fmt.Printf("    • %s %s [%s] (%ds)\n", exists, db.Path, db.Label, db.IntervalSeconds)
		}
	} else {
		fmt.Printf("\n  External databases: none configured\n")
	}

	fmt.Print("\n══════════════════════════════════════════\n")
	return 0
}
