package deviceadmin

import (
	"context"
	"path"
	"strings"
	"time"
)

// APSecurityOption 只描述系统报告的配置前提，不能代替客户端实际连接验收。
type APSecurityOption struct {
	Mode      string `json:"mode"`
	Label     string `json:"label"`
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

const apSecurityProfileFields = "802-11-wireless-security.key-mgmt,802-11-wireless-security.pmf," +
	"802-11-wireless-security.proto,802-11-wireless-security.pairwise,802-11-wireless-security.group"

func apSecurityArguments(mode string) ([]string, error) {
	if mode == "" {
		// 旧客户端及仅调整信道的自动化不应改变既有认证方式。
		return []string{}, nil
	}
	keyMgmt, pmf := "wpa-psk", "1"
	switch mode {
	case "wpa2":
		// 显式禁用 PMF 同时避免新版 NetworkManager 自动追加 SAE，确保 WPA2-only 语义。
	case "wpa3":
		keyMgmt, pmf = "sae", "3"
	case "wpa2-wpa3":
		return nil, &APOperationError{
			Code: "ap_security_unavailable", Message: "当前管理后端尚不能可靠配置 WPA2/WPA3 混合热点",
			Suggestion: "请选择 WPA2-Personal 或系统能力允许的 WPA3-Personal；不会自动降级。",
		}
	default:
		return nil, invalidInput("热点安全认证方式无效")
	}
	return []string{
		"802-11-wireless-security.key-mgmt", keyMgmt,
		"802-11-wireless-security.pmf", pmf,
		"802-11-wireless-security.proto", "rsn",
		"802-11-wireless-security.pairwise", "ccmp",
		"802-11-wireless-security.group", "ccmp",
		"802-11-wireless-security.auth-alg", "open",
		"802-11-wireless-security.wps-method", "1",
	}, nil
}

func parseAPSecurityMode(output string) string {
	lines := strings.Split(strings.TrimSuffix(output, "\n"), "\n")
	if len(lines) != 5 {
		return ""
	}
	for i := range lines {
		lines[i] = strings.ToLower(strings.TrimSpace(lines[i]))
	}
	// 不把默认值或宽松的 WPA-PSK 配置猜成严格 WPA2，更不能猜成混合模式。
	if lines[2] != "rsn" || lines[3] != "ccmp" || lines[4] != "ccmp" {
		return ""
	}
	pmf := strings.Fields(lines[1])
	if len(pmf) == 0 {
		return ""
	}
	if lines[0] == "sae" && pmf[0] == "3" {
		return "wpa3"
	}
	if lines[0] == "wpa-psk" && pmf[0] == "1" {
		return "wpa2"
	}
	return ""
}

func (m *Manager) apSecurityOptions(ctx context.Context, interfaceName string) []APSecurityOption {
	options := []APSecurityOption{
		{Mode: "wpa2", Label: "WPA2-Personal（AES，推荐）", Available: true},
		{Mode: "wpa2-wpa3", Label: "WPA2/WPA3 混合", Reason: "当前管理后端尚不能可靠配置混合认证，不能仅凭 WPA-PSK 属性认定支持。"},
		{Mode: "wpa3", Label: "WPA3-Personal（SAE）", Reason: "无法确认当前 AP 接口的 SAE 与管理帧保护能力；不代表网卡硬件一定不支持。"},
	}
	if interfaceName == "" {
		return options
	}
	// 只查询目标 AP 接口，不借用上游 STA 的 WPA3 能力；总预算避免增强查询拖慢整个设置页。
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	compatibilityAllowed := false
	for _, check := range []struct {
		field string
		value string
		label string
	}{
		{field: "modes", value: "AP", label: "AP 模式"},
		{field: "key_mgmt", value: "SAE", label: "SAE 认证"},
		{field: "pairwise", value: "CCMP", label: "CCMP 加密"},
		{field: "group_mgmt", value: "AES-128-CMAC", label: "AES-128-CMAC 管理帧保护"},
	} {
		output, err := m.run(probeCtx, "wpa_cli", "-i", interfaceName, "get_capability", check.field, "strict")
		if err != nil || !hasAPSecurityToken(string(output), check.value) {
			// 用户已确认 RTL8852BE 的 WPA3 可用；只豁免其驱动漏报 SAE，不能豁免其他前提或启动验收。
			if err == nil && check.field == "key_mgmt" && m.apSAECompatibilityAllowed(probeCtx, interfaceName) {
				compatibilityAllowed = true
				continue
			}
			options[2].Reason = "当前 AP 控制接口未明确报告“" + check.label + "”能力，暂不开放 WPA3；可能是控制接口不可读或系统能力不足，不等于网卡硬件不支持。"
			return options
		}
	}
	options[2].Available = true
	options[2].Reason = "接口报告支持 AP、SAE、CCMP 和管理帧保护；仍需启动及真实终端连接验证，不会自动降级。"
	if compatibilityAllowed {
		options[2].Reason = "RTL8852BE 白名单兼容放行：接口未报告 SAE，但 AP、CCMP 和管理帧保护前提已满足；仍需实际热点启动和终端连接验证，不会自动降级。"
	}
	return options
}

func (m *Manager) apSAECompatibilityAllowed(ctx context.Context, interfaceName string) bool {
	// 虚拟 AP 接口通过所属 PHY 找到实际驱动，不能按接口名或上游连接的加密类型放行。
	driverPath := path.Join("/sys/class/net", interfaceName, "phy80211/device/driver")
	output, err := m.run(ctx, "readlink", "-f", driverPath)
	if err != nil {
		return false
	}
	switch path.Base(strings.TrimSpace(string(output))) {
	case "rtl8852be", "8852be", "rtw89_8852be":
		return true
	default:
		return false
	}
}

func hasAPSecurityToken(output, token string) bool {
	for _, value := range strings.Fields(output) {
		if value == token {
			return true
		}
	}
	return false
}

func (m *Manager) readAPSecurity(ctx context.Context, settings *APSettings) {
	output, err := m.run(ctx, "nmcli", "--escape", "no", "-g", apSecurityProfileFields,
		"connection", "show", "uuid", settings.UUID)
	if err == nil {
		settings.SecurityMode = parseAPSecurityMode(string(output))
	}
	settings.SecurityOptions = m.apSecurityOptions(ctx, settings.Interface)
}

func (m *Manager) activeAPSecurity(ctx context.Context, interfaceName string) string {
	probeCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	output, err := m.run(probeCtx, "wpa_cli", "-i", interfaceName, "status")
	if err != nil {
		return ""
	}
	return parseActiveAPSecurity(string(output))
}

func parseActiveAPSecurity(output string) string {
	values := make(map[string]string)
	for _, line := range strings.Split(output, "\n") {
		if key, value, ok := strings.Cut(strings.TrimSpace(line), "="); ok {
			values[key] = value
		}
	}
	// status 在普通客户端上表示协商结果，不能误报成热点正在使用的认证方式。
	if values["mode"] != "AP" || values["wpa_state"] != "COMPLETED" {
		return ""
	}
	switch values["key_mgmt"] {
	case "SAE":
		return "wpa3"
	case "WPA2-PSK", "WPA2-PSK-SHA256":
		return "wpa2"
	default:
		return ""
	}
}
