package libvirt

import (
	"os"
	"testing"
)

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
