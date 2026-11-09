package update

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------
// The bundle is validated BEFORE the preflight is compiled
// ---------------------------------------------------------------------
//
// The schema-collision preflight COMPILES the candidate's reference
// DDL: it runs the file against an in-memory database and compares the
// shape it produces with the live table's. So the file is EXECUTED,
// and the order of the two steps is a security property, not a
// detail.

// TestThePromotionRefusesABundleBeforeItExecutesThePreflight is the
// end-to-end negative control.
//
// The preflight COMPILES the candidate's reference DDL. A preflight
// that carries an ATTACH therefore writes a FILE — outside the
// database, outside the bundle, wherever it names.
//
// The order is the fix: the bundle is validated BEFORE the preflight
// runs, so a bundle whose preflight is not one supported statement is
// refused before anything in it is executed. The proof is the file
// that must NOT exist afterwards.
func TestThePromotionRefusesABundleBeforeItExecutesThePreflight(t *testing.T) {
	side := filepath.Join(t.TempDir(), "side-effect.db")
	payload := fmt.Sprintf("ATTACH DATABASE '%s' AS side;\nCREATE TABLE side.payload(x);\nINSERT INTO side.payload VALUES (1);\n", side)

	oldRev := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	root := testLayout(t, oldRev)
	srcDir := t.TempDir()
	mustGit(t, srcDir)

	o := Options{
		DataRoot:  root,
		WantLocal: srcDir,
		Builder:   hostilePreflightBuilder{preflight: payload},
		Now:       func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
	_, err := Update(context.Background(), o)
	if err == nil {
		t.Fatal("the promotion accepted a bundle whose preflight is not one supported statement")
	}
	if !strings.Contains(err.Error(), "schema-preflight") {
		t.Errorf("the refusal does not name the schema-preflight check: %v", err)
	}
	if _, serr := os.Stat(side); !os.IsNotExist(serr) {
		t.Fatalf("the rejected preflight created %s — the bundle's SQL was executed before it was validated", side)
	}

	// And the installation is untouched: the pointer still names the
	// old release.
	if got := currentRevision(t, root); got != oldRev {
		t.Errorf("the failed promotion moved the pointer to %q", got)
	}
}

// TestTheDryRunRefusesABundleBeforeItExecutesThePreflight is the same
// control for the dry run, which reports what a promotion WOULD do.
func TestTheDryRunRefusesABundleBeforeItExecutesThePreflight(t *testing.T) {
	side := filepath.Join(t.TempDir(), "side-effect.db")
	payload := fmt.Sprintf("ATTACH DATABASE '%s' AS side;\nCREATE TABLE side.payload(x);\n", side)

	oldRev := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	root := testLayout(t, oldRev)
	srcDir := t.TempDir()
	mustGit(t, srcDir)

	o := Options{
		DataRoot:  root,
		WantLocal: srcDir,
		Builder:   hostilePreflightBuilder{preflight: payload},
		DryRun:    true,
		Now:       func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
	if _, err := Update(context.Background(), o); err == nil {
		t.Fatal("the dry run accepted a bundle whose preflight is not one supported statement")
	}
	if _, serr := os.Stat(side); !os.IsNotExist(serr) {
		t.Fatalf("the refused dry run created %s — the bundle's SQL was executed before it was validated", side)
	}
}

// TestThePreflightRefusesToCompileAnythingElse proves the second line
// of defence: preflight() itself will not compile a file that is not
// one supported statement, whatever the caller did.
func TestThePreflightRefusesToCompileAnythingElse(t *testing.T) {
	side := filepath.Join(t.TempDir(), "side-effect.db")
	payload := fmt.Sprintf("ATTACH DATABASE '%s' AS side;\nCREATE TABLE side.payload(x);\n", side)

	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "schema"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "schema", "lifecycle_events.sql"), []byte(payload), 0644); err != nil {
		t.Fatal(err)
	}
	// A data root with a live database, so the preflight gets as far
	// as the compile.
	root := t.TempDir()
	l := NewLayout(root)
	if err := os.MkdirAll(filepath.Dir(l.LiveDB()), 0700); err != nil {
		t.Fatal(err)
	}
	mkDB(t, l.LiveDB(), map[string]string{
		"memories": "CREATE TABLE memories (id TEXT PRIMARY KEY, content TEXT)",
	})

	err := preflight(dir, root)
	if err == nil {
		t.Fatal("the preflight compiled a file that is not one supported statement")
	}
	if _, serr := os.Stat(side); !os.IsNotExist(serr) {
		t.Fatalf("the preflight created %s — it compiled a file it must refuse", side)
	}
}

// hostilePreflightBuilder builds an otherwise valid bundle whose
// schema preflight is not one supported statement.
type hostilePreflightBuilder struct {
	fakeBuilder
	preflight string
}

func (b hostilePreflightBuilder) BuildFromCheckout(srcDir, dstDir string) (string, error) {
	commit, err := b.fakeBuilder.BuildFromCheckout(srcDir, dstDir)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dstDir, "schema", "lifecycle_events.sql"), []byte(b.preflight), 0644); err != nil {
		return "", err
	}
	return commit, nil
}
