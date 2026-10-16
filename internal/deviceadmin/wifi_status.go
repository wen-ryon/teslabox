package deviceadmin

import (
	"context"
	"strconv"
	"strings"
)

type wifiConnectionSnapshot struct {
	state string
	uuid  string
}

func (m *Manager) wifiConnectionSnapshot(ctx context.Context, interfaceName string) wifiConnectionSnapshot {
	// 在同一命令中读取状态与配置身份，避免分别查询时把两次激活的结果拼成“已连接”。
	output, err := m.run(ctx, "nmcli", "--escape", "no", "-g", "GENERAL.STATE,GENERAL.CON-UUID", "device", "show", interfaceName)
	if err != nil {
		return wifiConnectionSnapshot{state: "unknown"}
	}
	return parseWiFiConnectionSnapshot(string(output))
}

func parseWiFiConnectionSnapshot(output string) wifiConnectionSnapshot {
	result := wifiConnectionSnapshot{state: "unknown"}
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) < 2 {
		return result
	}
	fields := strings.Fields(lines[0])
	if len(fields) == 0 {
		return result
	}
	number, err := strconv.Atoi(fields[0])
	if err != nil {
		return result
	}
	uuid := strings.TrimSpace(lines[1])
	if validUUID(uuid) {
		result.uuid = uuid
	}
	switch {
	case number == 100 && result.uuid != "":
		result.state = "connected"
	case number >= 40 && number < 100:
		result.state = "connecting"
	case number == 110:
		result.state = "disconnecting"
	case number >= 0 && number <= 30 || number == 120:
		result.state = "disconnected"
		result.uuid = ""
	}
	return result
}
