# Security Model

**Core principle**: Never store secrets in memory. All content is scanned before any database write.

## Sensitive Content Patterns (17 regexes)

Implemented in `internal/memory.go` → `isSensitiveContent()`:

| # | Pattern | Example |
|---|---------|---------|
| 1 | OpenAI API Key | `sk-[a-zA-Z0-9_-]{20,}` |
| 2 | GitHub PAT | `ghp_[a-zA-Z0-9]{36}` |
| 3 | GitHub OAuth | `gho_[a-zA-Z0-9]{36}` |
| 4 | GitHub Refresh | `ghr_[a-zA-Z0-9]{72}` |
| 5 | AWS Access Key | `AKIA[A-Z0-9]{16}` |
| 6 | AWS Secret Key | `[A-Za-z0-9/+=]{40}` |
| 7 | Slack Token | `xox[baprs]-[0-9]+-[0-9]+` |
| 8-9 | Stripe Key | `sk_live_` / `sk_test_` + 24+ alphanumeric |
| 10 | JWT | `eyJ[a-zA-Z0-9_-]*\.eyJ...` |
| 11 | General API Key | `api_key=` or `apikey=` + value |
| 12 | Password | `password=` / `passwd=` / `pwd=` + value |
| 13 | Secret/Token | `secret=` / `token=` + value |
| 14 | Private Key | `-----BEGIN (RSA )?PRIVATE KEY-----` |
| 15 | SSH Key | `-----BEGIN OPENSSH KEY-----` |
| 16 | Bearer Token | `bearer ` + 20+ alphanumeric |
| 17 | DB Connection | `mysql://`, `postgres://`, `mongodb://`, `redis://` |

Blocked content is logged to `mirror.jsonl` with timestamp, pattern name, and snippet but never reaches the database.

## Toxic Phrase Detection

`toxicphrases.txt` loaded once via `sync.Once` and cached. Case-insensitive substring match (no regex, no ReDoS). Defaults to 20 phrases if file missing.

## Shred Protocol

Hard delete — `DELETE` + `VACUUM`. FTS5 DELETE triggers keep search indexes in sync. Post-shred verification: `SELECT COUNT(*)` confirms zero rows remain.

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
