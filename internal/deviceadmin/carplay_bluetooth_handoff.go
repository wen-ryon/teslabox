package deviceadmin

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// promoteCarPlayBluetoothLocked 仅在指定 PAN 接口通过联网探测后调用，保留原租约与蓝牙连接。
func (m *Manager) promoteCarPlayBluetoothLocked(ctx context.Context, lease *CarPlayNetworkLease, checkpoint func(*CarPlayNetworkLease) error) (result error) {
	original := *lease
	profiles, err := m.wirelessProfiles(ctx)
	if err != nil {
		return err
	}
	filtered := make([]profile, 0, len(profiles))
	status := WiFiStatus{Interface: original.Plan.UpstreamInterface}
	for _, item := range profiles {
		if item.Mode == "ap" && item.UUID != original.Plan.APUUID {
			continue
		}
		filtered = append(filtered, item)
		if item.Mode != "ap" && item.Active {
			status.CurrentConnection, status.currentConnectionUUID = item.SSID, item.UUID
		}
	}
	settings, err := m.apSettingsFromProfiles(ctx, filtered)
	if err != nil {
		return err
	}
	if status.currentConnectionUUID != "" && status.currentConnectionUUID != original.Plan.UpstreamUUID {
		return carPlayNetworkBusy()
	}
	// 媒体已在兼容的五 GHz 信道上运行时保留它，默认信道不应触发第二次无线交接。
	if original.Plan.Channel >= 36 {
		settings.Channel = original.Plan.Channel
	}
	target, err := planCarPlayBluetoothTarget(settings, status, m.carPlayVehicleChannels)
	if err != nil {
		return err
	}
	if target.Channel < 36 {
		return invalidInput("当前网卡与车机没有共同支持的 5 GHz 热点信道，保留现有网络")
	}
	// 使用同一份恢复身份，即使上游暂时消失也不能丢失原连接记录。
	target.UpstreamUUID = original.Plan.UpstreamUUID
	target.PauseUpstream = target.UpstreamUUID != ""
	if target.APUUID != original.Plan.APUUID || target.APInterface != original.Plan.APInterface || target.UpstreamInterface != original.Plan.UpstreamInterface {
		return carPlayNetworkBusy()
	}
	next := original
	next.Plan = target
	if !next.DeviceAutoConnectManaged {
		value, readErr := m.run(ctx, "nmcli", "-g", "GENERAL.AUTOCONNECT", "device", "show", target.UpstreamInterface)
		if readErr != nil {
			return readErr
		}
		policy := strings.TrimSpace(string(value))
		if policy != "yes" && policy != "no" {
			return invalidInput("无法确认无线自动连接策略，保留原网络")
		}
		next.DeviceAutoConnectManaged = true
		next.OriginalDeviceAutoConnect = policy == "yes"
	}
	// 修改前校验临时热点身份；不能覆盖外部替换的连接配置。
	identity, err := m.run(ctx, "nmcli", "-g", "connection.id,connection.interface-name,802-11-wireless.mode", "connection", "show", "uuid", next.RuntimeAPUUID)
	if err != nil || strings.TrimSpace(string(identity)) != next.RuntimeAPName+"\n"+target.APInterface+"\nap" {
		return invalidInput("CarPlay 热点身份改变，取消蓝牙供网切换")
	}
	if err = checkpoint(&next); err != nil {
		return err
	}
	*lease = next
	m.carPlayNetworkHold = &next
	defer func() {
		if result == nil {
			return
		}
		recovery, cancel := context.WithTimeout(context.WithoutCancel(ctx), 12*time.Second)
		defer cancel()
		// 回滚也保留 PAN；恢复旧信道后才能重新激活同射频的原上游。
		_, downErr := m.run(recovery, "nmcli", "--wait", "10", "connection", "down", "uuid", next.RuntimeAPUUID)
		if isNMDeviceInactive(downErr) {
			downErr = nil
		}
		channelErr := m.setCarPlayAPChannelLocked(recovery, original)
		var wifiErr error
		if original.Plan.UpstreamUUID != "" && !original.Plan.PauseUpstream {
			wifiErr = m.activateSavedWiFi(recovery, original.Plan.UpstreamUUID, original.Plan.UpstreamInterface,
				[]string{"--wait", "15", "connection", "up", "uuid", original.Plan.UpstreamUUID, "ifname", original.Plan.UpstreamInterface})
		}
		m.carPlayNetworkHold = &original
		_, apErr := m.startCarPlayAPLocked(recovery, original.Owner)
		var policyErr error
		if !original.DeviceAutoConnectManaged {
			policy := "no"
			if next.OriginalDeviceAutoConnect {
				policy = "yes"
			}
			_, policyErr = m.run(recovery, "nmcli", "device", "set", target.UpstreamInterface, "autoconnect", policy)
		}
		restoreErr := errors.Join(downErr, channelErr, wifiErr, apErr, policyErr)
		if restoreErr == nil {
			restoreErr = checkpoint(&original)
		}
		if restoreErr != nil {
			// 保留完整的恢复记录，不把部分回滚声明为成功。
			m.carPlayNetworkHold = &next
			result = errors.Join(result, errors.New("蓝牙供网切换失败，原网络尚未完全恢复"), restoreErr)
			return
		}
		*lease = original
	}()
	if _, err = m.run(ctx, "nmcli", "device", "set", target.UpstreamInterface, "autoconnect", "no"); err != nil {
		return err
	}
	if status.currentConnectionUUID != "" {
		if _, err = m.run(ctx, "nmcli", "--wait", "15", "connection", "down", "uuid", status.currentConnectionUUID); err != nil && !isNMDeviceInactive(err) {
			return err
		}
	}
	if next.Plan.Channel != original.Plan.Channel {
		if err = m.setCarPlayAPChannelLocked(ctx, next); err != nil {
			return err
		}
	}
	if _, err = m.startCarPlayAPLocked(ctx, next.Owner); err != nil {
		return err
	}
	return nil
}

func (m *Manager) setCarPlayAPChannelLocked(ctx context.Context, lease CarPlayNetworkLease) error {
	band := "bg"
	if lease.Plan.Channel >= 36 {
		band = "a"
	}
	_, err := m.run(ctx, "nmcli", "connection", "modify", "uuid", lease.RuntimeAPUUID,
		"802-11-wireless.band", band, "802-11-wireless.channel", fmt.Sprint(lease.Plan.Channel))
	return err
}
