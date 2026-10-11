// Package recordingcache 提供按需复制、完整性校验和任务固定，供导出与上传共享系统盘录像。
package recordingcache

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const gib = uint64(1 << 30)

// ErrSpace 表示系统盘容量或安全余量不足，调用方应停止新任务而不是删除正在使用的数据。
var ErrSpace = errors.New("系统盘录像暂存空间不足")

// Config 将原始缓存和全部媒体产物分别限制，避免后台上传挤满系统盘。
type Config struct {
	Root          string
	MediaRoot     string
	MaxBytes      uint64
	MaxTotalBytes uint64
	MinFreeBytes  uint64
	IdleTTL       time.Duration
}

// File 的 Identity 必须包含源盘代次与相对路径，Version 必须是不可变快照标识。
// Path 由调用方检查为只读快照中的绝对路径；版本变化时仍会校验源内容。
type File struct {
	Identity string
	Path     string
	Version  string
}

type record struct {
	Version string `json:"version"`
	Size    uint64 `json:"size"`
	ModTime int64  `json:"mtime_ns"`
	Digest  string `json:"sha256"`
}

type object struct {
	Size     uint64    `json:"size"`
	LastUsed time.Time `json:"last_used"`
	Pins     int       `json:"-"`
}

type manifest struct {
	Format  int                `json:"format"`
	Records map[string]record  `json:"records"`
	Objects map[string]*object `json:"objects"`
}

// Store 串行准备原始文件，既限制磁盘写入并发，也让等待者能够通过 context 取消。
// 一个 Root 只能由一个进程中的一个 Store 管理。
type Store struct {
	cfg      Config
	gate     chan struct{}
	state    manifest
	reserved uint64
	space    func(string) (uint64, uint64, error)
	now      func() time.Time
}

// Lease 固定任务需要的原始文件；Paths 的键为调用方传入的源绝对路径。
type Lease struct {
	Paths map[string]string
	store *Store
	keys  []string
	once  sync.Once
}

