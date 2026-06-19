package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"mpm/internal"
)

// ==================== Memories ====================

func (ws *WebServer) handleMemories(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		ws.listMemories(w, r)
	case "POST":
		ws.addMemory(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (ws *WebServer) handleMemoryByID(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/memories/")
	id := strings.Split(path, "/")[0]

	switch r.Method {
	case "GET":
		ws.getMemory(w, r, id)
	case "PUT":
		ws.editMemory(w, r, id)
	case "DELETE":
		ws.deleteMemory(w, r, id)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (ws *WebServer) listMemories(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	collection := r.URL.Query().Get("collection")
	limit := parseInt(r.URL.Query().Get("limit"), 50)
	offset := parseInt(r.URL.Query().Get("offset"), 0)
	primeOnly := r.URL.Query().Get("is_prime_directive") == "1"

	var mems []map[string]interface{}
	var err error

	if q != "" {
		mems, err = ws.db.SearchMemories(q, collection, primeOnly, limit, offset)
	} else {
		mems, err = ws.db.QueryMemories(collection, primeOnly, limit, offset)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"items":  mems,
		"count":  len(mems),
		"limit":  limit,
		"offset": offset,
	})
}

func (ws *WebServer) addMemory(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad request")
		return
	}

	var input struct {
		Content    string                 `json:"content"`
		Collection string                 `json:"collection"`
		Tags       []string               `json:"tags"`
		Metadata   map[string]interface{} `json:"metadata"`
	}
	if err := json.Unmarshal(body, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}

	if strings.TrimSpace(input.Content) == "" {
		writeError(w, http.StatusBadRequest, "content required")
		return
	}

	if input.Collection == "" {
		input.Collection = "memories"
	}

	// Convert []string tags to map[string]interface{}
	id, err := ws.db.SaveMemory(input.Collection, input.Content, "", input.Tags, input.Metadata, nil, false, 1)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	Broker().Broadcast("memory_saved", map[string]interface{}{
		"id":         id,
		"content":    input.Content,
		"collection": input.Collection,
		"tags":       input.Tags,
	})

	mem, err2 := ws.db.GetMemory(id)
	if err2 != nil {
		writeJSON(w, http.StatusCreated, map[string]string{"id": id})
		return
	}
	writeJSON(w, http.StatusCreated, mem)
}

func (ws *WebServer) getMemory(w http.ResponseWriter, r *http.Request, id string) {
	mem, err := ws.db.GetMemory(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "memory not found")
		return
	}
	writeJSON(w, http.StatusOK, mem)
}

func (ws *WebServer) editMemory(w http.ResponseWriter, r *http.Request, id string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad request")
		return
	}

	var input struct {
		Content  string   `json:"content"`
		Tags     []string `json:"tags"`
		Metadata string   `json:"metadata"`
	}
	if err := json.Unmarshal(body, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}

	existing, err := ws.db.GetMemory(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "memory not found")
		return
	}

	content, _ := existing["content"].(string)
	if input.Content != "" {
		content = input.Content
	}

	metadata := existing["metadata"]
	if input.Metadata != "" {
		json.Unmarshal([]byte(input.Metadata), &metadata)
	}

	// Convert []string tags to map[string]interface{}
	tagsMap := make(map[string]interface{})
	for _, t := range input.Tags {
		tagsMap[t] = true
	}

	metaMap, _ := metadata.(map[string]interface{})
	ws.db.UpdateMemory(id, content, tagsMap, metaMap)
	updated, _ := ws.db.GetMemory(id)
	writeJSON(w, http.StatusOK, updated)
}

func (ws *WebServer) deleteMemory(w http.ResponseWriter, r *http.Request, id string) {
	err := ws.db.ShredMemory(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": id})
}

// ==================== Topics ====================

func (ws *WebServer) handleTopics(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		ws.listTopics(w, r)
	case "POST":
		ws.createTopic(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (ws *WebServer) handleTopicByID(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/topics/")
	parts := strings.Split(path, "/")
	id := parts[0]
	action := ""
	if len(parts) > 1 {
		action = parts[1]
	}

	switch r.Method {
	case "GET":
		if action == "memories" {
			ws.getTopicMemories(w, r, id)
		} else {
			ws.getTopic(w, r, id)
		}
	case "POST":
		if action == "memories" {
			ws.addMemoryToTopic(w, r, id)
		} else {
			writeError(w, http.StatusNotFound, "not found")
		}
	case "DELETE":
		if action == "memories" && len(parts) > 2 {
			ws.removeMemoryFromTopic(w, r, id, parts[2])
		} else {
			ws.deleteTopic(w, r, id)
		}
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (ws *WebServer) listTopics(w http.ResponseWriter, r *http.Request) {
	topics, err := ws.db.ListTopics()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"items": topics, "count": len(topics)})
}

func (ws *WebServer) getTopic(w http.ResponseWriter, r *http.Request, id string) {
	topic, err := ws.db.GetTopic(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "topic not found")
		return
	}
	writeJSON(w, http.StatusOK, topic)
}

func (ws *WebServer) getTopicMemories(w http.ResponseWriter, r *http.Request, id string) {
	memories, err := ws.db.GetTopicMemories(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"items": memories, "count": len(memories)})
}

func (ws *WebServer) createTopic(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad request")
		return
	}

	var input struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(body, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}

	if strings.TrimSpace(input.Name) == "" {
		writeError(w, http.StatusBadRequest, "name required")
		return
	}

	id, err := ws.db.CreateTopic(input.Name, input.Description, "", "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	Broker().Broadcast("topic_created", map[string]interface{}{
		"id":   id,
		"name": input.Name,
	})

	topic, _ := ws.db.GetTopic(id)
	writeJSON(w, http.StatusCreated, topic)
}

