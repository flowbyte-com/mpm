# TASK: Implement the Context Switcher (`mpm ops switch`)

We are adding an interactive context switcher for 808's behavioral modes and personas. MPM already has a file-based mode/persona system — this task extends it with a unified TUI switcher, not a parallel system.

---

## 1. Existing Architecture (Do Not Replace)

The file-based system already exists:

```
~/.mpm/                          # or MPM_WORKSPACE
├── mode/                        # mode/*.md files
│   ├── programming.md
│   ├── research.md
│   └── standard.md
├── persona/                     # persona/*.md files
│   ├── whiterabbit.md
│   └── default.md
└── active.json                  # tracks current active state
```

**How it currently works:**
- `mpm mode` / `mpm persona` — interactive pickers (already exist)
- `active.json` — `{"persona": "whiterabbit", "modes": ["programming"], "updated": "..."}`
- `detectActiveContext()` — injects active mode/persona into memory metadata on `mpm add`
- File-based = human-readable, git-friendly, no compile step

**This task:** builds a single unified `mpm ops switch` TUI that handles both mode AND persona in one place, using the existing system — not replacing it.

---

## 2. `active.json` Structure

The existing format is:
```json
{
  "persona": "whiterabbit",
  "modes": ["programming", "research"],
  "updated": "2026-05-19T17:30:00Z"
}
```

Do NOT add new fields to `active.json`. Work with this structure.

If `active.json` doesn't exist, create it with defaults:
```json
{
  "persona": "default",
  "modes": ["standard"],
  "updated": "<current time>"
}
```

---

## 3. File Discovery

```go
func getModeFiles() []string {
    // List all .md files in the mode/ directory (MPM_DIR/mode/)
    // Strip .md extension, return names
}

func getPersonaFiles() []string {
    // List all .md files in the persona/ directory
    // Strip .md extension, return names
}
```

Use `os.ReadDir` or `filepath.Glob`. If the directory doesn't exist, return empty slice (not an error).

---

## 4. `handleSwitch` Implementation

In `cmd/mpm/handlers.go`:

```go
func handleSwitch(args []string) int {
    // 1. Load active.json (or create default if missing)
    active, err := loadActiveJSON()
    if err != nil {
        fmt.Printf("[!] Error loading active.json: %v\n", err)
        return 1
    }

    // 2. Print current context
    fmt.Println("⚡ MPM Context Switcher")
    fmt.Println("─────────────────────────────────────────")
    fmt.Printf("Active Persona: %s\n", active.Persona)
    fmt.Printf("Active Modes:  %s\n", strings.Join(active.Modes, ", "))
    fmt.Println("─────────────────────────────────────────")

    // 3. Present options
    fmt.Println("\nWhat do you want to change?")
    fmt.Println("  [1] Switch Persona")
    fmt.Println("  [2] Toggle Modes")
    fmt.Println("  [3] Exit")
    fmt.Print("\n> ")

    reader := bufio.NewReader(os.Stdin)
    line, err := reader.ReadString('\n')
    if err != nil {
        fmt.Println("[!] Read error")
        return 1
    }
    line = strings.TrimSpace(line)

    switch line {
    case "1":
        switchPersona(reader, active)
    case "2":
        toggleModes(reader, active)
    case "3":
        fmt.Println("No changes made.")
        return 0
    default:
        fmt.Println("[!] Invalid option")
        return 1
    }

    // 4. Save
    active.Updated = time.Now().UTC().Format(time.RFC3339)
    if err := saveActiveJSON(active); err != nil {
        fmt.Printf("[!] Error saving: %v\n", err)
        return 1
    }

    fmt.Printf("\n⚡ Context updated: [Persona: %s] | [Modes: %s]\n",
        active.Persona, strings.Join(active.Modes, ", "))
    return 0
}
```

---

## 5. Persona Switcher

```go
func switchPersona(reader *bufio.Reader, active *ActiveState) {
    personas := getPersonaFiles()
    if len(personas) == 0 {
        fmt.Println("[!] No persona files found in persona/")
        return
    }

    fmt.Println("\nAvailable Personas:")
    for i, p := range personas {
        marker := ""
        if p == active.Persona {
            marker = " (current)"
        }
        fmt.Printf("  [%d] %s%s\n", i+1, p, marker)
    }
    fmt.Print("\nSelect persona (number): ")

    line, _ := reader.ReadString('\n')
    line = strings.TrimSpace(line)
    idx, err := strconv.Atoi(line)
    if err != nil || idx < 1 || idx > len(personas) {
        fmt.Println("[!] Invalid selection — no change made.")
        return
    }
    active.Persona = personas[idx-1]
}
```

---

## 6. Mode Toggler

```go
func toggleModes(reader *bufio.Reader, active *ActiveState) {
    modes := getModeFiles()
    if len(modes) == 0 {
        fmt.Println("[!] No mode files found in mode/")
        return
    }

    fmt.Println("\nAvailable Modes (enter numbers separated by commas, e.g. 1,3):")
    activeMap := make(map[string]bool)
    for _, m := range active.Modes {
        activeMap[m] = true
    }

    for i, m := range modes {
        marker := ""
        if activeMap[m] {
            marker = " [*]"
        }
        fmt.Printf("  [%d] %s%s\n", i+1, m, marker)
    }
    fmt.Print("\nSelect modes: ")

    line, _ := reader.ReadString('\n')
    line = strings.TrimSpace(line)

    selected := parseModeSelection(line, modes)
    if selected == nil {
        fmt.Println("[!] Invalid selection — no change made.")
        return
    }
    active.Modes = selected
}

func parseModeSelection(line string, modes []string) []string {
    parts := strings.Split(line, ",")
    var result []string
    seen := make(map[string]bool)
    for _, p := range parts {
        p = strings.TrimSpace(p)
        idx, err := strconv.Atoi(p)
        if err != nil || idx < 1 || idx > len(modes) {
            return nil // invalid
        }
        name := modes[idx-1]
        if !seen[name] {
            result = append(result, name)
            seen[name] = true
        }
    }
    return result
}
```

