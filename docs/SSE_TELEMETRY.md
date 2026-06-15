# SSE Telemetry Upgrade — Live Web Dashboard

Adding real-time Server-Sent Events (SSE) to the MPM web UI. The Go backend broadcasts epistemology and memory events to the browser, so the dashboard updates instantly without polling or page reloads.

**Blast radius:** zero agent pipeline changes. One-way mirror only.

---

## Architecture

```
┌──────────────────────────────────────────────────────────────────────┐
│                         mpm binary                                   │
│                                                                      │
│   call.go ──────┐                                                   │
│                  ▼                                                   │
│   handlers.go ───┼──▶ SSEBroker ──────────────────▶ /api/stream     │
│                  │     (singleton)              (HTTP SSE push)       │
│   watch.go ──────┘                                                   │
│                                                                      │
│   Web UI ◀──────────────────────────────────────────────────────    │
│   (EventSource listener + DOM manipulators)                          │
└──────────────────────────────────────────────────────────────────────┘
```

**Key invariant:** The broker is a package-level singleton. `call.go`, `handlers.go`, and `watch.go` all broadcast to the same broker instance. The web server registers the `/api/stream` HTTP handler on it. No state is held inside HTTP request contexts.

---

## Phase 1: The Go Broadcaster (`cmd/mpm/stream.go`)

### Ring Buffer (Last-Event-ID Reconnection Resilience)

Browser connections can blip — Wi-Fi switch, waking from sleep. SSE natively supports a `Last-Event-ID` header on reconnection. The broker maintains an in-memory ring buffer of the last 50 events with incrementing IDs so any missed events are replayed instantly on reconnect.

```go
// ringBuffer holds the last N events for replay on client reconnect.
type ringBuffer struct {
    events []sseEvent     // oldest → newest
    size   int            // max capacity
    head   int            // overwrite cursor
    count  int            // total events ever written
    mu     sync.Mutex
}

type sseEvent struct {
    ID    int64  `json:"id"`
    Type  string `json:"type"`
    Data  string `json:"data"`
}

func (rb *ringBuffer) Push(typ, data string) sseEvent {
    rb.mu.Lock()
    defer rb.mu.Unlock()
    rb.count++
    ev := sseEvent{ID: rb.count, Type: typ, Data: data}
    if len(rb.events) < rb.size {
        rb.events = append(rb.events, ev)
    } else {
        rb.events[rb.head%rb.size] = ev
    }
    rb.head++
    return ev
}

// Replay returns all events with ID > lastSeen, oldest first.
func (rb *ringBuffer) Replay(lastSeen int64) []sseEvent {
    rb.mu.Lock()
    defer rb.mu.Unlock()
    if lastSeen >= rb.count {
        return nil
    }
    // Find start index for lastSeen+1
    start := int(lastSeen+1) - (rb.head - len(rb.events))
    if start < 0 {
        start = 0
    }
    return rb.events[start:]
}
```

### SSEBroker

```go
type client struct {
    ch   chan string   // buffered: 256 to handle burst
    id   int64         // for deregistration
}

type SSEBroker struct {
    clients    map[chan string]client
    register   chan client
    unregister chan chan string
    broadcast  chan string
    rb         *ringBuffer
    idSeq      int64
    mu         sync.Mutex
}

func NewSSEBroker() *SSEBroker {
    b := &SSEBroker{
        clients:    make(map[chan string]client),
        register:   make(chan client),
        unregister: make(chan chan string),
        broadcast:  make(chan string, 256), // backpressure: slow consumers don't block
        rb:         newRingBuffer(50),
    }
    go b.run()
    go b.heartbeat() // 15s keepalive ping — prevents proxy idle-kill
    return b
}

func (b *SSEBroker) run() {
    for {
        select {
        case ch := <-b.register:
            b.mu.Lock()
            b.idSeq++
            b.clients[ch] = client{ch: ch, id: b.idSeq}
            b.mu.Unlock()

        case ch := <-b.unregister:
            b.mu.Lock()
            delete(b.clients, ch)
            b.mu.Unlock()
            close(ch)

        case msg := <-b.broadcast:
            b.mu.Lock()
            for ch := range b.clients {
                select {
                case ch <- msg: // non-blocking: slow consumer skips this event
                default:
                    // Consumer too slow — skip, don't block broadcast
                }
            }
            b.mu.Unlock()
        }
    }
}

// heartbeat sends a ping every 15s to keep the connection alive through proxies.
func (b *SSEBroker) heartbeat() {
    ticker := time.NewTicker(15 * time.Second)
    for range ticker.C {
        b.Broadcast(`{"type":"ping"}`)
    }
}
```

