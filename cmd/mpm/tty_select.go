package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/creack/pty"
)

// SymAI theme colors
var (
	cyanStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#00D9FF")).Bold(true)
	magentaStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF00FF"))
	greenStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#00FF88"))
	dimStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#888888"))
	goldStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#FFD700"))
)

// =============================================================================
// Selector Item
// =============================================================================

type selectorItem struct {
	name     string
	subtitle string
	selected bool
}

func (i selectorItem) Title() string       { return i.name }
func (i selectorItem) Description() string { return i.subtitle }
func (i selectorItem) FilterValue() string { return i.name }

// =============================================================================
// Simple Selector TUI Model (no external bubbles/list needed)
// =============================================================================

type selectModel struct {
	items    []selectorItem
	cursor   int
	filtered []int // indices into items; if nil, show all
	termW    int
	termH    int
	quitting bool
	result   []string
	multi    bool
}

func newSelectModel(items []selectorItem, multi bool) *selectModel {
	return &selectModel{
		items:    items,
		cursor:   0,
		termW:    80,
		termH:    24,
		multi:    multi,
		filtered: nil, // nil = show all
	}
}

func (m *selectModel) filteredItems() []selectorItem {
	if m.filtered == nil {
		return m.items
	}
	result := make([]selectorItem, len(m.filtered))
	for i, idx := range m.filtered {
		result[i] = m.items[idx]
	}
	return result
}

func (m *selectModel) Init() tea.Cmd {
	return nil
}

func (m *selectModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.termW = msg.Width
		m.termH = msg.Height
		return m, nil

	case tea.KeyMsg:
		filtered := m.filteredItems()
		max := len(filtered) - 1

		switch msg.String() {
		case "up", "k":
			if m.cursor > 0 {
				m.cursor--
			}

		case "down", "j":
			if m.cursor < max {
				m.cursor++
			}

		case " ":
			if m.multi && max >= 0 {
				realIdx := m.realIndex(m.cursor)
				m.items[realIdx].selected = !m.items[realIdx].selected
			}

		case "enter":
			if max >= 0 {
				if m.multi {
					// Collect all selected
					for _, item := range m.items {
						if item.selected {
							m.result = append(m.result, item.name)
						}
					}
					if len(m.result) == 0 {
						// Nothing selected — use cursor
						realIdx := m.realIndex(m.cursor)
						m.result = []string{m.items[realIdx].name}
					}
				} else {
					realIdx := m.realIndex(m.cursor)
					m.result = []string{m.items[realIdx].name}
				}
				m.quitting = true
			}

		case "esc", "ctrl+c":
			m.result = []string{}
			m.quitting = true
		}
	}
	return m, nil
}

func (m *selectModel) realIndex(displayIdx int) int {
	if m.filtered == nil {
		return displayIdx
	}
	return m.filtered[displayIdx]
}

func (m *selectModel) View() string {
	if m.quitting {
		return ""
	}
	filtered := m.filteredItems()
	if len(filtered) == 0 {
		return "\n  No items to display.\n\n"
	}

	var b strings.Builder
	b.WriteString("\n")

	status := "single-select"
	if m.multi {
		status = "multi-select (space to toggle)"
	}
	b.WriteString(dimStyle.Render(fmt.Sprintf("  %s  ·  ↑↓ navigate  ·  enter confirm  ·  esc cancel\n", status)))
	b.WriteString(dimStyle.Render("  " + strings.Repeat("─", m.termW-4) + "\n"))

	for i, item := range filtered {
		cursor := " "
		nameDisplay := item.name
		if item.selected {
			nameDisplay = greenStyle.Render("✓ " + item.name)
		}
		if i == m.cursor {
			cursor = magentaStyle.Render("▶")
			b.WriteString(fmt.Sprintf("  %s  %s  %s\n", cursor, cyanStyle.Render(nameDisplay), dimStyle.Render(item.subtitle)))
		} else {
			b.WriteString(fmt.Sprintf("   %s  %s\n", cursor, dimStyle.Render(nameDisplay)))
		}
	}
	b.WriteString(dimStyle.Render("\n  ─"))
	return b.String()
}

// =============================================================================
// PTY-based Selector Runner
// =============================================================================

// runSelectorPTY executes the selector in a PTY subprocess and returns selected names.
func runSelectorPTY(items []selectorItem, multi bool, activeSet map[string]bool) ([]string, error) {
	// Mark active items as selected
	for i := range items {
		if activeSet[items[i].name] {
			items[i].selected = true
		}
	}

	// Build subprocess command
	cmd := exec.Command(os.Args[0], os.Args[1:]...)
	cmd.Env = append(os.Environ(),
		"MPM_SELECT=1",
		"MPM_SELECT_ITEMS="+encodeSelectorItems(items),
	)
	if multi {
		cmd.Env = append(cmd.Env, "MPM_SELECT_MULTI=1")
	}

	// Start with PTY (handles Setsid+Setctty internally)
	ptmx, err := pty.Start(cmd)
	if err != nil {
		return nil, fmt.Errorf("pty start: %w", err)
	}
	defer ptmx.Close()

	// Read result from PTY master (communicate via channel to avoid data race)
	resultCh := make(chan []string, 1)
	go func() {
		scanner := bufio.NewScanner(ptmx)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "MPM_SELECT_DONE:") {
				resultCh <- parseSelectorResult(line)
				return
			}
		}
		resultCh <- nil
	}()

	waitErr := cmd.Wait()

	if waitErr != nil {
		// May have been killed or exited
	}
	result := <-resultCh
	if result == nil {
		result = []string{}
	}
	return result, nil
}

// encodeSelectorItems encodes items to JSON for env var transport
func encodeSelectorItems(items []selectorItem) string {
	data, _ := json.Marshal(items)
	return string(data)
}

func parseSelectorResult(line string) []string {
	if len(line) < 18 {
		return nil
	}
	encoded := line[17:] // Remove "MPM_SELECT_DONE:"
	var result []string
	if err := json.Unmarshal([]byte(encoded), &result); err != nil {
		for _, s := range strings.Split(encoded, ",") {
			s = strings.Trim(s, ` "`)
			if s != "" {
				result = append(result, s)
			}
		}
	}
	return result
}

// RunSelectorStandalone runs the selector TUI in-process.
// Called when MPM_SELECT=1 env var is set (PTY subprocess context).
func RunSelectorStandalone() bool {
	itemsEnv := os.Getenv("MPM_SELECT_ITEMS")
	if itemsEnv == "" {
		fmt.Fprintf(os.Stderr, "MPM_SELECT_ITEMS not set\n")
		return false
	}

	items, err := parseSelectorItemsJSON(itemsEnv)
	if err != nil || len(items) == 0 {
		fmt.Fprintf(os.Stderr, "No items to select: %v\n", err)
		return false
	}

	multi := os.Getenv("MPM_SELECT_MULTI") == "1"

	model := newSelectModel(items, multi)
	p := tea.NewProgram(
		model,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)

	finalModel, err := p.Run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Selector error: %v\n", err)
		return false
	}

	sm, ok := finalModel.(*selectModel)
	if !ok || sm == nil {
		sm = model
	}
	if sm.quitting || len(sm.result) == 0 {
		fmt.Print("MPM_SELECT_DONE:[]")
		return true
	}

	encoded, _ := json.Marshal(sm.result)
	fmt.Printf("MPM_SELECT_DONE:%s\n", encoded)
	return true
}

func parseSelectorItemsJSON(encoded string) ([]selectorItem, error) {
	var items []selectorItem
	if err := json.Unmarshal([]byte(encoded), &items); err != nil {
		return nil, err
	}
	return items, nil
}