func (ws *WebServer) deleteTopic(w http.ResponseWriter, r *http.Request, id string) {
	err := ws.db.DeleteTopic(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": id})
}

func (ws *WebServer) addMemoryToTopic(w http.ResponseWriter, r *http.Request, topicID string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad request")
		return
	}

	var input struct {
		MemoryID string `json:"memory_id"`
		Role     string `json:"role"`
	}
	if err := json.Unmarshal(body, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}

	if input.MemoryID == "" {
		writeError(w, http.StatusBadRequest, "memory_id required")
		return
	}

	err = ws.db.AddMemoryToTopic(input.MemoryID, topicID, input.Role)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]string{"added": input.MemoryID})
}

func (ws *WebServer) removeMemoryFromTopic(w http.ResponseWriter, r *http.Request, topicID, memoryID string) {
	err := ws.db.RemoveMemoryFromTopic(memoryID, topicID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"removed": memoryID})
}

// ==================== Lessons ====================

func (ws *WebServer) handleLessons(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		ws.listLessons(w, r)
	case "POST":
		ws.addLesson(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (ws *WebServer) handleLessonByID(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/lessons/")
	id := strings.Split(path, "/")[0]

	switch r.Method {
	case "GET":
		ws.getLesson(w, r, id)
	case "DELETE":
		ws.deleteLesson(w, r, id)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (ws *WebServer) listLessons(w http.ResponseWriter, r *http.Request) {
	lessonType := r.URL.Query().Get("type")
	lessons, err := ws.db.ListLessons(lessonType)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"items": lessons, "count": len(lessons)})
}

func (ws *WebServer) getLesson(w http.ResponseWriter, r *http.Request, id string) {
	lesson, err := ws.db.GetLesson(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "lesson not found")
		return
	}
	writeJSON(w, http.StatusOK, lesson)
}

func (ws *WebServer) addLesson(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad request")
		return
	}

	var input struct {
		Content string   `json:"content"`
		Type    string   `json:"type"`
		Tags    []string `json:"tags"`
	}
	if err := json.Unmarshal(body, &input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json")
		return
	}

	if strings.TrimSpace(input.Content) == "" {
		writeError(w, http.StatusBadRequest, "content required")
		return
	}

	if input.Type == "" {
		input.Type = "insight"
	}

	lesson, err := ws.db.AddLesson(input.Content, internal.LessonType(input.Type), input.Tags, "")
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	Broker().Broadcast("lesson_saved", map[string]interface{}{
		"id":   lesson.ID,
		"type": input.Type,
		"fact": input.Content,
	})

	writeJSON(w, http.StatusCreated, lesson)
}

func (ws *WebServer) deleteLesson(w http.ResponseWriter, r *http.Request, id string) {
	err := ws.db.DeleteLesson(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": id})
}

// ==================== References ====================

func (ws *WebServer) handleReferences(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case "GET":
		ws.listReferences(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (ws *WebServer) handleReferenceByID(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/references/")
	id := strings.Split(path, "/")[0]

	switch r.Method {
	case "GET":
		ws.getReference(w, r, id)
	case "DELETE":
		ws.deleteReference(w, r, id)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (ws *WebServer) listReferences(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	limit := parseInt(r.URL.Query().Get("limit"), 50)
	offset := parseInt(r.URL.Query().Get("offset"), 0)

	var refs []map[string]interface{}
	var err error

	if q != "" {
		refs, err = ws.db.SearchReferences(q, limit)
	} else {
		refs, err = ws.db.ListReferences(limit, offset)
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"items":  refs,
		"count":  len(refs),
		"limit":  limit,
		"offset": offset,
	})
}

func (ws *WebServer) getReference(w http.ResponseWriter, r *http.Request, id string) {
	ref, err := ws.db.GetReference(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "reference not found")
		return
	}
	writeJSON(w, http.StatusOK, ref)
}

func (ws *WebServer) deleteReference(w http.ResponseWriter, r *http.Request, id string) {
	err := ws.db.DeleteReference(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"deleted": id})
}

// ==================== Search ====================

func (ws *WebServer) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if strings.TrimSpace(q) == "" {
		writeJSON(w, http.StatusOK, map[string]interface{}{"memories": []interface{}{}, "topics": []interface{}{}, "lessons": []interface{}{}})
		return
	}

	limit := parseInt(r.URL.Query().Get("limit"), 20)

	results := map[string]interface{}{}

	// Search memories
	mems, _ := ws.db.SearchMemories(q, "", false, limit, 0)
	results["memories"] = mems

	// Search topics
	topics, _ := ws.db.SearchTopics(q, limit)
	results["topics"] = topics

	// Search lessons
	lessons, _ := ws.db.SearchLessons(q, limit)
	results["lessons"] = lessons

	writeJSON(w, http.StatusOK, results)
}

// handleInternalBroadcast relays tool-call events from separate mpm call processes
// into the shared SSE broker. This lets CLI tool calls drive live telemetry.
func (ws *WebServer) handleInternalBroadcast(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var input struct {
		EventType string      `json:"eventType"`
		Payload  interface{} `json:"payload"`
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := json.Unmarshal(body, &input); err != nil || input.EventType == "" {
		http.Error(w, "invalid json or missing eventType", http.StatusBadRequest)
		return
	}
	Broker().Broadcast(input.EventType, input.Payload)
	writeJSON(w, http.StatusOK, map[string]string{"ok": "1"})
}
