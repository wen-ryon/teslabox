//go:build linux

package carplaypairing

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func serviceFixture(t *testing.T) *serviceClient {
	t.Helper()
	c := &serviceClient{runtime: t.TempDir()}
	if err := os.Chmod(c.runtime, 0700); err != nil {
		t.Fatal(err)
	}
	writeServiceStatus(t, c, serviceProvisioning{State: "idle"})
	return c
}

func writeServiceStatus(t *testing.T, c *serviceClient, status serviceProvisioning) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"running": true, "provisioning": status})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(c.runtime, "status.json"), body, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestServiceClientPrivateRequestAndExclusiveClaim(t *testing.T) {
	c := serviceFixture(t)
	claim, err := c.claim()
	if err != nil {
		t.Fatal(err)
	}
	if other, err := c.claim(); !errors.Is(err, ErrBusy) {
		if other != nil {
			other.Close()
		}
		t.Fatal(err)
	}
	request := serviceRequest{RequestID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PhoneID: testPhone, Action: "pause", ExpiresAtMS: time.Now().Add(time.Second).UnixMilli()}
	if err := c.write(request); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(c.runtime, "provisioning-request.json")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var actual serviceRequest
	if err := json.Unmarshal(body, &actual); err != nil || actual != request {
		t.Fatal(actual, err)
	}
	claim.Close()
	other, err := c.claim()
	if err != nil {
		t.Fatal(err)
	}
	other.Close()
}

func TestServiceClientStatusRejectsPublicLinksAndOldTerminalEvidence(t *testing.T) {
	c := serviceFixture(t)
	path := filepath.Join(c.runtime, "status.json")
	if _, err := c.status(); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(path, 0644)
	if _, err := c.status(); err == nil {
		t.Fatal("公开状态文件不能参与登记验收")
	}
	_ = os.Chmod(path, 0600)
	old := time.Now().Add(-11 * time.Second)
	_ = os.Chtimes(path, old, old)
	if _, err := c.status(); err == nil {
		t.Fatal("旧终态不能参与当前登记验收")
	}
	writeServiceStatus(t, c, serviceProvisioning{State: "video_verified", RequestID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PhoneID: testPhone, Running: true})
	if _, err := c.status(); err == nil {
		t.Fatal("登记仍活动不能声明已清理")
	}
	writeServiceStatus(t, c, serviceProvisioning{State: "idle"})
	_ = os.WriteFile(path, []byte(`{"running":false,"cleanup_complete":false,"provisioning":{"state":"idle","running":false}}`), 0600)
	if _, err := c.status(); err == nil {
		t.Fatal("服务尚未清理不能凭任务终态解除互斥")
	}
	writeServiceStatus(t, c, serviceProvisioning{State: "idle"})
	other := filepath.Join(c.runtime, "other")
	_ = os.Rename(path, other)
	_ = os.Symlink(other, path)
	if _, err := c.status(); err == nil {
		t.Fatal("软链接不能参与状态验收")
	}
	_ = os.Remove(path)
	_ = os.Link(other, path)
	if _, err := c.status(); err == nil {
		t.Fatal("硬链接不能参与状态验收")
	}
}

