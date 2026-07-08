package libvirt

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"

	iso9660 "github.com/kdomanski/iso9660"
)

func TestBuildConfigDriveISO(t *testing.T) {
	userData := `#cloud-config
autoinstall:
  version: 1
network:
  version: 2
  ethernets:
    enp3s0:
      dhcp4: false
      addresses:
        - 192.168.1.100/24
runcmd:
  - echo bootstrap
`

	isoBytes, err := BuildConfigDriveISO(userData, "instance-1", "my-host")
	if err != nil {
		t.Fatal(err)
	}
	if len(isoBytes) == 0 {
		t.Fatal("expected non-empty iso")
	}

	image, err := iso9660.OpenImage(bytes.NewReader(isoBytes))
	if err != nil {
		t.Fatal(err)
	}

	label, err := image.Label()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(label) != configDriveVolumeLabel {
		t.Fatalf("volume label = %q, want %q", label, configDriveVolumeLabel)
	}

	metaReader, err := openISOFile(image, "openstack", "latest", "meta_data.json")
	if err != nil {
		t.Fatal(err)
	}
	var meta map[string]string
	if err := json.NewDecoder(metaReader).Decode(&meta); err != nil {
		t.Fatal(err)
	}
	if meta["uuid"] != "instance-1" {
		t.Fatalf("meta uuid = %q", meta["uuid"])
	}

	userReader, err := openISOFile(image, "openstack", "latest", "user_data")
	if err != nil {
		t.Fatal(err)
	}
	gotUser, err := io.ReadAll(userReader)
	if err != nil {
		t.Fatal(err)
	}
	userText := string(gotUser)
	if strings.Contains(userText, "network:") {
		t.Fatalf("user_data should not contain network section:\n%s", userText)
	}
	if !strings.Contains(userText, "runcmd:") {
		t.Fatalf("user_data should preserve bootstrap commands:\n%s", userText)
	}

	networkReader, err := openISOFile(image, "openstack", "latest", "network_data.json")
	if err != nil {
		t.Fatal(err)
	}
	gotNetwork, err := io.ReadAll(networkReader)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(gotNetwork), "enp3s0:") {
		t.Fatalf("network_data missing interface:\n%s", string(gotNetwork))
	}
}

func openISOFile(image *iso9660.Image, path ...string) (io.Reader, error) {
	root, err := image.RootDir()
	if err != nil {
		return nil, err
	}

	current := root
	for _, part := range path[:len(path)-1] {
		current, err = findISOChild(current, part)
		if err != nil {
			return nil, err
		}
	}

	file, err := findISOChild(current, path[len(path)-1])
	if err != nil {
		return nil, err
	}
	return file.Reader(), nil
}

func findISOChild(dir *iso9660.File, name string) (*iso9660.File, error) {
	children, err := dir.GetAllChildren()
	if err != nil {
		return nil, err
	}
	for _, child := range children {
		if strings.EqualFold(child.Name(), name) {
			return child, nil
		}
	}
	return nil, io.EOF
}
