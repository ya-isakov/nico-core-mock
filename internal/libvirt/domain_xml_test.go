package libvirt

import (
	"strings"
	"testing"
)

func TestPatchDomainBootDiskXML(t *testing.T) {
	const domainXML = `<domain type='kvm'>
  <name>00000000-0000-4000-8000-000000000000</name>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='raw'/>
      <source file='/var/lib/libvirt/images/old.qcow2'/>
      <target dev='vda' bus='virtio'/>
    </disk>
    <disk type='file' device='cdrom'>
      <target dev='hda' bus='ide'/>
    </disk>
  </devices>
</domain>`

	updated, err := patchDomainBootDiskXML(
		domainXML,
		"/var/lib/libvirt/images/new.qcow2",
		"qcow2",
		"default",
		"00000000-0000-4000-8000-000000000000-root",
	)
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		`type='volume'`,
		`type='qcow2'`,
		`pool='default'`,
		`volume='00000000-0000-4000-8000-000000000000-root'`,
		`device='cdrom'`,
		`<boot order='1'/>`,
	} {
		if !strings.Contains(updated, want) {
			t.Fatalf("patched xml missing %q:\n%s", want, updated)
		}
	}
	if strings.Contains(updated, "old.qcow2") {
		t.Fatalf("patched xml still references old disk:\n%s", updated)
	}
}

func TestPatchDomainBootDiskXMLPrefersBootOrder(t *testing.T) {
	const domainXML = `<domain type='kvm'>
  <devices>
    <disk type='file' device='disk'>
      <driver name='qemu' type='raw'/>
      <source file='/old-secondary'/>
      <boot order='2'/>
    </disk>
    <disk type='file' device='disk'>
      <driver name='qemu' type='raw'/>
      <source file='/old-primary'/>
      <boot order='1'/>
    </disk>
  </devices>
</domain>`

	updated, err := patchDomainBootDiskXML(domainXML, "/new", "qcow2", "default", "machine-root")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(updated, "old-primary") {
		t.Fatalf("expected primary boot disk to be updated:\n%s", updated)
	}
	if !strings.Contains(updated, "old-secondary") {
		t.Fatalf("expected secondary disk to remain unchanged:\n%s", updated)
	}
	if !strings.Contains(updated, `<boot order='1'/>`) {
		t.Fatalf("expected boot order on primary disk:\n%s", updated)
	}
}

func TestEnsureOSBootFromDisk(t *testing.T) {
	const domainXML = `<domain type='kvm'>
  <os>
    <type arch='x86_64' machine='pc'>hvm</type>
    <boot dev='network'/>
  </os>
  <devices>
    <disk type='file' device='disk'>
      <source file='/root.qcow2'/>
    </disk>
  </devices>
</domain>`

	updated, err := patchDomainBootDiskXML(domainXML, "/new", "qcow2", "default", "machine-root")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(updated, `dev='network'`) {
		t.Fatalf("expected network boot removed:\n%s", updated)
	}
	if !strings.Contains(updated, `<boot dev='hd'/>`) {
		t.Fatalf("expected disk boot in os section:\n%s", updated)
	}
}

func TestPatchDomainBootDiskXMLErrorsWithoutDisk(t *testing.T) {
	_, err := patchDomainBootDiskXML(`<domain><devices></devices></domain>`, "/new", "qcow2", "default", "vol")
	if err == nil {
		t.Fatal("expected error when domain has no disks")
	}
}

func TestSetAttribute(t *testing.T) {
	got := setAttribute(`<disk type='file' device='disk'>`, "type", "volume")
	if got != `<disk type='volume' device='disk'>` {
		t.Fatalf("setAttribute() = %q", got)
	}

	got = setAttribute(`<disk type="file" device="disk">`, "type", "volume")
	if got != `<disk type="volume" device="disk">` {
		t.Fatalf("setAttribute() with double quotes = %q", got)
	}

	got = setAttribute(`<disk device='disk'>`, "type", "volume")
	if got != `<disk device='disk' type='volume'>` {
		t.Fatalf("setAttribute() insert = %q", got)
	}
}

func TestPatchDomainConfigDriveXMLInsertsCDROM(t *testing.T) {
	const domainXML = `<domain type='kvm'>
  <devices>
    <disk type='file' device='disk'>
      <source file='/root.qcow2'/>
    </disk>
  </devices>
</domain>`

	updated, err := patchDomainConfigDriveXML(domainXML, "default", "machine-config")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`device='cdrom'`,
		`pool='default'`,
		`volume='machine-config'`,
		`bus='sata'`,
		`<readonly/>`,
	} {
		if !strings.Contains(updated, want) {
			t.Fatalf("patched xml missing %q:\n%s", want, updated)
		}
	}
}

func TestPatchDomainConfigDriveXMLUpdatesExistingCDROM(t *testing.T) {
	const domainXML = `<domain type='kvm'>
  <devices>
    <disk type='file' device='cdrom'>
      <source file='/old-config.iso'/>
      <target dev='hda' bus='ide'/>
    </disk>
  </devices>
</domain>`

	updated, err := patchDomainConfigDriveXML(domainXML, "default", "machine-config")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(updated, "old-config.iso") {
		t.Fatalf("expected cdrom source to be replaced:\n%s", updated)
	}
	if !strings.Contains(updated, `volume='machine-config'`) {
		t.Fatalf("expected config volume attached:\n%s", updated)
	}
}

func TestPatchDomainConfigDriveXMLUsesSATAWithVirtioBootDisk(t *testing.T) {
	const domainXML = `<domain type='kvm'>
  <os>
    <type machine='q35'>hvm</type>
  </os>
  <devices>
    <disk type='file' device='disk'>
      <source file='/root.qcow2'/>
      <target dev='vda' bus='virtio'/>
    </disk>
    <disk type='file' device='cdrom'>
      <source file='/old-config.iso'/>
      <target dev='hda' bus='ide'/>
    </disk>
  </devices>
</domain>`

	updated, err := patchDomainConfigDriveXML(domainXML, "default", "machine-config")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(updated, `bus='ide'`) {
		t.Fatalf("expected ide cdrom bus to be upgraded:\n%s", updated)
	}
	if !strings.Contains(updated, `bus='sata'`) {
		t.Fatalf("expected sata cdrom bus:\n%s", updated)
	}
	if !strings.Contains(updated, `volume='machine-config'`) {
		t.Fatalf("expected config volume attached:\n%s", updated)
	}
}
