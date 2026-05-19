# TASK: Implement the Epistemology Engine (Wishlist #6 — Full Completion)

We are closing out the Epistemology Engine — the decision ledger and theory tracker that makes MPM a genuine knowledge partner rather than a passive store.

The core concept: **808 reasons explicitly.** It records hypotheses with validation criteria, tracks when they're resolved, and maintains an audit trail of decisions with their rationale. Everything is in the database. This task makes it accessible.

---

## 1. Data Model & Storage

The following collections already exist in `memories`:
- `theories` — HYPOTHESIS-format memories
- `decisions` — CHOICE-format memories

The following topics **must exist** in `topics`:
- `theories` — the topic that theories belong to
- `decisions` — the topic that decisions belong to

**Auto-create on first use:** Before saving any memory to `theories` or `decisions` collection, call `dm.GetOrCreateTopic("theories")` / `dm.GetOrCreateTopic("decisions")`. If the topic already exists, `GetOrCreateTopic` is a no-op (idempotent).

**Auto-link on save:** After `AddMemory` succeeds for `theories` or `decisions` collection, call `dm.AddMemoryToTopic(memID, topicID, "primary")` to create the `topic_memberships` link.

---

## 2. CLI Commands

### 2a. `mpm propose_theory <text>`

Parses a structured text blob into a theory memory and saves it to the `theories` collection.

**Input format** (flexible — match these prefixes case-insensitively):
```
HYPOTHESIS: <what you believe>
VALIDATION_CRITERIA: <how to know if it's true>
STATUS: pending  (or: resolved, disproven, pending)
TAGS: optional, comma-separated
```

**Behavior:**
1. Parse the input. If `HYPOTHESIS:` is not found, wrap the full text in `HYPOTHESIS: <text>`.
2. Extract `STATUS:` if present (default: `pending`).
3. Extract `TAGS:` if present.
4. Call `dm.AddMemory()` with `collection = "theories"`.
5. Auto-link to `theories` topic via `GetOrCreateTopic` + `AddMemoryToTopic`.
6. Print the new memory ID and confirm.

**Example:**
```
mpm propose_theory "HYPOTHESIS: the epistemology engine is now working
VALIDATION_CRITERIA: write a theory, recall it, verify FTS5 search works on decisions and theories
STATUS: pending"
→ saves to theories collection, links to theories topic, prints ID
```

### 2b. `mpm resolve_theory <id> <conclusion>`

Updates an existing theory's metadata to mark it as resolved.

**Behavior:**
1. Look up memory by ID.
2. If not found or not in `theories` collection → error.
3. Update metadata:
   - `status` → `resolved`
   - `conclusion` → `<conclusion text>`
   - `resolved_at` → current timestamp (UTC RFC3339)
4. Bump memory weight by +1 (reinforce the resolved theory).
5. Print confirmation with ID and conclusion.

**Error cases:**
- Unknown ID → "Theory not found: <id>"
- Wrong collection → "Memory <id> is not a theory (collection: <name>)"

### 2c. `mpm record_decision <text>`

Parses a structured decision record and saves it to the `decisions` collection.

**Input format** (flexible):
```
CONTEXT: <background or situation>
CHOICE: <what was decided>
RATIONALE: <why this choice was made>
TAGS: optional
```

**Behavior:**
1. Parse the input. If `CHOICE:` is not found, wrap full text in `CHOICE: <text>`.
2. Extract `CONTEXT:` and `RATIONALE:` if present.
3. Call `dm.AddMemory()` with `collection = "decisions"`.
4. Auto-link to `decisions` topic.
5. Print the new memory ID.

### 2d. `mpm theories [pending|resolved|all]`

List theories with status chips.

**Behavior:**
- Default (`mpm theories`): shows all theories with status
- `mpm theories pending`: filters to `STATUS: pending`
- `mpm theories resolved`: filters to `STATUS: resolved`
- Output format: one line per theory
  ```
  [<id>] <HYPOTHESIS first 80 chars>...  [status: pending]
  ```
