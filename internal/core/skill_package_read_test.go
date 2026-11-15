// skill_package_read_test.go — Stage P6: the read-only package retrieval
// and integrity-verification API.
//
// Design of record: PR #222 §8.1–§8.7 and §13.
//
// EVERY test here owns its workspace. `p3DM` pins MPM_WORKSPACE to a
// disposable directory before the manager is constructed, so no test can
// reach the live database, the live workspace or the operator's checkout.
//
// The acceptance rows this file pins:
//
//	T06 — a relative reference read returns the exact bytes;
//	T33 — a bounded read enforces the byte cap and truncates;
//	T34 — a corrupted stored file is refused with content_hash_mismatch;
//	T40 — the packaged-skill provenance (the MCP boundary is pinned in
//	       internal/core/tools);
//	T47 — the bounded read's next_offset advances and reproduces the file;
//	T49 — the terminal overlong line is marked and cannot loop.
//
// plus the coverage the stage asks for: every role, the original
// entrypoint versus the derived projection, empty and missing files, wrong
// package ids and invalid paths, shared blobs, corrupted manifests,
// projections and blob sizes, read-only behaviour with an unchanged
// database, no filesystem materialisation, deterministic listing, legacy
// skill compatibility and concurrent reads.

package internal

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// ─── Fixtures ──────────────────────────────────────────────────────────────

// p6PackageTree writes a package tree into a fresh temporary directory and
// returns its path.
//
// The files map is keyed by the path relative to the package root. The
// bytes are written verbatim, so a binary asset keeps its exact bytes.
func p6PackageTree(t *testing.T, files map[string][]byte) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		full := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		if err := os.WriteFile(full, content, 0o600); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
	return root
}

// p6SkillDoc builds a skill document with the given frontmatter.
//
// A version of "" produces a document that declares NO version, which is
// the case where the derived projection differs from the original bytes.
func p6SkillDoc(name, version, body string) string {
	fm := fmt.Sprintf("---\nname: %s\ndescription: a test skill\nwhen_to_use: when a test needs this exact skill\n", name)
	if version != "" {
		fm += fmt.Sprintf("version: %s\n", version)
	}
	return fm + "---\n" + body
}

// p6ImportPackage imports a package built from the given files and returns
// the import result.
//
// The entrypoint is added by the fixture: the caller supplies the
// supporting files.
func p6ImportPackage(t *testing.T, dm *DatabaseManager, name, version string, files map[string][]byte) *PackageImportResult {
	t.Helper()
	return p6ImportPackageOpts(t, dm, PackageImportOptions{}, name, version, files)
}

// p6ImportPackageOpts is p6ImportPackage with operator options.
func p6ImportPackageOpts(t *testing.T, dm *DatabaseManager, opts PackageImportOptions, name, version string, files map[string][]byte) *PackageImportResult {
	t.Helper()
	tree := map[string][]byte{}
	for k, v := range files {
		tree[k] = v
	}
	tree["SKILL.md"] = []byte(p6SkillDoc(name, version, "the original entrypoint body\n"))
	opts.Path = p6PackageTree(t, tree)
	result, err := dm.ImportSkillPackageFromDir(opts)
	if err != nil {
		t.Fatalf("import the package: %v", err)
	}
	return result
}

// p6Selector builds a selector for a skill id.
func p6Selector(skillID string) PackageSelector {
	return PackageSelector{SkillID: skillID}
}

// p6ReadReason returns the reason carried by err, or the empty string.
func p6ReadReason(err error) PackageReadReason {
	return PackageReadReasonOf(err)
}

// p6Digest digests the rows a query returns, in the query's own order.
//
// It is the census primitive: the same query over the same rows must
// produce the same digest, so a read that changed nothing leaves the
// digest unchanged.
func p6Digest(t *testing.T, dm *DatabaseManager, query string) string {
	t.Helper()
	rows, err := dm.db.Query(query)
	if err != nil {
		t.Fatalf("census query failed: %v", err)
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("census columns: %v", err)
	}
	h := sha256.New()
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("census scan: %v", err)
		}
		for i, v := range vals {
			fmt.Fprintf(h, "%s=%v\x00", cols[i], v)
		}
		h.Write([]byte{0x1e})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("census rows: %v", err)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// p6Census digests every row the package reader can see.
func p6Census(t *testing.T, dm *DatabaseManager) string {
	t.Helper()
	parts := []string{
		"packages=" + fmt.Sprint(p4Count(t, dm, `SELECT COUNT(*) FROM skill_packages`)),
		"files=" + fmt.Sprint(p4Count(t, dm, `SELECT COUNT(*) FROM skill_package_files`)),
		"blobs=" + fmt.Sprint(p4Count(t, dm, `SELECT COUNT(*) FROM skill_package_blobs`)),
		"skills=" + fmt.Sprint(p4Count(t, dm, `SELECT COUNT(*) FROM memories WHERE collection = 'skills'`)),
		p6Digest(t, dm, `SELECT skill_id, name, version, declared_version, version_source, format,
			entrypoint, entrypoint_sha256, manifest, package_hash FROM skill_packages ORDER BY skill_id`),
		p6Digest(t, dm, `SELECT skill_id, path, sha256, size_bytes, content_type, role, executable,
			secret_scanned, poison_scanned FROM skill_package_files ORDER BY skill_id, path`),
		p6Digest(t, dm, `SELECT sha256, size_bytes, content FROM skill_package_blobs ORDER BY sha256`),
		p6Digest(t, dm, `SELECT id, content, metadata FROM memories WHERE collection = 'skills' ORDER BY id`),
	}
	return strings.Join(parts, "\n")
}

// p6WorkspaceFiles lists every file under a root, with its size.
//
// It is the filesystem census: the package reader must never materialise a
// package on disk.
func p6WorkspaceFiles(t *testing.T, root string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		out = append(out, fmt.Sprintf("%s:%d", rel, info.Size()))
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	return out
}

// p6MutateBlob rewrites a blob row's content in place.
//
// It is the corruption primitive: the reader must notice.
func p6MutateBlob(t *testing.T, dm *DatabaseManager, sha, content string) {
	t.Helper()
	if _, err := dm.db.Exec(`UPDATE skill_package_blobs SET content = ? WHERE sha256 = ?`,
		[]byte(content), sha); err != nil {
		t.Fatalf("mutate the blob: %v", err)
	}
}

// p6PackageIdentityState is the package row's identity columns, as stored.
//
// It is the fixture primitive for the corruption tests: a corruption is
// only meaningful if the row is restored EXACTLY afterwards, because a
// leftover disagreement would satisfy the NEXT corruption's assertions
// on its own — and the test would then pass without testing anything.
type p6PackageIdentityState struct {
	declaredVersion sql.NullString
	format          string
	versionSource   string
}

// p6ReadPackageIdentityState reads the package row's identity columns.
func p6ReadPackageIdentityState(t *testing.T, dm *DatabaseManager, skillID string) p6PackageIdentityState {
	t.Helper()
	var state p6PackageIdentityState
	err := dm.db.QueryRow(`
		SELECT declared_version, format, version_source
		FROM skill_packages WHERE skill_id = ?
	`, skillID).Scan(&state.declaredVersion, &state.format, &state.versionSource)
	if err != nil {
		t.Fatalf("read the package identity state: %v", err)
	}
	return state
}

// p6RestorePackageIdentityState puts the package row's identity columns
// back exactly as they were.
//
// The restore is EXACT: a restore that leaves a disagreement behind would
// make the next corruption's assertions vacuous.
func p6RestorePackageIdentityState(t *testing.T, dm *DatabaseManager, skillID string, state p6PackageIdentityState) {
	t.Helper()
	var declared any
	if state.declaredVersion.Valid {
		declared = state.declaredVersion.String
	}
	if _, err := dm.db.Exec(`
		UPDATE skill_packages
		SET declared_version = ?, format = ?, version_source = ?
		WHERE skill_id = ?
	`, declared, state.format, state.versionSource, skillID); err != nil {
		t.Fatalf("restore the package identity state: %v", err)
	}
}

