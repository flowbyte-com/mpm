# Security Model

**Core principle**: Never store secrets in memory. All content is scanned before any database write.

## Sensitive Content Patterns (20 regexes, layered: specific prefixes first, general fallback last)

Implemented in `internal/memory.go` → `isSensitiveContent()`:

| # | Pattern | Example |
|---|---------|---------|
| 1 | OpenAI Project Key | `sk-proj-[a-zA-Z0-9_-]{20,}` |
| 2 | OpenAI Service Key | `sk-svc-[a-zA-Z0-9_-]{20,}` |
| 3 | Anthropic API Key | `sk-ant-[a-zA-Z0-9_-]{20,}` |
| 4 | Generic Secret Key | `sk-[a-zA-Z0-9_-]{20,}` (fallback for unknown `sk-` variants) |
| 5 | GitHub PAT | `ghp_[a-zA-Z0-9]{36}` |
| 6 | GitHub OAuth | `gho_[a-zA-Z0-9]{36}` |
| 7 | GitHub Refresh | `ghr_[a-zA-Z0-9]{72}` |
| 8 | AWS Access Key | `AKIA[A-Z0-9]{16}` |
| 9 | AWS Secret Key | `[A-Za-z0-9/+=]{40}` |
| 10 | Slack Token | `xox[baprs]-[0-9]+-[0-9]+` |
| 11-12 | Stripe Key | `sk_live_` / `sk_test_` + 24+ alphanumeric |
| 13 | JWT | `eyJ[a-zA-Z0-9_-]*\.eyJ...` |
| 14 | General API Key | `api_key=` or `apikey=` + value |
| 15 | Password | `password=` / `passwd=` / `pwd=` + value |
| 16 | Secret/Token | `secret=` / `token=` + value |
| 17 | Private Key | `-----BEGIN (RSA )?PRIVATE KEY-----` |
| 18 | SSH Key | `-----BEGIN OPENSSH KEY-----` |
| 19 | Bearer Token | `bearer ` + 20+ alphanumeric |
| 20 | DB Connection | `mysql://`, `postgres://`, `mongodb://`, `redis://` |

Blocked content is logged to `mirror.jsonl` with timestamp, pattern name, and snippet but never reaches the database.

## Toxic Phrase Detection

`toxicphrases.txt` loaded once via `sync.Once` and cached. Case-insensitive substring match (no regex, no ReDoS). Defaults to 20 phrases if file missing.

## Shred Protocol

Hard delete — `DELETE` (FTS5 DELETE triggers keep search indexes in sync immediately). `VACUUM` is deferred to the `mpm maintain` maintenance cycle to avoid blocking the Watch daemon.

## File Permissions

- Database: `0600` (owner-only)
- Config: `0700`
- Umask: `0077` set at process start

## Injection Prevention

- Parameterized queries (`?` placeholders) — no SQL injection
- Path normalization via `filepath.Clean()` — no path traversal
- No dynamic SQL from user input

## Audit Logging

`mirror.jsonl` records all blocked content attempts and shred operations. Format:

```json
{"timestamp":"...","reason":"OpenAI API Key","content_snippet":"sk-...","action":"blocked","type":"sensitive_attempt"}
```
