// handlers_reference_url_test.go — `mpm reference add --url` contract.
//
// History: this file used to pin a rejection ("--url is reserved for a
// future release") because the reference subsystem had no HTTP code at
// all, and a silently-ignored flag was worse than no flag. URL ingestion
// now exists, so the contract changed: a URL is fetched, and only a
// destination the SSRF defence refuses is rejected.
//
// These tests run in the production configuration on purpose. The fetch
// pipeline's loopback test seam is unexported, so the CLI cannot reach a
// loopback fixture — which is exactly the property worth pinning here:
// the CLI refuses loopback, and it refuses it as a fetch error rather
// than as an unimplemented feature.
package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/flowbyte-com/mpm-core/usererror"
)

// captureUserError redirects the usererror package writer to a buffer for
// the duration of fn, restoring the default afterwards. The CLI
// usererror package routes all Error/Usage/Notice output through one
// writer — SetWriter is the supported capture mechanism.
func captureUserError(t *testing.T, fn func() int) string {
	t.Helper()
	var buf bytes.Buffer
	usererror.SetWriter(&buf)
	defer usererror.SetWriter(nil)
	_ = fn()
	return buf.String()
}

// TestHandleRefAdd_URLFlag_IsIngestedNotReserved pins that --url is a real
// ingestion path now, and that the refusal it produces for a loopback
// destination is the security refusal, not "future feature".
func TestHandleRefAdd_URLFlag_IsIngestedNotReserved(t *testing.T) {
	out := captureUserError(t, func() int {
		return handleRefAdd([]string{"add", "--url", "http://127.0.0.1:9/doc", "--json"})
	})
	if strings.Contains(strings.ToLower(out), "future") {
		t.Errorf("--url is still reported as an unimplemented feature: %s", out)
	}
	if !strings.Contains(out, "loopback") && !strings.Contains(out, "refusing to fetch") {
		t.Errorf("loopback destination was not refused by the fetch pipeline: %s", out)
	}
}

// TestHandleRefAdd_URLFlag_RejectsNonHTTPScheme pins that a non-http(s)
// scheme is a usage error, never a fetch and never a local path.
func TestHandleRefAdd_URLFlag_RejectsNonHTTPScheme(t *testing.T) {
	out := captureUserError(t, func() int {
		return handleRefAdd([]string{"add", "--url", "ftp://example.org/doc"})
	})
	if !strings.Contains(strings.ToLower(out), "http") {
		t.Errorf("non-http scheme was not refused with a clear reason: %s", out)
	}
}

// TestHandleRefAdd_ConflictingSources pins that a path plus a URL is a
// conflict rather than a silent preference.
func TestHandleRefAdd_ConflictingSources(t *testing.T) {
	out := captureUserError(t, func() int {
		return handleRefAdd([]string{"add", "some-file.md", "--url", "https://example.org/doc"})
	})
	if !strings.Contains(strings.ToLower(out), "conflict") && !strings.Contains(strings.ToLower(out), "either") {
		t.Errorf("path+URL was not reported as a conflict: %s", out)
	}
}

// TestHandleRefAdd_NoArgs_ShowsUsage pins the missing-argument path.
func TestHandleRefAdd_NoArgs_ShowsUsage(t *testing.T) {
	out := captureUserError(t, func() int {
		return handleRefAdd([]string{"add"})
	})
	if !strings.Contains(out, "Usage") && !strings.Contains(out, "usage") {
		t.Errorf("missing-args path should print usage, got: %s", out)
	}
}
