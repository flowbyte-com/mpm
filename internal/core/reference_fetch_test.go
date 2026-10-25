// reference_fetch_test.go — URL ingestion, including the security
// boundary.
//
// The URL may come from an autonomous agent, so these tests exercise the
// fetch path the way an attacker would: IP literals, hostnames that
// resolve to loopback, redirects into metadata endpoints, oversized and
// mislabelled responses, credential-bearing URLs and unsupported
// schemes.
//
// The loopback test seam is `referenceFetchConfig.allowLoopback`, which
// is unexported and only reachable from this package. Production entry
// points construct the default config, in which loopback is refused.
package internal

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// loopbackFetchConfig is the test-only configuration that may reach the
// httptest fixture.
func loopbackFetchConfig() referenceFetchConfig {
	cfg := defaultReferenceFetchConfig()
	cfg.allowLoopback = true
	return cfg
}

// mustIdentity resolves a URL source for a test.
func mustIdentity(t *testing.T, raw string) SourceIdentity {
	t.Helper()
	id, err := ResolveReferenceIdentity(raw)
	if err != nil {
		t.Fatalf("resolve %q: %v", raw, err)
	}
	return id
}

const turnstileHTML = `<!doctype html>
<html><head>
<title>Server-side validation · Cloudflare Turnstile docs</title>
<meta property="og:site_name" content="Cloudflare Docs">
<meta name="author" content="Cloudflare">
<link rel="license" href="https://creativecommons.org/licenses/by/4.0/">
</head><body>
<nav class="site-nav"><a href="/">Docs home</a></nav>
<div class="cookie-banner">This site uses cookies.</div>
<main>
  <h1>Server-side validation</h1>
  <p>Validate the token returned by the Turnstile widget on your server.</p>
  <h2>Request</h2>
  <pre class="language-bash"><code>curl -X POST https://challenges.cloudflare.com/turnstile/v0/siteverify \
  -d "secret=SECRET&amp;response=TOKEN"</code></pre>
  <div class="note callout">Tokens are single-use and expire after 300 seconds.</div>
</main>
<footer>© Cloudflare</footer>
</body></html>`

// TestFetchReferenceURL_ExtractsDocumentAndProvenance covers the happy
// path: HTML is extracted structurally, provenance is captured, and the
// document title comes from the page rather than the URL.
func TestFetchReferenceURL_ExtractsDocumentAndProvenance(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Accept"); !strings.Contains(got, "text/markdown") {
			t.Errorf("Accept header did not offer markdown: %q", got)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("ETag", `"abc123"`)
		w.Header().Set("Last-Modified", "Wed, 01 Oct 2026 10:00:00 GMT")
		fmt.Fprint(w, turnstileHTML)
	}))
	defer srv.Close()

	acq, err := fetchReferenceURLWithConfig(context.Background(), mustIdentity(t, srv.URL+"/turnstile/"), nil, loopbackFetchConfig())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if acq.Title != "Server-side validation" {
		t.Errorf("title = %q, want the page <h1>", acq.Title)
	}
	for _, want := range []string{"# Server-side validation", "```bash", "siteverify", "**Note:**"} {
		if !strings.Contains(acq.Content, want) {
			t.Errorf("content missing %q\n%s", want, acq.Content)
		}
	}
	for _, unwanted := range []string{"This site uses cookies", "Docs home", "© Cloudflare"} {
		if strings.Contains(acq.Content, unwanted) {
			t.Errorf("content kept chrome %q\n%s", unwanted, acq.Content)
		}
	}
	if acq.SourceType != "html" {
		t.Errorf("source type = %q, want html", acq.SourceType)
	}
	if acq.ETag != `"abc123"` || acq.LastModified == "" {
		t.Errorf("HTTP validators not captured: etag=%q last-modified=%q", acq.ETag, acq.LastModified)
	}
	if acq.EffectiveURL != srv.URL+"/turnstile/" {
		t.Errorf("effective URL = %q", acq.EffectiveURL)
	}
	if acq.SiteName != "Cloudflare Docs" || acq.Byline != "Cloudflare" {
		t.Errorf("attribution = (%q, %q)", acq.SiteName, acq.Byline)
	}
	if acq.FetchedAt.IsZero() {
		t.Error("fetched_at not recorded")
	}
}

