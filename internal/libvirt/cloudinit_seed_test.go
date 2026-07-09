package libvirt

import (
	"fmt"
	"strings"
	"testing"
)

func TestBuildNoCloudSeedPreservesRawUserData(t *testing.T) {
	userData := `#cloud-config
autoinstall:
  version: 1
network:
  version: 2
  renderer: networkd
  ethernets:
    enp3s0:
      dhcp4: false
      addresses:
        - 192.168.1.100/24
runcmd:
  - echo bootstrap
`

	seed, err := BuildNoCloudSeed(userData, "instance-1", "my-host")
	if err != nil {
		t.Fatal(err)
	}

	if seed.UserData != userData {
		t.Fatalf("user-data should be passed through unchanged:\n%s", seed.UserData)
	}
	if strings.Contains(seed.UserData, "runcmd:") == false {
		t.Fatalf("user-data should preserve bootstrap commands:\n%s", seed.UserData)
	}
	if !strings.Contains(seed.UserData, "network:") {
		t.Fatalf("user-data should keep network section:\n%s", seed.UserData)
	}
}

func TestBuildNoCloudSeedRequiresUserData(t *testing.T) {
	if _, err := BuildNoCloudSeed("  ", "id", "name"); err == nil {
		t.Fatal("expected error for empty user-data")
	}
}

func TestWriteNoCloudSeedFilesOnlyUserData(t *testing.T) {
	tmpDir := t.TempDir()
	files, err := writeNoCloudSeedFiles(tmpDir, NoCloudSeed{UserData: "#cloud-config\npackages:\n  - curl\n"})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("expected 2 seed files, got %d: %+v", len(files), files)
	}
	if files[1].remotePath != forgeUserDataPath {
		t.Fatalf("unexpected user-data path: %s", files[1].remotePath)
	}
}

func TestLibguestfsToolEnvUsesDirectBackend(t *testing.T) {
	t.Setenv("LIBGUESTFS_HV", "/usr/bin/qemu-system-x86_64")

	env := libguestfsToolEnv("/data/tmp")
	if !containsEnv(env, "LIBGUESTFS_BACKEND=direct") {
		t.Fatalf("expected direct backend, got %v", env)
	}
	if !containsEnv(env, "LIBGUESTFS_SKIP_OS_CHECK=1") {
		t.Fatalf("expected LIBGUESTFS_SKIP_OS_CHECK, got %v", env)
	}
	if containsEnv(env, "LIBGUESTFS_HV=/usr/bin/qemu-system-x86_64") {
		t.Fatalf("LIBGUESTFS_HV should not be set, got %v", env)
	}
	if !containsEnv(env, "TMPDIR=/data/tmp") {
		t.Fatalf("expected TMPDIR, got %v", env)
	}
	if !containsEnv(env, "HOME=/data/tmp") {
		t.Fatalf("expected HOME, got %v", env)
	}
}

func TestVirtCustomizeArgsFormatBeforeDisk(t *testing.T) {
	args := virtCustomizeArgs("/tmp/disk.qcow2", "qcow2", []seedFile{
		{localPath: "/tmp/99-user-data.cfg", remotePath: forgeUserDataPath},
	})
	if len(args) < 4 || args[0] != "--format" || args[2] != "-a" {
		t.Fatalf("expected --format before -a, got %v", args)
	}
}

func TestIsSuperminError(t *testing.T) {
	if !isSuperminError(fmt.Errorf("guestfish: supermin exited with error")) {
		t.Fatal("expected supermin error detection")
	}
	if !isLibguestfsFallbackError(fmt.Errorf("virt-customize: unrecognized option '--backend'")) {
		t.Fatal("expected unrecognized backend detection")
	}
	if isSuperminError(fmt.Errorf("other error")) {
		t.Fatal("did not expect supermin detection")
	}
}

func containsEnv(env []string, want string) bool {
	for _, entry := range env {
		if entry == want {
			return true
		}
	}
	return false
}
