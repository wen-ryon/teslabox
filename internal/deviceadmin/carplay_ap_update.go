package deviceadmin

import "context"

func carPlayAPBusy() error {
	return &APOperationError{Code: "carplay_network_reserved", Message: "CarPlay 正在使用无线网络",
		Suggestion: "请确认暂时中断 CarPlay 后应用热点设置，或取消并保持当前会话。"}
}

func validateAPUpdate(update APUpdate) error {
	if err := validateSSID(update.SSID); err != nil {
		return err
	}
	if update.Password != "" {
		if err := validatePassword(update.Password); err != nil {
			return err
		}
	}
	if !validBandChannel(update.Band, update.Channel) {
		return invalidInput("热点频段或信道无效")
	}
	_, err := apSecurityArguments(update.SecurityMode)
	return err
}

func (m *Manager) updateAPWithCarPlayRestart(ctx context.Context, update APUpdate) error {
	m.mu.Lock()
	if m.carPlayRecoveryUncertain || m.carPlayWiFiReconnect {
		m.mu.Unlock()
		return &APOperationError{Code: "carplay_change_in_progress", Message: "CarPlay 网络正在变更或恢复状态尚未确认",
			Suggestion: "请等待当前操作结束，或下载诊断报告；不会强行覆盖网络租约。"}
	}
	if m.carPlayNetworkHold == nil {
		m.mu.Unlock()
		return m.updateAP(ctx, update, "")
	}
	err := validateAPUpdate(update)
	var settings APSettings
	if err == nil {
		settings, err = m.APSettings(ctx)
	}
	if err == nil && (!settings.Available || !validUUID(settings.UUID) || settings.UUID != m.carPlayNetworkHold.Plan.APUUID) {
		err = invalidInput("无法确认 CarPlay 使用的热点模板，请刷新后重试")
	}
	if err == nil && (update.SecurityMode == "wpa3" || update.SecurityMode == "" && settings.Security != "wpa-psk") {
		err = &APOperationError{Code: "carplay_ap_security_conflict", Message: "当前 CarPlay 实现需要 WPA2 热点模板，无法应用此认证并自动恢复",
			Suggestion: "请选择 WPA2，或先在 CarPlay 设置中关闭功能，再单独修改普通热点；不会自动降级。"}
	}
	if err == nil {
		for _, char := range update.Password {
			if char > 127 {
				err = invalidInput("CarPlay 热点密码必须是 8 到 63 个 ASCII 字符")
				break
			}
		}
	}
	if err != nil {
		m.mu.Unlock()
		return err
	}
	// 共用 WiFi 变更标记，阻止新租约、普通 AP 保存和自动协调在事务中抢占接口。
	m.carPlayWiFiReconnect = true
	m.mu.Unlock()
	return m.runCarPlayNetworkChange(ctx, "热点设置已确认中断 CarPlay", func(operationCtx context.Context) error {
		return m.updateAP(operationCtx, update, settings.UUID)
	})
}
