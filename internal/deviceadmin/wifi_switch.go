package deviceadmin

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type wifiSwitchState struct {
	ap         APSettings
	stopped    []profile
	stopAP     bool
	apChanged  bool
	connecting bool
	coupled    bool
}

func (m *Manager) resolveWiFiSwitchAccessPoint(
	ctx context.Context, interfaceName string, request ConnectRequest, ssid string,
) (AccessPoint, error) {
	if err := validateSSID(ssid); err != nil {
		return AccessPoint{}, err
	}
	if request.BSSID != "" && !validBSSID(request.BSSID) {
		return AccessPoint{}, invalidInput("WiFi BSSID 无效")
	}
	interfaces := []string{interfaceName}
	output, err := m.run(ctx, "nmcli", "-t", "--escape", "yes", "-f", "DEVICE,TYPE,STATE", "device", "status")
	if err != nil {
		return AccessPoint{}, err
	}
	for _, device := range parseWiFiDevices(string(output)) {
		if device.Name == interfaceName {
			continue
		}
		shared, known := m.sameWirelessPHY(interfaceName, device.Name)
		if shared && known {
			interfaces = append(interfaces, device.Name)
		}
	}
	find := func(rescan bool) (AccessPoint, bool, error) {
		var failures []error
		for _, device := range interfaces {
			if rescan && device != interfaceName {
				continue
			}
			args := []string{
				"-t", "--escape", "yes", "-f", "IN-USE,SSID,BSSID,SIGNAL,CHAN,SECURITY",
				"device", "wifi", "list", "ifname", device, "--rescan", "no",
			}
			if rescan {
				args[len(args)-1] = "yes"
				args = append([]string{"--wait", "12"}, args...)
			}
			output, err := m.run(ctx, "nmcli", args...)
			if err != nil {
				failures = append(failures, fmt.Errorf("读取 %s 扫描结果: %w", device, err))
				continue
			}
			if target, found := matchWiFiSwitchAccessPoint(string(output), request, ssid); found {
				return target, true, nil
			}
		}
		return AccessPoint{}, false, errors.Join(failures...)
	}
	if target, found, _ := find(false); found {
		return target, nil
	}
	// rescan 只确认请求受理；list --rescan yes 会等待扫描完成，不能立即读取旧缓存。
	// 只在所选 STA 强制扫描一次，同 PHY 的其他接口仅用于读取已有缓存。
	if target, found, err := find(true); found || err != nil {
		return target, err
	}
	return AccessPoint{}, invalidInput("刷新扫描后仍未找到目标 WiFi，请确认热点已开启")
}

func matchWiFiSwitchAccessPoint(output string, request ConnectRequest, ssid string) (AccessPoint, bool) {
	lines := []string{}
	for _, line := range strings.Split(output, "\n") {
		fields := splitNMCLI(line)
		if len(fields) != 6 || fields[1] != ssid {
			continue
		}
		bssidMatches := request.BSSID == "" || strings.EqualFold(fields[2], request.BSSID)
		channel, _ := strconv.Atoi(fields[4])
		channelMatches := request.Channel == 0 || channel == request.Channel
		if bssidMatches && channelMatches {
			lines = append(lines, line)
		}
	}
	// 先按用户选中的 BSSID/信道过滤，再同名去重，不能让更强的同名接入点覆盖指定目标。
	items := parseAccessPoints(strings.Join(lines, "\n"), map[string]bool{})
	if len(items) == 0 {
		return AccessPoint{}, false
	}
	return items[0], true
}