// TestFetchReferenceURL_PrefersPublisherMarkdown pins that a Markdown
// rendition is ingested as Markdown, not re-parsed as HTML.
func TestFetchReferenceURL_PrefersPublisherMarkdown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		fmt.Fprint(w, "---\ntitle: Turnstile\nsidebar: docs\n---\n\n# Turnstile\n\nVerify tokens server-side.\n")
	}))
	defer srv.Close()

	acq, err := fetchReferenceURLWithConfig(context.Background(), mustIdentity(t, srv.URL), nil, loopbackFetchConfig())
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	if acq.SourceType != "markdown" {
		t.Fatalf("source type = %q, want markdown", acq.SourceType)
	}
	if strings.Contains(acq.Content, "sidebar: docs") {
		t.Errorf("front matter kept in content: %q", acq.Content)
	}
	if !strings.Contains(acq.Content, "# Turnstile") {
		t.Errorf("markdown body lost: %q", acq.Content)
	}
}

// TestFetchReferenceURL_ConditionalRequest pins the freshness path: a
// stored validator turns a re-import into a 304 with no body transfer.
func TestFetchReferenceURL_ConditionalRequest(t *testing.T) {
	var seenIfNoneMatch string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenIfNoneMatch = r.Header.Get("If-None-Match")
		if seenIfNoneMatch == `"v1"` {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		w.Header().Set("ETag", `"v1"`)
		fmt.Fprint(w, "<html><body><main><h1>Doc</h1><p>Body.</p></main></body></html>")
	}))
	defer srv.Close()

	acq, err := fetchReferenceURLWithConfig(context.Background(), mustIdentity(t, srv.URL), &referenceConditional{ETag: `"v1"`}, loopbackFetchConfig())
	if err != nil {
		t.Fatalf("conditional fetch: %v", err)
	}
	if seenIfNoneMatch != `"v1"` {
		t.Errorf("If-None-Match = %q, want the stored validator", seenIfNoneMatch)
	}
	if !acq.NotModified {
		t.Error("304 response was not reported as NotModified")
	}
	if acq.Content != "" {
		t.Errorf("304 response carried content: %q", acq.Content)
	}
}

// TestFetchReferenceURL_RefusesLoopbackWithoutTheTestSeam is the core
// SSRF regression. The production config must not reach a loopback
// server, even when the host is a name rather than an IP literal — that
// is what proves the check happens at connect time on the resolved
// address, not as a string test before the request.
func TestFetchReferenceURL_RefusesLoopbackWithoutTheTestSeam(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html><body><main><p>should never be fetched</p></main></body></html>")
	}))
	defer srv.Close()

	port := srv.Listener.Addr().(*net.TCPAddr).Port
	for _, host := range []string{"127.0.0.1", "localhost", "[::1]"} {
		if host == "[::1]" {
			// Only meaningful when the fixture also listens on IPv6.
			if !strings.HasPrefix(srv.URL, "http://127.0.0.1") {
				continue
			}
		}
		raw := fmt.Sprintf("http://%s:%d/doc", host, port)
		_, err := fetchReferenceURLWithConfig(context.Background(), mustIdentity(t, raw), nil, defaultReferenceFetchConfig())
		if err == nil {
			t.Fatalf("production config fetched %s; loopback must be refused", raw)
		}
		if !strings.Contains(err.Error(), "loopback") && !strings.Contains(err.Error(), "refusing to fetch") {
			t.Errorf("refusal for %s did not name the reason: %v", raw, err)
		}
	}
}

// TestFetchReferenceURL_RefusesRedirectIntoForbiddenDestination pins that
// a redirect cannot be used to reach a destination the first hop could
// not.
//
// The fixture redirects to the link-local metadata address, which stays
// forbidden even with the loopback test seam enabled. The refusal must
// happen at the dial boundary for the second hop, so nothing is sent.
func TestFetchReferenceURL_RefusesRedirectIntoForbiddenDestination(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer srv.Close()

	_, err := fetchReferenceURLWithConfig(context.Background(), mustIdentity(t, srv.URL), nil, loopbackFetchConfig())
	if err == nil {
		t.Fatal("redirect to a link-local metadata destination was followed")
	}
	if !strings.Contains(err.Error(), "link-local") && !strings.Contains(err.Error(), "refusing to fetch") {
		t.Errorf("refusal did not name the destination class: %v", err)
	}
}

