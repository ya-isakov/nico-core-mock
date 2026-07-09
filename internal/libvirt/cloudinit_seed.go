package libvirt

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

const (
	cloudCfgDDir         = "/etc/cloud/cloud.cfg.d"
	forgeDSListPath      = cloudCfgDDir + "/98-forge-dslist.cfg"
	forgeUserDataPath    = cloudCfgDDir + "/99-user-data.cfg"
	forgeDSListContent   = "datasource_list: [ NoCloud, None ]\n"
	libguestfsBackend    = "direct"
)

// NoCloudSeed holds cloud-init user-data written into the root disk image.
type NoCloudSeed struct {
	UserData string
}

// BuildNoCloudSeed prepares the NoCloud seed user-data from the instance payload.
// The content is passed through unchanged.
func BuildNoCloudSeed(userData, instanceID, instanceName string) (NoCloudSeed, error) {
	_ = instanceID
	_ = instanceName
	if strings.TrimSpace(userData) == "" {
		return NoCloudSeed{}, fmt.Errorf("user-data is required")
	}
	return NoCloudSeed{UserData: userData}, nil
}

// InjectNoCloudSeed writes cloud-init config into a disk image, matching the
// pre-a6448ebd infra-controller disk_imaging.sh add_cloud_init workflow.
func InjectNoCloudSeed(imagePath, format string, seed NoCloudSeed) error {
	tmpDir, err := os.MkdirTemp("", "nico-nocloud-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	files, err := writeNoCloudSeedFiles(tmpDir, seed)
	if err != nil {
		return err
	}

	if err := requireContainerRoot(); err != nil {
		return err
	}

	var errs []error

	if _, lookErr := exec.LookPath("virt-customize"); lookErr == nil {
		injectErr := injectNoCloudSeedVirtCustomize(imagePath, format, tmpDir, files)
		if injectErr == nil {
			return nil
		}
		if !isLibguestfsFallbackError(injectErr) {
			errs = append(errs, fmt.Errorf("virt-customize: %w", injectErr))
		}
	}

	if _, lookErr := exec.LookPath("guestfish"); lookErr == nil {
		injectErr := injectNoCloudSeedGuestfish(imagePath, format, tmpDir, files)
		if injectErr == nil {
			return nil
		}
		if !isLibguestfsFallbackError(injectErr) {
			errs = append(errs, fmt.Errorf("guestfish: %w", injectErr))
		}
	}

	if len(errs) > 0 {
		return fmt.Errorf("inject nocloud seed failed: %w", errors.Join(errs...))
	}
	return fmt.Errorf("no tool available to inject nocloud seed (need libguestfs-tools)")
}

type seedFile struct {
	localPath  string
	remotePath string
}

func writeNoCloudSeedFiles(tmpDir string, seed NoCloudSeed) ([]seedFile, error) {
	userDataPath := filepath.Join(tmpDir, "99-user-data.cfg")
	dsListPath := filepath.Join(tmpDir, "98-forge-dslist.cfg")
	if err := os.WriteFile(userDataPath, []byte(seed.UserData), 0o644); err != nil {
		return nil, fmt.Errorf("write temp user-data: %w", err)
	}
	if err := os.WriteFile(dsListPath, []byte(forgeDSListContent), 0o644); err != nil {
		return nil, fmt.Errorf("write temp datasource list: %w", err)
	}

	return []seedFile{
		{localPath: dsListPath, remotePath: forgeDSListPath},
		{localPath: userDataPath, remotePath: forgeUserDataPath},
	}, nil
}

func injectNoCloudSeedVirtCustomize(imagePath, format, workDir string, files []seedFile) error {
	args := virtCustomizeArgs(imagePath, format, files)

	cmd := exec.Command("virt-customize", args...)
	cmd.Env = libguestfsToolEnv(workDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("virt-customize inject nocloud seed: %w: %s", err, out)
	}
	return nil
}

func virtCustomizeArgs(imagePath, format string, files []seedFile) []string {
	args := make([]string, 0, 8+len(files))
	if format != "" {
		args = append(args, "--format", format)
	}
	args = append(args, "-a", imagePath,
		"--mkdir", cloudCfgDDir,
	)
	for _, file := range files {
		args = append(args, "--upload", file.localPath+":"+file.remotePath)
	}
	return args
}

func injectNoCloudSeedGuestfish(imagePath, format, workDir string, files []seedFile) error {
	err := runGuestfish(imagePath, format, workDir, files, true)
	if err != nil && strings.Contains(err.Error(), "unrecognized option") {
		return runGuestfish(imagePath, format, workDir, files, false)
	}
	return err
}

func runGuestfish(imagePath, format, workDir string, files []seedFile, useBackendFlag bool) error {
	var script bytes.Buffer
	script.WriteString("run\n")
	script.WriteString("mkdir-p " + cloudCfgDDir + "\n")
	for _, file := range files {
		fmt.Fprintf(&script, "upload %s %s\n", file.localPath, file.remotePath)
	}

	args := []string{"--rw", "-a", imagePath}
	if useBackendFlag {
		args = append([]string{"--backend", libguestfsBackend}, args...)
	}
	if format != "" {
		args = append(args, "-f", format)
	}
	args = append(args, "-i")

	cmd := exec.Command("guestfish", args...)
	cmd.Stdin = &script
	cmd.Env = libguestfsToolEnv(workDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("guestfish inject nocloud seed: %w: %s", err, out)
	}
	return nil
}

func isLibguestfsFallbackError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "supermin") ||
		strings.Contains(msg, "unrecognized option '--backend'")
}

