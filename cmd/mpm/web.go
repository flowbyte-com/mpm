package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"

	"mpm/internal"
)

//go:embed web/dashboard.html
var webDashboardHTML string

// cmdWeb runs the HTTP dashboard server
func cmdWeb(basePath string, args []string) {
	port := "8765"
	if len(args) > 0 {
		port = args[0]
	}
	
	// Serve static HTML
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(webDashboardHTML))
	})
	
	// API endpoints
	http.HandleFunc("/api/status", func(w http.ResponseWriter, r *http.Request) {
		status := getStatus(basePath)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(status)
	})
	
	http.HandleFunc("/api/exec", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
			return
		}
		
		cmd := r.URL.Query().Get("cmd")
		if cmd == "" {
			http.Error(w, "No command provided", http.StatusBadRequest)
			return
		}
		
		// Execute command and capture output
		cmdParts := strings.Split(cmd, " ")
		output, err := exec.Command(cmdParts[0], cmdParts[1:]...).CombinedOutput()
		if err != nil {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"status":  "error",
				"cmd":     cmd,
				"output":  string(output),
				"error":   err.Error(),
			})
			return
		}
		
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"status": "success",
			"cmd":    cmd,
			"output": string(output),
		})
	})
	
	fmt.Printf("🌐 SymAI mpm Web Dashboard running on http://localhost:%s\n", port)
	fmt.Println("Press Ctrl+C to stop")
	
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		fmt.Fprintf(os.Stderr, "Error starting server: %v\n", err)
	}
}

func getStatus(basePath string) map[string]interface{} {
	pm := internal.NewPersonaManager(basePath)
	mm := internal.NewModeManager(basePath)
	
	// SQLite mode - no ChromaDB wrapper needed
	sqliteMode := true
	
	// Get memory store for stats (mirror entries count)
	memStore := internal.NewMemoryStore("")
	
	activePersona, _ := pm.GetActive()
	activeModes, _ := mm.GetActive()
	memStats := memStore.Stats()
	
	personaName := activePersona
	if personaName == "" {
		personaName = "default"
	}
	
	return map[string]interface{}{
		"persona":      personaName,
		"modes":        activeModes,
		"sqlite_mode":  sqliteMode,
		"memories":     memStats["mirror_entries"],
	}
}
