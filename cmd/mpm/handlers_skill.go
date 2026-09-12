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
//	mpm skill workshop --help
//
// The payload may be a bare WorkshopRequest, or already wrapped in
// {action:"workshop", params:{...}}. Bare payloads are wrapped
// automatically so the user does not need to know the MCP envelope.
//
// `--help` short-circuits to printSkillWorkshopHelp() (matches the
// pattern in handleSaveSkill at handlers_skill.go:25-29). Without it,
// the JSON parser below would reject `--help` as "unknown flag" and
// operators would have nowhere to discover the schema.
func handleSkillWorkshop(args []string) int {
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "help" {
			printSkillWorkshopHelp()
			return 0
		}
	}
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

// printSkillWorkshopHelp prints the canonical usage for
// `mpm skill workshop`. This is the operator-facing help page for the
// form/refine pipeline; it documents the boundary vocabulary, the
// change_type requirement, the three outcomes, and one complete
// example per mode using the actual current schema. A user reading
// only this page should have enough information to invoke the
// workshop without consulting the substrate source.
//
// Source of truth: docs/archive/2026-08-28-mpm-skill-workshop-design.md
// and agent_installation/mpm-agent-protocol.md §3.1.
func printSkillWorkshopHelp() {
	fmt.Println(`mpm skill workshop — Skills Workshop (form | refine)

What it does
  The Skills Workshop is the structured workflow for turning a repeated,
  non-obvious experience into a durable, reusable skill. It runs the
  same pipeline whether invoked from this CLI or from the MCP
  mpm_skills tool with action="workshop". The CLI is a thin shim —
  all validation lives in the substrate.

Modes
  form    Author a NEW skill. The proposal becomes the candidate
          record. Decision-model gate decides whether it is published
          or returned as a candidate (the agent then accepts/rejects).

  refine  Revise an EXISTING skill. The "intent" field is the skill's
          current name. "change_type" is REQUIRED and selects the
          version bump deterministically (see change_type below).

Required inputs (both modes)
  mode                  "form" or "refine"
  intent                (refine only) the existing skill's name
  change_type           (refine only) see below
  decision_model        see "Decision model" below
  proposal              name | version | when_to_use | steps | ...

Decision model
  decision_model.reusability        integer 0..5
  decision_model.non_obviousness    integer 0..5
  decision_model.stability          integer 0..5
  decision_model.leverage           integer 0..5
  decision_model.boundary           one of: procedure | judgment | knowledge

  Total = sum of the four axes (0..20).

  Decision rule:
    boundary != procedure      -> rejected (non-procedure is not skill-worthy)
    total <= 3                 -> rejected
    total 4..5                 -> candidate
    total >= 6                 -> published (subject to when_to_use, duplicate,
                                  validation downgrades)

change_type (refine only — REQUIRED)
  correction       patch     (1.0.0 -> 1.0.1)
  extension        minor     (1.0.0 -> 1.1.0)
  restructuring    minor     (1.0.0 -> 1.1.0)
  purpose_change   major     (1.0.0 -> 2.0.0)

  The workshop derives the next version from change_type + the
  existing skill's current version. Do NOT pick the version yourself —
  supply change_type and let the pipeline enforce the bump.

Outcomes
  published   Skill was written through the normal skill persistence
              path. For refine, the prior version is deprecated via
              SaveSkillAndDeprecatePrior. "skill_id" is populated.
  candidate   Workshop generated a proposal but did not publish.
              Inspect decision_model / duplicate_check / validation.
              "save_payload" is the exact params dict to hand off to
              "mpm_skills save" if you accept the candidate.
  rejected    Not skill-worthy. "reason" names the gate that fired.

Usage
  mpm skill workshop --file <path>      # read WorkshopRequest JSON
  cat request.json | mpm skill workshop # read from stdin

The JSON payload may be a bare WorkshopRequest, or already wrapped in
{"action":"workshop","params":{...}}. Bare payloads are accepted.

Examples

  # form — author a new skill
  mpm skill workshop --file form.json
  # form.json:
  {
    "mode": "form",
    "decision_model": {
      "reusability": 2, "non_obviousness": 2, "stability": 2, "leverage": 2,
      "boundary": "procedure"
    },
    "proposal": {
      "name": "release-checklist",
      "version": "1.0.0",
      "domain": "release",
      "description": "Pre-release smoke checks for MPM changes",
      "when_to_use": "before cutting a release, smoke-checking migrations, scheduler, and CLI",
      "steps": [{"call": "run the smoke test"}],
      "constraints": []
    },
    "task_context": "Repeated pre-release sequence",
    "workflow_description": "check migrations, scheduler, CLI",
    "failure_recovery": ""
  }

  # refine — bump a correction on an existing skill
  mpm skill workshop --file refine.json
  # refine.json:
  {
    "mode": "refine",
    "intent": "release-checklist",
    "change_type": "correction",
    "decision_model": {
      "reusability": 2, "non_obviousness": 2, "stability": 2, "leverage": 2,
      "boundary": "procedure"
    },
    "proposal": {
      "name": "release-checklist",
      "version": "1.0.1",
      "description": "Pre-release smoke checks for MPM changes",
      "when_to_use": "before cutting a release, smoke-checking migrations, scheduler, and CLI",
      "steps": [{"call": "run the smoke test"}]
    }
  }

MCP equivalent (canonical for agents)
  mpm call mpm_skills --payload '{"action":"workshop","params":{<workshop_request>}}'`)
}