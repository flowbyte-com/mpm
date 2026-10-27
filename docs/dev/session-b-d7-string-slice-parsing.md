# Session B — D7: the parse boundary must not lose a valid string list

**Branch:** `feat/session-b-d7-string-slice-parsing` (from `origin/main` = `4de89ea`)
**Status:** D7 **CLOSED** by a narrow, backward-compatible correction — a new native-slice
shape is accepted, and a citation list is now parsed **as a whole** or not at all.
**Scope:** the string-list parse boundary and the three citation call sites. No schema
change, no migration, no registry change, no new tool, no public tool-contract change.
D1, D3, D4, D6, the P2 evidence-identity contract and the P2b scheduler gate are untouched;
interactions are reported in §10.

---

## 1. Summary

`internal.ParseStringSliceOr` is the shared helper every tool handler uses to read a
list-of-strings parameter out of the weakly-typed payload map. It accepted two shapes:

| Shape | Pre-D7 result |
|---|---|
| `[]interface{}` of strings (the JSON-decoded array) | the list, in full |
| a comma-separated `string` | the list, split |
| **`[]string` (the Go-native array)** | **`nil` — the whole list, gone** |
| an array with a non-string element | the list **minus that element** |
| any other scalar (`42`, `true`, a map) | `nil` — read as "no value at all" |

The third row is the measured defect. A Go-native `[]string` — a perfectly valid
representation of the parameter, and the natural one for an in-process Go caller — fell
through every branch and hit the `return nil` at the bottom. **No error, no warning, no
trace**: the caller was told the operation succeeded.

The consequence was a **data-integrity** failure, not a convenience bug. A theory
proposal whose `source_ids` arrived in that shape was created with **no citations at
all**, while its author — who had declared the citations and was told "proposal
created" — reasonably believed they were recorded. The artifact and the record of what
it rests on disagreed, and nothing in the system flagged the disagreement.

The fourth and fifth rows are the same defect wearing a different hat. An element of the
wrong type was **dropped**, and a wholly malformed value was read as "the caller supplied
nothing". Both make a list *shorter* than what the caller supplied, and both do it
silently. For a **citation** list that is the difference between a proposal resting on
three artifacts and a proposal resting on two — an unsupported proposal, created
successfully.

**The correction has two levels, because the two problems are different problems.**

* **The bridge** — the lenient parser now accepts a Go-native `[]string` and copies it.
  The native form is a *complete* representation of the parameter, exactly like the
  JSON-decoded array, so the two now agree element for element. Every caller of the
  lenient helper benefits at once, with **no change to its fallback semantics**.
* **The contract** — a citation list is now parsed as a **whole**. The three citation
  sites use a new, explicitly-designed stricter API that accepts every shape the lenient
  form accepts (so no working caller loses a shape) and returns an **error** in exactly
  the cases where the lenient form would silently lose an element.

The strictness is applied **only to citation lists** — the fields whose partial
acceptance would produce an unsupported artifact. `tags`, `components` and `artifacts`
keep their documented behaviour; the residual risk in those two is reported in §11, not
silently changed.

---

## 2. Root cause

### 2.1 The parse boundary

`internal/core/parse.go` is the boundary between the weakly-typed payload map and
strongly-typed Go values. Every tool handler — CLI, MCP, and every in-process caller —
delivers `map[string]interface{}`, and the helpers bridge it:

```
cmd/mpm/call.go:477          json.Unmarshal(payload)      → map[string]interface{}
cmd/mpm-mcp/tools.go:573     req.GetArguments()           → map[string]interface{}
   ↓
internal/core/tools/handlers.go
   ↓
internal.ParseStringSliceOr(p["source_ids"])   ← the defect lived here
```

Pre-D7, the whole of the helper was:

```go
func parseStringSliceAny(v interface{}) []string {
	if v == nil {
		return nil
	}
	if arr, ok := v.([]interface{}); ok {
		out := make([]string, 0, len(arr))
		for _, x := range arr {
			if s, ok := x.(string); ok {
				out = append(out, s)      // ← a non-string element is DROPPED
			}
		}
		return out
	}
	if s, ok := v.(string); ok && s != "" {
		…                                          // JSON array string, else CSV
	}
	return nil                                     // ← []string lands HERE
}
```

