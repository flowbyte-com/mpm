// handlers_skill.go — CLI surface for the skills collection.
//
// Mirrors the `mpm save-skill` / `mpm list-skills` / `mpm read-skill` commands.
// All skill methods (SaveSkill, ReadSkill, ListSkills) are on the CoreDB
// interface — Task 3 added them; no type-assertions needed here.

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/flowbyte-com/mpm-core"
)

func handleSaveSkill(args []string) int {
	var path string
	force := false
	name := ""
	version := ""
	i := 0
	for i < len(args) {
		switch args[i] {
		case "--file", "-f":
			if i+1 >= len(args) {
				printError("--file requires a path")
				return 1
			}
			i++
			path = args[i]
		case "--force":
			force = true
		case "--name":
			if i+1 >= len(args) {
				printError("--name requires a value")
				return 1
			}
			i++
			name = args[i]
		case "--version":
			if i+1 >= len(args) {
				printError("--version requires a value")
				return 1
			}
			i++
			version = args[i]
		default:
			printError("unknown flag: %s", args[i])
			return 1
		}
		i++
	}

	if path == "" {
		printError("--file is required")
		return 1
	}

	content, err := os.ReadFile(path)
	if err != nil {
		printError("read file: %v", err)
		return 1
	}

	// If --name or --version weren't provided, parse them from frontmatter.
	fm, _, err := internal.ParseSkillFrontmatter(string(content))
	if err != nil {
		printError("parse frontmatter: %v", err)
		return 1
	}
	if name == "" {
		name = fm.Name
	}
	if version == "" {
		version = fm.Version
	}

	dm := getDB()
	if dm == nil {
		return 1
	}

	id, err := dm.SaveSkill(name, version, string(content), "cli", force)
	if err != nil {
		printError("save skill: %v", err)
		return 1
	}
	fmt.Printf("Saved skill %s (id=%s)\n", name, id)
	return 0
}

func handleListSkills(args []string) int {
	// Stage S4 of the CLI refactor (2026-09-06): scope is now
	// strictly validated via the canonical parseEnum helper. The
	// pre-S4 code accepted ANY string and passed it to
	// dm.ListSkills, which silently fell through to a default
	// scope (returning every skill) on any unknown value —
	// hiding operator typos as empty or full lists depending
	// on what the DM-side default returned. The canonical
	// vocabulary is all | local | shared (matches the help
	// text "scope: all|local|shared"). Empty input is the
	// "omitted" case (default to all); non-empty but invalid
	// produces a deterministic error listing allowed values.
	allowedScopes := []string{"all", "local", "shared"}
	scope := "all"
	if len(args) > 0 {
		v, err := parseEnum(args[0], "scope", allowedScopes)
		if err != nil {
			printError("%v", err)
			return 1
		}
		scope = v
	}
	dm := getDB()
	if dm == nil {
		return 1
	}
	skills, err := dm.ListSkills(scope)
	if err != nil {
		printError("list skills: %v", err)
		return 1
	}
	fmt.Printf("Skills (%d, scope=%s):\n", len(skills), scope)
	for _, s := range skills {
		marker := "  "
		if s.IsGlobal {
			marker = "* "
		}
		fmt.Printf("%s%s v%s — %s (weight=%d)\n", marker, s.Name, s.Version, s.WhenToUse, s.Weight)
	}
	return 0
}

func handleReadSkill(args []string) int {
	if len(args) < 1 {
		printError("usage: mpm read-skill <name> [version]")
		return 1
	}
	name := args[0]
	version := ""
	if len(args) > 1 {
		version = args[1]
	}
	dm := getDB()
	if dm == nil {
		return 1
	}
	skill, err := dm.ReadSkill(name, version)
	if err != nil {
		printError("read skill: %v", err)
		return 1
	}
	fmt.Printf("# %s v%s\n", skill.Name, skill.Version)
	if skill.WhenToUse != "" {
		fmt.Printf("\nWhen to use: %s\n", skill.WhenToUse)
	}
	if len(skill.Constraints) > 0 {
		fmt.Printf("\nConstraints:\n")
		for _, c := range skill.Constraints {
			fmt.Printf("  - %s\n", c)
		}
	}
	if len(skill.Steps) > 0 {
		fmt.Printf("\nSteps:\n")
		for i, s := range skill.Steps {
			fmt.Printf("  %d. %s\n", i+1, s.Call)
			if s.ArgsFrom != "" {
				fmt.Printf("     args_from: %s\n", s.ArgsFrom)
			}
		}
	}
	fmt.Printf("\n%s\n", skill.Body)
	return 0
}

// handleSkillWorkshop runs the Skill Workshop pipeline via the CLI.
// Reads a WorkshopRequest payload (JSON) from --file or stdin, then
// invokes internal.RunWorkshop directly. The CLI is a thin shim —
// all validation lives in the workshop itself.
//
// Usage:
//
//	mpm skill workshop --file request.json
//	echo '{"mode":"form",...}' | mpm skill workshop
//
// The payload may be a bare WorkshopRequest, or already wrapped in
// {action:"workshop", params:{...}}. Bare payloads are wrapped
// automatically so the user does not need to know the MCP envelope.
func handleSkillWorkshop(args []string) int {
	var path string
	i := 0
	for i < len(args) {
		switch args[i] {
		case "--file", "-f":
			if i+1 >= len(args) {
				printError("--file requires a path")
				return 1
			}
			i++
			path = args[i]
		default:
			printError("unknown flag: %s", args[i])
			return 1
		}
		i++
	}

	var payload []byte
	var err error
	if path != "" {
		payload, err = os.ReadFile(path)
		if err != nil {
			printError("read file: %v", err)
			return 1
		}
	} else {
		payload, err = io.ReadAll(os.Stdin)
		if err != nil {
			printError("read stdin: %v", err)
			return 1
		}
	}

	// Bare payload → map to WorkshopRequest. The MCP path uses
	// {action:"workshop",params:{...}}, but for ergonomics the CLI
	// accepts a bare WorkshopRequest too.
	var probe map[string]interface{}
	if err := json.Unmarshal(payload, &probe); err != nil {
		printError("parse JSON payload: %v", err)
		return 1
	}
	var req internal.WorkshopRequest
	if _, hasParams := probe["params"]; hasParams {
		// Already wrapped; extract params into WorkshopRequest.
		if err := mapToWorkshopRequest(probe["params"], &req); err != nil {
			printError("parse params: %v", err)
			return 1
		}
	} else {
		if err := mapToWorkshopRequest(probe, &req); err != nil {
			printError("parse payload: %v", err)
			return 1
		}
	}

	dm := getDBConcrete()
	if dm == nil {
		return 1
	}
	resp, err := internal.RunWorkshop(dm, &req)
	if err != nil {
		printError("workshop: %v", err)
		return 1
	}
	out, err := json.MarshalIndent(resp, "", "  ")
	if err != nil {
		printError("marshal response: %v", err)
		return 1
	}
	fmt.Println(string(out))
	return 0
}

// mapToWorkshopRequest round-trips a generic JSON map into a strongly
// typed internal.WorkshopRequest. Unknown fields are dropped silently
// (matches the MCP handler's behavior in internal/core/tools/handlers.go).
func mapToWorkshopRequest(src interface{}, dst *internal.WorkshopRequest) error {
	b, err := json.Marshal(src)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, dst)
}