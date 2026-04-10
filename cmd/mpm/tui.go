package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/charmbracelet/bubbles/list"
	"github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// ---------------------------------------------------------------------------
// Styles (808 aesthetic)
// ---------------------------------------------------------------------------
var (
	ColorIndigo  = lipgloss.Color("69")
	ColorCyan    = lipgloss.Color("87")
	ColorMagenta = lipgloss.Color("213")
	ColorGold    = lipgloss.Color("220")
	ColorBorder  = lipgloss.Color("99")
	ColorGreen   = lipgloss.Color("84")
	ColorRed     = lipgloss.Color("204")
	ColorDim     = lipgloss.Color("245")

	TitleStyle = lipgloss.NewStyle().
			Foreground(ColorGold).
			Bold(true)

	SubTitleStyle = lipgloss.NewStyle().
			Foreground(ColorCyan)

	SectionStyle = lipgloss.NewStyle().
			Foreground(ColorMagenta).
			Bold(true)

	RuleStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("252")).
			Padding(0, 0, 0, 2)

	ValueStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("252"))

	DimStyle = lipgloss.NewStyle().
			Foreground(ColorDim)

	HelpStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("241"))

	StatusOK   = lipgloss.NewStyle().Foreground(ColorGreen).Render("●")
	StatusDim  = lipgloss.NewStyle().Foreground(ColorDim).Render("●")
)

// ---------------------------------------------------------------------------
// Model
// ---------------------------------------------------------------------------
type Model struct {
	mpmDir        string
	modeList      list.Model
	personaList   list.Model
	activePanel   int          // 0 = modes, 1 = personas
	selectedModes map[int]bool
	quitting      bool

	// Full raw data for preview
	modeData    []map[string]interface{}
	personaData []map[string]interface{}
}

type ModeItem struct{ idx int }
type PersonaItemWrap struct{ idx int }

func (m ModeItem) Title() string {
	if m.idx < 0 || m.idx >= len(modeNames) {
		return "unknown"
	}
	return modeNames[m.idx]
}
func (m ModeItem) Description() string {
	if m.idx < 0 || m.idx >= len(modeDescs) {
		return ""
	}
	return modeDescs[m.idx]
}
func (m ModeItem) FilterValue() string { return m.Title() + " " + m.Description() }

func (p PersonaItemWrap) Title() string {
	if p.idx < 0 || p.idx >= len(personaNames) {
		return "unknown"
	}
	return personaNames[p.idx]
}
func (p PersonaItemWrap) Description() string {
	if p.idx < 0 || p.idx >= len(personaDescs) {
		return ""
	}
	return personaDescs[p.idx]
}
func (p PersonaItemWrap) FilterValue() string { return p.Title() + " " + p.Description() }

// Global name/desc slices populated at init (needed for Item interface methods)
var (
	modeNames   []string
	modeDescs   []string
	personaNames []string
	personaDescs []string
)

// ---------------------------------------------------------------------------
// Data loading
// ---------------------------------------------------------------------------
func loadModesFromDisk(mpmDir string) ([]map[string]interface{}, []string, []string) {
	var rawData []map[string]interface{}
	var names, descs []string

	paths := []string{
		filepath.Join(mpmDir, "mode"),
		filepath.Join(mpmDir, "..", "mpm", "mode"),
		"/home/v/.openclaw/workspace/flowbyte/mpm/mode",
	}
	var modeDir string
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			modeDir = p
			break
		}
	}
	if modeDir == "" {
		return nil, nil, nil
	}

	entries, _ := os.ReadDir(modeDir)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if e.Name() == "registry.md" || e.Name() == "files.md" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(modeDir, e.Name()))
		if err != nil {
			continue
		}
		var raw map[string]interface{}
		if json.Unmarshal(data, &raw) != nil {
			continue
		}
		rawData = append(rawData, raw)

		name := getString(raw, "name")
		if name == "" {
			name = strings.TrimSuffix(e.Name(), ".json")
		}
		title := getString(raw, "title")
		if title == "" {
			title = name
		}
		names = append(names, title)

		desc := getString(raw, "purpose")
		if desc == "" {
			desc = getString(raw, "focus")
		}
		if desc == "" {
			desc = getString(raw, "description")
		}
		descs = append(descs, desc)
	}
	return rawData, names, descs
}