### Broadcast method

```go
// Broadcast sends a typed JSON event to all connected clients.
// Event format: "id:<int64>\ntype:<type>\ndata:<json>\n\n"
func (b *SSEBroker) Broadcast(eventType string, payload interface{}) {
    data, _ := json.Marshal(payload)
    ev := b.rb.Push(eventType, string(data)) // stores in ring buffer

    msg := fmt.Sprintf("id: %d\ntype: %s\ndata: %s\n\n", ev.ID, eventType, string(data))
    b.broadcast <- msg
}

// ServeHTTP handles /api/stream — SSE endpoint.
// Reads Last-Event-ID header to replay missed events on reconnect.
func (b *SSEBroker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
    // Auth check (same as other /api/* routes)
    if !authValid(r) {
        http.Error(w, "unauthorized", http.StatusUnauthorized)
        return
    }

    // SSE headers
    w.Header().Set("Content-Type", "text/event-stream")
    w.Header().Set("Cache-Control", "no-cache")
    w.Header().Set("Connection", "keep-alive")
    w.Header().Set("Access-Control-Allow-Origin", "*")
    flusher, ok := w.(http.Flusher)
    if !ok {
        http.Error(w, "streaming not supported", http.StatusInternalServerError)
        return
    }

    // Replay missed events from Last-Event-ID
    lastID := r.Header.Get("Last-Event-ID")
    if lastID != "" {
        if id, err := strconv.ParseInt(lastID, 10, 64); err == nil {
            for _, ev := range b.rb.Replay(id) {
                msg := fmt.Sprintf("id: %d\ntype: %s\ndata: %s\n\n", ev.ID, ev.Type, ev.Data)
                fmt.Fprint(w, msg)
                flusher.Flush()
            }
        }
    }

    ch := make(chan string, 256) // buffered: burst-safe, skip-on-full
    b.register <- ch
    defer func() {
        b.unregister <- ch
    }()

    // Stream events until connection closes
    for {
        select {
        case msg, ok := <-ch:
            if !ok {
                return // client disconnected
            }
            fmt.Fprint(w, msg)
            flusher.Flush()

        case <-r.Context().Done():
            return // browser closed connection
        }
    }
}
```

---

## Phase 2a: Router Hooks — `call.go`

Inject event broadcasts into `handleCall` and each tool handler. The broker is a package-level singleton imported from `stream.go`.

```go
// stream.go exports the singleton
var broker = NewSSEBroker()

// In handleCall, after handler returns successfully:
result, err := handler(payload)
if err == nil {
    // Universal tool_exec event for every successful call
    broker.Broadcast("tool_exec", map[string]interface{}{
        "tool":    toolName,
        "payload": payload,
        "result":  result,
        "ts":      time.Now().UTC().Format(time.RFC3339),
    })
}
```

Tool-specific epistemology events fired from each handler:

| Handler | Event | When |
|---|---|---|
| `callSaveToMemory` | `memory_saved` | After `store.AddMemory` succeeds |
| `callChallengeMemory` | `immune_slash` | After `dm.ChallengeAndReinforce` + theory creation |
| `callProposeTheory` | `theory_proposed` | After theory memory saved |
| `callResolveTheory` | `theory_resolved` | After metadata patch + weight bump |
| `callSaveLesson` | `lesson_saved` | After `lessonStore.AddLesson` succeeds |