// New 启动时核对清单与文件状态并清理断电残片，完整内容校验延迟到任务实际取用。
// 避免数 GiB 的缓存校验阻塞服务启动和车载录像。
func New(cfg Config) (*Store, error) {
	if cfg.Root == "" {
		return nil, fmt.Errorf("缺少录像暂存目录")
	}
	var err error
	cfg.Root, err = filepath.Abs(cfg.Root)
	if err != nil {
		return nil, err
	}
	if cfg.MediaRoot == "" {
		cfg.MediaRoot = filepath.Dir(cfg.Root)
	}
	cfg.MediaRoot, err = filepath.Abs(cfg.MediaRoot)
	if err != nil {
		return nil, err
	}
	if !within(cfg.MediaRoot, cfg.Root) || cfg.Root == cfg.MediaRoot {
		return nil, fmt.Errorf("原始缓存必须是媒体目录的子目录")
	}
	if err := os.MkdirAll(cfg.Root, 0o700); err != nil {
		return nil, fmt.Errorf("创建录像暂存目录: %w", err)
	}
	root, err := filepath.EvalSymlinks(cfg.Root)
	if err != nil {
		return nil, err
	}
	mediaRoot, err := filepath.EvalSymlinks(cfg.MediaRoot)
	if err != nil {
		return nil, err
	}
	if !within(mediaRoot, root) || root == mediaRoot {
		return nil, fmt.Errorf("录像暂存目录的真实路径不在媒体目录内")
	}
	cfg.Root, cfg.MediaRoot = root, mediaRoot
	if err := ensureSystemDisk(cfg.Root); err != nil {
		return nil, err
	}
	if err := ensureSystemDisk(cfg.MediaRoot); err != nil {
		return nil, err
	}
	total, _, err := diskSpace(cfg.Root)
	if err != nil {
		return nil, fmt.Errorf("检查系统盘空间: %w", err)
	}
	if cfg.MaxBytes == 0 {
		cfg.MaxBytes = min(8*gib, total/5)
	}
	if cfg.MaxTotalBytes == 0 {
		cfg.MaxTotalBytes = min(12*gib, total/10*3)
	}
	if cfg.MinFreeBytes == 0 {
		cfg.MinFreeBytes = max(2*gib, total/10)
	}
	if cfg.IdleTTL < 0 {
		return nil, fmt.Errorf("录像缓存闲置时间不能为负数")
	}
	s := &Store{cfg: cfg, gate: make(chan struct{}, 1), space: diskSpace, now: time.Now}
	s.gate <- struct{}{}
	if err := os.MkdirAll(s.objectDir(), 0o700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(s.objectDir())
	if err != nil || !info.IsDir() || info.Mode()&fs.ModeSymlink != 0 {
		return nil, fmt.Errorf("录像缓存对象目录必须是受管的真实目录")
	}
	if err := s.restore(); err != nil {
		return nil, err
	}
	return s, nil
}

func within(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func (s *Store) lock(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.gate:
		if err := ctx.Err(); err != nil {
			s.unlock()
			return err
		}
		return nil
	}
}

func (s *Store) unlock()                         { s.gate <- struct{}{} }
func (s *Store) objectDir() string               { return filepath.Join(s.cfg.Root, "objects") }
func (s *Store) objectPath(digest string) string { return filepath.Join(s.objectDir(), digest+".mp4") }

type plannedFile struct {
	file File
	info fs.FileInfo
	key  string
}

// EnsureLocal 只复制所选完整原文件，同一内容跨快照、跨任务复用。
// progress 在串行准备期间同步调用，不应回调 Store 方法；回调可取消传入的 context。
func (s *Store) EnsureLocal(ctx context.Context, files []File, progress func(done, total uint64)) (*Lease, error) {
	lease, _, err := s.EnsureLocalReserved(ctx, files, 0, progress)
	return lease, err
}

// EnsureLocalReserved 在固定已有命中后统一预算源文件与输出，避免预留输出先淘汰本次需要的缓存。
// 失败会撤销预留；成功后调用方分别释放输入 Lease 和输出预算。
func (s *Store) EnsureLocalReserved(ctx context.Context, files []File, outputBytes uint64, progress func(done, total uint64)) (*Lease, func(), error) {
	if err := s.lock(ctx); err != nil {
		return nil, nil, err
	}
	defer s.unlock()
	lease, err := s.ensureLocalLocked(ctx, files, outputBytes, progress)
	if err != nil {
		return nil, nil, err
	}
	return lease, s.reservationRelease(outputBytes), nil
}

func (s *Store) ensureLocalLocked(ctx context.Context, files []File, outputBytes uint64, progress func(done, total uint64)) (*Lease, error) {
	lease := &Lease{Paths: make(map[string]string), store: s}
	success := false
	reservationHeld := false
	defer func() {
		if !success {
			if reservationHeld {
				s.reserved -= outputBytes
			}
			s.unpin(lease.keys)
			_ = s.save()
		}
	}()
	plans := make([]plannedFile, 0, len(files))
	seenPaths := make(map[string]File)
	seenKeys := make(map[string]bool)
	missing := make(map[string]uint64)
	var total, needed uint64
	for _, file := range files {
		if file.Identity == "" || file.Version == "" || !filepath.IsAbs(file.Path) {
			return nil, fmt.Errorf("录像源必须提供身份、不可变版本和绝对路径")
		}
		if previous, ok := seenPaths[file.Path]; ok {
			if previous != file {
				return nil, fmt.Errorf("同一录像路径提供了不同身份或版本")
			}
			continue
		}
		seenPaths[file.Path] = file
		info, err := os.Lstat(file.Path)
		if err != nil || !info.Mode().IsRegular() {
			if err == nil {
				err = fmt.Errorf("录像源不是普通文件")
			}
			return nil, fmt.Errorf("检查录像源: %w", err)
		}
		size := uint64(info.Size())
		if size > ^uint64(0)-total {
			return nil, fmt.Errorf("录像源总大小超出可支持范围")
		}
		total += size
		cached, ok := s.state.Records[file.Identity]
		key := ""
		if ok && cached.Version == file.Version && cached.Size == size && cached.ModTime == info.ModTime().UnixNano() {
			key = cached.Digest
		}
		if key == "" {
			key, err = hashFile(ctx, file.Path)
			if err != nil {
				return nil, err
			}
			if err := unchanged(file.Path, info); err != nil {
				return nil, err
			}
		}
		if !seenKeys[key] {
			seenKeys[key] = true
			obj, exists := s.state.Objects[key]
			if exists {
				info, hashErr := os.Lstat(s.objectPath(key))
				digest := ""
				if hashErr == nil && info.Mode().IsRegular() && uint64(info.Size()) == obj.Size {
					digest, hashErr = hashFile(ctx, s.objectPath(key))
				}
				if hashErr != nil || digest != key || obj.Size != size {
					if ctx.Err() != nil {
						return nil, ctx.Err()
					}
					if obj.Pins != 0 {
						return nil, fmt.Errorf("正在使用的录像缓存损坏")
					}
					if err := s.removeObject(key); err != nil {
						return nil, err
					}
					exists = false
				}
			}
			if exists {
				obj.Pins++
				lease.keys = append(lease.keys, key)
			} else {
				missing[key] = size
				if size > ^uint64(0)-needed {
					return nil, ErrSpace
				}
				needed += size
			}
		}
		plans = append(plans, plannedFile{file: file, info: info, key: key})
	}
	if err := s.makeSpace(ctx, needed, true); err != nil {
		return nil, err
	}
	// 清单通过临时文件替换，峰值包括旧清单和新清单，不能只预算 MP4 字节。
	metadataBytes, err := s.plannedManifestBytes(plans)
	if err != nil {
		return nil, err
	}
	if metadataBytes > ^uint64(0)-needed {
		return nil, ErrSpace
	}
	if outputBytes > ^uint64(0)-s.reserved {
		return nil, ErrSpace
	}
	s.reserved += outputBytes
	reservationHeld = true
	if err := s.makeSpace(ctx, needed+metadataBytes, false); err != nil {
		return nil, err
	}
	var done uint64
	if progress != nil {
		progress(done, total)
	}
	for _, plan := range plans {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if _, required := missing[plan.key]; required {
			base := done
			if err := s.copyFile(ctx, plan, func(n uint64) {
				if progress != nil {
					progress(base+n, total)
				}
			}); err != nil {
				return nil, err
			}
			s.state.Objects[plan.key] = &object{Size: uint64(plan.info.Size()), LastUsed: s.now(), Pins: 1}
			lease.keys = append(lease.keys, plan.key)
			delete(missing, plan.key)
		}
		s.state.Objects[plan.key].LastUsed = s.now()
		s.state.Records[plan.file.Identity] = record{Version: plan.file.Version, Size: uint64(plan.info.Size()), ModTime: plan.info.ModTime().UnixNano(), Digest: plan.key}
		lease.Paths[plan.file.Path] = s.objectPath(plan.key)
		done += uint64(plan.info.Size())
		if progress != nil {
			progress(done, total)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := s.save(); err != nil {
		return nil, err
	}
	success = true
	return lease, nil
}

// AcquireLocal 完整核验 SHA256 后固定受管缓存，不接受任意系统盘文件。
func (s *Store) AcquireLocal(ctx context.Context, paths []string) (*Lease, error) {
	return s.acquireLocal(ctx, paths, true)
}

// PinLocalForRecovery 只核对受管路径、普通文件与清单长度，快速重建恢复任务的固定。
// 返回路径尚未完成内容校验，必须在后台调用 AcquireLocal 完整核验后才可消费文件。
// 服务应先恢复所有任务的固定，再启动定时清理，避免校验前被闲置策略删除。
func (s *Store) PinLocalForRecovery(ctx context.Context, paths []string) (*Lease, error) {
	return s.acquireLocal(ctx, paths, false)
}

func (s *Store) acquireLocal(ctx context.Context, paths []string, verify bool) (*Lease, error) {
	if err := s.lock(ctx); err != nil {
		return nil, err
	}
	defer s.unlock()
	lease := &Lease{Paths: make(map[string]string), store: s}
	success := false
	defer func() {
		if !success {
			s.unpin(lease.keys)
		}
	}()
	seen := make(map[string]bool)
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		key := strings.TrimSuffix(filepath.Base(path), ".mp4")
		obj := s.state.Objects[key]
		if !filepath.IsAbs(path) || !validDigest(key) || filepath.Clean(path) != s.objectPath(key) || obj == nil {
			return nil, fmt.Errorf("任务输入不属于受管录像缓存")
		}
		if !seen[key] {
			info, err := os.Lstat(path)
			if err != nil || !info.Mode().IsRegular() || uint64(info.Size()) != obj.Size {
				return nil, fmt.Errorf("任务录像缓存缺失或长度变化")
			}
			if verify {
				digest, err := hashFile(ctx, path)
				if err != nil || digest != key {
					if ctx.Err() != nil {
						return nil, ctx.Err()
					}
					return nil, fmt.Errorf("任务录像缓存缺失或损坏")
				}
			}
			obj.Pins++
			lease.keys = append(lease.keys, key)
			seen[key] = true
		}
		lease.Paths[path] = s.objectPath(key)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	success = true
	return lease, nil
}

// CheckSpace 在复制和转码期间检查实际占用，不将已经生成的产物与预留预算重复计费。
// 检查不等待准备锁，从而能够及时触发正在写入任务的取消。
func (s *Store) CheckSpace(ctx context.Context) error {
	used, err := directoryBytes(ctx, s.cfg.MediaRoot)
	if err != nil {
		return err
	}
	_, free, err := s.space(s.cfg.Root)
	if err != nil {
		return err
	}
	if used > s.cfg.MaxTotalBytes || free < s.cfg.MinFreeBytes {
		return fmt.Errorf("%w：媒体实际占用 %d，系统盘可用 %d 字节", ErrSpace, used, free)
	}
	return nil
}

// Release 允许多次调用，最后一个任务释放后才开始计算闲置时间。
func (l *Lease) Release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		<-l.store.gate
		defer l.store.unlock()
		l.store.unpin(l.keys)
		// 释放失败不会删除正在使用的数据；重启仍会校验完整性并按上次时间清理。
		_ = l.store.save()
	})
}

