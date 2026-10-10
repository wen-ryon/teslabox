package deviceadmin

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCarPlayProbeDNSRecoveryAcceptsOnlyOwnedNewAndLegacyRecords(t *testing.T) {
	lease := testCarPlayLease()
	lease.WebAliasAddress, lease.WebAliasTarget = "60.162.38.124", "192.168.88.1"
	address := netip.MustParseAddr(lease.WebAliasAddress)
	content, err := carPlayDNSContent(lease, "carplay.teslabox.online", address)
	if err != nil || !strings.Contains(string(content), "host-record=connman.vn.cloud.tesla.cn,60.162.38.124\n") {
		t.Fatalf("content=%s err=%v", content, err)
	}
	if strings.Contains(string(content), "address=/") {
		t.Fatal("probe DNS must not match a wildcard domain")
	}
	legacy := fmt.Sprintf("# TeslaBox CarPlay lease %s\nhost-record=carplay.teslabox.online,%s\n", lease.RuntimeAPUUID, address)
	for _, record := range []string{string(content), legacy, legacy + "host-record=other.tesla.cn,60.162.38.124\n"} {
		directory := t.TempDir()
		if err := os.Chmod(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		manager := &Manager{carPlayDNSDirectory: directory}
		path, _ := manager.carPlayDNSPath(lease)
		if err := os.WriteFile(path, []byte(record), 0o644); err != nil {
			t.Fatal(err)
		}
		err := manager.removeCarPlayDNS(lease)
		foreign := strings.Contains(record, "other.tesla.cn")
		if (err != nil) != foreign {
			t.Fatalf("foreign=%v err=%v", foreign, err)
		}
		if foreign {
			remaining, _ := os.ReadFile(path)
			if string(remaining) != record {
				t.Fatal("foreign DNS changed")
			}
		}
	}
}

func TestCarPlayDNSLeaseLifecycleAndRestart(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	m := &Manager{carPlayDomain: "carplay.teslabox.dev", carPlayDNSDirectory: directory}
	lease := CarPlayNetworkLease{RuntimeAPUUID: "11111111-2222-4333-8444-555555555555"}
	address := netip.MustParseAddr("192.168.88.1")
	if err := m.prepareCarPlayDNS(lease, address); err != nil {
		t.Fatal(err)
	}
	path, _ := m.carPlayDNSPath(lease)
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := m.prepareCarPlayDNS(lease, netip.MustParseAddr("192.168.89.1")); err == nil {
		t.Fatal("overwrote existing mapping")
	}
	after, _ := os.ReadFile(path)
	if string(content) != string(after) {
		t.Fatal("mapping changed")
	}
	// 重启时产品域名配置即使被移除，也依租约身份回收旧文件。
	restarted := &Manager{carPlayDNSDirectory: directory}
	if err := restarted.removeCarPlayDNS(lease); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("DNS file remains")
	}
	if err := restarted.removeCarPlayDNS(lease); err != nil {
		t.Fatal(err)
	}
	if files, err := filepath.Glob(filepath.Join(directory, ".teslabox*")); err != nil || len(files) != 0 {
		t.Fatal("temporary DNS files remain")
	}
}

func TestCarPlayDNSRejectsUnknownContentAndInjection(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	m := &Manager{carPlayDomain: "carplay.teslabox.dev", carPlayDNSDirectory: directory}
	lease := CarPlayNetworkLease{RuntimeAPUUID: "11111111-2222-4333-8444-555555555555"}
	path, _ := m.carPlayDNSPath(lease)
	if err := os.WriteFile(path, []byte("unrelated dns configuration\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := m.removeCarPlayDNS(lease); err == nil {
		t.Fatal("removed unrelated file")
	}
	if err := m.prepareCarPlayDNS(lease, netip.MustParseAddr("192.168.88.1")); err == nil {
		t.Fatal("overwrote unrelated file")
	}
	if _, err := carPlayDNSContent(lease, "carplay.dev\nserver=evil", netip.MustParseAddr("192.168.88.1")); err == nil {
		t.Fatal("accepted DNS directive injection")
	}
	if _, err := carPlayDNSContent(lease, "teslabox.local", netip.MustParseAddr("192.168.88.1")); err == nil {
		t.Fatal("overrode mDNS name")
	}
	if _, err := carPlayDNSContent(lease, "carplay.teslabox.dev", netip.MustParseAddr("8.8.8.8")); err == nil {
		t.Fatal("accepted nonprivate AP address")
	}
}