- For each theory: show id + first line of HYPOTHESIS + status chip

### 2e. `mpm decisions`

Display the decision ledger.

**Behavior:**
- Query all memories in `decisions` collection.
- For each: print formatted decision:
  ```
  ─────────────────────
  CONTEXT: <context text or "—">
  CHOICE:  <choice text>
  RATIONALE: <rationale text or "—">
  [yyyy-mm-dd]
  ─────────────────────
  ```
- Empty state: "No decisions recorded yet. Run `mpm record_decision` to log your first decision."

---

## 3. Backfill — Link Existing Memories to Topics

When the epistemology engine initializes (or on first run), backfill the 4 existing memories:

**Query:**
```sql
SELECT id, collection, content FROM memories WHERE collection IN ('theories', 'decisions') AND deleted_at IS NULL;
```

**For each result:**
1. `GetOrCreateTopic` for the collection name
2. `AddMemoryToTopic(memoryID, topicID, "primary")` — skip if already linked

**Implementation:** Add a `backfillEpistemologyTopics()` function. Call it:
- At the start of `runDoctorCommand()` (so it runs when any doctor/maintenance command fires)
- Or: as part of the `ops maintain` flow before decay/pruning runs

---

## 4. Auto-Ingestion Hook

When the watcher daemon ingests a `.md` file that contains epistemology-style content, it should detect and route correctly:

**Detection rules:**
- If content contains `HYPOTHESIS:` → save to `theories` collection
- If content contains `CHOICE:` → save to `decisions` collection
- Normal `.md` files → save to `memories` as before

**Implementation:** In `processMarkdownFile()` (watch.go), before calling `AddMemory`, check for these prefixes. If found, override the collection to `theories` or `decisions`.

---

## 5. Router Registration

Add these commands to `cmd/mpm/router.go`:

```go
"propose_theory": {Name: "propose_theory", Description: "Record a hypothesis with validation criteria", MinArgs: 1},
"resolve_theory":  {Name: "resolve_theory",  Description: "Mark a theory as resolved", MinArgs: 2},
"record_decision": {Name: "record_decision", Description: "Record a decision with context, choice, and rationale", MinArgs: 1},
"theories":        {Name: "theories",        Description: "List theories [pending|resolved|all]", MinArgs: 0},
"decisions":       {Name: "decisions",       Description: "Show decision ledger", MinArgs: 0},
```

---

## Files to Modify

- `cmd/mpm/handlers.go` — add `handleProposeTheory`, `handleResolveTheory`, `handleRecordDecision`, `handleTheories`, `handleDecisions`
- `cmd/mpm/router.go` — register the 5 new commands
- `cmd/mpm/watch.go` — add epistemology-style content detection in `processMarkdownFile`
- `cmd/mpm/simple_cmds.go` — add backfill helper `backfillEpistemologyTopics`, call from doctor/maintain
- `cmd/mpm/doctors.go` — call backfill before doctor runs

---

## Verification

- `mpm propose_theory "HYPOTHESIS: foo is true\nVALIDATION_CRITERIA: test it"` → saves to theories, links to topic, prints ID
- `mpm theories` → lists all theories with status chips
- `mpm resolve_theory <id> "confirmed"` → updates metadata, bumps weight
- `mpm record_decision "CHOICE: used X\nCONTEXT: needed Y\nRATIONALE: because Z"` → saves to decisions
- `mpm decisions` → shows formatted ledger
- Backfill: `SELECT * FROM topic_memberships` shows links for existing theories/decisions memories
- `mpm doctor` output includes backfill confirmation: "Epistemology topics initialized"
- All tests pass

---

## Edge Cases

- Malformed input (no HYPOTHESIS/CHOICE prefix): auto-wrap full text as the body
- `resolve_theory` on a non-theory: clear error message
- `resolve_theory` on an already-resolved theory: update the conclusion anyway (idempotent)
- Duplicate backfill: `AddMemoryToTopic` uses `INSERT OR IGNORE` — safe to run multiple times
- Empty `theories` or `decisions` list: print "No theories/decisions yet"