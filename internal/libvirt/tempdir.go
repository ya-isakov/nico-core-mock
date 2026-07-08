package libvirt

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ResolveTempDir returns the directory used for temporary files such as guestfish staging.
func ResolveTempDir(explicit, stateFile string) string {
	if dir := strings.TrimSpace(explicit); dir != "" {
		return dir
	}
	if stateFile := strings.TrimSpace(stateFile); stateFile != "" {
		return filepath.Join(filepath.Dir(stateFile), "tmp")
	}
	if dir := strings.TrimSpace(os.Getenv("TMPDIR")); dir != "" {
		return dir
	}
	return os.TempDir()
}

// ConfigureWritableTempDir ensures TMPDIR points at a writable directory.
func ConfigureWritableTempDir(explicit, stateFile string) (string, error) {
	dir := ResolveTempDir(explicit, stateFile)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create temp dir %q: %w", dir, err)
	}
	if err := os.Setenv("TMPDIR", dir); err != nil {
		return "", fmt.Errorf("set TMPDIR: %w", err)
	}
	return dir, nil
}
