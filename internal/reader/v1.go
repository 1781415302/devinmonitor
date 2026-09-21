package reader

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/garywhat/devinmonitor/internal/model"
)

// v1Reader adapts the current Devin CLI schema (refinery v1 era).
type v1Reader struct {
	db   *sql.DB
	path string
	ver  int
}

func newV1Reader(path string, ver int) (*v1Reader, error) {
	// Read-only, WAL-safe, query_only to avoid blocking Devin's writes.
	// modernc.org/sqlite supports URI-style options via "file:" prefix.
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	dsn := fmt.Sprintf("file:%s?mode=ro&_journal_mode=WAL&_query_only=1&_busy_timeout=5000", abs)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	// Single connection avoids WAL contention surprises.
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}
	r := &v1Reader{db: db, path: path, ver: ver}
	if ver == 0 {
		r.ver = r.detectVersion()
	}
	return r, nil
}

func (r *v1Reader) detectVersion() int {
	var v int
	// Table may not exist on very old builds; ignore error.
	_ = r.db.QueryRow("SELECT COALESCE(MAX(version),0) FROM refinery_schema_history").Scan(&v)
	return v
}

func (r *v1Reader) SchemaVersion() int { return r.ver }
func (r *v1Reader) DBPath() string     { return r.path }
func (r *v1Reader) Close() error       { return r.db.Close() }

// ---- JSON helper structs (mirror Devin's chat_message shape) ----

type chatMessage struct {
	MessageID  string          `json:"message_id"`
	Role       string          `json:"role"`
	Content    string          `json:"content"`
	ToolCallID string          `json:"tool_call_id"`
	ToolCalls  []rawToolCall   `json:"tool_calls"`
	Thinking   json.RawMessage `json:"thinking"`
	Metadata   *msgMetadata    `json:"metadata"`
}

type rawToolCall struct {
	ID        string                 `json:"id"`
	Name      string                 `json:"name"`
	Arguments map[string]interface{} `json:"arguments"`
	Index     int                    `json:"index"`
	Kind      string                 `json:"kind"`
	// OpenAI-style nested form
	Function *struct {
		Name      string                 `json:"name"`
		Arguments map[string]interface{} `json:"arguments"`
	} `json:"function"`
}

type msgMetadata struct {
	NumTokens         *int     `json:"num_tokens"`
	RequestID         string   `json:"request_id"`
	Metrics           *metrics `json:"metrics"`
	FinishReason      string   `json:"finish_reason"`
	GenerationModel   string   `json:"generation_model"`
	CreatedAt         string   `json:"created_at"`
	NumTokensPreceding *int    `json:"num_tokens_preceding"`
}

type metrics struct {
	TTFTMs           *float64 `json:"ttft_ms"`
	TotalTimeMs      *float64 `json:"total_time_ms"`
	InputTokens      *int64   `json:"input_tokens"`
	OutputTokens     *int64   `json:"output_tokens"`
	CacheReadTokens  *int64   `json:"cache_read_tokens"`
	CacheWriteTokens *int64   `json:"cache_creation_tokens"`
	TokensPerSec     *float64 `json:"tokens_per_sec"`
}

type sessionMetadata struct {
	TotalCreditCost float64 `json:"total_credit_cost"`
	TotalACUCost    float64 `json:"total_acu_cost"`
}

// ---- Sessions ----

