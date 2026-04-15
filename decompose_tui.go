// decompose_tui.go — Bubbletea workbench for the decomposition pipeline.
// Launched via: cog decompose --workbench <file>

package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// === Styles ===

var (
	// Tier header colors match the CLI formatter.
	tierColors = [4]lipgloss.Color{
		lipgloss.Color("#00FFFF"), // T0 cyan
		lipgloss.Color("#00FF00"), // T1 green
		lipgloss.Color("#FFFF00"), // T2 yellow
		lipgloss.Color("#5555FF"), // T3 blue
	}

	tierNames = [4]string{"T0 Sentence", "T1 Paragraph", "T2 CogDoc", "T3 Raw"}

	activeBorderStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(lipgloss.Color("#00FFFF"))

	inactiveBorderStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(mutedColor)

	decompTitleBarStyle = lipgloss.NewStyle().
				Bold(true).
				Foreground(lipgloss.Color("#FFFFFF")).
				Background(lipgloss.Color("#7C3AED")).
				Padding(0, 1)

	metricsBarStyle = lipgloss.NewStyle().
			Foreground(lipgloss.Color("#D1D5DB")).
			Background(lipgloss.Color("#374151")).
			Padding(0, 1)

	helpBarStyle = lipgloss.NewStyle().
			Foreground(mutedColor)

	spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
)

// === Messages ===

type decompDoneMsg struct {
	result *DecompositionResult
	err    error
}

// === Model ===

type decompTUIModel struct {
	input     *DecompInput
	result    *DecompositionResult
	runner    *DecompositionRunner
	viewports [4]viewport.Model
	active    int  // 0-3, which viewport has focus
	running   bool
	err       error
	width     int
	height    int
	ready     bool
}

func initialDecompTUIModel(input *DecompInput, result *DecompositionResult, runner *DecompositionRunner) decompTUIModel {
	var vps [4]viewport.Model
	for i := range vps {
		vps[i] = viewport.New(40, 10)
	}

	m := decompTUIModel{
		input:     input,
		result:    result,
		runner:    runner,
		viewports: vps,
		active:    0,
	}
	if result != nil {
		m.populateViewports()
	}
	return m
}

func (m *decompTUIModel) populateViewports() {
	r := m.result
	if r == nil {
		return
	}

	// T0
	if r.Tier0 != nil {
		m.viewports[0].SetContent(r.Tier0.Summary)
	} else {
		m.viewports[0].SetContent("(not generated)")
	}

	// T1
	if r.Tier1 != nil {
		var sb strings.Builder
		sb.WriteString(r.Tier1.Summary)
		if len(r.Tier1.KeyTerms) > 0 {
			sb.WriteString("\n\nKey terms: ")
			sb.WriteString(strings.Join(r.Tier1.KeyTerms, ", "))
		}
		m.viewports[1].SetContent(sb.String())
	} else {
		m.viewports[1].SetContent("(not generated)")
	}

	// T2
	if r.Tier2 != nil {
		var sb strings.Builder
		fmt.Fprintf(&sb, "Title: %s\n", r.Tier2.Title)
		fmt.Fprintf(&sb, "Type:  %s\n", r.Tier2.Type)
		fmt.Fprintf(&sb, "Tags:  %s\n", strings.Join(r.Tier2.Tags, ", "))
		sb.WriteString("\n")
		sb.WriteString(r.Tier2.Summary)
		for _, s := range r.Tier2.Sections {
			fmt.Fprintf(&sb, "\n\n## %s\n%s", s.Heading, s.Content)
		}
		if len(r.Tier2.Refs) > 0 {
			sb.WriteString("\n\nRefs:")
			for _, ref := range r.Tier2.Refs {
				fmt.Fprintf(&sb, "\n  %s (%s)", ref.URI, ref.Relation)
			}
		}
		m.viewports[2].SetContent(sb.String())
	} else {
		m.viewports[2].SetContent("(not generated)")
	}

	// T3
	if r.Tier3Raw != "" {
		m.viewports[3].SetContent(r.Tier3Raw)
	} else {
		m.viewports[3].SetContent("(not generated)")
	}
}

// === Bubbletea Interface ===

func (m decompTUIModel) Init() tea.Cmd {
	return nil
}

func (m decompTUIModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "tab":
			m.active = (m.active + 1) % 4
			return m, nil
		case "shift+tab":
			m.active = (m.active + 3) % 4
			return m, nil
		case "1":
			m.active = 0
			return m, nil
		case "2":
			m.active = 1
			return m, nil
		case "3":
			m.active = 2
			return m, nil
		case "4":
			m.active = 3
			return m, nil
		case "r":
			if !m.running {
				m.running = true
				m.err = nil
				return m, m.rerunCmd()
			}
			return m, nil
		}

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		m.ready = true
		m.resizeViewports()
		return m, nil

	case decompDoneMsg:
		m.running = false
		if msg.err != nil {
			m.err = msg.err
		} else {
			m.result = msg.result
			m.populateViewports()
		}
		return m, nil
	}

	// Forward scroll events to the active viewport.
	var cmd tea.Cmd
	m.viewports[m.active], cmd = m.viewports[m.active].Update(msg)
	return m, cmd
}

