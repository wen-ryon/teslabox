// Package wireless 建立无线 CarPlay 的数据通道。
//
// 无线 CarPlay 的建立比有线长得多，大致是：
//
//  1. 蓝牙配对：iPhone 发现车机并完成配对（车机要声明支持 iAP2 的蓝牙服务）
//  2. iPhone 通过蓝牙 RFCOMM 连上 iAP2 控制通道
//  3. 在这个通道上跑 Identification / Authentication（与有线共用流程）
//  4. iPhone 通过 WiFi 共享消息把热点凭据交给车机，或者让车机起 AP
//  5. 双方切到 WiFi，视频流改走 TCP（AirPlay 那套），蓝牙通道只留控制
//
// 本包负责 1、2、4 三步的环境准备与通道建立，协议部分仍交给 carplay.Host。
package wireless

import (
	"errors"
	"fmt"
	"net"
	"os/exec"
	"strings"
	"time"
)

// ErrNotPaired 表示 iPhone 还没完成蓝牙配对。
var ErrNotPaired = errors.New("wireless: iPhone 尚未完成蓝牙配对")

// BluetoothRadio 抽象蓝牙适配器的操作，便于换成 BlueZ D-Bus 或命令行实现。
type BluetoothRadio interface {
	// Address 返回适配器 MAC 地址，Identification 消息里要上报给 iPhone。
	Address() (string, error)
	// MakeDiscoverable 让 iPhone 能搜到本机。
	MakeDiscoverable(timeout time.Duration) error
	// PairedDevices 返回已配对设备的 MAC 列表。
	PairedDevices() ([]string, error)
	// Listen 在指定 RFCOMM 通道上监听，返回接受连接的监听器。
	Listen(channel uint8) (net.Listener, error)
}

// AccessPoint 抽象 WiFi 热点控制。
type AccessPoint interface {
	// Start 启动热点，返回 SSID 与密码。
	Start(ssid, passphrase string) error
	// Stop 关闭热点。
	Stop() error
	// Join 作为客户端加入 iPhone 共享的热点。
	Join(ssid, passphrase string) error
}

// Setup 描述无线 CarPlay 的环境参数。
type Setup struct {
	Radio BluetoothRadio
	AP    AccessPoint
	// RFCOMMChannel 是 iAP2 控制通道的通道号，iPhone 按服务发现结果连接。
	RFCOMMChannel uint8
	// OwnSSID / OwnPassphrase 是本机作为热点时使用的凭据。
	OwnSSID       string
	OwnPassphrase string
}

// Prepare 完成无线连接的环境准备：开启可发现、等待配对、拉起 RFCOMM 监听。
//
// 返回的监听器上收到的第一条连接就是 iPhone 的 iAP2 控制通道。
func Prepare(s Setup) (net.Listener, error) {
	if s.Radio == nil {
		return nil, errors.New("wireless: 缺少蓝牙适配器")
	}
	channel := s.RFCOMMChannel
	if channel == 0 {
		channel = DefaultRFCOMMChannel
	}
	// iPhone 只有在配件可发现时才会发起配对。
	if err := s.Radio.MakeDiscoverable(PairingWindow); err != nil {
		return nil, fmt.Errorf("wireless: 开启蓝牙可发现失败: %w", err)
	}
	return s.Radio.Listen(channel)
}

// DefaultRFCOMMChannel 是 iAP2 控制通道的默认 RFCOMM 通道号。
const DefaultRFCOMMChannel = 1

// PairingWindow 是可发现的持续时间。
const PairingWindow = 2 * time.Minute

// WaitForPhone 阻塞等待 iPhone 配对完成，返回其 MAC 地址。
func WaitForPhone(radio BluetoothRadio, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		devices, err := radio.PairedDevices()
		if err == nil {
			for _, mac := range devices {
				// iPhone 的 MAC 前三个字节属于 Apple 的 OUI 段。
				if isAppleMAC(mac) {
					return mac, nil
				}
			}
		}
		if !time.Now().Before(deadline) {
			return "", ErrNotPaired
		}
		time.Sleep(time.Second)
	}
}

