// f4_mirror_credential_leak_regression_test.go — regression guard for the
// 2026-09-04 residual-inventory finding F-4.
//
// F-4 P0: appendBlockedAttempt wrote the first 100 bytes of blocked content
// verbatim to mirror.jsonl (memory.go:1235-1255). A blocked credential such
// as ghp_xxx, sk-ant-xxx, or PEM private-key material was retained on
// disk in the audit log — exactly the bytes an attacker needs to identify
// the secret family.
//
// The security invariant:
//   A credential rejected by the secret scanner must not be persisted
//   verbatim anywhere in the MPM substrate, including audit/mirror
//   artifacts.
//
// This test pins the new safe representation:
//   - mirror.jsonl entry contains a content_sha256 digest
//   - mirror.jsonl entry contains the matched pattern_family (no secret)
//   - mirror.jsonl entry does NOT contain the raw prefix or suffix
//   - mirror.jsonl entry does NOT contain the secret value verbatim
//
// Pre-fix: this test fails because content_snippet contains the raw prefix.
// Post-fix: this test passes because content_snippet is replaced with
//   content_sha256 + pattern_family.
package internal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestF4_BlockedCredentialPrefixDoesNotLeakIntoMirror is the primary
// regression guard. It synthesizes a known GitHub PAT, runs it through
// the scanner via appendBlockedAttempt, and asserts:
//
//  1. The mirror file exists.
//  2. The mirror entry has a content_sha256 field.
//  3. The mirror entry's content_sha256 matches sha256(content).
//  4. The raw secret value is NOT in the mirror entry.
//  5. The matched pattern_family (a non-secret label) IS in the mirror entry.
//  6. Neither "ghp_" nor any other prefix byte sequence appears in the
//     mirror entry.
func TestF4_BlockedCredentialPrefixDoesNotLeakIntoMirror(t *testing.T) {
	tmpDir := t.TempDir()
	mirrorPath := filepath.Join(tmpDir, "mirror.jsonl")

	store := &MemoryStore{
		MirrorFile: mirrorPath,
	}

	const fakeGHP = "ghp_aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789"
	reason := "blocked: GitHub Personal Token"
	require.NoError(t, store.appendBlockedAttempt(fakeGHP, reason, "sensitive_attempt"))

	// Read the mirror entry.
	data, err := os.ReadFile(mirrorPath)
	require.NoError(t, err, "mirror file should exist after appendBlockedAttempt")
	require.NotEmpty(t, data, "mirror file should not be empty")

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	require.Len(t, lines, 1, "exactly one entry expected")

	var entry map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &entry))

	// Invariant 1: no raw prefix or substring of the secret in any field.
	for _, field := range []string{"content_snippet", "content", "snippet", "prefix"} {
		if v, ok := entry[field].(string); ok {
			require.NotContains(t, v, "ghp_", "field %q must not contain raw 'ghp_' prefix", field)
			require.NotContains(t, v, fakeGHP[:10], "field %q must not contain raw secret prefix bytes", field)
			require.NotEqual(t, fakeGHP, v, "field %q must not equal the raw secret", field)
		}
	}

	// Invariant 2: the raw secret value itself never appears anywhere in
	// the mirror entry (defensive: catches future fields).
	require.NotContains(t, lines[0], fakeGHP, "raw secret must not appear anywhere in the mirror line")

	// Invariant 3: a sha256 digest of the original content IS present.
	expectedDigest := sha256.Sum256([]byte(fakeGHP))
	expectedDigestHex := hex.EncodeToString(expectedDigest[:])
	var foundDigest bool
	for _, field := range []string{"content_sha256", "digest", "sha256"} {
		if v, ok := entry[field].(string); ok && v == expectedDigestHex {
			foundDigest = true
			break
		}
	}
	require.True(t, foundDigest,
		"mirror entry must contain sha256 digest of blocked content; entry=%v", entry)

	// Invariant 4: the pattern family label (no secret material) is recorded.
	var family string
	for _, field := range []string{"pattern_family", "family", "matched_pattern"} {
		if v, ok := entry[field].(string); ok && v != "" {
			family = v
			break
		}
	}
	require.NotEmpty(t, family, "mirror entry must record the matched pattern family; entry=%v", entry)
	require.NotContains(t, family, fakeGHP[:5], "pattern family label must not contain secret material")
}

