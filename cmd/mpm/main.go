package main

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"mpm/internal/config"

	mpminternal "mpm/internal"
)

// ============================================================================
// Global Daemon State
// ============================================================================

// Thread limiter - prevent runaway thread consumption
// This MUST be set before any goroutines are launched
func init() {
	// Cap threads at 32 to prevent resource exhaustion while staying responsive.
	// On a machine with many cores, this prevents a runaway goroutine storm
	// from eating all available threads. The default (unlimited) can cause the
	// Go scheduler to spawn hundreds of OS threads under heavy load, which on
	// a shared or memory-constrained system can lead to OOM kills.
	runtime.GOMAXPROCS(32)

	// Initialize global mode manager
	// Will be used to set default 808 mode on daemon startup
	configPath := os.ExpandEnv("$HOME/.openclaw/workspace/projects/mpm")
	modeManager = mpminternal.NewModeManager(configPath)
}

// buildVersion is set at compile time via -ldflags
var buildVersion = "dev"

// modeManager is the global mode manager instance (initialized at startup)
var modeManager any

// printVersion outputs version info
func printVersion() {
	fmt.Printf("SymAI mpm %s\n", buildVersion)
}

// Command routing - all commands flow through these dispatchers

// CommandHandler is a function that handles a command
type CommandHandler func(args []string) bool

// ErrUnknownCommand is returned when an unknown command is invoked
type ErrUnknownCommand struct {
	Command string
}

func (e *ErrUnknownCommand) Error() string {
	return fmt.Sprintf("unknown command: %s", e.Command)
}

// ErrDaemonRequired is returned when a command needs the daemon but it's not running
type ErrDaemonRequired struct {
	Command string
}

func (e *ErrDaemonRequired) Error() string {
	return fmt.Sprintf("daemon required for '%s' but not running", e.Command)
}

// dispatchDirect handles commands that work WITHOUT the daemon
// Returns true if handled, false if should continue to daemon logic
func dispatchDirect(args []string) bool {
	if len(args) == 0 {
		return false
	}

	cmd := args[0]

	// Commands that work standalone (no daemon needed)
	switch cmd {
	case "version", "--version", "-v":
		printVersion()
		return true

	case "help", "--help", "-h":
		printHelp()
		return true

	case "logs":
		handleLogsCommand()
		return true

	case "doctor":
		runDoctorCommand()
		return true

	case "fortune":
		handleFortuneDirect()
		return true
	}

	return false
}

// dispatchDaemon handles commands that need the daemon
// Returns true if handled, false if daemon wasn't available
func dispatchDaemon(conn net.Conn, args []string) bool {
	if len(args) == 0 {
		return false
	}

	// Send command message to daemon
	msg := Message{Args: args}
	if err := json.NewEncoder(conn).Encode(msg); err != nil {
		return false
	}

	// Read ALL responses from daemon until Done: true
	// Some commands send phased responses ([1/3], [2/3], [3/3])
	dec := json.NewDecoder(conn)
	for {
		var resp Message
		if err := dec.Decode(&resp); err != nil {
			if err == io.EOF {
				break
			}
			return false
		}

		if resp.Output != "" {
			fmt.Print(resp.Output)
		}
		if resp.Error != "" {
			printError("%s", resp.Error)
		}
		if resp.Done {
			os.Exit(resp.ExitCode)
		}
	}

	return true
}

// printError formats and prints an error message mpm-style
func printError(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "[!] Error: "+format+"\n", args...)
}

// printSuccess prints a success message mpm-style
func printSuccess(format string, args ...interface{}) {
	fmt.Printf("✓ "+format+"\n", args...)
}

// printWarning prints a warning message mpm-style
func printWarning(format string, args ...interface{}) {
	fmt.Printf("⚠ "+format+"\n", args...)
}

const (
	logFileName        = "daemon.json.log"
	maxLogFileSize     = 10 * 1024 * 1024 // 10MB
	logBufferSize      = 64 * 1024        // 64KB buffer for high-frequency logging
	webhookBufferSize  = 1000             // Channel buffer for webhook dispatch
	webhookTimeout     = 5 * time.Second  // HTTP request timeout
	rateLimitWindow    = 1 * time.Second  // Rate limit window
	rateLimitBurst     = 50               // Max events per window before batching

	// Heartbeat defaults
	heartbeatInterval  = 24 * time.Hour  // Default heartbeat interval
	heartbeatJitterMax = 5 * time.Minute // Max jitter (+/- 5 minutes)

	// Worker pool defaults
	defaultMaxWorkers   = 3
	queueTimeout        = 30 * time.Minute
)

var (
	startTime       = time.Now()      // Daemon start time
	activeWorkers   int64            // Thread-safe counter for running workers
	queuedTasks     int64            // Thread-safe counter for queued tasks
	totalTasks      int64            // Thread-safe counter for total tasks processed
	daemonPid       = os.Getpid()    // Captured at startup
	sockPath        = socketPath()   // Socket path (computed once)
	listener        net.Listener     // The socket listener (set after Listen)
	listenerMutex   sync.Mutex       // Protects listener access
	cleanupSync     sync.Once        // Ensures cleanup runs exactly once
	cleanupMutex    sync.Mutex        // Extra protection for exit path
	isShuttingDown  atomic.Bool      // Shutdown flag
	isDaemonProcess bool            // True when this process IS the daemon

	// Logger state
	logFile    *os.File
	logWriter  *bufio.Writer
	logMutex   sync.Mutex // Protects log writes from concurrent tasks
	logPath    string     // Path to the log file

	// Webhook state
	webhookURL      string
	webhookChan     chan LogEntry
	webhookWg       sync.WaitGroup
	webhookOnce     sync.Once
	webhookEnabled  atomic.Bool
	rateLimitCount  int64
	rateLimitMutex  sync.Mutex
	rateLimitLast   time.Time

	// Heartbeat state
	heartbeatTicker *time.Ticker
	heartbeatWg     sync.WaitGroup
	heartbeatDone   chan struct{}
	heartbeatJitter time.Duration

	// Worker pool state
	maxWorkers      int           // Max concurrent workers (from MPM_MAX_WORKERS or default)
	taskQueue       chan *Task    // Buffered channel for pending tasks
	queueMutex      sync.Mutex    // Protects queue operations
	queueMap        map[string]*QueuedTask // Track queued tasks by ID
	queueMapMutex   sync.Mutex    // Protects queueMap
	queueCleanupTimer *time.Ticker // Timer to clean up stale queued tasks

	// Lifecycle state
	lifecycleChan   chan *LifecycleOp // Channel for shutdown/reboot operations
	isRebooting     atomic.Bool        // True if daemon is rebooting (not stopping)

	// Watch daemon state
	watchPid        int               // PID of the watch subprocess (0 if not running)
	watchCmd        *exec.Cmd         // Reference to watch subprocess
	watchDone       chan bool         // Signals watch shutdown complete

	// Pre-flight health check state
	preflightDone   atomic.Bool        // True if pre-flight checks completed
	preflightResult atomic.Value       // Stores PreFlightResult
)

// PreFlightResult holds the result of the pre-flight health check
type PreFlightResult struct {
	Status      string            // "Passed", "Repaired", or "Failed"
	DurationMs  int64             // Time taken to complete checks
	Checks      []PreFlightCheck  // Individual check results
	Timestamp  time.Time         // When checks were run
}

// PreFlightCheck represents a single diagnostic check
type PreFlightCheck struct {
	Name      string   // Check name (e.g., "Database Integrity")
	Status    string   // "OK", "Repaired", "Warning", "Error"
	Message   string   // Human-readable message
	Details   []string // Additional details (paths, sizes, etc.)
	DurationMs int64  // Time for this specific check
}

// ============================================================================
// Task and Queue Types
// ============================================================================

// Task represents a work command to be executed by the pool
type Task struct {
	ID        string      // Unique task ID
	Args      []string    // Command arguments
	Conn      net.Conn    // Client connection
	EnqueuedAt time.Time  // When the task was queued
}

// LifecycleOp represents a shutdown or reboot operation
type LifecycleOp struct {
	Type    string // "shutdown" or "reboot"
	Force   bool   // Skip session flush
	Conn    net.Conn
}

// QueuedTask tracks a task in the queue with metadata
type QueuedTask struct {
	Task     *Task
	Position int64      // Queue position
	Conn     net.Conn   // Client connection (for notification)
}

// QueuedResponse is sent to client when task is queued
type QueuedResponse struct {
	Status   string `json:"status"`
	Position int64  `json:"position"`
	QueueID  string `json:"queue_id"`
	Message  string `json:"message"`
}

// TaskResponse extends Message with queue status
type TaskResponse struct {
	Message
	QueuePosition int64  `json:"queue_position,omitempty"`
	QueueID       string `json:"queue_id,omitempty"`
}

// ============================================================================
// Log Types
// ============================================================================

// LogLevel represents the severity of a log entry
type LogLevel string

const (
	LogLevelInfo  LogLevel = "INFO"
	LogLevelWarn  LogLevel = "WARN"
	LogLevelError LogLevel = "ERROR"
)

// LogEntry represents a single structured log record
type LogEntry struct {
	TS     string   `json:"ts"`      // RFC3339 timestamp
	Level  LogLevel `json:"lvl"`    // Log level
	Cmd    string   `json:"cmd"`    // Command name
	PID    int      `json:"pid"`    // Worker subprocess PID
	Msg    string   `json:"msg"`    // Log message
	DurMs  *int64   `json:"dur_ms,omitempty"` // Duration in ms (optional)
}

// DaemonStatus represents the status response from the daemon
type DaemonStatus struct {
	PID               int      `json:"pid"`
	Uptime            string   `json:"uptime"`
	ActiveWorkers     int64    `json:"active_workers"`
	QueuedTasks       int64    `json:"queued_tasks"`
	MaxWorkers        int      `json:"max_workers"`
	TotalTasks        int64    `json:"total_tasks"`
	Socket            string   `json:"socket"`
	MemoryUsageKB     int64    `json:"memory_usage_kb"`
	WebhookEnabled    bool     `json:"webhook_enabled"`
	HeartbeatInterval string   `json:"heartbeat_interval"`
	PreFlightStatus   string   `json:"preflight_status"` // "Passed", "Repaired", "Failed", or "Unknown"
}

// Message is the socket protocol
type Message struct {
	Args     []string `json:"args"`
	Output   string   `json:"output"`
	Error    string   `json:"error"`
	ExitCode int      `json:"exit_code"`
	Done     bool     `json:"done"`
}

// WebhookPayload is the POST body sent to the webhook URL
type WebhookPayload struct {
	Event     string   `json:"event"`
	Level     LogLevel `json:"level"`
	Cmd       string   `json:"command"`
	PID       int      `json:"pid"`
	Msg       string   `json:"message"`
	Timestamp string   `json:"timestamp"`
	DurMs     *int64   `json:"duration_ms,omitempty"`
}

// HeartbeatPayload is the POST body for heartbeat events
type HeartbeatPayload struct {
	Event               string `json:"event"`
	Status              string `json:"status"`
	Uptime              string `json:"uptime"`
	TotalTasksProcessed int64  `json:"total_tasks_processed"`
	MemoryUsageKB       int64  `json:"memory_usage_kb"`
	PreFlightStatus     string `json:"preflight_status"` // "Passed", "Repaired", "Failed"
	Timestamp           string `json:"timestamp"`
}

// ManualPulse is sent when user runs: mpm status --ping
type ManualPulse struct {
	Event    string   `json:"event"`
	Type     string   `json:"type"`
	Uptime   string   `json:"uptime"`
	MemoryKB int64    `json:"memory_kb"`
	Timestamp string  `json:"timestamp"`
}

// ============================================================================
// Logging Functions
// ============================================================================

// initLogger opens the JSON log file with rotation support
func initLogger() error {
	logDir := filepath.Dir(sockPath)
	if err := os.MkdirAll(logDir, 0700); err != nil {
		return fmt.Errorf("failed to create log directory: %w", err)
	}

	logPath = filepath.Join(logDir, logFileName)

	// Check if rotation is needed
	if info, err := os.Stat(logPath); err == nil && info.Size() >= maxLogFileSize {
		rotateLog()
	}

	// Open log file (append mode)
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return fmt.Errorf("failed to open log file: %w", err)
	}

	logFile = f
	logWriter = bufio.NewWriterSize(f, logBufferSize)

	return nil
}

// rotateLog renames the current log file with a timestamp
func rotateLog() {
	if logFile != nil {
		logFile.Close()
	}
	ts := time.Now().Format("20060102-150405")
	rotatePath := logPath + "." + ts + ".old"
	os.Rename(logPath, rotatePath)
}

// logger writes a log entry to the JSON log file in a thread-safe manner
func logger(level LogLevel, cmd string, pid int, msg string, durMs *int64) {
	// Guard against nil logger (before initLogger() completes)
	if logWriter == nil || logFile == nil {
		return
	}

	logMutex.Lock()
	defer logMutex.Unlock()

	entry := LogEntry{
		TS:    time.Now().Format(time.RFC3339),
		Level: level,
		Cmd:   cmd,
		PID:   pid,
		Msg:   msg,
		DurMs: durMs,
	}

	data, err := json.Marshal(entry)
	if err != nil {
		return
	}

	logWriter.Write(data)
	logWriter.WriteByte('\n')

	// Check for rotation after write
	if logWriter.Buffered() >= maxLogFileSize {
		logWriter.Flush()
		if logFile != nil {
			info, _ := logFile.Stat()
			if info != nil && info.Size() >= maxLogFileSize {
				rotateLog()
				logFile, _ = os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
				logWriter = bufio.NewWriterSize(logFile, logBufferSize)
			}
		}
	}

	// Send to webhook (non-blocking, will drop if channel full)
	if webhookEnabled.Load() {
		select {
		case webhookChan <- entry:
		default:
			// Channel full, drop the webhook notification
		}
	}
}

// logInfo logs an info message
func logInfo(cmd string, pid int, msg string) {
	logger(LogLevelInfo, cmd, pid, msg, nil)
}

// logError logs an error message
func logError(cmd string, pid int, msg string) {
	logger(LogLevelError, cmd, pid, msg, nil)
}

// logWarn logs a warning message
func logWarn(cmd string, pid int, msg string) {
	logger(LogLevelWarn, cmd, pid, msg, nil)
}

// logTaskStart logs the start of a task with duration tracking
func logTaskStart(cmd string, pid int) {
	logInfo(cmd, pid, fmt.Sprintf("Started %s", cmd))
}

