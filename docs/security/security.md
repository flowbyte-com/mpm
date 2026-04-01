# Security Model

> **Whitepaper Version:** 1.1  
> **Date:** 2026-03-29  
> **Author:** 808 (AI Agent) & v (Human Developer)  
> **Project:** SymAI mpm (Memory-Persona-Mode Manager)

---

## Executive Summary

This document describes the security architecture and implementation of the SymAI mpm system — a SQLite-based embedded memory manager for AI agents. The security model prioritizes **prevention over detection** with automatic sensitive content blocking, file system hardening, and audit logging.

**Core Principles:**
1. **Defense in Depth** — Multiple layers of protection
2. **Fail Secure** — Deny by default, explicit allow
3. **Audit Trail** — All sensitive operations logged
4. **Minimal Privilege** — Owner-only file permissions
5. **Zero Trust** — No implicit trust in input data

---

## 1. Threat Model

### 1.1 Attack Vectors

| Attack Vector | Risk Level | Mitigation |
|---------------|------------|------------|
| API Key / Token Exfiltration | **High** | Automatic pattern blocking |
| Private Key Storage | **High** | Regex-based blocking |
| Database Injection | **Medium** | Parameterized queries |
| Path Traversal | **Low** | Path normalization |
| Unauthorized Access | **Medium** | File permissions (0600/0700) |
| Memory Dump Analysis | **Low** | Not applicable (embedded) |

### 1.2 Threat Source Analysis

- **Internal Threats:** Agent state corruption, accidental secret storage, unauthorized file access
- **External Threats:** None (embedded, no network exposure)
- **Physical Threats:** Disk theft, unauthorized system access

---

## 2. Security Architecture

### 2.1 Architecture Diagram

```
┌─────────────────────────────────────────────────────────────┐
│                     Agent Session                            │
└────────────────────┬────────────────────────────────────────┘
                     │
                     ▼
┌─────────────────────────────────────────────────────────────┐
│                  Input Layer                                 │
│  • Pattern Matching (Regex)                                 │
│  • Sensitive Content Detection                              │
└────────────────────┬────────────────────────────────────────┘
                     │
                     ▼
┌─────────────────────────────────────────────────────────────┐
│                  Security Layer                              │
│  • isSensitiveContent()                                     │
│  • logSensitiveAttempt()                                    │
│  • Audit Logging                                            │
└────────────────────┬────────────────────────────────────────┘
                     │
                     ▼
┌─────────────────────────────────────────────────────────────┐
│                  Storage Layer                               │
│  • SQLite Database (mpm.db)                         │
│  • JSONL Mirror (mirror.jsonl)                              │
│  • File Permissions (0600/0700)                             │
└─────────────────────────────────────────────────────────────┘
```

---

## 3. Sensitive Content Detection

### 3.1 Implementation

**File:** `internal/memory.go` → `isSensitiveContent()`

The function uses **17 regex patterns** to detect sensitive data before storage. If any pattern matches, content is blocked and logged.

```go
func isSensitiveContent(content string) (bool, string) {
    patterns := []struct {
        name    string
        pattern *regexp.Regexp
    }{
        {"OpenAI API Key", regexp.MustCompile(`sk-[a-zA-Z0-9_-]{20,}`)},
        {"GitHub Personal Token", regexp.MustCompile(`ghp_[a-zA-Z0-9]{36}`)},
        // ... (all 17 patterns)
    }
    for _, p := range patterns {
        if p.pattern.MatchString(content) {
            return true, p.name
        }
    }
    return false, ""
}
```

### 3.2 Complete Pattern Reference

