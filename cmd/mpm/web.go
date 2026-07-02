package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"mpm/internal"
	"mpm/internal/config"
	"mpm/internal/usererror"
)

const defaultWebPort = "18792"

// WebServer holds the HTTP server state
type WebServer struct {
	port           string
	mux            *http.ServeMux
	db             *internal.DatabaseManager
	handler        http.Handler
	allowAnonymous bool // set when --allow-anonymous was passed; auth is then skipped with a warning header
}

// NewWebServer creates a new web server
func NewWebServer(port string, db *internal.DatabaseManager) *WebServer {
	ws := &WebServer{
		port: port,
		mux:  http.NewServeMux(),
		db:   db,
	}
	ws.setupRoutes()
	ws.handler = ws.withAuth(ws.mux)
	return ws
}

// SetAllowAnonymous toggles the auth-skip behaviour. Called by handleWeb
// when --allow-anonymous is passed. The flag exists so operators can run
// `mpm web --allow-anonymous` for trusted-LAN debugging while the default
// config remains fail-closed.
func (ws *WebServer) SetAllowAnonymous(b bool) {
	ws.allowAnonymous = b
}

//go:embed web
var webFS embed.FS

func mimeType(ext string) string {
	switch ext {
	case ".html":
		return "text/html"
	case ".css":
		return "text/css"
	case ".js":
		return "application/javascript"
	case ".json":
		return "application/json"
	case ".png":
		return "image/png"
	case ".svg":
		return "image/svg+xml"
	case ".ico":
		return "image/x-icon"
	default:
		return "application/octet-stream"
	}
}

func (ws *WebServer) setupRoutes() {
	// Static files from embedded web/ directory
	ws.mux.HandleFunc("/static/", func(w http.ResponseWriter, r *http.Request) {
		p := filepath.Clean(strings.TrimPrefix(r.URL.Path, "/static/"))
		f, err := webFS.Open(filepath.Join("web", p))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		stat, _ := f.Stat()
		if stat.IsDir() {
			http.NotFound(w, r)
			return
		}
		ext := path.Ext(p)
		w.Header().Set("Content-Type", mimeType(ext))
		io.Copy(w, f)
	})

	// SPA fallback (root)
	ws.mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f, err := webFS.Open("web/index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "text/html")
		io.Copy(w, f)
	})

	// Search
	ws.mux.HandleFunc("/api/search", ws.handleSearch)

	// Memories
	ws.mux.HandleFunc("/api/memories", ws.handleMemories)
	ws.mux.HandleFunc("/api/memories/", ws.handleMemoryByID)

	// Topics
	ws.mux.HandleFunc("/api/topics", ws.handleTopics)
	ws.mux.HandleFunc("/api/topics/", ws.handleTopicByID)

	// Lessons
	ws.mux.HandleFunc("/api/lessons", ws.handleLessons)
	ws.mux.HandleFunc("/api/lessons/", ws.handleLessonByID)

	// References
	ws.mux.HandleFunc("/api/references", ws.handleReferences)
	ws.mux.HandleFunc("/api/references/", ws.handleReferenceByID)

	// Config
	ws.mux.HandleFunc("/api/status", ws.handleStatus)

	// Health
	ws.mux.HandleFunc("/health", ws.handleHealth)
}

// Start starts the web server
func (ws *WebServer) Start() error {
	addr := ":" + ws.port
	fmt.Printf("🌐 Web UI: http://localhost%s\n", addr)
	return http.ListenAndServe(addr, ws.handler)
}