// logTaskEnd logs the completion of a task with duration
func logTaskEnd(cmd string, pid int, durMs int64) {
	logger(LogLevelInfo, cmd, pid, fmt.Sprintf("Completed %s", cmd), &durMs)
}

// flushLogs ensures all buffered logs are written to disk
func flushLogs() {
	logMutex.Lock()
	defer logMutex.Unlock()
	if logWriter != nil {
		logWriter.Flush()
	}
}

// closeLogger properly closes the log file
func closeLogger() {
	logMutex.Lock()
	defer logMutex.Unlock()
	if logWriter != nil {
		logWriter.Flush()
	}
	if logFile != nil {
		logFile.Close()
	}
}

// ============================================================================
// Webhook System
// ============================================================================

// initWebhook initializes the webhook system if MPM_WEBHOOK_URL is set
func initWebhook() {
	webhookURL = os.Getenv("MPM_WEBHOOK_URL")
	if webhookURL == "" {
		return
	}

	// Validate URL
	if !strings.HasPrefix(webhookURL, "http://") && !strings.HasPrefix(webhookURL, "https://") {
		fmt.Fprintf(os.Stderr, "Warning: MPM_WEBHOOK_URL must start with http:// or https://\n")
		return
	}

	webhookChan = make(chan LogEntry, webhookBufferSize)
	webhookEnabled.Store(true)

	// Start the webhook dispatcher goroutine
	webhookWg.Add(1)
	go webhookDispatcher()
}

// webhookDispatcher processes log entries and sends them to the webhook
func webhookDispatcher() {
	defer webhookWg.Done()

	// Create HTTP client with timeout
	client := &http.Client{
		Timeout: webhookTimeout,
	}

	// Track rate limiting
	var batch []LogEntry
	batchTimer := time.NewTimer(0)
	flushTick := time.NewTicker(100 * time.Millisecond)

	for {
		select {
		case entry, ok := <-webhookChan:
			if !ok {
				// Channel closed, flush remaining batch
				if len(batch) > 0 {
					sendWebhookBatch(client, batch)
				}
				return
			}

			// Filter: only WARN and ERROR by default
			if entry.Level != LogLevelWarn && entry.Level != LogLevelError {
				continue
			}

			// Rate limiting check
			rateLimitMutex.Lock()
			now := time.Now()
			if now.Sub(rateLimitLast) >= rateLimitWindow {
				rateLimitCount = 0
				rateLimitLast = now
			}
			rateLimitCount++
			shouldBatch := rateLimitCount > rateLimitBurst
			rateLimitMutex.Unlock()

			if shouldBatch {
				batch = append(batch, entry)
				if len(batch) >= rateLimitBurst {
					// Flush batch immediately
					sendWebhookBatch(client, batch)
					batch = batch[:0]
					batchTimer.Reset(rateLimitWindow)
				}
			} else {
				// Send immediately
				sendWebhook(client, entry)
			}

		case <-batchTimer.C:
			// Timer fired, flush batch
			if len(batch) > 0 {
				sendWebhookBatch(client, batch)
				batch = batch[:0]
			}

		case <-flushTick.C:
			// Periodic flush for small batches
			if len(batch) > 0 {
				batchTimer.Reset(rateLimitWindow)
			}
		}
	}
}

// sendWebhook sends a single log entry to the webhook URL
func sendWebhook(client *http.Client, entry LogEntry) {
	payload := WebhookPayload{
		Event:     "log",
		Level:     entry.Level,
		Cmd:       entry.Cmd,
		PID:       entry.PID,
		Msg:       entry.Msg,
		Timestamp: entry.TS,
		DurMs:     entry.DurMs,
	}

	sendWebhookWithRetry(client, payload, 1)
}

