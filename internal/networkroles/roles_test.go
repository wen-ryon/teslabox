package networkroles

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var testPair = Pair{AP: "ap0", Station: "sta0", APMAC: "50:c2:e8:11:22:33", StationMAC: "52:c2:e8:11:22:33"}

func TestManagerStoppedRejectsStartingOrUnknown(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct {
		state string
		err   error
		want  bool
	}{
		{"active", nil, false}, {"activating", nil, false}, {"reloading", nil, false}, {"deactivating", nil, false}, {"", nil, false}, {"inactive", nil, true}, {"failed\n", nil, true}, {"inactive", errors.New("systemd unavailable"), false},
	} {
		t.Run(tt.state+fmt.Sprint(tt.err != nil), func(t *testing.T) {
			if managerStopped(tt.state, tt.err) != tt.want {
				t.Fatal("未确认停止时不得迁移连接")
			}
		})
	}
}

func TestManualNetworkManagerProcessAlsoBlocksMigration(t *testing.T) {
	root := t.TempDir()
	writeTest(t, filepath.Join(root, "123/comm"), "unrelated\n")
	if running, err := networkManagerProcessRunning(root); err != nil || running {
		t.Fatalf("running=%v err=%v", running, err)
	}
	writeTest(t, filepath.Join(root, "124/comm"), "NetworkManager\n")
	if running, err := networkManagerProcessRunning(root); err != nil || !running {
		t.Fatal("手动启动的 NM 不能漏过")
	}
	if _, err := networkManagerProcessRunning(filepath.Join(root, "missing")); err == nil {
		t.Fatal("进程清单不可读时不能认定已停止")
	}
}

func testProfile(mode, binding string) string {
	name := "WendeWiFi_5G"
	ssid := name
	if mode == "ap" {
		name = "TeslaBox-5G"
		ssid = name
	}
	return "# 保留注释\n[connection]\nid=" + name + "\nuuid=12345678-1234-4234-8234-123456789012\ntype=wifi\ninterface-name=" + binding + "\nautoconnect=true\n[wifi]\nmode=" + mode + "\nssid=" + ssid + "\n[wifi-security]\nkey-mgmt=sae\npsk=do-not-print-or-replace\n"
}

func TestMigrateProfile(t *testing.T) {
	t.Parallel()
	for _, tt := range []struct{ name, mode, binding, want string }{
		{"legacy_ap", "ap", "wlan0", "ap0"}, {"unbound_sta", "infrastructure", "", "sta0"}, {"wrong_sta_port", "infrastructure", "ap0", "sta0"}, {"old_secondary", "infrastructure", "wlan1", "sta0"}, {"explicit_other", "infrastructure", "wlp3s0", "wlp3s0"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			before := testProfile(tt.mode, tt.binding)
			after, err := MigrateProfile(before, testPair, "TeslaBox-5G")
			if err != nil {
				t.Fatal(err)
			}
			k, err := parseKeyfile(after)
			if err != nil {
				t.Fatal(err)
			}
			if k.get("connection", "interface-name") != tt.want {
				t.Fatalf("角色绑定错误 %s", tt.name)
			}
			for _, line := range []string{"# 保留注释", "uuid=12345678-1234-4234-8234-123456789012", "psk=do-not-print-or-replace", "key-mgmt=sae"} {
				if !strings.Contains(after, line) {
					t.Fatalf("破坏原字段 %s", strings.Split(line, "=")[0])
				}
			}
			again, err := MigrateProfile(after, testPair, "TeslaBox-5G")
			if err != nil || again != after {
				t.Fatal("迁移不幂等")
			}
		})
	}
}

func TestMigrateOwnSSIDAndOtherHardware(t *testing.T) {
	text := strings.ReplaceAll(testProfile("infrastructure", ""), "WendeWiFi_5G", "TeslaBox-5G")
	after, err := MigrateProfile(text, testPair, "TeslaBox-5G")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(after, "autoconnect=false") {
		t.Fatal("不能自动连接自己的热点")
	}
	other := strings.Replace(testProfile("infrastructure", ""), "[wifi]\n", "[wifi]\nmac-address=aa:bb:cc:dd:ee:ff\n", 1)
	after, err = MigrateProfile(other, testPair, "TeslaBox-5G")
	if err != nil || after != other {
		t.Fatal("误改其他硬件绑定")
	}
	if _, err = MigrateProfile(text+"[connection]\nid=other\n", testPair, "TeslaBox-5G"); err == nil {
		t.Fatal("重复段必须拒绝")
	}
	if _, err = MigrateProfile(strings.Replace(text, "type=wifi", "type=wifi\ntype=wifi", 1), testPair, "TeslaBox-5G"); err == nil {
		t.Fatal("重复键必须拒绝")
	}
	legacy := strings.ReplaceAll(testProfile("ap", "wlan0"), "type=wifi", "type=802-11-wireless")
	if _, err = MigrateProfile(legacy, testPair, "TeslaBox-5G"); err != nil {
		t.Fatal(err)
	}
}

