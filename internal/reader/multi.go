package reader

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/garywhat/devinmonitor/internal/model"
)

// SourceRef names one Devin data store.
type SourceRef struct {
	// Label is "local" or "wsl:<distro>".
	Label string
	// Reader is an already-opened single-source reader.
	Reader Reader
}

// MultiReader fans out Sessions()/Session() across several sources.
type MultiReader struct {
	sources []SourceRef
	// Primary DB path shown by DBPath().
	primaryPath string
	// cleanup dirs (WSL snapshots) removed on Close.
	cleanup []string
	// wsl tracks refreshable WSL snapshot sources.
	wsl []wslTrack
}

// wslTrack remembers how to re-snapshot a WSL source for live refresh.
type wslTrack struct {
	label      string // "wsl:Ubuntu-18.04" or "wsl-mimo:Ubuntu-18.04"
	distro     string // "Ubuntu-18.04"
	snapDir    string // current Windows-side snapshot dir
	mtime      int64
	size       int64
	isMiMo     bool
}

// trackWSLFP records a WSL Devin source with a known upstream fingerprint.
func (m *MultiReader) trackWSLFP(label, distro, snapDir string, mt, sz int64) {
	m.wsl = append(m.wsl, wslTrack{
		label: label, distro: distro, snapDir: snapDir, mtime: mt, size: sz,
	})
}

// trackWSLMiMoFP records a WSL MiMo source fingerprint.
func (m *MultiReader) trackWSLMiMoFP(label, distro, snapDir string, mt, sz int64) {
	m.wsl = append(m.wsl, wslTrack{
		label: label, distro: distro, snapDir: snapDir, mtime: mt, size: sz, isMiMo: true,
	})
}

// Refresh re-snapshots WSL sources when the in-distro db changed.
func (m *MultiReader) Refresh() error {
	if len(m.wsl) == 0 {
		return nil
	}
	for i := range m.wsl {
		t := &m.wsl[i]
		var mt, sz int64
		var ok bool
		if t.isMiMo {
			mt, sz, ok = WSLMiMoStat(t.distro)
		} else {
			mt, sz, ok = WSLDBStat(t.distro)
		}
		if !ok {
			continue
		}
		if mt == t.mtime && sz == t.size {
			continue
		}
		var newDir string
		var err error
		var r Reader
		if t.isMiMo {
			newDir, err = SnapshotWSLMiMo(t.distro)
			if err != nil {
				return fmt.Errorf("refresh wsl-mimo %s: %w", t.distro, err)
			}
			r, err = newMiMoReader(filepath.Join(newDir, "mimocode.db"))
		} else {
			newDir, err = SnapshotWSLDB(t.distro)
			if err != nil {
				return fmt.Errorf("refresh wsl %s: %w", t.distro, err)
			}
			r, err = openOne(newDir)
		}
		if err != nil {
			cleanupDir(newDir)
			return fmt.Errorf("refresh %s open: %w", t.label, err)
		}
		swapped := false
		for j := range m.sources {
			if m.sources[j].Label == t.label {
				_ = m.sources[j].Reader.Close()
				m.sources[j].Reader = r
				swapped = true
				break
			}
		}
		if !swapped {
			_ = r.Close()
			cleanupDir(newDir)
			return fmt.Errorf("refresh %s: source disappeared", t.label)
		}
		oldDir := t.snapDir
		t.snapDir = newDir
		t.mtime, t.size = mt, sz
		m.removeCleanup(oldDir)
		cleanupDir(oldDir)
		m.AddCleanup(newDir)
	}
	return nil
}

// NewMulti builds a MultiReader from one or more opened sources.
func NewMulti(sources ...SourceRef) *MultiReader {
	p := ""
	if len(sources) > 0 {
		p = sources[0].Reader.DBPath()
	}
	return &MultiReader{sources: sources, primaryPath: p}
}

// AddCleanup registers a directory to remove when Close is called.
func (m *MultiReader) AddCleanup(dir string) {
	if dir != "" {
		m.cleanup = append(m.cleanup, dir)
	}
}

func (m *MultiReader) removeCleanup(dir string) {
	out := m.cleanup[:0]
	for _, d := range m.cleanup {
		if d != dir {
			out = append(out, d)
		}
	}
	m.cleanup = out
}

// Sources returns the source labels in open order.
func (m *MultiReader) Sources() []string {
	out := make([]string, 0, len(m.sources))
	for _, s := range m.sources {
		out = append(out, s.Label)
	}
	return out
}

func (m *MultiReader) SchemaVersion() int {
	if len(m.sources) == 0 {
		return 0
	}
	return m.sources[0].Reader.SchemaVersion()
}

func (m *MultiReader) DBPath() string { return m.primaryPath }

