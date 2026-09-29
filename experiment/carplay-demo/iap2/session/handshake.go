// Package session 实现 iAP2 握手阶段的控制消息：Identification、Authentication、CarPlay 会话。
//
// 一次有线 CarPlay 的建立顺序：
//
//  1. USB 枚举完成 → 发 link.IAP2Marker → iPhone 切进 iAP2 模式 → NCM 网卡就绪
//  2. 链路层同步（link.Engine）
//  3. Identification：iPhone 索要身份，配件回 IdentificationInformation
//  4. Authentication：iPhone 索要证书、下发挑战，配件用 MFi 芯片签名后回传
//  5. CarPlay：配件发 CarPlayStartSession，iPhone 开始推视频流
//
// 消息 ID 与参数 ID 全部取自 iap2/endpoint，字段顺序取自 xcertplay 的实现。
package session

import (
	"errors"
	"fmt"

	"github.com/wen-ryon/teslabox/experiment/carplay-demo/iap2/csm"
	"github.com/wen-ryon/teslabox/experiment/carplay-demo/iap2/endpoint"
	"github.com/wen-ryon/teslabox/experiment/carplay-demo/iap2/link"
)

// Identity 是配件在 Identification 阶段自报的信息。
type Identity struct {
	Name               string
	ModelIdentifier    string
	Manufacturer       string
	SerialNumber       string
	FirmwareVersion    string
	HardwareVersion    string
	CurrentLanguage    string
	SupportedLanguages []string

	// SentMessages 是本端会发出的消息 ID；ReceivedMessages 是本端能处理的消息 ID。
	// iPhone 只会发 ReceivedMessages 里列出的消息，漏填会导致功能缺失。
	SentMessages     []uint16
	ReceivedMessages []uint16

	// USBDeviceTransport 声明支持 USB 主机模式（有线 CarPlay 必需）。
	USBDeviceTransport bool
	// BluetoothMAC 非空时声明支持蓝牙传输，是无线 CarPlay 的入口。
	BluetoothMAC           []byte
	BluetoothTransportName string
	// WirelessCarPlay 声明支持无线 CarPlay，需要同时提供蓝牙 MAC。
	WirelessCarPlay bool
	// HIDComponents 声明 HID 能力，触摸屏是 CarPlay 交互的基础。
	HIDComponents []HIDComponent
}

// HIDComponent 描述一个 iAP2 HID 组件。
type HIDComponent struct {
	ID       uint16
	Name     string
	Function uint8
}

// DefaultIdentity 返回一份可直接用于有线 CarPlay 的默认身份。
//
// 消息清单由 endpoint.Known 按方向自动生成，避免手抄 ID 时漏项或写错方向。
func DefaultIdentity() Identity {
	var sent, received []uint16
	for _, e := range endpoint.Known {
		switch e.Direction {
		case endpoint.ToIPhone:
			sent = append(sent, e.ID)
		case endpoint.ToAccessory:
			received = append(received, e.ID)
		case endpoint.Bidirectional:
			sent = append(sent, e.ID)
			received = append(received, e.ID)
		}
	}
	return Identity{
		Name:               "TeslaBox",
		ModelIdentifier:    "TeslaBox-CarPlay",
		Manufacturer:       "TeslaBox",
		SerialNumber:       "000000000001",
		FirmwareVersion:    "1.0.0",
		HardwareVersion:    "1.0.0",
		CurrentLanguage:    "zh-CN",
		SupportedLanguages: []string{"zh-CN", "en"},
		SentMessages:       sent,
		ReceivedMessages:   received,
		USBDeviceTransport: true,
		HIDComponents: []HIDComponent{
			{ID: 1, Name: "TouchScreen", Function: endpoint.HIDFunctionTouchScreen},
		},
	}
}

// 握手过程中的语义错误。
var (
	ErrIdentificationRejected = errors.New("session: iPhone 拒绝了配件身份")
	ErrAuthenticationFailed   = errors.New("session: MFi 认证失败")
	ErrUnexpectedMessage      = errors.New("session: 收到了非预期的消息")
)

