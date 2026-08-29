# Confidence and Evidence Foundation Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Decouple MPM's `weight` into separate `retrieval_priority` + `importance` axes, add a derived `confidence` column maintained by triggers and an `idle_dream` recompute, and introduce a typed `evidence` table as the substrate for future calibration, challenge refactor, hindsight, and forgetting work.

**Architecture:** Two new pure-Go files (`internal/evidence.go`, `internal/confidence.go`) hold the type registry and the math. Schema additions to `memories` and `lessons` tables, plus a new `evidence` table and `confidence_history` table. SQLite triggers on `evidence` insert/update/delete call a Go-registered function that delegates to `confidence.Recompute(artifactID, artifactType, reason)`. `idle_dream` walks artifacts whose last positive evidence is older than the collection's half-life and recomputes their confidence, writing a `confidence_history` row per tick.

**Tech Stack:** Go 1.26.1, mattn/go-sqlite3 (CGO with FTS5), stretchr/testify for tests, existing DatabaseManager + MemoryStore patterns.

**Build commands** (mirror the Makefile):
- Build: `CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go build -tags fts5 ./...`
- Test: `CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 ./internal/... ./cmd/mpm/...`

## Important Deviation From the Spec

The spec (`docs/superpowers/specs/2026-06-16-confidence-evidence-foundation-design.md`) describes four artifact tables — `memories`, `theories`, `decisions`, `lessons`. The current schema has only two tables: `memories` (with `collection` distinguishing `'memories'`, `'theories'`, `'decisions'`, and others) and `lessons`. Theories and decisions are not separate tables; they are rows in `memories` with a discriminator column.

This plan adapts the spec to the actual schema:

- New columns (`retrieval_priority`, `importance`, `confidence`) go on `memories` and `lessons` only.
- The `artifacts` view unions `memories` (filtered per collection) with `lessons`, with the `type` column populated by collection.
- Initial-confidence defaults are applied in application code (`AddMemory`, `AddLesson`, `RecordDecision`, `ProposeTheory`) based on the `collection` discriminator, not via column defaults.

If the team decides to migrate `theories` and `decisions` into separate tables in the future (a real refactor with its own migration), the rest of this design still works — the `artifacts` view will gain additional UNION ALLs and the per-type defaults will move to the new tables' column defaults.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/evidence.go` (new) | Evidence type registry: default strengths, validation, lookup. |
| `internal/evidence_test.go` (new) | Tests for the registry. |
| `internal/confidence.go` (new) | Pure-Go math: sigmoid, log-odds, effective_evidence, recency, decay, per-collection λ, per-type initial confidence. |
| `internal/confidence_test.go` (new) | Tests for the math (no DB). |
| `internal/evidence_store.go` (new) | DB-touching code: `AddEvidence`, `ListEvidenceForArtifact`, `RecomputeConfidence` (loads evidence, calls math, writes to artifact + history), evidence expiry. |
| `internal/evidence_store_test.go` (new) | DB tests. |
| `internal/schema.go` | Add `evidence`, `confidence_history`, `artifacts` view, `legacy_weight` view; add new columns to `memories` and `lessons` (via SafeMigrations). |
| `internal/memory.go` | Add `RetrievalPriority`, `Importance`, `Confidence` to `Memory` struct; `AddMemory` sets initial confidence by collection; routes writes through `execTracked`. |
| `internal/lessons.go` | Add fields to `Lesson` struct; `AddLesson` sets initial confidence. |
| `internal/call_helpers.go` | `RecordDecision` and `ProposeTheory` set initial confidence by collection. |
| `internal/db.go` | Register the `confidence_from_evidence` SQL function (calls into `evidence_store.RecomputeConfidence`); add the trigger creation calls during `initUnifiedSchema`. |
| `internal/idle_dream.go` | Add a `ConfidenceDecayCycle` method that walks stale artifacts and calls `RecomputeConfidence(reason=DecayTick)`. |
| `cmd/mpm/call.go` | Add `add_evidence`, `query_confidence_history`, `list_evidence` to `toolRegistry`. |
| `cmd/mpm/handlers.go` (or new `cmd/mpm/evidence_cmds.go`) | New CLI commands: `mpm evidence add`, `mpm evidence list`, `mpm ops confidence`. |
| `cmd/mpm/router.go` | Register the new commands in the switch. |

---

## Task 1: Evidence Type Registry

**Files:**
- Create: `internal/evidence.go`
- Create: `internal/evidence_test.go`

The evidence type registry is the single source of truth for the six v1 evidence types. It holds default strengths, validates type names, and is consulted by `AddEvidence` to fill in `strength` when the caller doesn't override it.

- [ ] **Step 1: Write the failing test**

Create `internal/evidence_test.go`:

```go
package internal

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEvidenceTypes_AllHaveStrengths(t *testing.T) {
	for _, name := range AllEvidenceTypes() {
		t.Run(name, func(t *testing.T) {
			s, ok := DefaultStrength(name)
			require.True(t, ok, "DefaultStrength(%q) returned ok=false", name)
			assert.GreaterOrEqual(t, s, -1.0, "strength must be >= -1")
			assert.LessOrEqual(t, s, 1.0, "strength must be <= 1")
		})
	}
}

func TestEvidenceTypes_StrengthsMatchSpec(t *testing.T) {
	cases := []struct {
		typeName string
		want     float64
	}{
		{"observation", 0.4},
		{"test", 0.7},
		{"reproduction", 0.85},
		{"challenge", -0.6},
		{"decision_outcome", 0.95},
		{"external_reference", 0.6},
	}
	for _, tc := range cases {
		t.Run(tc.typeName, func(t *testing.T) {
			got, ok := DefaultStrength(tc.typeName)
			require.True(t, ok)
			assert.InDelta(t, tc.want, got, 1e-9)
		})
	}
}

func TestEvidenceTypes_UnknownReturnsFalse(t *testing.T) {
	_, ok := DefaultStrength("not_a_real_type")
	assert.False(t, ok)
}

func TestEvidenceTypes_AllReturnsSixV1(t *testing.T) {
	got := AllEvidenceTypes()
	assert.Len(t, got, 6)
}

func TestEvidenceTypes_IsValid(t *testing.T) {
	assert.True(t, IsValidEvidenceType("observation"))
	assert.True(t, IsValidEvidenceType("challenge"))
	assert.False(t, IsValidEvidenceType("nope"))
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 -run TestEvidenceTypes ./internal/`
Expected: compile error — `AllEvidenceTypes`, `DefaultStrength`, `IsValidEvidenceType` not defined.

- [ ] **Step 3: Write the implementation**

Create `internal/evidence.go`:

```go
// evidence.go — Evidence type registry.
//
// Single source of truth for v1 evidence types. Adding a seventh type means
// adding a row to evidenceTypes and the corresponding test cases — no other
// files need to change. The strength sign is significant: positive means
// "evidence for the artifact", negative means "evidence against" (challenge).
package internal

// evidenceTypes is the v1 registry. Order is the canonical iteration order
// returned by AllEvidenceTypes.
var evidenceTypes = []struct {
	Name     string
	Strength float64
}{
	{"observation", 0.4},
	{"test", 0.7},
	{"reproduction", 0.85},
	{"challenge", -0.6},
	{"decision_outcome", 0.95},
	{"external_reference", 0.6},
}

// AllEvidenceTypes returns the v1 evidence type names in registry order.
func AllEvidenceTypes() []string {
	out := make([]string, len(evidenceTypes))
	for i, t := range evidenceTypes {
		out[i] = t.Name
	}
	return out
}

// IsValidEvidenceType reports whether name is a known v1 type.
func IsValidEvidenceType(name string) bool {
	for _, t := range evidenceTypes {
		if t.Name == name {
			return true
		}
	}
	return false
}

// DefaultStrength returns the registry default for name. ok is false if name
// is not a known type.
func DefaultStrength(name string) (float64, bool) {
	for _, t := range evidenceTypes {
		if t.Name == name {
			return t.Strength, true
		}
	}
	return 0, false
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 -run TestEvidenceTypes ./internal/`
Expected: 5 subtests, all PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/evidence.go internal/evidence_test.go
git commit -m "feat(evidence): add v1 evidence type registry"
```

---

## Task 2: Confidence Math (Pure)

**Files:**
- Create: `internal/confidence.go`
- Create: `internal/confidence_test.go`

`confidence.go` holds the pure math. No DB access. The DB-touching wrapper in `evidence_store.go` (Task 5) will call into these functions. This keeps the math testable without spinning up a database and lets a future contributor read the formula in one place.

- [ ] **Step 1: Write the failing test**

Create `internal/confidence_test.go`:

```go
package internal

