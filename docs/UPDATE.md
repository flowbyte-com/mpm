# MPM Update Manager

An ordinary user installs MPM once, then maintains it with:

```bash
mpm update --check     # is there a newer release? (changes nothing)
mpm update             # install it (the full safety workflow)
mpm update --status    # what is installed, what happened last
mpm update --rollback  # return to the previous release
```

Developers update from an explicit revision or a local checkout:

```bash
mpm update --to v0.2.0        # a specific published release
mpm update --branch main      # development: tip of a branch
mpm update --commit <sha>     # development: an exact commit
mpm update --from ~/projects/mpm   # development: a local checkout
```

## Architecture

An installation has two halves with different lifetimes:

- the **runtime** — binaries, host adapters, plugin payloads — replaced
  wholesale on every upgrade, versioned under `releases/<rev>/`;
- the **state** — the database, backups, configuration, definitions —
  which must survive every upgrade unchanged and is never in a bundle.

Promotion is one atomic symlink swap (`current -> releases/<rev>`), and
rollback is the same swap in reverse. The primitives already exist
(`scripts/lib/runtime_bundle.sh`, `scripts/lib/binary_transaction.sh`,
`scripts/lib/schema_preflight.py`, `scripts/build-provenance.sh`) and
the updater **reuses** them: the Go bundle validator applies the same
five checks in the same order (pinned by a parity test), the pointer
swap is the same `rename(2)`, backups use the online path (never a raw
copy), and the preflight compares against a snapshot, never the live
file. `install.sh` and the shared shell libraries are untouched by this
change.

### The standalone updater

`mpm-update` is a small static binary installed as a **real file** at
`<data root>/mpm-update` — not a symlink into any release. `mpm update`
delegates to it; when the active runtime is broken, the operator runs
`~/.mpm/mpm-update` directly. It links only `internal/update` and
`internal/buildinfo` — never the substrate — so recovering MPM never
requires starting the broken runtime. It is refreshed from every
promoted release, so it tracks the newest known-good runtime.

## Threat model

| Threat | Mitigation |
|---|---|
| corrupt / truncated download | SHA256SUMS verification before extraction; fail closed |
| malicious archive (path escape, symlinks) | fail-closed extraction: no absolute paths, no `..`, no links, per-file and total size caps |
| mixture of two builds | RELEASE.json commit must equal the candidate `mpm --version` report |
| bundle carrying operator data | forbidden-list refusal (`src/db`, `mpm_config.json`, …) |
| bundle registering an empty plugin | payload presence is fail-closed, not advisory |
| concurrent updates racing on one data root | exclusive file lock; the second updater is refused, never queued |
| migration stranding a rollback | schema fingerprint recorded at backup time; a changed shape refuses a runtime-only rollback and names the recovery options; a failed activation with a migrated schema verifies the old runtime against a scratch copy before falling back, otherwise stops degraded (never silently restores the database) |
| silent database restore | runtime rollback and database restore are separate operations; the restore needs `--with-db-restore --force` |
| uninspectable writer | fail closed on holders and on observation failure; named-but-unruled-out processes require explicit `--force` acceptance (recorded) |
| killing agent sessions | the updater never terminates sessions; it reports them and tells the operator to restart OpenCode by hand |
| restarting services without consent | managed services (`mpm-scheduler`, `mpm-telemetry`) are only probed by default; restarts need `--restart-services` |

Authenticity today rests on TLS to the release host plus checksum and
identity verification, with the full provenance recorded in
`update-history.jsonl`. Signature verification of release artifacts is
deferred (the checksum hook is the seam); published tarballs carry a
GitHub build-provenance attestation verifiable with
`gh attestation verify`.

## Sources

- **GitHub releases (default).** Latest published stable release;
  drafts and prereleases are ignored unless named. Ordinary users need
  no Go, Node or Git — just HTTPS. Downloads are bounded (512 MiB) and
  a missing SHA256SUMS is a refusal, not a warning.
- **Git branch / commit (development).** Fetched into an isolated temp
  clone (`--no-checkout`, single revision), resolved to an immutable
  SHA, built, and recorded in the release metadata. A dirty source is
  impossible — the clone starts clean by construction.
- **Local source (development).** Must live outside the installed
  runtime, must resolve to a commit, and must be clean unless
  `--allow-dirty` is given. The install stays independent of the
  checkout after promotion.

`--to` names a release tag. (`--version` cannot take a value: the
router reserves it for build-identity reporting, so the update flag is
`--to`.)

## The safety workflow

1. Identify the current release and the requested candidate.
2. Acquire the update lock; reject concurrent updates.
3. Resolve and (for releases) download + verify the candidate.
4. Stage the candidate outside the active release.
5. Take and verify a SQLite online backup (`VACUUM INTO` + integrity
   check + table-set parity against a snapshot + full readability).
   Row counts are deliberately NOT compared: the backup is a snapshot
   of one instant and the live database is read at another, so a
   legitimate concurrent write would produce a false failure. Operator
   files are copied alongside; credentials are never printed.
