package reader

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/garywhat/devinmonitor/internal/model"
)

// mimoReader reads MiMoCode / OpenCode usage. Schema family is the same
// (drizzle): session / message / part. Assistant messages carry
// tokens{input,output,reasoning,cache{read,write}} and cost.
// kind is "mimo" or "opencode" for labels/BackendType.
type mimoReader struct {
	db   *sql.DB
	path string
	kind string // "mimo" | "opencode"
}

// ResolveMiMoDBPath finds mimocode.db across platforms.
func ResolveMiMoDBPath(dataDir string) string {
	var candidates []string
	if dataDir != "" {
		candidates = append(candidates, filepath.Join(dataDir, "mimocode.db"))
	}
	if env := os.Getenv("MIMOCODE_DATA_DIR"); env != "" {
		candidates = append(candidates, filepath.Join(env, "mimocode.db"))
	}
	if env := os.Getenv("MIMO_DATA_DIR"); env != "" {
		candidates = append(candidates, filepath.Join(env, "mimocode.db"))
	}
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "windows":
		if home != "" {
			candidates = append(candidates,
				filepath.Join(home, ".local", "share", "mimocode", "mimocode.db"),
				filepath.Join(home, "AppData", "Roaming", "mimocode", "mimocode.db"),
			)
		}
		if appdata := os.Getenv("APPDATA"); appdata != "" {
			candidates = append(candidates, filepath.Join(appdata, "mimocode", "mimocode.db"))
		}
	case "darwin":
		if home != "" {
			candidates = append(candidates,
				filepath.Join(home, ".local", "share", "mimocode", "mimocode.db"),
				filepath.Join(home, "Library", "Application Support", "mimocode", "mimocode.db"),
			)
		}
	default:
		if home != "" {
			candidates = append(candidates, filepath.Join(home, ".local", "share", "mimocode", "mimocode.db"))
		}
		if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
			candidates = append(candidates, filepath.Join(xdg, "mimocode", "mimocode.db"))
		}
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c
		}
	}
	return ""
}

// ResolveOpenCodeDBPath finds opencode.db across platforms.
func ResolveOpenCodeDBPath(dataDir string) string {
	var candidates []string
	if dataDir != "" {
		candidates = append(candidates, filepath.Join(dataDir, "opencode.db"))
	}
	if env := os.Getenv("OPENCODE_DATA_DIR"); env != "" {
		candidates = append(candidates, filepath.Join(env, "opencode.db"))
	}
	home, _ := os.UserHomeDir()
	if home != "" {
		candidates = append(candidates,
			filepath.Join(home, ".local", "share", "opencode", "opencode.db"),
			filepath.Join(home, ".opencode", "opencode.db"),
		)
	}
	if runtime.GOOS == "windows" {
		if appdata := os.Getenv("APPDATA"); appdata != "" {
			candidates = append(candidates, filepath.Join(appdata, "opencode", "opencode.db"))
		}
	}
	if runtime.GOOS == "darwin" && home != "" {
		candidates = append(candidates,
			filepath.Join(home, "Library", "Application Support", "opencode", "opencode.db"),
		)
	}
	if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
		candidates = append(candidates, filepath.Join(xdg, "opencode", "opencode.db"))
	}
	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c
		}
	}
	return ""
}

// HasMiMoDB reports whether a mimocode.db is discoverable.
func HasMiMoDB(dataDir string) bool {
	return ResolveMiMoDBPath(dataDir) != ""
}

// HasOpenCodeDB reports whether an opencode.db is discoverable.
func HasOpenCodeDB(dataDir string) bool {
	return ResolveOpenCodeDBPath(dataDir) != ""
}

func openFamilyDB(path, kind string) (*mimoReader, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	dsn := fmt.Sprintf("file:%s?mode=ro&_journal_mode=WAL&_query_only=1&_busy_timeout=5000", abs)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open %s db: %w", kind, err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping %s db: %w", kind, err)
	}
	return &mimoReader{db: db, path: path, kind: kind}, nil
}

func newMiMoReader(path string) (*mimoReader, error) {
	return openFamilyDB(path, "mimo")
}

func newOpenCodeReader(path string) (*mimoReader, error) {
	return openFamilyDB(path, "opencode")
}

func (r *mimoReader) SchemaVersion() int { return 1 }
func (r *mimoReader) DBPath() string     { return r.path }
func (r *mimoReader) Close() error       { return r.db.Close() }

// SessionCount returns the number of sessions without loading messages.
func (r *mimoReader) SessionCount() (int, error) {
	var n int
	err := r.db.QueryRow(`SELECT COUNT(*) FROM session`).Scan(&n)
	return n, err
}

