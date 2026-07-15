package config

import (
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	t.Setenv("TESLABOX_HTTP_ADDR", "127.0.0.1:9090")
	t.Setenv("TESLABOX_DATA_UUID", "data-uuid")
	t.Setenv("TESLABOX_DATA_MOUNT_PATH", "/mnt/teslabox-data")
	t.Setenv("TESLABOX_SCAN_INTERVAL", "45s")

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
}

func TestLoadRejectsInvalidDuration(t *testing.T) {
	t.Setenv("TESLABOX_SCAN_INTERVAL", "immediately")
	if _, err := Load(); err == nil {
		t.Fatal("Load() error = nil, want invalid duration error")
	}
}

func TestConfigValidate(t *testing.T) {
	valid := Config{
		HTTPAddr:         ":8080",
		DataLabel:        "TESLABOX",
		DataMountPath:    "/var/lib/teslabox",
		ImagePath:        "images/teslacam.img",
		SnapshotPath:     "snapshots",
		IndexPath:        "index",
		StatePath:        "state",
		ConfigFSPath:     "/sys/kernel/config/usb_gadget",
		UDCPath:          "/sys/class/udc",
		ScanInterval:     time.Second,
		PollInterval:     time.Second,
		OperationTimeout: time.Second,
		ShutdownTimeout:  time.Second,
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
		{name: "missing selector", mutate: func(cfg *Config) { cfg.DataLabel = "" }, wantErr: true},
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
