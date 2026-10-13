package updater

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/wen-ryon/teslabox/internal/version"
)

// DefaultBackupDir 固定事务目录，避免用户请求控制恢复路径。
const DefaultBackupDir = "/var/lib/teslabox-system/update-backups"

// DefaultExportState 与主程序默认导出状态路径保持一致。
const DefaultExportState = "/var/lib/teslabox-system/export-state.json"

// TransactionProtocol 标识具有成对回滚和断电恢复能力的安装助手。
const TransactionProtocol = "teslabox-paired-state-v1"

// ProgramTransactionCapability 随主程序摘要一并签名，供助手离线拒绝没有保护能力的旧程序。
const ProgramTransactionCapability = "\x00teslabox-main-startup-protection:teslabox-paired-state-v1\x00"

// 禁止编译期跨调用常量折叠，确保实际启动检查使用的声明保留在主程序只读段。
//
//go:noinline
func programCapabilityDeclaration() string { return ProgramTransactionCapability }

// ActivationPending 防止健康检查前执行导出清理或接受会改变用户数据的请求。
func ActivationPending() bool {
	// 事务记录重命名与目录同步之间仍属未提交窗口，安装锁释放后才放行工作线程。
	if installationActive(DefaultBackupDir) {
		return true
	}
	return activationPending(filepath.Join(DefaultBackupDir, "active", transactionName))
}

func activationPending(path string) bool {
	if commitPending(filepath.Dir(path)) {
		return true
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	if err != nil {
		return true
	}
	var tx updateTransaction
	if json.Unmarshal(data, &tx) != nil || tx.Version != 1 {
		return true
	}
	return tx.Phase != "committed" && tx.Phase != "rolled_back"
}

// WaitActivation 在安装事务提交前暂停媒体任务，退出时仍由调用方释放任务资源。
func WaitActivation(ctx context.Context) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for ActivationPending() {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// ActivationHandler 只放行健康探测，避免试运行期间的用户写入被失败回滚撤销。
func ActivationHandler(next http.Handler) http.Handler {
	return activationHandler(next, ActivationPending)
}

func activationHandler(next http.Handler, pending func() bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.Header().Set("X-TeslaBox-Process", strconv.Itoa(os.Getpid()))
		}
		if pending() && (r.URL.Path != "/healthz" || r.Method != http.MethodGet) {
			w.Header().Set("Retry-After", "1")
			http.Error(w, "更新正在验证，请稍后重试", http.StatusServiceUnavailable)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// CheckStartupProtection 必须先于任何持久状态构造器执行，旧助手不能安全回滚迁移。
func CheckStartupProtection(ctx context.Context) error {
	if runtime.GOOS != "linux" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	run := func(name string, args ...string) ([]byte, error) {
		return exec.CommandContext(ctx, name, args...).Output()
	}
	if err := checkStartupProtection(run); err != nil {
		return err
	}
	if version.Version == KernelUpgradeVersion {
		return checkKernelStartup(run)
	}
	return nil
}

func checkKernelStartup(run func(string, ...string) ([]byte, error)) error {
	// 旧 959 助手仅支持程序/状态事务，不能把它误当作已具备内核强依赖保护。
	out, err := run("/usr/local/libexec/teslabox-update-helper", "--kernel-protocol")
	if err != nil || strings.TrimSpace(string(out)) != "teslabox-kernel-first-v1" {
		return errors.New("必须先安装支持 teslabox3 强制升级的维护助手；未执行状态迁移")
	}
	out, err = run("/usr/local/libexec/teslabox-update-helper", "--kernel-verify")
	if err != nil || strings.TrimSpace(string(out)) != "teslabox-kernel-ready-v1" {
		return errors.New("teslabox3 或 apfix6 尚未确认，拒绝启动 RC1；未执行状态迁移")
	}
	return nil
}

func checkStartupProtection(run func(string, ...string) ([]byte, error)) error {
	out, err := run("/usr/local/libexec/teslabox-update-helper", "--protocol")
	declared := strings.TrimSuffix(strings.TrimPrefix(programCapabilityDeclaration(), "\x00teslabox-main-startup-protection:"), "\x00")
	if err != nil || strings.TrimSpace(string(out)) != declared {
		return errors.New("必须先安装支持状态回滚的更新助手；未执行状态迁移")
	}
	out, err = run("systemctl", "show", "teslabox.service", "--property=ExecStartPre", "--value")
	if err != nil || !strings.Contains(string(out), "/usr/local/libexec/teslabox-update-helper --recover") {
		return errors.New("必须先安装启动恢复守卫；未执行状态迁移")
	}
	return nil
}

func checkRunningStatePath(ctx context.Context, config HelperConfig) error {
	out, err := exec.CommandContext(ctx, "systemctl", "show", config.Service, "--property=MainPID", "--value").Output()
	if err != nil {
		return err
	}
	pid := strings.TrimSpace(string(out))
	if pid == "0" {
		return nil
	}
	for _, c := range pid {
		if c < '0' || c > '9' {
			return errors.New("服务进程标识无效")
		}
	}
	if pid == "" {
		return errors.New("服务进程标识缺失")
	}
	data, err := os.ReadFile(filepath.Join("/proc", pid, "environ"))
	if err != nil {
		return err
	}
	state := DefaultExportState
	for _, entry := range strings.Split(string(data), "\x00") {
		if value, ok := strings.CutPrefix(entry, "TESLABOX_EXPORT_STATE="); ok && value != "" {
			state = value
		}
	}
	if state != transactionTargets(config)[1] {
		return errors.New("主程序与更新助手的导出状态路径不一致，拒绝安装")
	}
	return nil
}
