package deviceadmin

import (
	"context"

	"github.com/wen-ryon/teslabox/internal/regulatory"
)

// WirelessRegion 单独返回配置和实际域，即使当前网卡不支持 AP 也可查看。
func (m *Manager) WirelessRegion(ctx context.Context) (regulatory.Status, error) {
	if m.regulatory == nil {
		return regulatory.Status{}, invalidInput("当前程序未启用无线地区管理")
	}
	return m.regulatory.Status(ctx), nil
}

// SetWirelessRegion 只接受用户确认的地区变更，不在 CarPlay 占用无线时切换全局监管域。
func (m *Manager) SetWirelessRegion(ctx context.Context, country string) (regulatory.Status, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.carPlayNetworkReservedLocked() {
		return regulatory.Status{}, carPlayNetworkBusy()
	}
	if m.regulatory == nil {
		return regulatory.Status{}, invalidInput("当前程序未启用无线地区管理")
	}
	if !regulatory.ValidCountry(country) {
		return regulatory.Status{}, invalidInput("国家/地区代码无效，请使用 CN、US 等大写两位国家码")
	}
	return m.regulatory.Set(ctx, country)
}
