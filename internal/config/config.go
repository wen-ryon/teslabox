package config

import (
	"encoding/hex"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/wen-ryon/teslabox/internal/certdelivery"
	"github.com/wen-ryon/teslabox/internal/storagepolicy"
	"github.com/wen-ryon/teslabox/internal/webtls"
)

const (
	defaultHTTPAddr                 = ":80"
	defaultDataMountPath            = "/var/lib/teslabox"
	defaultDataLabel                = "TESLABOX"
	defaultImagePath                = "images/teslacam.img"
	defaultSnapshotPath             = "snapshots"
	defaultSnapshotMount            = "/run/teslabox/snapshots"
	defaultIndexPath                = "index"
	defaultStatePath                = "state"
	defaultExportPath               = "exports"
	defaultExportStatePath          = "/var/lib/teslabox-system/export-state.json"
	defaultRegistryPath             = "/var/lib/teslabox-system/storage-registry.json"
	defaultWebAuthPath              = "/var/lib/teslabox-system/web-auth.json"
	defaultMaintenancePath          = "/var/lib/teslabox-system/maintenance.json"
	defaultOnboardingPath           = "/var/lib/teslabox-system/onboarding.json"
	defaultTeslaTimeZone            = "Asia/Shanghai"
	defaultUpdateDir                = "/var/lib/teslabox-system/updates"
	defaultUpdateRequest            = "/var/lib/teslabox-system/update-request.json"
	defaultUpdateResult             = "/var/lib/teslabox-system/update-result.json"
	defaultUpdateChannelState       = "/var/lib/teslabox-system/update-channel.json"
	defaultGadgetPausePath          = "/var/lib/teslabox-system/gadget-paused"
	defaultMaintenanceLockPath      = "/run/teslabox/storage-maintenance.lock"
	defaultMaintenanceOperationPath = "/var/lib/teslabox-system/storage-maintenance.json"
	defaultBootIDPath               = "/proc/sys/kernel/random/boot_id"
	defaultBootUptimePath           = "/proc/uptime"
	defaultConfigFSPath             = "/sys/kernel/config/usb_gadget"
	defaultUDCPath                  = "/sys/class/udc"
	defaultGadgetName               = "teslabox"
	defaultGadgetSerial             = "TESLABOX-071C4AAFBA8BEB80"
	defaultReleaseGadget            = "radxa-otgutils"
	defaultAPInterface              = "ap0"
	defaultScanInterval             = time.Minute
	defaultSnapshotIdle             = 5 * time.Minute
	defaultSnapshotWindow           = 2 * time.Minute
	defaultPollInterval             = 3 * time.Second
	defaultOperationLimit           = 15 * time.Second
	defaultSnapshotLimit            = 2 * time.Minute
	defaultSnapshotRetry            = 10 * time.Minute
	defaultMediaProbeLimit          = 10 * time.Second
	defaultExportLimit              = time.Duration(0)
	defaultExportQueueSize          = 4
	defaultShutdownLimit            = 10 * time.Second
	defaultWebSessionTTL            = 7 * 24 * time.Hour
	defaultUpdateMaxBytes           = 128 * 1024 * 1024
)

