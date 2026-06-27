package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"mpm/internal/config"
	"mpm/internal/usererror"
)

// ── Ring Buffer ─────────────────────────────────────────────────────────────

// ringBuffer holds the last N events for replay on client reconnect.
// Thread-safe, fixed-capacity circular buffer.
type ringBuffer struct {
	events []sseEvent // oldest → newest live entries
	size   int        // max capacity
	head   int        // overwrite cursor (index of oldest when full)
	count  int64      // total events ever written
	mu     sync.Mutex
}

type sseEvent struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
	Data string `json:"data"`
}

func newRingBuffer(size int) *ringBuffer {
	return &ringBuffer{size: size}
}

// Push appends an event, overwriting the oldest slot when full.
func (rb *ringBuffer) Push(typ, data string) sseEvent {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	rb.count++
	ev := sseEvent{ID: rb.count, Type: typ, Data: data}
	if len(rb.events) < rb.size {
		rb.events = append(rb.events, ev)
	} else {
		rb.events[rb.head%rb.size] = ev
		rb.head++
	}
	return ev
}

// Replay returns all buffered events with ID > lastSeen, oldest first.
func (rb *ringBuffer) Replay(lastSeen int64) []sseEvent {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	if lastSeen >= rb.count {
		return nil
	}
	var out []sseEvent
	for _, ev := range rb.events {
		if ev.ID > lastSeen {
			out = append(out, ev)
		}
	}
	// C1 FIX: Guarantee chronological order regardless of buffer wrap.
	// After wrap, rb.events is not in ID order; sorting makes the
	// SSE stream preserve the contract that events arrive in ID order.
	sort.Slice(out, func(i, j int) bool {
		return out[i].ID < out[j].ID
	})
	return out
}

// Count returns the total number of events ever written.
// Thread-safe; safe to call from any goroutine.
func (rb *ringBuffer) Count() int64 {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	return rb.count
}

// ── SSEBroker ──────────────────────────────────────────────────────────────

// SSEBroker holds active SSE client connections and broadcasts events to all of them.
type SSEBroker struct {
	clients    map[chan string]int64 // client channel → client ID
	register   chan (chan string)    // channel to register
	unregister chan (chan string)    // channel to unregister
	broadcast  chan string           // message to fan out (buffered for backpressure)
	rb         *ringBuffer
	idSeq      int64
	mu         sync.Mutex
}

var broker = newBroker()

// allowAnonymousSSE mirrors the WebServer's allowAnonymous flag. It is set
// at server start by handleWeb when --allow-anonymous is passed. Used by
// authValid() so SSE auth matches web-server auth policy.
var allowAnonymousSSE bool

func newBroker() *SSEBroker {
	b := &SSEBroker{
		clients:    make(map[chan string]int64),
		register:   make(chan (chan string), 8), // H2 FIX: buffer so brief stalls in run() don't hang new SSE clients
		unregister: make(chan (chan string), 8), // H2 FIX: same — decouples disconnect cleanup from run() pace
		broadcast:  make(chan string, 256),      // buffered: slow consumers don't block broadcast
		rb:         newRingBuffer(50),
	}
	go b.run()
	go b.heartbeat()
	return b
}

func (b *SSEBroker) run() {
	defer func() {
		if r := recover(); r != nil {
			usererror.Warn("[SSEBroker] CRITICAL: run() panicked: %v. Restarting broker loop.", r)
			// C2 hardening: backoff before respawn to prevent a CPU-spinning
			// crash loop if the panic is persistent (e.g., closed channel stuck
			// in clients map).
			time.Sleep(1 * time.Second)
			go b.run() // C2 FIX: Resurrect the broker so the telemetry pipeline survives.
		}
	}()

	for {
		select {
		case ch := <-b.register:
			b.mu.Lock()
			b.idSeq++
			b.clients[ch] = b.idSeq
			b.mu.Unlock()

		case ch := <-b.unregister:
			b.mu.Lock()
			delete(b.clients, ch)
			b.mu.Unlock()
			// M2 invariant: close(ch) must be OUTSIDE b.mu (so a future refactor
			// that takes the lock before sending can't deadlock) and AFTER the
			// delete (so the broadcast loop never sends on a closed channel).
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
// Proxies often kill idle connections after ~30s; a silent ping prevents this.
func (b *SSEBroker) heartbeat() {
	ticker := time.NewTicker(15 * time.Second)
	for range ticker.C {
		// H1 FIX: Use thread-safe Count() to avoid data race on rb.count.
		b.BroadcastRaw(fmt.Sprintf("id: %d\ntype: ping\ndata: {}\n\n", b.rb.Count()+1))
	}
}

// Broker returns the package-level SSEBroker singleton.
func Broker() *SSEBroker { return broker }

// Broadcast sends a typed JSON event to all connected clients.
func (b *SSEBroker) Broadcast(eventType string, payload interface{}) {
	data, _ := json.Marshal(payload)
	ev := b.rb.Push(eventType, string(data))
	msg := fmt.Sprintf("id: %d\ntype: %s\ndata: %s\n\n", ev.ID, eventType, string(data))
	select {
	case b.broadcast <- msg:
	default:
		// C3 FIX: Drop event — broadcast pipeline is overloaded.
		// Prevents HTTP handlers and agent tools from hanging.
	}
}

// BroadcastRaw sends a raw SSE-formatted string to all connected clients.
func (b *SSEBroker) BroadcastRaw(msg string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.clients {
		select {
		case ch <- msg:
		default:
			// Slow consumer — skip
		}
	}
}

// ── HTTP Handler ───────────────────────────────────────────────────────────

// authValid checks the bearer token from mpm_config.json.
//
// Auth policy (post security-review-2026-06-15 fix): fail closed unless
// the operator started the server with --allow-anonymous. See
// WebServer.withAuth for the matching web-server policy and rationale.
func authValid(r *http.Request) bool {
	cfg, err := config.LoadConfig()
	if err != nil || cfg == nil {
		return false // No config — fail closed
	}
	token := cfg.WebToken
	if token == "" {
		// SSE is a server-internal endpoint but it has been used as a
		// side-channel for unauthenticated access historically. Mirror the
		// web-server policy: only allow if the operator opted in.
		return allowAnonymousSSE
	}
	authHeader := r.Header.Get("Authorization")
	if !strings.HasPrefix(authHeader, "Bearer ") {
		return false
	}
	return strings.TrimPrefix(authHeader, "Bearer ") == token
}

// ServeSSE handles GET /api/stream — SSE endpoint for live telemetry.
func (b *SSEBroker) ServeSSE(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !authValid(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// SSE headers
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("X-Accel-Buffering", "no") // Disable proxy buffering for SSE

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
				io.WriteString(w, msg)
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
			io.WriteString(w, msg)
			flusher.Flush()

		case <-r.Context().Done():
			return // browser closed connection
		}
	}
}
