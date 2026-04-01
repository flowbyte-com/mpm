package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"mpm/internal/config"

	mpminternal "mpm/internal"

	"github.com/fsnotify/fsnotify"
)

// =============================================================================
// Watch Command - fsnotify-based file watcher and ingestion daemon
// =============================================================================

func cmdWatch(args []string) bool {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	// Watch specific directories (defaults from config)
	watchDir := fs.String("dir", "", "Watch directory (default: memory dir from config)")
	sessionDir := fs.String("sessions", "", "Session directory (default: sessions dir from config)")
	dryRun := fs.Bool("dry-run", false, "Process files but don't delete them")
	once := fs.Bool("once", false, "Run startup sweep only, then exit")
	verbose := fs.Bool("v", false, "Verbose output")
	fs.Usage = func() {
		fmt.Println("Usage: mpm watch [options]")
		fmt.Println("\nWatch options:")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return false
	}

	// Determine watch directories
	watcherDirs := resolveWatchDirs(*watchDir, *sessionDir)
	if len(watcherDirs) == 0 {
		fmt.Fprintf(os.Stderr, "❌ No directories to watch. Configure memory_dir and sessions_dir in mpm_config.json\n")
		return false
	}

	// Initialize the watcher
	w, err := fsnotify.NewWatcher()
	if err != nil {
		fmt.Fprintf(os.Stderr, "❌ Failed to create watcher: %v\n", err)
		return false
	}
	defer w.Close()

	// Create the daemon with database connection
	daemon := newWatcherDaemon(w, watcherDirs, *dryRun, *verbose)
	if daemon == nil {
		return false // Error already printed
	}
	if !daemon.IsReady() {
		fmt.Fprintf(os.Stderr, "❌ Watch daemon not ready (database initialization failed)\n")
		return false
	}

	// Start the daemon
	fmt.Printf("🔍 MPM Watch Daemon starting...\n")
	fmt.Printf("   Watching: %v\n", watcherDirs)
	fmt.Printf("   Dry run: %v\n", *dryRun)

	if *once {
		fmt.Printf("\n⚡ Running one-shot startup sweep...\n")
		daemon.startupSweep()
		fmt.Printf("✅ Startup sweep complete.\n")
		return true
	}

	// Add directories to watcher
	for _, dir := range watcherDirs {
		if err := w.Add(dir); err != nil {
			fmt.Fprintf(os.Stderr, "❌ Failed to watch %s: %v\n", dir, err)
			return false
		}
	}

	// Run startup sweep first
	fmt.Printf("\n⚡ Running initial startup sweep...\n")
	daemon.startupSweep()
	fmt.Printf("✅ Startup sweep complete. Watching for changes...\n\n")

	// Setup signal handling for graceful shutdown
	done := make(chan bool)
	go daemon.handleSignals(done)

	// Main event loop
	for {
		select {
		case event, ok := <-w.Events:
			if !ok {
				return true
			}
			daemon.handleEvent(event)

		case err, ok := <-w.Errors:
			if !ok {
				return true
			}
			if *verbose {
				fmt.Fprintf(os.Stderr, "⚠️  Watch error: %v\n", err)
			}

		case <-done:
			fmt.Printf("\n👋 Watch daemon shutting down...\n")
			return true
		}
	}
}

// resolveWatchDirs determines which directories to watch for the daemon
// Priority: 1) CLI flags, 2) Config file (memory_dir/sessions_dir), 3) OpenClaw workspace defaults
// NOTE: These are the paths the DAEMON watches for OpenClaw-created .md and session files.
//       The MPM database is separate and always at mpm/src/db/mpm_memory.db.
func resolveWatchDirs(memDir, sesDir string) []string {
	var dirs []string

	// Load config for fallback paths
	cfg, _ := config.LoadConfig()

	// Determine memory directory to watch
	if memDir == "" {
		memDir = cfg.MemoryDir
	}
	if memDir != "" {
		resolved := config.ResolveEnvPath(memDir)
		if dirExists(resolved) {
			dirs = append(dirs, resolved)
		}
	} else {
		// Default: OpenClaw workspace memory directory
		workspace := config.GetWorkspace()
		defaultMem := filepath.Join(workspace, "memory")
		if dirExists(defaultMem) {
			dirs = append(dirs, defaultMem)
		}
	}

	// Determine sessions directory to watch
	if sesDir == "" {
		sesDir = cfg.SessionsDir
	}
	if sesDir != "" {
		resolved := config.ResolveEnvPath(sesDir)
		if dirExists(resolved) {
			dirs = append(dirs, resolved)
		}
	} else {
		// Default: OpenClaw agents sessions directory
		homeDir, _ := os.UserHomeDir()
		defaultSes := filepath.Join(homeDir, ".openclaw", "agents", "main", "sessions")
		if dirExists(defaultSes) {
			dirs = append(dirs, defaultSes)
		}
	}

	return dirs
}