func (s *Store) unpin(keys []string) {
	for _, key := range keys {
		if obj := s.state.Objects[key]; obj != nil && obj.Pins > 0 {
			obj.Pins--
			if obj.Pins == 0 {
				obj.LastUsed = s.now()
			}
		}
	}
}

// Reserve 为导出或转码的最大同时占用量预留预算，返回的释放函数可重复调用。
// 预留期间已有产物仍计入实际占用，因此检查偏保守，不会靠预估挤占系统安全余量。
func (s *Store) Reserve(ctx context.Context, bytes uint64) (func(), error) {
	if err := s.lock(ctx); err != nil {
		return nil, err
	}
	defer s.unlock()
	if bytes > ^uint64(0)-s.reserved {
		return nil, ErrSpace
	}
	if err := s.makeSpace(ctx, bytes, false); err != nil {
		return nil, err
	}
	s.reserved += bytes
	return s.reservationRelease(bytes), nil
}

func (s *Store) reservationRelease(bytes uint64) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			<-s.gate
			defer s.unlock()
			s.reserved -= bytes
		})
	}
}

// Policy 返回当前缓存预算与留存策略，供管理界面展示真实生效配置。
func (s *Store) Policy() Config {
	<-s.gate
	defer s.unlock()
	return s.cfg
}