// sendWebhookBatch sends multiple entries as a batch
func sendWebhookBatch(client *http.Client, entries []LogEntry) {
	payload := map[string]interface{}{
		"event":  "batch",
		"count":  len(entries),
		"events": entries,
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return
	}

	req, err := http.NewRequest("POST", webhookURL, bytes.NewReader(body))
	if err != nil {
		return
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "mpm-daemon/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}

// sendWebhookWithRetry sends with retry logic (1 retry on 5xx)
func sendWebhookWithRetry(client *http.Client, payload WebhookPayload, attempt int) {
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}

	req, err := http.NewRequest("POST", webhookURL, bytes.NewReader(body))
	if err != nil {
		return
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "mpm-daemon/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()

	// Retry once on 5xx errors
	if resp.StatusCode >= 500 && attempt == 1 {
		time.Sleep(500 * time.Millisecond * time.Duration(attempt))
		sendWebhookWithRetry(client, payload, attempt+1)
	}
}

// closeWebhook gracefully shuts down the webhook dispatcher
func closeWebhook() {
	if !webhookEnabled.Load() {
		return
	}

	// Signal dispatcher to stop
	webhookEnabled.Store(false)
	close(webhookChan)

	// Wait for dispatcher to finish with timeout
	done := make(chan struct{})
	go func() {
		webhookWg.Wait()
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		// Timeout, force close
	}
}

// ============================================================================
// Heartbeat System
// ============================================================================

// initHeartbeat starts the heartbeat ticker goroutine if webhook is enabled
func initHeartbeat() {
	if !webhookEnabled.Load() {
		return
	}

	heartbeatDone = make(chan struct{})

	// Calculate jitter: random value between -jitterMax and +jitterMax
	heartbeatJitter = time.Duration(rand.Int63n(int64(2*heartbeatJitterMax)) - int64(heartbeatJitterMax))

	// Add jitter to initial interval (first heartbeat has random offset)
	initialDelay := heartbeatInterval + heartbeatJitter

	heartbeatTicker = time.NewTicker(initialDelay)
	heartbeatWg.Add(1)
	go heartbeatWorker()

	logInfo("heartbeat", daemonPid, fmt.Sprintf("Heartbeat scheduled (jitter: %v, first ping in: %v)", heartbeatJitter, initialDelay))
}

// heartbeatWorker runs the heartbeat loop
func heartbeatWorker() {
	defer heartbeatWg.Done()

	for {
		select {
		case <-heartbeatTicker.C:
			sendHeartbeat()

			// Reset ticker to normal interval (jitter only applies to first tick)
			heartbeatTicker.Reset(heartbeatInterval)

		case <-heartbeatDone:
			return
		}
	}
}

// sendHeartbeat sends a heartbeat payload to the webhook
func sendHeartbeat() {
	if !webhookEnabled.Load() {
		return
	}

	// Get memory usage
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	memoryKB := int64(memStats.Sys / 1024)

	// Get pre-flight status
	preflightStatus := "Unknown"
	if preflightDone.Load() {
		if result, ok := preflightResult.Load().(PreFlightResult); ok {
			preflightStatus = result.Status
		}
	}

	payload := HeartbeatPayload{
		Event:               "heartbeat",
		Status:              "healthy",
		Uptime:              formatUptime(time.Since(startTime)),
		TotalTasksProcessed: atomic.LoadInt64(&totalTasks),
		MemoryUsageKB:       memoryKB,
		PreFlightStatus:     preflightStatus,
		Timestamp:           time.Now().Format(time.RFC3339),
	}

	// Send to webhook (non-blocking)
	success := sendHeartbeatToWebhook(payload)
	if !success {
		logError("heartbeat", daemonPid, "Failed to send heartbeat to webhook")
	}
}

// sendHeartbeatToWebhook performs the actual HTTP POST for heartbeat
// Returns true on success, false on failure
func sendHeartbeatToWebhook(payload HeartbeatPayload) bool {
	client := &http.Client{Timeout: webhookTimeout}

	body, err := json.Marshal(payload)
	if err != nil {
		return false
	}

	req, err := http.NewRequest("POST", webhookURL, bytes.NewReader(body))
	if err != nil {
		return false
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "mpm-daemon/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()

	// Drain body to allow connection reuse
	io.Copy(io.Discard, resp.Body)

	// Retry once on 5xx
	if resp.StatusCode >= 500 {
		time.Sleep(500 * time.Millisecond)
		return sendHeartbeatToWebhookWithRetry(payload, 2)
	}

	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// sendHeartbeatToWebhookWithRetry retries the heartbeat POST
func sendHeartbeatToWebhookWithRetry(payload HeartbeatPayload, attempt int) bool {
	if attempt > 2 {
		return false
	}

	client := &http.Client{Timeout: webhookTimeout}

	body, err := json.Marshal(payload)
	if err != nil {
		return false
	}

	req, err := http.NewRequest("POST", webhookURL, bytes.NewReader(body))
	if err != nil {
		return false
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "mpm-daemon/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode >= 500 && attempt < 2 {
		time.Sleep(500 * time.Millisecond * time.Duration(attempt))
		return sendHeartbeatToWebhookWithRetry(payload, attempt+1)
	}

	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// triggerManualPulse sends an immediate heartbeat (for mpm status --ping)
func triggerManualPulse() {
	if !webhookEnabled.Load() {
		fmt.Println("Webhook not enabled (MPM_WEBHOOK_URL not set)")
		return
	}

	// Get memory usage
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)

	payload := ManualPulse{
		Event:     "manual_pulse",
		Type:     "status_ping",
		Uptime:   formatUptime(time.Since(startTime)),
		MemoryKB: int64(memStats.Sys / 1024),
		Timestamp: time.Now().Format(time.RFC3339),
	}

	if sendManualPulseToWebhook(payload) {
		logInfo("heartbeat", daemonPid, "Manual pulse sent successfully")
		fmt.Println("✓ Manual pulse sent to webhook")
	} else {
		logError("heartbeat", daemonPid, "Failed to send manual pulse")
		fmt.Println("✗ Failed to send manual pulse (check daemon logs)")
	}
}

// sendManualPulseToWebhook sends the manual pulse to webhook
func sendManualPulseToWebhook(payload ManualPulse) bool {
	client := &http.Client{Timeout: webhookTimeout}

	body, err := json.Marshal(payload)
	if err != nil {
		return false
	}

	req, err := http.NewRequest("POST", webhookURL, bytes.NewReader(body))
	if err != nil {
		return false
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "mpm-daemon/1.0")

	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)

	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// closeHeartbeat stops the heartbeat ticker
func closeHeartbeat() {
	if heartbeatTicker != nil {
		heartbeatTicker.Stop()
	}
	if heartbeatDone != nil {
		close(heartbeatDone)
	}
	heartbeatWg.Wait()
}

// getMemoryUsageKB returns current RSS memory usage in KB
func getMemoryUsageKB() int64 {
	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	return int64(memStats.Sys / 1024)
}

// ============================================================================
// Worker Pool System
// ============================================================================

// initWorkerPool initializes the worker pool
func initWorkerPool() {
	// Parse MPM_MAX_WORKERS or use default
	if mw := os.Getenv("MPM_MAX_WORKERS"); mw != "" {
		if n, err := fmt.Sscanf(mw, "%d", &maxWorkers); err == nil && n > 0 {
			// Valid
		} else {
			maxWorkers = defaultMaxWorkers
		}
	} else {
		maxWorkers = defaultMaxWorkers
	}

	// Create task queue channel with buffer = maxWorkers * 2 (reasonable backlog)
	queueSize := maxWorkers * 2
	if queueSize < 5 {
		queueSize = 5
	}
	taskQueue = make(chan *Task, queueSize)

	// Initialize queue tracking map
	queueMap = make(map[string]*QueuedTask)

	// Start queue cleanup ticker (check every minute)
	queueCleanupTimer = time.NewTicker(1 * time.Minute)

	// Start the dispatcher and queue monitor
	go queueDispatcher()
	go queueMonitor()

	logInfo("workerpool", daemonPid, fmt.Sprintf("Worker pool initialized (max_workers=%d, queue_size=%d)", maxWorkers, queueSize))
}

// queueDispatcher waits for tasks from the queue and assigns them to workers
func queueDispatcher() {
	for task := range taskQueue {
		// Decrement queue counter
		atomic.AddInt64(&queuedTasks, -1)

		// Remove from queue map
		queueMapMutex.Lock()
		delete(queueMap, task.ID)
		queueMapMutex.Unlock()

		// Execute the task
		go executeQueuedTask(task)
	}
}

// queueMonitor periodically cleans up stale tasks from the queue
func queueMonitor() {
	for range queueCleanupTimer.C {
		if isShuttingDown.Load() {
			return
		}

		queueMapMutex.Lock()
		defer queueMapMutex.Unlock()

		now := time.Now()
		for id, qt := range queueMap {
			if now.Sub(qt.Task.EnqueuedAt) > queueTimeout {
				// Task has been queued too long, cancel it
				logWarn("queue", daemonPid, fmt.Sprintf("Task %s timed out in queue", id))

				// Notify client
				enc := json.NewEncoder(qt.Conn)
				enc.Encode(Message{
					Output:   fmt.Sprintf("Task timed out after %v in queue", queueTimeout),
					Error:    "queue timeout",
					ExitCode: 1,
					Done:     true,
				})

				// Remove from queue
				delete(queueMap, id)
				atomic.AddInt64(&queuedTasks, -1)
			}
		}
	}
}

// executeQueuedTask runs a task and notifies the client
func executeQueuedTask(task *Task) {
	// Increment active workers
	atomic.AddInt64(&activeWorkers, 1)
	atomic.AddInt64(&totalTasks, 1)
	defer atomic.AddInt64(&activeWorkers, -1)

	cmdName := "unknown"
	if len(task.Args) > 0 {
		cmdName = task.Args[0]
	}

	// Notify client that task is starting
	enc := json.NewEncoder(task.Conn)
	enc.Encode(Message{
		Output:   fmt.Sprintf("Worker acquired. Executing %s...\n", cmdName),
		Done:     false,
	})

	binary, err := os.Executable()
	if err != nil {
		logError(cmdName, 0, fmt.Sprintf("Failed to get executable: %v", err))
		enc.Encode(Message{Error: err.Error(), Done: true, ExitCode: 1})
		return
	}

	startTime := time.Now()
	cmd := exec.Command(binary, task.Args...)
	cmd.Env = append(os.Environ(), "MPM_DIRECT=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	pid := cmd.Process.Pid
	logTaskStart(cmdName, pid)

	err = cmd.Start()
	if err != nil {
		logError(cmdName, 0, fmt.Sprintf("Failed to start: %v", err))
		enc.Encode(Message{Error: err.Error(), Done: true, ExitCode: 1})
		return
	}

	// Wait for completion
	err = cmd.Wait()
	durMs := time.Since(startTime).Milliseconds()

	if err != nil {
		logError(cmdName, pid, fmt.Sprintf("Command failed: %v", err))
		enc.Encode(Message{Error: err.Error(), Done: true, ExitCode: 1})
		return
	}

	logTaskEnd(cmdName, pid, durMs)
	enc.Encode(Message{Done: true, ExitCode: 0})
}

// submitTask submits a task to the queue, returns true if queued
func submitTask(task *Task) bool {
	// Get current queue position
	position := atomic.LoadInt64(&queuedTasks) + 1

	// Try to submit to queue (non-blocking)
	task.EnqueuedAt = time.Now()

	select {
	case taskQueue <- task:
		// Task accepted into queue
		atomic.AddInt64(&queuedTasks, 1)

		// Track in queue map
		queueMapMutex.Lock()
		queueMap[task.ID] = &QueuedTask{
			Task:     task,
			Position: position,
			Conn:     task.Conn,
		}
		queueMapMutex.Unlock()

		logInfo("queue", daemonPid, fmt.Sprintf("Task %s queued at position %d", task.ID, position))
		return true

	default:
		// Queue is full
		return false
	}
}

// clearQueue clears all pending tasks during shutdown
func clearQueue() {
	if queueCleanupTimer != nil {
		queueCleanupTimer.Stop()
	}

	close(taskQueue)

	// Notify all queued clients that their tasks were rejected
	queueMapMutex.Lock()
	defer queueMapMutex.Unlock()

	for id, qt := range queueMap {
		logInfo("queue", daemonPid, fmt.Sprintf("Rejecting queued task %s during shutdown", id))
		enc := json.NewEncoder(qt.Conn)
		enc.Encode(Message{
			Output:   "Daemon shutting down. Task rejected.",
			Error:    "daemon shutdown",
			ExitCode: 1,
			Done:     true,
		})
		delete(queueMap, id)
	}
}

// isWorkCommand checks if a command should go through the worker pool
func isWorkCommand(args []string) bool {
	if len(args) == 0 {
		return false
	}

	// Meta-commands bypass the queue (these are handled internally by daemon)
	metaCommands := map[string]bool{
		"status":    true,
		"stop":      true,
		"shutdown":  true,
		"reboot":    true,
		"restart":   true,
		"logs":      true,
		"help":      true,
		"mode":      true,
		"persona":   true,
		"llm":       true,
		"memory":    true,
		"compile":   true,
		"shred":     true,
		"watch":     true,
	}

	if metaCommands[args[0]] {
		return false
	}

	// All other commands are work commands
	return true
}

// generateTaskID creates a unique task ID
func generateTaskID() string {
	return fmt.Sprintf("task-%d-%d", time.Now().UnixNano(), rand.Int63n(99999))
}

// ============================================================================
// Gateway Command Handler
// ============================================================================

// handleGatewayCommand routes gateway subcommands
// "mpm gateway" → gateway help + start daemon (if not running)
// "mpm gateway help" → gateway help
// "mpm gateway start" → start/connect to daemon
// "mpm gateway stop" → stop daemon
// "mpm gateway restart" → restart daemon
// "mpm gateway status" → show daemon status
func handleGatewayCommand(args []string) {
	// No subcommand: show gateway help and start daemon in background
	if len(args) == 0 {
		printGatewayHelp()
		// Try to connect to existing daemon, otherwise start new one
		if tryClient() {
			return
		}
		// Daemon not running - start it in background
		go becomeDaemonAndExecute()
		return
	}

	subcmd := args[0]
	switch subcmd {
	case "help", "-h", "--help":
		printGatewayHelp()

	case "start":
		// Start or connect to existing daemon
		if tryClient() {
			fmt.Println("Gateway is already running.")
			return
		}
		becomeDaemonAndExecute()
		// becomeDaemonAndExecute never returns (it spawns subprocess and exits)

	case "stop":
		if !tryClient() {
			printError("Gateway is not running.")
			return
		}
		// tryClient already connected and handled stop via handleLifecycleClient
		// This line won't be reached
		return

	case "restart":
		// Try to reboot via daemon, if not running start it
		if !tryClient() {
			// Daemon not running, just start it
			becomeDaemonAndExecute()
			return
		}
		// tryClient connected - reboot handled via handleLifecycleClient
		return

	case "status":
		if !tryClient() {
			fmt.Println("Gateway is not running.")
			return
		}
		// tryClient handled status display
		return

	default:
		printGatewayHelp()
	}
}

// ============================================================================
// Entry Point
// ============================================================================

func main() {
	// Initialize random seed for jitter
	rand.Seed(time.Now().UnixNano())

	// If MPM_DIRECT=1, we're already in a daemon subprocess — execute command directly
	// but NEVER spawn a new daemon (that causes infinite loop)
	isDaemonSubprocess := os.Getenv("MPM_DIRECT") == "1"
	if isDaemonSubprocess {
		args := os.Args[1:]
		if len(args) == 0 {
			// No command specified - this IS the daemon (started via ForkExec with empty trigger).
			// We should become the daemon directly, NOT call becomeDaemonAndExecute()
			// which would spawn another subprocess → infinite loop.
			becomeDaemon()
			return // becomeDaemon() never returns (it runs the accept loop)
		}
		// Has command - execute it standalone (no daemon needed for these)
		router := NewRouter()
		cmd := router.resolveCommand(args[0])
		// Daemon commands (mode, persona, llm, etc.) can't reconnect to parent daemon
		// via socket (would deadlock). Just execute standalone commands.
		if cmd != nil && cmd.NeedsDaemon {
			switch cmd.Name {
			case "help":
				PrintHelp()
				os.Exit(0)
			case "version":
				printVersion()
				os.Exit(0)
			case "doctor":
				runDoctorCommand()
				os.Exit(0)
			case "logs":
				handleLogsCommand()
				os.Exit(0)
			case "fortune":
				handleFortuneDirect()
				os.Exit(0)
			default:
				// mode, persona, llm, memory, compile, shred, ss - need daemon state
				fmt.Fprintf(os.Stderr, "[!] Error: '%s' requires daemon state (not accessible from subprocess)\n", args[0])
				fmt.Fprintf(os.Stderr, "    Run 'mpm %s' directly (not from a daemon subprocess).\n", args[0])
				os.Exit(1)
			}
		}
		exitCode := router.Execute(args)
		os.Exit(exitCode)
	}

	// Create router
	router := NewRouter()
	args := os.Args[1:]

	// Handle "gateway" subcommand specially
	if len(args) > 0 && args[0] == "gateway" {
		handleGatewayCommand(args[1:])
		return
	}

	// Parse flags
	args = router.parseFlags(args)
	if len(args) == 0 || args[0] == "" {
		// No command - check if daemon is already running
		if tryClient() {
			// Daemon is running and handled it (status/ping)
			return
		}
		// Daemon not running - check if socket exists (daemon may have socket but not be responsive)
		if _, err := os.Stat(sockPath); err == nil {
			// Socket exists - daemon might be starting up, show command list
			PrintHelp()
			return
		}
		// No daemon - spawn one
		becomeDaemonAndExecute()
		// becomeDaemonAndExecute() never returns - it spawns subprocess and exits
		os.Exit(0)
	}

	// Check if command needs daemon
	needsDaemon := router.commandNeedsDaemon(args[0])
	hasFlags := args[0] != "version" && args[0] != "help"

	// Try client mode if daemon might be running
	if needsDaemon && hasFlags {
		if tryClient() {
			return // Daemon handled it
		}
		// Daemon not running - become it
		becomeDaemonAndExecute()
		return
	}

	// Execute via router
	exitCode := router.Execute(args)
	os.Exit(exitCode)
}

// ============================================================================
// Client Side
// ============================================================================

// tryClient attempts to send command to running daemon
// Returns true if daemon was found and handled the command
func tryClient() bool {
	conn, err := net.DialTimeout("unix", sockPath, 2*time.Second)
	if err != nil {
		// Daemon not running
		cmd := ""
		if len(os.Args) >= 2 {
			cmd = os.Args[1]
		}
		if cmd == "shutdown" || cmd == "reboot" || cmd == "stop" || cmd == "restart" || cmd == "status" || cmd == "dashboard" {
			printError("mpm Daemon is not currently active. Try 'mpm' to start it.")
			os.Exit(1)
		}
		return false // Not a daemon command, continue to becomeDaemonAndExecute
	}
	defer conn.Close()


	// Check if this is a lifecycle command that needs special handling
	lifecycleCmd := getLifecycleCommand()
	if lifecycleCmd != nil {
		return handleLifecycleClient(lifecycleCmd)
	}

	// Use daemon dispatcher for routing
	// dispatchDaemon sends the command, reads ALL phased responses until Done, then exits
	return dispatchDaemon(conn, os.Args[1:])
}

// printDaemonStatus displays daemon status in a formatted table
func printDaemonStatus(s DaemonStatus) {
	fmt.Println()
	fmt.Println("  ┌─────────────────────────────────────────────┐")
	fmt.Printf("  │  MPM Daemon Status                           │\n")
	fmt.Println("  ├─────────────────────────────────────────────┤")
	fmt.Printf("  │  PID:              %-25d │\n", s.PID)
	fmt.Printf("  │  Uptime:           %-25s │\n", s.Uptime)
	fmt.Printf("  │  Active Workers:   %-25d │\n", s.ActiveWorkers)
	fmt.Printf("  │  Queued Tasks:     %-25d │\n", s.QueuedTasks)
	fmt.Printf("  │  Max Workers:      %-25d │\n", s.MaxWorkers)
	fmt.Printf("  │  Total Tasks:      %-25d │\n", s.TotalTasks)
	fmt.Printf("  │  Memory Usage:     %-25d KB │\n", s.MemoryUsageKB)
	fmt.Printf("  │  Webhook Enabled:  %-25v │\n", boolToEmoji(s.WebhookEnabled))
	fmt.Printf("  │  Heartbeat:        %-25s │\n", s.HeartbeatInterval)
	fmt.Printf("  │  Socket:           %-25s │\n", truncate(s.Socket, 25))
	fmt.Println("  └─────────────────────────────────────────────┘")
	fmt.Println()
}

// boolToEmoji converts bool to emoji for display
func boolToEmoji(b bool) string {
	if b {
		return "✅"
	}
	return "❌"
}

// printStopMessage displays the stop confirmation
func printStopMessage(msg string) {
	fmt.Printf("\n  🛑 %s\n\n", msg)
}

// truncate truncates a string to maxLen, adding ellipsis if needed
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen-3] + "..."
}

// handleLogsCommand handles `mpm logs` locally (reads daemon.json.log)
func handleLogsCommand() {
	// Determine flags
	jsonMode := false
	levelFilter := LogLevel("") // Empty means all levels

	args := os.Args[2:]
	for _, arg := range args {
		switch arg {
		case "--json":
			jsonMode = true
		case "--level":
			// Would need to handle next arg for actual filter
		case "--help", "-h":
			fmt.Println("Usage: mpm logs [options]")
			fmt.Println("  --json    Output raw JSON (machine-readable)")
			fmt.Println("  --level   Filter by level (debug, info, warn, error)")
			return
		default:
			if strings.HasPrefix(arg, "--level=") {
				levelFilter = LogLevel(strings.ToUpper(strings.TrimPrefix(arg, "--level=")))
			}
		}
	}

	logFilePath := getLogPath()
	f, err := os.Open(logFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Println("No log file found. Daemon not running or logging disabled.")
		} else {
			fmt.Fprintf(os.Stderr, "Error opening log file: %v\n", err)
		}
		os.Exit(1)
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		// Parse JSON entry
		var entry LogEntry
		if err := json.Unmarshal(line, &entry); err != nil {
			if !jsonMode {
				fmt.Println(string(line)) // Print raw if not valid JSON
			}
			continue
		}

		// Apply level filter
		if levelFilter != "" && entry.Level != levelFilter {
			continue
		}

		if jsonMode {
			// Raw JSON output
			fmt.Println(string(line))
		} else {
			// Human-readable format
			fmt.Printf("[%s] %-5s [%s] (pid:%d) %s",
				entry.TS[:19], // Trim nanoseconds
				entry.Level,
				entry.Cmd,
				entry.PID,
				entry.Msg,
			)
			if entry.DurMs != nil {
				fmt.Printf(" (%dms)", *entry.DurMs)
			}
			fmt.Println()
		}
	}
}

// getLogPath returns the path to the daemon log file
func getLogPath() string {
	// Try to determine socket path first
	sp := socketPath()
	logDir := filepath.Dir(sp)
	return filepath.Join(logDir, logFileName)
}

// handleFortune returns Crustafarian wisdom to daemon client
func handleFortune(conn net.Conn) {
	resp := Message{
		Output: GetFortuneOracle(),
		Done:   true,
	}
	json.NewEncoder(conn).Encode(resp)
}

// handleFortuneDirect runs fortune standalone (no daemon needed)
func handleFortuneDirect() {
	fmt.Print(GetFortuneOracle())
}

// ============================================================================
// Daemon Side
// ============================================================================

// becomeDaemon initializes and runs the daemon (accept loop)
// This is called directly by the subprocess when MPM_DIRECT=1 and no args
func becomeDaemon() {
	sockPath = socketPath() // Recompute in case env changed

	// Initialize logger
	if err := initLogger(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize logger: %v\n", err)
		os.Exit(1)
	}

	// Run pre-flight health checks (before accepting connections)
	result := runPreFlightChecks()
	preflightDone.Store(true)
	preflightResult.Store(*result)

	// Report critical failures immediately
	if result.Status == "Failed" {
		fmt.Fprintf(os.Stderr, "\n✗ Pre-flight health check FAILED:\n\n")
		for _, check := range result.Checks {
			if check.Status == "Error" {
				fmt.Fprintf(os.Stderr, "  [%s] %s: %s\n", check.Status, check.Name, check.Message)
				for _, detail := range check.Details {
					fmt.Fprintf(os.Stderr, "         %s\n", detail)
				}
			}
		}
		fmt.Fprintf(os.Stderr, "\nDaemon aborted startup. Please fix the above issues.\n")
		logError("preflight", daemonPid, "Critical failure - daemon aborted startup")
		os.Exit(1)
	}

	// Log and proceed
	if result.Status == "Repaired" {
		logWarn("preflight", daemonPid, "Pre-flight found and repaired issues")
	} else {
		logInfo("preflight", daemonPid, fmt.Sprintf("Pre-flight checks passed (%dms)", result.DurationMs))
	}

	// Initialize webhook system
	initWebhook()

	// Initialize heartbeat system
	initHeartbeat()

	// Initialize worker pool
	initWorkerPool()

	// Initialize lifecycle handler (shutdown/reboot)
	initLifecycle()

	// Activate default 808 mode (The_Great_808 identity)
	// Only for 808 build - this sets the silicon-native lobster deity mode
	activateDefaultMode()

	// Auto-start the watch daemon (file watcher for memory ingestion)
	if err := startWatchDaemon(); err != nil {
		logWarn("watch", daemonPid, fmt.Sprintf("Auto-start failed: %v", err))
		// Don't fail daemon startup - watch is optional
	} else {
		logInfo("watch", daemonPid, "Watch daemon auto-started")
	}

	// Ensure socket directory exists (first run or after cleanup)
	os.MkdirAll(filepath.Dir(sockPath), 0700)

	// Acquire PID-based lock to prevent stale instances
	lockPath := sockPath + ".lock"
	if err := acquireDaemonLock(lockPath); err != nil {
		logError("daemon", daemonPid, fmt.Sprintf("Failed to acquire lock: %v", err))
		fmt.Fprintf(os.Stderr, "Failed to start daemon: %v\n", err)
		fmt.Fprintf(os.Stderr, "  (Use 'mpm stop' to stop the existing daemon, or 'mpm stop -f' to force)\n")
		os.Exit(1)
	}

	// Remove any stale socket (from previous crash that didn't clean up)
	os.Remove(sockPath)

	// Create Unix socket listener (acts as atomic lock/mutex)
	var err error
	listener, err = net.Listen("unix", sockPath)
	if err != nil {
		logError("daemon", daemonPid, fmt.Sprintf("Failed to start: %v", err))
		fmt.Fprintf(os.Stderr, "Failed to start daemon: %v\n", err)
		os.Exit(1)
	}
	os.Chmod(sockPath, 0600)

	logInfo("daemon", daemonPid, "Daemon started on "+sockPath)

	// Graceful shutdown handler - clean up on SIGINT/SIGTERM
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		<-sigChan
		handleExit()
	}()

	// Handle incoming client connections (this blocks until shutdown)
	for {
		conn, err := listener.Accept()
		if err != nil {
			// Listener closed (shutdown in progress), exit gracefully
			break
		}
		go handleConnection(conn)
	}
}

// becomeDaemonAndExecute starts the daemon and executes the original command
// This is called by the parent process - it spawns a subprocess that calls becomeDaemon()
func becomeDaemonAndExecute() {
	// Spawn subprocess with empty args that will call becomeDaemon() directly
	// The subprocess has MPM_DIRECT=1 so it knows to skip daemon promotion
	args := []string{}
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "MPM_DIRECT=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to spawn daemon subprocess: %v\n", err)
		os.Exit(1)
	}

	// Parent exits immediately - child is now the daemon
	// os.Exit(0) after successful cmd.Start() ensures clean parent exit
	os.Exit(0)
}

