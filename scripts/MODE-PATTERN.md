# Mode Pattern - Stackable Task Behaviors

**Problem:** Personas define *who you are* but not *how you work*. Different tasks need different workflows, checklists, and best practices.

**Solution:** Modes are task-specific behavioral patterns that **stack** - run multiple simultaneously.

## Architecture

```
modes/
├── *.mode                 # Human-editable MD source files
│   ├── programming.mode   # (2-3KB, versionable, Git-friendly)
│   ├── design.mode
│   └── debugging.mode
└── loaded/
    ├── modes.db           # Compiled SQLite (compact, queryable)
    └── active_modes table # Stack tracking
```

## Key Difference: Persona vs Mode

| Aspect | Persona | Mode |
|--------|---------|------|
| **What** | Who you are | How you work |
| **Scope** | Identity, voice, personality | Task patterns, checklists, workflows |
| **Exclusivity** | One at a time (mutually exclusive) | Stack multiple (composable) |
| **Example** | "Pirate" voice | "Programming" workflow |
| **Duration** | Session-long | Task-long |
| **DB Table** | `personas` (single active) | `active_modes` (stack with order) |

## Stacking Example

```bash
# Stack multiple modes
./mode-stack.sh activate programming
./mode-stack.sh activate debugging
./mode-stack.sh activate design

# View active stack
./mode-ls.sh --active
# 1. programming
# 2. debugging
# 3. design

# Deactivate one
./mode-stack.sh deactivate debugging
# Still have: programming + design

# Clear all
./mode-stack.sh clear
```

## Mode File Structure

```markdown
# Mode: [Name]

## Purpose
[What task this mode is for]

## Behavioral Patterns
- [How to approach the task]
- [Workflow steps]

## Checklist
- [ ] Step 1
- [ ] Step 2

## Best Practices
- [Practice 1]

## Anti-Patterns (Avoid)
- [Don't do this]

## Tools & Commands
- [Tool 1]: [How to use]

## Exit Criteria
[When to exit this mode]
```

## Database Schema

```sql
-- Mode content (compiled from MD)
CREATE TABLE modes (
    name TEXT PRIMARY KEY,
    purpose TEXT,
    patterns TEXT,
    checklist TEXT,
    best_practices TEXT,
    anti_patterns TEXT,
    tools TEXT,
    exit_criteria TEXT,
    compiled_at TIMESTAMP,
    updated_at TIMESTAMP
);

-- File tracking (sync state)
CREATE TABLE mode_files (
    name TEXT PRIMARY KEY,
    file_path TEXT,
    file_hash TEXT,
    last_synced TIMESTAMP,
    status TEXT
);

-- Active stack (supports multiple)
CREATE TABLE active_modes (
    name TEXT PRIMARY KEY,
    activated_at TIMESTAMP,
    stack_order INTEGER  -- 0, 1, 2, ... (order matters)
);
```

## Commands

### Compile
```bash
./mode-compile.sh sync          # Sync all modes to DB
./mode-compile.sh compile <n>   # Compile single mode
./mode-compile.sh list          # List loaded in DB
./mode-compile.sh status        # Check sync status
```

### Stack Management
```bash
./mode-stack.sh activate <n>    # Activate (add to stack)
./mode-stack.sh deactivate <n>  # Deactivate (remove from stack)
./mode-stack.sh clear           # Deactivate all
```

### List & Query
```bash
./mode-ls.sh                    # Simple list
./mode-ls.sh --loaded           # Show loaded in DB
./mode-ls.sh --active           # Show active stack
./mode-ls.sh --tree             # Tree structure
./mode-dashboard.sh             # Full dashboard
```

## Workflow Example

```bash
# Start coding session
./mode-stack.sh activate programming
# → Loads: code patterns, TDD, clear naming

# Hit a bug
./mode-stack.sh activate debugging
# → Stacks: reproduce, read errors, binary search
# Now running: programming + debugging

# Need to design UI
./mode-stack.sh activate design
# → Stacks: user-first, consistency, accessibility
# Now running: programming + debugging + design

# Fix bug, exit debugging mode
./mode-stack.sh deactivate debugging
# Still running: programming + design

# Done for day
./mode-stack.sh clear
# All modes deactivated
```

## Token Savings

| Operation | MD Load | DB Query | Savings |
|-----------|---------|----------|---------|
| List modes | 6KB (~900 tokens) | 100 bytes (~50 tokens) | **94%** |
| Check active | 6KB (~900 tokens) | 50 bytes (~25 tokens) | **97%** |
| Activate | 2KB (~300 tokens) | 2KB (~300 tokens) | 0% (need full) |

## Benefits

### Stacking Power
- **Programming** + **Debugging** = Write code with bug-hunting mindset
- **Programming** + **Design** = Write code with UX awareness
- **Debugging** + **Design** = Fix bugs without breaking UX
- **All three** = Full-stack developer mode

### Composability
- Modes are orthogonal (don't conflict)
- Stack order matters (first activated = base context)
- Deactivate individually (fine-grained control)
- Clear all (quick reset)

### Task-Specific
- **Programming**: Code quality, testing, refactoring
- **Design**: User-first, visual hierarchy, accessibility
- **Debugging**: Systematic investigation, root cause
- **Writing**: Clear communication, audience awareness
- **Research**: Deep dive, source verification, synthesis

## Future: Auto-Activation

Skill could detect context and suggest modes:
- Editing code → suggest `programming.mode`
- User says "bug" → suggest `debugging.mode`
- Editing design files → suggest `design.mode`
- Multiple signals → stack multiple modes

```
Context: Fixing CSS layout bug in React component
Signals: "bug" + ".css" + "layout"
Suggest: stack debugging + design + programming
```

---

*Modes make you effective. Stack them for superpowers.* 🛠️⚡
