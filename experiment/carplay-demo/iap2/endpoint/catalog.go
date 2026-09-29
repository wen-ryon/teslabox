// Package endpoint 收录 iAP2 的端点（即 CSM 消息 ID）与常用参数 ID。
//
// 对照 xcertplay 的 iap2/catalog/Iap2Endpoints。这里只登记本项目要用到的部分，
// 以及完整的 Identification 参数集——因为握手第一步就得靠它自报家门。
//
// 方向的含义：
//
//	IPHONE_TO_ACCESSORY   iPhone 发给车机（车机要能解析）
//	ACCESSORY_TO_IPHONE   车机发给 iPhone（车机要能构造）
package endpoint

import "fmt"

// Direction 描述一条消息允许谁来发。
type Direction int

const (
	// ToAccessory 是 iPhone 发给配件（车机）的消息。
	ToAccessory Direction = iota
	// ToIPhone 是配件（车机）发给 iPhone 的消息。
	ToIPhone
	// Bidirectional 双方都可以发。
	Bidirectional
)

// Endpoint 是一条 CSM 消息的元信息。
type Endpoint struct {
	ID        uint16
	Name      string
	Feature   string
	Direction Direction
}

// ---------- Identification（0x1dxx）：握手第一阶段 ----------

const (
	StartIdentification             uint16 = 0x1d00 // iPhone → 车机，要求自报家门
	IdentificationInformation       uint16 = 0x1d01 // 车机 → iPhone，上报能力
	IdentificationAccepted          uint16 = 0x1d02 // iPhone → 车机，身份接受
	IdentificationRejected          uint16 = 0x1d03 // 双向，身份拒绝
	CancelIdentification            uint16 = 0x1d05 // 车机 → iPhone，取消
	IdentificationInformationUpdate uint16 = 0x1d06 // 车机 → iPhone，能力更新
)

// IdentificationInformation 的参数 ID。
//
// 这些是车机向 iPhone 声明「我是谁、我能干什么」的关键字段，
// 少填或填错会导致 iPhone 直接拒绝建立 CarPlay 会话。
const (
	IdentName                       uint16 = 0 // 配件名称，例如 "TeslaBox"
	IdentModelIdentifier            uint16 = 1 // 型号标识
	IdentManufacturer               uint16 = 2 // 制造商
	IdentSerialNumber               uint16 = 3 // 序列号
	IdentFirmwareVersion            uint16 = 4 // 固件版本
	IdentHardwareVersion            uint16 = 5 // 硬件版本
	IdentMessagesSentByAccessory    uint16 = 6 // 车机会发的消息 ID 清单（u16 数组）
	IdentMessagesReceivedFromDevice uint16 = 7 // 车机能收的消息 ID 清单（u16 数组）
	IdentPowerProvidingCapability   uint16 = 8
	IdentMaximumCurrentDrawn        uint16 = 9
	IdentExternalAccessoryProtocols uint16 = 10
	IdentCurrentLanguage            uint16 = 12 // 当前语言，例如 "en"
	IdentSupportedLanguages         uint16 = 13 // 支持的语言列表
	IdentUSBHostTransport           uint16 = 16
	IdentBluetoothTransport         uint16 = 17
	IdentIAP2HIDComponent           uint16 = 18 // 可重复：声明 HID 组件（触摸、按键）
	IdentVehicleInformation         uint16 = 20
	IdentVehicleStatus              uint16 = 21
	IdentLocationInformation        uint16 = 22
	IdentWirelessCarPlayTransport   uint16 = 24
)

// IdentificationInformation 内嵌分组的字段 ID（复用同一套参数命名空间）。
const (
	ComponentID        uint16 = 0
	ComponentName      uint16 = 1
	ComponentSupported uint16 = 2 // VOID：出现即表示支持
	ComponentAvailable uint16 = 4 // VOID：出现即表示可用
)

// USB 传输组件（参数 16）的子字段。
const (
	USBInterfaceNumber uint16 = 3
)

