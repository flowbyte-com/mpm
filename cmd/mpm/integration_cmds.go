// Package main — integration_cmds.go
//
// `mpm integration <sub>` cross-framework config emitters. The flagship
// command is `export-mcp`:
//
//   mpm integration export-mcp [--framework claude-code|hermes|opencl] [--json]
//
// Prints the exact JSON snippet to paste into the target framework's
// MCP-servers config. The snippet always references the canonical
// $HOME/.mpm/bin/mpm-mcp binary (resolved to absolute), so users never
// have to guess where mpm lives or which binary a stale install picked
// up. Catches the failure mode where a docs page promises "run
// `mpm-mcp`" but the user's PATH doesn't have it (because they
// installed somewhere non-canonical).
//
// Single substrate truth: the binary at $HOME/.mpm/bin/mpm-mcp. Every
// framework config should point there, not at /usr/local/bin/mpm-mcp
// (would break if MPM_USER is unset) or a path-relative value like
// ./bin/mpm-mcp (would break if the framework loads from a different
// cwd than the install dir).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/flowbyte-com/mpm-core/config"
	"github.com/flowbyte-com/mpm-core/usererror"
)

// integrationEmitter is the per-framework shape emitter. Each emit
// function returns the snippet as-is — no trailing newline, no
// surrounding prose — so the calling user can pipe the output
// straight into `pbcopy` / `xclip` / a copy buffer.
type integrationEmitter struct {
	name        string
	description string
	emit        func(mpmBin string, mpmWorkspace string, env []string) ([]byte, error)
}

// integrationRegistry orders the registered frameworks. First entry
// is the default when `--framework` is omitted.
var integrationRegistry []integrationEmitter

// canonicalMcpPath returns the absolute path to the canonical
// mpm-mcp binary. The canonical install location is
// $HOME/.mpm/bin/mpm-mcp (matches the data-root convention). If
// $HOME is unset or the binary doesn't exist there yet, fall
// back to whatever `which mpm-mcp` finds — better than printing
// a path that the user has to fix before they can paste.
func canonicalMcpPath() string {
	mpmDir := config.GetMPMDir()
	if mpmDir == "" {
		// config.GetMPMDir falls back to $HOME/.mpm — but may have
		// returned "" if HOME was unset. Final fallback: PATH lookup.
		if path, err := lookMcpOnPATH(); err == nil {
			return path
		}
		return ""
	}
	candidate := filepath.Join(mpmDir, "bin", "mpm-mcp")
	if _, err := os.Stat(candidate); err == nil {
		// Always emit the absolute path — `mcp.servers.mpm.command` in
		// every framework expects a process-spawnable path.
		if abs, absErr := filepath.Abs(candidate); absErr == nil {
			return abs
		}
		return candidate
	}
	// Canonical location doesn't have the binary. Most likely cause:
	// the user hasn't run `make install` yet. Try PATH as a friendly
	// fallback and let the framework config dry-run catch the rest.
	if path, err := lookMcpOnPATH(); err == nil {
		return path
	}
	// Last resort — return the canonical location even if missing.
	// The user will see the path and either (a) notice the install is
	// missing and run `make install`, or (b) update their MCP config
	// after installing to a different prefix.
	if mpmDir == "" {
		return filepath.Join(os.Getenv("HOME"), ".mpm", "bin", "mpm-mcp")
	}
	return candidate
}

func lookMcpOnPATH() (string, error) {
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		candidate := filepath.Join(dir, "mpm-mcp")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("mpm-mcp not found on PATH")
}

// workspaceForSnippet returns the canonical workspace path. Used in
// the emitted snippet so frameworks that pass env inherit the same
// workspace the active CLI uses (rather than re-deriving it).
func workspaceForSnippet() string {
	if d := config.GetMPMDir(); d != "" {
		return d
	}
	// config.GetMPMDir falls back to $HOME/.mpm — but if HOME is unset
	// and MPM_DIR isn't set, GetMPMDir may return "". Last-resort
	// derivation so the emitted snippet is always a paste-ready
	// absolute path.
	home := os.Getenv("HOME")
	if home == "" {
		home = "/root"
	}
	return filepath.Join(home, ".mpm")
}

