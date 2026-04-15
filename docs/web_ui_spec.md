# MPM Web UI — Specification

## Overview

Single-page web UI served by MPM daemon. Token auth via existing `openclaw.json` mechanism. Vanilla JS + CSS, no build step.

---

## Views

### 1. Search (`/`)

Unified search bar — searches memories, topics, references, lessons simultaneously.
Results grouped by type with highlighted snippets. Click to navigate.

### 2. Memories (`/memories`)

| Action | Method | Description |
|--------|--------|-------------|
| List | `GET /api/memories?q=&collection=` | Paginated list |
| Get one | `GET /api/memories/:id` | Full content + metadata |
| Add | `POST /api/memories` | `{content, collection, tags}` |
| Edit | `PUT /api/memories/:id` | `{content, tags}` |
| Delete/Shred | `DELETE /api/memories/:id` | Hard delete + VACUUM |

### 3. Topics (`/topics`)

| Action | Method | Description |
|--------|--------|-------------|
| List | `GET /api/topics` | All topics with memory counts |
| View | `GET /api/topics/:id` | Topic + linked memories |
| Create | `POST /api/topics` | `{name, description}` |
| Delete | `DELETE /api/topics/:id` | Soft delete |
| Add memory to topic | `POST /api/topics/:id/memories` | `{memory_id}` |
| Remove memory from topic | `DELETE /api/topics/:id/memories/:memory_id` | Unlink |

### 4. Lessons (`/lessons`)

| Action | Method | Description |
|--------|--------|-------------|
| List | `GET /api/lessons?type=` | Optional filter by type |
| View | `GET /api/lessons/:id` | Full lesson |
| Add | `POST /api/lessons` | `{content, type, tags}` |
| Delete | `DELETE /api/lessons/:id` | Remove lesson |

### 5. Prime Directives (`/directives`)

Filtered view of memories where `is_prime_directive=1`. All edit/delete via memories UI.

---

## Architecture

```
Browser (vanilla JS + HTML)  ←  HTTP API (token auth)  →  MPM Daemon (net/http + DatabaseManager)
```

**Files:**
- `cmd/mpm/web.go` — HTTP server + API handlers
- `web/index.html` — SPA
- `web/style.css` — Minimal styling
- `web/app.js` — Frontend logic

**Auth:** `Authorization: Bearer <token>` header, token from `openclaw.json`.

---

## Implementation Phases

### Phase 1: Core CRUD + Search (~6-8h)

1. HTTP server skeleton + auth middleware + JSON helpers
2. Memories: list, view, add, edit, delete
3. Topics: list, view, create, delete, add/remove memories
4. Lessons: list, view, add, delete
5. Unified search endpoint + results page

### Phase 2: Polish (~2-3h)

6. Prime directives filtered view
7. Empty states, loading indicators, error handling
8. Responsive layout, keyboard shortcuts

---

## Out of Scope (later)

- Mode/persona selection (TUI is better)
- Ingest review UI
- Reference management
- Real-time updates