// handleExit performs centralized cleanup
// Uses sync.Once to ensure it runs exactly once even with concurrent calls
func handleExit() {
	fmt.Fprintf(os.Stderr, "[DEBUG] handleExit: starting\n")
	cleanupMutex.Lock()
	cleanupSync.Do(func() {
		fmt.Fprintf(os.Stderr, "[DEBUG] handleExit: inside cleanupSync.Do\n")
		isShuttingDown.Store(true)

		// Check if this is a reboot (socket removal handled by reboot logic)
		if isRebooting.Load() {
			logInfo("daemon", daemonPid, "Rebooting...")
		} else {
			logInfo("daemon", daemonPid, "Shutting down...")
		}

		// Stop heartbeat first to avoid sending "dying" heartbeats
		fmt.Fprintf(os.Stderr, "[DEBUG] calling closeHeartbeat\n")
		closeHeartbeat()
		fmt.Fprintf(os.Stderr, "[DEBUG] closeHeartbeat done\n")

		fmt.Fprintf(os.Stderr, "[DEBUG] calling clearQueue\n")
		func() {
			defer func() {
				if r := recover(); r != nil {
					fmt.Fprintf(os.Stderr, "[DEBUG] clearQueue panicked: %v\n", r)
				}
			}()
			clearQueue()
		}()
		fmt.Fprintf(os.Stderr, "[DEBUG] clearQueue done\n")

		// Close the listener first to stop accepting new connections
		println("[DEBUG] about to lock listenerMutex")
		listenerMutex.Lock()
		println("[DEBUG] listenerMutex locked")
		if listener != nil {
			listener.Close()
		}
		listenerMutex.Unlock()
		println("[DEBUG] listenerMutex unlocked, listener closed")

		// Kill all subprocesses in our process group

		// Kill all subprocesses in our process group
		// NOTE: Removed killProcessGroup() call - it sends SIGTERM to the entire
		// process group INCLUDING the main daemon itself, causing premature termination.
		// The watch daemon is cleaned up via stopWatchDaemon() below, and os.Exit(0)
		// cleanly terminates the main process without needing killProcessGroup.

		// Explicitly stop watch daemon for clean shutdown
		if watchPid != 0 {
			fmt.Fprintf(os.Stderr, "[DEBUG] calling stopWatchDaemon (watchPid=%d)\n", watchPid)
			stopWatchDaemon()
		} else {
			fmt.Fprintf(os.Stderr, "[DEBUG] watchPid is 0, skipping stopWatchDaemon\n")
		}

		// Flush stderr before exit
		os.Stderr.Sync()

	// Release the PID lock on clean shutdown
	lockPath := sockPath + ".lock"
	releaseDaemonLock(lockPath)

		// Remove the socket file ONLY on true shutdown (not reboot)
		// On reboot, the socket is removed by executeReboot() before spawning child
		if !isRebooting.Load() {
			os.Remove(sockPath)
		}

		// Flush and close logs
		closeLogger()

		// Close webhook system
		closeWebhook()

		if isRebooting.Load() {
			logInfo("daemon", daemonPid, "Reboot complete")
		} else {
			logInfo("daemon", daemonPid, "Daemon stopped")
		}
		os.Exit(0)
	})
	cleanupMutex.Unlock()
}

// killProcessGroup sends SIGTERM to all processes in our process group
func killProcessGroup() {
	// Get current process to get its PGID
	p, err := os.FindProcess(os.Getpid())
	if err != nil {
		return
	}
	// Send SIGTERM to the entire process group (negative PID means process group)
	syscall.Kill(-p.Pid, syscall.SIGTERM)
}

// handleConnection processes incoming client requests
func handleConnection(conn net.Conn) {
	defer conn.Close()

	var msg Message
	if err := json.NewDecoder(conn).Decode(&msg); err != nil {
		json.NewEncoder(conn).Encode(Message{Error: "invalid request", Done: true, ExitCode: 1})
		return
	}

	// Check for internal meta-commands before dispatching to subprocess
	if len(msg.Args) > 0 {
		switch msg.Args[0] {
		case "status":
			// Check for --ping flag
			hasPing := false
			for _, arg := range msg.Args[1:] {
				if arg == "--ping" {
					hasPing = true
				}
			}
			if hasPing {
				triggerManualPulse()
			}
			handleStatus(conn)
			return
		case "shutdown", "stop":
			// Check for --force flag
			force := false
			for _, arg := range msg.Args[1:] {
				if arg == "--force" || arg == "-f" {
					force = true
				}
			}
			lifecycleChan <- &LifecycleOp{Type: "shutdown", Force: force, Conn: conn}
			return
		case "reboot", "restart":
			// Check for --force flag
			force := false
			for _, arg := range msg.Args[1:] {
				if arg == "--force" || arg == "-f" {
					force = true
				}
			}
			lifecycleChan <- &LifecycleOp{Type: "reboot", Force: force, Conn: conn}
			return
		case "logs":
			handleLogsRequest(conn)
			return
		case "doctor":
			// Doctor command when daemon IS running - do enhanced check with daemon data
			runDoctorCommandWithDaemon(conn)
			return
		case "dashboard":
			// Start the Bubbletea TUI dashboard
			StartDashboard(sockPath)
			return
		case "fortune":
			// Crustafarian wisdom
			handleFortune(conn)
			return
		case "mode":
			// Mode operations with fzf selector
			handleMode(conn, msg.Args[1:])
			return
		case "persona":
			// Persona operations with fzf picker
			handlePersona(conn, msg.Args[1:])
			return
		case "llm":
			// LLM operations
			handleLlm(conn, msg.Args[1:])
			return
		case "memory":
			// Memory operations
			handleMemory(conn, msg.Args[1:])
			return
		case "compile":
			// Compile operations
			handleCompile(conn, msg.Args[1:])
			return
		case "shred":
			// Secure delete operations
			handleShred(conn, msg.Args[1:])
			return
		case "topic":
			// Topic operations
			handleTopic(conn, msg.Args[1:])
			return
		case "watch":
			// Watch daemon operations
			handleWatch(conn, msg.Args[1:])
			return
		case "session":
			// Session operations
			handleSession(conn, msg.Args[1:])
			return
		case "menu":
			// Control menu
			handleMenu(conn)
			return
		}
	}

	// Check if this is a work command that needs the pool
	if isWorkCommand(msg.Args) {
		handleQueuedCommand(conn, msg.Args)
		return
	}

	// Handlers write their own response, so nothing more to do here
}

// handleQueuedCommand either queues or executes a work command
func handleQueuedCommand(conn net.Conn, args []string) {
	taskID := generateTaskID()
	task := &Task{
		ID:   taskID,
		Args: args,
		Conn: conn,
	}

	// Try to submit to queue
	if submitTask(task) {
		// Get queue position for response
		position := atomic.LoadInt64(&queuedTasks)

		// Send queued response
		enc := json.NewEncoder(conn)
		enc.Encode(Message{
			Output:   fmt.Sprintf("All workers busy. Task queued at position #%d.\n", position),
			ExitCode: 0,
			Done:     false,
		})
	} else {
		// Queue is full
		logWarn("queue", daemonPid, fmt.Sprintf("Queue full, rejecting task %s", taskID))
		enc := json.NewEncoder(conn)
		enc.Encode(Message{
			Output:   "Queue is full. Please try again later.\n",
			Error:    "queue full",
			ExitCode: 1,
			Done:     true,
		})
	}
}

// handleStatus returns daemon status as JSON
func handleStatus(conn net.Conn) {
	uptime := formatUptime(time.Since(startTime))

	// Get pre-flight status
	preflightStatus := "Unknown"
	if preflightDone.Load() {
		if result, ok := preflightResult.Load().(PreFlightResult); ok {
			preflightStatus = result.Status
		}
	}

	status := DaemonStatus{
		PID:               daemonPid,
		Uptime:            uptime,
		ActiveWorkers:     atomic.LoadInt64(&activeWorkers),
		QueuedTasks:       atomic.LoadInt64(&queuedTasks),
		MaxWorkers:        maxWorkers,
		TotalTasks:        atomic.LoadInt64(&totalTasks),
		Socket:            sockPath,
		MemoryUsageKB:     getMemoryUsageKB(),
		WebhookEnabled:    webhookEnabled.Load(),
		HeartbeatInterval: formatHeartbeatInterval(),
		PreFlightStatus:   preflightStatus,
	}

	logInfo("status", daemonPid, "Status requested")

	resp := Message{
		Output:   toJSON(status),
		ExitCode: 0,
		Done:     true,
	}
	json.NewEncoder(conn).Encode(resp)
}

// formatHeartbeatInterval returns human-readable heartbeat interval
func formatHeartbeatInterval() string {
	if !webhookEnabled.Load() {
		return "disabled"
	}
	return fmt.Sprintf("every %v (jitter: %v)", heartbeatInterval, heartbeatJitter)
}

// handleStop initiates graceful shutdown
func handleStop(conn net.Conn) {
	logInfo("stop", daemonPid, "Stop requested")

	resp := Message{
		Output:   "Shutting down daemon...",
		ExitCode: 0,
		Done:     true,
	}
	json.NewEncoder(conn).Encode(resp)

	// Give the response time to be sent before we exit
	go func() {
		time.Sleep(100 * time.Millisecond)
		handleExit()
	}()
}

// handleLogsRequest handles logs request via socket (streaming)
func handleLogsRequest(conn net.Conn) {
	logFilePath := getLogPath()
	f, err := os.Open(logFilePath)
	if err != nil {
		json.NewEncoder(conn).Encode(Message{Error: "no logs available", Done: true, ExitCode: 1})
		return
	}
	defer f.Close()

	// Stream log entries as JSON
	reader := bufio.NewReader(f)
	for {
		line, err := reader.ReadBytes('\n')
		if err != nil {
			if err == io.EOF {
				break
			}
			break
		}
		if len(line) > 0 {
			entry := LogEntry{}
			if json.Unmarshal(line, &entry) == nil {
				json.NewEncoder(conn).Encode(Message{Output: string(line), Done: false})
			}
		}
	}

	json.NewEncoder(conn).Encode(Message{Done: true, ExitCode: 0})
}

