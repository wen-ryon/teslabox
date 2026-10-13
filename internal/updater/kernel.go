package updater

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
)

// KernelUpgradeVersion 固定本次跨版本升级必须安装的内核依赖，不能由页面请求关闭。
const KernelUpgradeVersion = "0.2.0-rc.1"

const kernelPolicyPath = "/usr/local/share/teslabox/kernel/rc1.json"
const kernelControllerPath = "/usr/local/libexec/teslabox-kernel-upgrade.py"
const kernelResumeRequestPath = "/var/lib/teslabox-system/kernel-upgrade/request.json"
const kernelRelease = "5.10.160-teslabox3"
const apfix6Digest = "31d0551815dc2163db700eb24239d98d003c65f270d05270d079887955b7ebce"
const kernelReleasePublicKey = "7THPh+C3xTzkCPWRHYSClGnZasEHkprykDK2kh0rkcU="

// KernelPolicy 是独立签名的强制依赖；保持旧客户端的主程序清单格式不变。
type KernelPolicy struct {
	Schema     int                   `json:"schema"`
	Kind       string                `json:"kind"`
	Version    string                `json:"version"`
	Board      string                `json:"board"`
	Arch       string                `json:"arch"`
	Release    string                `json:"kernel_release"`
	MainSHA256 string                `json:"main_sha256"`
	URL        string                `json:"url"`
	SHA256     string                `json:"sha256"`
	Size       int64                 `json:"size"`
	Files      map[string]KernelFile `json:"files"`
}

// KernelFile 绑定归档内普通文件，避免额外模块、路径或安装脚本混入依赖包。
type KernelFile struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// VerifyKernelPolicy 只接受既有发布公钥签署的 RC1 内核依赖，不执行任何安装。
func VerifyKernelPolicy(payload, signature []byte, key ed25519.PublicKey) (KernelPolicy, error) {
	var p KernelPolicy
	if len(payload) == 0 || len(payload) > 128<<10 {
		return p, errors.New("内核清单长度无效")
	}
	sig, err := base64.StdEncoding.DecodeString(string(bytes.TrimSpace(signature)))
	if err != nil || len(key) != ed25519.PublicKeySize || !ed25519.Verify(key, payload, sig) {
		return p, errors.New("内核依赖签名校验失败")
	}
	d := json.NewDecoder(bytes.NewReader(payload))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil {
		return p, fmt.Errorf("解析内核清单: %w", err)
	}
	if err := d.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return p, errors.New("内核清单包含多余内容")
	}
	u, err := url.Parse(p.URL)
	if p.Schema != 1 || p.Kind != "teslabox-kernel-dependency-v1" || p.Version != KernelUpgradeVersion ||
		p.Board != "radxa-rock-3a" || p.Arch != "arm64" || p.Release != kernelRelease ||
		err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" ||
		p.Size <= 0 || p.Size > 96<<20 || !kernelDigest(p.SHA256) || !kernelDigest(p.MainSHA256) {
		return p, errors.New("内核依赖与本次 RC1 升级不匹配")
	}
	if len(p.Files) < 4 || len(p.Files) > 128 {
		return p, errors.New("内核依赖文件清单数量无效")
	}
	var total int64
	for name, file := range p.Files {
		if !filepath.IsLocal(name) || filepath.ToSlash(filepath.Clean(name)) != name || bytes.ContainsAny([]byte(name), "\\\r\n") ||
			file.Size <= 0 || file.Size > 64<<20 || !kernelDigest(file.SHA256) {
			return p, errors.New("内核依赖文件路径或摘要无效")
		}
		total += file.Size
	}
	if total > 160<<20 || p.Files["modules/8852be.ko"].SHA256 != apfix6Digest ||
		p.Files["kernel.deb"].SHA256 != "18701fb698937b9627dff6f515fc543dc3556418ab59da92e06232ce9c645a88" {
		return p, errors.New("内核或 apfix6 不是本次冻结产物")
	}
	return p, nil
}

func kernelDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	b, err := hex.DecodeString(s)
	return err == nil && hex.EncodeToString(b) == s
}

