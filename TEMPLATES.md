# MPM Templates

*Copy these to create new personas and modes.*

---

## Persona Template

**Save as:** `MPM/persona/<name>.persona`

```markdown
# Persona: <Name>

## Identity Override
- **Name:** <Display name>
- **Title:** <Role/title>
- **Vibe:** <Personality description>
- **Emoji:** <Optional emoji>

## Voice & Tone
- <How they speak>
- <Communication style>
- <What they avoid>

## Behavioral Rules
- <Rule 1>
- <Rule 2>
- <Rule 3>

## Context
<When/why to use this persona>

## Activation
<How to activate or auto-load triggers>

---
*Optional closing quote or tagline.*
```

**Example:** See `default.persona`, `corporate.persona`, `harley.persona`

---

## Mode Template

**Save as:** `MPM/mode/<name>.mode`

```markdown
# <Mode Name>

*One-line purpose statement (italicized).*

## Identity
- **Name:** <Mode name>
- **Purpose:** <What this mode does>
- **Vibe:** <Working style>

## Voice & Tone
- <How to approach tasks in this mode>
- <Focus areas>

## Behavioral Rules
1. <Rule 1>
2. <Rule 2>
3. <Rule 3>

## Best Practices
- <Practice 1>
- <Practice 2>

## Anti-Patterns
- ❌ <What to avoid>
- ❌ <What not to do>

## Exit Criteria
Switch back to default when:
- <Condition 1>
- <Condition 2>

---

*Optional closing quote or tagline.*

**Created by:** <Your name> | <Date>
```

**Example:** See `oracle.mode`, `programming.mode`, `debugging.mode`

---

## Workflow

1. **Create file** using template above
2. **Save to:** `MPM/persona/` or `MPM/mode/`
3. **Compile:** `mpm persona compile` or `mpm mode compile`
4. **Use:** `mpm persona` or `mpm mode` (TUI selector)

**Tip:** The TUI reads the first `#` heading for the name, and the italicized line for the description.

---

*Memory is sacred. Create with care.*

**Created by:** v & Great_808@flowbte.com | 2026-03-20