| # | Pattern Name | Regex | Example Blocked |
|---|--------------|-------|-----------------|
| 1 | OpenAI API Key | `sk-[a-zA-Z0-9_-]{20,}` | `sk-test1234567890abcdefghijklmnop` |
| 2 | GitHub PAT | `ghp_[a-zA-Z0-9]{36}` | `ghp_1234567890abcdefghijklmnopqrstuvwxyz` |
| 3 | GitHub OAuth Token | `gho_[a-zA-Z0-9]{36}` | `gho_1234567890abcdefghijklmnopqrstuvwxyz` |
| 4 | GitHub Refresh Token | `ghr_[a-zA-Z0-9]{72}` | `ghr_...` (72 chars) |
| 5 | AWS Access Key ID | `AKIA[A-Z0-9]{16}` | `AKIAIOSFODNN7EXAMPLE` |
| 6 | AWS Secret Key | `[A-Za-z0-9/+=]{40}` | `wJalrXUtnFEMI/K7MDENG/bPxRfiCY...` |
| 7 | Slack Token | `xox[baprs]-[0-9]+-[0-9]+` | `xoxb-1234567890-1234567890123` |
| 8 | Stripe Live Key | `sk_live_[0-9a-zA-Z]{24,}` | `sk_live_abcdefghijklmnopqrstuvwxyz` |
| 9 | Stripe Test Key | `sk_test_[0-9a-zA-Z]{24,}` | `sk_test_abcdefghijklmnopqrstuvwxyz` |
| 10 | JWT Token | `eyJ[a-zA-Z0-9_-]*\.eyJ...` | `eyJhbGciOiJIUzI1NiJ9...` |
| 11 | General API Key | `(?i)(api[_-]?key\|apikey)[=:]\s*[^\s]+` | `api_key=abc123` |
| 12 | Password | `(?i)(password\|passwd\|pwd)[=:]\s*[^\s]+` | `password=secret123` |
| 13 | Secret/Token | `(?i)(secret\|token)[=:]\s*[^\s]+` | `secret=mysecret` |
| 14 | Private Key | `-----BEGIN\s+(RSA\s+)?PRIVATE\s+KEY-----` | `-----BEGIN RSA PRIVATE KEY-----` |
| 15 | SSH Key | `-----BEGIN\s+OPENSSH\s+KEY-----` | `-----BEGIN OPENSSH KEY-----` |
| 16 | Bearer Token | `(?i)bearer\s+[a-zA-Z0-9_-]{20,}` | `bearer eyJhbGciOiJIUzI1NiJ9...` |
| 17 | Database Connection | `(?i)(mysql\|postgres\|mongodb\|redis)://[^\s]+` | `mysql://user:pass@localhost/db` |

### 3.3 Customization

To add or modify patterns, edit `internal/memory.go` and add to the patterns slice:

```go
{"Custom Pattern", regexp.MustCompile(`your-regex-here`)},
```

> ⚠️ **Note:** Pattern modifications require recompilation. Test patterns against known samples before deploying.

---

## 4. Compliance Checklist

### 4.1 GDPR "Right to be Forgotten" (Article 17)

| Requirement | MPM Implementation | Verified |
|-------------|-------------------|----------|
| Data deletion | `mpm memory shred <id>` | ✅ |
| Cascading deletes | Topic → topic_memberships | ✅ |
| VACUUM after delete | Shred protocol rewrites DB | ✅ |
| No backup recovery | JSONL mirror is append-only | ⚠️ |
| Deletion confirmation | Post-shred count verification | ✅ |

> ⚠️ **JSONL Mirror:** The `mirror.jsonl` file is append-only and not automatically pruned. For GDPR compliance, implement log rotation or use `mpm mirror clear` after data deletion.

### 4.2 CCPA Compliance

| Requirement | MPM Implementation | Verified |
|-------------|-------------------|----------|
| Right to delete | Shred protocol | ✅ |
| No selective deletion | Hard delete is irreversible | ✅ |
| Audit trail | JSONL logging | ✅ |

### 4.3 Verification Checklist

After running `mpm memory shred <id>`, verify deletion:

```bash
# 1. Confirm record is gone
mpm memory list | grep <id>
# Should return empty

# 2. Verify via direct DB query
sqlite3 ~/.openclaw/workspace/memory/mpm.db \
  "SELECT COUNT(*) FROM memories WHERE id = '<id>';"
# Should return: 0

# 3. Check audit log
grep "<id>" ~/.openclaw/workspace/memory/mirror.jsonl
```