**Example — `callSaveToMemory`:**
```go
mem, err := store.AddMemory(fact, collection, tags, meta, "", "call")
if err == nil {
    broker.Broadcast("memory_saved", map[string]interface{}{
        "id":        mem.ID,
        "content":   mem.Content,
        "weight":    mem.Weight,
        "collection": collection,
        "tags":      mem.Tags,
        "provenance": meta["provenance"],
        "ts":        time.Now().UTC().Format(time.RFC3339),
    })
}
```

---

## Phase 2b: Router Hooks — `handlers.go` (Web UI Path)

The web UI hits `/api/kb/memory/challenge` etc. — these handlers bypass `call.go` entirely and call the DB layer directly. They need the same broadcasts.

Each relevant handler in `handlers.go` gets an identical broadcast call after its DB mutation succeeds.

**Key handlers to patch:**
- `handleMemoryChallenge` → `immune_slash`
- `handleTheoryProposed` → `theory_proposed`
- `handleTheoryResolved` → `theory_resolved`
- `handleMemoryAdd` → `memory_saved`
- `handleLessonAdd` → `lesson_saved`

> The broker singleton is accessible from `handlers.go` since both import `stream.go`. No architectural change needed.

---

## Phase 3: Frontend — `cmd/mpm/web/app.js`

### EventSource Listener

```js
// Establish SSE connection; replay missed events via Last-Event-ID
let lastEventID = localStorage.getItem('mpm_sse_id') || '0';
const evtSource = new EventSource('/api/stream');

evtSource.addEventListener('open', () => {
    showToast('Live updates active', 'success');
});

evtSource.addEventListener('message', (e) => {
    // Update Last-Event-ID on every event for reconnect replay
    if (e.lastEventId) {
        lastEventID = e.lastEventId;
        localStorage.setItem('mpm_sse_id', lastEventID);
    }

    let ev;
    try { ev = JSON.parse(e.data); } catch { return; }

    switch (ev.type) {
        // ── Memory events ──
        case 'memory_saved':
            prependMemoryCard(ev);
            break;

        case 'immune_slash': {
            // Strike through the challenged memory card
            const card = document.querySelector(`[data-memory-id="${ev.memory_id}"]`);
            if (card) {
                card.classList.add('slashed');
                card.style.textDecoration = 'line-through';
                card.style.opacity = '0.5';
            }
            break;
        }

        // ── Epistemology events ──
        case 'theory_proposed':
        case 'theory_resolved':
            // Silent re-fetch of stats to update ISR matrix
            refreshStats();
            flashMatrixCell(ev.type === 'theory_resolved' ? 'resolved' : 'proposed');
            break;

        // ── Lesson events ──
        case 'lesson_saved':
            prependLessonCard(ev);
            break;

        // ── Keepalive (no DOM action) ──
        case 'ping':
            break;

        default:
            console.debug('[SSE]', ev.type, ev);
    }
});

evtSource.addEventListener('error', () => {
    // EventSource auto-reconnects with Last-Event-ID header automatically
    // Just update the UI to show reconnecting state
    showToast('Reconnecting...', 'warn');
});
```

### DOM Manipulators