// TestP6VerifyReportsACorruptedManifestIdentity pins that the package row
// and the manifest must agree about the DECLARED VERSION, the FORMAT and
// the VERSION SOURCE, as well as about the name, the version and the
// entrypoint.
//
// The row's `declared_version`, `format` and `version_source` columns are
// part of the package's identity provenance: a row that disagrees with its
// own manifest is a damaged package, and it must not verify.
//
// EACH corruption is proved LOAD-BEARING. Every case restores the row
// EXACTLY afterwards and asserts that the package verifies clean again,
// so a mutation that changes nothing cannot satisfy the next case's
// assertions on its own — the failure a case asserts is the failure that
// case caused, and not one left behind by an earlier one.
func TestP6VerifyReportsACorruptedManifestIdentity(t *testing.T) {
	dm := p3DM(t)
	result := p6ImportPackage(t, dm, "identity", "1.0.0", nil)
	sel := p6Selector(result.SkillID)

	// The row's pristine state. Every case restores it.
	orig := p6ReadPackageIdentityState(t, dm, result.SkillID)

	// A healthy package verifies.
	report, err := dm.PackageVerify(context.Background(), sel)
	if err != nil {
		t.Fatalf("PackageVerify: %v", err)
	}
	if !report.Verified {
		t.Fatalf("a healthy package did not verify: %+v", report.Failures)
	}

	// corrupt applies one mutation, asserts that `verify` reports the
	// identity failure AND that the ordinary reads fail closed on it,
	// then restores the row and asserts that the package verifies clean
	// again.
	corrupt := func(name string, mutate func() error) {
		t.Helper()
		if err := mutate(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		report, err := dm.PackageVerify(context.Background(), sel)
		if err != nil {
			t.Fatalf("%s: PackageVerify: %v", name, err)
		}
		if report.Verified {
			t.Errorf("%s: the package reported itself verified", name)
		}
		if !p6HasFailure(report, PackageVerifyCheckManifestIdentity, "") {
			t.Errorf("%s: the report does not name the identity failure: %+v", name, report.Failures)
		}
		if _, err := dm.PackageFiles(context.Background(), sel); p6ReadReason(err) != PackageReadReasonIntegrityFailure {
			t.Errorf("%s: the listing reported %q, want %q",
				name, p6ReadReason(err), PackageReadReasonIntegrityFailure)
		}

		// Restore, and the package must verify clean again. This is what
		// makes the case load-bearing: a mutation that changed nothing
		// would leave the package verifying, and the assertion above
		// would have failed.
		p6RestorePackageIdentityState(t, dm, result.SkillID, orig)
		report, err = dm.PackageVerify(context.Background(), sel)
		if err != nil {
			t.Fatalf("%s: PackageVerify after the restore: %v", name, err)
		}
		if !report.Verified {
			t.Errorf("%s: the package did not verify again after the restore: %+v", name, report.Failures)
		}
	}

	// ── A declared version that disagrees with the manifest. ──
	corrupt("a corrupted declared version", func() error {
		_, err := dm.db.Exec(`UPDATE skill_packages SET declared_version = ? WHERE skill_id = ?`,
			"9.9.9", result.SkillID)
		return err
	})

	// ── A format that disagrees with the manifest. ──
	corrupt("a corrupted format", func() error {
		_, err := dm.db.Exec(`UPDATE skill_packages SET format = ? WHERE skill_id = ?`,
			"mpm-skill-package/999", result.SkillID)
		return err
	})

	// ── A version SOURCE that disagrees with the manifest. ──
	//
	// The version source is how the effective version was arrived at: the
	// manifest's `version_override.reason` and the row's `version_source`
	// are the same record. A row that disagrees with its own manifest
	// about it is damaged in exactly the way the effective version is.
	//
	// The mutated value is DIFFERENT from the one the importer wrote —
	// this package declares 1.0.0, so the row's source is `declared` and
	// the corruption makes it `package_declares_no_version`.
	corrupt("a corrupted version source", func() error {
		_, err := dm.db.Exec(`UPDATE skill_packages SET version_source = ? WHERE skill_id = ?`,
			PackageVersionSourceUnversioned, result.SkillID)
		return err
	})
}

// p6DatabaseMethodNames are the methods that issue a statement.
//
// A call to one of them is a statement; a bare REFERENCE to one of them
// is a method value, which the call walk cannot see.
//
// `Prepare` and `PrepareContext` are in the set because a prepared
// statement's SQL lives at the PREPARE site: the `*sql.Stmt` it returns
// carries the SQL forward, so a write prepared there would be executed by
// a later `stmt.Query…` call that carries no SQL of its own.
var p6DatabaseMethodNames = map[string]bool{
	"Exec":            true,
	"ExecContext":     true,
	"Query":           true,
	"QueryRow":        true,
	"QueryContext":    true,
	"QueryRowContext": true,
	"Prepare":         true,
	"PrepareContext":  true,
}

// p6SQLKeywords are the keywords a SQL statement can begin with.
//
// A text whose first word is one of them is a STATEMENT, and it is
// therefore subject to the statement check. A text whose first word is
// not one of them — "skill_package", "utf-8", "manifest_identity" — is
// not a statement, and is left alone.
//
// `WITH` is in the list because a CTE is a statement: `WITH x AS (…)
// SELECT …` is a legitimate read, and `WITH x AS (… ) UPDATE …` is a
// write, and the write-keyword rule tells the two apart.
var p6SQLKeywords = []string{
	"SELECT", "INSERT", "UPDATE", "DELETE", "REPLACE", "DROP", "ALTER",
	"CREATE", "PRAGMA", "VACUUM", "ATTACH", "DETACH", "BEGIN", "COMMIT",
	"ROLLBACK", "REASSIGN", "GRANT", "REINDEX", "ANALYZE", "WITH",
	"EXPLAIN",
}

// p6SQLWriteKeywords are the keywords that make a statement a WRITE.
//
// A statement that carries one of them anywhere — not merely at its
// start — is a write, and the reader issues none.
var p6SQLWriteKeywords = []string{
	"INSERT", "UPDATE", "DELETE", "REPLACE", "DROP", "ALTER", "CREATE",
	"PRAGMA", "VACUUM", "ATTACH", "DETACH", "BEGIN", "COMMIT", "ROLLBACK",
	"REASSIGN", "GRANT", "REINDEX", "ANALYZE",
}

// p6UnquoteSQL decodes a Go string literal into the SQL it carries.
//
// It handles BOTH literal forms — the raw backtick string and the
// double-quoted interpreted string — so a statement written either way is
// checked, rather than only the raw form being checked and the other
// raising a false positive.
func p6UnquoteSQL(literal string) (string, bool) {
	unquoted, err := strconv.Unquote(literal)
	if err != nil {
		return "", false
	}
	return unquoted, true
}

// p6ConstStringValue returns the string a constant expression carries.
//
// It handles a plain literal and a CONCATENATION of literals, because
// `"DELETE " + "FROM …"` is a constant too — and a write written that
// way must not slip past the constant check.
func p6ConstStringValue(expr ast.Expr) (string, bool) {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		return p6UnquoteSQL(e.Value)
	case *ast.BinaryExpr:
		if e.Op != token.ADD {
			return "", false
		}
		left, ok := p6ConstStringValue(e.X)
		if !ok {
			return "", false
		}
		right, ok := p6ConstStringValue(e.Y)
		if !ok {
			return "", false
		}
		return left + right, true
	case *ast.ParenExpr:
		return p6ConstStringValue(e.X)
	}
	return "", false
}

// p6ScrubSQL strips the parts of a SQL text that are not statement
// structure: comments, string literals and quoted identifiers.
//
// What is left is the statement's CODE — the words that decide what it
// does.
//
// A comment is removed ENTIRELY, not replaced by a space. That matters:
// a comment in the middle of a keyword would otherwise SPLIT it, and
// `DE/*x*/LETE` would scrub to `DE LLETE` — two words, neither of them
// a keyword — and the statement would be misclassified as "not SQL" and
// skipped. Removing the comment whole leaves `DELETE`, which is what
// the text means.
//
// A line comment takes its terminating newline with it, for the same
// reason: `DE--x\nLETE` must become `DELETE`, not `DE LLETE`.
func p6ScrubSQL(sql string) string {
	var b strings.Builder
	runes := []rune(sql)
	for i := 0; i < len(runes); i++ {
		switch {
		case runes[i] == '-' && i+1 < len(runes) && runes[i+1] == '-':
			// The comment runs to the end of the line, and the line
			// break goes with it: `DE--x\nLETE` must become
			// `DELETE`, not `DE LETE`.
			//
			// The index is left ON the newline, because the loop's
			// own increment then steps past it — advancing it here
			// as well would swallow the first character after the
			// comment.
			for i < len(runes) && runes[i] != '\n' {
				i++
			}
		case runes[i] == '/' && i+1 < len(runes) && runes[i+1] == '*':
			i += 2
			for i+1 < len(runes) && !(runes[i] == '*' && runes[i+1] == '/') {
				i++
			}
			i++
		case runes[i] == '\'' || runes[i] == '"' || runes[i] == '`':
			quote := runes[i]
			i++
			for i < len(runes) && runes[i] != quote {
				if runes[i] == quote && i+1 < len(runes) && runes[i+1] == quote {
					i++
				}
				i++
			}
			b.WriteRune(' ')
		default:
			b.WriteRune(runes[i])
		}
	}
	return b.String()
}

// p6SQLWords splits a SQL text into its keywords and identifiers.
//
// A word is a run of letters, digits and UNDERSCORES, so an identifier
// like `last_update` is ONE word and is never mistaken for the keyword
// `UPDATE`.
func p6SQLWords(sql string) []string {
	var words []string
	var current strings.Builder
	for _, r := range sql {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			current.WriteRune(r)
		default:
			if current.Len() > 0 {
				words = append(words, current.String())
				current.Reset()
			}
		}
	}
	if current.Len() > 0 {
		words = append(words, current.String())
	}
	return words
}

// p6IsSQLStatement reports whether a SQL text is a STATEMENT at all.
//
// A text whose first word is a SQL keyword is one; a text whose first
// word is not — "skill_package", "utf-8", "manifest_identity" — is a
// constant that happens to hold a string, and is left alone.
func p6IsSQLStatement(sql string) bool {
	trimmed := strings.TrimSpace(sql)
	if trimmed == "" {
		return false
	}
	body := strings.TrimRight(strings.ToUpper(p6ScrubSQL(trimmed)), " \t\r\n")
	body = strings.TrimSuffix(body, ";")
	body = strings.TrimRight(body, " \t\r\n")
	words := p6SQLWords(body)
	if len(words) == 0 {
		return false
	}
	return p6WordIn(strings.ToUpper(words[0]), p6SQLKeywords)
}

