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

// CommandError 保留外部命令退出码，让调用方可以处理 fsck 等有语义的非零结果。
type CommandError struct {
	Name     string
	Code     int
	Output   string
	Internal error
}

// Error 返回适合日志记录的命令失败上下文。
func (e *CommandError) Error() string {
	if e.Output == "" {
		return fmt.Sprintf("执行 %s: %v", e.Name, e.Internal)
	}
	return fmt.Sprintf("执行 %s: %v: %s", e.Name, e.Internal, e.Output)
}

// Unwrap 保留 os/exec 原始错误链。
func (e *CommandError) Unwrap() error {
	return e.Internal
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
		code := -1
		if exitError, ok := err.(*exec.ExitError); ok {
			code = exitError.ExitCode()
		}
		return output, &CommandError{Name: name, Code: code, Output: message, Internal: err}
	}
	return output, nil
}
