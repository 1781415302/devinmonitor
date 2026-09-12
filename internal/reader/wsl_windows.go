//go:build windows

package reader

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf16"
)

// wslDevinDB is the in-distro path of Devin CLI's session store.
const wslDevinDB = ".local/share/devin/cli/sessions.db"

// WSLDistro describes a WSL distro that has a Devin sessions.db.
type WSLDistro struct {
	Name string // e.g. Ubuntu-18.04
	// SnapshotDir is the Windows directory holding the copied db files.
	SnapshotDir string
}

// DetectWSLDistros lists running/installed WSL distros that contain a
// Devin CLI sessions.db. Best-effort: failures are skipped silently.
func DetectWSLDistros(only []string) []string {
	if os.Getenv("DEVIN_NO_WSL") == "1" {
		return nil
	}
	if _, err := exec.LookPath("wsl.exe"); err != nil {
		return nil
	}
	raw, err := exec.Command("wsl.exe", "--list", "--quiet").Output()
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
		if wslHasDevinDB(name) {
			out = append(out, name)
		}
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
	script := fmt.Sprintf("test -f \"$HOME/%s\" && echo YES", wslDevinDB)
	out, err := exec.Command("wsl.exe", "-d", distro, "bash", "-c", script).Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), "YES")
}

// WSLDBStat returns mtime (unix sec) and size of the in-distro sessions.db.
// Used to skip re-snapshot when nothing changed (live refresh).
func WSLDBStat(distro string) (mtime, size int64, ok bool) {
	// Script file avoids wsl.exe eating `$` in inline bash -c args.
	tmp, err := os.MkdirTemp("", "devinmonitor-stat-")
	if err != nil {
		return 0, 0, false
	}
	defer os.RemoveAll(tmp)
	script := fmt.Sprintf("#!/bin/bash\nstat -c '%%Y %%s' \"$HOME/%s\"\n", wslDevinDB)
	sp := filepath.Join(tmp, "stat.sh")
	if err := os.WriteFile(sp, []byte(script), 0o755); err != nil {
		return 0, 0, false
	}
	wslSp, err := winToWSLPath(sp)
	if err != nil {
		return 0, 0, false
	}
	out, err := exec.Command("wsl.exe", "-d", distro, "bash", wslSp).Output()
	if err != nil {
		return 0, 0, false
	}
	var mt, sz int64
	if _, err := fmt.Sscan(strings.TrimSpace(string(out)), &mt, &sz); err != nil {
		return 0, 0, false
	}
	return mt, sz, true
}

// SnapshotWSLDB copies sessions.db (+ -wal/-shm) from the distro into a
// Windows temp dir and returns that directory. Avoids opening the live
// WSL file over \\wsl.localhost (SQLITE_BUSY on 9p locks).
//
// A bash script file is used because wsl.exe mangles `$var` in inline
// `bash -c` arguments (quotes/vars get eaten before bash runs).
func SnapshotWSLDB(distro string) (string, error) {
	winDir, err := os.MkdirTemp("", "devinmonitor-wsl-"+sanitizeFile(distro)+"-")
	if err != nil {
		return "", err
	}
	wslDir, err := winToWSLPath(winDir)
	if err != nil {
		os.RemoveAll(winDir)
		return "", err
	}
	script := fmt.Sprintf(`#!/bin/bash
set -e
src="$HOME/%s"
dir='%s'
mkdir -p "$dir"
cp -f "$src" "$dir/sessions.db"
if [ -f "$src-wal" ]; then cp -f "$src-wal" "$dir/sessions.db-wal"; fi
if [ -f "$src-shm" ]; then cp -f "$src-shm" "$dir/sessions.db-shm"; fi
echo OK
`, wslDevinDB, wslDir)
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
	cmd := exec.Command("wsl.exe", "-d", distro, "bash", wslScript)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		os.RemoveAll(winDir)
		return "", fmt.Errorf("wsl snapshot %s: %v (%s)", distro, err, strings.TrimSpace(stderr.String()))
	}
	if !strings.Contains(string(out), "OK") {
		os.RemoveAll(winDir)
		return "", fmt.Errorf("wsl snapshot %s: unexpected output %q", distro, out)
	}
	if _, err := os.Stat(filepath.Join(winDir, "sessions.db")); err != nil {
		os.RemoveAll(winDir)
		return "", fmt.Errorf("wsl snapshot %s: sessions.db missing after copy", distro)
	}
	// Drop the helper script so the dir only holds db files.
	_ = os.Remove(scriptPath)
	return winDir, nil
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