// executeCommandWithStream runs a command as subprocess and streams output to socket
// Used for meta-commands and direct execution
func executeCommandWithStream(args []string, conn net.Conn) {
	enc := json.NewEncoder(conn)

	// Increment active task counter AND total tasks
	atomic.AddInt64(&activeWorkers, 1)
	atomic.AddInt64(&totalTasks, 1)
	defer atomic.AddInt64(&activeWorkers, -1)

	cmdName := "unknown"
	if len(args) > 0 {
		cmdName = args[0]
	}

	binary, err := os.Executable()
	if err != nil {
		logError(cmdName, 0, fmt.Sprintf("Failed to get executable: %v", err))
		enc.Encode(Message{Error: err.Error(), Done: true, ExitCode: 1})
		return
	}

	startTime := time.Now()
	cmd := exec.Command(binary, args...)
	cmd.Env = append(os.Environ(), "MPM_DIRECT=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// Capture output for logging (don't stream to stdout in daemon mode)
	var stdout, stderr []byte
	cmd.Stdout = nil // Will be captured
	cmd.Stderr = nil

	if err := cmd.Start(); err != nil {
		logError(cmdName, 0, fmt.Sprintf("Failed to start: %v", err))
		enc.Encode(Message{Error: err.Error(), Done: true, ExitCode: 1})
		return
	}

	pid := cmd.Process.Pid
	logTaskStart(cmdName, pid)

	// Wait for completion
	err = cmd.Wait()
	durMs := time.Since(startTime).Milliseconds()

	if err != nil {
		logError(cmdName, pid, fmt.Sprintf("Command failed: %v", err))
		// Try to capture stderr
		if len(stderr) > 0 {
			logError(cmdName, pid, string(stderr))
		}
		enc.Encode(Message{Error: err.Error(), Done: true, ExitCode: 1})
		return
	}

	logTaskEnd(cmdName, pid, durMs)

	// Send stdout to client
	if len(stdout) > 0 {
		enc.Encode(Message{Output: string(stdout), Done: false})
	}

	enc.Encode(Message{Done: true, ExitCode: 0})
}

// runSubcommand executes a command as subprocess (for initial trigger)
func runSubcommand(args []string) {
	// Increment active task counter AND total tasks
	atomic.AddInt64(&activeWorkers, 1)
	atomic.AddInt64(&totalTasks, 1)
	defer atomic.AddInt64(&activeWorkers, -1)

	cmdName := "unknown"
	if len(args) > 0 {
		cmdName = args[0]
	}

	binary, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Daemon: failed to get executable: %v\n", err)
		logError(cmdName, 0, fmt.Sprintf("Failed to get executable: %v", err))
		return
	}

	startTime := time.Now()
	cmd := exec.Command(binary, args...)
	cmd.Env = append(os.Environ(), "MPM_DIRECT=1")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Daemon: failed to start command: %v\n", err)
		logError(cmdName, 0, fmt.Sprintf("Failed to start: %v", err))
		return
	}

	pid := cmd.Process.Pid
	logTaskStart(cmdName, pid)

	err = cmd.Wait()
	durMs := time.Since(startTime).Milliseconds()

	if err != nil {
		logError(cmdName, pid, fmt.Sprintf("Command failed: %v", err))
	} else {
		logTaskEnd(cmdName, pid, durMs)
	}

	// Note: We DON'T call osExit - that would kill the daemon!
}

