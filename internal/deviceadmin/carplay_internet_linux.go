//go:build linux

package deviceadmin

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func probeCarPlayInternet(ctx context.Context, interfaceName string, servers []string) bool {
	// DNS 和 HTTPS 都绑定被测接口，禁止把其他默认路由的成功算成该上游可用。
	control := func(_, _ string, raw syscall.RawConn) error {
		var bindErr error
		if err := raw.Control(func(fd uintptr) {
			bindErr = unix.SetsockoptString(int(fd), unix.SOL_SOCKET, unix.SO_BINDTODEVICE, interfaceName)
		}); err != nil {
			return err
		}
		return bindErr
	}
	for _, server := range servers {
		probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		dnsDialer := &net.Dialer{Control: control, Timeout: 3 * time.Second}
		resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dnsDialer.DialContext(ctx, network, net.JoinHostPort(server, "53"))
		}}
		dialer := &net.Dialer{Control: control, Resolver: resolver, Timeout: 3 * time.Second}
		transport := &http.Transport{DialContext: dialer.DialContext, DisableKeepAlives: true}
		client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
		request, _ := http.NewRequestWithContext(probeCtx, http.MethodGet, "https://www.apple.com/library/test/success.html", nil)
		response, err := client.Do(request)
		success := false
		if err == nil {
			body, readErr := io.ReadAll(io.LimitReader(response.Body, 4097))
			response.Body.Close()
			success = readErr == nil && len(body) <= 4096 && response.StatusCode == http.StatusOK && strings.Contains(string(body), "<BODY>Success</BODY>")
		}
		transport.CloseIdleConnections()
		cancel()
		if success {
			return true
		}
		if ctx.Err() != nil {
			return false
		}
	}
	return false
}
