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

// TestTriggerNormalisationEquivalencesIsT110T116 proves B1's benign
// differences compare EQUAL: identifier quoting, schema qualification,
// comments, whitespace, and whitespace around multi-character operators
// such as `||`.
func TestTriggerNormalisationEquivalencesIsT110T116(t *testing.T) {
	base := `CREATE TRIGGER trg_example BEFORE INSERT ON agent_tasks
FOR EACH ROW
WHEN (NEW.idempotency_key GLOB 'supersede:*')
BEGIN
    SELECT RAISE(ABORT, 'reserved');
END;`
	want := normalizeTriggerBody(base)
	cases := []struct{ name, sql string }{
		{"quoted name", `CREATE TRIGGER "trg_example" BEFORE INSERT ON agent_tasks
FOR EACH ROW WHEN (NEW.idempotency_key GLOB 'supersede:*')
BEGIN SELECT RAISE(ABORT, 'reserved'); END;`},
		{"schema-qualified quoted name", `CREATE TRIGGER "main"."trg_example" BEFORE INSERT ON agent_tasks
FOR EACH ROW WHEN (NEW.idempotency_key GLOB 'supersede:*')
BEGIN SELECT RAISE(ABORT, 'reserved'); END;`},
		{"bracket quoting", `CREATE TRIGGER [main].[trg_example] BEFORE INSERT ON [agent_tasks]
FOR EACH ROW WHEN (NEW.idempotency_key GLOB 'supersede:*')
BEGIN SELECT RAISE(ABORT, 'reserved'); END;`},
		{"if not exists", `CREATE TRIGGER IF NOT EXISTS trg_example BEFORE INSERT ON agent_tasks
FOR EACH ROW WHEN (NEW.idempotency_key GLOB 'supersede:*')
BEGIN SELECT RAISE(ABORT, 'reserved'); END;`},
		{"comments", `-- a leading comment
CREATE TRIGGER trg_example /* inline */ BEFORE INSERT ON agent_tasks
FOR EACH ROW WHEN (NEW.idempotency_key GLOB 'supersede:*') -- trailing
BEGIN
    SELECT RAISE(ABORT, 'reserved');
END;`},
		{"whitespace", "CREATE   TRIGGER\ntrg_example\tBEFORE  INSERT   ON agent_tasks\nFOR EACH ROW\nWHEN (NEW.idempotency_key GLOB 'supersede:*')\nBEGIN\n  SELECT RAISE(ABORT,'reserved');\nEND ;"},
	}
	for _, c := range cases {
		if got := normalizeTriggerBody(c.sql); got != want {
			t.Errorf("%s normalised to\n  %s\nwant\n  %s", c.name, got, want)
		}
	}
}

// TestTriggerNormalisationOperatorWhitespaceIsT116 pins the multi-
// character operator case specifically: `a || b` and `a||b` are the same
// expression, and the normaliser must not treat the spacing as
// significant.
func TestTriggerNormalisationOperatorWhitespaceIsT116(t *testing.T) {
	spaced := `CREATE TRIGGER trg_op BEFORE INSERT ON agent_tasks FOR EACH ROW
WHEN ('x' || NEW.id = 'y') BEGIN SELECT RAISE(ABORT, 'n'); END;`
	tight := `CREATE TRIGGER trg_op BEFORE INSERT ON agent_tasks FOR EACH ROW
WHEN ('x'||NEW.id='y') BEGIN SELECT RAISE(ABORT, 'n'); END;`
	if normalizeTriggerBody(spaced) != normalizeTriggerBody(tight) {
		t.Fatalf("spacing around || changed the meaning:\n  %s\n  %s",
			normalizeTriggerBody(spaced), normalizeTriggerBody(tight))
	}
}

// TestTriggerNormalisationKeepsMeaning proves the normaliser does not
// equate SEMANTICALLY DIFFERENT definitions: string literals are
// preserved byte for byte, and different operators stay different.
func TestTriggerNormalisationKeepsMeaning(t *testing.T) {
	different := [][2]string{
		{`SELECT RAISE(ABORT, 'a b');`, `SELECT RAISE(ABORT, 'a  b');`},
		{`SELECT RAISE(ABORT, 'Upper');`, `SELECT RAISE(ABORT, 'upper');`},
		{`WHEN (NEW.id = 'x')`, `WHEN (NEW.id <> 'x')`},
		{`WHEN (NEW.id = 'x')`, `WHEN (NEW.id >= 'x')`},
		{`SELECT 'a' || 'b';`, `SELECT 'a' | 'b';`},
		{`WHEN (NEW.a = 1)`, `WHEN (NEW.b = 1)`},
	}
	for _, pair := range different {
		a := normalizeTriggerBody(`CREATE TRIGGER t BEFORE INSERT ON agent_tasks FOR EACH ROW BEGIN ` + pair[0] + ` END;`)
		b := normalizeTriggerBody(`CREATE TRIGGER t BEFORE INSERT ON agent_tasks FOR EACH ROW BEGIN ` + pair[1] + ` END;`)
		if a == b {
			t.Errorf("the normaliser equated different definitions:\n  %s\n  %s", pair[0], pair[1])
		}
	}
}