func (r *v1Reader) Sessions() ([]model.Session, error) {
	rows, err := r.db.Query(`
		SELECT id, working_directory, backend_type, model, agent_mode,
		       created_at, last_activity_at, COALESCE(title,''), COALESCE(main_chain_id,0),
		       COALESCE(workspace_dirs,''), hidden, COALESCE(metadata,'')
		FROM sessions ORDER BY last_activity_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("query sessions: %w", err)
	}
	defer rows.Close()

	var out []model.Session
	for rows.Next() {
		var s model.Session
		var createdAt, lastAct int64
		var wsDirs, meta string
		if err := rows.Scan(&s.ID, &s.WorkingDir, &s.BackendType, &s.Model, &s.AgentMode,
			&createdAt, &lastAct, &s.Title, &s.MainChainID, &wsDirs, &s.Hidden, &meta); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		s.CreatedAt = time.Unix(createdAt, 0)
		s.LastActivityAt = time.Unix(lastAct, 0)
		s.WorkspaceDirs = parseStringArray(wsDirs)
		var sm sessionMetadata
		if meta != "" {
			_ = json.Unmarshal([]byte(meta), &sm)
		}
		s.CreditCost = sm.TotalCreditCost
		s.ACUCost = sm.TotalACUCost
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Load messages for each session.
	for i := range out {
		msgs, err := r.loadMessages(out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Messages = msgs
		aggregate(&out[i])
	}
	return out, nil
}

func (r *v1Reader) Session(id string) (*model.Session, error) {
	var s model.Session
	var createdAt, lastAct int64
	var wsDirs, meta string
	err := r.db.QueryRow(`
		SELECT id, working_directory, backend_type, model, agent_mode,
		       created_at, last_activity_at, COALESCE(title,''), COALESCE(main_chain_id,0),
		       COALESCE(workspace_dirs,''), hidden, COALESCE(metadata,'')
		FROM sessions WHERE id = ?`, id).
		Scan(&s.ID, &s.WorkingDir, &s.BackendType, &s.Model, &s.AgentMode,
			&createdAt, &lastAct, &s.Title, &s.MainChainID, &wsDirs, &s.Hidden, &meta)
	if err != nil {
		return nil, fmt.Errorf("query session %s: %w", id, err)
	}
	s.CreatedAt = time.Unix(createdAt, 0)
	s.LastActivityAt = time.Unix(lastAct, 0)
	s.WorkspaceDirs = parseStringArray(wsDirs)
	var sm sessionMetadata
	if meta != "" {
		_ = json.Unmarshal([]byte(meta), &sm)
	}
	s.CreditCost = sm.TotalCreditCost
	s.ACUCost = sm.TotalACUCost

	msgs, err := r.loadMessages(id)
	if err != nil {
		return nil, err
	}
	s.Messages = msgs
	aggregate(&s)
	return &s, nil
}

func (r *v1Reader) loadMessages(sessionID string) ([]model.Message, error) {
	rows, err := r.db.Query(`
		SELECT node_id, chat_message, created_at
		FROM message_nodes WHERE session_id = ? ORDER BY node_id ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("query messages: %w", err)
	}
	defer rows.Close()

	var out []model.Message
	for rows.Next() {
		var m model.Message
		var raw string
		var createdAt int64
		if err := rows.Scan(&m.NodeID, &raw, &createdAt); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		// created_at in message_nodes is nanoseconds since epoch (Rust SystemTime).
		m.CreatedAt = time.Unix(createdAt, 0)

		var cm chatMessage
		if err := json.Unmarshal([]byte(raw), &cm); err != nil {
			continue
		}
		m.Role = cm.Role
		m.Content = cm.Content
		m.ToolCallID = cm.ToolCallID
		if cm.Metadata != nil {
			m.RequestID = cm.Metadata.RequestID
			m.FinishReason = cm.Metadata.FinishReason
			m.GenerationModel = cm.Metadata.GenerationModel
			if cm.Metadata.NumTokensPreceding != nil {
				m.NumTokensPreceding = *cm.Metadata.NumTokensPreceding
			}
			if cm.Metadata.Metrics != nil {
				m.Metrics = decodeMetrics(cm.Metadata.Metrics)
			}
		}
		// Aggregation never reads assistant message bodies; drop them early
		// so large transcripts don't accumulate in memory.
		if m.Role == "assistant" {
			m.Content = ""
		}
		for _, tc := range cm.ToolCalls {
			name := tc.Name
			args := tc.Arguments
			if name == "" && tc.Function != nil {
				name = tc.Function.Name
				args = tc.Function.Arguments
			}
			if name == "" {
				continue
			}
			argBytes, _ := json.Marshal(args)
			m.ToolCalls = append(m.ToolCalls, model.ToolCall{ID: tc.ID, Name: name, Arguments: string(argBytes)})
		}
		out = append(out, m)
	}
	return dedupeAssistantByRequestID(out), rows.Err()
}

// dedupeAssistantByRequestID collapses Devin's streaming+final duplicate
// assistant rows so token metrics are not double-counted. Keeps the last
// copy (final response) when request_id matches.
func dedupeAssistantByRequestID(msgs []model.Message) []model.Message {
	if len(msgs) == 0 {
		return msgs
	}
	last := map[string]int{}
	keep := make([]bool, len(msgs))
	for i := range msgs {
		if msgs[i].Role != "assistant" || msgs[i].RequestID == "" {
			keep[i] = true
			continue
		}
		if j, ok := last[msgs[i].RequestID]; ok {
			keep[j] = false
		}
		last[msgs[i].RequestID] = i
		keep[i] = true
	}
	out := make([]model.Message, 0, len(msgs))
	for i := range msgs {
		if keep[i] {
			out = append(out, msgs[i])
		}
	}
	return out
}

func decodeMetrics(m *metrics) *model.Metrics {
	out := &model.Metrics{}
	if m.TTFTMs != nil {
		out.TTFTMs = *m.TTFTMs
	}
	if m.TotalTimeMs != nil {
		out.TotalTimeMs = *m.TotalTimeMs
	}
	if m.InputTokens != nil {
		out.InputTokens = *m.InputTokens
	}
	if m.OutputTokens != nil {
		out.OutputTokens = *m.OutputTokens
	}
	if m.CacheReadTokens != nil {
		out.CacheReadTokens = *m.CacheReadTokens
	}
	if m.CacheWriteTokens != nil {
		out.CacheWriteTokens = *m.CacheWriteTokens
	}
	if m.TokensPerSec != nil {
		out.TokensPerSec = *m.TokensPerSec
	}
	return out
}