// TestFetchReferenceURL_RefusesUnsupportedSchemesAndCredentials pins the
// string-level rules.
func TestFetchReferenceURL_RefusesUnsupportedSchemesAndCredentials(t *testing.T) {
	cases := map[string]string{
		"file:///etc/passwd":                "scheme",
		"data:text/html,<h1>x</h1>":         "scheme",
		"javascript:alert(1)":               "scheme",
		"ftp://example.org/doc":             "scheme",
		"https://user:secret@example.org/x": "credentials",
	}
	for raw, want := range cases {
		_, err := ResolveReferenceIdentity(raw)
		if err == nil {
			t.Errorf("%s was accepted as a source", raw)
			continue
		}
		if !strings.Contains(strings.ToLower(err.Error()), want) {
			t.Errorf("refusal for %s did not mention %q: %v", raw, want, err)
		}
	}

	// A URL that parses as a source still must not reach the fetcher with
	// a scheme the client cannot speak.
	_, err := fetchReferenceURL(context.Background(), SourceIdentity{
		Kind: ReferenceSourceURL, Canonical: "gopher://example.org/x", Raw: "gopher://example.org/x",
	}, nil)
	if err == nil || !strings.Contains(err.Error(), "scheme") {
		t.Errorf("gopher URL was not refused by the fetcher: %v", err)
	}
}

// TestFetchReferenceURL_RefusesOversizedResponse pins the size ceiling.
func TestFetchReferenceURL_RefusesOversizedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, "<html><body><main><p>")
		chunk := strings.Repeat("x", 4096)
		for i := 0; i < 64; i++ {
			fmt.Fprint(w, chunk)
		}
		fmt.Fprint(w, "</p></main></body></html>")
	}))
	defer srv.Close()

	cfg := loopbackFetchConfig()
	cfg.maxBytes = 32 << 10
	_, err := fetchReferenceURLWithConfig(context.Background(), mustIdentity(t, srv.URL), nil, cfg)
	if err == nil {
		t.Fatal("oversized response was accepted")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("oversize error did not say so: %v", err)
	}
}

// TestFetchReferenceURL_RefusesUnsupportedContentType pins that a binary
// payload is not stored as documentation.
func TestFetchReferenceURL_RefusesUnsupportedContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/pdf")
		fmt.Fprint(w, "%PDF-1.7 …")
	}))
	defer srv.Close()

	_, err := fetchReferenceURLWithConfig(context.Background(), mustIdentity(t, srv.URL), nil, loopbackFetchConfig())
	if err == nil {
		t.Fatal("a PDF response was accepted as documentation")
	}
	if !strings.Contains(err.Error(), "unsupported Content-Type") {
		t.Errorf("error did not name the content type: %v", err)
	}
}

// TestFetchReferenceURL_ReportsHTTPFailure pins that a 404 is a failure
// with the status and a bounded body excerpt, not an empty success.
func TestFetchReferenceURL_ReportsHTTPFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, "<html><body>Page not found</body></html>")
	}))
	defer srv.Close()

	_, err := fetchReferenceURLWithConfig(context.Background(), mustIdentity(t, srv.URL+"/missing"), nil, loopbackFetchConfig())
	if err == nil {
		t.Fatal("404 was treated as success")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error did not report the status: %v", err)
	}
}

// TestFetchReferenceURL_RefusesRedirectLoop pins the hop ceiling.
func TestFetchReferenceURL_RefusesRedirectLoop(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/again", http.StatusFound)
	}))
	defer srv.Close()

	_, err := fetchReferenceURLWithConfig(context.Background(), mustIdentity(t, srv.URL), nil, loopbackFetchConfig())
	if err == nil {
		t.Fatal("redirect loop was followed forever")
	}
	if !strings.Contains(err.Error(), "redirect") {
		t.Errorf("error did not mention redirects: %v", err)
	}
}

// TestFetchReferenceURL_HonoursTimeout pins the time bound.
func TestFetchReferenceURL_HonoursTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
		fmt.Fprint(w, "<html><body><main>late</main></body></html>")
	}))
	defer srv.Close()

	cfg := loopbackFetchConfig()
	cfg.timeout = 50 * time.Millisecond
	start := time.Now()
	_, err := fetchReferenceURLWithConfig(context.Background(), mustIdentity(t, srv.URL), nil, cfg)
	if err == nil {
		t.Fatal("slow response was accepted past the timeout")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("timeout took %s; the bound was not applied", elapsed)
	}
}