Two type switches, two accepted shapes, and one silent exit for everything else.

### 2.2 The defect was already known to the codebase

The helper was not the only place that had to think about this shape, and the repo
already contained the correct answer in two other places:

* **`cmd/mpm-mcp/tools.go:854` — `parseStringSliceArg`** already accepted `[]string`
  explicitly (`case []string: return t`). The MCP-side helper understood the native
  shape; the **core** helper — the one every handler actually calls — did not. The core
  helper was the outlier.
* **`internal/core/tools/handlers.go:3484` — `collectConfirmationSpecs`** already
  accepted `[]string`, `[]interface{}` and a bare string, with the same three-way
  switch.

And the audit comments left by earlier remediation passes had already named the class as
a defect:

* **`handlers.go:1239` (defect C.13, P1)** — on `mpm_decisions.record`'s `tags`:
  *"the previous shape routed every input through internal.ParseStringSliceOr, which
  silently returned nil for non-array scalars and silently dropped non-string elements
  from arrays. The caller believed the record succeeded with their tags — the persisted
  decision had tags=[] instead. … caller intent … was lost without any error."*
* **`handlers.go:1054` (defect C.14, P1)** — the same finding on `mpm_theories.propose`'s
  `dependencies`.

C.13 and C.14 fixed two **fields** by writing a new, strict, local helper. They did not
fix the **helper**, so every other field — and every other caller — kept the defect.
D7 is the C.13/C.14 fix generalised: one canonical implementation, in the right place.

### 2.3 Why the silent drop was possible at all

The lenient form was written for the *tags* use case: a metadata list where dropping an
unparseable element is harmless and a fallback is friendlier than an error. That is a
legitimate contract. The defect is that the **same helper** was used for **citation
lists**, where a dropped element is not a lost tag — it is a lost *basis*.

The codebase already encoded that distinction, informally, in the two helpers: the
lenient one for tags, the strict one (`parseStrictStringArrayOrEmpty`) for declared
array fields. What was missing was the middle case — the **citation** field, which
needs the leniency of the first (a client may send a CSV string) and the wholeness of
the second.

---

## 3. The audit: every call site

Every production call site of the shared parsing helpers in
`internal/core/tools/handlers.go`, with its semantics. Post-change line numbers.

### 3.1 The lenient helper — `internal.ParseStringSliceOr` (10 sites)

| Line | Tool / action | Field | Meaning of the list | Lenient still correct? |
|---|---|---|---|---|
| 179 | `mpm_memory.save` | `tags` | metadata | **yes** — tags are metadata |
| 298 | `mpm_commit_milestone` | `tags` | metadata (`type:milestone-*`) | **yes** |
| 1050 | `mpm_theories.propose` | `tags` | metadata | **yes** — note the asymmetry, §11.2 |
| 1334 | `mpm_decisions.supersede` | `tags` | metadata | **yes** |
| 1938 | `mpm_lessons.save` | `tags` | metadata | **yes** |
| 2356 | `mpm_references.add` | `tags` | metadata | **yes** |
| 3473 | `mpm_log_to_changelog` | `tags` | metadata | **yes** |
| 3476 | `mpm_log_to_changelog` | `tags` | metadata | **yes** |
| 6007 | `mpm_request_review` | `components` | **required** — the review's subject components | **partly** — an empty list already errors, but a *partial* list is accepted (§11.1) |
| 6029 | `mpm_request_review` | `artifacts` | the artifacts under review | **partly** — a partial list is accepted (§11.1) |

### 3.2 The strict helper — `parseStrictStringArrayOrEmpty` (4 sites)

| Line | Tool / action | Field | Contract |
|---|---|---|---|
| 1064 | `mpm_theories.propose` | `dependencies` | array of strings, optional; a scalar is an error |
| 1261 | `mpm_decisions.record` | `tags` | array of strings, optional; a scalar is an error |
| 4272 | `mpm_handoff.write` | `commitments` | array of strings, optional; a scalar is an error |
| 4276 | `mpm_handoff.write` | `open_questions` | array of strings, optional; a scalar is an error |