// aggregate fills session-level totals from messages.
func aggregate(s *model.Session) {
	if s.ToolCalls == nil {
		s.ToolCalls = map[string]int{}
	}

	// First pass: collect agent_id from tool result messages and
	// completion notifications from system messages.
	type completionInfo struct {
		endTime   time.Time
		outputLen int
	}
	completions := map[string]completionInfo{} // agent_id → completion
	toolCallIDToAgentID := map[string]string{}

	for i := range s.Messages {
		m := &s.Messages[i]
		// Tool result messages contain "Background subagent started with agent_id=XXX".
		if m.Role == "tool" {
			if aid := extractAgentID(m.Content); aid != "" && m.ToolCallID != "" {
				toolCallIDToAgentID[m.ToolCallID] = aid
			}
			// Body already consumed; free it before the assistant pass.
			m.Content = ""
		}
		// System messages may contain <subagent_completion_notification>.
		if m.Role == "system" && strings.Contains(m.Content, "<subagent_completion_notification>") {
			if aid := extractAgentID(m.Content); aid != "" {
				completions[aid] = completionInfo{
					endTime:   m.CreatedAt,
					outputLen: len(m.Content),
				}
			}
			m.Content = ""
		}
	}

	// Second pass: aggregate assistant messages.
	// Deduplicate run_subagent and read_subagent calls by tool_call_id
	// (Devin stores each assistant message twice: streaming + final).
	seenSubAgent := map[string]bool{}
	seenReadSubAgent := map[string]bool{}
	seenModel := map[string]bool{}
	for _, m := range s.Messages {
		if m.Role != "assistant" {
			continue
		}
		s.AssistantCount++
		if m.GenerationModel != "" && !seenModel[m.GenerationModel] {
			seenModel[m.GenerationModel] = true
			s.ModelsUsed = append(s.ModelsUsed, m.GenerationModel)
		}
		if m.Metrics != nil {
			s.InputTokens += m.Metrics.InputTokens
			s.OutputTokens += m.Metrics.OutputTokens
			s.CacheRead += m.Metrics.CacheReadTokens
			s.CacheWrite += m.Metrics.CacheWriteTokens
		}
		for _, tc := range m.ToolCalls {
			s.ToolCalls[tc.Name]++
			if tc.Name == "run_subagent" && tc.Arguments != "" {
				if tc.ID != "" && seenSubAgent[tc.ID] {
					continue
				}
				if tc.ID != "" {
					seenSubAgent[tc.ID] = true
				}
				sa := parseSubAgentCall(tc.Arguments, m.CreatedAt)
				if sa != nil {
					if aid, ok := toolCallIDToAgentID[tc.ID]; ok {
						sa.AgentID = aid
						if comp, ok := completions[aid]; ok {
							sa.HasCompletion = true
							sa.EndTime = comp.endTime
							sa.OutputLen = comp.outputLen
						}
					}
					s.SubAgentCalls = append(s.SubAgentCalls, *sa)
				}
			}
			if tc.Name == "read_subagent" {
				if tc.ID != "" && seenReadSubAgent[tc.ID] {
					continue
				}
				if tc.ID != "" {
					seenReadSubAgent[tc.ID] = true
				}
				s.ReadSubAgentCalls++
			}
		}
		if m.GenerationModel != "" {
			s.LatestModel = m.GenerationModel
		}
	}
	// Some Devin/WSL records leave sessions.model empty while each request
	// still carries generation_model — backfill so reports/web don't show unknown.
	if s.Model == "" && s.LatestModel != "" {
		s.Model = s.LatestModel
	}
}

// extractAgentID finds an agent_id (hex string) from message content.
var agentIDRe = regexp.MustCompile(`agent_id=([a-f0-9]+)`)

func extractAgentID(content string) string {
	m := agentIDRe.FindStringSubmatch(content)
	if len(m) >= 2 {
		return m[1]
	}
	return ""
}

// parseSubAgentCall extracts subagent metadata from run_subagent arguments JSON.
func parseSubAgentCall(argsJSON string, createdAt time.Time) *model.SubAgentCall {
	var args struct {
		Title        string `json:"title"`
		Profile      string `json:"profile"`
		IsBackground bool   `json:"is_background"`
		Task         string `json:"task"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return nil
	}
	return &model.SubAgentCall{
		Title:        args.Title,
		Profile:      args.Profile,
		IsBackground: args.IsBackground,
		Task:         args.Task,
		StartTime:    createdAt,
	}
}

// parseStringArray handles workspace_dirs which may be JSON array or empty.
func parseStringArray(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var arr []string
	if err := json.Unmarshal([]byte(s), &arr); err == nil {
		return arr
	}
	// Some builds store comma-separated.
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			arr = append(arr, p)
		}
	}
	return arr
}

// SortedToolNames returns tool names sorted by count desc, for stable display.
func SortedToolNames(tc map[string]int) []string {
	type kv struct {
		k string
		v int
	}
	var list []kv
	for k, v := range tc {
		list = append(list, kv{k, v})
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].v != list[j].v {
			return list[i].v > list[j].v
		}
		return list[i].k < list[j].k
	})
	out := make([]string, len(list))
	for i, e := range list {
		out[i] = e.k
	}
	return out
}

// itoa helper to avoid strconv import noise in callers.
func itoa(i int) string { return strconv.Itoa(i) }
var _ = itoa
