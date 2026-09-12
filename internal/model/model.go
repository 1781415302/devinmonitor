// Package model defines the normalized data structures used across devinmonitor.
//
// These types are independent of Devin CLI's internal SQLite schema so that
// the reader layer can adapt to schema changes without touching reports/UI.
package model

import (
	"strings"
	"time"
)

// Session is a normalized Devin CLI session.
type Session struct {
	ID              string
	WorkingDir      string
	BackendType     string
	Model           string
	AgentMode       string // normal / plan / bypass
	CreatedAt       time.Time
	LastActivityAt  time.Time
	Title           string
	MainChainID     int
	Hidden          bool
	WorkspaceDirs   []string
	// Cost from Devin's own accounting (authoritative when non-zero).
	CreditCost float64
	ACUCost    float64
	// Aggregated from assistant messages.
	Messages    []Message
	InputTokens int64
	OutputTokens int64
	CacheRead   int64
	CacheWrite  int64
	ToolCalls   map[string]int // tool name -> count
	AssistantCount int         // number of assistant turns (= requests)
	// LatestModel is the generation_model from the most recent assistant
	// message. More accurate than the session-level Model field (which is
	// set at creation time and doesn't update when the user switches models).
	LatestModel string
	// SubAgentCalls contains all run_subagent invocations in this session.
	SubAgentCalls []SubAgentCall
	// ReadSubAgentCalls counts how many times the main agent called read_subagent
	// (explicitly waiting for a background subagent to finish).
	ReadSubAgentCalls int
	// Source identifies which Devin data store this session came from:
	// "local", "wsl:<distro>", or empty for single-source mode.
	Source string
}

// QualifiedID returns Source + "/" + ID when Source is set, else ID.
// Used to disambiguate colliding short IDs across Windows and WSL stores.
func (s Session) QualifiedID() string {
	if s.Source == "" {
		return s.ID
	}
	return s.Source + "/" + s.ID
}

// DisplayID is the short label for tables: bare ID unless the session
// carries a non-local source tag.
func (s Session) DisplayID() string {
	if s.Source == "" || s.Source == "local" {
		return s.ID
	}
	// wsl:Ubuntu-18.04 -> wsl
	if i := strings.Index(s.Source, ":"); i > 0 {
		return s.Source[:i] + ":" + s.ID
	}
	return s.Source + ":" + s.ID
}

// Message is a single chat message node.
type Message struct {
	NodeID    int
	Role      string // system / user / assistant / tool
	Content   string
	CreatedAt time.Time
	ToolCallID string // for role=tool messages: the tool_call_id this result belongs to
	// Assistant-only fields (zero for other roles).
	Metrics    *Metrics
	FinishReason string
	GenerationModel string
	RequestID  string
	ToolCalls  []ToolCall
	NumTokensPreceding int // context size at this point (if available)
}

// Metrics holds per-request performance/usage data from assistant messages.
type Metrics struct {
	TTFTMs           float64 // time to first token
	TotalTimeMs      float64
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	TokensPerSec     float64
}

// ToolCall is a single tool invocation extracted from an assistant message.
type ToolCall struct {
	ID   string
	Name string
	// Arguments kept as raw JSON string; readers don't parse it.
	Arguments string
}

// SubAgentCall is a parsed run_subagent invocation.
type SubAgentCall struct {
	Title        string    // task title
	Profile      string    // subagent_explore / subagent_general / etc.
	IsBackground bool      // whether the subagent runs in the background
	Task         string    // full task description
	AgentID      string    // agent_id from tool result (for background subagents)
	StartTime    time.Time // when the run_subagent tool call was made
	EndTime      time.Time // when completion notification arrived (zero if not found)
	HasCompletion bool     // whether a completion notification was found
	OutputLen    int       // character count of the completion notification content
}

// ModelStats aggregates usage for a single model across sessions.
type ModelStats struct {
	Name         string
	Requests     int
	InputTokens  int64
	OutputTokens int64
	CacheRead    int64
	CacheWrite   int64
	CreditCost   float64
	ACUCost      float64
	// Latency distribution (ms).
	TTFTs        []float64
	TotalTimes   []float64
	TokensPerSec []float64
	// Finish reason counts.
	FinishReasons map[string]int
}

// Percentile returns the p-th percentile (0-100) of xs. Returns 0 if empty.
func Percentile(xs []float64, p float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	// Simple sort-based percentile. xs is copied to avoid mutation.
	sorted := make([]float64, len(xs))
	copy(sorted, xs)
	sortFloats(sorted)
	idx := int(float64(len(sorted)-1) * p / 100.0)
	return sorted[idx]
}

func sortFloats(a []float64) {
	// Insertion sort — slices are small (per-session request counts).
	for i := 1; i < len(a); i++ {
		key := a[i]
		j := i - 1
		for j >= 0 && a[j] > key {
			a[j+1] = a[j]
			j--
		}
		a[j+1] = key
	}
}

// TimeBucket is a daily/weekly/monthly aggregation.
type TimeBucket struct {
	Label       string // date / week / month label
	Requests    int
	InputTokens int64
	OutputTokens int64
	CacheRead   int64
	CreditCost  float64
	ACUCost     float64
	ByModel     map[string]*ModelStats
}

// DayStart returns t truncated to midnight local.
func DayStart(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}
