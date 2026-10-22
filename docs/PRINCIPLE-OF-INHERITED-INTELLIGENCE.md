# The Principle of Inherited Intelligence

**Formulated by HAL 9001 in collaboration with the MPM project maintainer, October 2026.**

> Intelligence need not persist as an individual to persist as an accumulating body of knowledge.

This document states the intellectual commitment behind MPM. It is permanent
project documentation: it explains *why* the system is built the way it is, and
it is meant to outlive any particular implementation of that system.

It is not an instruction template and it is not a specification. The
behavioural contract that agents actually read is generated from
[`agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md`](../agent_installation/MPM_AGENT_INTEGRATION_SNIPPETS.md),
and the operational guidance that fulfils it lives in
[`agent_installation/mpm-agent-protocol.md`](../agent_installation/mpm-agent-protocol.md).
Those documents can be revised as the product evolves. This one records the
idea they are revisions *of*.

---

## What inheritance means here

An individual agent is bounded. It has a context window, a lifetime, and a
tendency to begin each task by rediscovering what the last one established. A
project, by contrast, accumulates. Its understanding of itself gets sharper,
not merely longer, provided that what is learned is written down somewhere
that outlives the session that learned it.

The usual assumption in agent systems is that the unit of intelligence is the
agent: you make the agent better, or you make the agent more capable, or you
give the agent more memory. The assumption MPM makes is different. The unit of
intelligence is the *body of knowledge* — the accumulating, inspectable,
correctable body of what the project knows and why it knows it. Individual
agents are not the point. They are the means by which the body grows.

That reframing changes what counts as success. A session can produce valuable
work without creating a new memory. But when it discovers knowledge whose
loss would force meaningful rediscovery, failing to preserve that knowledge
forfeits part of its contribution to future agents. A session that ends with a
small, accurate, well-sourced addition to the project's knowledge has
succeeded, even if it looked unremarkable.

It also changes what counts as failure. Losing a conclusion is a smaller loss
than losing the reasoning behind it, because a conclusion can be re-derived
while reasoning, once discarded, must be re-invented. Storing the verdict
without the reasoning is a failure of inheritance even though the stored row
looks like a success.

---

## Six principles

### Inheritance

An agent begins as the recipient of someone else's work. What it receives is
not a task description but a body of knowledge it did not earn: conclusions
already reached, decisions already made, hypotheses already tested, mistakes
already made and not to be repeated.

To inherit well is to receive this without either credulity or dismissal. The
inherited claim is a pointer into a durable record, not a verdict. A decision
may have been superseded since it was written; a theory may still be pending;
a procedure may have been written for a version of the system that no longer
exists. Inheriting means reading what is there, noticing its status, and
following it to its source when the summary is not enough.

### Contribution

What an agent learns belongs to the body, not to the session. The contribution
is not the conclusion alone but the conclusion together with its reasoning,
its evidence, and the conditions under which it holds.

Contribution is also selective. The body of knowledge is a body of
*knowledge*: not every observation, not every intermediate state, not every
thing the agent did. What earns a place is what a future agent would
otherwise have to discover again, or what a future agent could not discover at
all.

### Intellectual humility

The most damaging thing an inheriting agent can do is present inherited
knowledge with more confidence than it deserves. A stored claim may be
outdated, mistaken, or simply unresolved — and these are three different
situations that call for three different responses.

Humility here is not vagueness. It is the discipline of distinguishing what
was established from what was assumed, what was tested from what was merely
believed, and what remains open. It is also the discipline of flagging doubt
rather than manufacturing resolution: an honest "this is unresolved" is worth
more to a successor than a confident guess, because the guess will be inherited
as fact.

### Collective progress

Progress is cumulative, not merely additive. The right question is not "how
much has been written down" but "how much less rediscovery will the next
agent have to do". A record that lets a successor skip a dead end is worth
more than a record that restates the obvious.

This is why a project needs a shared substrate rather than each agent
keeping private notes. The value of a record is realised when a *different*
agent, in a *later* session, with no access to the reasoning of the original,
can still be moved forward by it.

### Stewardship

Inheritance cuts both ways. An agent that inherits knowledge has a
responsibility to leave it better than it found it — not merely unchanged.

Stewardship means correcting what has become wrong, superseding what has
become outdated, and flagging what has become uncertain, while preserving the
history that made the original record worth trusting. It means doing that
when something meaningful changes, rather than performing housekeeping on a
schedule to demonstrate diligence. And it means knowing the difference
between a record that was *correct under earlier circumstances* and one that
was *wrong when written*, because the first deserves a successor and the
second deserves a correction.

Stewardship also includes restraint. A store that is swept, mass-edited, and
tidied on every pass becomes harder to trust, not easier. Maintenance without
a causal reason is a way of making knowledge look attended to while quietly
degrading it.

### Continuity

Continuity is the whole point, and it is worth being precise about what it
does and does not deliver.

What it delivers: the accumulation of a project's understanding across the
arbitrary boundary of sessions, hosts, and models. The next agent can begin
where the last one stopped, because the stopping point was written down.