All four keep their **exact** error strings and their **exact** semantics. They gain one
shape: the Go-native `[]string`, which they used to reject (§4.2).

### 3.3 The citation sites — the ones the brief is about (3 sites)

| Line | Tool / action | Field | Pre-D7 parse |
|---|---|---|---|
| 1068 → **1081** | `mpm_theories.propose` | `source_ids` | lenient — a native slice vanished; a mixed array lost elements; a scalar read as "none" |
| 1252 → **1268** | `mpm_decisions.record` | `source_ids` | lenient — the same three failures |
| 1328 → **1342** | `mpm_decisions.supersede` | `source_ids` | lenient — the same three failures |

These are the only three places in the substrate where a list of **artifact identifiers**
is read from a payload, and the only three places where a partial parse produces an
**unsupported artifact**. They are the ones the correction makes strict.

### 3.4 Do any callers depend on the current fallback behaviour?

Yes — and they are all in §3.1, none in §3.3.

* `mpm_memory.save`, `mpm_lessons.save`, `mpm_references.add`, `mpm_log_to_changelog`,
  `mpm_theories.propose` (tags), `mpm_decisions.supersede` (tags),
  `mpm_commit_milestone` (tags) — a malformed tag list silently becomes a shorter tag
  list. Making these strict would be a **breaking change to a shared public tool**:
  clients that today send `tags: "a,b"` or `tags: [1,2]` would start receiving an
  error where they used to receive a success. The brief's boundary — *"do not
  introduce breaking semantics into every caller"* — forbids it, and it would be the
  wrong call on the merits: a dropped tag is a cosmetic loss, not an unsupported
  claim.
* `mpm_request_review`'s `components` already errors on an **empty** list
  (`"'components' is required (at least one substrate component name…)"`), so its
  caller-visible behaviour is already strict in the important direction.
* `mpm_decisions.record`'s `tags` and `mpm_theories.propose`'s `dependencies` already
  error on a scalar — C.13/C.14's contract, preserved verbatim.

**No caller depends on the lenient behaviour of a *citation* list.** A caller that
supplied a malformed citation list today got a *successful* artifact with fewer
citations than it declared — the exact outcome the brief calls a data-integrity
problem. Changing that is a correctness fix, not a contract break.

---

## 4. What changed

### 4.1 The bridge — the lenient parser accepts `[]string`

`internal/core/parse.go:76`:

```go
if arr, ok := v.([]string); ok {
	if arr == nil {
		return nil
	}
	out := make([]string, len(arr))
	copy(out, arr)
	return out
}
```

Three deliberate properties:

* **Copied, not aliased.** The caller keeps ownership of the slice it handed over; a
  later mutation of the caller's slice must not change what was parsed.
* **nil stays nil.** An absent parameter and an explicitly nil slice mean the same
  thing ("no list"), so the native form agrees with the JSON form's `nil`.
* **An empty slice stays an empty slice.** `[]string{}` is an empty list, exactly like
  `[]interface{}{}` — *not* nil. The two representations agree element for element.

Every lenient caller benefits at once. **No fallback semantics change**: a non-string
element in a `[]interface{}` is still dropped, a non-list scalar is still read as "no
value", a CSV string is still split.

### 4.2 The canonical home of the array-only contract

`internal/core/parse.go:183` — `ParseStringSliceArrayOnly(field, v)` is now the single
implementation of the array-only strict contract, and the tools-module helper
`parseStrictStringArrayOrEmpty` (`handlers.go:1305`) **delegates** to it:

```go
func parseStrictStringArrayOrEmpty(field string, v interface{}) ([]string, error) {
	return internal.ParseStringSliceArrayOnly(field, v)
}
```

The error strings are **byte-identical** to the ones the boundary already produced:

```
tags must be an array of strings, got string
tags[1] must be a string, got int
```

The one gain is the `[]string` shape, which the old helper **rejected**:

```
before:  tags must be an array of strings, got []string
after:   accepted
```

That rejection was the in-process half of the same defect — a loud failure, but a
failure, on a representation that is perfectly valid Go and that the *other* helper in
the same file (`collectConfirmationSpecs`) already accepted.

