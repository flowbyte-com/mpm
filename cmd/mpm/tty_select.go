package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"github.com/charmbracelet/bubbles/list"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/creack/pty"
)

// Theme colors (808 palette)
const (
	cyan    = lipgloss.Color("#00D9FF")
	magenta = lipgloss.Color("#FF00FF")
	gold    = lipgloss.Color("#FFD700")
	green   = lipgloss.Color("#00FF88")
	dimmed  = lipgloss.Color("#888888")
)

// =============================================================================
// Selector Item
// =============================================================================

type selectorItem struct {
	name     string
	subtitle string
}

func (i selectorItem) Title() string       { return i.name }
func (i selectorItem) Description() string { return i.subtitle }
func (i selectorItem) FilterValue() string { return i.name }

// =============================================================================
// Selector Delegate (custom rendering with checkboxes + match highlighting)
// =============================================================================

type selectorDelegate struct {
	list.DefaultDelegate
	selected map[int]bool // tracks toggled state per visible index
}

func newSelectorDelegate(selected map[int]bool) selectorDelegate {
	d := list.NewDefaultDelegate()
	d.ShowDescription = false
	d.SetSpacing(1)

	// Custom 808 theme styles
	d.Styles = list.DefaultItemStyles{
		NormalTitle: lipgloss.NewStyle().
			Foreground(lipgloss.Color("#CCCCCC")).
			Padding(0, 0, 0, 2),

		NormalDesc: lipgloss.NewStyle().
			Foreground(dimmed).
			Padding(0, 0, 0, 2),

		SelectedTitle: lipgloss.NewStyle().
			Border(lipgloss.NormalBorder(), false, false, false, true).
			BorderForeground(magenta).
			Foreground(magenta).
			Padding(0, 0, 0, 1),

		SelectedDesc: lipgloss.NewStyle().
			Foreground(magenta).
			Padding(0, 0, 0, 1),

		DimmedTitle: lipgloss.NewStyle().
			Foreground(dimmed).
			Padding(0, 0, 0, 2),

		DimmedDesc: lipgloss.NewStyle().
			Foreground(dimmed).
			Padding(0, 0, 0, 2),

		FilterMatch: lipgloss.NewStyle().
			Foreground(cyan).
			Bold(true),
	}

	return selectorDelegate{DefaultDelegate: d, selected: selected}
}

func (d selectorDelegate) Render(w io.Writer, m list.Model, index int, item list.Item) {
	if index >= len(m.VisibleItems()) {
		return
	}

	si, ok := item.(selectorItem)
	if !ok {
		return
	}

	isSelected := index == m.Index()
	isFiltered := m.FilterState() != list.Unfiltered
	matchedRunes := m.MatchesForItem(index)
	isToggled := d.selected[index]

	// Checkbox
	var checkbox string
	if isToggled {
		checkbox = lipgloss.NewStyle().Foreground(magenta).Render("◉ ")
	} else {
		checkbox = lipgloss.NewStyle().Foreground(dimmed).Render("○ ")
	}

	// Title with match highlighting
	var title string
	if isFiltered && len(matchedRunes) > 0 {
		unmatchedStyle := lipgloss.NewStyle().Foreground(lipgloss.Color("#CCCCCC"))
		matchedStyle := unmatchedStyle.Foreground(cyan).Bold(true)
		title = lipgloss.StyleRunes(si.name, matchedRunes, matchedStyle, unmatchedStyle)
	} else if isSelected {
		title = lipgloss.NewStyle().Foreground(magenta).Render(si.name)
	} else {
		title = lipgloss.NewStyle().Foreground(lipgloss.Color("#CCCCCC")).Render(si.name)
	}

	// [active] indicator if originally active
	var activeStr string
	if si.subtitle == "[active]" {
		activeStr = lipgloss.NewStyle().Foreground(magenta).Render(" [active]")
	}

	line := checkbox + title + activeStr
	fmt.Fprintf(w, "%s", line)
}

