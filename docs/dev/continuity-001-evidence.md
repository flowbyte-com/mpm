# Continuity 001, PR 5 — the engineering evidence, the publisher and the read-only `check`

The design of record for the layer that lets another session, a reviewer or an
operator find the engineering queue and check its evidence without a pasted
handover.

The product contract is `docs/SPEC.md` §6.17. This note records the decisions
behind it, and the arguments for them.

---

## 1. The gap

PR 1 gave MPM a durable assignment. PR 2 gave it the service, the CLI and the
agent surface. PR 3 gave it the execution path and the independent reviewer.
PR 4 gave it the bounded dispatcher.

What none of them gave MPM is a way for somebody else to **find** that work and
**check** it. Today the queue exists in a database, the evidence exists in
review sessions and pull requests, and the only way to learn any of it is to be
told.

That is the gap PR 5 closes, and it closes it in two pieces:

- a **versioned, machine-readable evidence record** of engineering work, with a
  **published index** an independent reader can reach;
- a **read-only `check`** that reads the published index and verifies it
  against GitHub.

MPM coordinates. GitHub is the externally inspectable engineering record.
Neither may invent approval, review completion or successful execution.

---

## 2. The four facts that are not interchangeable

The single most important property of the contract is what it keeps apart.

| fact | what it is | what it is NOT |
|---|---|---|
| a runtime reported success | the adapter's own report of an attempt | task completion |
| captured findings | bytes the HOST captured over what a reviewer produced | a review verdict |
| a review verdict | the reviewer's own fenced transition | operator approval |
| an operator approval | a recorded decision: an action, a revision, an evidence pointer | a merge |

PR 1's schema anticipated this: `agent_tasks` carries `reviewed_sha`,
`findings_sha256`, `evidence_ref`, `base_sha`, `head_sha`, `issue_ref`,
`pr_ref`, `awaiting_action` and the four approval fields — and PR 3's review
path writes the captured evidence on the **reviewer's** row while the **verdict**
moves the **subject**. So the record is a projection of what the store already
holds, and it does not need a new schema. (§11 forbids one anyway.)

The **provenance categories** are published with the record:

- `observed` — a fact about the outside world, checked against GitHub.
- `recorded` — a claim the assignment store makes about itself.
- `decision` — a reviewer's verdict, or an operator's approval.

The categories are **assigned**, not chosen by the publisher: a field's category
is a property of what the field IS, and a publisher that could label a verdict
as an observation would be able to lie by labelling.

**Unknown values are explicit.** `unknown` is a value, not an absence. An
unknown SHA is never the current branch head, and never an empty field a reader
might mistake for a fact. The record validation refuses anything that is
neither a 40-hex commit id nor `unknown`.

---

## 3. The published layout

```
index.json                 the MUTABLE pointer at the current snapshot
records/<id>.json          one IMMUTABLE evidence record
records/<id>.md            the same record, human-readable
summaries/queue.md         the engineering queue, human-readable
```

A dedicated branch of the repository the evidence describes (`evidence` by
default). The alternatives were a GitHub issue, a gist, or a release — and each
of them is worse for the same reason: none of them is a **content-addressed,
immutable, verifiable** place to put a snapshot, and none of them can be read
with a single GET and a SHA-256.

**The identifier is the digest of the content.** A record is named by the
SHA-256 of its own bytes, so:

- the same evidence always lands on the same file, and a republication is
  invisible rather than noisy;
- a reader can download the record, hash it, and compare the hash with the
  name the index gave it;
- the snapshot's identity is the digest over the list of record identities, so
  the same corpus has the same snapshot id.

**Nothing contains the SHA of the content that contains it.** The index names
the records; the records do not name the index. Every digest a reader is given
is therefore one the reader can check — and the index cannot authenticate
itself by claiming its own SHA, which is a claim that could never be computed.

**The index names the HUMAN artefacts too.** Every `records/<id>.md` and the
queue summary is listed with its digest, so a reader can check the Markdown a
person reads as exactly as they can check the record a machine reads. Before
this, the human artefacts were covered by nothing: a reader who verified a
record had verified nothing about the documents anybody actually opens.

**A partial publication says it is partial.** The index carries `truncated`,
`scanned` and `scan_limit`. A reader that took a partial queue for the whole
one would be misled, and a silent truncation is worse than a visible one.

**Publication is deterministic** for identical input evidence: the records are
a pure function of the evidence, and the only field that differs between two
publications is `published_at` in the index, which is recorded metadata.

