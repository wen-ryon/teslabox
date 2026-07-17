package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const registryVersion = 1

// DeviceRecord 是保存在系统盘上的数据盘身份和初始化进度。
type DeviceRecord struct {
	UUID          string    `json:"uuid,omitempty"`
	Label         string    `json:"label,omitempty"`
	StablePath    string    `json:"stable_path,omitempty"`
	Serial        string    `json:"serial,omitempty"`
	Model         string    `json:"model,omitempty"`
	SizeBytes     int64     `json:"size_bytes,omitempty"`
	Status        string    `json:"status"`
	InitializedAt time.Time `json:"initialized_at,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
	LastError     string    `json:"last_error,omitempty"`
}

// IdentitySource 为发现和挂载逻辑提供可动态切换的活动数据盘身份。
type IdentitySource interface {
	Active() (DeviceRecord, bool, error)
}

type registryDocument struct {
	Version               int            `json:"version"`
	ActiveUUID            string         `json:"active_uuid,omitempty"`
	InitializationPending bool           `json:"initialization_pending,omitempty"`
	Devices               []DeviceRecord `json:"devices"`
}

// FileRegistry 使用原子替换保存设备登记，文件位于 TF 系统盘而不是可拔数据盘。
type FileRegistry struct {
	path string
	mu   sync.Mutex
}

// NewFileRegistry 创建系统盘设备登记册。
func NewFileRegistry(path string) *FileRegistry {
	return &FileRegistry{path: filepath.Clean(path)}
}

// EnsureLegacy 在升级旧配置时登记原有 UUID，避免现有数据盘失去身份。
func (r *FileRegistry) EnsureLegacy(uuid, label string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, err := os.Stat(r.path)
	if err == nil {
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("检查存储设备登记册: %w", err)
	}
	uuid = strings.TrimSpace(uuid)
	label = strings.TrimSpace(label)
	if uuid == "" && label == "" {
		return nil
	}
	now := time.Now().UTC()
	record := DeviceRecord{UUID: uuid, Label: label, Status: "ready", InitializedAt: now, UpdatedAt: now}
	document := registryDocument{Version: registryVersion, ActiveUUID: uuid, Devices: []DeviceRecord{record}}
	return r.save(document)
}

// Active 返回当前允许自动挂载和导出的已完成数据盘。
func (r *FileRegistry) Active() (DeviceRecord, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	document, err := r.load()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return DeviceRecord{}, false, nil
		}
		return DeviceRecord{}, false, err
	}
	if document.InitializationPending {
		return DeviceRecord{}, false, nil
	}
	for _, record := range document.Devices {
		if record.Status != "ready" {
			continue
		}
		if document.ActiveUUID != "" && strings.EqualFold(record.UUID, document.ActiveUUID) {
			return record, true, nil
		}
		if document.ActiveUUID == "" && record.UUID == "" && record.Label != "" {
			return record, true, nil
		}
	}
	return DeviceRecord{}, false, nil
}

// Records 返回登记册快照，供设置页面判断已完成、未完成和失败设备。
func (r *FileRegistry) Records() ([]DeviceRecord, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	document, err := r.load()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []DeviceRecord{}, nil
		}
		return nil, err
	}
	return slices.Clone(document.Devices), nil
}

// Begin 先持久化初始化意图，异常断电后仍能识别未完成设备。
func (r *FileRegistry) Begin(record DeviceRecord) error {
	record.Status = "initializing"
	record.UpdatedAt = time.Now().UTC()
	record.LastError = ""
	return r.upsert(record, false, true)
}

// Fail 保存初始化失败阶段，不会把未完成设备设为活动数据盘。
func (r *FileRegistry) Fail(record DeviceRecord, cause error) error {
	record.Status = "failed"
	record.UpdatedAt = time.Now().UTC()
	if cause != nil {
		record.LastError = cause.Error()
	}
	return r.upsert(record, false, true)
}

// Complete 只有整个镜像和目录布局完成后才切换活动数据盘。
func (r *FileRegistry) Complete(record DeviceRecord) error {
	now := time.Now().UTC()
	record.Status = "ready"
	record.InitializedAt = now
	record.UpdatedAt = now
	record.LastError = ""
	return r.upsert(record, true, false)
}

// CancelPending 删除未完成记录并恢复原活动设备选择。
func (r *FileRegistry) CancelPending(stablePath, serial string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	document, err := r.load()
	if err != nil {
		return err
	}
	filtered := document.Devices[:0]
	found := false
	for _, record := range document.Devices {
		if record.Status != "ready" && record.StablePath == stablePath && record.Serial == serial {
			found = true
			continue
		}
		filtered = append(filtered, record)
	}
	if !found {
		return errors.New("未找到可取消的初始化记录")
	}
	document.Devices = filtered
	document.InitializationPending = false
	return r.save(document)
}

func (r *FileRegistry) upsert(record DeviceRecord, activate, pending bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	document, err := r.load()
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if errors.Is(err, os.ErrNotExist) {
		document = registryDocument{Version: registryVersion, Devices: []DeviceRecord{}}
	}
	index := -1
	for current := range document.Devices {
		if sameRecord(document.Devices[current], record) {
			index = current
			break
		}
	}
	if index < 0 {
		document.Devices = append(document.Devices, record)
	} else {
		document.Devices[index] = record
	}
	if activate {
		document.ActiveUUID = record.UUID
	}
	document.InitializationPending = pending
	return r.save(document)
}

func sameRecord(left, right DeviceRecord) bool {
	if left.UUID != "" && right.UUID != "" {
		return strings.EqualFold(left.UUID, right.UUID)
	}
	if left.UUID == "" && left.Serial == "" && left.Label != "" && strings.EqualFold(left.Label, right.Label) {
		return true
	}
	return left.Serial != "" && left.Serial == right.Serial && left.SizeBytes == right.SizeBytes
}

func (r *FileRegistry) load() (registryDocument, error) {
	data, err := os.ReadFile(r.path)
	if err != nil {
		return registryDocument{}, err
	}
	var document registryDocument
	if err := json.Unmarshal(data, &document); err != nil {
		return registryDocument{}, fmt.Errorf("解析存储设备登记册: %w", err)
	}
	if document.Version != registryVersion {
		return registryDocument{}, fmt.Errorf("存储设备登记册版本不支持: %d", document.Version)
	}
	return document, nil
}

func (r *FileRegistry) save(document registryDocument) (resultErr error) {
	if err := os.MkdirAll(filepath.Dir(r.path), 0o750); err != nil {
		return fmt.Errorf("创建存储设备登记目录: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(r.path), ".storage-registry-*")
	if err != nil {
		return fmt.Errorf("创建临时存储设备登记册: %w", err)
	}
	temporaryName := temporary.Name()
	closed := false
	defer func() {
		if !closed {
			if closeErr := temporary.Close(); closeErr != nil {
				resultErr = errors.Join(resultErr, closeErr)
			}
		}
		if resultErr != nil {
			_ = os.Remove(temporaryName)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("设置存储设备登记册权限: %w", err)
	}
	encoder := json.NewEncoder(temporary)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(document); err != nil {
		return fmt.Errorf("写入存储设备登记册: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("同步存储设备登记册: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("关闭存储设备登记册: %w", err)
	}
	closed = true
	if err := os.Rename(temporaryName, r.path); err != nil {
		return fmt.Errorf("替换存储设备登记册: %w", err)
	}
	return nil
}
