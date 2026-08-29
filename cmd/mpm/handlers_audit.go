// handlers_audit.go — mpm audit CLI surface for the system_audit_log
// table.
//
// F-A1: this command exists so operators can recover the F14-1
// provenance audit trail for a memory that was dedup'd under F19
// idempotency. Without this surface, the dedup audit rows were written
// but invisible — operators had no way to learn which actors had
// re-saved the same content.
//
// Usage:
//
//	mpm audit [--level info|warn|error|fatal|critical]
//	          [--component <name>]
//	          [--artifact-id <memory|lesson|decision|... id>]
//	          [--days N]
//	          [--limit N]
//	          [--json]
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/usererror"
)

func handleAudit(args []string) int {
	fs := flag.NewFlagSet("audit", flag.ContinueOnError)
	level := fs.String("level", "", "Filter by level: info|warn|error|fatal|critical")
	component := fs.String("component", "", "Filter by component (e.g. 'provenance', 'security')")
	artifactID := fs.String("artifact-id", "", "Filter by canonical artifact id (recovers F14-1 dedup provenance for a memory)")
	days := fs.Int("days", 7, "Lookback window in days (default 7)")
	limit := fs.Int("limit", 20, "Max rows to return (default 20, max 500)")
	jsonOutput := fs.Bool("json", false, "Output JSON for tool integration")
	if err := fs.Parse(args); err != nil {
		return 1
	}

	dm := getDB()
	if dm == nil {
		return 1
	}

	// Normalize level to known values; reject unknown to avoid silent
	// no-result-filter behavior that hides typos.
	var lvl mpminternal.AuditLevel
	if *level != "" {
		switch strings.ToLower(*level) {
		case "info":
			lvl = mpminternal.AuditInfo
		case "warn", "warning":
			lvl = mpminternal.AuditWarn
		case "error":
			lvl = mpminternal.AuditError
		case "fatal":
			lvl = mpminternal.AuditFatal
		case "critical":
			lvl = mpminternal.AuditCritical
		default:
			usererror.Error("audit: unknown level %q (want info|warn|error|fatal|critical)", *level)
			return 1
		}
	}

	rows, err := dm.QueryAuditLog(lvl, *component, *artifactID, *days, *limit)
	if err != nil {
		usererror.Error("audit: %v", err)
		return 1
	}

	if *jsonOutput {
		out := map[string]interface{}{
			"success": true,
			"count":   len(rows),
			"results": rows,
		}
		if *artifactID != "" {
			out["artifact_id"] = *artifactID
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(out)
		return 0
	}

	if len(rows) == 0 {
		fmt.Println("No audit rows matched the filter.")
		return 0
	}

	fmt.Printf("Audit log (%d row(s)):\n", len(rows))
	for _, r := range rows {
		createdAt, _ := r["created_at"].(string)
		lvlStr, _ := r["level"].(string)
		compStr, _ := r["component"].(string)
		msg, _ := r["message"].(string)
		id, _ := r["id"].(string)
		fmt.Printf("  [%s] %s | %s | %s\n  %s\n",
			lvlStr, createdAt, compStr, id, msg)
		if ctx, ok := r["context"].(map[string]interface{}); ok {
			for k, v := range ctx {
				fmt.Printf("    %s = %v\n", k, v)
			}
		}
	}
	return 0
}