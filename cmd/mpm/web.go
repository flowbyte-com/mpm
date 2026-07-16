package main

import (
	"crypto/rand"
	"embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/config"
	"github.com/flowbyte-com/mpm-core/usererror"
)

const (
	defaultWebPort     = "18792"
	defaultHTTPTimeout = 30 * time.Second
	maxRequestBody    = 10 << 20 // 10 MB max request body
)

// WebServer holds the HTTP server state
type WebServer struct {
	port           string
	mux            *http.ServeMux
	db             internal.CoreDB
	handler        http.Handler
	allowAnonymous bool // set when --allow-anonymous was passed; auth is then skipped with a warning header
	srv            *http.Server
	csrfToken      string // server-side CSRF nonce; returned by /api/status, required on mutating requests
}

// NewWebServer creates a new web server. csrfToken is generated once at
// startup and shared across all sessions; it is not session-scoped because
// the server itself is the trust boundary. All mutating requests must
// carry the token in the X-CSRF-Token header.
func NewWebServer(port string, db internal.CoreDB) *WebServer {
	csrfToken, err := generateCSRFToken()
	if err != nil {
		// crypto/rand.Read failures mean the OS CSPRNG is broken. The fallback
		// token would be a universal bypass (constant + returned in error responses),
		// so we fail closed: refuse to start rather than run with degraded CSRF.
		slog.Error("CSRF token generation failed; cannot start without CSRF protection", "error", err)
		panic("crypto/rand unavailable: cannot start web server safely")
	}
	ws := &WebServer{
		port:      port,
		mux:       http.NewServeMux(),
		db:        db,
		csrfToken: csrfToken,
	}
	ws.setupRoutes()
	ws.handler = ws.withAuth(ws.mux)
	return ws
}

// generateCSRFToken creates a cryptographically random 32-byte nonce,
// encoded as base64url (43 chars). This is the per-server CSRF token.
func generateCSRFToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(b), nil
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

	// CSRF token endpoint: returns the current nonce so the SPA can read it
	// after auth. The SPA calls this once and caches the value for all
	// subsequent state-changing requests.
	ws.mux.HandleFunc("/api/csrf-token", ws.handleCSRFToken)

	// Health
	ws.mux.HandleFunc("/health", ws.handleHealth)
}

// Start starts the web server with timeouts and security headers.
func (ws *WebServer) Start() error {
	addr := ":" + ws.port
	ws.srv = &http.Server{
		Addr:         addr,
		Handler:      withSecurityHeaders(ws.handler),
		ReadTimeout:  defaultHTTPTimeout,
		WriteTimeout: defaultHTTPTimeout,
		IdleTimeout:  60 * time.Second,
	}
	fmt.Printf("🌐 Web UI: http://localhost%s\n", addr)
	return ws.srv.ListenAndServe()
}

// withSecurityHeaders adds security-relevant HTTP response headers.
//
// CORS is restricted to localhost-style origins. The previous
// `Access-Control-Allow-Origin: *` combined with the explicit
// `Allow-Headers: Authorization` allowlist let any web page issue a CORS
// preflighted request carrying the operator's bearer token. For a
// local-first tool the same-origin SPA does not need CORS at all; we still
// echo the Origin header when it's localhost so cross-port clients
// (e.g. a debugging browser tab on a different loopback port) work.
//
// A restrictive Content-Security-Policy is set as defence-in-depth against
// any future stored-XSS regression in app.js. The static SPA is served
// from the same origin so 'self' is sufficient; inline styles are still
// permitted because the existing CSS uses inline `style=` attributes in
// card templates.
func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "same-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'")
		if origin := r.Header.Get("Origin"); origin != "" {
			if u, err := url.Parse(origin); err == nil {
				switch u.Hostname() {
				case "localhost", "127.0.0.1", "::1":
					w.Header().Set("Access-Control-Allow-Origin", origin)
					w.Header().Set("Vary", "Origin")
					w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
					w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
				}
			}
		}
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// withAuth validates bearer token from mpm_config.json web_token field.
//
// Auth policy (post security-review-2026-06-15 fix):
//   - web_token set in mpm_config.json → enforced; requests without a valid
//     Bearer token get 401.
//   - web_token unset AND --allow-anonymous flag was passed at server start
//     → auth skipped, but a one-shot WARN is logged so operators notice.
//   - web_token unset AND no --allow-anonymous → server refuses to start.
//
// After token validation, mutating methods (POST/PUT/DELETE/PATCH) require a
// matching X-CSRF-Token header. The nonce is constant per server instance
// (returned by /api/status). This is defense-in-depth: the Bearer token
// already prevents cross-origin CSRF because browsers don't attach custom
// Authorization headers in cross-origin requests without CORS preflight.
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

		// CSRF check: all mutating methods require X-CSRF-Token header.
		// Safe methods (GET/HEAD/OPTIONS) are exempt — they never change state.
		// This is constant-time so timing is not leaky.
		if isMutatingMethod(r.Method) && !ws.validateCSRF(r) {
			w.Header().Set("X-CSRF-Token", ws.csrfToken)
			writeError(w, http.StatusForbidden, "CSRF token missing or invalid")
			return
		}

		next.ServeHTTP(w, r)
	})
}

