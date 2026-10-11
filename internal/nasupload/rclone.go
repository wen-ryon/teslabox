package nasupload

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Rclone 使用固定程序和独立配置；网络传输不占用媒体编码锁。
type Rclone struct {
	Executable string
	WorkDir    string
}

type commandFailure struct {
	message   string
	cause     error
	permanent bool
}

func (e *commandFailure) Error() string { return e.message }
func (e *commandFailure) Unwrap() error { return e.cause }

type limitedDiagnostic struct{ bytes.Buffer }

func (b *limitedDiagnostic) Write(data []byte) (int, error) {
	n := len(data)
	if b.Len() < 16384 {
		_, _ = b.Buffer.Write(data[:min(n, 16384-b.Len())])
	}
	return n, nil
}

func classifyCommandError(err error, diagnostic string) error {
	message := "网络、认证或目录操作失败"
	lower := strings.ToLower(diagnostic)
	switch {
	case strings.Contains(lower, "key mismatch") || strings.Contains(lower, "key is unknown") || strings.Contains(lower, "no authorities"):
		message = "SFTP 服务器公钥校验失败，请核对公钥及主机端口"
	case strings.Contains(lower, "unable to authenticate") || strings.Contains(lower, "authentication failed") || strings.Contains(lower, "logon failure") || strings.Contains(lower, "401 unauthorized"):
		message = "账号认证失败，请核对用户名和密码"
	case strings.Contains(lower, "permission denied") || strings.Contains(lower, "403 forbidden"):
		message = "上传账号没有目标目录的读写权限"
	case strings.Contains(lower, "certificate") || strings.Contains(lower, "tls handshake"):
		message = "WebDAV TLS 证书校验失败"
	case strings.Contains(lower, "connection refused") || strings.Contains(lower, "i/o timeout") || strings.Contains(lower, "no such host") || strings.Contains(lower, "network is unreachable"):
		message = "NAS 网络连接失败，请核对地址、端口和网络"
	case strings.Contains(lower, "directory not found"):
		message = "上传目标目录不存在"
	}
	permanent := strings.Contains(message, "认证失败") || strings.Contains(message, "读写权限") || strings.Contains(message, "公钥校验失败") || strings.Contains(message, "证书校验失败")
	if strings.Contains(lower, "quota") || strings.Contains(lower, "no space left") {
		message = "目标存储空间或配额不足"
		permanent = true
	}
	return &commandFailure{message: message, cause: err, permanent: permanent}
}

func permanentUploadError(err error) bool {
	var failure *commandFailure
	return errors.As(err, &failure) && failure.permanent
}

func (r *Rclone) command(ctx context.Context, t Target, args []string, stdout io.Writer, stdin io.Reader) error {
	if r.Executable == "" {
		return &commandFailure{message: "NAS 上传工具 rclone 尚未安装", permanent: true}
	}
	config, err := os.CreateTemp(r.WorkDir, ".rclone-*")
	if err != nil {
		return errors.New("无法创建上传工具配置")
	}
	configName := config.Name()
	_ = config.Close()
	defer os.Remove(configName)
	args = append(args, "--config", configName, "--log-level", "ERROR", "--retries", "1", "--low-level-retries", "1", "--contimeout", "15s", "--timeout", "60s", "--transfers", "1", "--checkers", "1")
	cmd := exec.CommandContext(ctx, r.Executable, args...)
	cmd.Stdout = stdout
	cmd.Stdin = stdin
	var diagnostic limitedDiagnostic
	cmd.Stderr = &diagnostic
	cmd.WaitDelay = 2 * time.Second
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(entry), "RCLONE_") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	add := func(k, v string) { cmd.Env = append(cmd.Env, "RCLONE_CONFIG_TBNAS_"+k+"="+v) }
	add("TYPE", t.Protocol)
	add("USER", t.User)
	add("PASS", t.Credential)
	if t.Protocol == "webdav" {
		add("URL", t.URL)
		add("VENDOR", "other")
	} else if t.Protocol == "smb" || t.Protocol == "sftp" {
		add("HOST", t.Host)
		port := t.Port
		if port == 0 {
			if t.Protocol == "smb" {
				port = 445
			} else {
				port = 22
			}
		}
		add("PORT", strconv.Itoa(port))
		if t.Protocol == "smb" {
			add("DOMAIN", t.Domain)
		} else if t.Protocol == "sftp" {
			// known_hosts 比较早支持，兼容当前固定版本，不自动信任首次连接。
			keyFile, e := os.CreateTemp(r.WorkDir, ".hostkey-*")
			if e != nil {
				return errors.New("无法创建 SFTP 公钥校验配置")
			}
			keyName := keyFile.Name()
			defer os.Remove(keyName)
			// OpenSSH 非默认端口必须使用 [host]:port；默认端口也允许此形式。
			_, e = keyFile.WriteString("[" + t.Host + "]:" + strconv.Itoa(port) + " " + t.HostKey + "\n")
			closeErr := keyFile.Close()
			if e != nil || closeErr != nil {
				return errors.New("保存 SFTP 公钥校验配置失败")
			}
			add("KNOWN_HOSTS_FILE", keyName)
			// NAS 同时提供多种主机密钥时，默认协商可能选到未保存的那一种。
			// 只协商用户已核验公钥的算法；RSA 使用 SHA-2 签名而不是旧 SHA-1。
			algorithm := strings.Fields(t.HostKey)[0]
			if algorithm == "ssh-rsa" {
				algorithm = "rsa-sha2-512 rsa-sha2-256"
			}
			add("HOST_KEY_ALGORITHMS", algorithm)
			add("DISABLE_HASHCHECK", "true")
		}
	}
	err = cmd.Run()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			return &commandFailure{message: "NAS 上传工具 rclone 尚未安装", cause: err, permanent: true}
		}
		return classifyCommandError(err, diagnostic.String())
	}
	return nil
}

