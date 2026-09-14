//go:build !windows

package reader

// WSLDistro is a no-op on non-Windows hosts (Devin already uses XDG paths).
type WSLDistro struct {
	Name        string
	SnapshotDir string
}

// DetectWSLDistros returns nil outside Windows.
func DetectWSLDistros(only []string) []string { return nil }

// ProbeWSLServices returns empty hits outside Windows.
func ProbeWSLServices(distro string) ProbeWSLHits { return ProbeWSLHits{} }

// SnapshotWSLDB is unsupported outside Windows.
func SnapshotWSLDB(distro string) (string, error) {
	return "", nil
}

// SnapshotWSLMiMo is unsupported outside Windows.
func SnapshotWSLMiMo(distro string) (string, error) {
	return "", nil
}

// WSLDBStat is unsupported outside Windows.
func WSLDBStat(distro string) (mtime, size int64, ok bool) {
	return 0, 0, false
}

// WSLMiMoStat is unsupported outside Windows.
func WSLMiMoStat(distro string) (mtime, size int64, ok bool) {
	return 0, 0, false
}

// WSLStatFile is unsupported outside Windows.
func WSLStatFile(distro, relHome string) (mtime, size int64, ok bool) {
	return 0, 0, false
}

func cleanupDir(dir string) {}
