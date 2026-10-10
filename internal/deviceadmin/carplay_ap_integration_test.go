package deviceadmin

import (
	"context"
	"crypto/rand"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/internal/system"
)

// TestCarPlayAPProfileNetworkManagerIntegration 仅显式创建不激活的内存配置，验证真实 D-Bus 属性契约。
func TestCarPlayAPProfileNetworkManagerIntegration(t *testing.T) {
	if os.Getenv("TESLABOX_CARPLAY_PROFILE_CHECK") != "1" {
		t.Skip("需要显式开启真实 NetworkManager 内存配置检查")
	}
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Fatal("真实网络配置检查只允许 Linux root")
	}
	template := os.Getenv("TESLABOX_CARPLAY_AP_TEMPLATE")
	interfaceName := os.Getenv("TESLABOX_CARPLAY_AP_INTERFACE")
	if !validUUID(template) || !carPlayInterfacePattern.MatchString(interfaceName) {
		t.Fatal("需要明确提供模板身份和热点接口")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	manager := New(system.NewOSRunner(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	before, err := manager.run(ctx, "nmcli", "-t", "-f", "UUID,DEVICE", "connection", "show", "--active")
	if err != nil {
		t.Fatal("无法获取原活动连接")
	}
	profile, err := manager.run(ctx, "nmcli", "--escape", "no", "--show-secrets", "-g", "802-11-wireless.ssid,802-11-wireless-security.psk", "connection", "show", "uuid", template)
	if err != nil {
		t.Fatal("无法读取指定模板")
	}
	credentials := strings.Split(strings.TrimSuffix(string(profile), "\n"), "\n")
	if len(credentials) != 2 || validateSSID(credentials[0]) != nil || validatePassword(credentials[1]) != nil {
		t.Fatal("模板凭据无效")
	}
	mac, err := manager.run(ctx, "nmcli", "--escape", "no", "-g", "GENERAL.HWADDR", "device", "show", interfaceName)
	if err != nil || !validBSSID(strings.TrimSpace(string(mac))) {
		t.Fatal("热点接口地址无效")
	}
	var identity [16]byte
	if _, err := rand.Read(identity[:]); err != nil {
		t.Fatal(err)
	}
	identity[6], identity[8] = identity[6]&15|64, identity[8]&63|128
	uuid := fmt.Sprintf("%x-%x-%x-%x-%x", identity[:4], identity[4:6], identity[6:8], identity[8:10], identity[10:])
	lease := CarPlayNetworkLease{Owner: "profile-contract-check", ExpiresAt: time.Now().Add(time.Minute), RuntimeAPUUID: uuid,
		RuntimeAPName: "TeslaBox-CarPlay-lease-" + uuid, Plan: CarPlayNetworkPlan{APUUID: template, APInterface: interfaceName, Channel: 149, ConcurrencyMode: "independent_radio"}}
	// 这里只借用身份校验与回收函数，不申请租约，不暂停上游，也不启动热点。
	if err := manager.RestoreCarPlayNetworkHold(lease); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cleanupCancel()
		if err := manager.ReleaseCarPlayNetwork(cleanupCtx, lease.Owner, func(*CarPlayNetworkLease) error { return nil }); err != nil {
			t.Errorf("内存配置清理失败，身份=%s", uuid)
		}
	}()
	if err := createCarPlayAPProfile(ctx, lease, strings.TrimSpace(string(mac)), credentials[0], credentials[1]); err != nil {
		t.Fatal(err)
	}
	properties, err := manager.run(ctx, "nmcli", "-g", "connection.uuid,connection.id,connection.interface-name,connection.autoconnect,802-11-wireless.channel", "connection", "show", "uuid", uuid)
	if err != nil || strings.TrimSpace(string(properties)) != uuid+"\n"+lease.RuntimeAPName+"\n"+interfaceName+"\nno\n149" {
		t.Fatal("内存配置身份、自动连接或信道不符合约定")
	}
	after, err := manager.run(ctx, "nmcli", "-t", "-f", "UUID,DEVICE", "connection", "show", "--active")
	if err != nil || string(before) != string(after) {
		t.Fatal("不激活的配置检查改变了活动连接")
	}
	t.Log("真实 D-Bus 配置创建成功；活动连接未变，退出只清理本次身份")
}
