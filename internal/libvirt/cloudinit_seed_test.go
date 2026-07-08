package libvirt

import (
	"fmt"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestBuildNoCloudSeedMovesNetworkToNetworkData(t *testing.T) {
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

	if strings.Contains(seed.UserData, "network:") {
		t.Fatalf("user-data should not contain network section:\n%s", seed.UserData)
	}
	if !strings.Contains(seed.UserData, "runcmd:") {
		t.Fatalf("user-data should preserve bootstrap commands:\n%s", seed.UserData)
	}
	if !strings.Contains(seed.NetworkData, "version: 2") {
		t.Fatalf("network-data missing netplan version:\n%s", seed.NetworkData)
	}
	if !strings.Contains(seed.NetworkData, "enp3s0:") {
		t.Fatalf("network-data missing interface:\n%s", seed.NetworkData)
	}
	if !strings.Contains(seed.MetaData, "instance-id: instance-1") {
		t.Fatalf("meta-data missing instance id:\n%s", seed.MetaData)
	}
	if !strings.Contains(seed.MetaData, "local-hostname: my-host") {
		t.Fatalf("meta-data missing hostname:\n%s", seed.MetaData)
	}
}

func TestBuildNoCloudSeedWithoutNetwork(t *testing.T) {
	userData := "#cloud-config\npackages:\n  - curl\n"

	seed, err := BuildNoCloudSeed(userData, "instance-1", "my-host")
	if err != nil {
		t.Fatal(err)
	}
	if seed.NetworkData != "" {
		t.Fatalf("expected empty network-data, got %q", seed.NetworkData)
	}
	if !strings.Contains(seed.UserData, "packages:") {
		t.Fatalf("user-data not preserved:\n%s", seed.UserData)
	}
}

func TestSplitNetworkFromUserDataPreservesOtherKeys(t *testing.T) {
	userData := `#cloud-config
write_files:
  - path: /etc/motd
    content: hello
network:
  version: 2
  ethernets:
    eth0:
      dhcp4: true
`

	clean, networkData, err := splitNetworkFromUserData(userData)
	if err != nil {
		t.Fatal(err)
	}

	cfg := map[string]any{}
	if err := yaml.Unmarshal([]byte(strings.TrimPrefix(clean, "#cloud-config\n")), &cfg); err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg["network"]; ok {
		t.Fatal("network key should be removed from user-data")
	}
	if _, ok := cfg["write_files"]; !ok {
		t.Fatal("write_files should remain in user-data")
	}
	if !strings.Contains(networkData, "eth0:") {
		t.Fatalf("network-data missing interface:\n%s", networkData)
	}
}

func TestBuildNoCloudSeedRequiresUserData(t *testing.T) {
	if _, err := BuildNoCloudSeed("  ", "id", "name"); err == nil {
		t.Fatal("expected error for empty user-data")
	}
}

func TestLibguestfsToolEnvUsesDirectBackend(t *testing.T) {
	env := libguestfsToolEnv("/data/tmp")
	if !containsEnv(env, "LIBGUESTFS_BACKEND=direct") {
		t.Fatalf("expected direct backend, got %v", env)
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
		{localPath: "/tmp/user-data", remotePath: "/var/lib/cloud/seed/nocloud-net/user-data"},
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