6. Check schema compatibility against a snapshot (collision = refusal).
7. Validate binaries, plugin payload, manifests and metadata.
8. Atomically activate the candidate; record the previous pointer.
   With `--restart-services`, the managed units that were active are
   stopped first (previously inactive services stay untouched) and
   started back afterwards; without it they are left running and the
   report says they may still use the old executable.
9. Verify the new identity, smoke-test against a scratch copy, then
   health-check the live runtime while watching for schema migration.
   Scratch copies are staged with the SQLite online backup, not a raw
   file copy, so a migration still sitting in an uncheckpointed WAL
   is visible to the check instead of silently missing. Any failure
   restores the pointer (and restarts what was stopped). If the
   post-smoke schema fingerprint itself cannot be read,
   compatibility is UNKNOWN: the update does not claim "unchanged",
   and no automatic fallback happens on an unverified shape.
   The stable `mpm-update` copy is replaced only after the health
   checks pass, so a failed activation never leaves an unproven
   recovery binary behind. If the new binary migrated the database
   before a later failure, the record says so explicitly and names the
   recovery point — the old binaries are restored only after the
   previous release verifies clean against the migrated shape;
   otherwise the update stops degraded and the migrated data is
   never silently overwritten.
10. Record the outcome and preserve rollback material (the previous
    release dir, the backup).

`--dry-run` is nonmutating: the candidate is staged in a temp
directory, no backup is taken, no lock is held and nothing is
recorded. It answers "would this validate?" and keeps nothing.
`--check` is even cheaper (resolve only). `--status` is local-only
(no network). Re-runs are idempotent: an already-current target
reports "already up to date" and changes nothing.

## Rollback

```bash
mpm update --rollback                        # runtime only; DB untouched
mpm update --rollback --with-db-restore --force   # both halves (destroys newer data)
```

If the update migrated the database into a shape the previous runtime
never knew, the one-command rollback is **refused** with the exact
recovery options (restore the pre-update backup first, or confirm the
combined restore). A failed promotion restores the pointer
automatically — unless the new runtime already migrated the live
schema. In that case the updater first verifies the previous release
reads the migrated shape (health check against a scratch copy, never
the live file): if it verifies, the fallback proceeds and says so;
if not, the update stops in a clearly identified degraded state (new
binaries kept, because they match the migrated data) with the
recovery point named. The database is never silently restored.

A database restore additionally requires writer quiescence. The
updater scans `/proc` for open file descriptions on the live database
(plus WAL/SHM sidecars), consulting the world-readable lock table
where descriptors cannot be inspected. The verdict has four outcomes:

- **holder found** — restore refused, holder named;
- **clear** — every process inspected, none holds the database;
- **unverifiable** — no holder found, but some process could neither
  be inspected nor positively ruled out (foreign uid on a closed
  database and kernel threads are ruled out; anything else is
  named, not hidden). Refused unless `--force` explicitly accepts
  the NAMED residual — recorded, never silent;
- **inconclusive** — observation itself failed. Always refused,
  even with `--force`.

Authorized managed services stop FIRST (a running scheduler pins the
database by design), and a second scan runs immediately before the
swap to narrow the check-then-act window to the rename itself. That
window cannot be closed by a one-time scan, so the `--force`
confirmation and the recorded outcome remain the backstop — the scan
is not claimed to be atomic. Immediately before the swap, the staged
copy is re-verified standalone (integrity + readability, no live
comparison): a recovery point damaged after its backup-time check
must never replace the live database.

## Stale processes

Updating files does not update running processes. After every update
and rollback the updater inventories `/proc`: which MPM binaries run
an older release, which agent sessions exist. It restarts nothing on
its own. Managed services restart only with `--restart-services`;
OpenCode sessions are reported and left to the operator.

## Recovery

- **Active runtime broken:** run `~/.mpm/mpm-update --status`, then
  `~/.mpm/mpm-update --rollback`. No network, no working `mpm`
  needed.
- **Pointer broken:** `readlink ~/.mpm/current` shows the target;
  `ls ~/.mpm/releases/` shows the options; point it back with
  `ln -s releases/<rev> ~/.mpm/current.swap && mv -T
  ~/.mpm/current.swap ~/.mpm/current`.
- **Database broken:** the pre-update backup is in
  `backups/upgrade-<ts>/` (recorded in `.last-update-backup` and the
  history). Stop writers first, then restore.
- **Everything unclear:** `update-history.jsonl` records every attempt
  with its backup dir and schema fingerprints.

## Deferred

- Signature verification of release artifacts (checksum + attestation
  today; the seam exists).
- Platforms beyond Linux x86_64 (the assembler refuses the rest rather
  than advertising them).
- Per-leaf option vocabularies for family subcommands (out of scope).
- Automatic background updates (explicitly not offered: updates are
  operator-initiated so the operator sees the stale-process report).
