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

// mimoReader reads MiMoCode / MiMo Desktop usage from mimocode.db.
// Schema (drizzle): session / message / part. Assistant messages carry
// tokens{input,output,reasoning,cache{read,write}} and cost.
type mimoReader struct {
	db   *sql.DB
	path string
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

// HasMiMoDB reports whether a mimocode.db is discoverable.
func HasMiMoDB(dataDir string) bool {
	return ResolveMiMoDBPath(dataDir) != ""
}

func newMiMoReader(path string) (*mimoReader, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	dsn := fmt.Sprintf("file:%s?mode=ro&_journal_mode=WAL&_query_only=1&_busy_timeout=5000", abs)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open mimo db: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping mimo db: %w", err)
	}
	return &mimoReader{db: db, path: path}, nil
}

func (r *mimoReader) SchemaVersion() int { return 1 }
func (r *mimoReader) DBPath() string     { return r.path }
func (r *mimoReader) Close() error       { return r.db.Close() }

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
			BackendType:    "mimo",
			AgentMode:      "build",
			CreatedAt:      msToTime(s.created),
			LastActivityAt: msToTime(s.updated),
			Source:         "mimo",
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
		BackendType:    "mimo",
		AgentMode:      "build",
		CreatedAt:      msToTime(created),
		LastActivityAt: msToTime(updated),
		Source:         "mimo",
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

	var node int
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
		node++
		m := model.Message{
			NodeID:    node,
			Role:      cm.Role,
			CreatedAt: msToTime(ts),
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
			// Wall-clock turn duration (created→completed). Includes tool
			// execution wait, so it is NOT generation speed.
			// TokensPerSec is left 0: Devin reports provider tokens_per_sec
			// (output / (total-ttft)); MiMo has no equivalent field. Dividing
			// output by this wall-clock yields misleading ~10 t/s vs Devin's
			// hundreds.
			if cm.Time != nil && cm.Time.Created != nil && cm.Time.Completed != nil && *cm.Time.Completed > *cm.Time.Created {
				met.TotalTimeMs = float64(*cm.Time.Completed - *cm.Time.Created)
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

func msToTime(ms int64) time.Time {
	if ms <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).Local()
}

// sortedSessionIDs is unused helper kept for symmetry with other readers.
var _ = sort.Strings
var _ = strings.TrimSpace
