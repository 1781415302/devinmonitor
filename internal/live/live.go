// Package live implements the real-time bubbletea dashboard.
//
// It polls the Devin sessions.db on an interval and renders a responsive
// dashboard with four breakpoint tiers (Full/Compact/Mini/Tiny) plus
// Devin-specific views: request stream, context growth, latency distribution.
package live

import (
	"fmt"
	"sort"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/garywhat/devinmonitor/internal/i18n"
	"github.com/garywhat/devinmonitor/internal/model"
	"github.com/garywhat/devinmonitor/internal/reader"
	"github.com/garywhat/devinmonitor/internal/report"
	"github.com/garywhat/devinmonitor/internal/ui"
)

// Breakpoint tiers based on terminal width.
const (
	bpFull    = 120
	bpCompact = 80
)

// Styles
var (
	borderStyle = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("39"))
	labelStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	valueStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	tokenStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("41"))
	costStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("220"))
	warnStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	dimStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	greenStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	yellowStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("220"))
	redStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
)

type model_ struct {
	reader    reader.Reader
	interval  time.Duration
	width     int
	height    int
	sessions  []model.Session
	current   int // index into sessions
	tab       int // 0=stream 1=context 2=latency 3=tools (compact mode)
	theme     int // 0=dark 1=light
	lastPoll  time.Time
	err       error
	quitting  bool
	loading   bool // true until first poll completes
}

type tickMsg time.Time

func tick(interval time.Duration) tea.Cmd {
	return tea.Tick(interval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

type pollMsg struct {
	sessions []model.Session
	err      error
}

func poll(r reader.Reader) tea.Cmd {
	return func() tea.Msg {
		// Multi-source readers (Windows + WSL) re-snapshot secondary stores
		// when the upstream sessions.db changed.
		if ref, ok := r.(reader.Refresher); ok {
			if err := ref.Refresh(); err != nil {
				// Keep last good data; surface refresh error only if Sessions also fails.
				_ = err
			}
		}
		ss, err := r.Sessions()
		return pollMsg{sessions: ss, err: err}
	}
}

// Run starts the live dashboard.
func Run(dataDir string, intervalSec int) error {
	r, err := reader.Open(dataDir)
	if err != nil {
		return err
	}
	defer r.Close()
	m := model_{
		reader:   r,
		interval: time.Duration(intervalSec) * time.Second,
		loading:  true,
	}
	if m.interval < 1*time.Second {
		m.interval = 3 * time.Second
	}
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, err = p.Run()
	return err
}

// RenderSnapshot renders the dashboard for the given sessions at a specific
// terminal size, without running bubbletea. Used for testing/visual inspection.
func RenderSnapshot(ss []model.Session, width, height int) string {
	return RenderSnapshotTab(ss, width, height, 0)
}

// RenderSnapshotTab renders the dashboard with a specific tab selected
// (0=stream, 1=context, 2=latency, 3=tools). Used for testing.
func RenderSnapshotTab(ss []model.Session, width, height, tab int) string {
	return RenderSnapshotSession(ss, width, height, 0, tab)
}

// RenderSnapshotSession renders the dashboard with a specific session index
// and tab selected. Used for testing.
func RenderSnapshotSession(ss []model.Session, width, height, current, tab int) string {
	m := model_{
		sessions: ss,
		width:    width,
		height:   height,
		current:  current,
		tab:      tab,
	}
	return m.View()
}

func (m model_) Init() tea.Cmd {
	return tea.Batch(poll(m.reader), tick(m.interval))
}

func (m model_) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case tickMsg:
		return m, tea.Batch(poll(m.reader), tick(m.interval))
	case pollMsg:
		m.loading = false
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		m.sessions = msg.sessions
		m.lastPoll = time.Now()
		m.err = nil
		// Keep current index valid.
		if m.current >= len(m.sessions) {
			m.current = 0
		}
		return m, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			m.quitting = true
			return m, tea.Quit
		case "r":
			// Cycle to next session.
			if len(m.sessions) > 0 {
				m.current = (m.current + 1) % len(m.sessions)
			}
		case "L":
			i18n.Cycle()
		case "1":
			m.tab = 0
		case "2":
			m.tab = 1
		case "3":
			m.tab = 2
		case "4":
			m.tab = 3
		case "tab":
			m.tab = (m.tab + 1) % 4
		}
	}
	return m, nil
}

func (m model_) View() string {
	if m.quitting {
		return ""
	}
	if m.loading {
		return "Loading… (polling sessions.db)"
	}
	if m.err != nil {
		return fmt.Sprintf("error: %v\npress q to quit", m.err)
	}
	if len(m.sessions) == 0 {
		return i18n.T("err.noSessions") + "\npress q to quit"
	}
	if m.width == 0 {
		return i18n.T("dash.header.session") + "..."
	}

	// Pick breakpoint tier.
	switch {
	case m.width >= bpFull && m.height >= 28:
		return m.viewFull()
	case m.width >= bpCompact && m.height >= 12:
		return m.viewCompact()
	case m.height < 6:
		return m.viewTiny()
	default:
		return m.viewMini()
	}
}

// ---- Full layout (>=120 cols, >=24 rows) ----

