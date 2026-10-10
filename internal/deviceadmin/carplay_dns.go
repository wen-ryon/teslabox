package deviceadmin

import (
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"strings"

	"github.com/wen-ryon/teslabox/internal/webtls"
)

func (m *Manager) carPlayDNSPath(lease CarPlayNetworkLease) (string, error) {
	if !validUUID(lease.RuntimeAPUUID) || !filepath.IsAbs(m.carPlayDNSDirectory) {
		return "", invalidInput("CarPlay DNS 租约或目录无效")
	}
	return filepath.Join(m.carPlayDNSDirectory, "teslabox-carplay-"+lease.RuntimeAPUUID+".conf"), nil
}

func carPlayDNSContent(lease CarPlayNetworkLease, domain string, address netip.Addr) ([]byte, error) {
	if !validUUID(lease.RuntimeAPUUID) || webtls.ValidateDomain(domain) != nil || !address.Is4() || (!address.IsPrivate() && (lease.WebAliasAddress != address.String() || validateCarPlayWebAlias(lease) != nil)) {
		return nil, invalidInput("CarPlay DNS 域名或热点地址无效")
	}
	// host-record 精确匹配产品主机名，不劫持同一域名下其他服务或公共 DNS 查询。
	content := fmt.Sprintf("# TeslaBox CarPlay lease %s\nhost-record=%s,%s\n", lease.RuntimeAPUUID, domain, address)
	if lease.WebAliasAddress != "" {
		// 仅兼容实车抓到的 HTTP 联网检测主机；不覆盖普通 Tesla API。
		content += fmt.Sprintf("host-record=connman.vn.cloud.tesla.cn,%s\n", address)
	}
	return []byte(content), nil
}

func (m *Manager) prepareCarPlayDNS(lease CarPlayNetworkLease, address netip.Addr) error {
	path, err := m.carPlayDNSPath(lease)
	if err != nil {
		return err
	}
	content, err := carPlayDNSContent(lease, m.carPlayDomain, address)
	if err != nil {
		return err
	}
	if err = os.MkdirAll(m.carPlayDNSDirectory, 0o755); err != nil {
		return errors.New("创建 CarPlay DNS 目录失败")
	}
	info, err := os.Lstat(m.carPlayDNSDirectory)
	if err != nil || !info.IsDir() || !carPlayDNSPermissionsSafe(info) {
		return errors.New("CarPlay DNS 目录不可写或权限无效")
	}
	if existing, err := readCarPlayDNS(path); err == nil {
		if string(existing) != string(content) {
			return errors.New("CarPlay DNS 文件已存在且内容不符")
		}
		return syncCarPlayDNSDirectory(m.carPlayDNSDirectory)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.CreateTemp(m.carPlayDNSDirectory, ".teslabox-carplay-dns-")
	if err != nil {
		return errors.New("创建 CarPlay DNS 临时文件失败")
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	defer file.Close()
	if err = file.Chmod(0o644); err == nil {
		_, err = file.Write(content)
	}
	if err == nil {
		err = file.Sync()
	}
	if err != nil {
		return errors.New("写入 CarPlay DNS 文件失败")
	}
	if err = file.Close(); err != nil {
		return errors.New("关闭 CarPlay DNS 文件失败")
	}
	// 同目录硬链接原子发布完整内容，O_EXCL 语义拒绝覆盖同时出现的其他文件。
	if err = os.Link(temporary, path); err != nil {
		return errors.New("发布 CarPlay DNS 文件失败，拒绝覆盖已有文件")
	}
	if err = os.Remove(temporary); err != nil {
		return errors.New("清理 CarPlay DNS 临时文件失败")
	}
	return syncCarPlayDNSDirectory(m.carPlayDNSDirectory)
}

func readCarPlayDNS(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 1024 || !carPlayDNSPermissionsSafe(info) {
		return nil, errors.New("CarPlay DNS 文件类型或权限无效")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	actual, err := file.Stat()
	if err != nil || !os.SameFile(info, actual) || !carPlayDNSPermissionsSafe(actual) {
		return nil, errors.New("CarPlay DNS 文件身份改变")
	}
	return io.ReadAll(io.LimitReader(file, 1025))
}

func (m *Manager) removeCarPlayDNS(lease CarPlayNetworkLease) error {
	if m.carPlayDNSDirectory == "" && m.carPlayDomain == "" {
		return nil
	}
	path, err := m.carPlayDNSPath(lease)
	if err != nil {
		return err
	}
	content, err := readCarPlayDNS(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	lines := strings.Split(string(content), "\n")
	if (len(lines) != 3 && len(lines) != 4) || lines[0] != "# TeslaBox CarPlay lease "+lease.RuntimeAPUUID || lines[len(lines)-1] != "" || !strings.HasPrefix(lines[1], "host-record=") {
		return errors.New("CarPlay DNS 文件身份无效，拒绝清理")
	}
	fields := strings.Split(strings.TrimPrefix(lines[1], "host-record="), ",")
	if len(fields) != 2 {
		return errors.New("CarPlay DNS 文件内容无效，拒绝清理")
	}
	address, err := netip.ParseAddr(fields[1])
	if err != nil {
		return errors.New("CarPlay DNS 文件地址无效，拒绝清理")
	}
	expected, err := carPlayDNSContent(lease, fields[0], address)
	// 升级恢复也允许精确的旧格式，但不能因此容忍额外 DNS 指令。
	legacy := fmt.Sprintf("# TeslaBox CarPlay lease %s\nhost-record=%s,%s\n", lease.RuntimeAPUUID, fields[0], address)
	if err != nil || (string(expected) != string(content) && string(content) != legacy) {
		return errors.New("CarPlay DNS 文件内容无效，拒绝清理")
	}
	if err = os.Remove(path); err != nil {
		return errors.New("清理 CarPlay DNS 文件失败")
	}
	return syncCarPlayDNSDirectory(m.carPlayDNSDirectory)
}
