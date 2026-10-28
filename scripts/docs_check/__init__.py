"""Documentation freshness gate for MPM.

This package implements `make docs-check`. It answers one question: *does the
documentation still agree with the repository?*

The design constraint that shapes everything here is the distinction between

  * an **objective defect** — a link that resolves to nothing, a command that
    does not exist, a tool name that is not in the registry, a managed block
    that has drifted from its source; and
  * a **semantic concern** — a document that is now stale but still parses, a
    contract change that probably needs a doc update somewhere.

Only the first class may fail CI. The second is reported as an actionable
finding, because a checker that fails on judgement calls gets disabled, and a
disabled checker protects nothing.

Nothing in this package opens a database, reads MPM state, contacts a network
service, or needs credentials. It reads files in the working tree and, for the
docs-impact check only, `git log` of the current branch.
"""

__version__ = "1.0.0"
