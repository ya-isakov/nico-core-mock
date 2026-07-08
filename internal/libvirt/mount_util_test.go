package libvirt

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsWritableDir(t *testing.T) {
	dir := t.TempDir()
	if !isWritableDir(dir) {
		t.Fatal("expected temp dir to be writable")
	}

	readOnly := filepath.Join(dir, "ro")
	if err := os.Mkdir(readOnly, 0o555); err != nil {
		t.Fatal(err)
	}
	if isWritableDir(readOnly) {
		t.Fatal("expected chmod 0555 dir to be non-writable")
	}
}

func TestRequireContainerRoot(t *testing.T) {
	if os.Getuid() == 0 {
		if err := requireContainerRoot(); err != nil {
			t.Fatalf("expected root check to pass: %v", err)
		}
		return
	}
	if err := requireContainerRoot(); err == nil {
		t.Fatal("expected non-root to fail")
	}
}
