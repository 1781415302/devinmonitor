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
<title>DevinMonitor · Token 用量</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link href="https://fonts.googleapis.com/css2?family=Fira+Code:wght@400;500;600&family=Fira+Sans:wght@300;400;500;600;700&display=swap" rel="stylesheet">
<style>
:root {
  --color-background: #0F172A;
  --color-card: #1B2336;
  --color-muted: #272F42;
  --color-border: rgba(148, 163, 184, 0.18);
  --color-border-strong: rgba(148, 163, 184, 0.28);
  --color-foreground: #F8FAFC;
  --color-muted-fg: #94A3B8;
  --color-accent: #22C55E;
  --color-accent-soft: rgba(34, 197, 94, 0.14);
  --color-wsl: #A78BFA;
  --color-mimo: #2DD4BF;
  --color-local: #60A5FA;
  --radius: 14px;
  --radius-sm: 10px;
  --shadow: 0 8px 32px rgba(0,0,0,.35);
  --font-sans: "Fira Sans", "PingFang SC", "Microsoft YaHei", system-ui, sans-serif;
  --font-mono: "Fira Code", ui-monospace, Consolas, monospace;
  --blur: 16px;
}
* { box-sizing: border-box; }
html, body { margin: 0; padding: 0; }
body {
  min-height: 100vh;
  font-family: var(--font-sans);
  font-size: 15px;
  line-height: 1.5;
  color: var(--color-foreground);
  background:
    radial-gradient(1200px 600px at 10% -10%, rgba(34,197,94,.12), transparent 55%),
    radial-gradient(900px 500px at 90% 0%, rgba(96,165,250,.10), transparent 50%),
    radial-gradient(800px 400px at 50% 100%, rgba(167,139,250,.08), transparent 50%),
    var(--color-background);
  background-attachment: fixed;
}
.shell { max-width: 1280px; margin: 0 auto; padding: 28px 22px 48px; }
header.top { display: flex; flex-wrap: wrap; align-items: center; gap: 12px 18px; margin-bottom: 22px; }
.brand { display: flex; align-items: center; gap: 12px; }
.mark {
  width: 38px; height: 38px; border-radius: 12px; display: grid; place-items: center;
  background: linear-gradient(145deg, rgba(34,197,94,.25), rgba(96,165,250,.18));
  border: 1px solid var(--color-border-strong); backdrop-filter: blur(var(--blur));
}
.mark svg { width: 20px; height: 20px; }
h1 { margin: 0; font-size: 1.25rem; font-weight: 600; letter-spacing: .01em; }
h1 span { color: var(--color-accent); font-weight: 700; }
.subtitle { margin: 2px 0 0; color: var(--color-muted-fg); font-size: .8rem; font-weight: 400; }
.live-pill {
  display: inline-flex; align-items: center; gap: 8px; padding: 6px 12px; border-radius: 999px;
  background: rgba(15,23,42,.55); border: 1px solid var(--color-border);
  backdrop-filter: blur(var(--blur)); font-size: .75rem; color: var(--color-muted-fg); font-family: var(--font-mono);
}
.live-pill i {
  width: 8px; height: 8px; border-radius: 50%; background: var(--color-accent);
  box-shadow: 0 0 0 3px var(--color-accent-soft), 0 0 10px var(--color-accent);
  animation: pulse 1.8s ease infinite;
}
@keyframes pulse { 50% { opacity: .45; } }
.updated { margin-left: auto; font-size: .75rem; color: var(--color-muted-fg); font-family: var(--font-mono); }
.kpi-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(148px, 1fr)); gap: 12px; margin-bottom: 18px; }
.card {
  background: linear-gradient(160deg, rgba(27,35,54,.92), rgba(27,35,54,.72));
  border: 1px solid var(--color-border); border-radius: var(--radius);
  box-shadow: var(--shadow); backdrop-filter: blur(var(--blur)); padding: 14px 16px;
}
.kpi .label { font-size: .72rem; color: var(--color-muted-fg); text-transform: uppercase; letter-spacing: .06em; margin-bottom: 8px; font-weight: 500; }
.kpi .value { font-family: var(--font-mono); font-size: 1.35rem; font-weight: 600; font-variant-numeric: tabular-nums; letter-spacing: -.02em; }
.kpi .value.accent { color: var(--color-accent); }
.kpi .value.local { color: var(--color-local); }
.kpi .value.mimo { color: var(--color-mimo); }
.kpi .value.wsl { color: var(--color-wsl); }
.panel {
  background: linear-gradient(160deg, rgba(27,35,54,.92), rgba(27,35,54,.70));
  border: 1px solid var(--color-border); border-radius: var(--radius);
  box-shadow: var(--shadow); backdrop-filter: blur(var(--blur)); padding: 16px 18px 18px; margin-bottom: 16px;
}
.panel-head { display: flex; flex-wrap: wrap; align-items: center; gap: 10px; margin-bottom: 14px; }
.panel-head h2 { margin: 0; font-size: .78rem; font-weight: 600; letter-spacing: .08em; text-transform: uppercase; color: var(--color-accent); }
.filters { margin-left: auto; display: flex; flex-wrap: wrap; gap: 6px; }
.filters button {
  font: inherit; font-size: .72rem; padding: 5px 12px; border-radius: 999px;
  border: 1px solid var(--color-border); background: rgba(15,23,42,.45);
  color: var(--color-muted-fg); cursor: pointer;
  transition: color .15s ease, border-color .15s ease, background .15s ease;
}
.filters button:hover { color: var(--color-foreground); border-color: var(--color-border-strong); }
.filters button:focus-visible { outline: 2px solid var(--color-accent); outline-offset: 2px; }
.filters button.on { color: var(--color-foreground); border-color: rgba(34,197,94,.55); background: var(--color-accent-soft); }
.src-list { display: flex; flex-direction: column; gap: 8px; }
.src-item {
  display: flex; flex-wrap: wrap; align-items: center; gap: 8px 14px; padding: 10px 12px;
  border-radius: var(--radius-sm); background: rgba(15,23,42,.4); border: 1px solid var(--color-border); font-size: .82rem;
}
.src-item .path { color: var(--color-muted-fg); font-family: var(--font-mono); font-size: .7rem; word-break: break-all; flex: 1 1 180px; min-width: 0; }
.badge {
  display: inline-flex; align-items: center; gap: 6px; padding: 2px 10px; border-radius: 999px;
  font-size: .7rem; font-weight: 600; font-family: var(--font-mono); border: 1px solid transparent;
}
.badge.local { background: rgba(96,165,250,.12); color: var(--color-local); border-color: rgba(96,165,250,.25); }
.badge.wsl { background: rgba(167,139,250,.12); color: var(--color-wsl); border-color: rgba(167,139,250,.25); }
.badge.mimo { background: rgba(45,212,191,.12); color: var(--color-mimo); border-color: rgba(45,212,191,.25); }
.bars { display: flex; flex-direction: column; gap: 12px; }
.bar-row { display: grid; grid-template-columns: 72px 1fr 72px; gap: 12px; align-items: center; font-size: .82rem; }
.bar-row .name { color: var(--color-muted-fg); }
.bar-track { height: 10px; border-radius: 999px; background: rgba(15,23,42,.65); border: 1px solid var(--color-border); overflow: hidden; }
.bar-fill { height: 100%; border-radius: 999px; background: linear-gradient(90deg, #22C55E, #60A5FA); transition: width .35s ease; }
.bar-fill.out { background: linear-gradient(90deg, #34D399, #2DD4BF); }
.bar-fill.cache { background: linear-gradient(90deg, #F59E0B, #F87171); }
.bar-row .val { text-align: right; font-family: var(--font-mono); font-variant-numeric: tabular-nums; }
.table-wrap { overflow-x: auto; border-radius: var(--radius-sm); }
table { width: 100%; border-collapse: collapse; font-size: .82rem; min-width: 640px; }
th, td { text-align: left; padding: 9px 10px; border-bottom: 1px solid var(--color-border); vertical-align: middle; }
th { color: var(--color-muted-fg); font-weight: 500; font-size: .7rem; text-transform: uppercase; letter-spacing: .05em; position: sticky; top: 0; background: rgba(27,35,54,.95); backdrop-filter: blur(8px); z-index: 1; }
td { font-variant-numeric: tabular-nums; }
tbody tr { transition: background .15s ease; }
tbody tr:hover { background: rgba(34,197,94,.05); }
tr:last-child td { border-bottom: none; }
.num { text-align: right; font-family: var(--font-mono); }
.mono { font-family: var(--font-mono); font-size: .78rem; }
.title-cell { max-width: 260px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.pct-track { height: 8px; min-width: 88px; background: rgba(15,23,42,.65); border-radius: 999px; overflow: hidden; border: 1px solid var(--color-border); }
.pct-fill { height: 100%; background: linear-gradient(90deg, #22C55E, #60A5FA); border-radius: 999px; transition: width .35s ease; }
.empty { color: var(--color-muted-fg); font-size: .85rem; padding: 8px 0; }
#alerts div { color: #FCA5A5; font-size: .85rem; padding: 6px 0; border-bottom: 1px solid var(--color-border); }
#alerts div:last-child { border-bottom: none; }
@media (max-width: 640px) {
  .shell { padding: 18px 12px 32px; }
  .updated { margin-left: 0; width: 100%; }
  .bar-row { grid-template-columns: 56px 1fr 64px; }
}
@media (prefers-reduced-motion: reduce) {
  .live-pill i { animation: none; }
  .bar-fill, .pct-fill, tbody tr, .filters button { transition: none; }
}
</style>
</head>
<body>
<div class="shell">
  <header class="top">
    <div class="brand">
      <div class="mark" aria-hidden="true">
        <svg viewBox="0 0 24 24" fill="none" stroke="#22C55E" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round">
          <path d="M4 19V5"/><path d="M10 19V9"/><path d="M16 19V7"/><path d="M22 19V3"/>
        </svg>
      </div>
      <div>
        <h1><span>Devin</span>Monitor</h1>
        <p class="subtitle">Devin · WSL · MiMo token 用量</p>
      </div>
    </div>
    <div class="live-pill" title="SSE 每 5 秒推送"><i></i> LIVE · 5s</div>
    <div class="updated" id="updated">连接中…</div>
  </header>
  <div class="kpi-grid" id="kpis" aria-live="polite"></div>
  <section class="panel">
    <div class="panel-head"><h2>数据源</h2></div>
    <div class="src-list" id="sources"><div class="empty">加载中…</div></div>
  </section>
  <section class="panel">
    <div class="panel-head"><h2>Token 构成</h2></div>
    <div class="bars" id="tokenBars"></div>
  </section>
  <section class="panel">
    <div class="panel-head"><h2>模型用量</h2></div>
    <div class="table-wrap">
      <table id="models">
        <thead><tr>
          <th>模型</th>
          <th class="num">会话</th><th class="num">请求</th>
          <th class="num">输入</th><th class="num">输出</th><th class="num">缓存读</th>
          <th class="num">合计</th><th class="num">速度</th><th class="num">成本</th>
          <th>占比</th>
        </tr></thead>
        <tbody></tbody>
      </table>
    </div>
  </section>
  <section class="panel">
    <div class="panel-head">
      <h2>会话用量</h2>
      <div class="filters" id="sourceFilter" role="group" aria-label="按来源筛选"></div>
    </div>
    <div class="table-wrap">
      <table id="sessions">
        <thead><tr>
          <th>ID</th><th>来源</th><th>标题</th><th>模型</th><th>项目</th>
          <th class="num">请求</th><th class="num">输入</th><th class="num">输出</th>
          <th class="num">缓存读</th><th class="num">时长</th><th class="num">成本</th>
        </tr></thead>
        <tbody></tbody>
      </table>
    </div>
  </section>
  <section class="panel">
    <div class="panel-head"><h2>告警</h2></div>
    <div id="alerts"><div class="empty">无</div></div>
  </section>
</div>
<script>
const es = new EventSource('/sse');
es.onmessage = function(e) {
  let d; try { d = JSON.parse(e.data); } catch (_) { return; }
  if (d.error) { document.getElementById('updated').textContent = '错误: ' + d.error; return; }
  render(d);
};
es.onerror = function() { document.getElementById('updated').textContent = '连接中断，重试中…'; };
let activeSource = 'all';
let lastSessions = [];
function fmtTok(n) {
  n = n || 0;
  if (n >= 1e9) return (n/1e9).toFixed(2) + 'B';
  if (n >= 1e6) return (n/1e6).toFixed(1) + 'M';
  if (n >= 1e3) return (n/1e3).toFixed(1) + 'k';
  return String(n);
}
function fmtDur(sec) {
  sec = sec || 0;
  if (sec < 60) return Math.round(sec) + 's';
  if (sec < 3600) return Math.round(sec/60) + 'm';
  if (sec < 86400) return (sec/3600).toFixed(1) + 'h';
  return (sec/86400).toFixed(1) + 'd';
}
function fmtCost(s) {
  if (s.IsFree || s.Cost === 0) return 'free';
  return '$' + (s.Cost || 0).toFixed(2);
}
function esc(s) {
  const d = document.createElement('div');
  d.textContent = s == null ? '' : String(s);
  return d.innerHTML;
}
function sourceBadge(src) {
  if (!src || src === 'local') return '<span class="badge local">local</span>';
  if (src === 'mimo') return '<span class="badge mimo">mimo</span>';
  const isWsl = src.indexOf('wsl') === 0;
  return '<span class="badge ' + (isWsl ? 'wsl' : 'local') + '">' + esc(src) + '</span>';
}
function renderSourceFilter(sessions) {
  const box = document.getElementById('sourceFilter');
  const set = {};
  sessions.forEach(function(x) { set[x.Source || 'local'] = true; });
  const keys = Object.keys(set).sort();
  if (keys.length <= 1) { box.innerHTML = ''; return; }
  const opts = ['all'].concat(keys);
  box.innerHTML = opts.map(function(k) {
    const on = (activeSource === k) ? ' on' : '';
    const label = k === 'all' ? '全部' : k;
    return '<button type="button" class="' + on.trim() + '" data-src="' + esc(k) + '" aria-pressed="' + (activeSource===k) + '">' + esc(label) + '</button>';
  }).join('');
  box.querySelectorAll('button').forEach(function(btn) {
    btn.onclick = function() {
      activeSource = btn.getAttribute('data-src');
      renderSessions(lastSessions);
      renderSourceFilter(lastSessions);
    };
  });
}
function renderSessions(sessions) {
  const tb = document.querySelector('#sessions tbody');
  const filtered = sessions.filter(function(x) {
    if (activeSource === 'all') return true;
    return (x.Source || 'local') === activeSource;
  });
  if (!filtered.length) { tb.innerHTML = '<tr><td colspan="11" class="empty">无会话</td></tr>'; return; }
  tb.innerHTML = filtered.slice(0, 80).map(function(x) {
    return '<tr><td class="mono">' + esc(x.ID) + '</td><td>' + sourceBadge(x.Source) + '</td><td class="title-cell" title="' + esc(x.Title) + '">' + esc(x.Title) + '</td><td class="mono">' + esc(x.Model) + '</td><td>' + esc(x.Project) + '</td><td class="num">' + (x.Requests || 0) + '</td><td class="num">' + fmtTok(x.InputTok) + '</td><td class="num">' + fmtTok(x.OutputTok) + '</td><td class="num">' + fmtTok(x.CacheRead) + '</td><td class="num">' + fmtDur(x.Duration / 1e9) + '</td><td class="num">' + fmtCost(x) + '</td></tr>';
  }).join('');
}
function render(d) {
  document.getElementById('updated').textContent = '更新于 ' + new Date(d.updated || Date.now()).toLocaleTimeString();
  const u = d.usage || {};
  const s = d.summary || {};
  const nSrc = (d.sources || []).length;
  const kpis = [
    ['会话', s.totalSessions || 0, ''],
    ['请求', u.requests || 0, ''],
    ['输入 Token', fmtTok(u.inputTokens), 'local'],
    ['输出 Token', fmtTok(u.outputTokens), 'mimo'],
    ['缓存读', fmtTok(u.cacheRead), ''],
    ['总 Token', fmtTok(u.totalTokens), 'accent'],
    ['成本', s.totalCost > 0 ? '$' + s.totalCost.toFixed(2) : 'free', 'accent'],
    ['数据源', nSrc, nSrc > 1 ? 'wsl' : '']
  ];
  const kg = document.getElementById('kpis');
  kg.innerHTML = '';
  kpis.forEach(function(item) {
    const c = document.createElement('div');
    c.className = 'card kpi';
    c.innerHTML = '<div class="label">' + item[0] + '</div><div class="value ' + (item[2] || '') + '">' + item[1] + '</div>';
    kg.appendChild(c);
  });
  const srcBox = document.getElementById('sources');
  const sources = d.sources || [];
  if (!sources.length) srcBox.innerHTML = '<div class="empty">无数据源</div>';
  else srcBox.innerHTML = sources.map(function(x) {
    return '<div class="src-item">' + sourceBadge(x.label) + '<strong>' + x.sessions + ' 会话</strong><span class="mono" style="color:var(--color-muted-fg);font-size:.72rem">schema ' + x.schema + '</span><span class="path">' + esc(x.path) + '</span></div>';
  }).join('');
  const maxTok = Math.max(u.inputTokens || 0, u.outputTokens || 0, u.cacheRead || 0, 1);
  document.getElementById('tokenBars').innerHTML = [['输入', u.inputTokens, ''], ['输出', u.outputTokens, 'out'], ['缓存读', u.cacheRead, 'cache']].map(function(b) {
    const pct = Math.round((b[1] / maxTok) * 100);
    return '<div class="bar-row"><span class="name">' + b[0] + '</span><div class="bar-track"><div class="bar-fill ' + b[2] + '" style="width:' + pct + '%"></div></div><span class="val">' + fmtTok(b[1]) + '</span></div>';
  }).join('');
  const models = d.models || [];
  const mMax = Math.max.apply(null, models.map(function(m) { return m.totalTokens || 0; }).concat([1]));
  const mtb = document.querySelector('#models tbody');
  if (!models.length) mtb.innerHTML = '<tr><td colspan="10" class="empty">无数据</td></tr>';
  else mtb.innerHTML = models.map(function(m) {
    const pct = Math.round(((m.totalTokens || 0) / mMax) * 100);
    const cost = m.isFree || !m.cost ? 'free' : '$' + (m.cost || 0).toFixed(2);
    const speed = m.tokPerSec > 0 ? Math.round(m.tokPerSec) + ' t/s' : '—';
    return '<tr><td class="mono">' + esc(m.name) + '</td><td class="num">' + (m.sessions || 0) + '</td><td class="num">' + (m.requests || 0) + '</td><td class="num">' + fmtTok(m.inputTokens) + '</td><td class="num">' + fmtTok(m.outputTokens) + '</td><td class="num">' + fmtTok(m.cacheRead) + '</td><td class="num">' + fmtTok(m.totalTokens) + '</td><td class="num">' + speed + '</td><td class="num">' + cost + '</td><td><div class="pct-track"><div class="pct-fill" style="width:' + pct + '%"></div></div></td></tr>';
  }).join('');
  lastSessions = d.sessions || [];
  renderSourceFilter(lastSessions);
  renderSessions(lastSessions);
  const al = document.getElementById('alerts');
  const alerts = d.alerts || [];
  al.innerHTML = alerts.length ? alerts.map(function(a) { return '<div>[' + esc(a.Severity) + '] ' + esc(a.Message) + '</div>'; }).join('') : '<div class="empty">无</div>';
}
</script>
</body>
</html>`
