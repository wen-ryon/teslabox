// Package mediaassets 管理 Tesla 虚拟盘内需要独占维护窗口的媒体写入和录像删除。
package mediaassets

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/png"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	mediasnapshot "github.com/wen-ryon/teslabox/internal/snapshot"
	"github.com/wen-ryon/teslabox/internal/system"
)

const (
	maximumWrapBytes      int64 = 1 * 1024 * 1024
	maximumBoomboxBytes   int64 = 16 * 1024 * 1024
	maximumLightShowBytes int64 = 64 * 1024 * 1024
	maximumMusicBytes     int64 = 256 * 1024 * 1024
	maximumMusicBatch     int64 = 512 * 1024 * 1024
	maximumWrapCount            = 10
	maximumBoomboxCount         = 5
)

var (
	assetIDPattern        = regexp.MustCompile(`^[a-f0-9]{32}$`)
	loopDevicePattern     = regexp.MustCompile(`^/dev/loop[0-9]+$`)
	recordingFilePattern  = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}_\d{2}-\d{2}-\d{2}-[a-z_]+\.mp4$`)
	ErrPreviewUnavailable = errors.New("当前媒体没有可用预览")
)

// Category 是媒体在 Tesla 虚拟盘内的固定目录类型。
type Category string

const (
	CategoryBoombox   Category = "boombox"
	CategoryWrap      Category = "wrap"
	CategoryLightShow Category = "lightshow"
	CategoryMusic     Category = "music"
)

// Config 固定原始虚拟盘、状态目录和临时挂载点，避免把用户输入解释为文件系统路径。
type Config struct {
	ImagePath   string
	StatePath   string
	StagingPath string
	MountPath   string
}

// Maintenance 在写入虚拟盘前独占 Gadget 和录像快照，避免 Linux 与车机同时写入同一文件系统。
type Maintenance interface {
	BeginMediaMaintenance(context.Context) (func(), error)
}

// SnapshotSource 提供已冻结的只读媒体视图，避免扫描直接读取仍由 USB 主机写入的原始镜像。
type SnapshotSource interface {
	ActivateMedia(context.Context) error
	AcquireMediaSnapshotView() (*mediasnapshot.View, func())
}

// UploadFile 仅在一次 HTTP 上传处理期间持有输入流，调用方不得在 Upload 返回后继续使用它。
type UploadFile struct {
	Role     string
	FileName string
	Content  io.Reader
}

// File 描述一个将被放入 Tesla 目录的文件，不泄露服务器真实路径。
type File struct {
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"`
	Staged    string `json:"staged,omitempty"`
}

// Asset 描述一条媒体中心管理的资源及其待应用状态。
type Asset struct {
	ID               string    `json:"id"`
	Category         Category  `json:"category"`
	Name             string    `json:"name"`
	Files            []File    `json:"files"`
	CreatedAt        time.Time `json:"created_at"`
	Applied          bool      `json:"applied"`
	Pending          bool      `json:"pending"`
	PendingDelete    bool      `json:"pending_delete"`
	PreviewAvailable bool      `json:"preview_available"`
	External         bool      `json:"external,omitempty"`
}

// Status 是媒体中心可安全展示的资源清单。
type Status struct {
	Available      bool    `json:"available"`
	ImageAvailable bool    `json:"image_available"`
	NeedsApply     bool    `json:"needs_apply"`
	Assets         []Asset `json:"assets"`
	Message        string  `json:"message,omitempty"`
}

// RecordingDeleteRequest 只接收服务端从当前媒体快照解析出的录像相对路径。
type RecordingDeleteRequest struct {
	Paths []string
}

// RecordingDeleteResult 返回实际从虚拟盘删除的普通文件、事件目录和字节数。
type RecordingDeleteResult struct {
	RequestedFiles int   `json:"requested_files"`
	DeletedFiles   int   `json:"deleted_files"`
	DeletedBytes   int64 `json:"deleted_bytes"`
	DeletedEvents  int   `json:"deleted_events"`
}

type manifest struct {
	Version int     `json:"version"`
	Assets  []Asset `json:"assets"`
}

// Manager 将上传先落到数据盘的受控暂存区，只有用户确认 OTG 已物理断开后才写回虚拟盘。
type Manager struct {
	cfg         Config
	runner      system.Runner
	maintenance Maintenance
	snapshots   SnapshotSource
	now         func() time.Time
	mu          sync.Mutex
}

type uploadSession struct {
	manager  *Manager
	ctx      context.Context
	category Category
	existing []Asset
	created  []Asset
	light    *lightStaging
	closed   bool
}

type lightStaging struct {
	id        string
	directory string
	files     []File
}

// New 创建媒体资源管理器，并在启动阶段拒绝不安全的固定路径。
func New(cfg Config, runner system.Runner, maintenance Maintenance, snapshots ...SnapshotSource) (*Manager, error) {
	for name, value := range map[string]string{
		"镜像路径": cfg.ImagePath, "状态目录": cfg.StatePath, "暂存目录": cfg.StagingPath, "挂载目录": cfg.MountPath,
	} {
		if !filepath.IsAbs(value) || filepath.Clean(value) == string(filepath.Separator) {
			return nil, fmt.Errorf("%s必须是非根目录绝对路径", name)
		}
	}
	if runner == nil || maintenance == nil {
		return nil, errors.New("媒体资源管理器依赖不能为空")
	}
	if len(snapshots) > 1 {
		return nil, errors.New("媒体快照来源不能重复配置")
	}
	manager := &Manager{cfg: cfg, runner: runner, maintenance: maintenance, now: time.Now}
	if len(snapshots) == 1 {
		manager.snapshots = snapshots[0]
	}
	return manager, nil
}

// Status 返回 TeslaBox 管理和从车机存储直接发现的媒体；原始镜像不存在时仍返回可解释的空状态。
func (m *Manager) Status(ctx context.Context) (Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	assets, err := m.loadAssets()
	if err != nil {
		return Status{}, err
	}
	return m.statusWithDiscoveredAssets(ctx, assets), nil
}

func (m *Manager) statusWithDiscoveredAssets(ctx context.Context, assets []Asset) Status {
	status := m.statusLocked(assets)
	if !status.ImageAvailable || m.snapshots == nil {
		return status
	}
	if err := m.snapshots.ActivateMedia(ctx); err != nil {
		status.Message = "媒体快照暂不可用，已显示 TeslaBox 管理的文件；稍后刷新可读取电脑或车机直接写入的文件。"
		return status
	}
	view, release := m.snapshots.AcquireMediaSnapshotView()
	defer release()
	if view == nil {
		status.Message = "媒体快照正在准备，已显示 TeslaBox 管理的文件；稍后刷新可读取电脑或车机直接写入的文件。"
		return status
	}
	discovered, err := m.discoverSnapshotAssets(ctx, *view)
	if err != nil {
		status.Message = "媒体快照读取不完整，已显示 TeslaBox 管理的文件；稍后刷新可重试。"
		return status
	}
	status.Assets = publicAssets(mergeAssets(assets, discovered))
	return status
}

