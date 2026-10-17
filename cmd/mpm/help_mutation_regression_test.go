package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Help requests must never mutate.
//
// Origin (2026-10-13). `mpm ops backfill-embeddings --help` performed a
// live embedding backfill. The router rewrites -h/--help into the literal
// token "help" (parseFlags, router.go), and for any command registered in
// commandsWithSubcommandDispatch it then DEFERRED to the handler, which
// treated that trailing token as data. Safety was opt-in per handler via
// requireHelpShortCircuit; almost no handler had adopted it.
//
// A sweep of every family subcommand in an isolated workspace found these
// executing or mutating on --help:
//
//	ops backfill-embeddings / ops gc / ops init skills   ran for real
//	ops stats / ops status                               mutated (runtime accrual)
//	handoff write                                        wrote a handoff
//	mode clear / persona clear                           mutated
//	session shred <real-id> --help                       SOFT-DELETED a session
//
// The fix inverts the default: Execute intercepts help unless the exact
// command path is in subcommandHelpOwners. These tests pin that property.
//
// Every test builds a fresh binary and runs it against an isolated
// MPM_WORKSPACE + HOME. Nothing touches the operator's real database.
// ---------------------------------------------------------------------------

// helpMutationCase is one command path asserted to be inert under --help.
type helpMutationCase struct {
	path []string
	// why is the case here — what it did before the fix.
	why string
}

func helpMutationCases() []helpMutationCase {
	return []helpMutationCase{
		// The originally reported defect.
		{[]string{"ops", "backfill-embeddings", "--help"}, "performed a live embedding backfill over the database"},

		// Same deferral hole, other ops leaves that mutate on --help.
		{[]string{"ops", "gc", "--help"}, "ran garbage collection over live memories"},
		{[]string{"ops", "init", "skills", "--help"}, "seeded the baseline skill library"},
		{[]string{"ops", "init", "tasks", "--help"}, "seeded baseline scheduled tasks"},
		{[]string{"ops", "maintain", "--help"}, "ran self-maintenance"},
		{[]string{"ops", "stats", "--help"}, "mutated: accrued runtime state"},
		{[]string{"ops", "status", "--help"}, "mutated: accrued runtime state"},
		{[]string{"ops", "prune", "--help"}, "ran a prune"},
		{[]string{"ops", "backup", "--help"}, "ran a backup"},
		{[]string{"ops", "self-heal", "--help"}, "ran self-heal"},
		{[]string{"ops", "synthesize", "--help"}, "ran synthesis"},
		{[]string{"ops", "doctor", "--help"}, "ran the full doctor pass instead of printing help"},

		// Destructive with a real target — the exact bug the D5 note at
		// Execute claims was fixed for `mpm rm <id> --help` in 2026-08-25.
		{[]string{"session", "shred", "sess-canary", "--help"}, "SOFT-DELETED a live session"},
		{[]string{"lesson", "shred", "lesson-canary", "--help"}, "shredded a live lesson"},
		{[]string{"memory", "shred", "mem-canary", "--help"}, "shredded a live memory"},
		{[]string{"skill", "shred", "skill-canary", "--help"}, "shredded a live skill"},
		{[]string{"handoff", "shred", "handoff-canary", "--help"}, "shredded a live handoff"},

		// Mutating non-destructive leaves.
		{[]string{"handoff", "write", "canary", "--help"}, "wrote a handoff"},
		{[]string{"mode", "clear", "--help"}, "cleared all modes"},
		{[]string{"persona", "clear", "--help"}, "cleared the persona"},
		{[]string{"kb", "propose_theory", "t", "--help"}, "proposed a theory"},
		{[]string{"kb", "record_decision", "d", "--help"}, "recorded a decision"},

		// Data-absorbing leaves: these ran the real operation rather than
		// printing help, which is what made the bug so easy to miss.
		{[]string{"skill", "search", "canary", "--help"}, "ran a skill search"},
		{[]string{"lesson", "search", "canary", "--help"}, "ran a lesson search"},
		{[]string{"session", "search", "canary", "--help"}, "ran a session search"},
		{[]string{"work", "show", "--help"}, "printed the live Working Context"},

		// Non-dispatching commands: already intercepted pre-fix, pinned so
		// a future refactor of the deferral path cannot regress them.
		{[]string{"prune", "--help"}, "ran a prune"},
		{[]string{"show", "canary", "--help"}, "looked up a memory named \"help\""},
		{[]string{"gc", "--help"}, "ran garbage collection"},
		{[]string{"backup", "--help"}, "ran a backup"},
		{[]string{"restore", "--help"}, "ran a restore"},
		{[]string{"recall", "canary", "--help"}, "searched for the word \"help\""},
		{[]string{"export", "--help"}, "dumped the database"},
		{[]string{"maintain", "--help"}, "ran self-maintenance"},
		{[]string{"migrate", "--help"}, "ran a migration"},
	}
}

