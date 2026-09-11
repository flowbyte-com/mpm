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
	// R3: --help / -h / "help" short-circuit. See handleMemoryAdd for
	// the rationale; same defect, same fix. Round 9 T52: the help text
	// documents that the schema requires a `version` field (either
	// via the YAML frontmatter `version:` key, or via --version on
	// the CLI) — the canonical skill id is `skill:<name>-v<version>`,
	// so a missing version yields an unambiguous error.
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "help" {
			return handleSaveSkillHelp()
		}
	}
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

// handleSaveSkillHelp prints the canonical usage for the save-skill
// CLI surface. Round 9 T52: documents the schema-required `version`
// field (either via YAML frontmatter or `--version`) so operators
// discover why their save failed when version is missing.
//
// Architectural note: the skill id is `skill:<name>-v<version>` — the
// `-v<version>` suffix disambiguates edits over time, but it requires
// a non-empty version string. We deliberately do NOT default to
// "0.0.0" or a content hash, because both obscure the authoring
// intent and break downstream tooling that parses the id suffix. The
// schema contract is "version is required"; the help advertises that
// explicitly.
func handleSaveSkillHelp() int {
	fmt.Println(`mpm save-skill — Save a skill from a markdown file

Usage:
  mpm save-skill --file <path> [--name <name>] [--version <version>] [--force]
  mpm skill add --file <path> [--name <name>] [--version <version>] [--force]
  mpm skill save --file <path> [--name <name>] [--version <version>] [--force]

Required schema fields (the markdown frontmatter MUST define both):
  name:    skill name (matches the file's first # heading if --name omitted)
  version: skill version, e.g. "1.0.0" (no version → save fails with
           "skill version must not be empty"; --version or frontmatter
           version: key supplies it)

Other frontmatter keys (description, when_to_use, inputs, outputs) are
optional. See 'mpm docs skill' or the canonical SKILL.md template.

Examples:
  mpm save-skill --file ./SKILL.md                  # uses frontmatter name+version
  mpm save-skill --file ./SKILL.md --version 2.0.0  # CLI overrides frontmatter
  mpm save-skill --file ./SKILL.md --name different --version 0.1.0  # rename via CLI`)
	return 0
}

func handleListSkills(args []string) int {
	scope := "all"
	if len(args) > 0 {
		scope = args[0]
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