func loadPersonasFromDisk(mpmDir string) ([]map[string]interface{}, []string, []string) {
	var rawData []map[string]interface{}
	var names, descs []string

	paths := []string{
		filepath.Join(mpmDir, "persona"),
		filepath.Join(mpmDir, "..", "mpm", "persona"),
		"/home/v/.openclaw/workspace/flowbyte/mpm/persona",
	}
	var personaDir string
	for _, p := range paths {
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			personaDir = p
			break
		}
	}
	if personaDir == "" {
		return nil, nil, nil
	}

	entries, _ := os.ReadDir(personaDir)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if e.Name() == "registry.md" || e.Name() == "files.md" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(personaDir, e.Name()))
		if err != nil {
			continue
		}
		var raw map[string]interface{}
		if json.Unmarshal(data, &raw) != nil {
			continue
		}
		rawData = append(rawData, raw)

		name := getString(raw, "name")
		if name == "" {
			name = strings.TrimSuffix(e.Name(), ".json")
		}
		title := getString(raw, "title")
		if title == "" {
			title = name
		}
		names = append(names, title)

		desc := getString(raw, "vibe")
		if desc == "" {
			desc = getString(raw, "creature")
		}
		if desc == "" {
			desc = getString(raw, "description")
		}
		descs = append(descs, desc)
	}
	return rawData, names, descs
}

func getString(raw map[string]interface{}, key string) string {
	if v, ok := raw[key].(string); ok {
		return v
	}
	return ""
}

