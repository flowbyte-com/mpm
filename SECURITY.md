# Security Policy

## Reporting a vulnerability

**Do not open a public GitHub issue for security vulnerabilities.** Anything
tagged with a label, comment, or commit message that hints at a vulnerability
becomes a public disclosure the moment a fork pulls it.

Report privately to **security@flowbyte.com** (routes to the project lead).
If you already have a direct channel to v, you can use that instead — but
email leaves a paper trail, and paper trails help when the timeline of a
disclosure is questioned later.

### What to include

The more of the following you can provide, the faster the report moves:

- **Affected component and version.** Commit SHA if you have one. Affected
  tag if you can narrow it down. "Latest" is acceptable but slower.
- **Reproduction steps.** Exact commands, inputs, and the workspace state
  you started from. The stranger test (`scripts/stranger-test.sh`) is a
  good baseline; if your repro looks nothing like it, say so.
- **Observed behaviour vs. expected behaviour.** One paragraph each is
  usually enough.
- **Logs, stack traces, or crash output** — *redacted of secrets*. If a
  trace contains an API key, redact the key and tell us the prefix; do
  not paste the key itself.
- **Exploitability assessment** if you have one. "Crash on bad input" and
  "attacker can read arbitrary files" need different urgency. You do not
  need a working exploit — a clear description of the vulnerable code
  path is enough.

### What to expect back

- **Acknowledgement within 72 hours.** If you don't hear back in that
  window, ping v directly. Email gets filtered; humans don't.
- **Triage within 7 days.** A response confirming whether the report is
  valid, what the planned fix is, and a rough timeline.
- **Fix timeline.** Critical (RCE, credential leak, data corruption
  reachable by a remote attacker): same week. High: two to four weeks.
  Medium and Low: next minor release.
- **Credit.** Tell us in the report if you want attribution. The default
  is credited; the opt-out is honoured.

If the report turns out to be a misunderstanding rather than a
vulnerability, you will still get a response. We do not punish reports
that turn out to be wrong.

## Supported versions

This is **alpha**. There is exactly one supported line:

- **Latest tagged alpha** (`mpm-alpha`, and any commits between this tag
  and the next release). Security fixes ship here as soon as they are
  ready.

There is no LTS branch. There is no backport policy. Older tags are not
patched — please upgrade.

APIs, CLI surfaces, on-disk formats, schema, and configuration layouts
may change without notice during alpha. This is the deal. Pin a SHA if
you depend on a specific shape; pin a tag if you want stable behaviour.

## Scope

### In scope

- The `mpm` CLI binary (single-process daemon + fsnotify watcher)
- `mpm-mcp` (MCP server surface)
- `mpm-scheduler` (universal wake executor)
- `mpm-agent` companion binary (Telegram bot, MCP client, CLI REPL)
- Agent plugins under `agent-plugins/` (hermes, openclaw, opencode)
- The SQLite data plane (`src/db/mpm.db`, FTS5 indexes, triggers)
- The **shared database** (`shared.memories`, `shared.memories_fts`,
  trigger-based sync, federated `scope=all` path)
- Backup and restore (`mpm backup-db`, `mpm restore-db`)
- Configuration (`mpm_config.json` and its equivalents)
- The skill system (loaded skills, skill content, registry interception)
- The watch daemon (fsnotify-based file ingestion)
- Pre-commit hooks and CI workflows (`.github/workflows/`,
  `.superpowers/`)
- Documentation that contradicts the actual behaviour of the code

### Out of scope for this repository

- Vulnerabilities in third-party LLM providers or embedding services
  (Ollama, OpenAI, etc.) — file with the provider
- Vulnerabilities in SQLite itself — file upstream
- Vulnerabilities in the agent frameworks that *consume* MPM (OpenClaw,
  Hermes, OpenCode) — file with those projects
- MPM itself behaving correctly when given malicious *content* (a
  poisoned memory). The 19-pattern scanner in `SaveMemoryNode` exists;
  bypasses of that scanner are in scope; behaviour with adversarial
  input that the scanner catches is not.

## A note specifically about credentials and runtime state

MPM integrates with agent frameworks and external providers. **Users
will try to store API keys, bot tokens, OAuth refresh tokens, session
transcripts, and other secrets in it — because that is what memory
systems get used for.** This is exactly what we do not want.

### Do not commit

- **API keys, bot tokens, OAuth refresh tokens, or any other secrets.**
  Not in code. Not in fixtures. Not in test corpora. Not in
  documentation. Not in issue reports. Not in benchmark scripts. If
  you need a token-shaped string in a test, use a clearly-fake value
  like `sk-test-aaaaaaaaaaaaaaaa`.
- **Runtime state.** This includes, but is not limited to:
  - The SQLite database (`src/db/mpm.db`)
  - Backup SQL dumps (`mpm-backup-*.sql`)
  - WAL / SHM journal files (`*.db-wal`, `*.db-shm`)
  - The populated runtime config (`mpm_config.json`; the
    `.example` template is fine, the populated file is not)
  - Session files, scratchpads, captured agent state
  - Personal workspace files (`MEMORY.md`, `IDENTITY.md`, `SOUL.md`,
    personal `.opencode/`, `reference/` corpora)
  - systemd unit files with real hostnames (the templates in
    `contrib/systemd/` are already excluded)

The `.gitignore` at the repository root catches the common cases. If
you find a gap, please open an issue rather than working around it by
deleting files you were about to commit — the workaround hides the
gap from the next person.

### If a secret has already entered git history

**Removing a secret from the working tree does not make it safe if it
has previously entered git history.** Once a secret is in a commit, it
is in every clone, every fork, every mirror, every CI cache, every
`git log -p` that anyone runs against the repo. Deleting the file in
a later commit removes it from the *current* working tree only.

This is not a hypothetical. It has happened in this project's own
development history: at various points, runtime state and developer
build artifacts (including a debug binary and several personal
workspace files) were tracked in commits, then untracked later. The
untracking is real and complete for the current working tree. The
historical commits remain in the git object database. If you find
something you believe is a real credential — yours or anyone else's —
in any branch, tag, or commit reachable from `origin`, treat it as a
security incident, not as cleanup:

1. Report to **security@flowbyte.com** immediately. Do not email the
   list and wait; the secret is exposed until rotation happens.
2. The maintainer rotates the credential at the provider.
3. The maintainer rewrites history (`git filter-repo` or equivalent)
   and force-pushes.
4. All collaborators and CI re-clone.
5. A post-mortem entry is added to `changelog.md` so the failure
   mode is visible to the next contributor.

If the "secret" is a local-only artifact with no provider-side value
(an internal debug binary, a captured agent transcript with no
real-world data), rotation is not required, but history rewriting
still is.

## Disclosures without prior public notice

For actively-exploitable vulnerabilities, security fixes may ship
without prior public disclosure. The fix commit and the corresponding
release notes will say so explicitly. Coordinated disclosure is the
default; silent embargo is reserved for things that are causing harm
right now.

If you would prefer to disclose outside GitHub — for example because
you have found something that affects a downstream user and want to
coordinate a public advisory — contact the maintainer directly. Do
not post to social media, public mailing lists, or chat rooms before
a disclosure timeline has been agreed.

## Acknowledgements

Reports that lead to a fix are credited in `changelog.md` under the
relevant release, unless the reporter opts out.

---

_Last updated: 2026-08-09, for the `mpm-alpha` release._