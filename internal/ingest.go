// ingest.go - OpenClaw Markdown File Ingestion
// Parses .md files from memory/ and sessions/ folders into the unified schema
// Format: # Session: or # Memory: headers, ## subheaders become topics

package internal

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// OpenClawMarkdown representing a parsed document
type OpenClawMarkdown struct {
	DocType    string            // "Session" or "Memory"
	Date       string            // YYYY-MM-DD
	Title      string            // Full header line
	Content    string            // Body content after headers
	Topics     []TopicSection     // ## subheaders
	Sections   []OpenClawSection // Parsed content sections
	SourcePath string
	ContentHash string
}

// TopicSection represents a ## header topic
type TopicSection struct {
	Name    string
	Content string
}

// OpenClawSection represents a parsed content section
type OpenClawSection struct {
	Type    string // "memory", "session", "note"
	Content string
}

// IngestResult holds the result of an ingestion operation
type IngestResult struct {
	SessionID   string
	MemoryIDs   []string
	TopicIDs    []string
	RowsInserted int
}

// processFile parses an OpenClaw markdown file and inserts into the database
func processFile(filePath string, db *sql.DB) error {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("failed to read file: %w", err)
	}

	contentStr := string(content)
	contentHash := hashContent(contentStr)

	// Check for duplicates
	var existingID string
	err = db.QueryRow("SELECT id FROM sessions WHERE content_hash = ?", contentHash).Scan(&existingID)
	if err == nil {
		fmt.Printf("Duplicate detected, deleting: %s\n", filepath.Base(filePath))
		return os.Remove(filePath)
	} else if err != sql.ErrNoRows {
		return fmt.Errorf("failed to check duplicates: %w", err)
	}

	// Parse the markdown
	doc, err := ParseOpenClawMarkdown(contentStr, filePath)
	if err != nil {
		return fmt.Errorf("failed to parse markdown: %w", err)
	}

	// Insert into database
	result, err := InsertOpenClawDocument(db, doc)
	if err != nil {
		return fmt.Errorf("failed to insert document: %w", err)
	}

	fmt.Printf("Ingested: %s (session: %s, %d memories, %d topics)\n", 
		filepath.Base(filePath), result.SessionID[:8]+"...", len(result.MemoryIDs), len(result.TopicIDs))

	// Delete the file after successful insertion
	return os.Remove(filePath)
}

// ParseOpenClawMarkdown parses OpenClaw markdown format into structured data
func ParseOpenClawMarkdown(content, sourcePath string) (*OpenClawMarkdown, error) {
	doc := &OpenClawMarkdown{
		SourcePath: sourcePath,
		Content:    content,
	}

	lines := strings.Split(content, "\n")
	var currentSection []string
	inTopic := false
	currentTopicName := ""

	// Regex patterns
	docTypeRegex := regexp.MustCompile(`^#\s+(Memory|Session)[:-]\s*(.*)$`)
	topicRegex := regexp.MustCompile(`^##\s+(.+)$`)

	for i, line := range lines {
		// Check for main document header (# Memory: or # Session:)
		if i == 0 && strings.HasPrefix(line, "#") {
			matches := docTypeRegex.FindStringSubmatch(line)
			if matches != nil {
				doc.DocType = matches[1]
				doc.Title = strings.TrimSpace(matches[2])
				// Extract date from title if it looks like YYYY-MM-DD
				if dateRegex := regexp.MustCompile(`(\d{4}-\d{2}-\d{2})`); dateRegex.MatchString(doc.Title) {
					doc.Date = dateRegex.FindStringSubmatch(doc.Title)[1]
				}
				continue
			}
		}

		// Check for ## topic headers
		topicMatch := topicRegex.FindStringSubmatch(line)
		if topicMatch != nil {
			// Save previous section content
			if len(currentSection) > 0 && currentTopicName != "" {
				sectionContent := strings.TrimRight(strings.Join(currentSection, "\n"), "\n")
				doc.Topics = append(doc.Topics, TopicSection{
					Name:    currentTopicName,
					Content: sectionContent,
				})
				currentSection = nil
			} else if len(currentSection) > 0 && doc.Sections == nil {
				// Content before any topic - it's the main body
				doc.Content = strings.TrimRight(strings.Join(currentSection, "\n"), "\n")
				currentSection = nil
			}
			currentTopicName = strings.TrimSpace(topicMatch[1])
			inTopic = true
			continue
		}

		// Regular content line
		if inTopic {
			currentSection = append(currentSection, line)
		} else if !strings.HasPrefix(line, "#") {
			currentSection = append(currentSection, line)
		}
	}

	// Don't forget the last section
	if len(currentSection) > 0 && currentTopicName != "" {
		doc.Topics = append(doc.Topics, TopicSection{
			Name:    currentTopicName,
			Content: strings.TrimRight(strings.Join(currentSection, "\n"), "\n"),
		})
	} else if len(currentSection) > 0 && doc.Content == "" {
		doc.Content = strings.TrimRight(strings.Join(currentSection, "\n"), "\n")
	}

	// Calculate content hash
	doc.ContentHash = hashContent(content)

	return doc, nil
}