// p6SQLStatementProblem describes why a SQL text is not a single read
// statement, or returns the empty string when it is one.
//
// The check is STATEMENT-based, not prefix-based, and it covers:
//
//   - the statement must BEGIN with `SELECT` — or with `WITH`, because a
//     CTE is a statement too. A `WITH … UPDATE` is caught by the
//     write-keyword rule below, so allowing `WITH` does not admit a
//     write; and `EXPLAIN` is refused, because the reader issues plain
//     SELECTs and nothing else;
//   - the statement must be a SINGLE statement — a `;` anywhere but at
//     the very end means a second statement follows;
//   - the statement must carry NO write keyword anywhere — so a
//     `SELECT 1; DELETE …` is caught by the second rule and a
//     `SELECT … WHERE x = 'UPDATE'` is not, because the string literal
//     is scrubbed first.
//
// It returns the empty string for a text that is not a statement at all
// (see p6IsSQLStatement), which is how a non-SQL constant is left alone.
func p6SQLStatementProblem(sql string) string {
	trimmed := strings.TrimSpace(sql)
	if trimmed == "" {
		return ""
	}
	code := p6ScrubSQL(trimmed)
	body := strings.TrimRight(strings.ToUpper(code), " \t\r\n")
	body = strings.TrimSuffix(body, ";")
	body = strings.TrimRight(body, " \t\r\n")

	words := p6SQLWords(body)
	if len(words) == 0 {
		return ""
	}
	first := strings.ToUpper(words[0])
	if !p6WordIn(first, p6SQLKeywords) {
		// Not a statement: a constant like "skill_package" or
		// "manifest_identity".
		return ""
	}
	if first != "SELECT" && first != "WITH" {
		return "the statement begins with " + first + ", not SELECT"
	}
	if strings.Contains(body, ";") {
		return "the statement carries more than one statement"
	}
	for _, word := range words[1:] {
		upper := strings.ToUpper(word)
		if p6WordIn(upper, p6SQLWriteKeywords) {
			return "the statement carries the write keyword " + upper
		}
	}
	return ""
}

// p6WordIn reports whether a word is in a keyword list.
func p6WordIn(word string, list []string) bool {
	for _, candidate := range list {
		if word == candidate {
			return true
		}
	}
	return false
}

// p6AssertReaderIssuesNoWrite walks ONE reader source file and asserts
// that it issues no write and that every statement it owns is a single
// read statement. It returns the number of statements it found.
//
// The walk has several halves, and each closes an evasion route:
//
//   - a CALL to a database method is inspected, and the SQL it carries is
//     checked — so a write smuggled through `QueryContext` is caught,
//     and so is one prepared through `PrepareContext`;
//   - a bare REFERENCE to a database method — a method value — is
//     refused, so `exec := db.ExecContext; exec(ctx, "UPDATE …")`
//     cannot slip past the call walk;
//   - every string constant the file declares is checked: one that is a
//     STATEMENT must be a single read statement, so a write cannot hide
//     in a constant — used or not, concatenated or not;
//   - a statement whose SQL is neither a literal nor one of the reader's
//     own constants is refused, so a statement built at runtime cannot
//     slip past either;
//   - every SQL constant the file declares must be USED by a statement,
//     so a statement no call site issues is still a statement the reader
//     owns — and so a file cannot declare SQL it never runs.

// Both literal forms are decoded, so a statement written as a raw
// backtick string and one written as a double-quoted string are checked
// alike — and a double-quoted SELECT is not a false positive.
func p6AssertReaderIssuesNoWrite(t *testing.T, sourceFile string) int {
	t.Helper()

	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, sourceFile, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", sourceFile, err)
	}

	// The SQL the reader's own constants declare — at the top level AND
	// inside a function, because a function-local constant is just as
	// much a statement the reader owns. A constant may be a plain
	// literal or a CONCATENATION of literals, and both are resolved.
	consts := map[string]string{}
	ast.Inspect(parsed, func(n ast.Node) bool {
		gen, ok := n.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			return true
		}
		for _, spec := range gen.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			for i, name := range vs.Names {
				if i >= len(vs.Values) {
					continue
				}
				value, ok := p6ConstStringValue(vs.Values[i])
				if !ok {
					continue
				}
				consts[name.Name] = value
			}
		}
		return true
	})

	// The database-method selectors that are the `Fun` of a CALL. A
	// selector that is not in this set is a bare reference — a method
	// value — and the reader must not bind one.
	calledSelectors := map[token.Pos]bool{}

	// The constants a statement actually used. A SQL constant no
	// statement uses is dead SQL the reader still owns.
	usedConstants := map[string]bool{}

	queries := 0
	ast.Inspect(parsed, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if !p6DatabaseMethodNames[sel.Sel.Name] {
			return true
		}
		calledSelectors[sel.Pos()] = true

		switch sel.Sel.Name {
		case "Exec", "ExecContext":
			t.Errorf("%s issues a write through %s at line %d: the reader must never write",
				sourceFile, sel.Sel.Name, fset.Position(call.Pos()).Line)
			return true
		}
		queries++

		// The context variants take the context first; the plain ones do
		// not. The SQL is the first argument after the context.
		sqlArg := 0
		switch sel.Sel.Name {
		case "QueryContext", "QueryRowContext", "PrepareContext":
			sqlArg = 1
		}
		if len(call.Args) <= sqlArg {
			t.Errorf("%s issues a statement with no SQL at line %d", sourceFile, fset.Position(call.Pos()).Line)
			return true
		}
		// The SQL is either a literal or one of the reader's own
		// constants; both are resolved here, so a statement smuggled in
		// through either route is caught.
		var sql string
		switch arg := call.Args[sqlArg].(type) {
		case *ast.BasicLit:
			if arg.Kind != token.STRING {
				t.Errorf("%s issues a statement whose SQL is not a string at line %d",
					sourceFile, fset.Position(call.Pos()).Line)
				return true
			}
			resolved, ok := p6UnquoteSQL(arg.Value)
			if !ok {
				t.Errorf("%s issues a statement whose SQL literal does not decode at line %d",
					sourceFile, fset.Position(call.Pos()).Line)
				return true
			}
			sql = resolved
		case *ast.Ident:
			resolved, ok := consts[arg.Name]
			if !ok {
				t.Errorf("%s issues a statement whose SQL is %s, which is not one of its own constants at line %d",
					sourceFile, arg.Name, fset.Position(call.Pos()).Line)
				return true
			}
			usedConstants[arg.Name] = true
			sql = resolved
		default:
			t.Errorf("%s issues a statement whose SQL is neither a literal nor a constant at line %d",
				sourceFile, fset.Position(call.Pos()).Line)
			return true
		}
		if problem := p6SQLStatementProblem(sql); problem != "" {
			t.Errorf("%s issues a statement that is not a single read statement at line %d: %s: %q",
				sourceFile, fset.Position(call.Pos()).Line, problem, sql)
		}
		return true
	})

	// ── The method-value route. ──────────────────────────────────────
	//
	// A bare reference to a database method is a METHOD VALUE. The call
	// walk cannot see what it is later called with, so the reference
	// itself is refused: `exec := db.ExecContext` is a write waiting to
	// happen, and the reader has no reason to bind one.
	ast.Inspect(parsed, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if !p6DatabaseMethodNames[sel.Sel.Name] {
			return true
		}
		if calledSelectors[sel.Pos()] {
			return true
		}
		t.Errorf("%s binds the database method %s at line %d: the reader must never bind a database method",
			sourceFile, sel.Sel.Name, fset.Position(sel.Pos()).Line)
		return true
	})

	// ── The constant route. ───────────────────────────────────────────
	//
	// Every string constant the file declares is checked: one that is a
	// STATEMENT must be a single read statement. A write can therefore
	// hide in a constant — used or not, at the top level or inside a
	// function, written as a literal or as a concatenation.
	for name, sql := range consts {
		if !p6IsSQLStatement(sql) {
			continue
		}
		if problem := p6SQLStatementProblem(sql); problem != "" {
			t.Errorf("the SQL constant %s is not a single read statement: %s: %q", name, problem, sql)
		}
	}

	// ── The dead-SQL route. ───────────────────────────────────────────
	//
	// A SQL constant no statement uses is still a statement the reader
	// owns, and a file that declares SQL it never runs is a file whose
	// guard has nothing to check. Both are refused.
	for name, sql := range consts {
		if !p6IsSQLStatement(sql) {
			continue
		}
		if !usedConstants[name] {
			t.Errorf("the SQL constant %s is never used by a statement: %q", name, sql)
		}
	}
	return queries
}

// TestP6TheReaderNeverWrites pins that the reader issues no write.
//
// The read snapshot is a DEFERRED transaction: the SQLite driver ignores
// `sql.TxOptions{ReadOnly}` and issues a plain `BEGIN`, so the snapshot's
// read-only nature rests on DISCIPLINE. This test makes the discipline
// structural: the reader's source contains no `Exec` call at all, it
// binds no database method, and EVERY statement it owns — issued or
// merely declared — is a single read statement.
//
// It is a STATIC check on purpose. A dynamic one could only prove that
// the paths a test happens to exercise did not write; a source check
// proves that there is no statement they could have written.
//
// The check is an AST walk, not a hand-maintained list: it finds EVERY
// call that issues a statement and checks the SQL that call carries. A
// list would go stale the moment a statement were added, and a write
// smuggled through `QueryContext` would evade it.
//
// BOTH reader files are walked, not one: the reader is the two files
// together, and a guard that covered only one of them would have left
// half of the reader unguarded.
func TestP6TheReaderNeverWrites(t *testing.T) {
	readerFiles := []string{
		"skill_package_read.go",
		"skill_package_read_errors.go",
	}

	total := 0
	for _, file := range readerFiles {
		total += p6AssertReaderIssuesNoWrite(t, file)
	}
	if total == 0 {
		t.Fatalf("the reader files issue no query at all: the static guard found nothing to check")
	}
}

// ─── T06 — the relative reference read ─────────────────────────────────────