func (m model_) viewFull() string {
	s := m.currentSession()
	w := m.width
	h := m.height
	// Reserve 1 column to avoid terminal autowrap issues when content width
	// exactly equals terminal width (some terminals don't wrap correctly,
	// causing the last column to be overwritten).
	w--

	header := m.renderHeader(s, w)
	controls := dimStyle.Render(m.renderControls())

	// Each panel has border(2) + title(1) = 3 lines overhead.
	// Row 1 (tokens/context/status): tokens has 8 content lines,
	// context and status have 4 each — we pad them to match tokens.
	const panelOverhead = 3 // border(2) + title(1)
	row1Content := 8        // tokens panel has 8 content lines
	row1H := row1Content + panelOverhead

	// Available height for stream + row3 + tools:
	availH := h - 1 - 1 - row1H
	if availH < 6 {
		availH = 6
	}

	// Distribute availH among 3 panel rows (stream, row3, tools).
	// Each panel needs panelOverhead(3) lines, so content = panelH - 3.
	// stream: 35%, row3: 30%, tools: 35%
	// row3 needs at least 4 content lines (context growth: sparkline + 3 stat lines).
	minPanelH := panelOverhead + 3 // 3 content lines minimum
	row3MinPanelH := panelOverhead + 4 // 4 content lines for context growth
	streamPanelH := availH * 7 / 20
	if streamPanelH < minPanelH {
		streamPanelH = minPanelH
	}
	row3PanelH := availH * 3 / 10
	if row3PanelH < row3MinPanelH {
		row3PanelH = row3MinPanelH
	}
	toolsPanelH := availH - streamPanelH - row3PanelH
	if toolsPanelH < minPanelH {
		toolsPanelH = minPanelH
	}

	// Content lines for each panel.
	streamContent := streamPanelH - panelOverhead
	row3Content := row3PanelH - panelOverhead
	toolsContent := toolsPanelH - panelOverhead

	// Row 1: three columns — tokens, context, status
	// 2 gaps of 1 space between 3 panels.
	gap := 1
	colW := (w - gap*2) / 3
	if colW < 10 {
		colW = 10
	}
	colW3 := w - colW*2 - gap*2
	row1 := lipgloss.JoinHorizontal(lipgloss.Top,
		m.panel(i18n.T("dash.tokens.title"), m.renderTokens(s, colW), colW),
		strings.Repeat(" ", gap),
		m.panel(i18n.T("dash.context.title"), m.renderContext(s, colW), colW),
		strings.Repeat(" ", gap),
		m.panel(i18n.T("dash.status.title"), m.renderStatus(s, colW3), colW3),
	)

	// Row 2: request stream (full width, height-limited)
	stream := m.panel(i18n.T("dash.stream.title"), m.renderStreamLimited(s, w, streamContent), w)

	// Row 3: context growth (60%) + latency (40%)
	// Both panels must have the same content line count to avoid JoinHorizontal
	// padding issues. Use the latency panel's content line count as the target.
	leftW := w * 3 / 5
	rightW := w - leftW - gap
	latencyContent := m.renderLatencyLimited(s, rightW, row3Content)
	latencyLines := strings.Count(latencyContent, "\n") + 1
	ctxGrowth := m.renderContextGrowthPadded(s, leftW, latencyLines)
	row3 := lipgloss.JoinHorizontal(lipgloss.Top,
		m.panel(i18n.T("dash.context.growth"), ctxGrowth, leftW),
		strings.Repeat(" ", gap),
		m.panel(i18n.T("dash.latency.title"), latencyContent, rightW),
	)

	// Row 4: tools (full width, height-limited)
	tools := m.panel(i18n.T("dash.tools.title"), m.renderToolsLimited(s, w, toolsContent), w)

	return lipgloss.JoinVertical(lipgloss.Left,
		header, row1, stream, row3, tools, controls,
	)
}

// ---- Compact layout (80-119 cols, >=12 rows) ----

func (m model_) viewCompact() string {
	s := m.currentSession()
	w := m.width
	h := m.height
	w-- // reserve 1 column to avoid terminal autowrap issues

	header := m.renderHeader(s, w)

	// Row 1: tokens (50%) + status (50%)
	gap := 1
	colW := (w - gap) / 2
	colW2 := w - colW - gap
	row1 := lipgloss.JoinHorizontal(lipgloss.Top,
		m.panel(i18n.T("dash.tokens.title"), m.renderTokens(s, colW), colW),
		strings.Repeat(" ", gap),
		m.panel(i18n.T("dash.status.title"), m.renderStatus(s, colW2), colW2),
	)

	// Tab bar
	tabs := m.renderTabBar(w)

	controls := dimStyle.Render(m.renderControls())

	// Calculate available height for tab content panel.
	// header(1) + row1(~7 lines with border) + tabs(1) + controls(1) = ~10
	// content panel needs border(2) + title(1) = 3 lines overhead.
	usedH := 1 + 7 + 1 + 1 // approximate
	contentMaxH := h - usedH - 3 // 3 = panel border + title
	if contentMaxH < 3 {
		contentMaxH = 3
	}

	// Tab content (full width, height-limited)
	var content string
	switch m.tab {
	case 0:
		content = m.panel(i18n.T("dash.stream.title"), m.renderStreamLimited(s, w, contentMaxH), w)
	case 1:
		content = m.panel(i18n.T("dash.context.growth"), m.renderContextGrowth(s, w), w)
	case 2:
		content = m.panel(i18n.T("dash.latency.title"), m.renderLatencyLimited(s, w, contentMaxH), w)
	case 3:
		content = m.panel(i18n.T("dash.tools.title"), m.renderToolsLimited(s, w, contentMaxH), w)
	}

	return lipgloss.JoinVertical(lipgloss.Left, header, row1, tabs, content, controls)
}

// ---- Mini layout (<80 cols) ----

