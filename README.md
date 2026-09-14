# DevinMonitor

**Token & cost monitor for Devin CLI, WSL, and MiMoCode.**

DevinMonitor reads local session databases and provides real-time
monitoring, usage reports, and cost tracking — with Devin-specific
metrics (TTFT, tokens/sec, finish-reason distribution, context growth,
sub-agent usage) that other monitors don't offer. On Windows it merges
local Devin, Devin inside WSL distros, and MiMoCode usage by default.

[中文文档](README.zh-CN.md)

---

## Live dashboard

The `live` command renders a real-time bubbletea TUI that polls
`sessions.db` every few seconds and shows a full at-a-glance view of
the current session:

![Live dashboard](docs/images/live_dashboard.png)

**Panels:**
- **Tokens** — input / output / cache-read / cache-write totals, live
  generation **rate** (tok/s, summed across concurrent requests over a
  60-second rolling window), and averages.
- **Context** — current context size vs. model window, fill bar,
  average / peak / remaining tokens, cache-write indicator.
- **Status** — session cost, request count (with sub-agent count),
  duration, average request time, TTFT / total p50, total tool calls.
- **Request stream** — scrolling list of recent requests with TTFT,
  tok/s, elapsed, and finish reason, plus a TTFT sparkline with p50/p95.
- **Context growth** — full-session context size history with cache-hit
  ratio.
- **Latency** — TTFT / total / tok/s percentiles and finish-reason
  distribution bar.
- **Tools** — per-tool call counts with horizontal bars, including
  `run_subagent` and `read_subagent` calls.

## Reports

All report commands render styled tables with consistent grey borders
and a `TOTALS` row. Column widths adapt to the terminal; on wide
terminals columns expand, on narrow terminals they truncate gracefully.

### `sessions` — session list

![Sessions](docs/images/sessions.png)

### `session <id>` — session detail

Shows metadata, token breakdown, sub-agent call list (with profile,
task, background/foreground, completion status), and a per-tool call
count breakdown.

![Session detail](docs/images/session_detail.png)

### `daily` / `weekly` / `monthly` — time-bucketed usage

Token, request, sub-agent, and cost totals per day / week / month,
with the list of models used. Use `--breakdown` to add a per-model
sub-table. `weekly` supports `--start-day monday|sunday|...`.

![Daily report](docs/images/daily.png)

![Weekly report](docs/images/weekly.png)

![Monthly report](docs/images/monthly.png)

### `models` — per-model analytics

Requests, tokens (input / output / cache-read / cache-write), cost,
cost share, and average generation speed per model.

![Models](docs/images/models.png)

### `model <name>` — model detail

First/last used, days active, token totals, cost, average per day and
per session, p50/p95 TTFT and total time, truncation rate, and a
per-tool breakdown for that model.

![Model detail](docs/images/model_detail.png)

### `projects` — per-project usage

Sessions, requests, tokens, cost, and models used, grouped by working
directory's project name.

![Projects](docs/images/projects.png)

### `agents` — sub-agent usage statistics

Detailed sub-agent usage grouped by profile: total calls, sessions
involved, background/foreground split, completion count, `read_subagent`
waits, average/max duration, average/max task length, and average/max
output length.

![Sub-agent usage](docs/images/agents.png)

## Install

