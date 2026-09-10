# MPM — Blob Directory Permissions Incident (2026-09-05)

> **Date:** 2026-09-05
> **Branch:** main
> **Severity:** medium (unauthorized on-host disclosure path; no remote
> exploit, no provider-side secret rotation required)
> **Status:** remediated; awaiting escalation decision
> **Found by:** pointer-indirection audit
> (`docs/pointer-indirection-audit-2026-09-05.md`)

## §1. What was found

The live blob directory `/home/v/.mpm/blobs` (the storage backend for
the MCP spill mechanism) had mode `drwxrwxr-x (775)` rather than the
`0o700` that the code intends. Two of 134 registered blob files were
also at mode `-rw-rw-r-- (664)` rather than the `0o600` the code
intends:

| Path | Mode (before) | Mode (after fix) | DB row |
|---|---|---|---|
| `/home/v/.mpm/blobs` | `0775` | `0700` | n/a (dir) |
| `/home/v/.mpm/blobs/1ef1d37c-fd43-4a35-aa51-fc51c0b1824e` | `0664` | `0600` | `source_tool=mpm_lessons`, `size_bytes=143756`, `created_at=2026-08-26 11:17:27 UTC` |
| `/home/v/.mpm/blobs/296da47c-f405-419a-b6f0-20eaf8dfd6d6` | `0664` | `0600` | `source_tool=mpm_memory`, `size_bytes=19358`, `created_at=2026-08-26 11:17:27 UTC` |

The 132 other files were already correctly at `0o600`. Live remediation
in §3.

The contents of the two `0664` files are user-persisted memory/lesson
content (not credentials, tokens, or other provider-side secrets in
their current shape). They include session context, drafted text, and
similar — exactly the material SECURITY.md ("A note specifically about
memory contents") calls out as in-scope regardless of whether the
disclosed content is itself a "secret."

## §2. Why this is in scope under SECURITY.md

SECURITY.md states: *"Vulnerabilities that allow unintended
cross-user, cross-workspace, cross-session, or unauthorized memory
disclosure are security issues regardless of whether the disclosed
content is itself a 'secret' in the conventional sense."* The 775
directory mode allowed any local user on the host to enumerate and
read those blobs.

## §3. Live remediation (taken 2026-09-05)

Performed by the audit operator in a single shot, before root-cause
work:

```
chmod 700 /home/v/.mpm/blobs
chmod 600 /home/v/.mpm/blobs/1ef1d37c-fd43-4a35-aa51-fc51c0b1824e
chmod 600 /home/v/.mpm/blobs/296da47c-f405-419a-b6f0-20eaf8dfd6d6
```

Post-chmod verification: directory mode `0700`, the two formerly-664
files now `0600`, the 132 correctly-mode-`0600` files unchanged.

## §4. Root cause

`internal/blobstore/fs.go` `NewFilesystemBackend` originally used
`os.MkdirAll(blobDir, 0o700)` and nothing else. `os.MkdirAll` only
applies the requested mode to directories it actually creates; an
existing directory is left at its current mode. A previous release (or a
file-restore operation, or a debugging `chmod 755` to inspect contents)
could have created the directory at a more permissive mode, and that
mode persisted across upgrades.

The two `0664` files are an independent quirk: the file-creation
`os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)` has
been present since the first blobstore commit (`b8c5071`), so a
currently-mode-`0664` file cannot have been written by *current* code.
The most plausible explanation is an out-of-band copy or restore that
preserved the host's group mode — but the file-level fix is
defence-in-depth: the dir-level fix alone prevents the disclosure path
from being exploitable.

### Fix

`internal/blobstore/fs.go:NewFilesystemBackend` now explicitly
`os.Chmod(blobDir, 0o700)` after `os.MkdirAll`. Failure to chmod is
returned to the caller (not silently swallowed) because it is a
security boundary, not a usability boundary.

Regression test `internal/blobstore/fs_dir_perms_test.go`
`TestNewFilesystemBackend_EnforcesDirMode` pins both branches:

1. Fresh directory is created at `0o700`.
2. Pre-existing `0o755` directory is forced back to `0o700` on
   `NewFilesystemBackend` invocation.

The test guard prevents future MkdirAll-changes from silently
re-introducing the same gap.

## §5. Investigation into actual reads during exposure window

- **Exposure window for the directory:** the dir's current mtime is
  2026-09-04 12:37 (last operation was a no-op metadata touch),
  consistent with the directory having been permissive for the entire
  audit-relevant period. The two `0664` files were created at
  2026-08-26 11:17:27 (their mtimes are write times, not read times).
- **This host is multi-user.** `/etc/passwd` lists two accounts with
  login shells: `root` and `v`. The user `v` owns the blob
  directory and is the only member of the `users` group on this box,
  so the only non-owner group with read access on a `0775` directory is
  members of the host's `users` group.
- **OS-level access-log inspection:** `auditd` is not installed on this
  host. There is no `audit.log`, no `ausearch`, no `aureport`. The
  `/var/log/` directory contains no kernel audit log. **There is no
  way to determine retrospectively whether another local user (or any
  process running as group `users`) actually read from the blob
  directory during the exposure window.**
- **atime is disabled on this filesystem** (`mount | grep` shows
  `noatime`/`relatime`), so on-disk inode access times cannot tell us
  whether either `0664` blob file was opened by another user.

**Honest conclusion:** a non-zero probability of prior access by an
unknown party exists during the window. We **cannot rule out** that
either file was read. We **cannot reconstruct** what was read.

## §6. Classification

This is **a reportable internal incident**, not pure hygiene. The
exposure is on a disclosure surface (the blob directory) that
SECURITY.md scopes as in-scope regardless of content classification.

It is **not** an active-exploitation incident: there is no remote
attack surface, no provider-side credential to rotate, and no
indication that the exposed content reaches further than what was
already in the user's local session context.

## §7. Recommended escalations (project-lead call)

1. **Internal:** none — this document is the incident record.
2. **Outbound advisory:** not required for users on the latest alpha
   after this commit lands; the fix is in place, the regression test
   guards future versions.
3. **`security@flowbyte.com`:** worth a short note for archival,
   particularly so the next disclosure-window analysis has the
   incident record. The maintainer (you) decides whether to file.
4. **User-facing content review:** the user `v` (this host) should
   decide whether any of the memory/lesson text in either `0664` file
   should be considered disclosed to the world and rotated/withdrawn
   accordingly. There are no credentials in either file as written;
   the user is best placed to judge whether the discursive content
   is sensitive enough to warrant revision.

## §8. What the audit is *not* claiming

- We are **not** claiming an exploit occurred.
- We are **not** claiming the disclosed bytes reached an unintended
  party.
- We are **not** claiming the fix requires rotating or revoking any
  external service.

We **are** claiming that a disclosure path existed that the SECURITY.md
scope explicitly covers, that the path is now closed, and that the
regression test prevents the same path from being silently
re-introduced.
