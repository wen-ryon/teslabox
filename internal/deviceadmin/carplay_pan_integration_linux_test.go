//go:build linux

package deviceadmin

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/internal/system"
)

func TestCarPlayPANMemoryProfileOnBoard(t *testing.T) {
	if os.Getenv("TESLABOX_CARPLAY_PAN_INTEGRATION") != "1" {
		t.Skip("需要显式选择测试板")
	}
	address, err := net.ParseMAC(os.Getenv("TESLABOX_CARPLAY_PAN_PHONE"))
	if err != nil || len(address) != 6 {
		t.Fatal("测试手机地址无效")
	}
	var identity [16]byte
	if _, err := rand.Read(identity[:]); err != nil {
		t.Fatal(err)
	}
	identity[6], identity[8] = identity[6]&15|64, identity[8]&63|128
	lease := testCarPlayLease()
	lease.RuntimePANUUID = fmt.Sprintf("%x-%x-%x-%x-%x", identity[:4], identity[4:6], identity[6:8], identity[8:10], identity[10:])
	lease.RuntimePANAddress = os.Getenv("TESLABOX_CARPLAY_PAN_PHONE")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	m := New(system.NewOSRunner(), nil)
	if err := createCarPlayPANProfile(ctx, lease.RuntimePANUUID, address); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := m.removeCarPlayPAN(cleanupCtx, lease); err != nil {
			t.Error(err)
		}
	})
	exists, err := m.carPlayPANExists(ctx, lease)
	if err != nil || !exists {
		t.Fatalf("实际 NetworkManager 不接受 PAN 配置: %v", err)
	}
	if os.Getenv("TESLABOX_CARPLAY_PAN_CONNECT") == "1" {
		// 实测候选保持网线为默认路由，只有绑定 BNEP 的请求走手机，避免中断调试通道。
		connectCtx, cancel := context.WithTimeout(context.Background(), 55*time.Second)
		defer cancel()
		if _, err := m.run(connectCtx, "nmcli", "connection", "modify", "uuid", lease.RuntimePANUUID, "ipv4.route-metric", "10000", "ipv4.dns-priority", "10000"); err != nil {
			t.Fatal("无法限定实测路由优先级")
		}
		if _, err := m.run(connectCtx, "nmcli", "--wait", "20", "connection", "up", "uuid", lease.RuntimePANUUID); err != nil {
			t.Fatal("手机 PAN 未建立，需要检查个人热点及连接状态")
		}
		name, err := activeCarPlayPANInterface(connectCtx, lease.RuntimePANUUID)
		if err != nil || name == "" {
			t.Fatal("PAN 未返回实际 IP 接口")
		}
		if !m.carPlayConnectionInternet(connectCtx, lease.RuntimePANUUID, name) {
			t.Fatal("PAN 接口 DNS 与可信 HTTPS 探测未通过")
		}
		t.Log("蓝牙供网实际 IP 接口与互联网已验证")
		if os.Getenv("TESLABOX_CARPLAY_PAN_WSS_PROBE") == "1" {
			body, err := m.run(connectCtx, "/var/lib/teslabox-carplay-experiment/carplay-proxy-probe-20261005-r12")
			var result struct {
				Passed bool `json:"passed"`
				Frames int  `json:"frames"`
			}
			if err != nil || json.Unmarshal(body, &result) != nil || !result.Passed || result.Frames < 3 {
				t.Fatal("PAN 已供网，但同时运行的 CarPlay WSS 验证未通过")
			}
			t.Log("PAN 连接保持期间收到真实手机 CarPlay WSS 视频")
		}
	}
}

