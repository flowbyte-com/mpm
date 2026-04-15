package main

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// Bubbletea dashboard styles
var (
	headerStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#00D9FF")).
			Bold(true)

	workerStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FF00FF"))

	queueStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#FFD700"))

	logStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#888888"))

	sparklineStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#00FF88"))

	statusOK = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#00FF88"))

	statusIdle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#888888"))

	borderStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#333333"))
)

// DashboardStatus mirrors the daemon's status response
type DashboardStatus struct {
	PID             int          `json:"pid"`
	Uptime          string       `json:"uptime"`
	MemoryUsageKB   int          `json:"memory_usage_kb"`
	ActiveWorkers   int          `json:"active_workers"`
	MaxWorkers      int          `json:"max_workers"`
	TotalTasks      int          `json:"total_tasks"`
	QueueDepth      int          `json:"queued_tasks"`
	PreFlightStatus string       `json:"preflight_status"`
	ActivePersona   string       `json:"active_persona"`
	ActiveModes     []string     `json:"active_modes"`
	Workers         []WorkerInfo `json:"workers"`
	QueueTasks      []string     `json:"queue_tasks"`
}

type WorkerInfo struct {
	ID      int    `json:"id"`
	Task    string `json:"task"`
	Started string `json:"started"`
}

type logEntry struct {
	Time  string `json:"time"`
	Level string `json:"level"`
	Msg   string `json:"msg"`
}

// dashboardModel is the Bubbletea model for our dashboard
type dashboardModel struct {
	status        *DashboardStatus
	logs          []logEntry
	memoryHistory []int // Last 60 readings for sparkline
	sockPath      string
	width         int
	height        int
	connected     bool
	err           error
	uptimeSecs    int
	lastRefresh   int64 // Unix timestamp of last refresh (for throttling)
}

func newDashboardModel(sockPath string) dashboardModel {
	return dashboardModel{
		sockPath:      sockPath,
		memoryHistory: make([]int, 60),
		connected:     false,
		uptimeSecs:    0,
		width:         120, // Safe default; will be updated on resize
		lastRefresh:   0,
	}
}

const dashboardRefreshInterval = 1 // Minimum seconds between refreshes

// Init initializes the dashboard model
func (m dashboardModel) Init() tea.Cmd {
	return tea.Batch(
		fetchStatus(m.sockPath),
		fetchLogs(),
	)
}

// Update handles messages and updates the model
func (m dashboardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case statusMsg:
		m.status = msg.status
		m.connected = true
		m.err = nil
		m.uptimeSecs++
		// Update memory history
		if m.status != nil && len(m.memoryHistory) > 0 {
			m.memoryHistory = append(m.memoryHistory[1:], m.status.MemoryUsageKB)
		}
		// Throttle refreshes to avoid hammering the daemon
		now := time.Now().Unix()
		if now > m.lastRefresh+dashboardRefreshInterval {
			m.lastRefresh = now
			return m, tea.Batch(
				fetchStatus(m.sockPath),
				fetchLogs(),
			)
		}
		return m, nil

	case logsMsg:
		m.logs = msg.logs
		return m, nil

	case errMsg:
		m.err = msg.err
		m.connected = false
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "esc":
			return m, tea.Quit
		case "r":
			// Trigger reboot via socket
			return m, triggerReboot(m.sockPath)
		}

	case tea.WindowSizeMsg:
		// Bound width/height to prevent strings.Repeat panic with huge values
		m.width = msg.Width
		m.height = msg.Height
		if m.width < 40 || m.width > 1000 {
			m.width = 120
		}
		if m.height < 10 || m.height > 200 {
			m.height = 40
		}
		return m, nil
	}

	return m, nil
}