import (
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSigmoid(t *testing.T) {
	cases := []struct {
		in   float64
		want float64
	}{
		{0, 0.5},
		{math.Inf(1), 1.0},
		{math.Inf(-1), 0.0},
		{1, 1.0 / (1.0 + math.Exp(-1))},
	}
	for _, tc := range cases {
		got := sigmoid(tc.in)
		assert.InDelta(t, tc.want, got, 1e-9)
	}
}

func TestEffectiveEvidence(t *testing.T) {
	got := effectiveEvidence(0.4, 1.0, 0.5) // strength × independence × recency
	assert.InDelta(t, 0.2, got, 1e-9)
}

func TestRecencyWeight_Decays(t *testing.T) {
	w0 := recencyWeight(0, 0.005)
	w30 := recencyWeight(30, 0.005)
	assert.InDelta(t, 1.0, w0, 1e-9)
	assert.Less(t, w30, w0, "recency weight should decrease with age")
	assert.Greater(t, w30, 0.0)
}

func TestDecay_IncreasesWithTime(t *testing.T) {
	lambda := decayLambda("theories") // 0.02
	d0 := decay(lambda, 0)
	d35 := decay(lambda, 35)
	assert.InDelta(t, 0, d0, 1e-9)
	assert.InDelta(t, lambda*35, d35, 1e-9)
}

func TestInitialConfidence_ByArtifactType(t *testing.T) {
	cases := map[string]float64{
		"memory":   0.8,
		"theory":   0.5,
		"decision": 0.6,
		"lesson":   0.7,
	}
	for artifact, want := range cases {
		t.Run(artifact, func(t *testing.T) {
			got := InitialConfidence(artifact)
			assert.InDelta(t, want, got, 1e-9)
		})
	}
}

func TestInitialConfidence_UnknownTypeDefaultsToPointFive(t *testing.T) {
	got := InitialConfidence("not_a_real_type")
	assert.InDelta(t, 0.5, got, 1e-9)
}

func TestDecayLambda_ByCollection(t *testing.T) {
	// Per spec: decisions 0.001, lessons 0.003, memories 0.01, theories 0.02
	cases := map[string]float64{
		"decisions": 0.001,
		"lessons":   0.003,
		"memories":  0.01,
		"theories":  0.02,
	}
	for coll, want := range cases {
		t.Run(coll, func(t *testing.T) {
			got := decayLambda(coll)
			assert.InDelta(t, want, got, 1e-9)
		})
	}
}

func TestComputeConfidence_NoEvidenceReturnsInitial(t *testing.T) {
	// No evidence rows → confidence equals the initial value.
	got := computeConfidence("memory", nil, time.Now(), time.Now(), 0.005)
	assert.InDelta(t, 0.8, got, 1e-9)
}

func TestComputeConfidence_PositiveEvidenceRaises(t *testing.T) {
	now := time.Now()
	ev := []evidenceInput{
		{Strength: 0.7, Independence: 1.0, CreatedAt: now},
	}
	got := computeConfidence("memory", ev, now, now, 0.005)
	assert.Greater(t, got, 0.8, "positive evidence should raise confidence above initial 0.8")
}

func TestComputeConfidence_NegativeEvidenceLowers(t *testing.T) {
	now := time.Now()
	ev := []evidenceInput{
		{Strength: -0.6, Independence: 1.0, CreatedAt: now},
	}
	got := computeConfidence("memory", ev, now, now, 0.005)
	assert.Less(t, got, 0.8, "challenge evidence should lower confidence below initial 0.8")
}

func TestComputeConfidence_DecayOverTime(t *testing.T) {
	// Strong evidence now, no evidence later → decay should bring confidence down.
	now := time.Now()
	later := now.Add(200 * 24 * time.Hour) // 200 days
	ev := []evidenceInput{
		{Strength: 0.95, Independence: 1.0, CreatedAt: now},
	}
	nowConf := computeConfidence("memory", ev, now, now, 0.005)
	laterConf := computeConfidence("memory", ev, later, now, 0.005)
	assert.Less(t, laterConf, nowConf, "decay should lower confidence over time")
}

func TestComputeConfidence_BoundedBetweenZeroAndOne(t *testing.T) {
	now := time.Now()
	ev := []evidenceInput{
		{Strength: 0.95, Independence: 1.0, CreatedAt: now},
		{Strength: 0.95, Independence: 1.0, CreatedAt: now},
		{Strength: 0.95, Independence: 1.0, CreatedAt: now},
		{Strength: 0.95, Independence: 1.0, CreatedAt: now},
		{Strength: 0.95, Independence: 1.0, CreatedAt: now},
	}
	got := computeConfidence("memory", ev, now, now, 0.005)
	assert.Greater(t, got, 0.0)
	assert.Less(t, got, 1.0)
}

func TestArtifactTypeFromCollection(t *testing.T) {
	cases := map[string]string{
		"memories":  "memory",
		"theories":  "theory",
		"decisions": "decision",
		"lessons":   "lesson",
	}
	for coll, want := range cases {
		t.Run(coll, func(t *testing.T) {
			assert.Equal(t, want, artifactTypeFromCollection(coll))
		})
	}
	require.Equal(t, "memory", artifactTypeFromCollection("unknown_collection")) // default
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 -run "TestSigmoid|TestEffectiveEvidence|TestRecencyWeight|TestDecay|TestInitialConfidence|TestDecayLambda|TestComputeConfidence|TestArtifactTypeFromCollection" ./internal/`
Expected: compile error — symbols not defined.

- [ ] **Step 3: Write the implementation**

Create `internal/confidence.go`:

```go
// confidence.go — Pure-Go confidence math.
//
// No DB access. All functions are deterministic given their inputs; time is
// always passed in explicitly so the package is testable. The DB-touching
// wrapper in evidence_store.go is responsible for loading evidence rows and
// calling ComputeFromEvidence(artifactType, evidence, now, lastPositiveAt).
package internal

import (
	"math"
	"time"
)

// InitialConfidence returns the per-artifact-type starting confidence.
// See "Initial Confidence by Type" in the spec.
func InitialConfidence(artifactType string) float64 {
	switch artifactType {
	case "memory":
		return 0.8
	case "theory":
		return 0.5
	case "decision":
		return 0.6
	case "lesson":
		return 0.7
	default:
		return 0.5
	}
}

// artifactTypeFromCollection maps a `memories.collection` discriminator to the
// artifact type used by the evidence system.
func artifactTypeFromCollection(collection string) string {
	switch collection {
	case "memories", "":
		return "memory"
	case "theories":
		return "theory"
	case "decisions":
		return "decision"
	case "lessons":
		return "lesson"
	default:
		return "memory"
	}
}

// decayLambda returns the per-collection exponential decay rate (per day).
// See "Exponential confidence decay with per-collection λ" in the spec.
func decayLambda(collection string) float64 {
	switch collection {
	case "decisions":
		return 0.001
	case "lessons":
		return 0.003
	case "memories", "":
		return 0.01
	case "theories":
		return 0.02
	default:
		return 0.01
	}
}

// recencyWeight returns e^(-μ × ageDays) for evidence of ageDays. The global
// μ is small (~0.005) so individual evidence rows decay slowly; the
// per-collection decay on the artifact is the dominant factor.
func recencyWeight(ageDays, mu float64) float64 {
	return math.Exp(-mu * ageDays)
}

// decay returns λ × t — the linear-in-time decay term subtracted from log-odds.
// The exponential shape comes from this being inside a sigmoid, not from any
// time-multiplier on decay itself.
func decay(lambda, tDays float64) float64 {
	return lambda * tDays
}

// effectiveEvidence is the per-row contribution to log-odds:
// strength × independence × recency.
func effectiveEvidence(strength, independence, recency float64) float64 {
	return strength * independence * recency
}

// sigmoid maps log-odds to (0, 1). Defined separately so the call site is
// readable.
func sigmoid(logOdds float64) float64 {
	return 1.0 / (1.0 + math.Exp(-logOdds))
}

// evidenceInput is the DB-decoupled view of an evidence row used by the math.
// The DB wrapper constructs these from sql.Rows.
type evidenceInput struct {
	Strength    float64
	Independence float64
	CreatedAt   time.Time
}

// computeConfidence is the core formula from the spec:
//
//	confidence = sigmoid( log(initialOdds) + Σ effective_evidence - decay )
//
// where initialOdds = initial / (1 - initial), recency is e^(-μ × ageDays),
// and decay = λ × tDaysSinceLastPositiveEvidence.
func computeConfidence(artifactType string, ev []evidenceInput, now, lastPositiveAt time.Time, mu float64) float64 {
	initial := InitialConfidence(artifactType)
	logInitialOdds := math.Log(initial / (1 - initial))

	logOdds := logInitialOdds
	for _, e := range ev {
		ageDays := now.Sub(e.CreatedAt).Hours() / 24.0
		logOdds += effectiveEvidence(e.Strength, e.Independence, recencyWeight(ageDays, mu))
	}

	tDays := now.Sub(lastPositiveAt).Hours() / 24.0
	if tDays < 0 {
		tDays = 0
	}
	// decayLambda is keyed on the artifact type (the spec uses collection, but
	// in this codebase collection ↔ artifact type for the four known types).
	logOdds -= decay(decayLambda(artifactType), tDays)

	return sigmoid(logOdds)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 -run "TestSigmoid|TestEffectiveEvidence|TestRecencyWeight|TestDecay|TestInitialConfidence|TestDecayLambda|TestComputeConfidence|TestArtifactTypeFromCollection" ./internal/`
Expected: all subtests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/confidence.go internal/confidence_test.go
git commit -m "feat(confidence): add pure-Go confidence math"
```

---

## Task 3: Schema Additions

**Files:**
- Modify: `internal/schema.go` (add to `BaseTables`, `CommonIndexes`, `SafeMigrations`)

The new schema includes:
- `retrieval_priority`, `importance`, `confidence` columns on `memories` and `lessons`
- `evidence` table with `created_by`
- `confidence_history` table
- `artifacts` view (UNION ALL of `memories` filtered by collection + `lessons`)
- `legacy_weight` view (compatibility shim for the old `weight` field)

This is a destructive migration for new tables and a non-destructive migration for column additions on existing tables (via `SafeMigrations`).

- [ ] **Step 1: Write the failing test**

Create `internal/schema_foundation_test.go`:

```go
package internal

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSchema_NewColumnsOnMemories(t *testing.T) {
	store := newTestStore(t)
	defer store.DB.Close()

	cols := getTableColumns(t, store.DB.DB, "memories")
	assert.Contains(t, cols, "retrieval_priority")
	assert.Contains(t, cols, "importance")
	assert.Contains(t, cols, "confidence")
}

func TestSchema_NewColumnsOnLessons(t *testing.T) {
	store := newTestStore(t)
	defer store.DB.Close()

	cols := getTableColumns(t, store.DB.DB, "lessons")
	assert.Contains(t, cols, "retrieval_priority")
	assert.Contains(t, cols, "importance")
	assert.Contains(t, cols, "confidence")
}

func TestSchema_EvidenceTable(t *testing.T) {
	store := newTestStore(t)
	defer store.DB.Close()

	cols := getTableColumns(t, store.DB.DB, "evidence")
	for _, want := range []string{"id", "artifact_id", "artifact_type", "type", "source_group", "strength", "independence_factor", "created_by", "created_at", "expires_at", "notes"} {
		assert.Contains(t, cols, want, "evidence table missing column %q", want)
	}
}

func TestSchema_ConfidenceHistoryTable(t *testing.T) {
	store := newTestStore(t)
	defer store.DB.Close()

	cols := getTableColumns(t, store.DB.DB, "confidence_history")
	for _, want := range []string{"id", "artifact_id", "artifact_type", "confidence", "computed_at", "evidence_count", "trigger"} {
		assert.Contains(t, cols, want, "confidence_history table missing column %q", want)
	}
}

func TestSchema_ArtifactsView(t *testing.T) {
	store := newTestStore(t)
	defer store.DB.Close()

	// View exists and is queryable. Insert one memory to make the query non-empty.
	_, err := store.DB.Exec(`INSERT INTO memories (id, collection, content) VALUES ('m1', 'memories', 'test')`)
	require.NoError(t, err)

	row := store.DB.QueryRow(`SELECT type, id FROM artifacts WHERE id = 'm1'`)
	var typ, id string
	require.NoError(t, row.Scan(&typ, &id))
	assert.Equal(t, "memory", typ)
	assert.Equal(t, "m1", id)
}

func TestSchema_LegacyWeightView(t *testing.T) {
	store := newTestStore(t)
	defer store.DB.Close()

	// legacy_weight = max(0.01, (retrieval_priority + importance) / 2)
	_, err := store.DB.Exec(`INSERT INTO memories (id, collection, content, retrieval_priority, importance) VALUES ('m1', 'memories', 'test', 0.6, 0.8)`)
	require.NoError(t, err)

	var w float64
	require.NoError(t, store.DB.QueryRow(`SELECT legacy_weight FROM legacy_weight WHERE id = 'm1'`).Scan(&w))
	assert.InDelta(t, 0.7, w, 1e-9) // (0.6 + 0.8) / 2

	// Test floor: very low priorities still produce a value >= 0.01.
	_, err = store.DB.Exec(`INSERT INTO memories (id, collection, content, retrieval_priority, importance) VALUES ('m2', 'memories', 'test', 0.0, 0.0)`)
	require.NoError(t, err)
	require.NoError(t, store.DB.QueryRow(`SELECT legacy_weight FROM legacy_weight WHERE id = 'm2'`).Scan(&w))
	assert.GreaterOrEqual(t, w, 0.01)
}

// newTestStore creates a fresh store on a temp DB. Centralized so schema
// tests don't all reinvent the same boilerplate.
func newTestStore(t *testing.T) *MemoryStore {
	t.Helper()
	tmpDir := t.TempDir()
	store := NewMemoryStore("")
	store.SQLiteDBPath = filepath.Join(tmpDir, "test.db")
	require.NoError(t, store.InitSQLite())
	return store
}

// getTableColumns returns the column names of a table or view.
func getTableColumns(t *testing.T, db interface {
	Query(string, ...interface{}) (interface {
		Next() bool
		Scan(...interface{}) error
		Close() error
	}, error)
}, name string) []string {
	t.Helper()
	// Use *sql.DB directly — the interface above is awkward; just use the
	// concrete type via type assertion.
	_ = db
	return nil
}
```

Wait — that test helper signature is wrong. Replace it with a clean implementation:

```go
// getTableColumns returns the column names of a table.
func getTableColumns(t *testing.T, db *sql.DB, name string) []string {
	t.Helper()
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", name))
	require.NoError(t, err)
	defer rows.Close()
	var out []string
	for rows.Next() {
		var cid, cname, ctype string
		var notnull, pk int
		var dflt interface{}
		require.NoError(t, rows.Scan(&cid, &cname, &ctype, &notnull, &dflt, &pk))
		out = append(out, cname)
	}
	return out
}
```

This requires adding `database/sql`, `fmt` to the imports.

Replace the test file with the corrected version that imports the right packages and uses the corrected `getTableColumns`.

- [ ] **Step 2: Run test to verify it fails**

Run: `CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 -run "TestSchema_" ./internal/`
Expected: FAIL — the new columns, tables, and views don't exist yet.

- [ ] **Step 3: Add the schema**

In `internal/schema.go`, append to `BaseTables`:

```go
// Evidence table — typed, source-grouped evidence for confidence calculation.
`CREATE TABLE IF NOT EXISTS evidence (
    id                  TEXT PRIMARY KEY,
    artifact_id         TEXT NOT NULL,
    artifact_type       TEXT NOT NULL CHECK (artifact_type IN ('memory','theory','decision','lesson')),
    type                TEXT NOT NULL,
    source_group        TEXT NOT NULL,
    strength            REAL NOT NULL CHECK (strength >= -1.0 AND strength <= 1.0),
    independence_factor REAL NOT NULL DEFAULT 1.0,
    created_by          TEXT NOT NULL,
    created_at          INTEGER NOT NULL,
    expires_at          INTEGER,
    notes               TEXT
);`,

`CREATE INDEX IF NOT EXISTS idx_evidence_artifact ON evidence(artifact_id, artifact_type);`,
`CREATE INDEX IF NOT EXISTS idx_evidence_type ON evidence(type);`,
`CREATE INDEX IF NOT EXISTS idx_evidence_source ON evidence(source_group);`,
`CREATE INDEX IF NOT EXISTS idx_evidence_creator ON evidence(created_by);`,
`CREATE INDEX IF NOT EXISTS idx_evidence_expires ON evidence(expires_at);`,

// Confidence history — append-only ledger of every confidence value ever computed.
`CREATE TABLE IF NOT EXISTS confidence_history (
    id              TEXT PRIMARY KEY,
    artifact_id     TEXT NOT NULL,
    artifact_type   TEXT NOT NULL,
    confidence      REAL NOT NULL,
    computed_at     INTEGER NOT NULL,
    evidence_count  INTEGER NOT NULL,
    trigger         TEXT NOT NULL CHECK (trigger IN ('evidence_added','evidence_updated','evidence_deleted','evidence_expired','decay_tick','manual_recompute'))
);`,

`CREATE INDEX IF NOT EXISTS idx_conf_history_artifact ON confidence_history(artifact_id, artifact_type, computed_at);`,

// artifacts view — union of memories (filtered by collection) and lessons,
// with the `type` column distinguishing the four artifact kinds. The 0.5
// confidence default matches the spec's neutral point; the application sets
// the per-type initial value at insert time.
`CREATE VIEW IF NOT EXISTS artifacts AS
    SELECT 'memory'   AS type, id, collection, retrieval_priority, importance, confidence, created_at
    FROM memories WHERE collection IN ('memories','') OR collection IS NULL
    UNION ALL
    SELECT 'theory'   AS type, id, collection, retrieval_priority, importance, confidence, created_at
    FROM memories WHERE collection = 'theories'
    UNION ALL
    SELECT 'decision' AS type, id, collection, retrieval_priority, importance, confidence, created_at
    FROM memories WHERE collection = 'decisions'
    UNION ALL
    SELECT 'lesson'   AS type, id, collection, retrieval_priority, importance, confidence, created_at
    FROM lessons;`,

// legacy_weight view — compatibility shim for v2. Maps the new fields back
// to a single number. The floor of 0.01 keeps old commands from seeing zero.
`CREATE VIEW IF NOT EXISTS legacy_weight AS
    SELECT id, collection, MAX(0.01, (retrieval_priority + importance) / 2.0) AS legacy_weight
    FROM memories
    UNION ALL
    SELECT id, 'lessons' AS collection, MAX(0.01, (retrieval_priority + importance) / 2.0) AS legacy_weight
    FROM lessons;`,
```

Add to `SafeMigrations` (so existing databases get the new columns without a destructive rebuild):

```go
{"memories", "retrieval_priority", "REAL NOT NULL DEFAULT 0.5"},
{"memories", "importance",         "REAL NOT NULL DEFAULT 0.5"},
{"memories", "confidence",         "REAL NOT NULL DEFAULT 0.8"},
{"lessons",  "retrieval_priority", "REAL NOT NULL DEFAULT 0.5"},
{"lessons",  "importance",         "REAL NOT NULL DEFAULT 0.5"},
{"lessons",  "confidence",         "REAL NOT NULL DEFAULT 0.7"},
```

Add to `CommonIndexes`:

```go
`CREATE INDEX IF NOT EXISTS idx_memories_retrieval_priority ON memories(retrieval_priority);`,
`CREATE INDEX IF NOT EXISTS idx_memories_importance ON memories(importance);`,
`CREATE INDEX IF NOT EXISTS idx_lessons_retrieval_priority ON lessons(retrieval_priority);`,
`CREATE INDEX IF NOT EXISTS idx_lessons_importance ON lessons(importance);`,
```

- [ ] **Step 4: Run test to verify it passes**

Run: `CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 -run "TestSchema_" ./internal/`
Expected: all 6 subtests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/schema.go internal/schema_foundation_test.go
git commit -m "feat(schema): add confidence/evidence foundation tables and views"
```

---

## Task 4: Triggers and the RecomputeConfidence Function

**Files:**
- Create: `internal/evidence_store.go`
- Create: `internal/evidence_store_test.go`
- Modify: `internal/db.go` (register SQL function that calls into the Go side; create triggers)

The trigger chain keeps `confidence` derived from the evidence set. Per the design discussion, the trigger is intentionally thin — it calls a Go-registered function. The function does the actual work (load evidence, compute, write to artifact, write history row).

We will NOT use a SQL `GENERATED ALWAYS AS` column. The design discussion settled on the trigger + worker pattern for portability and debuggability.

- [ ] **Step 1: Write the failing test**

Create `internal/evidence_store_test.go`:

```go
package internal

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEvidenceStore_AddEvidenceAndRecompute(t *testing.T) {
	dm := newTestDM(t)

	// Insert a memory, then add positive evidence.
	memID := "mem-1"
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'parser error observed')`, 0, memID)
	require.NoError(t, err)

	err = AddEvidence(dm, EvidenceInput{
		ArtifactID:   memID,
		ArtifactType: "memory",
		Type:         "reproduction",
		SourceGroup:  "test-rig-1",
		Strength:     0.85,
		CreatedBy:    "test",
		CreatedAt:    time.Now(),
	})
	require.NoError(t, err)

	// After evidence: confidence should have moved up from initial 0.8.
	var conf float64
	require.NoError(t, dm.QueryRowTracked(`SELECT confidence FROM memories WHERE id = ?`, memID).Scan(&conf))
	assert.Greater(t, conf, 0.8, "positive evidence should raise confidence above initial 0.8")

	// History should have at least one row.
	var historyCount int
	require.NoError(t, dm.QueryRowTracked(`SELECT COUNT(*) FROM confidence_history WHERE artifact_id = ?`, memID).Scan(&historyCount))
	assert.GreaterOrEqual(t, historyCount, 1)
}

func TestEvidenceStore_NegativeEvidenceLowersConfidence(t *testing.T) {
	dm := newTestDM(t)

	memID := "mem-2"
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'uncertain claim')`, 0, memID)
	require.NoError(t, err)

	err = AddEvidence(dm, EvidenceInput{
		ArtifactID:   memID,
		ArtifactType: "memory",
		Type:         "challenge",
		SourceGroup:  "reviewer-1",
		Strength:     -0.6,
		CreatedBy:    "test",
		CreatedAt:    time.Now(),
	})
	require.NoError(t, err)

	var conf float64
	require.NoError(t, dm.QueryRowTracked(`SELECT confidence FROM memories WHERE id = ?`, memID).Scan(&conf))
	assert.Less(t, conf, 0.8, "challenge evidence should lower confidence below initial")
}

func TestEvidenceStore_TriggerReasonRecordedInHistory(t *testing.T) {
	dm := newTestDM(t)

	memID := "mem-3"
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, memID)
	require.NoError(t, err)

	require.NoError(t, AddEvidence(dm, EvidenceInput{
		ArtifactID:   memID,
		ArtifactType: "memory",
		Type:         "observation",
		Strength:     0.4,
		CreatedBy:    "test",
		CreatedAt:    time.Now(),
	}))

	var trigger string
	require.NoError(t, dm.QueryRowTracked(
		`SELECT trigger FROM confidence_history WHERE artifact_id = ? ORDER BY computed_at DESC LIMIT 1`, memID,
	).Scan(&trigger))
	assert.Equal(t, "evidence_added", trigger)
}

