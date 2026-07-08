package libvirt

import (
	"context"
	"encoding/xml"
	"fmt"
	"path"
	"strings"

	golibvirt "github.com/digitalocean/go-libvirt"
	"github.com/rs/zerolog/log"
)

// ProvisionRequest describes a machine to provision from an OS image URL.
type ProvisionRequest struct {
	MachineID          string
	InstanceID         string
	InstanceName       string
	ImageURL           string
	ImageDigest        string
	ImageCapacityBytes uint64
	UserData           string
}

// Provisioner creates libvirt volumes and starts existing domains for allocated instances.
type Provisioner struct {
	cfg Config
}

// NewProvisioner returns a provisioner for the given libvirt configuration.
func NewProvisioner(cfg Config) *Provisioner {
	if cfg.StoragePool == "" {
		cfg.StoragePool = "default"
	}
	if cfg.DefaultVolumeBytes == 0 {
		cfg.DefaultVolumeBytes = defaultVolumeBytes
	}
	cfg.Endpoint = SanitizeEndpoint(cfg.Endpoint)
	return &Provisioner{cfg: cfg}
}

func (p *Provisioner) Enabled() bool {
	return p != nil && p.cfg.Endpoint != ""
}

// ProvisionMachine downloads an OS image, creates a storage volume, and starts the existing domain.
func (p *Provisioner) ProvisionMachine(ctx context.Context, req ProvisionRequest) error {
	if !p.Enabled() {
		return fmt.Errorf("libvirt provisioner is not configured")
	}

	machineID := canonicalMachineID(req.MachineID)
	if machineID == "" {
		return fmt.Errorf("machine id is required")
	}
	if strings.TrimSpace(req.ImageURL) == "" {
		return p.startExistingDomain(ctx, req)
	}

	log.Info().
		Str("machine_id", machineID).
		Str("image_url", req.ImageURL).
		Msg("starting libvirt provisioning")

	l, err := Connect(p.cfg.Endpoint)
	if err != nil {
		return err
	}
	defer l.Disconnect()

	domain, err := lookupDomainByMachineID(l, machineID)
	if err != nil {
		return err
	}

	if err := stopDomainIfRunning(l, domain, machineID); err != nil {
		return err
	}

	pool, err := l.StoragePoolLookupByName(p.cfg.StoragePool)
	if err != nil {
		return fmt.Errorf("lookup storage pool %q: %w", p.cfg.StoragePool, err)
	}

	volName := volumeName(machineID)
	if err := deleteVolumeIfExists(l, pool, volName); err != nil {
		return err
	}

	imageFormat := imageFormatFromURL(req.ImageURL)
	imageSize, body, err := openCachedOrDownloadImage(ctx, req.ImageURL, req.ImageDigest, p.cfg.ImageCacheDir)
	if err != nil {
		return err
	}
	defer body.Close()

	if strings.TrimSpace(req.UserData) != "" {
		var cleanupImage func()
		body, imageSize, cleanupImage, err = materializeImageWithNoCloudSeed(body, imageFormat, req)
		if err != nil {
			return err
		}
		defer cleanupImage()
		defer body.Close()
	}

	volCapacity := rootVolumeCapacity(imageSize, req.ImageCapacityBytes, p.cfg.DefaultVolumeBytes)
	vol, err := createVolume(l, pool, volName, volCapacity, imageFormat)
	if err != nil {
		return err
	}

	if err := l.StorageVolUpload(vol, body, 0, 0, 0); err != nil {
		_ = l.StorageVolDelete(vol, 0)
		return fmt.Errorf("upload image to volume %q: %w", volName, err)
	}

	if err := expandRootVolume(l, vol, volCapacity, imageFormat); err != nil {
		_ = l.StorageVolDelete(vol, 0)
		return fmt.Errorf("expand root volume %q to %d bytes: %w", volName, volCapacity, err)
	}

	log.Info().
		Str("machine_id", machineID).
		Str("volume", volName).
		Uint64("capacity_bytes", volCapacity).
		Str("format", imageFormat).
		Msg("created root volume")

	volPath, err := l.StorageVolGetPath(vol)
	if err != nil {
		return fmt.Errorf("get volume path for %q: %w", volName, err)
	}

	domain, err = updateDomainBootDisk(l, domain, volPath, imageFormat, p.cfg.StoragePool, volName)
	if err != nil {
		return err
	}

	domain, err = p.cleanupConfigDrive(l, pool, domain, machineID)
	if err != nil {
		return err
	}

	if strings.TrimSpace(req.UserData) != "" {
		log.Info().
			Str("machine_id", machineID).
			Msg("injected nocloud seed into root disk image")
	}

	if err := startDomain(l, domain, machineID); err != nil {
		return err
	}

	log.Info().
		Str("machine_id", machineID).
		Str("volume", volName).
		Str("volume_path", volPath).
		Msg("libvirt provisioning complete")

	return nil
}