// View renders the dashboard
func (m dashboardModel) View() string {
	var sb strings.Builder

	// Header
	sb.WriteString("\n")
	sb.WriteString(headerStyle.Render("  🤖 Flowbyte mpm Dashboard"))
	sb.WriteString("\n")
	sb.WriteString(borderStyle.Render("  " + strings.Repeat("─", m.width-4)))
	sb.WriteString("\n\n")

	if !m.connected || m.err != nil {
		sb.WriteString(statusIdle.Render("  ⚠ Daemon not connected"))
		if m.err != nil {
			sb.WriteString(fmt.Sprintf(": %v", m.err))
		}
		sb.WriteString("\n\n")
		sb.WriteString("  Start daemon with: mpm daemon\n")
		sb.WriteString("  Press 'q' to quit\n")
		return sb.String()
	}

	if m.status == nil {
		sb.WriteString("  Loading...\n")
		return sb.String()
	}

	// === LEFT COLUMN ===
	leftWidth := (m.width - 4) / 2

	// Header Pane
	sb.WriteString(statusOK.Render("  ✓ Daemon Running"))
	sb.WriteString(fmt.Sprintf("  |  PID: %d  |  Uptime: %s", m.status.PID, m.status.Uptime))
	sb.WriteString("\n")
	sb.WriteString(borderStyle.Render("  " + strings.Repeat("─", m.width-4)))
	sb.WriteString("\n\n")

	// Active Persona/Modes
	if m.status.ActivePersona != "" {
		sb.WriteString(fmt.Sprintf("  %s %s\n", headerStyle.Render("Persona:"), m.status.ActivePersona))
	}
	if len(m.status.ActiveModes) > 0 {
		sb.WriteString(fmt.Sprintf("  %s %s\n", headerStyle.Render("Modes:"), strings.Join(m.status.ActiveModes, ", ")))
	}
	sb.WriteString("\n")

	// === Worker Pane ===
	sb.WriteString(workerStyle.Render("  ◈ WORKERS"))
	sb.WriteString(fmt.Sprintf("  (%d/%d active)\n", m.status.ActiveWorkers, m.status.MaxWorkers))
	sb.WriteString(borderStyle.Render("  ├" + strings.Repeat("─", leftWidth-2) + "┤"))
	sb.WriteString("\n")

	for i := 0; i < m.status.MaxWorkers; i++ {
		var workerStr string
		if i < len(m.status.Workers) {
			w := m.status.Workers[i]
			if w.Task == "" {
				workerStr = fmt.Sprintf("  │ Worker %d: %s", w.ID, statusIdle.Render("[IDLE]"))
			} else {
				workerStr = fmt.Sprintf("  │ Worker %d: %s", w.ID, truncate(w.Task, 30))
			}
		} else {
			workerStr = fmt.Sprintf("  │ Worker %d: %s", i, statusIdle.Render("[IDLE]"))
		}
		// Pad to align
		workerStr = padRight(workerStr, leftWidth-1)
		sb.WriteString(workerStr + borderStyle.Render("│") + "\n")
	}
	sb.WriteString(borderStyle.Render("  " + strings.Repeat("─", leftWidth)))
	sb.WriteString("\n")

	// === Queue Pane ===
	sb.WriteString(queueStyle.Render("  ◈ QUEUE"))
	sb.WriteString(fmt.Sprintf("  (%d pending)\n", m.status.QueueDepth))
	sb.WriteString(borderStyle.Render("  ├" + strings.Repeat("─", leftWidth-2) + "┤"))
	sb.WriteString("\n")

	if len(m.status.QueueTasks) == 0 {
		sb.WriteString(padRight("  │ (empty)", leftWidth-1) + borderStyle.Render("│") + "\n")
	} else {
		for i, task := range m.status.QueueTasks {
			if i >= 5 {
				break // Show max 5
			}
			taskStr := fmt.Sprintf("  │ %d. %s", i+1, truncate(task, leftWidth-6))
			taskStr = padRight(taskStr, leftWidth-1)
			sb.WriteString(taskStr + borderStyle.Render("│") + "\n")
		}
	}
	sb.WriteString(borderStyle.Render("  " + strings.Repeat("─", leftWidth)))
	sb.WriteString("\n")

	// === Memory Sparkline ===
	rightWidth := (m.width - 4) - leftWidth
	sb.WriteString(sparklineStyle.Render("  ◈ MEMORY"))
	sb.WriteString(fmt.Sprintf("  (%d KB current)\n", m.status.MemoryUsageKB))
	sb.WriteString(borderStyle.Render("  ├" + strings.Repeat("─", rightWidth-2) + "┐"))
	sb.WriteString("\n")

	sparkline := renderSparkline(m.memoryHistory, rightWidth-4)
	for _, line := range sparkline {
		sb.WriteString("  │" + sparklineStyle.Render(line) + borderStyle.Render("│") + "\n")
	}
	sb.WriteString(borderStyle.Render("  " + strings.Repeat("─", rightWidth)))
	sb.WriteString("\n")

	// === Log Pane ===
	sb.WriteString(logStyle.Render("  ◈ RECENT LOGS"))
	sb.WriteString("\n")
	sb.WriteString(borderStyle.Render("  ├" + strings.Repeat("─", m.width-4) + "┤"))
	sb.WriteString("\n")

	logWidth := m.width - 6
	for i := len(m.logs) - 1; i >= 0; i-- {
		if len(m.logs)-i > 10 {
			break // Last 10 entries
		}
		entry := m.logs[i]
		levelStr := "[" + entry.Level + "]"
		logLine := fmt.Sprintf("%s %s %s", entry.Time, levelStr, entry.Msg)
		logLine = truncate(logLine, logWidth)
		logLine = padRight(logLine, logWidth)

		var levelColor lipgloss.TerminalColor
		switch entry.Level {
		case "ERROR", "WARN":
			levelColor = lipgloss.Color("#FF5555")
		case "INFO":
			levelColor = lipgloss.Color("#00D9FF")
		default:
			levelColor = lipgloss.Color("#888888")
		}

		sb.WriteString("  │ ")
		sb.WriteString(logStyle.Foreground(levelColor).Render(logLine))
		sb.WriteString(borderStyle.Render(" │"))
		sb.WriteString("\n")
	}
	sb.WriteString(borderStyle.Render("  " + strings.Repeat("─", m.width-4)))
	sb.WriteString("\n")

	// Footer
	sb.WriteString("\n")
	sb.WriteString(fmt.Sprintf("  %s Refresh: 1s  |  %s Quit  |  %s Reboot",
		statusOK.Render("●"),
		headerStyle.Render("q/Esc"),
		queueStyle.Render("r")))
	sb.WriteString("\n\n")

	return sb.String()
}

