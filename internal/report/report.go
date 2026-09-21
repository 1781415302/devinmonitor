// Package report computes aggregated views over sessions for CLI output.
package report

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/garywhat/devinmonitor/internal/model"
)

// FormatDur formats a duration as compact h/m/s.
func FormatDur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d >= time.Hour:
		return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
	case d >= time.Minute:
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
}

// FormatTok formats token counts with k/M suffixes.
func FormatTok(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// FormatCost formats USD cost; shows "free" for zero on free models.
func FormatCost(cost float64, isFree bool) string {
	if isFree || cost == 0 {
		return "free"
	}
	return fmt.Sprintf("$%.2f", cost)
}

// SessionCost returns the authoritative cost (credit) if non-zero,
// otherwise an estimate from token pricing.
func SessionCost(s *model.Session) (cost float64, estimated bool) {
	if s.CreditCost > 0 || s.ACUCost > 0 {
		return s.CreditCost + s.ACUCost, false
	}
	p := model.LookupPricing(s.EffectiveModel())
	return model.EstimateCost(p, s.InputTokens, s.OutputTokens, s.CacheRead, s.CacheWrite), p.Free || p.InputPerM == 0 && p.OutputPerM == 0
}

// ---- Sessions report ----

// SessionModelStat is per-model usage inside one session (fusion/multi-model).
type SessionModelStat struct {
	Name       string `json:"name"`
	Requests   int    `json:"requests"`
	InputTok   int64  `json:"inputTokens"`
	OutputTok  int64  `json:"outputTokens"`
	CacheRead  int64  `json:"cacheRead"`
	CacheWrite int64  `json:"cacheWrite"`
}

// DayModelStat is usage for one model on one calendar day (local time).
// Date-range filters use these instead of whole-session totals.
type DayModelStat struct {
	Date       string `json:"date"`
	Name       string `json:"name"`
	Requests   int    `json:"requests"`
	InputTok   int64  `json:"inputTokens"`
	OutputTok  int64  `json:"outputTokens"`
	CacheRead  int64  `json:"cacheRead"`
	CacheWrite int64  `json:"cacheWrite"`
}

type SessionRow struct {
	ID           string
	Source       string
	Title        string
	Model        string
	Models       []string // all models used (fusion sessions have >1)
	ModelStats   []SessionModelStat
	DayStats     []DayModelStat
	Mode         string
	Project      string
	Requests     int
	InputTok     int64
	OutputTok    int64
	CacheRead    int64
	Duration     time.Duration
	LastActivity time.Time
	Cost         float64
	CostEstimated bool
	IsFree       bool
	ToolCalls    map[string]int
	SubAgents    int // workflow sub-agent count (0 if standalone)
}

// BuildSessionRows converts sessions to display rows, sorted newest first.
func BuildSessionRows(ss []model.Session) []SessionRow {
	rows := make([]SessionRow, 0, len(ss))
	for _, s := range ss {
		dur := s.LastActivityAt.Sub(s.CreatedAt)
		cost, est := SessionCost(&s)
		displayModel := s.EffectiveModel()
		models := s.ModelsUsed
		if len(models) == 0 && displayModel != "" {
			models = []string{displayModel}
		}
		p := model.LookupPricing(displayModel)
		rows = append(rows, SessionRow{
			ID:            s.DisplayID(),
			Source:        s.Source,
			Title:         s.Title,
			Model:         displayModel,
			Models:        models,
			ModelStats:    buildSessionModelStats(&s, displayModel),
			DayStats:      buildSessionDayStats(&s),
			Mode:          s.AgentMode,
			Project:       baseProject(s.WorkingDir),
			Requests:      s.AssistantCount,
			InputTok:      s.InputTokens,
			OutputTok:     s.OutputTokens,
			CacheRead:     s.CacheRead,
			Duration:      dur,
			LastActivity:  s.LastActivityAt,
			Cost:          cost,
			CostEstimated: est,
			IsFree:        p.Free,
			ToolCalls:     s.ToolCalls,
			SubAgents:     len(s.SubAgentCalls),
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].Requests > rows[j].Requests // by activity for now
	})
	return rows
}

func buildSessionModelStats(s *model.Session, fallback string) []SessionModelStat {
	by := map[string]*SessionModelStat{}
	var order []string
	for _, m := range s.Messages {
		if m.Role != "assistant" || m.GenerationModel == "" {
			continue
		}
		st := by[m.GenerationModel]
		if st == nil {
			st = &SessionModelStat{Name: m.GenerationModel}
			by[m.GenerationModel] = st
			order = append(order, m.GenerationModel)
		}
		st.Requests++
		if m.Metrics != nil {
			st.InputTok += m.Metrics.InputTokens
			st.OutputTok += m.Metrics.OutputTokens
			st.CacheRead += m.Metrics.CacheReadTokens
			st.CacheWrite += m.Metrics.CacheWriteTokens
		}
	}
	if len(order) == 0 {
		if fallback == "" {
			return nil
		}
		return []SessionModelStat{{
			Name:       fallback,
			Requests:   s.AssistantCount,
			InputTok:   s.InputTokens,
			OutputTok:  s.OutputTokens,
			CacheRead:  s.CacheRead,
			CacheWrite: s.CacheWrite,
		}}
	}
	out := make([]SessionModelStat, 0, len(order))
	for _, n := range order {
		out = append(out, *by[n])
	}
	return out
}

func buildSessionDayStats(s *model.Session) []DayModelStat {
	type key struct{ day, model string }
	by := map[key]*DayModelStat{}
	var order []key
	for _, m := range s.Messages {
		if m.Role != "assistant" || m.Metrics == nil {
			continue
		}
		day := m.CreatedAt.Local().Format("2006-01-02")
		name := m.GenerationModel
		if name == "" {
			name = s.EffectiveModel()
		}
		if name == "" {
			name = "unknown"
		}
		k := key{day, name}
		st := by[k]
		if st == nil {
			st = &DayModelStat{Date: day, Name: name}
			by[k] = st
			order = append(order, k)
		}
		st.Requests++
		st.InputTok += m.Metrics.InputTokens
		st.OutputTok += m.Metrics.OutputTokens
		st.CacheRead += m.Metrics.CacheReadTokens
		st.CacheWrite += m.Metrics.CacheWriteTokens
	}
	if len(order) == 0 {
		return nil
	}
	// Stable order: date asc, then model name.
	sort.Slice(order, func(i, j int) bool {
		if order[i].day != order[j].day {
			return order[i].day < order[j].day
		}
		return order[i].model < order[j].model
	})
	out := make([]DayModelStat, 0, len(order))
	for _, k := range order {
		out = append(out, *by[k])
	}
	return out
}

func baseProject(dir string) string {
	if dir == "" {
		return "-"
	}
	// Use last path component.
	dir = strings.TrimRight(dir, "/")
	if i := strings.LastIndex(dir, "/"); i >= 0 {
		return dir[i+1:]
	}
	return dir
}

// ---- Projects report ----

// ProjectRow contains per-project aggregated stats.
type ProjectRow struct {
	Name       string
	Sessions   int
	Requests   int
	InputTok   int64
	OutputTok  int64
	CacheRead  int64
	CacheWrite int64
	Cost       float64
	IsFree     bool
	Models     []string
}

// BuildProjectRows aggregates stats per project (by working directory).
func BuildProjectRows(ss []model.Session) []ProjectRow {
	byProj := map[string]*ProjectRow{}
	for _, s := range ss {
		proj := baseProject(s.WorkingDir)
		pr := byProj[proj]
		if pr == nil {
			pr = &ProjectRow{Name: proj}
			byProj[proj] = pr
		}
		pr.Sessions++
		pr.Requests += s.AssistantCount
		pr.InputTok += s.InputTokens
		pr.OutputTok += s.OutputTokens
		pr.CacheRead += s.CacheRead
		pr.CacheWrite += s.CacheWrite
		cost, _ := SessionCost(&s)
		pr.Cost += cost
		if s.CreditCost > 0 || s.ACUCost > 0 {
			pr.IsFree = false
		}
		// Collect unique models.
		for _, m := range s.Messages {
			if m.Role != "assistant" || m.GenerationModel == "" {
				continue
			}
			if !contains(pr.Models, m.GenerationModel) {
				pr.Models = append(pr.Models, m.GenerationModel)
			}
		}
	}
	rows := make([]ProjectRow, 0, len(byProj))
	for _, pr := range byProj {
		rows = append(rows, *pr)
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].Requests > rows[j].Requests
	})
	return rows
}