**The pointer is written last, and the commit is atomic.** The records are
staged first, the index last, and the publication is one commit that moves the
branch's ref. An interruption before the commit leaves the branch's pointer
naming the PREVIOUS snapshot — which is true — rather than a corpus that is
half written. A retry is safe: the records are content-addressed, so a retry
rewrites the same files.

---

## 4. What is published, and what is not

The publisher publishes the assignment store's **claims** and GitHub's
**observed facts**.

- A **reviewer finding captured from a real host session** may be published as
  host-captured evidence — with its digest, so a reader can check it. It is
  **not** a GitHub review unless such a review actually exists, and the record
  keeps the two apart.
- A **manifest supplied by an agent** cannot become authoritative because the
  agent wrote it. The publisher reads the store and GitHub, and verifies the
  digest of any host-captured evidence before publishing it.
- Where the evidence is **missing**, the record says so: `unknown`, and a
  `review_disposition` of `none`. A missing digest never becomes an approval.

**The captured digest is CHECKED, and the answer is published.** A digest in
the store is a **claim**: anybody can put 64 hex characters in that column. So
the publisher reads the bytes and hashes them, and there are three answers —
which are three different facts:

| status | what the publisher found | what is published |
|---|---|---|
| `verified` | the bytes are there and they hash to the digest | the digest |
| `unverifiable` | the bytes are somewhere the publisher cannot read | `unknown` |
| `contradicted` | the bytes are there and they do **not** hash to the digest | `unknown`, and the contradiction is named |

A **refuted** digest is never published as though it were true. The status
travels **with** the digest, so a reader can tell a checked digest from an
unchecked one without looking anywhere else.

**Nothing that could leak is published.** The record has no field that carries
model output at all — a review's findings are published as a digest and a
pointer, never as text — and every string it does carry is screened before the
record is hashed:

- credential shapes (provider keys, GitHub tokens, cloud keys, chat tokens,
  bearer headers, private keys);
