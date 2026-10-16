//go:build linux

package deviceadmin

import (
	"os"
	"path/filepath"
)

func readEthernetDuplex(iface string) string {
	if !ethernetInterfaceValid(iface) {
		return ""
	}
	// 只读取当前协商结果，不读取或改写驱动参数；链路未建立时的 unknown/EIO 必须仍显示未知。
	content, err := os.ReadFile(filepath.Join("/sys/class/net", iface, "duplex"))
	if err != nil {
		return ""
	}
	return normalizeEthernetDuplex(string(content))
}