// ---- Agents report ----

// AgentStats holds subagent usage statistics per profile.
type AgentStats struct {
	Profile       string
	Calls         int
	Sessions      int   // number of distinct sessions using this profile
	Background    int   // background calls
	Foreground    int   // foreground calls
	ReadCalls     int   // read_subagent calls (main agent waiting for subagent)
	Completed     int   // calls with a completion notification
	Durations     []time.Duration // durations for completed calls
	AvgDuration   time.Duration
	MaxDuration   time.Duration
	TaskLens      []int // task description lengths
	AvgTaskLen    int
	MaxTaskLen    int
	OutputLens    []int // completion notification content lengths
	AvgOutputLen  int
	MaxOutputLen  int
	SessionIDs    map[string]bool
}

// BuildAgentStats aggregates subagent usage across all sessions, grouped by profile.
func BuildAgentStats(ss []model.Session) []AgentStats {
	byProfile := map[string]*AgentStats{}
	for _, s := range ss {
		for _, sa := range s.SubAgentCalls {
			prof := sa.Profile
			if prof == "" {
				prof = "unknown"
			}
			st := byProfile[prof]
			if st == nil {
				st = &AgentStats{Profile: prof, SessionIDs: map[string]bool{}}
				byProfile[prof] = st
			}
			st.Calls++
			if sa.IsBackground {
				st.Background++
			} else {
				st.Foreground++
			}
			if sa.HasCompletion {
				st.Completed++
				d := sa.EndTime.Sub(sa.StartTime)
				if d > 0 {
					st.Durations = append(st.Durations, d)
				}
				if sa.OutputLen > 0 {
					st.OutputLens = append(st.OutputLens, sa.OutputLen)
				}
			}
			if len(sa.Task) > 0 {
				st.TaskLens = append(st.TaskLens, len(sa.Task))
			}
			if !st.SessionIDs[s.QualifiedID()] {
				st.SessionIDs[s.QualifiedID()] = true
				st.Sessions++
			}
		}
		// Count read_subagent calls per profile is not possible (read_subagent
		// args only have agent_id, not profile). So we distribute read_subagent
		// calls as a session-level metric, not per-profile. We'll track it
		// separately.
	}
	// Aggregate read_subagent calls across all sessions.
	// Since read_subagent doesn't specify a profile, we attribute them to
	// the profile that was used in that session. If a session has multiple
	// profiles, we can't attribute precisely, so we count per-session.
	// For simplicity, we count total read_subagent calls and distribute
	// them proportionally. But actually, it's more useful to show per-profile.
	// Let's count read_subagent per session and attribute to the profile
	// that has the most calls in that session.
	for _, s := range ss {
		if s.ReadSubAgentCalls == 0 {
			continue
		}
		// Find the dominant profile in this session.
		profCalls := map[string]int{}
		for _, sa := range s.SubAgentCalls {
			prof := sa.Profile
			if prof == "" {
				prof = "unknown"
			}
			profCalls[prof]++
		}
		var domProf string
		var domCount int
		for p, c := range profCalls {
			if c > domCount {
				domCount = c
				domProf = p
			}
		}
		if domProf != "" && byProfile[domProf] != nil {
			byProfile[domProf].ReadCalls += s.ReadSubAgentCalls
		}
	}
	// Compute averages.
	for _, st := range byProfile {
		if len(st.Durations) > 0 {
			var sum time.Duration
			for _, d := range st.Durations {
				sum += d
				if d > st.MaxDuration {
					st.MaxDuration = d
				}
			}
			st.AvgDuration = sum / time.Duration(len(st.Durations))
		}
		if len(st.TaskLens) > 0 {
			var sum int
			for _, l := range st.TaskLens {
				sum += l
				if l > st.MaxTaskLen {
					st.MaxTaskLen = l
				}
			}
			st.AvgTaskLen = sum / len(st.TaskLens)
		}
		if len(st.OutputLens) > 0 {
			var sum int
			for _, l := range st.OutputLens {
				sum += l
				if l > st.MaxOutputLen {
					st.MaxOutputLen = l
				}
			}
			st.AvgOutputLen = sum / len(st.OutputLens)
		}
	}
	rows := make([]AgentStats, 0, len(byProfile))
	for _, st := range byProfile {
		rows = append(rows, *st)
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].Calls > rows[j].Calls
	})
	return rows
}

