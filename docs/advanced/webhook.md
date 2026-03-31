# Webhook System

> **Note:** The webhook feature is part of the experimental daemon system.

The webhook system enables MPM to send real-time notifications to external services (Slack, Discord, custom APIs) when important events occur.

---

## Overview

```
┌─────────────────────────────────────────────────────────────┐
│                      MPM Daemon                             │
│                                                             │
│  logger() ──────────────────────────────────────────────┐  │
│     │                                                      │  │
│     │ (non-blocking send)                                 │  │
│     ▼                                                      │  │
│  webhookChan (buffered 1000)                              │  │
│     │                                                      │  │
│     ▼                                                      │  │
│  webhookDispatcher() ── goroutine ──► HTTP POST ──► Slack  │
│                                          ──► Discord       │
│                                          ──► Custom API    │
└─────────────────────────────────────────────────────────────┘
```

### Key Design Principles

1. **Non-blocking** — The daemon never waits for webhook delivery
2. **Fault-tolerant** — Slow/down webhooks don't affect `sync`, `shred`, etc.
3. **Rate-limited** — Batches events when >50/second to avoid API rate limits
4. **Filtered** — Only WARN and ERROR events are sent (by default)

---

## Configuration

### Environment Variable

```bash
export MPM_WEBHOOK_URL=https://hooks.slack.com/services/T00000000/B00000000/XXXXXXXXXXXXXXX
# or Discord, custom endpoint, etc.
```

### Validation

The URL must start with `http://` or `https://`. Invalid URLs are silently ignored with a warning:

```go
if !strings.HasPrefix(webhookURL, "http://") && !strings.HasPrefix(webhookURL, "https://") {
    fmt.Fprintf(os.Stderr, "Warning: MPM_WEBHOOK_URL must start with http:// or https://\n")
    return
}
```

---

## Event Types

### Log Event (WARN/ERROR only)

```json
{
  "event": "log",
  "level": "ERROR",
  "command": "sync",
  "pid": 12345,
  "message": "Command failed: connection refused",
  "timestamp": "2026-03-29T08:25:00Z",
  "duration_ms": null
}
```

### Heartbeat Event (24h interval)

```json
{
  "event": "heartbeat",
  "status": "healthy",
  "uptime": "3d 4h 20m",
  "total_tasks_processed": 142,
  "memory_usage_kb": 8192,
  "timestamp": "2026-03-29T08:00:00Z"
}
```

### Manual Pulse Event (`mpm status --ping`)

```json
{
  "event": "manual_pulse",
  "type": "status_ping",
  "uptime": "5m 30s",
  "memory_kb": 8192,
  "timestamp": "2026-03-29T08:05:30Z"
}
```

### Batched Events (rate-limited)

```json
{
  "event": "batch",
  "count": 50,
  "events": [...]
}
```

When >50 events occur within 1 second, events are batched:

```json
{
  "event": "batch",
  "count": 50,
  "events": [
    {
      "ts": "2026-03-29T08:25:00Z",
      "lvl": "ERROR",
      "cmd": "sync",
      "pid": 12345,
      "msg": "Command failed: connection refused",
      "dur_ms": null
    },
    // ... up to 50 events
  ]
}
```

---

## Filtering & Throttling

### Default Filter: WARN + ERROR Only

To avoid spamming external services, only `WARN` and `ERROR` level events are sent:

| Level | Sent to Webhook? | Written to daemon.json.log? |
|-------|------------------|----------------------------|
| DEBUG | No | Yes |
| INFO | No | Yes |
| WARN | **Yes** | Yes |
| ERROR | **Yes** | Yes |

### Rate Limiting

| Parameter | Value | Description |
|-----------|-------|-------------|
| Window | 1 second | Rolling time window |
| Burst limit | 50 events | Max events per window |
| Batch flush | 1 second | Time to wait before sending batch |

**Behavior:**

- **Normal mode** (< 50 events/sec): Each event sent immediately
- **Batching mode** (≥ 50 events/sec): Events accumulated and sent as batch

```
Time →
[Event1] [Event2] ... [Event50]  ──► Batch sent at T+1s
     ↑                                  │
     └── normal (immediate) before 50 ──┘
```

---

## Retry Logic

### Retry on 5xx Errors

If the webhook endpoint returns a 5xx error (server error), MPM retries once:

```
Attempt 1: HTTP 503 → Wait 500ms → Attempt 2: HTTP 200 → Success
Attempt 1: HTTP 200 → Success (no retry needed)
Attempt 1: HTTP 400 → Fail (no retry - client error)
```

```go
// Simplified retry logic
if resp.StatusCode >= 500 && attempt == 1 {
    time.Sleep(500 * time.Millisecond)
    sendWebhookWithRetry(client, payload, attempt+1)
}
```

### What Doesn't Retry

- **4xx errors** — Client error (bad request, unauthorized), retrying won't help
- **Network timeout** — Already limited by 5-second timeout
- **Connection refused** — Endpoint is down, no point retrying immediately

---

## Timeout Protection

Each HTTP request has a strict **5-second timeout**:

```go
client := &http.Client{
    Timeout: 5 * time.Second,
}
```

This prevents hanging goroutines if the webhook endpoint stops responding.

---

## Thread Safety

| Component | Mechanism |
|-----------|-----------|
| `webhookEnabled` | `atomic.Bool` — check without locking |
| `rateLimitCount` | `sync.Mutex` — protect counter increment |
| `webhookChan` | Buffered channel — goroutine-safe queue |
| `webhookWg` | `sync.WaitGroup` — graceful shutdown |

### Shutdown Sequence

```go
func closeWebhook() {
    webhookEnabled.Store(false)  // Stop new dispatches
    close(webhookChan)           // Signal dispatcher to exit
    
    done := make(chan struct{})
    go func() {
        webhookWg.Wait()        // Wait for pending requests
        close(done)
    }()
    
    select {
    case <-done:
    case <-time.After(2 * time.Second):
        // Force close after timeout
    }
}
```

---

## Integration Examples

### Slack Incoming Webhook

1. Create an Incoming Webhook at `https://api.slack.com/messaging/webhooks`
2. Set environment variable:
   ```bash
   export MPM_WEBHOOK_URL=https://hooks.slack.com/services/T00000000/B00000000/XXXXXXXXXXXXXXX
   ```
3. Restart the daemon

**Slack will receive:**
```json
{
  "event": "log",
  "level": "ERROR",
  "command": "sync",
  "pid": 12345,
  "message": "Command failed: connection refused",
  "timestamp": "2026-03-29T08:25:00Z"
}
```

### Discord Webhook

1. Create a Discord webhook (Server Settings → Integrations → Webhooks)
2. Set environment variable:
   ```bash
   export MPM_WEBHOOK_URL=https://discord.com/api/webhooks/123456789/abcdef
   ```

### Custom API Endpoint

```bash
export MPM_WEBHOOK_URL=https://api.example.com/mpm-events
```

Your API receives POST requests with the payloads described above.

---

## Environment Variables

| Variable | Required | Description |
|----------|----------|-------------|
| `MPM_WEBHOOK_URL` | Yes | Full HTTPS/HTTP URL for POST requests |

---

## Related

- [Daemon Architecture](daemon.md) — Core daemon implementation
- [Structured Logging](daemon.md#structured-json-logging) — Log file format