// appleOUIs 是 Apple 分配到的 MAC 前缀（前三个字节）。
var appleOUIs = map[string]bool{
	"f0:99:bf": true, "f0:c1:f1": true, "f0:d1:a9": true, "f0:db:e2": true,
	"f0:dc:e2": true, "f0:f6:1c": true, "00:1c:b3": true, "00:1e:c2": true,
	"00:21:e9": true, "00:23:12": true, "00:23:32": true, "00:25:00": true,
	"00:25:bc": true, "00:26:08": true, "00:26:4a": true, "00:26:b0": true,
	"00:26:bb": true, "00:3e:e1": true, "00:56:cd": true, "00:61:71": true,
	"00:69:7c": true, "00:88:65": true, "04:0c:ce": true, "04:15:52": true,
	"04:1e:64": true, "04:26:65": true, "04:4b:ed": true, "04:52:f3": true,
	"04:69:f8": true, "04:d3:cf": true, "04:db:56": true, "04:e5:36": true,
	"04:f1:3e": true, "04:f7:e4": true, "08:66:98": true, "08:70:45": true,
	"08:74:02": true, "08:f4:ab": true, "0c:15:39": true, "0c:29:24": true,
	"0c:3e:9f": true, "0c:74:c2": true, "0c:bc:9f": true, "10:1c:0c": true,
	"10:40:f3": true, "10:93:e9": true, "10:9a:dd": true, "10:dd:b1": true,
	"14:5a:05": true, "14:8f:c6": true, "14:99:e2": true, "18:20:32": true,
	"18:34:51": true, "18:65:90": true, "18:9e:fc": true, "18:af:61": true,
	"18:e7:f4": true, "18:f6:43": true, "1c:1a:c0": true, "1c:5c:f2": true,
	"1c:91:48": true, "1c:ab:a7": true, "1c:e6:2b": true, "20:3c:ae": true,
	"20:76:8f": true, "20:7d:74": true, "20:a2:e4": true, "20:ab:37": true,
	"20:c9:d0": true, "24:1e:eb": true, "24:5b:e7": true, "24:a0:74": true,
	"24:ab:81": true, "24:e3:14": true, "28:0b:5c": true, "28:37:37": true,
	"28:5a:eb": true, "28:6a:b8": true, "28:6a:ba": true, "28:cf:da": true,
	"28:cf:e9": true, "28:e0:2c": true, "28:e7:cf": true, "28:ed:6a": true,
	"28:f0:76": true, "2c:1f:23": true, "2c:33:61": true, "2c:54:cf": true,
	"2c:61:f6": true, "2c:b4:3a": true, "2c:be:08": true, "30:10:e4": true,
	"30:35:ad": true, "30:63:6b": true, "30:90:ab": true, "30:f7:c5": true,
	"34:12:98": true, "34:15:9e": true, "34:36:3b": true, "34:51:c9": true,
	"34:a3:95": true, "34:ab:37": true, "34:c0:59": true, "38:0f:4a": true,
	"38:48:4c": true, "38:71:de": true, "38:86:e8": true, "38:b5:4d": true,
	"38:c9:86": true, "38:ca:da": true, "38:f9:d3": true, "3c:07:54": true,
	"3c:15:c2": true, "3c:2e:f9": true, "3c:ab:8e": true, "3c:d0:f8": true,
	"3c:e0:72": true, "40:30:04": true, "40:33:1a": true, "40:3c:fc": true,
	"40:6c:8f": true, "40:98:ad": true, "40:a6:d9": true, "40:b3:95": true,
	"40:d3:2d": true, "44:00:10": true, "44:2a:60": true, "44:4c:0c": true,
	"44:d8:84": true, "44:fb:42": true, "48:1f:77": true, "48:43:7c": true,
	"48:60:bc": true, "48:74:6e": true, "48:a1:95": true, "48:d7:05": true,
	"48:e1:e9": true, "4c:32:75": true, "4c:57:ca": true, "4c:7c:5f": true,
	"4c:8d:79": true, "4c:b1:99": true, "50:32:37": true, "50:7a:55": true,
	"50:8f:4c": true, "50:c8:e5": true, "50:ea:d6": true, "54:26:96": true,
	"54:4e:90": true, "54:72:4f": true, "54:9f:13": true, "54:ae:27": true,
	"54:e4:3a": true, "54:ea:a8": true, "58:1f:aa": true, "58:40:4e": true,
	"58:55:ca": true, "58:7f:57": true, "58:b0:35": true, "58:e2:8f": true,
	"5c:59:48": true, "5c:8d:4e": true, "5c:95:ae": true, "5c:97:f3": true,
	"5c:ad:cf": true, "5c:f5:da": true, "5c:f9:38": true, "60:03:08": true,
	"60:30:d4": true, "60:69:44": true, "60:92:17": true, "60:a3:7d": true,
	"60:c5:47": true, "60:d9:c7": true, "60:fa:cd": true, "60:fb:42": true,
	"64:20:0c": true, "64:5a:ed": true, "64:76:ba": true, "64:a3:cb": true,
	"64:b9:e8": true, "64:e6:82": true, "68:09:27": true, "68:5b:35": true,
	"68:96:7b": true, "68:9c:5e": true, "68:ae:20": true, "68:d9:3c": true,
	"68:db:ca": true, "68:fe:f7": true, "6c:3e:6d": true, "6c:40:08": true,
	"6c:70:9f": true, "6c:72:e7": true, "6c:8d:c1": true, "6c:94:f8": true,
	"6c:96:cf": true, "6c:ab:31": true, "6c:c2:6b": true, "70:11:24": true,
	"70:14:a6": true, "70:3e:ac": true, "70:56:81": true, "70:70:0d": true,
	"70:73:cb": true, "70:81:eb": true, "70:de:e2": true, "70:ec:e4": true,
	"70:f0:87": true, "74:1b:b2": true, "74:81:14": true, "74:8d:08": true,
	"74:e2:f5": true, "78:31:c1": true, "78:3a:84": true, "78:4f:43": true,
	"78:67:d1": true, "78:6c:1c": true, "78:7b:8a": true, "78:9f:70": true,
	"78:a3:e4": true, "78:ca:39": true, "78:fd:94": true, "7c:04:d0": true,
	"7c:11:be": true, "7c:2a:31": true, "7c:6d:62": true, "7c:c5:37": true,
	"7c:c7:09": true, "7c:d1:c3": true, "7c:f0:5f": true, "7c:fa:df": true,
	"80:00:6e": true, "80:49:71": true, "80:5a:04": true, "80:92:9f": true,
	"80:b0:3d": true, "80:be:06": true, "80:d0:9b": true, "80:e6:50": true,
	"80:ea:96": true, "84:29:99": true, "84:38:35": true, "84:4b:f5": true,
	"84:68:3a": true, "84:85:d5": true, "84:8e:0c": true, "84:b1:53": true,
	"84:fc:fe": true, "88:1f:a1": true, "88:53:95": true, "88:63:df": true,
	"88:66:a5": true, "88:c6:63": true, "88:cb:87": true, "88:e8:7d": true,
	"8c:00:6d": true, "8c:29:37": true, "8c:2d:aa": true, "8c:58:77": true,
	"8c:7b:9d": true, "8c:85:90": true, "8c:8e:f2": true, "8c:fa:ba": true,
	"90:27:e4": true, "90:60:f1": true, "90:72:40": true, "90:84:0d": true,
	"90:8d:6c": true, "90:b0:ed": true, "90:b2:1f": true, "90:c1:c6": true,
	"90:dd:5d": true, "94:94:26": true, "94:bf:2d": true, "94:f6:a3": true,
	"98:01:a7": true, "98:0c:82": true, "98:1f:29": true, "98:5a:eb": true,
	"98:b8:e3": true, "98:d6:bb": true, "98:e0:d9": true, "98:f0:ab": true,
	"98:fe:94": true, "9c:04:eb": true, "9c:20:7b": true, "9c:29:3f": true,
	"9c:35:5e": true, "9c:4f:da": true, "9c:5b:94": true, "9c:84:bf": true,
	"9c:f3:87": true, "9c:fc:01": true, "a0:18:28": true, "a0:3b:e3": true,
	"a0:5b:78": true, "a0:99:9b": true, "a0:ed:cd": true, "a4:31:35": true,
	"a4:5e:60": true, "a4:67:06": true, "a4:83:e7": true, "a4:b1:97": true,
	"a4:b8:05": true, "a4:c3:61": true, "a4:d1:8c": true, "a4:d9:c0": true,
	"a8:20:66": true, "a8:51:5b": true, "a8:5b:78": true, "a8:66:7f": true,
	"a8:88:08": true, "a8:96:8a": true, "a8:bb:cf": true, "a8:fa:d8": true,
	"ac:29:3a": true, "ac:3c:0b": true, "ac:7f:3e": true, "ac:87:a3": true,
	"ac:bc:32": true, "ac:cf:5c": true, "ac:de:48": true, "ac:fd:ec": true,
	"b0:34:95": true, "b0:65:bd": true, "b0:9f:ba": true, "b0:ac:25": true,
	"b4:18:d1": true, "b4:8b:19": true, "b4:f0:ab": true, "b8:09:8a": true,
	"b8:17:c2": true, "b8:41:a4": true, "b8:53:ac": true, "b8:78:d5": true,
	"b8:8d:12": true, "b8:c7:5d": true, "b8:e8:56": true, "b8:f6:b1": true,
	"bc:3b:af": true, "bc:4c:c4": true, "bc:52:b7": true, "bc:54:36": true,
	"bc:67:78": true, "bc:6c:21": true, "bc:92:6b": true, "bc:ec:5d": true,
	"c0:1a:da": true, "c0:63:94": true, "c0:84:7a": true, "c0:9f:42": true,
	"c0:a8:bd": true, "c0:cc:f8": true, "c0:f2:fb": true, "c4:2c:03": true,
	"c4:84:66": true, "c4:8e:8f": true, "c4:b3:01": true, "c8:1f:e9": true,
	"c8:2a:14": true, "c8:33:4b": true, "c8:3c:85": true, "c8:69:cd": true,
	"c8:6f:1d": true, "c8:85:50": true, "c8:bc:c8": true, "c8:d0:83": true,
	"c8:e0:eb": true, "cc:08:8d": true, "cc:20:e8": true, "cc:25:ef": true,
	"cc:29:f5": true, "cc:44:63": true, "cc:78:5f": true, "cc:9e:a2": true,
	"cc:c7:60": true, "d0:03:4b": true, "d0:23:db": true, "d0:33:11": true,
	"d0:4f:7e": true, "d0:81:7a": true, "d0:a6:37": true, "d0:c5:f3": true,
	"d0:d2:b0": true, "d0:e1:40": true, "d4:61:9d": true, "d4:9a:20": true,
	"d4:a3:3d": true, "d4:f4:6f": true, "d8:00:4d": true, "d8:1d:72": true,
	"d8:30:62": true, "d8:8f:76": true, "d8:96:95": true, "d8:a2:5e": true,
	"d8:bb:2c": true, "d8:cf:9c": true, "d8:d1:cb": true, "dc:08:cf": true,
	"dc:2b:2a": true, "dc:37:14": true, "dc:41:5f": true, "dc:56:e7": true,
	"dc:86:d8": true, "dc:9b:9c": true, "dc:a4:ca": true, "dc:b4:c4": true,
	"e0:66:78": true, "e0:98:06": true, "e0:ac:cb": true, "e0:b5:2d": true,
	"e0:b9:ba": true, "e0:c7:67": true, "e0:f5:c6": true, "e0:f8:47": true,
	"e4:2b:34": true, "e4:35:a8": true, "e4:64:9f": true, "e4:8b:7f": true,
	"e4:98:d6": true, "e4:9a:79": true, "e4:b2:1b": true, "e4:c6:3d": true,
	"e4:ce:8f": true, "e4:e0:a6": true, "e8:06:88": true, "e8:2a:ea": true,
	"e8:4e:84": true, "e8:80:2e": true, "e8:b2:09": true, "e8:ba:70": true,
	"ec:35:86": true, "ec:85:2f": true, "ec:ad:b8": true, "f0:18:98": true,
	"f0:24:75": true, "f0:79:60": true, "f0:7b:7a": true, "f0:98:9d": true,
	"f0:9f:c2": true, "f0:b0:14": true, "f0:b2:f1": true, "f0:cb:a1": true,
	"f0:d8:48": true, "f4:04:69": true, "f4:1a:a1": true, "f4:31:c3": true,
	"f4:37:b7": true, "f4:5c:89": true, "f4:62:b4": true, "f4:f1:5a": true,
	"f4:f9:51": true, "f8:03:77": true, "f8:1e:df": true, "f8:27:93": true,
	"f8:38:80": true, "f8:6f:b1": true, "f8:95:c7": true, "f8:9e:94": true,
	"fc:25:3f": true, "fc:e9:98": true, "fc:fc:48": true,
}

