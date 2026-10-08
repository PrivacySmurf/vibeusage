// Package executil provides helpers for locating external CLI binaries.
package executil

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// ResolveBinary finds a binary by name. It first checks the system PATH
// via exec.LookPath, then probes common installation directories that may
// not be in PATH (e.g. when vibeusage is launched from a GUI, cron, or
// launchd where the shell profile hasn't been sourced).
//
// Returns the resolved absolute path, or "" if not found.
func ResolveBinary(name string) string {
	if p, err := exec.LookPath(name); err == nil {
		return p
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	candidates := []string{
		filepath.Join(home, ".local", "bin", name),
		filepath.Join(home, "bin", name),
	}

	if runtime.GOOS == "darwin" {
		candidates = append(candidates,
			filepath.Join("/opt/homebrew/bin", name),
			filepath.Join("/usr/local/bin", name),
		)
	} else {
		candidates = append(candidates,
			filepath.Join("/usr/local/bin", name),
		)
	}

	for _, p := range candidates {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	return ""
}
