// references_surface_test.go — the agent-facing mpm_references contract.
//
// Two gaps motivated these tests:
//
//  1. `mpm_references.add` only accepted `filepath` and a title. The CLI
//     could attach tags, an import reason and a chunk size; an agent could
//     not, so agent-imported documents were second-class in the corpus
//     they were supposed to enrich.
//  2. Search results carried no provenance — no source path or URL — so
//     citing a retrieved chunk required a second call, and an agent that
//     did not make that call could not say where the knowledge came from.
//
// Both are pinned here through the public handler, not the internals.
package tools

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"

	mpminternal "github.com/flowbyte-com/mpm-core"
)

const agentDocMarkdown = `# WordPress Menus

## wp_nav_menu()

Renders a navigation menu for a theme location.

## register_nav_menus()

Registers the menu locations a theme offers.
`

// newReferencesDM builds an isolated DatabaseManager for a handler test.
func newReferencesDM(t *testing.T) *mpminternal.DatabaseManager {
	t.Helper()
	mpminternal.PinIsolatedWorkspace(t)
	// File-backed, not ":memory:": database/sql pools connections and each
	// :memory: connection gets its own empty database, so a write on one
	// connection is invisible to the next (the reference tables "do not
	// exist" on the read connection).
	db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "mpm.db"))
	if err != nil {
		t.Fatalf("open in-memory db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	dm := mpminternal.NewDatabaseManagerForDB(db)
	if err := dm.InitSchema(); err != nil {
		t.Fatalf("init schema: %v", err)
	}
	return dm
}

func callReferences(t *testing.T, dm *mpminternal.DatabaseManager, payload map[string]interface{}) map[string]interface{} {
	t.Helper()
	out, err := handleMpmReferences(dm, mpminternal.ActiveContext{}, payload)
	if err != nil {
		t.Fatalf("mpm_references %v: %v", payload["action"], err)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	return m
}

// TestMpmReferences_AddCarriesTagsAndReason pins parity with the CLI: the
// agent surface can attach the same metadata.
func TestMpmReferences_AddCarriesTagsAndReason(t *testing.T) {
	dm := newReferencesDM(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "menus.md")
	if err := os.WriteFile(path, []byte(agentDocMarkdown), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	res := callReferences(t, dm, map[string]interface{}{
		"action": "add",
		"params": map[string]interface{}{
			"filepath": path,
			"tags":     []interface{}{"wordpress", "menus"},
			"reason":   "AgentShell development",
		},
	})
	if res["success"] != true {
		t.Fatalf("add failed: %v", res)
	}
	if res["status"] != mpminternal.ReferenceStatusImported {
		t.Errorf("status = %v, want imported", res["status"])
	}
	if res["source_kind"] != mpminternal.ReferenceSourceFile {
		t.Errorf("source_kind = %v, want file", res["source_kind"])
	}
	if res["title"] != "WordPress Menus" {
		t.Errorf("title = %v, want the document's own heading", res["title"])
	}

	read := callReferences(t, dm, map[string]interface{}{
		"action": "read",
		"params": map[string]interface{}{"id": res["id"]},
	})
	ref, _ := read["reference"].(map[string]interface{})
	if ref == nil {
		t.Fatalf("read returned no reference: %v", read)
	}
	if ref["import_reason"] != "AgentShell development" {
		t.Errorf("import_reason = %v", ref["import_reason"])
	}
	tags, _ := ref["tags"].(string)
	if !strings.Contains(tags, "wordpress") || !strings.Contains(tags, "menus") {
		t.Errorf("tags = %v, want both labels", ref["tags"])
	}
	// The source is reported as an absolute path, not the caller's string.
	if src, _ := ref["source_path"].(string); !filepath.IsAbs(src) {
		t.Errorf("source_path = %q, want an absolute canonical path", src)
	}
}

// TestMpmReferences_AddRejectsConflictingSources pins that a path plus a
// URL is an error, not a silent preference.
func TestMpmReferences_AddRejectsConflictingSources(t *testing.T) {
	dm := newReferencesDM(t)
	_, err := handleMpmReferences(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "add",
		"params": map[string]interface{}{
			"filepath": "/tmp/menus.md",
			"url":      "https://example.org/docs",
		},
	})
	if err == nil {
		t.Fatal("conflicting sources were accepted")
	}
	if !strings.Contains(err.Error(), "conflicting sources") {
		t.Errorf("error did not name the conflict: %v", err)
	}
}

// TestMpmReferences_AddRequiresASource pins the empty-source error, which
// is what an agent sees if it calls add with no parameters.
func TestMpmReferences_AddRequiresASource(t *testing.T) {
	dm := newReferencesDM(t)
	_, err := handleMpmReferences(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "add",
		"params": map[string]interface{}{},
	})
	if err == nil {
		t.Fatal("add with no source was accepted")
	}
	if !strings.Contains(err.Error(), "source is required") {
		t.Errorf("error did not say a source is required: %v", err)
	}
}

// TestMpmReferences_ImportReportsPerDocumentStatus pins the batch surface.
func TestMpmReferences_ImportReportsPerDocumentStatus(t *testing.T) {
	dm := newReferencesDM(t)
	dir := t.TempDir()
	a := filepath.Join(dir, "a.md")
	b := filepath.Join(dir, "b.md")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte(agentDocMarkdown), 0o644); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
	}

	res := callReferences(t, dm, map[string]interface{}{
		"action": "import",
		"params": map[string]interface{}{
			"documents": []interface{}{
				map[string]interface{}{"path": a, "tags": []interface{}{"wordpress"}},
				map[string]interface{}{"path": b},
				map[string]interface{}{"path": filepath.Join(dir, "missing.md")},
			},
		},
	})
	if res["total"].(float64) != 3 {
		t.Fatalf("total = %v, want 3", res["total"])
	}
	if res["imported"].(float64) != 2 || res["failed"].(float64) != 1 {
		t.Errorf("counts = %v, want 2 imported / 1 failed: %v", res, res)
	}
	if res["ok"] != false {
		t.Error("batch reported ok despite a failed document")
	}
	docs, _ := res["documents"].([]interface{})
	if len(docs) != 3 {
		t.Fatalf("documents = %d, want 3", len(docs))
	}
	last, _ := docs[2].(map[string]interface{})
	if last["status"] != "failed" || last["error"] == "" {
		t.Errorf("failed entry = %v, want status=failed with an error", last)
	}
}

