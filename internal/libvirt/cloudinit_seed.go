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

	"gopkg.in/yaml.v3"
)

const (
	noCloudSeedDir     = "/var/lib/cloud/seed/nocloud-net"
	forgeDSListPath    = "/etc/cloud/cloud.cfg.d/98-forge-dslist.cfg"
	forgeDSListContent = "datasource_list: [ NoCloud, None ]\n"
	cloudConfigHeader  = "#cloud-config"
	libguestfsBackend  = "direct"
)

// NoCloudSeed holds cloud-init files written into the root disk image.
type NoCloudSeed struct {
	UserData    string
	MetaData    string
	NetworkData string
}

// BuildNoCloudSeed prepares NoCloud seed files from instance userdata.
// Network configuration is moved from user-data into network-data when present.
func BuildNoCloudSeed(userData, instanceID, instanceName string) (NoCloudSeed, error) {
	userData = strings.TrimSpace(userData)
	if userData == "" {
		return NoCloudSeed{}, fmt.Errorf("user-data is required")
	}
	instanceID = strings.TrimSpace(instanceID)
	if instanceID == "" {
		return NoCloudSeed{}, fmt.Errorf("instance id is required")
	}

	cleanUserData, networkData, err := splitNetworkFromUserData(userData)
	if err != nil {
		return NoCloudSeed{}, err
	}

	var meta strings.Builder
	meta.WriteString("instance-id: ")
	meta.WriteString(instanceID)
	meta.WriteByte('\n')
	if name := strings.TrimSpace(instanceName); name != "" {
		meta.WriteString("local-hostname: ")
		meta.WriteString(name)
		meta.WriteByte('\n')
	}

	return NoCloudSeed{
		UserData:    cleanUserData,
		MetaData:    meta.String(),
		NetworkData: networkData,
	}, nil
}

func splitNetworkFromUserData(userData string) (cleanUserData, networkData string, err error) {
	body := strings.TrimSpace(userData)
	if strings.HasPrefix(body, cloudConfigHeader) {
		body = strings.TrimSpace(strings.TrimPrefix(body, cloudConfigHeader))
	}

	cfg := map[string]any{}
	if body != "" {
		if err := yaml.Unmarshal([]byte(body), &cfg); err != nil {
			return "", "", fmt.Errorf("parse user-data: %w", err)
		}
	}

	network, hasNetwork := cfg["network"]
	if !hasNetwork {
		return ensureCloudConfigHeader(userData), "", nil
	}
	delete(cfg, "network")

	networkYAML, err := yaml.Marshal(network)
	if err != nil {
		return "", "", fmt.Errorf("marshal network-data: %w", err)
	}

	out, err := yaml.Marshal(cfg)
	if err != nil {
		return "", "", fmt.Errorf("marshal user-data: %w", err)
	}
	cleanUserData = formatCloudConfigBody(string(out))

	networkData = strings.TrimRight(string(networkYAML), "\n") + "\n"
	return cleanUserData, networkData, nil
}

func ensureCloudConfigHeader(userData string) string {
	body := strings.TrimSpace(userData)
	if body == "" {
		return formatCloudConfigBody("")
	}
	if strings.HasPrefix(body, cloudConfigHeader) {
		rest := strings.TrimSpace(strings.TrimPrefix(body, cloudConfigHeader))
		return formatCloudConfigBody(rest)
	}
	return formatCloudConfigBody(body)
}

func formatCloudConfigBody(body string) string {
	body = strings.TrimSpace(body)
	if body == "" {
		return cloudConfigHeader + "\n"
	}
	return cloudConfigHeader + "\n" + body + "\n"
}

// InjectNoCloudSeed writes cloud-init seed files into a disk image, matching the
// infra-controller disk_imaging.sh add_cloud_init workflow.
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
	userDataPath := filepath.Join(tmpDir, "user-data")
	metaDataPath := filepath.Join(tmpDir, "meta-data")
	dsListPath := filepath.Join(tmpDir, "98-forge-dslist.cfg")
	if err := os.WriteFile(userDataPath, []byte(seed.UserData), 0o644); err != nil {
		return nil, fmt.Errorf("write temp user-data: %w", err)
	}
	if err := os.WriteFile(metaDataPath, []byte(seed.MetaData), 0o644); err != nil {
		return nil, fmt.Errorf("write temp meta-data: %w", err)
	}
	if err := os.WriteFile(dsListPath, []byte(forgeDSListContent), 0o644); err != nil {
		return nil, fmt.Errorf("write temp datasource list: %w", err)
	}

	files := []seedFile{
		{localPath: dsListPath, remotePath: forgeDSListPath},
		{localPath: userDataPath, remotePath: noCloudSeedDir + "/user-data"},
		{localPath: metaDataPath, remotePath: noCloudSeedDir + "/meta-data"},
	}
	if networkData := strings.TrimSpace(seed.NetworkData); networkData != "" {
		networkDataPath := filepath.Join(tmpDir, "network-data")
		if err := os.WriteFile(networkDataPath, []byte(networkData), 0o644); err != nil {
			return nil, fmt.Errorf("write temp network-data: %w", err)
		}
		files = append(files, seedFile{
			localPath:  networkDataPath,
			remotePath: noCloudSeedDir + "/network-data",
		})
	}
	return files, nil
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
		"--mkdir", filepath.ToSlash(filepath.Dir(forgeDSListPath)),
		"--mkdir", noCloudSeedDir,
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
	script.WriteString("mkdir-p " + filepath.ToSlash(filepath.Dir(forgeDSListPath)) + "\n")
	script.WriteString("mkdir-p " + noCloudSeedDir + "\n")
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

	instanceID := strings.TrimSpace(req.InstanceID)
	if instanceID == "" {
		instanceID = canonicalMachineID(req.MachineID)
	}

	seed, err := BuildNoCloudSeed(userData, instanceID, req.InstanceName)
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