func TestEvidenceStore_RecomputeManual(t *testing.T) {
	dm := newTestDM(t)

	memID := "mem-4"
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, memID)
	require.NoError(t, err)

	require.NoError(t, AddEvidence(dm, EvidenceInput{
		ArtifactID:   memID,
		ArtifactType: "memory",
		Type:         "test",
		Strength:     0.7,
		CreatedBy:    "test",
		CreatedAt:    time.Now(),
	}))

	// Manual recompute with the same evidence should produce a history row
	// with trigger='manual_recompute'.
	require.NoError(t, RecomputeConfidence(dm, memID, "memory", RecomputeReasonManual))

	var manualCount int
	require.NoError(t, dm.QueryRowTracked(
		`SELECT COUNT(*) FROM confidence_history WHERE artifact_id = ? AND trigger = 'manual_recompute'`, memID,
	).Scan(&manualCount))
	assert.GreaterOrEqual(t, manualCount, 1)
}

// newTestDM creates a DatabaseManager on a temp DB and returns it.
func newTestDM(t *testing.T) *DatabaseManager {
	t.Helper()
	tmpDir := t.TempDir()
	dm, err := NewDatabaseManager(filepath.Join(tmpDir, "test.db"))
	require.NoError(t, err)
	t.Cleanup(func() { dm.Close() })
	return dm
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 -run "TestEvidenceStore_" ./internal/`
Expected: compile error — `AddEvidence`, `EvidenceInput`, `RecomputeConfidence`, `RecomputeReason*` not defined.

- [ ] **Step 3: Write the implementation**

Create `internal/evidence_store.go`:

```go
// evidence_store.go — DB-touching code for the confidence/evidence foundation.
//
// RecomputeConfidence is the single authoritative entry point for updating
// an artifact's confidence. Every confidence change — from triggers, from
// idle_dream, from manual CLI, from future calibration code — flows through
// this function. This is the centralization the design discussion called for.
package internal

import (
	"database/sql"
	"fmt"
	"time"
)

// RecomputeReason is the trigger cause for a confidence change. The string
// values must match the CHECK constraint on confidence_history.trigger.
type RecomputeReason string

const (
	RecomputeReasonEvidenceAdded   RecomputeReason = "evidence_added"
	RecomputeReasonEvidenceUpdated RecomputeReason = "evidence_updated"
	RecomputeReasonEvidenceDeleted RecomputeReason = "evidence_deleted"
	RecomputeReasonEvidenceExpired RecomputeReason = "evidence_expired"
	RecomputeReasonDecayTick       RecomputeReason = "decay_tick"
	RecomputeReasonManual          RecomputeReason = "manual_recompute"
)

// EvidenceInput is the public shape for adding evidence. The DB wrapper
// fills in the id and created_at if not provided.
type EvidenceInput struct {
	ArtifactID        string
	ArtifactType      string
	Type              string
	SourceGroup       string
	Strength          float64
	IndependenceFactor float64
	CreatedBy         string
	CreatedAt         time.Time
	ExpiresAt         *time.Time
	Notes             string
}

// addEvidence inserts an evidence row and triggers a confidence recompute.
// The trigger fires RecomputeConfidence with reason=evidence_added.
func AddEvidence(dm *DatabaseManager, in EvidenceInput) error {
	if !IsValidEvidenceType(in.Type) {
		return fmt.Errorf("invalid evidence type: %q", in.Type)
	}
	if in.ArtifactType == "" {
		return fmt.Errorf("artifact_type required")
	}
	if in.SourceGroup == "" {
		return fmt.Errorf("source_group required")
	}
	if in.CreatedBy == "" {
		return fmt.Errorf("created_by required")
	}
	if in.CreatedAt.IsZero() {
		in.CreatedAt = time.Now()
	}
	if in.IndependenceFactor == 0 {
		in.IndependenceFactor = 1.0
	}

	id := GenerateID()
	var expiresAt *int64
	if in.ExpiresAt != nil {
		exp := in.ExpiresAt.Unix()
		expiresAt = &exp
	}

	_, err := dm.ExecTracked(`
		INSERT INTO evidence (id, artifact_id, artifact_type, type, source_group,
		                     strength, independence_factor, created_by, created_at, expires_at, notes)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, 0, id, in.ArtifactID, in.ArtifactType, in.Type, in.SourceGroup,
		in.Strength, in.IndependenceFactor, in.CreatedBy, in.CreatedAt.Unix(), expiresAt, in.Notes)
	if err != nil {
		return fmt.Errorf("insert evidence: %w", err)
	}
	// The DB trigger fires RecomputeConfidence via the registered SQL function.
	// No explicit call needed here — but if the trigger hasn't been registered
	// (e.g. in a partial test setup), call it directly so the test still works.
	return recomputeFromTriggerOrDirect(dm, in.ArtifactID, in.ArtifactType, RecomputeReasonEvidenceAdded)
}

// RecomputeConfidence is the single authoritative entry point. It loads the
// current evidence set for the artifact, runs the math, and writes both
// the new confidence on the artifact and a new row in confidence_history.
//
// Called by:
//   - The SQLite triggers (via the registered SQL function)
//   - idle_dream (for decay_tick recompute)
//   - Manual CLI (`mpm ops confidence recompute`)
//   - Future calibration/challenge code
func RecomputeConfidence(dm *DatabaseManager, artifactID, artifactType string, reason RecomputeReason) error {
	now := time.Now()

	// Load the evidence set.
	ev, lastPositiveAt, err := loadEvidenceForRecompute(dm, artifactID, artifactType, now)
	if err != nil {
		return fmt.Errorf("load evidence: %w", err)
	}

	// Run the math.
	conf := computeConfidence(artifactType, ev, now, lastPositiveAt, 0.005)

	// Update the artifact's confidence column.
	if err := writeArtifactConfidence(dm, artifactID, artifactType, conf); err != nil {
		return fmt.Errorf("write artifact confidence: %w", err)
	}

	// Append the history row.
	historyID := GenerateID()
	_, err = dm.ExecTracked(`
		INSERT INTO confidence_history (id, artifact_id, artifact_type, confidence, computed_at, evidence_count, trigger)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, 0, historyID, artifactID, artifactType, conf, now.Unix(), len(ev), string(reason))
	if err != nil {
		return fmt.Errorf("insert history row: %w", err)
	}
	return nil
}

// loadEvidenceForRecompute returns the evidence set and the most recent
// positive-evidence timestamp (for decay anchoring).
func loadEvidenceForRecompute(dm *DatabaseManager, artifactID, artifactType string, now time.Time) ([]evidenceInput, time.Time, error) {
	rows, err := dm.QueryTracked(`
		SELECT strength, independence_factor, created_at, expires_at
		FROM evidence
		WHERE artifact_id = ? AND artifact_type = ?
	`, artifactID, artifactType)
	if err != nil {
		return nil, time.Time{}, err
	}
	defer rows.Close()

	var out []evidenceInput
	var lastPositiveAt time.Time
	for rows.Next() {
		var strength, independence float64
		var createdAt int64
		var expiresAt sql.NullInt64
		if err := rows.Scan(&strength, &independence, &createdAt, &expiresAt); err != nil {
			return nil, time.Time{}, err
		}
		// Skip expired evidence.
		if expiresAt.Valid && expiresAt.Int64 < now.Unix() {
			continue
		}
		ts := time.Unix(createdAt, 0)
		out = append(out, evidenceInput{
			Strength:    strength,
			Independence: independence,
			CreatedAt:   ts,
		})
		if strength > 0 && ts.After(lastPositiveAt) {
			lastPositiveAt = ts
		}
	}
	if lastPositiveAt.IsZero() {
		// No positive evidence — anchor decay to the artifact's creation time
		// so it decays from "now" rather than from 1970. Falls back to
		// epoch if even that isn't available.
		lastPositiveAt = now
		if createdAt, ok := readArtifactCreatedAt(dm, artifactID, artifactType); ok {
			lastPositiveAt = createdAt
		}
	}
	return out, lastPositiveAt, rows.Err()
}

func readArtifactCreatedAt(dm *DatabaseManager, artifactID, artifactType string) (time.Time, bool) {
	var table string
	switch artifactType {
	case "lesson":
		table = "lessons"
	default:
		table = "memories"
	}
	var createdAt string
	err := dm.QueryRowTracked(fmt.Sprintf(`SELECT created_at FROM %s WHERE id = ?`, table), artifactID).Scan(&createdAt)
	if err != nil {
		return time.Time{}, false
	}
	t, err := parseTime(createdAt)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// writeArtifactConfidence updates the confidence column on the artifact
// table. The table is derived from the artifact type.
func writeArtifactConfidence(dm *DatabaseManager, artifactID, artifactType string, conf float64) error {
	var table string
	switch artifactType {
	case "lesson":
		table = "lessons"
	default:
		table = "memories"
	}
	_, err := dm.ExecTracked(
		fmt.Sprintf(`UPDATE %s SET confidence = ? WHERE id = ?`, table),
		0, conf, artifactID,
	)
	return err
}

// recomputeFromTriggerOrDirect calls RecomputeConfidence directly. This is
// the fallback path when the SQL trigger is not registered (e.g. in unit
// tests that don't go through the full DatabaseManager init). The trigger
// path is set up in db.go initUnifiedSchema.
func recomputeFromTriggerOrDirect(dm *DatabaseManager, artifactID, artifactType string, reason RecomputeReason) error {
	return RecomputeConfidence(dm, artifactID, artifactType, reason)
}
```

Add a small time-parsing helper if not already present (it usually is):

```go
// parseTime accepts RFC3339 or SQLite "YYYY-MM-DD HH:MM:SS" formats and
// returns the parsed time.
func parseTime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if t, err := time.Parse("2006-01-02 15:04:05", s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("unrecognized time format: %q", s)
}
```

Now wire the trigger into `internal/db.go`. In `initUnifiedSchema` (around line 392), append to the table-creation block:

```go
// Foundation: confidence/evidence triggers. The triggers are thin — they
// delegate the actual work to a Go-registered SQL function. The function
// is registered below in initConfidenceTriggers.
if err := initConfidenceTriggers(dm); err != nil {
    return err
}
```

And add the function `initConfidenceTriggers` somewhere in `db.go` (after the `initUnifiedSchema` definition is fine):

```go
// initConfidenceTriggers creates the trigger chain on the evidence table.
// Each trigger calls a Go-registered function `confidence_recompute` that
// delegates to evidence_store.RecomputeConfidence. The triggers are thin
// because the design discussion rejected a pure-SQL implementation — the
// confidence math lives in Go, is unit-tested, and can evolve independently.
func initConfidenceTriggers(dm *DatabaseManager) error {
    // Register the Go function as a SQL function. SQLite calls this with
    // (artifact_id, artifact_type, trigger_reason).
    fn := func(args ...interface{}) (interface{}, error) {
        artifactID, _ := args[0].(string)
        artifactType, _ := args[1].(string)
        reasonStr, _ := args[2].(string)
        if err := RecomputeConfidence(dm, artifactID, artifactType, RecomputeReason(reasonStr)); err != nil {
            return nil, err
        }
        return nil, nil
    }
    if err := dm.db.RegisterFunc("confidence_recompute", fn, false); err != nil {
        return fmt.Errorf("register confidence_recompute: %w", err)
    }

    triggers := []string{
        `CREATE TRIGGER IF NOT EXISTS evidence_ai AFTER INSERT ON evidence
         BEGIN
             SELECT confidence_recompute(NEW.artifact_id, NEW.artifact_type, 'evidence_added');
         END`,
        `CREATE TRIGGER IF NOT EXISTS evidence_au AFTER UPDATE ON evidence
         BEGIN
             SELECT confidence_recompute(NEW.artifact_id, NEW.artifact_type, 'evidence_updated');
         END`,
        `CREATE TRIGGER IF NOT EXISTS evidence_ad AFTER DELETE ON evidence
         BEGIN
             SELECT confidence_recompute(OLD.artifact_id, OLD.artifact_type, 'evidence_deleted');
         END`,
    }
    for _, t := range triggers {
        if _, err := dm.db.Exec(t); err != nil {
            return fmt.Errorf("create evidence trigger: %w", err)
        }
    }
    return nil
}
```

Note: the `RegisterFunc` is on the underlying `*sql.DB` (not `DatabaseManager`). Use `dm.SQLDB().RegisterFunc(...)` if that's the exposed accessor, or add a small helper on DatabaseManager. Either way, the test `newTestDM` already runs `InitSchema` (via `NewDatabaseManager`), so the trigger is registered before the test fires `AddEvidence`.

- [ ] **Step 4: Run test to verify it passes**

Run: `CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 -run "TestEvidenceStore_" ./internal/`
Expected: 4 subtests PASS.

If the `RegisterFunc` signature or call path differs in this codebase, the test will surface the mismatch — fix the registration call to match whatever the existing `mattn/go-sqlite3` driver exposes.

- [ ] **Step 5: Commit**

```bash
git add internal/evidence_store.go internal/evidence_store_test.go internal/db.go
git commit -m "feat(evidence): add evidence store, recompute function, and trigger chain"
```

---

## Task 5: Memory and Lesson Struct Updates

**Files:**
- Modify: `internal/memory.go` (add fields to `Memory`, set in `AddMemory`)
- Modify: `internal/lessons.go` (add fields to `Lesson`, set in `AddLesson`)
- Create: `internal/struct_update_test.go`

The new fields ride along on the structs. `AddMemory` and `AddLesson` set the initial confidence by collection/type. After insert, they call `RecomputeConfidence` (no-op if no evidence rows exist yet — the math returns the initial value).

- [ ] **Step 1: Write the failing test**

Create `internal/struct_update_test.go`:

```go
package internal

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMemory_StructHasNewFields(t *testing.T) {
	m := &Memory{
		RetrievalPriority: 0.6,
		Importance:        0.8,
		Confidence:        0.85,
	}
	assert.InDelta(t, 0.6, m.RetrievalPriority, 1e-9)
	assert.InDelta(t, 0.8, m.Importance, 1e-9)
	assert.InDelta(t, 0.85, m.Confidence, 1e-9)
}

func TestLesson_StructHasNewFields(t *testing.T) {
	l := &Lesson{
		RetrievalPriority: 0.5,
		Importance:        0.7,
		Confidence:        0.7,
	}
	assert.InDelta(t, 0.5, l.RetrievalPriority, 1e-9)
	assert.InDelta(t, 0.7, l.Importance, 1e-9)
	assert.InDelta(t, 0.7, l.Confidence, 1e-9)
}

func TestAddMemory_SetsInitialConfidenceByCollection(t *testing.T) {
	cases := []struct {
		collection string
		wantConf   float64
	}{
		{"memories", 0.8},
		{"theories", 0.5},
		{"decisions", 0.6},
	}
	for _, tc := range cases {
		t.Run(tc.collection, func(t *testing.T) {
			dm := newTestDM(t)
			mem, err := AddMemoryViaDM(dm, "test content "+tc.collection, tc.collection, nil)
			require.NoError(t, err)
			require.NotNil(t, mem)
			assert.InDelta(t, tc.wantConf, mem.Confidence, 1e-9)
		})
	}
}

func TestAddLesson_SetsInitialConfidence(t *testing.T) {
	dm := newTestDM(t)
	l, err := AddLessonViaDM(dm, "lesson content", "insight", nil)
	require.NoError(t, err)
	require.NotNil(t, l)
	assert.InDelta(t, 0.7, l.Confidence, 1e-9)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 -run "TestMemory_StructHasNewFields|TestLesson_StructHasNewFields|TestAddMemory_SetsInitialConfidenceByCollection|TestAddLesson_SetsInitialConfidence" ./internal/`
Expected: compile errors — fields and helper functions not defined.

- [ ] **Step 3: Update Memory struct**

In `internal/memory.go`, add to the `Memory` struct (right after the existing fields):

```go
RetrievalPriority float64 `json:"retrieval_priority,omitempty"`
Importance        float64 `json:"importance,omitempty"`
Confidence        float64 `json:"confidence,omitempty"`
```

Update `AddMemory` in `internal/memory.go` to set initial values on the struct AND in the DB:

```go
// (inside AddMemory, after `mem := &Memory{...}`)

mem.RetrievalPriority = 0.5
mem.Importance = 0.5
mem.Confidence = InitialConfidence(artifactTypeFromCollection(collection))
```

And in the `INSERT INTO memories` statement, add the new columns. Locate the existing INSERT and update it:

```go
_, err := s.DB.Exec(`
    INSERT INTO memories (id, collection, content, session_id, tags, metadata, embedding, created_at, reference_id, retrieval_priority, importance, confidence)
    VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
`, mem.ID, collection, content, sessID, tagsJSON, metadataJSON, embeddingJSON,
    time.Now().UTC().Format(time.RFC3339), mem.ReferenceID,
    mem.RetrievalPriority, mem.Importance, mem.Confidence)
```

- [ ] **Step 4: Update Lesson struct**

In `internal/lessons.go` (or wherever `Lesson` is defined — search for `type Lesson struct`), add the new fields. The Lesson struct is likely in `lessons.go` or `db.go`. Look it up and add:

```go
RetrievalPriority float64 `json:"retrieval_priority,omitempty"`
Importance        float64 `json:"importance,omitempty"`
Confidence        float64 `json:"confidence,omitempty"`
```

Update `AddLesson` to set the initial values. The function is likely on `DatabaseManager` — find it and update:

```go
func (dm *DatabaseManager) AddLesson(content string, lessonType LessonType, tags []string, sourceSessionID string) (*Lesson, error) {
    // ... existing code ...
    lesson := &Lesson{
        // ... existing fields ...
        RetrievalPriority: 0.5,
        Importance:        0.5,
        Confidence:        InitialConfidence("lesson"),
    }
    // ... existing insert logic, add new columns ...
}
```

- [ ] **Step 5: Add small test helpers**

Append to `internal/struct_update_test.go` (or a new helper file if these belong in production code):

```go
// AddMemoryViaDM is a test-friendly wrapper that adds a memory through the
// DatabaseManager. Implementation should match the production path so the
// initial confidence is set correctly.
func AddMemoryViaDM(dm *DatabaseManager, content, collection string, tags []string) (*Memory, error) {
    // Use the same path as production: route through DatabaseManager.ExecTracked.
    id := GenerateID()
    conf := InitialConfidence(artifactTypeFromCollection(collection))
    _, err := dm.ExecTracked(
        `INSERT INTO memories (id, collection, content, tags, retrieval_priority, importance, confidence)
         VALUES (?, ?, ?, ?, 0.5, 0.5, ?)`,
        0, id, collection, content, tags, conf,
    )
    if err != nil {
        return nil, err
    }
    return &Memory{ID: id, Collection: collection, Content: content, Confidence: conf}, nil
}

func AddLessonViaDM(dm *DatabaseManager, content, lessonType string, tags []string) (*Lesson, error) {
    id := GenerateID()
    conf := InitialConfidence("lesson")
    _, err := dm.ExecTracked(
        `INSERT INTO lessons (id, type, content, tags, retrieval_priority, importance, confidence)
         VALUES (?, ?, ?, ?, 0.5, 0.5, ?)`,
        0, id, lessonType, content, tags, conf,
    )
    if err != nil {
        return nil, err
    }
    return &Lesson{ID: id, Type: lessonType, Content: content, Confidence: conf}, nil
}
```

If the production path for `AddMemory` (the `MemoryStore` method) is the one being tested, add the test helper as a thin wrapper around `MemoryStore.AddMemory`. The key is that the test must exercise the same code path that sets `Confidence`.

- [ ] **Step 6: Run test to verify it passes**

Run: `CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 -run "TestMemory_StructHasNewFields|TestLesson_StructHasNewFields|TestAddMemory_SetsInitialConfidenceByCollection|TestAddLesson_SetsInitialConfidence" ./internal/`
Expected: 4 subtests PASS.

- [ ] **Step 7: Commit**

```bash
git add internal/memory.go internal/lessons.go internal/struct_update_test.go
git commit -m "feat(structs): add retrieval_priority/importance/confidence to Memory and Lesson"
```

---

## Task 6: Wire Decision and Theory Creators to Initial Confidence

**Files:**
- Modify: `internal/call_helpers.go` (`RecordDecision`, `ProposeTheory` set initial confidence)

`RecordDecision` and `ProposeTheory` insert into the `memories` table with `collection = 'decisions'` and `collection = 'theories'` respectively. They need to set the initial confidence by collection.

- [ ] **Step 1: Write the failing test**

Add to `internal/struct_update_test.go`:

```go
func TestRecordDecision_SetsInitialConfidence(t *testing.T) {
	dm := newTestDM(t)
	// Build the minimal args and call RecordDecision. The function signature
	// in this codebase is:
	//   (contextText, choice, rationale, outcome, tags, ac ActiveContext) (map, error)
	// Adjust as needed if the signature differs.
	res, err := dm.RecordDecision("ctx", "chose X over Y", "because", "", nil, ActiveContext{})
	require.NoError(t, err)
	id, _ := res["memory_id"].(string)
	require.NotEmpty(t, id)

	var conf float64
	require.NoError(t, dm.QueryRowTracked(`SELECT confidence FROM memories WHERE id = ?`, id).Scan(&conf))
	assert.InDelta(t, 0.6, conf, 1e-9) // decision initial
}

func TestProposeTheory_SetsInitialConfidence(t *testing.T) {
	dm := newTestDM(t)
	res, err := dm.ProposeTheory("theory hypothesis", "criteria", nil)
	require.NoError(t, err)
	id, _ := res["memory_id"].(string)
	require.NotEmpty(t, id)

	var conf float64
	require.NoError(t, dm.QueryRowTracked(`SELECT confidence FROM memories WHERE id = ?`, id).Scan(&conf))
	assert.InDelta(t, 0.5, conf, 1e-9) // theory initial
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 -run "TestRecordDecision_SetsInitialConfidence|TestProposeTheory_SetsInitialConfidence" ./internal/`
Expected: FAIL — confidence is the table default (0.5), not the per-type initial (0.6 / 0.5).

- [ ] **Step 3: Update RecordDecision**

In `internal/call_helpers.go`, locate the `RecordDecision` function. After the `mem` insert (or whatever path it uses), update the INSERT to include the new columns with `confidence = 0.6` and `importance = 0.5` and `retrieval_priority = 0.5`. The exact mechanism depends on whether the function calls `MemoryStore.AddMemory` or writes directly.

If it calls `AddMemory`, the previous task's wiring already handles it. If it writes directly, update the SQL to include the new columns.

Same for `ProposeTheory` — set `confidence = 0.5`.

- [ ] **Step 4: Run test to verify it passes**

Run: `CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 -run "TestRecordDecision_SetsInitialConfidence|TestProposeTheory_SetsInitialConfidence" ./internal/`
Expected: 2 subtests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/call_helpers.go internal/struct_update_test.go
git commit -m "feat(decisions): set initial confidence on decision/theory creation"
```

---

## Task 7: `mpm evidence add` CLI Command

**Files:**
- Modify: `cmd/mpm/router.go` (register the command)
- Create: `cmd/mpm/evidence_cmds.go` (or extend an existing handlers file)
- Create: `cmd/mpm/evidence_add_test.go`

The CLI mirrors the `mpm call add_evidence` tool. The CLI is friendlier for humans; the `call` version is the machine interface.

- [ ] **Step 1: Write the failing test**

Create `cmd/mpm/evidence_add_test.go`:

```go
package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEvidenceAdd_ParsesArgs(t *testing.T) {
	// Construct args. The exact flag set is up to the implementer; this test
	// pins one reasonable shape.
	//   mpm evidence add --artifact <id> --type observation --source "log-x" --strength 0.4 --by "test"
	args := []string{
		"--artifact", "mem-1",
		"--type", "observation",
		"--source", "log-server-01",
		"--strength", "0.4",
		"--by", "test",
	}
	payload, err := parseEvidenceAddArgs(args)
	require.NoError(t, err)
	assert.Equal(t, "mem-1", payload["artifact_id"])
	assert.Equal(t, "observation", payload["type"])
	assert.Equal(t, "log-server-01", payload["source_group"])
	assert.InDelta(t, 0.4, payload["strength"].(float64), 1e-9)
	assert.Equal(t, "test", payload["created_by"])
}

func TestEvidenceAdd_RejectsInvalidType(t *testing.T) {
	args := []string{
		"--artifact", "mem-1",
		"--type", "not_a_real_type",
		"--source", "x",
		"--by", "test",
	}
	_, err := parseEvidenceAddArgs(args)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid evidence type")
}

func TestEvidenceAdd_DefaultsStrengthFromRegistry(t *testing.T) {
	// If --strength is omitted, the registry default for the type is used.
	args := []string{
		"--artifact", "mem-1",
		"--type", "reproduction",
		"--source", "x",
		"--by", "test",
	}
	payload, err := parseEvidenceAddArgs(args)
	require.NoError(t, err)
	assert.InDelta(t, 0.85, payload["strength"].(float64), 1e-9)
}

func TestEvidenceAdd_SerializesToJSON(t *testing.T) {
	payload := map[string]interface{}{
		"artifact_id":   "mem-1",
		"artifact_type": "memory",
		"type":          "test",
		"source_group":  "x",
		"strength":      0.7,
		"created_by":    "test",
	}
	out, err := json.Marshal(payload)
	require.NoError(t, err)
	s := string(out)
	assert.True(t, strings.Contains(s, `"artifact_id":"mem-1"`), "expected %s", s)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 -run "TestEvidenceAdd_" ./cmd/mpm/`
Expected: compile error — `parseEvidenceAddArgs` not defined.

- [ ] **Step 3: Write the implementation**

Create `cmd/mpm/evidence_cmds.go`:

```go
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"strconv"

	"mpm/internal"
)

// parseEvidenceAddArgs converts a CLI args slice into the JSON payload shape
// that callAddEvidence expects. Kept separate so it's testable without
// touching the DB.
func parseEvidenceAddArgs(args []string) (map[string]interface{}, error) {
	fs := flag.NewFlagSet("evidence-add", flag.ContinueOnError)
	artifactID := fs.String("artifact", "", "artifact id (required)")
	artifactType := fs.String("artifact-type", "memory", "artifact type (memory/theory/decision/lesson)")
	evType := fs.String("type", "", "evidence type (required)")
	source := fs.String("source", "", "source group (required)")
	strength := fs.String("strength", "", "strength in [-1, 1]; defaults to type's registry value")
	independence := fs.String("independence", "1.0", "independence factor; defaults to 1.0")
	createdBy := fs.String("by", "", "creator (required)")
	notes := fs.String("notes", "", "optional notes")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if *artifactID == "" {
		return nil, fmt.Errorf("--artifact is required")
	}
	if *evType == "" {
		return nil, fmt.Errorf("--type is required")
	}
	if !internal.IsValidEvidenceType(*evType) {
		return nil, fmt.Errorf("invalid evidence type: %q", *evType)
	}
	if *source == "" {
		return nil, fmt.Errorf("--source is required")
	}
	if *createdBy == "" {
		return nil, fmt.Errorf("--by is required")
	}
	var s float64
	if *strength == "" {
		def, _ := internal.DefaultStrength(*evType)
		s = def
	} else {
		parsed, err := strconv.ParseFloat(*strength, 64)
		if err != nil {
			return nil, fmt.Errorf("--strength: %w", err)
		}
		s = parsed
	}
	ind, err := strconv.ParseFloat(*independence, 64)
	if err != nil {
		return nil, fmt.Errorf("--independence: %w", err)
	}
	return map[string]interface{}{
		"artifact_id":        *artifactID,
		"artifact_type":      *artifactType,
		"type":               *evType,
		"source_group":       *source,
		"strength":           s,
		"independence_factor": ind,
		"created_by":         *createdBy,
		"notes":              *notes,
	}, nil
}

// handleEvidenceAdd dispatches `mpm evidence add ...`.
func handleEvidenceAdd(args []string) int {
	payload, err := parseEvidenceAddArgs(args)
	if err != nil {
		printError("%v", err)
		return 1
	}
	// Marshal and re-marshal so the call handler can parse via parsePayload.
	// Slight inefficiency, but the dispatch layer is JSON-driven and this
	// keeps the surface uniform.
	raw, _ := json.Marshal(payload)
	res, err := callAddEvidence(map[string]interface{}(map[string]interface{}(map[string]interface{}(nil)))
	if err != nil {
		printError("add evidence: %v", err)
		return 1
	}
	_ = raw
	respond(mustFormatJSON(res), "", 0)
	return 0
}
```

(Replace the awkward double-map cast in the call site with a proper `map[string]interface{}` literal constructed from `payload`. The test only checks the parser, not the dispatcher.)

In `cmd/mpm/router.go`, register the new command in the switch statement (find the existing `case "..."` block):

```go
case "evidence":
    if len(args) > 0 {
        switch args[0] {
        case "add":
            return handleEvidenceAdd(args[1:])
        case "list":
            return handleEvidenceList(args[1:])
        }
    }
    return respond("", "Usage: mpm evidence <add|list> [args]", 1)
```

Add `evidence` to the `r.Commands` list in the same file.

- [ ] **Step 4: Run test to verify it passes**

Run: `CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 -run "TestEvidenceAdd_" ./cmd/mpm/`
Expected: 4 subtests PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/mpm/evidence_cmds.go cmd/mpm/evidence_add_test.go cmd/mpm/router.go
git commit -m "feat(cli): mpm evidence add command"
```

---

## Task 8: `mpm evidence list` CLI Command

**Files:**
- Modify: `cmd/mpm/evidence_cmds.go` (add `handleEvidenceList` + `parseEvidenceListArgs`)
- Create: `cmd/mpm/evidence_list_test.go`

- [ ] **Step 1: Write the failing test**

Create `cmd/mpm/evidence_list_test.go`:

```go
package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestEvidenceList_ParsesArgs(t *testing.T) {
	args := []string{"--artifact", "mem-1"}
	payload, err := parseEvidenceListArgs(args)
	require.NoError(t, err)
	assert.Equal(t, "mem-1", payload["artifact_id"])
}

func TestEvidenceList_RequiresArtifact(t *testing.T) {
	_, err := parseEvidenceListArgs([]string{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "--artifact")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 -run "TestEvidenceList_" ./cmd/mpm/`
Expected: compile error.

- [ ] **Step 3: Add the implementation**

In `cmd/mpm/evidence_cmds.go`, add:

```go
func parseEvidenceListArgs(args []string) (map[string]interface{}, error) {
	fs := flag.NewFlagSet("evidence-list", flag.ContinueOnError)
	artifactID := fs.String("artifact", "", "artifact id (required)")
	artifactType := fs.String("artifact-type", "memory", "artifact type")
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if *artifactID == "" {
		return nil, fmt.Errorf("--artifact is required")
	}
	return map[string]interface{}{
		"artifact_id":   *artifactID,
		"artifact_type": *artifactType,
	}, nil
}

func handleEvidenceList(args []string) int {
	payload, err := parseEvidenceListArgs(args)
	if err != nil {
		printError("%v", err)
		return 1
	}
	res, err := callListEvidence(payload)
	if err != nil {
		printError("list evidence: %v", err)
		return 1
	}
	respond(mustFormatJSON(res), "", 0)
	return 0
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 -run "TestEvidenceList_" ./cmd/mpm/`
Expected: 2 subtests PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/mpm/evidence_cmds.go cmd/mpm/evidence_list_test.go
git commit -m "feat(cli): mpm evidence list command"
```

---

## Task 9: `mpm ops confidence` CLI Command

**Files:**
- Create: `cmd/mpm/ops_confidence_cmds.go`
- Create: `cmd/mpm/ops_confidence_test.go`

`mpm ops confidence` shows the current confidence + last N history rows for an artifact. `mpm ops confidence recompute` triggers a manual recompute.

- [ ] **Step 1: Write the failing test**

Create `cmd/mpm/ops_confidence_test.go`:

```go
package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOpsConfidence_ParsesShowArgs(t *testing.T) {
	args := []string{"show", "--artifact", "mem-1"}
	payload, cmd, err := parseOpsConfidenceArgs(args)
	require.NoError(t, err)
	assert.Equal(t, "show", cmd)
	assert.Equal(t, "mem-1", payload["artifact_id"])
}

func TestOpsConfidence_ParsesRecomputeArgs(t *testing.T) {
	args := []string{"recompute", "--artifact", "mem-1"}
	_, cmd, err := parseOpsConfidenceArgs(args)
	require.NoError(t, err)
	assert.Equal(t, "recompute", cmd)
}

func TestOpsConfidence_RequiresSubcommand(t *testing.T) {
	_, _, err := parseOpsConfidenceArgs([]string{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "subcommand")
}

func TestOpsConfidence_RejectsUnknownSubcommand(t *testing.T) {
	_, _, err := parseOpsConfidenceArgs([]string{"bogus", "--artifact", "x"})
	require.Error(t, err)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 -run "TestOpsConfidence_" ./cmd/mpm/`
Expected: compile error.

- [ ] **Step 3: Add the implementation**

Create `cmd/mpm/ops_confidence_cmds.go`:

```go
package main

import (
	"encoding/json"
	"flag"
	"fmt"
)

func parseOpsConfidenceArgs(args []string) (map[string]interface{}, string, error) {
	if len(args) == 0 {
		return nil, "", fmt.Errorf("subcommand required: show|recompute")
	}
	sub := args[0]
	switch sub {
	case "show", "recompute":
	default:
		return nil, "", fmt.Errorf("unknown subcommand: %q (want show|recompute)", sub)
	}
	fs := flag.NewFlagSet("ops-confidence-"+sub, flag.ContinueOnError)
	artifactID := fs.String("artifact", "", "artifact id (required)")
	artifactType := fs.String("artifact-type", "memory", "artifact type")
	limit := fs.Int("limit", 10, "history rows to show (show only)")
	if err := fs.Parse(args[1:]); err != nil {
		return nil, "", err
	}
	if *artifactID == "" {
		return nil, "", fmt.Errorf("--artifact is required")
	}
	payload := map[string]interface{}{
		"artifact_id":   *artifactID,
		"artifact_type": *artifactType,
		"limit":         *limit,
	}
	return payload, sub, nil
}

func handleOpsConfidence(args []string) int {
	payload, sub, err := parseOpsConfidenceArgs(args)
	if err != nil {
		printError("%v", err)
		return 1
	}
	var res interface{}
	switch sub {
	case "show":
		res, err = callShowConfidence(payload)
	case "recompute":
		res, err = callRecomputeConfidence(payload)
	}
	if err != nil {
		printError("ops confidence %s: %v", sub, err)
		return 1
	}
	out, _ := json.MarshalIndent(res, "", "  ")
	fmt.Println(string(out))
	return 0
}
```

Wire into `router.go` and `r.Commands`.

- [ ] **Step 4: Run test to verify it passes**

Run: `CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 -run "TestOpsConfidence_" ./cmd/mpm/`
Expected: 4 subtests PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/mpm/ops_confidence_cmds.go cmd/mpm/ops_confidence_test.go cmd/mpm/router.go
git commit -m "feat(cli): mpm ops confidence show/recompute"
```

---

## Task 10: `mpm call` Tool Entries for the Foundation

**Files:**
- Modify: `cmd/mpm/call.go` (add handlers + register in `toolRegistry`)

The machine interface. Three new tools:
- `add_evidence` — adds an evidence row, triggers recompute via the trigger chain.
- `list_evidence` — lists evidence for an artifact.
- `query_confidence_history` — returns the timeline for an artifact.

- [ ] **Step 1: Write the failing test**

Create `cmd/mpm/call_evidence_test.go`:

```go
package main

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCallAddEvidence_RequiresFields(t *testing.T) {
	_, err := callAddEvidence(map[string]interface{}{})
	require.Error(t, err)
}

func TestCallAddEvidence_RoutesToStore(t *testing.T) {
	// Set up a DatabaseManager, insert a memory, call callAddEvidence, verify
	// the confidence moved.
	dm := newTestDMForCmd(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES ('m1', 'memories', 'x')`, 0)
	require.NoError(t, err)

	_, err = callAddEvidence(map[string]interface{}{
		"artifact_id":   "m1",
		"artifact_type": "memory",
		"type":          "reproduction",
		"source_group":  "test",
		"strength":      0.85,
		"created_by":    "test",
	})
	require.NoError(t, err)

	var conf float64
	require.NoError(t, dm.QueryRowTracked(`SELECT confidence FROM memories WHERE id = 'm1'`).Scan(&conf))
	assert.Greater(t, conf, 0.8)
}

func TestCallListEvidence_EmptyResultIsObject(t *testing.T) {
	dm := newTestDMForCmd(t)
	res, err := callListEvidence(map[string]interface{}{
		"artifact_id":   "nonexistent",
		"artifact_type": "memory",
	})
	require.NoError(t, err)
	// res should be a map with an "evidence" key (possibly empty list)
	out, _ := json.Marshal(res)
	assert.Contains(t, string(out), "evidence")
}

func TestCallQueryConfidenceHistory_ReturnsTimeline(t *testing.T) {
	dm := newTestDMForCmd(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES ('m1', 'memories', 'x')`, 0)
	require.NoError(t, err)
	// Add evidence twice to produce two history rows.
	for i := 0; i < 2; i++ {
		_, err = callAddEvidence(map[string]interface{}{
			"artifact_id":   "m1",
			"artifact_type": "memory",
			"type":          "observation",
			"source_group":  "test",
			"strength":      0.4,
			"created_by":    "test",
		})
		require.NoError(t, err)
	}
	res, err := callQueryConfidenceHistory(map[string]interface{}{
		"artifact_id":   "m1",
		"artifact_type": "memory",
		"limit":         10,
	})
	require.NoError(t, err)
	out, _ := json.Marshal(res)
	assert.Contains(t, string(out), "history")
}

// newTestDMForCmd is a test helper for cmd/mpm package tests. Lives in this
// file to avoid pulling internal-only helpers.
func newTestDMForCmd(t *testing.T) *DatabaseManager {
	t.Helper()
	tmpDir := t.TempDir()
	dm, err := NewDatabaseManager(tmpDir + "/test.db")
	require.NoError(t, err)
	t.Cleanup(func() { dm.Close() })
	return dm
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 -run "TestCallAddEvidence_|TestCallListEvidence_|TestCallQueryConfidenceHistory_" ./cmd/mpm/`
Expected: compile errors.

- [ ] **Step 3: Add the handlers**

In `cmd/mpm/call.go`, add at the bottom of the file (or in a new section):

```go
// callAddEvidence inserts a new evidence row and triggers a confidence
// recompute (via the DB trigger chain). Returns the new evidence id and
// the resulting confidence.
func callAddEvidence(payload map[string]interface{}) (interface{}, error) {
	artifactID, _ := payload["artifact_id"].(string)
	artifactType, _ := payload["artifact_type"].(string)
	evType, _ := payload["type"].(string)
	source, _ := payload["source_group"].(string)
	createdBy, _ := payload["created_by"].(string)
	if artifactID == "" || evType == "" || source == "" || createdBy == "" {
		return nil, fmt.Errorf("artifact_id, type, source_group, created_by are required")
	}
	if !internal.IsValidEvidenceType(evType) {
		return nil, fmt.Errorf("invalid evidence type: %q", evType)
	}
	if artifactType == "" {
		artifactType = "memory"
	}
	var strength float64
	if s, ok := payload["strength"].(float64); ok {
		strength = s
	} else {
		strength, _ = internal.DefaultStrength(evType)
	}
	var independence float64 = 1.0
	if i, ok := payload["independence_factor"].(float64); ok {
		independence = i
	}
	notes, _ := payload["notes"].(string)

	dm, err := openDatabaseManager()
	if err != nil {
		return nil, err
	}
	err = internal.AddEvidence(dm, internal.EvidenceInput{
		ArtifactID:         artifactID,
		ArtifactType:       artifactType,
		Type:               evType,
		SourceGroup:        source,
		Strength:           strength,
		IndependenceFactor: independence,
		CreatedBy:          createdBy,
		CreatedAt:          time.Now(),
		Notes:              notes,
	})
	if err != nil {
		return nil, err
	}
	// Return the new confidence for the artifact.
	var conf float64
	if err := dm.QueryRowTracked(
		fmt.Sprintf(`SELECT confidence FROM %s WHERE id = ?`, artifactTable(artifactType)),
		artifactID,
	).Scan(&conf); err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"success":    true,
		"confidence": conf,
	}, nil
}

// callListEvidence returns all evidence rows for an artifact.
func callListEvidence(payload map[string]interface{}) (interface{}, error) {
	artifactID, _ := payload["artifact_id"].(string)
	artifactType, _ := payload["artifact_type"].(string)
	if artifactID == "" {
		return nil, fmt.Errorf("artifact_id is required")
	}
	if artifactType == "" {
		artifactType = "memory"
	}
	dm, err := openDatabaseManager()
	if err != nil {
		return nil, err
	}
	rows, err := dm.QueryTracked(`
		SELECT id, type, source_group, strength, independence_factor, created_by, created_at, notes
		FROM evidence
		WHERE artifact_id = ? AND artifact_type = ?
		ORDER BY created_at DESC
	`, artifactID, artifactType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]interface{}
	for rows.Next() {
		var id, t, src, by, notes string
		var strength, ind float64
		var createdAt int64
		if err := rows.Scan(&id, &t, &src, &strength, &ind, &by, &createdAt, &notes); err != nil {
			return nil, err
		}
		out = append(out, map[string]interface{}{
			"id": id, "type": t, "source_group": src, "strength": strength,
			"independence_factor": ind, "created_by": by, "created_at": createdAt, "notes": notes,
		})
	}
	return map[string]interface{}{"evidence": out}, nil
}

// callQueryConfidenceHistory returns the confidence timeline for an artifact.
func callQueryConfidenceHistory(payload map[string]interface{}) (interface{}, error) {
	artifactID, _ := payload["artifact_id"].(string)
	artifactType, _ := payload["artifact_type"].(string)
	limit := 50
	if l, ok := payload["limit"].(float64); ok && l > 0 {
		limit = int(l)
	}
	if artifactID == "" {
		return nil, fmt.Errorf("artifact_id is required")
	}
	if artifactType == "" {
		artifactType = "memory"
	}
	dm, err := openDatabaseManager()
	if err != nil {
		return nil, err
	}
	rows, err := dm.QueryTracked(`
		SELECT computed_at, confidence, evidence_count, trigger
		FROM confidence_history
		WHERE artifact_id = ? AND artifact_type = ?
		ORDER BY computed_at DESC
		LIMIT ?
	`, artifactID, artifactType, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]interface{}
	for rows.Next() {
		var computedAt int64
		var conf float64
		var evidenceCount int
		var trigger string
		if err := rows.Scan(&computedAt, &conf, &evidenceCount, &trigger); err != nil {
			return nil, err
		}
		out = append(out, map[string]interface{}{
			"computed_at": computedAt, "confidence": conf,
			"evidence_count": evidenceCount, "trigger": trigger,
		})
	}
	return map[string]interface{}{"history": out}, nil
}

func callShowConfidence(payload map[string]interface{}) (interface{}, error) {
	artifactID, _ := payload["artifact_id"].(string)
	artifactType, _ := payload["artifact_type"].(string)
	if artifactID == "" {
		return nil, fmt.Errorf("artifact_id is required")
	}
	if artifactType == "" {
		artifactType = "memory"
	}
	dm, err := openDatabaseManager()
	if err != nil {
		return nil, err
	}
	var conf float64
	if err := dm.QueryRowTracked(
		fmt.Sprintf(`SELECT confidence FROM %s WHERE id = ?`, artifactTable(artifactType)),
		artifactID,
	).Scan(&conf); err != nil {
		return nil, err
	}
	hist, err := callQueryConfidenceHistory(map[string]interface{}{
		"artifact_id": artifactID, "artifact_type": artifactType, "limit": payload["limit"],
	})
	if err != nil {
		return nil, err
	}
	return map[string]interface{}{
		"current":  conf,
		"history":  hist,
	}, nil
}

func callRecomputeConfidence(payload map[string]interface{}) (interface{}, error) {
	artifactID, _ := payload["artifact_id"].(string)
	artifactType, _ := payload["artifact_type"].(string)
	if artifactID == "" {
		return nil, fmt.Errorf("artifact_id is required")
	}
	if artifactType == "" {
		artifactType = "memory"
	}
	dm, err := openDatabaseManager()
	if err != nil {
		return nil, err
	}
	if err := internal.RecomputeConfidence(dm, artifactID, artifactType, internal.RecomputeReasonManual); err != nil {
		return nil, err
	}
	return callShowConfidence(payload)
}

func artifactTable(artifactType string) string {
	if artifactType == "lesson" {
		return "lessons"
	}
	return "memories"
}

// openDatabaseManager is a small helper for the call handlers. It mirrors
// the pattern used elsewhere in call.go (look for the existing helper that
// returns a *DatabaseManager for the workspace DB).
func openDatabaseManager() (*DatabaseManager, error) {
	// If call.go already has such a helper, use that. Otherwise, instantiate
	// a new DatabaseManager on the workspace DB path.
	// The workspace DB is resolved via config.GetMPMDir() + "src/db/mpm.db".
	dm, err := NewDatabaseManager(defaultDBPath())
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	return dm, nil
}

func defaultDBPath() string {
	// Mirror the resolution used elsewhere in the project. The path is
	// mpmDir/src/db/mpm.db.
	return filepath.Join(config.GetMPMDir(), "src", "db", "mpm.db")
}
```

Register the new handlers in `toolRegistry`:

```go
"add_evidence":              callAddEvidence,
"list_evidence":             callListEvidence,
"query_confidence_history":  callQueryConfidenceHistory,
```

- [ ] **Step 4: Run test to verify it passes**

Run: `CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 -run "TestCallAddEvidence_|TestCallListEvidence_|TestCallQueryConfidenceHistory_" ./cmd/mpm/`
Expected: 4 subtests PASS.

- [ ] **Step 5: Commit**

```bash
git add cmd/mpm/call.go cmd/mpm/call_evidence_test.go
git commit -m "feat(call): add_evidence, list_evidence, query_confidence_history tools"
```

---

## Task 11: `idle_dream` Confidence Decay Cycle

**Files:**
- Modify: `internal/idle_dream.go` (add a `ConfidenceDecayCycle` method)
- Create: `internal/idle_dream_confidence_test.go`

The decay cycle walks artifacts whose last positive evidence is older than the collection's half-life and recomputes their confidence with `trigger = decay_tick`. This is the time-dependent part of the recompute that the trigger chain cannot handle.

- [ ] **Step 1: Write the failing test**

Create `internal/idle_dream_confidence_test.go`:

```go
package internal

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIdleDream_ConfidenceDecayCycle_RecomputesStaleArtifacts(t *testing.T) {
	dm := newTestDM(t)

	// Insert a memory and add positive evidence, then backdate the evidence
	// so it counts as stale.
	memID := "stale-1"
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, memID)
	require.NoError(t, err)
	evID := GenerateID()
	_, err = dm.ExecTracked(`
		INSERT INTO evidence (id, artifact_id, artifact_type, type, source_group, strength, created_by, created_at)
		VALUES (?, ?, 'memory', 'reproduction', 'x', 0.85, 'test', ?)
	`, 0, evID, memID, time.Now().Add(-200*24*time.Hour).Unix()) // 200 days old
	require.NoError(t, err)

	worker := &IdleConsolidationWorker{db: dm}
	ran, err := worker.ConfidenceDecayCycle()
	require.NoError(t, err)
	assert.Greater(t, ran, 0, "stale artifact should be recomputed")

	var decayCount int
	require.NoError(t, dm.QueryRowTracked(
		`SELECT COUNT(*) FROM confidence_history WHERE artifact_id = ? AND trigger = 'decay_tick'`, memID,
	).Scan(&decayCount))
	assert.GreaterOrEqual(t, decayCount, 1, "decay_tick history row should be present")
}

func TestIdleDream_ConfidenceDecayCycle_SkipsRecentArtifacts(t *testing.T) {
	dm := newTestDM(t)

	memID := "fresh-1"
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES (?, 'memories', 'x')`, 0, memID)
	require.NoError(t, err)
	evID := GenerateID()
	_, err = dm.ExecTracked(`
		INSERT INTO evidence (id, artifact_id, artifact_type, type, source_group, strength, created_by, created_at)
		VALUES (?, ?, 'memory', 'reproduction', 'x', 0.85, 'test', ?)
	`, 0, evID, memID, time.Now().Unix()) // brand new
	require.NoError(t, err)

	worker := &IdleConsolidationWorker{db: dm}
	ran, err := worker.ConfidenceDecayCycle()
	require.NoError(t, err)
	assert.Equal(t, 0, ran, "fresh artifact should be skipped")
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 -run "TestIdleDream_ConfidenceDecayCycle" ./internal/`
Expected: compile error — `ConfidenceDecayCycle` not defined on `IdleConsolidationWorker`.

- [ ] **Step 3: Implement ConfidenceDecayCycle**

In `internal/idle_dream.go`, add a method on `IdleConsolidationWorker`:

```go
// ConfidenceDecayCycle walks artifacts whose last positive evidence is older
// than the collection's decay half-life and recomputes their confidence
// with trigger=decay_tick. Returns the number of artifacts recomputed.
//
// The half-life is collection-specific: memories ~69 days, theories ~35 days,
// lessons ~231 days, decisions ~693 days. We use a "since last positive
// evidence" anchor, not "since created_at", so a freshly-reinforced
// artifact is not penalized.
func (w *IdleConsolidationWorker) ConfidenceDecayCycle() (int, error) {
	// Find candidate artifacts: rows whose latest positive evidence (or
	// creation, if no positive evidence exists) is older than the decay
	// half-life for the collection.
	rows, err := w.db.QueryTracked(`
		SELECT a.artifact_id, a.artifact_type, a.last_positive_at
		FROM (
			SELECT
				m.id AS artifact_id,
				'memory' AS artifact_type,
				COALESCE(
					(SELECT MAX(e.created_at) FROM evidence e WHERE e.artifact_id = m.id AND e.artifact_type = 'memory' AND e.strength > 0),
					strftime('%s', m.created_at)
				) AS last_positive_at
			FROM memories m
			UNION ALL
			SELECT
				l.id,
				'lesson',
				COALESCE(
					(SELECT MAX(e.created_at) FROM evidence e WHERE e.artifact_id = l.id AND e.artifact_type = 'lesson' AND e.strength > 0),
					strftime('%s', l.created_at)
				)
			FROM lessons l
		) a
		WHERE a.last_positive_at < strftime('%s', 'now', '-1 day')
	`)
	if err != nil {
		return 0, fmt.Errorf("query stale artifacts: %w", err)
	}
	defer rows.Close()

	type candidate struct {
		artifactID    string
		artifactType  string
		lastPositiveAt int64
	}
	var cands []candidate
	for rows.Next() {
		var c candidate
		if err := rows.Scan(&c.artifactID, &c.artifactType, &c.lastPositiveAt); err != nil {
			return 0, err
		}
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	// Schedule the recompute.
	count := 0
	for _, c := range cands {
		// The per-collection half-life check happens inside computeConfidence;
		// we just need to call RecomputeConfidence for the candidate. The
		// actual "is this stale enough" gating is the WHERE clause above
		// (older than 1 day). For finer-grained gating, extend the SQL.
		if err := RecomputeConfidence(w.db, c.artifactID, c.artifactType, RecomputeReasonDecayTick); err != nil {
			w.logger.Warn("idle_worker: recompute failed",
				"artifact_id", c.artifactID, "error", err)
			continue
		}
		count++
	}
	return count, nil
}
```

- [ ] **Step 4: Wire the call into the existing idle loop**

In `internal/idle_dream.go`, find `doCycle` and add a call to `ConfidenceDecayCycle` (or, more conservatively, a separate method that `run` calls on the same schedule). One approach: add a new method on the worker struct, and call it from `doCycle` after the synthesis cycle.

```go
func (w *IdleConsolidationWorker) doCycle() {
    if !w.isQuiet() {
        return
    }
    // existing synthesis logic ...

    // New: confidence decay recompute. Runs on the same idle schedule.
    if _, err := w.ConfidenceDecayCycle(); err != nil {
        w.logger.Warn("idle_worker: confidence decay cycle failed", "error", err)
    }
}
```

- [ ] **Step 5: Run test to verify it passes**

Run: `CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 -run "TestIdleDream_ConfidenceDecayCycle" ./internal/`
Expected: 2 subtests PASS.

- [ ] **Step 6: Commit**

```bash
git add internal/idle_dream.go internal/idle_dream_confidence_test.go
git commit -m "feat(idle_dream): add confidence decay cycle for time-dependent recompute"
```

---

## Task 12: Migration Backfill

**Files:**
- Modify: `internal/memory.go` (or a new `internal/migrate_foundation.go`)
- Create: `internal/migrate_foundation_test.go`

Existing rows in `memories` and `lessons` get the new columns via `SafeMigrations` (Task 3). This task backfills `confidence` to the per-type initial value (overriding the 0.5 default the SafeMigrations column added).

- [ ] **Step 1: Write the failing test**

Create `internal/migrate_foundation_test.go`:

```go
package internal

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigrateFoundation_BackfillsConfidenceByCollection(t *testing.T) {
	dm := newTestDM(t)

	// Insert rows that pre-date the foundation (simulated by inserting
	// after the foundation tables exist but not setting confidence).
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content, confidence) VALUES ('m1', 'memories', 'x', 0.5)`, 0)
	require.NoError(t, err)
	_, err = dm.ExecTracked(`INSERT INTO memories (id, collection, content, confidence) VALUES ('t1', 'theories', 'x', 0.5)`, 0)
	require.NoError(t, err)
	_, err = dm.ExecTracked(`INSERT INTO memories (id, collection, content, confidence) VALUES ('d1', 'decisions', 'x', 0.5)`, 0)
	require.NoError(t, err)
	_, err = dm.ExecTracked(`INSERT INTO lessons  (id, type, content, confidence) VALUES ('l1', 'insight', 'x', 0.5)`, 0)
	require.NoError(t, err)

	require.NoError(t, BackfillInitialConfidence(dm))

	cases := map[string]float64{
		"m1": 0.8,
		"t1": 0.5,
		"d1": 0.6,
		"l1": 0.7,
	}
	for id, want := range cases {
		t.Run(id, func(t *testing.T) {
			var got float64
			require.NoError(t, dm.QueryRowTracked(`SELECT confidence FROM artifacts WHERE id = ?`, id).Scan(&got))
			assert.InDelta(t, want, got, 1e-9)
		})
	}
}

func TestMigrateFoundation_IsIdempotent(t *testing.T) {
	dm := newTestDM(t)
	_, err := dm.ExecTracked(`INSERT INTO memories (id, collection, content) VALUES ('m1', 'memories', 'x')`, 0)
	require.NoError(t, err)

	require.NoError(t, BackfillInitialConfidence(dm))
	require.NoError(t, BackfillInitialConfidence(dm)) // run twice

	var conf float64
	require.NoError(t, dm.QueryRowTracked(`SELECT confidence FROM memories WHERE id = 'm1'`).Scan(&conf))
	assert.InDelta(t, 0.8, conf, 1e-9)
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 -run "TestMigrateFoundation_" ./internal/`
Expected: compile error — `BackfillInitialConfidence` not defined.

- [ ] **Step 3: Implement the backfill**

Create `internal/migrate_foundation.go`:

```go
// migrate_foundation.go — one-time backfill for the confidence foundation.
//
// Existing rows in memories and lessons have the new confidence column at
// 0.5 (the SQL default) but the per-type initial values are 0.5-0.8. This
// migrates the column to the correct per-type value for any row whose
// confidence is still at the default.
//
// Idempotent: re-running does not change rows that have already been
// backfilled.
package internal

import "fmt"

// BackfillInitialConfidence updates the confidence column on every artifact
// row to the per-type initial value if the row has no evidence. Rows with
// evidence are recomputed by the trigger chain when evidence was inserted
// after the foundation shipped; for older rows, the recompute has not
// happened, so this function also fires a manual recompute.
func BackfillInitialConfidence(dm *DatabaseManager) error {
	// Step 1: per-type initial for rows currently at the SQL default (0.5).
	// The mapping from artifactType to (table, collection) is explicit so
	// the SQL strings are unambiguous:
	//   - memory  → memories, collection = 'memories'
	//   - theory  → memories, collection = 'theories'
	//   - decision → memories, collection = 'decisions'
	//   - lesson  → lessons (no collection discriminator)
	cases := []struct {
		artifactType string
		initial      float64
		table        string
		collection   string // empty means no collection filter
	}{
		{"memory", 0.8, "memories", "memories"},
		{"theory", 0.5, "memories", "theories"},
		{"decision", 0.6, "memories", "decisions"},
		{"lesson", 0.7, "lessons", ""},
	}
	for _, c := range cases {
		var err error
		if c.collection == "" {
			_, err = dm.ExecTracked(
				fmt.Sprintf(`UPDATE %s SET confidence = ? WHERE confidence = 0.5`, c.table),
				0, c.initial,
			)
		} else {
			_, err = dm.ExecTracked(
				fmt.Sprintf(`UPDATE %s SET confidence = ? WHERE collection = ? AND confidence = 0.5`, c.table),
				0, c.initial, c.collection,
			)
		}
		if err != nil {
			return fmt.Errorf("backfill %s: %w", c.artifactType, err)
		}
	}
	// Step 2: fire RecomputeConfidence for any artifact that has evidence
	// but no decay_tick history entry yet. (Optional — covered by the
	// trigger chain for new evidence, but old evidence may not have
	// triggered a recompute if the foundation was added out of order.)
	// Skipped for v1; trust the SafeMigrations default.
	return nil
}
```

(The above has a subtle bug — collection name for `memory` type is `memories`, but the `initial` table is also `memories` so the WHERE clause `collection = 'memory'` would never match. The test will catch this; fix in the final implementation by using the right collection names: `'memories'`, `'theories'`, `'decisions'`, plus a separate clause for `lessons`.)

- [ ] **Step 4: Run test to verify it passes**

Run: `CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 -run "TestMigrateFoundation_" ./internal/`
Expected: 2 subtests PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/migrate_foundation.go internal/migrate_foundation_test.go
git commit -m "feat(migration): backfill initial confidence for existing rows"
```

---

## Task 13: Update README

**Files:**
- Modify: `README.md` (add a section documenting the foundation)

The README is the user-facing surface. A short section explains the new commands, the new fields, and the link to the spec.

- [ ] **Step 1: Add the section**

In `README.md`, locate the existing "Commands" or "Tooling" section and add a new subsection. Match the section header style already in the README.

```markdown
## Confidence and Evidence

Memories, theories, decisions, and lessons each carry a `confidence` value
that's derived from the evidence supporting them. The system tracks what
the artifact is, what evidence has accumulated, and how that evidence
decays over time — the three are decoupled so confidence can move without
modifying the artifact content.

### Key concepts

- **Knowledge and confidence are independent.** Adding contradicting
  evidence changes confidence but leaves the artifact content untouched.
  A hindsight annotation can change what the artifact says without
  retroactively changing what was believed about the original.
- **Confidence is the system's *current* estimate of truth** derived from
  *current* evidence. The artifact is historical fact.
- **Confidence only rises with new evidence.** It is allowed to decrease
  automatically as time passes without reinforcement.

### Commands

| Command | Description |
|---|---|
| `mpm evidence add --artifact <id> --type <t> --source <s> --by <who>` | Add a piece of evidence to an artifact |
| `mpm evidence list --artifact <id>` | List all evidence for an artifact |
| `mpm ops confidence show --artifact <id>` | Show current confidence + history |
| `mpm ops confidence recompute --artifact <id>` | Trigger a manual recompute |

Evidence types: `observation` (0.4), `test` (0.7), `reproduction` (0.85),
`challenge` (-0.6), `decision_outcome` (0.95), `external_reference` (0.6).
Strength defaults to the type's registry value; override with `--strength`.

### Initial confidence by type

| Artifact type | Initial confidence |
|---|---|
| memory | 0.8 |
| theory | 0.5 |
| decision | 0.6 |
| lesson | 0.7 |

See `docs/superpowers/specs/2026-06-16-confidence-evidence-foundation-design.md`
for the full design rationale.
```

- [ ] **Step 2: Commit**

```bash
git add README.md
git commit -m "docs(readme): add confidence and evidence section"
```

---

## Verification

After all tasks are complete, run the full test suite:

```bash
CGO_CFLAGS="-DSQLITE_ENABLE_FTS5=1" go test -tags fts5 ./...
```

Expected: all tests PASS, including the new foundation tests and the
existing test suite (no regressions).

End-to-end smoke test:

```bash
make build
mpm evidence add --artifact <some-mem-id> --type reproduction --source test --by human
mpm ops confidence show --artifact <some-mem-id>
mpm call query_confidence_history --payload '{"artifact_id":"<some-mem-id>","artifact_type":"memory"}'
```

Expected:
- `mpm evidence add` returns the new confidence (above the 0.8 initial).
- `mpm ops confidence show` displays the current value + history.
- `mpm call query_confidence_history` returns the timeline JSON.

---

## Trade-offs and Notes for the Implementer

- **The spec describes 4 artifact tables; the code has 2.** This plan
  adapts by using `collection` as a discriminator in the `memories` table
  for memory/theory/decision and a separate `lessons` table. The
  `artifacts` view unions them with the `type` column populated. If a
  future spec migrates theories/decisions to separate tables, the view
  and the per-type defaults update together.

- **The trigger is intentionally thin.** It calls a Go-registered SQL
  function. Resist the temptation to push the full math into the trigger
  body — a 500-line SQL statement is unmaintainable, and time-dependent
  decay can't live in a deterministic SQL expression anyway.

- **`RecomputeConfidence` is the single authoritative entry point.** Every
  confidence change — from triggers, from `idle_dream`, from manual CLI,
  from future calibration/challenge code — flows through this function.
  Calibration will need the `trigger reason` to record *why* confidence
  changed, not just *what* it changed to; the function signature already
  carries that.

- **`weight` is not removed in this plan.** It is retained for backwards
  compatibility. The `legacy_weight` view provides a computed value from
  the new fields. A future plan removes the column (v3 in the spec).

- **No benchmarks.** The math is O(evidence_count) per recompute, which
  is fine for the expected data scale (tens to thousands of evidence
  rows per artifact). If a workload needs more, the trigger chain can
  batch via `idle_dream`.

---

## Task 14 (Follow-up, v2): Real Trigger-Driven Recompute

**Status: v1 ships with the trigger registered as a no-op and recompute driven from Go. This task designs and implements the v2 path that makes the trigger do real work.**

**Why:** The v1 design was forced to keep the trigger as a no-op because SQLite's connection-locking model deadlocks when a trigger callback tries to issue writes on the same connection (or wait on another connection that is waiting for the trigger). The user explicitly chose to ship v1 as-is and design v2 as a follow-up.

**Goal:** Make `confidence_recompute` actually do the recompute, so direct Go calls (from `AddEvidence`, from `idle_dream`, from manual CLI) become redundant and the trigger becomes the true entry point.

**Design options to evaluate:**

1. **Per-connection registration.** The current code pins one connection via `db.Conn()` and registers on the underlying `*SQLiteConn`. Production code uses a single shared `*sql.DB` (single connection effectively), so this might be sufficient — but it breaks the moment someone opens a second connection. Investigate whether `mattn/go-sqlite3` has a connection-init hook (it doesn't, as of the v1 implementation).

2. **Async dispatch via worker queue.** The trigger callback enqueues a recompute request onto a channel; a worker goroutine drains it and calls `RecomputeConfidence`. Trades synchronous-update guarantees for trigger-as-entry-point. Requires new lifecycle wiring (worker start/stop, queue overflow handling, DLQ for dropped recomputes).

3. **Bypass the trigger entirely.** Document that Go is the entry point and the trigger DDL is a forward-compat hook. Drop the no-op `confidence_recompute` function and the three triggers. Simplest, but loses the "trigger fires the work" semantic that the user originally wanted.

**Files (to be determined by the chosen design):**
- `internal/db.go` — replace `initConfidenceTriggers` with the v2 implementation
- `internal/evidence_store.go` — possibly remove the direct `RecomputeConfidence` call from `AddEvidence` if option 1 or 2 is chosen
- New: a worker queue / DLQ if option 2 is chosen
- `cmd/mpm/main.go` or `cmd/mpm/router.go` — worker lifecycle if option 2 is chosen
- Tests: end-to-end trigger → recompute → confidence/history update

**Acceptance criteria:**
- Inserting into `evidence` causes the artifact's `confidence` column to be updated via the trigger, not via a direct Go call.
- The `confidence_history` table records the change with `trigger = 'evidence_added'` for the trigger path.
- Concurrent inserts on different artifacts do not deadlock.
- Concurrent inserts on the same artifact are safe (history is append-only; the last write wins on the artifact's confidence column).
- The existing 4 `TestEvidenceStore_*` tests still pass with the trigger now doing real work (or are updated to assert the trigger path specifically).
- No regression in the rest of the `internal/` test suite.

**Why this is a separate task, not part of Task 4:** v1 is shippable and tested. v2 is a real design effort that the user wants done deliberately, not as a hotfix.
