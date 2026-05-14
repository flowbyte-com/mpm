# MPM v2.0: Backend JSON Fixes, Router Rename & TypeScript Plugin Expansion

Expanding the MPM (Memory Persistence Module) OpenClaw plugin to expose the full Go backend capability set. Three phases: fix JSON output in Go handlers, rename the directives router, then add 8 new TypeScript tools.

---

## Phase 0: Go Backend JSON Output Fixes

The Go handlers currently output human-readable plain text. Each handler needs a `--json` flag and structured JSON output for tool integration. Implement this consistently across all 11 affected handlers.

### Reference Handlers (`cmd/mpm/simple_cmds.go`)

#### `handleRefAdd` — add reference document
- **Current:** only `--tag` flag, plain text output
- **Add:** `jsonOutput := fs.Bool("json", false, "Output JSON for tool integration")`
- **JSON output when flag set:**
  ```json
  {"success": true, "id": "<doc-id>", "title": "<title>", "total_chunks": <n>, "tags": "<tags>"}
  ```
- **On error:** `{"success": false, "error": "<message>"}`

#### `handleRefList` — list all references
- **Current:** plain text table, no `--json` support
- **Add:** `jsonOutput := fs.Bool("json", false, ...)`
- **JSON output when flag set:**
  ```json
  {"references": [{"id": "...", "title": "...", "total_chunks": 5, "tags": "...", "created_at": "..."}]}
  ```
- **Empty state:** `{"references": [], "message": "No references stored"}`

#### `handleRefSearch` — search reference chunks
- **Current:** prints human-readable results, no `--json`
- **Add:** `jsonOutput := fs.Bool("json", false, ...)`
- **JSON output when flag set:**
  ```json
  {"query": "<search>", "results": [{"doc_id": "...", "doc_title": "...", "chunk_index": 0, "content": "...", "score": 0.95}]}
  ```
- **No results:** `{"query": "<search>", "results": [], "message": "No results found"}`

#### `handleRefShow` — show reference with chunks
- **Current:** plain text, no `--json`
- **Add:** `jsonOutput := fs.Bool("json", false, ...)`
- **JSON output when flag set:**
  ```json
  {"id": "...", "title": "...", "tags": "...", "created_at": "...", "chunks": [{"index": 0, "content": "..."}]}
  ```

### Lesson Handlers (`cmd/mpm/handlers.go`)

#### `handleLessonAdd` — add a lesson
- **Current:** `--type`, `--tags` flags, plain text output
- **Add:** flag + JSON output: `{"success": true, "id": "<id>", "type": "<type>", "reinforcement": <n>}`

#### `handleLessonList` — list lessons
- **Current:** `--type=` flag support, plain text output
- **Add:** `jsonOutput := fs.Bool("json", false, ...)`
- **JSON output when flag set:**
  ```json
  {"lessons": [{"id": "...", "type": "warning|practice|insight", "content": "...", "tags": [...], "created_at": "..."}]}
  ```
- **Empty state:** `{"lessons": [], "message": "No lessons stored"}`

#### `handleLessonSearch` — search lessons
- **Current:** plain text output, no `--json`
- **Add:** `jsonOutput := fs.Bool("json", false, ...)`
- **JSON output when flag set:**
  ```json
  {"query": "<search>", "results": [{"id": "...", "type": "...", "content": "...", "tags": [...], "created_at": "..."}]}
  ```

### Topic Handlers (`cmd/mpm/handlers.go`)

#### `handleTopicAdd` — add a topic
- **Current:** plain text confirmation, no `--json`
- **Add:** `jsonOutput := fs.Bool("json", false, ...)`
- **JSON output when flag set:** `{"success": true, "id": "<id>", "name": "<name>", "description": "<desc>"}`

#### `handleTopicSearch` — search topics
- **Current:** plain text output, no `--json`
- **Add:** `jsonOutput := fs.Bool("json", false, ...)`
- **JSON output when flag set:**
  ```json
  {"query": "<search>", "results": [{"id": "...", "name": "...", "description": "...", "created_at": "..."}]}
  ```

#### `handleTopicList` — list topics
- **Current:** plain text output, no `--json`
- **Add:** `jsonOutput := fs.Bool("json", false, ...)`
- **JSON output when flag set:**
  ```json
  {"topics": [{"id": "...", "name": "...", "description": "...", "created_at": "..."}]}
  ```