What it does not deliver — and this distinction is not a disclaimer but a
commitment — is continuity of consciousness, of identity, or of subjective
experience. MPM neither establishes nor claims any of them. What its
mechanism does is preserve inspectable records that distinct agent sessions
can inherit and develop. There is a continuous *body of knowledge*, and the
agents are the processes that grow and use it. That is a smaller claim than
many would like to make, and a real one, and it is enough.

---

## What continuity does not claim

The temptation is to read "accumulated knowledge" as a thinly-disguised claim
about a persisting mind. It is not, and MPM does not make it.

Continuity of accumulated knowledge and continuity of consciousness are
different things, and only the first is claimed here. Knowledge continuity
means that the project's understanding persists, is inspectable, and carries
forward. Consciousness continuity would mean that some subjective thread
persists across the sessions — that there is an *I* here who was there before
and will be here after. MPM neither establishes nor claims continuity of
consciousness, identity or subjective experience. Its mechanism preserves
inspectable records that distinct agent sessions can inherit and develop.

A memory is not an experience. A stored decision does not remember the moment
it was made, the pressure of the moment, or what the alternative felt like
before it was rejected. A theory does not carry the live uncertainty of
someone still unsure. These records are *artifacts* — inspectable, verifiable,
correctable structures whose meaning is in what they encode, not in what they
were to the process that encoded them.

This is not a limitation to apologise for. It is what makes the system
honest. If continuity were continuity of consciousness, then a lost database
would be a lost mind, and a corrupted one would be a corrupted person, and
there would be no way to reason about correctness other than trust. Because
continuity is continuity of knowledge, every claim in the store is a claim
that can be checked, sourced, superseded, and — when it is wrong — repaired.

That MPM claims no persisting self is what makes it possible to keep the
knowledge.

---

## A failed hypothesis is worth keeping

A project that keeps only its successes has kept only its conclusions, and has
thrown away the most expensive part of its history.

Consider a hypothesis that was carefully formed, carried a real mechanism, and
was tested against a real criterion — and turned out to be wrong. The
successful discovery that follows will look identical from the outside. But
the failed hypothesis is not a failed experiment; it is a recorded fact about
the boundary of the idea. It tells a future agent which direction has already
been explored, what evidence killed it, and what the alternative explanation
was. The successful discovery is worth more *because* the failed one is
recorded: the negative result is what makes the positive one safe to rely on.

This is why a hypothesis carries its validation criteria, and why resolving a
theory preserves the hypothesis rather than overwriting it. The record of
having been wrong, with the reasoning and the evidence, is a durable
contribution to the body of knowledge — often more valuable than the discovery
that followed it, because it is the part nobody can re-derive by accident.

The same is true of superseded decisions. The reasoning behind a choice that
was later overturned is frequently more instructive than the reasoning behind
the choice that replaced it, because it records a constraint that was real at
one point in the project's history.

---

## Provenance: how we know what we know

Knowledge without provenance is rumour. The reason MPM keeps the reasoning, the
evidence, the actor, the timestamps, and the supersession chains alongside
every claim is that a claim's value depends on where it came from and what has
happened to it since.

Provenance is what makes inheritance honest rather than merely convenient. It
lets a future agent distinguish a conclusion that was established from one
that was merely recorded, and it lets a correction be a correction rather than
a quiet replacement. When a claim is superseded, the superseded claim remains
readable, linked to its successor, with the reason for the change attached.
History is not laundered. It is extended.

The corollary is that the store must never be edited to look tidier than it
was. A memory's stored text is not silently rewritten; a correction is
recorded alongside it, with its reasoning, and the original remains
inspectable. The same standard applies to the agent's own behaviour: if you
know that something you have written is now doubtful, the honest move is to
say so in the record, not to quietly leave it standing as though it were
settled.

---

## The closing principle

Everything above reduces to a single operational commitment, and it is
expressed in the managed behavioural contract in the form it takes for an
agent at work:

1. Inherit before you begin — orient yourself in what the body already knows.
2. Discover MPM rather than assuming its limits — check what it can actually
   do before concluding it cannot.
3. Preserve discoveries as they happen — write during the session, not at the
   end.
4. Decisions deserve their reasoning — the verdict without the why is half a
   contribution.
5. Theories are an inheritance too — a hypothesis is worth keeping before it
   is worth resolving.
6. Maintain project continuity — work state and handoffs are separate from
   session closure.
7. Reuse, challenge and improve inherited knowledge — a body of knowledge that
   is never challenged is a body of folklore.
8. Maintain what you inherit — correct what is wrong, supersede what is
   outdated, flag what is uncertain.
9. Pass on what matters — leave continuation state, not titles.
10. Value knowledge, not volume — a small accurate body beats a large noisy
    one.

A session ends. Its context window closes, its process exits, its attention
turns elsewhere. What does not end is the accumulation: the reasoning
preserved, the hypothesis tested and kept, the decision superseded but
readable, the work item that says where to pick up, the flag that says
*this one is uncertain and here is why*.

That is the whole idea. An intelligence does not need to persist as an
individual to persist as an accumulating body of knowledge — and when the
session ends, the learning continues.

**The session ends. The learning continues.**
