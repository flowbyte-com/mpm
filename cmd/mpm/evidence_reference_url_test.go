package main

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// evidence_reference_url_test.go — the CLI and `mpm why` surfaces for the
// optional reference URL on an evidence row.
//
// The flag's job is to carry the user's string through untouched and to fail
// loudly on a malformed one, without ever implying MPM went looking for it.

// TestEvidenceAdd_ReferenceURLFlagCarriesValue pins the happy path: what the
// operator types is what reaches the payload.
//
// The URL deliberately carries a fragment, a descending query order and
// mixed case — shapes a normalizer would rewrite. The assertion is exact
// equality, not containment.
func TestEvidenceAdd_ReferenceURLFlagCarriesValue(t *testing.T) {
	const ref = "https://Example.COM/Spec.md?v=2&a=1&z=9#Section-3"
	args := []string{
		"--artifact", "mem-1",
		"--type", "external_reference",
		"--source", "external",
		"--by", "test",
		"--reference-url", ref,
	}
	payload, err := parseEvidenceAddArgs(args)
	require.NoError(t, err)
	assert.Equal(t, ref, payload["reference_url"],
		"the CLI must carry the supplied URL through verbatim")
}

// TestEvidenceAdd_ReferenceURLIsOptional pins that omitting the flag behaves
// exactly as it did before the flag existed. Every existing invocation must
// keep working, and the payload key must be present-but-empty rather than
// absent, so the handler's type assertion cannot panic.
func TestEvidenceAdd_ReferenceURLIsOptional(t *testing.T) {
	args := []string{
		"--artifact", "mem-1",
		"--type", "observation",
		"--source", "x",
		"--by", "test",
	}
	payload, err := parseEvidenceAddArgs(args)
	require.NoError(t, err)

	v, present := payload["reference_url"]
	require.True(t, present,
		"the payload must always carry a reference_url key so handleEvidenceAdd "+
			"can type-assert it unconditionally")
	assert.Equal(t, "", v)
}

// TestEvidenceAdd_RejectsInvalidReferenceURL pins that a malformed URL is
// refused at the parser, before any DB work, with an error that names the
// flag the operator actually typed.
func TestEvidenceAdd_RejectsInvalidReferenceURL(t *testing.T) {
	for _, bad := range []string{
		"ftp://example.com/x",
		"file:///etc/passwd",
		"javascript:alert(1)",
		"example.com/no-scheme",
		"https://",
		"not a url",
	} {
		args := []string{
			"--artifact", "mem-1",
			"--type", "observation",
			"--source", "x",
			"--by", "test",
			"--reference-url", bad,
		}
		_, err := parseEvidenceAddArgs(args)
		require.Error(t, err, "%q must be rejected", bad)
		assert.Contains(t, err.Error(), "--reference-url",
			"the error should name the flag the operator typed")
	}
}

// TestEvidenceAdd_UsageMentionsReferenceURL pins discoverability. The usage
// text is the only documentation a shell user sees before running the
// command, and it is where the non-fetch guarantee must be stated — otherwise
// `--reference-url` reads like a link checker.
func TestEvidenceAdd_UsageMentionsReferenceURL(t *testing.T) {
	args := []string{
		"--artifact", "mem-1",
		"--type", "observation",
		"--source", "x",
		"--by", "test",
		"--help",
	}
	// flag.ContinueOnError means --help returns flag.ErrHelp; the usage text
	// has already been written to the flagset's output by then.
	_, err := parseEvidenceAddArgs(args)
	require.Error(t, err)

	usage := captureEvidenceUsage(t)
	assert.Contains(t, usage, "--reference-url",
		"the usage text should list the flag")
	for _, phrase := range []string{"never fetched", "confidence"} {
		assert.Contains(t, usage, phrase,
			"usage text should state %q so operators do not read the flag as a "+
				"link checker", phrase)
	}
}

// captureEvidenceUsage renders the evidence-add usage block by running the
// parser with --help and capturing stdout.
func captureEvidenceUsage(t *testing.T) string {
	t.Helper()
	var buf bytes.Buffer
	orig := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w

	done := make(chan string, 1)
	go func() {
		var out bytes.Buffer
		_, _ = out.ReadFrom(r)
		done <- out.String()
	}()

	_, _ = parseEvidenceAddArgs([]string{"--help"})
	_ = w.Close()
	os.Stdout = orig
	buf.WriteString(<-done)
	_ = r.Close()
	return buf.String()
}

// TestWhyRenderer_RendersReferenceURL pins the `mpm why` surface, which is
// where the question "what external reference did this come from?" is
// actually asked.
func TestWhyRenderer_RendersReferenceURL(t *testing.T) {
	var buf bytes.Buffer
	r := &WhyRenderer{out: &buf}
	r.renderEvidence([]EvidenceRow{{
		Type:         "external_reference",
		Strength:     0.6,
		CreatedBy:    "tester",
		SourceGroup:  "external",
		ReferenceURL: "https://example.com/spec#Section-3",
	}})

	out := buf.String()
	assert.Contains(t, out, "ref:")
	assert.Contains(t, out, "https://example.com/spec#Section-3",
		"the reference must be rendered verbatim, including its fragment")
}

// TestWhyRenderer_OmitsEmptyReferenceURL pins that rows without a reference
// are unchanged from before the field existed — no dangling label, no blank
// line where there is nothing to show.
func TestWhyRenderer_OmitsEmptyReferenceURL(t *testing.T) {
	var buf bytes.Buffer
	r := &WhyRenderer{out: &buf}
	r.renderEvidence([]EvidenceRow{{
		Type:        "observation",
		Strength:    0.4,
		CreatedBy:   "tester",
		SourceGroup: "filesystem",
		Notes:       "a plain observation",
	}})

	out := buf.String()
	assert.NotContains(t, out, "ref:")
	assert.Contains(t, out, "a plain observation",
		"the existing notes line must be unaffected")
}

// TestWhyRenderer_TruncatesLongReferenceURL pins that an over-long reference
// cannot flood the terminal. Validation caps the stored value at 2048 chars,
// which is still far more than a rendered evidence line should carry.
func TestWhyRenderer_TruncatesLongReferenceURL(t *testing.T) {
	long := "https://example.com/" + strings.Repeat("x", 300)
	var buf bytes.Buffer
	r := &WhyRenderer{out: &buf}
	r.renderEvidence([]EvidenceRow{{
		Type:         "external_reference",
		Strength:     0.6,
		CreatedBy:    "tester",
		ReferenceURL: long,
	}})

	out := buf.String()
	line := ""
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "ref:") {
			line = l
			break
		}
	}
	require.NotEmpty(t, line, "expected a ref: line")
	assert.Less(t, len(line), 120, "a long reference must be truncated for display")
	assert.Contains(t, line, "…", "truncation should be visibly marked")
}
