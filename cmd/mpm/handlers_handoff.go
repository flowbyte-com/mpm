// cmd/mpm/handlers_handoff.go — `mpm handoff` CLI surface.
//
// Final release-pass: the substrate has supported handoff write/read/
// list/shred since launch, but the human CLI never exposed them as
// a coherent family. Operators had to drop down to
// `mpm call mpm_handoff --payload '{...}'` for every operation.
// This file surfaces the canonical lifecycle:
//
//   mpm handoff write   - record a session-end handoff
//   mpm handoff read    - fetch a specific handoff by id
//   mpm handoff list    - list recent handoffs
//   mpm handoff shred   - permanently delete a handoff
//
// The mpm call / mpm_handoff MCP path remains available for
// agents; this CLI is the human discoverable surface.
//
// All handoffs are immutable post-write — there is no update path.
// The substrate's 90-day retention sweep via `mpm gc` is the
// terminal-state cleanup.

package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/flowbyte-com/mpm-core/usererror"
	"strings"
	"time"

	mpminternal "github.com/flowbyte-com/mpm-core"
	"github.com/flowbyte-com/mpm-core/tools"
)

// handleHandoff dispatches `mpm handoff <sub>`. Subcommands:
// write, read, list, shred. The handoff family is intentionally
// minimal — no update, no restore. Handoffs are write-once.
func handleHandoff(args []string) int {
	if len(args) == 0 {
		printHandoffHelp()
		return 0
	}
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "write":
		return handleHandoffWriteCLI(rest)
	case "read":
		return handleHandoffReadCLI(rest)
	case "list":
		return handleHandoffListCLI(rest)
	case "shred":
		return handleHandoffShredCLI(rest)
	case "help", "-h", "--help":
		printHandoffHelp()
		return 0
	default:
		fmt.Printf("mpm handoff: unknown subcommand %q (available: write, read, list, shred)\n", sub)
		return 1
	}
}

func handleHandoffWriteCLI(args []string) int {
	summary := ""
	var sessionID string
	var tags []string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--summary":
			if i+1 < len(args) {
				summary = args[i+1]
				i++
			}
		case "--session-id":
			if i+1 < len(args) {
				sessionID = args[i+1]
				i++
			}
		case "--tag":
			if i+1 < len(args) {
				for _, t := range strings.Split(args[i+1], ",") {
					t = strings.TrimSpace(t)
					if t != "" {
						tags = append(tags, t)
					}
				}
				i++
			}
		default:
			// Positional: first arg is summary if not yet set.
			if summary == "" {
				summary = args[i]
			}
		}
	}
	if summary == "" {
		fmt.Println("Usage: mpm handoff write --summary <text> [--session-id <id>] [--tag t1,t2]")
		return 1
	}

	dm := getDB()
	if dm == nil {
		return 1
	}

	payload := map[string]interface{}{
		"summary":    summary,
		"session_id": sessionID,
		"tags":       tags,
		"created_at": time.Now().UTC().Format(time.RFC3339),
	}
	out, err := toolsCall(dm, mpminternal.ActiveContext{}, "mpm_handoff", map[string]interface{}{
		"action": "write",
		"params": payload,
	})
	if err != nil {
		return usererror.Error("handoff write: %v", err)
	}
	id, _ := out["id"].(string)
	fmt.Printf("✓ handoff written (id=%s)\n", id)
	return 0
}

func handleHandoffReadCLI(args []string) int {
	if len(args) == 0 {
		fmt.Println("Usage: mpm handoff read <handoff-id>")
		return 1
	}
	dm := getDB()
	if dm == nil {
		return 1
	}
	out, err := toolsCall(dm, mpminternal.ActiveContext{}, "mpm_handoff", map[string]interface{}{
		"action": "read",
		"params": map[string]interface{}{"handoff_id": args[0]},
	})
	if err != nil {
		return usererror.Error("handoff read: %v", err)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
	return 0
}

func handleHandoffListCLI(args []string) int {
	dm := getDB()
	if dm == nil {
		return 1
	}
	limit := 20
	unreadOnly := false
	jsonOutput := false
	for _, a := range args {
		if a == "--json" || a == "-j" {
			jsonOutput = true
			continue
		}
		if strings.HasPrefix(a, "--limit=") {
			if _, err := fmt.Sscanf(strings.TrimPrefix(a, "--limit="), "%d", &limit); err != nil {
				return usererror.Error("handoff list: invalid --limit value: %v", err)
			}
		}
		if a == "--unread" {
			unreadOnly = true
		}
	}
	out, err := toolsCall(dm, mpminternal.ActiveContext{}, "mpm_handoff", map[string]interface{}{
		"action": "list",
		"params": map[string]interface{}{
			"limit":       limit,
			"unread_only": unreadOnly,
		},
	})
	if err != nil {
		return usererror.Error("handoff list: %v", err)
	}
	handoffs, _ := out["handoffs"].([]interface{})
	if handoffs == nil {
		handoffs = []interface{}{}
	}

	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(map[string]interface{}{
			"success": true,
			"count":   len(handoffs),
			"handoffs": handoffs,
		})
		return 0
	}

	if len(handoffs) == 0 {
		fmt.Println("No handoffs.")
		return 0
	}
	for _, h := range handoffs {
		hm, _ := h.(map[string]interface{})
		if hm == nil {
			continue
		}
		fmt.Printf("[%v] %s\n", hm["id"], hm["summary"])
	}
	return 0
}

func handleHandoffShredCLI(args []string) int {
	if len(args) == 0 {
		fmt.Println("Usage: mpm handoff shred <handoff-id>")
		return 1
	}
	dm := getDB()
	if dm == nil {
		return 1
	}
	_, err := toolsCall(dm, mpminternal.ActiveContext{}, "mpm_handoff", map[string]interface{}{
		"action": "shred",
		"params": map[string]interface{}{"handoff_id": args[0]},
	})
	if err != nil {
		return usererror.Error("handoff shred: %v", err)
	}
	fmt.Printf("⚠ handoff %s shredded (not recoverable)\n", args[0])
	return 0
}

func printHandoffHelp() {
	fmt.Println(`mpm handoff — Inter-session handoff lifecycle

Subcommands:
  write   Record a session-end handoff (summary required)
  read    Fetch a specific handoff by id
  list    List recent handoffs (--limit N, --unread)
  shred   Permanently delete a handoff (not recoverable)

Examples:
  mpm handoff write --summary "completed parser fix; pending work item #abc"
  mpm handoff list --limit 5
  mpm handoff read <handoff-id>
  mpm handoff shred <handoff-id>

Notes:
  - Handoffs are write-once. There is no update or restore path.
  - Retention sweep via "mpm gc" handles terminal cleanup.
  - The mpm call / mpm_handoff MCP path remains available for agents.`)
}

// toolsCall invokes a registered tool handler by name. Thin wrapper
// around tools.ByName so handlers_handoff.go stays in package main
// without importing the tool registry symbol directly everywhere.
func toolsCall(dm mpminternal.CoreDB, ac mpminternal.ActiveContext, name string, payload map[string]interface{}) (map[string]interface{}, error) {
	tool, ok := tools.ByName(name)
	if !ok {
		return nil, fmt.Errorf("unknown tool: %s", name)
	}
	out, err := tool.Handler(dm, ac, payload)
	if err != nil {
		return nil, err
	}
	m, _ := out.(map[string]interface{})
	return m, nil
}