// ---- Models report ----

type ModelRow struct {
	Name         string
	Requests     int
	Sessions     int
	InputTok     int64
	OutputTok    int64
	CacheRead    int64
	CacheWrite   int64
	CreditCost   float64
	ACUCost      float64
	EstCost      float64
	IsFree       bool
	TTFTP50      float64
	TTFTP95      float64
	TotalP50     float64
	TotalP95     float64
	TokPerSecP50 float64
	TruncPct     float64 // % of finish_reason == "length"
	CostPct      float64 // % of total cost across all models
}

// BuildModelRows aggregates per-model stats across all sessions.
func BuildModelRows(ss []model.Session) []ModelRow {
	byModel := map[string]*model.ModelStats{}
	modelSessions := map[string]map[string]bool{} // model -> set of session IDs
	for _, s := range ss {
		for _, m := range s.Messages {
			if m.Role != "assistant" || m.GenerationModel == "" {
				continue
			}
			ms := byModel[m.GenerationModel]
			if ms == nil {
				ms = &model.ModelStats{Name: m.GenerationModel, FinishReasons: map[string]int{}}
				byModel[m.GenerationModel] = ms
				modelSessions[m.GenerationModel] = map[string]bool{}
			}
			ms.Requests++
			if m.Metrics != nil {
				ms.InputTokens += m.Metrics.InputTokens
				ms.OutputTokens += m.Metrics.OutputTokens
				ms.CacheRead += m.Metrics.CacheReadTokens
				ms.CacheWrite += m.Metrics.CacheWriteTokens
				if m.Metrics.TTFTMs > 0 {
					ms.TTFTs = append(ms.TTFTs, m.Metrics.TTFTMs)
				}
				if m.Metrics.TotalTimeMs > 0 {
					ms.TotalTimes = append(ms.TotalTimes, m.Metrics.TotalTimeMs)
				}
				if m.Metrics.TokensPerSec > 0 {
					ms.TokensPerSec = append(ms.TokensPerSec, m.Metrics.TokensPerSec)
				}
			}
			if m.FinishReason != "" {
				ms.FinishReasons[m.FinishReason]++
			}
			if !modelSessions[m.GenerationModel][s.QualifiedID()] {
				modelSessions[m.GenerationModel][s.QualifiedID()] = true
			}
		}
		// Credit cost attribution: attribute session credit to its model.
		if s.CreditCost > 0 || s.ACUCost > 0 {
			if ms, ok := byModel[s.Model]; ok {
				ms.CreditCost += s.CreditCost
				ms.ACUCost += s.ACUCost
			}
		}
	}

	// Compute total cost across all models for cost% calculation.
	totalCostAll := 0.0
	for _, ms := range byModel {
		p := model.LookupPricing(ms.Name)
		cost := ms.CreditCost + ms.ACUCost
		if cost == 0 {
			cost = model.EstimateCost(p, ms.InputTokens, ms.OutputTokens, ms.CacheRead, ms.CacheWrite)
		}
		totalCostAll += cost
	}

	rows := make([]ModelRow, 0, len(byModel))
	for _, ms := range byModel {
		p := model.LookupPricing(ms.Name)
		est := model.EstimateCost(p, ms.InputTokens, ms.OutputTokens, ms.CacheRead, ms.CacheWrite)
		trunc := 0
		total := 0
		for r, c := range ms.FinishReasons {
			total += c
			if r == "length" {
				trunc += c
			}
		}
		truncPct := 0.0
		if total > 0 {
			truncPct = float64(trunc) / float64(total) * 100
		}
		// Cost% based on actual cost (credit or estimate).
		actualCost := ms.CreditCost + ms.ACUCost
		if actualCost == 0 {
			actualCost = est
		}
		costPct := 0.0
		if totalCostAll > 0 {
			costPct = actualCost / totalCostAll * 100
		}
		rows = append(rows, ModelRow{
			Name:         ms.Name,
			Requests:     ms.Requests,
			Sessions:     len(modelSessions[ms.Name]),
			InputTok:     ms.InputTokens,
			OutputTok:    ms.OutputTokens,
			CacheRead:    ms.CacheRead,
			CacheWrite:   ms.CacheWrite,
			CreditCost:   ms.CreditCost,
			ACUCost:      ms.ACUCost,
			EstCost:      est,
			IsFree:       p.Free,
			TTFTP50:      model.Percentile(ms.TTFTs, 50),
			TTFTP95:      model.Percentile(ms.TTFTs, 95),
			TotalP50:     model.Percentile(ms.TotalTimes, 50),
			TotalP95:     model.Percentile(ms.TotalTimes, 95),
			TokPerSecP50: model.Percentile(ms.TokensPerSec, 50),
			TruncPct:     truncPct,
			CostPct:      costPct,
		})
	}
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].InputTok > rows[j].InputTok
	})
	return rows
}