func (m model_) viewMini() string {
	s := m.currentSession()
	modelName := s.LatestModel
	if modelName == "" {
		modelName = s.Model
	}
	cost, _ := report.SessionCost(s)
	p := model.LookupPricing(modelName)
	if cost == 0 && !p.Free && p.InputPerM > 0 {
		cost = model.EstimateCost(p, s.InputTokens, s.OutputTokens, s.CacheRead, s.CacheWrite)
	}
	lines := []string{
		fmt.Sprintf("%s · %s", s.ID, modelName),
		fmt.Sprintf("tok %s  ctx %s  %s  %s",
			report.FormatTok(s.InputTokens+s.OutputTokens),
			"?", report.FormatCost(cost, p.Free), report.FormatDur(s.LastActivityAt.Sub(s.CreatedAt))),
	}
	// Recent request
	reqs := m.recentRequests(s, 2)
	for _, r := range reqs {
		lines = append(lines, fmt.Sprintf("#%d %s %.1fs %.0ftok/s",
			r.idx, i18n.T("dash.stream.ttft"), r.ttftSec, r.tokPerSec))
	}
	// sparkline
	if sl := m.ttftSparkline(s, m.width-10); sl != "" {
		lines = append(lines, sl)
	}
	// tools compact
	tools := m.toolSummary(s, 3)
	if tools != "" {
		lines = append(lines, tools)
	}
	lines = append(lines, dimStyle.Render(m.renderControls()))
	return strings.Join(lines, "\n")
}

// ---- Tiny layout (<6 rows) ----

func (m model_) viewTiny() string {
	s := m.currentSession()
	modelName := s.LatestModel
	if modelName == "" {
		modelName = s.Model
	}
	cost, _ := report.SessionCost(s)
	p := model.LookupPricing(modelName)
	if cost == 0 && !p.Free && p.InputPerM > 0 {
		cost = model.EstimateCost(p, s.InputTokens, s.OutputTokens, s.CacheRead, s.CacheWrite)
	}
	sep := " " + i18n.T("dash.tiny.sep") + " "
	return strings.Join([]string{
		modelName,
		"tok " + report.FormatTok(s.InputTokens+s.OutputTokens),
		"ctx ?",
		report.FormatCost(cost, p.Free),
		fmt.Sprintf("reqs %d", s.AssistantCount),
	}, sep)
}

// ---- Renderers ----

func (m model_) currentSession() *model.Session {
	if len(m.sessions) == 0 || m.current >= len(m.sessions) {
		return &model.Session{}
	}
	return &m.sessions[m.current]
}

func (m model_) renderHeader(s *model.Session, w int) string {
	title := s.Title
	if title == "" {
		title = s.ID
	}
	// Use LatestModel (from most recent assistant message) when available;
	// falls back to session-level Model (set at creation time).
	modelName := s.LatestModel
	if modelName == "" {
		modelName = s.Model
	}
	now := time.Now().Format("15:04:05")
	return titleStyle.Render(i18n.T("app.name")) + " " +
		labelStyle.Render(i18n.T("dash.header.session")+": ") + valueStyle.Render(title) + " " +
		labelStyle.Render(i18n.T("dash.header.model")+": ") + valueStyle.Render(modelName) + " " +
		labelStyle.Render(i18n.T("dash.header.mode")+": ") + valueStyle.Render(s.AgentMode) + " " +
		dimStyle.Render(now)
}

// labelValue renders a label and value with the label padded to a fixed display width.
// The label is padded BEFORE styling to ensure CJK width is correctly calculated.
func labelValue(label, value string, labelW int) string {
	// Pad the plain-text label to labelW display width.
	padded := label
	if dw := lipgloss.Width(label); dw < labelW {
		padded = label + strings.Repeat(" ", labelW-dw)
	}
	return labelStyle.Render(padded) + value
}

// rollingOutputRate computes aggregate output rate over a rolling time window.
// Devin issues concurrent requests, so a single request's tokens_per_sec
// understates true throughput. This function finds all unique assistant
// requests that completed within the last `window` duration and sums their
// individual tokens_per_sec — naturally accounting for concurrency.
//
// Devin stores each request twice (streaming + final) with the same request_id,
// so we deduplicate by request_id to avoid double-counting.
func rollingOutputRate(s *model.Session, window time.Duration) float64 {
	type evt struct {
		endTime time.Time
		tps     float64
	}
	seen := map[string]bool{}
	var events []evt
	for _, msg := range s.Messages {
		if msg.Role != "assistant" || msg.Metrics == nil {
			continue
		}
		m := msg.Metrics
		if m.TotalTimeMs <= 0 || m.OutputTokens <= 0 || m.TokensPerSec <= 0 {
			continue
		}
		// Deduplicate by request_id — Devin stores each request twice.
		if msg.RequestID != "" {
			if seen[msg.RequestID] {
				continue
			}
			seen[msg.RequestID] = true
		}
		end := msg.CreatedAt.Add(time.Duration(m.TotalTimeMs * float64(time.Millisecond)))
		events = append(events, evt{end, m.TokensPerSec})
	}
	if len(events) == 0 {
		return 0
	}
	// Find the latest completion time.
	var latest time.Time
	for _, e := range events {
		if e.endTime.After(latest) {
			latest = e.endTime
		}
	}
	// Sum tokens_per_sec of all requests completed within the window.
	// This gives the aggregate concurrent throughput.
	windowStart := latest.Add(-window)
	var sum float64
	for _, e := range events {
		if !e.endTime.Before(windowStart) {
			sum += e.tps
		}
	}
	return sum
}

