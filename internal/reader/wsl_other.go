//go:build !windows

package reader

// WSLDistro is a no-op on non-Windows hosts (Devin already uses XDG paths).
type WSLDistro struct {
	Name        string
	SnapshotDir string
}

// DetectWSLDistros returns nil outside Windows.
func DetectWSLDistros(only []string) []string { return nil }

// SnapshotWSLDB is unsupported outside Windows.
func SnapshotWSLDB(distro string) (string, error) {
	return "", nil
}

// WSLDBStat is unsupported outside Windows.
func WSLDBStat(distro string) (mtime, size int64, ok bool) {
	return 0, 0, false
}

func cleanupDir(dir string) {}
