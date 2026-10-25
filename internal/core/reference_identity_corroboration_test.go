// reference_identity_corroboration_test.go — a relative path is not
// evidence of identity on its own.
//
// Legacy rows store the string the caller typed. A relative path's meaning
// depends on a working directory that was never recorded, so resolving
// `docs/guide.md` against an ancestor of the imported file proves only
// that the imported file *could* have been the one written. On a machine
// with two projects laid out the same way —
//
//	~/alpha/docs/guide.md
//	~/beta/docs/guide.md
//
// — that resolution matches both, and adopting the legacy row rewrites its
// content and its provenance to point at a different file. The historical
// document is then gone: its text is replaced, its file_path now names a
// file it never came from, and the operator has no record that it happened.
//
// The rule these tests pin: a relative-path match is adopted only when the
// stored content hash corroborates that it is the same document. Content
// hashes are still not identity for *new* sources (two publishers may ship
// identical text); here the hash is a witness for an otherwise-ambiguous
// historical alias. When it does not corroborate, the legacy row is left
// alone, the import creates a new document, and the caller is told.
package internal

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

// TestIngestReference_LegacySuffixMatchDoesNotHijackAnotherFile is the RED
// case: importing /beta/docs/guide.md must not take over a legacy row that
// stores "docs/guide.md" for /alpha/docs/guide.md.
func TestIngestReference_LegacySuffixMatchDoesNotHijackAnotherFile(t *testing.T) {
	dm := NewTestDM(t)
	alpha := t.TempDir()
	beta := t.TempDir()

	const alphaBody = "# Guide A\n\nAlpha content, from project A.\n"
	writeTestDoc(t, filepath.Join(alpha, "docs"), "guide.md", alphaBody)
	betaPath := writeTestDoc(t, filepath.Join(beta, "docs"), "guide.md", "# Guide B\n\nBeta content, entirely different.\n")

	// The legacy row, written by a pre-identity binary from alpha.
	insertLegacyReferenceDoc(t, dm, "legacyhijack001", "docs/guide.md", alphaBody)

	res, err := dm.IngestReference(context.Background(), ReferenceIngestRequest{Source: betaPath})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.ID == "legacyhijack001" {
		t.Errorf("a different file was adopted by the legacy row (status=%s): provenance was silently redirected", res.Status)
	}
	if res.Status != ReferenceStatusImported {
		t.Errorf("status = %q, want imported (the legacy row is not this file)", res.Status)
	}

	var storedPath, storedContent string
	if err := dm.SQLDB().QueryRow(
		`SELECT file_path, content FROM reference_docs WHERE id='legacyhijack001'`,
	).Scan(&storedPath, &storedContent); err != nil {
		t.Fatalf("read legacy row: %v", err)
	}
	if !strings.Contains(storedContent, "Alpha content") {
		t.Errorf("legacy document content was overwritten: %q", strings.SplitN(storedContent, "\n", 3)[0])
	}
	if storedPath != "docs/guide.md" {
		t.Errorf("legacy file_path was rewritten to %q; a document may not be repointed at another file", storedPath)
	}
	if n := referenceDocCount(t, dm); n != 2 {
		t.Errorf("documents = %d, want 2 (the legacy document plus the new one)", n)
	}
	if len(res.Warnings) == 0 {
		t.Error("the rejected near-match was not disclosed to the caller")
	} else if !strings.Contains(strings.Join(res.Warnings, " "), "legacyhijack001") {
		t.Errorf("warning does not name the rejected legacy row: %v", res.Warnings)
	}
}

// TestIngestReference_LegacyRelativeMatchWithSameContentIsAdopted pins the
// behaviour the corroboration rule must preserve: the same file, written by
// an older binary under a relative path, is still recognised and upgraded
// in place rather than duplicated.
func TestIngestReference_LegacyRelativeMatchWithSameContentIsAdopted(t *testing.T) {
	dm := NewTestDM(t)
	corpus := t.TempDir()
	body := "# Menus\n\nNavigation menu documentation.\n"
	path := writeTestDoc(t, filepath.Join(corpus, "docs"), "menus.md", body)

	insertLegacyReferenceDoc(t, dm, "legacysame00001", "docs/menus.md", body)

	res, err := dm.IngestReference(context.Background(), ReferenceIngestRequest{Source: path})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.ID != "legacysame00001" {
		t.Fatalf("same file under a legacy relative path produced %s instead of adopting legacysame00001", res.ID)
	}
	if res.Status != ReferenceStatusUnchanged {
		t.Errorf("status = %q, want unchanged", res.Status)
	}
	var storedPath string
	if err := dm.SQLDB().QueryRow(`SELECT file_path FROM reference_docs WHERE id='legacysame00001'`).Scan(&storedPath); err != nil {
		t.Fatalf("read path: %v", err)
	}
	if storedPath != path {
		t.Errorf("legacy row was not upgraded to the canonical path: %q", storedPath)
	}
	if n := referenceDocCount(t, dm); n != 1 {
		t.Errorf("documents = %d, want 1", n)
	}
}

// TestIngestReference_AmbiguousRelativeRowsAreNotAdoptedWithoutEvidence
// pins the same rule across several plausible legacy rows: when the content
// does not corroborate any of them, none is adopted.
func TestIngestReference_AmbiguousRelativeRowsAreNotAdoptedWithoutEvidence(t *testing.T) {
	dm := NewTestDM(t)
	corpus := t.TempDir()
	path := writeTestDoc(t, filepath.Join(corpus, "docs"), "menus.md", "# Menus\n\nNew body.\n")

	insertLegacyReferenceDoc(t, dm, "legacyother0001", "menus.md", "# Menus\n\nAn older, different body.\n")
	insertLegacyReferenceDoc(t, dm, "legacyother0002", "docs/menus.md", "# Menus\n\nYet another body.\n")

	res, err := dm.IngestReference(context.Background(), ReferenceIngestRequest{Source: path})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.Status != ReferenceStatusImported {
		t.Fatalf("status = %q, want imported: neither legacy row's content matches", res.Status)
	}
	joined := strings.Join(res.Warnings, " ")
	if !strings.Contains(joined, "legacyother0001") || !strings.Contains(joined, "legacyother0002") {
		t.Errorf("warnings do not name both rejected rows: %v", res.Warnings)
	}
	if n := referenceDocCount(t, dm); n != 3 {
		t.Errorf("documents = %d, want 3 (two legacy rows untouched + the new one)", n)
	}
}

// TestIngestReference_AbsoluteLegacyPathStillAdoptsOnEdit pins that the
// corroboration requirement is scoped to relative paths. An absolute stored
// path is decisive regardless of content, so editing a legacy file and
// re-importing still updates in place.
func TestIngestReference_AbsoluteLegacyPathStillAdoptsOnEdit(t *testing.T) {
	dm := NewTestDM(t)
	corpus := t.TempDir()
	path := writeTestDoc(t, corpus, "guide.md", "# Guide\n\nEdited body.\n")

	// Legacy row with an absolute path and older content.
	insertLegacyReferenceDoc(t, dm, "legacyabs000001", path, "# Guide\n\nOriginal body.\n")

	res, err := dm.IngestReference(context.Background(), ReferenceIngestRequest{Source: path})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.ID != "legacyabs000001" {
		t.Fatalf("absolute legacy path was not adopted on edit; got %s", res.ID)
	}
	if res.Status != ReferenceStatusUpdated {
		t.Errorf("status = %q, want updated", res.Status)
	}
}