func (m *Manager) discoverSnapshotAssets(ctx context.Context, view mediasnapshot.View) ([]Asset, error) {
	assets, err := scanPrimaryAssets(view.MountPath)
	if err != nil {
		return nil, err
	}
	// 快照租约已保证该 loop 在本次扫描结束前不会被回收；复用它可避免再次探测分区丢失 p2。
	if !loopDevicePattern.MatchString(view.LoopDevice) {
		return assets, nil
	}
	music, musicPresent, err := m.scanSnapshotMusic(ctx, view.LoopDevice)
	if err != nil {
		return nil, err
	}
	if musicPresent {
		assets = append(assets, music...)
	}
	return assets, nil
}

func scanPrimaryAssets(root string) ([]Asset, error) {
	if _, err := trustedDirectory(root); err != nil {
		return nil, fmt.Errorf("检查媒体快照主分区: %w", err)
	}
	assets := make([]Asset, 0)
	for _, entry := range []struct {
		category  Category
		directory string
	}{
		{category: CategoryBoombox, directory: "Boombox"},
		{category: CategoryWrap, directory: "Wraps"},
		{category: CategoryLightShow, directory: "LightShow"},
	} {
		categoryAssets, err := scanCategoryDirectory(filepath.Join(root, entry.directory), entry.category)
		if err != nil {
			return nil, err
		}
		assets = append(assets, categoryAssets...)
	}
	return assets, nil
}

func (m *Manager) scanSnapshotMusic(ctx context.Context, loopDevice string) (assets []Asset, present bool, resultErr error) {
	if !loopDevicePattern.MatchString(loopDevice) {
		return nil, false, fmt.Errorf("媒体快照音乐分区 loop 设备无效: %q", loopDevice)
	}
	musicDevice := loopDevice + "p2"
	if _, err := m.runner.Run(ctx, "blkid", "-s", "TYPE", "-o", "value", musicDevice); err != nil {
		return nil, false, nil
	}
	mountPath := filepath.Join(filepath.Dir(m.cfg.MountPath), "media-assets-scan", "music")
	if err := os.MkdirAll(mountPath, 0o750); err != nil {
		return nil, false, fmt.Errorf("创建媒体快照音乐挂载点: %w", err)
	}
	if _, err := m.runner.Run(ctx, "mount", "--options", "ro,nosuid,nodev,noexec", "--", musicDevice, mountPath); err != nil {
		return nil, false, fmt.Errorf("只读挂载媒体快照音乐分区: %w", err)
	}
	defer func() {
		if _, err := m.runner.Run(context.WithoutCancel(ctx), "umount", "--", mountPath); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("卸载媒体快照音乐分区: %w", err))
		}
	}()
	assets, resultErr = scanMusicDirectory(mountPath)
	return assets, true, resultErr
}