// SetIdleTTL 支持即时调整零至七天的闲置留存，零表示释放后可立即清理。
func (s *Store) SetIdleTTL(ttl time.Duration) error {
	if ttl < 0 || ttl > 168*time.Hour {
		return fmt.Errorf("录像缓存闲置留存必须在零至七天之间")
	}
	<-s.gate
	defer s.unlock()
	s.cfg.IdleTTL = ttl
	return nil
}

func (s *Store) cacheBytes() uint64 {
	var size uint64
	for _, obj := range s.state.Objects {
		size += obj.Size
	}
	return size
}

type byteCounter uint64

func (n *byteCounter) Write(data []byte) (int, error) {
	*n += byteCounter(len(data))
	return len(data), nil
}

func (s *Store) plannedManifestBytes(plans []plannedFile) (uint64, error) {
	projected := manifest{Format: 1, Records: make(map[string]record), Objects: make(map[string]*object)}
	for identity, rec := range s.state.Records {
		projected.Records[identity] = rec
	}
	for key, obj := range s.state.Objects {
		projected.Objects[key] = obj
	}
	for _, plan := range plans {
		projected.Records[plan.file.Identity] = record{Version: plan.file.Version, Size: uint64(plan.info.Size()), ModTime: plan.info.ModTime().UnixNano(), Digest: plan.key}
		projected.Objects[plan.key] = &object{Size: uint64(plan.info.Size()), LastUsed: s.now()}
	}
	var size byteCounter
	if err := json.NewEncoder(&size).Encode(projected); err != nil {
		return 0, err
	}
	// 时间戳小数位数可能变化，为每个新增计划留出少量序列化余量。
	return uint64(size) + uint64(len(plans))*64, nil
}