// mimoMsg is the assistant/user message JSON stored in message.data.
type mimoMsg struct {
	Role       string `json:"role"`
	Cost       float64
	ModelID    string `json:"modelID"`
	ProviderID string `json:"providerID"`
	Finish     string `json:"finish"`
	Tokens     *struct {
		Total    int64 `json:"total"`
		Input    int64 `json:"input"`
		Output   int64 `json:"output"`
		Reasoning int64 `json:"reasoning"`
		Cache    *struct {
			Read  int64 `json:"read"`
			Write int64 `json:"write"`
		} `json:"cache"`
	} `json:"tokens"`
	Time *struct {
		Created   *int64 `json:"created"`
		Completed *int64 `json:"completed"`
	} `json:"time"`
	// User messages may nest model
	Model *struct {
		ProviderID string `json:"providerID"`
		ModelID    string `json:"modelID"`
	} `json:"model"`
}

func (r *mimoReader) Sessions() ([]model.Session, error) {
	rows, err := r.db.Query(`
		SELECT id, COALESCE(title,''), COALESCE(directory,''),
		       COALESCE(time_created,0), COALESCE(time_updated,0),
		       COALESCE(project_id,'')
		FROM session
		ORDER BY time_updated DESC`)
	if err != nil {
		return nil, fmt.Errorf("query mimo sessions: %w", err)
	}
	defer rows.Close()

	type sessRow struct {
		id, title, dir, projectID string
		created, updated          int64
	}
	var list []sessRow
	for rows.Next() {
		var s sessRow
		if err := rows.Scan(&s.id, &s.title, &s.dir, &s.created, &s.updated, &s.projectID); err != nil {
			return nil, err
		}
		list = append(list, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]model.Session, 0, len(list))
	for _, s := range list {
		sess := model.Session{
			ID:             s.id,
			Title:          s.title,
			WorkingDir:     s.dir,
			BackendType:    r.kind,
			AgentMode:      "build",
			CreatedAt:      msToTime(s.created),
			LastActivityAt: msToTime(s.updated),
			Source:         r.kind,
			ToolCalls:      map[string]int{},
		}
		if err := r.fillSession(&sess); err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, nil
}

func (r *mimoReader) Session(id string) (*model.Session, error) {
	var title, dir string
	var created, updated int64
	err := r.db.QueryRow(`
		SELECT COALESCE(title,''), COALESCE(directory,''),
		       COALESCE(time_created,0), COALESCE(time_updated,0)
		FROM session WHERE id = ?`, id).
		Scan(&title, &dir, &created, &updated)
	if err != nil {
		return nil, fmt.Errorf("query mimo session %s: %w", id, err)
	}
	s := &model.Session{
		ID:             id,
		Title:          title,
		WorkingDir:     dir,
		BackendType:    r.kind,
		AgentMode:      "build",
		CreatedAt:      msToTime(created),
		LastActivityAt: msToTime(updated),
		Source:         r.kind,
		ToolCalls:      map[string]int{},
	}
	if err := r.fillSession(s); err != nil {
		return nil, err
	}
	return s, nil
}

func (r *mimoReader) fillSession(s *model.Session) error {
	// Messages
	rows, err := r.db.Query(`
		SELECT id, COALESCE(time_created,0), data
		FROM message WHERE session_id = ?
		ORDER BY time_created ASC, id ASC`, s.ID)
	if err != nil {
		return fmt.Errorf("query mimo messages: %w", err)
	}
	defer rows.Close()

	type msgRow struct {
		id string
		ts int64
		cm mimoMsg
	}
	var msgs []msgRow
	for rows.Next() {
		var id string
		var ts int64
		var raw string
		if err := rows.Scan(&id, &ts, &raw); err != nil {
			return err
		}
		var cm mimoMsg
		if err := json.Unmarshal([]byte(raw), &cm); err != nil {
			continue
		}
		msgs = append(msgs, msgRow{id: id, ts: ts, cm: cm})
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// Cache text-part timings per message (streaming start/end).
	// Used to approximate Devin-style TTFT and generation speed.
	timings, err := r.textPartTimings(s.ID)
	if err != nil {
		timings = map[string][]partSpan{}
	}

	var node int
	for _, mr := range msgs {
		cm := mr.cm
		node++
		m := model.Message{
			NodeID:    node,
			Role:      cm.Role,
			CreatedAt: msToTime(mr.ts),
		}
		if cm.Finish != "" {
			m.FinishReason = cm.Finish
		}
		if cm.ModelID != "" {
			m.GenerationModel = cm.ModelID
			if cm.ProviderID != "" {
				m.GenerationModel = cm.ProviderID + "/" + cm.ModelID
			}
		} else if cm.Model != nil && cm.Model.ModelID != "" {
			m.GenerationModel = cm.Model.ModelID
		}
		if cm.Tokens != nil {
			met := &model.Metrics{
				InputTokens:  cm.Tokens.Input,
				OutputTokens: cm.Tokens.Output,
			}
			if cm.Tokens.Cache != nil {
				met.CacheReadTokens = cm.Tokens.Cache.Read
				met.CacheWriteTokens = cm.Tokens.Cache.Write
			}
			// Devin: tokens_per_sec = output / (total_time - ttft)
			// MiMo has no provider field; approximate from text parts:
			//   ttft  ≈ first text start - message created
			//   gen   ≈ sum(text.end - text.start)   (streaming only, excludes tool wait)
			//   tps   ≈ output / gen_seconds
			// Wall-clock created→completed is kept as TotalTimeMs only.
			if cm.Time != nil && cm.Time.Created != nil && cm.Time.Completed != nil && *cm.Time.Completed > *cm.Time.Created {
				met.TotalTimeMs = float64(*cm.Time.Completed - *cm.Time.Created)
			}
			if spans := timings[mr.id]; len(spans) > 0 && cm.Time != nil && cm.Time.Created != nil {
				first := spans[0].start
				for _, sp := range spans {
					if sp.start < first {
						first = sp.start
					}
				}
				if first > *cm.Time.Created {
					met.TTFTMs = float64(first - *cm.Time.Created)
				}
				var genMs int64
				for _, sp := range spans {
					if sp.end > sp.start {
						genMs += sp.end - sp.start
					}
				}
				if cm.Tokens.Output > 0 && genMs > 0 {
					met.TokensPerSec = float64(cm.Tokens.Output) / (float64(genMs) / 1000.0)
				}
			}
			m.Metrics = met
		}
		if cm.Role == "assistant" {
			s.AssistantCount++
			if m.Metrics != nil {
				s.InputTokens += m.Metrics.InputTokens
				s.OutputTokens += m.Metrics.OutputTokens
				s.CacheRead += m.Metrics.CacheReadTokens
				s.CacheWrite += m.Metrics.CacheWriteTokens
			}
			s.CreditCost += cm.Cost
			if m.GenerationModel != "" {
				s.LatestModel = m.GenerationModel
			}
		}
		if s.Model == "" && m.GenerationModel != "" {
			s.Model = m.GenerationModel
		}
		s.Messages = append(s.Messages, m)
	}
	if err := rows.Err(); err != nil {
		return err
	}

	// Tool calls from parts
	trows, err := r.db.Query(`
		SELECT data FROM part
		WHERE session_id = ? AND data LIKE '%"tool"%'`, s.ID)
	if err != nil {
		return nil
	}
	defer trows.Close()
	for trows.Next() {
		var raw string
		if err := trows.Scan(&raw); err != nil {
			continue
		}
		var p struct {
			Type string `json:"type"`
			Tool string `json:"tool"`
		}
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			continue
		}
		if p.Type == "tool" && p.Tool != "" {
			s.ToolCalls[p.Tool]++
		}
	}
	return nil
}

// partSpan is a streaming text part's start/end in unix milliseconds.
type partSpan struct{ start, end int64 }

// textPartTimings loads type=text parts that carry time.{start,end} for a session.
func (r *mimoReader) textPartTimings(sessionID string) (map[string][]partSpan, error) {
	rows, err := r.db.Query(`
		SELECT message_id, data FROM part
		WHERE session_id = ? AND data LIKE '%"time"%start%'`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]partSpan{}
	for rows.Next() {
		var mid, raw string
		if err := rows.Scan(&mid, &raw); err != nil {
			continue
		}
		var p struct {
			Type string `json:"type"`
			Time *struct {
				Start *int64 `json:"start"`
				End   *int64 `json:"end"`
			} `json:"time"`
		}
		if err := json.Unmarshal([]byte(raw), &p); err != nil || p.Time == nil {
			continue
		}
		if p.Time.Start == nil || p.Time.End == nil {
			continue
		}
		out[mid] = append(out[mid], partSpan{start: *p.Time.Start, end: *p.Time.End})
	}
	return out, rows.Err()
}

func msToTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).Local()
}

// sortedSessionIDs is unused helper kept for symmetry with other readers.
var _ = sort.Strings
var _ = strings.TrimSpace
