package runtimecomponents

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"debug/elf"
	_ "embed"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

//go:embed COPYING.rclone
var rcloneLicense []byte

// NASExecutable 的位置由签名描述决定，旧版本和旧兼容路径永不覆盖。
func NASExecutable(parent string, c Component) string {
	return filepath.Join(parent, c.SHA256, "bin", "rclone")
}

// InstallNAS 使用独立的不可变版本目录，main 回滚时旧工具仍在原处。
func InstallNAS(ctx context.Context, c Component, archive, parent string) error {
	if runtime.GOOS != "linux" || runtime.GOARCH != "arm64" || os.Geteuid() != 0 {
		return errors.New("NAS 产品运行时仅支持 root ARM64 Linux")
	}
	return installNAS(ctx, c, archive, parent, checkNASVersion)
}
func installNAS(ctx context.Context, c Component, archive, parent string, preflight func(context.Context, string, string) error) error {
	if c.ID != "nas" || c.Format != "zip" {
		return errors.New("NAS 组件类型不正确")
	}
	if err := ValidateFile(ctx, c, archive); err != nil {
		return err
	}
	if err := secureDirectory(parent); err != nil {
		return err
	}
	target := filepath.Join(parent, c.SHA256)
	if _, err := os.Lstat(target); err == nil {
		if err = secureDirectory(target); err != nil {
			return err
		}
		if err = checkNASFiles(ctx, c, archive, target, false); err != nil {
			return err
		}
		return preflight(ctx, filepath.Join(target, "bin", "rclone"), c.Version)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	stage, err := os.MkdirTemp(parent, ".nas-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err = checkNASFiles(ctx, c, archive, stage, true); err != nil {
		return err
	}
	if err = preflight(ctx, filepath.Join(stage, "bin", "rclone"), c.Version); err != nil {
		return err
	}
	if err = syncDirectory(stage); err != nil {
		return err
	}
	if err = os.Rename(stage, target); err != nil {
		return err
	}
	return syncDirectory(parent)
}
func checkNASVersion(ctx context.Context, binary, version string) error {
	f, err := elf.Open(binary)
	if err != nil {
		return err
	}
	valid := f.Class == elf.ELFCLASS64 && f.Data == elf.ELFDATA2LSB && f.Machine == elf.EM_AARCH64 && (f.Type == elf.ET_EXEC || f.Type == elf.ET_DYN)
	f.Close()
	if !valid {
		return errors.New("NAS 工具不是 ARM64 ELF")
	}
	test, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	command := exec.CommandContext(test, binary, "version")
	command.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent"}
	var out limitedOutput
	command.Stdout = &out
	command.Stderr = io.Discard
	if err = command.Run(); err != nil {
		return errors.New("NAS 工具版本自检失败")
	}
	if !strings.HasPrefix(out.String(), "rclone v"+version+"\n") {
		return errors.New("NAS 工具版本不匹配")
	}
	return nil
}

type limitedOutput struct{ bytes.Buffer }

func (b *limitedOutput) Write(p []byte) (int, error) {
	n := len(p)
	if b.Len()+n > 4096 {
		return 0, errors.New("组件自检输出过大")
	}
	return b.Buffer.Write(p)
}
func checkNASFiles(ctx context.Context, c Component, archive, root string, extract bool) error {
	reader, err := zip.OpenReader(archive)
	if err != nil {
		return err
	}
	defer reader.Close()
	prefix := "rclone-v" + c.Version + "-linux-arm64/"
	destinations := map[string]string{prefix: "", prefix + "rclone": "bin/rclone", prefix + "README.txt": "README.txt", prefix + "README.html": "README.html", prefix + "rclone.1": "rclone.1", prefix + "git-log.txt": "git-log.txt"}
	if len(reader.File) != len(destinations) {
		return errors.New("NAS ZIP 成员不完整")
	}
	seen := map[string]bool{}
	for _, member := range reader.File {
		rel, ok := destinations[member.Name]
		if !ok || seen[member.Name] || member.Mode()&os.ModeSymlink != 0 {
			return errors.New("NAS ZIP 成员不安全")
		}
		seen[member.Name] = true
		if rel == "" {
			if !member.FileInfo().IsDir() {
				return errors.New("NAS ZIP 根目录不正确")
			}
			continue
		}
		if !member.Mode().IsRegular() || member.UncompressedSize64 > 100<<20 {
			return errors.New("NAS ZIP 成员类型或大小无效")
		}
		if rel != "bin/rclone" && member.UncompressedSize64 > 8<<20 {
			return errors.New("NAS 文档过大")
		}
		in, e := member.Open()
		if e != nil {
			return e
		}
		target := filepath.Join(root, filepath.FromSlash(rel))
		mode := os.FileMode(0644)
		if rel == "bin/rclone" {
			mode = 0755
		}
		e = installOrVerify(ctx, in, target, int64(member.UncompressedSize64), mode, extract)
		closeErr := in.Close()
		if e = errors.Join(e, closeErr); e != nil {
			return e
		}
	}
	return installOrVerify(ctx, bytes.NewReader(rcloneLicense), filepath.Join(root, "LICENSE.rclone"), int64(len(rcloneLicense)), 0644, extract)
}
func installOrVerify(ctx context.Context, source io.Reader, target string, size int64, mode os.FileMode, extract bool) error {
	if err := secureDirectory(filepath.Dir(target)); err != nil {
		return err
	}
	expected := sha256.New()
	if extract {
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		f, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			return err
		}
		if err = f.Chmod(mode); err != nil {
			f.Close()
			return err
		}
		n, e := io.Copy(io.MultiWriter(f, expected), io.LimitReader(contextReader{ctx, source}, size+1))
		if e == nil && n != size {
			e = errors.New("NAS ZIP 展开长度不一致")
		}
		if e == nil {
			e = f.Sync()
		}
		e = errors.Join(e, f.Close())
		if e != nil {
			return e
		}
		return syncDirectory(filepath.Dir(target))
	}
	if err := checkFile(target); err != nil {
		return err
	}
	info, err := os.Lstat(target)
	if err != nil {
		return err
	}
	if info.Size() != size || runtime.GOOS == "linux" && info.Mode().Perm() != mode {
		return errors.New("已安装 NAS 工具长度或权限改变")
	}
	if _, err = io.Copy(expected, io.LimitReader(contextReader{ctx, source}, size+1)); err != nil {
		return err
	}
	f, err := os.Open(target)
	if err != nil {
		return err
	}
	actual := sha256.New()
	_, e := io.Copy(actual, contextReader{ctx, f})
	e = errors.Join(e, f.Close())
	if e != nil {
		return e
	}
	if hex.EncodeToString(expected.Sum(nil)) != hex.EncodeToString(actual.Sum(nil)) {
		return errors.New("已安装 NAS 工具内容改变")
	}
	return nil
}
