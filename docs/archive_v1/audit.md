     Here is the structured audit report.                                                                       
                                                                                                                
     ─────────────────────────────────────────────────────────────────────────────────────────────────────────  
                                                                                                                
     MPM Core & Concurrency — Architectural Audit                                                               
                                                                                                                
     HIGH Severity                                                                                              
                                                                                                                
     H1. logWatchdogOp double-writes every synthesis event to watchdog.jsonl                                    
                                                                                                                
     Location: internal/synthesize.go:373–403                                                                   
     Vulnerability: logWatchdogOp() calls dm.logWatchdog(…) (line 384) which writes one JSON entry, AND then    
     opens the same file directly (line 396) to write a second entry with different structure. Every            
     synthesis success, failure, and skip produces two entries per event with different schemas.                
                                                                                                                
     Required fix: Remove the manual os.OpenFile block (lines 389–402). dm.logWatchdog() already handles the    
     file write with proper mutex serialisation. Alternatively, change dm.logWatchdog to accept arbitrary       
     structured data (map[string]interface{}) and eliminate logWatchdogOp entirely.                             
                                                                                                                
     ─────────────────────────────────────────────────────────────────────────────────────────────────────────  
                                                                                                                
     H2. AutoSynthesize goroutines in handleMemoryAdd can use-closed DB connection                              
                                                                                                                
     Location: cmd/mpm/handlers.go:345                                                                          
     Vulnerability: The goroutine creates its own DatabaseManager via NewDatabaseManager("") which opens a      
     fresh SQLite connection. This is correct for the CLI path (addressed in the previous session's fix).       
     However, the goroutine has no mechanism to be cancelled — if the main process shuts down, this goroutine   
     may be writing to a DB file that's being torn down. There is no context.Context wired in.                  
                                                                                                                
     Required fix:                                                                                              
     1. Accept ctx context.Context in AutoSynthesize and thread it through to SaveMemory.                       
     2. In the goroutine, derive from context.Background() but listen for a global shutdown signal.             
     3. Even without a fix, the DB write will either succeed (WAL checkpoint) or fail gracefully — the log      
        already handles failure. Low risk in practice.    

                                                                                                              
     H3. synthSeen package-level map grows without bound                                                        
                                                                                                                
     Location: internal/synthesize.go:232–235                                                                   
     Vulnerability: synthSeen is a map[string]bool at package scope that is never cleared. Every pair of        
     memory IDs ever synthesised accumulates permanently. In a long-running watcher process that ingests        
     thousands of .md files, this map grows linearly with ingested memory count and never shrinks.              
                                                                                                                
     Required fix: Add a periodic eviction or cap, e.g. wrap in an LRU or clear every N ingest cycles. For a    
     short-term fix, after AutoSynthesize returns, clear entries older than a threshold. Alternatively, use a   
     map[[2]string]time.Time and sweep entries older than 1 hour.                                               
                                                                                                                
     ─────────────────────────────────────────────────────────────────────────────────────────────────────────  
                                                                                                                
     MEDIUM Severity                                                                                            
                                                                                                                
     M1. Raw SQL via MemoryStore.DB bypasses DatabaseManager entirely                                           
                                                                                                                
     Locations:                                                                                                 
     - internal/memory.go:1200 — s.DB.Exec(UPDATE memories SET deleted_at = …) inside ConsolidateMemories       
     - internal/memory.go:2161,2265 — s.DB.Exec(UPDATE memories SET deleted_at = …) inside DedupeMemories       
     - internal/memory.go:326,533,543,552,673,720,810,874,887,1013,1053,1066,1083,1116,1258,1354,1442,1487,     
       1503,1572,1840,1876 — all route through s.DB (a *SQLiteConnection wrapping *sql.DB), not through         
       DatabaseManager                                                                                          
     Impact: These bypass the watchdog (ExecTracked/QueryTracked), the retry-backoff in ExecTracked, and any    
     future instrumentation added to DatabaseManager. In WAL mode, multiple open connections to the same DB     
     file are safe (SQLite handles concurrency via WAL), but the direct s.DB.Exec calls lose the exponential    
     backoff on SQLITE_BUSY.                                                                                    
                                                                                                                
     Required fix: Either:                                                                                      
     (a) Add ExecTracked/QueryTracked wrappers to SQLiteConnection/MemoryStore, or                              
     (b) Refactor MemoryStore to use DatabaseManager internally instead of holding its own *SQLiteConnection.   
     Option (b) is the long-term correct fix and aligns with "All access via one DatabaseManager".              
                                                                                                                
     ────────────────────────────────────────────────────────────────────────────────────────────────────────
                                                                                                                
     M2. ConsolidateMemories runs O(n²) embedding comparison inside a single worker goroutine                   
                                                                                                                
     Location: internal/memory.go:1157–1206                                                                     
     Vulnerability: The nested loop for i … for j := i+1 … over all non-deleted LTM/reinforced memories is O(   
     n²) in both memory fetches and embedding cosine-similarity. With maxPerTopic=50, worst-case is 50×50 = 2,  
     500 comparisons per cluster, but if the initial query returns thousands of memories (e.g., after months    
     of use), the first two loops scan the entire set.                                                          
                                                                                                                
     Impact: mpm maintain (called via RunSelfMaintenance) blocks its calling goroutine for the full duration.   
     If called on a large database, it can stall the worker pool for seconds.                                   
                                                                                                                
     Required fix: Add a LIMIT 500 to the initial SELECT (line 1116–1122) or paginate. The current query has    
     no LIMIT clause.                                                                                           
                                                                                                                
     ─────────────────────────────────────────────────────────────────────────────────────────────────────────  
                                                                                                                
     M3. FTS5 near-miss query in DetectNearMiss passes raw content words as query                               
                                                                                                                
     Location: internal/synthesize.go:188–204                                                                   
     Vulnerability: Lines 189–193 take the first 100 space-separated words from memory content and join them    
     with spaces to form an FTS5 query. FTS5 has special operators (*, ", AND, OR, NOT, NEAR). If memory        
     content contains FTS5 reserved words or syntax, the query could produce a syntax error (caught by line     
     205) or unexpected results.                                                                                
                                                                                                                
     Impact: Low — the error is caught and logged. But it means the near-miss detection silently degrades on    
     content with FTS5 special tokens.                                                                          
                                                                                                                
     Required fix: Sanitise query terms by stripping FTS5 operators or use escape/quote helper.                 
                                                                                                                
     ─────────────────────────────────────────────────────────────────────────────────────────────────────────  

     M4. sweepDirectory and processMarkdownFile silently skip .jsonl at startup                                 
                                                                                                                
     Location: cmd/mpm/watch.go:404–407                                                                         
     Vulnerability: The startup sweep explicitly skips .jsonl files with a comment that they are "processed     
     by the orphan sweep triggered when a new session lock (.jsonl.lock) is created." If the watcher restarts   
     and there are orphan .jsonl files that were never locked (and never swept), those files are permanently    
     lost — no fsnotify event for an existing file will fire.                                                   
                                                                                                                
     Impact: The reconciliationSweep goroutine (startReconciliationSweepGoroutine) does re-check orphan .       
     jsonl files every 10 minutes (via processReconciliationSweep → reconcileDirectory → wd.                    
     sweepOrphanSessions), so this is partially mitigated. However, the initial 30-second delay + 10-minute     
     interval means up to 10m 30s of data loss risk after startup.                                              
                                                                                                                
     Required fix: Fire an explicit EventReconciliationSweep at startup (not just EventStartupSweep), or have   
     startupSweep also sweep .jsonl orphans.                                                                    
                                                                                                                
     ─────────────────────────────────────────────────────────────────────────────────────────────────────────  
                                                                                                                
     M5. DetectNearMiss single-threaded API calls stall the FTS5 query goroutine                                
                                                                                                                
     Location: internal/synthesize.go:270                                                                       
     Vulnerability: AutoSynthesize is called synchronously inside the DetectNearMiss call flow (though the      
     caller fires it in a goroutine). The Synthesize LLM call uses no retry on transient network failures. If   
     the API is slow, goroutines accumulate in the fire-and-forget pattern.                                     
                                                                                                                
     Impact: Each failing or slow API call holds a DatabaseManager connection open for up to sc.Timeout (       
     default 300 seconds). With 3 pool workers, 3 slow syntheses exhaust the pool.                              
                                                                                                                
     Required fix: Add http.Client.Timeout already at client level (line 126). Add a small retry (1 attempt,    
     5s backoff) for transient HTTP 5xx errors inside Synthesize.                                               
                                                                                                                
     ─────────────────────────────────────────────────────────────────────────────────────────────────────────  
                                                                           
     LOW Severity                                                                                               
                                                                                                                
     L1. checkTopicClustering called after every ingest but never rate-limited                                  
                                                                                                                
     Location: cmd/mpm/watch.go:463                                                                             
     Vulnerability: checkTopicClustering runs SELECT id, collection, content, tags, embedding, created_at       
     FROM memories WHERE (is_long_term = 1 OR weight >= 10) AND deleted_at IS NULL which is a full table scan   
     on every .md file ingestion. With high ingest volume, this is wasteful.                                    
                                                                                                                
     ─────────────────────────────────────────────────────────────────────────────────────────────────────────  
                                                                                                                
     L2. mirror.jsonl write silently dropped on error                                                           
                                                                                                                
     Location: cmd/mpm/watch.go:1469–1476                                                                       
     Vulnerability: appendToMirror returns silently after json.Marshal failure (line 1470–1471) and after os.   
     OpenFile failure (line 1476). The error is swallowed. This is consistent with the "best-effort mirror"     
     design, but if the mirror file is important for debugging, these failures should at least log to stderr    
     in verbose mode.                                                                                           
                                                                                                                
     ─────────────────────────────────────────────────────────────────────────────────────────────────────────  
                                                                                                                
     L3. handleWatchStop sends Interrupt signal to detached PID but only waits 500ms                            
                                                                                                                
     Location: cmd/mpm/handlers.go:3282–3284                                                                    
     Vulnerability: proc.Signal(os.Interrupt) followed by time.Sleep(500ms) then a PID-liveness check. If the   
     child process is in the middle of a long synthesis API call (300s timeout), 500ms is insufficient. The     
     kill signal may never arrive before the parent reports "stopped."                                          
                                                                                                                
     ─────────────────────────────────────────────────────────────────────────────────────────────────────────  
                                                                                                   
     L4. synthSeen map is per-process — no persistence across restarts                                          
                                                                                                                
     Location: internal/synthesize.go:232                                                                       
     Vulnerability: The synthSeen dedup map is in-memory only. After a process restart, the same near-miss      
     pairs will be re-detected and re-synthesised. The soft-deleted originals are gone, so the FTS5 query       
     won't match them, but the duplicate pair could still cause a synthesis call for a pair that was already    
     merged in a previous process lifetime. This is acceptable behaviour but should be documented.              
                                                                                                                
     ─────────────────────────────────────────────────────────────────────────────────────────────────────────  
                                                                                                                
     L5. AutoSynthesize in watcher re-opens SynthClient on every ingest                                         
                                                                                                                
     Location: cmd/mpm/watch.go:459                                                                             
     Vulnerability: NewSynthClient() (which calls config.LoadConfig() and reads MINIMAX_API_KEY) fires on       
     every single .md file ingestion. This is a filesystem read + env var read per file. Not a correctness      
     issue, but wasteful.                                                                                       
                                                                                                                
     Required fix: Cache the SynthClient in the watcherDaemon struct and reuse it across calls.                 
                                                                                                                
     ─────────────────────────────────────────────────────────────────────────────────────────────────────────  
                                                               
     Summary Table                                                                                              
                                                                                                                
     ID  Severit  File                 Line(s)                   Issue                                          
         y                                                                                                      
     H1  HIGH     internal/            384–402                   logWatchdogOp double-writes watchdog.jsonl     
                  synthesize.go                                                                                 
     H2  HIGH     cmd/mpm/handlers.go  345                       Goroutine context not cancellable on shutdown  
     H3  HIGH     internal/            232–235                   synthSeen map grows without bound              
                  synthesize.go                                                                                 
     M1  MEDIUM   internal/memory.go   1200,2161,2265 + 30+      Raw s.DB.Exec bypasses DatabaseManager         
                                       others                    watchdog                                       
     M2  MEDIUM   internal/memory.go   1116–1151                 ConsolidateMemories has no LIMIT, O(n²) risk   
     M3  MEDIUM   internal/            188–204                   FTS5 query unsanitised; special tokens cause   
                  synthesize.go                                  errors                                         
     M4  MEDIUM   cmd/mpm/watch.go     404–407                   .jsonl skipped on startup; 10m gap before      
                                                                 sweep                                          
     M5  MEDIUM   internal/            126,317                   No retry on transient API failures             
                  synthesize.go                                                                                 
     L1  LOW      cmd/mpm/watch.go     463                       checkTopicClustering full scan on every        
                                                                 ingest                                         
     L2  LOW      cmd/mpm/watch.go     1469–1476                 Mirror write errors silently swallowed         
     L3  LOW      cmd/mpm/handlers.go  3282–3284                 Stop signal race; 500ms too short for 300s     
                                                                 API call                                       
     L4  LOW      internal/            232                       synthSeen not persisted; resets on restart     
                  synthesize.go                                                                                 
     L5  LOW      cmd/mpm/watch.go     459                       NewSynthClient() re-reads config on every      
                                                                 ingest                                         
                                                                          
                                                                                                           