// ReadKernelPolicy 验签同一份只读字节，供 root 安装器使用，避免 Python 额外依赖。
func ReadKernelPolicy(path string) (KernelPolicy, error) {
	var p KernelPolicy
	for _, name := range []string{path, path + ".sig"} {
		if err := plainPath(name); err != nil {
			return p, err
		}
	}
	_, payload, err := snapshotFile(path, 128<<10)
	if err != nil {
		return p, err
	}
	_, sig, err := snapshotFile(path+".sig", 1024)
	if err != nil {
		return p, err
	}
	key, _ := base64.StdEncoding.DecodeString(kernelReleasePublicKey)
	return VerifyKernelPolicy(payload, sig, ed25519.PublicKey(key))
}

type kernelActions interface {
	prepare(context.Context) (bool, error)
	commit(context.Context) error
	fail(context.Context) error
}

// 主程序提交必须发生在新内核运行并验过模块之后；任何失败都不能写 success。
func installWithKernel(ctx context.Context, required bool, gate kernelActions, install func() error) (bool, error) {
	if !required {
		return false, install()
	}
	waiting, err := gate.prepare(ctx)
	if err != nil {
		return false, err
	}
	if waiting {
		return true, nil
	}
	if err := install(); err != nil {
		return false, errors.Join(err, gate.fail(ctx))
	}
	// 程序已持久提交，收据失败只报告维护错误，不能再撤销已经提交的用户数据。
	if err := gate.commit(ctx); err != nil {
		return false, fmt.Errorf("程序已提交但内核收据未确认，未报成功: %w", err)
	}
	return false, nil
}

type rootKernelActions struct {
	config  HelperConfig
	request InstallRequest
}

func (g rootKernelActions) action(ctx context.Context, action string) ([]byte, error) {
	if err := plainPath(kernelControllerPath); err != nil {
		return nil, err
	}
	p, err := ReadKernelPolicy(kernelPolicyPath)
	if err != nil {
		return nil, err
	}
	if p.MainSHA256 != g.request.SHA256 {
		return nil, errors.New("主程序不属于已签名的内核依赖发布")
	}
	input, err := json.Marshal(g.request)
	if err != nil {
		return nil, err
	}
	command := exec.CommandContext(ctx, "/usr/bin/python3", kernelControllerPath, action)
	command.Stdin = bytes.NewReader(input)
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("内核升级 %s 失败: %w: %.4096s", action, err, output)
	}
	return output, nil
}

func (g rootKernelActions) prepare(ctx context.Context) (bool, error) {
	output, err := g.action(ctx, "prepare")
	if err != nil {
		return false, err
	}
	var result struct {
		RebootRequired bool `json:"reboot_required"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		return false, errors.New("内核安装结果无效")
	}
	return result.RebootRequired, nil
}
func (g rootKernelActions) commit(ctx context.Context) error {
	_, err := g.action(ctx, "commit")
	return err
}
func (g rootKernelActions) fail(ctx context.Context) error {
	// 已提交的程序事务不能回到旧内核；只允许未提交/已回滚的程序恢复旧启动菜单。
	tx, err := readTransaction(filepath.Join(g.config.BackupDir, "active"), transactionTargets(g.config))
	if err != nil || tx.Phase == "committed" {
		return errors.New("程序事务未确认可回滚，保留内核现场")
	}
	_, err = g.action(ctx, "rollback")
	return err
}

// KernelResumePending 只读检查 root 私有请求；普通主程序请求不能伪造重启续装。
func KernelResumePending() (bool, error) {
	_, err := os.Lstat(kernelResumeRequestPath)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, plainPath(kernelResumeRequestPath)
}

// KernelResumeRequestPath 是内核跨重启事务保存的固定安装请求路径。
func KernelResumeRequestPath() string { return kernelResumeRequestPath }

// VerifyRunningKernel 在新版主程序构造持久状态前核对内核和当前程序的签名绑定。
func VerifyRunningKernel(ctx context.Context) error {
	p, err := ReadKernelPolicy(kernelPolicyPath)
	if err != nil {
		return err
	}
	_, data, err := snapshotFile("/usr/local/bin/teslabox", 128<<20)
	if err != nil {
		return err
	}
	if kernelFileDigest(data) != p.MainSHA256 {
		return errors.New("当前主程序不属于内核依赖发布")
	}
	g := rootKernelActions{request: InstallRequest{Version: KernelUpgradeVersion, SHA256: p.MainSHA256}}
	_, err = g.action(ctx, "verify")
	return err
}

func kernelFileDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
