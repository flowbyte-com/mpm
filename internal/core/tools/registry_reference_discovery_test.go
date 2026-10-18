// registry_reference_discovery_test.go — regression coverage for the
// agent reference-discovery defect (issue #25).
//
// The WordPress incident: an agent asked whether MPM held WordPress API
// documentation, searched its MEMORIES, found nothing, and concluded
// the corpus was empty. The document was present the whole time
// (WordPress REST API reference, reference_docs row ebcd27dfc078ed31).
//
// Retrieval was never broken. Retrieval was never reached, because
// nothing an agent reads before deciding what to look at mentioned
// that references are a subsystem you query separately. Two surfaces
// carry that instruction, and this file pins both:
//
//   - the managed behavioural block, whose canonical source is
//     agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md and whose
//     per-host render is asserted in
//     agent_installation/tests/test_reference_discovery_contract.py;
//   - the mpm_references tool description below, which is what an MCP
//     client reads when it decides which tool to reach for.
//
// The description used to be written entirely from the ingestion side:
// it opened "Add reference documents to your library" and closed with
// a pointer to mpm_evidence for URLs. Every sentence was about
// getting a document IN. Nothing said the corpus could be searched or
// read, so the tool that would have answered the question advertised
// itself only as a writer. It led an agent away from lookup rather
// than toward it.
//
// These assertions are deliberately about the SHAPE of the guidance —
// leads with retrieval, separates the two subsystems, names the two
// actions, insists on reading the document, names freshness — and not
// about the prose. A rewrite that keeps those five properties passes;
// a rewrite that quietly drops one fails.
package tools

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// referencesDescription returns the registered mpm_references
// description, failing if the tool is not registered.
func referencesDescription(t *testing.T) string {
	t.Helper()
	tool, ok := ByName("mpm_references")
	require.True(t, ok, "mpm_references must be registered")
	require.NotEmpty(t, tool.Description, "mpm_references must carry a description")
	return tool.Description
}

// TestReferenceToolDescription_LeadsWithRetrieval pins the first
// failure that produced the incident. An agent reads the first line to
// decide what the tool is for, so a description that opens on
// ingestion reads as "this tool adds documents" — which is true, and
// useless to an agent holding a question.
func TestReferenceToolDescription_LeadsWithRetrieval(t *testing.T) {
	desc := referencesDescription(t)
	firstLine := strings.SplitN(strings.TrimSpace(desc), "\n", 2)[0]

	require.Contains(t, strings.ToLower(firstLine), "search and read",
		"the opening line must describe reading and searching, which is what an agent is doing when it reaches for this tool; it opens %q", firstLine)
}

// TestReferenceToolDescription_SeparatesMemoriesFromReferences pins
// the specific inference that failed. "An empty memory search is not
// evidence of absence" is the whole defect in one sentence: without
// it, a memory query that returns nothing is indistinguishable from an
// empty reference corpus, and the agent concludes from the wrong
// subsystem.
func TestReferenceToolDescription_SeparatesMemoriesFromReferences(t *testing.T) {
	desc := referencesDescription(t)
	lower := strings.ToLower(desc)

	require.Contains(t, lower, "separate subsystem from memories",
		"the description must state that references are a subsystem distinct from memories")
	require.Contains(t, lower, "not evidence of absence",
		"the description must state that an empty memory search does not establish absence; that inference is the incident")
}

// TestReferenceToolDescription_NamesBothLookupActions pins the two
// actions an agent needs, and pins them to the schema enum so the
// description cannot advertise an action the schema would reject.
func TestReferenceToolDescription_NamesBothLookupActions(t *testing.T) {
	desc := strings.ToLower(referencesDescription(t))

	for _, action := range []string{"search", "read"} {
		require.Contains(t, desc, action,
			"the description must name the %q action so the agent knows the lookup is reachable", action)
	}

	tool, ok := ByName("mpm_references")
	require.True(t, ok)

	var schema map[string]interface{}
	require.NoError(t, json.Unmarshal(tool.Schema, &schema))

	props, ok := schema["properties"].(map[string]interface{})
	require.True(t, ok)
	action, ok := props["action"].(map[string]interface{})
	require.True(t, ok)
	enum, ok := action["enum"].([]interface{})
	require.True(t, ok)

	advertised := map[string]bool{}
	for _, v := range enum {
		if s, ok := v.(string); ok {
			advertised[s] = true
		}
	}
	for _, required := range []string{"search", "read"} {
		require.True(t, advertised[required],
			"the description advertises the %q action, so the schema enum must accept it", required)
	}
}

// TestReferenceToolDescription_RequiresReadingTheDocument pins the
// second inference the incident exposed. A search result is a snippet
// and a score; treating it as the answer is how an agent ends up
// quoting a signature it saw mid-file. The description must send the
// agent on to the document itself.
func TestReferenceToolDescription_RequiresReadingTheDocument(t *testing.T) {
	desc := strings.ToLower(referencesDescription(t))

	require.Contains(t, desc, "do not act on a search-result summary",
		"the description must tell the agent a search summary is not the document")
	require.Contains(t, desc, "using the id",
		"the description must say the read action takes an id, or the agent cannot act on a search hit")
}

// TestReferenceToolDescription_NamesFreshness pins the third failure
// mode. Reference documents carry a version and an ingestion date;
// acting on a stale or version-bound manual is a wrong answer that
// looks like a right one. The classification values are named in the
// description, so this test also pins them against the vocabulary the
// retrieval path actually produces.
func TestReferenceToolDescription_NamesFreshness(t *testing.T) {
	desc := referencesDescription(t)
	lower := strings.ToLower(desc)

	require.Contains(t, lower, "freshness",
		"the description must tell the agent to check freshness before relying on technical documentation")

	for _, classification := range []string{"current", "stale", "version-bound", "historical", "unknown"} {
		require.Contains(t, lower, classification,
			"the description must name the %q freshness classification so the agent knows what it is looking at", classification)
	}
}
