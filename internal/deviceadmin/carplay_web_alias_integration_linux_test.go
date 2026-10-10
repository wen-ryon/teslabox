//go:build linux

package deviceadmin

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/wen-ryon/teslabox/internal/system"
)

func TestCarPlayWebAliasTLSAndCleanupInIsolatedNetwork(t *testing.T) {
	if os.Getenv("TESLABOX_CARPLAY_ALIAS_NETNS") != "1" || os.Geteuid() != 0 {
		t.Skip("需要显式启用的独立 Linux root 网络命名空间")
	}
	current, e1 := os.Readlink("/proc/self/ns/net")
	initial, e2 := os.Readlink("/proc/1/ns/net")
	if e1 != nil || e2 != nil || current == initial {
		t.Fatal("拒绝在板子的实际网络命名空间测试")
	}
	for _, name := range []string{"ip", "nsenter", "unshare", "sleep", "curl", "iptables"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Fatalf("缺少测试依赖 %s", name)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	runner := system.NewOSRunner()
	run := func(name string, args ...string) string {
		t.Helper()
		out, err := runner.Run(ctx, name, args...)
		if err != nil {
			t.Fatalf("隔离网络命令失败: %v", err)
		}
		return string(out)
	}
	child := exec.CommandContext(ctx, "unshare", "--net", "sleep", "30")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill(); _ = child.Wait() }()
	pid := strconv.Itoa(child.Process.Pid)
	deadline := time.Now().Add(time.Second)
	for {
		ns, err := os.Readlink("/proc/" + pid + "/ns/net")
		if err == nil && ns != current {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("客户端独立网络未就绪")
		}
		time.Sleep(10 * time.Millisecond)
	}
	run("ip", "link", "set", "lo", "up")
	run("ip", "link", "add", "ap0", "type", "veth", "peer", "name", "cpclient")
	run("ip", "addr", "add", "192.168.88.1/24", "dev", "ap0")
	run("ip", "link", "set", "ap0", "up")
	run("ip", "link", "set", "cpclient", "netns", pid)
	for _, args := range [][]string{{"link", "set", "lo", "up"}, {"addr", "add", "192.168.88.2/24", "dev", "cpclient"}, {"link", "set", "cpclient", "up"}, {"route", "add", "default", "via", "192.168.88.1"}} {
		run("nsenter", append([]string{"--target", pid, "--net", "ip"}, args...)...)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "carplay.teslabox.test"}, DNSNames: []string{"carplay.teslabox.test"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	pair, err := tls.X509KeyPair(certificate, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: private}))
	if err != nil {
		t.Fatal(err)
	}
	ca := filepath.Join(t.TempDir(), "test-ca.pem")
	if err := os.WriteFile(ca, certificate, 0o600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "192.168.88.1:443")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "carplay.teslabox.test" {
			http.Error(w, "host mismatch", 400)
			return
		}
		_, _ = w.Write([]byte("local-https-verified"))
	}), ReadHeaderTimeout: 3 * time.Second}
	defer server.Close()
	go func() {
		_ = server.Serve(tls.NewListener(listener, &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}))
	}()
	probeListener, err := net.Listen("tcp", "192.168.88.1:80")
	if err != nil {
		t.Fatal(err)
	}
	probeServer := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "connman.vn.cloud.tesla.cn" || r.URL.Path != "/online/status.html" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("X-ConnMan-Status", "online")
		_, _ = w.Write([]byte("local-probe-verified"))
	}), ReadHeaderTimeout: 3 * time.Second}
	defer probeServer.Close()
	go func() { _ = probeServer.Serve(probeListener) }()
	lease := testCarPlayLease()
	lease.WebAliasAddress, lease.WebAliasTarget = "60.162.38.124", "192.168.88.1"
	manager := New(runner, nil)
	request := func(port string) (string, error) {
		returnData, e := runner.Run(ctx, "nsenter", "--target", pid, "--net", "curl", "--noproxy", "*", "--max-time", "2", "--cacert", ca, "--resolve", "carplay.teslabox.test:"+port+":"+lease.WebAliasAddress, "-sS", "https://carplay.teslabox.test:"+port+"/")
		return strings.TrimSpace(string(returnData)), e
	}
	probeRequest := func() (string, error) {
		out, err := runner.Run(ctx, "nsenter", "--target", pid, "--net", "curl", "--noproxy", "*", "--max-time", "2", "--resolve", "connman.vn.cloud.tesla.cn:80:"+lease.WebAliasAddress, "-fsS", "-D", "-", "http://connman.vn.cloud.tesla.cn/online/status.html")
		return string(out), err
	}
	if _, err := request("443"); err == nil {
		t.Fatal("映射建立前目标不应可达")
	}
	if err := manager.prepareCarPlayWebAlias(ctx, lease); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := manager.removeCarPlayWebAlias(ctx, lease); err != nil {
			t.Error(err)
		}
	}()
	if err := manager.prepareCarPlayWebAlias(ctx, lease); err != nil {
		t.Fatal(err)
	}
	rules := run("iptables", "-t", "nat", "-S", "PREROUTING")
	if strings.Count(rules, "TeslaBox-CarPlay-HTTPS-"+lease.RuntimeAPUUID) != 1 {
		t.Fatal("重复准备创建了多条规则")
	}
	if body, err := request("443"); err != nil || body != "local-https-verified" {
		t.Fatalf("绑定域名的可信本地 HTTPS 未通过: %q %v", body, err)
	}
	if response, err := probeRequest(); err != nil || !strings.Contains(response, "X-Connman-Status: online") || !strings.Contains(response, "local-probe-verified") {
		t.Fatalf("独立客户端未收到本地联网检测响应: %q %v", response, err)
	}
	if _, err := request("4443"); err == nil {
		t.Fatal("映射意外开放了其他端口")
	}
	restarted := New(runner, nil)
	if err := restarted.removeCarPlayWebAlias(ctx, lease); err != nil {
		t.Fatal(err)
	}
	if _, err := request("443"); err == nil {
		t.Fatal("释放后仍能通过映射连接")
	}
	if _, err := probeRequest(); err == nil {
		t.Fatal("释放后联网检测映射仍存在")
	}
	if rules := run("iptables", "-t", "nat", "-S", "PREROUTING"); strings.Contains(rules, "TeslaBox-CarPlay-HTTPS-") || strings.Contains(rules, "TeslaBox-CarPlay-Probe-") {
		t.Fatal("释放后规则未清理")
	}
	if err := manager.prepareCarPlayWebAlias(ctx, lease); err != nil {
		t.Fatal(err)
	}
	if body, err := request("443"); err != nil || body != "local-https-verified" {
		t.Fatalf("规则恢复后真实 HTTPS 未通过: %q %v", body, err)
	}
	t.Log("独立客户端可信 HTTPS、端口限制、重复启动、释放和重建通过")
}
