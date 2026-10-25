// reference_fetch.go — HTTP retrieval for reference URLs.
//
// The URL may be supplied by an autonomous agent, so the fetch path is a
// security boundary first and a convenience second. Every control below
// is enforced at the point where it can actually be enforced:
//
//	control	                  where it is enforced
//	------------------------  --------------------------------------------
//	scheme allowlist          URL parse + per-redirect check
//	forbidden destinations    net.Dialer.Control, i.e. AFTER DNS
//	                          resolution and immediately before connect
//	redirect into forbidden   same dialer control on the redirected hop,
//	                          plus a per-hop scheme/credential check
//	credentials in the URL    rejected at parse time (never stored)
//	credentials to a peer     never sent; a downgrade or cross-origin
//	                          redirect drops them
//	TLS verification          never disabled (no custom TLS config)
//	bounded download          io.LimitReader on the DECOMPRESSED body
//	bounded time              client timeout + context deadline
//	bounded redirects         CheckRedirect
//	content type              parsed media type allowlist
//
// Doing the destination check in Dialer.Control rather than as a string
// test on the URL is the difference between a real SSRF defence and a
// decorative one: the address passed to Control is the address that is
// about to be dialled, so `localhost`, a DNS name pointing at
// 169.254.169.254, an IPv4-mapped IPv6 literal and a rebinding answer
// are all judged on their actual resolved address, not on their spelling.
//
// The one seam for tests is `allowLoopback`, which is an unexported
// field on the unexported fetch config. No exported entry point sets it,
// so a production caller cannot enable it by accident — and a test that
// forgets to turn it off simply cannot reach a loopback fixture.
package internal

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"

	"golang.org/x/net/html/charset"
)

// Fetch limits. These are ceilings, not targets: documentation pages are
// far smaller, and a response that hits one is reported rather than
// silently truncated.
const (
	defaultFetchMaxBytes   = 16 << 20 // 16 MiB of decompressed body
	defaultFetchTimeout    = 20 * time.Second
	defaultFetchMaxHops    = 5
	referenceUserAgent     = "mpm-reference/alpha (+https://github.com/flowbyte-com/mpm)"
	referenceAcceptHeader  = "text/markdown, text/x-markdown;q=0.9, text/html;q=0.8, application/xhtml+xml;q=0.8, text/plain;q=0.7, */*;q=0.1"
	maxReferenceURLLength  = 4096
	referenceMaxBodyToScan = 64 << 10 // cap for reading a non-2xx error body
)

// referenceFetchConfig is the fetcher's configuration. It is unexported
// on purpose: `allowLoopback` is a test seam, and an exported config
// would let a production caller disable the SSRF defence.
type referenceFetchConfig struct {
	// allowLoopback permits connections to loopback addresses. Tests set
	// it for httptest servers; nothing else may.
	allowLoopback bool
	maxBytes      int64
	timeout       time.Duration
	maxHops       int
	// client overrides the transport (used by tests that need to observe
	// connections). nil means "build the secured transport".
	client *http.Client
}

func defaultReferenceFetchConfig() referenceFetchConfig {
	return referenceFetchConfig{
		maxBytes: defaultFetchMaxBytes,
		timeout:  defaultFetchTimeout,
		maxHops:  defaultFetchMaxHops,
	}
}

// forbiddenDestinationError names the reason a destination was refused so
// an agent can tell "blocked by policy" from "the server was down".
type forbiddenDestinationError struct {
	addr   string
	reason string
}

func (e *forbiddenDestinationError) Error() string {
	return fmt.Sprintf("refusing to fetch %s: %s", e.addr, e.reason)
}

// referenceConditional carries stored validators so a re-import can ask
// the publisher "has this changed?" instead of downloading it again.
type referenceConditional struct {
	ETag         string
	LastModified string
}

// fetchReferenceURL performs the retrieval and extraction.
//
// A conditional request that the server answers with 304 returns a
// result with NotModified=true and no content; the caller then keeps the
// stored document untouched.
func fetchReferenceURL(ctx context.Context, identity SourceIdentity, cond *referenceConditional) (*acquiredReferenceContent, error) {
	cfg := defaultReferenceFetchConfig()
	return fetchReferenceURLWithConfig(ctx, identity, cond, cfg)
}