// Config 集中保存运行参数，避免系统路径和策略散落在业务代码中。
type Config struct {
	HTTPAddr                    string
	HTTPSAddr                   string
	HTTPSDomain                 string
	CarPlayPublicAddress        string
	HTTPSCertificateBundle      string
	HTTPSCertificateEndpoint    string
	HTTPSCertificatePublicKey   string
	HTTPSCertificateIdentity    string
	CarPlayBrowserToken         string
	CarPlayServiceSettings      string
	CarPlayMFIDirectory         string
	DataUUID                    string
	DataLabel                   string
	DataMountPath               string
	ImagePath                   string
	SnapshotPath                string
	SnapshotMountPath           string
	IndexPath                   string
	StatePath                   string
	ExportPath                  string
	ExportStatePath             string
	IndexCacheDir               string
	RegistryPath                string
	WebAuthStatePath            string
	MaintenanceStatePath        string
	OnboardingStatePath         string
	TeslaTimeZone               string
	UpdateManifestURL           string
	UpdateCatalogURL            string
	UpdateBetaManifestURL       string
	UpdateBetaCatalogURL        string
	UpdatePublicKey             string
	UpdateChannel               string
	UpdateChannelStatePath      string
	UpdateBoard                 string
	UpdateDownloadDir           string
	UpdateRequestPath           string
	UpdateResultPath            string
	UpdateMaxBytes              int64
	GadgetPauseStatePath        string
	MaintenanceLockPath         string
	MaintenanceOperationPath    string
	BootIDPath                  string
	BootUptimePath              string
	ConfigFSPath                string
	UDCPath                     string
	UDCName                     string
	GadgetName                  string
	GadgetSerial                string
	ReleaseGadget               string
	WiFiInterface               string
	APInterface                 string
	ScanInterval                time.Duration
	SnapshotIdleInterval        time.Duration
	SnapshotActiveWindow        time.Duration
	PollInterval                time.Duration
	OperationTimeout            time.Duration
	SnapshotTimeout             time.Duration
	SnapshotRetryMax            time.Duration
	MediaProbeTimeout           time.Duration
	ExportMaxDuration           time.Duration
	ExportQueueSize             int
	ShutdownTimeout             time.Duration
	MinFreeBytes                uint64
	MinFreePercent              uint64
	InitializationHeadroomBytes uint64
	WebPasswordSHA256           string
	WebSessionSecret            string
	WebSessionTTL               time.Duration
	FFmpegPath                  string
	FFprobePath                 string
}