### 4.3 The contract — a citation list is parsed as a whole

`internal/core/parse.go:136` — `ParseStringSliceStrict(field, v)`:

```go
switch t := v.(type) {
case nil:
	return []string{}, nil
case []string:                      // native slice → copied, whole
case []interface{}:                 // JSON array → element-wise, ERROR on a non-string
case string:                        // JSON-encoded array, or CSV → whole parse
}
return nil, fmt.Errorf("%s must be a string array (or a comma-separated string), got %T", field, v)
```

The design rule is one sentence: **it accepts every shape the lenient form accepts, and
errors in exactly the cases where the lenient form would silently lose an element.**

* A **CSV string** and a **JSON-encoded array string** are accepted, because neither
  loses an element. (The MCP server does **not** validate arguments against the schema
  it publishes — there is no validation path in `cmd/mpm-mcp` — so a CSV string really
  does reach the handler. Rejecting it would break a working caller.)
* A **malformed element** is an error, not a drop.
* A **non-list scalar** is an error, not "no value".

Used at the three citation sites — `handlers.go:1081` (`mpm_theories.propose`),
`:1268` (`mpm_decisions.record`), `:1342` (`mpm_decisions.supersede`).

---

## 5. Behaviour changes

Only two kinds of change reach a caller, and both are narrow.

### 5.1 A citation list that cannot be represented exactly is now an error

| Input to `source_ids` | Pre-D7 | Post-D7 |
|---|---|---|
| `["mem-1","mem-2"]` (JSON) | cited both | cited both — **unchanged** |
| `[]string{"mem-1","mem-2"}` (native) | **cited neither; proposal created** | cited both |
| `["mem-1", 42]` | cited `mem-1` only; **proposal created** | **error; no proposal** |
| `42` / `true` / a map | cited none; **proposal created** | **error; no proposal** |
| `"mem-1,mem-2"` (CSV) | cited both | cited both — **unchanged** |
| `"[\"mem-1\",\"mem-2\"]"` (JSON string) | cited both | cited both — **unchanged** |
| absent / `null` | no citations | no citations — **unchanged** |
| `[]` | no citations | no citations — **unchanged** |

The new error messages:

```
source_ids[1] must be a string, got int
source_ids must be a string array (or a comma-separated string), got int
```

### 5.2 A declared array field now accepts the native shape

`tags`, `dependencies`, `commitments` and `open_questions` — the four strict sites —
now accept `[]string{"a","b"}` instead of rejecting it with
`"<field> must be an array of strings, got []string"`. Every other shape, and every
error message, is unchanged.

### 5.3 What did **not** change

* The lenient helper's fallback semantics, for every remaining caller (§3.1).
* The four strict sites' error strings and their scalar rejection (§4.2).
* The D3 legacy citation policy — a legacy (untyped) citation is still **never**
  rejected; the typed path is where resolution is validated, and that path is untouched.
* The P2 evidence-identity contract — no identity mechanism was touched.
* D4 proposal identity — untouched.
* The registry, the schemas, the tool list — untouched. `source_ids` was already
  declared `{"type":"array","items":{"type":"string"}}`; the correction makes the
  implementation match the **declared** contract, it does not change the contract.

---

## 6. Regression coverage

Two new files, 16 tests, no database required for the parser-level suite.

### 6.1 `internal/core/d7_string_slice_parsing_test.go` (9 tests)

| Test | Requirement |
|---|---|
| `TestD7_ParseStringSliceOr_NativeStringSlice` | **1** — native `[]string`; also proves the result does not alias the caller's slice |
| `TestD7_ParseStringSliceOr_JSONDecodedArrays` | **2** — JSON array, JSON-encoded array string, CSV |
| `TestD7_ParseStringSliceOr_EmptyArraysAndNil` | **3** — `nil`, `[]string(nil)`, `""`, `[]string{}`, `[]interface{}{}` |
| `TestD7_ParseStringSliceOr_MissingOptionalParameter` | **4** — a missing key is `nil` |
| `TestD7_ParseStringSliceOr_MalformedAndMixedType` | **5** — the lenient form's documented fallback: a mixed array drops the malformed element; a scalar is "no list" |
| `TestD7_ParseStringSliceStrict_WholeList` | **6** — the strict form's fallback semantics: every lenient shape accepted, whole |
| `TestD7_ParseStringSliceStrict_RejectsLossyInput` | **5/6** — a malformed element and a non-list value are errors, with the exact messages |
| `TestD7_ParseStringSliceArrayOnly_SchemaContract` | **10** — the array-only contract, with the **original** error strings |
| `TestD7_CitationListIsAcceptedWholeOrNotAtAll` | **7** — three identifiers in, three identifiers out, in both representations |

