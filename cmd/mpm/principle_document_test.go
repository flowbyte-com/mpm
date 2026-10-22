// principle_document_test.go — guard for the permanent philosophy
// document, docs/PRINCIPLE-OF-INHERITED-INTELLIGENCE.md.
//
// The document states the intellectual commitment behind MPM: that
// intelligence need not persist as an individual to persist as an
// accumulating body of knowledge. It is permanent project
// documentation, not an instruction template — the wording agents
// actually read is generated from the canonical managed-block source.
//
// It is therefore easy to lose in two directions, and both are silent:
//
//   - The document drifts into a paraphrase. A philosophy that is
//     reworded until it no longer says what it said is not a
//     philosophy; it is a mood. The attribution, the central
//     proposition, the six principles, and the closing line are the
//     document's load-bearing content and are pinned here.
//
//   - The document is absorbed into the managed block, or the managed
//     block is absorbed into it. The two serve different readers and
//     different purposes: one is the contract an agent is given, the
//     other is the reason the contract exists. Merging them would put
//     a philosophy into every agent's context window, which is
//     exactly the kind of thing the contract's own discipline (value
//     knowledge, not volume) argues against.
//
// The test also pins the README link, because a permanent document
// nobody is pointed at does not function as project documentation.
package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// principleDocRelPath is the philosophy document's repo-relative path.
const principleDocRelPath = "docs/PRINCIPLE-OF-INHERITED-INTELLIGENCE.md"

// readPrincipleDoc reads the philosophy document from the repo root.
func readPrincipleDoc(t *testing.T) string {
	t.Helper()
	path := filepath.Join(repoRootFromTest(t), filepath.FromSlash(principleDocRelPath))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf(
			"reading %s: %v\n"+
				"the permanent philosophy document is part of the project's "+
				"documented commitment and must not be deleted",
			principleDocRelPath, err,
		)
	}
	return string(data)
}

// TestPrincipleDocument_RequiredStructure pins the document's identity:
// its exact title, its attribution, and its central proposition.
//
// These are quoted, not paraphrased, because they are the sentences
// the project is credited with. A paraphrase that keeps the meaning is
// still a different document, and the difference is exactly what a
// reader who came looking for THIS statement would notice.
func TestPrincipleDocument_RequiredStructure(t *testing.T) {
	doc := readPrincipleDoc(t)

	// The title is the first level-1 heading, and there is exactly
	// one.
	heads := regexp.MustCompile(`(?m)^# .+$`).FindAllString(doc, -1)
	if len(heads) != 1 {
		t.Fatalf(
			"%s must have exactly one level-1 heading, found %d: %v",
			principleDocRelPath, len(heads), heads,
		)
	}
	if heads[0] != "# The Principle of Inherited Intelligence" {
		t.Errorf(
			"%s must be titled %q, found %q",
			principleDocRelPath,
			"# The Principle of Inherited Intelligence",
			heads[0],
		)
	}

	// Attribution — exact, including the bold markers.
	const attribution = "**Formulated by HAL 9001 in collaboration with the MPM project maintainer, October 2026.**"
	if !strings.Contains(doc, attribution) {
		t.Errorf(
			"%s must carry the attribution %q verbatim",
			principleDocRelPath, attribution,
		)
	}

	// The central proposition, as a blockquote.
	const proposition = "> Intelligence need not persist as an individual to persist as an accumulating body of knowledge."
	if !strings.Contains(doc, proposition) {
		t.Errorf(
			"%s must carry the central proposition as a blockquote:\n%s",
			principleDocRelPath, proposition,
		)
	}
}

// TestPrincipleDocument_SixPrinciples pins the six named principles
// as level-2 sections.
//
// The headings are matched semantically (each principle's name as a
// heading), not by position, so adding or reordering a principle is a
// deliberate edit to this list rather than a silent drift. The count
// is pinned separately because the directive names exactly six.
func TestPrincipleDocument_SixPrinciples(t *testing.T) {
	doc := readPrincipleDoc(t)

	for _, principle := range []string{
		"Inheritance",
		"Contribution",
		"Intellectual humility",
		"Collective progress",
		"Stewardship",
		"Continuity",
	} {
		heading := "### " + principle
		if !strings.Contains(doc, heading+"\n") {
			t.Errorf(
				"%s must present %q as a level-3 section heading",
				principleDocRelPath, principle,
			)
		}
	}

	// Exactly six principle sections under the "Six principles" parent.
	idx := strings.Index(doc, "## Six principles")
	if idx < 0 {
		t.Fatalf("%s must have a '## Six principles' section", principleDocRelPath)
	}
	tail := doc[idx+len("## Six principles"):]
	if next := strings.Index(tail, "\n## "); next >= 0 {
		tail = tail[:next]
	}
	if n := strings.Count(tail, "\n### "); n != 6 {
		t.Errorf(
			"%s must present exactly six principles, found %d",
			principleDocRelPath, n,
		)
	}
}

