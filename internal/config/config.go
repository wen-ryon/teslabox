package config

import (
	"encoding/hex"
	"fmt"
	"os"
	"path"
	"strconv"
	"strings"
	"time"
)

const (
	defaultHTTPAddr        = ":80"
	defaultDataMountPath   = "/var/lib/teslabox"
	defaultDataLabel       = "TESLABOX"
	defaultImagePath       = "images/teslacam.img"
	defaultSnapshotPath    = "snapshots"
	defaultSnapshotMount   = "/run/teslabox/snapshots"
	defaultIndexPath       = "index"
	defaultStatePath       = "state"
	defaultExportPath      = "exports"
	defaultRegistryPath    = "/var/lib/teslabox-system/storage-registry.json"
	defaultWebAuthPath     = "/var/lib/teslabox-system/web-auth.json"
	defaultUpdateDir       = "/var/lib/teslabox-system/updates"
	defaultUpdateRequest   = "/var/lib/teslabox-system/update-request.json"
	defaultUpdateResult    = "/var/lib/teslabox-system/update-result.json"
	defaultConfigFSPath    = "/sys/kernel/config/usb_gadget"
	defaultUDCPath         = "/sys/class/udc"
	defaultGadgetName      = "teslabox"
	defaultGadgetSerial    = "TESLABOX-071C4AAFBA8BEB80"
	defaultReleaseGadget   = "radxa-otgutils"
	defaultScanInterval    = 30 * time.Second
	defaultPollInterval    = 3 * time.Second
	defaultOperationLimit  = 15 * time.Second
	defaultSnapshotLimit   = 2 * time.Minute
	defaultSnapshotRetry   = 10 * time.Minute
	defaultMediaProbeLimit = 10 * time.Second
	defaultExportLimit     = 30 * time.Minute
	defaultExportQueueSize = 4
	defaultShutdownLimit   = 10 * time.Second
	defaultWebSessionTTL   = 7 * 24 * time.Hour
	defaultMinFreeBytes    = 8 * 1024 * 1024 * 1024
	defaultMinFreePercent  = 5
	defaultUpdateMaxBytes  = 128 * 1024 * 1024
)