// TestP6ReadFileReturnsTheExactReferenceBytes pins T06.
//
// The read returns the reference file's EXACT bytes, the hash matches the
// bytes, and the read describes exactly ONE file — no other file's content
// is inlined beside it.
func TestP6ReadFileReturnsTheExactReferenceBytes(t *testing.T) {
	dm := p3DM(t)
	reference := "# Templates\n\nthe reference body\n"
	result := p6ImportPackage(t, dm, "refreader", "1.0.0", map[string][]byte{
		"references/templates.md": []byte(reference),
	})

	listing, err := dm.PackageFiles(context.Background(), p6Selector(result.SkillID))
	if err != nil {
		t.Fatalf("PackageFiles: %v", err)
	}
	if listing.FileCount != 2 {
		t.Fatalf("the package carries %d files, want 2 (the entrypoint and the reference)", listing.FileCount)
	}

	read, err := dm.PackageReadFile(context.Background(), p6Selector(result.SkillID),
		"references/templates.md", PackageReadBounds{})
	if err != nil {
		t.Fatalf("PackageReadFile: %v", err)
	}
	if string(read.Content) != reference {
		t.Errorf("the read returned %q, want the exact bytes %q", read.Content, reference)
	}
	if want := sha256Hex([]byte(reference)); read.SHA256 != want {
		t.Errorf("the read's hash is %s, want %s", read.SHA256, want)
	}
	if read.SizeBytes != int64(len(reference)) {
		t.Errorf("the read's size is %d, want %d", read.SizeBytes, len(reference))
	}
	if read.Path != "references/templates.md" {
		t.Errorf("the read's path is %q, want the canonical path", read.Path)
	}
	if read.Source != PackageReadSourceSkillPackage {
		t.Errorf("the read's source is %q, want %q", read.Source, PackageReadSourceSkillPackage)
	}
	if read.Role != PackageRoleReference {
		t.Errorf("the read's role is %q, want %q", read.Role, PackageRoleReference)
	}
	if !read.Scanned.Secret || !read.Scanned.Poison {
		t.Errorf("the read's scan status is %+v, want both scanners to have run", read.Scanned)
	}
	// Exclusivity: exactly one file's content, and the entrypoint's content
	// is not inlined beside it.
	if bytes.Contains(read.Content, []byte("the original entrypoint body")) {
		t.Errorf("the read inlined the entrypoint's content beside the reference")
	}
	if read.Truncated || read.NextOffset != nil {
		t.Errorf("a complete read reported truncated=%v next_offset=%v", read.Truncated, read.NextOffset)
	}
}

// ─── The listing ───────────────────────────────────────────────────────────

// TestP6FilesListsMetadataOnly pins that the listing carries METADATA and
// never content, and that it is deterministic.
func TestP6FilesListsMetadataOnly(t *testing.T) {
	dm := p3DM(t)
	result := p6ImportPackage(t, dm, "lister", "1.0.0", map[string][]byte{
		"references/principles.md": []byte("principles\n"),
		"scripts/run.sh":           []byte("#!/bin/sh\necho hi\n"),
	})

	listing, err := dm.PackageFiles(context.Background(), p6Selector(result.SkillID))
	if err != nil {
		t.Fatalf("PackageFiles: %v", err)
	}
	if listing.FileCount != len(listing.Files) {
		t.Errorf("the file count is %d, the listing carries %d records", listing.FileCount, len(listing.Files))
	}
	if listing.PackageHash != result.PackageHash {
		t.Errorf("the listing's package hash is %s, want %s", listing.PackageHash, result.PackageHash)
	}

	// The order is canonical-path order, and it is stable.
	wantPaths := []string{"SKILL.md", "references/principles.md", "scripts/run.sh"}
	if len(listing.Files) != len(wantPaths) {
		t.Fatalf("the listing carries %d files, want %d", len(listing.Files), len(wantPaths))
	}
	for i, want := range wantPaths {
		if listing.Files[i].Path != want {
			t.Errorf("file %d is %q, want %q", i, listing.Files[i].Path, want)
		}
	}

	// The listing carries no content: every record is metadata, and the
	// type has no field a content could hide in.
	for _, f := range listing.Files {
		if f.SizeBytes <= 0 {
			t.Errorf("the file %q reports size %d", f.Path, f.SizeBytes)
		}
		if f.SHA256 == "" {
			t.Errorf("the file %q reports no hash", f.Path)
		}
	}

	// Determinism: the same package lists the same way, every time.
	second, err := dm.PackageFiles(context.Background(), p6Selector(result.SkillID))
	if err != nil {
		t.Fatalf("the second PackageFiles: %v", err)
	}
	for i := range listing.Files {
		if listing.Files[i] != second.Files[i] {
			t.Errorf("the listing is not deterministic at %d: %+v vs %+v", i, listing.Files[i], second.Files[i])
		}
	}
}

// TestP6FilesCoversEveryRole pins that all four roles are reported, and
// that the scan statuses are the ACTUAL ones.
//
// The binary asset is the interesting half: its poison scan is a DISCLOSED
// SKIP, so the listing must report poison=false for it and must not
// pretend the scan passed.
func TestP6FilesCoversEveryRole(t *testing.T) {
	dm := p3DM(t)
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x01, 0x02, 0x03}
	result := p6ImportPackageOpts(t, dm, PackageImportOptions{AllowBinary: true}, "roles", "1.0.0",
		map[string][]byte{
			"references/principles.md": []byte("principles\n"),
			"scripts/run.sh":           []byte("#!/bin/sh\necho hi\n"),
			"assets/logo.png":          png,
		})

	listing, err := dm.PackageFiles(context.Background(), p6Selector(result.SkillID))
	if err != nil {
		t.Fatalf("PackageFiles: %v", err)
	}
	byPath := map[string]PackageManifestFile{}
	for _, f := range listing.Files {
		byPath[f.Path] = f
	}
	if len(byPath) != 4 {
		t.Fatalf("the package carries %d files, want 4", len(byPath))
	}

	if got := byPath["SKILL.md"].Role; got != PackageRoleEntrypoint {
		t.Errorf("SKILL.md's role is %q, want %q", got, PackageRoleEntrypoint)
	}
	if got := byPath["references/principles.md"].Role; got != PackageRoleReference {
		t.Errorf("the reference's role is %q, want %q", got, PackageRoleReference)
	}
	if got := byPath["scripts/run.sh"].Role; got != PackageRoleScript {
		t.Errorf("the script's role is %q, want %q", got, PackageRoleScript)
	}
	if got := byPath["assets/logo.png"].Role; got != PackageRoleAsset {
		t.Errorf("the asset's role is %q, want %q", got, PackageRoleAsset)
	}

	// The script is flagged executable and is never executed.
	if !byPath["scripts/run.sh"].Executable {
		t.Errorf("the script is not flagged executable")
	}
	if byPath["SKILL.md"].Executable {
		t.Errorf("the entrypoint is flagged executable")
	}

	// The ACTUAL scan statuses. The text files ran both scanners; the
	// binary asset ran the secret scanner only.
	for _, path := range []string{"SKILL.md", "references/principles.md", "scripts/run.sh"} {
		f := byPath[path]
		if !f.Scanned.Secret || !f.Scanned.Poison {
			t.Errorf("%s's scan status is %+v, want both scanners to have run", path, f.Scanned)
		}
	}
	asset := byPath["assets/logo.png"]
	if !asset.Scanned.Secret {
		t.Errorf("the binary asset's scan status is %+v, want the secret scanner to have run", asset.Scanned)
	}
	if asset.Scanned.Poison {
		t.Errorf("the binary asset claims a poison scan, which does not apply to binary content")
	}
	if asset.ContentType != "image/png" {
		t.Errorf("the binary asset's content type is %q, want image/png", asset.ContentType)
	}
}

// ─── The original entrypoint versus the derived projection ────────────────

// TestP6ReadFileReturnsTheOriginalEntrypointNotTheProjection pins the
// distinction the design is explicit about.
//
// A versionless entrypoint's projection GAINS a version line, so the two
// differ byte for byte — and the read must return the ORIGINAL.
func TestP6ReadFileReturnsTheOriginalEntrypointNotTheProjection(t *testing.T) {
	dm := p3DM(t)
	result := p6ImportPackage(t, dm, "versionless", "", nil)

	original := p6SkillDoc("versionless", "", "the original entrypoint body\n")
	read, err := dm.PackageReadFile(context.Background(), p6Selector(result.SkillID),
		"SKILL.md", PackageReadBounds{})
	if err != nil {
		t.Fatalf("PackageReadFile: %v", err)
	}
	if string(read.Content) != original {
		t.Errorf("the read returned %q, want the ORIGINAL bytes %q", read.Content, original)
	}

	// The projection is a DIFFERENT document.
	var projection string
	if err := dm.db.QueryRow(`SELECT content FROM memories WHERE id = ?`, result.SkillID).
		Scan(&projection); err != nil {
		t.Fatalf("read the projection: %v", err)
	}
	if projection == original {
		t.Fatalf("the fixture did not produce a distinct projection")
	}
	if read.SHA256 == sha256Hex([]byte(projection)) {
		t.Errorf("the read's hash describes the PROJECTION, not the original entrypoint")
	}
	if read.SHA256 != result.EntrypointSHA256 {
		t.Errorf("the read's hash is %s, want the package's entrypoint hash %s",
			read.SHA256, result.EntrypointSHA256)
	}
}

// ─── Failures ──────────────────────────────────────────────────────────────