// TestF4_BlockedPEMDoesNotLeakIntoMirror — same shape, different family.
// A blocked PEM private key prefix is exactly the bytes an attacker needs
// to identify a private key. Pre-fix: prefix persisted. Post-fix: replaced
// with sha256 + family="Private Key".
func TestF4_BlockedPEMDoesNotLeakIntoMirror(t *testing.T) {
	tmpDir := t.TempDir()
	mirrorPath := filepath.Join(tmpDir, "mirror.jsonl")

	store := &MemoryStore{MirrorFile: mirrorPath}

	const fakePEM = "-----BEGIN RSA PRIVATE KEY-----\nMIIEpAIBAAKCAQEAabcd1234FAKEKEY\n-----END RSA PRIVATE KEY-----\n"
	require.NoError(t, store.appendBlockedAttempt(fakePEM, "blocked: Private Key", "sensitive_attempt"))

	data, err := os.ReadFile(mirrorPath)
	require.NoError(t, err)

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	require.GreaterOrEqual(t, len(lines), 1)

	for _, line := range lines {
		require.NotContains(t, line, "BEGIN RSA PRIVATE KEY",
			"PEM header must not appear in mirror entry")
		require.NotContains(t, line, "MIIEpAIBAAKCAQEA",
			"PEM key material prefix must not appear in mirror entry")
	}
}

// TestF4_BlockedAnthropicKeyDoesNotLeakIntoMirror — Anthropic API key
// prefix sk-ant-api03- must not appear in mirror.
func TestF4_BlockedAnthropicKeyDoesNotLeakIntoMirror(t *testing.T) {
	tmpDir := t.TempDir()
	mirrorPath := filepath.Join(tmpDir, "mirror.jsonl")

	store := &MemoryStore{MirrorFile: mirrorPath}

	const fakeAnthropic = "sk-ant-api03-aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789abcdefABCDEFGHIJ"
	require.NoError(t, store.appendBlockedAttempt(fakeAnthropic, "blocked: Anthropic API Key", "sensitive_attempt"))

	data, err := os.ReadFile(mirrorPath)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	require.Len(t, lines, 1)
	require.NotContains(t, lines[0], "sk-ant-", "Anthropic prefix must not appear in mirror entry")
	require.NotContains(t, lines[0], fakeAnthropic, "raw Anthropic key must not appear in mirror entry")

	// sha256 still present.
	expectedDigest := sha256.Sum256([]byte(fakeAnthropic))
	expectedHex := hex.EncodeToString(expectedDigest[:])
	var entry map[string]interface{}
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &entry))
	var digestField string
	for _, f := range []string{"content_sha256", "digest"} {
		if v, ok := entry[f].(string); ok {
			digestField = v
			break
		}
	}
	require.Equal(t, expectedHex, digestField,
		"sha256 digest must match sha256(original blocked content)")
}

// TestF4_DigestMatchesOriginalContent — explicit invariant: the digest
// stored in the mirror entry is the digest of the FULL original content
// (not a prefix or suffix), so it can still be used for forensic
// correlation by an operator who knows both sides of the hash.
func TestF4_DigestMatchesOriginalContent(t *testing.T) {
	tmpDir := t.TempDir()
	mirrorPath := filepath.Join(tmpDir, "mirror.jsonl")

	store := &MemoryStore{MirrorFile: mirrorPath}

	const content = "ghp_aBcDeFgHiJkLmNoPqRsTuVwXyZ0123456789 extra context that the operator would not have seen"
	require.NoError(t, store.appendBlockedAttempt(content, "blocked: GitHub Personal Token", "sensitive_attempt"))

	data, err := os.ReadFile(mirrorPath)
	require.NoError(t, err)

	var entry map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &entry))

	var digestField string
	for _, f := range []string{"content_sha256", "digest"} {
		if v, ok := entry[f].(string); ok {
			digestField = v
			break
		}
	}
	require.NotEmpty(t, digestField, "digest field must be present")

	expected := sha256.Sum256([]byte(content))
	expectedHex := hex.EncodeToString(expected[:])
	require.Equal(t, expectedHex, digestField,
		"digest must match sha256 of FULL original content, not a truncated prefix")
}

// TestF4_MultipleEntriesEachGetOwnDigest — proves the digest path is
// per-entry, not a single static value.
func TestF4_MultipleEntriesEachGetOwnDigest(t *testing.T) {
	tmpDir := t.TempDir()
	mirrorPath := filepath.Join(tmpDir, "mirror.jsonl")

	store := &MemoryStore{MirrorFile: mirrorPath}

	const s1 = "ghp_firsttoken123456789012345678901234567"
	const s2 = "ghp_secondtoken098765432109876543210987654"
	require.NoError(t, store.appendBlockedAttempt(s1, "blocked", "sensitive_attempt"))
	require.NoError(t, store.appendBlockedAttempt(s2, "blocked", "sensitive_attempt"))

	data, err := os.ReadFile(mirrorPath)
	require.NoError(t, err)

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	require.Len(t, lines, 2)

	d1Sum := sha256.Sum256([]byte(s1))
	d1Expected := hex.EncodeToString(d1Sum[:])
	d2Sum := sha256.Sum256([]byte(s2))
	d2Expected := hex.EncodeToString(d2Sum[:])

	require.Contains(t, lines[0], d1Expected, "first entry must contain sha256(s1)")
	require.Contains(t, lines[1], d2Expected, "second entry must contain sha256(s2)")
	require.NotContains(t, lines[0], s1, "first entry must not contain raw s1")
	require.NotContains(t, lines[1], s2, "second entry must not contain raw s2")
}
