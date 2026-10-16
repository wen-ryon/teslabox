//go:build integration

package hostmigration

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNativeAPScriptDoesNotCreateThirdInterfaceOrChangeMAC(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if runtime.GOOS == "windows" {
		sh = `D:\msys64\usr\bin\sh.exe`
		_, err = os.Stat(sh)
	}
	if err != nil {
		t.Skip("本机没有 POSIX shell，脚本执行测试需要 Linux 或 MSYS2")
	}
	for _, tt := range []struct {
		name, auto string
		envOK      bool
		wantUp     bool
	}{
		{"auto_start", "yes", true, true}, {"disabled", "no", true, false}, {"uncertain_native_role", "yes", false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := filepath.ToSlash(t.TempDir())
			log := dir + "/commands.log"
			write := func(name, text string) string {
				t.Helper()
				path := dir + "/" + name
				if err := os.WriteFile(path, []byte(text), 0o700); err != nil {
					t.Fatal(err)
				}
				return path
			}
			for _, name := range []string{"ap0", "sta0"} {
				if err := os.MkdirAll(filepath.FromSlash(dir+"/sys/class/net/"+name), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.MkdirAll(filepath.FromSlash(dir+"/sys/module/8852be"), 0o700); err != nil {
				t.Fatal(err)
			}
			envText := "#!/bin/sh\nprintf 'TESLABOX_AP_INTERFACE=ap0\\nTESLABOX_WIFI_INTERFACE=sta0\\n'\n"
			if !tt.envOK {
				envText = "#!/bin/sh\nexit 1\n"
			}
			helper := write("helper", envText)
			nm := write("nmcli", "#!/bin/sh\nprintf '%s\\n' \"$*\" >> '"+log+"'\ncase \"$*\" in\n*connection.autoconnect*) printf '"+tt.auto+"\\n';;\n*GENERAL.STATE*) printf '100\\n';;\n*IN-USE,CHAN*) printf '*:44\\n';;\nesac\n")
			iw := write("iw", "#!/bin/sh\nprintf 'iw %s\\n' \"$*\" >> '"+log+"'\ncat <<'CAP'\nSupported interface modes:\n * managed\n * AP\nvalid interface combinations:\n * #{ managed } <= 1, #{ AP } <= 1, total <= 2, #channels <= 1\nFrequencies:\n * 5220 MHz [44] (20.0 dBm)\nCAP\n")
			ip := write("ip", "#!/bin/sh\nprintf 'ip %s\\n' \"$*\" >> '"+log+"'\nexit 42\n")
			readlink := write("readlink", "#!/bin/sh\nprintf '/isolated/phy0\\n'\n")
			asset, err := managedAPAssets.ReadFile("assets/teslabox-ap-setup.sh")
			if err != nil {
				t.Fatal(err)
			}
			script := string(asset)
			for from, to := range map[string]string{"/sys": dir + "/sys", "/usr/local/bin/teslabox": helper, "/usr/bin/nmcli": nm, "/usr/sbin/iw": iw, "/usr/sbin/ip": ip, "readlink -f": "\"" + readlink + "\" -f", "/usr/local/libexec/teslabox-wireless-region.sh": dir + "/absent-region"} {
				script = strings.ReplaceAll(script, from, to)
			}
			path := write("ap-test.sh", script)
			cmd := exec.Command(sh, path)
			cmd.Env = append(os.Environ(), "TESLABOX_AP_INTERFACE=", "TESLABOX_WIFI_INTERFACE=")
			if runtime.GOOS == "windows" {
				cmd.Env = append(cmd.Env, "PATH="+filepath.Dir(sh)+string(os.PathListSeparator)+os.Getenv("PATH"))
			}
			output, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("隔离脚本失败: %v %s", err, output)
			}
			trace, _ := os.ReadFile(log)
			text := string(trace)
			if strings.Contains(text, "interface add") || strings.Contains(text, "ip ") {
				t.Fatalf("原生 AP 被创建第三接口或改 MAC: %s", text)
			}
			if tt.envOK {
				no := strings.Index(text, "device set ap0 autoconnect no")
				yes := strings.Index(text, "device set ap0 managed yes")
				if no < 0 || yes < no {
					t.Fatalf("接管保护顺序错误: %s", text)
				}
				if !strings.Contains(text, "802-11-wireless.channel 44") {
					t.Fatal("未跟随上游同信道")
				}
			} else if text != "" {
				t.Fatalf("角色未知仍操作网络: %s", text)
			}
			if strings.Contains(text, "connection up id TeslaBox-5G ifname ap0") != tt.wantUp {
				t.Fatalf("自动启动语义错误: %s", text)
			}
		})
	}
}