// renderSparkline creates ASCII sparkline from data
func renderSparkline(data []int, width int) []string {
	if len(data) == 0 {
		return []string{strings.Repeat(" ", width)}
	}

	// Find min/max for scaling
	min := math.MaxInt
	max := 0
	for _, v := range data {
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	if max == min {
		max = min + 1
	}

	// Create sparkline characters
	sparkChars := " ▁▂▃▄▅▆▇█"
	rows := 4
	lines := make([]string, rows)

	for row := 0; row < rows; row++ {
		line := ""
		for i := 0; i < width && i < len(data); i++ {
			val := data[i]
			// Scale to 0-8
			scaled := float64(val-min) / float64(max-min) * 8
			if scaled < 0 {
				scaled = 0
			}
			if scaled > 8 {
				scaled = 8
			}
			// Assign to row (higher values go to lower rows)
			threshold := 8 - (row+1)*(8/rows)
			if int(scaled) >= threshold {
				line += string(sparkChars[8])
			} else {
				line += string(sparkChars[0])
			}
		}
		lines[rows-1-row] = line
	}

	return lines
}

// padRight pads a string with spaces to the right
func padRight(s string, length int) string {
	if len(s) >= length {
		return s[:length-1] + " "
	}
	return s + strings.Repeat(" ", length-len(s))
}

// Message types for Bubbletea
type (
	statusMsg struct{ status *DashboardStatus }
	logsMsg   struct{ logs []logEntry }
	errMsg    struct{ err error }
)

// fetchStatus connects to daemon and fetches status
func fetchStatus(sockPath string) tea.Cmd {
	return func() tea.Msg {
		conn, err := net.Dial("unix", sockPath)
		if err != nil {
			return errMsg{err: fmt.Errorf("cannot connect to daemon: %w", err)}
		}
		defer conn.Close()

		// Send status request
		msg := Message{Args: []string{"status"}}
		if err := json.NewEncoder(conn).Encode(msg); err != nil {
			return errMsg{err: err}
		}

		// Read response
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		var resp Message
		if err := json.NewDecoder(conn).Decode(&resp); err != nil {
			return errMsg{err: err}
		}

		var status DashboardStatus
		if err := json.Unmarshal([]byte(resp.Output), &status); err != nil {
			return errMsg{err: err}
		}

		return statusMsg{status: &status}
	}
}

// fetchLogs reads recent entries from daemon.json.log
// Uses efficient backward scanning to find last N lines without loading entire file
func fetchLogs() tea.Cmd {
	return func() tea.Msg {
		paths := []string{
			"/tmp/mpm/daemon.json.log",
			filepath.Join(os.Getenv("HOME"), ".openclaw", "workspace", "mpm", "daemon.json.log"),
		}

		const maxLogs = 10
		var logs []logEntry

		for _, path := range paths {
			f, err := os.Open(path)
			if err != nil {
				continue
			}

			// Get file size
			info, err := f.Stat()
			if err != nil {
				f.Close()
				continue
			}

			// If file is small, read it all
			if info.Size() < 64*1024 {
				f.Close()
				logs = readLastLinesEfficient(path, maxLogs)
				break
			}

			// For large files, scan backward from end
			logs = scanLastLinesBackward(f, info, maxLogs)
			f.Close()
			break
		}

		return logsMsg{logs: logs}
	}
}

// readLastLinesEfficient reads last N lines from a small file
func readLastLinesEfficient(path string, maxLines int) []logEntry {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}

	// Find last N newline positions
	lines := strings.Split(string(data), "\n")
	if len(lines) <= maxLines {
		return parseLogLines(lines)
	}

	// Take only last maxLines
	return parseLogLines(lines[len(lines)-maxLines:])
}