func fetchReferenceURLWithConfig(ctx context.Context, identity SourceIdentity, cond *referenceConditional, cfg referenceFetchConfig) (*acquiredReferenceContent, error) {
	if cfg.maxBytes <= 0 {
		cfg.maxBytes = defaultFetchMaxBytes
	}
	if cfg.timeout <= 0 {
		cfg.timeout = defaultFetchTimeout
	}
	if cfg.maxHops <= 0 {
		cfg.maxHops = defaultFetchMaxHops
	}
	if len(identity.Canonical) > maxReferenceURLLength {
		return nil, fmt.Errorf("URL is too long (%d characters, max %d)", len(identity.Canonical), maxReferenceURLLength)
	}

	target, err := url.Parse(identity.Canonical)
	if err != nil {
		return nil, fmt.Errorf("parse URL: %w", err)
	}
	if err := validateFetchURL(target); err != nil {
		return nil, err
	}

	client := cfg.client
	if client == nil {
		client = newReferenceHTTPClient(cfg)
	}

	reqCtx, cancel := context.WithTimeout(ctx, cfg.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target.String(), nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("User-Agent", referenceUserAgent)
	// Ask for a Markdown rendition when the publisher offers one. This is
	// content negotiation, not a site-specific hack: WordPress serves
	// `?output_format=md` only because it honours this header, and a
	// publisher that does not simply returns HTML.
	req.Header.Set("Accept", referenceAcceptHeader)
	req.Header.Set("Accept-Language", "en")
	if cond != nil {
		if cond.ETag != "" {
			req.Header.Set("If-None-Match", cond.ETag)
		}
		if cond.LastModified != "" {
			req.Header.Set("If-Modified-Since", cond.LastModified)
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		var forbidden *forbiddenDestinationError
		if errors.As(err, &forbidden) {
			return nil, forbidden
		}
		if reqCtx.Err() != nil {
			return nil, fmt.Errorf("fetch %s: timed out after %s", target.Redacted(), cfg.timeout)
		}
		return nil, fmt.Errorf("fetch %s: %w", target.Redacted(), err)
	}
	defer resp.Body.Close()

	effective := resp.Request.URL
	if err := validateFetchURL(effective); err != nil {
		return nil, err
	}

	if resp.StatusCode == http.StatusNotModified {
		return &acquiredReferenceContent{NotModified: true}, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, referenceMaxBodyToScan))
		return nil, fmt.Errorf("fetch %s: HTTP %d %s%s",
			effective.Redacted(), resp.StatusCode, http.StatusText(resp.StatusCode), firstLineSnippet(snippet))
	}

	contentType := resp.Header.Get("Content-Type")
	mediaType, params, err := parseMediaType(contentType)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: unreadable Content-Type %q: %w", effective.Redacted(), contentType, err)
	}
	sourceType, err := referenceSourceTypeForMediaType(mediaType)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: %w", effective.Redacted(), err)
	}

	// Reject an encoding we cannot decode rather than storing the
	// compressed bytes as if they were text. Go's transport already
	// decompressed gzip/deflate when it negotiated them itself.
	switch enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))); enc {
	case "", "identity", "gzip", "deflate":
	default:
		return nil, fmt.Errorf("fetch %s: unsupported Content-Encoding %q", effective.Redacted(), enc)
	}

	// charset.NewReader transcodes a declared legacy charset to UTF-8 and
	// falls back to the HTML spec's default for HTML. It never silently
	// passes invalid bytes through.
	body, err := charset.NewReader(resp.Body, contentType)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: decode body (%s): %w", effective.Redacted(), contentType, err)
	}

	limited := io.LimitReader(body, cfg.maxBytes+1)
	raw, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("fetch %s: read body: %w", effective.Redacted(), err)
	}
	if int64(len(raw)) > cfg.maxBytes {
		return nil, fmt.Errorf("fetch %s: response exceeds the %d-byte limit; ingest a downloaded copy with `mpm reference add <file>` instead",
			effective.Redacted(), cfg.maxBytes)
	}
	if len(raw) == 0 {
		return nil, fmt.Errorf("fetch %s: empty response body", effective.Redacted())
	}

	out := &acquiredReferenceContent{
		SourceType:   sourceType,
		EffectiveURL: effective.String(),
		RequestedURL: identity.Canonical,
		FetchedAt:    time.Now(),
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
	}
	charsetName := strings.ToLower(strings.TrimSpace(params["charset"]))
	if charsetName != "" && charsetName != "utf-8" && charsetName != "utf8" && charsetName != "us-ascii" {
		out.Notes = append(out.Notes, fmt.Sprintf("decoded from declared charset %q", charsetName))
	}

	text := strings.ToValidUTF8(string(raw), "\uFFFD")
	switch sourceType {
	case "html":
		doc, err := ExtractHTMLDocument(strings.NewReader(text), effective)
		if err != nil {
			return nil, fmt.Errorf("extract %s: %w", effective.Redacted(), err)
		}
		out.Content = doc.Body
		out.Title = doc.Title
		out.SiteName = doc.SiteName
		out.Byline = doc.Byline
		out.License = doc.License
		out.Notes = append(out.Notes, doc.Notes...)
	case "markdown":
		body, notes := prepareMarkdown(text)
		out.Content = body
		out.Notes = append(out.Notes, notes...)
	default:
		out.Content = text
	}

	scheme := effective.Scheme
	out.Notes = append(out.Notes, fmt.Sprintf("fetched %d bytes over %s", len(raw), strings.ToUpper(scheme)))
	if effective.String() != identity.Canonical {
		out.Notes = append(out.Notes, "redirected to "+effective.String())
		out.Warnings = append(out.Warnings,
			"the requested URL redirects; the document identity is the effective URL, and the requested URL is recorded in requested_url")
	}
	if strings.TrimSpace(out.Content) == "" {
		return nil, fmt.Errorf("fetch %s: no extractable text", effective.Redacted())
	}
	return out, nil
}