// isAppleMAC 判断一个 MAC 是否属于 Apple 设备。
//
// 注意 iPhone 默认启用随机 MAC（私有地址），配对后拿到的可能是随机地址，
// 所以这个函数只能作为兜底，不能当作唯一判据。
func isAppleMAC(mac string) bool {
	mac = strings.ToLower(strings.TrimSpace(mac))
	parts := strings.Split(mac, ":")
	if len(parts) != 6 {
		return false
	}
	return appleOUIs[strings.Join(parts[:3], ":")]
}

// HostapdAccessPoint 用 hostapd 拉起本机热点。
type HostapdAccessPoint struct {
	Interface  string
	ConfigPath string
}

// Start 启动热点。
func (h HostapdAccessPoint) Start(ssid, passphrase string) error {
	if h.Interface == "" {
		return errors.New("wireless: 未指定无线接口")
	}
	config := h.ConfigPath
	if config == "" {
		config = "/etc/hostapd/carplay.conf"
	}
	// 实际部署时配置文件由部署脚本生成，这里只负责拉起。
	return exec.Command("hostapd", "-B", "-i", h.Interface, config).Run()
}

// Stop 关闭热点。
func (h HostapdAccessPoint) Stop() error {
	return exec.Command("pkill", "-f", "hostapd.*"+h.Interface).Run()
}

// Join 作为客户端加入 iPhone 的热点。
func (h HostapdAccessPoint) Join(ssid, passphrase string) error {
	// 通过 wpa_supplicant 的控制接口加入网络。
	return exec.Command("wpa_cli", "-i", h.Interface, "add_network").Run()
}