- private paths (the operator's home, the data root, the platform homes);
- transcript shapes (more than one line, more than the length cap, more than
  the word cap).

**The OBSERVED side is screened like the claims.** The store's own text is
MPM's, and the substrate saw it; a pull request's title, a review's body and a
workflow's or job's name are **other people's text**, and the substrate never
saw any of it. That is exactly the text that must not reach the published
bytes, so it goes through the same redactor. A withheld value becomes
`unknown`, and the record **names the field it withheld** — never the content.

The redaction is the first line of defence and the publishable check is the
second: a record that somehow carries a credential in an observed field is
**refused**, not published.

---

## 5. The queue's vocabulary

The published records are filed into the queue's sections, and the filing is
**derived** from each record's own fields by one function — so the human
report and the machine report cannot disagree about where a task sits.

| disposition | what it means |
|---|---|
| `active` | the work is in flight |
| `awaiting_review` | the work is finished and no reviewer has recorded a verdict |
| `awaiting_ci` | the work is finished and its CI is not green — or is not known |
| `awaiting_operator` | the work is waiting for a person |
| `blocked` | the work was cancelled, or a reviewer asked for changes |
| `ambiguous` | the outcome could not be determined |
| `completed` | the work finished, its CI is green and nothing is pending |

Two of those rules are worth stating outright, because both were found by
review rather than by a failure:

- A finished task with **no verdict** is `awaiting_review`, not `awaiting_ci`.
  Filing it under CI would tell an operator the queue is waiting on the wrong
  thing, and the reviewer would never be dispatched.
- A task in a state the publisher does **not** know is `ambiguous`. The
  substrate said something the record cannot place, and a record that claimed
  a place would be inventing one.

A **reviewer's own record is filed by the review**. The rules above are the
*subject's* rules — they ask whether the work's CI is green and whether a
verdict about the work is outstanding — and neither question is about the
review itself. A reviewer's assignment IS the review: a finished review is
done work, and the verdict it reached belongs to the subject it judged, which
carries it on the subject's own record. Without this rule a finished review
was filed under "awaiting review", which tells an operator the queue is
waiting for a reviewer who has already finished.

---

## 6. The verification

A published manifest is an **index of evidence**, not proof that its claims are
still true. The moment it was published, the pull request could have advanced,
a workflow run could have started, and a review could have been overtaken by a
new commit.

So `check` checks every material external assertion against GitHub:

| check | what it compares |
|---|---|
| `pull_request` | the pull request exists |
| `repository` | it belongs to the expected repository |
| `pr_head` | its actual head equals the recorded head |
| `pr_state` | merged, open or draft — and a record that calls the work completed while the pull request is an open draft is a contradiction |
| `reviewed_sha` | the reviewed revision matches the intended one, and a head that advanced makes the review **stale** |
| `ci` | each cited run exists, belongs to the recorded commit, concluded `success`, and has no job that is not green |
| `review_evidence` | the published bytes hash to the recorded digest |
| `review_verdict` | a verdict is a verdict the service records, or a reported one labelled as reported |
| `operator_approval` | an approval exists only where one was recorded |

Each check ends in one of five states: `verified`, `stale`, `failed`, `missing`
and `unverified`. The record's verdict is the **worst** of them, and the worst
is ordered so that an unverified record is never presented as a verified one.

**The decision checks are falsifiable.** "The record says a reviewer passed
this" is a claim about the world, so it has to be checkable and it has to be
able to come out wrong: a verdict for a revision the pull request has moved
past is **stale**, and an approval to merge a pull request that is not merged
is **failed**. Each check's detail says explicitly what was **not**
established, because "verified" with a silent gap is the failure mode.

**The stale section asks the checks, not the aggregate.** The aggregate verdict
is the worst check, and `unverified` outranks `stale` — so a record whose review
is stale and whose CI could not be read aggregates to `unverified`, and a
reader would never learn that the review is stale. One stale check is enough.

**The verifier looks on the branch the READER was told.** The branch is the
reader's configuration; a record may not name its own branch. So the branch is
threaded into the verifier, and a verifier that hard-coded the default would
report a missing capture for a capture that is right there.

**A published pointer is data, not a command.** The verifier fetches only safe
relative paths inside the evidence repository, so a record that was fetched
from anywhere cannot make the verifier read anything else.

**A network failure is never a successful verification.** `unverified` is a
distinct state precisely because "GitHub could not be asked" and "the thing is
not there" are different facts, and a reader who confuses them will report a
missing pull request as a verified absence.

The digest check comes FIRST, before any claim is read out of a record: a
record whose bytes do not hash to the identifier the index gave it is not the
record the index names, and everything said about it afterwards would be about
something else.

---

## 7. The read-only `check`

Three properties matter more than the shape.

**It works without the operator's local MPM database.** The queue comes from
the published evidence and from GitHub. A fresh reviewer on a machine that has
never had MPM installed can run it, and it needs no publication credential: a
public repository is readable without one.

**It is strictly read-only.** It writes nothing to the substrate, nothing to
GitHub, and it starts nothing. The `GitHub` interface it reads through has no
mutating method at all — the capability is not merely unused, it is absent.

**It does not report stale information as current.** If the published index
cannot be retrieved or read, the command reports that condition and exits
non-zero. There is no local fallback, because a fallback that looks current is
worse than no answer.

The sections are the queue's own: active, awaiting review, awaiting CI,
awaiting the operator, blocked, ambiguous, completed — and **stale or
contradictory evidence**, which is not a disposition but what the verification
found, reported separately so a reader sees both where a task sits and whether
its evidence still holds.

The report's `verified` flag covers the **whole published corpus**, not the
filtered view: a reader who filtered to one project must not be told
"verified" while another project's records are stale. The report says how many
records the publication holds (`record_count`) and how many the filters left
(`shown_count`), so a filtered view can never be mistaken for the whole thing.
An empty publication is not a verified one.

The filters are the project's own dimensions (`--repo`, `--branch`,
`--project`, `--role`, `--disposition`). There is no dashboard, and there is no
new taxonomy.

---

## 8. The publication authority

Publication is a **controlled evidence operation**, not another way for an
agent to gain a privilege.

- The **target** — repository and branch — comes from the operator's
  configuration file (`~/.mpm/evidence.json`). A missing configuration is an
  **error**, never a default: a publisher that invented a target would publish
  the evidence somewhere nobody chose. A **dry run** may be pointed at a target
  explicitly (`--repo`, `--branch`), because a rehearsal that needs a
  configuration file to exist cannot be run anywhere else — and a rehearsal
  writes nothing. A write always uses the configured target.
- The **write** goes through the operator's own `gh`, so the credential never
  enters MPM, never enters a record, and never reaches a contained worker or
  Charlie's isolated reviewer. The read path needs no credential at all.
- The path is **explicit** and has a **dry run**. `mpm engineering publish
  --dry-run` builds the publication and writes nothing, and it is the *same*
  plan — not a second rendering that might differ from the one that ships.
- **No model-facing payload** may select the repository, supply an operator
  approval, or replace independently captured reviewer evidence. There is no
  agent-facing action for publication at all: the operator's shell is the door,
  exactly as PR 3's `execute` and PR 4's `dispatch` are.