// 蓝牙传输组件（参数 17）的子字段。
const (
	BluetoothMAC           uint16 = 3
	BluetoothTransportName uint16 = 4
)

// iAP2 HID 组件（参数 18）的子字段。
const (
	HIDComponentFunction uint16 = 2
)

// 无线 CarPlay 传输组件（参数 24）的子字段。
const (
	WirelessSSID               uint16 = 1
	WirelessTransportIdentifer uint16 = 3
	WirelessIsEnabled          uint16 = 5
)

// ---------- Authentication（0xaaxx）：握手第二阶段 ----------

const (
	RequestAuthenticationCertificate       uint16 = 0xaa00 // iPhone → 车机，索要证书
	AuthenticationCertificate              uint16 = 0xaa01 // 车机 → iPhone，回传证书
	RequestAuthenticationChallengeResponse uint16 = 0xaa02 // iPhone → 车机，下发挑战
	AuthenticationResponse                 uint16 = 0xaa03 // 车机 → iPhone，回传签名
	AuthenticationFailed                   uint16 = 0xaa04
	AuthenticationSucceeded                uint16 = 0xaa05
	RequestForLocalAccessoryGuidance       uint16 = 0xaa06
)

// AuthenticationCertificate / ChallengeResponse 的参数 ID。
const (
	AuthCertificateSerial uint16 = 0 // 证书本体（PKCS#7）
	AuthChallengeData     uint16 = 0 // 挑战数据
)

// ---------- CarPlay（0x43xx） ----------

const (
	CarPlayAvailability   uint16 = 0x4300 // iPhone → 车机，告知可用性
	CarPlayStartSession   uint16 = 0x4301 // 车机 → iPhone，起会话（携带 IP）
	CarPlayStopSession    uint16 = 0x4302
	CarPlaySessionStopped uint16 = 0x4303
)

// CarPlayAvailability 的参数 ID。
const (
	CarPlayWired       uint16 = 0
	CarPlayWireless    uint16 = 1
	CarPlayThemeAssets uint16 = 2
)

// CarPlayStartSession 的参数 ID。
const (
	CarPlayStartWired    uint16 = 0
	CarPlayStartWireless uint16 = 1
)

// ---------- HID（0x68xx）：触摸与按键回传 ----------

const (
	HIDStartHandler    uint16 = 0x6800 // 车机 → iPhone，开启 HID
	HIDOutReport       uint16 = 0x6801 // iPhone → 车机
	HIDAccessoryReport uint16 = 0x6802 // 车机 → iPhone，上报触摸/按键
	HIDStopHandler     uint16 = 0x6803
	HIDGetReport       uint16 = 0x6804
	HIDComponentUpdate uint16 = 0x6807
)

// HID 组件的功能类型。
const (
	HIDFunctionTouchScreen uint8 = 0x01
	HIDFunctionKnob        uint8 = 0x02
	HIDFunctionButton      uint8 = 0x03
)

// ---------- Power（0xaexx） ----------

const (
	StartPowerUpdates     uint16 = 0xae00
	PowerUpdate           uint16 = 0xae01
	StopPowerUpdates      uint16 = 0xae02
	PowerSourceUpdate     uint16 = 0xae03
	AccessoryPowerUpdate  uint16 = 0xae05
	AccessoryResetCurrent uint16 = 0xae06
)

// ---------- Location / NowPlaying / MediaLibrary ----------

const (
	StartLocationUpdates uint16 = 0x4e00
	LocationInformation  uint16 = 0x4e01
	StopLocationUpdates  uint16 = 0x4e02
	VehicleInformation   uint16 = 0x4e09
	VehicleStatus        uint16 = 0x4e0a
	NightModeHint        uint16 = 0x4e0c

	NowPlayingUpdate        uint16 = 0x5001
	MediaLibraryInformation uint16 = 0x4c01
	MediaLibraryUpdate      uint16 = 0x4c04
)

// ---------- 蓝牙带外配对（0x00Bx / 0xB1xx）：无线 CarPlay 用 ----------

