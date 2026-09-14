package reader

import (
	"fmt"
	"os"
)

// SourceKind identifies which AI CLI store was found.
type SourceKind string

const (
	KindDevin    SourceKind = "devin"
	KindMiMo     SourceKind = "mimo"
	KindOpenCode SourceKind = "opencode"
)

// DiscoveredSource is one AI session database found on this machine.
// Path for WSL sources is the in-distro path; OpenWith snapshots it.
type DiscoveredSource struct {
	Kind   SourceKind
	Host   string // "windows" | "unix" | "wsl:<distro>"
	Path   string // local filesystem path, or in-distro relative path for WSL
	Distro string // set for WSL
	Label  string
}

// ProbeWSLHits lists which supported AI stores exist in a WSL distro.
type ProbeWSLHits struct {
	Devin    bool
	MiMo     bool
	OpenCode bool
}

// DiscoverSources scans the local host and (on Windows) every WSL distro for
// supported AI session stores (Devin, MiMoCode, OpenCode). Lightweight:
// probes file existence only — callers snapshot/open as needed.
func DiscoverSources(onlyDistros []string, includeWSL, includeMiMo bool) []DiscoveredSource {
	if os.Getenv("DEVIN_NO_WSL") == "1" {
		includeWSL = false
	}
	if os.Getenv("DEVIN_NO_MIMO") == "1" {
		includeMiMo = false
	}
	includeOpenCode := os.Getenv("DEVIN_NO_OPENCODE") != "1"

	var out []DiscoveredSource
	host := localHostTag()

	// Local Devin (platform defaults + env overrides).
	if p, err := ResolveDBPath(""); err == nil && p != "" {
		out = append(out, DiscoveredSource{
			Kind: KindDevin, Host: host, Path: p, Label: "local",
		})
	}
	// Local MiMoCode.
	if includeMiMo {
		if mp := ResolveMiMoDBPath(""); mp != "" {
			out = append(out, DiscoveredSource{
				Kind: KindMiMo, Host: host, Path: mp, Label: "mimo",
			})
		}
	}
	// Local OpenCode.
	if includeOpenCode {
		if op := ResolveOpenCodeDBPath(""); op != "" {
			out = append(out, DiscoveredSource{
				Kind: KindOpenCode, Host: host, Path: op, Label: "opencode",
			})
		}
	}

	if !includeWSL {
		return out
	}

	for _, d := range DetectWSLDistros(onlyDistros) {
		hits := ProbeWSLServices(d)
		if hits.Devin {
			out = append(out, DiscoveredSource{
				Kind: KindDevin, Host: "wsl:" + d, Path: wslDevinDB,
				Distro: d, Label: "wsl:" + d,
			})
		}
		if includeMiMo && hits.MiMo {
			out = append(out, DiscoveredSource{
				Kind: KindMiMo, Host: "wsl:" + d, Path: wslMiMoDB,
				Distro: d, Label: "wsl-mimo:" + d,
			})
		}
		if includeOpenCode && hits.OpenCode {
			out = append(out, DiscoveredSource{
				Kind: KindOpenCode, Host: "wsl:" + d, Path: wslOpenCodeDB,
				Distro: d, Label: "wsl-opencode:" + d,
			})
		}
	}
	return out
}

func localHostTag() string {
	if os.Getenv("WINDIR") != "" || os.Getenv("SystemRoot") != "" {
		return "windows"
	}
	return "unix"
}

// Describe returns a one-line summary for logging.
func (s DiscoveredSource) Describe() string {
	return fmt.Sprintf("%s [%s] %s", s.Label, s.Kind, s.Path)
}
