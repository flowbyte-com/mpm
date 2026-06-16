MPM - Memory Persistence Module

SQLite-native memory and reasoning infrastructure for autonomous AI agents

Modern AI agents remember facts but forget reasoning.

They repeat failed experiments, revisit decisions that were already made, and lose the context that led to previous conclusions.

MPM solves this problem by persisting not only what an agent knows, but also:

Why it believes it
How decisions were made
Which hypotheses are still unproven
What evidence challenged existing knowledge

All of this lives inside a single SQLite database with no external services required.

The Problem

Most AI memory systems focus on retrieval.

An agent stores information, retrieves information, and continues operating.

The problem is that real cognition involves more than facts.

Consider a software engineering agent:

Fact:
WordPress strips inline style tags.

Decision:
Store widget CSS in wp_options.

Hypothesis:
The CLI parser fails when --json appears before positional arguments.

Evidence:
Unit tests confirm the parser bug.

Conclusion:
Flag ordering caused the issue.

Most memory systems flatten these into generic notes.

Months later the agent remembers the conclusion but not the reasoning.

The result:

Repeated investigations
Reopened decisions
Contradictory conclusions
Lost institutional knowledge
The Solution

MPM is a persistent memory and reasoning layer for AI agents.

It provides:

Long-term memory
Decision tracking
Hypothesis management
Proactive recall
Behavioral modes
Persona management
Knowledge self-correction

Everything is stored in a unified SQLite database.

No vector database.

No daemon.

No external services.

No distributed infrastructure.

Just one binary.

Why MPM Is Different

Most memory systems store facts.

MPM stores reasoning.

Capability	Traditional Memory	MPM
Fact Storage	✓	✓
Long-Term Recall	✓	✓
Semantic Search	✓	✓
Decision Tracking	✗	✓
Hypothesis Management	✗	✓
Evidence Chains	✗	✓
Knowledge Challenges	✗	✓
Proactive Recall	Partial	✓
Reasoning Persistence	✗	✓

The goal is not simply remembering information.

The goal is preserving intellectual progress.

Core Concepts
Memories

Memories are general facts, observations, and synthesized insights.

Germany leads Group E with +6 goal differential.

Memories are weighted, searchable, reinforced, challenged, and eventually archived.

Decisions

Decisions capture choices together with context and rationale.

CONTEXT:
Need CSS injection that survives wp_kses filtering.

CHOICE:
Store widget CSS in wp_options.

RATIONALE:
WordPress strips inline style tags.

This allows agents to reconstruct previous reasoning instead of re-evaluating the same problems repeatedly.

Theories

Theories represent hypotheses that have not yet been proven.

HYPOTHESIS:
Flag ordering causes parser failure.

VALIDATION:
Run parser tests with positional-first and flag-first inputs.

STATUS:
Pending

Theories create a structured workflow for experimentation and debugging.

Lessons

Lessons capture reusable knowledge.

Examples:

Best practices
Warnings
Patterns
Insights

Lessons are intended to survive beyond individual tasks and projects.

Sessions

Sessions store operational context.

Examples:

Current project
Active model
Working directory
Runtime state

This allows agents to resume work after interruptions.

The Epistemology Engine

The Epistemology Engine is MPM's defining capability.

It answers questions that traditional memory systems cannot.

Not:

What does the agent know?

But:

Why does the agent believe it?

And:

Has that belief been tested?

And:

What evidence could invalidate it?

This creates a persistent record of reasoning over time.

Decision Ledger

The Decision Ledger records architectural and operational choices.

Weeks or months later, an agent can reconstruct:

Context
Choice
Rationale
Outcome

Instead of rediscovering previous conclusions, the agent can continue building upon them.

Theory Tracker

The Theory Tracker manages assumptions and experiments.

Workflow:

Hypothesis
    ↓
Validation
    ↓
Evidence
    ↓
Confirmed / Disproven

This encourages testable reasoning rather than speculation.

The Cognitive Immune System

Knowledge becomes obsolete.

Most memory systems never address this problem.

MPM introduces a challenge workflow.

Memory
    ↓
Challenge
    ↓
Theory
    ↓
Evidence Collection
    ↓
Confirmed / Disproven

When new evidence appears:

A memory is challenged
A theory is created
Evidence is gathered
The theory is resolved
Knowledge is updated

This provides a mechanism for self-correction.

Proactive Recall

Traditional memory systems wait for a search query.