const (
	OOBPairingStartPairing   uint16 = 0x00b0
	OOBPairingAccessoryInfo  uint16 = 0x00b1
	OOBPairingLinkKeyInfo    uint16 = 0x00b2
	OOBPairingCompletionInfo uint16 = 0x00b3
	OOBPairingStopPairing    uint16 = 0x00b4
	BLEPairingStateUpdate    uint16 = 0xb102
	BLEPairingAccessoryInfo  uint16 = 0xb103
	BLEPairingAccessoryData  uint16 = 0xb105
	BLEPairingStopUpdates    uint16 = 0xb107
)

// Known 是已登记端点的完整清单。
var Known = []Endpoint{
	{StartIdentification, "StartIdentification", "Identification", ToAccessory},
	{IdentificationInformation, "IdentificationInformation", "Identification", ToIPhone},
	{IdentificationAccepted, "IdentificationAccepted", "Identification", ToAccessory},
	{IdentificationRejected, "IdentificationRejected", "Identification", Bidirectional},
	{CancelIdentification, "CancelIdentification", "Identification", ToIPhone},
	{IdentificationInformationUpdate, "IdentificationInformationUpdate", "Identification", ToIPhone},

	{RequestAuthenticationCertificate, "RequestAuthenticationCertificate", "Authentication", ToAccessory},
	{AuthenticationCertificate, "AuthenticationCertificate", "Authentication", ToIPhone},
	{RequestAuthenticationChallengeResponse, "RequestAuthenticationChallengeResponse", "Authentication", ToAccessory},
	{AuthenticationResponse, "AuthenticationResponse", "Authentication", ToIPhone},

	{CarPlayAvailability, "CarPlayAvailability", "CarPlay", ToAccessory},
	{CarPlayStartSession, "CarPlayStartSession", "CarPlay", ToIPhone},
	{CarPlayStopSession, "CarPlayStopSession", "CarPlay", ToIPhone},
	{CarPlaySessionStopped, "CarPlaySessionStopped", "CarPlay", ToAccessory},

	{HIDStartHandler, "hid_StartHIDMsgHandler", "HID", ToIPhone},
	{HIDOutReport, "hid_OutReport", "HID", ToAccessory},
	{HIDAccessoryReport, "hid_AccessoryHIDReportHandler", "HID", ToIPhone},
	{HIDStopHandler, "hid_StopHIDMsgHandler", "HID", ToIPhone},
	{HIDComponentUpdate, "hid_ComponentUpdate", "HID", ToAccessory},

	{StartPowerUpdates, "StartPowerUpdates", "Power", ToIPhone},
	{PowerUpdate, "PowerUpdate", "Power", ToAccessory},
	{StopPowerUpdates, "StopPowerUpdates", "Power", ToIPhone},
	{AccessoryPowerUpdate, "AccessoryPowerUpdate", "Power", ToIPhone},

	{StartLocationUpdates, "StartLocationUpdates", "Location", ToIPhone},
	{LocationInformation, "LocationInformation", "Location", ToIPhone},
	{StopLocationUpdates, "StopLocationUpdates", "Location", ToIPhone},
	{NowPlayingUpdate, "NowPlayingUpdate", "NowPlaying", ToAccessory},
}

var byID = func() map[uint16]Endpoint {
	m := make(map[uint16]Endpoint, len(Known))
	for _, e := range Known {
		m[e.ID] = e
	}
	return m
}()

// Lookup 按消息 ID 查端点，未登记时返回 nil。
func Lookup(id uint16) *Endpoint {
	if e, ok := byID[id]; ok {
		return &e
	}
	return nil
}

// Name 返回消息 ID 的人类可读名字，未登记时返回十六进制形式。
func Name(id uint16) string {
	if e := Lookup(id); e != nil {
		return e.Name
	}
	return fmt.Sprintf("0x%04x", id)
}

// Filter 返回指定功能域的全部端点。
func Filter(feature string) []Endpoint {
	var out []Endpoint
	for _, e := range Known {
		if e.Feature == feature {
			out = append(out, e)
		}
	}
	return out
}