func writeTest(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}
func linkTest(t *testing.T, target, path string) {
	t.Helper()
	// 普通文件只用于模拟内核提供的拓扑关系，不要求 Windows 符号链接权限。
	writeTest(t, path+".link", target)
}

func resolveTest(path string) (string, error) {
	text, err := os.ReadFile(path + ".link")
	return string(text), err
}
func detectTest(sys string) (*Pair, error) { return detect(sys, resolveTest) }
func prepareTest(p Paths) error            { return prepare(p, detectTest) }

func fixture(t *testing.T) Paths {
	t.Helper()
	root := t.TempDir()
	p := Paths{filepath.Join(root, "sys"), filepath.Join(root, "profiles"), filepath.Join(root, "state"), filepath.Join(root, "run/roles.env"), filepath.Join(root, "run/nm.conf")}
	module := filepath.Join(p.Sys, "module/8852be")
	if err := os.MkdirAll(module, 0o700); err != nil {
		t.Fatal(err)
	}
	phy := filepath.Join(p.Sys, "phy0")
	if err := os.MkdirAll(phy, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, mac := range map[string]string{testPair.AP: testPair.APMAC, testPair.Station: testPair.StationMAC} {
		base := filepath.Join(p.Sys, "class/net", name)
		writeTest(t, filepath.Join(base, "address"), mac)
		linkTest(t, phy, filepath.Join(base, "phy80211"))
		linkTest(t, module, filepath.Join(base, "device/driver/module"))
	}
	writeTest(t, filepath.Join(module, "parameters/ifname"), "ap%d\n")
	writeTest(t, filepath.Join(module, "parameters/if2name"), "sta%d\n")
	writeTest(t, filepath.Join(p.Profiles, "ap.nmconnection"), testProfile("ap", "wlan0"))
	writeTest(t, filepath.Join(p.Profiles, "sta"), testProfile("infrastructure", ""))
	return p
}

func TestPrepareIgnoresNMTemporaryFilesButRejectsRealDuplicate(t *testing.T) {
	for _, tc := range []struct {
		name    string
		ignored bool
	}{
		{"ap.nmconnection.ZL33V3", true},
		{"ap.nmconnection~", true},
		{"credentials.pem", true},
		{"second-ap.nmconnection", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := fixture(t)
			path := filepath.Join(p.Profiles, tc.name)
			before := testProfile("ap", "wlan0")
			writeTest(t, path, before)
			err := prepareTest(p)
			if tc.ignored && err != nil {
				t.Fatal(err)
			}
			if !tc.ignored && err == nil {
				t.Fatal("真实重复 AP 模板必须拒绝迁移")
			}
			data, _ := os.ReadFile(path)
			if string(data) != before {
				t.Fatal("不得修改被忽略或冲突的配置")
			}
		})
	}
}

func TestPrepareColdBootIdempotenceAndRollback(t *testing.T) {
	p := fixture(t)
	before, err := os.ReadFile(filepath.Join(p.Profiles, "sta"))
	if err != nil {
		t.Fatal(err)
	}
	if err = prepareTest(p); err != nil {
		t.Fatal(err)
	}
	sta, _ := os.ReadFile(filepath.Join(p.Profiles, "sta"))
	ap, _ := os.ReadFile(filepath.Join(p.Profiles, "ap.nmconnection"))
	if !bytes.Contains(sta, []byte("interface-name=sta0")) || !bytes.Contains(ap, []byte("interface-name=ap0")) {
		t.Fatal("未完成角色迁移")
	}
	first, _ := os.ReadFile(filepath.Join(p.State, "transaction.json"))
	if err = prepareTest(p); err != nil {
		t.Fatal(err)
	}
	again, _ := os.ReadFile(filepath.Join(p.State, "transaction.json"))
	if !bytes.Equal(first, again) {
		t.Fatal("幂等执行覆盖回退基线")
	}
	// 模拟新一轮启动的 /run 清空；必须仍能恢复首次迁移前的原始配置。
	if err = os.Remove(p.Runtime); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(p.NMConfig); err != nil {
		t.Fatal(err)
	}
	if err = prepareTest(p); err != nil {
		t.Fatal(err)
	}
	if err = Rollback(p); err != nil {
		t.Fatal(err)
	}
	restored, _ := os.ReadFile(filepath.Join(p.Profiles, "sta"))
	if !bytes.Equal(before, restored) {
		t.Fatal("跨冷启动丢失原始配置回退基线")
	}
}

