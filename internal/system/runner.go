package system

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// Runner 隔离外部命令调用，便于测试 Linux 挂载流程。
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// OSRunner 在真实系统中执行受上下文约束的命令。
type OSRunner struct{}

// NewOSRunner 创建无隐藏状态的命令执行器。
func NewOSRunner() *OSRunner {
	return &OSRunner{}
}

// Run 执行命令并在失败时保留必要的诊断输出。
func (r *OSRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	output, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		message := strings.TrimSpace(string(output))
		if message == "" {
			return nil, fmt.Errorf("执行 %s: %w", name, err)
		}
		return nil, fmt.Errorf("执行 %s: %w: %s", name, err, message)
	}
	return output, nil
}
