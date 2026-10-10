package webtls

import (
	"bytes"
	"crypto/x509"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"time"
)

// InstallBundle 先验证完整证书包再原子替换，下载失败不得损坏当前 HTTPS 入口。
func InstallBundle(path, domain string, bundle []byte) (bool, error) {
	return installBundle(path, domain, bundle, nil, time.Now())
}

func installBundle(path, domain string, bundle []byte, roots *x509.CertPool, now time.Time) (bool, error) {
	if err := ValidateDomain(domain); err != nil {
		return false, err
	}
	if !filepath.IsAbs(path) || len(bundle) == 0 || len(bundle) > 128*1024 {
		return false, errors.New("HTTPS 证书更新路径或大小无效")
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return false, errors.New("无法创建 HTTPS 私有目录")
	}
	if err := checkPrivateDirectory(directory); err != nil {
		return false, err
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || checkPrivateBundle(info) != nil {
			return false, errors.New("HTTPS 当前证书文件身份无效")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, errors.New("无法检查 HTTPS 当前证书")
	}
	file, err := os.CreateTemp(directory, ".certificate-*")
	if err != nil {
		return false, errors.New("无法创建 HTTPS 证书临时文件")
	}
	name := file.Name()
	defer os.Remove(name)
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return false, err
	}
	if _, err := file.Write(bundle); err != nil {
		return false, errors.New("HTTPS 证书写入失败")
	}
	if err := file.Sync(); err != nil {
		return false, errors.New("HTTPS 证书持久化失败")
	}
	if err := file.Close(); err != nil {
		return false, err
	}
	candidate, err := loadCertificate(name, domain, roots, now)
	if err != nil {
		return false, err
	}
	if current, err := loadCertificate(path, domain, roots, now); err == nil {
		if bytes.Equal(current.Certificate[0], candidate.Certificate[0]) {
			return false, nil
		}
		if candidate.Leaf.NotAfter.Before(current.Leaf.NotAfter) {
			return false, errors.New("HTTPS 拒绝回退至更早到期的证书")
		}
	}
	if err := os.Rename(name, path); err != nil {
		return false, errors.New("HTTPS 证书原子替换失败")
	}
	if runtime.GOOS != "linux" {
		return true, nil
	}
	// 同步目录使断电后仍能恢复新文件；替换不触发服务或 USB 重启。
	dir, err := os.Open(directory)
	if err != nil {
		return true, err
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return true, errors.New("HTTPS 证书目录持久化失败")
	}
	return true, nil
}