func TestPrepareInterruptedAndAdminConflict(t *testing.T) {
	p := fixture(t)
	path := filepath.Join(p.Profiles, "sta")
	before, _, mode, err := regularRead(path)
	if err != nil {
		t.Fatal(err)
	}
	after, err := MigrateProfile(string(before), testPair, "TeslaBox-5G")
	if err != nil {
		t.Fatal(err)
	}
	pending := transaction{Phase: "prepared", Changes: []change{{path, before, true, []byte(after), uint32(mode)}}}
	if err = journal(p, pending); err != nil {
		t.Fatal(err)
	}
	writeTest(t, path, after)
	if err = prepareTest(p); err != nil {
		t.Fatal(err)
	}
	writeTest(t, path, after+"# 管理员随后更新\n")
	if err = Rollback(p); err == nil {
		t.Fatal("不得覆盖管理员新修改")
	}
	data, _ := os.ReadFile(path)
	if !bytes.Contains(data, []byte("管理员随后更新")) {
		t.Fatal("管理员修改丢失")
	}
}

func TestDetectRejectsUnsafeTopologies(t *testing.T) {
	for _, name := range []string{"old_parameters", "duplicate_mac", "multiple_phy", "third_vif"} {
		t.Run(name, func(t *testing.T) {
			p := fixture(t)
			switch name {
			case "old_parameters":
				writeTest(t, filepath.Join(p.Sys, "module/8852be/parameters/ifname"), "wlan%d")
			case "duplicate_mac":
				writeTest(t, filepath.Join(p.Sys, "class/net/sta0/address"), testPair.APMAC)
			case "multiple_phy":
				phy := filepath.Join(p.Sys, "phy1")
				os.MkdirAll(phy, 0o700)
				linkTest(t, phy, filepath.Join(p.Sys, "class/net/mt0/phy80211"))
			case "third_vif":
				linkTest(t, filepath.Join(p.Sys, "phy0"), filepath.Join(p.Sys, "class/net/wlan2/phy80211"))
				linkTest(t, filepath.Join(p.Sys, "module/8852be"), filepath.Join(p.Sys, "class/net/wlan2/device/driver/module"))
			}
			if _, err := detectTest(p.Sys); err == nil {
				t.Fatal("未拒绝不确定的接口角色")
			}
			before, _ := os.ReadFile(filepath.Join(p.Profiles, "ap.nmconnection"))
			if err := prepareTest(p); err == nil {
				t.Fatal("不能迁移不明确角色")
			}
			after, _ := os.ReadFile(filepath.Join(p.Profiles, "ap.nmconnection"))
			if !bytes.Equal(before, after) {
				t.Fatal("拒绝之前修改了配置")
			}
		})
	}
}

func TestMT7922AndCustomRulesUntouched(t *testing.T) {
	root := t.TempDir()
	p := Paths{filepath.Join(root, "sys"), filepath.Join(root, "profiles"), filepath.Join(root, "state"), filepath.Join(root, "runtime"), filepath.Join(root, "conf")}
	os.MkdirAll(filepath.Join(p.Sys, "class/net"), 0o700)
	writeTest(t, filepath.Join(p.Profiles, "mt.nmconnection"), testProfile("ap", "ap0"))
	writeTest(t, p.NMConfig, "# 管理员自己的规则\n")
	if err := Prepare(p); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(p.NMConfig)
	if string(data) != "# 管理员自己的规则\n" {
		t.Fatal("覆盖管理员规则")
	}
	data, _ = os.ReadFile(filepath.Join(p.Profiles, "mt.nmconnection"))
	if string(data) != testProfile("ap", "ap0") {
		t.Fatal("MT7922 配置不应走 Realtek 迁移")
	}
}
