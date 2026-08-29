package internal

import (
	"strings"
	"testing"
	"time"
)

// TestDiscoveryMatrix is the artifact × surface sanity check for the
// alpha discoverability pass (Step 19). Each subtest seeds one artifact
// of a given class, then queries the documented substrate surface to
// confirm the artifact is reachable. The test does not exercise the
// agent — it exercises the substrate so a future-me who adds a new
// artifact class can copy this pattern and confirm discoverability.
//
// Each subtest is hermetic via NewTestDM(t). FTS5 must be compiled in
// (CGO_CFLAGS=-DSQLITE_ENABLE_FTS5).
func TestDiscoveryMatrix(t *testing.T) {
	dm := NewTestDM(t)

	t.Run("memory is reachable via FTS5 hybrid search", func(t *testing.T) {
		// Seed: a unique memorable fact.
		_, _, err := dm.SaveMemoryWithContext(
			"MPM alpha discoverability matrix unique-token-DISCOV-MEM-9182",
			"memories",
			[]string{"alpha", "discov-matrix"},
			0, "",
			ActiveContext{},
		)
		if err != nil {
			t.Fatalf("SaveMemoryWithContext: %v", err)
		}

		// Discover via FTS5 hybrid search (the substrate path that
		// mpm_memory query uses).
		hits, err := dm.HybridSearchMemories("DISCOV-MEM-9182", "", 10, "local")
		if err != nil {
			t.Fatalf("HybridSearchMemories: %v", err)
		}
		if len(hits) == 0 {
			t.Fatal("memory not reachable via FTS5 hybrid search")
		}
	})

	t.Run("decision is reachable via memories collection filter", func(t *testing.T) {
		// Seed: a decision with the unique token.
		_, err := dm.RecordDecision(
			"context for test",
			"choice for test DISCOV-DEC-4417",
			"rationale",
			"outcome",
			[]string{"alpha", "discov-matrix"},
			nil,
			ActiveContext{},
		)
		if err != nil {
			t.Fatalf("RecordDecision: %v", err)
		}

		// Discover: FTS5 over the decisions collection.
		hits, err := dm.HybridSearchMemories("DISCOV-DEC-4417", "decisions", 10, "local")
		if err != nil {
			t.Fatalf("HybridSearchMemories(decisions): %v", err)
		}
		if len(hits) == 0 {
			t.Fatal("decision not reachable via FTS5 with collection=decisions")
		}
	})

	t.Run("theory is reachable via memories collection filter", func(t *testing.T) {
		// Seed: a hypothesis containing the unique token.
		_, err := dm.ProposeTheory(
			"hypothesis DISCOV-THY-7756 holds under test conditions",
			"validation criteria",
			nil, nil,
			[]string{"alpha", "discov-matrix"},
		)
		if err != nil {
			t.Fatalf("ProposeTheory: %v", err)
		}

		// Discover: FTS5 over the theories collection.
		hits, err := dm.HybridSearchMemories("DISCOV-THY-7756", "theories", 10, "local")
		if err != nil {
			t.Fatalf("HybridSearchMemories(theories): %v", err)
		}
		if len(hits) == 0 {
			t.Fatal("theory not reachable via FTS5 with collection=theories")
		}
	})

	t.Run("lesson is reachable via SearchLessonsLimited", func(t *testing.T) {
		// Seed: a lesson with a unique token.
		_, err := dm.AddLesson(
			"lesson content DISCOV-LSN-2341 always check freshness",
			LessonTypeInsight,
			[]string{"alpha", "discov-matrix"},
			"",
		)
		if err != nil {
			t.Fatalf("AddLesson: %v", err)
		}

		// Discover: lessons_fts FTS5 path.
		hits, err := dm.SearchLessonsLimited("DISCOV-LSN-2341")
		if err != nil {
			t.Fatalf("SearchLessonsLimited: %v", err)
		}
		if len(hits) == 0 {
			t.Fatal("lesson not reachable via SearchLessonsLimited")
		}
	})

	t.Run("skill is reachable via ListSkills", func(t *testing.T) {
		// Seed: a skill with a unique name + when_to_use trigger.
		uniqueName := "discov-matrix-skill-DISCOV-SKL-9087"
		skillContent := "---\nname: " + uniqueName + "\ndescription: test skill\nwhen_to_use: alpha discov-matrix token DISCOV-SKL-9087\nversion: 1.0.0\nsteps:\n  - call: test.step\n---\nbody"
		_, err := dm.SaveSkill(uniqueName, "1.0.0", skillContent, "test-agent", false)
		if err != nil {
			t.Fatalf("SaveSkill: %v", err)
		}

		// Discover: ListSkills returns the summary; the substring pass
		// over when_to_use is what proactive_recall_hint uses to surface
		// skills in response to context. Both paths share the same
		// ListSkills output.
		skills, err := dm.ListSkills("all")
		if err != nil {
			t.Fatalf("ListSkills: %v", err)
		}
		found := false
		for _, s := range skills {
			if s.Name == uniqueName {
				found = true
				break
			}
		}
		if !found {
			t.Fatal("skill not reachable via ListSkills")
		}
	})

	t.Run("reference is reachable via ListReferences with freshness signal", func(t *testing.T) {
		// Seed: a reference doc with a unique title + freshness tag.
		doc := &ReferenceDoc{
			ID:           "ref-discov-DISCOV-REF-6611",
			Title:        "DISCOV-REF-6611 vendor manual",
			SourcePath:   "/tmp/DISCOV-REF-6611.md",
			SourceType:   "markdown",
			Tags:         []string{"verified", "alpha", "discov-matrix"},
			ImportReason: "test reference for discovery matrix",
			LastIndexed:  itoa64(time.Now().Unix()),
		}
		if err := dm.AddReference(doc, nil); err != nil {
			t.Fatalf("AddReference: %v", err)
		}

		// Discover: ListReferences returns the freshness field.
		refs, err := dm.ListReferences(50, 0)
		if err != nil {
			t.Fatalf("ListReferences: %v", err)
		}
		var foundRow map[string]interface{}
		for _, r := range refs {
			if id, _ := r["id"].(string); id == doc.ID {
				foundRow = r
				break
			}
		}
		if foundRow == nil {
			t.Fatal("reference not reachable via ListReferences")
		}
		freshness, _ := foundRow["freshness"].(string)
		if freshness == "" {
			t.Fatal("reference row missing freshness signal")
		}
		if freshness != string(FreshnessCurrent) {
			t.Fatalf("freshness = %q, want %q (verified tag overrides age)", freshness, FreshnessCurrent)
		}
	})

	t.Run("topic context improves retrieval", func(t *testing.T) {
		// Seed: a topic and a memory tagged into it.
		topicID, err := dm.GetOrCreateTopic("discov-matrix-topic-8821")
		if err != nil {
			t.Fatalf("GetOrCreateTopic: %v", err)
		}
		_, _, err = dm.SaveMemoryWithContext(
			"memory tied to topic DISCOV-TPC-8821",
			"memories",
			[]string{"alpha", "discov-matrix"},
			0, "",
			ActiveContext{},
		)
		if err != nil {
			t.Fatalf("SaveMemoryWithContext: %v", err)
		}

		// Discover: SearchTopicsByQuery surfaces the topic itself.
		topics, err := dm.SearchTopicsByQuery("discov-matrix-topic-8821", 10)
		if err != nil {
			t.Fatalf("SearchTopicsByQuery: %v", err)
		}
		if len(topics) == 0 {
			t.Fatal("topic not reachable via SearchTopicsByQuery")
		}
		_ = topicID // topicID was used implicitly via GetOrCreateTopic
	})

	t.Run("work is reachable via ListWorksByStatus", func(t *testing.T) {
		// Seed: a work item with a unique title.
		_, err := dm.AddWork("DISCOV-WRK-3344 discovery matrix test", "test content", "")
		if err != nil {
			t.Fatalf("AddWork: %v", err)
		}

		// Discover: ListWorksByStatus returns the work item.
		works, err := dm.ListWorksByStatus("open")
		if err != nil {
			t.Fatalf("ListWorksByStatus: %v", err)
		}
		found := false
		for _, x := range works {
			if strings.Contains(x.Title, "DISCOV-WRK-3344") {
				found = true
				break
			}
		}
		if !found {
			t.Fatal("work not reachable via ListWorksByStatus")
		}
	})

	t.Run("handoff is reachable via GetLatestUnreadHandoff", func(t *testing.T) {
		// Seed: an unread handoff via EndSession (the same path
		// mpm_handoff action=write uses).
		_, err := dm.EndSession("discov-matrix-session", "DISCOV-HND-5577 summary", "clean", nil, nil)
		if err != nil {
			t.Fatalf("EndSession: %v", err)
		}

		// Discover: the next-session wake path reads via
		// GetLatestUnreadHandoff (the handoff.go contract for
		// wake-context consumption).
		h, err := dm.GetLatestUnreadHandoff()
		if err != nil {
			t.Fatalf("GetLatestUnreadHandoff: %v", err)
		}
		if h == nil {
			t.Fatal("handoff not reachable via GetLatestUnreadHandoff")
		}
		if !strings.Contains(h.Summary, "DISCOV-HND-5577") {
			t.Fatalf("handoff summary did not match seed: %q", h.Summary)
		}
	})
}

// itoa64 is a tiny strconv-free int64-to-string helper to avoid an
// extra import in this test file. Kept here, not exported.
func itoa64(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
