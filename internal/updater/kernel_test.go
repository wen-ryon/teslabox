package updater

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestVerifyKernelPolicy(t *testing.T) {
	key, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		mutate  func(*KernelPolicy)
		tamper  bool
		wantErr bool
	}{
		{name: "signed RC2"},
		{name: "RC1 is not RC2 policy", mutate: func(p *KernelPolicy) { p.Version = "0.2.0-rc.1" }, wantErr: true},
		{name: "signed audited startup", mutate: func(p *KernelPolicy) {
			p.StartupMainSHA256 = []string{"768b24c9b91bd2c2648c94d5d254cb75858dac70a418988e7fa2bc2f565e06d1"}
		}},
		{name: "unknown startup", mutate: func(p *KernelPolicy) { p.StartupMainSHA256 = []string{strings.Repeat("c", 64)} }, wantErr: true},
		{name: "public RC1 explicit startup permission", mutate: func(p *KernelPolicy) {
			p.StartupMainSHA256 = []string{"5ee5385c2aa82bd7106ea4f861eee947db4379f749fda4d945959bae5ad6a48a"}
		}},
		{name: "frozen apfix6 requires paired rollback", mutate: func(p *KernelPolicy) {
			p.Files["modules/8852be.ko"] = KernelFile{SHA256: "31d0551815dc2163db700eb24239d98d003c65f270d05270d079887955b7ebce", Size: 6011152}
		}, wantErr: true},
		{name: "tampered bytes", tamper: true, wantErr: true},
		{name: "wrong ABI", mutate: func(p *KernelPolicy) { p.Release = "5.10.160teslabox2" }, wantErr: true},
		{name: "old RTL module", mutate: func(p *KernelPolicy) {
			p.Files["modules/8852be.ko"] = KernelFile{SHA256: strings.Repeat("b", 64), Size: 1}
		}, wantErr: true},
		{name: "old kernel deb", mutate: func(p *KernelPolicy) {
			p.Files["kernel.deb"] = KernelFile{SHA256: "18701fb698937b9627dff6f515fc543dc3556418ab59da92e06232ce9c645a88", Size: 10}
		}, wantErr: true},
		{name: "missing bluetooth module", mutate: func(p *KernelPolicy) { delete(p.Files, "modules/btusb.ko") }, wantErr: true},
		{name: "old bluetooth module", mutate: func(p *KernelPolicy) {
			p.Files["modules/btusb.ko"] = KernelFile{SHA256: "d1b340eae226d397d259b714bf6a940d242f14cba7e2ccd511135767a62a9b4f", Size: 105848}
		}, wantErr: true},
		{name: "http downgrade", mutate: func(p *KernelPolicy) { p.URL = "http://example.org/k.tar.gz" }, wantErr: true},
		{name: "path traversal", mutate: func(p *KernelPolicy) { p.Files["../escape"] = KernelFile{SHA256: strings.Repeat("a", 64), Size: 1} }, wantErr: true},
		{name: "oversized", mutate: func(p *KernelPolicy) { p.Size = 128 << 20 }, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			p := KernelPolicy{Schema: 1, Kind: "teslabox-kernel-dependency-v1", Version: KernelUpgradeVersion, Board: "radxa-rock-3a", Arch: "arm64", Release: kernelRelease,
				MainSHA256: strings.Repeat("a", 64), URL: "https://example.org/kernel.tar.gz", SHA256: strings.Repeat("b", 64), Size: 100,
				Files: map[string]KernelFile{"kernel.deb": {SHA256: kernelDebDigest, Size: 10}, "modules/btusb.ko": {SHA256: btusbMT79xxDigest, Size: 1037880},
					"modules/8852be.ko": {SHA256: rtl8852beDigest, Size: 20}, "modules/compat.ko": {SHA256: strings.Repeat("a", 64), Size: 10}, "licenses/LICENCE.mediatek": {SHA256: strings.Repeat("a", 64), Size: 10}}}
			if test.mutate != nil {
				test.mutate(&p)
			}
			payload, err := json.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			signature := []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(private, payload)))
			if test.tamper {
				payload = append(payload, ' ')
			}
			_, err = VerifyKernelPolicy(payload, signature, key)
			if (err != nil) != test.wantErr {
				t.Fatalf("verify error=%v, wantErr=%v", err, test.wantErr)
			}
		})
	}
}

