# MPM Architecture

This document is the canonical home for MPM's load-bearing architectural principles. It is short by design — every principle here is referenced from elsewhere in the documentation; this is where they are *stated*.

For operator-facing system architecture (Core vs Runtime, retrieval, MCP, multi-agent federation), see the [README](../README.md).

---

## The Projection Principle

MPM is *closed under observation.* Every node, action, and lifecycle event is introspectable via the substrate itself. To maintain this:

- The substrate records facts.
- The substrate records facts about facts (events, invocations).
- The substrate **never** records views of facts.
- Every projection or view is computed from authoritative state at read time.
- Commands and CLI surfaces may evolve. Truth may not.

The operational form of this principle is the [Projection Test](../CLAUDE.md), which gates every schema change at PR review. Both say the same thing; the Principle is what the architecture is built on, the Test is what you apply before adding a new table, column, cache, score, or summary.

---

## Pointer Architecture (Phase 1)

Phase 1 adds blob storage for oversized MCP tool results and a URI-based resolution layer. Oversized JSON responses that exceed the MCP payload limit are written to `internal/blobstore/` as files and replaced in the response with a `mpm://blob/<id>` pointer URI. `internal/pointer/` defines the URI grammar (`mpm://blob/<id>`) and the `Resolver` interface; consumers implement the interface to retrieve the resolved content. The blob store uses atomic `Put` operations, enforces UTF-8 boundary protection on filenames, and supports garbage collection of expired and orphan files via `mpm blob gc`.
