//go:build windows

package reader

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"
)

// wsl timeouts keep the CLI/web responsive when WSL is slow or wedged.
const (
	wslListTimeout  = 20 * time.Second
	wslProbeTimeout = 25 * time.Second
	wslStatTimeout  = 25 * time.Second
	// Snapshot copies a large sqlite file; allow a few minutes.
	wslSnapTimeout = 3 * time.Minute
)

func wslCtx(parent context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	return context.WithTimeout(parent, d)
}

// wslDevinDB is the in-distro path of Devin CLI's session store.
const wslDevinDB = ".local/share/devin/cli/sessions.db"

// wslMiMoDB is the in-distro path of MiMoCode's session store.
const wslMiMoDB = ".local/share/mimocode/mimocode.db"

// wslOpenCodeDB is the in-distro path of OpenCode's session store.
const wslOpenCodeDB = ".local/share/opencode/opencode.db"

// WSLDistro describes a WSL distro that has a Devin sessions.db.
type WSLDistro struct {
	Name string // e.g. Ubuntu-18.04
	// SnapshotDir is the Windows directory holding the copied db files.
	SnapshotDir string
}

// DetectWSLDistros lists installed WSL distros (excluding docker-desktop).
// Callers probe each distro for Devin / MiMo / OpenCode stores.
func DetectWSLDistros(only []string) []string {
	if os.Getenv("DEVIN_NO_WSL") == "1" {
		return nil
	}
	if _, err := exec.LookPath("wsl.exe"); err != nil {
		return nil
	}
	ctx, cancel := wslCtx(nil, wslListTimeout)
	defer cancel()
	raw, err := exec.CommandContext(ctx, "wsl.exe", "--list", "--quiet").Output()
	if err != nil {
		return nil
	}
	names := parseWSLList(raw)
	if len(names) == 0 {
		return nil
	}
	want := map[string]bool{}
	for _, n := range only {
		if n != "" {
			want[n] = true
		}
	}
	var out []string
	for _, name := range names {
		if len(want) > 0 && !want[name] {
			continue
		}
		out = append(out, name)
	}
	return out
}

// parseWSLList decodes wsl.exe --list --quiet output (often UTF-16LE with BOM).
func parseWSLList(raw []byte) []string {
	text := decodeWSLOutput(raw)
	var names []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(strings.Trim(line, "\r\x00"))
		// Skip default marker like "Ubuntu-18.04 (Default)"
		if i := strings.Index(line, " (Default)"); i >= 0 {
			line = line[:i]
		}
		if line == "" || strings.HasPrefix(line, "docker") {
			continue
		}
		names = append(names, line)
	}
	return names
}

func decodeWSLOutput(raw []byte) string {
	// UTF-16LE with BOM?
	if len(raw) >= 2 && raw[0] == 0xFF && raw[1] == 0xFE {
		return decodeUTF16LE(raw[2:])
	}
	// Heuristic: many NUL bytes => UTF-16LE without BOM
	nuls := 0
	for i := 1; i < len(raw) && i < 40; i += 2 {
		if raw[i] == 0 {
			nuls++
		}
	}
	if nuls > 5 {
		return decodeUTF16LE(raw)
	}
	return string(raw)
}

func decodeUTF16LE(b []byte) string {
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		u = append(u, uint16(b[i])|uint16(b[i+1])<<8)
	}
	return string(utf16.Decode(u))
}

func wslHasDevinDB(distro string) bool {
	return ProbeWSLServices(distro).Devin
}

// ProbeWSLServices reports which supported AI stores exist in the distro.
// One bash call per distro (script file avoids $ mangling).
func ProbeWSLServices(distro string) ProbeWSLHits {
	var hits ProbeWSLHits
	if _, err := exec.LookPath("wsl.exe"); err != nil {
		return hits
	}
	tmp, err := os.MkdirTemp("", "devinmonitor-probe-")
	if err != nil {
		return hits
	}
	defer os.RemoveAll(tmp)
	script := fmt.Sprintf(`#!/bin/bash
[ -f "$HOME/%s" ] && echo DEVIN
[ -f "$HOME/%s" ] && echo MIMO
[ -f "$HOME/%s" ] && echo OPENCODE
exit 0
`, wslDevinDB, wslMiMoDB, wslOpenCodeDB)
	sp := filepath.Join(tmp, "probe.sh")
	if err := os.WriteFile(sp, []byte(script), 0o755); err != nil {
		return hits
	}
	wslSp, err := winToWSLPath(sp)
	if err != nil {
		return hits
	}
	ctx, cancel := wslCtx(nil, wslProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "wsl.exe", "-d", distro, "bash", wslSp).Output()
	if err != nil {
		return hits
	}
	s := string(out)
	hits.Devin = strings.Contains(s, "DEVIN")
	hits.MiMo = strings.Contains(s, "MIMO")
	hits.OpenCode = strings.Contains(s, "OPENCODE")
	return hits
}

// WSLStatFile returns mtime/size of an in-distro file (relative to $HOME).
func WSLStatFile(distro, relHome string) (mtime, size int64, ok bool) {
	tmp, err := os.MkdirTemp("", "devinmonitor-stat-")
	if err != nil {
		return 0, 0, false
	}
	defer os.RemoveAll(tmp)
	script := fmt.Sprintf("#!/bin/bash\nstat -c '%%Y %%s' \"$HOME/%s\"\n", relHome)
	sp := filepath.Join(tmp, "stat.sh")
	if err := os.WriteFile(sp, []byte(script), 0o755); err != nil {
		return 0, 0, false
	}
	wslSp, err := winToWSLPath(sp)
	if err != nil {
		return 0, 0, false
	}
	ctx, cancel := wslCtx(nil, wslStatTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "wsl.exe", "-d", distro, "bash", wslSp).Output()
	if err != nil {
		return 0, 0, false
	}
	var mt, sz int64
	if _, err := fmt.Sscan(strings.TrimSpace(string(out)), &mt, &sz); err != nil {
		return 0, 0, false
	}
	return mt, sz, true
}