// emitterClaudeCode emits the canonical MCP snippet for Claude Code's
// .claude.json `mcpServers` config.
func emitterClaudeCode(mpmBin, mpmWorkspace string, env []string) ([]byte, error) {
	servers := buildServersObject(mpmBin, mpmWorkspace, env)
	return json.MarshalIndent(map[string]interface{}{
		"mcpServers": servers,
	}, "", "  ")
}

// emitterHermes emits the Hermes mcp.json shape. The Hermes field is
// `mcp_servers` (snake_case) at the top level — easy mistake to make
// for anyone copying a Claude Code snippet into Hermes. Confirmed
// via the Hermes docs snapshot checked into the workspace prior to
// this implementation; if their config schema changes, this emitter
// is the single place to update.
func emitterHermes(mpmBin, mpmWorkspace string, env []string) ([]byte, error) {
	servers := buildServersObject(mpmBin, mpmWorkspace, env)
	return json.MarshalIndent(map[string]interface{}{
		"mcp_servers": servers,
	}, "", "  ")
}

// emitterOpenCl emits the OpenClaw `mcp.servers.mpm` shape, which
// is the LIVE config already shipped in ~/.openclaw/openclaw.json.
// Surfacing it via `mpm integration export-mcp --framework opencl`
// gives v (and anyone forking the project) a printable reference
// to compare against — drift between this emitter and the actual
// openclaw.json is detectable at a glance.
func emitterOpenCl(mpmBin, mpmWorkspace string, env []string) ([]byte, error) {
	return json.MarshalIndent(map[string]interface{}{
		"command": mpmBin,
		"env": map[string]string{
			"MPM_WORKSPACE": mpmWorkspace,
		},
	}, "", "  ")
}

// buildServersObject builds the {mpm: {...}} value that every framework
// uses (Claude Code's mcpServers, Hermes's mcp_servers — both are maps
// keyed by server name).
func buildServersObject(mpmBin, mpmWorkspace string, env []string) map[string]interface{} {
	envMap := map[string]string{
		"MPM_WORKSPACE": mpmWorkspace,
	}
	for _, kv := range env {
		idx := strings.IndexByte(kv, '=')
		if idx <= 0 {
			continue
		}
		envMap[kv[:idx]] = kv[idx+1:]
	}
	return map[string]interface{}{
		"mpm": map[string]interface{}{
			"command": mpmBin,
			"env":     envMap,
		},
	}
}

func handleIntegrationExportMcp(args []string) int {
	fs := flag.NewFlagSet("integration export-mcp", flag.ContinueOnError)
	framework := fs.String("framework", "", "Target framework: claude-code (default), hermes, opencl. Empty = claude-code.")
	jsonFlag := fs.Bool("json", false, "Emit JSON wrapper (default already JSON; reserved for future formats like yaml).")
	fs.Usage = func() {
		fmt.Println("Usage: mpm integration export-mcp [--framework <name>] [--json]")
		fmt.Println()
		fmt.Println("Prints the exact JSON snippet to paste into your framework's MCP-servers")
		fmt.Println("config. Always points at the canonical $HOME/.mpm/bin/mpm-mcp binary.")
		fmt.Println()
		fmt.Println("Frameworks:")
		for _, e := range integrationRegistry {
			fmt.Printf("  %-12s  %s\n", e.name, e.description)
		}
		fmt.Println()
		fmt.Println("Examples:")
		fmt.Println("  mpm integration export-mcp                 # Claude Code (.claude.json)")
		fmt.Println("  mpm integration export-mcp --framework hermes")
		fmt.Println("  mpm integration export-mcp --framework opencl | pbcopy")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return 1
	}

	// Resolve framework: default = first registered.
	target := *framework
	if target == "" {
		target = integrationRegistry[0].name
	}

	var emitter integrationEmitter
	for _, e := range integrationRegistry {
		if e.name == target {
			emitter = e
			break
		}
	}
	if emitter.name == "" {
		usererror.Error("unknown framework: %q\navailable: %s",
			target, strings.Join(frameworkNames(), ", "))
		return 1
	}

	mpmBin := canonicalMcpPath()
	workspace := workspaceForSnippet()

	// Default env set. Include MPM_ACTIVE_MODE / MPM_ACTIVE_PERSONA only
	// if the local config has them set — never inject placeholders.
	env := []string{}
	// Reserved for future: --env key=value flags, etc.

	snippet, err := emitter.emit(mpmBin, workspace, env)
	if err != nil {
		usererror.Error("failed to render snippet: %v", err)
		return 1
	}

	// Always emit trailing newline (snippets are easier to paste
	// when they have a terminator). With --json, suppress; the
	// framework config typically expects compact JSON.
	if !*jsonFlag {
		fmt.Println(string(snippet))
	} else {
		compact := compactJSON(snippet)
		os.Stdout.Write(compact)
		os.Stdout.Write([]byte{'\n'})
	}
	return 0
}