// TestP6ReadFileDistinguishesItsFailures pins that the four read
// failures are four DIFFERENT classifications, and that corruption is
// never reported as a missing file.
func TestP6ReadFileDistinguishesItsFailures(t *testing.T) {
	dm := p3DM(t)
	result := p6ImportPackage(t, dm, "failures", "1.0.0", map[string][]byte{
		"references/templates.md": []byte("the reference\n"),
	})
	sel := p6Selector(result.SkillID)

	// A missing file.
	_, err := dm.PackageReadFile(context.Background(), sel, "references/absent.md", PackageReadBounds{})
	if got := p6ReadReason(err); got != PackageReadReasonFileNotFound {
		t.Errorf("a missing file reported %q, want %q", got, PackageReadReasonFileNotFound)
	}

	// An invalid path — a traversal, an absolute path, an empty path.
	for _, path := range []string{"../escape.md", "/etc/passwd", "", "references/../../escape.md"} {
		_, err := dm.PackageReadFile(context.Background(), sel, path, PackageReadBounds{})
		if got := p6ReadReason(err); got != PackageReadReasonInvalidPath {
			t.Errorf("the path %q reported %q, want %q", path, got, PackageReadReasonInvalidPath)
		}
	}

	// A package that does not exist.
	_, err = dm.PackageReadFile(context.Background(), p6Selector("skill:nosuchskill-v1.0.0"),
		"SKILL.md", PackageReadBounds{})
	if got := p6ReadReason(err); got != PackageReadReasonPackageNotFound {
		t.Errorf("a missing package reported %q, want %q", got, PackageReadReasonPackageNotFound)
	}

	// A skill that exists but owns no package is ALSO `package_not_found`:
	// the reader never falls back to the ordinary skill's projection.
	if _, err := dm.SaveSkill("ordinary", "1.0.0",
		p6SkillDoc("ordinary", "1.0.0", "an ordinary body\n"), "p6-test", false); err != nil {
		t.Fatalf("SaveSkill: %v", err)
	}
	_, err = dm.PackageReadFile(context.Background(), p6Selector("skill:ordinary-v1.0.0"),
		"SKILL.md", PackageReadBounds{})
	if got := p6ReadReason(err); got != PackageReadReasonPackageNotFound {
		t.Errorf("an ordinary skill reported %q, want %q", got, PackageReadReasonPackageNotFound)
	}
}

// TestP6SelectorRejectsAnAmbiguousIdentity pins that the selector never
// picks a version silently and never merges two identities.
func TestP6SelectorRejectsAnAmbiguousIdentity(t *testing.T) {
	dm := p3DM(t)
	result := p6ImportPackage(t, dm, "selector", "1.0.0", nil)

	// A name without a version does not identify one package.
	_, err := dm.PackageFiles(context.Background(), PackageSelector{Name: "selector"})
	if got := p6ReadReason(err); got != PackageReadReasonSelectorInvalid {
		t.Errorf("a name without a version reported %q, want %q", got, PackageReadReasonSelectorInvalid)
	}

	// Both forms at once is refused, not merged.
	_, err = dm.PackageFiles(context.Background(), PackageSelector{
		SkillID: result.SkillID, Name: "selector", Version: "1.0.0",
	})
	if got := p6ReadReason(err); got != PackageReadReasonSelectorInvalid {
		t.Errorf("a merged selector reported %q, want %q", got, PackageReadReasonSelectorInvalid)
	}

	// The name-and-version form resolves to the same package.
	byName, err := dm.PackageFiles(context.Background(), PackageSelector{Name: "selector", Version: "1.0.0"})
	if err != nil {
		t.Fatalf("PackageFiles by name and version: %v", err)
	}
	byID, err := dm.PackageFiles(context.Background(), p6Selector(result.SkillID))
	if err != nil {
		t.Fatalf("PackageFiles by skill id: %v", err)
	}
	if byName.PackageHash != byID.PackageHash {
		t.Errorf("the two selector forms describe different packages")
	}
}

// TestP6ReadFileRefusesACorruptedBlob pins T34.
//
// The blob's bytes are mutated in place. The read must REFUSE with
// `content_hash_mismatch` — it must not return the mutated bytes, and it
// must not report the file as missing.
func TestP6ReadFileRefusesACorruptedBlob(t *testing.T) {
	dm := p3DM(t)
	reference := "the original reference bytes\n"
	result := p6ImportPackage(t, dm, "corrupt", "1.0.0", map[string][]byte{
		"references/templates.md": []byte(reference),
	})
	sel := p6Selector(result.SkillID)

	// The read works before the corruption.
	if _, err := dm.PackageReadFile(context.Background(), sel, "references/templates.md", PackageReadBounds{}); err != nil {
		t.Fatalf("the read before the corruption: %v", err)
	}

	blobSHA := sha256Hex([]byte(reference))
	p6MutateBlob(t, dm, blobSHA, "the tampered bytes\n")

	read, err := dm.PackageReadFile(context.Background(), sel, "references/templates.md", PackageReadBounds{})
	if err == nil {
		t.Fatalf("the read returned the corrupted file: %q", read.Content)
	}
	if got := p6ReadReason(err); got != PackageReadReasonContentHashMismatch {
		t.Errorf("the corrupted read reported %q, want %q", got, PackageReadReasonContentHashMismatch)
	}
	if read != nil {
		t.Errorf("the refused read returned a payload: %+v", read)
	}
}

// ─── Binary content ────────────────────────────────────────────────────────

// TestP6ReadFileCarriesBinaryBytesOnlyWhenRaw pins that binary content is
// never carried through a text field by accident.
func TestP6ReadFileCarriesBinaryBytesOnlyWhenRaw(t *testing.T) {
	dm := p3DM(t)
	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x01, 0x02, 0x03, 0xff, 0xfe}
	result := p6ImportPackageOpts(t, dm, PackageImportOptions{AllowBinary: true}, "binary", "1.0.0",
		map[string][]byte{"assets/logo.png": png})
	sel := p6Selector(result.SkillID)

	// Without the raw form the content is refused: a JSON layer could not
	// carry these bytes in a text field without corrupting them.
	read, err := dm.PackageReadFile(context.Background(), sel, "assets/logo.png", PackageReadBounds{})
	if err == nil {
		t.Fatalf("the binary read succeeded without raw=true: %q", read.Content)
	}
	if got := p6ReadReason(err); got != PackageReadReasonBinaryRequiresRaw {
		t.Errorf("the binary read reported %q, want %q", got, PackageReadReasonBinaryRequiresRaw)
	}

	// With the raw form the bytes come back EXACTLY, and the transport
	// encoding is explicit.
	read, err = dm.PackageReadFile(context.Background(), sel, "assets/logo.png",
		PackageReadBounds{Raw: true})
	if err != nil {
		t.Fatalf("the raw binary read: %v", err)
	}
	if !bytes.Equal(read.Content, png) {
		t.Errorf("the raw read returned %v, want the exact bytes %v", read.Content, png)
	}
	if read.Encoding != PackageReadEncodingBase64 {
		t.Errorf("the raw read's encoding is %q, want %q", read.Encoding, PackageReadEncodingBase64)
	}
	encoded, err := read.EncodedContent()
	if err != nil {
		t.Fatalf("EncodedContent: %v", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("the encoded content is not base64: %v", err)
	}
	if !bytes.Equal(decoded, png) {
		t.Errorf("the round trip returned %v, want %v", decoded, png)
	}
	if read.Scanned.Poison {
		t.Errorf("the binary read claims a poison scan")
	}
}

// ─── T33 — the bounded read ────────────────────────────────────────────────

// TestP6BoundedReadEnforcesTheByteCap pins T33.
//
// The cap is enforced at a LINE boundary: the response never exceeds the
// cap, it says it was truncated, and it carries the offset the caller
// resumes from.
func TestP6BoundedReadEnforcesTheByteCap(t *testing.T) {
	dm := p3DM(t)
	var body strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&body, "line %03d: %s\n", i, strings.Repeat("x", 100))
	}
	reference := body.String()
	result := p6ImportPackage(t, dm, "bounded", "1.0.0", map[string][]byte{
		"references/lines.md": []byte(reference),
	})
	sel := p6Selector(result.SkillID)

	read, err := dm.PackageReadFile(context.Background(), sel, "references/lines.md",
		PackageReadBounds{MaxBytes: 512})
	if err != nil {
		t.Fatalf("PackageReadFile: %v", err)
	}
	if len(read.Content) > 512 {
		t.Errorf("the read returned %d bytes, above the 512 byte cap", len(read.Content))
	}
	if !read.Truncated {
		t.Errorf("the read did not report truncation")
	}
	if read.NextOffset == nil {
		t.Fatalf("the truncated read carried no next_offset")
	}
	if *read.NextOffset <= read.Offset {
		t.Errorf("next_offset %d did not advance past offset %d", *read.NextOffset, read.Offset)
	}
	if read.Lines == 0 {
		t.Errorf("the truncated read returned no lines")
	}
	// The returned window ends on a LINE boundary.
	if !strings.HasSuffix(string(read.Content), "\n") {
		t.Errorf("the returned window does not end on a line boundary: %q", read.Content)
	}

	// The default cap is 64 KiB.
	defaultRead, err := dm.PackageReadFile(context.Background(), sel, "references/lines.md",
		PackageReadBounds{})
	if err != nil {
		t.Fatalf("the default-cap read: %v", err)
	}
	if len(defaultRead.Content) > PackageReadDefaultMaxBytes {
		t.Errorf("the default read returned %d bytes, above the %d byte default",
			len(defaultRead.Content), PackageReadDefaultMaxBytes)
	}

	// The hard cap is 1 MiB, and a request above it is refused rather than
	// clamped.
	_, err = dm.PackageReadFile(context.Background(), sel, "references/lines.md",
		PackageReadBounds{MaxBytes: PackageReadMaxBytesLimit + 1})
	if got := p6ReadReason(err); got != PackageReadReasonInvalidBounds {
		t.Errorf("a cap above the hard maximum reported %q, want %q", got, PackageReadReasonInvalidBounds)
	}
	// The hard cap itself is admissible.
	if _, err := dm.PackageReadFile(context.Background(), sel, "references/lines.md",
		PackageReadBounds{MaxBytes: PackageReadMaxBytesLimit}); err != nil {
		t.Errorf("the hard cap was refused: %v", err)
	}
}