// ---- Model detail ----

// ModelDetail contains detailed stats for a single model.
type ModelDetail struct {
	Name         string
	FirstUsed   time.Time
	LastUsed    time.Time
	Sessions    int
	DaysUsed    int
	Requests    int
	InputTok    int64
	OutputTok   int64
	CacheRead   int64
	CacheWrite  int64
	CreditCost  float64
	ACUCost     float64
	EstCost     float64
	IsFree      bool
	TTFTP50     float64
	TTFTP95     float64
	TotalP50    float64
	TotalP95    float64
	TokPerSecP50 float64
	TruncPct    float64
	Tools       []ToolUsageRow
}

// ToolUsageRow contains per-tool usage stats.
type ToolUsageRow struct {
	Name        string
	Calls       int
	Success     int
	Failed      int
	SuccessRate float64
}

// BuildModelDetail aggregates detailed stats for a single model (fuzzy match).
func BuildModelDetail(ss []model.Session, query string) (*ModelDetail, error) {
	// Find matching model name (case-insensitive substring).
	query = strings.ToLower(query)
	var matchedName string
	byModel := map[string]*model.ModelStats{}
	for _, s := range ss {
		for _, m := range s.Messages {
			if m.Role != "assistant" || m.GenerationModel == "" {
				continue
			}
			ms := byModel[m.GenerationModel]
			if ms == nil {
				ms = &model.ModelStats{Name: m.GenerationModel, FinishReasons: map[string]int{}}
				byModel[m.GenerationModel] = ms
				if strings.Contains(strings.ToLower(m.GenerationModel), query) {
					if matchedName == "" || len(m.GenerationModel) < len(matchedName) {
						matchedName = m.GenerationModel
					}
				}
			}
			// Aggregate stats.
			ms.Requests++
			if m.Metrics != nil {
				ms.InputTokens += m.Metrics.InputTokens
				ms.OutputTokens += m.Metrics.OutputTokens
				ms.CacheRead += m.Metrics.CacheReadTokens
				ms.CacheWrite += m.Metrics.CacheWriteTokens
				if m.Metrics.TTFTMs > 0 {
					ms.TTFTs = append(ms.TTFTs, m.Metrics.TTFTMs)
				}
				if m.Metrics.TotalTimeMs > 0 {
					ms.TotalTimes = append(ms.TotalTimes, m.Metrics.TotalTimeMs)
				}
				if m.Metrics.TokensPerSec > 0 {
					ms.TokensPerSec = append(ms.TokensPerSec, m.Metrics.TokensPerSec)
				}
			}
			if m.FinishReason != "" {
				ms.FinishReasons[m.FinishReason]++
			}
		}
		// Credit cost attribution.
		if s.CreditCost > 0 || s.ACUCost > 0 {
			if ms, ok := byModel[s.Model]; ok {
				ms.CreditCost += s.CreditCost
				ms.ACUCost += s.ACUCost
			}
		}
	}
	if matchedName == "" {
		return nil, fmt.Errorf("no model matching %q found", query)
	}

	ms := byModel[matchedName]
	p := model.LookupPricing(matchedName)
	est := model.EstimateCost(p, ms.InputTokens, ms.OutputTokens, ms.CacheRead, ms.CacheWrite)

	// Collect sessions and time range for this model.
	sessionIDs := map[string]bool{}
	var firstUsed, lastUsed time.Time
	daysUsed := map[string]bool{}
	for _, s := range ss {
		for _, m := range s.Messages {
			if m.Role != "assistant" || m.GenerationModel != matchedName {
				continue
			}
			sessionIDs[s.QualifiedID()] = true
			if firstUsed.IsZero() || m.CreatedAt.Before(firstUsed) {
				firstUsed = m.CreatedAt
			}
			if m.CreatedAt.After(lastUsed) {
				lastUsed = m.CreatedAt
			}
			daysUsed[m.CreatedAt.Format("2006-01-02")] = true
		}
	}

	// Truncation percentage.
	trunc, total := 0, 0
	for r, c := range ms.FinishReasons {
		total += c
		if r == "length" {
			trunc += c
		}
	}
	truncPct := 0.0
	if total > 0 {
		truncPct = float64(trunc) / float64(total) * 100
	}

	// Tool usage for this model.
	// Note: Devin's ToolCall doesn't track success/failure, so we only
	// report call counts. The SuccessRate column is shown as "-" when
	// success tracking is unavailable.
	toolStats := map[string]*ToolUsageRow{}
	for _, s := range ss {
		for _, m := range s.Messages {
			if m.Role != "assistant" || m.GenerationModel != matchedName {
				continue
			}
			for _, tc := range m.ToolCalls {
				name := tc.Name
				if name == "" {
					continue
				}
				tr := toolStats[name]
				if tr == nil {
					tr = &ToolUsageRow{Name: name}
					toolStats[name] = tr
				}
				tr.Calls++
			}
		}
	}
	tools := make([]ToolUsageRow, 0, len(toolStats))
	for _, tr := range toolStats {
		if tr.Calls > 0 {
			tr.SuccessRate = float64(tr.Success) / float64(tr.Calls) * 100
		}
		tools = append(tools, *tr)
	}
	sort.Slice(tools, func(i, j int) bool {
		return tools[i].Calls > tools[j].Calls
	})

	return &ModelDetail{
		Name:          matchedName,
		FirstUsed:     firstUsed,
		LastUsed:      lastUsed,
		Sessions:      len(sessionIDs),
		DaysUsed:      len(daysUsed),
		Requests:      ms.Requests,
		InputTok:      ms.InputTokens,
		OutputTok:     ms.OutputTokens,
		CacheRead:     ms.CacheRead,
		CacheWrite:    ms.CacheWrite,
		CreditCost:    ms.CreditCost,
		ACUCost:       ms.ACUCost,
		EstCost:       est,
		IsFree:        p.Free,
		TTFTP50:       model.Percentile(ms.TTFTs, 50),
		TTFTP95:       model.Percentile(ms.TTFTs, 95),
		TotalP50:      model.Percentile(ms.TotalTimes, 50),
		TotalP95:      model.Percentile(ms.TotalTimes, 95),
		TokPerSecP50:  model.Percentile(ms.TokensPerSec, 50),
		TruncPct:      truncPct,
		Tools:         tools,
	}, nil
}