#### `handleTopicShow` — show topic by ID
- **Current:** plain text, no `--json`
- **Add:** `jsonOutput := fs.Bool("json", false, ...)`
- **JSON output when flag set:**
  ```json
  {"id": "...", "name": "...", "description": "...", "created_at": "...", "memory_ids": [...], "chunk_count": <n>}
  ```

### Directives Handler (`cmd/mpm/handlers.go`)

#### `handleDirectives` (renamed from `handlePrimeDirectives`) — read prime directives
- **Current:** queries `memories` table where `is_prime_directive = 1`, outputs plain text with emoji/boxes
- **Add:** `jsonOutput := fs.Bool("json", false, ...)`
- **JSON output when flag set:**
  ```json
  {"directives": [{"id": "...", "collection": "...", "content": "...", "created_at": "..."}]}
  ```
- **Empty state:** `{"directives": [], "message": "No prime directives found"}`

### Implementation Notes

- Use the same pre-scan pattern for `--json` as `handleRecall` in `recall.go`:
  ```go
  jsonOutput := fs.Bool("json", false, "Output JSON for tool integration")
  // Pre-scan for --json since callers may place it after the query
  for _, arg := range args {
      if arg == "--json" || arg == "-j" {
          *jsonOutput = true
      }
  }
  ```
- Output format: `fmt.Println(string(json.Marshal(result)))` for JSON mode; keep existing `respond()` for plain text mode
- Ensure all JSON outputs are valid, compact, and parseable by `python3 -c "import json; ..."` without errors

---

## Phase 1: Go Backend Router Rename

Rename `prime-directives` → `directives` throughout the CLI router and help text.

### `cmd/mpm/handlers.go`
- Rename `handlePrimeDirectives()` → `handleDirectives()`
- Update the switch case in `router.go` (or wherever routing happens): `case "prime-directives":` → `case "directives":`

### `cmd/mpm/main.go`
- Line ~1103: `fmt.Println("  prime-directives View current behavioral rules")` → `fmt.Println("  directives       View current behavioral rules")`
- Line ~1134: `{"prime-directives", "Show 808 directives", false}` → `{"directives", "Show current directives", false}`

### Quicklinks display (`cmd/mpm/main.go`)
- Update any "prime-directives" references in `PrintQuicklinks()` and help sections to `directives`

### `openclaw.plugin.json` and `src/index.ts`
- The name/description can stay as "MPM (Memory Persistence Module)" — this rename only affects the CLI command name the agent types, not the product branding

---

## Phase 2: TypeScript Plugin Expansion (`openclaw/mpm-plugin/src/index.ts`)

Leave `query_long_term_memory` and `save_to_memory` completely intact. Add 8 new tools alongside them. All must use `runMpm` and `parseMpmResult` consistently, always passing `--json` to the Go binary.

### Tool: `save_lesson`
- **Schema:**
  ```typescript
  const SAVE_LESSON_SCHEMA = {
    type: "object",
    properties: {
      fact: { type: "string", description: "The lesson content — what was learned or observed." },
      type: { type: "string", enum: ["warning", "practice", "insight"], description: "Lesson type.", default: "insight" },
      tags: { type: "array", items: { type: "string" }, description: "Optional tags for retrieval.", default: [] }
    },
    required: ["fact"],
    additionalProperties: false,
  } as const;
  ```
- **Execute:** `runMpm(["lesson", "add", fact, "--type", type, "--tags", tags.join(","), "--json"])`
- **Parse** result; return `{id, success, type, reinforcement}` in tool result

### Tool: `search_lessons`
- **Schema:**
  ```typescript
  const SEARCH_LESSONS_SCHEMA = {
    type: "object",
    properties: {
      query: { type: "string", description: "Search query for lessons." }
    },
    required: ["query"],
    additionalProperties: false,
  } as const;
  ```
- **Execute:** `runMpm(["lesson", "search", query, "--json"])`
- **Parse** JSON results array; format as readable text for LLM context

### Tool: `list_lessons`
- **Schema:**
  ```typescript
  const LIST_LESSONS_SCHEMA = {
    type: "object",
    properties: {
      type: { type: "string", enum: ["warning", "practice", "insight"], description: "Filter by lesson type (optional)." }
    },
    additionalProperties: false,
  } as const;
  ```
- **Execute:** `runMpm(["lesson", "list", type ? "--type=" + type : "", "--json"].filter(Boolean))`
- **Parse** JSON results; format as readable list