func TestKernelStartupRequiresSignedAndAuditedIdentity(t *testing.T) {
	const old = "768b24c9b91bd2c2648c94d5d254cb75858dac70a418988e7fa2bc2f565e06d1"
	p := KernelPolicy{MainSHA256: strings.Repeat("a", 64)}
	if kernelStartupMainAllowed(p, old) || !kernelStartupMainAllowed(p, p.MainSHA256) {
		t.Fatal("未签署的旧主不能启动，目标主必须匹配")
	}
	p.StartupMainSHA256 = []string{old, strings.Repeat("b", 64)}
	if !kernelStartupMainAllowed(p, old) || kernelStartupMainAllowed(p, strings.Repeat("b", 64)) {
		t.Fatal("启动身份必须同时满足签署名单和固定审核名单")
	}
}

func TestLegacyStartupCannotAuthorizeRC1InstallOrRC2Commit(t *testing.T) {
	legacy := KernelPolicy{Version: "0.2.0-rc.1", MainSHA256: publicRC1Main}
	request := InstallRequest{Version: legacy.Version, SHA256: legacy.MainSHA256}
	for _, action := range []string{"prepare", "commit", "rollback", "verify"} {
		if validateKernelActionPolicy(legacy, request, action) == nil {
			t.Fatalf("旧政策不应允许 %s", action)
		}
	}
	if err := validateKernelActionPolicy(legacy, request, "verify-legacy"); err != nil {
		t.Fatal(err)
	}
	rc2 := KernelPolicy{Version: KernelUpgradeVersion, MainSHA256: strings.Repeat("a", 64), StartupMainSHA256: []string{publicRC1Main}}
	if validateKernelActionPolicy(rc2, request, "commit") == nil {
		t.Fatal("启动过渡名单不能授权旧主提交 RC2")
	}
}

type fakeKernelActions struct {
	events                *[]string
	waiting               bool
	prepareErr, commitErr error
}

func TestKernelStartupRejectsOldHelperAndUnverifiedKernel(t *testing.T) {
	for _, test := range []struct {
		name, protocol, ready string
		wantErr               bool
	}{
		{name: "old paired helper", protocol: "teslabox-paired-state-v1", wantErr: true},
		{name: "kernel not verified", protocol: "teslabox-kernel-first-v1", ready: "not ready", wantErr: true},
		{name: "kernel and apfix6 verified", protocol: "teslabox-kernel-first-v1", ready: "teslabox-kernel-ready-v1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := checkKernelStartup(func(_ string, args ...string) ([]byte, error) {
				if args[0] == "--kernel-protocol" {
					return []byte(test.protocol), nil
				}
				return []byte(test.ready), nil
			})
			if (err != nil) != test.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, test.wantErr)
			}
		})
	}
}

func (g fakeKernelActions) prepare(context.Context) (bool, error) {
	*g.events = append(*g.events, "kernel")
	return g.waiting, g.prepareErr
}
func (g fakeKernelActions) commit(context.Context) error {
	*g.events = append(*g.events, "commit")
	return g.commitErr
}
func (g fakeKernelActions) fail(context.Context) error {
	*g.events = append(*g.events, "rollback boot")
	return nil
}

func TestInstallWithKernel(t *testing.T) {
	for _, test := range []struct {
		name                                                    string
		required, waiting, kernelFail, programFail, receiptFail bool
		want                                                    []string
		wantErr                                                 bool
	}{
		{name: "legacy unchanged", want: []string{"program"}},
		{name: "must reboot before program", required: true, waiting: true, want: []string{"kernel"}},
		{name: "new kernel confirmed then program", required: true, want: []string{"kernel", "program", "commit"}},
		{name: "kernel failure never installs program", required: true, kernelFail: true, want: []string{"kernel"}, wantErr: true},
		{name: "program failure restores boot", required: true, programFail: true, want: []string{"kernel", "program", "rollback boot"}, wantErr: true},
		{name: "receipt failure cannot report success or undo committed data", required: true, receiptFail: true, want: []string{"kernel", "program", "commit"}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var events []string
			gate := fakeKernelActions{events: &events, waiting: test.waiting}
			if test.kernelFail {
				gate.prepareErr = errors.New("kernel failed")
			}
			if test.receiptFail {
				gate.commitErr = errors.New("receipt failed")
			}
			waiting, err := installWithKernel(context.Background(), test.required, gate, func() error {
				events = append(events, "program")
				if test.programFail {
					return errors.New("program failed")
				}
				return nil
			})
			if waiting != test.waiting || (err != nil) != test.wantErr || !reflect.DeepEqual(events, test.want) {
				t.Fatalf("waiting=%v err=%v events=%v want=%v", waiting, err, events, test.want)
			}
		})
	}
}