### 6.2 `internal/core/tools/d7_string_slice_tools_test.go` (7 tests)

Disposable file-backed databases in an isolated `t.TempDir()` workspace root
(`HOME`, `MPM_WORKSPACE` and `MPM_DB_PATH` all redirected). No live workspace is touched.

| Test | Requirement |
|---|---|
| `TestD7_NativeStringSliceSourceIDsProduceTypedResolvableCitations` | **the end-to-end proof** — a native `[]string` of two memory ids, with `source_type: "memory"`, produces exactly two `epistemic_provenance` rows, each naming the declared type, each pointing at a **live** row of that type |
| `TestD7_ProposalCarriesEverySuppliedSourceID` | **7** — the same, on the legacy (untyped) path |
| `TestD7_PartiallyAcceptedCitationListCreatesNoProposal` | **8** — a mixed array, a scalar and a map are each rejected, and the theory-row count is **unchanged** after each: a rejected citation list leaves no proposal behind |
| `TestD7_CLIMCPAndInProcessParity` | **9** — the JSON-decoded path and the native in-process path produce equivalent proposals citing the same artifact |
| `TestD7_DecisionRecordKeepsEverySuppliedSourceID` | **7** — the decision side of the same guarantee |
| `TestD7_ExistingCallersRetainTheirDocumentedBehaviour` | **10** — `mpm_decisions.record`'s scalar-`tags` error and `mpm_theories.propose`'s scalar-`dependencies` error keep their **exact** messages; a valid JSON array still works; the native shape is now accepted by the same contract |
| `TestD7_CitationSitesStillAcceptTheCompactStringForms` | **10** — the comma-separated string and the JSON-encoded array string still work at a citation site, and cite **exactly** the two supplied artifacts: a working caller that sends either form is not broken by the correction |
| `TestD7_NativeTagsAndSourceIDsAreAcceptedEverywhere` | the lenient path's parity — native tags are persisted, not dropped |

### 6.3 The end-to-end proof, in detail

`TestD7_NativeStringSliceSourceIDsProduceTypedResolvableCitations` is the test the brief
names. It:

1. seeds two live memories in a disposable database;
2. proposes a theory with `source_ids` as a **Go-native `[]string`** and
   `source_type: "memory"`;
3. asserts the proposal **exists** (one theory row);
4. asserts the `epistemic_provenance` rows for that theory are **exactly** the two
   memories that were supplied — no more, no fewer;
5. asserts each citation row carries `source_type = 'memory'` and that the cited id
   resolves to a live, undeleted row of that type in `memories`.

Step 5 is what makes the proof *typed and resolvable* rather than merely *present*.

---

## 7. Non-vacuity — proven, not asserted

The suite was run against a **reinstated** defect in an isolated git worktree
(`/tmp/opencode/d7-nonvacuity`, detached at the D7 head, removed afterwards). Three
probes, each reverting exactly one part of the correction.

**Probe A — remove the `[]string` case from the lenient parser.**
The native form falls through to `return nil` again.

```
--- FAIL: TestD7_ParseStringSliceOr_NativeStringSlice
    ParseStringSliceOr([]string) = []string(nil), want [mem-1 mem-2]
--- FAIL: TestD7_ParseStringSliceOr_EmptyArraysAndNil
    ParseStringSliceOr([]string{}) = nil, want an empty (non-nil) list
--- FAIL: TestD7_NativeTagsAndSourceIDsAreAcceptedEverywhere
    theory tags = "[]", want it to contain alpha and beta
```

