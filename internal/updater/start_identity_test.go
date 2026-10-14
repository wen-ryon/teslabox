package updater

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWaitForProgramIdentityAcceptsOnlyCompletedExecution(t *testing.T) {
	calls := 0
	pid, err := waitForProgramIdentity(t.Context(), time.Second, time.Millisecond, func(context.Context) (string, error) {
		calls++
		if calls < 3 {
			return "", errors.New("运行进程不是本次安装的程序")
		}
		return "123", nil
	})
	if err != nil || pid != "123" || calls != 3 {
		t.Fatalf("pid=%q calls=%d err=%v", pid, calls, err)
	}
}

func TestWaitForProgramIdentityRejectsPersistentWrongProgram(t *testing.T) {
	wrongProgram := errors.New("运行进程不是本次安装的程序")
	calls := 0
	pid, err := waitForProgramIdentity(t.Context(), 20*time.Millisecond, time.Millisecond, func(context.Context) (string, error) {
		calls++
		return "", wrongProgram
	})
	if pid != "" || calls == 0 || !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, wrongProgram) {
		t.Fatalf("pid=%q calls=%d err=%v", pid, calls, err)
	}
}

func TestWaitForProgramIdentityUsesParentDeadline(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	parentDeadline, _ := ctx.Deadline()
	_, err := waitForProgramIdentity(ctx, time.Hour, time.Millisecond, func(checkCtx context.Context) (string, error) {
		deadline, ok := checkCtx.Deadline()
		if !ok || deadline.After(parentDeadline) {
			t.Fatalf("启动检查延长了原截止时间: %v -> %v", parentDeadline, deadline)
		}
		cancel()
		return "", errors.New("尚未完成执行")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestWaitForProgramIdentityDoesNotAcceptLateSuccess(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	pid, err := waitForProgramIdentity(ctx, time.Second, time.Millisecond, func(context.Context) (string, error) {
		cancel()
		return "123", nil
	})
	if pid != "" || !errors.Is(err, context.Canceled) {
		t.Fatalf("pid=%q err=%v", pid, err)
	}
}

func TestWaitForProgramIdentityDoesNotCheckExpiredContext(t *testing.T) {
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	calls := 0
	_, err := waitForProgramIdentity(ctx, time.Second, time.Millisecond, func(context.Context) (string, error) {
		calls++
		return "123", nil
	})
	if calls != 0 || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("calls=%d err=%v", calls, err)
	}
}

func TestProgramHealthRetainsStrictIdentity(t *testing.T) {
	for _, test := range []struct {
		name        string
		header      string
		actualPID   string
		verifyErr   error
		badHeader   bool
		cancelAfter bool
		wantErr     string
		wantChecks  int
	}{
		{name: "same pid", header: "123", actualPID: "123", wantChecks: 1},
		{name: "old health pid", header: "456", badHeader: true, wantErr: "context deadline exceeded"},
		{name: "missing health pid", badHeader: true, wantErr: "context deadline exceeded"},
		{name: "pid changed", header: "123", actualPID: "456", wantChecks: 1, wantErr: "健康检查期间主进程已变化"},
		{name: "wrong executable", header: "123", verifyErr: errors.New("运行进程不是本次安装的程序"), wantChecks: 1, wantErr: "运行进程不是本次安装的程序"},
		{name: "canceled final check", header: "123", actualPID: "123", cancelAfter: true, wantChecks: 1, wantErr: "context canceled"},
	} {
		t.Run(test.name, func(t *testing.T) {
			timeout := time.Second
			if test.badHeader {
				timeout = 200 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(t.Context(), timeout)
			defer cancel()
			requested := make(chan struct{}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-TeslaBox-Process", test.header)
				w.WriteHeader(http.StatusOK)
				select {
				case requested <- struct{}{}:
				default:
				}
			}))
			defer server.Close()
			checks := 0
			err := waitForProgramHealth(ctx, server.Client(), server.URL, "123", true, func(context.Context) (string, error) {
				checks++
				if test.cancelAfter {
					cancel()
				}
				return test.actualPID, test.verifyErr
			})
			if checks != test.wantChecks || (test.wantErr == "" && err != nil) || (test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr))) {
				t.Fatalf("checks=%d err=%v", checks, err)
			}
			select {
			case <-requested:
			default:
				t.Fatal("未实际请求健康端点，不能作为身份验收证据")
			}
		})
	}
}

func TestProgramHealthDoesNotRequestAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	err := waitForProgramHealth(ctx, nil, "", "123", true, func(context.Context) (string, error) {
		t.Fatal("取消后不应核验身份")
		return "", nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestInstalledProgramIdentityRejectsNonRegularTarget(t *testing.T) {
	if _, err := installedProgramIdentity(t.TempDir()); err == nil {
		t.Fatal("不能把目录当作安装程序")
	}
	path := filepath.Join(t.TempDir(), "program")
	if err := os.WriteFile(path, []byte("program"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := installedProgramIdentity(path); err != nil {
		t.Fatal(err)
	}
}
