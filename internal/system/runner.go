package system

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
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
type OSRunner struct {
	mediaOnce sync.Once
	mediaGate chan struct{}
}

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

// RunStreaming 避免在内存中缓存视频正文，并限制 FFmpeg 诊断信息占用。
func (r *OSRunner) RunStreaming(ctx context.Context, name string, args []string, output io.Writer) error {
	if err := r.acquireMedia(ctx); err != nil {
		return err
	}
	defer r.releaseMedia()
	command := exec.CommandContext(ctx, name, args...)
	command.Stdout = output
	stderr := &limitedBuffer{limit: 64 * 1024}
	command.Stderr = stderr
	if err := command.Run(); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		code := -1
		if exitError, ok := err.(*exec.ExitError); ok {
			code = exitError.ExitCode()
		}
		return &CommandError{Name: name, Code: code, Output: strings.TrimSpace(stderr.String()), Internal: err}
	}
	return nil
}

// RunProgress 单独消费 FFmpeg 进度管道，避免导出期间积压输出并允许实时反馈进度。
func (r *OSRunner) RunProgress(ctx context.Context, name string, args []string, progress func(time.Duration)) error {
	if err := r.acquireMedia(ctx); err != nil {
		return err
	}
	defer r.releaseMedia()
	command := exec.CommandContext(ctx, name, args...)
	stdout, err := command.StdoutPipe()
	if err != nil {
		return fmt.Errorf("创建 %s 进度管道: %w", name, err)
	}
	stderr := &limitedBuffer{limit: 64 * 1024}
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		return commandFailure(ctx, name, stderr, err)
	}

	scanner := bufio.NewScanner(stdout)
	for scanner.Scan() {
		position, ok := parseFFmpegProgress(scanner.Text())
		if ok && progress != nil {
			progress(position)
		}
	}
	if scanErr := scanner.Err(); scanErr != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return fmt.Errorf("读取 %s 进度: %w", name, scanErr)
	}
	if err := command.Wait(); err != nil {
		return commandFailure(ctx, name, stderr, err)
	}
	return nil
}

func (r *OSRunner) acquireMedia(ctx context.Context) error {
	r.mediaOnce.Do(func() {
		r.mediaGate = make(chan struct{}, 1)
		r.mediaGate <- struct{}{}
	})
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-r.mediaGate:
		return nil
	}
}

func (r *OSRunner) releaseMedia() {
	r.mediaGate <- struct{}{}
}

func parseFFmpegProgress(line string) (time.Duration, bool) {
	value, found := strings.CutPrefix(strings.TrimSpace(line), "out_time_us=")
	if !found {
		return 0, false
	}
	microseconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil || microseconds < 0 {
		return 0, false
	}
	return time.Duration(microseconds) * time.Microsecond, true
}

func commandFailure(ctx context.Context, name string, stderr *limitedBuffer, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	code := -1
	if exitError, ok := err.(*exec.ExitError); ok {
		code = exitError.ExitCode()
	}
	return &CommandError{Name: name, Code: code, Output: strings.TrimSpace(stderr.String()), Internal: err}
}

type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(value []byte) (int, error) {
	originalLength := len(value)
	remaining := b.limit - b.Len()
	if remaining > 0 {
		if len(value) > remaining {
			value = value[:remaining]
		}
		if _, err := b.Buffer.Write(value); err != nil {
			return 0, err
		}
	}
	return originalLength, nil
}