// TestParseTriggerTargetUsesTheHeaderIsF11 proves the target is read
// from the validated statement header, not guessed from the body: a
// trigger on an unrelated table whose body merely reads a managed table
// is NOT a managed-table trigger.
func TestParseTriggerTargetUsesTheHeaderIsF11(t *testing.T) {
	cases := []struct{ name, sql, want string }{
		{"bare", `CREATE TRIGGER t AFTER INSERT ON memories BEGIN SELECT 1; END;`, "memories"},
		{"quoted", `CREATE TRIGGER t AFTER INSERT ON "memories" BEGIN SELECT 1; END;`, "memories"},
		{"schema-qualified", `CREATE TRIGGER t AFTER INSERT ON "main"."memories" BEGIN SELECT 1; END;`, "memories"},
		{"bracketed", `CREATE TRIGGER t AFTER INSERT ON [memories] BEGIN SELECT 1; END;`, "memories"},
		{"managed target", `CREATE TRIGGER t BEFORE UPDATE ON agent_tasks BEGIN SELECT 1; END;`, "agent_tasks"},
		{"body mentions managed", `CREATE TRIGGER t AFTER INSERT ON memories BEGIN
			INSERT INTO memories_fts(rowid, content) SELECT new.rowid, agent_tasks.title FROM agent_tasks; END;`, "memories"},
	}
	for _, c := range cases {
		got, ok := parseTriggerTarget(c.sql)
		if !ok || got != c.want {
			t.Errorf("%s: parseTriggerTarget=%q ok=%v, want %q", c.name, got, ok, c.want)
		}
	}
}

// TestTriggerHeaderParsingIsTokenAwareIsF13 proves the header is resolved
// from TOKENS, so a name, a quoted identifier or a string literal that
// contains BEGIN cannot truncate it, and an unparseable header is
// reported as unknown rather than guessed.
func TestTriggerHeaderParsingIsTokenAwareIsF13(t *testing.T) {
	cases := []struct {
		name       string
		sql        string
		wantName   string
		wantTarget string
		wantOK     bool
	}{
		{
			"name containing BEGIN",
			`CREATE TRIGGER trg_begin_shadow BEFORE INSERT ON agent_tasks
			 FOR EACH ROW BEGIN SELECT 1; END;`,
			"trg_begin_shadow", "agent_tasks", true,
		},
		{
			"quoted name containing BEGIN",
			`CREATE TRIGGER "trg_begin_shadow" BEFORE INSERT ON agent_tasks
			 FOR EACH ROW BEGIN SELECT 1; END;`,
			"trg_begin_shadow", "agent_tasks", true,
		},
		{
			"schema-qualified quoted name containing BEGIN",
			`CREATE TRIGGER "main"."trg_begin_shadow" BEFORE INSERT ON "main"."agent_tasks"
			 FOR EACH ROW BEGIN SELECT 1; END;`,
			"trg_begin_shadow", "agent_tasks", true,
		},
		{
			"name and target containing ON and BEGIN",
			`CREATE TRIGGER trg_on_begin BEFORE UPDATE OF title ON agent_tasks
			 FOR EACH ROW BEGIN SELECT 1; END;`,
			"trg_on_begin", "agent_tasks", true,
		},
		{
			"string literal containing BEGIN in the WHEN clause",
			`CREATE TRIGGER t AFTER INSERT ON projects FOR EACH ROW
			 WHEN (NEW.created_by = 'BEGIN') BEGIN SELECT 1; END;`,
			"t", "projects", true,
		},
		{
			"unrelated trigger reading a managed table",
			`CREATE TRIGGER t AFTER INSERT ON projects BEGIN
			 INSERT INTO memories_fts(rowid) SELECT new.rowid FROM agent_tasks; END;`,
			"t", "projects", true,
		},
		{
			"no ON clause",
			`CREATE TRIGGER t BEFORE INSERT agent_tasks BEGIN SELECT 1; END;`,
			"t", "", false,
		},
		{
			"unterminated quoted name",
			`CREATE TRIGGER "t BEFORE INSERT ON agent_tasks BEGIN SELECT 1; END;`,
			"", "", false,
		},
		{
			"unterminated quoted target",
			`CREATE TRIGGER t BEFORE INSERT ON "agent_tasks BEGIN SELECT 1; END;`,
			"t", "", false,
		},
		{
			"not a trigger",
			`CREATE TABLE t (a TEXT);`,
			"", "", false,
		},
	}
	for _, c := range cases {
		name, target, ok := parseTriggerHeader(c.sql)
		if ok != c.wantOK {
			t.Errorf("%s: ok=%v, want %v (name=%q target=%q)", c.name, ok, c.wantOK, name, target)
			continue
		}
		if !ok {
			continue
		}
		if name != c.wantName || target != c.wantTarget {
			t.Errorf("%s: got (%q,%q), want (%q,%q)", c.name, name, target, c.wantName, c.wantTarget)
		}
	}
}

// TestTriggerHeaderNameMatchesStatementParser pins the two parsers to
// the same name: the canonical inventory keys and the classification
// map must agree for every statement shape.
func TestTriggerHeaderNameMatchesStatementParser(t *testing.T) {
	for _, sql := range []string{
		`CREATE TRIGGER trg_x BEFORE INSERT ON agent_tasks BEGIN SELECT 1; END;`,
		`CREATE TRIGGER "main"."trg_x" BEFORE INSERT ON agent_tasks BEGIN SELECT 1; END;`,
		`CREATE TRIGGER IF NOT EXISTS [trg_begin_y] BEFORE INSERT ON agent_tasks BEGIN SELECT 1; END;`,
	} {
		name, _, ok := parseTriggerHeader(sql)
		if !ok {
			t.Fatalf("header parse failed for %q", sql)
		}
		stmtName, _, err := parseTriggerStatement(sql)
		if err != nil {
			t.Fatalf("statement parse failed for %q: %v", sql, err)
		}
		if name != stmtName {
			t.Fatalf("parsers disagree: header=%q statement=%q", name, stmtName)
		}
	}
}