### Tool: `create_topic`
- **Schema:**
  ```typescript
  const CREATE_TOPIC_SCHEMA = {
    type: "object",
    properties: {
      name: { type: "string", description: "Topic name." },
      description: { type: "string", description: "Optional description." }
    },
    required: ["name"],
    additionalProperties: false,
  } as const;
  ```
- **Execute:** `runMpm(["topic", "add", name, description || "", "--json"])`
- **Parse** JSON response

### Tool: `search_topics`
- **Schema:**
  ```typescript
  const SEARCH_TOPICS_SCHEMA = {
    type: "object",
    properties: {
      query: { type: "string", description: "Search query for topics." }
    },
    required: ["query"],
    additionalProperties: false,
  } as const;
  ```
- **Execute:** `runMpm(["topic", "search", query, "--json"])`
- **Parse** JSON results

### Tool: `add_reference`
- **Schema:**
  ```typescript
  const ADD_REFERENCE_SCHEMA = {
    type: "object",
    properties: {
      filepath: { type: "string", description: "Absolute path to the document to ingest." },
      title: { type: "string", description: "Optional title for the document." }
    },
    required: ["filepath"],
    additionalProperties: false,
  } as const;
  ```
- **Execute:** `runMpm(["reference", "add", filepath, title ? "--title", title : "", "--json"].filter(Boolean))`
- **Parse** JSON; return `{success, id, total_chunks}`

### Tool: `search_references`
- **Schema:**
  ```typescript
  const SEARCH_REFERENCES_SCHEMA = {
    type: "object",
    properties: {
      query: { type: "string", description: "Search string for reference content." },
      limit: { type: "number", description: "Max results (default: 5).", default: 5 }
    },
    required: ["query"],
    additionalProperties: false,
  } as const;
  ```
- **Execute:** `runMpm(["reference", "search", query, String(limit || 5), "--json"])`
- **Parse** JSON results; format chunks as readable text for LLM

### Tool: `list_references`
- **Schema:** empty `{}` — no parameters
- **Execute:** `runMpm(["reference", "ls", "--json"])`
- **Parse** JSON `references` array; format as list for LLM

### Tool: `read_directives`
- **Schema:** empty `{}` — no parameters
- **Execute:** `runMpm(["directives", "--json"])`
- **Parse** JSON `directives` array; format as readable list of operating principles

### Registration

Add all 8 new tools to the `register(api)` block:
```typescript
api.registerTool((ctx) => makeSaveLessonTool(ctx), { names: ["save_lesson"], optional: false });
api.registerTool((ctx) => makeSearchLessonsTool(ctx), { names: ["search_lessons"], optional: false });
api.registerTool((ctx) => makeListLessonsTool(ctx), { names: ["list_lessons"], optional: false });
api.registerTool((ctx) => makeCreateTopicTool(ctx), { names: ["create_topic"], optional: false });
api.registerTool((ctx) => makeSearchTopicsTool(ctx), { names: ["search_topics"], optional: false });
api.registerTool((ctx) => makeAddReferenceTool(ctx), { names: ["add_reference"], optional: false });
api.registerTool((ctx) => makeSearchReferencesTool(ctx), { names: ["search_references"], optional: false });
api.registerTool((ctx) => makeListReferencesTool(ctx), { names: ["list_references"], optional: false });
api.registerTool((ctx) => makeReadDirectivesTool(ctx), { names: ["read_directives"], optional: false });
```

### After Phase 2: Build and Test

```bash
# Build the Go binary
cd /home/v/workspace/projects/mpm
PATH=/usr/local/go/bin:$PATH make build

# Test JSON output for each handler
MPM_WORKSPACE=/home/v/workspace/projects/mpm ./bin/mpm lesson add "test" --type insight --json
MPM_WORKSPACE=/home/v/workspace/projects/mpm ./bin/mpm lesson list --json
MPM_WORKSPACE=/home/v/workspace/projects/mpm ./bin/mpm topic add "test" "desc" --json
MPM_WORKSPACE=/home/v/workspace/projects/mpm ./bin/mpm topic list --json
MPM_WORKSPACE=/home/v/workspace/projects/mpm ./bin/mpm directives --json

# Build TypeScript plugin
cd openclaw/mpm-plugin && npm run build 2>&1

# Restart Gateway
openclaw gateway restart

# Verify tools registered
openclaw plugins inspect mpm --runtime --json | python3 -c "import sys,json; d=json.load(sys.stdin); print('Tools:', d['plugin'].get('toolNames',[]))"
```