// TestP6BoundedReadRejectsInvalidBounds pins the bounds rules.
func TestP6BoundedReadRejectsInvalidBounds(t *testing.T) {
	dm := p3DM(t)
	result := p6ImportPackage(t, dm, "bounds", "1.0.0", map[string][]byte{
		"references/lines.md": []byte("one\ntwo\nthree\n"),
	})
	sel := p6Selector(result.SkillID)

	for _, bounds := range []PackageReadBounds{
		{Offset: -1},
		{Limit: -1},
		{MaxBytes: -1},
		{Offset: 99},
	} {
		_, err := dm.PackageReadFile(context.Background(), sel, "references/lines.md", bounds)
		if got := p6ReadReason(err); got != PackageReadReasonInvalidBounds {
			t.Errorf("the bounds %+v reported %q, want %q", bounds, got, PackageReadReasonInvalidBounds)
		}
	}

	// An offset equal to the line count is valid: it is the natural
	// terminal resume, and it is complete rather than truncated.
	read, err := dm.PackageReadFile(context.Background(), sel, "references/lines.md",
		PackageReadBounds{Offset: 3})
	if err != nil {
		t.Fatalf("the terminal resume: %v", err)
	}
	if read.Lines != 0 || read.Truncated || read.NextOffset != nil {
		t.Errorf("the terminal resume returned lines=%d truncated=%v next=%v",
			read.Lines, read.Truncated, read.NextOffset)
	}
}

// ─── T47 — the continuation semantics ──────────────────────────────────────

// TestP6BoundedReadContinuationReproducesTheFile pins T47.
//
// Concatenating every successive bounded read reproduces the requested
// file without a gap, without a repeated line and without an altered byte.
func TestP6BoundedReadContinuationReproducesTheFile(t *testing.T) {
	dm := p3DM(t)
	// A file with mixed line endings, a missing final newline and UTF-8.
	reference := "alpha\nbeta\r\ngamma\nδέλτα\nepsilon"
	result := p6ImportPackage(t, dm, "continuation", "1.0.0", map[string][]byte{
		"references/mixed.md": []byte(reference),
	})
	sel := p6Selector(result.SkillID)

	for _, limit := range []int{1, 2, 3, 7} {
		var out []byte
		offset := 0
		guard := 0
		for {
			guard++
			if guard > 100 {
				t.Fatalf("the continuation looped with limit %d", limit)
			}
			read, err := dm.PackageReadFile(context.Background(), sel, "references/mixed.md",
				PackageReadBounds{Offset: offset, Limit: limit})
			if err != nil {
				t.Fatalf("the continuation read: %v", err)
			}
			out = append(out, read.Content...)
			if !read.Truncated {
				if read.NextOffset != nil {
					t.Errorf("a complete read carried a next_offset")
				}
				break
			}
			if read.NextOffset == nil {
				t.Fatalf("a truncated read carried no next_offset")
			}
			if *read.NextOffset <= offset {
				t.Fatalf("the offset did not advance: %d -> %d", offset, *read.NextOffset)
			}
			offset = *read.NextOffset
		}
		if string(out) != reference {
			t.Errorf("the continuation with limit %d produced %q, want %q", limit, out, reference)
		}
	}
}

// TestP6BoundedReadToleratesAnAbsurdLimit pins that a huge line-count
// limit selects the whole file rather than overflowing into an empty
// range.
func TestP6BoundedReadToleratesAnAbsurdLimit(t *testing.T) {
	dm := p3DM(t)
	reference := "one\ntwo\nthree\n"
	result := p6ImportPackage(t, dm, "absurd", "1.0.0", map[string][]byte{
		"references/lines.md": []byte(reference),
	})
	read, err := dm.PackageReadFile(context.Background(), p6Selector(result.SkillID),
		"references/lines.md", PackageReadBounds{Limit: 1 << 62})
	if err != nil {
		t.Fatalf("PackageReadFile: %v", err)
	}
	if string(read.Content) != reference {
		t.Errorf("the read returned %q, want the whole file", read.Content)
	}
	if read.Truncated {
		t.Errorf("the read reported truncation")
	}
}

// TestP6BoundedReadHandlesTheAwkwardFiles pins the empty file, the file
// with no final newline, the CRLF file and the empty line.
func TestP6BoundedReadHandlesTheAwkwardFiles(t *testing.T) {
	dm := p3DM(t)
	result := p6ImportPackage(t, dm, "awkward", "1.0.0", map[string][]byte{
		"references/empty.md": []byte(""),
		"references/nonl.md":  []byte("no final newline"),
		"references/crlf.md":  []byte("one\r\ntwo\r\n"),
		"references/blank.md": []byte("\n\n"),
	})
	sel := p6Selector(result.SkillID)

	// The empty file: zero lines, complete.
	read, err := dm.PackageReadFile(context.Background(), sel, "references/empty.md",
		PackageReadBounds{})
	if err != nil {
		t.Fatalf("the empty file: %v", err)
	}
	if read.Lines != 0 {
		t.Errorf("the empty file reported %d lines", read.Lines)
	}
	if read.Truncated || read.NextOffset != nil {
		t.Errorf("the empty file reported truncated=%v next=%v", read.Truncated, read.NextOffset)
	}
	if len(read.Content) != 0 {
		t.Errorf("the empty file returned %d bytes", len(read.Content))
	}

	// The file with no final newline: one line, and it is complete.
	read, err = dm.PackageReadFile(context.Background(), sel, "references/nonl.md",
		PackageReadBounds{})
	if err != nil {
		t.Fatalf("the file with no final newline: %v", err)
	}
	if read.Lines != 1 {
		t.Errorf("the file with no final newline reported %d lines, want 1", read.Lines)
	}
	if string(read.Content) != "no final newline" {
		t.Errorf("the file with no final newline returned %q", read.Content)
	}
	if read.Truncated {
		t.Errorf("the file with no final newline reported truncation")
	}

	// The CRLF file: the terminators are preserved.
	read, err = dm.PackageReadFile(context.Background(), sel, "references/crlf.md",
		PackageReadBounds{})
	if err != nil {
		t.Fatalf("the CRLF file: %v", err)
	}
	if string(read.Content) != "one\r\ntwo\r\n" {
		t.Errorf("the CRLF file returned %q, want the CRLF bytes preserved", read.Content)
	}
	if read.Lines != 2 {
		t.Errorf("the CRLF file reported %d lines, want 2", read.Lines)
	}

	// The file of two empty lines.
	read, err = dm.PackageReadFile(context.Background(), sel, "references/blank.md",
		PackageReadBounds{})
	if err != nil {
		t.Fatalf("the blank file: %v", err)
	}
	if read.Lines != 2 {
		t.Errorf("the blank file reported %d lines, want 2", read.Lines)
	}
	if string(read.Content) != "\n\n" {
		t.Errorf("the blank file returned %q", read.Content)
	}
}

// ─── T49 — the terminal case ───────────────────────────────────────────────

// TestP6BoundedReadTerminalOverlongLine pins T49.
//
// The first line alone exceeds the cap: the response carries ZERO lines,
// says it is truncated, keeps the offset where it was, and names the
// reason. Resuming returns the same response — which is why the caller
// must stop instead of resuming.
func TestP6BoundedReadTerminalOverlongLine(t *testing.T) {
	dm := p3DM(t)
	long := strings.Repeat("x", 4096)
	reference := long + "\nshort\n"
	result := p6ImportPackage(t, dm, "terminal", "1.0.0", map[string][]byte{
		"references/long.md": []byte(reference),
	})
	sel := p6Selector(result.SkillID)

	read, err := dm.PackageReadFile(context.Background(), sel, "references/long.md",
		PackageReadBounds{MaxBytes: 64})
	if err != nil {
		t.Fatalf("PackageReadFile: %v", err)
	}
	if read.Lines != 0 {
		t.Errorf("the terminal read returned %d lines, want 0", read.Lines)
	}
	if len(read.Content) != 0 {
		t.Errorf("the terminal read returned %d bytes, want 0", len(read.Content))
	}
	if !read.Truncated {
		t.Errorf("the terminal read did not report truncation")
	}
	if read.NextOffset == nil {
		t.Fatalf("the terminal read carried no next_offset")
	}
	if *read.NextOffset != read.Offset {
		t.Errorf("the terminal read's next_offset is %d, want the requested offset %d",
			*read.NextOffset, read.Offset)
	}
	if read.Reason != PackageReadReasonLineTooLarge {
		t.Errorf("the terminal read's reason is %q, want %q", read.Reason, PackageReadReasonLineTooLarge)
	}
	if !read.Terminal {
		t.Errorf("the terminal read is not marked terminal")
	}

	// Resuming returns the SAME empty response: that is why the response
	// forbids resuming.
	again, err := dm.PackageReadFile(context.Background(), sel, "references/long.md",
		PackageReadBounds{Offset: *read.NextOffset, MaxBytes: 64})
	if err != nil {
		t.Fatalf("the resumed read: %v", err)
	}
	if again.Lines != 0 || !again.Truncated || again.NextOffset == nil ||
		*again.NextOffset != again.Offset || again.Reason != PackageReadReasonLineTooLarge {
		t.Errorf("the resumed read is not the same terminal response: %+v", again)
	}

	// Raising the cap is the only way forward, and it works.
	raised, err := dm.PackageReadFile(context.Background(), sel, "references/long.md",
		PackageReadBounds{MaxBytes: 8192})
	if err != nil {
		t.Fatalf("the raised-cap read: %v", err)
	}
	if raised.Lines == 0 || raised.Terminal {
		t.Errorf("the raised-cap read did not serve the file: %+v", raised)
	}
	if string(raised.Content) != reference {
		t.Errorf("the raised-cap read returned %q, want the exact bytes", raised.Content)
	}
}

