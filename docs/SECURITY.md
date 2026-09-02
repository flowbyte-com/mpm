# Security Policy

## Reporting a vulnerability

**Do not open a public GitHub issue for security vulnerabilities.** Anything
tagged with a label, comment, or commit message that hints at a vulnerability
becomes a public disclosure the moment a fork pulls it.

Report privately to **security@flowbyte.com** (routes to the project lead).
Email leaves a paper trail, and paper trails help when the timeline of a
disclosure is questioned later. Reporters who already know the maintainer
directly may use that channel — but the public policy routes through email
so there is exactly one auditable disclosure path.

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

**When uncertain whether something is security-sensitive, report it
privately rather than testing it publicly.** False positives are cheap;
public testing of a real vulnerability can burn the disclosure window
before a fix ships, and the reporter ends up being the disclosure
vector.

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

- The `mpm` CLI binary (single-process daemon; file ingestion is
  operator-driven via `mpm cascade materialize`, not watcher-based —
  the watch daemon was deprecated in commit `6588cb8` and
  hard-removed in `215fd09`)
- `mpm-mcp` (MCP server surface)
- `mpm-scheduler` (universal wake executor)
- `mpm-agent` companion binary (Telegram bot, MCP client, CLI REPL)
- Agent plugins under `agent_installation/` (hermes, openclaw, opencode)
- The SQLite data plane (`src/db/mpm.db`, FTS5 indexes, triggers)
- The **shared database** (`shared.memories`, `shared.memories_fts`,
  trigger-based sync, federated `scope=all` path)
- Backup and restore (`mpm backup-db`, `mpm restore-db`)
- Configuration (`mpm_config.json` and its equivalents)
- The skill system (loaded skills, skill content, registry interception)
- File ingestion surface (`mpm cascade materialize`); the legacy
  fsnotify-based watch daemon is no longer compiled into `mpm` and is
  not reachable as an attack surface
- Pre-commit hooks and CI workflows (`.github/workflows/`,
  `.superpowers/`)
- **Dependencies used directly by MPM.** Vulnerabilities in a Go
  module that MPM imports, or a binary MPM invokes, are in scope
  when the vulnerability materially affects MPM users. Report upstream
  dependency vulnerabilities to the upstream project as appropriate,
  but also notify MPM when MPM users are exposed — supply-chain
  vulnerabilities are part of application security.
- **Security-relevant documentation defects.** Documentation that
  causes users to expose credentials, disable security controls,
  misconfigure authentication, or otherwise create a security
  vulnerability. The general "behaviour change without a corresponding
  README change is a documentation bug" rule lives in `CONTRIBUTING.md`
  and is broader than security; only the security-relevant subset
  belongs here.

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

## A note specifically about memory contents

MPM is a memory system. Users may reasonably store private or sensitive
information in memories — not just credentials, but session context,
working notes, draft text, and other material the user did not intend
to publish. Vulnerabilities that allow unintended cross-user,
cross-workspace, cross-session, or unauthorized memory disclosure are
security issues regardless of whether the disclosed content is itself
a "secret" in the conventional sense. This is broader than the
"credentials in code" rule below; memory contents are the substrate's
primary data, and protections against unauthorized disclosure of that
data are part of application security, not just hygiene.

Report anything that looks like a memory-disclosure path through the
same channel as any other vulnerability. If uncertain whether observed
behaviour counts, report it — false positives are cheap.

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
has previously entered git history.** Once a secret is in a reachable
commit, assume it may have been copied into clones, forks, mirrors,
CI caches, backups, and other retained Git objects — anywhere that
commit has been fetched, it persists until actively removed. Deleting
the file in a later commit removes it from the *current* working tree
only.

This is not a hypothetical. It has happened in this project's own
development history: at various points, runtime state and developer
build artifacts (including a debug binary and several personal
workspace files) were tracked in commits, then untracked later. The
untracking is real and complete for the current working tree. The
historical commits remain in the git object database and, where they
were pushed, in retained remote objects as well. If you find something
you believe is a real credential — yours or anyone else's — in any
branch, tag, or commit reachable from `origin`, treat it as a security
incident, not as cleanup:

1. **Report to `security@flowbyte.com` immediately.** Do not email
   the list and wait; the secret is exposed until rotation happens.
2. **The maintainer rotates or revokes the credential at the
   provider.** Rotation is the actual remediation; everything below
   is damage control for what was already fetched.
3. **The maintainer rewrites local history** (`git filter-repo` or
   equivalent) and force-pushes the cleaned history. The force-push
   replaces the rewritten refs on `origin`; it does **not** by itself
   remove objects already retained by the remote host's object store,
   fork network, or any mirror that has fetched the affected SHAs.
4. **If the secret was pushed to GitHub, the maintainer requests
   GitHub's sensitive-data / history removal workflow where
   necessary.** A force-push alone is not assumed to remove retained
   GitHub objects. The "remove sensitive data from a repository"
   contact path is the appropriate escalation for objects that have
   already reached the host — required for things that a
   `filter-repo` + force-push cannot reach (cached views, fork
   graph traversal surfaces, internal snapshots).
5. **All collaborators and CI re-clone from the cleaned history.**
   Existing clones, forks, and CI caches must be replaced; reflogs,
   ref bundles, and unreachable objects in those clones are not
   cleared by a force-push.
6. **The incident is recorded internally.** A public disclosure in
   `changelog.md` is included only when appropriate — security
   incidents can themselves contain information we don't want
   publicly documented, and we do not want to turn every credential
   mistake into a permanent public artifact. The goal is that the
   next contributor learns the failure mode; whether that lesson is
   public is a per-incident call.

If the "secret" is a local-only artifact with no provider-side value
(an internal debug binary, a captured agent transcript with no
real-world data), rotation is not required, but history rewriting
still is — and the same remote-retained-objects caveat applies if
the artifact was ever pushed.

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
relevant release, unless the reporter opts out. Internal incident
records are kept regardless.

---

_Last updated: 2026-08-10, for the `mpm-alpha` release._