// BuildIdentificationInformation 构造 0x1d01。
//
// 必填字段（name/modelIdentifier/manufacturer/serialNumber/firmwareVersion/
// hardwareVersion/currentLanguage/supportedLanguages）缺一个，iPhone 就会拒绝身份。
func BuildIdentificationInformation(id Identity) (*csm.Frame, error) {
	params := csm.NewList()

	for _, spec := range []struct {
		id       uint16
		value    string
		required bool
	}{
		{endpoint.IdentName, id.Name, true},
		{endpoint.IdentModelIdentifier, id.ModelIdentifier, true},
		{endpoint.IdentManufacturer, id.Manufacturer, true},
		{endpoint.IdentSerialNumber, id.SerialNumber, true},
		{endpoint.IdentFirmwareVersion, id.FirmwareVersion, true},
		{endpoint.IdentHardwareVersion, id.HardwareVersion, true},
		{endpoint.IdentCurrentLanguage, id.CurrentLanguage, true},
	} {
		if spec.value == "" && spec.required {
			return nil, fmt.Errorf("session: 参数 0x%04x 是必填字段", spec.id)
		}
		encoded, err := csm.String(spec.value)
		if err != nil {
			return nil, err
		}
		params.Append(csm.MustParameter(spec.id, encoded))
	}

	if len(id.SupportedLanguages) == 0 {
		return nil, errors.New("session: 至少要声明一种支持的语言")
	}
	langs, err := csm.Strings(id.SupportedLanguages...)
	if err != nil {
		return nil, err
	}
	params.Append(csm.MustParameter(endpoint.IdentSupportedLanguages, langs))

	params.Append(
		csm.MustParameter(endpoint.IdentMessagesSentByAccessory, csm.U16List(id.SentMessages...)),
		csm.MustParameter(endpoint.IdentMessagesReceivedFromDevice, csm.U16List(id.ReceivedMessages...)),
	)

	// USB 传输组件：出现即表示支持。
	if id.USBDeviceTransport {
		usb := csm.NewList(
			csm.MustParameter(endpoint.ComponentID, csm.U16(1)),
			csm.MustParameter(endpoint.ComponentName, csm.MustString("USB Host")),
			csm.MustParameter(endpoint.ComponentSupported, nil),
		)
		body, err := usb.Encode()
		if err != nil {
			return nil, err
		}
		params.Append(csm.MustParameter(endpoint.IdentUSBHostTransport, body))
	}

	// 蓝牙传输组件：无线 CarPlay 的入口，需带本端 MAC。
	if len(id.BluetoothMAC) > 0 {
		bt := csm.NewList(
			csm.MustParameter(endpoint.ComponentID, csm.U16(2)),
			csm.MustParameter(endpoint.ComponentName, csm.MustString("Bluetooth")),
			csm.MustParameter(endpoint.ComponentSupported, nil),
			csm.MustParameter(endpoint.BluetoothMAC, id.BluetoothMAC),
		)
		if id.BluetoothTransportName != "" {
			bt.Append(csm.MustParameter(endpoint.BluetoothTransportName,
				csm.MustString(id.BluetoothTransportName)))
		}
		body, err := bt.Encode()
		if err != nil {
			return nil, err
		}
		params.Append(csm.MustParameter(endpoint.IdentBluetoothTransport, body))
	}

	// 无线 CarPlay 传输组件。
	if id.WirelessCarPlay {
		wireless := csm.NewList(
			csm.MustParameter(endpoint.ComponentID, csm.U16(3)),
			csm.MustParameter(endpoint.ComponentSupported, nil),
		)
		body, err := wireless.Encode()
		if err != nil {
			return nil, err
		}
		params.Append(csm.MustParameter(endpoint.IdentWirelessCarPlayTransport, body))
	}

	// HID 组件：可重复参数，每个组件一条。
	for _, component := range id.HIDComponents {
		hid := csm.NewList(
			csm.MustParameter(endpoint.ComponentID, csm.U16(component.ID)),
			csm.MustParameter(endpoint.ComponentName, csm.MustString(component.Name)),
			csm.MustParameter(endpoint.HIDComponentFunction, csm.U8(component.Function)),
		)
		body, err := hid.Encode()
		if err != nil {
			return nil, err
		}
		params.Append(csm.MustParameter(endpoint.IdentIAP2HIDComponent, body))
	}

	return csm.NewFrameOf(endpoint.IdentificationInformation, params)
}

// BuildAuthenticationCertificate 构造 0xaa01，把 MFi 芯片里的证书交给 iPhone。
func BuildAuthenticationCertificate(certificate []byte) (*csm.Frame, error) {
	if len(certificate) == 0 {
		return nil, errors.New("session: 证书为空")
	}
	return csm.NewFrameOf(endpoint.AuthenticationCertificate, csm.NewList(
		csm.MustParameter(endpoint.AuthCertificateSerial, certificate),
	))
}

// ParseChallenge 从 0xaa02 取出 iPhone 下发的挑战数据，交给 MFi 芯片签名。
func ParseChallenge(frame *csm.Frame) ([]byte, error) {
	if frame.MessageID != endpoint.RequestAuthenticationChallengeResponse {
		return nil, fmt.Errorf("%w: 期望 0x%04x，收到 0x%04x",
			ErrUnexpectedMessage, endpoint.RequestAuthenticationChallengeResponse, frame.MessageID)
	}
	params, err := frame.Params()
	if err != nil {
		return nil, err
	}
	p, err := params.Get(endpoint.AuthChallengeData)
	if err != nil {
		return nil, err
	}
	return p.Payload(), nil
}

// BuildAuthenticationResponse 构造 0xaa03，回传 MFi 芯片对挑战的签名。
func BuildAuthenticationResponse(signature []byte) (*csm.Frame, error) {
	if len(signature) == 0 {
		return nil, errors.New("session: 签名为空")
	}
	return csm.NewFrameOf(endpoint.AuthenticationResponse, csm.NewList(
		csm.MustParameter(endpoint.AuthCertificateSerial, signature),
	))
}

