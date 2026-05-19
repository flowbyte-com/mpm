# TASK: Implement the Invisible CLI Bundle

We are streamlining the MPM CLI to feel like a natural extension of the terminal — minimal surface area, smart defaults, and an invisible layer of complexity that does exactly what you mean.

---

## 1. Stdin Detection (The Write Path)

**Goal:** `cat idea.md | mpm` should ingest the piped content without requiring an `add` verb.

**Implementation:**
- In `cmd/mpm/main.go`, before any flag parsing or routing, check `os.Stdin.Stat()` to detect if data is present in the pipe.
- If stdin has data AND no `--json` flag is passed:
  - Read all of stdin into a string.
  - Create a temporary memory record (as if `mpm add "<stdin content>"` was called).
  - Show a preview: `"Will save: <first 120 chars>..."` and ask `[y/N]`.
  - On confirmation: proceed with standard `AddMemory` pipeline (including auto-synthesis).
  - On decline or error: discard cleanly, exit 0.
- **Constraint:** The preview + confirmation is mandatory. Reading stdin silently is not acceptable — the user must see what they're about to save.
- If no piped data: proceed normally to flag parsing and routing.
- If `--json` flag is passed: route stdin data as structured JSON (future-proofing; for now, treat as plain text).

**Edge cases:**
- Empty stdin (zero bytes): skip silently, route normally.
- Binary data detected (contains null bytes or non-printable ratio above threshold): print a warning "stdin appears to be binary; skipping" and route normally.

---

## 2. The `ops` Subcommand (The Junk Drawer)

**Goal:** Keep the root `--help` clean by bundling all maintenance/diagnostic tools under a single `ops` parent.

**Root `--help` after this change:**
```
mpm <query>       — search memories (default when no flags)
mpm add <text>    — add a memory
mpm add -i        — interactive add via $EDITOR
mpm snooze <id>   — bump a memory's relevance
mpm ops           — maintenance, diagnostics, and engine-room tools
mpm help          — full help
```

**`mpm ops` subcommands (move, don't rewrite):**
```
mpm ops doctor [--explain]   — diagnostics (with optional --explain for FTS5 query plan)
mpm ops maintain            — run maintenance (decay, pruning, FTS backfill)
mpm ops synthesize          — run LLM synthesis on all memories
mpm ops gc [--dry-run]      — memory garbage collection
mpm ops watch              — start the watcher daemon
mpm ops watch --stop        — stop the watcher daemon
mpm ops watch --status      — watcher status
```

**Implementation:**
- Add a new `ops` case in `cmd/mpm/router.go`.
- `ops` is a parent command that delegates to sub-commands (`doctor`, `maintain`, `synthesize`, `gc`, `watch`).
- Use a simple sub-router within the `ops` case — no need for a full nested flag set.
- Existing handlers (`runDoctorCommand`, etc.) are unchanged — just registered under `ops` instead of at the root.
- `mpm doctor` and `mpm maintain` and `mpm synthesize` should **still work at the root** (backwards compatible alias) — but should also appear in `mpm ops` help.

**Note:** `watch`, `maintain`, `synthesize` currently live at the root via `router.go`. Add backwards-compatible aliases so both `mpm doctor` and `mpm ops doctor` work.

---

## 3. User-Defined Aliases (Personalized DWIM)

**Goal:** The alias pre-processor in `main.go` is the DWIM layer. Instead of baking magic into the root, make it configurable so each user decides what shortcuts they want.

**Config extension (`mpm_config.json`):**
```json
"aliases": {
  "s": "recall",
  "in": "add -i",
  "mem": "recall --collection memories"
}
```

**Behavior:**
- Aliases are loaded from `mpm_config.json` via existing `config.LoadConfig()`.
- Pre-processor in `main.go` intercepts `os.Args[1]` before routing.
- If `os.Args[1]` matches an alias key, expand it in-place.
- Chain expansion not required (e.g., `mpm s foo` → `mpm recall foo`, args after the expanded alias are passed through).
- `mpm help` and `mpm --version` / `-v` bypass alias resolution entirely (return immediately).
- Unknown commands pass through to the router unchanged.
- Silent fallback: unrecognized alias keys route normally (no error).

**Example flows after config:**
```
mpm s "token budget"        → mpm recall "token budget"
mpm in                      → mpm add -i
mpm mem                     → mpm recall --collection memories
```

---

## Files to Modify

- `cmd/mpm/main.go` — stdin detection (before routing), alias pre-processor (already exists, refine to support chain args)
- `cmd/mpm/router.go` — add `ops` parent command with sub-router, add backwards-compatible aliases for existing root commands
- `internal/config/config.go` — extend `Config` struct with `Aliases map[string]string`

---

## Codebase Rigidity Rules

- All DB writes route through `DatabaseManager`.
- Stdin detection happens before flag parsing; must not break existing `mpm recall --json` calls that pipe structured data.
- `ops` aliases must not break existing shell scripts that call `mpm doctor`, `mpm maintain`, etc.
- All tests must pass.

---

## Verification

- `cat idea.md | mpm` → shows preview, on `y` saves to memory
- `echo "" | mpm` → routes normally (empty stdin skipped)
- `mpm ops` → shows ops subcommand help
- `mpm ops doctor --explain` → runs EXPLAIN QUERY PLAN
- `mpm doctor` still works (backwards compatible)
- With `"mem": "recall --collection memories"` in config: `mpm mem foo` → behaves as `mpm recall --collection memories foo`
- All tests pass