// dirExists checks if a directory exists
func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
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
	topicCache map[string]*topicCluster // topic name -> cluster info
}

type topicCluster struct {
	ID       string
	Name     string
	Tag      string
	MemIDs   []string
	Count    int
	LastSeen time.Time
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
		watcher:    w,
		dirs:       dirs,
		db:         db,
		memory:     memoryStore,
		dryRun:     dryRun,
		verbose:    verbose,
		stopCh:     make(chan struct{}),
		topicCache: make(map[string]*topicCluster),
	}
}

// IsReady returns true if the daemon is properly initialized
func (d *watcherDaemon) IsReady() bool {
	return d != nil && d.db != nil && d.db.DB != nil
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

		// Skip OpenClaw system metadata files (not session data)
		switch nameLower {
		case "sessions.json", "session.json", "workspace.json", "config.json":
			if d.verbose {
				fmt.Printf("   ⏭️  Skipping OpenClaw system file: %s\n", name)
			}
			continue
		}

		ext := strings.ToLower(filepath.Ext(name))

		switch ext {
		case ".md":
			// Route A: Process and delete .md memory files
			d.processMarkdownFile(path, true)

		case ".json":
			// Route C: OpenClaw system config files — parse, hash, store (never delete)
			if nameLower == "sessions.json" || nameLower == "workspace.json" || nameLower == "config.json" {
				d.processSessionsConfig(path)
			}

		case ".jsonl":
			// Skip in sweep — only process via handleLockRemoved when .lock is explicitly
			// removed (signals session complete). Without a .lock, the session is still being
			// written and must not be touched.
		}
	}
}

// =============================================================================
// PART 2: Watcher Event Handlers
// =============================================================================

func (d *watcherDaemon) handleEvent(event fsnotify.Event) {
	path := event.Name
	name := filepath.Base(path)
	ext := strings.ToLower(filepath.Ext(name))

	// Skip conflicted files
	if strings.Contains(strings.ToLower(name), "conflicted") {
		return
	}

	switch {
	// Route A: .md files - Create or Write events
	case ext == ".md" && (event.Has(fsnotify.Create) || event.Has(fsnotify.Write)):
		d.processMarkdownFile(path, false)

	// Route B: .lock files - Remove events (lock deleted = session complete)
	case ext == ".lock" && event.Has(fsnotify.Remove):
		d.handleLockRemoved(path)
	}
}

// handleLockRemoved is triggered when a .lock file is deleted
func (d *watcherDaemon) handleLockRemoved(lockPath string) {
	// Derive .jsonl path from .lock path
	jsonlPath := strings.TrimSuffix(lockPath, ".lock")

	// Verify .jsonl exists
	if _, err := os.Stat(jsonlPath); os.IsNotExist(err) {
		if d.verbose {
			fmt.Printf("   ⏭️  Lock removed but no .jsonl found: %s\n", filepath.Base(lockPath))
		}
		return
	}

	d.processSessionFile(jsonlPath, false)
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

	// Route A: Insert as LTM (is_long_term = true, weight = 10)
	memID, err := d.ingestAsLongTermMemory(contentStr, path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "   ❌ Failed to ingest %s: %v\n", name, err)
		return
	}

	// Check for topic clustering
	d.checkTopicClustering()

	fmt.Printf("   %s LTM memory saved: %s (id: %s)\n", prefix, name, memID[:8])
	d.deleteFile(path, "processed successfully")
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

	// Insert facts as regular memories (weight = 1)
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

	d.deleteFile(path, "processed successfully")
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
		"session_key":      "",
		"session_id":      "",
		"channel":          "",
		"model":            "",
		"provider":         "",
		"skills_count":     0,
		"skills":           []string{},
		"workspace_files":  []string{},
		"system_prompt_chars": 0,
		"context_tokens":   0,
		"runtime_ms":       int64(0),
		"updated_at":       int64(0),
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

	// Convert string tags to map format for SaveMemory
	tagsMap := make(map[string]interface{})
	for _, t := range tags {
		tagsMap[t] = true
	}

	// Prepare metadata with LTM flag
	metadata := map[string]interface{}{
		"is_long_term": true,
		"weight":       10,
		"source_path":  sourcePath,
		"promoted_at":  time.Now().UTC().Format(time.RFC3339),
	}

	// Generate hash-based embedding
	embedding := mpminternal.HashEmbed(content)

	// Save to database
	id, err := d.db.SaveMemory("memories", content, "", tagsMap, metadata, embedding)
	if err != nil {
		return "", err
	}

	// Also append to mirror
	d.appendToMirror(id, content, "memories", tags, metadata)

	return id, nil
}