func (m model_) renderTokens(s *model.Session, w int) string {
	const lw = 10 // label column width (display)
	totalTok := s.InputTokens + s.OutputTokens + s.CacheRead + s.CacheWrite
	// Per-request averages.
	reqs := s.AssistantCount
	avgIn := int64(0)
	avgOut := int64(0)
	if reqs > 0 {
		avgIn = s.InputTokens / int64(reqs)
		avgOut = s.OutputTokens / int64(reqs)
	}
	// Output ratio.
	netTok := s.InputTokens + s.OutputTokens
	outPct := 0.0
	if netTok > 0 {
		outPct = float64(s.OutputTokens) / float64(netTok) * 100
	}
	// Output rate: rolling 60-second window throughput.
	// Devin makes concurrent requests, so a single request's tokens_per_sec
	// understates the true throughput. Instead, we sum output_tokens from all
	// requests that completed within the last 60 seconds and divide by the
	// actual time span, giving an aggregate rate that accounts for concurrency.
	curRate := rollingOutputRate(s, 60*time.Second)
	lines := []string{
		labelValue(i18n.T("common.input"), tokenStyle.Render(report.FormatTok(s.InputTokens)), lw),
		labelValue(i18n.T("common.output"), tokenStyle.Render(report.FormatTok(s.OutputTokens)), lw),
		labelValue(i18n.T("common.cacheRead"), tokenStyle.Render(report.FormatTok(s.CacheRead)), lw),
		labelValue(i18n.T("common.cacheWr"), tokenStyle.Render(report.FormatTok(s.CacheWrite)), lw),
		labelValue(i18n.T("dash.tokens.rate"), tokenStyle.Render(fmt.Sprintf("%.0f tok/s", curRate)), lw),
		labelValue(i18n.T("common.total"), tokenStyle.Render(report.FormatTok(totalTok)), lw),
		labelValue(i18n.T("dash.tokens.avgIn"), dimStyle.Render(report.FormatTok(avgIn)), lw),
		labelValue(i18n.T("dash.tokens.avgOut"), dimStyle.Render(report.FormatTok(avgOut)+" "+fmt.Sprintf("%.0f%%", outPct)), lw),
	}
	return strings.Join(lines, "\n")
}

// contextSize returns the best estimate of current context window usage.
// Prefers num_tokens_preceding (exact), falls back to input_tokens of the
// latest assistant message (approximate — input_tokens ≈ context size).
func contextSize(s *model.Session) int64 {
	for i := len(s.Messages) - 1; i >= 0; i-- {
		msg := s.Messages[i]
		if msg.Role != "assistant" {
			continue
		}
		if msg.NumTokensPreceding > 0 {
			return int64(msg.NumTokensPreceding)
		}
		if msg.Metrics != nil && msg.Metrics.InputTokens > 0 {
			return msg.Metrics.InputTokens
		}
	}
	return 0
}

// contextGrowthPoints collects context size estimates over the session.
// Uses num_tokens_preceding when available, otherwise input_tokens.
func contextGrowthPoints(s *model.Session) []int64 {
	var pts []int64
	for _, msg := range s.Messages {
		if msg.Role != "assistant" {
			continue
		}
		if msg.NumTokensPreceding > 0 {
			pts = append(pts, int64(msg.NumTokensPreceding))
		} else if msg.Metrics != nil && msg.Metrics.InputTokens > 0 {
			pts = append(pts, msg.Metrics.InputTokens)
		}
	}
	return pts
}

func (m model_) renderContext(s *model.Session, w int) string {
	innerW := w - 4
	if innerW < 10 {
		innerW = 10
	}
	ctxSize := contextSize(s)
	// Estimate window from model (default 200k).
	window := 200000
	pct := float64(ctxSize) / float64(window) * 100
	if pct > 100 {
		pct = 100
	}
	bar := progressBar(pct, float64(innerW-14))
	color := pctColor(pct)
	remaining := int64(window) - ctxSize
	if remaining < 0 {
		remaining = 0
	}
	// Average and peak from growth points.
	pts := contextGrowthPoints(s)
	var avgCtx, peakCtx int64
	if len(pts) > 0 {
		var sum int64
		for _, v := range pts {
			sum += v
			if v > peakCtx {
				peakCtx = v
			}
		}
		avgCtx = sum / int64(len(pts))
	}
	// Request count with subagent distinction.
	subCount := len(s.SubAgentCalls)
	reqsStr := fmt.Sprintf("%d", s.AssistantCount)
	if subCount > 0 {
		reqsStr = fmt.Sprintf("%d (%d %s)", s.AssistantCount, subCount, i18n.T("common.subAgents"))
	}
	const lw = 10
	lines := []string{
		labelValue(i18n.T("dash.context.size"), valueStyle.Render(report.FormatTok(ctxSize)), lw),
		labelValue(i18n.T("dash.context.window"), valueStyle.Render(report.FormatTok(int64(window))), lw),
		color.Render(bar),
		labelValue(i18n.T("dash.context.avg"), dimStyle.Render(report.FormatTok(avgCtx)), lw),
		labelValue(i18n.T("dash.context.peak"), dimStyle.Render(report.FormatTok(peakCtx)), lw),
		labelValue(i18n.T("dash.context.remain"), dimStyle.Render(report.FormatTok(remaining)), lw),
		labelValue(i18n.T("common.cacheWr"), dimStyle.Render(report.FormatTok(s.CacheWrite)), lw),
		labelValue(i18n.T("common.requests"), valueStyle.Render(reqsStr), lw),
	}
	return strings.Join(lines, "\n")
}