// ReleaseMachine stops the existing domain and deletes its root volume.
func (p *Provisioner) ReleaseMachine(ctx context.Context, machineID string) error {
	if !p.Enabled() {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	machineID = canonicalMachineID(machineID)
	if machineID == "" {
		return nil
	}

	l, err := Connect(p.cfg.Endpoint)
	if err != nil {
		return err
	}
	defer l.Disconnect()

	if domain, err := lookupDomainByMachineID(l, machineID); err == nil {
		if err := stopDomainIfRunning(l, domain, machineID); err != nil {
			log.Warn().Err(err).Str("machine_id", machineID).Msg("failed to stop libvirt domain")
		}
	}

	pool, err := l.StoragePoolLookupByName(p.cfg.StoragePool)
	if err != nil {
		return fmt.Errorf("lookup storage pool %q: %w", p.cfg.StoragePool, err)
	}

	volName := volumeName(machineID)
	if err := deleteVolumeIfExists(l, pool, volName); err != nil {
		return err
	}
	if err := deleteVolumeIfExists(l, pool, configDriveVolumeName(machineID)); err != nil {
		return err
	}

	log.Info().Str("machine_id", machineID).Msg("released libvirt volume")
	return nil
}

func (p *Provisioner) startExistingDomain(ctx context.Context, req ProvisionRequest) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	machineID := canonicalMachineID(req.MachineID)
	log.Info().Str("machine_id", machineID).Msg("starting existing libvirt domain without os image")

	l, err := Connect(p.cfg.Endpoint)
	if err != nil {
		return err
	}
	defer l.Disconnect()

	domain, err := lookupDomainByMachineID(l, machineID)
	if err != nil {
		return err
	}

	pool, err := l.StoragePoolLookupByName(p.cfg.StoragePool)
	if err != nil {
		return fmt.Errorf("lookup storage pool %q: %w", p.cfg.StoragePool, err)
	}

	if strings.TrimSpace(req.UserData) != "" {
		log.Warn().
			Str("machine_id", machineID).
			Msg("userdata provided without image URL; cloud-init seed not injected (reprovision with image to apply)")
	}

	domain, err = p.cleanupConfigDrive(l, pool, domain, machineID)
	if err != nil {
		return err
	}

	if err := startDomain(l, domain, machineID); err != nil {
		return err
	}

	log.Info().Str("machine_id", machineID).Msg("libvirt domain started")
	return nil
}

func (p *Provisioner) cleanupConfigDrive(l *golibvirt.Libvirt, pool golibvirt.StoragePool, domain golibvirt.Domain, machineID string) (golibvirt.Domain, error) {
	if err := deleteVolumeIfExists(l, pool, configDriveVolumeName(machineID)); err != nil {
		return golibvirt.Domain{}, err
	}
	return removeDomainConfigDrive(l, domain)
}

func deleteVolumeIfExists(l *golibvirt.Libvirt, pool golibvirt.StoragePool, name string) error {
	vol, err := l.StorageVolLookupByName(pool, name)
	if err != nil {
		return nil
	}
	if err := l.StorageVolDelete(vol, 0); err != nil {
		return fmt.Errorf("delete volume %q: %w", name, err)
	}
	return nil
}

func createVolume(l *golibvirt.Libvirt, pool golibvirt.StoragePool, name string, capacity uint64, format string) (golibvirt.StorageVol, error) {
	xmlDesc, err := volumeXML(name, capacity, format)
	if err != nil {
		return golibvirt.StorageVol{}, err
	}

	vol, err := l.StorageVolCreateXML(pool, xmlDesc, 0)
	if err != nil {
		return golibvirt.StorageVol{}, fmt.Errorf("create volume %q: %w", name, err)
	}
	return vol, nil
}

func rootVolumeCapacity(imageSize int64, imageCapacityBytes, defaultBytes uint64) uint64 {
	if defaultBytes == 0 {
		defaultBytes = defaultVolumeBytes
	}

	capacity := defaultBytes
	if imageCapacityBytes > capacity {
		capacity = imageCapacityBytes
	}
	if imageSize > 0 && uint64(imageSize) > capacity {
		capacity = uint64(imageSize)
	}
	return capacity
}

func expandRootVolume(l *golibvirt.Libvirt, vol golibvirt.StorageVol, capacity uint64, format string) error {
	if capacity == 0 {
		return nil
	}
	switch format {
	case "qcow2", "raw":
	default:
		return nil
	}
	flags := storageVolResizeFlags(format)
	if err := l.StorageVolResize(vol, capacity, flags); err != nil {
		return err
	}
	return nil
}

// storageVolResizeFlags returns libvirt resize flags appropriate for the volume format.
// Preallocation (StorageVolResizeAllocate) is only supported for raw volumes.
func storageVolResizeFlags(format string) golibvirt.StorageVolResizeFlags {
	if format == "raw" {
		return golibvirt.StorageVolResizeAllocate
	}
	return 0
}

func volumeCapacity(imageSize int64, imageCapacityBytes, defaultBytes uint64) uint64 {
	return rootVolumeCapacity(imageSize, imageCapacityBytes, defaultBytes)
}

func volumeName(machineID string) string {
	return machineID + "-root"
}

func configDriveVolumeName(machineID string) string {
	return machineID + "-config"
}

func imageFormatFromURL(imageURL string) string {
	ext := strings.ToLower(path.Ext(imageURL))
	switch ext {
	case ".qcow2":
		return "qcow2"
	case ".raw":
		return "raw"
	default:
		return "qcow2"
	}
}

type volumeSpec struct {
	XMLName  xml.Name `xml:"volume"`
	Name     string   `xml:"name"`
	Capacity struct {
		Value uint64 `xml:",chardata"`
		Unit  string `xml:"unit,attr"`
	} `xml:"capacity"`
	Target struct {
		Format struct {
			Type string `xml:"type,attr"`
		} `xml:"format"`
	} `xml:"target"`
}

func volumeXML(name string, capacity uint64, format string) (string, error) {
	spec := volumeSpec{Name: name}
	spec.Capacity.Value = capacity
	spec.Capacity.Unit = "bytes"
	spec.Target.Format.Type = format

	out, err := xml.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("marshal volume xml: %w", err)
	}
	return string(out), nil
}
