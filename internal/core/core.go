// core.go — CoreDB interface: the contract between Runtime and Core.
//
// All Runtime code (cmd/mpm, internal/core/tools) depends on this interface
// rather than *DatabaseManager. DatabaseManager satisfies the interface.
//
// Phase 2 of the Architecture Split (2026-07-07):
// extract the interface, switch consumers, keep the concrete type internal.

package internal

import (
	"context"
	"database/sql"
	"time"

	"github.com/flowbyte-com/mpm-core/seed"
)

// DecisionFilter narrows the result set for ListDecisions. The zero value
// returns all active (non-superseded, non-invalidated) decisions up to the
// default page size. alpha-4 D-005.
type DecisionFilter struct {
	Status string   // "active" (default), "all", "superseded", "invalidated"
	Tags   []string // empty = no tag filter
	Limit  int      // 0 means default (50)
}

// TheoryFilter narrows the result set for ListTheories. The zero value
// returns all pending (un-resolved) theories up to the default page size.
// alpha-4 audit D-006 (theory MCP read surface).
type TheoryFilter struct {
	Status string   // "pending" (default), "all", "proven", "disproven", "resolved"
	Tags   []string // empty = no tag filter
	Limit  int      // 0 means default (50)
}

// CoreDB is the persistent-storage interface that the Agent Runtime consumes.
// DatabaseManager implements every method.
type CoreDB interface {
	// ─── Lifecycle & Connection ──────────────────────────────────────
	Close() error
	SQLDB() *sql.DB
	DBPath() string
	IsOpen() bool
	BusyRetryCount() uint64
	HealthCheck() (map[string]interface{}, error)
	InitSchema() error
	ExecTracked(query string, retries int, args ...interface{}) (sql.Result, error)
	QueryTracked(query string, args ...interface{}) (*sql.Rows, error)
	QueryRowTracked(query string, args ...interface{}) *sql.Row
	WithTx(fn func(DBNode) error) error
	WatchdogPath() string
	SharedAttached() string
	RecentWatchdogOps(n int, opPrefix string) ([]WatchdogOp, error)
	NewSession() (CoreDB, error)

	// ─── Cascade Materializer ─────────────────────────────────────────
	MaterializeCascadeIntents(ctx context.Context, limit int) (MaterializationReport, error)

	// ─── Memory CRUD ─────────────────────────────────────────────────
	SaveMemory(collection, content, sessionID string, tags []string, metadata map[string]interface{}, embedding []float32, isLongTerm bool, weight float64, expiresAt ...time.Time) (string, error)
	SaveMemoryWithExtras(collection, content, sessionID string, tags []string, metadata map[string]interface{}, embedding []float32, isLongTerm bool, weight float64, referenceID, retrievalPriority, importance, createdAt string, expiresAt ...time.Time) (string, error)
	GetMemory(id string) (map[string]interface{}, error)
	GetMemoryByExternalID(sourceDB, sourceID string) (map[string]interface{}, error)
	UpdateMemory(id, content string, tags map[string]interface{}, metadata map[string]interface{}) error
	UpdateMemoryMetadata(id string, patchJSON string) error
	ShredMemory(id string) error
	ShredMemoryWithCascade(memoryID string) (map[string]interface{}, error)
	SaveMemoryWithContext(fact, collection string, tags []string, weight float64, ttl string, ac ActiveContext) (map[string]interface{}, *Memory, error)
	SaveMemoryWithContextAndSnapshot(fact, collection string, tags []string, weight float64, ttl string, ac ActiveContext, wc *WrapperContext) (map[string]interface{}, *Memory, error)
	SaveMemoryNode(node DBNode, collection, content, sessionID string, tags []string, metadata map[string]interface{}, embedding []float32, isLongTerm bool, weight float64, referenceID, retrievalPriority, importance, createdAt string, expiresAt ...time.Time) (string, error)

	// ─── Memory Search & Query ───────────────────────────────────────
	QueryMemories(collection string, primeOnly bool, limit, offset int) ([]map[string]interface{}, error)
	SearchMemories(q, collection string, primeOnly bool, limit, offset int) ([]map[string]interface{}, error)
	HybridSearchMemories(query, collection string, limit int, scope string) ([]map[string]interface{}, error)
	GetMemoryStats() (map[string]interface{}, error)
	GetMemoriesForExport(collection, since, until string) ([]map[string]interface{}, error)
	GetNegativeWeightMemories() ([]map[string]interface{}, error)
	GetProvenTheoryForMemory(memoryID string) (map[string]interface{}, error)
	GetMemoryRevisions(memoryID string) ([]MemoryRevision, error)
	GetMemoryRevisionAtTime(memoryID string, asOf time.Time) (*MemoryRevision, error)
	GetMemoriesByRelevance(collection string, limit int) ([]map[string]interface{}, error)
	GetContextualMemories(contextTags []string, sessionContext string, limit int) ([]map[string]interface{}, error)
	VectorSearch(tier string, queryEmbedding []float32, limit int) ([]map[string]interface{}, error)
	GetSpacedReinforcementReview(daysSinceAccess, limit int) ([]map[string]interface{}, error)

	// ─── Memory Mutations ────────────────────────────────────────────
	ReinforceMemory(id string, delta int) error
	ReinforceMemoryTool(memoryID string, delta int) (map[string]interface{}, error)
	WeakenMemory(id string, delta int) error
	WeakenMemoryTool(memoryID string, delta int) (map[string]interface{}, error)
	AdjustMemoryWeight(id string, delta int) error
	SetMemoryWeight(memoryID string, weight float64) (map[string]interface{}, error)
	SoftDeleteMemory(memoryID string) (map[string]interface{}, error)
	RestoreMemory(memoryID string) (map[string]interface{}, error)
	ChallengeMemory(memoryID string, slashAmount int, evidence string) error
	ChallengeMemoryAsync(memoryID string, evidence string)
	ChallengeAndReinforce(id string, delta int) error
	SetMemoryTTL(id string, expiresAt time.Time) error
	SnoozeMemory(memoryID string, days int) (map[string]interface{}, error)
	PromoteMemory(memoryID string) (map[string]interface{}, error)
	PatchMemoryMetadata(memoryID string, patchJSON string) (map[string]interface{}, error)
	SynthesizeMemoryFor(ctx context.Context, memoryID string) (map[string]interface{}, error)
	CompactEpistemology(ctx context.Context, force bool) (*CompactEpistemologyResult, error)
	CompactEpistemologyDrain(ctx context.Context, force bool, maxBatches int) (*CompactEpistemologyDrainResult, error)
	PruneExpired() (int, error)
	PruneOlderThan(beforeUnixSec int64) (int, error)
	PruneNeverAccessed() (int, error)
	DecayWeights(policies map[string]DecayPolicy, intervalDays int) (int, error)
	RunSelfMaintenance() (map[string]interface{}, error)

	// ─── Topics ──────────────────────────────────────────────────────
	CreateTopic(name, description, fromDate, toDate string) (string, error)
	GetOrCreateTopic(name string) (string, error)
	GetTopicByName(name string) (string, error)
	GetTopic(id string) (map[string]interface{}, error)
	ListTopics() ([]map[string]interface{}, error)
	SearchTopics(q string, limit int) ([]map[string]interface{}, error)
	SearchTopicsByQuery(query string, limit int) ([]map[string]interface{}, error)
	GetTopicMemories(topicID string) ([]map[string]interface{}, error)
	GetTopicTopMemories(topicID string, limit int) ([]MemoryRef, int, error)
	AddMemoryToTopic(memoryID, topicID, role string) error
	RemoveMemoryFromTopic(memoryID, topicID string) (bool, error)
	DeleteTopic(topicID string) error
	CreateTopicWithDescription(name, description string) (string, error)
	GetMemoryTopics(memoryID string) ([]TopicRef, error)
	GetRecentUserTopics(limit int) ([]string, error)

	// ─── Lessons ─────────────────────────────────────────────────────
	AddLesson(content string, lessonType LessonType, tags []string, sourceSessionID string) (*Lesson, error)
	GetLesson(id string) (*Lesson, error)
	ListLessons(lessonType string) ([]*Lesson, error)
	SearchLessons(query string, limit int) ([]*Lesson, error)
	DeleteLesson(id string) error
	RestoreLesson(id string) error
	ShredLesson(id string) error
	GetLessonStats() (map[string]interface{}, error)
	SaveLesson(fact, lessonType string, tags []string) (map[string]interface{}, *Lesson, error)
	SearchLessonsLimited(query string) ([]map[string]interface{}, error)
	ListLessonsFiltered(lessonType string) ([]map[string]interface{}, error)

	// ─── Sessions ────────────────────────────────────────────────────
	GetLastSession() (map[string]interface{}, error)
	GetSessionMemories(sessionID string, limit int) ([]map[string]interface{}, error)
	UpdateSessionSummary(sessionID, summary string) error
	GetRecentInteractions(limit int) ([]map[string]interface{}, error)

	// ─── System Config ───────────────────────────────────────────────
	SaveSystemConfig(key, rawJSON, contentHash string, snapshotJSON string) (bool, error)
	GetSystemConfig(key string) (map[string]interface{}, error)
	DeleteSystemConfig(key string) error
	GetAllSystemConfigs() ([]map[string]interface{}, error)

	// ─── Config Helpers ────────────────────────────────────────────
	// GetConfigInt reads an integer config from system_config with env fallback.
	GetConfigInt(key string, defaultValue int) int
	GetConfigInt64(key string, defaultValue int64) int64
	GetConfigFloat64(key string, defaultValue float64) float64
	GetConfigString(key string, defaultValue string) string

	// ─── References ──────────────────────────────────────────────────
	AddReference(doc *ReferenceDoc, chunks []ReferenceChunk) error
	DeleteReference(id string) error
	GetReference(refID string) (map[string]interface{}, error)
	ListReferences(limit, offset int) ([]map[string]interface{}, error)
	SearchReferences(q string, limit int) ([]map[string]interface{}, error)
	SearchReferenceChunks(q string, limit int) ([]map[string]interface{}, error)
	AddReferenceFromFile(filepath, title string) (map[string]interface{}, error)
	AddReferenceFromFileWith(filepath, title string, tags []string, reason string, chunkSize int) (map[string]interface{}, error)
	EmbedReferenceChunks(ctx context.Context, docID string) (embedded int, failed int, err error)
	GetReferenceDoc(docID string) (*ReferenceDocRef, error)
	GetInteractionsForDoc(docID string, limit int) ([]map[string]interface{}, error)
	GetMostUsedReferences(limit int) ([]map[string]interface{}, error)

	// ─── Changelog ──────────────────────────────────────────────
	LogChangelogEntry(fact, commitHash string, extraTags []string) (string, error)
	LogChangelogEntryWithConfirmations(fact, commitHash string, extraTags []string, confirmations []ConfirmationSpec) (string, error)
	LogChangelogEntryWithAssertions(fact, commitHash string, extraTags []string, confirmations []ConfirmationSpec, contradictions []ContradictionSpec) (string, error)

	// ─── Cluster Proposals ───────────────────────────────────────────
	ActiveClusters() (known, unknown []ClusterProposal, err error)
	SetClusterStatus(clusterKey, status, snoozeUntil, reason string) error
	AnnotateCluster(clusterKey, annotation, reason string) error

	// ─── Evidence & Confidence ───────────────────────────────────────
	AddEvidence(in EvidenceInput) (map[string]interface{}, error)
	ListEvidence(artifactID, artifactType string) (map[string]interface{}, error)
	QueryConfidenceHistory(artifactID, artifactType string, limit int) (map[string]interface{}, error)
	QueryConfidenceChanges(filter ConfidenceChangesFilter) (map[string]interface{}, error)
	QueryConfidenceTrend(artifactID, artifactType string, windowDays int) (map[string]interface{}, error)
	QueryMemoryQuality() (map[string]interface{}, error)
	ShowConfidence(artifactID, artifactType string) (map[string]interface{}, error)
	RecomputeConfidence(artifactID, artifactType string) (map[string]interface{}, error)
	ExplainConfidence(artifactID, artifactType string) (map[string]interface{}, error)

	// ─── Audit ───────────────────────────────────────────────────────
	LogAudit(level AuditLevel, component, message, stack string, ctx AuditContext)
	QueryAuditLog(level AuditLevel, component, artifactID string, days, limit int, includeStack bool) ([]map[string]interface{}, error)
	AuditSummary() string
	PruneAuditLog(retentionDays int) (int64, error)

	// ─── Ingest ──────────────────────────────────────────────────────
	IngestOpenClaw(sourcePath string, batchSize int, importBatch string, dryRun bool) (*IngestStats, error)
	IngestFromAdapter(dbPath string, adapter SchemaAdapter, batchSize int, importBatch string, dryRun bool) (*IngestStats, error)
	IngestFromMarkdownFile(sourcePath, importBatch string, dryRun bool) (*MigrateStats, error)
	IngestFromJsonFile(sourcePath, importBatch string, dryRun bool) (*MigrateStats, error)
	PromoteRawMemoryBatch(importBatch string, dryRun bool) (int, error)
	GetRawMemoriesByStatus(status string, limit int) ([]*RawMemory, error)
	ResetStaleReviewing() (int, error)
	GetIngestStatus() (map[string]int, error)
	ListIngestBatches() ([]map[string]interface{}, error)
	GetExternalDBCursor(label string) (string, error)

	// ─── Epistemology ────────────────────────────────────────────────
	ProposeTheory(hypothesis, validationCriteria string, dependencies []string, sourceIDs []string, tags []string) (map[string]interface{}, error)
	ProposeTheoryWithExtras(hypothesis, validationCriteria string, dependencies []string, sourceIDs []string, tags []string, cascadeFields map[string]interface{}) (map[string]interface{}, error)
	ResolveTheory(theoryID, conclusion, newStatus string) (map[string]interface{}, error)
	ResolveArbitrationTheory(theoryID, winnerID, conclusion string) (map[string]interface{}, error)
	ChallengeMemoryWithTheory(memoryID, evidence string) (map[string]interface{}, error)
	// Theory read symmetry (alpha-4 audit D-006): expose read paths that
	// mirror the existing decision read surface. The CLI has always been
	// able to `mpm theories` list, but the `mpm call mpm_theories`
	// machine surface only had propose/resolve — agents reading via MCP
	// could not enumerate or look up theories. contract mirrors
	// GetDecision / ListDecisions / QueryDecisions.
	GetTheory(id string) (map[string]interface{}, error)
	ListTheories(filter TheoryFilter) ([]map[string]interface{}, error)
	QueryTheories(query string, limit int) ([]map[string]interface{}, error)
	// RestoreMemoryFromChallenge resolves the challenged-theory record
	// against memoryID and clears the memory's challenged status.
	// F7-1 surface parity: previously CLI-only; now reachable through
	// the canonical agent path (mpm_memory.challenge action).
	RestoreMemoryFromChallenge(memoryID string) (map[string]interface{}, error)
	RecordDecision(contextText, choice, rationale, outcome string, tags []string, sourceIDs []string, ac ActiveContext) (map[string]interface{}, error)
	// SupersedeDecision records a replacement decision and marks the
	// original superseded (F9). InvalidateDecision retires a decision
	// without a replacement.
	SupersedeDecision(originalID, contextText, choice, rationale, outcome string, tags []string, sourceIDs []string, ac ActiveContext) (map[string]interface{}, error)
	InvalidateDecision(decisionID, reason string) (map[string]interface{}, error)
	// Decision read symmetry (alpha-4 D-005): expose read paths that
	// mirror the write surface so an agent that wrote a decision can
	// retrieve, list, or query it without dropping to SQL. All return
	// generic map[string]interface{} rows so they round-trip through
	// the existing tool/CLI JSON marshallers without struct-tag coupling.
	GetDecision(id string) (map[string]interface{}, error)
	ListDecisions(filter DecisionFilter) ([]map[string]interface{}, error)
	QueryDecisions(query string, limit int) ([]map[string]interface{}, error)
	ReviewMemories(daysSinceAccess, limit int) (map[string]interface{}, error)

	// ─── Cascade provenance (Task 2) ──────────────────────────────────
	// Typed citation log so the cascade materializer can walk the
	// dependency graph for a dead source without having to scan
	// retrieval_metadata (which is observability-only and not
	// type-filtered). Empty sourceType on RecordProvenance triggers
	// resolution against the local memories/lessons tables so legacy
	// untyped IDs land with the correct type column. polarity is the
	// explicit opt-in for the positive-direction cascade feature —
	// empty string means "no polarity", which is the safe default
	// (NULL storage, discovery-skipped).
	RecordProvenance(sourceID, sourceType, downstreamID, downstreamType, eventID, polarity string) error
	ListDownstreamCitations(sourceID string, allowedTypes []string) ([]ProvenanceCitation, error)

	// ─── Cascade outbox (Task 3) ──────────────────────────────────────
	// Transactional capture of invalidation events and per-target
	// cascade intents. The outbox dedupes on (dead, downstream,
	// event) so a noisy recall turn cannot produce redundant intents
	// for the same invalidation. ListPendingCascadeIntents is the
	// materializer's working-set read; only status='pending' rows
	// surface so already-handled intents are not re-claimed.
	CreateInvalidationEvent(tx *sql.Tx, deadArtifactID, deadArtifactType, triggerEvidenceID, reason string, depth int) (string, error)
	EnqueueCascadeIntents(tx *sql.Tx, event CascadeInvalidation, targets []ProvenanceTarget) (int, error)
	ListPendingCascadeIntents(limit int) ([]CascadeIntent, error)

	// ─── Cascade invalidation hook (Task 4) ──────────────────────────
	// Transaction-aware helper that captures the evidence snapshot,
	// mints the event, discovers downstream targets, and enqueues
	// intents — all inside the supplied *sql.Tx so the root mutation
	// (theory disprove, memory shred, confidence cross) and the
	// cascade intents commit atomically. The three explicit
	// invalidation paths in Task 4 route through this single
	// integration point so a partial failure rolls back the root
	// mutation rather than diverging.
	EnqueueCascadeInvalidation(tx *sql.Tx, deadArtifactID, deadArtifactType, reason, triggerEvidenceID string, depth int) (int, error)

	// ─── Handoffs ────────────────────────────────────────────────────
	EndSession(sessionID, summary, endedState string, commitments, openQuestions []string) (*Handoff, error)
	GetLatestHandoff() (*Handoff, error)
	GetLatestUnreadHandoff() (*Handoff, error)
	GetHandoffByID(id string) (*Handoff, error)
	GetHandoffBySessionID(sessionID string) (*Handoff, error)
	MarkHandoffRead(id, readBy string) error
	MarkLatestHandoffRead(readBy string) (*Handoff, error)
	ListHandoffs(limit int, unreadOnly bool) ([]*Handoff, error)
	PruneHandoffs(retentionDays int) (int64, error)
	DeleteHandoff(id string) (int64, error)

	// ─── Cascade Outbox ──────────────────────────────────────────────
	PruneCascadeOutbox(retentionDays int) (int64, error)

	// ─── Wakes ───────────────────────────────────────────────────────
	ScheduleWake(reason, targetTime, theoryID, recurringRule, createdBy string, metadata map[string]interface{}) (map[string]interface{}, error)
	CheckPendingWakes(now time.Time, kinds []string) ([]map[string]interface{}, error)
	ListScheduledWakes(includeFired, overdueOnly bool, limit int) ([]map[string]interface{}, error)
	DigestScheduledWakes(topN int) (map[string]interface{}, error)
	FireStaleFoundationWakes(deletedArtifactID string) (int, error)

	// ─── Scheduled Tasks (Agentic Cron) ─────────────────────────────
	// Recurring agentic workflows. The mpm-scheduler daemon's 60s tick
	// loop polls these via ProcessScheduledTasks, injects a standard
	// scheduled_wakes row at each fire, and rolls over next_run_at.
	UpsertScheduledTask(task ScheduledTask) error
	ListScheduledTasks() ([]ScheduledTask, error)
	DeleteScheduledTask(id string) error
	// SeedBaselineScheduledTasks is the public wrapper around the
	// Baseline Cognitive Bootstrap for scheduled tasks (registry in
	// internal/core/seed/scheduled_tasks.go, apply logic in db.go).
	// Idempotent: existing rows with the canonical stable id are
	// preserved verbatim; only the canonical rows that are absent
	// are inserted. Called by `mpm ops init tasks` and by
	// NewDatabaseManager at every production boot.
	SeedBaselineScheduledTasks() (seed.SeedTaskSummary, error)

	// ─── Arc 2: Active Dissemination ────────────────────────────────
	BroadcastMemory(memoryID string, opts BroadcastOpts) (*BroadcastReport, error)
	CheckPendingEventWakes(sessionID string) ([]EventWake, error)
	Heartbeat(sessionID, agentID, hostname string, metadata map[string]interface{}) error
	DiscoverActiveSessions() ([]ActiveSession, error)

	// ─── Directives ──────────────────────────────────────────────────
	ReadDirectives() ([]map[string]interface{}, error)
	ReadDirectivesForFramework(fw string) ([]map[string]interface{}, error)
	ProactiveRecallHint(conversationText string, maxHints int, minScore float64) ([]map[string]interface{}, error)

	// ─── GC ──────────────────────────────────────────────────────────
	RunGC(opts GCOptions) (*GCRunResult, error)

	// ─── Wake Context ────────────────────────────────────────────────
	GatherWakeContext() (WakeContextData, error)
	// GatherWakeContextReadOnly assembles wake context without consuming
	// the unread handoff — for presentation-only callers.
	GatherWakeContextReadOnly() (WakeContextData, error)
	ReadWakeContext() (string, error)
	ScratchpadOrphansSummary() (string, error)

	// ─── Global / Shared ─────────────────────────────────────────────
	QueryGlobalRules(query string, limit int, includeRetired bool) ([]map[string]interface{}, error)
	RecordGlobalRule(content string, tags []string, weight float64, provenance string) (string, error)
	RetireGlobalRule(ruleID, reason string, confirm bool) (map[string]interface{}, error)
	PromoteToGlobal(localID string) (string, error)

	// ─── Admission ───────────────────────────────────────────────────
	FindAdmissionCandidates(limit int) ([]*AdmissionCandidate, error)
	RecordAdmissionOutcome(candidate *AdmissionCandidate, result *AdmitResult, admissionModel string) error

	// ─── Misc ────────────────────────────────────────────────────────
	WipeRecord(tier, id string) error

	// ─── Skills (procedural memory) ─────────────────────────────────
	ReadSkill(nameOrID, version string) (*Skill, error)
	ListSkills(scope string) ([]SkillSummary, error)
	SaveSkill(name, version, content, authorAgent string, force bool) (string, error)
	PromoteSkillToGlobal(skillID string, confirm bool) error
	ShredSkill(skillID string) error

	// ─── Retrieval metadata (Observability Layer, 2026-07-26) ─────
	// Fire-and-forget telemetry for adaptive retrieval. Called from
	// MCP read handlers and wake_context after a successful retrieval.
	// Implementations must be cheap (single-row UPSERT) and must not
	// fail the user-facing path.
	RecordRetrieval(nodeID, nodeType string) error
	RecordRetrievalSuccess(nodeID, nodeType string) error
	IncrementSuccess(nodeID, nodeType string) error
	GetRetrievalMetadata(nodeID string) (RetrievalMetadata, error)

	// ─── Work (Durable Intended Actions) ──────────────────────────
	AddWork(title, content, sessionID string) (*Work, error)
	GetWork(id string) (*Work, error)
	ListWorks() ([]*Work, error)
	ListAllWorks() ([]*Work, error)
	ListWorksByStatus(status string) ([]*Work, error)
	// ResolveFrameworkModelForInvocations batch-loads framework/model
	// provenance for work history display (F16).
	ResolveFrameworkModelForInvocations(workID string, invocationIDs []string) map[string]WorkFrameworkModel
	UpdateWork(id string, status WorkStatus) (*Work, error)
	CompleteWork(id string) (*Work, error)
	CancelWork(id string) (*Work, error)
	// Thin-handler delegation targets (event-sourced, atomic projection)
	CreateWorkWithContext(title, content, sessionID string, ac ActiveContext) (*Work, error)
	CompleteWorkWithContext(workID, note string, ac ActiveContext) (*Work, error)
	CancelWorkWithContext(workID, note string, ac ActiveContext) (*Work, error)
	AddWorkNoteWithContext(workID, note string, ac ActiveContext) (*WorkEvent, error)
	ReopenWorkWithContext(workID string, ac ActiveContext) (*Work, error)
	UpdateWorkWithContext(workID, title, content, statusStr string, ac ActiveContext) (*Work, error)
	GetActiveDirectiveIDs(framework string) []string
	RecordGitEvidenceForWork(workID string)
	DeriveWorkVerification(workID string) (WorkVerification, error)
	// ResolveWorkContradiction is the F6-1 / T20-1 agent-facing recovery
	// path: withdraw unsubstantiated dispute evidence from a work item so
	// the verification state can re-derive to its true value. Evidence
	// rows are neutralized (expires_at set) but never deleted; the audit
	// trail remains intact.
	ResolveWorkContradiction(workID, reason string) error

	// ─── Work Events (Event-Sourced History) ────────────────────
	AppendWorkEvent(workID string, event WorkEvent, ep *EffectiveProvenance, node DBNode) (*WorkEvent, error)
	GetWorkEvents(workID string) ([]*WorkEvent, error)
	GetLatestWorkEvent(workID string) (*WorkEvent, error)
	RecomputeWorkProjection(workID string) error
}

// Compile-time assertion that *DatabaseManager satisfies CoreDB.
var _ CoreDB = (*DatabaseManager)(nil)