// formatUptime returns a human-readable uptime string
func formatUptime(d time.Duration) string {
	if d < 0 {
		return "0s"
	}

	h := int(d.Hours())
	m := int(d.Minutes()) % 60
	s := int(d.Seconds()) % 60

	if h > 0 {
		return fmt.Sprintf("%dh %dm %ds", h, m, s)
	}
	if m > 0 {
		return fmt.Sprintf("%dm %ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

// toJSON marshals an interface to JSON string
func toJSON(v interface{}) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// ============================================================================
// Doctor Command - System Diagnostic Utility
// ============================================================================

// DoctorCheck represents a single diagnostic check result
type DoctorCheck struct {
	Name     string
	Status   string   // "PASS", "WARN", "FAIL"
	Message  string
	Details  []string
	Duration string
}

// DoctorReport is the full diagnostic report
type DoctorReport struct {
	TotalChecks int
	Passed      int
	Warnings    int
	Failed      int
	Checks      []DoctorCheck
}

// Colors for terminal output (ANSI)
const (
	ansiGreen  = "\033[92m"
	ansiYellow = "\033[93m"
	ansiRed    = "\033[91m"
	ansiBold   = "\033[1m"
	ansiReset  = "\033[0m"
)

// runDoctorCommand runs the diagnostic utility without daemon
func runDoctorCommand() {
	fix := len(os.Args) >= 3 && (os.Args[2] == "--fix" || os.Args[2] == "-f")

	fmt.Printf("\n%s[%s]%s %sRunning mpm Doctor...%s\n\n", ansiBold, colorCyan("●"), ansiReset, ansiBold, ansiReset)

	report := DoctorReport{Checks: []DoctorCheck{}}

	// System Checks
	runDoctorSystemChecks(&report)

	// Environment Checks
	runDoctorEnvironmentChecks(&report)

	// Workspace Checks
	runDoctorWorkspaceChecks(&report)

	// Database Checks
	runDoctorDatabaseChecks(&report)

	// Network Checks
	runDoctorNetworkChecks(&report)

	// Dependency Checks
	runDoctorDependencyChecks(&report)

	// Apply fixes if requested
	if fix {
		runDoctorApplyFixes(&report)
	}

	// Print summary
	printDoctorSummary(&report)

	// Exit with appropriate code
	if report.Failed > 0 {
		os.Exit(1)
	}
}

// runDoctorCommandWithDaemon runs enhanced diagnostics when daemon is running
func runDoctorCommandWithDaemon(conn net.Conn) {
	// First run standard diagnostics
	fix := len(os.Args) >= 3 && (os.Args[2] == "--fix" || os.Args[2] == "-f")

	fmt.Printf("\n%s[%s]%s %sRunning mpm Doctor (Enhanced)...%s\n\n", ansiBold, colorCyan("●"), ansiReset, ansiBold, ansiReset)

	report := DoctorReport{Checks: []DoctorCheck{}}

	// System Checks
	runDoctorSystemChecks(&report)

	// Environment Checks
	runDoctorEnvironmentChecks(&report)

	// Workspace Checks
	runDoctorWorkspaceChecks(&report)

	// Database Checks (enhanced with daemon access)
	runDoctorDatabaseChecksWithDaemon(&report, conn)

	// Network Checks
	runDoctorNetworkChecks(&report)

	// Dependency Checks
	runDoctorDependencyChecks(&report)

	// Daemon Status Check
	runDoctorDaemonStatusCheck(&report, conn)

	// Apply fixes if requested
	if fix {
		runDoctorApplyFixes(&report)
	}

	// Print summary
	printDoctorSummary(&report)

	// Exit with appropriate code
	if report.Failed > 0 {
		os.Exit(1)
	}
}

// Color helper functions (no external dependencies)
func colorCyan(s string) string {
	return "\033[36m" + s + "\033[0m"
}

func colorGreen(s string) string {
	return ansiGreen + s + ansiReset
}

func colorYellow(s string) string {
	return ansiYellow + s + ansiReset
}

func colorRed(s string) string {
	return ansiRed + s + ansiReset
}

func ansiBoldString(s string) string {
	return ansiBold + s + ansiReset
}

// Doctor check functions
func runDoctorSystemChecks(report *DoctorReport) {
	fmt.Printf("  %s%sSystem Information%s\n\n", ansiBold, colorCyan("▸"), ansiReset)

	// OS Check
	check := DoctorCheck{Name: "Operating System", Status: "PASS", Details: []string{}}
	check.Message = runtime.GOOS + "/" + runtime.GOARCH
	check.Details = append(check.Details, "Go Version: "+runtime.Version())
	check.Duration = "0ms"
	report.Checks = append(report.Checks, check)
	report.Passed++
	report.TotalChecks++
	fmt.Printf("    [%s] %s\n", colorGreen("PASS"), check.Name)
	fmt.Printf("          %s\n\n", check.Message)

	// User permissions
	uid := os.Getuid()
	check = DoctorCheck{Name: "User Permissions", Status: "PASS", Details: []string{}}
	check.Message = fmt.Sprintf("Running as UID %d", uid)
	if uid == 0 {
		check.Status = "WARN"
		check.Message = "Running as root (not recommended)"
		report.Warnings++
	} else {
		report.Passed++
	}
	check.Duration = "0ms"
	report.Checks = append(report.Checks, check)
	report.TotalChecks++
	statusStr := colorGreen("PASS")
	if check.Status == "WARN" {
		statusStr = colorYellow("WARN")
	}
	fmt.Printf("    [%s] %s\n", statusStr, check.Name)
	fmt.Printf("          %s\n\n", check.Message)
}

func runDoctorEnvironmentChecks(report *DoctorReport) {
	fmt.Printf("  %s%sEnvironment Variables%s\n\n", ansiBold, colorCyan("▸"), ansiReset)

	// Workspace
	workspace := config.GetWorkspace()
	if workspace == "" {
		report.Checks = append(report.Checks, DoctorCheck{
			Name:     "MPM_WORKSPACE",
			Status:   "WARN",
			Message:  "Not set (will use default)",
			Duration: "0ms",
		})
		report.Warnings++
		report.TotalChecks++
		fmt.Printf("    [%s] %s\n", colorYellow("WARN"), "MPM_WORKSPACE")
		fmt.Printf("          %s\n\n", "Not set - using default location")
	} else {
		report.Checks = append(report.Checks, DoctorCheck{
			Name:     "MPM_WORKSPACE",
			Status:   "PASS",
			Message:  workspace,
			Duration: "0ms",
		})
		report.Passed++
		report.TotalChecks++
		fmt.Printf("    [%s] %s\n", colorGreen("PASS"), "MPM_WORKSPACE")
		fmt.Printf("          %s\n\n", workspace)
	}

	// XDG_RUNTIME_DIR
	xdgRuntime := os.Getenv("XDG_RUNTIME_DIR")
	socketDir := sockPath
	if xdgRuntime != "" {
		socketDir = filepath.Join(xdgRuntime, "mpm.sock")
	}
	report.Checks = append(report.Checks, DoctorCheck{
		Name:     "Socket Path",
		Status:   "PASS",
		Message:  socketDir,
		Duration: "0ms",
	})
	report.Passed++
	report.TotalChecks++
	fmt.Printf("    [%s] %s\n", colorGreen("PASS"), "Socket Path")
	fmt.Printf("          %s\n\n", socketDir)

	// Webhook URL
	webhookURL := os.Getenv("MPM_WEBHOOK_URL")
	if webhookURL == "" {
		report.Checks = append(report.Checks, DoctorCheck{
			Name:     "Webhook URL",
			Status:   "WARN",
			Message:  "Not configured (webhooks disabled)",
			Duration: "0ms",
		})
		report.Warnings++
		report.TotalChecks++
		fmt.Printf("    [%s] %s\n", colorYellow("WARN"), "Webhook URL")
		fmt.Printf("          %s\n\n", "Not configured - heartbeats will be local only")
	} else {
		report.Checks = append(report.Checks, DoctorCheck{
			Name:     "Webhook URL",
			Status:   "PASS",
			Message:  webhookURL,
			Duration: "0ms",
		})
		report.Passed++
		report.TotalChecks++
		fmt.Printf("    [%s] %s\n", colorGreen("PASS"), "Webhook URL")
		fmt.Printf("          %s\n\n", webhookURL)
	}
}

func runDoctorWorkspaceChecks(report *DoctorReport) {
	fmt.Printf("  %s%sWorkspace Structure%s\n\n", ansiBold, colorCyan("▸"), ansiReset)

	workspace := config.GetWorkspace()
	if workspace == "" {
		report.Checks = append(report.Checks, DoctorCheck{
			Name:     "Workspace Directory",
			Status:   "FAIL",
			Message:  "Workspace not found",
			Duration: "0ms",
		})
		report.Failed++
		report.TotalChecks++
		fmt.Printf("    [%s] %s\n", colorRed("FAIL"), "Workspace Directory")
		fmt.Printf("          %s\n\n", "Cannot determine workspace path")
		return
	}

	// Check critical directories
	dirs := map[string]string{
		"mode":       filepath.Join(workspace, "mode"),
		"persona":    filepath.Join(workspace, "persona"),
		"src/db":     filepath.Join(workspace, "src", "db"),
		"sessions":   filepath.Join(workspace, "sessions"),
	}

	for name, path := range dirs {
		info, err := os.Stat(path)
		if err != nil && !os.IsNotExist(err) {
			// Unexpected error — log it and skip
			fmt.Printf("    [%s] %s\n", colorYellow("WARN"), name+" Directory")
			fmt.Printf("          Stat error: %v\n\n", err)
			continue
		}
		if os.IsNotExist(err) || info == nil {
			report.Checks = append(report.Checks, DoctorCheck{
				Name:     name + " Directory",
				Status:   "FAIL",
				Message:  "Missing: " + path,
				Duration: "0ms",
			})
			report.Failed++
			report.TotalChecks++
			fmt.Printf("    [%s] %s\n", colorRed("FAIL"), name+" Directory")
			fmt.Printf("          Missing: %s\n\n", path)
		} else if !info.IsDir() {
			report.Checks = append(report.Checks, DoctorCheck{
				Name:     name + " Directory",
				Status:   "FAIL",
				Message:  "Not a directory: " + path,
				Duration: "0ms",
			})
			report.Failed++
			report.TotalChecks++
			fmt.Printf("    [%s] %s\n", colorRed("FAIL"), name+" Directory")
			fmt.Printf("          Not a directory: %s\n\n", path)
		} else {
			report.Checks = append(report.Checks, DoctorCheck{
				Name:     name + " Directory",
				Status:   "PASS",
				Message:  path,
				Duration: "0ms",
			})
			report.Passed++
			report.TotalChecks++
			fmt.Printf("    [%s] %s\n", colorGreen("PASS"), name+" Directory")
			fmt.Printf("          %s\n\n", path)
		}
	}

	// Check socket directory permissions
	socketDir := filepath.Dir(sockPath)
	if err := testWritable(socketDir); err != nil {
		report.Checks = append(report.Checks, DoctorCheck{
			Name:     "Socket Directory Writable",
			Status:   "FAIL",
			Message:  "Cannot write to: " + socketDir,
			Details:  []string{err.Error()},
			Duration: "0ms",
		})
		report.Failed++
		report.TotalChecks++
		fmt.Printf("    [%s] %s\n", colorRed("FAIL"), "Socket Directory Writable")
		fmt.Printf("          Cannot write to: %s\n", socketDir)
		fmt.Printf("          Error: %s\n\n", err.Error())
	} else {
		report.Checks = append(report.Checks, DoctorCheck{
			Name:     "Socket Directory Writable",
			Status:   "PASS",
			Message:  socketDir,
			Duration: "0ms",
		})
		report.Passed++
		report.TotalChecks++
		fmt.Printf("    [%s] %s\n", colorGreen("PASS"), "Socket Directory Writable")
		fmt.Printf("          %s\n\n", socketDir)
	}
}

func runDoctorDatabaseChecks(report *DoctorReport) {
	fmt.Printf("  %s%sDatabase Integrity%s\n\n", ansiBold, colorCyan("▸"), ansiReset)

	// The database is ALWAYS at mpm/src/db/mpm_memory.db
	dbPath := filepath.Join(config.GetMPMDir(), "src", "db", "mpm_memory.db")

	info, err := os.Stat(dbPath)
	if err != nil || info.IsDir() {
		report.Checks = append(report.Checks, DoctorCheck{
			Name:     "Database File",
			Status:   "WARN",
			Message:  "No database found (first run?)",
			Duration: "0ms",
		})
		report.Warnings++
		report.TotalChecks++
		fmt.Printf("    [%s] %s\n", colorYellow("WARN"), "Database File")
		fmt.Printf("          No database found - daemon will create on first run\n\n")
		return
	}
	dbSize := info.Size()

	// Check database
	check := DoctorCheck{
		Name:    "Database File",
		Status:  "PASS",
		Message: dbPath,
		Details: []string{},
	}
	check.Details = append(check.Details, fmt.Sprintf("Size: %s", formatBytes(dbSize)))

	sqlDB, err := sql.Open("sqlite", dbPath+"?mode=ro")
	if err != nil {
		check.Status = "FAIL"
		check.Details = append(check.Details, "Cannot open: "+err.Error())
		report.Failed++
		report.TotalChecks++
		fmt.Printf("    [%s] %s\n", colorRed("FAIL"), "Database File")
		fmt.Printf("          Cannot open: %s\n\n", err.Error())
		return
	}
	defer sqlDB.Close()

	// Run integrity check
	var integrityResult string
	if err := sqlDB.QueryRow("PRAGMA integrity_check").Scan(&integrityResult); err != nil {
		check.Status = "FAIL"
		check.Details = append(check.Details, "Integrity check failed: "+err.Error())
		report.Failed++
		report.TotalChecks++
		fmt.Printf("    [%s] %s\n", colorRed("FAIL"), "Database Integrity")
		fmt.Printf("          Integrity check failed: %s\n\n", err.Error())
		return
	}

	if integrityResult != "ok" {
		check.Status = "FAIL"
		check.Details = append(check.Details, "Integrity check result: "+integrityResult)
		report.Failed++
		report.TotalChecks++
		fmt.Printf("    [%s] %s\n", colorRed("FAIL"), "Database Integrity")
		fmt.Printf("          Corruption detected: %s\n\n", integrityResult)
		return
	}

	// Quick stats
	var tableCount int
	sqlDB.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table'").Scan(&tableCount)
	check.Details = append(check.Details, fmt.Sprintf("Tables: %d", tableCount))

	report.Passed++
	report.TotalChecks++
	check.Status = "PASS"
	check.Message = "Database valid"
	check.Duration = "0ms"
	report.Checks = append(report.Checks, check)
	fmt.Printf("    [%s] %s\n", colorGreen("PASS"), "Database File")
	fmt.Printf("          %s (%s)\n", dbPath, formatBytes(dbSize))
	fmt.Printf("          Tables: %d | Integrity: OK\n\n", tableCount)
}

func runDoctorDatabaseChecksWithDaemon(report *DoctorReport, conn net.Conn) {
	// Run basic database checks first
	runDoctorDatabaseChecks(report)

	fmt.Printf("  %s%sDatabase Records (Daemon)%s\n\n", ansiBold, colorCyan("▸"), ansiReset)

	// Query daemon for database stats
	msg := Message{Args: []string{"status"}}
	if err := json.NewEncoder(conn).Encode(msg); err != nil {
		return
	}

	// Read response
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	dec := json.NewDecoder(conn)
	var resp Message
	if err := dec.Decode(&resp); err != nil {
		return
	}

	// Parse status for memory stats
	var status DaemonStatus
	if err := json.Unmarshal([]byte(resp.Output), &status); err == nil {
		if status.MemoryUsageKB > 0 {
			check := DoctorCheck{
				Name:     "Daemon Memory Usage",
				Status:   "PASS",
				Message:  fmt.Sprintf("%d KB", status.MemoryUsageKB),
				Duration: "0ms",
			}
			report.Checks = append(report.Checks, check)
			report.Passed++
			report.TotalChecks++
			fmt.Printf("    [%s] %s\n", colorGreen("PASS"), "Daemon Memory Usage")
			fmt.Printf("          %d KB\n\n", status.MemoryUsageKB)
		}
	}
}

func runDoctorNetworkChecks(report *DoctorReport) {
	fmt.Printf("  %s%sNetwork Connectivity%s\n\n", ansiBold, colorCyan("▸"), ansiReset)

	webhookURL := os.Getenv("MPM_WEBHOOK_URL")
	if webhookURL == "" {
		check := DoctorCheck{
			Name:     "Webhook Connectivity",
			Status:   "WARN",
			Message:  "No webhook configured",
			Duration: "0ms",
		}
		report.Checks = append(report.Checks, check)
		report.Warnings++
		report.TotalChecks++
		fmt.Printf("    [%s] %s\n", colorYellow("WARN"), "Webhook Connectivity")
		fmt.Printf("          Webhook not configured\n\n")
		return
	}

	// Perform HEAD request to webhook
	start := time.Now()
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Head(webhookURL)
	duration := time.Since(start)

	if err != nil {
		check := DoctorCheck{
			Name:     "Webhook Connectivity",
			Status:   "WARN",
			Message:  "Cannot reach webhook: " + err.Error(),
			Details:  []string{"Local logging still active"},
			Duration: duration.String(),
		}
		report.Checks = append(report.Checks, check)
		report.Warnings++
		report.TotalChecks++
		fmt.Printf("    [%s] %s\n", colorYellow("WARN"), "Webhook Connectivity")
		fmt.Printf("          Cannot reach: %s\n", webhookURL)
		fmt.Printf("          Error: %s\n", err.Error())
		fmt.Printf("          (Local logging still active)\n\n")
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		check := DoctorCheck{
			Name:     "Webhook Connectivity",
			Status:   "PASS",
			Message:  fmt.Sprintf("HTTP %d - API reachable", resp.StatusCode),
			Duration: duration.String(),
		}
		report.Checks = append(report.Checks, check)
		report.Passed++
		report.TotalChecks++
		fmt.Printf("    [%s] %s\n", colorGreen("PASS"), "Webhook Connectivity")
		fmt.Printf("          HTTP %d | Latency: %s\n\n", resp.StatusCode, duration)
	} else {
		check := DoctorCheck{
			Name:     "Webhook Connectivity",
			Status:   "WARN",
			Message:  fmt.Sprintf("HTTP %d - check API key", resp.StatusCode),
			Duration: duration.String(),
		}
		report.Checks = append(report.Checks, check)
		report.Warnings++
		report.TotalChecks++
		fmt.Printf("    [%s] %s\n", colorYellow("WARN"), "Webhook Connectivity")
		fmt.Printf("          HTTP %d | Latency: %s\n", resp.StatusCode, duration)
		fmt.Printf("          (Check if API key is valid)\n\n")
	}
}

func runDoctorDependencyChecks(report *DoctorReport) {
	fmt.Printf("  %s%sExternal Dependencies%s\n\n", ansiBold, colorCyan("▸"), ansiReset)

	// Check for common optional tools
	tools := []struct {
		name    string
		command string
		required bool
	}{
		{"fzf", "fzf --version", false},
		{"sqlite3", "sqlite3 --version", false},
		{"git", "git --version", false},
	}

	for _, tool := range tools {
		_, err := exec.LookPath(tool.command)
		if err != nil {
			status := "WARN"
			report.Warnings++
			fmt.Printf("    [%s] %s\n", colorYellow(status), tool.name)
			fmt.Printf("          Not found (optional)\n\n")
			check := DoctorCheck{
				Name:     tool.name,
				Status:   status,
				Message:  "Not found (optional)",
				Duration: "0ms",
			}
			report.Checks = append(report.Checks, check)
			report.TotalChecks++
		} else {
			// Get version
			out, _ := exec.Command(tool.name, "--version").Output()
			version := strings.TrimSpace(string(out))
			if len(version) > 50 {
				version = version[:50] + "..."
			}
			report.Passed++
			report.TotalChecks++
			fmt.Printf("    [%s] %s\n", colorGreen("PASS"), tool.name)
			fmt.Printf("          %s\n\n", version)
			check := DoctorCheck{
				Name:     tool.name,
				Status:   "PASS",
				Message:  version,
				Duration: "0ms",
			}
			report.Checks = append(report.Checks, check)
		}
	}
}

func runDoctorDaemonStatusCheck(report *DoctorReport, conn net.Conn) {
	fmt.Printf("  %s%sDaemon Status%s\n\n", ansiBold, colorCyan("▸"), ansiReset)

	// Query daemon status
	msg := Message{Args: []string{"status"}}
	json.NewEncoder(conn).Encode(msg)

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	dec := json.NewDecoder(conn)
	var resp Message
	if err := dec.Decode(&resp); err != nil {
		check := DoctorCheck{
			Name:     "Daemon Response",
			Status:   "FAIL",
			Message:  "No response from daemon",
			Duration: "0ms",
		}
		report.Checks = append(report.Checks, check)
		report.Failed++
		report.TotalChecks++
		fmt.Printf("    [%s] %s\n", colorRed("FAIL"), "Daemon Response")
		fmt.Printf("          No response from daemon\n\n")
		return
	}

	var status DaemonStatus
	if err := json.Unmarshal([]byte(resp.Output), &status); err != nil {
		check := DoctorCheck{
			Name:     "Daemon Status Parse",
			Status:   "FAIL",
			Message:  "Cannot parse daemon response",
			Duration: "0ms",
		}
		report.Checks = append(report.Checks, check)
		report.Failed++
		report.TotalChecks++
		fmt.Printf("    [%s] %s\n", colorRed("FAIL"), "Daemon Status Parse")
		fmt.Printf("          Cannot parse daemon response\n\n")
		return
	}

	// Check daemon health
	check := DoctorCheck{
		Name:    "Daemon Process",
		Status:  "PASS",
		Details: []string{},
	}
	check.Message = fmt.Sprintf("PID %d | Uptime: %s", status.PID, status.Uptime)
	check.Details = append(check.Details, fmt.Sprintf("Workers: %d/%d", status.ActiveWorkers, status.MaxWorkers))
	check.Details = append(check.Details, fmt.Sprintf("Tasks: %d total", status.TotalTasks))
	check.Details = append(check.Details, fmt.Sprintf("Pre-flight: %s", status.PreFlightStatus))

	report.Passed++
	report.TotalChecks++
	report.Checks = append(report.Checks, check)
	fmt.Printf("    [%s] %s\n", colorGreen("PASS"), "Daemon Process")
	fmt.Printf("          PID: %d | Uptime: %s\n", status.PID, status.Uptime)
	fmt.Printf("          Workers: %d/%d | Tasks: %d total\n", status.ActiveWorkers, status.MaxWorkers, status.TotalTasks)
	fmt.Printf("          Pre-flight: %s\n\n", status.PreFlightStatus)
}

func runDoctorApplyFixes(report *DoctorReport) {
	fmt.Printf("  %s%sApplying Fixes%s\n\n", ansiBold, colorCyan("▸"), ansiReset)

	// Fix socket directory permissions
	socketDir := filepath.Dir(sockPath)
	if err := os.Chmod(socketDir, 0755); err == nil {
		fmt.Printf("    [%s] %s\n", colorGreen("FIXED"), "Socket Directory Permissions")
		fmt.Printf("          chmod 755 %s\n\n", socketDir)
	} else {
		fmt.Printf("    [%s] %s\n", colorRed("FAIL"), "Socket Directory Permissions")
		fmt.Printf("          Cannot fix: %s\n\n", err.Error())
	}

	// Fix database directory permissions (always mpm/src/db/)
	dbDir := filepath.Join(config.GetMPMDir(), "src", "db")
	if err := os.Chmod(dbDir, 0755); err == nil {
		fmt.Printf("    [%s] %s\n", colorGreen("FIXED"), "Database Directory Permissions")
		fmt.Printf("          chmod 755 %s\n\n", dbDir)
	} else {
		fmt.Printf("    [%s] %s\n", colorRed("FAIL"), "Database Directory Permissions")
		fmt.Printf("          Cannot fix: %s\n\n", err.Error())
	}
}

func printDoctorSummary(report *DoctorReport) {
	total := report.TotalChecks
	fmt.Printf("%s%s─────────────────────────────────────────────────────────────%s\n", ansiBold, colorCyan("─"), ansiReset)
	fmt.Printf("\n  %s%sSummary%s\n\n", ansiBold, ansiBoldString("▸"), ansiReset)

	fmt.Printf("    Total Checks:  %d\n", total)
	fmt.Printf("    %s Passed:  %d%s\n", colorGreen("●"), report.Passed, ansiReset)
	if report.Warnings > 0 {
		fmt.Printf("    %s Warnings: %d%s\n", colorYellow("●"), report.Warnings, ansiReset)
	}
	if report.Failed > 0 {
		fmt.Printf("    %s Failed:  %d%s\n", colorRed("●"), report.Failed, ansiReset)
	}
	fmt.Printf("\n")

	if report.Failed > 0 {
		fmt.Printf("  %s  Some checks failed. Run 'mpm doctor --fix' to attempt repairs.%s\n\n", colorRed("!"), ansiReset)
	} else if report.Warnings > 0 {
		fmt.Printf("  %s  All critical checks passed. Review warnings above.%s\n\n", colorYellow("!"), ansiReset)
	} else {
		fmt.Printf("  %s  All systems operational.%s\n\n", colorGreen("✓"), ansiReset)
	}
}

// ============================================================================
// Pre-Flight Health Check System
// ============================================================================

// runPreFlightChecks executes the diagnostic suite and returns results
// Target: Complete within 1-2 seconds for fast startup
func runPreFlightChecks() *PreFlightResult {
	startTime := time.Now()
	result := &PreFlightResult{
		Timestamp: startTime,
		Checks:    []PreFlightCheck{},
	}

	// Run all checks (order: fastest first, critical last)
	runLockfileCheck(result)
	runDirectoryCheck(result)
	runDatabaseCheck(result)
	runPersonaCheck(result)
	runPermissionsCheck(result)

	result.DurationMs = time.Since(startTime).Milliseconds()

	// Determine overall status
	hasError := false
	hasRepair := false
	for _, check := range result.Checks {
		if check.Status == "Error" {
			hasError = true
			break
		}
		if check.Status == "Repaired" {
			hasRepair = true
		}
	}
	if hasError {
		result.Status = "Failed"
	} else if hasRepair {
		result.Status = "Repaired"
	} else {
		result.Status = "Passed"
	}

	return result
}

// runLockfileCheck removes stale lockfiles from previous crash
func runLockfileCheck(result *PreFlightResult) {
	start := time.Now()
	details := []string{}

	// Check for common lockfile patterns
	lockPatterns := []string{
		filepath.Join(os.TempDir(), "mpm.lock"),
		filepath.Join(os.TempDir(), "mpm-daemon.lock"),
	}

	for _, lockPath := range lockPatterns {
		info, err := os.Stat(lockPath)
		if err == nil && !info.IsDir() {
			// Lockfile exists - check if it's stale (>24h old)
			if time.Since(info.ModTime()) > 24*time.Hour {
				if err := os.Remove(lockPath); err == nil {
					details = append(details, fmt.Sprintf("Removed stale lockfile: %s", lockPath))
				}
			}
		}
	}

	// Also check socket directory for orphaned sockets without daemon
	socketInfo, err := os.Stat(sockPath)
	if err == nil && socketInfo.Mode()&os.ModeSocket != 0 {
		// Socket exists but no one is listening - try to connect
		conn, err := net.DialTimeout("unix", sockPath, 100*time.Millisecond)
		if err != nil {
			// No one listening - orphaned socket
			os.Remove(sockPath)
			details = append(details, fmt.Sprintf("Removed orphaned socket: %s", sockPath))
		} else {
			conn.Close()
		}
	}

	status := "OK"
	message := "No stale lockfiles found"
	if len(details) > 0 {
		status = "Repaired"
		message = fmt.Sprintf("Cleaned %d stale lockfile(s)", len(details))
	}

	result.Checks = append(result.Checks, PreFlightCheck{
		Name:       "Lockfile Cleanup",
		Status:     status,
		Message:    message,
		Details:    details,
		DurationMs: time.Since(start).Milliseconds(),
	})
}

// runDirectoryCheck ensures all required directories exist
func runDirectoryCheck(result *PreFlightResult) {
	start := time.Now()
	details := []string{}

	// Determine workspace and data directories
	workspace := config.GetWorkspace()
	dirs := []string{
		filepath.Dir(sockPath),                 // Socket directory (XDG_RUNTIME_DIR or ~/.mpm)
		filepath.Join(workspace, "mode"),       // Mode configurations
		filepath.Join(workspace, "persona"),    // Persona configurations
		filepath.Join(workspace, "src", "db"),  // Database directory
	}

	// Also check sessions directory (may not exist yet)
	sessionsPath := getSessionsDir()
	if sessionsPath != "" {
		dirs = append(dirs, sessionsPath)
	}

	for _, dir := range dirs {
		info, err := os.Stat(dir)
		if os.IsNotExist(err) {
			// Create missing directory
			if err := os.MkdirAll(dir, 0755); err != nil {
				result.Checks = append(result.Checks, PreFlightCheck{
					Name:    "Directory Check",
					Status:  "Error",
					Message: fmt.Sprintf("Failed to create directory: %s", dir),
					Details: []string{err.Error()},
				})
				return
			}
			details = append(details, fmt.Sprintf("Created: %s", dir))
		} else if err != nil || !info.IsDir() {
			result.Checks = append(result.Checks, PreFlightCheck{
				Name:    "Directory Check",
				Status:  "Error",
				Message: fmt.Sprintf("Path exists but is not a directory: %s", dir),
				Details: []string{},
			})
			return
		}
	}

	status := "OK"
	message := "All required directories exist"
	if len(details) > 0 {
		status = "Repaired"
		message = fmt.Sprintf("Created %d missing directory(ies)", len(details))
	}

	result.Checks = append(result.Checks, PreFlightCheck{
		Name:       "Directory Check",
		Status:     status,
		Message:    message,
		Details:    details,
		DurationMs: time.Since(start).Milliseconds(),
	})
}

// runDatabaseCheck validates database integrity (pre-flight)
func runDatabaseCheck(result *PreFlightResult) {
	start := time.Now()

	// The database is ALWAYS at mpm/src/db/mpm_memory.db
	dbPath := filepath.Join(config.GetMPMDir(), "src", "db", "mpm_memory.db")

	info, err := os.Stat(dbPath)
	if err != nil || info.IsDir() {
		// No database found - this is OK for first run
		result.Checks = append(result.Checks, PreFlightCheck{
			Name:      "Database Integrity",
			Status:    "OK",
			Message:   "No database file found (first run expected)",
			Details:   []string{},
			DurationMs: time.Since(start).Milliseconds(),
		})
		return
	}
	dbSize := info.Size()

	// Database exists - check if it's readable and valid
	details := []string{fmt.Sprintf("Database: %s (%s)", dbPath, formatBytes(dbSize))}

	// Try to open the database with SQLite
	sqlDB, err := openDatabase(dbPath)
	if err != nil {
		result.Checks = append(result.Checks, PreFlightCheck{
			Name:      "Database Integrity",
			Status:    "Error",
			Message:   fmt.Sprintf("Cannot open database: %v", err),
			Details:   details,
			DurationMs: time.Since(start).Milliseconds(),
		})
		return
	}
	defer sqlDB.Close()

	// Run PRAGMA integrity_check
	var integrityResult string
	row := sqlDB.QueryRow("PRAGMA integrity_check")
	if err := row.Scan(&integrityResult); err != nil {
		details = append(details, fmt.Sprintf("Integrity check failed to run: %v", err))
		result.Checks = append(result.Checks, PreFlightCheck{
			Name:      "Database Integrity",
			Status:    "Warning",
			Message:   "Integrity check query failed",
			Details:   details,
			DurationMs: time.Since(start).Milliseconds(),
		})
		return
	}

	if integrityResult != "ok" {
		details = append(details, fmt.Sprintf("Integrity check result: %s", integrityResult))
		result.Checks = append(result.Checks, PreFlightCheck{
			Name:      "Database Integrity",
			Status:    "Error",
			Message:   "Database corruption detected",
			Details:   details,
			DurationMs: time.Since(start).Milliseconds(),
		})
		return
	}

	details = append(details, "Integrity check: ok")

	// Quick table count check (non-blocking)
	var tableCount int
	sqlDB.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table'").Scan(&tableCount)
	details = append(details, fmt.Sprintf("Tables: %d", tableCount))

	result.Checks = append(result.Checks, PreFlightCheck{
		Name:      "Database Integrity",
		Status:    "OK",
		Message:   "Database valid",
		Details:   details,
		DurationMs: time.Since(start).Milliseconds(),
	})
}

// openDatabase opens a SQLite database and returns the connection
// Uses modernc.org/sqlite (pure Go, no cgo)
func openDatabase(path string) (*sql.DB, error) {
	return sql.Open("sqlite", path+"?mode=ro") // Read-only mode for checks
}

// runPersonaCheck validates active persona exists
func runPersonaCheck(result *PreFlightResult) {
	start := time.Now()

	personaPath := filepath.Join(config.GetWorkspace(), "persona")
	details := []string{}

	// Check if persona directory exists
	if _, err := os.Stat(personaPath); os.IsNotExist(err) {
		// Persona directory missing - CRITICAL
		result.Checks = append(result.Checks, PreFlightCheck{
			Name:      "Persona Validation",
			Status:    "Error",
			Message:   "Persona directory missing: " + personaPath,
			Details:   []string{"Daemon cannot start without persona directory"},
			DurationMs: time.Since(start).Milliseconds(),
		})
		return
	}
	details = append(details, fmt.Sprintf("Persona dir: %s", personaPath))

	// Check for active persona marker or default persona
	defaultPersonaPath := filepath.Join(personaPath, "default.json")
	activePersonaPath := filepath.Join(personaPath, "active.json")

	defaultExists := fileExists(defaultPersonaPath)
	activeExists := fileExists(activePersonaPath)

	if !defaultExists && !activeExists {
		// No persona found - CRITICAL (daemon needs at least a default)
		result.Checks = append(result.Checks, PreFlightCheck{
			Name:      "Persona Validation",
			Status:    "Error",
			Message:   "No default or active persona found",
			Details:   details,
			DurationMs: time.Since(start).Milliseconds(),
		})
		return
	}

	if defaultExists {
		details = append(details, fmt.Sprintf("Default persona: %s", defaultPersonaPath))
	}
	if activeExists {
		details = append(details, fmt.Sprintf("Active persona: %s", activePersonaPath))
	}

	// Count available personas
	entries, err := os.ReadDir(personaPath)
	if err == nil {
		personaCount := 0
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
				personaCount++
			}
		}
		details = append(details, fmt.Sprintf("Total personas: %d", personaCount))
	}

	result.Checks = append(result.Checks, PreFlightCheck{
		Name:      "Persona Validation",
		Status:    "OK",
		Message:   "Persona configuration valid",
		Details:   details,
		DurationMs: time.Since(start).Milliseconds(),
	})
}

// runPermissionsCheck verifies R/W permissions on critical paths
func runPermissionsCheck(result *PreFlightResult) {
	start := time.Now()
	details := []string{}

	// Critical paths that must be writable
	writablePaths := []string{
		filepath.Dir(sockPath),                 // Socket directory
		filepath.Join(config.GetWorkspace(), "src", "db"), // DB directory
	}

	// Check read/write access
	for _, path := range writablePaths {
		if err := testWritable(path); err != nil {
			details = append(details, fmt.Sprintf("NOT WRITABLE: %s", path))
		} else {
			details = append(details, fmt.Sprintf("WRITABLE: %s", path))
		}
	}

	status := "OK"
	message := "All critical paths have correct permissions"
	if len(details) > 0 {
		hasError := false
		for _, d := range details {
			if strings.HasPrefix(d, "NOT") {
				hasError = true
				break
			}
		}
		if hasError {
			status = "Error"
			message = "Some critical paths are not writable"
		}
	}

	result.Checks = append(result.Checks, PreFlightCheck{
		Name:       "Permissions Check",
		Status:     status,
		Message:    message,
		Details:    details,
		DurationMs: time.Since(start).Milliseconds(),
	})
}

// testWritable checks if a directory is writable by attempting to create a temp file
func testWritable(dir string) error {
	testFile := filepath.Join(dir, ".mpm-perm-test-"+fmt.Sprintf("%d", os.Getpid()))
	defer os.Remove(testFile)
	f, err := os.Create(testFile)
	if err != nil {
		return err
	}
	f.Close()
	return nil
}

// getSessionsDir returns the sessions directory path
func getSessionsDir() string {
	// Try config first
	if config, err := config.LoadConfig(); err == nil && config.SessionsDir != "" {
		return config.SessionsDir
	}
	// Fallback to workspace
	workspace := config.GetWorkspace()
	return filepath.Join(workspace, "mpm", "sessions")
}

// fileExists checks if a file exists
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// formatBytes returns a human-readable byte count
func formatBytes(bytes int64) string {
	const unit = 1024
	if bytes < unit {
		return fmt.Sprintf("%d B", bytes)
	}
	div, exp := int64(unit), 0
	for n := bytes / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(bytes)/float64(div), "KMGTPE"[exp])
}