func (m *decompTUIModel) rerunCmd() tea.Cmd {
	return func() tea.Msg {
		result, err := m.runner.Run(context.Background(), m.input)
		return decompDoneMsg{result: result, err: err}
	}
}

func (m *decompTUIModel) resizeViewports() {
	if m.width == 0 || m.height == 0 {
		return
	}
	// Layout: title(1) + grid(2 rows) + metrics(1) + help(1) + borders
	// Each panel has 2 lines of border (top+bottom) and 1 line header
	panelW := (m.width - 1) / 2 // two columns, 1 char divider
	contentW := panelW - 4       // border(2) + padding(2)
	if contentW < 10 {
		contentW = 10
	}

	// Vertical: title=1, metrics=1, help=1, two rows of panels
	usableH := m.height - 3 // title + metrics + help
	panelH := usableH / 2
	contentH := panelH - 3 // border(2) + header(1)
	if contentH < 2 {
		contentH = 2
	}

	for i := range m.viewports {
		m.viewports[i].Width = contentW
		m.viewports[i].Height = contentH
	}
}

func (m decompTUIModel) View() string {
	if !m.ready {
		return "Initializing..."
	}

	var sb strings.Builder

	// Title bar
	sourceName := "stdin"
	if m.input.SourceURI != "" {
		sourceName = filepath.Base(strings.TrimPrefix(m.input.SourceURI, "file://"))
	}
	title := fmt.Sprintf(" Decomposition Workbench — %s (%s bytes) ",
		sourceName, decompFormatBytes(m.input.ByteSize))
	titleBar := decompTitleBarStyle.Width(m.width).Render(title)
	sb.WriteString(titleBar)
	sb.WriteString("\n")

	// 2x2 grid
	panelW := (m.width - 1) / 2
	if panelW < 12 {
		panelW = 12
	}

	topRow := m.renderPanelRow(0, 1, panelW)
	sb.WriteString(topRow)

	bottomRow := m.renderPanelRow(2, 3, panelW)
	sb.WriteString(bottomRow)

	// Metrics bar
	metrics := m.renderMetrics()
	metricsBar := metricsBarStyle.Width(m.width).Render(metrics)
	sb.WriteString(metricsBar)
	sb.WriteString("\n")

	// Help bar
	help := "[r] Re-run  [1-4] Focus tier  [Tab] Next  [q] Quit"
	if m.running {
		help = "Running...  [q] Quit"
	}
	helpBar := helpBarStyle.Width(m.width).Render(help)
	sb.WriteString(helpBar)

	return sb.String()
}

func (m decompTUIModel) renderPanelRow(left, right, panelW int) string {
	leftPanel := m.renderPanel(left, panelW)
	rightPanel := m.renderPanel(right, panelW)
	return lipgloss.JoinHorizontal(lipgloss.Top, leftPanel, " ", rightPanel) + "\n"
}

func (m decompTUIModel) renderPanel(idx, totalW int) string {
	style := inactiveBorderStyle
	if idx == m.active {
		style = activeBorderStyle.BorderForeground(tierColors[idx])
	}

	headerStyle := lipgloss.NewStyle().Bold(true).Foreground(tierColors[idx])
	header := headerStyle.Render(tierNames[idx])

	content := header + "\n" + m.viewports[idx].View()

	return style.Width(totalW - 2).Render(content)
}

func (m decompTUIModel) renderMetrics() string {
	if m.err != nil {
		return fmt.Sprintf("Error: %v", m.err)
	}
	if m.running {
		return "Running decomposition..."
	}
	if m.result == nil {
		return "No results yet"
	}

	r := m.result
	parts := []string{
		fmt.Sprintf("Total: %s", formatLatency(r.Metrics.TotalLatencyMs)),
	}
	if r.Metrics.CompressionRatio > 0 {
		parts = append(parts, fmt.Sprintf("%.0f:1 compression", r.Metrics.CompressionRatio))
	}
	parts = append(parts, r.InputHash)
	return strings.Join(parts, " | ")
}

// === Entry Point ===

func runDecompWorkbench(input *DecompInput, runner *DecompositionRunner) error {
	// Run initial decomposition before starting TUI
	result, err := runner.Run(context.Background(), input)
	if err != nil {
		return fmt.Errorf("initial decomposition: %w", err)
	}

	m := initialDecompTUIModel(input, result, runner)
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err = p.Run()
	return err
}
