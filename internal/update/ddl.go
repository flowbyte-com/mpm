package update

import (
	"fmt"
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
		if strings.HasPrefix(strings.ToUpper(strings.TrimLeft(s, " \t\n")), prefix) {
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
