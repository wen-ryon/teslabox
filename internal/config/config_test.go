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
	t.Setenv("TESLABOX_MIN_FREE_BYTES", "1073741824")
	t.Setenv("TESLABOX_MIN_FREE_PERCENT", "7")

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
	if cfg.MinFreeBytes != 1073741824 || cfg.MinFreePercent != 7 {
		t.Errorf("free-space thresholds = %d bytes, %d percent", cfg.MinFreeBytes, cfg.MinFreePercent)
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

func TestConfigValidate(t *testing.T) {
	valid := Config{
		HTTPAddr:          ":8080",
		DataLabel:         "TESLABOX",
		DataMountPath:     "/var/lib/teslabox",
		ImagePath:         "images/teslacam.img",
		SnapshotPath:      "snapshots",
		SnapshotMountPath: "/run/teslabox/snapshots",
		IndexPath:         "index",
		StatePath:         "state",
		RegistryPath:      "/var/lib/teslabox-system/storage-registry.json",
		ConfigFSPath:      "/sys/kernel/config/usb_gadget",
		UDCPath:           "/sys/class/udc",
		GadgetName:        "teslabox",
		GadgetSerial:      "TESLABOX-TEST",
		ReleaseGadget:     "radxa-otgutils",
		ScanInterval:      time.Second,
		PollInterval:      time.Second,
		OperationTimeout:  time.Second,
		SnapshotTimeout:   time.Minute,
		SnapshotRetryMax:  10 * time.Minute,
		ShutdownTimeout:   time.Second,
		MinFreeBytes:      8 * 1024 * 1024 * 1024,
		MinFreePercent:    5,
		WebSessionTTL:     7 * 24 * time.Hour,
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
		{name: "relative snapshot mount path", mutate: func(cfg *Config) { cfg.SnapshotMountPath = "run/teslabox/snapshots" }, wantErr: true},
		{name: "relative registry path", mutate: func(cfg *Config) { cfg.RegistryPath = "var/lib/registry.json" }, wantErr: true},
		{name: "missing selector", mutate: func(cfg *Config) { cfg.DataLabel = "" }, wantErr: true},
		{name: "unsafe gadget name", mutate: func(cfg *Config) { cfg.GadgetName = "../gadget" }, wantErr: true},
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