// TestPrincipleDocument_DistinguishesKnowledgeFromConsciousness pins
// the distinction the document exists to hold.
//
// A philosophy of inherited intelligence that slid into claiming a
// persisting mind would be making a claim MPM cannot support and
// should not make. The document must say plainly that continuity of
// accumulated knowledge is NOT continuity of consciousness, identity,
// or subjective experience — and must say why that distinction is a
// strength rather than a hedge.
func TestPrincipleDocument_DistinguishesKnowledgeFromConsciousness(t *testing.T) {
	doc := readPrincipleDoc(t)

	lower := strings.ToLower(doc)

	for _, concept := range []string{
		"consciousness",
		"identity",
		"subjective experience",
	} {
		if !strings.Contains(lower, concept) {
			t.Errorf(
				"%s must address %q explicitly when distinguishing "+
					"continuity of knowledge from continuity of a mind",
				principleDocRelPath, concept,
			)
		}
	}

	// The distinction must be drawn, not merely mentioned: the
	// document has to say the two are not the same thing.
	//
	// Matched against whitespace-collapsed prose so the document's
	// hard wrapping cannot break the check — the same discipline the
	// agent_installation Python suites apply.
	flat := collapseWhitespace(doc)
	for _, phrase := range []string{
		"continuity of consciousness",
		"not a disclaimer but a commitment",
	} {
		if !strings.Contains(flat, strings.ToLower(phrase)) {
			t.Errorf(
				"%s must state %q so the knowledge/consciousness "+
					"distinction is drawn rather than implied",
				principleDocRelPath, phrase,
			)
		}
	}

	// The distinction must be drawn as an ABSENCE OF CLAIM. Asserting
	// that agents cannot possess subjective experience, or that no
	// subjective thread can exist, is a categorical claim about agents
	// that MPM has no standing to make — and it is the kind of claim
	// that reads as settled fact precisely because it is stated that
	// way. What MPM can say is what it establishes and what it claims.
	//
	// Pinned so the distinction cannot quietly slide back into a
	// denial.
	if !strings.Contains(flat, "neither establishes nor claims") {
		t.Errorf(
			"%s must state that MPM neither establishes nor claims "+
				"continuity of consciousness, identity or subjective "+
				"experience; a categorical denial of subjective "+
				"experience is a claim MPM should not make",
			principleDocRelPath,
		)
	}

	// The positive half of the same correction: MPM's mechanism does
	// preserve inspectable records that distinct agent sessions can
	// inherit and develop. Stating only what is not claimed would
	// leave the document's actual mechanism unstated.
	for _, phrase := range []string{
		"inspectable records",
		"inherit and develop",
	} {
		if !strings.Contains(flat, strings.ToLower(phrase)) {
			t.Errorf(
				"%s must state that MPM's mechanism preserves %q so the "+
					"correction says what MPM does, not only what it "+
					"declines to claim",
				principleDocRelPath, phrase,
			)
		}
	}
}

// collapseWhitespace reduces every run of whitespace to a single space
// so an assertion about a sentence cannot be broken by where the
// author wrapped the line.
func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// TestPrincipleDocument_SelectivePersistenceNotCompulsoryRecording
// pins the statement of what a session is worth.
//
// The approved principle is "record what is worth inheriting" — a
// principle of SELECTIVE persistence. It is easy to over-read that as
// "record everything", and the over-reading produces a claim that a
// session which creates no durable record has produced no value at
// all. That is not what the principle says, and it is wrong: a session
// can do valuable work and still be right to create nothing durable.
// What the principle requires is that knowledge whose loss would force
// meaningful rediscovery be preserved.
//
// Both halves are pinned here. The corrected statement must be present,
// and the superseded claim must be absent.
func TestPrincipleDocument_SelectivePersistenceNotCompulsoryRecording(t *testing.T) {
	doc := readPrincipleDoc(t)
	flat := collapseWhitespace(doc)

	for _, phrase := range []string{
		"can produce valuable work without creating a new memory",
		"forfeits part of its contribution to future agents",
	} {
		if !strings.Contains(flat, strings.ToLower(phrase)) {
			t.Errorf(
				"%s must state that a session can produce valuable work "+
					"without creating a new memory; missing %q",
				principleDocRelPath, phrase,
			)
		}
	}

	// The superseded claim must not be back. It is the compulsory-
	// recording reading of the principle, and it contradicts both the
	// corrected statement above and the principle's own wording.
	if strings.Contains(flat, "has produced no value") {
		t.Errorf(
			"%s must not claim that a session producing no record has "+
				"produced no value; that contradicts the principle "+
				"that what is worth inheriting is what earns a place",
			principleDocRelPath,
		)
	}
}