// ---- Time series (daily / weekly) ----

type TimeRow struct {
	Label       string
	Requests    int
	Sessions    int
	SubAgents   int
	InputTok    int64
	OutputTok   int64
	CacheRead   int64
	CacheWrite  int64
	Cost        float64
	CostEstimated bool
	Models      []string
	ByModel     map[string]*model.ModelStats
}

// BuildDaily aggregates by calendar day (local time).
func BuildDaily(ss []model.Session) []TimeRow {
	return buildTimeBuckets(ss, func(t time.Time) string {
		return t.Format("2006-01-02")
	})
}

// BuildWeekly aggregates by week, with configurable start day.
// startDay: 0=Sunday, 1=Monday, ..., 6=Saturday.
func BuildWeekly(ss []model.Session, startDay time.Weekday) []TimeRow {
	return buildTimeBuckets(ss, func(t time.Time) string {
		// Snap to week start.
		days := (int(t.Weekday()) - int(startDay) + 7) % 7
		weekStart := t.AddDate(0, 0, -days)
		return weekStart.Format("2006-01-02")
	})
}

// BuildMonthly aggregates by calendar month.
func BuildMonthly(ss []model.Session) []TimeRow {
	return buildTimeBuckets(ss, func(t time.Time) string {
		return t.Format("2006-01")
	})
}