// TestMpmReferences_SearchCarriesProvenance pins that a hit can be cited
// without a second call.
func TestMpmReferences_SearchCarriesProvenance(t *testing.T) {
	dm := newReferencesDM(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "menus.md")
	if err := os.WriteFile(path, []byte(agentDocMarkdown), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	callReferences(t, dm, map[string]interface{}{
		"action": "add",
		"params": map[string]interface{}{"filepath": path, "tags": []interface{}{"wordpress"}},
	})

	res := callReferences(t, dm, map[string]interface{}{
		"action": "search",
		"params": map[string]interface{}{"query": "render a navigation menu"},
	})
	results, _ := res["results"].([]interface{})
	if len(results) == 0 {
		t.Fatalf("search returned nothing: %v", res)
	}
	first, _ := results[0].(map[string]interface{})
	for _, key := range []string{"chunk_id", "doc_id", "doc_title", "section", "source_path", "source_kind", "relevance_score"} {
		if _, ok := first[key]; !ok {
			t.Errorf("search result is missing %q: %v", key, first)
		}
	}
	if src, _ := first["source_path"].(string); src != path {
		t.Errorf("source_path = %q, want the ingested path %q", src, path)
	}
	if first["source_kind"] != mpminternal.ReferenceSourceFile {
		t.Errorf("source_kind = %v, want file", first["source_kind"])
	}
}

// TestMpmReferences_UnknownActionPointsAtDiscovery pins the fix for the
// "mpm_references.help returned unknown action" dead end: the error names
// the discovery affordance instead of only listing actions.
func TestMpmReferences_UnknownActionPointsAtDiscovery(t *testing.T) {
	dm := newReferencesDM(t)
	_, err := handleMpmReferences(dm, mpminternal.ActiveContext{}, map[string]interface{}{
		"action": "help",
	})
	if err == nil {
		t.Fatal("unknown action was accepted")
	}
	msg := err.Error()
	if !strings.Contains(msg, "mpm_help") {
		t.Errorf("error does not point at mpm_help: %v", msg)
	}
	if !strings.Contains(msg, "add, import, read, search, list") {
		t.Errorf("error does not list the valid actions: %v", msg)
	}
}