func fits(used, extra, limit uint64) bool { return used <= limit && extra <= limit-used }

func (s *Store) makeSpace(ctx context.Context, bytes uint64, cache bool) error {
	keys := s.reclaimable()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		used, err := directoryBytes(ctx, s.cfg.MediaRoot)
		if err != nil {
			return err
		}
		_, free, err := s.space(s.cfg.Root)
		if err != nil {
			return err
		}
		cacheFits := !cache || fits(s.cacheBytes(), bytes, s.cfg.MaxBytes)
		totalFits := fits(used, s.reserved, s.cfg.MaxTotalBytes) && fits(used+s.reserved, bytes, s.cfg.MaxTotalBytes)
		freeFits := fits(s.cfg.MinFreeBytes, s.reserved, free) && fits(s.cfg.MinFreeBytes+s.reserved, bytes, free)
		if cacheFits && totalFits && freeFits {
			return nil
		}
		if len(keys) == 0 {
			return fmt.Errorf("%w：缓存上限 %d，媒体上限 %d，需新增 %d 字节", ErrSpace, s.cfg.MaxBytes, s.cfg.MaxTotalBytes, bytes)
		}
		if err := s.removeObject(keys[0]); err != nil {
			return err
		}
		keys = keys[1:]
		if err := s.save(); err != nil {
			return err
		}
	}
}

func (s *Store) reclaimable() []string {
	keys := make([]string, 0, len(s.state.Objects))
	for key, obj := range s.state.Objects {
		if obj.Pins == 0 {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool {
		return s.state.Objects[keys[i]].LastUsed.Before(s.state.Objects[keys[j]].LastUsed)
	})
	return keys
}

// Cleanup 只淘汰闲置缓存，活动任务固定的输入和调用方管理的成品不会被删除。
func (s *Store) Cleanup(ctx context.Context) error {
	if err := s.lock(ctx); err != nil {
		return err
	}
	defer s.unlock()
	changed := false
	for _, key := range s.reclaimable() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.now().Sub(s.state.Objects[key].LastUsed) >= s.cfg.IdleTTL {
			if err := s.removeObject(key); err != nil {
				return err
			}
			changed = true
		}
	}
	if changed {
		if err := s.save(); err != nil {
			return err
		}
	}
	return s.makeSpace(ctx, 0, true)
}

// Run 由服务生命周期调用，取消后退出，不在包内生成独立后台协程。
func (s *Store) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = s.Cleanup(ctx)
		}
	}
}

func (s *Store) removeObject(key string) error {
	if obj := s.state.Objects[key]; obj != nil && obj.Pins != 0 {
		return fmt.Errorf("拒绝删除活动任务的录像缓存")
	}
	if err := os.Remove(s.objectPath(key)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	delete(s.state.Objects, key)
	for identity, rec := range s.state.Records {
		if rec.Digest == key {
			delete(s.state.Records, identity)
		}
	}
	return nil
}

func (s *Store) copyFile(ctx context.Context, plan plannedFile, progress func(uint64)) error {
	source, err := os.Open(plan.file.Path)
	if err != nil {
		return err
	}
	defer source.Close()
	tmp, err := os.CreateTemp(s.objectDir(), ".copy-*.part")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer func() { _ = tmp.Close(); _ = os.Remove(tmpPath) }()
	hash := sha256.New()
	n, err := copyContext(ctx, io.MultiWriter(tmp, hash), source, progress)
	if err != nil {
		return fmt.Errorf("复制录像到系统盘: %w", err)
	}
	if n != uint64(plan.info.Size()) || hex.EncodeToString(hash.Sum(nil)) != plan.key {
		return fmt.Errorf("录像复制期间源内容变化")
	}
	if err := unchanged(plan.file.Path, plan.info); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, s.objectPath(plan.key)); err != nil {
		return err
	}
	return syncDirectory(s.objectDir())
}

