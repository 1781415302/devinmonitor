package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/spf13/cobra"

	"github.com/garywhat/devinmonitor/internal/i18n"
	"github.com/garywhat/devinmonitor/internal/model"
	"github.com/garywhat/devinmonitor/internal/reader"
	"github.com/garywhat/devinmonitor/internal/report"
)

// ---- Web Dashboard ----

var cmdWeb = func() *cobra.Command {
	var (
		port int
		open bool
	)
	c := &cobra.Command{
		Use:   "web",
		Short: i18n.T("cmd.web"),
		Run: func(cmd *cobra.Command, args []string) {
			runWebServer(cmd, port, open)
		},
	}
	c.Flags().IntVar(&port, "port", 19191, "port to listen on (default 19191; avoid common 8080/3000)")
	c.Flags().BoolVar(&open, "open", false, "open the dashboard in the default browser")
	return c
}

// webState holds SSE subscriber channels and a long-lived multi-source reader.
type webState struct {
	mu          sync.Mutex
	subscribers map[chan string]bool
	r           reader.Reader
	dataDir     string
}

func runWebServer(cmd *cobra.Command, port int, openBrowser bool) {
	dataDir := dataDirFrom(cmd)
	r, err := reader.Open(dataDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	st := &webState{
		subscribers: make(map[chan string]bool),
		r:           r,
		dataDir:     dataDir,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", st.handleIndex)
	mux.HandleFunc("/api/sessions", st.handleSessions)
	mux.HandleFunc("/api/cost-summary", st.handleCostSummary)
	mux.HandleFunc("/api/sources", st.handleSources)
	mux.HandleFunc("/api/models", st.handleModels)
	mux.HandleFunc("/api/alerts", st.handleAlerts)
	mux.HandleFunc("/sse", st.handleSSE)

	go st.pollLoop()

	url := fmt.Sprintf("http://localhost:%d", port)
	fmt.Fprintf(os.Stderr, "DevinMonitor web dashboard: %s\n", url)
	if mr, ok := r.(*reader.MultiReader); ok {
		fmt.Fprintf(os.Stderr, "Sources: %s\n", strings.Join(mr.Sources(), ", "))
	}
	fmt.Fprintf(os.Stderr, "Press Ctrl+C to stop.\n")
	if openBrowser {
		openURL(url)
	}

	srv := &http.Server{Addr: fmt.Sprintf(":%d", port), Handler: mux}

	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt)
		<-sigCh
		fmt.Fprintf(os.Stderr, "\nShutting down web server...\n")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = r.Close()
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintf(os.Stderr, "web server error: %v\n", err)
		_ = r.Close()
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "Web server stopped.\n")
}

// loadSessions refreshes secondary sources (WSL snapshots) then reads all sessions.
func (st *webState) loadSessions() ([]model.Session, error) {
	if ref, ok := st.r.(reader.Refresher); ok {
		_ = ref.Refresh()
	}
	return st.r.Sessions()
}

func (st *webState) pollLoop() {
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		data := st.snapshotJSON()
		st.mu.Lock()
		for ch := range st.subscribers {
			select {
			case ch <- data:
			default:
			}
		}
		st.mu.Unlock()
	}
}

type usageTotals struct {
	Requests   int   `json:"requests"`
	InputTok   int64 `json:"inputTokens"`
	OutputTok  int64 `json:"outputTokens"`
	CacheRead  int64 `json:"cacheRead"`
	CacheWrite int64 `json:"cacheWrite"`
	TotalTok   int64 `json:"totalTokens"`
}

type sourceInfo struct {
	Label    string `json:"label"`
	Path     string `json:"path"`
	Schema   int    `json:"schema"`
	Sessions int    `json:"sessions"`
}

type webSnapshot struct {
	Summary  costSummary          `json:"summary"`
	Usage    usageTotals          `json:"usage"`
	Sessions []report.SessionRow  `json:"sessions"`
	Models   []modelRow           `json:"models"`
	Alerts   []model.AlertItem    `json:"alerts"`
	Sources  []sourceInfo         `json:"sources"`
	Updated  string               `json:"updated"`
}

type modelRow struct {
	Name       string  `json:"name"`
	Sessions   int     `json:"sessions"`
	Requests   int     `json:"requests"`
	InputTok   int64   `json:"inputTokens"`
	OutputTok  int64   `json:"outputTokens"`
	CacheRead  int64   `json:"cacheRead"`
	TotalTok   int64   `json:"totalTokens"`
	Cost       float64 `json:"cost"`
	IsFree     bool    `json:"isFree"`
	TokPerSec  float64 `json:"tokPerSec"`
}