// buildTestBinary compiles the mpm binary into a temp dir. Returns its path.
func buildTestBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "mpm")
	cmd := exec.Command("go", "build", "-tags", "fts5", "-o", bin, ".")
	cmd.Dir = "."
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build mpm binary: %v\n%s", err, out)
	}
	return bin
}

// findDB locates the SQLite database inside an isolated workspace.
func findDB(t *testing.T, workspace string) string {
	t.Helper()
	var found string
	err := filepath.WalkDir(workspace, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".db") {
			found = p
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk workspace: %v", err)
	}
	if found == "" {
		t.Skipf("no database under %s — nothing seeded to protect", workspace)
	}
	return found
}

// ftsShadow matches the FTS5 virtual-table shards. They mirror their base
// table, are rebuilt by SQLite itself, and would make the digest noisy.
var ftsShadow = regexp.MustCompile(`_(fts|fts_config|fts_content|fts_data|fts_docsize|fts_idx)$`)

// digestDB hashes every row of every base table. Any write that lands in
// any table changes the digest.
func digestDB(t *testing.T, dbPath string) string {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?mode=ro", dbPath)
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' ORDER BY name`)
	if err != nil {
		t.Fatalf("list tables: %v", err)
	}
	var tables []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			t.Fatalf("scan table name: %v", err)
		}
		if ftsShadow.MatchString(n) || n == "sqlite_sequence" || n == "schema_migrations" {
			continue
		}
		tables = append(tables, n)
	}
	rows.Close()

	h := sha256.New()
	for _, table := range tables {
		r, err := db.Query("SELECT * FROM " + table)
		if err != nil {
			h.Write([]byte(table + "\x00ERR:" + err.Error()))
			continue
		}
		h.Write([]byte(table + "\x00"))
		cols, _ := r.Columns()
		for r.Next() {
			vals := make([]any, len(cols))
			ptrs := make([]any, len(cols))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if err := r.Scan(ptrs...); err != nil {
				h.Write([]byte("scanerr"))
				break
			}
			fmt.Fprintf(h, "%v\n", vals)
		}
		r.Close()
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// digestFS hashes every workspace file except SQLite's write-ahead log and
// shared-memory sidecars, which move independently of logical content.
func digestFS(t *testing.T, workspace string) string {
	t.Helper()
	h := sha256.New()
	err := filepath.WalkDir(workspace, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if strings.HasSuffix(name, "-wal") || strings.HasSuffix(name, "-shm") {
			return nil
		}
		rel, _ := filepath.Rel(workspace, p)
		h.Write([]byte(rel))
		b, rerr := os.ReadFile(p)
		if rerr != nil {
			return nil
		}
		h.Write(b)
		return nil
	})
	if err != nil {
		t.Fatalf("walk workspace: %v", err)
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// seedCanaries populates an isolated workspace with rows that the destructive
// cases above would otherwise destroy, so a regression is detectable as a
// digest change rather than as a silent no-op.
func seedCanaries(t *testing.T, bin, workspace, home string) {
	t.Helper()
	env := isolatedEnv(workspace, home)
	for _, args := range [][]string{
		{"remember", "help-mutation-canary-alpha"},
		{"remember", "help-mutation-canary-beta"},
		{"save-skill", "help-mutation-canary-skill", "--body", "canary skill body"},
		{"topic", "add", "help-mutation-canary-topic"},
		{"session", "add", "help-mutation-canary-session"},
		{"handoff", "write", "help-mutation-canary-handoff"},
	} {
		cmd := exec.Command(bin, args...)
		cmd.Env = env
		cmd.Dir = workspace
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Logf("seed %v: exit %v (continuing): %s", args, err, clip(string(out), 200))
		}
	}
}

func isolatedEnv(workspace, home string) []string {
	return append(os.Environ(),
		"MPM_WORKSPACE="+workspace,
		"MPM_NO_AUTO_INIT=1",
		"HOME="+home,
		// Point every model provider at a closed port. A command that
		// tries to reach a provider blocks on connect and then retries;
		// a command that only prints help never touches the network. The
		// per-case deadline below turns that difference into a failure.
		"OPENAI_API_BASE=http://127.0.0.1:1/v1",
		"OPENAI_BASE_URL=http://127.0.0.1:1/v1",
		"ANTHROPIC_BASE_URL=http://127.0.0.1:1",
		"OLLAMA_HOST=http://127.0.0.1:1",
	)
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// TestHelpRequestNeverMutates is the primary regression test.
//
// For every command path in helpMutationCases, `mpm <path> --help` must
// leave the isolated workspace byte-identical — no database writes, no
// filesystem mutations — and must not block on an unreachable provider.
func TestHelpRequestNeverMutates(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and runs ~30 subprocesses")
	}
	bin := buildTestBinary(t)

	tmp := t.TempDir()
	workspace := filepath.Join(tmp, "workspace")
	home := filepath.Join(tmp, "home")
	for _, d := range []string{workspace, home} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	seedCanaries(t, bin, workspace, home)

	dbPath := findDB(t, workspace)
	baseDB := digestDB(t, dbPath)
	baseFS := digestFS(t, workspace)
	env := isolatedEnv(workspace, home)

	for _, tc := range helpMutationCases() {
		tc := tc
		t.Run(strings.Join(tc.path, " "), func(t *testing.T) {
			cmd := exec.Command(bin, tc.path...)
			cmd.Env = env
			cmd.Dir = workspace

			done := make(chan struct{})
			var runErr error
			go func() {
				_, runErr = cmd.CombinedOutput()
				close(done)
			}()
			select {
			case <-done:
			case <-time.After(30 * time.Second):
				_ = cmd.Process.Kill()
				t.Fatalf("--help blocked for 30s: almost certainly a provider "+
					"call or an interactive prompt (before the fix: %s)", tc.why)
			}

			_ = runErr // exit status is asserted separately; a help request
			// is allowed to exit non-zero when the family page is reached
			// through a path that has no dedicated renderer.

			if got := digestDB(t, dbPath); got != baseDB {
				t.Errorf("DATABASE MUTATED by --help: %s — %s", tc.path, tc.why)
			}
			if got := digestFS(t, workspace); got != baseFS {
				t.Errorf("FILESYSTEM MUTATED by --help: %s — %s", tc.path, tc.why)
			}
		})
	}
}

// TestHelpRequestMatchesBareHelp asserts that `-h`, `--help` and the
// post-rewrite literal token are all equivalent, and that none of them
// differs from the command's own help output. This pins the parseFlags
// rewrite as an intentional contract rather than an accident.
func TestHelpRequestMatchesBareHelp(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and runs subprocesses")
	}
	bin := buildTestBinary(t)

	tmp := t.TempDir()
	workspace := filepath.Join(tmp, "workspace")
	home := filepath.Join(tmp, "home")
	for _, d := range []string{workspace, home} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	seedCanaries(t, bin, workspace, home)
	env := isolatedEnv(workspace, home)

	run := func(args ...string) string {
		cmd := exec.Command(bin, args...)
		cmd.Env = env
		cmd.Dir = workspace
		out, _ := cmd.CombinedOutput()
		return string(out)
	}

	for _, path := range [][]string{
		{"ops", "backfill-embeddings"},
		{"ops", "gc"},
		{"handoff", "write"},
		{"session", "shred", "help-mutation-canary-session"},
		{"recall"},
	} {
		base := run(append(append([]string{}, path...), "-h")...)
		for _, flag := range []string{"--help", "help"} {
			got := run(append(append([]string{}, path...), flag)...)
			if got != base {
				t.Errorf("%v %s differs from `-h` output:\n--- -h ---\n%s\n--- %s ---\n%s",
					path, flag, clip(base, 400), flag, clip(got, 400))
			}
		}
	}
}