---

## 5. File System Security

### 5.1 Permission Model

| File/Directory | Permissions | Owner | Purpose |
|----------------|-------------|-------|---------|
| Memory directory | `0700` | User | Memory storage directory |
| Database file | `0600` | User | SQLite database |
| JSONL mirror | `0600` | User | JSONL backup file |
| Config directory | `0700` | User | Configuration directory |

### 5.2 Umask Configuration

```go
// Set restrictive umask for this process (owner-only access)
oldUmask := syscall.Umask(0077)
defer syscall.Umask(oldUmask)
```

### 5.3 Best Practices

- **Single-user systems:** Default permissions are sufficient
- **Multi-user systems:** Ensure each user has their own workspace
- **Shared systems:** Set `MPM_WORKSPACE` to a user-private directory

---

## 6. Database Security

### 6.1 SQLite Configuration

| Setting | Value | Purpose |
|---------|-------|---------|
| Driver | `modernc.org/sqlite` | Pure Go, no cgo |
| Encryption | None | Embedded use case |
| FTS5 | Enabled | Full-text search |

### 6.2 Injection Prevention

| Vulnerability | Mitigation |
|--------------|------------|
| SQL Injection | Parameterized queries (`?` placeholders) |
| Path Traversal | Path normalization via `filepath.Clean()` |
| Schema Injection | No dynamic SQL from user input |

---

## 7. Audit Logging

### 7.1 What Gets Logged

| Event | Location | Format |
|-------|----------|--------|
| Sensitive content blocked | stderr + mirror.jsonl | JSONL |
| Shred operations | stderr + mirror.jsonl | JSONL |
| File permission errors | stderr | Plain text |
| Database write failures | stderr | Plain text |

### 7.2 Log Format

```json
{
  "timestamp": "2026-03-29T06:30:00Z",
  "reason": "OpenAI API Key",
  "content_snippet": "sk-test1234567890abcdefghijklmnop...",
  "action": "blocked",
  "type": "sensitive_attempt"
}
```

### 7.3 Log Retention

| Log Location | Retention | Management |
|--------------|-----------|-----------|
| stderr | System controlled | System log rotation |
| mirror.jsonl | Persistent | User responsibility |

> 💡 **Tip:** Implement log rotation for `mirror.jsonl`:
> ```bash
> # Backup and clear mirror
> cp ~/.openclaw/workspace/memory/mirror.jsonl \
>    ~/backups/mirror-$(date +%Y%m%d).jsonl
> > ~/.openclaw/workspace/memory/mirror.jsonl
> ```

---

## 8. Incident Response

### 8.1 Suspected Secret Exposure

1. **Identify:** Check `mirror.jsonl` for blocked attempts
2. **Contain:** Run `mpm memory shred <id>` for affected records
3. **Verify:** Confirm deletion with verification checklist
4. **Rotate:** If actual secret was exposed, rotate it immediately
5. **Document:** Record incident in audit log

### 8.2 Unintended Data Storage

1. **Identify:** `grep` for sensitive patterns in `mirror.jsonl`
2. **Assess:** Determine if content actually reached database
3. **Delete:** Use `mpm memory shred` for any affected records
4. **Review:** Check if blocking patterns need updating

---

## 9. Summary

| Security Feature | Implementation |
|-----------------|----------------|
| Input Validation | 17 regex patterns scan all content |
| Storage Protection | File permissions (0600/0700) |
| Audit Logging | JSONL mirror + stderr |
| SQL Injection | Parameterized queries |
| Path Traversal | Path normalization |
| Hard Delete | DELETE + VACUUM |
| Compliance | GDPR/CCPA ready |

**Security First Principle:** "Never store secrets in memory" → The system proactively blocks patterns before they can be saved to disk.

---

**Document Version:** 1.1  
**Last Updated:** 2026-03-29