func (m model_) renderStatus(s *model.Session, w int) string {
	innerW := w - 4
	if innerW < 10 {
		innerW = 10
	}
	// Use LatestModel for pricing lookup — it reflects the model the user
	// is actually using now, not the one the session started with.
	modelName := s.LatestModel
	if modelName == "" {
		modelName = s.Model
	}
	p := model.LookupPricing(modelName)
	cost, est := report.SessionCost(s)
	// If Devin's credit/ACU is zero but the model is paid, estimate from tokens.
	if cost == 0 && !p.Free && p.InputPerM > 0 {
		cost = model.EstimateCost(p, s.InputTokens, s.OutputTokens, s.CacheRead, s.CacheWrite)
		est = true
	}
	costStr := report.FormatCost(cost, p.Free)
	if est && cost > 0 {
		costStr = costStr + " " + dimStyle.Render("("+i18n.T("common.est")+")")
	}
	dur := s.LastActivityAt.Sub(s.CreatedAt)
	maxDur := 5 * time.Hour
	durPct := float64(dur) / float64(maxDur) * 100
	if durPct > 100 {
		durPct = 100
	}
	durBar := progressBar(durPct, float64(innerW-14))
	durColor := pctColor(durPct)
	// Average request duration, TTFT p50, and total time p50.
	avgReqDur := time.Duration(0)
	var ttfts, totals []float64
	toolCallCount := 0
	if s.AssistantCount > 0 {
		avgReqDur = dur / time.Duration(s.AssistantCount)
	}
	for _, msg := range s.Messages {
		if msg.Role != "assistant" || msg.Metrics == nil {
			continue
		}
		if msg.Metrics.TTFTMs > 0 {
			ttfts = append(ttfts, msg.Metrics.TTFTMs)
		}
		if msg.Metrics.TotalTimeMs > 0 {
			totals = append(totals, msg.Metrics.TotalTimeMs)
		}
	}
	for _, c := range s.ToolCalls {
		toolCallCount += c
	}
	ttftP50 := model.Percentile(ttfts, 50) / 1000   // seconds
	totalP50 := model.Percentile(totals, 50) / 1000 // seconds
	subCount := len(s.SubAgentCalls)
	toolsStr := fmt.Sprintf("%d", toolCallCount)
	if subCount > 0 {
		toolsStr = fmt.Sprintf("%d (%d %s)", toolCallCount, subCount, i18n.T("common.subAgents"))
	}
	const lw = 10
	lines := []string{
		labelValue(i18n.T("dash.status.costSess"), costStyle.Render(costStr), lw),
		labelValue(i18n.T("common.requests"), valueStyle.Render(fmt.Sprintf("%d", s.AssistantCount)), lw),
		labelValue(i18n.T("dash.status.duration"), valueStyle.Render(report.FormatDur(dur)), lw),
		durColor.Render(durBar),
		labelValue(i18n.T("dash.status.avgReq"), dimStyle.Render(report.FormatDur(avgReqDur)), lw),
		labelValue(i18n.T("dash.status.ttft"), dimStyle.Render(fmt.Sprintf("%.1fs", ttftP50)), lw),
		labelValue(i18n.T("dash.status.totalP50"), dimStyle.Render(fmt.Sprintf("%.1fs", totalP50)), lw),
		labelValue(i18n.T("dash.status.tools"), dimStyle.Render(toolsStr), lw),
	}
	return strings.Join(lines, "\n")
}

type reqInfo struct {
	idx        int
	ttftSec    float64
	totalSec   float64
	tokPerSec  float64
	finish     string
	createdAt  time.Time
}

func (m model_) recentRequests(s *model.Session, n int) []reqInfo {
	var out []reqInfo
	for i := len(s.Messages) - 1; i >= 0 && len(out) < n; i-- {
		msg := s.Messages[i]
		if msg.Role != "assistant" || msg.Metrics == nil {
			continue
		}
		out = append(out, reqInfo{
			idx:       i,
			ttftSec:   msg.Metrics.TTFTMs / 1000,
			totalSec:  msg.Metrics.TotalTimeMs / 1000,
			tokPerSec: msg.Metrics.TokensPerSec,
			finish:    msg.FinishReason,
			createdAt: msg.CreatedAt,
		})
	}
	return out
}