func unchanged(path string, before fs.FileInfo) error {
	after, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return fmt.Errorf("录像源在准备期间发生变化，请重新选择")
	}
	return nil
}

func copyContext(ctx context.Context, dst io.Writer, src io.Reader, progress func(uint64)) (uint64, error) {
	buf := make([]byte, 256*1024)
	var total uint64
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		n, readErr := src.Read(buf)
		if n > 0 {
			written, err := dst.Write(buf[:n])
			total += uint64(written)
			if err != nil {
				return total, err
			}
			if written != n {
				return total, io.ErrShortWrite
			}
			if progress != nil {
				progress(total)
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return total, ctx.Err()
			}
			return total, readErr
		}
	}
}

func hashFile(ctx context.Context, path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := copyContext(ctx, hash, file, nil); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func directoryBytes(ctx context.Context, root string) (uint64, error) {
	var total uint64
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && path != root {
				return nil
			}
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("媒体目录不允许符号链接: %s", path)
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("媒体目录存在非普通文件: %s", path)
		}
		if uint64(info.Size()) > ^uint64(0)-total {
			return ErrSpace
		}
		total += uint64(info.Size())
		return nil
	})
	return total, err
}

func validDigest(key string) bool {
	decoded, err := hex.DecodeString(key)
	return err == nil && len(decoded) == sha256.Size && key == strings.ToLower(key)
}

func (s *Store) restore() error {
	s.state = manifest{Format: 1, Records: make(map[string]record), Objects: make(map[string]*object)}
	data, err := os.ReadFile(filepath.Join(s.cfg.Root, "manifest.json"))
	if err == nil {
		if err := json.Unmarshal(data, &s.state); err != nil {
			return fmt.Errorf("读取录像缓存清单: %w", err)
		}
		if s.state.Format != 1 || s.state.Records == nil || s.state.Objects == nil {
			return fmt.Errorf("录像缓存清单格式不支持")
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	for key, obj := range s.state.Objects {
		if !validDigest(key) || obj == nil {
			delete(s.state.Objects, key)
			continue
		}
		info, err := os.Lstat(s.objectPath(key))
		// 启动只核对文件形态与大小；每个任务拿到本地路径前仍需验证完整 SHA256。
		if err != nil || !info.Mode().IsRegular() || uint64(info.Size()) != obj.Size {
			delete(s.state.Objects, key)
			continue
		}
		obj.Pins = 0
	}
	for identity, rec := range s.state.Records {
		if _, exists := s.state.Objects[rec.Digest]; !exists {
			delete(s.state.Records, identity)
		}
	}
	entries, err := os.ReadDir(s.objectDir())
	if err != nil {
		return err
	}
	for _, entry := range entries {
		key := strings.TrimSuffix(entry.Name(), ".mp4")
		_, exists := s.state.Objects[key]
		if !exists || entry.Name() != key+".mp4" {
			// 只清理本包管理目录中的非目录文件，不向其他路径递归删除。
			if entry.IsDir() {
				return fmt.Errorf("原始缓存目录存在未知子目录")
			}
			if err := os.Remove(filepath.Join(s.objectDir(), entry.Name())); err != nil {
				return err
			}
		}
	}
	entries, err = os.ReadDir(s.cfg.Root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".manifest-") && strings.HasSuffix(entry.Name(), ".part") && !entry.IsDir() {
			if err := os.Remove(filepath.Join(s.cfg.Root, entry.Name())); err != nil {
				return err
			}
		}
	}
	return s.save()
}

func (s *Store) save() error {
	tmp, err := os.CreateTemp(s.cfg.Root, ".manifest-*.part")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() { _ = tmp.Close(); _ = os.Remove(name) }()
	if err := json.NewEncoder(tmp).Encode(s.state); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, filepath.Join(s.cfg.Root, "manifest.json")); err != nil {
		return err
	}
	return syncDirectory(s.cfg.Root)
}

func openDirectory(path string) (*os.File, error) { return os.Open(path) }