func buildModelRows(ss []model.Session) []modelRow {
	rows := report.BuildModelRows(ss)
	out := make([]modelRow, 0, len(rows))
	for _, r := range rows {
		cost := r.CreditCost + r.ACUCost
		if cost == 0 {
			cost = r.EstCost
		}
		out = append(out, modelRow{
			Name:      r.Name,
			Sessions:  r.Sessions,
			Requests:  r.Requests,
			InputTok:  r.InputTok,
			OutputTok: r.OutputTok,
			CacheRead: r.CacheRead,
			TotalTok:  r.InputTok + r.OutputTok + r.CacheRead + r.CacheWrite,
			Cost:      cost,
			IsFree:    r.IsFree,
			TokPerSec: r.TokPerSecP50,
		})
	}
	return out
}

func openURL(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
}

func (st *webState) buildSnapshot() webSnapshot {
	ss, err := st.loadSessions()
	if err != nil {
		return webSnapshot{Summary: costSummary{}, Updated: time.Now().Format(time.RFC3339)}
	}
	sum := computeCostSummary(ss)
	rows := report.BuildSessionRows(ss)
	var u usageTotals
	for _, s := range ss {
		u.Requests += s.AssistantCount
		u.InputTok += s.InputTokens
		u.OutputTok += s.OutputTokens
		u.CacheRead += s.CacheRead
		u.CacheWrite += s.CacheWrite
	}
	u.TotalTok = u.InputTok + u.OutputTok + u.CacheRead + u.CacheWrite

	var sources []sourceInfo
	if mr, ok := st.r.(*reader.MultiReader); ok {
		for _, s := range mr.SourcesSummary() {
			sources = append(sources, sourceInfo{s.Label, s.Path, s.Schema, s.Sessions})
		}
	} else {
		n := len(ss)
		sources = append(sources, sourceInfo{"local", st.r.DBPath(), st.r.SchemaVersion(), n})
	}
	return webSnapshot{
		Summary:  sum,
		Usage:    u,
		Sessions: rows,
		Models:   buildModelRows(ss),
		Alerts:   detectAlerts(ss),
		Sources:  sources,
		Updated:  time.Now().Format(time.RFC3339),
	}
}

func (st *webState) snapshotJSON() string {
	snap := st.buildSnapshot()
	data, _ := json.Marshal(snap)
	return string(data)
}

func (st *webState) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, webDashboardHTML)
}

func (st *webState) handleSessions(w http.ResponseWriter, r *http.Request) {
	ss, err := st.loadSessions()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	rows := report.BuildSessionRows(ss)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(rows)
}

func (st *webState) handleCostSummary(w http.ResponseWriter, r *http.Request) {
	ss, err := st.loadSessions()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	sum := computeCostSummary(ss)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(sum)
}

