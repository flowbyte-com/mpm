package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm-core"
)

// stubToken saves and restores the webTokenLookup package var.
func stubToken(t *testing.T, token string, present bool) {
	t.Helper()
	orig := webTokenLookup
	webTokenLookup = func() (string, bool) {
		if !present {
			return "", false
		}
		return token, true
	}
	t.Cleanup(func() { webTokenLookup = orig })
}

func newTestWS(t *testing.T, allowAnon bool) *WebServer {
	t.Helper()
	db, err := internal.NewDatabaseManager("")
	if err != nil {
		// Security tests must not silently skip: a missing FTS5 build or
		// broken sqlite driver would let auth-bypass regressions land
		// unnoticed. The tests below guard against exactly that class of
		// bug; failing to set up the DB is a hard CI error.
		t.Fatalf("db unavailable in test env: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	ws := NewWebServer("0", db)
	ws.SetAllowAnonymous(allowAnon)
	return ws
}

func TestWithAuth_RejectsMissingTokenWhenTokenConfigured(t *testing.T) {
	stubToken(t, "secret-token", true)
	ws := newTestWS(t, false)

	handler := ws.withAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("next handler should not have been invoked")
	}))

	req := httptest.NewRequest("GET", "/api/memories", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", rr.Code)
	}
}

func TestWithAuth_AcceptsValidToken(t *testing.T) {
	stubToken(t, "secret-token", true)
	ws := newTestWS(t, false)

	called := false
	handler := ws.withAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/api/memories", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}
	if !called {
		t.Fatalf("next handler was not invoked")
	}
}

func TestWithAuth_FailsClosedWhenTokenEmpty(t *testing.T) {
	// Token absent (no config).
	stubToken(t, "", false)
	ws := newTestWS(t, false)

	called := false
	handler := ws.withAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/api/memories", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 (fail-closed default), got %d", rr.Code)
	}
	if called {
		t.Fatalf("next handler should not have been invoked when token empty")
	}
}

func TestWithAuth_AllowAnonymousAllowsWhenEmpty(t *testing.T) {
	stubToken(t, "", false)
	ws := newTestWS(t, true)

	called := false
	handler := ws.withAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest("GET", "/api/memories", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200 with --allow-anonymous, got %d", rr.Code)
	}
	if !called {
		t.Fatalf("next handler should have been invoked")
	}
	if h := rr.Header().Get("X-MPM-Auth"); !strings.Contains(h, "anonymous") {
		t.Errorf("expected X-MPM-Auth warning header, got %q", h)
	}
}

func TestWithAuth_StaticAssetsBypass(t *testing.T) {
	stubToken(t, "secret-token", true)
	ws := newTestWS(t, false)

	called := false
	handler := ws.withAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	for _, path := range []string{"/", "/static/app.js", "/health"} {
		req := httptest.NewRequest("GET", path, nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if !called {
			t.Errorf("static path %q should bypass auth", path)
		}
	}
}

// ==================== CSRF ====================

func newTestWSForCSRF(t *testing.T, token string) *WebServer {
	t.Helper()
	stubToken(t, token, true)
	db, err := internal.NewDatabaseManager("")
	if err != nil {
		// CSRF tests guard against cross-origin request forgery; a
		// silently-skipped CSRF test is worse than a hard failure.
		t.Fatalf("db unavailable in test env: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	ws := NewWebServer("0", db)
	ws.SetAllowAnonymous(false)
	return ws
}

func TestCSRF_RejectsMutatingWithoutToken(t *testing.T) {
	ws := newTestWSForCSRF(t, "secret-token")

	handler := ws.withAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not have been invoked")
	}))

	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		req := httptest.NewRequest(method, "/api/memories", nil)
		req.Header.Set("Authorization", "Bearer secret-token")
		// No X-CSRF-Token header
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusForbidden {
			t.Errorf("[%s] expected 403, got %d", method, rr.Code)
		}
	}
}

func TestCSRF_AcceptsMutatingWithValidToken(t *testing.T) {
	ws := newTestWSForCSRF(t, "secret-token")

	called := false
	handler := ws.withAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		called = false
		req := httptest.NewRequest(method, "/api/memories", nil)
		req.Header.Set("Authorization", "Bearer secret-token")
		req.Header.Set("X-CSRF-Token", ws.csrfToken)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusOK {
			t.Errorf("[%s] expected 200, got %d", method, rr.Code)
		}
		if !called {
			t.Errorf("[%s] next handler was not invoked", method)
		}
	}
}

func TestCSRF_RejectsMutatingWithInvalidToken(t *testing.T) {
	ws := newTestWSForCSRF(t, "secret-token")

	handler := ws.withAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("next handler should not have been invoked")
	}))

	for _, method := range []string{"POST", "PUT", "PATCH", "DELETE"} {
		req := httptest.NewRequest(method, "/api/memories", nil)
		req.Header.Set("Authorization", "Bearer secret-token")
		req.Header.Set("X-CSRF-Token", "wrong-token")
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusForbidden {
			t.Errorf("[%s] expected 403, got %d", method, rr.Code)
		}
	}
}

func TestCSRF_GetExempt(t *testing.T) {
	ws := newTestWSForCSRF(t, "secret-token")

	called := false
	handler := ws.withAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))

	// GET with no CSRF token should succeed
	req := httptest.NewRequest("GET", "/api/memories", nil)
	req.Header.Set("Authorization", "Bearer secret-token")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("GET without CSRF token: expected 200, got %d", rr.Code)
	}
	if !called {
		t.Error("GET without CSRF token: next handler was not invoked")
	}
}

func TestCSRF_ValidateCSRF(t *testing.T) {
	ws := newTestWSForCSRF(t, "secret-token")

	// Empty token in request → invalid
	req, _ := http.NewRequest("POST", "/api/memories", nil)
	if ws.validateCSRF(req) {
		t.Error("empty header should not validate")
	}

	// Wrong token → invalid
	req, _ = http.NewRequest("POST", "/api/memories", nil)
	req.Header.Set("X-CSRF-Token", "wrong")
	if ws.validateCSRF(req) {
		t.Error("wrong token should not validate")
	}

	// Correct token → valid
	req, _ = http.NewRequest("POST", "/api/memories", nil)
	req.Header.Set("X-CSRF-Token", ws.csrfToken)
	if !ws.validateCSRF(req) {
		t.Error("correct token should validate")
	}
}