---

## 7. ActiveState Struct and JSON helpers

In `cmd/mpm/simple_cmds.go` (or a new `cmd/mpm/active.go`):

```go
type ActiveState struct {
    Persona string   `json:"persona"`
    Modes   []string `json:"modes"`
    Updated string   `json:"updated"`
}

func activeJSONPath() string {
    return filepath.Join(getMPMDir(), "active.json")
}

func loadActiveJSON() (*ActiveState, error) {
    path := activeJSONPath()
    data, err := os.ReadFile(path)
    if os.IsNotExist(err) {
        return &ActiveState{
            Persona: "default",
            Modes:   []string{"standard"},
            Updated: time.Now().UTC().Format(time.RFC3339),
        }, nil
    }
    if err != nil {
        return nil, err
    }
    var state ActiveState
    if err := json.Unmarshal(data, &state); err != nil {
        return nil, err
    }
    return &state, nil
}

func saveActiveJSON(s *ActiveState) error {
    data, err := json.MarshalIndent(s, "", "  ")
    if err != nil {
        return err
    }
    return os.WriteFile(activeJSONPath(), data, 0644)
}
```

---

## 8. `GetSystemPrompt()` — Read from active files, not config

Add to `cmd/mpm/simple_cmds.go`:

```go
// GetSystemPrompt reads the active persona and mode .md files and concatenates
// their frontmatter directives into a single system prompt string.
func GetSystemPrompt() string {
    active, err := loadActiveJSON()
    if err != nil {
        return ""
    }

    var parts []string

    // Read persona file
    personaPath := filepath.Join(getMPMDir(), "persona", active.Persona+".md")
    if data, err := os.ReadFile(personaPath); err == nil {
        if content := extractFrontmatterDirective(string(data)); content != "" {
            parts = append(parts, content)
        }
    }

    // Read mode files
    for _, mode := range active.Modes {
        modePath := filepath.Join(getMPMDir(), "mode", mode+".md")
        if data, err := os.ReadFile(modePath); err == nil {
            if content := extractFrontmatterDirective(string(data)); content != "" {
                parts = append(parts, content)
            }
        }
    }

    return strings.TrimSpace(strings.Join(parts, "\n\n"))
}

// extractFrontmatterDirective reads YAML frontmatter from a .md file and returns
// the "directive" or "purpose" or "description" field, whichever is found first.
func extractFrontmatterDirective(content string) string {
    // Strip everything before first `---` (start of frontmatter)
    idx := strings.Index(content, "---")
    if idx == -1 {
        return ""
    }
    body := content[idx+3:]
    endIdx := strings.Index(body, "---")
    if endIdx == -1 {
        return ""
    }
    fm := body[:endIdx]

    for _, key := range []string{"directive", "purpose", "description"} {
        prefix := key + ":"
        for _, line := range strings.Split(fm, "\n") {
            if strings.HasPrefix(strings.TrimSpace(line), prefix) {
                return strings.TrimSpace(strings.TrimPrefix(line, prefix))
            }
        }
    }
    return ""
}
```

**Where to use `GetSystemPrompt()`:**
- In `mpm add` output (optional — show which context you're operating in)
- NOT prepended to LLM synthesis prompts — the "senior archivist" framing is intentional and persona directives would conflict
- NOT in the proactive hook — it uses FTS5, not LLM

---

## 9. Graceful Fallback

- Missing `active.json` → create with defaults
- Missing persona file → fall back to "default" if exists, else first available
- Missing mode directory → show "no modes available" cleanly, no crash
- `GetSystemPrompt()` on error → returns empty string, no panic

---

## 10. Router Registration

Already done — `switch` is in `opsSubcommands` as of the CLI Simplify prompt. Just wire `handleSwitch` to it.

```go
// In opsSubcommands map, already registered:
"switch": {Name: "switch", Description: "Interactive persona/mode switcher"},
```

---

## 11. Files to Modify

- `cmd/mpm/handlers.go` — add `handleSwitch`, `switchPersona`, `toggleModes`
- `cmd/mpm/simple_cmds.go` — add `ActiveState` struct, `loadActiveJSON`, `saveActiveJSON`, `GetSystemPrompt`, `extractFrontmatterDirective`
- No new config fields
- No new tables

---

## 12. Verification

| Test | Expected |
|---|---|
| `mpm ops switch` | Shows current persona + modes, menu options, exits cleanly |
| Switch persona | Updates `active.json`, confirms with "⚡ Context updated" |
| Toggle modes (multi-select) | Parses "1,3" format, updates modes array in `active.json` |
| Invalid selection | "Invalid selection — no change made", no crash |
| Missing `active.json` | Creates with defaults silently |
| `mpm mode` / `mpm persona` (existing commands) | Still work — no regression |
| All tests pass | ✅ |