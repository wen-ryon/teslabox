package deviceadmin

import (
	"context"
	"testing"

	"github.com/godbus/dbus/v5"
)

func TestCarPlayAPProfilePreservesTemplateAndRejectsChangedCredentials(t *testing.T) {
	lease := testCarPlayLease()
	settings := carPlayNMSettings{
		"connection":               {"uuid": dbus.MakeVariant(lease.Plan.APUUID), "autoconnect": dbus.MakeVariant(true)},
		"802-11-wireless":          {"ssid": dbus.MakeVariant([]byte("TeslaBox")), "mode": dbus.MakeVariant("ap"), "powersave": dbus.MakeVariant(uint32(2))},
		"802-11-wireless-security": {"key-mgmt": dbus.MakeVariant("wpa-psk"), "pmf": dbus.MakeVariant(uint32(2))},
		"ipv4":                     {"method": dbus.MakeVariant("shared")},
		"ipv6":                     {"method": dbus.MakeVariant("auto")},
		"user":                     {"data": dbus.MakeVariant(map[string]string{"vendor": "board-adaptation"})},
	}
	secrets := carPlayNMSettings{"802-11-wireless-security": {"psk": dbus.MakeVariant("test-password")}}
	copy, err := prepareCarPlayAPProfile(settings, secrets, lease, "AA:BB:CC:DD:EE:FF", "TeslaBox", "test-password")
	if err != nil {
		t.Fatal(err)
	}
	if copy["ipv6"]["method"].Value() != "auto" || copy["802-11-wireless"]["powersave"].Value() != uint32(2) ||
		copy["802-11-wireless-security"]["pmf"].Value() != uint32(2) || copy["user"]["data"].Value().(map[string]string)["vendor"] != "board-adaptation" {
		t.Fatal("丢失原模板的适配属性")
	}
	if copy["connection"]["uuid"].Value() != lease.RuntimeAPUUID || copy["connection"]["autoconnect"].Value() != false ||
		settings["connection"]["uuid"].Value() != lease.Plan.APUUID || settings["connection"]["autoconnect"].Value() != true {
		t.Fatal("临时身份错误或修改了正式模板")
	}
	if copy["802-11-wireless"]["assigned-mac-address"].Signature().String() != "s" ||
		copy["802-11-wireless"]["channel"].Signature().String() != "u" ||
		copy["802-11-wireless"]["ssid"].Signature().String() != "ay" {
		t.Fatal("热点属性使用了与 NetworkManager D-Bus 契约不符的类型")
	}
	if _, err := prepareCarPlayAPProfile(settings, secrets, lease, "AA:BB:CC:DD:EE:FF", "TeslaBox", "old-password"); err == nil {
		t.Fatal("接受了读取期间发生变化的凭据")
	}
	lease.Plan.Channel = 6
	if err := validateCarPlayLease(lease); err != nil {
		t.Fatal(err)
	}
	copy, err = prepareCarPlayAPProfile(settings, secrets, lease, "AA:BB:CC:DD:EE:FF", "TeslaBox", "test-password")
	if err != nil || copy["802-11-wireless"]["band"].Value() != "bg" || copy["802-11-wireless"]["channel"].Value() != uint32(6) {
		t.Fatalf("2.4 GHz 配置错误: %v %v", copy, err)
	}
	if _, exists := settings["802-11-wireless"]["band"]; exists {
		t.Fatal("改写了正式模板频段")
	}
}

func TestCarPlayAPHealthRejectsForeignConnectionAndChangedChannel(t *testing.T) {
	lease := testCarPlayLease()
	for _, test := range []struct {
		name, uuid, channel string
		healthy             bool
	}{
		{"正常", lease.RuntimeAPUUID, "149", true},
		{"其他连接", lease.Plan.APUUID, "149", false},
		{"信道改变", lease.RuntimeAPUUID, "48", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &fakeRunner{responses: map[string]string{
				"nmcli -t -f UUID,DEVICE connection show --active": test.uuid + ":ap0\n",
				"/usr/sbin/iw dev ap0 info":                        "type AP\nchannel " + test.channel + " (5745 MHz)\n",
			}}
			manager := New(runner, nil)
			if err := manager.RestoreCarPlayNetworkHold(lease); err != nil {
				t.Fatal(err)
			}
			healthy, err := manager.CarPlayAPHealthy(context.Background(), lease.Owner)
			if err != nil || healthy != test.healthy {
				t.Fatalf("状态错误: %v %v", healthy, err)
			}
		})
	}
}

func TestCarPlayIPv6LegacyStructuresRetainValuesAndSignature(t *testing.T) {
	address := []byte{0x20, 1, 0xdb, 8, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1}
	settings := carPlayNMSettings{"ipv6": {
		"addresses": dbus.MakeVariant([][]any{{address, uint32(64), make([]byte, 16)}}),
		"routes":    dbus.MakeVariant([][]any{{address, uint32(64), make([]byte, 16), uint32(10)}}),
	}}
	if err := normalizeCarPlayIPProperties(settings); err != nil {
		t.Fatal(err)
	}
	if settings["ipv6"]["addresses"].Signature().String() != "a(ayuay)" || settings["ipv6"]["routes"].Signature().String() != "a(ayuayu)" {
		t.Fatal("旧 IPv6 结构失去 D-Bus 签名")
	}
	if settings["ipv6"]["addresses"].Value().([]carPlayIPv6Address)[0].Prefix != 64 || settings["ipv6"]["routes"].Value().([]carPlayIPv6Route)[0].Metric != 10 {
		t.Fatal("旧 IPv6 地址和路由被丢弃")
	}
}
