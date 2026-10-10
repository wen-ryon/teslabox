// Package webtls 为本地页面提供可信域名的 HTTPS 入口，证书签发不依赖本机公网监听。
package webtls

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

var domainLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidateDomain 拒绝无法作为产品公网证书名称的 IP、局域网名称及 URL。
func ValidateDomain(domain string) error {
	if domain != strings.ToLower(domain) || len(domain) > 253 || net.ParseIP(domain) != nil {
		return errors.New("HTTPS 域名格式无效")
	}
	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		return errors.New("HTTPS 必须配置完整公网域名")
	}
	for _, label := range labels {
		if !domainLabel.MatchString(label) {
			return errors.New("HTTPS 域名格式无效")
		}
	}
	switch labels[len(labels)-1] {
	case "local", "localhost", "test", "invalid", "example", "onion":
		return errors.New("HTTPS 必须配置产品公网域名")
	}
	return nil
}

func loadCertificate(bundlePath, domain string, roots *x509.CertPool, now time.Time) (*tls.Certificate, error) {
	if err := checkPrivateDirectory(filepath.Dir(bundlePath)); err != nil {
		return nil, err
	}
	before, err := os.Lstat(bundlePath)
	if err != nil || !before.Mode().IsRegular() || checkPrivateBundle(before) != nil {
		return nil, errors.New("HTTPS 私有证书文件不可用")
	}
	file, err := os.Open(bundlePath)
	if err != nil {
		return nil, errors.New("HTTPS 私有证书文件不可用")
	}
	defer file.Close()
	actual, err := file.Stat()
	if err != nil || !os.SameFile(before, actual) || checkPrivateBundle(actual) != nil {
		return nil, errors.New("HTTPS 证书文件身份已改变")
	}
	bytes, err := io.ReadAll(io.LimitReader(file, 128*1024+1))
	defer clear(bytes)
	if err != nil || len(bytes) > 128*1024 {
		return nil, errors.New("HTTPS 证书文件读取失败")
	}
	// 证书链和私钥放在同一私有文件，原子替换即可避免更新途中读到不匹配的一对文件。
	certificate, err := tls.X509KeyPair(bytes, bytes)
	if err != nil || len(certificate.Certificate) == 0 {
		return nil, errors.New("HTTPS 证书与私钥不匹配")
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return nil, errors.New("HTTPS 证书格式无效")
	}
	intermediates := x509.NewCertPool()
	for _, der := range certificate.Certificate[1:] {
		entry, err := x509.ParseCertificate(der)
		if err != nil {
			return nil, errors.New("HTTPS 证书链格式无效")
		}
		intermediates.AddCert(entry)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: domain, Roots: roots, Intermediates: intermediates, CurrentTime: now}); err != nil {
		return nil, errors.New("HTTPS 证书信任、域名或有效期验证失败")
	}
	certificate.Leaf = leaf
	return &certificate, nil
}

// NewServer 复用现有认证页面，通过每次握手读取私有原子证书文件支持在线续期。
func NewServer(address, domain, bundlePath string, handler http.Handler) (*http.Server, error) {
	if err := ValidateDomain(domain); err != nil {
		return nil, err
	}
	if handler == nil || bundlePath == "" {
		return nil, errors.New("HTTPS 页面或证书配置缺失")
	}
	return &http.Server{
		Addr: address,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			host := r.Host
			if name, _, err := net.SplitHostPort(host); err == nil {
				host = name
			}
			if !strings.EqualFold(host, domain) {
				http.Error(w, "HTTPS 域名不匹配", http.StatusMisdirectedRequest)
				return
			}
			handler.ServeHTTP(w, r)
		}),
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			if !strings.EqualFold(hello.ServerName, domain) {
				return nil, errors.New("HTTPS 请求域名不匹配")
			}
			return loadCertificate(bundlePath, domain, nil, time.Now())
		}},
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Minute,
		IdleTimeout:       60 * time.Second,
	}, nil
}