// Load 从环境变量加载并校验配置，保证错误在启动阶段暴露。
func Load() (Config, error) {
	scanInterval, err := envDuration("TESLABOX_SCAN_INTERVAL", defaultScanInterval)
	if err != nil {
		return Config{}, err
	}
	snapshotIdleInterval, err := envDuration("TESLABOX_SNAPSHOT_IDLE_INTERVAL", defaultSnapshotIdle)
	if err != nil {
		return Config{}, err
	}
	snapshotActiveWindow, err := envDuration("TESLABOX_SNAPSHOT_ACTIVE_WINDOW", defaultSnapshotWindow)
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
	exportMaxDuration, err := envNonNegativeDuration("TESLABOX_EXPORT_MAX_DURATION", defaultExportLimit)
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
	minFreeBytes, err := envUint64("TESLABOX_MIN_FREE_BYTES", storagepolicy.DefaultMinFreeBytes)
	if err != nil {
		return Config{}, err
	}
	minFreePercent, err := envUint64("TESLABOX_MIN_FREE_PERCENT", storagepolicy.DefaultMinFreePercent)
	if err != nil {
		return Config{}, err
	}
	initializationHeadroomBytes, err := envUint64("TESLABOX_INIT_FREE_HEADROOM_BYTES", storagepolicy.DefaultInitializationHeadroomBytes)
	if err != nil {
		return Config{}, err
	}
	webSessionTTL, err := envDuration("TESLABOX_WEB_SESSION_TTL", defaultWebSessionTTL)
	if err != nil {
		return Config{}, err
	}
	updateManifestURL := strings.TrimSpace(os.Getenv("TESLABOX_UPDATE_MANIFEST_URL"))
	updateCatalogURL := strings.TrimSpace(os.Getenv("TESLABOX_UPDATE_CATALOG_URL"))
	if updateCatalogURL == "" {
		updateCatalogURL = catalogURLForManifest(updateManifestURL)
	}
	updateBetaManifestURL := strings.TrimSpace(os.Getenv("TESLABOX_UPDATE_BETA_MANIFEST_URL"))
	if updateBetaManifestURL == "" {
		updateBetaManifestURL = betaManifestURLForStable(updateManifestURL)
	}
	updateBetaCatalogURL := strings.TrimSpace(os.Getenv("TESLABOX_UPDATE_BETA_CATALOG_URL"))
	if updateBetaCatalogURL == "" {
		updateBetaCatalogURL = catalogURLForManifest(updateBetaManifestURL)
	}

	cfg := Config{
		HTTPAddr:                    envString("TESLABOX_HTTP_ADDR", defaultHTTPAddr),
		HTTPSAddr:                   strings.TrimSpace(os.Getenv("TESLABOX_HTTPS_ADDR")),
		HTTPSDomain:                 strings.TrimSpace(os.Getenv("TESLABOX_HTTPS_DOMAIN")),
		HTTPSCertificateEndpoint:    envString("TESLABOX_HTTPS_CERTIFICATE_ENDPOINT", certdelivery.DefaultEndpoint),
		HTTPSCertificatePublicKey:   envString("TESLABOX_HTTPS_CERTIFICATE_PUBLIC_KEY", certdelivery.DefaultPublicKey),
		HTTPSCertificateIdentity:    envString("TESLABOX_HTTPS_CERTIFICATE_IDENTITY", "/var/lib/teslabox-system/cert-delivery/identity.json"),
		CarPlayPublicAddress:        strings.TrimSpace(os.Getenv("TESLABOX_CARPLAY_PUBLIC_ADDRESS")),
		HTTPSCertificateBundle:      strings.TrimSpace(os.Getenv("TESLABOX_HTTPS_CERTIFICATE_BUNDLE")),
		CarPlayBrowserToken:         strings.TrimSpace(os.Getenv("TESLABOX_CARPLAY_BROWSER_TOKEN")),
		CarPlayServiceSettings:      strings.TrimSpace(os.Getenv("TESLABOX_CARPLAY_SERVICE_SETTINGS")),
		CarPlayMFIDirectory:         strings.TrimSpace(os.Getenv("TESLABOX_CARPLAY_MFI_DIRECTORY")),
		DataUUID:                    strings.TrimSpace(os.Getenv("TESLABOX_DATA_UUID")),
		DataLabel:                   envString("TESLABOX_DATA_LABEL", defaultDataLabel),
		DataMountPath:               envString("TESLABOX_DATA_MOUNT_PATH", defaultDataMountPath),
		ImagePath:                   envString("TESLABOX_IMAGE_PATH", defaultImagePath),
		SnapshotPath:                envString("TESLABOX_SNAPSHOT_PATH", defaultSnapshotPath),
		SnapshotMountPath:           envString("TESLABOX_SNAPSHOT_MOUNT_PATH", defaultSnapshotMount),
		IndexPath:                   envString("TESLABOX_INDEX_PATH", defaultIndexPath),
		StatePath:                   envString("TESLABOX_STATE_PATH", defaultStatePath),
		ExportPath:                  envString("TESLABOX_EXPORT_PATH", defaultExportPath),
		ExportStatePath:             envString("TESLABOX_EXPORT_STATE", defaultExportStatePath),
		IndexCacheDir:               envString("TESLABOX_INDEX_CACHE_DIR", "/var/lib/teslabox-system/index-cache"),
		RegistryPath:                envString("TESLABOX_STORAGE_REGISTRY", defaultRegistryPath),
		WebAuthStatePath:            envString("TESLABOX_WEB_AUTH_STATE", defaultWebAuthPath),
		MaintenanceStatePath:        envString("TESLABOX_MAINTENANCE_STATE", defaultMaintenancePath),
		OnboardingStatePath:         envString("TESLABOX_ONBOARDING_STATE", defaultOnboardingPath),
		TeslaTimeZone:               envString("TESLABOX_TESLA_TIME_ZONE", defaultTeslaTimeZone),
		UpdateManifestURL:           updateManifestURL,
		UpdateCatalogURL:            updateCatalogURL,
		UpdateBetaManifestURL:       updateBetaManifestURL,
		UpdateBetaCatalogURL:        updateBetaCatalogURL,
		UpdatePublicKey:             strings.TrimSpace(os.Getenv("TESLABOX_UPDATE_PUBLIC_KEY")),
		UpdateChannel:               envString("TESLABOX_UPDATE_CHANNEL", "stable"),
		UpdateChannelStatePath:      envString("TESLABOX_UPDATE_CHANNEL_STATE", defaultUpdateChannelState),
		UpdateBoard:                 envString("TESLABOX_UPDATE_BOARD", "radxa-rock-3a"),
		UpdateDownloadDir:           defaultUpdateDir,
		UpdateRequestPath:           defaultUpdateRequest,
		UpdateResultPath:            defaultUpdateResult,
		UpdateMaxBytes:              defaultUpdateMaxBytes,
		GadgetPauseStatePath:        envString("TESLABOX_GADGET_PAUSE_STATE", defaultGadgetPausePath),
		MaintenanceLockPath:         envString("TESLABOX_MAINTENANCE_LOCK_PATH", defaultMaintenanceLockPath),
		MaintenanceOperationPath:    envString("TESLABOX_MAINTENANCE_OPERATION_STATE", defaultMaintenanceOperationPath),
		BootIDPath:                  envString("TESLABOX_BOOT_ID_PATH", defaultBootIDPath),
		BootUptimePath:              envString("TESLABOX_BOOT_UPTIME_PATH", defaultBootUptimePath),
		ConfigFSPath:                envString("TESLABOX_CONFIGFS_PATH", defaultConfigFSPath),
		UDCPath:                     envString("TESLABOX_UDC_PATH", defaultUDCPath),
		UDCName:                     strings.TrimSpace(os.Getenv("TESLABOX_UDC_NAME")),
		GadgetName:                  envString("TESLABOX_GADGET_NAME", defaultGadgetName),
		GadgetSerial:                envString("TESLABOX_GADGET_SERIAL", defaultGadgetSerial),
		ReleaseGadget:               envString("TESLABOX_RELEASE_GADGET", defaultReleaseGadget),
		WiFiInterface:               strings.TrimSpace(os.Getenv("TESLABOX_WIFI_INTERFACE")),
		APInterface:                 envString("TESLABOX_AP_INTERFACE", defaultAPInterface),
		ScanInterval:                scanInterval,
		SnapshotIdleInterval:        snapshotIdleInterval,
		SnapshotActiveWindow:        snapshotActiveWindow,
		PollInterval:                pollInterval,
		OperationTimeout:            operationTimeout,
		SnapshotTimeout:             snapshotTimeout,
		SnapshotRetryMax:            snapshotRetryMax,
		MediaProbeTimeout:           mediaProbeTimeout,
		ExportMaxDuration:           exportMaxDuration,
		ExportQueueSize:             exportQueueSize,
		ShutdownTimeout:             shutdownTimeout,
		MinFreeBytes:                minFreeBytes,
		MinFreePercent:              minFreePercent,
		InitializationHeadroomBytes: initializationHeadroomBytes,
		WebPasswordSHA256:           strings.ToLower(strings.TrimSpace(os.Getenv("TESLABOX_WEB_PASSWORD_SHA256"))),
		WebSessionSecret:            strings.ToLower(strings.TrimSpace(os.Getenv("TESLABOX_WEB_SESSION_SECRET"))),
		WebSessionTTL:               webSessionTTL,
		FFmpegPath:                  envString("TESLABOX_FFMPEG_PATH", "/opt/teslabox/media/bin/ffmpeg"),
		FFprobePath:                 envString("TESLABOX_FFPROBE_PATH", "/opt/teslabox/media/bin/ffprobe"),
	}

	if cfg.HTTPSCertificateEndpoint != "" || cfg.HTTPSCertificatePublicKey != "" {
		if cfg.CarPlayMFIDirectory == "" {
			cfg.CarPlayMFIDirectory = "/var/lib/teslabox-system/carplay/mfi"
		}
		if cfg.CarPlayServiceSettings == "" {
			cfg.CarPlayServiceSettings = "/var/lib/teslabox-system/carplay/service.json"
		}
		if cfg.CarPlayBrowserToken == "" {
			cfg.CarPlayBrowserToken = "/run/teslabox-carplay/browser-token"
		}
		if cfg.HTTPSDomain == "" {
			cfg.HTTPSDomain = certdelivery.ProductDomain
		}
		if cfg.HTTPSCertificateBundle == "" {
			cfg.HTTPSCertificateBundle = "/var/lib/teslabox-system/tls/certificate.pem"
		}
		if cfg.CarPlayPublicAddress == "" {
			cfg.CarPlayPublicAddress = certdelivery.DefaultPublicAddress
		}
	}
	if cfg.CarPlayServiceSettings == "" {
		// 旧实验部署尚未迁移时，登记入口仍读取本机已有配置。
		cfg.CarPlayServiceSettings = "/var/lib/teslabox-carplay-experiment/carplay-service.json"
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func catalogURLForManifest(manifestURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(manifestURL))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || !strings.HasSuffix(parsed.Path, "/latest.json") {
		return ""
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/latest.json") + "/history.json"
	parsed.RawPath = ""
	parsed.Fragment = ""
	return parsed.String()
}

func betaManifestURLForStable(manifestURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(manifestURL))
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || !strings.HasSuffix(parsed.Path, "/stable/latest.json") {
		return ""
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/stable/latest.json") + "/beta/latest.json"
	parsed.RawPath = ""
	parsed.Fragment = ""
	return parsed.String()
}

// Validate 拒绝可能挂载错误设备或造成忙循环的配置。
func (c Config) Validate() error {
	if c.HTTPSCertificateEndpoint != "" || c.HTTPSCertificatePublicKey != "" {
		if c.HTTPSDomain != certdelivery.ProductDomain || c.CarPlayPublicAddress == "" || !isSafeAbsolutePath(c.HTTPSCertificateIdentity) {
			return fmt.Errorf("统一证书更新需要产品固定域名、网络别名和私有身份路径")
		}
		if _, err := certdelivery.NewClient(certdelivery.ClientConfig{Endpoint: c.HTTPSCertificateEndpoint, PublicKey: c.HTTPSCertificatePublicKey, IdentityPath: c.HTTPSCertificateIdentity, CertificatePath: c.HTTPSCertificateBundle, MFIDirectory: c.CarPlayMFIDirectory}); err != nil {
			return fmt.Errorf("统一证书更新配置: %w", err)
		}
	}
	if c.CarPlayPublicAddress != "" {
		a, err := netip.ParseAddr(c.CarPlayPublicAddress)
		if err != nil || !a.Is4() || !a.IsGlobalUnicast() || a.IsPrivate() || c.HTTPSDomain == "" {
			return fmt.Errorf("TESLABOX_CARPLAY_PUBLIC_ADDRESS 需要公网形式的 IPv4 地址和有效 HTTPS 域名")
		}
	}
	if c.CarPlayBrowserToken != "" && !isSafeAbsolutePath(c.CarPlayBrowserToken) {
		return fmt.Errorf("TESLABOX_CARPLAY_BROWSER_TOKEN 必须是非根目录的绝对路径")
	}
	if c.CarPlayServiceSettings != "" && (!isSafeAbsolutePath(c.CarPlayServiceSettings) || path.Clean(c.CarPlayServiceSettings) != c.CarPlayServiceSettings || strings.Contains(c.CarPlayServiceSettings, `\`)) {
		return fmt.Errorf("TESLABOX_CARPLAY_SERVICE_SETTINGS 必须是非根目录的绝对路径")
	}
	if c.HTTPSAddr != "" || c.HTTPSDomain != "" || c.HTTPSCertificateBundle != "" {
		if err := webtls.ValidateDomain(c.HTTPSDomain); err != nil {
			return fmt.Errorf("TESLABOX_HTTPS_DOMAIN: %w", err)
		}
		if !isSafeAbsolutePath(c.HTTPSCertificateBundle) {
			return fmt.Errorf("TESLABOX_HTTPS_CERTIFICATE_BUNDLE 必须是非根目录的绝对路径")
		}
	}
	if strings.TrimSpace(c.HTTPAddr) == "" {
		return fmt.Errorf("TESLABOX_HTTP_ADDR 不能为空")
	}
	if !isSafeAbsolutePath(c.DataMountPath) {
		return fmt.Errorf("TESLABOX_DATA_MOUNT_PATH 必须是非根目录的绝对路径")
	}
	if !isSafeAbsolutePath(c.MaintenanceLockPath) || !isSafeAbsolutePath(c.MaintenanceOperationPath) {
		return fmt.Errorf("维护锁和维护操作状态必须是非根目录绝对路径")
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
	if c.WiFiInterface != "" && !isSafeInterfaceName(c.WiFiInterface) {
		return fmt.Errorf("TESLABOX_WIFI_INTERFACE 不是有效的网络接口名")
	}
	if !isSafeInterfaceName(c.APInterface) {
		return fmt.Errorf("TESLABOX_AP_INTERFACE 不是有效的网络接口名")
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
	if c.MaintenanceStatePath != "" && !isSafeAbsolutePath(c.MaintenanceStatePath) {
		return fmt.Errorf("TESLABOX_MAINTENANCE_STATE 必须是非根目录的绝对路径")
	}
	if c.OnboardingStatePath != "" && !isSafeAbsolutePath(c.OnboardingStatePath) {
		return fmt.Errorf("TESLABOX_ONBOARDING_STATE 必须是非根目录的绝对路径")
	}
	if !isSafeAbsolutePath(c.ExportStatePath) {
		return fmt.Errorf("TESLABOX_EXPORT_STATE 必须是非根目录的绝对路径")
	}
	timeZone := strings.TrimSpace(c.TeslaTimeZone)
	if timeZone == "" {
		timeZone = defaultTeslaTimeZone
	}
	if _, err := time.LoadLocation(timeZone); err != nil {
		return fmt.Errorf("TESLABOX_TESLA_TIME_ZONE 无效: %w", err)
	}
	for name, value := range map[string]string{
		"TESLABOX_UPDATE_DOWNLOAD_DIR": c.UpdateDownloadDir,
		"TESLABOX_UPDATE_REQUEST_PATH": c.UpdateRequestPath,
		"TESLABOX_UPDATE_RESULT_PATH":  c.UpdateResultPath,
		"TESLABOX_GADGET_PAUSE_STATE":  c.GadgetPauseStatePath,
		"TESLABOX_BOOT_ID_PATH":        c.BootIDPath,
		"TESLABOX_BOOT_UPTIME_PATH":    c.BootUptimePath,
	} {
		if !isSafeAbsolutePath(value) {
			return fmt.Errorf("%s 必须是非根目录的绝对路径", name)
		}
	}
	if c.IndexCacheDir != "" && !isSafeAbsolutePath(c.IndexCacheDir) {
		return fmt.Errorf("TESLABOX_INDEX_CACHE_DIR 必须是非根目录的绝对路径")
	}
	if c.UpdateChannelStatePath != "" && !isSafeAbsolutePath(c.UpdateChannelStatePath) {
		return fmt.Errorf("TESLABOX_UPDATE_CHANNEL_STATE 必须是非根目录的绝对路径")
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
	if c.ScanInterval <= 0 || c.SnapshotIdleInterval <= 0 || c.SnapshotActiveWindow <= 0 || c.PollInterval <= 0 || c.OperationTimeout <= 0 || c.SnapshotTimeout <= 0 || c.SnapshotRetryMax <= 0 || c.MediaProbeTimeout <= 0 || c.ExportMaxDuration < 0 || c.ShutdownTimeout <= 0 {
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
	if _, err := storagepolicy.NewSnapshotReservePolicy(c.MinFreeBytes, c.MinFreePercent, c.InitializationHeadroomBytes); err != nil {
		return fmt.Errorf("快照空间策略配置无效: %w", err)
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

func isSafeInterfaceName(value string) bool {
	if len(value) == 0 || len(value) > 15 {
		return false
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune("._-", char) {
			continue
		}
		return false
	}
	return true
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

func envNonNegativeDuration(name string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed < 0 {
		return 0, fmt.Errorf("解析 %s: 必须是非负时间", name)
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
