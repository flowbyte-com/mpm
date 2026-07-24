// core.go — CoreDB interface: the contract between Runtime and Core.
//
// All Runtime code (cmd/mpm, internal/core/tools) depends on this interface
// rather than *DatabaseManager. DatabaseManager satisfies the interface.
//
// Phase 2 of the Architecture Split proposal (docs/ARCHITECTURE_SPLIT.md):
// extract the interface, switch consumers, keep the concrete type internal.

package internal

import (
	"context"
	"database/sql"
	"time"
)

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

	// ─── Memory CRUD ─────────────────────────────────────────────────
	SaveMemory(collection, content, sessionID string, tags []string, metadata map[string]interface{}, embedding []float32, isLongTerm bool, weight int, expiresAt ...time.Time) (string, error)
	SaveMemoryWithExtras(collection, content, sessionID string, tags []string, metadata map[string]interface{}, embedding []float32, isLongTerm bool, weight int, referenceID, retrievalPriority, importance, createdAt string, expiresAt ...time.Time) (string, error)
	GetMemory(id string) (map[string]interface{}, error)
	GetMemoryByExternalID(sourceDB, sourceID string) (map[string]interface{}, error)
	UpdateMemory(id, content string, tags map[string]interface{}, metadata map[string]interface{}) error
	UpdateMemoryMetadata(id string, patchJSON string) error
	ShredMemory(id string) error
	ShredMemoryWithCascade(memoryID string) (map[string]interface{}, error)
	SaveMemoryWithContext(fact, collection string, tags []string, weight float64, ttl string, ac ActiveContext) (map[string]interface{}, *Memory, error)
	SaveMemoryNode(node DBNode, collection, content, sessionID string, tags []string, metadata map[string]interface{}, embedding []float32, isLongTerm bool, weight int, referenceID, retrievalPriority, importance, createdAt string, expiresAt ...time.Time) (string, error)

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
	SetMemoryWeight(memoryID string, weight int) (map[string]interface{}, error)
	ChallengeMemory(memoryID string, slashAmount int, evidence string) error
	ChallengeMemoryAsync(memoryID string, evidence string)
	ChallengeAndReinforce(id string, delta int) error
	SetMemoryTTL(id string, expiresAt time.Time) error
	SnoozeMemory(memoryID string, days int) (map[string]interface{}, error)
	PromoteMemory(memoryID string) (map[string]interface{}, error)
	PatchMemoryMetadata(memoryID string, patchJSON string) (map[string]interface{}, error)
	SynthesizeMemoryFor(ctx context.Context, memoryID string) (map[string]interface{}, error)
	PruneExpired() (int, error)
	PruneOlderThan(before time.Time) (int, error)
	PruneNeverAccessed() (int, error)
	DecayWeights(policies map[string]DecayPolicy, intervalDays int) (int, error)
	ArchiveStaleMemories(archiveDays int) (int, error)
	RunLifecycleDecayAndArchival(decayRate float64, archiveDays int) error
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
	RemoveMemoryFromTopic(memoryID, topicID string) error
	DeleteTopic(topicID string) error
	CreateTopicWithDescription(name, description string) (string, error)
	GetMemoryTopics(memoryID string) ([]TopicRef, error)
	GetRecentUserTopics(limit int) []string

	// ─── Lessons ─────────────────────────────────────────────────────
	AddLesson(content string, lessonType LessonType, tags []string, sourceSessionID string) (*Lesson, error)
	GetLesson(id string) (*Lesson, error)
	ListLessons(lessonType string) ([]*Lesson, error)
	SearchLessons(query string, limit int) ([]*Lesson, error)
	DeleteLesson(id string) error
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
	QueryAuditLog(level AuditLevel, component string, days, limit int) ([]map[string]interface{}, error)
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
	ProposeTheory(hypothesis, validationCriteria string, dependencies []string, tags []string) (map[string]interface{}, error)
	ResolveTheory(theoryID, conclusion, newStatus string) (map[string]interface{}, error)
	ResolveArbitrationTheory(theoryID, winnerID, conclusion string) (map[string]interface{}, error)
	ChallengeMemoryWithTheory(memoryID, evidence string) (map[string]interface{}, error)
	RecordDecision(contextText, choice, rationale, outcome string, tags []string, ac ActiveContext) (map[string]interface{}, error)
	ReviewMemories(daysSinceAccess, limit int) (map[string]interface{}, error)

	// ─── Handoffs ────────────────────────────────────────────────────
	EndSession(sessionID, summary, endedState string, commitments, openQuestions []string) (*Handoff, error)
	GetLatestHandoff() (*Handoff, error)
	GetLatestUnreadHandoff() (*Handoff, error)
	GetHandoffByID(id string) (*Handoff, error)
	MarkHandoffRead(id, readBy string) error
	MarkLatestHandoffRead(readBy string) (*Handoff, error)
	ListHandoffs(limit int, unreadOnly bool) ([]*Handoff, error)
	PruneHandoffs(retentionDays int) (int64, error)

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

	// ─── Arc 2: Active Dissemination ────────────────────────────────
	BroadcastMemory(memoryID string, opts BroadcastOpts) (*BroadcastReport, error)
	CheckPendingEventWakes(sessionID string) ([]EventWake, error)
	Heartbeat(sessionID, agentID, hostname string, metadata map[string]interface{}) error
	DiscoverActiveSessions() ([]ActiveSession, error)

	// ─── Directives ──────────────────────────────────────────────────
	ReadDirectives() ([]map[string]interface{}, error)
	ProactiveRecallHint(conversationText string, maxHints int, minScore float64) ([]map[string]interface{}, error)

	// ─── GC ──────────────────────────────────────────────────────────
	RunGC(opts GCOptions) (*GCRunResult, error)

	// ─── Wake Context ────────────────────────────────────────────────
	GatherWakeContext() (WakeContextData, error)
	ReadWakeContext() (string, error)
	ScratchpadOrphansSummary() string

	// ─── Global / Shared ─────────────────────────────────────────────
	QueryGlobalRules(query string, limit int) ([]map[string]interface{}, error)
	RecordGlobalRule(content string, tags []string, weight int, provenance string) (string, error)
	PromoteToGlobal(localID string) (string, error)

	// ─── Admission ───────────────────────────────────────────────────
	FindAdmissionCandidates(limit int) ([]*AdmissionCandidate, error)
	RecordAdmissionOutcome(candidate *AdmissionCandidate, result *AdmitResult, admissionModel string) error

	// ─── Misc ────────────────────────────────────────────────────────
	WipeRecord(tier, id string) error
}

// Compile-time assertion that *DatabaseManager satisfies CoreDB.
var _ CoreDB = (*DatabaseManager)(nil)
