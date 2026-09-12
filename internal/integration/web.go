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
<title>DevinMonitor 用量面板</title>
<style>
  :root {
    --bg: #0f1219;
    --panel: #171c27;
    --panel2: #1e2533;
    --border: #2a3347;
    --text: #e8ecf4;
    --muted: #8b95a8;
    --accent: #6c8cff;
    --gold: #ffd76a;
    --green: #3dd68c;
    --amber: #f5a524;
    --red: #f07178;
    --wsl: #c792ea;
  }
  * { box-sizing: border-box; }
  body {
    margin: 0; padding: 24px;
    font-family: "Segoe UI", "PingFang SC", "Microsoft YaHei", system-ui, sans-serif;
    background: var(--bg); color: var(--text);
    line-height: 1.45;
  }
  header {
    display: flex; flex-wrap: wrap; align-items: baseline; gap: 12px 20px;
    margin-bottom: 20px;
  }
  h1 { margin: 0; font-size: 22px; font-weight: 600; letter-spacing: 0.02em; }
  h1 span { color: var(--accent); }
  .live {
    display: inline-flex; align-items: center; gap: 6px;
    font-size: 12px; color: var(--muted);
  }
  .live i {
    width: 8px; height: 8px; border-radius: 50%;
    background: var(--green); display: inline-block;
    box-shadow: 0 0 8px var(--green);
    animation: pulse 1.6s ease infinite;
  }
  @keyframes pulse { 50% { opacity: 0.35; } }
  .updated { margin-left: auto; font-size: 12px; color: var(--muted); }
  .grid {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(150px, 1fr));
    gap: 12px; margin-bottom: 20px;
  }
  .card {
    background: var(--panel); border: 1px solid var(--border);
    border-radius: 10px; padding: 14px 16px;
  }
  .card .label { font-size: 12px; color: var(--muted); margin-bottom: 6px; }
  .card .value { font-size: 22px; font-weight: 600; font-variant-numeric: tabular-nums; }
  .card .value.gold { color: #ffd76a; }
  .card .value.accent { color: var(--accent); }
  .card .value.green { color: var(--green); }
  .card .value.wsl { color: var(--wsl); }
  section {
    background: var(--panel); border: 1px solid var(--border);
    border-radius: 10px; padding: 16px 18px; margin-bottom: 16px;
  }
  section h2 {
    margin: 0 0 12px; font-size: 14px; font-weight: 600;
    color: var(--accent); text-transform: uppercase; letter-spacing: 0.06em;
  }
  table { width: 100%; border-collapse: collapse; font-size: 13px; }
  th, td { text-align: left; padding: 8px 10px; border-bottom: 1px solid var(--border); }
  th { color: var(--muted); font-weight: 500; font-size: 12px; }
  td { font-variant-numeric: tabular-nums; }
  tr:last-child td { border-bottom: none; }
  .num { text-align: right; }
  .badge {
    display: inline-block; padding: 1px 8px; border-radius: 999px;
    font-size: 11px; font-weight: 600;
  }
  .badge.local { background: #243044; color: var(--accent); }
  .badge.wsl { background: #2d2140; color: var(--wsl); }
  .badge.mimo { background: #1a2f2a; color: #5eead4; }
  .badge.free { background: #1a3328; color: var(--green); }
  .src-item {
    display: flex; flex-wrap: wrap; gap: 8px 16px; align-items: center;
    padding: 10px 12px; border: 1px solid var(--border); border-radius: 8px;
    margin-bottom: 8px; background: var(--panel2); font-size: 13px;
  }
  .src-item .path { color: var(--muted); font-size: 12px; word-break: break-all; }
  .bars { display: flex; flex-direction: column; gap: 10px; }
  .bar-row { display: grid; grid-template-columns: 110px 1fr 70px; gap: 10px; align-items: center; font-size: 13px; }
  .bar-track { height: 10px; background: #222a3a; border-radius: 5px; overflow: hidden; }
  .bar-fill { height: 100%; border-radius: 5px; background: linear-gradient(90deg, #6c8cff, #9b7bff); }
  .bar-fill.out { background: linear-gradient(90deg, #3dd68c, #6c8cff); }
  .bar-fill.cache { background: linear-gradient(90deg, #f5a524, #f07178); }
  #alerts div { color: var(--red); margin: 4px 0; font-size: 13px; }
  .empty { color: var(--muted); font-size: 13px; }
  .title-cell { max-width: 280px; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
  .filters { float: right; font-weight: 400; text-transform: none; letter-spacing: 0; }
  .filters button {
    background: var(--panel2); color: var(--muted);
    border: 1px solid var(--border); border-radius: 999px;
    padding: 3px 10px; margin-left: 6px; font-size: 12px; cursor: pointer;
  }
  .filters button.on { color: var(--text); border-color: var(--accent); background: #243044; }
  .pct-track { height: 8px; background: #222a3a; border-radius: 4px; overflow: hidden; }
  .pct-fill { height: 100%; background: linear-gradient(90deg, #6c8cff, #c792ea); border-radius: 4px; }
</style>
</head>
<body>
<header>
  <h1><span>Devin</span>Monitor 用量面板</h1>
  <div class="live"><i></i> SSE 实时 · 5s</div>
  <div class="updated" id="updated">连接中…</div>
</header>

<div class="grid" id="kpis"></div>

<section>
  <h2>数据源</h2>
  <div id="sources"><div class="empty">加载中…</div></div>
</section>

<section>
  <h2>Token 构成</h2>
  <div class="bars" id="tokenBars"></div>
</section>

<section>
  <h2>模型用量</h2>
  <div style="overflow-x:auto">
  <table id="models">
    <thead><tr>
      <th>模型</th>
      <th class="num">会话</th><th class="num">请求</th>
      <th class="num">输入</th><th class="num">输出</th><th class="num">缓存读</th>
      <th class="num">合计</th><th class="num">速度</th><th class="num">成本</th>
      <th style="width:120px">占比</th>
    </tr></thead>
    <tbody></tbody>
  </table>
  </div>
</section>

<section>
  <h2>
    会话用量
    <span class="filters" id="sourceFilter"></span>
  </h2>
  <div style="overflow-x:auto">
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

<section>
  <h2>告警</h2>
  <div id="alerts"><div class="empty">无</div></div>
</section>

<script>
const es = new EventSource('/sse');

es.onmessage = function(e) {
  let d;
  try { d = JSON.parse(e.data); } catch (_) { return; }
  if (d.error) {
    document.getElementById('updated').textContent = '错误: ' + d.error;
    return;
  }
  render(d);
};

function fmtTok(n) {
  n = n || 0;
  if (n >= 1e9) return (n/1e9).toFixed(2) + 'B';
  if (n >= 1e6) return (n/1e6).toFixed(1) + 'M';
  if (n >= 1e3) return (n/1e3).toFixed(1) + 'k';
  return String(n);
}

function fmtDur(sec) {
  sec = sec || 0;
  if (sec < 60) return sec + 's';
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
  const isWsl = src.indexOf('wsl') === 0;
  const isMimo = src === 'mimo';
  const cls = isMimo ? 'mimo' : (isWsl ? 'wsl' : 'local');
  return '<span class="badge ' + cls + '">' + esc(src) + '</span>';
}

let activeSource = 'all';
let lastSessions = [];

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
    return '<button class="' + on.trim() + '" data-src="' + esc(k) + '">' + esc(label) + '</button>';
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
  if (!filtered.length) {
    tb.innerHTML = '<tr><td colspan="11" class="empty">无会话</td></tr>';
    return;
  }
  tb.innerHTML = filtered.slice(0, 80).map(function(x) {
    return '<tr>' +
      '<td>' + esc(x.ID) + '</td>' +
      '<td>' + sourceBadge(x.Source) + '</td>' +
      '<td class="title-cell" title="' + esc(x.Title) + '">' + esc(x.Title) + '</td>' +
      '<td>' + esc(x.Model) + '</td>' +
      '<td>' + esc(x.Project) + '</td>' +
      '<td class="num">' + (x.Requests || 0) + '</td>' +
      '<td class="num">' + fmtTok(x.InputTok) + '</td>' +
      '<td class="num">' + fmtTok(x.OutputTok) + '</td>' +
      '<td class="num">' + fmtTok(x.CacheRead) + '</td>' +
      '<td class="num">' + fmtDur(x.Duration / 1e9) + '</td>' +
      '<td class="num">' + fmtCost(x) + '</td>' +
      '</tr>';
  }).join('');
}

function render(d) {
  document.getElementById('updated').textContent =
    '更新于 ' + new Date(d.updated || Date.now()).toLocaleTimeString();

  const u = d.usage || {};
  const s = d.summary || {};
  const nSrc = (d.sources || []).length;
  const kpis = [
    ['会话', s.totalSessions || 0, 'accent'],
    ['请求', u.requests || 0, ''],
    ['输入 Token', fmtTok(u.inputTokens), ''],
    ['输出 Token', fmtTok(u.outputTokens), 'green'],
    ['缓存读', fmtTok(u.cacheRead), ''],
    ['总 Token', fmtTok(u.totalTokens), 'gold'],
    ['成本', s.totalCost > 0 ? '$' + s.totalCost.toFixed(2) : 'free', 'gold'],
    ['数据源', nSrc, nSrc > 1 ? 'wsl' : ''],
  ];
  const kg = document.getElementById('kpis');
  kg.innerHTML = '';
  kpis.forEach(function(item) {
    const c = document.createElement('div');
    c.className = 'card';
    c.innerHTML = '<div class="label">' + item[0] + '</div>' +
      '<div class="value ' + (item[2] || '') + '">' + item[1] + '</div>';
    kg.appendChild(c);
  });

  // Sources
  const srcBox = document.getElementById('sources');
  const sources = d.sources || [];
  if (!sources.length) {
    srcBox.innerHTML = '<div class="empty">无数据源</div>';
  } else {
    srcBox.innerHTML = sources.map(function(x) {
      return '<div class="src-item">' + sourceBadge(x.label) +
        '<strong>' + x.sessions + ' 会话</strong>' +
        '<span>schema ' + x.schema + '</span>' +
        '<span class="path">' + esc(x.path) + '</span></div>';
    }).join('');
  }

  // Token bars
  const maxTok = Math.max(u.inputTokens || 0, u.outputTokens || 0, u.cacheRead || 0, 1);
  const barData = [
    ['输入', u.inputTokens, ''],
    ['输出', u.outputTokens, 'out'],
    ['缓存读', u.cacheRead, 'cache'],
  ];
  document.getElementById('tokenBars').innerHTML = barData.map(function(b) {
    const pct = Math.round((b[1] / maxTok) * 100);
    return '<div class="bar-row"><span>' + b[0] + '</span>' +
      '<div class="bar-track"><div class="bar-fill ' + b[2] + '" style="width:' + pct + '%"></div></div>' +
      '<span class="num">' + fmtTok(b[1]) + '</span></div>';
  }).join('');

  // Models
  const models = d.models || [];
  const mMax = Math.max.apply(null, models.map(function(m) { return m.totalTokens || 0; }).concat([1]));
  const mtb = document.querySelector('#models tbody');
  if (!models.length) {
    mtb.innerHTML = '<tr><td colspan="10" class="empty">无数据</td></tr>';
  } else {
    mtb.innerHTML = models.map(function(m) {
      const pct = Math.round(((m.totalTokens || 0) / mMax) * 100);
      const cost = m.isFree || !m.cost ? 'free' : '$' + (m.cost || 0).toFixed(2);
      return '<tr>' +
        '<td>' + esc(m.name) + '</td>' +
        '<td class="num">' + (m.sessions || 0) + '</td>' +
        '<td class="num">' + (m.requests || 0) + '</td>' +
        '<td class="num">' + fmtTok(m.inputTokens) + '</td>' +
        '<td class="num">' + fmtTok(m.outputTokens) + '</td>' +
        '<td class="num">' + fmtTok(m.cacheRead) + '</td>' +
        '<td class="num">' + fmtTok(m.totalTokens) + '</td>' +
        '<td class="num">' + Math.round(m.tokPerSec || 0) + ' t/s</td>' +
        '<td class="num">' + cost + '</td>' +
        '<td><div class="pct-track"><div class="pct-fill" style="width:' + pct + '%"></div></div></td>' +
        '</tr>';
    }).join('');
  }

  // Sessions + filter
  lastSessions = d.sessions || [];
  renderSourceFilter(lastSessions);
  renderSessions(lastSessions);

  // Alerts
  const al = document.getElementById('alerts');
  const alerts = d.alerts || [];
  al.innerHTML = alerts.length
    ? alerts.map(function(a) { return '<div>[' + esc(a.Severity) + '] ' + esc(a.Message) + '</div>'; }).join('')
    : '<div class="empty">无</div>';
}
</script>
</body>
</html>`