func getStringSlice(raw map[string]interface{}, key string) []string {
	var out []string
	if arr, ok := raw[key].([]interface{}); ok {
		for _, v := range arr {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func cleanMarkdown(s string) string {
	s = regexp.MustCompile(`\*\*(.+?)\*\*`).ReplaceAllString(s, "$1")
	s = regexp.MustCompile(`\*(.+?)\*`).ReplaceAllString(s, "$1")
	s = regexp.MustCompile(`_(.+?)_`).ReplaceAllString(s, "$1")
	return strings.TrimSpace(s)
}

// ---------------------------------------------------------------------------
// Init
// ---------------------------------------------------------------------------
func newTUIModel() *Model {
	mpmDir := "/home/v/.openclaw/workspace/flowbyte/mpm"
	if _, err := os.Stat(mpmDir); os.IsNotExist(err) {
		mpmDir = "/home/v/.openclaw/workspace"
	}

	modeRaw, mNames, mDescs := loadModesFromDisk(mpmDir)
	personaRaw, pNames, pDescs := loadPersonasFromDisk(mpmDir)

	modeNames, modeDescs = mNames, mDescs
	personaNames, personaDescs = pNames, pDescs

	var modeItems []list.Item
	for i := range modeNames {
		modeItems = append(modeItems, ModeItem{idx: i})
	}
	if len(modeItems) == 0 {
		modeItems = []list.Item{ModeItem{idx: -1}}
		modeNames = []string{"default"}
		modeDescs = []string{"General purpose"}
	}

	var personaItems []list.Item
	for i := range personaNames {
		personaItems = append(personaItems, PersonaItemWrap{idx: i})
	}
	if len(personaItems) == 0 {
		personaItems = []list.Item{PersonaItemWrap{idx: -1}}
		personaNames = []string{"808"}
		personaDescs = []string{"The Great 808"}
	}

	modeList := list.New(modeItems, newModeDelegate(), 36, 18)
	modeList.Title = " Modes "
	modeList.SetStatusBarItemName("mode", "modes")
	modeList.SetShowFilter(true)
	modeList.SetFilteringEnabled(true)

	personaList := list.New(personaItems, newPersonaDelegate(), 36, 18)
	personaList.Title = " Personas "
	personaList.SetStatusBarItemName("persona", "personas")
	personaList.SetShowFilter(true)
	personaList.SetFilteringEnabled(true)

	return &Model{
		mpmDir:        mpmDir,
		modeList:      modeList,
		personaList:   personaList,
		activePanel:   0,
		selectedModes: make(map[int]bool),
		modeData:      modeRaw,
		personaData:   personaRaw,
	}
}

func (m *Model) Init() tea.Cmd {
	return nil
}

// ---------------------------------------------------------------------------
// Update
// ---------------------------------------------------------------------------
func (m *Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c", "q":
			m.quitting = true
			return m, tea.Quit

		case "tab":
			m.activePanel = 1 - m.activePanel
			return m, nil

		case " ":
			if m.activePanel == 0 {
				idx := m.modeList.Index()
				if _, ok := m.selectedModes[idx]; ok {
					delete(m.selectedModes, idx)
				} else {
					m.selectedModes[idx] = true
				}
				m.modeList.CursorDown()
				return m, nil
			}

		case "enter":
			if m.activePanel == 0 {
				return m, m.applyModes
			}
			return m, m.applyPersona
		}
	}

	var cmd tea.Cmd
	if m.activePanel == 0 {
		m.modeList, cmd = m.modeList.Update(msg)
	} else {
		m.personaList, cmd = m.personaList.Update(msg)
	}
	return m, cmd
}

// ---------------------------------------------------------------------------
// Commands
// ---------------------------------------------------------------------------
func (m *Model) applyModes() tea.Msg {
	toApply := m.selectedModes
	if len(toApply) == 0 {
		idx := m.modeList.Index()
		toApply = map[int]bool{idx: true}
	}
	for idx := range toApply {
		if idx >= 0 && idx < len(m.modeList.Items()) {
			item := m.modeList.Items()[idx].(ModeItem)
			exec.Command("/home/v/.openclaw/workspace/flowbyte/mpm/mpm", "mode", "set", modeNames[item.idx]).Run()
		}
	}
	m.selectedModes = make(map[int]bool)
	return nil
}

func (m *Model) applyPersona() tea.Msg {
	idx := m.personaList.Index()
	if idx >= 0 && idx < len(m.personaList.Items()) {
		item := m.personaList.Items()[idx].(PersonaItemWrap)
		exec.Command("/home/v/.openclaw/workspace/flowbyte/mpm/mpm", "persona", "set", personaNames[item.idx]).Run()
	}
	return nil
}

// ---------------------------------------------------------------------------
// View
// ---------------------------------------------------------------------------
func (m *Model) View() string {
	if m.quitting {
		return "\n  Goodbye, lobster friend! 🦞\n\n"
	}

	modePane := m.renderModeList()
	personaPane := m.renderPersonaList()
	previewPane := m.renderPreview()

	lists := lipgloss.JoinVertical(lipgloss.Top, modePane, personaPane)

	return fmt.Sprintf(
		"%s\n\n  %s\n\n%s\n%s\n\n%s\n",
		TitleStyle.Render("⟨ mpm-tui ⟩  ·  MPM Dashboard"),
		m.renderTabBar(),
		lists,
		previewPane,
		m.renderFooter(),
	)
}

func (m *Model) renderTabBar() string {
	dim := "  modes"
	active := "◀ modes ▶"
	modeBar := SubTitleStyle.Render(active) + DimStyle.Render(dim)
	if m.activePanel == 1 {
		modeBar = DimStyle.Render("◀ " + dim[2:]) + SubTitleStyle.Render("modes ▶")
	}

	dim2 := "personas  "
	active2 := "◀ personas ▶"
	persBar := SubTitleStyle.Render(active2) + DimStyle.Render(dim2)
	if m.activePanel == 0 {
		persBar = DimStyle.Render("◀ " + dim2[2:]) + SubTitleStyle.Render("personas ▶")
	}

	focus := "← →  switch pane   space  toggle   ↵ apply   q  quit"
	return fmt.Sprintf("%s     %s     %s", modeBar, persBar, HelpStyle.Render(focus))
}

func (m *Model) renderModeList() string {
	v := m.modeList.View()
	bar := ""
	if len(m.selectedModes) > 0 {
		bar = fmt.Sprintf(" %d selected", len(m.selectedModes))
		bar = lipgloss.NewStyle().Foreground(ColorCyan).Bold(true).Render(bar)
	}
	title := fmt.Sprintf(" Modes %s ", bar)
	v = withTitle(title, v, m.activePanel == 0)
	return v
}

func (m *Model) renderPersonaList() string {
	v := m.personaList.View()
	title := " Personas "
	v = withTitle(title, v, m.activePanel == 1)
	return v
}

func withTitle(title string, content string, focused bool) string {
	border := lipgloss.NormalBorder()
	if focused {
		border = lipgloss.ThickBorder()
	}
	bc := ColorBorder
	if focused {
		bc = ColorCyan
	}
	styled := lipgloss.NewStyle().
		BorderStyle(border).
		BorderForeground(bc).
		Padding(0, 1, 0, 1).
		Render(title + "\n" + content)
	return styled
}

// ---------------------------------------------------------------------------
// Preview pane
// ---------------------------------------------------------------------------
func (m *Model) renderPreview() string {
	var body string
	if m.activePanel == 0 {
		idx := m.modeList.Index()
		if idx >= 0 && idx < len(m.modeData) {
			body = formatModePreview(m.modeData[idx])
		} else {
			body = DimStyle.Render("No mode data")
		}
	} else {
		idx := m.personaList.Index()
		if idx >= 0 && idx < len(m.personaData) {
			body = formatPersonaPreview(m.personaData[idx])
		} else {
			body = DimStyle.Render("No persona data")
		}
	}

	header := SectionStyle.Render("Preview")
	styled := lipgloss.NewStyle().
		BorderStyle(lipgloss.NormalBorder()).
		BorderForeground(ColorBorder).
		Padding(1, 2).
		Width(52).
		Render(header + "\n" + body)
	return lipgloss.NewStyle().MarginLeft(2).Render(styled)
}

func formatModePreview(raw map[string]interface{}) string {
	var lines []string

	if s := getString(raw, "title"); s != "" {
		lines = append(lines, SubTitleStyle.Render("→ "+s))
	}
	if s := getString(raw, "purpose"); s != "" {
		lines = append(lines, "", ValueStyle.Render(cleanMarkdown(s)))
	}
	if s := getString(raw, "focus"); s != "" {
		lines = append(lines, "", SectionStyle.Render("Focus"), RuleStyle.Render(cleanMarkdown(s)))
	}

	if rules := getStringSlice(raw, "rules"); len(rules) > 0 {
		lines = append(lines, "", SectionStyle.Render("Rules"))
		for _, r := range rules {
			lines = append(lines, RuleStyle.Render("• "+cleanMarkdown(r)))
		}
	}

	if patterns := getStringSlice(raw, "behavioral_patterns"); len(patterns) > 0 {
		lines = append(lines, "", SectionStyle.Render("Patterns"))
		for _, p := range patterns {
			if len(lines) > 12 {
				break
			}
			lines = append(lines, RuleStyle.Render("+ "+cleanMarkdown(p)))
		}
	}

	if anti := getStringSlice(raw, "anti_patterns"); len(anti) > 0 {
		lines = append(lines, "", SectionStyle.Render("Anti-Patterns"))
		for _, a := range anti {
			lines = append(lines, RuleStyle.Render("− "+cleanMarkdown(a)))
		}
	}

	if exit := getStringSlice(raw, "exit_criteria"); len(exit) > 0 {
		lines = append(lines, "", SectionStyle.Render("Exit Criteria"))
		for _, e := range exit {
			lines = append(lines, RuleStyle.Render("✓ "+cleanMarkdown(e)))
		}
	}

	if len(lines) > 20 {
		lines = append(lines[:20], DimStyle.Render("... more in file"))
	}
	return strings.Join(lines, "\n")
}

func formatPersonaPreview(raw map[string]interface{}) string {
	var lines []string

	if s := getString(raw, "title"); s != "" {
		lines = append(lines, SubTitleStyle.Render("→ "+s))
	}
	if s := getString(raw, "creature"); s != "" {
		lines = append(lines, ValueStyle.Render(cleanMarkdown(s)))
	}
	if s := getString(raw, "vibe"); s != "" {
		lines = append(lines, "", ValueStyle.Render(cleanMarkdown(s)))
	}
	if s := getString(raw, "voice"); s != "" {
		lines = append(lines, "", SectionStyle.Render("Voice"), RuleStyle.Render(cleanMarkdown(s)))
	}

	if rules := getStringSlice(raw, "rules"); len(rules) > 0 {
		lines = append(lines, "", SectionStyle.Render("Rules"))
		for _, r := range rules {
			if len(lines) > 12 {
				break
			}
			lines = append(lines, RuleStyle.Render("• "+cleanMarkdown(r)))
		}
	}

	if quirks := getStringSlice(raw, "quirks"); len(quirks) > 0 {
		lines = append(lines, "", SectionStyle.Render("Quirks"))
		for _, q := range quirks {
			lines = append(lines, RuleStyle.Render("~ "+cleanMarkdown(q)))
		}
	}

	if topics := getStringSlice(raw, "topics"); len(topics) > 0 {
		lines = append(lines, "", SectionStyle.Render("Topics"))
		topicStr := ""
		for _, t := range topics {
			if len(topicStr) > 60 {
				break
			}
			if topicStr != "" {
				topicStr += ", "
			}
			topicStr += cleanMarkdown(t)
		}
		lines = append(lines, RuleStyle.Render(topicStr))
	}

	if s := getString(raw, "tone"); s != "" {
		lines = append(lines, "", SectionStyle.Render("Tone"), RuleStyle.Render(cleanMarkdown(s)))
	}
	if s := getString(raw, "style"); s != "" {
		lines = append(lines, "", SectionStyle.Render("Style"), RuleStyle.Render(cleanMarkdown(s)))
	}

	if emojis, ok := raw["emoji"].([]interface{}); ok && len(emojis) > 0 {
		var emojiStrs []string
		for _, e := range emojis {
			if es, ok := e.(string); ok {
				emojiStrs = append(emojiStrs, es)
			}
		}
		if len(emojiStrs) > 0 {
			lines = append(lines, "", strings.Join(emojiStrs, "  "))
		}
	}

	if len(lines) > 20 {
		lines = append(lines[:20], DimStyle.Render("... more in file"))
	}
	return strings.Join(lines, "\n")
}

// ---------------------------------------------------------------------------
// Footer: system status
// ---------------------------------------------------------------------------
func (m *Model) renderFooter() string {
	var parts []string

	// MPM daemon status
	sockPath := "/home/v/.openclaw/workspace/flowbyte/mpm/mpm.sock"
	if _, err := os.Stat(sockPath); err == nil {
		parts = append(parts, fmt.Sprintf("daemon %s running", StatusOK))
	} else {
		xdgRuntime := os.Getenv("XDG_RUNTIME_DIR")
		if xdgRuntime != "" {
			if _, err := os.Stat(filepath.Join(xdgRuntime, "mpm.sock")); err == nil {
				parts = append(parts, fmt.Sprintf("daemon %s running", StatusOK))
			} else {
				parts = append(parts, fmt.Sprintf("daemon %s offline", StatusDim))
			}
		} else {
			parts = append(parts, fmt.Sprintf("daemon %s offline", StatusDim))
		}
	}

	// Active mode
	modeActive := "not set"
	if data, err := os.ReadFile(filepath.Join(m.mpmDir, "active.json")); err == nil {
		var raw map[string]interface{}
		if json.Unmarshal(data, &raw) == nil {
			if v, ok := raw["mode"].(string); ok && v != "" {
				modeActive = v
			}
		}
	}
	parts = append(parts, fmt.Sprintf("mode: %s", SubTitleStyle.Render(modeActive)))

	// Active persona
	persActive := "not set"
	if data, err := os.ReadFile(filepath.Join(m.mpmDir, "active.json")); err == nil {
		var raw map[string]interface{}
		if json.Unmarshal(data, &raw) == nil {
			if v, ok := raw["persona"].(string); ok && v != "" {
				persActive = v
			}
		}
	}
	parts = append(parts, fmt.Sprintf("persona: %s", SubTitleStyle.Render(persActive)))

	parts = append(parts, fmt.Sprintf("%d modes · %d personas", len(modeNames), len(personaNames)))

	return HelpStyle.Render(strings.Join(parts, "   ·   "))
}

// ---------------------------------------------------------------------------
// Delegates
// ---------------------------------------------------------------------------
func newModeDelegate() list.ItemDelegate {
	delegate := list.NewDefaultDelegate()
	delegate.Styles.SelectedTitle = lipgloss.NewStyle().
		Foreground(ColorMagenta).
		Bold(true)
	delegate.Styles.NormalTitle = lipgloss.NewStyle().
		Foreground(lipgloss.Color("252"))
	delegate.Styles.FilterMatch = lipgloss.NewStyle().
		Foreground(ColorCyan).
		Bold(true)
	return delegate
}

func newPersonaDelegate() list.ItemDelegate {
	delegate := list.NewDefaultDelegate()
	delegate.Styles.SelectedTitle = lipgloss.NewStyle().
		Foreground(ColorCyan).
		Bold(true)
	delegate.Styles.NormalTitle = lipgloss.NewStyle().
		Foreground(lipgloss.Color("252"))
	delegate.Styles.FilterMatch = lipgloss.NewStyle().
		Foreground(ColorCyan).
		Bold(true)
	return delegate
}

// ---------------------------------------------------------------------------
// StartTUI is the entry point for `mpm tui`
// ---------------------------------------------------------------------------
func StartTUI() {
	// Check terminal size before launching
	if !checkTerminalSize() {
		return
	}
	m := newTUIModel()
	if _, err := tea.NewProgram(m, tea.WithAltScreen()).Run(); err != nil {
		fmt.Fprintf(os.Stderr, "Error running TUI: %v\n", err)
		os.Exit(1)
	}
}

// checkTerminalSize verifies the terminal is large enough for the TUI
func checkTerminalSize() bool {
	cols := 80
	lines := 24
	if c := os.Getenv("COLUMNS"); c != "" {
		if n, err := fmt.Sscanf(c, "%d", &cols); err == nil && n > 0 {
		}
	}
	if l := os.Getenv("LINES"); l != "" {
		if n, err := fmt.Sscanf(l, "%d", &lines); err == nil && n > 0 {
		}
	}
	if cols < 80 || lines < 20 {
		fmt.Fprintf(os.Stderr, "\n  mpm tui requires at least 80x24 terminal\n")
		fmt.Fprintf(os.Stderr, "  Current: %dx%d\n\n", cols, lines)
		return false
	}
	return true
}