// WeekLabel converts a week-start date string (2006-01-02) to a "2006-W01" label.
func WeekLabel(weekStartStr string) string {
	t, err := time.Parse("2006-01-02", weekStartStr)
	if err != nil {
		return weekStartStr
	}
	_, week := t.ISOWeek()
	return fmt.Sprintf("%d-W%02d", t.Year(), week)
}

// WeekDateRange returns a human-readable date range for a week-start date string.
func WeekDateRange(weekStartStr string) string {
	t, err := time.Parse("2006-01-02", weekStartStr)
	if err != nil {
		return weekStartStr
	}
	end := t.AddDate(0, 0, 6)
	return fmt.Sprintf("%s ~ %s", t.Format("01-02"), end.Format("01-02"))
}

func buildTimeBuckets(ss []model.Session, key func(time.Time) string) []TimeRow {
	buckets := map[string]*TimeRow{}
	// Track which sessions have been counted per bucket to avoid double-counting.
	sessionSeen := map[string]map[string]bool{}
	for _, s := range ss {
		for _, m := range s.Messages {
			if m.Role != "assistant" || m.Metrics == nil {
				continue
			}
			k := key(m.CreatedAt)
			b := buckets[k]
			if b == nil {
				b = &TimeRow{Label: k, ByModel: map[string]*model.ModelStats{}}
				buckets[k] = b
				sessionSeen[k] = map[string]bool{}
			}
			b.Requests++
			b.InputTok += m.Metrics.InputTokens
			b.OutputTok += m.Metrics.OutputTokens
			b.CacheRead += m.Metrics.CacheReadTokens
			b.CacheWrite += m.Metrics.CacheWriteTokens
			mn := m.GenerationModel
			if mn == "" {
				mn = s.Model
			}
			if b.ByModel[mn] == nil {
				b.ByModel[mn] = &model.ModelStats{Name: mn, FinishReasons: map[string]int{}}
			}
			bm := b.ByModel[mn]
			bm.Requests++
			bm.InputTokens += m.Metrics.InputTokens
			bm.OutputTokens += m.Metrics.OutputTokens
			bm.CacheRead += m.Metrics.CacheReadTokens
			bm.CacheWrite += m.Metrics.CacheWriteTokens
			if !contains(b.Models, mn) {
				b.Models = append(b.Models, mn)
			}
			// Count unique sessions per bucket.
			if !sessionSeen[k][s.QualifiedID()] {
				sessionSeen[k][s.QualifiedID()] = true
				b.Sessions++
			}
		}
		// Credit cost and subagent attribution to the day of session activity.
		if s.CreditCost > 0 || s.ACUCost > 0 || len(s.SubAgentCalls) > 0 {
			k := key(s.LastActivityAt)
			b := buckets[k]
			if b == nil {
				b = &TimeRow{Label: k, ByModel: map[string]*model.ModelStats{}}
				buckets[k] = b
				sessionSeen[k] = map[string]bool{}
			}
			b.Cost += s.CreditCost + s.ACUCost
			b.SubAgents += len(s.SubAgentCalls)
		}
	}

	// Fill estimated cost for buckets with zero credit cost.
	for _, b := range buckets {
		if b.Cost == 0 {
			var est float64
			for mn, ms := range b.ByModel {
				p := model.LookupPricing(mn)
				est += model.EstimateCost(p, ms.InputTokens, ms.OutputTokens, ms.CacheRead, ms.CacheWrite)
			}
			b.Cost = est
			b.CostEstimated = true
		}
	}

	out := make([]TimeRow, 0, len(buckets))
	for _, b := range buckets {
		out = append(out, *b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// ParseWeekday converts "monday".."sunday" to time.Weekday. Default Monday.
func ParseWeekday(s string) time.Weekday {
	days := map[string]time.Weekday{
		"sunday": time.Sunday, "monday": time.Monday, "tuesday": time.Tuesday,
		"wednesday": time.Wednesday, "thursday": time.Thursday,
		"friday": time.Friday, "saturday": time.Saturday,
	}
	if d, ok := days[strings.ToLower(s)]; ok {
		return d
	}
	return time.Monday
}
