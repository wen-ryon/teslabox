package deviceadmin

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"github.com/wen-ryon/teslabox/internal/carplaypairing"
)

type carPlayEnrollmentHandoff struct {
	Version     int    `json:"version"`
	Operation   string `json:"operation"`
	Phone       string `json:"phone"`
	UUID        string `json:"uuid"`
	ConfigHash  string `json:"config_hash"`
	AutoConnect bool   `json:"auto_connect"`
	Active      bool   `json:"active"`
	Committed   bool   `json:"committed"`
	Suspended   bool   `json:"suspended"`
	Restoring   bool   `json:"restoring"`
}

var handoffOperation = regexp.MustCompile(`^[a-f0-9]{32}$`)
var handoffDigest = regexp.MustCompile(`^[a-f0-9]{64}$`)

func handoffHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func (m *Manager) handoffPath() string { return m.carPlaySettingsPath + ".ap-handoff" }

func (m *Manager) handoffProfile(ctx context.Context, uuid string) (string, bool, bool, error) {
	if !validUUID(uuid) {
		return "", false, false, invalidInput("原热点身份无效")
	}
	// 密码只参与摘要计算，不写入交接记录、公开状态或日志。
	body, err := m.run(ctx, "nmcli", "--show-secrets", "-g", "connection.uuid,connection.interface-name,802-11-wireless,802-11-wireless-security,ipv4,ipv6", "connection", "show", "uuid", uuid)
	if err != nil {
		return "", false, false, errors.New("无法核对原热点配置")
	}
	auto, err := m.run(ctx, "nmcli", "-g", "connection.autoconnect", "connection", "show", "uuid", uuid)
	if err != nil || strings.TrimSpace(string(auto)) != "yes" && strings.TrimSpace(string(auto)) != "no" {
		return "", false, false, errors.New("无法核对原热点自动启动")
	}
	active, err := m.run(ctx, "nmcli", "-t", "-f", "UUID", "connection", "show", "--active")
	if err != nil {
		return "", false, false, err
	}
	on := false
	for _, value := range strings.Split(strings.TrimSpace(string(active)), "\n") {
		on = on || value == uuid
	}
	return handoffHash(string(body)), strings.TrimSpace(string(auto)) == "yes", on, nil
}

func handoffPlan(hash string, auto, active bool) carplaypairing.HandoffPlan {
	return carplaypairing.HandoffPlan{Fingerprint: handoffHash(hash + yesNo(auto) + yesNo(active)), Required: auto || active}
}

// PlanCarPlayHandoff 只绑定当前普通热点，不把扫描或页面查询解释成网络变更授权。
func (m *Manager) PlanCarPlayHandoff(ctx context.Context) (carplaypairing.HandoffPlan, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.carPlayRecoveryUncertain {
		return carplaypairing.HandoffPlan{}, carPlayNetworkBusy()
	}
	if m.carPlayNetworkHold != nil {
		return carplaypairing.HandoffPlan{}, nil
	}
	settings, _, err := m.carPlayNetworkSnapshot(ctx)
	if err != nil {
		return carplaypairing.HandoffPlan{}, err
	}
	hash, auto, active, err := m.handoffProfile(ctx, settings.UUID)
	return handoffPlan(hash, auto, active), err
}

func (m *Manager) saveHandoff(value *carPlayEnrollmentHandoff) error {
	path := m.handoffPath()
	if !filepath.IsAbs(m.carPlaySettingsPath) {
		return errors.New("交接记录必须位于本机绝对路径")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".carplay-handoff-")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err = file.Chmod(0600); err == nil {
		err = json.NewEncoder(file).Encode(value)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(file.Name(), path); err != nil {
		return err
	}
	if err = syncCarPlayDNSDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	m.carPlayEnrollmentHandoff = value
	return nil
}

func (m *Manager) loadHandoff() error {
	if m.carPlaySettingsPath == "" {
		return nil
	}
	info, err := os.Lstat(m.handoffPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 4096 || runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 || !carPlayDNSPermissionsSafe(info) {
		return errors.New("交接记录权限或大小无效")
	}
	file, err := os.Open(m.handoffPath())
	if err != nil {
		return err
	}
	defer file.Close()
	var value *carPlayEnrollmentHandoff
	decoder := json.NewDecoder(io.LimitReader(file, 4097))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return errors.New("交接记录无法解析")
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("交接记录含多余内容")
	}
	if value != nil && (value.Version != 1 || !handoffOperation.MatchString(value.Operation) || !carPlayBluetoothAddressPattern.MatchString(value.Phone) || !validUUID(value.UUID) || !handoffDigest.MatchString(value.ConfigHash)) {
		return errors.New("交接记录身份无效")
	}
	m.carPlayEnrollmentHandoff = value
	return nil
}

// BeginCarPlayHandoff 仅在蓝牙数字确认之后调用，先持久记录再暂停精确的原热点。
func (m *Manager) BeginCarPlayHandoff(ctx context.Context, operation, phone string, plan carplaypairing.HandoffPlan) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !handoffOperation.MatchString(operation) || !carPlayBluetoothAddressPattern.MatchString(phone) {
		return invalidInput("交接任务身份无效")
	}
	if m.carPlayRecoveryUncertain || m.carPlayWiFiReconnect {
		return carPlayNetworkBusy()
	}
	if m.carPlayNetworkHold != nil {
		if plan.Fingerprint != "" {
			return carPlayNetworkBusy()
		}
		return nil
	}
	if old := m.carPlayEnrollmentHandoff; old != nil {
		if old.Operation == operation {
			return m.suspendHandoffLocked(ctx)
		}
		if old.Committed && !old.Restoring {
			return nil
		}
		return carPlayNetworkBusy()
	}
	settings, _, err := m.carPlayNetworkSnapshot(ctx)
	if err != nil {
		return err
	}
	hash, auto, active, err := m.handoffProfile(ctx, settings.UUID)
	if err != nil {
		return err
	}
	if plan != handoffPlan(hash, auto, active) {
		return invalidInput("热点配置已变化，请重新确认")
	}
	value := &carPlayEnrollmentHandoff{Version: 1, Operation: operation, Phone: phone, UUID: settings.UUID, ConfigHash: hash, AutoConnect: auto, Active: active}
	if err = m.saveHandoff(value); err != nil {
		return err
	}
	m.carPlayEnrollmentLive = operation
	m.logger.Info("CarPlay 首次登记热点交接已准备", "operation_id", operation)
	return m.suspendHandoffLocked(ctx)
}