// newReferenceHTTPClient builds the secured client.
func newReferenceHTTPClient(cfg referenceFetchConfig) *http.Client {
	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
		// Control runs after resolution and before connect. This is the
		// only place where the decision is made against an address that
		// is definitely the one being dialled.
		Control: func(network, address string, _ syscall.RawConn) error {
			return checkDialAddress(network, address, cfg.allowLoopback)
		},
	}
	transport := &http.Transport{
		// No Proxy: an environment-configured proxy would resolve and
		// connect on our behalf, moving the destination check to a host
		// we do not control.
		Proxy:                 nil,
		DialContext:           dialer.DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          4,
		IdleConnTimeout:       30 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// TLS configuration is deliberately left at its secure default:
		// no InsecureSkipVerify, no custom RootCAs.
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}
	return &http.Client{
		Transport: transport,
		Timeout:   cfg.timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) > cfg.maxHops {
				return fmt.Errorf("stopped after %d redirects", cfg.maxHops)
			}
			if err := validateFetchURL(req.URL); err != nil {
				return err
			}
			// A downgrade from https to http loses transport integrity
			// for the remainder of the fetch.
			if prev := via[len(via)-1]; prev.URL.Scheme == "https" && req.URL.Scheme == "http" {
				return fmt.Errorf("refusing to follow a redirect from https to http (%s)", req.URL.Redacted())
			}
			return nil
		},
	}
}