// WSLDBStat returns mtime (unix sec) and size of the in-distro sessions.db.
func WSLDBStat(distro string) (mtime, size int64, ok bool) {
	return WSLStatFile(distro, wslDevinDB)
}

// WSLMiMoStat fingerprint for live refresh of WSL MiMo stores.
func WSLMiMoStat(distro string) (mtime, size int64, ok bool) {
	return WSLStatFile(distro, wslMiMoDB)
}

// WSLOpenCodeStat fingerprint for live refresh of WSL OpenCode stores.
func WSLOpenCodeStat(distro string) (mtime, size int64, ok bool) {
	return WSLStatFile(distro, wslOpenCodeDB)
}

// snapshotWSLFile copies a $HOME-relative sqlite db (+wal/shm) into a temp dir.
func snapshotWSLFile(distro, relHome, outName, tag string) (string, error) {
	winDir := stableSnapDir(tag, distro)
	if err := os.MkdirAll(winDir, 0o755); err != nil {
		return "", err
	}
	wslDir, err := winToWSLPath(winDir)
	if err != nil {
		return "", err
	}
	script := fmt.Sprintf(`#!/bin/bash
set -e
src="$HOME/%s"
dir='%s'
mkdir -p "$dir"
# Copy via temp + rename so a concurrent reader never sees a half-written db.
cp -f "$src" "$dir/%s.tmp"
mv -f "$dir/%s.tmp" "$dir/%s"
if [ -f "$src-wal" ]; then cp -f "$src-wal" "$dir/%s-wal" 2>/dev/null || true; fi
if [ -f "$src-shm" ]; then cp -f "$src-shm" "$dir/%s-shm" 2>/dev/null || true; fi
echo OK
`, relHome, wslDir, outName, outName, outName, outName, outName)
	scriptPath := filepath.Join(winDir, "snap.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		os.RemoveAll(winDir)
		return "", err
	}
	wslScript, err := winToWSLPath(scriptPath)
	if err != nil {
		os.RemoveAll(winDir)
		return "", err
	}
	ctx, cancel := wslCtx(nil, wslSnapTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "wsl.exe", "-d", distro, "bash", wslScript)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("wsl %s snapshot %s: %v (%s)", tag, distro, err, strings.TrimSpace(stderr.String()))
	}
	if !strings.Contains(string(out), "OK") {
		return "", fmt.Errorf("wsl %s snapshot %s: unexpected output %q", tag, distro, out)
	}
	if _, err := os.Stat(filepath.Join(winDir, outName)); err != nil {
		return "", fmt.Errorf("wsl %s snapshot %s: %s missing", tag, distro, outName)
	}
	_ = os.Remove(scriptPath)
	return winDir, nil
}

// SnapshotWSLMiMo copies mimocode.db (+ -wal/-shm) from the distro.
func SnapshotWSLMiMo(distro string) (string, error) {
	return snapshotWSLFile(distro, wslMiMoDB, "mimocode.db", "wslmimo")
}

// SnapshotWSLOpenCode copies opencode.db (+ -wal/-shm) from the distro.
func SnapshotWSLOpenCode(distro string) (string, error) {
	return snapshotWSLFile(distro, wslOpenCodeDB, "opencode.db", "wslopencode")
}

// SnapshotWSLDB copies sessions.db (+ -wal/-shm) from the distro into a
// stable Windows temp dir (reused across refreshes) and returns that directory.
// Avoids opening the live WSL file over \\wsl.localhost (SQLITE_BUSY on 9p locks).
func SnapshotWSLDB(distro string) (string, error) {
	return snapshotWSLFile(distro, wslDevinDB, "sessions.db", "wsl")
}

// winToWSLPath maps C:\foo\bar -> /mnt/c/foo/bar
func winToWSLPath(win string) (string, error) {
	abs, err := filepath.Abs(win)
	if err != nil {
		return "", err
	}
	// \\?\C:\... strip prefix
	abs = strings.TrimPrefix(abs, `\\?\`)
	if len(abs) < 2 || abs[1] != ':' {
		return "", fmt.Errorf("cannot map path %q into WSL (need drive letter)", abs)
	}
	drive := strings.ToLower(string(abs[0]))
	rest := filepath.ToSlash(abs[2:])
	return "/mnt/" + drive + rest, nil
}

func sanitizeFile(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "distro"
	}
	return b.String()
}

// cleanupDir removes a snapshot directory (used by Multi open).
func cleanupDir(dir string) {
	if dir != "" {
		_ = os.RemoveAll(dir)
	}
}

// stableSnapDir is the single reused snapshot location for one source.
// Reusing a fixed path avoids leaking a new ~300MB copy on every refresh
// when the process is killed without Close().
func stableSnapDir(tag, distro string) string {
	return filepath.Join(os.TempDir(), "devinmonitor-"+tag+"-"+sanitizeFile(distro))
}

// PurgeStaleSnapshots removes leftover devinmonitor-* temp dirs except those
// in keep. Call on startup so killed processes don't pin C: space forever.
func PurgeStaleSnapshots(keep map[string]bool) {
	tmp := os.TempDir()
	ents, err := os.ReadDir(tmp)
	if err != nil {
		return
	}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, "devinmonitor-") {
			continue
		}
		full := filepath.Join(tmp, name)
		if keep[full] {
			continue
		}
		_ = os.RemoveAll(full)
	}
}