// =============================================================================
// Selector Model
// =============================================================================

type selectorModel struct {
	list      list.Model
	multi     bool
	selected  map[int]bool // visible-index -> toggled
	done      bool
	quitting  bool
	choice    string // for single-select
	choices   []string // for multi-select
	origItems []selectorItem // for tracking [active] status
}

func newSelectorModel(items []selectorItem, multi bool, activeSet map[string]bool) selectorModel {
	selected := make(map[int]bool)

	// Pre-toggle items that are already active
	for i, item := range items {
		if activeSet[item.name] {
			selected[i] = true
		}
	}

	delegate := newSelectorDelegate(selected)
	l := list.New([]list.Item{}, delegate, 80, 20)

	// Style the list
	styles := list.DefaultStyles()
	styles.TitleBar = lipgloss.NewStyle().
		Foreground(cyan).
		Bold(true).
		Padding(0, 0, 1, 2)
	styles.FilterPrompt = lipgloss.NewStyle().Foreground(magenta)
	styles.FilterCursor = lipgloss.NewStyle().Foreground(magenta)
	styles.StatusBar = lipgloss.NewStyle().Foreground(dimmed)
	styles.ActivePaginationDot = lipgloss.NewStyle().Foreground(magenta)
	styles.InactivePaginationDot = lipgloss.NewStyle().Foreground(dimmed)
	l.Styles = styles

	l.Title = ""
	l.SetShowTitle(false)
	l.SetShowFilter(true)
	l.SetShowPagination(true)
	l.SetShowHelp(false)
	l.SetStatusBarItemName("item", "items")
	l.SetFilteringEnabled(true)

	// Populate items
	goItems := make([]list.Item, len(items))
	for i := range items {
		goItems[i] = items[i]
	}
	l.SetItems(goItems)

	return selectorModel{
		list:      l,
		multi:     multi,
		selected:  selected,
		origItems: items,
	}
}

func (m *selectorModel) Init() tea.Cmd {
	return nil
}

func (m *selectorModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.list.SetSize(msg.Width, msg.Height-5)
		return m, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "esc":
			m.quitting = true
			m.done = true
			m.choice = ""
			m.choices = nil
			return m, tea.Quit

		case " ":
			// Space toggles selection in multi-select mode
			if m.multi {
				idx := m.list.Index()
				items := m.list.VisibleItems()
				if idx < len(items) {
					// Toggle and move down
					m.selected[idx] = !m.selected[idx]
					m.list.CursorDown()
				}
				return m, nil
			}

		case "enter":
			idx := m.list.Index()
			items := m.list.VisibleItems()

			if m.multi {
				// Toggle current and collect all selected
				m.selected[idx] = !m.selected[idx]
				var chosen []string
				for i, item := range items {
					if m.selected[i] {
						chosen = append(chosen, item.(selectorItem).name)
					}
				}
				// If nothing selected, confirm as-is
				if len(chosen) == 0 {
					chosen = append(chosen, items[idx].(selectorItem).name)
				}
				m.choices = chosen
			} else {
				// Single select
				if idx < len(items) {
					m.choice = items[idx].(selectorItem).name
				}
			}
			m.done = true
			return m, tea.Quit
		}

		// Pass to list for navigation/filtering
		var cmd tea.Cmd
		m.list, cmd = m.list.Update(msg)
		return m, cmd
	}

	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