// BuildCarPlayStartSession 构造 0x4301，请求启动 CarPlay 会话。
//
// localAddress 是车机在这一侧的地址：有线模式用 NCM 网卡上的 IPv6，
// 无线模式用蓝牙标识。
func BuildCarPlayStartSession(localAddress string, wireless bool) (*csm.Frame, error) {
	if localAddress == "" {
		return nil, errors.New("session: 启动会话必须带上本机地址")
	}
	group := csm.NewList(csm.MustParameter(0, csm.MustString(localAddress)))
	body, err := group.Encode()
	if err != nil {
		return nil, err
	}
	paramID := endpoint.CarPlayStartWired
	if wireless {
		paramID = endpoint.CarPlayStartWireless
	}
	return csm.NewFrameOf(endpoint.CarPlayStartSession, csm.NewList(
		csm.MustParameter(paramID, body),
	))
}

// BuildStartHID 构造 0x6800，开启 HID 通道（触摸回传的前提）。
func BuildStartHID(componentID uint16) (*csm.Frame, error) {
	return csm.NewFrameOf(endpoint.HIDStartHandler, csm.NewList(
		csm.MustParameter(endpoint.ComponentID, csm.U16(componentID)),
	))
}

// CarPlayAvailability 是 iPhone 上报的可用性信息。
type CarPlayAvailability struct {
	WiredAvailable      bool
	USBIdentifier       string
	WirelessAvailable   bool
	BluetoothIdentifier string
}

// ParseCarPlayAvailability 解析 0x4300。
func ParseCarPlayAvailability(frame *csm.Frame) (*CarPlayAvailability, error) {
	if frame.MessageID != endpoint.CarPlayAvailability {
		return nil, fmt.Errorf("%w: 期望 0x%04x，收到 0x%04x",
			ErrUnexpectedMessage, endpoint.CarPlayAvailability, frame.MessageID)
	}
	params, err := frame.Params()
	if err != nil {
		return nil, err
	}
	out := &CarPlayAvailability{}
	if p := params.First(endpoint.CarPlayWired); p != nil {
		group, err := csm.ParseList(p.Payload())
		if err != nil {
			return nil, err
		}
		if v := group.First(0); v != nil {
			if flag, err := csm.ReadU8(v.Payload(), 0); err == nil {
				out.WiredAvailable = flag != 0
			}
		}
		if v := group.First(1); v != nil {
			out.USBIdentifier, _ = csm.ReadString(v.Payload())
		}
	}
	if p := params.First(endpoint.CarPlayWireless); p != nil {
		group, err := csm.ParseList(p.Payload())
		if err != nil {
			return nil, err
		}
		if v := group.First(0); v != nil {
			if flag, err := csm.ReadU8(v.Payload(), 0); err == nil {
				out.WirelessAvailable = flag != 0
			}
		}
		if v := group.First(1); v != nil {
			out.BluetoothIdentifier, _ = csm.ReadString(v.Payload())
		}
	}
	return out, nil
}

// MessageRole 表示一条消息在握手流程里的角色。
type MessageRole int

const (
	RoleOther MessageRole = iota
	RoleIdentificationAccepted
	RoleIdentificationRejected
	RoleCertificateRequested
	RoleChallengeReceived
	RoleAuthenticationSucceeded
	RoleAuthenticationFailed
	RoleCarPlayAvailable
	RoleSessionStopped
)

// Classify 判定消息角色，便于上层写状态机。
func Classify(frame *csm.Frame) MessageRole {
	switch frame.MessageID {
	case endpoint.IdentificationAccepted:
		return RoleIdentificationAccepted
	case endpoint.IdentificationRejected:
		return RoleIdentificationRejected
	case endpoint.RequestAuthenticationCertificate:
		return RoleCertificateRequested
	case endpoint.RequestAuthenticationChallengeResponse:
		return RoleChallengeReceived
	case endpoint.AuthenticationSucceeded:
		return RoleAuthenticationSucceeded
	case endpoint.AuthenticationFailed:
		return RoleAuthenticationFailed
	case endpoint.CarPlayAvailability:
		return RoleCarPlayAvailable
	case endpoint.CarPlaySessionStopped:
		return RoleSessionStopped
	default:
		return RoleOther
	}
}

// SplitForLink 把一条 CSM 消息切成适合链路上传输的块。
func SplitForLink(frame *csm.Frame, chunkSize int) ([][]byte, error) {
	if chunkSize <= 0 {
		chunkSize = link.MaxPayloadBytes
	}
	complete := frame.Encode()
	if len(complete) > link.MaxWireFrameBytes {
		return nil, fmt.Errorf("session: CSM 帧 %d 字节，超过链路帧上限", len(complete))
	}
	var chunks [][]byte
	for offset := 0; offset < len(complete); offset += chunkSize {
		end := offset + chunkSize
		if end > len(complete) {
			end = len(complete)
		}
		chunks = append(chunks, complete[offset:end])
	}
	return chunks, nil
}