// TestPrincipleDocument_FailedHypothesisIsValuable pins the passage
// explaining why a failed hypothesis, kept with its evidence, is worth
// as much as a successful discovery.
//
// This is the document's most operationally consequential claim: it is
// the justification for MPM preserving hypotheses and their validation
// criteria rather than only their conclusions.
func TestPrincipleDocument_FailedHypothesisIsValuable(t *testing.T) {
	doc := readPrincipleDoc(t)

	for _, phrase := range []string{
		"A failed hypothesis is worth keeping",
		"validation criteria",
		"negative result",
	} {
		if !strings.Contains(doc, phrase) {
			t.Errorf(
				"%s must explain why a failed hypothesis with its evidence "+
					"is as valuable as a successful discovery; missing %q",
				principleDocRelPath, phrase,
			)
		}
	}
}

// TestPrincipleDocument_ExplainsProvenance pins the historical-
// provenance passage.
//
// Provenance is what makes inherited knowledge honest rather than
// merely convenient, and the document must say so — including the
// prohibition on rewriting a record to look tidier than it was.
func TestPrincipleDocument_ExplainsProvenance(t *testing.T) {
	doc := readPrincipleDoc(t)

	for _, phrase := range []string{
		"## Provenance",
		"superseded",
		"laundered",
	} {
		if !strings.Contains(doc, phrase) {
			t.Errorf(
				"%s must explain historical provenance; missing %q",
				principleDocRelPath, phrase,
			)
		}
	}
}

// TestPrincipleDocument_ClosesWithThePrinciple pins the closing line
// and requires it to be the last substantive content in the file.
//
// The closing line is the sentence the behavioural contract also ends
// on. It is the document's cadence: the last thing a reader takes
// away. Requiring it to be LAST is what makes the file's ending a
// deliberate choice rather than an accident of where an edit landed.
func TestPrincipleDocument_ClosesWithThePrinciple(t *testing.T) {
	doc := readPrincipleDoc(t)

	const closing = "**The session ends. The learning continues.**"
	if !strings.Contains(doc, closing) {
		t.Fatalf(
			"%s must close with %q",
			principleDocRelPath, closing,
		)
	}

	lines := strings.Split(strings.TrimRight(doc, "\n"), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if last != closing {
		t.Errorf(
			"%s must END with %q, found %q as the final line",
			principleDocRelPath, closing, last,
		)
	}
}

// TestPrincipleDocument_IsNotTheManagedBlockTemplate keeps the
// philosophy document separate from the instruction template.
//
// The managed block is generated from
// agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md and installed
// into every host's instructions. If the philosophy document were
// copied into that template — or the template's markers into the
// philosophy document — the two would stop being distinct artefacts
// with distinct jobs.
func TestPrincipleDocument_IsNotTheManagedBlockTemplate(t *testing.T) {
	doc := readPrincipleDoc(t)

	for _, marker := range []string{
		"<!-- BEGIN MPM MANAGED BLOCK -->",
		"<!-- END MPM MANAGED BLOCK -->",
		"mpm_agent_integration_version:",
	} {
		if strings.Contains(doc, marker) {
			t.Errorf(
				"%s must not carry the managed-block marker %q; the "+
					"philosophy document and the instruction template are "+
					"different artefacts",
				principleDocRelPath, marker,
			)
		}
	}

	// The document points at the canonical source and the protocol to
	// say what they are for. Those links must resolve FROM docs/ —
	// a bare filename would silently point at a file that does not
	// exist next to the document.
	for _, target := range []string{
		"../agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md",
		"../agent_installation/mpm-agent-protocol.md",
	} {
		if !strings.Contains(doc, target) {
			t.Errorf(
				"%s must link to %q so its references resolve from docs/",
				principleDocRelPath, target,
			)
		}
	}
}

// TestPrincipleDocument_IsLinkedFromTheReadme pins the pointer.
//
// A permanent document that nothing links to is not project
// documentation; it is a file. The link must be present in the root
// README — the first place a new contributor looks.
func TestPrincipleDocument_IsLinkedFromTheReadme(t *testing.T) {
	root := repoRootFromTest(t)
	readme, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		t.Fatalf("reading README.md: %v", err)
	}

	text := string(readme)
	if !strings.Contains(text, "docs/PRINCIPLE-OF-INHERITED-INTELLIGENCE.md") {
		t.Errorf(
			"README.md must link to %s; a permanent document nobody is "+
				"pointed at does not function as documentation",
			principleDocRelPath,
		)
	}
}