func (m selectorModel) View() string {
	if m.done {
		return ""
	}

	var sb strings.Builder
	sb.WriteString("\n")

	if m.multi {
		sb.WriteString(lipgloss.NewStyle().
			Foreground(cyan).
			Bold(true).
			Render("  🤖 Select Modes"))
	} else {
		sb.WriteString(lipgloss.NewStyle().
			Foreground(cyan).
			Bold(true).
			Render("  🤖 Select Persona"))
	}
	sb.WriteString("\n")
	sb.WriteString(lipgloss.NewStyle().Foreground(dimmed).Render("  " + strings.Repeat("─", 70)))
	sb.WriteString("\n")

	// Count selected
	selectedCount := 0
	for _, v := range m.selected {
		if v {
			selectedCount++
		}
	}
	if m.multi && selectedCount > 0 {
		sb.WriteString(fmt.Sprintf("  %s %d selected\n",
			lipgloss.NewStyle().Foreground(gold).Render("[✓]"), selectedCount))
		sb.WriteString(lipgloss.NewStyle().Foreground(dimmed).Render("  " + strings.Repeat("─", 70)))
		sb.WriteString("\n")
	}

	sb.WriteString(m.list.View())

	sb.WriteString("\n")
	if m.multi {
		sb.WriteString(fmt.Sprintf("  %s %s %s %s\n\n",
			lipgloss.NewStyle().Foreground(dimmed).Render("space"),
			lipgloss.NewStyle().Foreground(cyan).Render("Toggle"),
			lipgloss.NewStyle().Foreground(dimmed).Render("| enter"),
			lipgloss.NewStyle().Foreground(cyan).Render("Confirm"),
			lipgloss.NewStyle().Foreground(dimmed).Render("| q"),
			lipgloss.NewStyle().Foreground(cyan).Render("Quit"),
		))
	} else {
		sb.WriteString(fmt.Sprintf("  %s %s %s %s\n\n",
			lipgloss.NewStyle().Foreground(dimmed).Render("enter"),
			lipgloss.NewStyle().Foreground(cyan).Render("Select"),
			lipgloss.NewStyle().Foreground(dimmed).Render("| q"),
			lipgloss.NewStyle().Foreground(cyan).Render("Quit"),
		))
	}

	return sb.String()
}

// =============================================================================
// PTY Subprocess Runner
// =============================================================================

// runSelectorPTY runs the selector TUI in a PTY subprocess.
// Returns selected names and nil error on confirmed selection, or nil/err on cancel.
func runSelectorPTY(items []selectorItem, multi bool, activeSet map[string]bool) ([]string, error) {
	// Create PTY
	ptmx, ptys, err := pty.Open()
	if err != nil {
		return nil, fmt.Errorf("failed to open PTY: %w", err)
	}
	defer ptmx.Close()

	// Run selector as subprocess
	cmd := exec.Command(os.Args[0], os.Args[1:]...)
	cmd.Env = append(os.Environ(),
		"MPM_SELECT=1",
		"MPM_SELECT_MULTI="+boolStr(multi),
		"MPM_SELECT_ITEMS="+encodeSelectorItems(items),
		"MPM_SELECT_ACTIVE="+encodeActiveSet(activeSet),
	)
	cmd.Stdin = ptys
	cmd.Stdout = ptys
	cmd.Stderr = ptys

	// Create new session and set controlling TTY
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    int(ptys.Fd()),
	}

	ptys.Close()

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start selector: %w", err)
	}

	// Read result from subprocess output (synchronous read)
	var result []string
	var confirmed bool
	scanner := newWordScanner(ptmx)
	for scanner.Next() {
		token := scanner.Token()
		if strings.HasPrefix(token, "MPM_SELECT_RESULT:") {
			result = parseSelectorResult(token)
		} else if token == "MPM_SELECT_CONFIRMED:1" {
			confirmed = true
		}
	}

	cmd.Wait()

	if !confirmed {
		return nil, fmt.Errorf("selection cancelled")
	}
	return result, nil
}

func boolStr(b bool) string {
	if b {
		return "1"
	}
	return "0"
}

// =============================================================================
// Standalone Entry Point
// =============================================================================