func isSuperminError(err error) bool {
	return isLibguestfsFallbackError(err)
}

func libguestfsToolEnv(workDir string) []string {
	env := unsetEnvVar(os.Environ(), "LIBGUESTFS_HV")
	env = setEnvVar(env, "LIBGUESTFS_BACKEND", libguestfsBackend)
	env = setEnvVar(env, "LIBGUESTFS_SKIP_OS_CHECK", "1")

	tmp := strings.TrimSpace(workDir)
	if tmp == "" {
		tmp = strings.TrimSpace(os.Getenv("TMPDIR"))
	}
	if tmp == "" {
		tmp = os.TempDir()
	}
	env = setEnvVar(env, "TMPDIR", tmp)
	env = setEnvVar(env, "HOME", tmp)
	return env
}

func unsetEnvVar(env []string, key string) []string {
	prefix := key + "="
	filtered := env[:0]
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			continue
		}
		filtered = append(filtered, entry)
	}
	return filtered
}

func setEnvVar(env []string, key, value string) []string {
	prefix := key + "="
	filtered := env[:0]
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) {
			continue
		}
		filtered = append(filtered, entry)
	}
	return append(filtered, prefix+value)
}

func materializeImageWithNoCloudSeed(body io.Reader, imageFormat string, req ProvisionRequest) (io.ReadCloser, int64, string, func(), error) {
	noop := func() {}

	userData := strings.TrimSpace(req.UserData)
	if userData == "" {
		return nil, 0, imageFormat, noop, fmt.Errorf("internal error: materializeImageWithNoCloudSeed called without user-data")
	}

	tempFile, err := os.CreateTemp("", "nico-root-*."+imageFormat)
	if err != nil {
		return nil, 0, imageFormat, noop, fmt.Errorf("create temp image file: %w", err)
	}
	tempPath := tempFile.Name()
	cleanup := func() {
		_ = tempFile.Close()
		_ = os.Remove(tempPath)
	}

	if _, err := io.Copy(tempFile, body); err != nil {
		cleanup()
		return nil, 0, imageFormat, noop, fmt.Errorf("copy image to temp file: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		cleanup()
		return nil, 0, imageFormat, noop, fmt.Errorf("close temp image file: %w", err)
	}

	imageFormat = resolveImageFormat(tempPath, imageFormat)

	seed, err := BuildNoCloudSeed(req.UserData, req.InstanceID, req.InstanceName)
	if err != nil {
		cleanup()
		return nil, 0, imageFormat, noop, err
	}
	if err := InjectNoCloudSeed(tempPath, imageFormat, seed); err != nil {
		cleanup()
		return nil, 0, imageFormat, noop, err
	}

	info, err := os.Stat(tempPath)
	if err != nil {
		cleanup()
		return nil, 0, imageFormat, noop, fmt.Errorf("stat temp image file: %w", err)
	}

	opened, err := os.Open(tempPath)
	if err != nil {
		cleanup()
		return nil, 0, imageFormat, noop, fmt.Errorf("open temp image file: %w", err)
	}

	return opened, info.Size(), imageFormat, cleanup, nil
}
