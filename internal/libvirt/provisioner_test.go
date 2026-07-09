package libvirt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProvisionerEnabled(t *testing.T) {
	if (&Provisioner{}).Enabled() {
		t.Fatal("empty provisioner should be disabled")
	}
	if !NewProvisioner(Config{Endpoint: "qemu+tcp://example/system"}).Enabled() {
		t.Fatal("provisioner with endpoint should be enabled")
	}
}

func TestVolumeName(t *testing.T) {
	got := volumeName("00000000-0000-4000-8000-000000000000")
	want := "00000000-0000-4000-8000-000000000000-root"
	if got != want {
		t.Fatalf("volumeName() = %q, want %q", got, want)
	}
}

func TestImageFormatFromURL(t *testing.T) {
	tests := []struct {
		url  string
		want string
	}{
		{"https://example.com/image.qcow2", "qcow2"},
		{"https://example.com/image.raw", "raw"},
		{"https://example.com/image.img", "raw"},
	}
	for _, tc := range tests {
		if got := imageFormatFromURL(tc.url); got != tc.want {
			t.Fatalf("imageFormatFromURL(%q) = %q, want %q", tc.url, got, tc.want)
		}
	}
}

func TestDetectImageFormat(t *testing.T) {
	dir := t.TempDir()

	qcow2Path := filepath.Join(dir, "disk.qcow2")
	if err := os.WriteFile(qcow2Path, []byte{0x51, 0x46, 0x49, 0xfb, 0x00}, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := detectImageFormat(qcow2Path); got != "qcow2" {
		t.Fatalf("detectImageFormat(qcow2) = %q, want qcow2", got)
	}

	rawPath := filepath.Join(dir, "disk.raw")
	if err := os.WriteFile(rawPath, []byte{0x00, 0x00, 0x00, 0x00}, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := detectImageFormat(rawPath); got != "raw" {
		t.Fatalf("detectImageFormat(raw) = %q, want raw", got)
	}

	if got := resolveImageFormat(qcow2Path, "raw"); got != "qcow2" {
		t.Fatalf("resolveImageFormat() = %q, want qcow2", got)
	}
}

func TestVolumeCapacity(t *testing.T) {
	got := rootVolumeCapacity(500<<20, 0, 20<<30)
	if got != 20<<30 {
		t.Fatalf("expected 20 GiB minimum root volume, got %d", got)
	}

	got = volumeCapacity(5<<30, 30<<30, 20<<30)
	if got != 30<<30 {
		t.Fatalf("expected image capacity to win over default, got %d", got)
	}
	got = volumeCapacity(25<<30, 10<<30, 20<<30)
	if got != 25<<30 {
		t.Fatalf("expected downloaded size to win, got %d", got)
	}

	got = rootVolumeCapacity(0, 0, 0)
	if got != defaultVolumeBytes {
		t.Fatalf("expected built-in default volume size, got %d", got)
	}
}

func TestVolumeXML(t *testing.T) {
	xml, err := volumeXML("test-vol", 1<<30, "qcow2")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"<name>test-vol</name>", `unit="bytes"`, `type="qcow2"`} {
		if !strings.Contains(xml, want) {
			t.Fatalf("volumeXML missing %q in:\n%s", want, xml)
		}
	}
}

func TestNewProvisionerDefaults(t *testing.T) {
	p := NewProvisioner(Config{Endpoint: "qemu+tcp://example/system"})
	if p.cfg.StoragePool != "default" {
		t.Fatalf("storage pool = %q, want default", p.cfg.StoragePool)
	}
	if p.cfg.DefaultVolumeBytes != defaultVolumeBytes {
		t.Fatalf("default volume bytes = %d, want %d", p.cfg.DefaultVolumeBytes, defaultVolumeBytes)
	}
}

func TestStorageVolResizeFlags(t *testing.T) {
	if got := storageVolResizeFlags("raw"); got != 1 {
		t.Fatalf("raw resize flags = %d, want StorageVolResizeAllocate (1)", got)
	}
	if got := storageVolResizeFlags("qcow2"); got != 0 {
		t.Fatalf("qcow2 resize flags = %d, want 0", got)
	}
}
