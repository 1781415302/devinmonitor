// Package reader adapts Devin CLI's internal SQLite schema to model types.
//
// It isolates schema-specific SQL/JSON parsing so that reports and UI never
// touch the raw DB. When Devin CLI changes its schema, only this package
// needs a new adapter (e.g. v2.go) selected by DetectSchemaVersion.
package reader

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/garywhat/devinmonitor/internal/model"
)

// MaxSupportedSchema is the highest refinery schema version we understand.
// Bump when a new adapter is added.
const MaxSupportedSchema = 999 // current schema has no version row; treat as v1

// ErrNoDB is returned when sessions.db cannot be located.
type ErrNoDB struct{ Path string }

func (e *ErrNoDB) Error() string { return fmt.Sprintf("sessions.db not found at: %s", e.Path) }

// ErrSchemaUnsupported is returned when the schema version exceeds our adapters.
type ErrSchemaUnsupported struct{ Ver, Max int }

func (e *ErrSchemaUnsupported) Error() string {
	return fmt.Sprintf("unsupported schema version %d (expected <= %d)", e.Ver, e.Max)
}

// Reader reads normalized data from Devin CLI's session store.
type Reader interface {
	// Sessions returns all sessions (newest first), with messages aggregated.
	Sessions() ([]model.Session, error)
	// Session returns a single session by ID, with messages.
	Session(id string) (*model.Session, error)
	// SchemaVersion returns the detected refinery schema version (0 if none).
	SchemaVersion() int
	// DBPath returns the resolved database path.
	DBPath() string
	// Close releases any resources.
	Close() error
}

// Refresher is implemented by readers that can pick up newer upstream data
// without reopening the process (e.g. re-snapshot WSL stores).
// Live dashboards call Refresh before each Sessions() poll.
type Refresher interface {
	// Refresh reloads any secondary sources. No-op if nothing changed.
	Refresh() error
}

// OpenOptions controls multi-source opening.
type OpenOptions struct {
	// DataDir forces a single local source when non-empty.
	DataDir string
	// IncludeWSL merges WSL distro snapshots (Windows only). Default true
	// when DataDir is empty; ignored when DataDir is set.
	IncludeWSL bool
	// IncludeMiMo merges local MiMoCode mimocode.db when present.
	IncludeMiMo bool
	// WSLDistros limits which distros to snapshot (empty = all with Devin DB).
	WSLDistros []string
}

// Default multi-source settings (feature packages call Open which honors these).
var (
	defaultIncludeWSL  bool     = true
	defaultIncludeMiMo bool     = true
	defaultWSLDistros  []string = nil
)

// SetDefaultIncludeWSL toggles WSL merging for subsequent Open() calls.
func SetDefaultIncludeWSL(v bool) { defaultIncludeWSL = v }

// SetDefaultIncludeMiMo toggles MiMoCode merging for subsequent Open() calls.
func SetDefaultIncludeMiMo(v bool) { defaultIncludeMiMo = v }

// SetDefaultWSLDistros limits Open() WSL snapshots to these distros.
func SetDefaultWSLDistros(distros []string) {
	defaultWSLDistros = append([]string(nil), distros...)
}

// Open auto-detects the DB path and returns a Reader for the current schema.
// dataDir overrides the auto-detected directory when non-empty (single source).
// When dataDir is empty on Windows, also merges WSL Devin stores by default.
func Open(dataDir string) (Reader, error) {
	return OpenWith(OpenOptions{
		DataDir:     dataDir,
		IncludeWSL:  defaultIncludeWSL,
		IncludeMiMo: defaultIncludeMiMo,
		WSLDistros:  defaultWSLDistros,
	})
}

