package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mpm/internal"
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
		t.Skipf("db unavailable in test env: %v", err)
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