// Config 集中保存运行参数，避免系统路径和策略散落在业务代码中。
type Config struct {
	HTTPAddr          string
	DataUUID          string
	DataLabel         string
	DataMountPath     string
	ImagePath         string
	SnapshotPath      string
	SnapshotMountPath string
	IndexPath         string
	StatePath         string
	ExportPath        string
	RegistryPath      string
	WebAuthStatePath  string
	UpdateManifestURL string
	UpdatePublicKey   string
	UpdateChannel     string
	UpdateBoard       string
	UpdateDownloadDir string
	UpdateRequestPath string
	UpdateResultPath  string
	UpdateMaxBytes    int64
	ConfigFSPath      string
	UDCPath           string
	UDCName           string
	GadgetName        string
	GadgetSerial      string
	ReleaseGadget     string
	ScanInterval      time.Duration
	PollInterval      time.Duration
	OperationTimeout  time.Duration
	SnapshotTimeout   time.Duration
	SnapshotRetryMax  time.Duration
	MediaProbeTimeout time.Duration
	ExportMaxDuration time.Duration
	ExportQueueSize   int
	ShutdownTimeout   time.Duration
	MinFreeBytes      uint64
	MinFreePercent    uint64
	WebPasswordSHA256 string
	WebSessionSecret  string
	WebSessionTTL     time.Duration
	FFmpegPath        string
	FFprobePath       string
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
	snapshotTimeout, err := envDuration("TESLABOX_SNAPSHOT_TIMEOUT", defaultSnapshotLimit)
	if err != nil {
		return Config{}, err
	}
	snapshotRetryMax, err := envDuration("TESLABOX_SNAPSHOT_RETRY_MAX", defaultSnapshotRetry)
	if err != nil {
		return Config{}, err
	}
	mediaProbeTimeout, err := envDuration("TESLABOX_MEDIA_PROBE_TIMEOUT", defaultMediaProbeLimit)
	if err != nil {
		return Config{}, err
	}
	exportMaxDuration, err := envDuration("TESLABOX_EXPORT_MAX_DURATION", defaultExportLimit)
	if err != nil {
		return Config{}, err
	}
	exportQueueSize, err := envInt("TESLABOX_EXPORT_QUEUE_SIZE", defaultExportQueueSize)
	if err != nil {
		return Config{}, err
	}
	shutdownTimeout, err := envDuration("TESLABOX_SHUTDOWN_TIMEOUT", defaultShutdownLimit)
	if err != nil {
		return Config{}, err
	}
	minFreeBytes, err := envUint64("TESLABOX_MIN_FREE_BYTES", defaultMinFreeBytes)
	if err != nil {
		return Config{}, err
	}
	minFreePercent, err := envUint64("TESLABOX_MIN_FREE_PERCENT", defaultMinFreePercent)
	if err != nil {
		return Config{}, err
	}
	webSessionTTL, err := envDuration("TESLABOX_WEB_SESSION_TTL", defaultWebSessionTTL)
	if err != nil {
		return Config{}, err
	}

	cfg := Config{
		HTTPAddr:          envString("TESLABOX_HTTP_ADDR", defaultHTTPAddr),
		DataUUID:          strings.TrimSpace(os.Getenv("TESLABOX_DATA_UUID")),
		DataLabel:         envString("TESLABOX_DATA_LABEL", defaultDataLabel),
		DataMountPath:     envString("TESLABOX_DATA_MOUNT_PATH", defaultDataMountPath),
		ImagePath:         envString("TESLABOX_IMAGE_PATH", defaultImagePath),
		SnapshotPath:      envString("TESLABOX_SNAPSHOT_PATH", defaultSnapshotPath),
		SnapshotMountPath: envString("TESLABOX_SNAPSHOT_MOUNT_PATH", defaultSnapshotMount),
		IndexPath:         envString("TESLABOX_INDEX_PATH", defaultIndexPath),
		StatePath:         envString("TESLABOX_STATE_PATH", defaultStatePath),
		ExportPath:        envString("TESLABOX_EXPORT_PATH", defaultExportPath),
		RegistryPath:      envString("TESLABOX_STORAGE_REGISTRY", defaultRegistryPath),
		WebAuthStatePath:  envString("TESLABOX_WEB_AUTH_STATE", defaultWebAuthPath),
		UpdateManifestURL: strings.TrimSpace(os.Getenv("TESLABOX_UPDATE_MANIFEST_URL")),
		UpdatePublicKey:   strings.TrimSpace(os.Getenv("TESLABOX_UPDATE_PUBLIC_KEY")),
		UpdateChannel:     envString("TESLABOX_UPDATE_CHANNEL", "stable"),
		UpdateBoard:       envString("TESLABOX_UPDATE_BOARD", "radxa-rock-3a"),
		UpdateDownloadDir: defaultUpdateDir,
		UpdateRequestPath: defaultUpdateRequest,
		UpdateResultPath:  defaultUpdateResult,
		UpdateMaxBytes:    defaultUpdateMaxBytes,
		ConfigFSPath:      envString("TESLABOX_CONFIGFS_PATH", defaultConfigFSPath),
		UDCPath:           envString("TESLABOX_UDC_PATH", defaultUDCPath),
		UDCName:           strings.TrimSpace(os.Getenv("TESLABOX_UDC_NAME")),
		GadgetName:        envString("TESLABOX_GADGET_NAME", defaultGadgetName),
		GadgetSerial:      envString("TESLABOX_GADGET_SERIAL", defaultGadgetSerial),
		ReleaseGadget:     envString("TESLABOX_RELEASE_GADGET", defaultReleaseGadget),
		ScanInterval:      scanInterval,
		PollInterval:      pollInterval,
		OperationTimeout:  operationTimeout,
		SnapshotTimeout:   snapshotTimeout,
		SnapshotRetryMax:  snapshotRetryMax,
		MediaProbeTimeout: mediaProbeTimeout,
		ExportMaxDuration: exportMaxDuration,
		ExportQueueSize:   exportQueueSize,
		ShutdownTimeout:   shutdownTimeout,
		MinFreeBytes:      minFreeBytes,
		MinFreePercent:    minFreePercent,
		WebPasswordSHA256: strings.ToLower(strings.TrimSpace(os.Getenv("TESLABOX_WEB_PASSWORD_SHA256"))),
		WebSessionSecret:  strings.ToLower(strings.TrimSpace(os.Getenv("TESLABOX_WEB_SESSION_SECRET"))),
		WebSessionTTL:     webSessionTTL,
		FFmpegPath:        envString("TESLABOX_FFMPEG_PATH", "/opt/teslabox/media/bin/ffmpeg"),
		FFprobePath:       envString("TESLABOX_FFPROBE_PATH", "/opt/teslabox/media/bin/ffprobe"),
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
		"TESLABOX_EXPORT_PATH":   c.ExportPath,
	} {
		if !isSafeRelativePath(value) {
			return fmt.Errorf("%s 必须是数据盘内的安全相对路径", name)
		}
	}
	if !isSafeAbsolutePath(c.ConfigFSPath) || !isSafeAbsolutePath(c.UDCPath) {
		return fmt.Errorf("configfs 和 UDC 路径必须是非根目录的绝对路径")
	}
	if !isSafeAbsolutePath(c.SnapshotMountPath) {
		return fmt.Errorf("TESLABOX_SNAPSHOT_MOUNT_PATH 必须是非根目录的绝对路径")
	}
	if !isSafeAbsolutePath(c.RegistryPath) {
		return fmt.Errorf("TESLABOX_STORAGE_REGISTRY 必须是非根目录的绝对路径")
	}
	if !isSafeAbsolutePath(c.WebAuthStatePath) {
		return fmt.Errorf("TESLABOX_WEB_AUTH_STATE 必须是非根目录的绝对路径")
	}
	for name, value := range map[string]string{
		"TESLABOX_UPDATE_DOWNLOAD_DIR": c.UpdateDownloadDir,
		"TESLABOX_UPDATE_REQUEST_PATH": c.UpdateRequestPath,
		"TESLABOX_UPDATE_RESULT_PATH":  c.UpdateResultPath,
	} {
		if !isSafeAbsolutePath(value) {
			return fmt.Errorf("%s 必须是非根目录的绝对路径", name)
		}
	}
	if c.UpdateMaxBytes <= 0 {
		return fmt.Errorf("更新包大小上限必须大于零")
	}
	if !isSafeAbsolutePath(c.FFmpegPath) || !isSafeAbsolutePath(c.FFprobePath) {
		return fmt.Errorf("TESLABOX_FFMPEG_PATH 和 TESLABOX_FFPROBE_PATH 必须是非根目录的绝对路径")
	}
	for name, value := range map[string]string{
		"TESLABOX_UDC_NAME":       c.UDCName,
		"TESLABOX_GADGET_NAME":    c.GadgetName,
		"TESLABOX_GADGET_SERIAL":  c.GadgetSerial,
		"TESLABOX_RELEASE_GADGET": c.ReleaseGadget,
	} {
		if strings.ContainsAny(value, "/\\\t\r\n") {
			return fmt.Errorf("%s 包含非法字符", name)
		}
	}
	if c.GadgetName == "" || c.GadgetSerial == "" {
		return fmt.Errorf("Gadget 名称和序列号不能为空")
	}
	if c.ScanInterval <= 0 || c.PollInterval <= 0 || c.OperationTimeout <= 0 || c.SnapshotTimeout <= 0 || c.SnapshotRetryMax <= 0 || c.MediaProbeTimeout <= 0 || c.ExportMaxDuration <= 0 || c.ShutdownTimeout <= 0 {
		return fmt.Errorf("时间间隔必须大于零")
	}
	if c.ExportQueueSize <= 0 || c.ExportQueueSize > 16 {
		return fmt.Errorf("TESLABOX_EXPORT_QUEUE_SIZE 必须在 1 到 16 之间")
	}
	if c.MinFreeBytes == 0 {
		return fmt.Errorf("TESLABOX_MIN_FREE_BYTES 必须大于零")
	}
	if c.MinFreePercent == 0 || c.MinFreePercent >= 100 {
		return fmt.Errorf("TESLABOX_MIN_FREE_PERCENT 必须在 1 到 99 之间")
	}
	passwordConfigured := c.WebPasswordSHA256 != ""
	secretConfigured := c.WebSessionSecret != ""
	if passwordConfigured != secretConfigured {
		return fmt.Errorf("TESLABOX_WEB_PASSWORD_SHA256 和 TESLABOX_WEB_SESSION_SECRET 必须同时配置")
	}
	if passwordConfigured {
		if len(c.WebPasswordSHA256) != 64 || !isHexBytes(c.WebPasswordSHA256, 32) {
			return fmt.Errorf("TESLABOX_WEB_PASSWORD_SHA256 必须是 32 字节 SHA-256 十六进制摘要")
		}
		if !isHexBytes(c.WebSessionSecret, 32) {
			return fmt.Errorf("TESLABOX_WEB_SESSION_SECRET 必须是至少 32 字节的十六进制密钥")
		}
	}
	if c.WebSessionTTL <= 0 || c.WebSessionTTL > 365*24*time.Hour {
		return fmt.Errorf("TESLABOX_WEB_SESSION_TTL 必须大于零且不超过 365 天")
	}
	return nil
}

func isHexBytes(value string, minimumBytes int) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) >= minimumBytes
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

func envUint64(name string, fallback uint64) (uint64, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.ParseUint(value, 10, 64)
	if err != nil || parsed == 0 {
		return 0, fmt.Errorf("解析 %s: 必须是大于零的整数", name)
	}
	return parsed, nil
}

func envInt(name string, fallback int) (int, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("解析 %s: 必须是大于零的整数", name)
	}
	return parsed, nil
}