// OpenWith opens one or more sources according to opts.
// When DataDir is empty it auto-discovers supported AI stores on the
// local host and (Windows) every WSL distro: Devin + MiMoCode.
func OpenWith(opts OpenOptions) (Reader, error) {
	// Explicit --data-dir: single forced source, no discovery.
	if opts.DataDir != "" {
		return openOne(opts.DataDir)
	}

	found := DiscoverSources(opts.WSLDistros, opts.IncludeWSL, opts.IncludeMiMo)
	if len(found) == 0 {
		return openOne("")
	}

	// Open local sources first, then snapshot WSL.
	var sources []SourceRef
	var cleanups []string
	type wslTrackInit struct {
		label, distro, snapDir string
		mt, sz                 int64
		family                 string // devin|mimo|opencode
	}
	var tracks []wslTrackInit

	for _, ds := range found {
		if ds.Distro == "" {
			// Local host file.
			var r Reader
			var err error
			switch ds.Kind {
			case KindMiMo:
				r, err = newMiMoReader(ds.Path)
			case KindOpenCode:
				r, err = newOpenCodeReader(ds.Path)
			default:
				r, err = openOneFile(ds.Path)
			}
			if err != nil {
				fmt.Fprintf(os.Stderr, "devinmonitor: skip %s: %v\n", ds.Label, err)
				continue
			}
			sources = append(sources, SourceRef{Label: ds.Label, Reader: r})
			continue
		}

		// WSL source — snapshot then open.
		if ds.Kind == KindDevin {
			mt, sz, _ := WSLDBStat(ds.Distro)
			snapDir, err := SnapshotWSLDB(ds.Distro)
			if err != nil {
				fmt.Fprintf(os.Stderr, "devinmonitor: skip %s: %v\n", ds.Label, err)
				continue
			}
			r, err := openOne(snapDir)
			if err != nil {
				cleanupDir(snapDir)
				fmt.Fprintf(os.Stderr, "devinmonitor: skip %s: %v\n", ds.Label, err)
				continue
			}
			sources = append(sources, SourceRef{Label: ds.Label, Reader: r})
			cleanups = append(cleanups, snapDir)
			tracks = append(tracks, wslTrackInit{ds.Label, ds.Distro, snapDir, mt, sz, "devin"})
			continue
		}
		if ds.Kind == KindMiMo || ds.Kind == KindOpenCode {
			isMiMo := ds.Kind == KindMiMo
			var mt, sz int64
			var snapDir string
			var err error
			var r Reader
			if isMiMo {
				mt, sz, _ = WSLMiMoStat(ds.Distro)
				snapDir, err = SnapshotWSLMiMo(ds.Distro)
				if err == nil {
					r, err = newMiMoReader(filepath.Join(snapDir, "mimocode.db"))
				}
			} else {
				mt, sz, _ = WSLOpenCodeStat(ds.Distro)
				snapDir, err = SnapshotWSLOpenCode(ds.Distro)
				if err == nil {
					r, err = newOpenCodeReader(filepath.Join(snapDir, "opencode.db"))
				}
			}
			if err != nil {
				if snapDir != "" {
					cleanupDir(snapDir)
				}
				fmt.Fprintf(os.Stderr, "devinmonitor: skip %s: %v\n", ds.Label, err)
				continue
			}
			sources = append(sources, SourceRef{Label: ds.Label, Reader: r})
			cleanups = append(cleanups, snapDir)
			if isMiMo {
				tracks = append(tracks, wslTrackInit{ds.Label, ds.Distro, snapDir, mt, sz, "mimo"})
			} else {
				tracks = append(tracks, wslTrackInit{ds.Label, ds.Distro, snapDir, mt, sz, "opencode"})
			}
		}
	}

	if len(sources) == 0 {
		return openOne("")
	}
	if len(sources) == 1 {
		return sources[0].Reader, nil
	}
	m := NewMulti(sources...)
	for _, d := range cleanups {
		m.AddCleanup(d)
	}
	for _, t := range tracks {
		switch t.family {
		case "mimo":
			m.trackWSLMiMoFP(t.label, t.distro, t.snapDir, t.mt, t.sz)
		case "opencode":
			m.trackWSLOpenCodeFP(t.label, t.distro, t.snapDir, t.mt, t.sz)
		default:
			m.trackWSLFP(t.label, t.distro, t.snapDir, t.mt, t.sz)
		}
	}
	return m, nil
}

