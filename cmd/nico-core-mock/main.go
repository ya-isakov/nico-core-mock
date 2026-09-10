package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"golang.org/x/term"

	"github.com/jumpojoy/nico-core-mock/internal/config"
	"github.com/jumpojoy/nico-core-mock/internal/libvirt"
	"github.com/jumpojoy/nico-core-mock/internal/server"
)

func main() {
	configPath := flag.String("config", "/config/values.yaml", "path to machines YAML (plain machines: or Helm values with inventory:)")
	listenAddr := flag.String("listen", ":11079", "gRPC listen address")
	logLevel := flag.String("log-level", "debug", "log level: trace, debug, info, warn, error")
	libvirtEndpoint := flag.String("libvirt-endpoint", "", "libvirt URI (e.g. qemu+tcp://host:16509/system); when set, only inventory machines with a matching libvirt domain are exposed")
	libvirtStoragePool := flag.String("libvirt-storage-pool", "default", "libvirt storage pool used for instance root volumes")
	libvirtVolumeGiB := flag.Uint("libvirt-volume-gib", 20, "default root volume size in GiB when OS image capacity is unknown")
	stateFile := flag.String("state-file", "", "path to JSON file for persisting mutable Forge state across restarts")
	imageCacheDir := flag.String("image-cache-dir", "", "directory for cached OS images; defaults to <state-file-dir>/os-image-cache when state-file is set")
	tempDir := flag.String("temp-dir", "", "writable directory for temporary files; defaults to <state-file-dir>/tmp when state-file is set")
	flag.Parse()

	initLogging(resolveLogLevel(*logLevel))

	if tempDirPath, err := libvirt.ConfigureWritableTempDir(*tempDir, *stateFile); err != nil {
		log.Fatal().Err(err).Msg("failed to configure writable temp directory")
	} else if tempDirPath != "" {
		log.Info().Str("temp_dir", tempDirPath).Msg("configured writable temp directory")
	}

	inventory, err := config.Load(*configPath)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to load machines config")
	}

	log.Debug().
		Str("config", *configPath).
		Int("machines", len(inventory.Machines)).
		Int("expected_machines", len(inventory.ExpectedMachines)).
		Msg("loaded machines config")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	powerChecker := libvirt.PowerChecker(libvirt.NoopChecker{})
	var provisioner *libvirt.Provisioner
	if *libvirtEndpoint != "" {
		cacheDir := strings.TrimSpace(*imageCacheDir)
		if cacheDir == "" && strings.TrimSpace(*stateFile) != "" {
			cacheDir = filepath.Join(filepath.Dir(*stateFile), "os-image-cache")
		}

		libvirtCfg := libvirt.Config{
			Endpoint:           *libvirtEndpoint,
			StoragePool:        *libvirtStoragePool,
			DefaultVolumeBytes: uint64(*libvirtVolumeGiB) << 30,
			ImageCacheDir:      cacheDir,
		}

		filter, err := libvirt.NewPowerFilter(*libvirtEndpoint)
		if err != nil {
			log.Fatal().Err(err).Str("endpoint", *libvirtEndpoint).Msg("failed to initialize libvirt power filter")
		}
		powerChecker = filter
		provisioner = libvirt.NewProvisioner(libvirtCfg)

		log.Info().
			Str("endpoint", *libvirtEndpoint).
			Str("storage_pool", libvirtCfg.StoragePool).
			Uint64("root_volume_gib", uint64(*libvirtVolumeGiB)).
			Str("image_cache_dir", libvirtCfg.ImageCacheDir).
			Msg("libvirt integration enabled")
	}

	if err := server.Run(ctx, *listenAddr, inventory, powerChecker, provisioner, *stateFile); err != nil {
		log.Fatal().Err(err).Msg("gRPC server stopped with error")
	}
}

func resolveLogLevel(flagLevel string) string {
	if v := os.Getenv("LOG_LEVEL"); v != "" {
		return v
	}
	return flagLevel
}

func initLogging(level string) {
	zerolog.TimeFieldFormat = zerolog.TimeFormatUnix

	lvl, err := zerolog.ParseLevel(level)
	if err != nil {
		log.Fatal().Str("level", level).Msg("invalid log level")
	}
	zerolog.SetGlobalLevel(lvl)

	if term.IsTerminal(int(os.Stderr.Fd())) {
		log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})
	} else {
		log.Logger = zerolog.New(os.Stderr).With().Timestamp().Logger()
	}

	log.Info().Str("level", lvl.String()).Msg("logging configured")
}
