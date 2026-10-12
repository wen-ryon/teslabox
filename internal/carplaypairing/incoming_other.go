//go:build !linux

package carplaypairing

import (
	"context"
	"log/slog"
)

// StartIncoming 在不支持系统蓝牙的开发平台上不启动硬件配对。
func (m *Manager) StartIncoming(ctx context.Context, logger *slog.Logger) {}