func (m model_) renderStream(s *model.Session, w int) string {
	innerW := w - 4 // border + padding
	if innerW < 10 {
		innerW = 10
	}
	reqs := m.recentRequests(s, 8)
	if len(reqs) == 0 {
		return dimStyle.Render(i18n.T("common.none"))
	}
	// Reverse to newest at bottom (scrolling feel).
	var lines []string
	for i := len(reqs) - 1; i >= 0; i-- {
		r := reqs[i]
		reason := r.finish
		if reason == "" {
			reason = "-"
		}
		finishStyle := labelStyle
		if reason == "length" {
			finishStyle = warnStyle
		}
		lines = append(lines, fmt.Sprintf("#%-4d %s %.1fs %s %.0f %s %.1fs %s",
			r.idx,
			labelStyle.Render(i18n.T("dash.stream.ttft")),
			r.ttftSec,
			labelStyle.Render(i18n.T("dash.stream.rate")),
			r.tokPerSec,
			labelStyle.Render(i18n.T("dash.stream.elapsed")),
			r.totalSec,
			finishStyle.Render(reason),
		))
	}
	// Sparkline of ttft
	if line := m.renderTrendLine(s, innerW); line != "" {
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

// renderTrendLine builds the TTFT sparkline trend line with p50/p95 suffix.
// The sparkline width is computed from the actual suffix width so the line
// never overflows innerW.
func (m model_) renderTrendLine(s *model.Session, innerW int) string {
	p50 := m.ttftP50(s) / 1000
	p95 := m.ttftP95(s) / 1000
	suffix := "  " + labelStyle.Render(fmt.Sprintf(i18n.T("dash.stream.p50")+"=%.1fs ", p50)) +
		labelStyle.Render(fmt.Sprintf(i18n.T("dash.stream.p95")+"=%.1fs", p95))
	prefix := dimStyle.Render(i18n.T("dash.stream.trend") + ": ")
	// Available width for sparkline = innerW - display width of prefix - display width of suffix
	sparkW := innerW - lipgloss.Width(prefix) - lipgloss.Width(suffix)
	if sparkW < 4 {
		// Not enough room; drop the sparkline, just show stats.
		return prefix + suffix
	}
	sl := m.ttftSparkline(s, sparkW)
	if sl == "" {
		return ""
	}
	return prefix + sl + suffix
}

// renderStreamLimited renders the request stream with at most maxLines content lines
// (excluding the sparkline trend line). Used by Full layout to fit terminal height.
func (m model_) renderStreamLimited(s *model.Session, w, maxLines int) string {
	innerW := w - 4
	if innerW < 10 {
		innerW = 10
	}
	if maxLines < 2 {
		maxLines = 2
	}
	// Reserve 1 line for sparkline trend if available.
	hasSpark := m.ttftSparkline(s, 4) != ""
	reqLines := maxLines
	if hasSpark {
		reqLines = maxLines - 1
	}
	if reqLines < 1 {
		reqLines = 1
	}
	reqs := m.recentRequests(s, reqLines)
	if len(reqs) == 0 {
		return dimStyle.Render(i18n.T("common.none"))
	}
	var lines []string
	for i := len(reqs) - 1; i >= 0; i-- {
		r := reqs[i]
		reason := r.finish
		if reason == "" {
			reason = "-"
		}
		finishStyle := labelStyle
		if reason == "length" {
			finishStyle = warnStyle
		}
		lines = append(lines, fmt.Sprintf("#%-4d %s %.1fs %s %.0f %s %.1fs %s",
			r.idx,
			labelStyle.Render(i18n.T("dash.stream.ttft")),
			r.ttftSec,
			labelStyle.Render(i18n.T("dash.stream.rate")),
			r.tokPerSec,
			labelStyle.Render(i18n.T("dash.stream.elapsed")),
			r.totalSec,
			finishStyle.Render(reason),
		))
	}
	if hasSpark {
		if line := m.renderTrendLine(s, innerW); line != "" {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

func (m model_) renderContextGrowth(s *model.Session, w int) string {
	innerW := w - 4
	if innerW < 10 {
		innerW = 10
	}
	pts := contextGrowthPoints(s)
	if len(pts) == 0 {
		return dimStyle.Render(i18n.T("common.none"))
	}
	// Sparkline uses full inner width.
	sparkW := innerW
	if sparkW < 4 {
		sparkW = 4
	}
	// Convert to float64 for sparkline rendering.
	fpts := make([]float64, len(pts))
	for i, v := range pts {
		fpts[i] = float64(v)
	}
	spark := sparklineFloats(fpts, sparkW)
	// Find max, current, avg, peak index.
	var maxVal, sumVal int64
	maxIdx := 0
	for i, v := range pts {
		if v > maxVal {
			maxVal = v
			maxIdx = i
		}
		sumVal += v
	}
	curVal := pts[len(pts)-1]
	avgVal := sumVal / int64(len(pts))
	// Estimate context window (default 200k).
	window := int64(200000)
	pct := float64(curVal) / float64(window) * 100
	if pct > 100 {
		pct = 100
	}
	// Cache hit rate = CacheRead / (CacheRead + InputTokens).
	cacheHit := 0.0
	totalIn := s.CacheRead + s.InputTokens
	if totalIn > 0 {
		cacheHit = float64(s.CacheRead) / float64(totalIn) * 100
	}
	// Per-request growth = (last - first) / num_points.
	perReqGrowth := int64(0)
	if len(pts) >= 2 {
		perReqGrowth = (curVal - pts[0]) / int64(len(pts))
	}
	// Estimated remaining requests until window is full.
	estLeft := "—"
	if perReqGrowth > 0 {
		remaining := (window - curVal) / perReqGrowth
		if remaining < 0 {
			remaining = 0
		}
		estLeft = fmt.Sprintf("~%d", remaining)
	}
	// Growth trend: compare last 25% to first 25%.
	var growthTrend string
	if len(pts) >= 4 {
		q1 := len(pts) / 4
		q3 := len(pts) * 3 / 4
		var earlySum, lateSum int64
		for i := 0; i < q1 && i < len(pts); i++ {
			earlySum += pts[i]
		}
		for i := q3; i < len(pts); i++ {
			lateSum += pts[i]
		}
		earlyAvg := float64(earlySum) / float64(q1)
		lateAvg := float64(lateSum) / float64(len(pts)-q3)
		if earlyAvg > 0 {
			delta := (lateAvg - earlyAvg) / earlyAvg * 100
			if delta > 0 {
				growthTrend = fmt.Sprintf("↑%.0f%%", delta)
			} else {
				growthTrend = fmt.Sprintf("↓%.0f%%", -delta)
			}
		}
	}

	// Compact label-value pair: "label value" with label dimmed.
	kv := func(label, value string) string {
		return dimStyle.Render(label+" ") + valueStyle.Render(value)
	}

	// Per-request growth display: show sign for positive, "↓" for negative (compacted).
	perReqStr := report.FormatTok(perReqGrowth)
	if perReqGrowth > 0 {
		perReqStr = "+" + perReqStr
	}

	// Line 1: sparkline (full width)
	// Line 2: current + pct | avg | peak
	// Line 3: cache hit | per-req growth | est. left
	// Line 4 (if available): growth trend | points count
	lines := []string{
		spark,
		kv(i18n.T("dash.context.size"), report.FormatTok(curVal)) + " " +
			dimStyle.Render(fmt.Sprintf("%.0f%%", pct)) + "  " +
			kv(i18n.T("dash.context.avg"), report.FormatTok(avgVal)) + "  " +
			kv(i18n.T("dash.context.peak"), report.FormatTok(maxVal)+" #"+fmt.Sprintf("%d", maxIdx+1)),
		kv(i18n.T("dash.context.cacheHit"), fmt.Sprintf("%.1f%%", cacheHit)) + "  " +
			kv(i18n.T("dash.context.perReq"), perReqStr) + "  " +
			kv(i18n.T("dash.context.estLeft"), estLeft),
	}
	if growthTrend != "" {
		lines = append(lines,
			dimStyle.Render(fmt.Sprintf("%d pts · ", len(pts)))+labelStyle.Render(growthTrend))
	}
	return strings.Join(lines, "\n")
}

// renderContextGrowthPadded renders context growth padded to exactly targetLines lines.
func (m model_) renderContextGrowthPadded(s *model.Session, w, targetLines int) string {
	content := m.renderContextGrowth(s, w)
	lines := strings.Split(content, "\n")
	for len(lines) < targetLines {
		lines = append(lines, "")
	}
	if len(lines) > targetLines {
		lines = lines[:targetLines]
	}
	return strings.Join(lines, "\n")
}

func (m model_) renderLatency(s *model.Session, w int) string {
	innerW := w - 4
	if innerW < 10 {
		innerW = 10
	}
	var ttfts, totals, tps []float64
	finishCounts := map[string]int{}
	for _, msg := range s.Messages {
		if msg.Role != "assistant" || msg.Metrics == nil {
			continue
		}
		if msg.Metrics.TTFTMs > 0 {
			ttfts = append(ttfts, msg.Metrics.TTFTMs)
		}
		if msg.Metrics.TotalTimeMs > 0 {
			totals = append(totals, msg.Metrics.TotalTimeMs)
		}
		if msg.Metrics.TokensPerSec > 0 {
			tps = append(tps, msg.Metrics.TokensPerSec)
		}
		if msg.FinishReason != "" {
			finishCounts[msg.FinishReason]++
		}
	}
	lines := []string{
		fmt.Sprintf("%s p50=%.1fs p95=%.1fs",
			labelStyle.Render(i18n.T("dash.latency.ttft")),
			model.Percentile(ttfts, 50)/1000, model.Percentile(ttfts, 95)/1000),
		fmt.Sprintf("%s p50=%.1fs p95=%.1fs",
			labelStyle.Render(i18n.T("dash.latency.total")),
			model.Percentile(totals, 50)/1000, model.Percentile(totals, 95)/1000),
		fmt.Sprintf("%s p50=%.0f",
			labelStyle.Render(i18n.T("dash.latency.toks")),
			model.Percentile(tps, 50)),
	}
	// Finish reason distribution
	total := 0
	for _, c := range finishCounts {
		total += c
	}
	if total > 0 {
		lines = append(lines, labelStyle.Render(i18n.T("dash.latency.finish"))+":")
		// Sort by count desc.
		type kv struct {
			k string
			v int
		}
		var kvs []kv
		for k, v := range finishCounts {
			kvs = append(kvs, kv{k, v})
		}
		sort.Slice(kvs, func(i, j int) bool { return kvs[i].v > kvs[j].v })
		for _, e := range kvs {
			pct := float64(e.v) / float64(total) * 100
			label := e.k
			style := labelStyle
			if e.k == "length" {
				label = i18n.T("dash.latency.length")
				style = warnStyle
			} else if e.k == "tool_calls" {
				label = i18n.T("dash.latency.tool")
			} else if e.k == "stop" {
				label = i18n.T("dash.latency.stop")
			}
			lines = append(lines, fmt.Sprintf("  %s %d%% %s",
				style.Render(label), int(pct), progressBar(pct, float64(innerW-20))))
		}
	}
	return strings.Join(lines, "\n")
}

// renderLatencyLimited renders latency stats with at most maxLines content lines.
func (m model_) renderLatencyLimited(s *model.Session, w, maxLines int) string {
	if maxLines < 3 {
		maxLines = 3
	}
	full := m.renderLatency(s, w)
	lines := strings.Split(full, "\n")
	if len(lines) <= maxLines {
		return full
	}
	// Keep first 3 stat lines, then as many finish-reason lines as fit.
	return strings.Join(lines[:maxLines], "\n")
}

func (m model_) renderTools(s *model.Session, w int) string {
	innerW := w - 4
	if innerW < 10 {
		innerW = 10
	}
	names := reader.SortedToolNames(s.ToolCalls)
	if len(names) == 0 {
		return dimStyle.Render(i18n.T("common.none"))
	}
	max := 0
	for _, n := range names {
		if s.ToolCalls[n] > max {
			max = s.ToolCalls[n]
		}
	}
	var lines []string
	for _, n := range names {
		c := s.ToolCalls[n]
		barLen := int(float64(c) / float64(max) * float64(innerW-20))
		if barLen < 1 {
			barLen = 1
		}
		lines = append(lines, fmt.Sprintf("%-12s %4d %s", n, c, strings.Repeat("█", barLen)))
	}
	return strings.Join(lines, "\n")
}

// renderToolsLimited renders tool usage with at most maxLines tools.
func (m model_) renderToolsLimited(s *model.Session, w, maxLines int) string {
	if maxLines < 1 {
		maxLines = 1
	}
	innerW := w - 4
	if innerW < 10 {
		innerW = 10
	}
	names := reader.SortedToolNames(s.ToolCalls)
	if len(names) == 0 {
		return dimStyle.Render(i18n.T("common.none"))
	}
	max := 0
	for _, n := range names {
		if s.ToolCalls[n] > max {
			max = s.ToolCalls[n]
		}
	}
	// Limit to top N tools.
	if len(names) > maxLines {
		names = names[:maxLines]
	}
	var lines []string
	for _, n := range names {
		c := s.ToolCalls[n]
		barLen := int(float64(c) / float64(max) * float64(innerW-20))
		if barLen < 1 {
			barLen = 1
		}
		lines = append(lines, fmt.Sprintf("%-12s %4d %s", n, c, strings.Repeat("█", barLen)))
	}
	return strings.Join(lines, "\n")
}

func (m model_) renderTabBar(w int) string {
	tabs := []string{
		i18n.T("dash.tab.stream"),
		i18n.T("dash.tab.context"),
		i18n.T("dash.tab.latency"),
		i18n.T("dash.tab.tools"),
	}
	var parts []string
	for i, t := range tabs {
		label := fmt.Sprintf("[%d] %s", i+1, t)
		if i == m.tab {
			parts = append(parts, titleStyle.Render(label))
		} else {
			parts = append(parts, dimStyle.Render(label))
		}
	}
	return strings.Join(parts, "  ")
}

func (m model_) renderControls() string {
	return strings.Join([]string{
		i18n.T("dash.controls.quit"),
		i18n.T("dash.controls.switch"),
		i18n.T("dash.controls.jump"),
		i18n.T("dash.controls.locale"),
	}, "   ")
}

func (m model_) toolSummary(s *model.Session, n int) string {
	names := reader.SortedToolNames(s.ToolCalls)
	if len(names) == 0 {
		return ""
	}
	if n > len(names) {
		n = len(names)
	}
	var parts []string
	for i := 0; i < n; i++ {
		parts = append(parts, fmt.Sprintf("%s%d", names[i], s.ToolCalls[names[i]]))
	}
	return strings.Join(parts, " ")
}

func (m model_) ttftSparkline(s *model.Session, w int) string {
	var pts []float64
	for _, msg := range s.Messages {
		if msg.Role == "assistant" && msg.Metrics != nil && msg.Metrics.TTFTMs > 0 {
			pts = append(pts, msg.Metrics.TTFTMs)
		}
	}
	if len(pts) == 0 {
		return ""
	}
	return sparklineFloats(pts, w)
}

func (m model_) ttftP50(s *model.Session) float64 {
	var pts []float64
	for _, msg := range s.Messages {
		if msg.Role == "assistant" && msg.Metrics != nil && msg.Metrics.TTFTMs > 0 {
			pts = append(pts, msg.Metrics.TTFTMs)
		}
	}
	return model.Percentile(pts, 50)
}

func (m model_) ttftP95(s *model.Session) float64 {
	var pts []float64
	for _, msg := range s.Messages {
		if msg.Role == "assistant" && msg.Metrics != nil && msg.Metrics.TTFTMs > 0 {
			pts = append(pts, msg.Metrics.TTFTMs)
		}
	}
	return model.Percentile(pts, 95)
}

// ---- Helpers ----

func (m model_) panel(title, content string, width int) string {
	// width = total display width including border.
	if width < 10 {
		width = 10
	}
	// innerW = width - 2(borders) - 2(padding)
	innerW := width - 4
	if innerW < 2 {
		innerW = 2
	}
	// Title on top, content below — matches ocmonitor's panel layout.
	t := titleStyle.Render(title)
	// Truncate content lines to inner width.
	var lines []string
	for _, line := range strings.Split(content, "\n") {
		lines = append(lines, ui.Truncate(line, innerW))
	}
	// Build panel with manual border coloring (lipgloss strips ANSI from
	// pure-symbol strings when output is not a TTY).
	bc := "\x1b[38;5;238m"
	rst := "\x1b[0m"
	horiz := strings.Repeat("─", innerW+2)
	top := bc + "╭" + horiz + "╮" + rst
	bot := bc + "╰" + horiz + "╯" + rst
	v := bc + "│" + rst
	var b strings.Builder
	b.WriteString(top)
	b.WriteString("\n")
	// Title line.
	b.WriteString(v)
	b.WriteString(" " + ui.Truncate(t, innerW))
	// Pad title line to inner width.
	titleDisplayW := lipgloss.Width(" " + t)
	if titleDisplayW < innerW+1 {
		b.WriteString(strings.Repeat(" ", innerW+1-titleDisplayW))
	}
	b.WriteString(" " + v)
	b.WriteString("\n")
	// Body lines.
	for _, line := range lines {
		b.WriteString(v)
		cell := " " + line
		cellW := lipgloss.Width(cell)
		if cellW < innerW+1 {
			cell += strings.Repeat(" ", innerW+1-cellW)
		}
		b.WriteString(cell + " " + v)
		b.WriteString("\n")
	}
	b.WriteString(bot)
	return b.String()
}

func progressBar(pct, width float64) string {
	if width < 4 {
		width = 4
	}
	filled := int(width * pct / 100)
	if filled > int(width) {
		filled = int(width)
	}
	bar := strings.Repeat("█", filled) + strings.Repeat("░", int(width)-filled)
	return fmt.Sprintf("%s %.0f%%", bar, pct)
}

func pctColor(pct float64) lipgloss.Style {
	switch {
	case pct >= 90:
		return redStyle
	case pct >= 70:
		return yellowStyle
	default:
		return greenStyle
	}
}

// sparklineFloats renders a unicode sparkline of float values scaled to width.
func sparklineFloats(pts []float64, width int) string {
	if len(pts) == 0 || width < 4 {
		return ""
	}
	// Downsample to at most width points.
	sampled := downsample(pts, width)
	if len(sampled) > width {
		sampled = sampled[:width]
	}
	min, max := minMax(sampled)
	rng := max - min
	if rng == 0 {
		rng = 1
	}
	blocks := []rune("▁▂▃▄▅▆▇█")
	var b strings.Builder
	for _, v := range sampled {
		idx := int((v - min) / rng * float64(len(blocks)-1))
		if idx < 0 {
			idx = 0
		}
		if idx >= len(blocks) {
			idx = len(blocks) - 1
		}
		b.WriteRune(blocks[idx])
	}
	return b.String()
}

func sparklineInts(pts []int, width, ceiling int) string {
	fs := make([]float64, len(pts))
	for i, v := range pts {
		fs[i] = float64(v)
	}
	return sparklineFloats(fs, width)
}

func downsample(pts []float64, width int) []float64 {
	if len(pts) <= width {
		return pts
	}
	out := make([]float64, 0, width)
	step := float64(len(pts)) / float64(width)
	for i := 0.0; i < float64(len(pts)); i += step {
		idx := int(i)
		if idx >= len(pts) {
			idx = len(pts) - 1
		}
		out = append(out, pts[idx])
	}
	return out
}

func minMax(xs []float64) (float64, float64) {
	if len(xs) == 0 {
		return 0, 0
	}
	min, max := xs[0], xs[0]
	for _, v := range xs[1:] {
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	}
	return min, max
}

func maxInt(xs []int) int {
	m := 0
	for _, v := range xs {
		if v > m {
			m = v
		}
	}
	return m
}
