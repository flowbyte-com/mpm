package update

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// extractLifecycleDDL pulls the lifecycle_events CREATE TABLE statement
// out of the Go schema source. It mirrors schema_preflight.extract_go_ddl
// for the one table install.sh embeds in the bundle; a checkout whose
// DDL is not extractable simply ships no DDL, and the preflight
// degrades gracefully rather than failing the build.
func extractLifecycleDDL(src string) (string, error) {
	const prefix = "CREATE TABLE IF NOT EXISTS lifecycle_events"
	var spans []string
	i := 0
	for {
		a := strings.Index(src[i:], "`")
		if a < 0 {
			break
		}
		a += i
		b := strings.Index(src[a+1:], "`")
		if b < 0 {
			break
		}
		b += a + 1
		spans = append(spans, src[a+1:b])
		i = b + 1
	}
	var cands []string
	for _, s := range spans {
		// The span is upper-cased and so must be the prefix: comparing
		// an upper-cased span against a mixed-case prefix can never
		// match, and the failure is SILENT — stageSchemaDDL treats an
		// unextractable DDL as "this checkout ships no DDL" and
		// returns nil, so the bundle was published with no schema
		// preflight and the collision check degraded to a no-op.
		//
		// scripts/lib/schema_preflight.py compares prefix.upper(), and
		// the two must agree or the Go side silently ships less than
		// the shell side.
		if strings.HasPrefix(strings.ToUpper(strings.TrimLeft(s, " \t\n")), strings.ToUpper(prefix)) {
			cands = append(cands, s)
		}
	}
	if len(cands) != 1 {
		return "", fmt.Errorf("no unambiguous lifecycle_events DDL (%d candidates)", len(cands))
	}
	ddl := cands[0]
	if !strings.HasSuffix(strings.TrimSpace(ddl), ";") {
		return "", fmt.Errorf("lifecycle_events DDL does not end with ';'")
	}
	return ddl, nil
}

// SchemaPreflightRelPath is where a bundle carries the substrate's
// lifecycle_events DDL.
//
// WHY THE BUNDLE CARRIES IT. The schema-collision preflight compares
// the live table against the DDL the substrate itself would apply. A
// bundle that carries that DDL lets the check run without a source
// checkout; a bundle that does not carry it degrades the check to a
// no-op, SILENTLY. That silent degradation is the failure this
// existence is here to prevent, so the preflight is a required part of
// a candidate bundle — not an optional extra.
const SchemaPreflightRelPath = "schema/lifecycle_events.sql"

// lifecycleEventsDDLPattern matches a CREATE TABLE statement for the
// lifecycle_events table.
//
// IT IS THE SHAPE THE EXTRACTOR PRODUCES, and it is deliberately
// narrow. The preflight must not be satisfied by an unrelated or
// destructive statement: "DROP TABLE lifecycle_events;" names the
// table but says nothing about the shape the substrate applies, and a
// file containing only that would pass a looser check while leaving
// the collision preflight with nothing to compare.
//
// The rule is applied identically by the shell validator
// (rb_validate_schema_preflight in scripts/lib/runtime_bundle.sh), so
// the two validators cannot disagree about what counts as a preflight.
var lifecycleEventsDDLPattern = regexp.MustCompile(`(?i)CREATE\s+TABLE\s+(IF\s+NOT\s+EXISTS\s+)?lifecycle_events\b`)

// schemaPreflightProblems checks a bundle's schema preflight and
// returns one message per problem.
//
// REQUIRED (candidate scope): the preflight must be present, non-empty
// and carry the lifecycle_events CREATE TABLE. A bundle without one
// would install a runtime whose collision preflight cannot run.
//
// OPTIONAL (installed scope): a release promoted before the preflight
// travelled with the bundle may not carry one. That is a documented
// historic gap, not a corruption, so it is reported and not refused —
// but if the file IS there, it must be valid.
func schemaPreflightProblems(bundleDir string, required bool) []string {
	path := filepath.Join(bundleDir, filepath.FromSlash(SchemaPreflightRelPath))
	fi, err := os.Lstat(path)
	if err != nil {
		if !required {
			return nil
		}
		return []string{fmt.Sprintf("the bundle carries no schema preflight (%s); the schema-collision preflight would degrade to a no-op", SchemaPreflightRelPath)}
	}
	if !fi.Mode().IsRegular() {
		return []string{fmt.Sprintf("the bundle's schema preflight (%s) is not a regular file", SchemaPreflightRelPath)}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return []string{fmt.Sprintf("the bundle's schema preflight (%s) could not be read: %v", SchemaPreflightRelPath, err)}
	}
	text := strings.TrimSpace(string(raw))
	if text == "" {
		return []string{fmt.Sprintf("the bundle's schema preflight (%s) is empty; the schema-collision preflight would have nothing to compare the live table against", SchemaPreflightRelPath)}
	}
	if !lifecycleEventsDDLPattern.MatchString(text) {
		return []string{fmt.Sprintf("the bundle's schema preflight (%s) does not declare the lifecycle_events table; it must carry the substrate's own CREATE TABLE for it, or the schema-collision preflight compares the live table against nothing", SchemaPreflightRelPath)}
	}
	return nil
}