func (m *Manager) activateNewWiFi(ctx context.Context, interfaceName, ssid string, args []string) error {
	_, err := m.run(ctx, "nmcli", args...)
	if !isNMCLIWaitTimeout(err) {
		return err
	}
	current, queryErr := m.run(ctx, "nmcli", "-g", "GENERAL.CON-UUID", "device", "show", interfaceName)
	if queryErr != nil {
		return errors.Join(err, queryErr)
	}
	uuid := strings.TrimSpace(string(current))
	if !validUUID(uuid) {
		return err
	}
	activeSSID, queryErr := m.run(ctx, "nmcli", "-g", "802-11-wireless.ssid", "connection", "show", "uuid", uuid)
	if queryErr != nil || strings.TrimSuffix(string(activeSSID), "\n") != ssid {
		return errors.Join(err, queryErr)
	}
	// 新建配置同样只等待原激活；先核对实际SSID，不能把另一连接拿到IP当作本次成功。
	waitCtx, cancel := context.WithTimeout(ctx, recoveryTimeout)
	defer cancel()
	return m.waitWiFiUUIDActivation(waitCtx, interfaceName, uuid)
}

func (m *Manager) guardReservedAPAutoConnect(ctx context.Context) error {
	_, err := m.guardReservedAPAutoConnectState(ctx)
	return err
}

func (m *Manager) guardReservedAPAutoConnectState(ctx context.Context) (map[string]bool, error) {
	stopped := map[string]bool{}
	profiles, err := m.wirelessProfiles(ctx)
	if err != nil {
		return stopped, err
	}
	station, ap, available, apAvailable := m.networkInterfaces(ctx, profiles)
	if !available || !apAvailable || station == ap {
		return stopped, nil
	}
	shared, known := m.sameWirelessPHY(station, ap)
	if !shared || !known {
		return stopped, nil
	}
	output, err := m.run(ctx, "nmcli", "-g", "GENERAL.AUTOCONNECT", "device", "show", ap)
	if err != nil {
		return stopped, err
	}
	if strings.TrimSpace(string(output)) == "yes" {
		// 只关闭预留接口的运行时自动连接，不修改任何用户连接配置或密码。
		if _, err := m.run(ctx, "nmcli", "device", "set", ap, "autoconnect", "no"); err != nil {
			return stopped, fmt.Errorf("保护预留 AP 接口: %w", err)
		}
	}
	stationActive := false
	for _, item := range profiles {
		if item.Active && item.Mode != "ap" && item.Device == station {
			stationActive = true
		}
	}
	if !stationActive {
		return stopped, nil
	}
	for _, item := range profiles {
		if item.Active && item.Mode != "ap" && item.Device == ap {
			// 同一射频已有正常上游时，另一 STA 的自动重试只会拖垮关联和 DHCP。
			if _, err := m.run(ctx, "nmcli", "device", "disconnect", ap); err != nil && !isNMDeviceInactive(err) {
				return stopped, fmt.Errorf("停止预留 AP 接口上的重复上游: %w", err)
			}
			stopped[item.UUID] = true
		}
	}
	return stopped, nil
}

func (m *Manager) prepareWiFiSwitch(ctx context.Context, status WiFiStatus, channel int) (*wifiSwitchState, error) {
	profiles, err := m.wirelessProfiles(ctx)
	if err != nil {
		return nil, err
	}
	ap, err := m.apSettingsFromProfiles(ctx, profiles)
	if err != nil {
		return nil, err
	}
	state := &wifiSwitchState{ap: ap, stopped: []profile{}, coupled: apRequiresSameChannel(ap.ConcurrencyMode)}
	if ap.Available && ap.DesiredEnabled && state.coupled {
		if err := m.apChannelPreflight(ctx, ap.Interface, channel); err != nil {
			return nil, err
		}
	}
	roleCorrections, err := m.guardReservedAPAutoConnectState(ctx)
	if err != nil {
		return nil, err
	}
	selected := []profile{}
	for _, item := range profiles {
		// 角色保护已纠正的重复STA不能进入业务回滚，否则失败会重新制造跨信道冲突。
		if roleCorrections[item.UUID] {
			continue
		}
		if !item.Active || item.Device == "" {
			continue
		}
		shared, known := m.sameWirelessPHY(status.Interface, item.Device)
		if item.Device == status.Interface {
			shared, known = true, true
		}
		if !known {
			return nil, invalidInput("无法确认活动无线接口的射频归属，本次未切换网络")
		}
		if !shared {
			continue
		}
		if item.Mode == "ap" {
			if state.coupled {
				selected = append(selected, item)
			}
			continue
		}
		// 即使旧上游占用了预留 AP 接口，也必须停掉，不能只断开新选中的空闲接口。
		selected = append(selected, item)
	}
	for _, item := range selected {
		if item.Mode == "ap" {
			state.stopAP = true
		} else {
			state.stopped = append(state.stopped, item)
		}
		if _, err := m.run(ctx, "nmcli", "device", "disconnect", item.Device); err != nil && !isNMDeviceInactive(err) {
			return state, fmt.Errorf("切换前停止无线接口 %s: %w", item.Device, err)
		}
	}
	return state, nil
}