func TestCarPlayInternetInterfaceBindingOnBoard(t *testing.T) {
	if os.Getenv("TESLABOX_CARPLAY_PAN_INTEGRATION") != "1" {
		t.Skip("需要显式选择测试板")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	m := New(system.NewOSRunner(), nil)
	if !m.carPlayConnectionInternet(ctx, os.Getenv("TESLABOX_CARPLAY_WIRED_UUID"), "eth0") {
		t.Fatal("真实有线上游探测失败")
	}
	if probeCarPlayInternet(ctx, "ap0", []string{"192.168.0.1"}) {
		t.Fatal("热点接口错误复用了有线默认路由")
	}
}

func TestCarPlayWiFiInternetBindingOnBoard(t *testing.T) {
	identity := os.Getenv("TESLABOX_CARPLAY_WIFI_UUID")
	if os.Getenv("TESLABOX_CARPLAY_PAN_INTEGRATION") != "1" || !validUUID(identity) {
		t.Skip("需要明确的测试 Wi-Fi 上游身份")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	m := New(system.NewOSRunner(), nil)
	if !m.carPlayConnectionInternet(ctx, identity, "wlan0") {
		t.Fatal("真实 Wi-Fi 上游的接口绑定联网探测失败")
	}
}

func TestCarPlayPANFallbackAndRecoveryOnBoard(t *testing.T) {
	if os.Getenv("TESLABOX_CARPLAY_PAN_FALLBACK") != "1" {
		t.Skip("需要显式选择受控回退测试")
	}
	phone := os.Getenv("TESLABOX_CARPLAY_PAN_PHONE")
	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Second)
	defer cancel()
	m := New(system.NewOSRunner(), nil)
	lease := testCarPlayLease()
	lease.ExpiresAt = time.Now().Add(5 * time.Minute)
	if err := m.RestoreCarPlayNetworkHold(lease); err != nil {
		t.Fatal(err)
	}
	var journal *CarPlayNetworkLease
	checkpoint := func(value *CarPlayNetworkLease) error {
		copy := *value
		journal = &copy
		return nil
	}
	m.carPlayPANProfileCreate = func(ctx context.Context, identity string, address []byte) error {
		if journal == nil || journal.RuntimePANUUID != identity {
			t.Fatal("实际创建前缺少恢复身份")
		}
		if err := createCarPlayPANProfile(ctx, identity, address); err != nil {
			return err
		}
		// 只降低诊断连接的优先级，真实管理网络保持可用。
		_, err := m.run(ctx, "nmcli", "connection", "modify", "uuid", identity, "ipv4.route-metric", "10000", "ipv4.dns-priority", "10000")
		return err
	}
	m.carPlayInternetProbe = func(ctx context.Context, name string, servers []string) bool {
		if !strings.HasPrefix(name, "bnep") {
			return false
		}
		return probeCarPlayInternet(ctx, name, servers)
	}
	t.Cleanup(func() {
		if journal != nil {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := m.removeCarPlayPAN(cleanupCtx, *journal); err != nil {
				t.Error(err)
			}
		}
	})
	state, err := m.EnsureCarPlayUpstream(ctx, lease.Owner, phone, checkpoint)
	if err != nil || state.Source != "bluetooth" || !state.Internet {
		t.Fatalf("受控回退未取得实际蓝牙互联网: %+v %v", state, err)
	}
	t.Log("受控上游失效后，正式管理器建立 PAN 并验证 BNEP 互联网")
	m.carPlayInternetProbe = probeCarPlayInternet
	state, err = m.EnsureCarPlayUpstream(ctx, lease.Owner, phone, checkpoint)
	if err != nil || state.Source != "existing_upstream" || !state.Internet {
		t.Fatalf("已有上游恢复后未撤回蓝牙供网: %+v %v", state, err)
	}
	if exists, err := m.carPlayPANExists(ctx, *journal); err != nil || exists {
		t.Fatal("恢复后遗留了 PAN 配置")
	}
	// 恢复记录仍可被新管理器认领，不会把已回收的内存配置当成外部连接。
	recovered := New(system.NewOSRunner(), nil)
	if err := recovered.RestoreCarPlayNetworkHold(*journal); err != nil {
		t.Fatal(err)
	}
	state, err = recovered.EnsureCarPlayUpstream(ctx, lease.Owner, phone, checkpoint)
	if err != nil || !state.Internet || state.Source != "existing_upstream" {
		t.Fatalf("恢复记录认领失败: %v", err)
	}
	t.Log("已有上游恢复与 PAN 清理通过，恢复记录可重新认领")
}
