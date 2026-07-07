// topic_tools.go — DM methods for topic search and creation.
//
// Topics organize memories into thematic clusters. Two methods:
// search (find by query) and create (with optional description).
package internal

import "fmt"

// SearchTopicsByQuery wraps MemoryStore.SearchTopics. Wire format matches
// callSearchTopics.
func (dm *DatabaseManager) SearchTopicsByQuery(query string, limit int) ([]map[string]interface{}, error) {
	if limit <= 0 {
		limit = 20
	}
	store, err := dm.getSharedStore()
	if err != nil {
		return nil, fmt.Errorf("get memory store: %w", err)
	}
	results, err := store.SearchTopics(query, limit)
	if err != nil {
		return nil, fmt.Errorf("search topics: %w", err)
	}
	items := make([]map[string]interface{}, 0, len(results))
	for _, r := range results {
		items = append(items, map[string]interface{}{
			"id":          r.ID,
			"name":        r.Title,
			"description": r.Snippet,
			"created_at":  r.Created,
		})
	}
	return items, nil
}

// CreateTopicWithDescription wraps CreateTopic with the (description, "", "")
// signature used by the CLI. Mirrors callCreateTopic.
func (dm *DatabaseManager) CreateTopicWithDescription(name, description string) (string, error) {
	return dm.CreateTopic(name, description, "", "")
}