// InsertOpenClawDocument inserts a parsed document into the database
func InsertOpenClawDocument(db *sql.DB, doc *OpenClawMarkdown) (*IngestResult, error) {
	result := &IngestResult{}

	// Generate IDs
	sessionID := generateID()
	result.SessionID = sessionID

	// Extract auto-tags from source path and content
	autoTags := extractAutoTags(doc)

	// Prepare metadata
	metadata := map[string]interface{}{
		"doc_type": doc.DocType,
		"date":     doc.Date,
		"topics":   len(doc.Topics),
	}
	metadataJSON, _ := json.Marshal(metadata)

	// Insert main session record
	_, err := db.Exec(`
		INSERT INTO sessions (id, session_id, content, content_hash, source_path, metadata)
		VALUES (?, ?, ?, ?, ?, ?)
	`, sessionID, doc.Title, doc.Content, doc.ContentHash, doc.SourcePath, string(metadataJSON))
	if err != nil {
		return nil, fmt.Errorf("failed to insert session: %w", err)
	}

	result.RowsInserted++

	// Insert main content as a memory if substantial
	if len(doc.Content) > 100 {
		memoryID := generateID()
		tags := append([]string{"main"}, autoTags...)
		tagsJSON, _ := json.Marshal(tags)
		_, err := db.Exec(`
			INSERT INTO memories (id, collection, content, session_id, tags, metadata)
			VALUES (?, ?, ?, ?, ?, ?)
		`, memoryID, strings.ToLower(doc.DocType), doc.Content, sessionID, string(tagsJSON), `{"type":"body"}`)
		if err != nil {
			return nil, fmt.Errorf("failed to insert memory: %w", err)
		}
		result.MemoryIDs = append(result.MemoryIDs, memoryID)
		result.RowsInserted++
	}

	// Process each topic (## header)
	for _, topic := range doc.Topics {
		// Insert or get existing topic
		topicID, err := insertTopic(db, topic.Name, topic.Content)
		if err != nil {
			continue // Skip failed topics
		}
		result.TopicIDs = append(result.TopicIDs, topicID)

		// Create membership link for session
		_, err = db.Exec(`
			INSERT OR IGNORE INTO topic_memberships (session_id, topic_id)
			VALUES (?, ?)
		`, sessionID, topicID)
		if err != nil {
			continue
		}

		// Insert topic content as memory
		if len(topic.Content) > 50 {
			memoryID := generateID()
			tags := append([]string{"topic", sanitizeTopicName(topic.Name)}, autoTags...)
			tagsJSON, _ := json.Marshal(tags)
			_, err := db.Exec(`
				INSERT INTO memories (id, collection, content, session_id, tags, metadata)
				VALUES (?, ?, ?, ?, ?, ?)
			`, memoryID, "topic", topic.Content, sessionID, string(tagsJSON), fmt.Sprintf(`{"topic_id":"%s","topic_name":"%s"}`, topicID, topic.Name))
			if err == nil {
				result.MemoryIDs = append(result.MemoryIDs, memoryID)
				result.RowsInserted++

				// Create membership link for this memory (TODO-007)
				db.Exec(`
					INSERT OR IGNORE INTO topic_memberships (memory_id, topic_id)
					VALUES (?, ?)
				`, memoryID, topicID)
			}
		}
	}

	return result, nil
}

