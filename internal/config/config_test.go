package config

import (
	"strings"
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	t.Setenv("TESLABOX_HTTP_ADDR", "127.0.0.1:9090")
	t.Setenv("TESLABOX_DATA_UUID", "data-uuid")
	t.Setenv("TESLABOX_DATA_MOUNT_PATH", "/mnt/teslabox-data")
	t.Setenv("TESLABOX_SCAN_INTERVAL", "45s")
	t.Setenv("TESLABOX_SNAPSHOT_IDLE_INTERVAL", "6m")
	t.Setenv("TESLABOX_SNAPSHOT_ACTIVE_WINDOW", "3m")
	t.Setenv("TESLABOX_MIN_FREE_BYTES", "1073741824")
	t.Setenv("TESLABOX_MIN_FREE_PERCENT", "7")
	t.Setenv("TESLABOX_INIT_FREE_HEADROOM_BYTES", "2147483648")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.HTTPAddr != "127.0.0.1:9090" {
		t.Errorf("HTTPAddr = %q", cfg.HTTPAddr)
	}
	if cfg.DataUUID != "data-uuid" {
		t.Errorf("DataUUID = %q", cfg.DataUUID)
	}
	if cfg.DataMountPath != "/mnt/teslabox-data" {
		t.Errorf("DataMountPath = %q", cfg.DataMountPath)
	}
	if cfg.ScanInterval != 45*time.Second {
		t.Errorf("ScanInterval = %s", cfg.ScanInterval)
	}
	if cfg.SnapshotIdleInterval != 6*time.Minute || cfg.SnapshotActiveWindow != 3*time.Minute {
		t.Errorf("snapshot cadence = idle %s, active window %s", cfg.SnapshotIdleInterval, cfg.SnapshotActiveWindow)
	}
	if cfg.MinFreeBytes != 1073741824 || cfg.MinFreePercent != 7 {
		t.Errorf("free-space thresholds = %d bytes, %d percent", cfg.MinFreeBytes, cfg.MinFreePercent)
	}
	if cfg.InitializationHeadroomBytes != 2147483648 {
		t.Errorf("InitializationHeadroomBytes = %d", cfg.InitializationHeadroomBytes)
	}
	if cfg.FFmpegPath != "/opt/teslabox/media/bin/ffmpeg" || cfg.FFprobePath != "/opt/teslabox/media/bin/ffprobe" || cfg.MediaProbeTimeout != 10*time.Second {
		t.Errorf("media config = %q, %q, %s", cfg.FFmpegPath, cfg.FFprobePath, cfg.MediaProbeTimeout)
	}
	if cfg.ExportPath != "exports" || cfg.ExportStatePath != "/var/lib/teslabox-system/export-state.json" || cfg.ExportMaxDuration != 0 || cfg.ExportQueueSize != 4 {
		t.Errorf("export config = %q, %q, %s, %d", cfg.ExportPath, cfg.ExportStatePath, cfg.ExportMaxDuration, cfg.ExportQueueSize)
	}
	if cfg.WebAuthStatePath != "/var/lib/teslabox-system/web-auth.json" {
		t.Errorf("WebAuthStatePath = %q", cfg.WebAuthStatePath)
	}
	if cfg.MaintenanceStatePath != "/var/lib/teslabox-system/maintenance.json" || cfg.TeslaTimeZone != "Asia/Shanghai" {
		t.Errorf("maintenance/timezone config = %q, %q", cfg.MaintenanceStatePath, cfg.TeslaTimeZone)
	}
	if cfg.OnboardingStatePath != "/var/lib/teslabox-system/onboarding.json" {
		t.Errorf("OnboardingStatePath = %q", cfg.OnboardingStatePath)
	}
	if cfg.WiFiInterface != "" || cfg.APInterface != "ap0" {
		t.Errorf("network interfaces = %q, %q", cfg.WiFiInterface, cfg.APInterface)
	}
	if cfg.BootIDPath != "/proc/sys/kernel/random/boot_id" || cfg.BootUptimePath != "/proc/uptime" || cfg.GadgetPauseStatePath != "/var/lib/teslabox-system/gadget-paused" {
		t.Errorf("gadget safety paths = %q, %q, %q", cfg.BootIDPath, cfg.BootUptimePath, cfg.GadgetPauseStatePath)
	}
}