```js
function prependMemoryCard(ev) {
    const html = `
        <div class="memory-card" data-memory-id="${ev.id}">
            <div class="meta">
                <span class="badge">${ev.collection}</span>
                ${(ev.tags || []).map(t => `<span class="tag">${t}</span>`).join('')}
            </div>
            <p>${escHtml(ev.content)}</p>
            <div class="provenance">compute: ${ev.provenance?.compute || 'unknown'}</div>
        </div>`;
    const feed = document.getElementById('memory-feed');
    if (feed) {
        feed.insertAdjacentHTML('afterbegin', html);
        // Fade-in animation
        feed.firstElementChild.style.animation = 'fadeSlideIn 0.3s ease';
    }
}

function prependLessonCard(ev) {
    const html = `
        <div class="lesson-card" data-lesson-id="${ev.id}">
            <span class="badge ${ev.lesson_type}">${ev.lesson_type}</span>
            <p>${escHtml(ev.content)}</p>
        </div>`;
    const feed = document.getElementById('lesson-feed');
    if (feed) {
        feed.insertAdjacentHTML('afterbegin', html);
        feed.firstElementChild.style.animation = 'fadeSlideIn 0.3s ease';
    }
}

async function refreshStats() {
    try {
        const data = await apiFetch('/api/stats');
        renderStatsDashboard(data);
    } catch {}
}

function flashMatrixCell(type) {
    const cell = document.querySelector(`[data-isr-cell="${type}"]`);
    if (cell) {
        cell.style.transition = 'background 0.2s';
        cell.style.background = type === 'resolved' ? '#22c55e' : '#f59e0b';
        setTimeout(() => cell.style.background = '', 800);
    }
}

function escHtml(s) {
    const d = document.createElement('div');
    d.textContent = s;
    return d.innerHTML;
}
```

### CSS for Fade-In and Struck Memories

```css
@keyframes fadeSlideIn {
    from { opacity: 0; transform: translateY(-8px); }
    to   { opacity: 1; transform: translateY(0); }
}

.memory-card.slashed,
.lesson-card.slashed {
    text-decoration: line-through;
    opacity: 0.5;
}
```

---

## Phase 4: Live Fire Test

```
Monitor A: Browser → localhost:18792 (MPM dashboard)
Monitor B: Terminal

1. Start mpm:   mpm ops web
2. Open browser: http://localhost:18792
3. In terminal: mpm call save_to_memory --payload '{"fact":"SSE ring buffer test memory","tags":["sse"]}'
4. Verify:      Memory card prepends in <1s with fade-in animation
5. In terminal:  mpm call challenge_memory --payload '{"memoryId":"<id from step 3>","evidence":"testing slash"}'
6. Verify:      Memory card gets struck through, ISR matrix flashes
7. Disconnect Wi-Fi on Monitor A laptop for 5s, reconnect
8. Verify:      No missed events — Last-Event-ID replayed all events from step 3-6
```

---

## Key Implementation Notes

| Concern | Decision |
|---|---|
| Auth | `/api/stream` uses same `authValid()` as other `/api/*` routes |
| Backpressure | `broadcast` channel buffered 256; slow consumers skip via `default:` select |
| Goroutine leak | Client goroutine exits on `r.Context().Done()` + deferred `unregister` |
| Client buffer | Each client channel buffered 256 to handle burst without drops |
| Keepalive | 15s `ping` ticker prevents proxy idle-kill |
| Reconnect | Ring buffer 50 events; `Last-Event-ID` header triggers replay on reconnect |
| Event ordering | ID per-event, monotonically increasing, stored in ring buffer |
| Phase 2a vs 2b | Both needed — `call.go` for CLI/agent path, `handlers.go` for web UI direct path |
| `memory_saved` payload | Full card data included — zero extra round-trips for render |

---

## File Changes Summary

| File | Change |
|---|---|
| `cmd/mpm/stream.go` | **New** — SSEBroker, ring buffer, heartbeat, /api/stream handler |
| `cmd/mpm/call.go` | Add `broker.Broadcast()` calls to each tool handler |
| `cmd/mpm/handlers.go` | Add `broker.Broadcast()` calls to each kb mutation handler |
| `cmd/mpm/web/app.js` | EventSource listener + switch statement for all event types |
| `cmd/mpm/web/style.css` | `fadeSlideIn` keyframe + `.slashed { text-decoration: line-through; opacity: 0.5; }` |
