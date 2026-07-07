package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/flowbyte-com/mpm-core"
)

func handleTopic(args []string) int {
	if len(args) < 1 {
		return handleTopicHelp()
	}

	subCmd := args[0]
	switch subCmd {
	case "help":
		return handleTopicHelp()
	case "add":
		return handleTopicAdd(args[1:])
	case "search":
		return handleTopicSearch(args[1:])
	case "show":
		return handleTopicShow(args[1:])
	case "promote":
		return handleTopicPromote(args[1:])
	case "shred":
		if len(args) < 2 {
			return handleTopicHelp()
		}
		return handleShredTopic(args[1])
	case "list":
		return handleTopicList(args[1:])
	default:
		return handleTopicHelp()
	}
}

func handleTopicHelp() int {
	output := `mpm topic - Topic operations

Usage:
  mpm topic add <name> [description]    Add a new topic
  mpm topic search <query>            Search topics
  mpm topic show <id>                  Show topic by ID
  mpm topic promote <id>               Promote topic to memory
  mpm topic shred <id>                 Secure delete topic
  mpm topic list                       List topics

Examples:
  mpm topic add "golang patterns" "Things to remember about Go"
  mpm topic search "golang"
  mpm topic show abc123
  mpm topic promote abc123
`
	return respond(output, "", 0)
}

func handleTopicAdd(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm topic add <name> [description] [--json]", 1)
	}

	name := args[0]
	description := ""
	jsonOutput, cleanedArgs := ExtractJSONFlag(args)

	// Extract description from non-flag args
	for _, arg := range cleanedArgs[1:] {
		if !strings.HasPrefix(arg, "-") {
			description = arg
		}
	}

	store := getMemoryStore()
	db := store.DB

	// Add to topics table
	id := internal.GenerateID()
	_, err := db.Exec(`
		INSERT INTO topics (id, name, description)
		VALUES (?, ?, ?)
	`, id, name, description)

	if err != nil {
		if jsonOutput {
			data, _ := json.Marshal(map[string]interface{}{"success": false, "error": fmt.Sprintf("Failed to add topic: %v", err)})
			fmt.Println(string(data))
		} else {
			respond("", fmt.Sprintf("Failed to add topic: %v", err), 1)
		}
		return 1
	}

	if jsonOutput {
		data, _ := json.Marshal(map[string]interface{}{
			"success":     true,
			"id":          id,
			"name":        name,
			"description": description,
		})
		fmt.Println(string(data))
	} else {
		respond(fmt.Sprintf("Topic added: %s\n", name), "", 0)
	}
	return 0
}

func handleTopicSearch(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm topic search <query> [--json]", 1)
	}

	query := args[0]
	jsonOutput, queryArgs := ExtractJSONFlag(args)
	if len(queryArgs) > 0 {
		query = queryArgs[0]
	}

	store := getMemoryStore()

	results, err := store.SearchTopics(query, 20)
	if err != nil {
		if jsonOutput {
			data, _ := json.Marshal(map[string]interface{}{"query": query, "results": []interface{}{}, "error": fmt.Sprintf("Search failed: %v", err)})
			fmt.Println(string(data))
		} else {
			respond("", fmt.Sprintf("Search failed: %v", err), 1)
		}
		return 1
	}

	if len(results) == 0 {
		if jsonOutput {
			data, _ := json.Marshal(map[string]interface{}{"query": query, "results": []interface{}{}, "message": "No topics found"})
			fmt.Println(string(data))
		} else {
			respond("No topics found.\n", "", 0)
		}
		return 0
	}

	if jsonOutput {
		type topicResult struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Description string `json:"description"`
			CreatedAt   string `json:"created_at"`
		}
		result := make([]topicResult, 0, len(results))
		for _, r := range results {
			result = append(result, topicResult{
				ID:          r.ID,
				Name:        r.Snippet,
				Description: r.Snippet,
				CreatedAt:   r.Created,
			})
		}
		data, _ := json.Marshal(map[string]interface{}{"query": query, "results": result})
		fmt.Println(string(data))
		return 0
	}

	var output strings.Builder
	output.WriteString(fmt.Sprintf("Found %d topics:\n\n", len(results)))

	for _, r := range results {
		snippet := r.Snippet
		if len(snippet) > 500 {
			snippet = snippet[:500] + "..."
		}
		output.WriteString(fmt.Sprintf("[%s]\n", r.ID))
		output.WriteString(fmt.Sprintf("    %s\n\n", snippet))
	}

	return respond(output.String(), "", 0)
}