// TestP6BoundedReadNeverLoops pins the progress guarantee directly: no
// response is ever truncated with an unchanged offset unless it is the
// terminal one, and the terminal one always says so.
func TestP6BoundedReadNeverLoops(t *testing.T) {
	dm := p3DM(t)
	var body strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&body, "row %02d %s\n", i, strings.Repeat("y", 50))
	}
	body.WriteString(strings.Repeat("z", 5000) + "\n")
	result := p6ImportPackage(t, dm, "progress", "1.0.0", map[string][]byte{
		"references/mixed.md": []byte(body.String()),
	})
	sel := p6Selector(result.SkillID)

	offset := 0
	for step := 0; step < 100; step++ {
		read, err := dm.PackageReadFile(context.Background(), sel, "references/mixed.md",
			PackageReadBounds{Offset: offset, MaxBytes: 256})
		if err != nil {
			t.Fatalf("the progress read: %v", err)
		}
		if !read.Truncated {
			break
		}
		if read.NextOffset == nil {
			t.Fatalf("a truncated read carried no next_offset")
		}
		if *read.NextOffset == offset {
			// The ONLY response that may not advance is the terminal one,
			// and it must say so.
			if !read.Terminal || read.Reason != PackageReadReasonLineTooLarge {
				t.Fatalf("a truncated response did not advance and is not marked terminal: %+v", read)
			}
			return
		}
		if *read.NextOffset < offset {
			t.Fatalf("the offset went BACKWARDS: %d -> %d", offset, *read.NextOffset)
		}
		offset = *read.NextOffset
	}
	t.Fatalf("the progress loop did not terminate")
}

// ─── Verify ────────────────────────────────────────────────────────────────