Download a pre-built binary from
[Releases](https://github.com/1781415302/devinmonitor/releases)
(Windows / Linux / macOS, amd64 + arm64).

Or build from source:

```bash
git clone https://github.com/1781415302/devinmonitor.git
cd devinmonitor
go build -o devinmonitor .
```

## Usage

```bash
# Data sources (local Devin + WSL + MiMo)
devinmonitor sources

# Web usage dashboard (default :19191, SSE live)
devinmonitor web --open

# Live TUI (needs a TTY)
devinmonitor live

# Sessions
devinmonitor sessions
devinmonitor session wsl:halved-noodle
devinmonitor session mimo:ses_xxx

# Reports
devinmonitor daily --breakdown
devinmonitor weekly --start-day monday --breakdown
devinmonitor monthly
devinmonitor models
devinmonitor model xiaomi/mimo-x-pro-preview
devinmonitor projects
devinmonitor agents

# Prometheus metrics
devinmonitor metrics --addr :9101

# Export normalized JSON
devinmonitor export --detailed > usage.json
```

### Global flags

```
--data-dir string   Single data directory (skips WSL / MiMo merge)
--locale string     Language: en / zh (default: auto-detect)
--no-wsl            Do not merge WSL Devin stores
--no-mimo           Do not merge MiMoCode mimocode.db
--wsl stringArray   Only merge these WSL distros (repeatable)
```

### Live dashboard controls

```
q          quit
r          switch to next session
1-4 / Tab  jump to section (compact mode)
L          toggle locale (en <-> zh)
```

## Responsive TUI

The live dashboard adapts to terminal size with four breakpoints:

| Breakpoint | Size | Layout |
|------------|------|--------|
| **Full** | >= 120 cols, >= 24 rows | complete 4-row dashboard |
| **Compact** | 80-119 cols | 2-column + tabbed views |
| **Mini** | < 80 cols | single-column flow for narrow windows / termux |
| **Tiny** | < 6 rows | single-line ticker for tmux splits |

Window resize is handled instantly via bubbletea's `WindowSizeMsg`.

## Data source

DevinMonitor reads `sessions.db` from Devin CLI's data directory:
- **Linux**: `~/.local/share/devin/cli/sessions.db`
- **macOS**: `~/Library/Application Support/devin/cli/sessions.db`
- **Windows**: `%APPDATA%\devin\cli\sessions.db`

Override with `--data-dir` or the `DEVIN_DATA_DIR` environment variable.

### Windows + WSL + MiMo merge

On Windows, sources are auto-detected and merged:

| Source | Path | Notes |
|--------|------|-------|
| `local` | `%APPDATA%\devin\cli\sessions.db` | Local Devin |
| `wsl:<distro>` | `~/.local/share/devin/cli/sessions.db` in each WSL | Snapshotted via `wsl.exe` (avoids SQLITE_BUSY) |
| `mimo` | `~/.local/share/mimocode/mimocode.db` | MiMoCode tokens / tools |

```bash
devinmonitor sources                 # inspect sources
devinmonitor --no-wsl sessions
devinmonitor --no-mimo sessions
devinmonitor --wsl Ubuntu-18.04 sessions
devinmonitor --data-dir /path/to/cli sessions   # single source
```

After merge:
- Session IDs are prefixed: `wsl:halved-noodle`, `mimo:ses_xxx`
- Full form also works: `session wsl:Ubuntu-18.04/halved-noodle`
- `models` / `cost` / `daily` / `web` aggregate all sources

MiMo tokens/sec is estimated from text part `start`/`end` timings
(excluding tool wait). Same order of magnitude as Devin's reported
generation speed, but not the same field.

Connections are read-only + WAL + `query_only`.

### Web usage dashboard

```bash
devinmonitor web                 # http://localhost:19191
devinmonitor web --open
devinmonitor web --port 19273
```

Default port **19191** (avoids common 8080/3000). SSE refreshes usage
every 5s; rebuild + restart `web` after code changes.

Light Swiss-minimal UI: KPIs, source chips, stacked token bar, model
table (t/s, cost, share), filterable sessions, alerts.

APIs: `/api/sessions` `/api/models` `/api/sources` `/api/cost-summary` `/sse`

### Schema adaptation

Devin CLI's SQLite schema is an internal implementation detail that may
change between versions. The `reader` package isolates schema-specific
SQL/JSON parsing behind a version-detected adapter. When Devin CLI
changes its schema, only a new adapter (e.g. `v2.go`) is needed —
reports and UI are unaffected.

## Cost calculation

| Layer | Source | When used |
|-------|--------|-----------|
| Authoritative | `sessions.metadata.total_credit_cost` / `total_acu_cost` | Non-zero (paid models) |
| Estimate | Built-in token x price table | Credit is zero (free models) |
| Future | External pricing API (openrouter etc.) | Planned |

Free models (e.g. `glm-5-2`) show `free` in cost columns.

## Prometheus metrics

The `metrics` command starts an HTTP server (default `:9101`) exposing
Prometheus-format gauges:

- `devinmonitor_sessions_total`
- `devinmonitor_requests_total`
- `devinmonitor_input_tokens_total` / `devinmonitor_output_tokens_total`
- `devinmonitor_cache_read_tokens_total` / `devinmonitor_cache_write_tokens_total`
- `devinmonitor_cost_total`
- `devinmonitor_model_*` — per-model breakdown
- `devinmonitor_project_*` — per-project breakdown

Scrape it with Prometheus or curl:
```bash
devinmonitor metrics &
curl http://localhost:9101/metrics
```

## Architecture

```
Devin local sessions.db ──┐
WSL sessions.db (snapshot) ┼─ MultiReader ─ Normalized models
MiMo mimocode.db ──────────┘        │
                                    ├─ Report (sessions/daily/…/models/projects/agents)
                                    ├─ Live (bubbletea)
                                    ├─ Web (HTTP + SSE, :19191)
                                    ├─ Export (stable JSON)
                                    └─ Metrics (Prometheus)
```

The export format (`export_schema: 1`) is independent of Devin's
internal schema, designed as a stable contract for future web-based
sharing (token leaderboards, burn-rate comparisons, etc.).

## Platform & locale

- **Multi-arch**: linux / darwin / windows x amd64 / arm64 (single binary, no CGO)
- **i18n**: English + 中文, auto-detected from system `LANG` / `LC_ALL`
- **CJK rendering**: correct character width via go-runewidth

## Tech stack

- **Go** — single binary, no runtime dependencies
- **bubbletea + lipgloss** — responsive TUI framework
- **modernc.org/sqlite** — pure-Go SQLite (no CGO, cross-compile friendly)
- **cobra** — CLI command framework

## License

MIT