// Obscure 通过标准输入传密码，避免进入进程参数和日志；结果仍需私有文件权限保护。
func (r *Rclone) Obscure(ctx context.Context, password string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var out bytes.Buffer
	if err := r.command(ctx, Target{}, []string{"obscure", "-"}, &out, strings.NewReader(password+"\n")); err != nil {
		return "", errors.New("无法保存密码，请确认 rclone 已安装")
	}
	value := strings.TrimSpace(out.String())
	if value == "" || len(value) > 4096 {
		return "", errors.New("上传密码处理失败")
	}
	return value, nil
}

func (r *Rclone) exists(ctx context.Context, t Target, name string) (bool, error) {
	err := r.command(ctx, t, []string{"lsjson", "tbnas:" + name, "--stat"}, io.Discard, nil)
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) && (exit.ExitCode() == 3 || exit.ExitCode() == 4) {
		return false, nil
	}
	return false, fmt.Errorf("无法读取 NAS 目录：%w", err)
}

func (r *Rclone) verify(ctx context.Context, t Target, name, expected string) error {
	h := sha256.New()
	if err := r.command(ctx, t, []string{"cat", "tbnas:" + name}, h, nil); err != nil {
		return fmt.Errorf("远端文件读回失败，未记为上传成功：%w", err)
	}
	if hex.EncodeToString(h.Sum(nil)) != expected {
		return &commandFailure{message: "远端文件 SHA256 不一致，未记为上传成功", permanent: true}
	}
	return nil
}

// Upload 先核验已有文件，再上传到任务专用临时名；发布后再次读回核验才返回成功。
func (r *Rclone) Upload(ctx context.Context, t Target, local, remote, hash, taskID string) error {
	exists, err := r.exists(ctx, t, remote)
	if err != nil {
		return err
	}
	if exists {
		return r.verify(ctx, t, remote, hash)
	}
	partial := remote + ".teslabox-" + taskID + ".partial"
	if err = r.command(ctx, t, []string{"copyto", local, "tbnas:" + partial, "--ignore-times"}, io.Discard, nil); err != nil {
		return fmt.Errorf("NAS 写入失败，系统盘输入已保留：%w", err)
	}
	if err = r.verify(ctx, t, partial, hash); err != nil {
		return err
	}
	// 内容摘要隔离远端目录，不覆盖其它内容；成功响应丢失后重试会重新核验最终文件。
	if err = r.command(ctx, t, []string{"moveto", "tbnas:" + partial, "tbnas:" + remote, "--immutable"}, io.Discard, nil); err != nil {
		return fmt.Errorf("发布远端文件失败，请重试核验：%w", err)
	}
	return r.verify(ctx, t, remote, hash)
}

// Test 写读删专用随机文件，不能用目录列表成功冒充上传权限验证。
func (r *Rclone) Test(ctx context.Context, t Target) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	f, err := os.CreateTemp(r.WorkDir, ".nas-probe-*")
	if err != nil {
		return errors.New("无法创建连接测试样本")
	}
	local := f.Name()
	defer os.Remove(local)
	content := []byte("TeslaBox NAS permission probe " + newID())
	_, err = f.Write(content)
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		return errors.New("无法写入测试样本")
	}
	h := sha256.Sum256(content)
	remote := path.Join(t.Path, ".teslabox-probes", filepath.Base(local))
	if err = r.command(ctx, t, []string{"copyto", local, "tbnas:" + remote, "--immutable"}, io.Discard, nil); err != nil {
		return fmt.Errorf("NAS 写入测试失败：%w", err)
	}
	verifyErr := r.verify(ctx, t, remote, hex.EncodeToString(h[:]))
	deleteErr := r.command(ctx, t, []string{"deletefile", "tbnas:" + remote}, io.Discard, nil)
	if verifyErr != nil {
		return verifyErr
	}
	if deleteErr != nil {
		return errors.New("读写测试通过，但专用测试文件删除失败，请检查权限")
	}
	return nil
}
