// Package maintenanceguard 协调跨进程存储维护与 USB Gadget 绑定。
package maintenanceguard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// OperationState 是异常重启后仍可解释的维护操作状态。
type OperationState string

const (
	StateIdle            OperationState = "idle"
	StateRunning         OperationState = "running"
	StateInterrupted     OperationState = "interrupted"
	StateCompleted       OperationState = "completed"
	StateCancelledSafe   OperationState = "cancelled_safe"
	StateCancelledUnsafe OperationState = "cancelled_unsafe"
	StateFailed          OperationState = "failed"
)

// State 保存恢复所需事实，不把运行期文件锁误当成持久化进度。
type State struct {
	Version        int            `json:"version"`
	Operation      string         `json:"operation,omitempty"`
	State          OperationState `json:"state"`
	StartedAt      time.Time      `json:"started_at,omitempty"`
	UpdatedAt      time.Time      `json:"updated_at"`
	TargetBytes    uint64         `json:"target_bytes,omitempty"`
	ReclaimedBytes uint64         `json:"reclaimed_bytes,omitempty"`
	LastError      string         `json:"last_error,omitempty"`
}

// Guard 使用运行期锁和系统盘状态文件提供启动顺序无关的守卫。
type Guard struct {
	lockPath        string
	statePath       string
	gadgetPausePath string
	bootIDPath      string
}

// New 创建维护守卫，路径必须是非根目录绝对路径。
func New(lockPath, statePath string, startupPaths ...string) (*Guard, error) {
	for name, value := range map[string]string{"维护锁": lockPath, "维护状态": statePath} {
		if !filepath.IsAbs(value) || filepath.Clean(value) == string(filepath.Separator) {
			return nil, fmt.Errorf("%s路径必须是非根目录绝对路径", name)
		}
	}
	guard := &Guard{lockPath: lockPath, statePath: statePath}
	if len(startupPaths) == 2 {
		guard.gadgetPausePath = startupPaths[0]
		guard.bootIDPath = startupPaths[1]
	} else if len(startupPaths) != 0 {
		return nil, errors.New("Gadget 启动保护路径必须同时提供暂停状态和 boot_id")
	}
	return guard, nil
}

// Acquire 取得跨进程独占锁；释放函数在异常返回路径也必须调用。
func (g *Guard) Acquire(ctx context.Context) (func(), error) {
	return platformAcquire(ctx, g.lockPath)
}

// AcquireGadgetStart 在同一临界区检查状态，防止检查后到绑定前被维护抢占。
func (g *Guard) AcquireGadgetStart(ctx context.Context) (func(), error) {
	release, err := g.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	state, exists, err := g.Load()
	if err != nil {
		release()
		return nil, fmt.Errorf("读取维护状态，拒绝绑定 Gadget: %w", err)
	}
	if exists && blocksGadget(state.State) {
		release()
		return nil, fmt.Errorf("存储维护状态为 %s，拒绝绑定 Gadget", state.State)
	}
	if blocked, reason, err := g.gadgetStartBlocked(); err != nil {
		release()
		return nil, fmt.Errorf("读取 Gadget 启动保护，拒绝绑定: %w", err)
	} else if blocked {
		release()
		return nil, errors.New(reason)
	}
	return release, nil
}

func (g *Guard) gadgetStartBlocked() (bool, string, error) {
	if g.gadgetPausePath == "" {
		return false, "", nil
	}
	content, err := os.ReadFile(g.gadgetPausePath)
	if errors.Is(err, os.ErrNotExist) {
		return false, "", nil
	}
	if err != nil {
		return false, "", err
	}
	bootContent, err := os.ReadFile(g.bootIDPath)
	if err != nil {
		return false, "", err
	}
	value := strings.TrimSpace(string(content))
	bootID := strings.TrimSpace(string(bootContent))
	if value == "" || value == "paused" {
		return true, "USB Gadget 处于人工维护暂停状态", nil
	}
	for _, prefix := range []string{"attempt:", "service:", "resume-after:"} {
		if savedBootID, found := strings.CutPrefix(value, prefix); found {
			if savedBootID == "" || bootID == "" {
				return false, "", errors.New("Gadget 启动保护 boot_id 为空")
			}
			if savedBootID == bootID {
				return true, "本次开机周期不允许独立命令重新绑定 USB Gadget", nil
			}
			return false, "", nil
		}
	}
	return false, "", errors.New("Gadget 启动保护状态无法识别")
}

// Load 读取持久化状态；不存在表示设备从未进入可恢复维护流程。
func (g *Guard) Load() (State, bool, error) {
	info, err := os.Lstat(g.statePath)
	if errors.Is(err, os.ErrNotExist) {
		return State{}, false, nil
	}
	if err != nil {
		return State{}, false, fmt.Errorf("检查维护状态: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return State{}, false, errors.New("维护状态必须是普通文件")
	}
	content, err := os.ReadFile(g.statePath)
	if err != nil {
		return State{}, false, fmt.Errorf("读取维护状态: %w", err)
	}
	var state State
	if err := json.Unmarshal(content, &state); err != nil {
		return State{}, false, fmt.Errorf("解析维护状态: %w", err)
	}
	if state.Version != 1 || !validState(state.State) {
		return State{}, false, errors.New("维护状态版本或取值无效")
	}
	return state, true, nil
}

// Save 原子保存恢复状态，写入失败时不得解除 Gadget 安全暂停。
func (g *Guard) Save(state State) (resultErr error) {
	if !validState(state.State) {
		return errors.New("维护状态取值无效")
	}
	state.Version = 1
	state.Operation = strings.TrimSpace(state.Operation)
	state.UpdatedAt = time.Now().UTC()
	if err := os.MkdirAll(filepath.Dir(g.statePath), 0o750); err != nil {
		return fmt.Errorf("创建维护状态目录: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(g.statePath), ".operation-*")
	if err != nil {
		return fmt.Errorf("创建维护状态临时文件: %w", err)
	}
	name := temporary.Name()
	defer func() {
		_ = temporary.Close()
		if resultErr != nil {
			_ = os.Remove(name)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return err
	}
	if err := json.NewEncoder(temporary).Encode(state); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, g.statePath); err != nil {
		return fmt.Errorf("发布维护状态: %w", err)
	}
	return nil
}

// RecoverInterrupted 把上次进程未正常结束的运行态转换成可解释的中断态。
func (g *Guard) RecoverInterrupted() (State, bool, error) {
	state, exists, err := g.Load()
	if err != nil || !exists || state.State != StateRunning {
		return state, exists, err
	}
	state.State = StateInterrupted
	state.LastError = "维护进程未正常结束，请继续修复或执行安全放弃校验"
	if err := g.Save(state); err != nil {
		return State{}, true, err
	}
	return state, true, nil
}

func blocksGadget(state OperationState) bool {
	return state == StateRunning || state == StateInterrupted || state == StateCancelledUnsafe || state == StateFailed
}

func validState(state OperationState) bool {
	switch state {
	case StateIdle, StateRunning, StateInterrupted, StateCompleted, StateCancelledSafe, StateCancelledUnsafe, StateFailed:
		return true
	default:
		return false
	}
}