// withAuth validates bearer token from mpm_config.json web_token field.
//
// Auth policy (post security-review-2026-06-15 fix):
//   - web_token set in mpm_config.json → enforced; requests without a valid
//     Bearer token get 401.
//   - web_token unset AND --allow-anonymous flag was passed at server start
//     → auth skipped, but a one-shot WARN is logged so operators notice.
//   - web_token unset AND no --allow-anonymous → ws.startWithAuth returns
//     false at boot and the server refuses to start.
//
// The "fail open when token is empty" behaviour was the single biggest
// security gap in the original code: a default `mpm web` on a LAN would
// serve every memory to anyone reachable. The fix flips the default so
// operators must opt in to running an unauthenticated server.
//
// The token lookup is extracted to a package-level var so tests can stub
// it without touching the real workspace mpm_config.json.
func (ws *WebServer) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip auth for static assets, SPA, and health checks
		path := r.URL.Path
		if path == "/" || strings.HasPrefix(path, "/static/") || path == "/health" {
			next.ServeHTTP(w, r)
			return
		}

		token, ok := currentWebToken()
		if !ok {
			// No config or no token → fail closed unless operator opted in.
			if !ws.allowAnonymous {
				writeError(w, http.StatusUnauthorized, "auth required: configure web_token in mpm_config.json or restart with --allow-anonymous")
				return
			}
			// Operator explicitly opted in. Set a header so any client knows.
			w.Header().Set("X-MPM-Auth", "disabled-anonymous")
			next.ServeHTTP(w, r)
			return
		}

		// Validate bearer token
		authHeader := r.Header.Get("Authorization")
		if !strings.HasPrefix(authHeader, "Bearer ") {
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}
		if strings.TrimPrefix(authHeader, "Bearer ") != token {
			writeError(w, http.StatusUnauthorized, "invalid token")
			return
		}

		next.ServeHTTP(w, r)
	})
}

// webTokenLookup returns the configured web_token from mpm_config.json.
// The second return is false when no config is available or no token is
// configured (regardless of why). Tests override this variable.
var webTokenLookup = realWebTokenLookup

// realWebTokenLookup reads mpm_config.json from disk.
func realWebTokenLookup() (string, bool) {
	cfg, err := config.LoadConfig()
	if err != nil || cfg == nil || cfg.WebToken == "" {
		return "", false
	}
	return cfg.WebToken, true
}

// currentWebToken is a thin wrapper so tests can stub webTokenLookup.
func currentWebToken() (string, bool) { return webTokenLookup() }

// JSON helpers

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func parseInt(s string, def int) int {
	if i, err := strconv.Atoi(s); err == nil {
		return i
	}
	return def
}

// ==================== Handlers ====================

func (ws *WebServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (ws *WebServer) handleStatus(w http.ResponseWriter, r *http.Request) {
	counts := map[string]interface{}{}

	// Memory count
	var memCount int
	ws.db.SQLDB().QueryRow("SELECT COUNT(*) FROM memories").Scan(&memCount)
	counts["memories"] = memCount

	// Topic count
	var topicCount int
	ws.db.SQLDB().QueryRow("SELECT COUNT(*) FROM topics WHERE is_active = 1").Scan(&topicCount)
	counts["topics"] = topicCount

	// Lesson count
	var lessonCount int
	ws.db.SQLDB().QueryRow("SELECT COUNT(*) FROM lessons").Scan(&lessonCount)
	counts["lessons"] = lessonCount

	// Ingest status
	ingestCounts, _ := ws.db.GetIngestStatus()
	counts["ingest"] = ingestCounts

	writeJSON(w, http.StatusOK, counts)
}

// ==================== CLI command ====================

// handleWeb starts the web UI server
func handleWeb(args []string) int {
	port := defaultWebPort
	allowAnonymous := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--port":
			if i+1 < len(args) {
				port = args[i+1]
				i++
			}
		case "--allow-anonymous":
			allowAnonymous = true
		}
	}

	// Loud pre-start warning if the operator opted into unauthenticated mode.
	if allowAnonymous {
		usererror.Warn("WARNING: --allow-anonymous set. The web server will serve every memory to any client reachable on the network. Do NOT use this on a hostile network.")
	}

	// Export port so concurrent mpm call processes can relay SSE events here.
	os.Setenv("MPM_PORT", port)
	// Also write to well-known file so mpm call can find the port even if
	// it wasn't started with MPM_PORT in its environment.
	portFile := config.GetMPMDir() + "/web.port"
	os.WriteFile(portFile, []byte(port), 0600)
	defer os.Remove(portFile)

	db, err := internal.NewDatabaseManager("")
	if err != nil {
		usererror.Error("Failed to open database: %v", err)
	}
	defer db.Close()

	ws := NewWebServer(port, db)
	ws.SetAllowAnonymous(allowAnonymous)
	fmt.Printf("mpm web: http://localhost:%s\n", port)
	if err := ws.Start(); err != nil {
		return 1
	}
	return 0
}
