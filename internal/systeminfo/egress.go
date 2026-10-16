package systeminfo

import (
	"net"
	"os"
	"path/filepath"
	"strings"
)

// InternetEgress 描述最近一次成功连通性探测实际使用的本机出口，不代表所有目标或所有地址族。
type InternetEgress struct {
	Interface string `json:"interface,omitempty"`
	Kind      string `json:"kind"`
	LocalIP   string `json:"local_ip,omitempty"`
	Family    string `json:"family,omitempty"`
}

type egressInterface struct {
	name      string
	addresses []net.Addr
}

func describeInternetEgress(address net.Addr) InternetEgress {
	interfaces, err := net.Interfaces()
	if err != nil {
		return InternetEgress{Kind: "unknown"}
	}
	var candidates []egressInterface
	for _, iface := range interfaces {
		addresses, err := iface.Addrs()
		if err != nil {
			// 清单不完整时不能把另一个具有相同地址的接口排除在外。
			return InternetEgress{Kind: "unknown"}
		}
		candidates = append(candidates, egressInterface{iface.Name, addresses})
	}
	result := matchEgressInterface(address, candidates)
	if result.Interface != "" {
		result.Kind = egressKind("/sys", result.Interface)
	}
	return result
}

func matchEgressInterface(address net.Addr, interfaces []egressInterface) InternetEgress {
	result := InternetEgress{Kind: "unknown"}
	local, ok := address.(*net.TCPAddr)
	if !ok || local == nil || local.IP.IsUnspecified() || local.IP.IsLoopback() || local.IP == nil {
		return result
	}
	result.LocalIP, result.Family = local.IP.String(), "ipv6"
	if local.IP.To4() != nil {
		result.Family = "ipv4"
	}
	for _, iface := range interfaces {
		matched := false
		for _, address := range iface.addresses {
			ip, _, err := net.ParseCIDR(address.String())
			if err == nil && ip.Equal(local.IP) {
				matched = true
				break
			}
		}
		if matched {
			if result.Interface != "" {
				result.Interface = ""
				return result
			}
			result.Interface = iface.name
		}
	}
	return result
}

func egressKind(sys, name string) string {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return "unknown"
	}
	base := filepath.Join(sys, "class/net", name)
	if _, err := os.Stat(filepath.Join(base, "phy80211")); err == nil {
		return "wifi"
	}
	if _, err := os.Stat(filepath.Join(base, "wireless")); err == nil {
		return "wifi"
	}
	driver, _ := os.Readlink(filepath.Join(base, "device/driver"))
	return classifyEgressDriver(filepath.Base(driver), name)
}

func classifyEgressDriver(driver, name string) string {
	switch driver {
	case "qmi_wwan", "cdc_mbim", "mhi_net", "mhi_wwan_mbim":
		return "cellular"
	case "rndis_host", "cdc_ether", "cdc_ncm", "ipheth":
		return "usb"
	case "bnep":
		return "bluetooth"
	case "rk_gmac-dwmac", "st_gmac", "r8125", "r8169", "r8152", "igb", "e1000e", "ax88179_178a", "asix":
		return "ethernet"
	}
	if strings.HasPrefix(name, "bnep") {
		return "bluetooth"
	}
	// 隧道、网桥和未识别驱动保留接口名，不凭 eth/wlan/wwan 名字猜网络来源。
	return "unknown"
}