func TestLoadUsesDefaultHTTPAddr(t *testing.T) {
	t.Setenv("TESLABOX_HTTP_ADDR", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.HTTPAddr != ":80" {
		t.Errorf("HTTPAddr = %q, want :80", cfg.HTTPAddr)
	}
}

func TestLoadDerivesSignedCatalogFromManifest(t *testing.T) {
	t.Setenv("TESLABOX_UPDATE_MANIFEST_URL", "https://gitee.example/repository/raw/master/stable/latest.json")
	t.Setenv("TESLABOX_UPDATE_CATALOG_URL", "")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.UpdateCatalogURL != "https://gitee.example/repository/raw/master/stable/history.json" {
		t.Fatalf("UpdateCatalogURL = %q", cfg.UpdateCatalogURL)
	}
	if cfg.UpdateBetaManifestURL != "https://gitee.example/repository/raw/master/beta/latest.json" ||
		cfg.UpdateBetaCatalogURL != "https://gitee.example/repository/raw/master/beta/history.json" {
		t.Fatalf("derived beta URLs = %q, %q", cfg.UpdateBetaManifestURL, cfg.UpdateBetaCatalogURL)
	}

	t.Setenv("TESLABOX_UPDATE_CATALOG_URL", "https://updates.example/custom/catalog.json")
	cfg, err = Load()
	if err != nil {
		t.Fatalf("Load() with explicit catalog error = %v", err)
	}
	if cfg.UpdateCatalogURL != "https://updates.example/custom/catalog.json" {
		t.Fatalf("explicit UpdateCatalogURL = %q", cfg.UpdateCatalogURL)
	}
}

func TestBetaManifestURLForStableRejectsUnrelatedOrInsecureURL(t *testing.T) {
	t.Parallel()
	for _, value := range []string{
		"",
		"http://gitee.example/repository/raw/master/stable/latest.json",
		"https://gitee.example/repository/raw/master/latest.json",
		"https://gitee.example/repository/raw/master/stable/release.json",
	} {
		if got := betaManifestURLForStable(value); got != "" {
			t.Fatalf("betaManifestURLForStable(%q) = %q", value, got)
		}
	}
}

func TestCatalogURLForManifestRejectsUnrelatedOrInsecureURL(t *testing.T) {
	t.Parallel()
	for _, value := range []string{
		"",
		"http://gitee.example/stable/latest.json",
		"https://gitee.example/stable/release.json",
		"not-a-url",
	} {
		if got := catalogURLForManifest(value); got != "" {
			t.Fatalf("catalogURLForManifest(%q) = %q", value, got)
		}
	}
}

func TestLoadRejectsInvalidDuration(t *testing.T) {
	t.Setenv("TESLABOX_SCAN_INTERVAL", "immediately")
	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want invalid duration error")
	}
}

func TestLoadRejectsInvalidFreeSpaceThreshold(t *testing.T) {
	t.Setenv("TESLABOX_MIN_FREE_BYTES", "eight-gib")
	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want invalid free-space threshold")
	}
}

func TestLoadRejectsOverflowingSnapshotPolicy(t *testing.T) {
	t.Setenv("TESLABOX_MIN_FREE_BYTES", "18446744073709551615")
	t.Setenv("TESLABOX_INIT_FREE_HEADROOM_BYTES", "1")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), "策略配置无效") {
		t.Fatalf("Load() error = %v", err)
	}
}

func TestLoadRejectsInvalidNetworkInterfaces(t *testing.T) {
	t.Setenv("TESLABOX_WIFI_INTERFACE", "wlan0;reboot")
	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want invalid interface error")
	}
}