The corpus is either the **assignment store** or a named **fixture**. The
fixture exists because Continuity 001's own work predates the store's use of
it: the four pull requests were reviewed and merged before the substrate was
running the work. A fixture is a versioned set of **claims**; the publisher
reads the **observed facts** from GitHub at publication time, so a fixture
cannot state a fact about GitHub even if it wanted to.

### The store corpus is not the fixture corpus

The **store** path publishes an OBSERVED side, and it is not optional: a
record that names no repository, no pull request state, no head and no workflow
run cannot be checked by anybody, so the checks would look in "unknown" and
find nothing and the record would say so forever. So the publisher reads the
facts for every assignment that names a pull request, exactly as it does for a
fixture — the repository comes from the assignment's own pull request
reference, and the observed repository replaces it when GitHub could be asked.

**The NEWEST review of a subject is the one that is published.** Two reviewers
can review one subject: the head advanced and the work was re-reviewed. The
later review says what the subject's state is now, and publishing the earlier
one would publish a superseded verdict as the current one.

The rows arrive newest first, so a publisher that kept the last one it saw
would publish the OLDEST review — the opposite of what the contract needs. So
the rule lives in the package, not in a caller: every candidate review for a
task is compared, and only the newest is used, with the tie broken by the
reviewer's id so the answer does not depend on the order the candidates
arrived in.

**Two accounts of ONE review are merged, not chosen between.** A review can be
described twice: the tree's own correlation reads the reviewer's CLAIM, and
the corpus's candidates read the LEDGER. They are two halves of one review,
not two accounts of it — so they are merged, and the candidate's verdict and
capture status fill in what the claim did not carry. Before this they
competed, the claim won the tie, and the subject's record was published with
NO verdict at all: which reads as "no review happened" and files finished work
under "awaiting review" forever.

---

## 9. The first corpus, and what it does not claim

The first corpus is pull requests #259, #260, #261 and #262, reconstructed from
the GitHub record.

Their reviews happened in contained sessions whose findings were **never
published durably**. So the corpus records each review as a **reported**
verdict — with **its own vocabulary**, `reported_approved` and
`reported_changes_requested`, never the service's `passed` and
`changes_requested` — with **no digest**, and `check` reports the review
evidence as `missing`.

The prefix is not a nicety. A reader that saw `changes_requested` could not
tell a reported one from a recorded one without reading the review disposition
as well — and the whole point of keeping the two apart is that a reader should
not have to.

That is the honest state. The alternative — a digest nobody can check, or an
approval nobody gave — would be a fiction, and a fiction in an evidence
contract is the one failure mode the whole design exists to prevent.

---

## 10. The negative controls

Twelve controls remove an invariant from the implementation and require the
acceptance suite to **fail**. A control that removes nothing measures nothing;
a suite that does not notice is a suite that does not test the invariant.

| control | the invariant it removes |
|---|---|
| stale-head acceptance | a head that advanced invalidates the review of the old one |
| false CI success | a failed job is not a green run |
| forged review approval | a verdict that no reviewer recorded is not a verdict |
| prohibited write order | the pointer is published after the records it names |
| a record's own digest | a record's bytes must hash to the identifier the index gave them |
| the observed side is screened | third-party text from GitHub must not reach the published bytes |
| a finished task with no verdict | work with no verdict is waiting for a reviewer, not for CI |
| the oldest review is published | a superseded verdict must not be published as the current one |
| the newest candidate is not chosen | the newest review of a task is the one that is published |
| a refuted digest is published | a digest the publisher REFUTED must not be published as true |
| the ledger's account of a review is lost | two accounts of ONE review are merged, not chosen between |
| a finished review is filed as waiting for a review | a reviewer's own record is filed by the review, not by the subject's rules |

They run **in Go**, inside the package's own test, because the package is
observable by itself and depends on nothing but the standard library. A control
copies the package into a temporary module, mutates it there, and runs the
suite against the copy. The copy needs no network and no module cache, so the
control runs anywhere the suite runs.

The store path has its own tests, in `cmd/mpm`, because the store is not part
of the package: they run the publication against a **local GitHub server**, so
the whole path is exercised without the network and without a credential.

---

## 11. What PR 5 does not do

It does not merge, deploy or approve anything; it does not publish to the
production repository without the operator's decision; it does not implement
PR 6's Skills P8 assignment; it does not add a GitHub agent, a language-model
runtime, an authentication framework, a web dashboard or a new
project-continuity schema; and it does not replace PR 4's dispatcher. The
dispatcher, the budgets, the fencing, the disabled-by-default execution and PR
2's negative controls are untouched.