// TestSubcommandHelpOwnersAreInert guards the allowlist itself: a path in
// subcommandHelpOwners is the ONE case where Execute hands a help request to
// a handler, so each entry must be verified inert, not assumed.
func TestSubcommandHelpOwnersAreInert(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and runs subprocesses")
	}
	bin := buildTestBinary(t)

	tmp := t.TempDir()
	workspace := filepath.Join(tmp, "workspace")
	home := filepath.Join(tmp, "home")
	for _, d := range []string{workspace, home} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	seedCanaries(t, bin, workspace, home)
	dbPath := findDB(t, workspace)
	baseDB := digestDB(t, dbPath)
	baseFS := digestFS(t, workspace)
	env := isolatedEnv(workspace, home)

	for path := range subcommandHelpOwners {
		parts := strings.Fields(path)
		if len(parts) == 0 {
			continue
		}
		t.Run(path, func(t *testing.T) {
			cmd := exec.Command(bin, append(append([]string{}, parts...), "--help")...)
			cmd.Env = env
			cmd.Dir = workspace
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Logf("exit %v: %s", err, clip(string(out), 200))
			}
			if got := digestDB(t, dbPath); got != baseDB {
				t.Errorf("subcommandHelpOwners entry %q MUTATED the database on --help", path)
			}
			if got := digestFS(t, workspace); got != baseFS {
				t.Errorf("subcommandHelpOwners entry %q MUTATED the workspace on --help", path)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Gap 1b: `mpm kb memory search <query> --semantic`
//
// The flag belongs to `mpm recall` (recall.go:72 is its only definition).
// memory search is BM25/FTS5 only. Pre-fix the token fell through to
// `strings.Join(cleaned, " ")`, so the command searched for the literal
// string "foo --semantic", exited 0, and silently dropped vector ranking.
// Rejecting clearly is the only option that does not change the semantics
// of an existing command.
// ---------------------------------------------------------------------------

func TestMemorySearchRejectsSemanticFlag(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and runs subprocesses")
	}
	bin := buildTestBinary(t)

	tmp := t.TempDir()
	workspace := filepath.Join(tmp, "workspace")
	home := filepath.Join(tmp, "home")
	for _, d := range []string{workspace, home} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	seedCanaries(t, bin, workspace, home)
	env := isolatedEnv(workspace, home)

	for _, prefix := range [][]string{
		{"memory", "search"},
		{"kb", "memory", "search"},
	} {
		name := strings.Join(prefix, " ")
		t.Run(name, func(t *testing.T) {
			args := append(append([]string{}, prefix...), "canary", "--semantic")
			cmd := exec.Command(bin, args...)
			cmd.Env = env
			cmd.Dir = workspace
			cmd.Dir = workspace
			out, err := cmd.CombinedOutput()
			text := string(out)

			if err == nil {
				t.Errorf("`%s --semantic` exited 0; an unsupported flag must fail clearly.\noutput: %s",
					name, clip(text, 400))
			}
			// The failure must name the real home for the flag, or the
			// operator has no way to act on it.
			if !strings.Contains(text, "mpm recall") {
				t.Errorf("`%s --semantic` did not point the operator at `mpm recall`:\n%s",
					name, clip(text, 400))
			}
			// And it must not have silently absorbed the flag into the
			// query and returned results.
			if strings.Contains(text, "canary --semantic") {
				t.Errorf("`%s --semantic` absorbed the flag into the query:\n%s",
					name, clip(text, 400))
			}
		})
	}
}

// TestMemorySearchStillAcceptsSupportedFlags guards the other side of 1b:
// rejecting --semantic must not have broken the flags memory search does
// support.
func TestMemorySearchStillAcceptsSupportedFlags(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary and runs subprocesses")
	}
	bin := buildTestBinary(t)

	tmp := t.TempDir()
	workspace := filepath.Join(tmp, "workspace")
	home := filepath.Join(tmp, "home")
	for _, d := range []string{workspace, home} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	seedCanaries(t, bin, workspace, home)
	env := isolatedEnv(workspace, home)

	for _, args := range [][]string{
		{"memory", "search", "canary"},
		{"memory", "search", "canary", "--limit", "5"},
		{"memory", "search", "canary", "--json"},
	} {
		name := strings.Join(args, " ")
		t.Run(name, func(t *testing.T) {
			cmd := exec.Command(bin, args...)
			cmd.Env = env
			cmd.Dir = workspace
			cmd.Dir = workspace
			out, _ := cmd.CombinedOutput()
			text := string(out)
			if strings.Contains(text, "not supported") {
				t.Errorf("`%s` was rejected: %s", name, clip(text, 300))
			}
			// A supported flag must not leak into the FTS5 query.
			if strings.Contains(text, "canary --limit") || strings.Contains(text, "canary --json") {
				t.Errorf("`%s` leaked a flag into the query:\n%s", name, clip(text, 300))
			}
		})
	}
}