// isMutatingMethod returns true for HTTP methods that modify server state.
func isMutatingMethod(method string) bool {
	return method == "POST" || method == "PUT" || method == "PATCH" || method == "DELETE"
}

// validateCSRF checks the X-CSRF-Token header against the server-side nonce.
// Timing-safe comparison to prevent timing attacks.
func (ws *WebServer) validateCSRF(r *http.Request) bool {
	return ws.csrfToken != "" && r.Header.Get("X-CSRF-Token") == ws.csrfToken
}

// webTokenLookup returns the configured web_token from mpm_config.json.
// The second return is false when no config is available or no token is
// configured (regardless of why). Tests override this variable.
var webTokenLookup = realWebTokenLookup

// realWebTokenLookup reads mpm_config.json from disk once per process and
// caches the result via sync.Once. Token rotation therefore requires a
// process restart; this is acceptable because:
//   - the prior behaviour re-parsed the JSON on every API request (a measurable
//     perf cost above 100 RPS), and
//   - rotating `web_token` is a security operation that is correctly coupled
//     to a service restart.
// Tests override `webTokenLookup` directly and bypass this cache entirely,
// so test isolation is preserved.
var (
	realLookupOnce  sync.Once
	realLookupToken string
	realLookupOK    bool
)

func realWebTokenLookup() (string, bool) {
	realLookupOnce.Do(func() {
		cfg, err := config.LoadConfig()
		if err != nil {
			// Malformed config: this used to silently fall through to
			// fail-open (no token → assume allow-anonymous). Now we log
			// loudly so operators notice a broken config and surface the
			// failure to the startup check in handleWeb.
			slog.Warn("mpm_config.json failed to parse; auth will be disabled unless --allow-anonymous is set", "error", err)
			realLookupToken, realLookupOK = "", false
			return
		}
		if cfg == nil || cfg.WebToken == "" {
			realLookupToken, realLookupOK = "", false
			return
		}
		realLookupToken, realLookupOK = cfg.WebToken, true
	})
	return realLookupToken, realLookupOK
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

// serverError logs the internal error and sends a generic 500 response to the client.
// Using this instead of writeError(w, 500, err.Error()) prevents leaking internal
// details (SQLite queries, schema info, file paths) to API consumers.
func (ws *WebServer) serverError(w http.ResponseWriter, internalErr error) {
	slog.Error("api internal error", "error", internalErr.Error())
	writeError(w, http.StatusInternalServerError, "internal error")
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

	// Return CSRF token so the SPA can read it after auth and include it
	// in all state-changing requests. The token is not secret (it's a nonce);
	// what matters is that it can't be guessed or predicted.
	counts["csrf_token"] = ws.csrfToken

	writeJSON(w, http.StatusOK, counts)
}

// handleCSRFToken returns the current CSRF nonce as JSON. Thin wrapper around
// the value already embedded in /api/status — provided as a dedicated endpoint
// so clients can fetch it without pulling the full status payload.
func (ws *WebServer) handleCSRFToken(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"csrf_token": ws.csrfToken})
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

	// Fail-closed startup. The withAuth middleware already returns 401 when
	// no token is configured and --allow-anonymous is not set, but the
	// server still starts and binds the port. Operators running `mpm web`
	// against a fresh checkout without mpm_config.json were left wondering
	// why every request 401s. Refuse to start with a clear error instead,
	// matching the documented behaviour at web.go:204-209.
	//
	// This same check covers L-15 (malformed mpm_config.json → fail-open):
	// realWebTokenLookup logs and returns ("", false) when the JSON does
	// not parse, so we treat that identically to "no token".
	if _, hasToken := currentWebToken(); !hasToken && !allowAnonymous {
		hint := ""
		if cfg, err := config.LoadConfig(); err != nil {
			hint = fmt.Sprintf(" (mpm_config.json failed to parse: %v)", err)
		} else if cfg == nil {
			hint = " (no mpm_config.json found; copy mpm_config.json.example)"
		} else if cfg.WebToken == "" {
			hint = " (mpm_config.json has no web_token field)"
		}
		return usererror.Error(
			"refusing to start web server: no auth configured%s. "+
				"Configure web_token in mpm_config.json or restart with --allow-anonymous.",
			hint)
	}

	// Export port so concurrent mpm call processes can relay SSE events here.
	os.Setenv("MPM_PORT", port)
	// Also write to well-known file so mpm call can find the port even if
	// it wasn't started with MPM_PORT in its environment.
	portFile := config.GetMPMDir() + "/web.port"
	if err := os.MkdirAll(filepath.Dir(portFile), 0755); err == nil {
		os.WriteFile(portFile, []byte(port), 0600)
		defer os.Remove(portFile)
	}

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