**Probe B — revert the three citation sites to the lenient parser.**
The unsupported proposal is reproduced exactly:

```
--- FAIL: TestD7_PartiallyAcceptedCitationListCreatesNoProposal
    mixed-type array: the malformed citation list was accepted — a partially accepted list must be rejected
    mixed-type array: theory rows = 1, want 0 — a rejected citation list must leave no proposal behind
    non-list value:  theory rows = 1, want 0
    map:             theory rows = 1, want 0
```

`theory rows = 1, want 0` is the defect itself: **the unsupported proposal exists**.

**Probe C — make the strict parser drop a malformed element instead of rejecting it.**
The same test fails, on the same count — a partial parse still produces the artifact.

**Probe D — make the strict parser reject the compact string forms.**
The test that pins the compact forms at a citation site fails, and the error is the
one a broken caller would see:

```
--- FAIL: TestD7_CitationSitesStillAcceptTheCompactStringForms
    comma-separated string: propose: source_ids must be a string array (or a comma-separated string), got string
```

Probes A–C were run in the isolated worktree, which was restored with
`git checkout -- .`, re-run green, and removed. Probe D was run in the main checkout
against a byte-copy of `parse.go` that was restored immediately afterwards; `git diff`
confirmed the only remaining change was the new test file. The working tree was never
left modified by a probe.

---

## 8. Gates

| Gate | Result |
|---|---|
| `internal/core` (`go test -tags fts5 ./...`) | **ok** — 211.1 s |
| `internal/core/tools` | **ok** — 42.9 s |
| root module (all 17 packages) | **ok** — incl. `cmd/mpm` 405.1 s, `cmd/mpm-mcp` 8.1 s, `internal/critic` 5.4 s |
| `go vet ./...` | clean |
| `gofmt -l` on the four touched files | clean |
| `mpm-lint --gate` | **PASS** (exit 0) — import-forbidden actual=0 threshold=0 |

One incidental repair: `internal/core/parse.go` was **not gofmt-clean at baseline**
(`origin/main`) — the file ended without a trailing newline. The D7 edit fixed it
(one byte). No other file was reformatted.

---

## 9. CI

The three CI jobs on the PR head, at the final commit:

<!-- CI_RESULTS -->

---

## 10. Interactions with the other M5 defects