// TestClassifyFetchAddr pins the destination classifier directly, so
// every forbidden range is covered without needing a live listener.
func TestClassifyFetchAddr(t *testing.T) {
	forbidden := []string{
		"127.0.0.1", "127.255.255.254", "::1",
		"10.0.0.1", "172.16.5.4", "192.168.1.1",
		"169.254.169.254", // cloud metadata
		"100.64.0.1",      // CGNAT
		"0.0.0.0", "::",
		"224.0.0.1", "ff02::1",
		"198.18.0.1", "192.0.2.5", "198.51.100.7", "203.0.113.9",
		"240.0.0.1", "2001:db8::1", "fd00::1",
		"::ffff:127.0.0.1", // IPv4-mapped loopback
	}
	for _, raw := range forbidden {
		addr, err := netip.ParseAddr(raw)
		if err != nil {
			t.Fatalf("parse %s: %v", raw, err)
		}
		if reason := classifyFetchAddr(addr.Unmap(), false); reason == "" {
			t.Errorf("%s was classified as allowed", raw)
		}
	}

	allowed := []string{"93.184.216.34", "2606:2800:220:1:248:1893:25c8:1946"}
	for _, raw := range allowed {
		addr, err := netip.ParseAddr(raw)
		if err != nil {
			t.Fatalf("parse %s: %v", raw, err)
		}
		if reason := classifyFetchAddr(addr.Unmap(), false); reason != "" {
			t.Errorf("public address %s was refused: %s", raw, reason)
		}
	}

	loopback, _ := netip.ParseAddr("127.0.0.1")
	if reason := classifyFetchAddr(loopback, true); reason != "" {
		t.Errorf("loopback refused even with the test seam on: %s", reason)
	}
}

// TestReferenceIngest_URLIsIdempotent end-to-end: two fetches of the
// same URL are one document, and the second reports unchanged.
func TestReferenceIngest_URLIsIdempotent(t *testing.T) {
	dm := NewTestDM(t)
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, turnstileHTML)
	}))
	defer srv.Close()
	url := srv.URL + "/turnstile/get-started/server-side-validation/"

	first, err := dm.ingestReferenceForTest(t, ReferenceIngestRequest{Source: url}, loopbackFetchConfig())
	if err != nil {
		t.Fatalf("first URL ingest: %v", err)
	}
	if first.Status != ReferenceStatusImported {
		t.Fatalf("status = %q, want imported", first.Status)
	}
	if first.Source != url {
		t.Errorf("canonical source = %q, want %q", first.Source, url)
	}
	if first.SourceKind != ReferenceSourceURL {
		t.Errorf("source kind = %q, want url", first.SourceKind)
	}

	second, err := dm.ingestReferenceForTest(t, ReferenceIngestRequest{Source: url}, loopbackFetchConfig())
	if err != nil {
		t.Fatalf("second URL ingest: %v", err)
	}
	if second.Status != ReferenceStatusUnchanged || second.ID != first.ID {
		t.Fatalf("re-import = (%q, %s), want (unchanged, %s)", second.Status, second.ID, first.ID)
	}
	if n := referenceDocCount(t, dm); n != 1 {
		t.Errorf("stored %d documents for one URL; want 1", n)
	}
	if hits != 2 {
		t.Errorf("server saw %d requests, want 2 (one per ingest)", hits)
	}
}

// ingestReferenceForTest runs the ingest pipeline with a custom fetch
// config so the loopback seam can be enabled for one call.
func (dm *DatabaseManager) ingestReferenceForTest(t *testing.T, req ReferenceIngestRequest, cfg referenceFetchConfig) (*ReferenceIngestResult, error) {
	t.Helper()
	source, err := req.resolveRequestSource()
	if err != nil {
		return nil, err
	}
	identity, err := ResolveReferenceIdentity(source)
	if err != nil {
		return nil, err
	}
	preMatch, err := resolveReferenceIdentity(dm.db, identity)
	if err != nil {
		return nil, err
	}
	var cond *referenceConditional
	if preMatch != nil && preMatch.Doc != nil && (preMatch.Doc.HTTPETag != "" || preMatch.Doc.HTTPLastModified != "") {
		cond = &referenceConditional{ETag: preMatch.Doc.HTTPETag, LastModified: preMatch.Doc.HTTPLastModified}
	}
	acq, err := fetchReferenceURLWithConfig(context.Background(), identity, cond, cfg)
	if err != nil {
		return nil, err
	}
	return dm.ingestAcquiredReference(context.Background(), identity, acq, req, source)
}