MPM actively surfaces relevant knowledge before it is requested.

For example:

mpm kb hint "discussing widget CSS architecture"

Output:

You previously decided:

Route widget CSS through wp_options.

RATIONALE:
WordPress strips inline style tags.

This reduces duplicated work and improves continuity.

Long-Term Memory

Every memory has a weight.

Weights increase through reinforcement.

Weights decrease through decay.

This creates a dynamic memory system that naturally prioritizes useful information.

Weight ↑
    Frequently used knowledge

Weight ↓
    Stale or irrelevant knowledge

Memories crossing defined thresholds become long-term memories.

Retrieval Architecture

MPM combines:

SQLite FTS5
BM25 ranking
Semantic embeddings
Reinforcement history
Recency scoring

This enables both:

mpm "world cup prediction"

and

mpm recall --semantic "为什么德国队表现这么好"

to retrieve relevant knowledge.

Machine Interface

All functionality is exposed through a universal JSON boundary.

mpm call <tool> --payload JSON

Examples:

mpm call save_to_memory \
  --payload '{"fact":"Germany leads Group E"}'

mpm call query_long_term_memory \
  --payload '{"query":"World Cup prediction"}'

mpm call propose_theory \
  --payload '{"hypothesis":"Flag ordering bug"}'

OpenClaw and Hermes both use this same interface.

Agent Integration

MPM integrates directly with AI agents.

Current integrations:

OpenClaw
Hermes

Both systems share:

The same database
The same reasoning model
The same tool interface
The same memory lifecycle

This enables consistent cognition across platforms.

Real-Time Telemetry

The web interface includes live telemetry through Server-Sent Events.

Events include:

Memory creation
Theory proposals
Theory resolution
Challenge actions
Lesson creation

Connected clients receive updates immediately without polling.

Reliability

MPM is designed for long-running autonomous operation.

Features include:

SQLite WAL mode
Dead-letter queues
Synthesis isolation
Retry pipelines
Event replay buffers
Watchdog telemetry
Overflow protection

The goal is predictable behavior under sustained workloads.

Security

Before data reaches storage, MPM scans content for:

API keys
JWT tokens
SSH keys
Password patterns
Connection strings

Sensitive content can be blocked before entering the database.

Future roadmap items include SQLCipher-based encryption at rest.

Architecture
┌─────────────────────────────────┐
│            AI Agent             │
└──────────────┬──────────────────┘
               │
               ▼
┌─────────────────────────────────┐
│            MPM Core             │
│                                 │
│ • Memory Engine                 │
│ • Epistemology Engine           │
│ • Decision Ledger              │
│ • Theory Tracker               │
│ • Recall Engine                │
│ • Telemetry Layer              │
└──────────────┬──────────────────┘
               │
               ▼
┌─────────────────────────────────┐
│         SQLite Database         │
│                                 │
│ • Memories                      │
│ • Decisions                     │
│ • Theories                      │
│ • Lessons                       │
│ • Topics                        │
│ • References                    │
└─────────────────────────────────┘
Quick Start

Search memory:

mpm "World Cup prediction"

Add a memory:

mpm add "Germany leads Group E"

Record a decision:

mpm record_decision

Create a theory:

mpm propose_theory

View recent context:

mpm wake

View system status:

mpm status
Use Cases
Software Engineering Agents

Preserve debugging investigations, architectural decisions, and implementation rationale.

Research Agents

Track hypotheses, evidence, and conclusions across long-running investigations.

Enterprise Knowledge Retention

Capture institutional reasoning that would otherwise disappear into chat logs.

Multi-Agent Systems

Provide a shared cognitive substrate across multiple autonomous agents.

Autonomous Operations

Maintain continuity across long-running workflows.

Roadmap

Planned future capabilities include:

Concept Drift Detection
Multi-Agent Shared Epistemology
Event-Driven Hooks
Memory Encryption at Rest
Advanced Cognitive Analytics
Governance and Compliance Extensions
Vision

Most AI memory systems answer:

What does the agent remember?

MPM attempts to answer:

What does the agent know, why does it believe it, and what evidence could prove it wrong?

That distinction transforms memory from passive storage into persistent reasoning.

Conclusion

MPM is not merely a memory database for AI agents.

It is a reasoning infrastructure layer that preserves facts, decisions, theories, lessons, and evidence across time.

By making reasoning explicit and durable, MPM enables autonomous systems to build upon prior knowledge rather than continually rediscover it.