// ============================================================================
// Watch Daemon Control
// ============================================================================

// startWatchDaemon starts the watch subprocess
func startWatchDaemon() error {
	if watchPid != 0 {
		return fmt.Errorf("watch daemon already running (PID: %d)", watchPid)
	}

	binary, err := os.Executable()
	if err != nil {
		return fmt.Errorf("failed to get executable: %w", err)
	}

	cmd := exec.Command(binary, "watch")
	// Filter daemon state from subprocess env - let watch be standalone
	filteredEnv := os.Environ()
	var newEnv []string
	for _, e := range filteredEnv {
		if !strings.HasPrefix(e, "MPM_STATE=") && !strings.HasPrefix(e, "MPM_SOCKET_PATH=") {
			newEnv = append(newEnv, e)
		}
	}
	cmd.Env = newEnv
	cmd.Env = append(cmd.Env, "MPM_STATE=direct")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("failed to start watch: %w", err)
	}

	watchPid = cmd.Process.Pid
	watchCmd = cmd
	watchDone = make(chan bool)

	logInfo("watch", daemonPid, fmt.Sprintf("Watch daemon started (PID: %d)", watchPid))

	// Monitor the subprocess in background
	go func() {
		cmd.Wait()
		watchPid = 0
		watchCmd = nil
		select {
		case watchDone <- true:
		default:
		}
		logInfo("watch", daemonPid, "Watch daemon exited")
	}()

	return nil
}

// stopWatchDaemon sends SIGTERM to the watch subprocess and waits for it to exit.
// Uses targeted signal (not process group) to avoid killing unrelated processes.
func stopWatchDaemon() error {
	fmt.Fprintf(os.Stderr, "[DEBUG] stopWatchDaemon: watchPid=%d, watchCmd=%v\n", watchPid, watchCmd)
	if watchPid == 0 {
		return fmt.Errorf("watch daemon not running")
	}

	proc, err := os.FindProcess(watchPid)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[DEBUG] stopWatchDaemon: FindProcess error: %v\n", err)
		return fmt.Errorf("failed to find watch process: %w", err)
	}
	fmt.Fprintf(os.Stderr, "[DEBUG] stopWatchDaemon: sending SIGTERM to PID %d\n", watchPid)

	// Send SIGTERM to the specific PID only (not the process group).
	// Using process group (-proc.Pid) can affect unrelated processes if
	// the PGID happens to match another process, or reparent the child to systemd.
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		// Process may have already exited
		if !errors.Is(err, os.ErrProcessDone) {
			return fmt.Errorf("failed to signal watch: %w", err)
		}
	}

	// Wait for the watch process to actually exit so it doesn't get reparented to systemd.
	// This also prevents the goroutine in startWatchDaemon from racing with pid cleanup.
	if watchCmd != nil {
		watchCmd.Wait()
		watchCmd = nil
	}

	logInfo("watch", daemonPid, fmt.Sprintf("Watch daemon stopped (PID: %d was)", watchPid))
	watchPid = 0

	return nil
}

// restartWatchDaemon restarts the watch subprocess
func restartWatchDaemon() error {
	if watchPid != 0 {
		if err := stopWatchDaemon(); err != nil {
			logWarn("watch", daemonPid, fmt.Sprintf("Failed to stop watch during restart: %v", err))
		}
		// Wait for cleanup
		time.Sleep(100 * time.Millisecond)
	}
	return startWatchDaemon()
}

// getWatchStatus returns current watch daemon status
func getWatchStatus() map[string]interface{} {
	status := map[string]interface{}{
		"running": watchPid != 0,
		"pid":     watchPid,
	}
	if watchPid != 0 {
		status["status"] = "running"
	} else {
		status["status"] = "stopped"
	}
	return status
}

// ============================================================================
// Lifecycle Management (Shutdown/Reboot)
// ============================================================================

// initLifecycle initializes the lifecycle handler channel
func initLifecycle() {
	lifecycleChan = make(chan *LifecycleOp, 1)
	go lifecycleWorker()
}

