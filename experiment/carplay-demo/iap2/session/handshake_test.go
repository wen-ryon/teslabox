package session

import (
	"bytes"
	"testing"

	"github.com/wen-ryon/teslabox/experiment/carplay-demo/iap2/csm"
	"github.com/wen-ryon/teslabox/experiment/carplay-demo/iap2/endpoint"
)

func TestIdentificationCarriesRequiredFields(t *testing.T) {
	id := DefaultIdentity()
	frame, err := BuildIdentificationInformation(id)
	if err != nil {
		t.Fatalf("构造身份消息失败: %v", err)
	}
	if frame.MessageID != endpoint.IdentificationInformation {
		t.Errorf("消息 ID = %#x, 期望 0x1d01", frame.MessageID)
	}

	params, err := frame.Params()
	if err != nil {
		t.Fatalf("解析参数失败: %v", err)
	}
	for _, spec := range []struct {
		id    uint16
		want  string
		label string
	}{
		{endpoint.IdentName, id.Name, "name"},
		{endpoint.IdentModelIdentifier, id.ModelIdentifier, "modelIdentifier"},
		{endpoint.IdentManufacturer, id.Manufacturer, "manufacturer"},
		{endpoint.IdentSerialNumber, id.SerialNumber, "serialNumber"},
		{endpoint.IdentFirmwareVersion, id.FirmwareVersion, "firmwareVersion"},
		{endpoint.IdentHardwareVersion, id.HardwareVersion, "hardwareVersion"},
		{endpoint.IdentCurrentLanguage, id.CurrentLanguage, "currentLanguage"},
	} {
		p := params.First(spec.id)
		if p == nil {
			t.Errorf("缺少必填参数 0x%04x (%s)", spec.id, spec.label)
			continue
		}
		got, err := csm.ReadString(p.Payload())
		if err != nil || got != spec.want {
			t.Errorf("%s = %q (%v), 期望 %q", spec.label, got, err, spec.want)
		}
	}
}

func TestIdentificationDeclaresBothDirections(t *testing.T) {
	id := DefaultIdentity()
	frame, err := BuildIdentificationInformation(id)
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	params, err := frame.Params()
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	// 两个清单都必须非空：空的接收清单会让 iPhone 什么都不发过来。
	for _, spec := range []struct {
		id    uint16
		label string
	}{
		{endpoint.IdentMessagesSentByAccessory, "车机会发的消息"},
		{endpoint.IdentMessagesReceivedFromDevice, "车机能收的消息"},
	} {
		p := params.First(spec.id)
		if p == nil {
			t.Errorf("缺少 %s 清单", spec.label)
			continue
		}
		list, err := csm.ReadU16List(p.Payload())
		if err != nil {
			t.Errorf("%s 解析失败: %v", spec.label, err)
			continue
		}
		if len(list) == 0 {
			t.Errorf("%s 为空，iPhone 将不会收到/发送对应消息", spec.label)
		}
	}
}

func TestIdentificationDeclaresUSBAndHID(t *testing.T) {
	frame, err := BuildIdentificationInformation(DefaultIdentity())
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	params, err := frame.Params()
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if params.First(endpoint.IdentUSBHostTransport) == nil {
		t.Error("未声明 USB 传输组件，有线 CarPlay 无法建立")
	}
	// HID 组件是可重复参数，必须能取出全部。
	if got := len(params.Same(endpoint.IdentIAP2HIDComponent)); got != 1 {
		t.Errorf("HID 组件数 %d, 期望 1", got)
	}
}

func TestIdentificationRejectsMissingRequiredField(t *testing.T) {
	id := DefaultIdentity()
	id.SerialNumber = ""
	if _, err := BuildIdentificationInformation(id); err == nil {
		t.Error("缺少必填字段时应报错")
	}
}

func TestAuthenticationRoundTrip(t *testing.T) {
	certificate := bytes.Repeat([]byte{0x30, 0x82}, 8)
	frame, err := BuildAuthenticationCertificate(certificate)
	if err != nil {
		t.Fatalf("构造证书消息失败: %v", err)
	}
	if frame.MessageID != endpoint.AuthenticationCertificate {
		t.Errorf("消息 ID = %#x, 期望 0xaa01", frame.MessageID)
	}
	params, err := frame.Params()
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	got, err := params.Get(endpoint.AuthCertificateSerial)
	if err != nil {
		t.Fatalf("取证书失败: %v", err)
	}
	if !bytes.Equal(got.Payload(), certificate) {
		t.Error("回读的证书与原始不符")
	}
}

func TestParseChallenge(t *testing.T) {
	challenge := bytes.Repeat([]byte{0x5a}, 20)
	incoming, err := csm.NewFrameOf(endpoint.RequestAuthenticationChallengeResponse, csm.NewList(
		csm.MustParameter(endpoint.AuthChallengeData, challenge),
	))
	if err != nil {
		t.Fatalf("构造挑战消息失败: %v", err)
	}
	got, err := ParseChallenge(incoming)
	if err != nil {
		t.Fatalf("ParseChallenge 失败: %v", err)
	}
	if !bytes.Equal(got, challenge) {
		t.Errorf("挑战数据不符: %d 字节 vs %d 字节", len(got), len(challenge))
	}
}

func TestParseChallengeRejectsWrongMessage(t *testing.T) {
	wrong, err := csm.NewFrameOf(endpoint.IdentificationAccepted, csm.NewList())
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if _, err := ParseChallenge(wrong); err == nil {
		t.Error("非挑战消息应被拒绝")
	}
}

func TestCarPlayStartSession(t *testing.T) {
	frame, err := BuildCarPlayStartSession("fe80::1", false)
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if frame.MessageID != endpoint.CarPlayStartSession {
		t.Errorf("消息 ID = %#x, 期望 0x4301", frame.MessageID)
	}
	params, err := frame.Params()
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if params.First(endpoint.CarPlayStartWired) == nil {
		t.Error("缺少有线会话参数")
	}
	if _, err := BuildCarPlayStartSession("", false); err == nil {
		t.Error("缺少本机地址时应报错")
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		messageID uint16
		want      MessageRole
	}{
		{endpoint.RequestAuthenticationCertificate, RoleCertificateRequested},
		{endpoint.RequestAuthenticationChallengeResponse, RoleChallengeReceived},
		{endpoint.AuthenticationSucceeded, RoleAuthenticationSucceeded},
		{endpoint.IdentificationAccepted, RoleIdentificationAccepted},
		{endpoint.CarPlayAvailability, RoleCarPlayAvailable},
		{0x1234, RoleOther},
	}
	for _, tc := range cases {
		frame, err := csm.NewFrame(tc.messageID, nil)
		if err != nil {
			t.Fatalf("构造失败: %v", err)
		}
		if got := Classify(frame); got != tc.want {
			t.Errorf("Classify(0x%04x) = %v, 期望 %v", tc.messageID, got, tc.want)
		}
	}
}

func TestSplitForLink(t *testing.T) {
	frame, err := BuildIdentificationInformation(DefaultIdentity())
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	chunks, err := SplitForLink(frame, 8)
	if err != nil {
		t.Fatalf("切分失败: %v", err)
	}
	// 拼回去必须和原始帧逐字节一致，否则对端解不出消息。
	complete := frame.Encode()
	joined := make([]byte, 0, len(complete))
	for _, c := range chunks {
		joined = append(joined, c...)
	}
	if !bytes.Equal(joined, complete) {
		t.Errorf("切分后拼接与原始不符: %d vs %d 字节", len(joined), len(complete))
	}
}