func (s *wifiSwitchState) restoreAPAfterConnect(ctx context.Context, m *Manager, status WiFiStatus) error {
	if !s.ap.Available || !s.ap.DesiredEnabled || !s.coupled {
		return nil
	}
	_, channel := m.activeWirelessChannel(ctx, status.Interface)
	if channel <= 0 {
		return errors.New("连接后无法确认上游实际信道，未恢复热点")
	}
	// 以关联后的实际信道为准，扫描结果可能已因路由器漫游而过期。
	band, ok := concurrentBand(channel)
	if !ok {
		return invalidInput("上游 WiFi 实际信道不支持 AP+STA 并发")
	}
	if err := m.apChannelPreflight(ctx, s.ap.Interface, channel); err != nil {
		return err
	}
	s.apChanged = true
	if _, err := m.run(
		ctx, "nmcli", "connection", "modify", "uuid", s.ap.UUID,
		"802-11-wireless.band", band,
		"802-11-wireless.channel", strconv.Itoa(channel),
	); err != nil {
		return fmt.Errorf("同步热点实际信道: %w", err)
	}
	if _, err := m.run(ctx, "nmcli", "--wait", "20", "connection", "up", "uuid", s.ap.UUID, "ifname", s.ap.Interface); err != nil {
		return fmt.Errorf("在上游连接后恢复热点: %w", err)
	}
	runtime := m.activeAPRuntime(ctx, s.ap.Interface)
	if runtime.Type != "AP" || runtime.SSID != s.ap.SSID || runtime.Channel != channel {
		return &APOperationError{
			Code: "ap_not_broadcasting", Message: "上游已连接，但热点没有在相同信道正常广播",
			Suggestion: "本次切换将恢复原网络，请检查无线驱动日志。",
		}
	}
	return nil
}

func (s *wifiSwitchState) rollback(m *Manager, targetInterface string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	failures := []error{}
	disconnect := func(device string) {
		if _, err := m.run(ctx, "nmcli", "device", "disconnect", device); err != nil && !isNMDeviceInactive(err) {
			failures = append(failures, fmt.Errorf("回滚时停止无线接口 %s: %w", device, err))
		}
	}
	if s.connecting {
		disconnect(targetInterface)
	}
	if s.apChanged {
		disconnect(s.ap.Interface)
		if _, err := m.run(
			ctx, "nmcli", "connection", "modify", "uuid", s.ap.UUID,
			"802-11-wireless.band", s.ap.Band,
			"802-11-wireless.channel", strconv.Itoa(s.ap.Channel),
			"connection.autoconnect", yesNo(s.ap.AutoConnect),
		); err != nil {
			failures = append(failures, fmt.Errorf("回滚热点配置: %w", err))
		}
	}
	for _, item := range s.stopped {
		if _, err := m.run(ctx, "nmcli", "--wait", "15", "connection", "up", "uuid", item.UUID, "ifname", item.Device); err != nil {
			failures = append(failures, fmt.Errorf("恢复原上游 %s: %w", item.SSID, err))
		}
	}
	if s.stopAP {
		if _, err := m.run(ctx, "nmcli", "--wait", "20", "connection", "up", "uuid", s.ap.UUID, "ifname", s.ap.Interface); err != nil {
			failures = append(failures, fmt.Errorf("恢复原热点: %w", err))
		}
	}
	return errors.Join(failures...)
}