// lifecycleWorker processes shutdown and reboot requests
func lifecycleWorker() {
	for op := range lifecycleChan {
		switch op.Type {
		case "shutdown":
			handleShutdown(op.Force, op.Conn)
		case "reboot":
			handleReboot(op.Force, op.Conn)
		}
	}
}

// activateDefaultMode activates the default 808 mode on daemon startup
// This sets The_Great_808 as the default silicon-native lobster deity identity
func activateDefaultMode() {
	// Only activate in 808 build
	if !strings.Contains(buildVersion, "808") {
		return
	}

	// Set the default 808 mode
	defaultMode := "~m.808"

	// Initialize mode manager if not already done
	if modeManager == nil {
		configPath := os.ExpandEnv("$HOME/.openclaw/workspace/projects/mpm")
		modeManager = mpminternal.NewModeManager(configPath)
	}

	// Ensure the active.json file exists (type assertion)
	if mm, ok := modeManager.(interface{ InitDB() error }); ok {
		if err := mm.InitDB(); err != nil {
			logWarn("mode", daemonPid, fmt.Sprintf("Mode InitDB: %v", err))
		}
	}

	// Set the default 808 mode (type assertion)
	if mm, ok := modeManager.(interface{ SetActive([]string) error }); ok {
		if err := mm.SetActive([]string{defaultMode}); err != nil {
			logWarn("mode", daemonPid, fmt.Sprintf("Failed to activate default 808 mode: %v", err))
			return
		}
		logInfo("mode", daemonPid, "Activated default mode: ~m.808 (The_Great_808)")
	}
}

// getLifecycleCommand checks if args contain a lifecycle command
// Returns the parsed LifecycleOp or nil
func getLifecycleCommand() *LifecycleOp {
	args := os.Args[1:]
	if len(args) == 0 {
		return nil
	}

	// Parse lifecycle commands
	var op *LifecycleOp

	switch args[0] {
	case "shutdown", "stop":
		op = &LifecycleOp{Type: "shutdown"}
	case "reboot", "restart":
		op = &LifecycleOp{Type: "reboot"}
	default:
		return nil
	}

	// Check for flags
	for _, arg := range args[1:] {
		if arg == "--force" || arg == "-f" {
			op.Force = true
		}
		// Note: --ui/-i flags are detected by client for TUI launch
		// but passed through to daemon for logging purposes
	}

	return op
}

// handleLifecycleClient sends lifecycle command to daemon and waits for response
func handleLifecycleClient(op *LifecycleOp) bool {
	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: Could not connect to daemon: %v\n", err)
		return true
	}
	defer conn.Close()

	// Send the command
	msg := Message{
		Args:     []string{op.Type},
		ExitCode: 0,
	}
	if op.Force {
		msg.Args = append(msg.Args, "--force")
	}

	if err := json.NewEncoder(conn).Encode(msg); err != nil {
		fmt.Fprintf(os.Stderr, "Error: Failed to send command: %v\n", err)
		return true
	}

	// Read phased response
	dec := json.NewDecoder(conn)

	for {
		var resp Message
		if err := dec.Decode(&resp); err != nil {
			if err == io.EOF {
				break
			}
			fmt.Fprintf(os.Stderr, "Error: Connection closed: %v\n", err)
			return true
		}

		if resp.Output != "" {
			// Check for phase markers (for visual feedback)
			if strings.Contains(resp.Output, "[1/3]") {
				fmt.Print(resp.Output)
			} else if strings.Contains(resp.Output, "[2/3]") {
				fmt.Print(resp.Output)
			} else if strings.Contains(resp.Output, "[3/3]") {
				fmt.Print(resp.Output)
			} else {
				fmt.Print(resp.Output)
			}
		}

		if resp.Done {
			if resp.ExitCode != 0 && resp.Error != "" {
				fmt.Fprintf(os.Stderr, "Error: %s\n", resp.Error)
			}
			os.Exit(resp.ExitCode)
		}
	}

	return true
}

// handleShutdown performs graceful daemon shutdown
func handleShutdown(force bool, conn net.Conn) {
	enc := json.NewEncoder(conn)
	fmt.Fprintf(os.Stderr, "[DEBUG] handleShutdown called (shuttingDown=%v, rebooting=%v)\n", isShuttingDown.Load(), isRebooting.Load())

	if !force {
		// Phase 1: Save session
		enc.Encode(Message{Output: "\n  🔐 [1/3] Saving session...\n", Done: false})
		flushSession()
	}

	// Phase 2: Terminate
	enc.Encode(Message{Output: "  ⏹  [2/3] Terminating daemon...\n", Done: false})

	// Phase 3: Exit
	enc.Encode(Message{Output: "  ✅ [3/3] Daemon stopped.\n", Done: true, ExitCode: 0})

	// Give response time to be sent, then exit
	// NOTE: We must call handleExit synchronously here, not in a goroutine.
	// If we spawn a goroutine and os.Exit(0) in dispatchDaemon fires first,
	// the goroutine never runs and the daemon never actually exits.
	time.Sleep(200 * time.Millisecond)
	handleExit()
}

// handleReboot performs daemon restart
func handleReboot(force bool, conn net.Conn) {
	enc := json.NewEncoder(conn)

	if !force {
		// Phase 1: Save session
		enc.Encode(Message{Output: "\n  🔐 [1/3] Saving session...\n", Done: false})
		flushSession()
	}

	// Phase 2: Restarting
	enc.Encode(Message{Output: "  🔄 [2/3] Restarting daemon...\n", Done: false})

	// Signal that we're rebooting (not just stopping)
	isRebooting.Store(true)

	// Phase 3: Spawn new process and exit
	enc.Encode(Message{Output: "  ✅ [3/3] Daemon restarted.\n", Done: true, ExitCode: 0})

	// Give response time to be sent, then do the actual reboot
	go func() {
		time.Sleep(200 * time.Millisecond)
		executeReboot()
	}()
}

// flushSession - sessions are now auto-saved at startup, no manual save needed
func flushSession() {
	logInfo("lifecycle", daemonPid, "Session auto-saved at startup (ss command deprecated)")
}

// executeReboot spawns a new daemon process and exits the current one
func executeReboot() {
	// Step 1: Remove the socket file FIRST (atomic guarantee)
	// This ensures no new connections can be made to the old daemon
	socketToRemove := sockPath
	listenerMutex.Lock()
	if listener != nil {
		listener.Close()
	}
	listenerMutex.Unlock()

	// Remove socket file to signal we're going down
	os.Remove(socketToRemove)

	// Step 2: Fork a new process
	// We use syscall.ForkExec for proper process inheritance
	binary, err := os.Executable()
	if err != nil {
		logError("lifecycle", daemonPid, fmt.Sprintf("Reboot failed: could not get executable: %v", err))
		handleExit() // Fall back to regular exit
		return
	}

	// Prepare environment - inherit all current env vars
	env := os.Environ()

	// Add MPM_DIRECT=1 to child so it knows to run as daemon
	env = append(env, "MPM_DIRECT=1")

	// Use syscall.ForkExec for clean process replacement
	// This replaces the current process entirely
	attr := &syscall.ProcAttr{
		Dir:   "", // Inherit current directory
		Env:   env,
		Files: []uintptr{os.Stdin.Fd(), os.Stdout.Fd(), os.Stderr.Fd()},
		Sys:   &syscall.SysProcAttr{},
	}

	// ForkExec the new daemon
	_, err = syscall.ForkExec(binary, []string{binary}, attr)
	if err != nil {
		logError("lifecycle", daemonPid, fmt.Sprintf("Reboot failed: %v", err))
		handleExit() // Fall back to regular exit
		return
	}

	logInfo("lifecycle", daemonPid, "Reboot complete - new daemon started")
	handleExit()
}

// ============================================================================
// Command Dispatcher (preserves existing handler functions)
// ============================================================================

// socketPath returns the daemon socket path, preferring user-specific locations
func socketPath() string {
	// Try XDG_RUNTIME_DIR first (Linux/BSD standard)
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		sockPath := filepath.Join(xdg, "mpm.sock")
		os.MkdirAll(xdg, 0700)
		return sockPath
	}
	// Fallback to ~/.mpm/mpm.sock for portability
	if home, err := os.UserHomeDir(); err == nil {
		mpmDir := filepath.Join(home, ".mpm")
		os.MkdirAll(mpmDir, 0700)
		return filepath.Join(mpmDir, "mpm.sock")
	}
	// Last resort - /tmp (note: shared in multi-user systems!)
	return "/tmp/mpm.sock"
}

// acquireDaemonLock attempts to acquire a PID-based lock file.
// If the lock file exists and the PID inside is still alive, it returns an error.
// If the lock file is stale (PID no longer alive), it is cleared and acquisition proceeds.
func acquireDaemonLock(lockPath string) error {
	// Check if lock file exists
	info, err := os.Stat(lockPath)
	if err == nil && !info.IsDir() {
		// Lock file exists — read the PID and check if it's alive
		data, readErr := os.ReadFile(lockPath)
		if readErr == nil {
			trimmed := strings.TrimSpace(string(data))
			if pid, parseErr := strconv.Atoi(trimmed); parseErr == nil {
				if isProcessAlive(pid) {
					// Another daemon is still running with this socket
					return fmt.Errorf("daemon already running (PID %d) — use 'mpm stop' first", pid)
				}
				// Stale lock — PID is dead, clear it
				os.Remove(lockPath)
			}
		}
		// Couldn't read or parse — remove and retry
		os.Remove(lockPath)
	}

	// Write our PID into the lock file
	if err := os.WriteFile(lockPath, []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		return fmt.Errorf("failed to write lock file: %w", err)
	}
	return nil
}

// releaseDaemonLock removes the PID lock file if it belongs to this process.
// This is called on clean shutdown; stale locks are cleaned up by the next startup.
func releaseDaemonLock(lockPath string) {
	data, err := os.ReadFile(lockPath)
	if err != nil {
		return
	}
	trimmed := strings.TrimSpace(string(data))
	if pid, err := strconv.Atoi(trimmed); err == nil && pid == os.Getpid() {
		os.Remove(lockPath)
	}
}

// isProcessAlive checks if a process with the given PID is currently running.
func isProcessAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// Send signal 0 — doesn't actually send anything, just checks if process exists
	err = p.Signal(syscall.Signal(0))
	return err == nil
}

// printHelp displays the mpm help text with SymAI branding
func printHelp() {
	fmt.Println()
	fmt.Println("  mpm - Memory-Persona-Mode Manager")
	fmt.Println()
	fmt.Println("Usage: mpm <command> [subcommand] [options]")
	fmt.Println()
	fmt.Println("Core Commands:")
	fmt.Println("  status               Show current status")
	fmt.Println("  dashboard            Live terminal dashboard")
	fmt.Println("  web [port]           Start HTTP web dashboard")
	fmt.Println("  persona              Persona management")
	fmt.Println("  mode                 Mode management")
	fmt.Println("  memory               Memory management")
	fmt.Println("  session              Session management")
	fmt.Println("  ss                   Quick session save")
	fmt.Println("  compile              Compile JSON files to database")
	fmt.Println("  reference            Reference library management")
	fmt.Println()
	fmt.Println("System Management:")
	fmt.Println("  shutdown             Graceful daemon shutdown")
	fmt.Println("  shutdown --force     Immediate shutdown (skip session save)")
	fmt.Println("  reboot               Graceful daemon restart")
	fmt.Println("  reboot --force       Immediate restart (skip session save)")
	fmt.Println("  logs                 Stream daemon log entries")
	fmt.Println()
	fmt.Println("Gateway (Daemon) Commands:")
	fmt.Println("  gateway              Gateway control (see 'mpm gateway help')")
	fmt.Println("  gateway help        Show gateway commands")
	fmt.Println("  gateway start       Start or connect to gateway")
	fmt.Println("  gateway stop        Stop the gateway")
	fmt.Println("  gateway restart     Restart the gateway")
	fmt.Println("  gateway status      Show gateway status")
	fmt.Println()
	fmt.Println("Run 'mpm <command> help' for more options (e.g., 'mpm session help')")
	fmt.Println()
	fmt.Println("Quick Examples:")
	fmt.Println("  ~p                              Select persona (fzf)")
	fmt.Println("  ~m                              Select modes (fzf)")
	fmt.Println("  mpm ss                          Quick session save")
	fmt.Println("  mpm persona set ~p.default      Activate persona")
	fmt.Println("  mpm mode add ~m.code            Add mode to stack")
	fmt.Println("  mpm mode clear                  Clear all modes")
	fmt.Println("  mpm compile all                 Rebuild database from JSON")
	fmt.Println("  mpm session list                View recent sessions")
	fmt.Println("  mpm memory search \"docker\"      Find memories about topic")
	fmt.Println("  mpm reference add book.pdf      Add reference library file")
	fmt.Println("  mpm reference search \"strategy\" Find in references")
	fmt.Println("  mpm watch                      Start fsnotify file watcher daemon")
	fmt.Println("  mpm watch --dry-run            Test without deleting files")
	fmt.Println("  mpm watch --once               Run startup sweep only")
	fmt.Println()
}

// printGatewayHelp outputs gateway-specific help
func printGatewayHelp() {
	fmt.Println()
	fmt.Println("  mpm gateway - Memory Persona Manager Gateway")
	fmt.Println()
	fmt.Println("  The gateway is a background service that handles:")
	fmt.Println("    - Persona management and mode stacking")
	fmt.Println("    - Memory storage and retrieval")
	fmt.Println("    - Session persistence and recovery")
	fmt.Println()
	fmt.Println("  Gateway Commands:")
	fmt.Println("    help              Show this help")
	fmt.Println("    start             Start gateway (or connect if already running)")
	fmt.Println("    stop              Stop the gateway")
	fmt.Println("    restart           Restart the gateway")
	fmt.Println("    status            Show gateway status")
	fmt.Println()
	fmt.Println("  Examples:")
	fmt.Println("    mpm gateway start     # Start/restart gateway")
	fmt.Println("    mpm gateway stop     # Stop gateway")
	fmt.Println("    mpm gateway status   # Check if gateway is running")
	fmt.Println()
}

// parseWorkspaceFlag extracts --workspace from args (doesn't mutate global state)
func parseWorkspaceFlag(args []string) string {
	for i, arg := range args {
		if arg == "--workspace" && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(arg, "--workspace=") {
			return strings.TrimPrefix(arg, "--workspace=")
		}
	}
	return ""
}