func (m *MultiReader) Close() error {
	var first error
	for _, s := range m.sources {
		if err := s.Reader.Close(); err != nil && first == nil {
			first = err
		}
	}
	for _, d := range m.cleanup {
		cleanupDir(d)
	}
	return first
}

// Sessions returns all sessions from every source, newest activity first.
// Each session is tagged with Source; IDs are qualified when the same
// short id appears in more than one store.
func (m *MultiReader) Sessions() ([]model.Session, error) {
	var out []model.Session
	seen := map[string]int{} // bare ID -> count across sources
	for _, src := range m.sources {
		ss, err := src.Reader.Sessions()
		if err != nil {
			return nil, fmt.Errorf("%s: %w", src.Label, err)
		}
		for i := range ss {
			ss[i].Source = src.Label
			seen[ss[i].ID]++
		}
		out = append(out, ss...)
	}
	// Qualify colliding bare IDs so Session() and the UI can address them.
	for i := range out {
		if seen[out[i].ID] > 1 {
			// Keep ID as-is; lookup uses QualifiedID / DisplayID.
			// For uniqueness in maps keyed by ID, callers should use QualifiedID.
			_ = out[i].QualifiedID()
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].LastActivityAt.After(out[j].LastActivityAt)
	})
	return out, nil
}

// Session looks up by:
//  1. qualified id  "wsl:Ubuntu-18.04/halved-noodle"
//  2. display id    "wsl:halved-noodle" (maps to any wsl:* source)
//  3. bare id unique across sources
//  4. bare id with multiple hits -> error listing candidates
func (m *MultiReader) Session(id string) (*model.Session, error) {
	if id == "" {
		return nil, fmt.Errorf("empty session id")
	}

	// Qualified form: source/id (source may contain ':')
	if i := strings.Index(id, "/"); i > 0 {
		srcLabel, bare := id[:i], id[i+1:]
		for _, src := range m.sources {
			if src.Label == srcLabel {
				s, err := src.Reader.Session(bare)
				if err != nil {
					return nil, err
				}
				s.Source = src.Label
				return s, nil
			}
		}
		return nil, fmt.Errorf("unknown source %q in id %q", srcLabel, id)
	}

	// Display form "wsl:foo" / "mimo:ses_x" / "local:foo".
	if i := strings.Index(id, ":"); i > 0 {
		prefix, bare := id[:i], id[i+1:]
		// Exact source label match (e.g. "mimo", "local").
		for _, src := range m.sources {
			if src.Label == prefix {
				s, err := src.Reader.Session(bare)
				if err != nil {
					return nil, err
				}
				s.Source = src.Label
				return s, nil
			}
		}
		// Group match "wsl:foo" against every wsl:* source.
		if prefix == "wsl" {
			var hits []*model.Session
			var labels []string
			for _, src := range m.sources {
				if !strings.HasPrefix(src.Label, "wsl:") {
					continue
				}
				s, err := src.Reader.Session(bare)
				if err != nil {
					continue
				}
				s.Source = src.Label
				hits = append(hits, s)
				labels = append(labels, src.Label)
			}
			switch len(hits) {
			case 0:
				return nil, fmt.Errorf("session %q not found in any WSL source", bare)
			case 1:
				return hits[0], nil
			default:
				return nil, fmt.Errorf("session %q is ambiguous across WSL sources %s; use %s/%s",
					bare, strings.Join(labels, ", "), labels[0], bare)
			}
		}
		// Unknown prefix — fall through to bare search of full id.
	}

	// Bare id: search every source.
	var hits []*model.Session
	var labels []string
	for _, src := range m.sources {
		s, err := src.Reader.Session(id)
		if err != nil {
			// no rows / not found — keep searching other sources
			continue
		}
		s.Source = src.Label
		hits = append(hits, s)
		labels = append(labels, src.Label)
	}
	switch len(hits) {
	case 0:
		return nil, fmt.Errorf("session %q not found in any source", id)
	case 1:
		return hits[0], nil
	default:
		return nil, fmt.Errorf("session id %q is ambiguous across sources %s; use source/id (e.g. %s/%s)",
			id, strings.Join(labels, ", "), labels[0], id)
	}
}

// SourcesSummary describes opened sources for status/debug output.
func (m *MultiReader) SourcesSummary() []SourceSummary {
	out := make([]SourceSummary, 0, len(m.sources))
	for _, s := range m.sources {
		n := 0
		if ss, err := s.Reader.Sessions(); err == nil {
			n = len(ss)
		}
		out = append(out, SourceSummary{
			Label:      s.Label,
			Path:       s.Reader.DBPath(),
			Schema:     s.Reader.SchemaVersion(),
			Sessions:   n,
		})
	}
	return out
}

// SourceSummary is a one-line description of an open data source.
type SourceSummary struct {
	Label    string
	Path     string
	Schema   int
	Sessions int
}
