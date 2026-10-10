package deviceadmin

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestCarPlayDNSActualDnsmasqResolution(t *testing.T) {
	if os.Getenv("TESLABOX_TEST_DNSMASQ") != "1" || os.Geteuid() != 0 {
		t.Skip("需要显式启用的 Linux root 板端 DNS 集成窗口")
	}
	const binary = "/usr/sbin/dnsmasq"
	if info, err := os.Stat(binary); err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		t.Fatal("板端 dnsmasq 不存在或不可执行")
	}
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{carPlayDomain: "carplay.teslabox.dev", carPlayDNSDirectory: directory}
	lease := CarPlayNetworkLease{RuntimeAPUUID: "11111111-2222-4333-8444-555555555555"}
	lease.WebAliasAddress, lease.WebAliasTarget = "60.162.38.124", "192.168.88.1"
	if err := manager.prepareCarPlayDNS(lease, netip.MustParseAddr(lease.WebAliasAddress)); err != nil {
		t.Fatal(err)
	}
	path, _ := manager.carPlayDNSPath(lease)
	// 独立回环高端口，不修改系统解析、热点实例或公网记录。
	reservation, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reservation.LocalAddr().(*net.UDPAddr)
	port := address.Port
	reservation.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, binary, "--no-daemon", fmt.Sprintf("--port=%d", port), "--listen-address=127.0.0.1", "--bind-interfaces", "--no-hosts", "--no-resolv", "--conf-file="+path, "--pid-file=", "--user=root", "--log-facility=-")
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		command.Process.Kill()
		command.Wait()
		if err := manager.removeCarPlayDNS(lease); err != nil {
			t.Error(err)
		}
	}()
	resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, fmt.Sprintf("127.0.0.1:%d", port))
	}}
	deadline := time.Now().Add(3 * time.Second)
	var ips []net.IP
	for time.Now().Before(deadline) {
		query, stop := context.WithTimeout(ctx, 300*time.Millisecond)
		ips, err = resolver.LookupIP(query, "ip4", manager.carPlayDomain)
		stop()
		if err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil || len(ips) != 1 || ips[0].String() != lease.WebAliasAddress {
		t.Fatalf("实际域名解析异常：%v %v", ips, err)
	}
	query, stop := context.WithTimeout(ctx, time.Second)
	defer stop()
	probeIPs, err := resolver.LookupIP(query, "ip4", "connman.vn.cloud.tesla.cn")
	if err != nil || len(probeIPs) != 1 || probeIPs[0].String() != lease.WebAliasAddress {
		t.Fatalf("实际车机探测主机解析异常：%v %v", probeIPs, err)
	}
	if _, err := resolver.LookupIP(query, "ip4", "api-prd.vn.cloud.tesla.cn"); err == nil {
		t.Fatal("意外覆盖了普通 Tesla 服务")
	}
	if _, err := resolver.LookupIP(query, "ip6", "connman.vn.cloud.tesla.cn"); err == nil {
		t.Fatal("联网检测主机意外产生 IPv6 地址")
	}
	if _, err := resolver.LookupIP(query, "ip4", "other."+manager.carPlayDomain); err == nil {
		t.Fatal("意外解析了其他主机名")
	}
	if _, err := resolver.LookupIP(query, "ip6", manager.carPlayDomain); err == nil {
		t.Fatal("意外生成了不可用 IPv6 地址")
	}
}