// TestP6VerifyReportsAHealthyPackage pins that a sound package verifies.
func TestP6VerifyReportsAHealthyPackage(t *testing.T) {
	dm := p3DM(t)
	result := p6ImportPackage(t, dm, "healthy", "1.0.0", map[string][]byte{
		"references/templates.md": []byte("the reference\n"),
	})

	report, err := dm.PackageVerify(context.Background(), p6Selector(result.SkillID))
	if err != nil {
		t.Fatalf("PackageVerify: %v", err)
	}
	if !report.Verified {
		t.Errorf("a healthy package did not verify: %+v", report.Failures)
	}
	if len(report.Failures) != 0 {
		t.Errorf("a healthy package reported %d failures: %+v", len(report.Failures), report.Failures)
	}
	if report.FileCount != 2 {
		t.Errorf("the report counts %d files, want 2", report.FileCount)
	}
	for _, check := range []string{
		PackageVerifyCheckManifestHash,
		PackageVerifyCheckManifestIdentity,
		PackageVerifyCheckFileMetadata,
		PackageVerifyCheckBlobExists,
		PackageVerifyCheckBlobHash,
		PackageVerifyCheckBlobSize,
		PackageVerifyCheckEntrypoint,
		PackageVerifyCheckProjection,
		PackageVerifyCheckVersion,
	} {
		found := false
		for _, ran := range report.Checks {
			if ran == check {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("the report does not name the check %q", check)
		}
	}
}

// TestP6VerifyReportsCorruption pins that corruption is REPORTED, never
// silently accepted and never repaired.
func TestP6VerifyReportsCorruption(t *testing.T) {
	dm := p3DM(t)
	reference := "the reference bytes\n"
	result := p6ImportPackage(t, dm, "damaged", "1.0.0", map[string][]byte{
		"references/templates.md": []byte(reference),
	})
	sel := p6Selector(result.SkillID)

	// ── A corrupted blob. ──
	p6MutateBlob(t, dm, sha256Hex([]byte(reference)), "the tampered bytes\n")
	report, err := dm.PackageVerify(context.Background(), sel)
	if err != nil {
		t.Fatalf("PackageVerify: %v", err)
	}
	if report.Verified {
		t.Errorf("a corrupted package reported itself verified")
	}
	if !p6HasFailure(report, PackageVerifyCheckBlobHash, "references/templates.md") {
		t.Errorf("the report does not name the corrupted blob: %+v", report.Failures)
	}

	// The verification REPAIRED nothing: the blob is still corrupt.
	var stored string
	if err := dm.db.QueryRow(`SELECT CAST(content AS TEXT) FROM skill_package_blobs WHERE sha256 = ?`,
		sha256Hex([]byte(reference))).Scan(&stored); err != nil {
		t.Fatalf("read the blob: %v", err)
	}
	if stored != "the tampered bytes\n" {
		t.Errorf("the verification repaired the blob: %q", stored)
	}

	// ── A corrupted blob SIZE. ──
	if _, err := dm.db.Exec(`UPDATE skill_package_blobs SET size_bytes = 999 WHERE sha256 = ?`,
		sha256Hex([]byte(reference))); err != nil {
		t.Fatalf("corrupt the blob size: %v", err)
	}
	report, err = dm.PackageVerify(context.Background(), sel)
	if err != nil {
		t.Fatalf("PackageVerify: %v", err)
	}
	if report.Verified {
		t.Errorf("a package with a corrupted blob size reported itself verified")
	}
	if !p6HasFailure(report, PackageVerifyCheckBlobSize, "references/templates.md") {
		t.Errorf("the report does not name the corrupted size: %+v", report.Failures)
	}
}

// TestP6VerifyReportsACorruptedManifest pins that a manifest that does not
// describe its own content is reported, and that the ordinary reads refuse
// it rather than serving it.
func TestP6VerifyReportsACorruptedManifest(t *testing.T) {
	dm := p3DM(t)
	result := p6ImportPackage(t, dm, "badmanifest", "1.0.0", nil)
	sel := p6Selector(result.SkillID)

	if _, err := dm.db.Exec(`UPDATE skill_packages SET manifest = ? WHERE skill_id = ?`,
		`{"format":"mpm-skill-package/1","name":"badmanifest"}`, result.SkillID); err != nil {
		t.Fatalf("corrupt the manifest: %v", err)
	}

	report, err := dm.PackageVerify(context.Background(), sel)
	if err != nil {
		t.Fatalf("PackageVerify: %v", err)
	}
	if report.Verified {
		t.Errorf("a package with a corrupted manifest reported itself verified")
	}
	if !p6HasFailure(report, PackageVerifyCheckManifestHash, "") {
		t.Errorf("the report does not name the corrupted manifest: %+v", report.Failures)
	}

	// The ordinary reads FAIL CLOSED on it.
	if _, err := dm.PackageFiles(context.Background(), sel); p6ReadReason(err) != PackageReadReasonIntegrityFailure {
		t.Errorf("the listing of a corrupted package reported %q, want %q",
			p6ReadReason(err), PackageReadReasonIntegrityFailure)
	}
	if _, err := dm.PackageReadFile(context.Background(), sel, "SKILL.md", PackageReadBounds{}); p6ReadReason(err) != PackageReadReasonIntegrityFailure {
		t.Errorf("the read of a corrupted package reported %q, want %q",
			p6ReadReason(err), PackageReadReasonIntegrityFailure)
	}
}

// TestP6VerifyReportsACorruptedProjection pins that the projection must
// match the manifest's projection_sha256.
func TestP6VerifyReportsACorruptedProjection(t *testing.T) {
	dm := p3DM(t)
	result := p6ImportPackage(t, dm, "badprojection", "1.0.0", nil)
	sel := p6Selector(result.SkillID)

	if _, err := dm.db.Exec(`UPDATE memories SET content = ? WHERE id = ?`,
		"---\nname: badprojection\nversion: 1.0.0\n---\na DIFFERENT body\n", result.SkillID); err != nil {
		t.Fatalf("corrupt the projection: %v", err)
	}

	report, err := dm.PackageVerify(context.Background(), sel)
	if err != nil {
		t.Fatalf("PackageVerify: %v", err)
	}
	if report.Verified {
		t.Errorf("a package with a corrupted projection reported itself verified")
	}
	if !p6HasFailure(report, PackageVerifyCheckProjection, "") {
		t.Errorf("the report does not name the corrupted projection: %+v", report.Failures)
	}
}

// TestP6VerifyReportsAMissingBlob pins that a blob the rows reference but
// that is gone is reported.
func TestP6VerifyReportsAMissingBlob(t *testing.T) {
	dm := p3DM(t)
	reference := "the reference bytes\n"
	result := p6ImportPackage(t, dm, "missingblob", "1.0.0", map[string][]byte{
		"references/templates.md": []byte(reference),
	})
	sel := p6Selector(result.SkillID)

	// The blob is deleted with the foreign keys suspended, on ONE pinned
	// connection: the schema's ON DELETE NO ACTION is a real protection,
	// and this test is simulating damage that the schema itself would
	// never allow.
	conn, err := dm.db.Conn(context.Background())
	if err != nil {
		t.Fatalf("pin a connection: %v", err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(context.Background(), `PRAGMA foreign_keys = OFF`); err != nil {
		t.Fatalf("suspend the foreign keys: %v", err)
	}
	if _, err := conn.ExecContext(context.Background(), `DELETE FROM skill_package_blobs WHERE sha256 = ?`,
		sha256Hex([]byte(reference))); err != nil {
		t.Fatalf("delete the blob: %v", err)
	}

	report, err := dm.PackageVerify(context.Background(), sel)
	if err != nil {
		t.Fatalf("PackageVerify: %v", err)
	}
	if report.Verified {
		t.Errorf("a package with a missing blob reported itself verified")
	}
	if !p6HasFailure(report, PackageVerifyCheckBlobExists, "references/templates.md") {
		t.Errorf("the report does not name the missing blob: %+v", report.Failures)
	}

	// The read refuses it — and refuses it as an INTEGRITY failure, not
	// as a missing file.
	_, err = dm.PackageReadFile(context.Background(), sel, "references/templates.md", PackageReadBounds{})
	if got := p6ReadReason(err); got != PackageReadReasonIntegrityFailure {
		t.Errorf("the read of a missing blob reported %q, want %q", got, PackageReadReasonIntegrityFailure)
	}
}

// p6HasFailure reports whether the report names a failure for a check (and,
// when a path is given, for that path).
func p6HasFailure(report *PackageVerificationResult, check, path string) bool {
	for _, f := range report.Failures {
		if f.Check != check {
			continue
		}
		if path == "" || f.Path == path {
			return true
		}
	}
	return false
}

// ─── Read-only behaviour ───────────────────────────────────────────────────

// TestP6ReadsChangeNothing pins that the read API is read-only: the
// database's census is identical before and after, and no file is
// materialised on disk.
func TestP6ReadsChangeNothing(t *testing.T) {
	dm := p3DM(t)
	workspace := os.Getenv("MPM_WORKSPACE")
	result := p6ImportPackage(t, dm, "readonly", "1.0.0", map[string][]byte{
		"references/templates.md": []byte("the reference\n"),
		"scripts/run.sh":          []byte("#!/bin/sh\necho hi\n"),
	})
	sel := p6Selector(result.SkillID)

	before := p6Census(t, dm)
	filesBefore := p6WorkspaceFiles(t, workspace)

	// Every read operation, several times over.
	for i := 0; i < 3; i++ {
		if _, err := dm.PackageFiles(context.Background(), sel); err != nil {
			t.Fatalf("PackageFiles: %v", err)
		}
		if _, err := dm.PackageReadFile(context.Background(), sel, "SKILL.md", PackageReadBounds{}); err != nil {
			t.Fatalf("PackageReadFile: %v", err)
		}
		if _, err := dm.PackageReadFile(context.Background(), sel, "references/templates.md",
			PackageReadBounds{MaxBytes: 16}); err != nil {
			t.Fatalf("the bounded read: %v", err)
		}
		if _, err := dm.PackageVerify(context.Background(), sel); err != nil {
			t.Fatalf("PackageVerify: %v", err)
		}
		if _, err := dm.SkillPackageProvenance(context.Background(), result.SkillID); err != nil {
			t.Fatalf("SkillPackageProvenance: %v", err)
		}
	}

	after := p6Census(t, dm)
	if before != after {
		t.Errorf("the reads changed the database:\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}

	filesAfter := p6WorkspaceFiles(t, workspace)
	if len(filesBefore) != len(filesAfter) {
		t.Errorf("the reads materialised %d files: %v", len(filesAfter)-len(filesBefore), filesAfter)
		return
	}
	for i := range filesBefore {
		if filesBefore[i] != filesAfter[i] {
			t.Errorf("the reads changed the filesystem: %q became %q", filesBefore[i], filesAfter[i])
		}
	}
}

// ─── Shared blobs ──────────────────────────────────────────────────────────

// TestP6SharedBlobsAreReadThroughBothPackages pins that a
// content-addressed blob shared by two packages is read through both, and
// that it is stored ONCE.
func TestP6SharedBlobsAreReadThroughBothPackages(t *testing.T) {
	dm := p3DM(t)
	shared := []byte("the shared reference bytes\n")
	first := p6ImportPackage(t, dm, "first", "1.0.0", map[string][]byte{
		"references/shared.md": shared,
	})
	second := p6ImportPackage(t, dm, "second", "1.0.0", map[string][]byte{
		"references/shared.md": shared,
	})

	// The blob is stored once.
	if n := p4Count(t, dm, `SELECT COUNT(*) FROM skill_package_blobs WHERE sha256 = ?`,
		sha256Hex(shared)); n != 1 {
		t.Errorf("the shared blob is stored %d times, want 1", n)
	}

	for _, id := range []string{first.SkillID, second.SkillID} {
		read, err := dm.PackageReadFile(context.Background(), p6Selector(id),
			"references/shared.md", PackageReadBounds{})
		if err != nil {
			t.Fatalf("PackageReadFile for %s: %v", id, err)
		}
		if !bytes.Equal(read.Content, shared) {
			t.Errorf("the shared blob read through %s returned %q", id, read.Content)
		}
	}
}

// ─── Legacy skills ─────────────────────────────────────────────────────────

// TestP6OrdinarySkillHasNoProvenance pins that an ordinary single-document
// skill has NO package object: the provenance is absent, not an empty one.
func TestP6OrdinarySkillHasNoProvenance(t *testing.T) {
	dm := p3DM(t)
	id, err := dm.SaveSkill("legacy", "1.0.0",
		p6SkillDoc("legacy", "1.0.0", "an ordinary body\n"), "p6-test", false)
	if err != nil {
		t.Fatalf("SaveSkill: %v", err)
	}

	prov, err := dm.SkillPackageProvenance(context.Background(), id)
	if err != nil {
		t.Fatalf("SkillPackageProvenance: %v", err)
	}
	if prov != nil {
		t.Errorf("an ordinary skill carries a provenance object: %+v", prov)
	}

	// And the package operations refuse it, rather than describing the
	// ordinary skill as a one-file package.
	if _, err := dm.PackageFiles(context.Background(), p6Selector(id)); p6ReadReason(err) != PackageReadReasonPackageNotFound {
		t.Errorf("the listing of an ordinary skill reported %q, want %q",
			p6ReadReason(err), PackageReadReasonPackageNotFound)
	}
}

// TestP6ProvenanceDescribesThePackage pins the provenance object's own
// fields.
func TestP6ProvenanceDescribesThePackage(t *testing.T) {
	dm := p3DM(t)
	result := p6ImportPackage(t, dm, "provenance", "1.0.0", map[string][]byte{
		"references/templates.md": []byte("the reference\n"),
	})

	prov, err := dm.SkillPackageProvenance(context.Background(), result.SkillID)
	if err != nil {
		t.Fatalf("SkillPackageProvenance: %v", err)
	}
	if prov == nil {
		t.Fatalf("a packaged skill carries no provenance")
	}
	if prov.Entrypoint != "SKILL.md" {
		t.Errorf("the provenance's entrypoint is %q, want SKILL.md", prov.Entrypoint)
	}
	if prov.EntrypointSHA256 != result.EntrypointSHA256 {
		t.Errorf("the provenance's entrypoint hash is %s, want %s",
			prov.EntrypointSHA256, result.EntrypointSHA256)
	}
	if prov.PackageHash != result.PackageHash {
		t.Errorf("the provenance's package hash is %s, want %s",
			prov.PackageHash, result.PackageHash)
	}
	if prov.FileCount != 2 {
		t.Errorf("the provenance's file count is %d, want 2", prov.FileCount)
	}
	if !prov.Scanned {
		t.Errorf("the provenance's scan status is %+v, want the entrypoint to be scanned", prov.ScanStatus)
	}
	if !prov.ScanStatus.Secret || !prov.ScanStatus.Poison {
		t.Errorf("the provenance's scan status is %+v, want both scanners to have run", prov.ScanStatus)
	}
	if prov.VersionOverride == nil {
		t.Errorf("the provenance carries no version override")
	} else if prov.VersionOverride.Effective != "1.0.0" {
		t.Errorf("the provenance's version override is %+v", prov.VersionOverride)
	}
}

// ─── Concurrency ───────────────────────────────────────────────────────────

// TestP6ConcurrentReadsAreConsistent pins that concurrent readers see the
// same bytes and the same metadata.
func TestP6ConcurrentReadsAreConsistent(t *testing.T) {
	dm := p3DM(t)
	reference := strings.Repeat("the concurrent reference line\n", 200)
	result := p6ImportPackage(t, dm, "concurrent", "1.0.0", map[string][]byte{
		"references/lines.md": []byte(reference),
	})
	sel := p6Selector(result.SkillID)

	const readers = 8
	var wg sync.WaitGroup
	errs := make(chan error, readers*4)
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for round := 0; round < 4; round++ {
				read, err := dm.PackageReadFile(context.Background(), sel, "references/lines.md",
					PackageReadBounds{MaxBytes: 4096})
				if err != nil {
					errs <- err
					return
				}
				if !strings.HasPrefix(reference, string(read.Content)) {
					errs <- fmt.Errorf("the concurrent read returned bytes that are not the file's")
					return
				}
				listing, err := dm.PackageFiles(context.Background(), sel)
				if err != nil {
					errs <- err
					return
				}
				if listing.FileCount != 2 {
					errs <- fmt.Errorf("the concurrent listing counted %d files", listing.FileCount)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("a concurrent read failed: %v", err)
	}
}
