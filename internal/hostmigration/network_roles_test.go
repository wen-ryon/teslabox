package hostmigration

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeRoleAssetsAndStartupContract(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, dir string }{
		{"teslabox-network-roles.service", "systemd"}, {"90-teslabox-8852be-roles.conf", "modprobe"}, {"90-teslabox-native-ap.rules", "udev"}, {"10-teslabox-native-wireless.link", "network"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			a, err := nativeRoleAssets.ReadFile("assets/native/" + tt.name)
			if err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(filepath.Join("../../deploy", tt.dir, tt.name))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(a, b) {
				t.Fatal("首装和 OTA 资产不一致")
			}
		})
	}
	unit, _ := nativeRoleAssets.ReadFile("assets/native/teslabox-network-roles.service")
	for _, part := range []string{"Before=NetworkManager.service network-pre.target", "ExecStartPre=/usr/bin/udevadm settle --timeout=15", "ExecStart=/usr/local/bin/teslabox network-roles-prepare", "WantedBy=NetworkManager.service"} {
		if !bytes.Contains(unit, []byte(part)) {
			t.Fatalf("缺少启动约束 %s", part)
		}
	}
	script, err := managedAPAssets.ReadFile("assets/teslabox-ap-setup.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(script)
	no := strings.Index(text, `nmcli device set "$AP_INTERFACE" autoconnect no`)
	yes := strings.Index(text, `nmcli device set "$AP_INTERFACE" managed yes`)
	if no < 0 || yes < no {
		t.Fatal("接管之前必须先关闭设备自动连接")
	}
	if !strings.Contains(text, `if [ -z "$NATIVE_AP_INTERFACE" ] && [ "$CONCURRENT_MODE"`) {
		t.Fatal("不能给原生双接口强行改 MAC")
	}
	if !strings.Contains(text, `if [ -n "$NATIVE_AP_INTERFACE" ]; then`) {
		t.Fatal("缺少原生双接口分支")
	}
}

func nativeTestPaths(t *testing.T) NativeRolePaths {
	t.Helper()
	dir := t.TempDir()
	return NativeRolePaths{filepath.Join(dir, "teslabox-network-roles.service"), filepath.Join(dir, "90-teslabox-8852be-roles.conf"), filepath.Join(dir, "90-teslabox-native-ap.rules"), filepath.Join(dir, "10-teslabox-native-wireless.link")}
}

func TestNativeRoleOTAOnlyStagesNextBoot(t *testing.T) {
	t.Parallel()
	paths := nativeTestPaths(t)
	runner := &recordingRunner{}
	changed, err := EnsureNativeRoleComponents(t.Context(), paths, runner)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	changed, err = EnsureNativeRoleComponents(t.Context(), paths, runner)
	if err != nil || changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	for _, call := range runner.calls {
		if strings.Contains(call, "restart") || strings.Contains(call, " start ") || strings.Contains(call, "modprobe") || strings.Contains(call, "trigger") {
			t.Fatalf("不能修改运行中的网卡: %s", call)
		}
	}
	if runner.calls[0] != "systemctl daemon-reload" || runner.calls[1] != "udevadm control --reload-rules" {
		t.Fatalf("意外操作 %v", runner.calls)
	}
}

func TestNativeRoleCustomComponentBlocksWholeStage(t *testing.T) {
	t.Parallel()
	paths := nativeTestPaths(t)
	runner := &recordingRunner{}
	if err := os.WriteFile(paths.Module, []byte("# 管理员配置\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureNativeRoleComponents(t.Context(), paths, runner); err == nil {
		t.Fatal("未拒绝自定义组件")
	}
	if _, err := os.Stat(paths.Unit); !os.IsNotExist(err) {
		t.Fatal("检查完成之前不能部分安装")
	}
	if len(runner.calls) > 0 {
		t.Fatal("冲突时不能启用启动服务")
	}
}