// openOneFile opens a reader when only the sessions.db full path is known.
func openOneFile(dbPath string) (Reader, error) {
	ver, err := DetectSchemaVersion(dbPath)
	if err != nil {
		return nil, fmt.Errorf("detect schema: %w", err)
	}
	if ver > MaxSupportedSchema {
		return nil, &ErrSchemaUnsupported{Ver: ver, Max: MaxSupportedSchema}
	}
	return newV1Reader(dbPath, ver)
}

// openOne resolves and opens a single local/snapshot database.
func openOne(dataDir string) (Reader, error) {
	path, err := ResolveDBPath(dataDir)
	if err != nil {
		return nil, err
	}
	ver, err := DetectSchemaVersion(path)
	if err != nil {
		return nil, fmt.Errorf("detect schema: %w", err)
	}
	if ver > MaxSupportedSchema {
		return nil, &ErrSchemaUnsupported{Ver: ver, Max: MaxSupportedSchema}
	}
	// Currently only v1 exists.
	return newV1Reader(path, ver)
}

// ResolveDBPath finds sessions.db across platforms.
//
// Order: dataDir arg > DEVIN_DATA_DIR env > platform defaults.
func ResolveDBPath(dataDir string) (string, error) {
	candidates := []string{}

	if dataDir != "" {
		candidates = append(candidates, filepath.Join(dataDir, "sessions.db"))
	}
	if env := os.Getenv("DEVIN_DATA_DIR"); env != "" {
		candidates = append(candidates, filepath.Join(env, "sessions.db"))
	}

	for _, p := range platformDefaults() {
		candidates = append(candidates, filepath.Join(p, "sessions.db"))
	}

	for _, c := range candidates {
		if info, err := os.Stat(c); err == nil && !info.IsDir() {
			return c, nil
		}
	}
	if len(candidates) > 0 {
		return "", &ErrNoDB{Path: candidates[0]}
	}
	return "", &ErrNoDB{Path: "(unknown)"}
}

// platformDefaults returns candidate Devin CLI data directories per OS.
func platformDefaults() []string {
	home, _ := os.UserHomeDir()
	var out []string
	switch runtime.GOOS {
	case "windows":
		if appdata := os.Getenv("APPDATA"); appdata != "" {
			out = append(out, filepath.Join(appdata, "devin", "cli"))
		}
		if localappdata := os.Getenv("LOCALAPPDATA"); localappdata != "" {
			out = append(out, filepath.Join(localappdata, "devin", "cli"))
		}
	default: // linux, darwin, freebsd...
		if home != "" {
			// XDG default.
			out = append(out, filepath.Join(home, ".local", "share", "devin", "cli"))
			// macOS sometimes uses Library/Application Support.
			if runtime.GOOS == "darwin" {
				out = append(out, filepath.Join(home, "Library", "Application Support", "devin", "cli"))
			}
		}
		if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
			out = append(out, filepath.Join(xdg, "devin", "cli"))
		}
	}
	return out
}

// DetectSchemaVersion reads refinery_schema_history max(version).
// Returns 0 if the table is empty or missing (treated as v1 baseline).
func DetectSchemaVersion(dbPath string) (int, error) {
	r, err := newV1Reader(dbPath, 0)
	if err != nil {
		return 0, err
	}
	defer r.Close()
	return r.SchemaVersion(), nil
}

// IsTTY reports whether the given fd is a terminal.
func IsTTY(fd uintptr) bool {
	info, err := os.Stat("/dev/stdin")
	if err == nil && (info.Mode()&os.ModeCharDevice) != 0 {
		return true
	}
	// Fallback: check if stdin is a char device via Stat on fd.
	_ = errors.New
	return false
}