func TestServiceClientMissingCapabilityOrActiveOtherTaskNeverStartsNativePairing(t *testing.T) {
	c := serviceFixture(t)
	for _, capable := range []bool{false, true} {
		if capable {
			writeServiceStatus(t, c, serviceProvisioning{State: "paused", Running: true, RequestID: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", PhoneID: testPhone})
		} else {
			_ = os.WriteFile(filepath.Join(c.runtime, "status.json"), []byte(`{"running":true}`), 0600)
		}
		called := false
		runner := c.runner(func(context.Context, string, *Confirmation) error { called = true; return nil })
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		prompt, _ := NewConfirmation(ctx, time.Second)
		if err := runner(ctx, testPhone, prompt); err == nil || called {
			t.Fatal("能力不足或其他任务活动时不能执行配对", err, called)
		}
		cancel()
		if _, err := os.Stat(filepath.Join(c.runtime, "provisioning-request.json")); !os.IsNotExist(err) {
			t.Fatal("不能覆盖他人的服务任务", err)
		}
	}
}

func TestServiceClientWaitTimeoutDoesNotWriteOrRestart(t *testing.T) {
	c := serviceFixture(t)
	request := serviceRequest{RequestID: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PhoneID: testPhone, Action: "pause", ExpiresAtMS: time.Now().Add(time.Second).UnixMilli()}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := c.wait(ctx, request, "paused"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(c.runtime, "provisioning-request.json")); !os.IsNotExist(err) {
		t.Fatal("观察超时不能重发请求", err)
	}
}

func TestServiceClientWorkflowWithActualPythonProvisioning(t *testing.T) {
	module := os.Getenv("TESLABOX_PROVISIONING_TEST_MODULE")
	if module == "" {
		t.Skip("需要实际 Python 登记模块；板端隔离测试提供源码路径")
	}
	for _, scenario := range []struct {
		name                  string
		pairFails, onboarding bool
	}{
		{"verified_video", false, false}, {"pair_failed_cleanup", true, false},
		{"onboarding_verified_video", false, true}, {"onboarding_pair_failed", true, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			pairFails := scenario.pairFails
			c := serviceFixture(t)
			if scenario.onboarding {
				body, _ := json.Marshal(map[string]any{"running": false, "cleanup_complete": true, "onboarding_ready": true, "provisioning": serviceProvisioning{State: "idle"}})
				if err := os.WriteFile(filepath.Join(c.runtime, "status.json"), body, 0600); err != nil {
					t.Fatal(err)
				}
			}
			childCtx, childCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer childCancel()
			program := `import importlib.util,json,pathlib,sys,time,os
spec=importlib.util.spec_from_file_location('flow',sys.argv[1]);m=importlib.util.module_from_spec(spec);spec.loader.exec_module(m)
root=pathlib.Path(sys.argv[2]);flow=m.Provisioning(root);events=[];onboarding=sys.argv[3]=='onboarding'
def switch(mode,phone,seconds):
 events.append(mode);return True
while True:
 flow.poll(switch)
 if flow.active and flow.state['state']=='enrolling':
  flow.observe(dict(stage='phone_media_success',session_verified=True,media=dict(frames=4,source_connected=True,codec_ready=True)),flow.active['phone_id'])
  switch('resume',None,0)
 running=not onboarding or flow.state['state']=='video_verified'
 temporary=root/'status-writing';temporary.write_text(json.dumps(dict(running=running,cleanup_complete=True,onboarding_ready=onboarding and not running,provisioning=flow.snapshot())));temporary.chmod(0o600);temporary.replace(root/'status.json')
 try:
  request,_=m.read_request(root/'provisioning-request.json')
  if request['action']=='cancel' and not flow.state['running']:
   (root/'events.json').write_text(json.dumps(events));break
 except FileNotFoundError:pass
 time.sleep(.01)
`
			mode := "active"
			if scenario.onboarding {
				mode = "onboarding"
			}
			cmd := exec.CommandContext(childCtx, "python3", "-c", program, module, c.runtime, mode)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { childCancel(); _ = cmd.Wait() }()
			calls := 0
			pair := func(context.Context, string, *Confirmation) error {
				calls++
				if pairFails {
					return errors.New("synthetic pairing failure")
				}
				return nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			prompt, _ := NewConfirmation(ctx, 2*time.Second)
			handoff := &workflowHandoff{paired: &calls}
			prompt.handoff = handoff
			err := c.runner(pair)(ctx, testPhone, prompt)
			if (err != nil) != pairFails || errors.Is(err, ErrCleanupPending) || calls != 1 {
				t.Fatal(err, calls)
			}
			if handoff.begun == pairFails || handoff.success == pairFails || !handoff.finished {
				t.Fatal("配对与交接顺序错误", handoff)
			}
			if err := cmd.Wait(); err != nil {
				t.Fatal(err)
			}
			body, err := os.ReadFile(filepath.Join(c.runtime, "events.json"))
			if err != nil {
				t.Fatal(err)
			}
			var events []string
			_ = json.Unmarshal(body, &events)
			expected := "[\"paused\",\"enroll\",\"resume\"]"
			if pairFails {
				expected = "[\"paused\",\"resume\"]"
			}
			encoded, _ := json.Marshal(events)
			if string(encoded) != expected {
				t.Fatal(events)
			}
			status, err := c.status()
			if err != nil || status.Running {
				t.Fatal(status, err)
			}
			if !pairFails && status.State != "video_verified" {
				t.Fatal(status)
			}
		})
	}
}

type workflowHandoff struct {
	paired                   *int
	begun, finished, success bool
}

func (h *workflowHandoff) PlanCarPlayHandoff(context.Context) (HandoffPlan, error) {
	return HandoffPlan{}, nil
}
func (h *workflowHandoff) BeginCarPlayHandoff(context.Context, string, string, HandoffPlan) error {
	if *h.paired != 1 {
		return errors.New("在数字核对之前交接热点")
	}
	h.begun = true
	return nil
}
func (h *workflowHandoff) FinishCarPlayHandoff(_ context.Context, _ string, success bool) error {
	h.finished = true
	h.success = success
	return nil
}
