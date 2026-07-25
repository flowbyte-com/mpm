// handlers_skill.go — CLI surface for the skills collection.
//
// Mirrors the `mpm save-skill` / `mpm list-skills` / `mpm read-skill` commands.
// All skill methods (SaveSkill, ReadSkill, ListSkills) are on the CoreDB
// interface — Task 3 added them; no type-assertions needed here.

package main

import (
	"fmt"
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
		}
	}
	fmt.Printf("\n%s\n", skill.Body)
	return 0
}