// RunSelectorStandalone runs the selector TUI directly (PTY subprocess mode).
// Returns exit code: 0 on confirmed selection, 1 on cancel/error.
func RunSelectorStandalone() int {
	multi := os.Getenv("MPM_SELECT_MULTI") == "1"

	items, err := parseSelectorItems(os.Getenv("MPM_SELECT_ITEMS"))
	if err != nil || len(items) == 0 {
		fmt.Fprintf(os.Stderr, "Selector: no items provided\n")
		return 1
	}

	activeSet := parseActiveSet(os.Getenv("MPM_SELECT_ACTIVE"))

	model := newSelectorModel(items, multi, activeSet)
	p := tea.NewProgram(&model, tea.WithAltScreen())

	if err := p.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Selector error: %v\n", err)
		return 1
	}

	// Output result
	if model.done && !model.quitting {
		var result []string
		if model.multi {
			result = model.choices
		} else {
			if model.choice != "" {
				result = []string{model.choice}
			}
		}

		fmt.Printf("MPM_SELECT_CONFIRMED:1\n")
		if len(result) > 0 {
			fmt.Printf("MPM_SELECT_RESULT:%s\n", strings.Join(result, ","))
		}
	}

	return 0
}

// =============================================================================
// Helpers
// =============================================================================

func encodeSelectorItems(items []selectorItem) string {
	type encItem struct {
		Name     string `json:"n"`
		Subtitle string `json:"s"`
	}
	enc := make([]encItem, len(items))
	for i, item := range items {
		enc[i] = encItem{Name: item.name, Subtitle: item.subtitle}
	}
	data, _ := json.Marshal(enc)
	return string(data)
}

func parseSelectorItems(encoded string) ([]selectorItem, error) {
	if encoded == "" {
		return nil, fmt.Errorf("empty")
	}
	type encItem struct {
		Name     string `json:"n"`
		Subtitle string `json:"s"`
	}
	var enc []encItem
	if err := json.Unmarshal([]byte(encoded), &enc); err != nil {
		return nil, err
	}
	items := make([]selectorItem, len(enc))
	for i := range enc {
		items[i] = selectorItem{name: enc[i].Name, subtitle: enc[i].Subtitle}
	}
	return items, nil
}

func encodeActiveSet(activeSet map[string]bool) string {
	var names []string
	for name := range activeSet {
		names = append(names, name)
	}
	data, _ := json.Marshal(names)
	return string(data)
}

func parseActiveSet(encoded string) map[string]bool {
	set := make(map[string]bool)
	if encoded == "" {
		return set
	}
	var names []string
	if err := json.Unmarshal([]byte(encoded), &names); err != nil {
		return set
	}
	for _, n := range names {
		set[n] = true
	}
	return set
}

func parseSelectorResult(token string) []string {
	prefix := "MPM_SELECT_RESULT:"
	if len(token) <= len(prefix) {
		return nil
	}
	encoded := token[len(prefix):]
	if encoded == "" {
		return nil
	}
	parts := strings.Split(encoded, ",")
	var result []string
	for _, p := range parts {
		if p != "" {
			result = append(result, p)
		}
	}
	return result
}

// =============================================================================
// Simple Token Scanner (avoids bufio size limits)
// =============================================================================

type wordScanner struct {
	r   *os.File
	buf []byte
	pos int
	len int
}

func newWordScanner(r *os.File) *wordScanner {
	return &wordScanner{r: r, buf: make([]byte, 4096)}
}

func (s *wordScanner) Next() bool {
	// Read more if needed
	if s.pos >= s.len {
		n, err := s.r.Read(s.buf)
		if err != nil || n == 0 {
			return false
		}
		s.pos = 0
		s.len = n
	}
	return true
}

func (s *wordScanner) Token() string {
	start := s.pos
	for s.pos < s.len {
		if s.buf[s.pos] == '\n' {
			token := string(s.buf[start:s.pos])
			s.pos++ // skip newline
			return token
		}
		s.pos++
	}
	// No newline found - return rest
	token := string(s.buf[start:s.len])
	s.len = s.pos // force next read
	return token
}
