package main

import (
	"encoding/json"
	"fmt"
	"math"
	"net"
	"strings"
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
	DaemonStartTime int64        `json:"daemon_start_time"` // Unix timestamp
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
	status           *DashboardStatus
	logs             []logEntry
	memoryHistory    []int // Last 60 readings for sparkline
	sockPath         string
	width            int
	height           int
	connected        bool
	err              error
	displayUptimeSecs int    // Client-side uptime tracking
	lastRefresh      int64   // Unix timestamp of last refresh (for throttling)
}

func newDashboardModel(sockPath string) dashboardModel {
	return dashboardModel{
		sockPath:         sockPath,
		memoryHistory:    make([]int, 60),
		connected:        false,
		displayUptimeSecs: 0,
		width:            120, // Safe default; will be updated on resize
		lastRefresh:      0,
	}
}

const dashboardRefreshInterval = 1 // Minimum seconds between refreshes

// Init initializes the dashboard model
func (m dashboardModel) Init() tea.Cmd {
	return fetchStatus(m.sockPath)
}

// Update handles messages and updates the model
func (m dashboardModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case statusMsg:
		m.status = msg.status
		m.connected = true
		m.err = nil
		// Calculate uptime from daemon start time
		if m.status != nil && m.status.DaemonStartTime > 0 {
			m.displayUptimeSecs = int(time.Now().Unix() - m.status.DaemonStartTime)
		} else {
			m.displayUptimeSecs++
		}
		// Update memory history
		if m.status != nil && len(m.memoryHistory) > 0 {
			m.memoryHistory = append(m.memoryHistory[1:], m.status.MemoryUsageKB)
		}
		// Throttle refreshes to avoid hammering the daemon
		now := time.Now().Unix()
		if now > m.lastRefresh+dashboardRefreshInterval {
			m.lastRefresh = now
			return m, fetchStatus(m.sockPath)
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
			return m, func() tea.Msg {
				conn, err := net.Dial("unix", m.sockPath)
				if err != nil {
					return errMsg{err: fmt.Errorf("cannot connect for reboot: %w", err)}
				}
				defer conn.Close()
				msg := Message{Args: []string{"reboot"}}
				json.NewEncoder(conn).Encode(msg)
				return nil
			}
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
		sb.WriteString("  Dashboard (TUI) unavailable in unified mode.\n")
		sb.WriteString("  Use: mpm stats, mpm watch status, mpm doctor\n")
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
	sb.WriteString(fmt.Sprintf("  |  PID: %d  |  Uptime: %s", m.status.PID, formatUptime(time.Duration(m.displayUptimeSecs)*time.Second)))
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
			return errMsg{err: fmt.Errorf("status response: %w", err)}
		}

		var status DashboardStatus
		if err := json.Unmarshal([]byte(resp.Output), &status); err != nil {
			return errMsg{err: fmt.Errorf("parse status: %w", err)}
		}
		return statusMsg{status: &status}
	}
}

// StartDashboard launches the Bubbletea dashboard
// In the unified architecture, the TUI-based dashboard is not available.
// All commands run in-process with no socket daemon to connect to.
func StartDashboard(sockPath string) {
	fmt.Println("Dashboard (TUI) is not available in unified mode.")
	fmt.Println("All commands run in-process — use the following instead:")
	fmt.Println("  mpm stats        Memory statistics")
	fmt.Println("  mpm watch status  File watcher status")
	fmt.Println("  mpm menu         Interactive mode/persona picker")
	fmt.Println("  mpm doctor       System diagnostics")
	fmt.Println("  mpm help         Full command listing")
}