// compactJSON marshals the parsed value with no indent and emits to
// stdout. Falls back to the input on parse failure (defensive — the
// snippets we emit are known-good JSON).
func compactJSON(in []byte) []byte {
	var v interface{}
	if err := json.Unmarshal(in, &v); err != nil {
		return in
	}
	out, err := json.Marshal(v)
	if err != nil {
		return in
	}
	return out
}

func frameworkNames() []string {
	names := make([]string, 0, len(integrationRegistry))
	for _, e := range integrationRegistry {
		names = append(names, e.name)
	}
	sort.Strings(names)
	return names
}

func init() {
	integrationRegistry = []integrationEmitter{
		{
			name:        "claude-code",
			description: "Claude Code ~/.claude.json (mcpServers map at top level)",
			emit:        emitterClaudeCode,
		},
		{
			name:        "hermes",
			description: "Hermes ~/.hermes/mcp.json (mcp_servers, snake_case at top level)",
			emit:        emitterHermes,
		},
		{
			name:        "opencl",
			description: "OpenClaw ~/.openclaw/openclaw.json (mcp.servers.mpm object — useful for drift comparison)",
			emit:        emitterOpenCl,
		},
	}
}

// handleIntegration dispatches `mpm integration <subcommand>`.
// Kept thin — every concrete subcommand lives in its own function
// in this file (or successors).
func handleIntegration(args []string) int {
	if len(args) < 1 {
		return printIntegrationHelp()
	}
	switch args[0] {
	case "export-mcp":
		return handleIntegrationExportMcp(args[1:])
	case "help", "-h", "--help":
		return printIntegrationHelp()
	default:
		usererror.Error("unknown integration subcommand: %q — run `mpm integration help`", args[0])
		return 1
	}
}

func printIntegrationHelp() int {
	fmt.Println("Usage: mpm integration <subcommand>")
	fmt.Println()
	fmt.Println("Cross-framework config emitters. The flagship command is `export-mcp`,")
	fmt.Println("which prints the exact JSON snippet users paste into their framework's")
	fmt.Println("MCP-servers config — always pointing at the canonical $HOME/.mpm/bin/mpm-mcp")
	fmt.Println("binary, never leaving room for a stale /usr/local/bin/mpm-mcp to sneak in.")
	fmt.Println()
	fmt.Println("Subcommands:")
	fmt.Println("  export-mcp   Print the MCP-servers JSON snippet for a target framework")
	fmt.Println()
	fmt.Println("Examples:")
	fmt.Println("  mpm integration export-mcp                       # Claude Code (default)")
	fmt.Println("  mpm integration export-mcp --framework hermes   # Hermes mcp.json")
	fmt.Println("  mpm integration export-mcp --framework opencl  # OpenClaw — diff against your config")
	fmt.Println("  mpm integration export-mcp | pbcopy             # copy to clipboard")
	return 0
}