// ingestAsSessionMemory inserts extracted session facts (weight = 1)
func (d *watcherDaemon) ingestAsSessionMemory(content, sourcePath string) (string, error) {
	// Extract keywords/tags
	tags := d.extractKeywords(content)
	tags = append(tags, "session-fact")

	// Convert string tags to map format for SaveMemory
	tagsMap := make(map[string]interface{})
	for _, t := range tags {
		tagsMap[t] = true
	}

	// Prepare metadata
	metadata := map[string]interface{}{
		"is_long_term": false,
		"weight":        1,
		"source_path":   sourcePath,
		"promoted_at":   time.Now().UTC().Format(time.RFC3339),
	}

	// Generate embedding
	embedding := mpminternal.HashEmbed(content)

	// Save to database
	id, err := d.db.SaveMemory("memories", content, "", tagsMap, metadata, embedding)
	if err != nil {
		return "", err
	}

	// Also append to mirror
	d.appendToMirror(id, content, "memories", tags, metadata)

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

	// Patterns that indicate declarative statements worth preserving
	factPatterns := []*regexp.Regexp{
		regexp.MustCompile(`(?i)^.*"(name|key|value|path|url|endpoint|config|setting|preference)":\s*".*"$`), // JSON key-value pairs
		regexp.MustCompile(`(?i)^.*'(name|key|value|path|url|endpoint|config|setting|preference)':\s*'.*'$`), // JSON single-quoted
		regexp.MustCompile(`(?i)^\s*[-*]\s+[A-Z].*:.*`),                                                     // Bullet points with capital first letter (likely facts)
		regexp.MustCompile(`(?i)^(user|preference|config|setting|path|name|key|value)[\s:-]+.+`),            // Explicit fact patterns
	}

	// Patterns to skip (conversational filler) - only for non-JSON lines
	skipPatterns := []*regexp.Regexp{
		regexp.MustCompile(`(?i)^(okay|ok|yes|yeah|yep|sure|great|thanks|thank you|i think|i believe|i feel)`),
		regexp.MustCompile(`(?i)^(the user|they|them|this is|here is|i'll|i will|i can|i could|let me|would you|could you)`),
		regexp.MustCompile(`^//.*`),   // Comments
		regexp.MustCompile(`^\s*#.*`), // Code/comments
	}

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
	CWD       string `json:"cwd,omitempty"`
	ModelID   string `json:"modelId,omitempty"`
	Provider  string `json:"provider,omitempty"`
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
	// Look for explicit fact patterns
	factPatterns := []*regexp.Regexp{
		regexp.MustCompile(`(?i)^(my |the |user )?[a-z]+ [a-z]+ is|are|has|was|were`),
		regexp.MustCompile(`(?i)^(remember|note|fact|important|preference)`),
		regexp.MustCompile(`(?i)^\(.*\) `), // Message metadata prefix
	}
	for _, p := range factPatterns {
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

	// Extract # tags
	hashPattern := regexp.MustCompile(`#([a-zA-Z][a-zA-Z0-9_-]*)`)
	for _, match := range hashPattern.FindAllStringSubmatch(content, -1) {
		tag := strings.ToLower(match[1])
		if !seen[tag] && len(tag) > 2 {
			tags = append(tags, tag)
			seen[tag] = true
		}
	}

	// Extract CamelCase words (potential project/component names)
	camelPattern := regexp.MustCompile(`([A-Z][a-z]+[A-Z][a-zA-Z]*)`)
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

// checkTopicClustering implements the Topic Clustering trigger
// When N or more LTM memories share the same tag, create a Topic record
func (d *watcherDaemon) checkTopicClustering() {
	const clusterThreshold = 3 // N = 3 memories with same tag triggers clustering

	d.mu.Lock()
	defer d.mu.Unlock()

	// Get all LTM memories (is_long_term = true, weight = 10)
	ltmMemories, err := d.getLTMMemories()
	if err != nil {
		if d.verbose {
			fmt.Fprintf(os.Stderr, "⚠️  Failed to get LTM memories: %v\n", err)
		}
		return
	}

	// Count tag occurrences
	tagCounts := make(map[string][]string) // tag -> []memoryIDs
	for _, mem := range ltmMemories {
		for _, tag := range mem.Tags {
			tagCounts[tag] = append(tagCounts[tag], mem.ID)
		}
	}

	// Check for tags that meet the threshold
	for tag, memIDs := range tagCounts {
		if len(memIDs) < clusterThreshold {
			continue
		}

		// Check if we already have a topic for this tag
		if _, exists := d.topicCache[tag]; exists {
			// Update existing cluster
			d.topicCache[tag].MemIDs = memIDs
			d.topicCache[tag].Count = len(memIDs)
			d.topicCache[tag].LastSeen = time.Now()
			continue
		}

		// Check if topic already exists in database
		existingTopic, err := d.findTopicByTag(tag)
		if err == nil && existingTopic != nil {
			d.topicCache[tag] = &topicCluster{
				ID:       existingTopic.ID,
				Name:     existingTopic.Name,
				Tag:      tag,
				MemIDs:   memIDs,
				Count:    len(memIDs),
				LastSeen: time.Now(),
			}
			continue
		}

		// Create new topic
		topicID, err := d.createTopicFromCluster(tag, memIDs)
		if err != nil {
			if d.verbose {
				fmt.Fprintf(os.Stderr, "⚠️  Failed to create topic for tag '%s': %v\n", tag, err)
			}
			continue
		}

		// Add to cache
		d.topicCache[tag] = &topicCluster{
			ID:       topicID,
			Name:     formatTopicName(tag),
			Tag:      tag,
			MemIDs:   memIDs,
			Count:    len(memIDs),
			LastSeen: time.Now(),
		}

		fmt.Printf("   🏷️  Topic auto-created: '%s' (tag: %s, %d memories)\n", d.topicCache[tag].Name, tag, len(memIDs))
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
	// Query memories with weight >= 10 (LTM flag)
	query := `SELECT id, content, tags, metadata, created_at FROM memories WHERE metadata LIKE '%is_long_term":true%' OR metadata LIKE '%weight":10%' LIMIT 1000`

	rows, err := d.db.DB.Query(query)
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
	tagPattern := "%" + tag + "%"

	row := d.db.DB.QueryRow(query, tagPattern, tagPattern)
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
	_, err := d.db.DB.Exec(`
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
		row := d.db.DB.QueryRow("SELECT session_id FROM memories WHERE id = ?", memID)
		if err := row.Scan(&sessionID); err != nil {
			continue
		}

		// Get or create session record
		if sessionID == "" {
			sessionID = mpminternal.GenerateID()
		}

		// Insert membership link
		d.db.DB.Exec(`
			INSERT OR IGNORE INTO topic_memberships (session_id, topic_id)
			VALUES (?, ?)
		`, sessionID, topicID)
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
// Utility Functions
// =============================================================================

// deleteFile deletes a file (or logs if dry-run mode)
func (d *watcherDaemon) deleteFile(path, reason string) {
	if d.dryRun {
		fmt.Printf("   � dry-run: would delete %s (%s)\n", filepath.Base(path), reason)
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
		return
	}

	f, err := os.OpenFile(mirrorPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()

	f.WriteString(string(data) + "\n")
}

// isPoisoned checks content against toxic phrases
func (d *watcherDaemon) isPoisoned(content string) (bool, string) {
	// Use the existing poison phrase check from memory store
	return d.memory.IsPoisonedForTest(content)
}