* **D1 / D5 (evidence identity, PR #130)** — untouched. D1's mechanism identifies an
  *observation*; D7 fixes a *parameter parse*. No shared code path.
* **D3 (legacy citation policy)** — untouched, and deliberately so. A legacy
  (untyped) citation is still never rejected: D7's strictness is at the **parse**, not
  at the resolution. A legacy citation that does not resolve still takes the historical
  path; a legacy citation list that cannot be *parsed* is now an error, which is a
  different question from whether the citation *resolves*.
* **D4 (proposal identity)** — untouched. The proposal's id derivation is unchanged.
* **D6 (invocation provenance, PR #132)** — untouched and on its own branch
  (`feat/session-b-d6-invocation-provenance`, head `a36bbae`). D7 was branched from
  `origin/main` = `4de89ea`, so the two PRs do not overlap. D6 changed the *provenance*
  columns of the rows D7 writes to; D6 does not change how `source_ids` is parsed.
* **P2b (scheduler gate)** — untouched.
* **The C.13 / C.14 audit findings** — D7 is their generalisation. The two fields they
  fixed keep their exact behaviour; the helper they worked around now has one
  canonical implementation with the same error strings.

---

## 11. Residual limitations — reported, not repaired

D7 is a focused defect fix. The following were found during the audit and are **not**
changed here. Each is a real limitation of the current state, stated plainly.

### 11.1 `mpm_request_review` still accepts a partial `components` / `artifacts` list

`handlers.go:6007` and `:6029` route through the lenient helper. `components` errors on
an **empty** list, but a *partially valid* list (one non-string element) is accepted with
that element dropped. `artifacts` behaves the same way, and an empty `artifacts` list
is accepted as "no artifacts".

Why not changed: `mpm_request_review` is a **shared public tool**, and the brief forbids
introducing breaking semantics into every caller. Making these strict would turn
today's successes into errors for any client that sends a mildly malformed list.
`components`/`artifacts` are a *routing and review-scope* list, not a claim about what
the reviewed artifact rests on — the partial-acceptance risk is materially lower than
it is for a citation list. The right shape for a follow-up is a dedicated strict
contract, decided on the merits by whoever owns that tool — not a silent change here.

### 11.2 `mpm_theories.propose` treats `tags` leniently and `dependencies` strictly

Within one handler, `dependencies` rejects a scalar (C.14) while `tags` accepts one.
That asymmetry predates D7 and is unchanged by it. It is defensible — `dependencies`
declares *forward edges to other theories* and a silently empty dependency list makes a
theory look standalone to the wake-on-delete scan, whereas a tag is metadata — but it is
an inconsistency a reader will notice, and it is recorded here rather than papered over.

### 11.3 The lenient helper still drops a malformed element, by design

`ParseStringSliceOr` keeps its fallback semantics for the ten lenient callers (§3.1).
That is a deliberate contract, not an oversight: it is what makes the correction
backward-compatible. The stricter forms are opt-in per field.

### 11.4 The `[]string` shape is accepted, but nothing yet *produces* it

At the time of writing, no production path constructs a tool payload with a Go-native
`[]string` value — the CLI and the MCP server both deliver JSON-decoded arrays. The
native shape is therefore a **latent** exposure today (an in-process caller, or a
client that builds its payload in Go) rather than an observed one; the **observed**
silent-loss path is the mixed-type array and the non-list scalar, which D7 closes at the
citation sites.

This is worth stating plainly: the defect the brief measured — a Go-native `[]string`
returning `nil` — is real and is now fixed, but the *live* production impact is
concentrated in the two neighbouring shapes. Both are now covered.

### 11.5 `cmd/mpm-mcp/tools.go:854` — `parseStringSliceArg` is dead code

The MCP-side helper that already accepted `[]string` is **defined and never called**
(`grep` finds no call site). It is the historical evidence that the native shape was
understood on the MCP side, and it is left in place — removing it is an unrelated
cleanup, and D7's boundary says no unrelated repairs.

---

## 12. What was NOT done

* No schema change. No migration. No registry change. No new tool.
* No change to the P2 evidence-identity contract.
* No broad rewrite of the shared parsing call sites — 10 of 13 lenient sites are
  **untouched**, and the 4 strict sites keep their exact semantics.
* No change to the D3 legacy citation policy or to D4 proposal identity.
* No change to the P2b scheduler gate.
* No live-workspace or installed-runtime modification. Every test used a disposable
  file-backed database in an isolated `t.TempDir()` root.
* No merge and no deployment.
* The D6 branch was **not** modified and **not** rebased — `feat/session-b-d6-invocation-provenance`
  remains at `a36bbae`, PR #132 open and `MERGEABLE`.
* No unrelated repairs, including the dead `parseStringSliceArg` helper (§11.5).

---

## 13. Commit and PR

| | |
|---|---|
| Branch | `feat/session-b-d7-string-slice-parsing` |
| Base | `origin/main` = `4de89ea` |
| Commits | `f2ab066` — *fix(core): a Go-native []string must not vanish at the parse boundary (D7)*<br>`3ce3953` — *docs(dev): the D7 report*<br>`c39edac` — *test(core): the compact citation forms must survive the strict parse too* |
| PR | [#135](https://github.com/flowbyte-com/mpm/pull/135) |

Files changed:

| File | Change |
|---|---|
| `internal/core/parse.go` | `parseStringSliceAny` accepts `[]string`; new `ParseStringSliceStrict`; new `ParseStringSliceArrayOnly`; `fmt` import |
| `internal/core/tools/handlers.go` | the three citation sites use `ParseStringSliceStrict`; `parseStrictStringArrayOrEmpty` delegates to `ParseStringSliceArrayOnly` |
| `internal/core/d7_string_slice_parsing_test.go` | **new** — 9 parser-level tests |
| `internal/core/tools/d7_string_slice_tools_test.go` | **new** — 7 tool-level tests, including the end-to-end proof |
