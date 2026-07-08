package libvirt

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const nbdDevice = "/dev/nbd0"

func injectNoCloudSeedQEMUNBD(imagePath, format, workDir string, files []seedFile) error {
	if _, err := exec.LookPath("qemu-nbd"); err != nil {
		return fmt.Errorf("qemu-nbd not found (install qemu-utils): %w", err)
	}
	if err := requireContainerRoot(); err != nil {
		return err
	}
	if err := requireWritableLockDir(); err != nil {
		return err
	}

	return injectNoCloudSeedQEMUNBDLocked(imagePath, format, workDir, files)
}

func injectNoCloudSeedQEMUNBDLocked(imagePath, format, workDir string, files []seedFile) error {
	if err := ensureNBDDevice(); err != nil {
		return err
	}

	args := []string{"--connect=" + nbdDevice}
	if format != "" {
		args = append(args, "-f", format)
	}
	args = append(args, imagePath)

	out, err := exec.Command("qemu-nbd", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("qemu-nbd connect: %w: %s", err, out)
	}
	defer func() {
		_, _ = exec.Command("qemu-nbd", "--disconnect", nbdDevice).CombinedOutput()
	}()

	refreshNBDPartitions()

	rootPart, err := waitForNBDPartition(nbdDevice)
	if err != nil {
		return err
	}

	mountPoint := filepath.Join(workDir, "mnt")
	if err := os.MkdirAll(mountPoint, 0o755); err != nil {
		return fmt.Errorf("create mount point: %w", err)
	}
	defer func() {
		_, _ = exec.Command("umount", mountPoint).CombinedOutput()
		_ = os.Remove(mountPoint)
	}()

	out, err = exec.Command("mount", rootPart, mountPoint).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mount %s: %w: %s", rootPart, err, out)
	}

	for _, file := range files {
		dest := filepath.Join(mountPoint, strings.TrimPrefix(file.remotePath, "/"))
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", filepath.Dir(dest), err)
		}
		if err := copyFile(file.localPath, dest); err != nil {
			return err
		}
	}

	flushImage(imagePath)
	return nil
}

func ensureNBDDevice() error {
	if _, err := os.Stat(nbdDevice); err == nil {
		return nil
	}

	if out, err := exec.Command("modprobe", "nbd", "max_part=8").CombinedOutput(); err != nil {
		return fmt.Errorf("modprobe nbd: %w: %s", err, out)
	}

	if err := waitForDevice(nbdDevice, 5*time.Second); err != nil {
		return fmt.Errorf("%w; ensure the node has nbd available (modprobe nbd max_part=8) and mount host /dev into the pod", err)
	}
	return nil
}

func waitForDevice(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("%s not found", path)
}

func refreshNBDPartitions() {
	_ = exec.Command("partprobe", nbdDevice).Run()
	_ = exec.Command("udevadm", "settle").Run()
}

func flushImage(imagePath string) {
	if file, err := os.OpenFile(imagePath, os.O_RDWR, 0); err == nil {
		_ = file.Sync()
		_ = file.Close()
	}
}

func waitForNBDPartition(device string) (string, error) {
	candidates := []string{
		device + "p1",
		device + "p2",
		device + "1",
		device + "2",
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, part := range candidates {
			if _, err := os.Stat(part); err == nil {
				return part, nil
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return "", fmt.Errorf("nbd root partition not found for %s", device)
}

func copyFile(src, dest string) error {
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open %s: %w", src, err)
	}
	defer in.Close()

	out, err := os.OpenFile(dest, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("create %s: %w", dest, err)
	}
	defer out.Close()

	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copy %s to %s: %w", src, dest, err)
	}
	return nil
}