// parseLogLines parses JSON lines into logEntry structs
func parseLogLines(lines []string) []logEntry {
	var logs []logEntry
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var entry logEntry
		if err := json.Unmarshal([]byte(line), &entry); err == nil {
			logs = append(logs, entry)
		}
	}
	return logs
}

// scanLastLinesBackward efficiently reads last N lines from large file by scanning backward
func scanLastLinesBackward(f *os.File, info os.FileInfo, maxLines int) []logEntry {
	fileSize := info.Size()
	bufSize := 8192
	if bufSize > int(fileSize) {
		bufSize = int(fileSize)
	}

	lines := make([]string, 0, maxLines)
	remainder := ""

	for linesRead := 0; linesRead < maxLines && fileSize > 0; {
		// Calculate read position
		readStart := fileSize - int64(bufSize)
		if readStart < 0 {
			readStart = 0
			bufSize = int(fileSize)
		}

		// Read chunk
		f.Seek(readStart, io.SeekStart)
		buf := make([]byte, bufSize)
		n, err := f.Read(buf)
		if err != nil && err != io.EOF {
			break
		}

		// Prepend remainder from previous iteration
		data := remainder + string(buf[:n])
		chunkLines := strings.Split(data, "\n")

		// If we're not at the start of file, discard the last partial line
		if readStart > 0 && len(chunkLines) > 0 {
			remainder = chunkLines[0]
			chunkLines = chunkLines[1:]
		} else {
			remainder = ""
		}

		// Take lines from the end (most recent first)
		for i := len(chunkLines) - 1; i >= 0 && linesRead < maxLines; i-- {
			line := strings.TrimSpace(chunkLines[i])
			if line == "" {
				continue
			}
			var entry logEntry
			if err := json.Unmarshal([]byte(line), &entry); err == nil {
				lines = append(lines, line) // Store raw for now
				linesRead++
			}
		}

		fileSize = readStart
	}

	// Reverse to get chronological order (oldest first)
	for i, j := 0, len(lines)-1; i < j; i, j = i+1, j-1 {
		lines[i], lines[j] = lines[j], lines[i]
	}

	// Parse stored raw lines
	return parseLogLines(lines)
}

// triggerReboot sends reboot command to daemon
func triggerReboot(sockPath string) tea.Cmd {
	return func() tea.Msg {
		conn, err := net.Dial("unix", sockPath)
		if err != nil {
			return errMsg{err: err}
		}
		defer conn.Close()

		msg := Message{Args: []string{"reboot"}}
		if err := json.NewEncoder(conn).Encode(msg); err != nil {
			return errMsg{err: err}
		}

		// Read response
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		var resp Message
		json.NewDecoder(conn).Decode(&resp)

		return tea.Quit
	}
}

// StartDashboard launches the Bubbletea dashboard
func StartDashboard(sockPath string) {
	p := tea.NewProgram(
		newDashboardModel(sockPath),
		tea.WithAltScreen(),       // Use alternate screen buffer
		tea.WithMouseCellMotion(), // Enable mouse tracking
	)

	// Handle signals for graceful exit
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		<-sigChan
		p.Quit()
	}()

	if err := p.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Dashboard error: %v\n", err)
	}
}
