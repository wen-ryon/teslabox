package deviceadmin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const carPlayServiceUnit = "teslabox-carplay-experiment.service"

func (m *Manager) connectWithCarPlayRestart(ctx context.Context, request ConnectRequest) (operationErr error) {
	m.mu.Lock()
	if err := m.carPlayBluetoothWiFiGuardLocked(); err != nil {
		m.mu.Unlock()
		return err
	}
	if m.carPlayRecoveryUncertain || m.carPlayWiFiReconnect {
		m.mu.Unlock()
		return carPlayNetworkBusy()
	}
	if !m.carPlayBlocksWiFiLocked() {
		m.mu.Unlock()
		request.ConfirmCarPlayReconnect = false
		return m.Connect(ctx, request)
	}
	// 先校验目标与密码，错误输入不能中断正在播放的会话。
	status, err := m.WiFiStatus(ctx)
	if err == nil {
		err = validateCarPlayWiFiConnect(status, request)
	}
	if err != nil {
		m.mu.Unlock()
		return err
	}
	m.carPlayWiFiReconnect = true
	m.mu.Unlock()
	active, err := m.run(ctx, "systemctl", "show", carPlayServiceUnit, "-p", "ActiveState", "--value")
	if err != nil || strings.TrimSpace(string(active)) != "active" {
		m.mu.Lock()
		m.carPlayWiFiReconnect = false
		m.mu.Unlock()
		return &APOperationError{Code: "carplay_restart_unavailable", Message: "CarPlay 服务状态异常，无法安全释放无线网络",
			Suggestion: "请下载 CarPlay 日志诊断；本次没有更改 Wi-Fi。"}
	}
	// 已确认的操作在 AP 短暂断开、HTTP 客户端离线后仍须完成恢复。
	operationCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 90*time.Second)
	defer cancel()
	defer func() {
		m.mu.Lock()
		m.carPlayWiFiReconnect = false
		m.mu.Unlock()
		restoreCtx, restoreCancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer restoreCancel()
		// 异步启动避免等待新会话调用主程序租约接口造成循环等待。
		if _, err := m.run(restoreCtx, "systemctl", "--no-block", "start", carPlayServiceUnit); err != nil {
			operationErr = errors.Join(operationErr, fmt.Errorf("恢复 CarPlay 服务失败: %w", err))
		}
	}()
	m.logger.Info("手动 Wi-Fi 连接已确认中断 CarPlay")
	// 停止过程会调用租约释放接口，此时不能持有 Manager.mu。
	if _, err := m.run(operationCtx, "systemctl", "stop", carPlayServiceUnit); err != nil {
		return fmt.Errorf("暂时停止 CarPlay 失败: %w", err)
	}
	m.mu.Lock()
	released := m.carPlayNetworkHold == nil && !m.carPlayRecoveryUncertain
	m.mu.Unlock()
	if !released {
		return &APOperationError{Code: "carplay_release_incomplete", Message: "CarPlay 无线网络尚未安全释放",
			Suggestion: "已尝试恢复 CarPlay，本次未强行覆盖网络租约。"}
	}
	request.ConfirmCarPlayReconnect = false
	request.ConfirmAPDisconnect = true
	return m.connectWiFi(operationCtx, request, true)
}

func validateCarPlayWiFiConnect(status WiFiStatus, request ConnectRequest) error {
	if !status.Available || status.Interface == "" {
		return ErrWiFiUnavailable
	}
	if request.UUID != "" {
		if !validUUID(request.UUID) {
			return invalidInput("连接 UUID 无效")
		}
		network, found := findSavedNetwork(status.SavedNetworks, request.UUID)
		if !found {
			return invalidInput("未找到可连接的已保存 WiFi")
		}
		if request.Password == "" && network.RequiresPassword {
			return invalidInput(errWiFiPasswordRequired.Error())
		}
		if request.Password != "" && !profileUsesPSK(network.Security) {
			return ErrUnsupportedSecurity
		}
	} else {
		if err := validateSSID(request.SSID); err != nil {
			return err
		}
		security := normalizeSecurity(request.Security)
		if security != "open" && security != "wpa" {
			return ErrUnsupportedSecurity
		}
		if security != "open" && request.Password == "" {
			return invalidInput(errWiFiPasswordRequired.Error())
		}
	}
	if request.BSSID != "" && !validBSSID(request.BSSID) {
		return invalidInput("WiFi BSSID 无效")
	}
	if request.Channel < 0 || request.Channel != 0 && !validCarPlayChannel(request.Channel) {
		return invalidInput("WiFi 信道无效")
	}
	if request.Password != "" {
		return validatePassword(request.Password)
	}
	return nil
}