func TestConfigValidate(t *testing.T) {
	valid := Config{
		HTTPAddr:                    ":80",
		DataLabel:                   "TESLABOX",
		DataMountPath:               "/var/lib/teslabox",
		ImagePath:                   "images/teslacam.img",
		SnapshotPath:                "snapshots",
		SnapshotMountPath:           "/run/teslabox/snapshots",
		IndexPath:                   "index",
		StatePath:                   "state",
		ExportPath:                  "exports",
		ExportStatePath:             "/var/lib/teslabox-system/export-state.json",
		RegistryPath:                "/var/lib/teslabox-system/storage-registry.json",
		WebAuthStatePath:            "/var/lib/teslabox-system/web-auth.json",
		UpdateChannel:               "stable",
		UpdateBoard:                 "radxa-rock-3a",
		UpdateDownloadDir:           "/var/lib/teslabox-system/updates",
		UpdateRequestPath:           "/var/lib/teslabox-system/update-request.json",
		UpdateResultPath:            "/var/lib/teslabox-system/update-result.json",
		UpdateMaxBytes:              128 * 1024 * 1024,
		GadgetPauseStatePath:        "/var/lib/teslabox-system/gadget-paused",
		MaintenanceLockPath:         "/run/teslabox/storage-maintenance.lock",
		MaintenanceOperationPath:    "/var/lib/teslabox-system/storage-maintenance.json",
		BootIDPath:                  "/proc/sys/kernel/random/boot_id",
		BootUptimePath:              "/proc/uptime",
		ConfigFSPath:                "/sys/kernel/config/usb_gadget",
		UDCPath:                     "/sys/class/udc",
		GadgetName:                  "teslabox",
		GadgetSerial:                "TESLABOX-TEST",
		ReleaseGadget:               "radxa-otgutils",
		APInterface:                 "ap0",
		ScanInterval:                time.Second,
		SnapshotIdleInterval:        time.Minute,
		SnapshotActiveWindow:        time.Minute,
		PollInterval:                time.Second,
		OperationTimeout:            time.Second,
		SnapshotTimeout:             time.Minute,
		SnapshotRetryMax:            10 * time.Minute,
		MediaProbeTimeout:           10 * time.Second,
		ExportMaxDuration:           30 * time.Minute,
		ExportQueueSize:             4,
		ShutdownTimeout:             time.Second,
		MinFreeBytes:                8 * 1024 * 1024 * 1024,
		MinFreePercent:              5,
		InitializationHeadroomBytes: 4 * 1024 * 1024 * 1024,
		WebSessionTTL:               7 * 24 * time.Hour,
		FFmpegPath:                  "/opt/teslabox/media/bin/ffmpeg",
		FFprobePath:                 "/opt/teslabox/media/bin/ffprobe",
	}

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr bool
	}{
		{name: "valid config"},
		{name: "relative data mount", mutate: func(cfg *Config) { cfg.DataMountPath = "var/lib/teslabox" }, wantErr: true},
		{name: "root data mount", mutate: func(cfg *Config) { cfg.DataMountPath = "/" }, wantErr: true},
		{name: "escaping image path", mutate: func(cfg *Config) { cfg.ImagePath = "../system.img" }, wantErr: true},
		{name: "absolute snapshot path", mutate: func(cfg *Config) { cfg.SnapshotPath = "/tmp/snapshots" }, wantErr: true},
		{name: "escaping export path", mutate: func(cfg *Config) { cfg.ExportPath = "../exports" }, wantErr: true},
		{name: "relative export state path", mutate: func(cfg *Config) { cfg.ExportStatePath = "var/lib/export-state.json" }, wantErr: true},
		{name: "relative snapshot mount path", mutate: func(cfg *Config) { cfg.SnapshotMountPath = "run/teslabox/snapshots" }, wantErr: true},
		{name: "relative registry path", mutate: func(cfg *Config) { cfg.RegistryPath = "var/lib/registry.json" }, wantErr: true},
		{name: "relative web auth path", mutate: func(cfg *Config) { cfg.WebAuthStatePath = "var/lib/web-auth.json" }, wantErr: true},
		{name: "relative maintenance path", mutate: func(cfg *Config) { cfg.MaintenanceStatePath = "var/lib/maintenance.json" }, wantErr: true},
		{name: "invalid Tesla time zone", mutate: func(cfg *Config) { cfg.TeslaTimeZone = "Asia/NotAZone" }, wantErr: true},
		{name: "relative gadget pause path", mutate: func(cfg *Config) { cfg.GadgetPauseStatePath = "var/lib/gadget-paused" }, wantErr: true},
		{name: "relative boot id path", mutate: func(cfg *Config) { cfg.BootIDPath = "proc/sys/kernel/random/boot_id" }, wantErr: true},
		{name: "relative boot uptime path", mutate: func(cfg *Config) { cfg.BootUptimePath = "proc/uptime" }, wantErr: true},
		{name: "relative ffmpeg path", mutate: func(cfg *Config) { cfg.FFmpegPath = "ffmpeg" }, wantErr: true},
		{name: "zero media probe timeout", mutate: func(cfg *Config) { cfg.MediaProbeTimeout = 0 }, wantErr: true},
		{name: "zero idle snapshot interval", mutate: func(cfg *Config) { cfg.SnapshotIdleInterval = 0 }, wantErr: true},
		{name: "zero active snapshot window", mutate: func(cfg *Config) { cfg.SnapshotActiveWindow = 0 }, wantErr: true},
		{name: "excessive export queue", mutate: func(cfg *Config) { cfg.ExportQueueSize = 17 }, wantErr: true},
		{name: "missing selector", mutate: func(cfg *Config) { cfg.DataLabel = "" }, wantErr: true},
		{name: "unsafe gadget name", mutate: func(cfg *Config) { cfg.GadgetName = "../gadget" }, wantErr: true},
		{name: "unsafe wifi interface", mutate: func(cfg *Config) { cfg.WiFiInterface = "wlan0;reboot" }, wantErr: true},
		{name: "same network interfaces", mutate: func(cfg *Config) { cfg.WiFiInterface = "ap0" }},
		{name: "zero minimum free bytes", mutate: func(cfg *Config) { cfg.MinFreeBytes = 0 }, wantErr: true},
		{name: "invalid minimum free percent", mutate: func(cfg *Config) { cfg.MinFreePercent = 100 }, wantErr: true},
		{name: "password without session secret", mutate: func(cfg *Config) { cfg.WebPasswordSHA256 = strings.Repeat("a", 64) }, wantErr: true},
		{name: "invalid session ttl", mutate: func(cfg *Config) { cfg.WebSessionTTL = 0 }, wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := valid
			if test.mutate != nil {
				test.mutate(&cfg)
			}
			err := cfg.Validate()
			if (err != nil) != test.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}