func (m *Manager) suspendHandoffLocked(ctx context.Context) error {
	value := m.carPlayEnrollmentHandoff
	if value == nil {
		return nil
	}
	hash, auto, active, err := m.handoffProfile(ctx, value.UUID)
	if err != nil {
		return err
	}
	if hash != value.ConfigHash || value.Suspended && (auto || active) {
		return invalidInput("原热点配置已被修改，交接已停止")
	}
	// 准备记录允许恢复修改自动启动后、尚未完成停热点时的断电窗口。
	if auto {
		if _, err = m.run(ctx, "nmcli", "connection", "modify", "uuid", value.UUID, "connection.autoconnect", "no"); err != nil {
			return err
		}
	}
	if active {
		if _, err = m.run(ctx, "nmcli", "--wait", "15", "connection", "down", "uuid", value.UUID); err != nil {
			return err
		}
	}
	next := *value
	next.Suspended = true
	if err = m.saveHandoff(&next); err != nil {
		return err
	}
	m.apUpdateGeneration++
	m.logger.Info("CarPlay 原热点已暂停", "operation_id", value.Operation)
	return nil
}

func (m *Manager) restoreHandoffLocked(ctx context.Context) error {
	value := m.carPlayEnrollmentHandoff
	if value == nil {
		return nil
	}
	if m.carPlayNetworkHold != nil {
		return carPlayNetworkBusy()
	}
	hash, auto, active, err := m.handoffProfile(ctx, value.UUID)
	if err != nil {
		return err
	}
	if hash != value.ConfigHash || value.Suspended && !value.Restoring && (auto || active) || value.Restoring && auto && !value.AutoConnect {
		return invalidInput("原热点配置已变化，拒绝覆盖用户修改")
	}
	if !value.Restoring {
		next := *value
		next.Restoring = true
		if err := m.saveHandoff(&next); err != nil {
			return err
		}
		value = &next
	}
	if auto != value.AutoConnect {
		if _, err = m.run(ctx, "nmcli", "connection", "modify", "uuid", value.UUID, "connection.autoconnect", yesNo(value.AutoConnect)); err != nil {
			return err
		}
	}
	if value.Active && !active {
		if _, err = m.run(ctx, "nmcli", "--wait", "15", "connection", "up", "uuid", value.UUID); err != nil {
			return err
		}
	}
	m.apUpdateGeneration++
	if err := m.saveHandoff(nil); err != nil {
		return err
	}
	m.carPlayEnrollmentLive = ""
	m.logger.Info("CarPlay 原热点已恢复", "operation_id", value.Operation)
	return nil
}

// FinishCarPlayHandoff 保留成功交接的原配置；失败必须先清理 CarPlay 自有租约。
func (m *Manager) FinishCarPlayHandoff(ctx context.Context, operation string, success bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	value := m.carPlayEnrollmentHandoff
	if value == nil {
		return nil
	}
	if value.Operation != operation {
		if value.Committed {
			return nil
		}
		return carPlayNetworkBusy()
	}
	if !success {
		return m.restoreHandoffLocked(ctx)
	}
	next := *value
	next.Committed = true
	if err := m.saveHandoff(&next); err != nil {
		return err
	}
	m.logger.Info("CarPlay 首次登记热点交接已提交", "operation_id", operation)
	return nil
}

// RecoverCarPlayHandoff 在恢复租约所有权后处理断电遗留；绝不抢占未清理的自有热点。
func (m *Manager) RecoverCarPlayHandoff(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.carPlayEnrollmentHandoff == nil || m.carPlayEnrollmentHandoff.Committed || m.carPlayNetworkHold != nil {
		return nil
	}
	return m.restoreHandoffLocked(ctx)
}
