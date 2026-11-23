package assignment

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	internal "github.com/flowbyte-com/mpm-core"
)

// The PROJECT-CREATION operation.
//
// The project row is the containment boundary every assignment names. The
// schema existed; the supported way to create the row did not. These
// tests pin the operation's contract: operator-only, idempotent for a
// live identity, refusing a withdrawn one, atomic, and persistent.

func newProjectStore(t *testing.T) (*Store, *internal.DatabaseManager) {
	t.Helper()
	dm := internal.NewTestLocalOnlyDM(t)
	return NewStore(dm), dm
}

func projectID(seed string) string {
	// 16 lowercase hex characters, as the project model defines.
	return fmt.Sprintf("%s%016x", ProjectIDPrefix, []byte(seed)[0])
}

func countProjects(t *testing.T, dm *internal.DatabaseManager, id string) int {
	t.Helper()
	var n int
	if err := dm.SQLDB().QueryRow(
		`SELECT COUNT(*) FROM projects WHERE id = ?`, id).Scan(&n); err != nil {
		t.Fatalf("count the projects: %v", err)
	}
	return n
}

func TestCreateProjectIsOperatorOnly(t *testing.T) {
	store, _ := newProjectStore(t)
	if _, _, err := store.CreateProject(nil, ProjectParams{ID: projectID("a")}); !errors.Is(err, ErrNotAuthorised) {
		t.Errorf("project creation without an operator authority returned %v, want %v", err, ErrNotAuthorised)
	}
}

func TestCreateProjectIsIdempotent(t *testing.T) {
	store, dm := newProjectStore(t)
	id := projectID("a")

	first, created, err := store.CreateProject(NewLocalOperatorAuthority("operator"), ProjectParams{ID: id})
	if err != nil {
		t.Fatalf("create the project: %v", err)
	}
	if !created {
		t.Error("the first creation reported created=false")
	}
	if first.ID != id {
		t.Errorf("the project's id is %q, want %q", first.ID, id)
	}
	if first.CreatedBy != "operator" {
		t.Errorf("the project's created_by is %q, want the operator's attribution", first.CreatedBy)
	}

	second, created, err := store.CreateProject(NewLocalOperatorAuthority("operator"), ProjectParams{ID: id})
	if err != nil {
		t.Fatalf("repeat the creation: %v", err)
	}
	if created {
		t.Error("the repeated creation reported created=true")
	}
	if second.ID != first.ID || second.CreatedAt != first.CreatedAt {
		t.Errorf("the repeated request returned a different row: %+v vs %+v", second, first)
	}
	if n := countProjects(t, dm, id); n != 1 {
		t.Errorf("the projects table holds %d rows for %s, want 1", n, id)
	}
}

func TestCreateProjectRefusesAWithdrawnIdentity(t *testing.T) {
	store, dm := newProjectStore(t)
	id := projectID("b")
	if _, _, err := store.CreateProject(NewLocalOperatorAuthority("operator"), ProjectParams{ID: id}); err != nil {
		t.Fatalf("create the project: %v", err)
	}
	if _, err := dm.SQLDB().Exec(
		`UPDATE projects SET deleted_at = 1, deleted_by = 'operator' WHERE id = ?`, id); err != nil {
		t.Fatalf("withdraw the project: %v", err)
	}
	_, _, err := store.CreateProject(NewLocalOperatorAuthority("operator"), ProjectParams{ID: id})
	if !errors.Is(err, ErrConstraint) {
		t.Errorf("re-creating a withdrawn identity returned %v, want %v", err, ErrConstraint)
	}
	if n := countProjects(t, dm, id); n != 1 {
		t.Errorf("the projects table holds %d rows, want the withdrawn row only", n)
	}
}