func handleTopicShow(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm topic show <id> [--json]", 1)
	}

	id := args[0]
	jsonOutput, _ := ExtractJSONFlag(args)

	store := getMemoryStore()
	db := store.DB

	var name, description, created string
	err := db.QueryRow(`
		SELECT name, description, created_at FROM topics WHERE id = ?
	`, id).Scan(&name, &description, &created)

	if err != nil {
		if jsonOutput {
			data, _ := json.Marshal(map[string]interface{}{"success": false, "error": "Topic not found: " + id})
			fmt.Println(string(data))
		} else {
			respond("", fmt.Sprintf("Topic not found: %s\n", id), 1)
		}
		return 1
	}

	if jsonOutput {
		// Get memory IDs for this topic
		memoryIDs := []string{}
		if rows, err := db.Query("SELECT memory_id FROM topic_memberships WHERE topic_id = ?", id); err == nil {
			for rows.Next() {
				var mid string
				rows.Scan(&mid)
				memoryIDs = append(memoryIDs, mid)
			}
			rows.Close()
		}
		chunkCount := len(memoryIDs)
		data, _ := json.Marshal(map[string]interface{}{
			"id":          id,
			"name":        name,
			"description": description,
			"created_at":  created,
			"memory_ids":  memoryIDs,
			"chunk_count": chunkCount,
		})
		fmt.Println(string(data))
		return 0
	}

	var output strings.Builder
	output.WriteString(fmt.Sprintf("ID:          %s\n", id))
	output.WriteString(fmt.Sprintf("Name:        %s\n", name))
	output.WriteString(fmt.Sprintf("Created:     %s\n", created))
	if description != "" {
		output.WriteString(fmt.Sprintf("\nDescription:\n%s\n", description))
	}

	// Fetch top 3 memories for this topic
	if dm := getDBConcrete(); dm != nil {
		memories, total, _ := dm.GetTopicTopMemories(id, 3)
		if len(memories) > 0 {
			bold := "\033[1m"
			reset := "\033[0m"
			dim := "\033[2m"

			output.WriteString(fmt.Sprintf("\n%s─ Top Memories ──────────────────────────%s\n", bold, reset))
			for i, mem := range memories {
				content := mem.Content
				if len(content) > 120 {
					content = content[:120] + "…"
				}
				output.WriteString(fmt.Sprintf("\n%d. %s\n", i+1, content))
			}
			if total > 3 {
				remaining := total - 3
				output.WriteString(fmt.Sprintf("\n%s[+ %d other linked memories]%s\n", dim, remaining, reset))
			}
			output.WriteString(fmt.Sprintf("%s─────────────────────────────────────────%s\n", bold, reset))
		}
	}

	return respond(output.String(), "", 0)
}

func handleTopicPromote(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm topic promote <id>", 1)
	}

	id := args[0]
	store := getMemoryStore()

	err := store.PromoteTopicToMemory(id, "memories", nil)
	if err != nil {
		return respond("", fmt.Sprintf("Failed to promote topic: %v", err), 1)
	}

	return respond(fmt.Sprintf("Topic promoted to memory: %s\n", id), "", 0)
}

func handleTopicList(args []string) int {
	jsonOutput, _ := ExtractJSONFlag(args)

	store := getMemoryStore()
	db := store.DB

	rows, err := db.Query(`
		SELECT id, name, description, created_at FROM topics
		ORDER BY created_at DESC LIMIT 20
	`)
	if err != nil {
		return respond("", fmt.Sprintf("Failed to list topics: %v", err), 1)
	}
	defer rows.Close()

	if jsonOutput {
		type topicEntry struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Description string `json:"description"`
			CreatedAt   string `json:"created_at"`
		}
		result := make([]topicEntry, 0)
		for rows.Next() {
			var id, name, description, created string
			if err := rows.Scan(&id, &name, &description, &created); err != nil {
				continue
			}
			result = append(result, topicEntry{
				ID:          id,
				Name:        name,
				Description: description,
				CreatedAt:   created,
			})
		}
		data, _ := json.Marshal(map[string]interface{}{"topics": result})
		fmt.Println(string(data))
		return 0
	}

	var output strings.Builder
	output.WriteString("Topics:\n\n")

	for rows.Next() {
		var id, name, description, created string
		if err := rows.Scan(&id, &name, &description, &created); err != nil {
			continue
		}
		if len(description) > 100 {
			description = description[:100] + "..."
		}
		output.WriteString(fmt.Sprintf("[%s] %s\n", id, name))
		if description != "" {
			output.WriteString(fmt.Sprintf("    %s\n", description))
		}
	}

	return respond(output.String(), "", 0)
}
