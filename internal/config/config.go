package config

import (
	"fmt"
	"os"
	"path"
	"strings"
	"time"
)

const (
	defaultHTTPAddr       = ":8080"
	defaultDataMountPath  = "/var/lib/teslabox"
	defaultDataLabel      = "TESLABOX"
	defaultImagePath      = "images/teslacam.img"
	defaultSnapshotPath   = "snapshots"
	defaultIndexPath      = "index"
	defaultStatePath      = "state"
	defaultConfigFSPath   = "/sys/kernel/config/usb_gadget"
	defaultUDCPath        = "/sys/class/udc"
	defaultScanInterval   = 30 * time.Second
	defaultPollInterval   = 3 * time.Second
	defaultOperationLimit = 15 * time.Second
	defaultShutdownLimit  = 10 * time.Second
)

// Config 集中保存运行参数，避免系统路径和策略散落在业务代码中。
type Config struct {
	HTTPAddr         string
	DataUUID         string
	DataLabel        string
	DataMountPath    string
	ImagePath        string
	SnapshotPath     string
	IndexPath        string
	StatePath        string
	ConfigFSPath     string
	UDCPath          string
	ScanInterval     time.Duration
	PollInterval     time.Duration
	OperationTimeout time.Duration
	ShutdownTimeout  time.Duration
}

// Load 从环境变量加载并校验配置，保证错误在启动阶段暴露。
func Load() (Config, error) {
	scanInterval, err := envDuration("TESLABOX_SCAN_INTERVAL", defaultScanInterval)
	if err != nil {
		return Config{}, err
	}
	pollInterval, err := envDuration("TESLABOX_POLL_INTERVAL", defaultPollInterval)
	if err != nil {
		return Config{}, err
	}
	operationTimeout, err := envDuration("TESLABOX_OPERATION_TIMEOUT", defaultOperationLimit)
	if err != nil {
		return Config{}, err
	}
	shutdownTimeout, err := envDuration("TESLABOX_SHUTDOWN_TIMEOUT", defaultShutdownLimit)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		HTTPAddr:         envString("TESLABOX_HTTP_ADDR", defaultHTTPAddr),
		DataUUID:         strings.TrimSpace(os.Getenv("TESLABOX_DATA_UUID")),
		DataLabel:        envString("TESLABOX_DATA_LABEL", defaultDataLabel),
		DataMountPath:    envString("TESLABOX_DATA_MOUNT_PATH", defaultDataMountPath),
		ImagePath:        envString("TESLABOX_IMAGE_PATH", defaultImagePath),
		SnapshotPath:     envString("TESLABOX_SNAPSHOT_PATH", defaultSnapshotPath),
		IndexPath:        envString("TESLABOX_INDEX_PATH", defaultIndexPath),
		StatePath:        envString("TESLABOX_STATE_PATH", defaultStatePath),
		ConfigFSPath:     envString("TESLABOX_CONFIGFS_PATH", defaultConfigFSPath),
		UDCPath:          envString("TESLABOX_UDC_PATH", defaultUDCPath),
		ScanInterval:     scanInterval,
		PollInterval:     pollInterval,
		OperationTimeout: operationTimeout,
		ShutdownTimeout:  shutdownTimeout,
	}

	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Validate 拒绝可能挂载错误设备或造成忙循环的配置。
func (c Config) Validate() error {
	if strings.TrimSpace(c.HTTPAddr) == "" {
		return fmt.Errorf("TESLABOX_HTTP_ADDR 不能为空")
	}
	if !isSafeAbsolutePath(c.DataMountPath) {
		return fmt.Errorf("TESLABOX_DATA_MOUNT_PATH 必须是非根目录的绝对路径")
	}
	if strings.TrimSpace(c.DataUUID) == "" && strings.TrimSpace(c.DataLabel) == "" {
		return fmt.Errorf("TESLABOX_DATA_UUID 和 TESLABOX_DATA_LABEL 至少配置一个")
	}
	for name, value := range map[string]string{
		"TESLABOX_IMAGE_PATH":    c.ImagePath,
		"TESLABOX_SNAPSHOT_PATH": c.SnapshotPath,
		"TESLABOX_INDEX_PATH":    c.IndexPath,
		"TESLABOX_STATE_PATH":    c.StatePath,
	} {
		if !isSafeRelativePath(value) {
			return fmt.Errorf("%s 必须是数据盘内的安全相对路径", name)
		}
	}
	if !isSafeAbsolutePath(c.ConfigFSPath) || !isSafeAbsolutePath(c.UDCPath) {
		return fmt.Errorf("configfs 和 UDC 路径必须是非根目录的绝对路径")
	}
	if c.ScanInterval <= 0 || c.PollInterval <= 0 || c.OperationTimeout <= 0 || c.ShutdownTimeout <= 0 {
		return fmt.Errorf("时间间隔必须大于零")
	}
	return nil
}

func isSafeAbsolutePath(value string) bool {
	cleaned := path.Clean(strings.ReplaceAll(strings.TrimSpace(value), "\\", "/"))
	return path.IsAbs(cleaned) && cleaned != "/"
}

func isSafeRelativePath(value string) bool {
	cleaned := path.Clean(strings.ReplaceAll(strings.TrimSpace(value), "\\", "/"))
	return cleaned != "." && cleaned != ".." && !path.IsAbs(cleaned) && !strings.HasPrefix(cleaned, "../")
}

func envString(name, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	return value
}

func envDuration(name string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("解析 %s: 必须是大于零的时间", name)
	}
	return parsed, nil
}