func TestCreateProjectValidatesItsIdentity(t *testing.T) {
	store, _ := newProjectStore(t)
	cases := []string{
		"",
		"pilot",
		"cpj-",
		"cpj-0123456789abcde",    // 15 hex
		"cpj-0123456789abcdef0",  // 17 hex
		"cpj-0123456789ABCDEF",   // uppercase
		"cpj-0123456789abcdeg",   // not hex
		"cpj-0123456789abcde-",   // trailing separator
		"cpj-0123 456789abcdef",  // embedded space
		" cpj-0123456789abcdef",  // leading space
		"cpj-0123456789abcdef ",  // trailing space
		"\tcpj-0123456789abcdef", // leading tab
	}
	for _, id := range cases {
		if _, _, err := store.CreateProject(NewLocalOperatorAuthority("operator"), ProjectParams{ID: id}); err == nil {
			t.Errorf("the invalid identity %q was accepted", id)
		}
	}
}

func TestCreateProjectPersistsAcrossAServiceRestart(t *testing.T) {
	dm := internal.NewTestLocalOnlyDM(t)
	first := NewStore(dm)
	id := projectID("c")
	if _, _, err := first.CreateProject(NewLocalOperatorAuthority("operator"), ProjectParams{ID: id}); err != nil {
		t.Fatalf("create the project: %v", err)
	}

	// A NEW SERVICE over the SAME file-backed database is a restart of
	// the service, not of the database. The row must be there, and a
	// repeated request must still be idempotent.
	second := NewStore(dm)
	project, created, err := second.CreateProject(NewLocalOperatorAuthority("operator"), ProjectParams{ID: id})
	if err != nil {
		t.Fatalf("read the project after the restart: %v", err)
	}
	if project.ID != id {
		t.Errorf("the project after the restart is %q, want %q", project.ID, id)
	}
	if created {
		t.Error("the request after the restart reported created=true")
	}
}

func TestConcurrentProjectCreationCreatesOneRow(t *testing.T) {
	store, dm := newProjectStore(t)
	id := projectID("d")

	const writers = 8
	var wg sync.WaitGroup
	results := make(chan bool, writers)
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, created, err := store.CreateProject(
				NewLocalOperatorAuthority("operator"), ProjectParams{ID: id})
			if err != nil {
				errs <- err
				return
			}
			results <- created
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Errorf("a concurrent creation failed: %v", err)
	}
	created := 0
	for got := range results {
		if got {
			created++
		}
	}
	if created != 1 {
		t.Errorf("%d concurrent creators reported created=true, want exactly 1", created)
	}
	if n := countProjects(t, dm, id); n != 1 {
		t.Errorf("the projects table holds %d rows, want 1", n)
	}
}

func TestCreateProjectDoesNotTouchTheNameDomain(t *testing.T) {
	store, dm := newProjectStore(t)
	if _, _, err := store.CreateProject(NewLocalOperatorAuthority("operator"), ProjectParams{ID: projectID("e")}); err != nil {
		t.Fatalf("create the project: %v", err)
	}
	for _, table := range []string{"project_names", "continuity_threads", "continuity_checkpoints"} {
		var n int
		if err := dm.SQLDB().QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		if n != 0 {
			t.Errorf("the creation wrote %d %s rows; it must touch the projects row only", n, table)
		}
	}
}

// TestCreateProjectRefusesAnEmptyAuthorityActor is a guard on the
// boundary value itself: the local authority always records a name, so
// an empty created_by cannot come from this path.
func TestCreateProjectRecordsAWriterAttribution(t *testing.T) {
	store, dm := newProjectStore(t)
	id := projectID("f")
	if _, _, err := store.CreateProject(NewLocalOperatorAuthority(""), ProjectParams{ID: id}); err != nil {
		t.Fatalf("create the project: %v", err)
	}
	var createdBy string
	if err := dm.SQLDB().QueryRow(`SELECT created_by FROM projects WHERE id = ?`, id).Scan(&createdBy); err != nil {
		t.Fatalf("read created_by: %v", err)
	}
	// NewLocalOperatorAuthority substitutes "operator" for an empty
	// actor. It is a LABEL, not a credential; the test pins the
	// substitution so the attribution is never blank.
	if createdBy != "operator" {
		t.Errorf("the project's created_by is %q, want the authority's substitution", createdBy)
	}
}