// validateFetchURL applies the string-level rules that do not need a
// resolved address. Destination checks live in checkDialAddress.
func validateFetchURL(u *url.URL) error {
	if u == nil {
		return fmt.Errorf("no URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("unsupported URL scheme %q: only http and https are fetched", u.Scheme)
	}
	if u.User != nil {
		return fmt.Errorf("URL must not embed credentials (%s)", u.Redacted())
	}
	if strings.TrimSpace(u.Hostname()) == "" {
		return fmt.Errorf("URL has no host: %s", u.Redacted())
	}
	if u.Hostname() == "0.0.0.0" || u.Hostname() == "::" {
		return fmt.Errorf("refusing to fetch the unspecified address %s", u.Hostname())
	}
	return nil
}

// checkDialAddress rejects a resolved destination that is not public.
//
// Called from net.Dialer.Control, so `address` is "ip:port" for the
// address about to be connected to. A non-TCP network is refused: the
// fetcher only ever speaks TCP, and a different network family would
// bypass the IP classification below.
func checkDialAddress(network, address string, allowLoopback bool) error {
	switch network {
	case "tcp", "tcp4", "tcp6":
	default:
		return &forbiddenDestinationError{addr: address, reason: "unsupported network " + network}
	}
	addrPort, err := netip.ParseAddrPort(address)
	if err != nil {
		return &forbiddenDestinationError{addr: address, reason: "unparseable destination address"}
	}
	addr := addrPort.Addr().Unmap()
	if reason := classifyFetchAddr(addr, allowLoopback); reason != "" {
		return &forbiddenDestinationError{addr: addr.String(), reason: reason}
	}
	return nil
}

// classifyFetchAddr returns a human reason when addr must not be dialled,
// or "" when it may be.
//
// The list mirrors the destinations that make an SSRF interesting:
// loopback (local services), private ranges (internal networks),
// link-local (cloud metadata at 169.254.169.254), CGNAT, unique-local
// IPv6, multicast and the reserved/documentation ranges.
func classifyFetchAddr(addr netip.Addr, allowLoopback bool) string {
	if !addr.IsValid() {
		return "invalid address"
	}
	if allowLoopback && addr.IsLoopback() {
		return ""
	}
	switch {
	case addr.IsLoopback():
		return "loopback addresses are not fetched (SSRF protection)"
	case addr.IsUnspecified():
		return "the unspecified address is not fetched"
	case addr.IsInterfaceLocalMulticast(), addr.IsMulticast():
		return "multicast addresses are not fetched"
	case addr.IsLinkLocalUnicast(), addr.IsLinkLocalMulticast():
		return "link-local addresses are not fetched (cloud metadata endpoints live here)"
	case addr.IsPrivate():
		return "private-network addresses are not fetched (SSRF protection)"
	case addr.IsGlobalUnicast() == false:
		return "non-global addresses are not fetched"
	}
	// Ranges that are global-unicast by classification but never a
	// documentation host.
	for _, prefix := range []netip.Prefix{
		netip.MustParsePrefix("100.64.0.0/10"),   // CGNAT
		netip.MustParsePrefix("192.0.0.0/24"),    // IETF protocol assignments
		netip.MustParsePrefix("198.18.0.0/15"),   // benchmarking
		netip.MustParsePrefix("192.0.2.0/24"),    // TEST-NET-1
		netip.MustParsePrefix("198.51.100.0/24"), // TEST-NET-2
		netip.MustParsePrefix("203.0.113.0/24"),  // TEST-NET-3
		netip.MustParsePrefix("240.0.0.0/4"),     // reserved
		netip.MustParsePrefix("2001:db8::/32"),   // documentation
		netip.MustParsePrefix("fc00::/7"),        // unique local (also IsPrivate, belt and braces)
	} {
		if prefix.Contains(addr) {
			return "reserved or non-public address range " + prefix.String() + " is not fetched"
		}
	}
	return ""
}

// parseMediaType parses Content-Type, tolerating a missing header.
func parseMediaType(contentType string) (string, map[string]string, error) {
	if strings.TrimSpace(contentType) == "" {
		// No Content-Type at all: treat as HTML, which is what a
		// documentation server that omits the header is serving.
		return "text/html", map[string]string{}, nil
	}
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil {
		return "", nil, err
	}
	return strings.ToLower(mediaType), params, nil
}

// referenceSourceTypeForMediaType maps a media type onto the parser
// vocabulary. Anything else is refused with the type named, because
// storing an unparsed blob as searchable documentation is worse than
// saying no.
func referenceSourceTypeForMediaType(mediaType string) (string, error) {
	switch mediaType {
	case "text/html", "application/xhtml+xml":
		return "html", nil
	case "text/markdown", "text/x-markdown", "application/markdown":
		return "markdown", nil
	case "text/plain", "application/json", "text/json":
		return "text", nil
	}
	return "", fmt.Errorf("unsupported Content-Type %q: fetch the document and convert it, then ingest the local file", mediaType)
}

// firstLineSnippet renders a bounded error-body excerpt for diagnostics.
func firstLineSnippet(b []byte) string {
	text := strings.TrimSpace(strings.ToValidUTF8(string(b), "\uFFFD"))
	if text == "" {
		return ""
	}
	if i := strings.IndexByte(text, '\n'); i >= 0 {
		text = text[:i]
	}
	text = strings.Join(strings.Fields(text), " ")
	if len(text) > 160 {
		text = text[:160] + "…"
	}
	return ": " + text
}