// extractAutoTags extracts project, date, keyword, and entity tags from the document
func extractAutoTags(doc *OpenClawMarkdown) []string {
	tagsSet := make(map[string]bool)

	// 1. Project tags from source path
	projectRegex := regexp.MustCompile(`flowbyte/(\w+)`)
	matches := projectRegex.FindAllStringSubmatch(doc.SourcePath, -1)
	for _, match := range matches {
		tagsSet["project:"+match[1]] = true
	}

	// Also check content for project references
	matches = projectRegex.FindAllStringSubmatch(doc.Content, -1)
	for _, match := range matches {
		tagsSet["project:"+match[1]] = true
	}

	// 2. Date tags - extract YYYY-MM-DD from source path or content
	dateRegex := regexp.MustCompile(`(\d{4}-\d{2}-\d{2})`)
	if match := dateRegex.FindStringSubmatch(doc.SourcePath); len(match) > 1 {
		tagsSet["date:"+match[1]] = true
	}
	if match := dateRegex.FindStringSubmatch(doc.Content); len(match) > 1 {
		tagsSet["date:"+match[1]] = true
	}

	// 3. Keyword tags - pattern matching
	keywordPatterns := []string{"bug", "feature", "decision", "lesson", "fix", "refactor", "todo", "hack", "note"}
	contentLower := strings.ToLower(doc.Content)
	for _, kw := range keywordPatterns {
		pattern := regexp.MustCompile(`\b` + kw + `\b`)
		if pattern.MatchString(contentLower) {
			tagsSet["keyword:"+kw] = true
		}
	}

	// 4. Entity tags - @mentions
	mentionRegex := regexp.MustCompile(`@(\w+)`)
	mentionMatches := mentionRegex.FindAllStringSubmatch(doc.Content, -1)
	for _, m := range mentionMatches {
		tagsSet["person:"+m[1]] = true
	}

	// Convert set to slice
	tags := make([]string, 0, len(tagsSet))
	for tag := range tagsSet {
		tags = append(tags, tag)
	}
	return tags
}

// insertTopic inserts a topic or returns existing ID
func insertTopic(db *sql.DB, name, description string) (string, error) {
	// Check if topic exists
	var existingID string
	err := db.QueryRow("SELECT id FROM topics WHERE name = ?", name).Scan(&existingID)
	if err == nil {
		return existingID, nil
	}
	if err != sql.ErrNoRows {
		return "", err
	}

	// Insert new topic
	topicID := generateID()
	_, err = db.Exec(`
		INSERT INTO topics (id, name, description)
		VALUES (?, ?, ?)
	`, topicID, name, truncateString(description, 500))
	if err != nil {
		return "", err
	}
	return topicID, nil
}

// sanitizeTopicName converts topic name to valid tag
func sanitizeTopicName(name string) string {
	// Convert to lowercase, replace spaces with underscores, remove special chars
	sanitized := strings.ToLower(name)
	re := regexp.MustCompile(`[^a-z0-9_]+`)
	sanitized = re.ReplaceAllString(sanitized, "_")
	sanitized = strings.Trim(sanitized, "_")
	return sanitized
}

// truncateString truncates a string to maxLen
func truncateString(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

// hashContent computes SHA256 hash of content
func hashContent(content string) string {
	hash := sha256.Sum256([]byte(content))
	return hex.EncodeToString(hash[:])
}

// generateID generates a unique ID using timestamp + hash
func generateID() string {
	timestamp := time.Now().UnixNano()
	hash := sha256.Sum256([]byte(fmt.Sprintf("%d", timestamp)))
	return hex.EncodeToString(hash[:])[:16]
}

// StartIngestWatcher watches directories for new .md files and ingests them
func StartIngestWatcher(db *sql.DB, dirs []string) error {
	// Process existing files first
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0755); err != nil {
			return fmt.Errorf("failed to create directory %s: %w", dir, err)
		}

		files, err := filepath.Glob(filepath.Join(dir, "*.md"))
		if err != nil {
			return fmt.Errorf("failed to glob %s: %w", dir, err)
		}

		for _, file := range files {
			if err := processFile(file, db); err != nil {
				fmt.Printf("Error processing %s: %v\n", file, err)
			}
		}
	}
	return nil
}