// scanMusicDirectory 递归读取 MUSIC 分区中的音频文件。
// 用户通过电脑直接写入时通常会保留歌单的分类目录，因此不能只扫描分区根目录。
func scanMusicDirectory(directory string) ([]Asset, error) {
	if _, err := trustedDirectory(directory); err != nil {
		return nil, fmt.Errorf("检查 MUSIC 分区: %w", err)
	}
	assets := make([]Asset, 0)
	err := filepath.WalkDir(directory, func(name string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("读取 MUSIC 文件: %w", walkErr)
		}
		if name == directory {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		relative, err := filepath.Rel(directory, name)
		if err != nil {
			return fmt.Errorf("计算 MUSIC 文件相对路径: %w", err)
		}
		if entry.IsDir() {
			if !safeMusicDirectoryPath(relative) {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		relative = filepath.ToSlash(relative)
		if !safeMusicRelativePath(relative) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("读取 MUSIC 文件 %s: %w", relative, err)
		}
		file := File{Name: relative, SizeBytes: info.Size()}
		assets = append(assets, Asset{
			ID: externalAssetID(CategoryMusic, []File{file}), Category: CategoryMusic, Name: relative, Files: []File{file},
			CreatedAt: info.ModTime().UTC(), Applied: true, External: true,
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.SortFunc(assets, func(left, right Asset) int { return strings.Compare(left.Name, right.Name) })
	return assets, nil
}

func scanCategoryDirectory(directory string, category Category) ([]Asset, error) {
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return []Asset{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取媒体目录 %s: %w", filepath.Base(directory), err)
	}
	assets := make([]Asset, 0, len(entries))
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || !entry.Type().IsRegular() || !safeTargetName(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("读取媒体文件 %s: %w", entry.Name(), err)
		}
		file := File{Name: entry.Name(), SizeBytes: info.Size()}
		assets = append(assets, Asset{
			ID: externalAssetID(category, []File{file}), Category: category, Name: entry.Name(), Files: []File{file},
			CreatedAt: info.ModTime().UTC(), Applied: true, External: true,
		})
	}
	slices.SortFunc(assets, func(left, right Asset) int { return strings.Compare(left.Name, right.Name) })
	return assets, nil
}

func mergeAssets(managed, discovered []Asset) []Asset {
	merged := make([]Asset, 0, len(managed)+len(discovered))
	knownFiles := make(map[string]struct{}, len(managed))
	for _, asset := range managed {
		merged = append(merged, asset)
		if !asset.Applied && !asset.PendingDelete {
			continue
		}
		for _, file := range asset.Files {
			knownFiles[assetFileKey(asset.Category, file.Name)] = struct{}{}
		}
	}
	for _, asset := range discovered {
		allKnown := len(asset.Files) > 0
		for _, file := range asset.Files {
			if _, found := knownFiles[assetFileKey(asset.Category, file.Name)]; !found {
				allKnown = false
				break
			}
		}
		if !allKnown {
			merged = append(merged, asset)
		}
	}
	return merged
}

func assetFileKey(category Category, name string) string {
	return string(category) + "\x00" + strings.ToLower(name)
}

func externalAssetID(category Category, files []File) string {
	var value strings.Builder
	value.WriteString(string(category))
	for _, file := range files {
		value.WriteByte(0)
		value.WriteString(file.Name)
	}
	sum := sha256.Sum256([]byte(value.String()))
	return hex.EncodeToString(sum[:16])
}

func validExternalAsset(asset Asset) bool {
	if !asset.External || !asset.Applied || asset.Pending || asset.PendingDelete || !validCategory(asset.Category) || len(asset.Files) == 0 {
		return false
	}
	if !assetIDPattern.MatchString(asset.ID) || asset.ID != externalAssetID(asset.Category, asset.Files) {
		return false
	}
	for _, file := range asset.Files {
		if !safeExternalFileName(asset.Category, file.Name) || file.Staged != "" {
			return false
		}
	}
	return true
}

// Upload 校验并暂存上传内容，上传完成后不会立刻触碰正在提供给车机的虚拟盘。
func (m *Manager) Upload(ctx context.Context, category Category, files []UploadFile) ([]Asset, error) {
	return m.UploadStream(ctx, category, func(add func(UploadFile) error) error {
		for _, file := range files {
			if err := add(file); err != nil {
				return err
			}
		}
		return nil
	})
}

// UploadStream 允许 HTTP multipart 处理器边读取边写入数据盘暂存区，避免把大文件落到 TF 系统盘或一次性读入内存。
func (m *Manager) UploadStream(ctx context.Context, category Category, consume func(func(UploadFile) error) error) ([]Asset, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !validCategory(category) || consume == nil {
		return nil, errors.New("不支持的媒体分类或上传流为空")
	}
	existing, err := m.loadAssets()
	if err != nil {
		return nil, err
	}
	if err := m.ensureStagingRoot(); err != nil {
		return nil, err
	}
	session := &uploadSession{manager: m, ctx: ctx, category: category, existing: existing}
	cleanup := true
	defer func() {
		if cleanup {
			session.cleanup()
		}
	}()
	if err := consume(session.add); err != nil {
		return nil, err
	}
	created, err := session.complete()
	if err != nil {
		return nil, err
	}
	if err := validateNewAssets(category, created, existing); err != nil {
		return nil, err
	}
	if err := m.saveAssets(append(existing, created...)); err != nil {
		return nil, err
	}
	cleanup = false
	return publicAssets(created), nil
}

func (s *uploadSession) add(file UploadFile) error {
	if s.closed {
		return errors.New("媒体上传会话已结束")
	}
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if s.category == CategoryLightShow {
		return s.addLightShow(file)
	}
	limit := 20
	if s.category == CategoryMusic {
		limit = 50
	}
	if len(s.created) >= limit {
		return fmt.Errorf("一次最多上传 %d 个文件", limit)
	}
	asset, err := s.manager.stageSingle(s.ctx, s.category, file)
	if err != nil {
		return err
	}
	if s.category == CategoryMusic && totalAssetBytes(s.created)+asset.Files[0].SizeBytes > maximumMusicBatch {
		_ = os.RemoveAll(filepath.Join(s.manager.cfg.StagingPath, asset.ID))
		return fmt.Errorf("一次上传音乐总大小不能超过 %s", humanBytes(maximumMusicBatch))
	}
	s.created = append(s.created, asset)
	return nil
}

func (s *uploadSession) addLightShow(upload UploadFile) error {
	if s.light != nil && len(s.light.files) >= 2 {
		return errors.New("自定义灯光秀只能包含一个 .fseq 和一个音频文件")
	}
	name, err := normalizeName(CategoryLightShow, upload.FileName)
	if err != nil {
		return err
	}
	extension := strings.ToLower(filepath.Ext(name))
	if extension != ".fseq" && extension != ".mp3" && extension != ".wav" {
		return errors.New("自定义灯光秀仅支持 .fseq、.mp3 或 .wav")
	}
	if s.light == nil {
		id, err := newAssetID()
		if err != nil {
			return err
		}
		directory := filepath.Join(s.manager.cfg.StagingPath, id)
		if err := os.Mkdir(directory, 0o750); err != nil {
			return fmt.Errorf("创建灯光秀暂存目录: %w", err)
		}
		s.light = &lightStaging{id: id, directory: directory}
	}
	for _, existing := range s.light.files {
		if strings.EqualFold(filepath.Ext(existing.Name), extension) {
			return errors.New("灯光秀不能重复上传同类文件")
		}
	}
	staged := "sequence.fseq"
	if extension != ".fseq" {
		staged = "audio" + extension
	}
	size, err := copyUpload(s.ctx, filepath.Join(s.light.directory, staged), upload.Content, maximumLightShowBytes)
	if err != nil {
		return err
	}
	s.light.files = append(s.light.files, File{Name: name, SizeBytes: size, Staged: staged})
	return nil
}

func (s *uploadSession) complete() ([]Asset, error) {
	defer func() { s.closed = true }()
	if s.category != CategoryLightShow {
		if len(s.created) == 0 {
			return nil, errors.New("请选择至少一个文件")
		}
		return s.created, nil
	}
	if s.light == nil || len(s.light.files) != 2 {
		return nil, errors.New("自定义灯光秀必须同时上传一个 .fseq 文件和一个 .wav 或 .mp3 音频文件")
	}
	var sequence, audio File
	for _, file := range s.light.files {
		if strings.EqualFold(filepath.Ext(file.Name), ".fseq") {
			sequence = file
		} else {
			audio = file
		}
	}
	if sequence.Name == "" || audio.Name == "" || !strings.EqualFold(strings.TrimSuffix(sequence.Name, filepath.Ext(sequence.Name)), strings.TrimSuffix(audio.Name, filepath.Ext(audio.Name))) {
		return nil, errors.New("灯光秀 .fseq 与音频文件必须使用相同文件名")
	}
	asset := Asset{
		ID: s.light.id, Category: CategoryLightShow, Name: strings.TrimSuffix(sequence.Name, filepath.Ext(sequence.Name)),
		Files: []File{sequence, audio}, CreatedAt: s.manager.now().UTC(), Pending: true,
	}
	s.created = append(s.created, asset)
	s.light = nil
	return s.created, nil
}

func (s *uploadSession) cleanup() {
	for _, asset := range s.created {
		_ = os.RemoveAll(filepath.Join(s.manager.cfg.StagingPath, asset.ID))
	}
	if s.light != nil {
		_ = os.RemoveAll(s.light.directory)
	}
}

// Delete 将已应用文件标记为待删除；未应用文件会从暂存区直接移除。
func (m *Manager) Delete(_ context.Context, id string) (Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !assetIDPattern.MatchString(id) {
		return Status{}, errors.New("媒体资源标识不合法")
	}
	assets, err := m.loadAssets()
	if err != nil {
		return Status{}, err
	}
	position := slices.IndexFunc(assets, func(asset Asset) bool { return asset.ID == id })
	if position < 0 {
		return Status{}, errors.New("未找到指定媒体资源")
	}
	asset := assets[position]
	if asset.Pending && !asset.Applied {
		assets = append(assets[:position], assets[position+1:]...)
		if err := m.saveAssets(assets); err != nil {
			return Status{}, err
		}
		if err := os.RemoveAll(filepath.Join(m.cfg.StagingPath, asset.ID)); err != nil {
			return Status{}, fmt.Errorf("删除待应用媒体暂存文件: %w", err)
		}
	} else {
		assets[position].PendingDelete = true
		assets[position].Pending = false
		if err := m.saveAssets(assets); err != nil {
			return Status{}, err
		}
	}
	return m.statusLocked(assets), nil
}

// DeleteExternal 在独占维护窗口内删除电脑或车机直接写入的媒体文件。
// 文件名和目标目录均会再次校验，避免把浏览器输入解释为任意主机路径。
func (m *Manager) DeleteExternal(ctx context.Context, asset Asset) (Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !validExternalAsset(asset) {
		return Status{}, errors.New("直接写入媒体资源不合法")
	}
	if !regularFile(m.cfg.ImagePath) {
		return Status{}, errors.New("TeslaCam 虚拟盘尚未初始化")
	}
	assets, err := m.loadAssets()
	if err != nil {
		return Status{}, err
	}
	release, err := m.maintenance.BeginMediaMaintenance(ctx)
	if err != nil {
		return Status{}, fmt.Errorf("准备直接删除媒体维护窗口: %w", err)
	}
	defer release()
	asset.PendingDelete = true
	if err := m.applyLocked(ctx, []Asset{asset}); err != nil {
		return Status{}, fmt.Errorf("删除直接写入媒体: %w", err)
	}
	return m.statusLocked(assets), nil
}

// DeleteRecordings 在独占维护窗口内删除当前快照已经确认的 TeslaCam 录像。
// 已保存和哨兵录像按事件目录整体删除，最近录像只删除快照列出的 MP4，避免破坏共享目录。
// 路径在进入维护前全部归一化，避免无效请求先断开正在工作的 Gadget。
func (m *Manager) DeleteRecordings(ctx context.Context, request RecordingDeleteRequest) (result RecordingDeleteResult, resultErr error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	paths, err := normalizeRecordingPaths(request.Paths)
	if err != nil {
		return RecordingDeleteResult{}, err
	}
	if !regularFile(m.cfg.ImagePath) {
		return RecordingDeleteResult{}, errors.New("TeslaCam 虚拟盘尚未初始化")
	}
	result.RequestedFiles = len(paths)
	release, err := m.maintenance.BeginMediaMaintenance(ctx)
	if err != nil {
		return RecordingDeleteResult{}, fmt.Errorf("准备录像删除维护窗口: %w", err)
	}
	defer release()

	if err := os.MkdirAll(m.cfg.MountPath, 0o750); err != nil {
		return RecordingDeleteResult{}, fmt.Errorf("创建录像删除挂载目录: %w", err)
	}
	loopDevice, err := m.attachLoop(ctx)
	if err != nil {
		return RecordingDeleteResult{}, err
	}
	defer func() {
		if err := m.detachLoop(context.WithoutCancel(ctx), loopDevice); err != nil {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	if _, err := m.runner.Run(ctx, "udevadm", "settle"); err != nil {
		return RecordingDeleteResult{}, fmt.Errorf("等待录像分区设备: %w", err)
	}
	primary, err := m.mountPartition(ctx, loopDevice+"p1", 1, true)
	if err != nil {
		return RecordingDeleteResult{}, err
	}
	defer func() {
		if err := m.unmountPartition(context.WithoutCancel(ctx), primary); err != nil {
			resultErr = errors.Join(resultErr, err)
		}
	}()

	targets, err := recordingDeleteTargets(primary, paths)
	if err != nil {
		return RecordingDeleteResult{}, err
	}
	for _, target := range targets {
		if target.event {
			err = os.RemoveAll(target.name)
		} else {
			err = os.Remove(target.name)
		}
		if err != nil {
			return RecordingDeleteResult{}, fmt.Errorf("删除 TeslaCam 录像目标: %w", err)
		}
		result.DeletedFiles += target.files
		result.DeletedBytes += target.size
		if target.event {
			result.DeletedEvents++
		}
	}
	if _, err := m.runner.Run(ctx, "sync"); err != nil {
		return RecordingDeleteResult{}, fmt.Errorf("同步 TeslaCam 虚拟盘: %w", err)
	}
	return result, nil
}

// Apply 在安全维护窗口内把所有待应用变更写入对应分区，并保持 Gadget 暂停等待温重启恢复。
func (m *Manager) Apply(ctx context.Context) (Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	assets, err := m.loadAssets()
	if err != nil {
		return Status{}, err
	}
	if !hasPendingChanges(assets) {
		return m.statusLocked(assets), nil
	}
	if !regularFile(m.cfg.ImagePath) {
		return Status{}, errors.New("TeslaCam 虚拟盘尚未初始化")
	}
	release, err := m.maintenance.BeginMediaMaintenance(ctx)
	if err != nil {
		return Status{}, fmt.Errorf("准备媒体写入维护窗口: %w", err)
	}
	defer release()
	if err := m.applyLocked(ctx, assets); err != nil {
		return Status{}, err
	}
	for index := range assets {
		assets[index].Applied = true
		assets[index].Pending = false
	}
	deleted := make([]Asset, 0)
	remaining := make([]Asset, 0, len(assets))
	for _, asset := range assets {
		if asset.PendingDelete {
			deleted = append(deleted, asset)
			continue
		}
		asset.PendingDelete = false
		remaining = append(remaining, asset)
	}
	if err := m.saveAssets(remaining); err != nil {
		return Status{}, err
	}
	for _, asset := range deleted {
		if err := os.RemoveAll(filepath.Join(m.cfg.StagingPath, asset.ID)); err != nil {
			return Status{}, fmt.Errorf("清理已删除媒体预览: %w", err)
		}
	}
	return m.statusLocked(remaining), nil
}

// OpenPreview 仅为受支持的小型音频和皮肤文件提供受控预览句柄。
func (m *Manager) OpenPreview(_ context.Context, id string) (*os.File, Asset, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !assetIDPattern.MatchString(id) {
		return nil, Asset{}, errors.New("媒体资源标识不合法")
	}
	assets, err := m.loadAssets()
	if err != nil {
		return nil, Asset{}, err
	}
	assetIndex := slices.IndexFunc(assets, func(asset Asset) bool { return asset.ID == id })
	if assetIndex < 0 || !assets[assetIndex].PreviewAvailable || assets[assetIndex].PendingDelete {
		return nil, Asset{}, ErrPreviewUnavailable
	}
	asset := assets[assetIndex]
	if len(asset.Files) == 0 || !safeStagedName(asset.Files[0].Staged) {
		return nil, Asset{}, ErrPreviewUnavailable
	}
	name := filepath.Join(m.cfg.StagingPath, asset.ID, asset.Files[0].Staged)
	info, err := os.Lstat(name)
	if err != nil || !info.Mode().IsRegular() {
		return nil, Asset{}, ErrPreviewUnavailable
	}
	file, err := os.Open(name)
	if err != nil {
		return nil, Asset{}, fmt.Errorf("打开媒体预览: %w", err)
	}
	return file, publicAsset(asset), nil
}

func (m *Manager) stageSingle(ctx context.Context, category Category, upload UploadFile) (Asset, error) {
	name, err := normalizeName(category, upload.FileName)
	if err != nil {
		return Asset{}, err
	}
	id, err := newAssetID()
	if err != nil {
		return Asset{}, err
	}
	directory := filepath.Join(m.cfg.StagingPath, id)
	if err := os.Mkdir(directory, 0o750); err != nil {
		return Asset{}, fmt.Errorf("创建媒体暂存目录: %w", err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.RemoveAll(directory)
		}
	}()
	extension := strings.ToLower(filepath.Ext(name))
	staged := "asset" + extension
	size, err := copyUpload(ctx, filepath.Join(directory, staged), upload.Content, maximumSize(category))
	if err != nil {
		return Asset{}, err
	}
	asset := Asset{
		ID: id, Category: category, Name: name, Files: []File{{Name: name, SizeBytes: size, Staged: staged}},
		CreatedAt: m.now().UTC(), Pending: true, PreviewAvailable: category == CategoryBoombox || category == CategoryWrap,
	}
	if err := validateAssetContent(asset, directory); err != nil {
		return Asset{}, err
	}
	cleanup = false
	return asset, nil
}

func (m *Manager) applyLocked(ctx context.Context, assets []Asset) (resultErr error) {
	needsPrimary := false
	needsMusic := false
	for _, asset := range assets {
		if !asset.Pending && !asset.PendingDelete {
			continue
		}
		if asset.Category == CategoryMusic {
			needsMusic = true
		} else {
			needsPrimary = true
		}
	}
	if !needsPrimary && !needsMusic {
		return nil
	}
	if err := os.MkdirAll(m.cfg.MountPath, 0o750); err != nil {
		return fmt.Errorf("创建媒体维护挂载目录: %w", err)
	}
	loopDevice, err := m.attachLoop(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if err := m.detachLoop(context.WithoutCancel(ctx), loopDevice); err != nil {
			resultErr = errors.Join(resultErr, err)
		}
	}()
	if _, err := m.runner.Run(ctx, "udevadm", "settle"); err != nil {
		return fmt.Errorf("等待媒体分区设备: %w", err)
	}
	mounts := make(map[int]string, 2)
	if needsPrimary {
		primary, err := m.mountPartition(ctx, loopDevice+"p1", 1, true)
		if err != nil {
			return err
		}
		mounts[1] = primary
		defer func() {
			if err := m.unmountPartition(context.WithoutCancel(ctx), primary); err != nil {
				resultErr = errors.Join(resultErr, err)
			}
		}()
		if err := ensureTeslaMediaRootLayout(primary); err != nil {
			return err
		}
	}
	if needsMusic {
		music, err := m.mountPartition(ctx, loopDevice+"p2", 2, false)
		if err != nil {
			return err
		}
		mounts[2] = music
		defer func() {
			if err := m.unmountPartition(context.WithoutCancel(ctx), music); err != nil {
				resultErr = errors.Join(resultErr, err)
			}
		}()
	}
	for _, asset := range assets {
		if !asset.Pending {
			continue
		}
		if err := m.writeAsset(mounts, asset); err != nil {
			return err
		}
	}
	for _, asset := range assets {
		if !asset.PendingDelete {
			continue
		}
		if err := m.removeAsset(mounts, asset); err != nil {
			return err
		}
	}
	if _, err := m.runner.Run(ctx, "sync"); err != nil {
		return fmt.Errorf("同步媒体虚拟盘: %w", err)
	}
	return nil
}

func (m *Manager) attachLoop(ctx context.Context) (string, error) {
	output, err := m.runner.Run(ctx, "losetup", "--find", "--show", "--partscan", "--", m.cfg.ImagePath)
	if err != nil {
		return "", fmt.Errorf("建立媒体虚拟盘 loop 设备: %w", err)
	}
	device := strings.TrimSpace(string(output))
	if !loopDevicePattern.MatchString(device) {
		return "", fmt.Errorf("媒体虚拟盘 loop 设备无效: %q", device)
	}
	return device, nil
}

func (m *Manager) detachLoop(ctx context.Context, device string) error {
	if !loopDevicePattern.MatchString(device) {
		return errors.New("拒绝解绑无效媒体 loop 设备")
	}
	if _, err := m.runner.Run(ctx, "losetup", "--detach", device); err != nil {
		return fmt.Errorf("解绑媒体虚拟盘 loop 设备: %w", err)
	}
	return nil
}

func (m *Manager) mountPartition(ctx context.Context, device string, partition int, exFAT bool) (string, error) {
	mountPath := filepath.Join(m.cfg.MountPath, fmt.Sprintf("p%d", partition))
	if err := os.MkdirAll(mountPath, 0o750); err != nil {
		return "", fmt.Errorf("创建媒体分区挂载点: %w", err)
	}
	if exFAT {
		if _, err := m.runner.Run(ctx, "fsck.exfat", "-p", device); err != nil {
			var commandErr *system.CommandError
			if !errors.As(err, &commandErr) || commandErr.Code != 1 {
				return "", fmt.Errorf("检查媒体 exFAT 分区: %w", err)
			}
		}
	}
	if _, err := m.runner.Run(ctx, "mount", "--options", "rw,nosuid,nodev,noexec", "--", device, mountPath); err == nil {
		return mountPath, nil
	} else if !exFAT || !unknownExFAT(err) {
		return "", fmt.Errorf("挂载媒体分区: %w", err)
	}
	if _, err := m.runner.Run(ctx, "mount.exfat-fuse", "-o", "rw,nosuid,nodev,noexec", device, mountPath); err != nil {
		return "", fmt.Errorf("使用 exfat-fuse 挂载媒体分区: %w", err)
	}
	return mountPath, nil
}

func (m *Manager) unmountPartition(ctx context.Context, mountPath string) error {
	if _, err := m.runner.Run(ctx, "umount", "--", mountPath); err != nil {
		return fmt.Errorf("卸载媒体分区: %w", err)
	}
	return nil
}

func (m *Manager) writeAsset(mounts map[int]string, asset Asset) error {
	partition, directory, err := targetLocation(asset.Category)
	if err != nil {
		return err
	}
	root := mounts[partition]
	if root == "" {
		if asset.Category == CategoryMusic {
			return errors.New("当前虚拟盘没有 MUSIC FAT32 分区，请在设置中心重新初始化并启用音乐分区")
		}
		return errors.New("媒体主分区未挂载")
	}
	targetRoot, err := ensureTargetDirectory(root, directory)
	if err != nil {
		return err
	}
	for index, file := range asset.Files {
		if !safeTargetName(file.Name) || !safeStagedName(file.Staged) {
			return errors.New("媒体清单文件名不合法")
		}
		source := filepath.Join(m.cfg.StagingPath, asset.ID, file.Staged)
		if err := copyReplace(source, targetRoot, file.Name, asset.ID, index); err != nil {
			return fmt.Errorf("写入 %s: %w", file.Name, err)
		}
	}
	return nil
}

func (m *Manager) removeAsset(mounts map[int]string, asset Asset) error {
	partition, directory, err := targetLocation(asset.Category)
	if err != nil {
		return err
	}
	root := mounts[partition]
	if root == "" {
		if asset.Category == CategoryMusic {
			return errors.New("当前虚拟盘没有 MUSIC FAT32 分区，无法删除音乐文件")
		}
		return errors.New("媒体主分区未挂载")
	}
	targetRoot, err := ensureTargetDirectory(root, directory)
	if err != nil {
		return err
	}
	for _, file := range asset.Files {
		target, err := mediaTargetPath(targetRoot, asset.Category, file.Name)
		if err != nil {
			return err
		}
		info, err := os.Lstat(target)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("检查待删除媒体文件: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return errors.New("拒绝删除非普通媒体文件")
		}
		if err := os.Remove(target); err != nil {
			return fmt.Errorf("删除媒体文件: %w", err)
		}
	}
	return nil
}

func (m *Manager) loadAssets() ([]Asset, error) {
	name := filepath.Join(m.cfg.StatePath, "media-assets.json")
	info, err := os.Lstat(name)
	if errors.Is(err, os.ErrNotExist) {
		return []Asset{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取媒体清单: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("媒体清单不是可信普通文件")
	}
	content, err := os.ReadFile(name)
	if err != nil {
		return nil, fmt.Errorf("读取媒体清单内容: %w", err)
	}
	data := manifest{}
	if err := json.Unmarshal(content, &data); err != nil {
		return nil, fmt.Errorf("解析媒体清单: %w", err)
	}
	if data.Version != 1 {
		return nil, errors.New("媒体清单版本不受支持")
	}
	if err := validateManifest(data.Assets); err != nil {
		return nil, err
	}
	slices.SortFunc(data.Assets, func(left, right Asset) int { return right.CreatedAt.Compare(left.CreatedAt) })
	return data.Assets, nil
}

func (m *Manager) saveAssets(assets []Asset) (resultErr error) {
	if err := os.MkdirAll(m.cfg.StatePath, 0o750); err != nil {
		return fmt.Errorf("创建媒体状态目录: %w", err)
	}
	if err := validateManifest(assets); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(m.cfg.StatePath, ".media-assets-*")
	if err != nil {
		return fmt.Errorf("创建媒体清单临时文件: %w", err)
	}
	temporaryName := temporary.Name()
	closed := false
	defer func() {
		if !closed {
			if closeErr := temporary.Close(); closeErr != nil && resultErr == nil {
				resultErr = closeErr
			}
		}
		if resultErr != nil {
			_ = os.Remove(temporaryName)
		}
	}()
	if err := temporary.Chmod(0o640); err != nil {
		return err
	}
	if err := json.NewEncoder(temporary).Encode(manifest{Version: 1, Assets: assets}); err != nil {
		return fmt.Errorf("编码媒体清单: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("同步媒体清单: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("关闭媒体清单: %w", err)
	}
	closed = true
	if err := os.Rename(temporaryName, filepath.Join(m.cfg.StatePath, "media-assets.json")); err != nil {
		return fmt.Errorf("发布媒体清单: %w", err)
	}
	return nil
}

func (m *Manager) ensureStagingRoot() error {
	if err := os.MkdirAll(m.cfg.StagingPath, 0o750); err != nil {
		return fmt.Errorf("创建媒体暂存根目录: %w", err)
	}
	info, err := os.Lstat(m.cfg.StagingPath)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("媒体暂存目录不可信")
	}
	return nil
}

func (m *Manager) statusLocked(assets []Asset) Status {
	status := Status{Available: true, ImageAvailable: regularFile(m.cfg.ImagePath), Assets: publicAssets(assets)}
	status.NeedsApply = hasPendingChanges(assets)
	if !status.ImageAvailable {
		status.Message = "TeslaCam 虚拟盘尚未初始化；可先选择外接存储完成初始化。"
	}
	return status
}

func validateUploadShape(category Category, files []UploadFile) error {
	if !validCategory(category) {
		return errors.New("不支持的媒体分类")
	}
	if len(files) == 0 {
		return errors.New("请选择至少一个文件")
	}
	if category == CategoryLightShow {
		if len(files) != 2 {
			return errors.New("自定义灯光秀必须同时上传序列和音频文件")
		}
		return nil
	}
	limit := 20
	if category == CategoryMusic {
		limit = 50
	}
	if len(files) > limit {
		return fmt.Errorf("一次最多上传 %d 个文件", limit)
	}
	return nil
}

func validateNewAssets(category Category, created, existing []Asset) error {
	active := make([]Asset, 0, len(existing))
	for _, asset := range existing {
		if asset.Category == category && !asset.PendingDelete {
			active = append(active, asset)
		}
	}
	if category == CategoryWrap && len(active)+len(created) > maximumWrapCount {
		return fmt.Errorf("喷漆车间最多保留 %d 张皮肤", maximumWrapCount)
	}
	if category == CategoryBoombox && len(active)+len(created) > maximumBoomboxCount {
		return fmt.Errorf("锁车音效最多保留 %d 个文件", maximumBoomboxCount)
	}
	seen := make(map[string]struct{}, len(active)+len(created))
	for _, asset := range append(active, created...) {
		for _, file := range asset.Files {
			key := strings.ToLower(file.Name)
			if _, exists := seen[key]; exists {
				return fmt.Errorf("同一分类中已存在同名文件：%s", file.Name)
			}
			seen[key] = struct{}{}
		}
	}
	return nil
}

func validateAssetContent(asset Asset, directory string) error {
	if asset.Category != CategoryWrap {
		return nil
	}
	file, err := os.Open(filepath.Join(directory, asset.Files[0].Staged))
	if err != nil {
		return fmt.Errorf("读取皮肤文件: %w", err)
	}
	defer file.Close()
	configuration, format, err := image.DecodeConfig(file)
	if err != nil || format != "png" {
		return errors.New("喷漆车间皮肤必须是原生 PNG 文件")
	}
	if configuration.Width != configuration.Height || configuration.Width < 512 || configuration.Width > 1024 {
		return errors.New("喷漆车间皮肤必须是 512 到 1024 像素的正方形 PNG")
	}
	return nil
}

func validateManifest(assets []Asset) error {
	seenIDs := make(map[string]struct{}, len(assets))
	for _, asset := range assets {
		if !assetIDPattern.MatchString(asset.ID) || !validCategory(asset.Category) || asset.Name == "" || asset.CreatedAt.IsZero() {
			return errors.New("媒体清单包含不合法记录")
		}
		if _, found := seenIDs[asset.ID]; found {
			return errors.New("媒体清单存在重复资源标识")
		}
		seenIDs[asset.ID] = struct{}{}
		if len(asset.Files) == 0 || (asset.Category == CategoryLightShow && len(asset.Files) != 2) {
			return errors.New("媒体清单文件数量不合法")
		}
		for _, file := range asset.Files {
			if !safeTargetName(file.Name) || !safeStagedName(file.Staged) || file.SizeBytes < 0 {
				return errors.New("媒体清单文件不合法")
			}
		}
	}
	return nil
}

func normalizeName(category Category, raw string) (string, error) {
	name := strings.TrimSpace(raw)
	if name == "" || !utf8.ValidString(name) || filepath.Base(name) != name || strings.ContainsAny(name, "/\\\x00\r\n") || strings.HasPrefix(name, ".") {
		return "", errors.New("文件名不合法")
	}
	if len(name) > 100 {
		return "", errors.New("文件名过长")
	}
	extension := strings.ToLower(filepath.Ext(name))
	base := strings.TrimSuffix(name, filepath.Ext(name))
	if base == "" {
		return "", errors.New("文件名不合法")
	}
	switch category {
	case CategoryWrap:
		if extension != ".png" {
			return "", errors.New("喷漆车间皮肤仅支持 PNG 文件")
		}
		return normalizedASCIIName(base, extension, 30, true), nil
	case CategoryBoombox:
		if extension != ".mp3" && extension != ".wav" {
			return "", errors.New("锁车音效仅支持 .mp3 或 .wav 文件")
		}
		return normalizedASCIIName(base, extension, 64, false), nil
	case CategoryLightShow:
		if extension != ".fseq" && extension != ".mp3" && extension != ".wav" {
			return "", errors.New("灯光秀仅支持 .fseq、.mp3 或 .wav 文件")
		}
		return normalizedASCIIName(base, extension, 64, true), nil
	case CategoryMusic:
		if !slices.Contains([]string{".mp3", ".wav", ".m4a", ".aac", ".flac"}, extension) || !safeMusicName(name) {
			return "", errors.New("音乐仅支持 .mp3、.wav、.m4a、.aac 或 .flac 文件")
		}
	}
	return name, nil
}

func normalizedASCIIName(base, extension string, maximumBaseLength int, allowSpaces bool) string {
	var builder strings.Builder
	for _, value := range base {
		switch {
		case value >= 'a' && value <= 'z', value >= 'A' && value <= 'Z', value >= '0' && value <= '9', value == '_', value == '-':
			builder.WriteRune(value)
		case value == ' ' && allowSpaces:
			builder.WriteRune(value)
		case value == ' ':
			builder.WriteByte('_')
		default:
			builder.WriteByte('-')
		}
	}
	cleaned := strings.Trim(builder.String(), " .-_")
	if cleaned == "" {
		cleaned = "TeslaBox"
	}
	if len(cleaned) > maximumBaseLength {
		cleaned = strings.Trim(cleaned[:maximumBaseLength], " .-_")
		if cleaned == "" {
			cleaned = "TeslaBox"
		}
	}
	return cleaned + extension
}

func safeMusicName(name string) bool {
	for _, value := range name {
		if unicode.IsLetter(value) || unicode.IsNumber(value) || value == ' ' || value == '_' || value == '-' || value == '.' {
			continue
		}
		return false
	}
	return true
}

func copyUpload(ctx context.Context, target string, source io.Reader, maximum int64) (int64, error) {
	if source == nil {
		return 0, errors.New("上传文件内容为空")
	}
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o640)
	if err != nil {
		return 0, fmt.Errorf("创建上传暂存文件: %w", err)
	}
	defer file.Close()
	written, err := io.Copy(file, io.LimitReader(&contextReader{ctx: ctx, reader: source}, maximum+1))
	if err != nil {
		return 0, fmt.Errorf("保存上传文件: %w", err)
	}
	if written == 0 {
		return 0, errors.New("上传文件为空")
	}
	if written > maximum {
		return 0, fmt.Errorf("文件超过 %s 限制", humanBytes(maximum))
	}
	if err := file.Sync(); err != nil {
		return 0, fmt.Errorf("同步上传文件: %w", err)
	}
	return written, nil
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(buffer []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(buffer)
}

func targetLocation(category Category) (int, string, error) {
	switch category {
	case CategoryBoombox:
		return 1, "Boombox", nil
	case CategoryWrap:
		return 1, "Wraps", nil
	case CategoryLightShow:
		return 1, "LightShow", nil
	case CategoryMusic:
		return 2, "", nil
	default:
		return 0, "", errors.New("不支持的媒体分类")
	}
}

func ensureTargetDirectory(root, directory string) (string, error) {
	if directory == "" {
		return trustedDirectory(root)
	}
	target := filepath.Join(root, directory)
	if filepath.Dir(target) != filepath.Clean(root) {
		return "", errors.New("媒体目标目录不合法")
	}
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.Mkdir(target, 0o755); err != nil {
			return "", fmt.Errorf("创建 Tesla 媒体目录: %w", err)
		}
		return target, nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", errors.New("Tesla 媒体目录不可信")
	}
	return target, nil
}

// ensureTeslaMediaRootLayout 保持车机可识别目录始终位于主 exFAT 分区根目录，避免用户上传一个媒体类型后缺少其余目录。
func ensureTeslaMediaRootLayout(root string) error {
	for _, directory := range []string{"TeslaCam", "Boombox", "Wraps", "LightShow"} {
		if _, err := ensureTargetDirectory(root, directory); err != nil {
			return err
		}
	}
	return nil
}

func trustedDirectory(name string) (string, error) {
	info, err := os.Lstat(name)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", errors.New("媒体分区挂载目录不可信")
	}
	return name, nil
}

func copyReplace(source, directory, targetName, assetID string, index int) (resultErr error) {
	sourceInfo, err := os.Lstat(source)
	if err != nil || sourceInfo.Mode()&os.ModeSymlink != 0 || !sourceInfo.Mode().IsRegular() {
		return errors.New("媒体暂存文件不可信或不存在")
	}
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	temporaryName := filepath.Join(directory, fmt.Sprintf(".%s-%d.partial", assetID, index))
	output, err := os.OpenFile(temporaryName, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if !closed {
			if closeErr := output.Close(); closeErr != nil && resultErr == nil {
				resultErr = closeErr
			}
		}
		if resultErr != nil {
			_ = os.Remove(temporaryName)
		}
	}()
	if _, err := io.Copy(output, input); err != nil {
		return err
	}
	if err := output.Sync(); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	closed = true
	if err := os.Rename(temporaryName, filepath.Join(directory, targetName)); err != nil {
		return err
	}
	return nil
}

func maximumSize(category Category) int64 {
	switch category {
	case CategoryWrap:
		return maximumWrapBytes
	case CategoryBoombox:
		return maximumBoomboxBytes
	case CategoryLightShow:
		return maximumLightShowBytes
	default:
		return maximumMusicBytes
	}
}

func totalAssetBytes(assets []Asset) int64 {
	var total int64
	for _, asset := range assets {
		for _, file := range asset.Files {
			total += file.SizeBytes
		}
	}
	return total
}

func validCategory(category Category) bool {
	return category == CategoryBoombox || category == CategoryWrap || category == CategoryLightShow || category == CategoryMusic
}

func publicAssets(assets []Asset) []Asset {
	result := make([]Asset, len(assets))
	for index, asset := range assets {
		result[index] = publicAsset(asset)
	}
	return result
}

func publicAsset(asset Asset) Asset {
	asset.Files = slices.Clone(asset.Files)
	for index := range asset.Files {
		asset.Files[index].Staged = ""
	}
	return asset
}

func hasPendingChanges(assets []Asset) bool {
	return slices.ContainsFunc(assets, func(asset Asset) bool { return asset.Pending || asset.PendingDelete })
}

func regularFile(name string) bool {
	info, err := os.Lstat(name)
	return err == nil && info.Mode()&os.ModeSymlink == 0 && info.Mode().IsRegular()
}

func safeTargetName(name string) bool {
	return name != "" && filepath.Base(name) == name && !strings.ContainsAny(name, "/\\\x00\r\n") && !strings.HasPrefix(name, ".")
}

const maximumRecordingDeleteFiles = 10000

func normalizeRecordingPaths(values []string) ([]string, error) {
	if len(values) == 0 || len(values) > maximumRecordingDeleteFiles {
		return nil, fmt.Errorf("录像删除文件数量必须在 1 到 %d 之间", maximumRecordingDeleteFiles)
	}
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "" || strings.ContainsAny(value, "\\\x00\r\n") || strings.HasPrefix(value, "/") {
			return nil, errors.New("录像路径不合法")
		}
		cleaned := path.Clean(value)
		if cleaned != value || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
			return nil, errors.New("录像路径越过 TeslaCam 目录")
		}
		parts := strings.Split(cleaned, "/")
		validLayout := len(parts) == 3 && parts[0] == "TeslaCam" && parts[1] == "RecentClips"
		validLayout = validLayout || len(parts) == 4 && parts[0] == "TeslaCam" &&
			(parts[1] == "SavedClips" || parts[1] == "SentryClips")
		if !validLayout {
			return nil, errors.New("录像路径不属于可删除的 TeslaCam 目录")
		}
		for _, part := range parts {
			if !safeTargetName(part) {
				return nil, errors.New("录像路径包含不安全目录名")
			}
		}
		if !recordingFilePattern.MatchString(strings.ToLower(parts[len(parts)-1])) {
			return nil, errors.New("录像文件名不符合 TeslaCam 规则")
		}
		if _, found := seen[cleaned]; found {
			continue
		}
		seen[cleaned] = struct{}{}
		result = append(result, cleaned)
	}
	if len(result) == 0 {
		return nil, errors.New("录像删除文件为空")
	}
	return result, nil
}

type recordingDeleteTarget struct {
	name  string
	size  int64
	files int
	event bool
}

func recordingDeleteTargets(root string, paths []string) ([]recordingDeleteTarget, error) {
	if _, err := trustedDirectory(root); err != nil {
		return nil, fmt.Errorf("检查录像分区挂载点: %w", err)
	}
	targets := make([]recordingDeleteTarget, 0, len(paths))
	seen := make(map[string]struct{}, len(paths))
	for _, relative := range paths {
		parts := strings.Split(relative, "/")
		event := len(parts) == 4 && (parts[1] == "SavedClips" || parts[1] == "SentryClips")
		targetRelative := relative
		if event {
			targetRelative = strings.Join(parts[:3], "/")
		}
		if _, found := seen[targetRelative]; found {
			continue
		}
		seen[targetRelative] = struct{}{}

		target, info, found, err := recordingDeleteTargetPath(root, targetRelative, event)
		if err != nil {
			return nil, err
		}
		if !found {
			continue
		}
		if !event {
			targets = append(targets, recordingDeleteTarget{name: target, size: info.Size(), files: 1})
			continue
		}
		files, size, err := recordingEventUsage(target)
		if err != nil {
			return nil, err
		}
		targets = append(targets, recordingDeleteTarget{name: target, size: size, files: files, event: true})
	}
	return targets, nil
}

func recordingDeleteTargetPath(root, relative string, directory bool) (string, os.FileInfo, bool, error) {
	parts := strings.Split(relative, "/")
	parent := filepath.Clean(root)
	for _, part := range parts[:len(parts)-1] {
		parent = filepath.Join(parent, part)
		info, err := os.Lstat(parent)
		if errors.Is(err, os.ErrNotExist) {
			return "", nil, false, nil
		}
		if err != nil {
			return "", nil, false, fmt.Errorf("检查录像目录: %w", err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", nil, false, errors.New("拒绝经过非普通目录删除录像")
		}
	}
	target := filepath.Join(parent, parts[len(parts)-1])
	info, err := os.Lstat(target)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil, false, nil
	}
	if err != nil {
		return "", nil, false, fmt.Errorf("检查待删除录像: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", nil, false, errors.New("拒绝删除非普通录像文件")
	}
	if directory && !info.IsDir() {
		return "", nil, false, errors.New("录像事件目标不是可信目录")
	}
	if !directory && !info.Mode().IsRegular() {
		return "", nil, false, errors.New("录像目标不是可信普通文件")
	}
	return target, info, true, nil
}

func recordingEventUsage(directory string) (files int, size int64, resultErr error) {
	err := filepath.WalkDir(directory, func(name string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("读取录像事件目录: %w", walkErr)
		}
		if name == directory || entry.IsDir() {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("录像事件目录包含符号链接，拒绝递归删除")
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("读取录像事件文件: %w", err)
		}
		if !info.Mode().IsRegular() {
			return errors.New("录像事件目录包含非普通文件，拒绝递归删除")
		}
		files++
		size += info.Size()
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	return files, size, nil
}

func safeExternalFileName(category Category, name string) bool {
	if category == CategoryMusic {
		return safeMusicRelativePath(name)
	}
	return safeTargetName(name)
}

func safeMusicDirectoryPath(name string) bool {
	return safeMusicPath(filepath.ToSlash(name), false)
}

func safeMusicRelativePath(name string) bool {
	return safeMusicPath(name, true)
}

func safeMusicPath(name string, requireAudioFile bool) bool {
	if name == "" || strings.ContainsAny(name, "\\\x00\r\n") || strings.HasPrefix(name, "/") {
		return false
	}
	cleaned := path.Clean(name)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") || cleaned != name {
		return false
	}
	for _, part := range strings.Split(cleaned, "/") {
		if !safeTargetName(part) {
			return false
		}
	}
	return !requireAudioFile || isSupportedMusicFile(cleaned)
}

func isSupportedMusicFile(name string) bool {
	return slices.Contains([]string{".mp3", ".wav", ".m4a", ".aac", ".flac"}, strings.ToLower(path.Ext(name)))
}

func mediaTargetPath(root string, category Category, name string) (string, error) {
	if !safeExternalFileName(category, name) {
		return "", errors.New("媒体清单文件名不合法")
	}
	if category != CategoryMusic {
		return filepath.Join(root, name), nil
	}
	target := filepath.Join(root, filepath.FromSlash(name))
	relative, err := filepath.Rel(root, target)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", errors.New("MUSIC 文件路径不合法")
	}
	return target, nil
}

func safeStagedName(name string) bool {
	return regexp.MustCompile(`^(asset|audio|sequence)\.[a-z0-9]+$`).MatchString(name)
}

func unknownExFAT(err error) bool {
	var commandErr *system.CommandError
	return errors.As(err, &commandErr) && strings.Contains(strings.ToLower(commandErr.Output), "unknown filesystem type 'exfat'")
}

func newAssetID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("生成媒体资源标识: %w", err)
	}
	return hex.EncodeToString(value), nil
}

func humanBytes(value int64) string {
	return fmt.Sprintf("%.0f MB", float64(value)/(1024*1024))
}
