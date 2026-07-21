package updater

import "context"

type commandRunner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

// SystemdStarter 启动安装阶段专用的固定 oneshot 单元。
type SystemdStarter struct {
	runner commandRunner
}

// NewSystemdStarter 使用现有受约束命令执行器创建 systemd 启动器。
func NewSystemdStarter(runner commandRunner) *SystemdStarter {
	return &SystemdStarter{runner: runner}
}

// StartUpdate 不接受来自清单或 HTTP 请求的命令参数，避免命令注入。
func (s *SystemdStarter) StartUpdate(ctx context.Context) error {
	_, err := s.runner.Run(ctx, "systemctl", "start", "--no-block", "teslabox-update.service")
	return err
}
