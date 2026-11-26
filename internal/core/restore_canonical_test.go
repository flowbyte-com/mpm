package internal

import (
	"strings"
	"testing"
)

// TestCanonicalTriggersMatchLiveSchema proves the normalizer is sound on
// the identity path: every managed trigger the migration installs,
// read back from sqlite_master, normalises to exactly the canonical
// inventory entry. If normalisation drifted (or the migration text
// drifted from what it installs), this fails before any dump is
// involved.
func TestCanonicalTriggersMatchLiveSchema(t *testing.T) {
	dm := NewTestLocalOnlyDM(t)
	inv, err := canonicalManagedTriggers()
	if err != nil {
		t.Fatalf("build the canonical inventory: %v", err)
	}
	if len(inv) == 0 {
		t.Fatal("the canonical inventory is empty")
	}
	rows, err := dm.SQLDB().Query(
		`SELECT name, sql FROM sqlite_master WHERE type = 'trigger'`)
	if err != nil {
		t.Fatalf("read sqlite_master: %v", err)
	}
	defer rows.Close()
	live := map[string]string{}
	for rows.Next() {
		var name, sql string
		if err := rows.Scan(&name, &sql); err != nil {
			t.Fatal(err)
		}
		live[name] = sql
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	missing := 0
	for name, want := range inv {
		gotSQL, ok := live[name]
		if !ok {
			t.Errorf("canonical trigger %q is not installed", name)
			missing++
			continue
		}
		if got := normalizeTriggerBody(gotSQL); got != want {
			t.Errorf("canonical trigger %q normalises differently:\n live: %.200s\n want: %.200s", name, got, want)
		}
	}
	if missing > 0 {
		t.Fatalf("%d canonical triggers missing from the live schema", missing)
	}
	t.Logf("identity holds for %d managed triggers", len(inv))
}

// TestTriggerNormalizationIgnoresBenignDifferences proves T116: quoting,
// schema qualification, IF NOT EXISTS, comments, whitespace and keyword
// case do not change the normalised form, while a changed check does.
func TestTriggerNormalizationIgnoresBenignDifferences(t *testing.T) {
	base := `CREATE TRIGGER trg_x BEFORE UPDATE OF state ON agent_tasks
FOR EACH ROW
WHEN NEW.state = 'completed' -- require completion
BEGIN
    SELECT RAISE(ABORT, 'nope');
END;`
	variants := []string{
		`create trigger if not exists trg_x before update of state on agent_tasks
		for each row
		when new.state='completed'
		begin select raise(abort,'nope'); end;`,
		`CREATE TRIGGER "main"."trg_x" BEFORE UPDATE OF [state] ON [agent_tasks]
		FOR EACH ROW WHEN NEW.[state] = 'completed' BEGIN SELECT RAISE(ABORT, 'nope'); END;`,
	}
	want := normalizeTriggerBody(base)
	for i, v := range variants {
		if got := normalizeTriggerBody(v); got != want {
			t.Fatalf("variant %d differs:\n got:  %s\n want: %s", i, got, want)
		}
	}
	altered := strings.Replace(base, `'completed'`, `'cancelled'`, 1)
	_ = altered
	weakened := `CREATE TRIGGER trg_x BEFORE UPDATE OF state ON agent_tasks
FOR EACH ROW
WHEN NEW.state = 'completed' OR TRUE
BEGIN
    SELECT RAISE(ABORT, 'nope');
END;`
	if normalizeTriggerBody(weakened) == want {
		t.Fatal("a weakened body normalises identically: the comparison would miss it")
	}
}