func (st *webState) handleSources(w http.ResponseWriter, r *http.Request) {
	var sources []sourceInfo
	if mr, ok := st.r.(*reader.MultiReader); ok {
		for _, s := range mr.SourcesSummary() {
			sources = append(sources, sourceInfo{s.Label, s.Path, s.Schema, s.Sessions})
		}
	} else {
		sources = append(sources, sourceInfo{"local", st.r.DBPath(), st.r.SchemaVersion(), 0})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(sources)
}

func (st *webState) handleModels(w http.ResponseWriter, r *http.Request) {
	ss, err := st.loadSessions()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(buildModelRows(ss))
}

func (st *webState) handleAlerts(w http.ResponseWriter, r *http.Request) {
	ss, err := st.loadSessions()
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	alerts := detectAlerts(ss)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(alerts)
}

func (st *webState) handleSSE(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := make(chan string, 1)
	st.mu.Lock()
	st.subscribers[ch] = true
	st.mu.Unlock()
	defer func() {
		st.mu.Lock()
		delete(st.subscribers, ch)
		st.mu.Unlock()
	}()

	fmt.Fprintf(w, "data: %s\n\n", st.snapshotJSON())
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	for {
		select {
		case data := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", data)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		case <-r.Context().Done():
			return
		}
	}
}

const webDashboardHTML = `<!DOCTYPE html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Token Ledger · Devin / WSL / MiMo</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link href="https://fonts.googleapis.com/css2?family=IBM+Plex+Sans:wght@300;400;500;600;700&family=JetBrains+Mono:wght@400;500;600&display=swap" rel="stylesheet">
<style>
/* Minimalism / Swiss · light · from-scratch (not glassmorphism) */
:root {
  --ink: #111827;
  --ink-2: #374151;
  --ink-3: #6B7280;
  --paper: #F7F6F3;
  --surface: #FFFFFF;
  --line: #E5E2DA;
  --line-strong: #CFC9BC;
  --accent: #0B6E4F;
  --accent-soft: #E6F2EC;
  --warn: #B45309;
  --danger: #B42318;
  --src-local: #1D4ED8;
  --src-wsl: #7C3AED;
  --src-mimo: #0F766E;
  --src-opencode: #B45309;
  --r: 2px;
  --font: "IBM Plex Sans", "PingFang SC", "Microsoft YaHei", system-ui, sans-serif;
  --mono: "JetBrains Mono", ui-monospace, Consolas, monospace;
  --max: 1180px;
}
* { box-sizing: border-box; }
html, body { margin: 0; padding: 0; }
body {
  background: var(--paper);
  color: var(--ink);
  font-family: var(--font);
  font-size: 14px;
  line-height: 1.45;
  -webkit-font-smoothing: antialiased;
}
a { color: var(--accent); }
.wrap { max-width: var(--max); margin: 0 auto; padding: 0 28px 56px; }

/* Top bar — flat, Swiss rule lines */
.topbar {
  border-bottom: 1px solid var(--ink);
  padding: 22px 0 0;
  margin-bottom: 28px;
}
.topbar-row {
  display: flex;
  flex-wrap: wrap;
  align-items: flex-end;
  gap: 16px 28px;
  padding-bottom: 16px;
}
.wordmark {
  font-size: 1.65rem;
  font-weight: 600;
  letter-spacing: -0.03em;
  line-height: 1.1;
  margin: 0;
}
.wordmark em {
  font-style: normal;
  color: var(--accent);
  font-weight: 700;
}
.tagline {
  margin: 6px 0 0;
  color: var(--ink-3);
  font-size: .85rem;
  font-weight: 400;
}
.meta-col {
  margin-left: auto;
  text-align: right;
  font-family: var(--mono);
  font-size: .72rem;
  color: var(--ink-3);
  line-height: 1.6;
}
.meta-col .live {
  color: var(--accent);
  font-weight: 600;
  letter-spacing: .04em;
}
.meta-col .live::before {
  content: "";
  display: inline-block;
  width: 6px; height: 6px;
  border-radius: 50%;
  background: var(--accent);
  margin-right: 6px;
  vertical-align: middle;
  animation: blink 1.6s step-end infinite;
}
@keyframes blink { 50% { opacity: .2; } }

/* Source strip under title */
.source-strip {
  display: flex;
  flex-wrap: wrap;
  gap: 8px 10px;
  padding: 12px 0;
  border-top: 1px solid var(--line);
}
.chip {
  display: inline-flex;
  align-items: baseline;
  gap: 8px;
  padding: 6px 10px;
  border: 1px solid var(--line-strong);
  background: var(--surface);
  border-radius: var(--r);
  font-size: .78rem;
}
.chip b { font-weight: 600; font-family: var(--mono); font-size: .72rem; }
.chip .n { color: var(--ink-3); font-family: var(--mono); font-size: .72rem; }
.chip.local { border-left: 3px solid var(--src-local); }
.chip.wsl { border-left: 3px solid var(--src-wsl); }
.chip.mimo { border-left: 3px solid var(--src-mimo); }
.chip.opencode { border-left: 3px solid var(--src-opencode); }
.chip .path {
  color: var(--ink-3);
  font-family: var(--mono);
  font-size: .65rem;
  max-width: 280px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

/* Metrics — printed ledger strip, not glass cards */
.metrics {
  display: grid;
  grid-template-columns: repeat(4, 1fr);
  border: 1px solid var(--ink);
  background: var(--surface);
  margin-bottom: 28px;
}
.metric {
  padding: 18px 16px 16px;
  border-right: 1px solid var(--line);
  border-bottom: 1px solid var(--line);
}
.metrics .metric:nth-child(4n) { border-right: none; }
.metrics .metric:nth-last-child(-n+4) { border-bottom: none; }
.metric .k {
  font-size: .68rem;
  text-transform: uppercase;
  letter-spacing: .1em;
  color: var(--ink-3);
  font-weight: 500;
  margin-bottom: 10px;
}
.metric .v {
  font-family: var(--mono);
  font-size: 1.55rem;
  font-weight: 500;
  letter-spacing: -0.04em;
  line-height: 1;
}
.metric .v.accent { color: var(--accent); }
.metric .v.muted { color: var(--ink-2); }

/* Section titles with hairline */
.sec {
  margin: 0 0 14px;
  display: flex;
  align-items: center;
  gap: 12px;
}
.sec h2 {
  margin: 0;
  font-size: .72rem;
  font-weight: 600;
  letter-spacing: .12em;
  text-transform: uppercase;
  color: var(--ink);
  white-space: nowrap;
}
.sec::after {
  content: "";
  flex: 1;
  height: 1px;
  background: var(--line-strong);
}
.sec-tools { margin-left: auto; display: flex; gap: 6px; }
.sec-tools button {
  font: inherit;
  font-size: .72rem;
  padding: 4px 10px;
  border: 1px solid var(--line-strong);
  background: var(--surface);
  color: var(--ink-2);
  border-radius: var(--r);
  cursor: pointer;
  transition: background .15s ease, color .15s ease, border-color .15s ease;
}
.sec-tools button:hover { border-color: var(--ink); color: var(--ink); }
.sec-tools button:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
.sec-tools button.on {
  background: var(--ink);
  border-color: var(--ink);
  color: #fff;
}
.filter-bar {
  border: 1px solid var(--ink);
  background: var(--surface);
  padding: 10px 12px;
  margin-bottom: 10px;
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.filter-row {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px 10px;
}
.filter-label {
  font-size: .68rem;
  text-transform: uppercase;
  letter-spacing: .08em;
  color: var(--ink-3);
  font-weight: 600;
  min-width: 48px;
}
.filter-count {
  font-family: var(--mono);
  font-size: .72rem;
  color: var(--ink-3);
}

.block { margin-bottom: 32px; }

/* Token composition — horizontal stacked bar + legend (new layout) */
.stack {
  border: 1px solid var(--ink);
  background: var(--surface);
  padding: 18px 16px;
}
.stack-bar {
  display: flex;
  height: 28px;
  border: 1px solid var(--line-strong);
  overflow: hidden;
  margin-bottom: 14px;
}
.stack-bar span {
  display: block;
  height: 100%;
  transition: width .3s ease;
}
.stack-bar .in { background: #111827; }
.stack-bar .out { background: var(--accent); }
.stack-bar .cache { background: #C4B5A0; }
.legend {
  display: flex;
  flex-wrap: wrap;
  gap: 14px 22px;
  font-size: .8rem;
}
.legend i {
  display: inline-block;
  width: 10px; height: 10px;
  margin-right: 6px;
  vertical-align: -1px;
  border: 1px solid var(--ink);
}
.legend .in i { background: #111827; }
.legend .out i { background: var(--accent); }
.legend .cache i { background: #C4B5A0; }
.legend .val {
  font-family: var(--mono);
  color: var(--ink-3);
  margin-left: 4px;
}

/* Tables — Swiss data tables */
.panel {
  border: 1px solid var(--ink);
  background: var(--surface);
}
.panel-scroll { overflow-x: auto; }
table { width: 100%; border-collapse: collapse; min-width: 720px; }
thead th {
  text-align: left;
  font-size: .68rem;
  font-weight: 600;
  letter-spacing: .08em;
  text-transform: uppercase;
  color: var(--ink-3);
  padding: 10px 12px;
  border-bottom: 1px solid var(--ink);
  background: var(--paper);
  white-space: nowrap;
}
tbody td {
  padding: 10px 12px;
  border-bottom: 1px solid var(--line);
  vertical-align: middle;
}
tbody tr:last-child td { border-bottom: none; }
tbody tr:hover { background: #FBFAF7; }
.num { text-align: right; font-family: var(--mono); font-size: .78rem; font-variant-numeric: tabular-nums; }
.mono { font-family: var(--mono); font-size: .75rem; }
.trunc {
  max-width: 240px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.tag {
  display: inline-block;
  font-family: var(--mono);
  font-size: .68rem;
  font-weight: 500;
  padding: 2px 6px;
  border: 1px solid var(--line-strong);
  border-radius: var(--r);
  background: var(--paper);
}
.tag.local { color: var(--src-local); border-color: #BFDBFE; background: #EFF6FF; }
.tag.wsl { color: var(--src-wsl); border-color: #DDD6FE; background: #F5F3FF; }
.tag.mimo { color: var(--src-mimo); border-color: #99F6E4; background: #F0FDFA; }
.tag.opencode { color: var(--src-opencode); border-color: #FDE68A; background: #FFFBEB; }
.share {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 100px;
}
.share-track {
  flex: 1;
  height: 4px;
  background: var(--line);
  position: relative;
}
.share-fill {
  position: absolute;
  left: 0; top: 0; bottom: 0;
  background: var(--ink);
}
.share-fill.is-accent { background: var(--accent); }
.share-pct {
  font-family: var(--mono);
  font-size: .68rem;
  color: var(--ink-3);
  width: 36px;
  text-align: right;
}

.alerts {
  border: 1px solid var(--ink);
  background: var(--surface);
  padding: 12px 16px;
}
.alerts .empty, .empty {
  color: var(--ink-3);
  font-size: .85rem;
}
.alerts li {
  color: var(--danger);
  margin: 0 0 6px;
  padding: 0;
  list-style: none;
  font-size: .85rem;
}
.alerts ul { margin: 0; padding: 0; }

footer.foot {
  margin-top: 8px;
  padding-top: 14px;
  border-top: 1px solid var(--line);
  font-size: .72rem;
  color: var(--ink-3);
  font-family: var(--mono);
}

@media (max-width: 900px) {
  .metrics { grid-template-columns: repeat(2, 1fr); }
  .metrics .metric:nth-child(4n) { border-right: 1px solid var(--line); }
  .metrics .metric:nth-child(2n) { border-right: none; }
  .metrics .metric:nth-last-child(-n+4) { border-bottom: 1px solid var(--line); }
  .metrics .metric:nth-last-child(-n+2) { border-bottom: none; }
  .meta-col { margin-left: 0; text-align: left; width: 100%; }
}
@media (max-width: 520px) {
  .wrap { padding: 0 14px 40px; }
  .metrics { grid-template-columns: 1fr 1fr; }
  .wordmark { font-size: 1.3rem; }
}
@media (prefers-reduced-motion: reduce) {
  .meta-col .live::before { animation: none; }
  .stack-bar span { transition: none; }
  tbody tr { transition: none; }
}
</style>
</head>
<body>
<div class="wrap">
  <header class="topbar">
    <div class="topbar-row">
      <div>
        <h1 class="wordmark">Token <em>Ledger</em></h1>
        <p class="tagline">Devin · WSL · MiMo  —  用量账本</p>
      </div>
      <div class="meta-col">
        <div class="live" id="liveFlag">LIVE</div>
        <div id="updated">连接中…</div>
        <div>SSE · 5s</div>
      </div>
    </div>
    <div class="source-strip" id="sources"><span class="empty">加载数据源…</span></div>
  </header>

  <div class="filter-bar" id="sessionFilters">
    <div class="filter-row">
      <span class="filter-label">服务商</span>
      <div class="sec-tools" id="providerFilter" role="group" aria-label="按服务商筛选"></div>
    </div>
    <div class="filter-row">
      <span class="filter-label">实例</span>
      <div class="sec-tools" id="sourceFilter" role="group" aria-label="按实例筛选"></div>
    </div>
    <div class="filter-row filter-count" id="filterCount"></div>
  </div>

  <div class="metrics" id="kpis" aria-live="polite"></div>

  <section class="block">
    <div class="sec"><h2>Token 构成</h2></div>
    <div class="stack">
      <div class="stack-bar" id="stackBar" role="img" aria-label="Token 构成比例"></div>
      <div class="legend" id="stackLegend"></div>
    </div>
  </section>

  <section class="block">
    <div class="sec"><h2>模型</h2></div>
    <div class="panel panel-scroll">
      <table id="models">
        <thead><tr>
          <th>模型</th>
          <th class="num">会话</th>
          <th class="num">请求</th>
          <th class="num">输入</th>
          <th class="num">输出</th>
          <th class="num">缓存</th>
          <th class="num">合计</th>
          <th class="num">t/s</th>
          <th class="num">成本</th>
          <th>占比</th>
        </tr></thead>
        <tbody></tbody>
      </table>
    </div>
  </section>

  <section class="block">
    <div class="sec">
      <h2>会话</h2>
    </div>
    <div class="panel panel-scroll">
      <table id="sessions">
        <thead><tr>
          <th>ID</th>
          <th>服务</th>
          <th>实例</th>
          <th>标题</th>
          <th>模型</th>
          <th>项目</th>
          <th class="num">请求</th>
          <th class="num">输入</th>
          <th class="num">输出</th>
          <th class="num">缓存</th>
          <th class="num">时长</th>
          <th class="num">成本</th>
        </tr></thead>
        <tbody></tbody>
      </table>
    </div>
  </section>

  <section class="block">
    <div class="sec"><h2>告警</h2></div>
    <div class="alerts" id="alerts"><span class="empty">无</span></div>
  </section>

  <footer class="foot">devinmonitor web · 只读快照 · 不写回 Devin / MiMo</footer>
</div>

<script>
const es = new EventSource('/sse');
es.onmessage = function (e) {
  let d;
  try { d = JSON.parse(e.data); } catch (_) { return; }
  if (d.error) {
    document.getElementById('updated').textContent = '错误 · ' + d.error;
    return;
  }
  render(d);
};
es.onerror = function () {
  document.getElementById('updated').textContent = '连接中断 · 重试中';
};

var activeProvider = 'all';
var activeSource = 'all';
var lastSessions = [];
var lastData = null;

function fmtTok(n) {
  n = n || 0;
  if (n >= 1e9) return (n / 1e9).toFixed(2) + 'B';
  if (n >= 1e6) return (n / 1e6).toFixed(1) + 'M';
  if (n >= 1e3) return (n / 1e3).toFixed(1) + 'k';
  return String(n);
}
function fmtDur(sec) {
  sec = sec || 0;
  if (sec < 60) return Math.round(sec) + 's';
  if (sec < 3600) return Math.round(sec / 60) + 'm';
  if (sec < 86400) return (sec / 3600).toFixed(1) + 'h';
  return (sec / 86400).toFixed(1) + 'd';
}
function fmtCost(s) {
  if (s.IsFree || s.Cost === 0) return 'free';
  return '$' + (s.Cost || 0).toFixed(2);
}
function esc(s) {
  var d = document.createElement('div');
  d.textContent = s == null ? '' : String(s);
  return d.innerHTML;
}
// Provider: Devin / MiMo / OpenCode
function providerOf(src) {
  src = src || 'local';
  if (src === 'mimo' || src.indexOf('mimo') >= 0) return 'mimo';
  if (src === 'opencode' || src.indexOf('opencode') >= 0) return 'opencode';
  return 'devin';
}
function providerLabel(p) {
  if (p === 'mimo') return 'MiMo';
  if (p === 'opencode') return 'OpenCode';
  if (p === 'devin') return 'Devin';
  return p;
}
// Instance: local / wsl:Ubuntu-18.04 / wsl-opencode:Ubuntu-20.04 ...
function instanceOf(src) {
  return src || 'local';
}
function instanceLabel(src) {
  if (!src || src === 'local') return 'Windows 本机';
  if (src === 'mimo') return 'Windows 本机';
  if (src === 'opencode') return 'Windows 本机';
  return src; // e.g. wsl:Ubuntu-18.04
}
function srcClass(src) {
  var p = providerOf(src);
  if (p === 'mimo') return 'mimo';
  if (p === 'opencode') return 'opencode';
  if (src && src.indexOf('wsl') === 0) return 'wsl';
  return 'local';
}
function srcLabel(src) {
  return instanceLabel(src);
}

function renderSources(sources) {
  var box = document.getElementById('sources');
  if (!sources || !sources.length) {
    box.innerHTML = '<span class="empty">无数据源</span>';
    return;
  }
  box.innerHTML = sources.map(function (x) {
    var c = srcClass(x.label);
    var p = providerOf(x.label);
    return '<span class="chip ' + c + '"><b>' + esc(providerLabel(p)) + '</b>' +
      '<span class="n">' + esc(instanceLabel(x.label)) + (x.label && x.label !== 'local' && x.label !== 'mimo' && x.label !== 'opencode' ? '' : '') + '</span>' +
      '<span class="n">' + x.sessions + ' 会话</span>' +
      '<span class="n">v' + x.schema + '</span>' +
      '<span class="path" title="' + esc(x.path) + '">' + esc(x.path) + '</span></span>';
  }).join('');
}

function matchesFilters(x) {
  var src = instanceOf(x.Source);
  if (activeProvider !== 'all' && providerOf(src) !== activeProvider) return false;
  if (activeSource !== 'all' && src !== activeSource) return false;
  return true;
}

function renderFilters(sessions) {
  var providers = {};
  var instances = {};
  sessions.forEach(function (x) {
    var src = instanceOf(x.Source);
    providers[providerOf(src)] = true;
    if (activeProvider === 'all' || providerOf(src) === activeProvider) {
      instances[src] = true;
    }
  });

  var pbox = document.getElementById('providerFilter');
  var pkeys = ['devin', 'mimo', 'opencode'].filter(function (k) { return providers[k]; });
  var popts = ['all'].concat(pkeys);
  pbox.innerHTML = popts.map(function (k) {
    var on = activeProvider === k ? ' on' : '';
    var label = k === 'all' ? '全部' : providerLabel(k);
    return '<button type="button" class="' + on.trim() + '" data-p="' + esc(k) + '" aria-pressed="' + (activeProvider === k) + '">' + esc(label) + '</button>';
  }).join('');
  Array.prototype.forEach.call(pbox.querySelectorAll('button'), function (btn) {
    btn.onclick = function () {
      activeProvider = btn.getAttribute('data-p');
      activeSource = 'all';
      renderFilters(lastSessions);
      renderSessions(lastSessions);
    };
  });

  var sbox = document.getElementById('sourceFilter');
  var skeys = Object.keys(instances).sort();
  // Friendly order: local mimo opencode then wsl:*
  skeys.sort(function (a, b) {
    var rank = function (s) {
      if (s === 'local') return 0;
      if (s === 'mimo') return 1;
      if (s === 'opencode') return 2;
      return 10;
    };
    return rank(a) - rank(b) || a.localeCompare(b);
  });
  var sopts = ['all'].concat(skeys);
  sbox.innerHTML = sopts.map(function (k) {
    var on = activeSource === k ? ' on' : '';
    var label = k === 'all' ? '全部' : instanceLabel(k);
    // count for this instance under current provider
    var n = sessions.filter(function (x) {
      return instanceOf(x.Source) === k &&
        (activeProvider === 'all' || providerOf(instanceOf(x.Source)) === activeProvider);
    }).length;
    if (k !== 'all') label = label + ' (' + n + ')';
    return '<button type="button" class="' + on.trim() + '" data-s="' + esc(k) + '" aria-pressed="' + (activeSource === k) + '">' + esc(label) + '</button>';
  }).join('');
  Array.prototype.forEach.call(sbox.querySelectorAll('button'), function (btn) {
    btn.onclick = function () {
      activeSource = btn.getAttribute('data-s');
      renderFilters(lastSessions);
      renderSessions(lastSessions);
    };
  });

  var n = sessions.filter(matchesFilters).length;
  document.getElementById('filterCount').textContent =
    '显示 ' + n + ' / ' + sessions.length + ' 个会话';
}

function render(d) {
  lastData = d;
  document.getElementById('updated').textContent =
    new Date(d.updated || Date.now()).toLocaleTimeString();
  lastSessions = d.sessions || [];
  renderFilters(lastSessions);
  renderAll();
}

function renderAll() {
  if (!lastData) return;
  var d = lastData;
  var list = lastSessions.filter(matchesFilters);

  // KPIs from filtered sessions
  var req = 0, inn = 0, out = 0, cache = 0, cost = 0;
  list.forEach(function (s) {
    req += s.Requests || 0;
    inn += s.InputTok || 0;
    out += s.OutputTok || 0;
    cache += s.CacheRead || 0;
    if (!s.IsFree) cost += s.Cost || 0;
  });
  var totalTok = inn + out + cache;
  var nSrc = {};
  list.forEach(function (s) { nSrc[instanceOf(s.Source)] = true; });
  var nSrcCount = Object.keys(nSrc).length;
  var kpis = [
    ['会话', list.length, ''],
    ['请求', req, ''],
    ['输入', fmtTok(inn), ''],
    ['输出', fmtTok(out), ''],
    ['缓存读', fmtTok(cache), 'muted'],
    ['总 Token', fmtTok(totalTok), 'accent'],
    ['成本', cost > 0 ? '$' + cost.toFixed(2) : 'free', 'accent'],
    ['数据源', nSrcCount, 'muted']
  ];
  var kg = document.getElementById('kpis');
  kg.innerHTML = '';
  kpis.forEach(function (item) {
    var el = document.createElement('div');
    el.className = 'metric';
    el.innerHTML = '<div class="k">' + item[0] + '</div><div class="v ' + (item[2] || '') + '">' + item[1] + '</div>';
    kg.appendChild(el);
  });

  renderSources(d.sources || []);

  // Token stack from filtered sessions
  var sum = inn + out + cache || 1;
  document.getElementById('stackBar').innerHTML =
    '<span class="in" style="width:' + (inn / sum * 100) + '%"></span>' +
    '<span class="out" style="width:' + (out / sum * 100) + '%"></span>' +
    '<span class="cache" style="width:' + (cache / sum * 100) + '%"></span>';
  document.getElementById('stackLegend').innerHTML =
    '<span class="in"><i></i>输入<span class="val">' + fmtTok(inn) + '</span></span>' +
    '<span class="out"><i></i>输出<span class="val">' + fmtTok(out) + '</span></span>' +
    '<span class="cache"><i></i>缓存读<span class="val">' + fmtTok(cache) + '</span></span>';

  // Models re-aggregated from filtered sessions (keep API t/s when name matches)
  var apiModels = {};
  (d.models || []).forEach(function (m) { apiModels[m.name] = m; });
  var agg = {};
  list.forEach(function (s) {
    var name = s.Model || 'unknown';
    var a = agg[name];
    if (!a) {
      a = agg[name] = {
        name: name, sessions: 0, requests: 0,
        inputTokens: 0, outputTokens: 0, cacheRead: 0, totalTokens: 0,
        cost: 0, isFree: true, tokPerSec: 0
      };
    }
    a.sessions++;
    a.requests += s.Requests || 0;
    a.inputTokens += s.InputTok || 0;
    a.outputTokens += s.OutputTok || 0;
    a.cacheRead += s.CacheRead || 0;
    a.totalTokens = a.inputTokens + a.outputTokens + a.cacheRead;
    if (s.IsFree === false && s.Cost) {
      a.cost += s.Cost;
      a.isFree = false;
    }
    if (apiModels[name]) a.tokPerSec = apiModels[name].tokPerSec || 0;
  });
  var models = Object.keys(agg).map(function (k) { return agg[k]; })
    .sort(function (a, b) { return b.totalTokens - a.totalTokens; });
  var mMax = Math.max.apply(null, models.map(function (m) { return m.totalTokens || 0; }).concat([1]));
  var mtb = document.querySelector('#models tbody');
  if (!models.length) {
    mtb.innerHTML = '<tr><td colspan="10" class="empty">无数据</td></tr>';
  } else {
    mtb.innerHTML = models.map(function (m) {
      var pct = Math.round(((m.totalTokens || 0) / mMax) * 100);
      var costStr = m.isFree || !m.cost ? 'free' : '$' + (m.cost || 0).toFixed(2);
      var speed = m.tokPerSec > 0 ? Math.round(m.tokPerSec) : '—';
      return '<tr>' +
        '<td class="mono">' + esc(m.name) + '</td>' +
        '<td class="num">' + (m.sessions || 0) + '</td>' +
        '<td class="num">' + (m.requests || 0) + '</td>' +
        '<td class="num">' + fmtTok(m.inputTokens) + '</td>' +
        '<td class="num">' + fmtTok(m.outputTokens) + '</td>' +
        '<td class="num">' + fmtTok(m.cacheRead) + '</td>' +
        '<td class="num">' + fmtTok(m.totalTokens) + '</td>' +
        '<td class="num">' + speed + '</td>' +
        '<td class="num">' + costStr + '</td>' +
        '<td><div class="share"><div class="share-track"><div class="share-fill' + (pct > 60 ? ' is-accent' : '') + '" style="width:' + pct + '%"></div></div><span class="share-pct">' + pct + '%</span></div></td>' +
        '</tr>';
    }).join('');
  }

  renderSessions(list);
  renderAlerts(list);
}

function renderSessions(filtered) {
  var tb = document.querySelector('#sessions tbody');
  if (!filtered.length) {
    tb.innerHTML = '<tr><td colspan="12" class="empty">无会话</td></tr>';
    return;
  }
  tb.innerHTML = filtered.slice(0, 80).map(function (x) {
    var src = instanceOf(x.Source);
    var p = providerOf(src);
    var c = srcClass(src);
    return '<tr>' +
      '<td class="mono">' + esc(x.ID) + '</td>' +
      '<td><span class="tag ' + c + '">' + esc(providerLabel(p)) + '</span></td>' +
      '<td class="mono">' + esc(instanceLabel(src)) + '</td>' +
      '<td class="trunc" title="' + esc(x.Title) + '">' + esc(x.Title) + '</td>' +
      '<td class="mono">' + esc(x.Model) + '</td>' +
      '<td class="trunc" title="' + esc(x.Project) + '">' + esc(x.Project) + '</td>' +
      '<td class="num">' + (x.Requests || 0) + '</td>' +
      '<td class="num">' + fmtTok(x.InputTok) + '</td>' +
      '<td class="num">' + fmtTok(x.OutputTok) + '</td>' +
      '<td class="num">' + fmtTok(x.CacheRead) + '</td>' +
      '<td class="num">' + fmtDur(x.Duration / 1e9) + '</td>' +
      '<td class="num">' + fmtCost(x) + '</td>' +
      '</tr>';
  }).join('');
}

function renderAlerts(filtered) {
  var al = document.getElementById('alerts');
  var alerts = (lastData && lastData.alerts) || [];
  if (!alerts.length) {
    al.innerHTML = '<span class="empty">无</span>';
    return;
  }
  var allIds = (lastSessions || []).map(function (s) { return s.ID; });
  var visibleIds = {};
  filtered.forEach(function (s) { visibleIds[s.ID] = true; });

  var shown = alerts.filter(function (a) {
    // JSON keys are lowercase (severity/message)
    var msg = a.message || a.Message || '';
    var mentions = null;
    for (var i = 0; i < allIds.length; i++) {
      if (allIds[i] && msg.indexOf(allIds[i]) >= 0) { mentions = allIds[i]; break; }
    }
    if (mentions) return !!visibleIds[mentions];
    // Budget / global alerts: hide when a strict filter is active
    if (activeProvider === 'all' && activeSource === 'all') return true;
    return false;
  });

  if (!shown.length) {
    al.innerHTML = '<span class="empty">无（当前筛选下）</span>';
    return;
  }
  al.innerHTML = '<ul>' + shown.map(function (a) {
    var sev = a.severity || a.Severity || 'info';
    var msg = a.message || a.Message || '';
    return '<li>[' + esc(sev) + '] ' + esc(msg) + '</li>';
  }).join('') + '</ul>';
}
</script>
</body>
</html>`
