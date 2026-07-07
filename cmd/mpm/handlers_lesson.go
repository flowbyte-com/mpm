package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/flowbyte-com/mpm-core"
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
	case "shred":
		return handleLessonShred(args[1:])
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
  mpm lesson shred <id>
  mpm lesson stats

Examples:
  mpm lesson add "Check file extensions before executing" --type warning --tags safety,files
  mpm lesson add "Use gofmt for Go code formatting" --type practice --tags go,style
  mpm lesson list
  mpm lesson list --type warning
  mpm lesson search "safety"
  mpm lesson get abc123
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
	if len(args) == 0 {
		return respond("", "Usage: mpm lesson add <content> [--type warning|practice|insight] [--tags tags] [--json]", 1)
	}

	content := ""
	lessonType := "insight"
	var tags []string
	jsonOutput, filteredArgs := ExtractJSONFlag(args)
	args = filteredArgs

	i := 0
	for i < len(args) {
		switch args[i] {
		case "--type":
			if i+1 < len(args) {
				lessonType = args[i+1]
				i += 2
			} else {
				i++
			}
		case "--tags":
			if i+1 < len(args) {
				tags = strings.Split(args[i+1], ",")
				i += 2
			} else {
				i++
			}
		default:
			// First non-flag arg is the content; consume it and stop
			content = args[i]
			// Check if there are more args that aren't flags
			i++
			for i < len(args) && !strings.HasPrefix(args[i], "--") {
				content += " " + args[i]
				i++
			}
			// Skip any remaining flags
			for i < len(args) {
				i++
			}
			break
		}
	}

	if content == "" {
		return respond("", "Usage: mpm lesson add <content>", 1)
	}
	if err := internal.ValidateLessonType(lessonType); err != nil {
		return respond("", err.Error(), 1)
	}

	lessonStore, err := internal.NewLessonStore("")
	if err != nil {
		if jsonOutput {
			data, _ := json.Marshal(map[string]interface{}{"success": false, "error": fmt.Sprintf("Failed to initialize lesson store: %v", err)})
			fmt.Println(string(data))
		} else {
			respond("", fmt.Sprintf("Failed to initialize lesson store: %v", err), 1)
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

	lessonStore, storeErr := internal.NewLessonStore("")
	if storeErr != nil {
		return respond("", fmt.Sprintf("Failed to initialize lesson store: %v", storeErr), 1)
	}
	lessons, listErr := lessonStore.ListLessons(internal.LessonType(lessonType))
	if listErr != nil {
		return respond("", fmt.Sprintf("Failed to list lessons: %v", listErr), 1)
	}

	if len(lessons) == 0 {
		if jsonOutput {
			fmt.Println(`{"lessons": [], "message": "No lessons stored"}`)
		} else {
			respond("No lessons stored.\n", "", 0)
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

	var output strings.Builder
	output.WriteString(fmt.Sprintf("Lessons (%d):\n\n", len(lessons)))
	for _, l := range lessons {
		typeLabel := string(l.Type)
		output.WriteString(fmt.Sprintf("[%s] [%s] %s\n", l.ID, typeLabel, l.Created[:10]))
		snippet := l.Content
		if len(snippet) > 100 {
			snippet = snippet[:100] + "..."
		}
		output.WriteString(fmt.Sprintf("    %s\n\n", snippet))
	}

	return respond(output.String(), "", 0)
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

	lessonStore, storeErr := internal.NewLessonStore("")
	if storeErr != nil {
		return respond("", fmt.Sprintf("Failed to initialize lesson store: %v", storeErr), 1)
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
	lessonStore, storeErr := internal.NewLessonStore("")
	if storeErr != nil {
		return respond("", fmt.Sprintf("Failed to initialize lesson store: %v", storeErr), 1)
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

func handleLessonShred(args []string) int {
	if len(args) == 0 {
		return respond("", "Usage: mpm lesson shred <id>", 1)
	}

	id := args[0]
	lessonStore, storeErr := internal.NewLessonStore("")
	if storeErr != nil {
		return respond("", fmt.Sprintf("Failed to initialize lesson store: %v", storeErr), 1)
	}
	err := lessonStore.DeleteLesson(id)
	if err != nil {
		return respond("", fmt.Sprintf("Failed to shred lesson: %v", err), 1)
	}

	return respond(fmt.Sprintf("Lesson shredded: %s\n", id), "", 0)
}

func handleLessonStats() int {
	lessonStore, storeErr := internal.NewLessonStore("")
	if storeErr != nil {
		return respond("", fmt.Sprintf("Failed to initialize lesson store: %v", storeErr), 1)
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
