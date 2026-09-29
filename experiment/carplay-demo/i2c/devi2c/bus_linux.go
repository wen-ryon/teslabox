//go:build linux

// Package devi2c 通过 Linux 的 /dev/i2c-N 字符设备访问 I2C 总线。
//
// 这是量产形态：MFi 认证芯片直接焊在板子的 I2C 引脚上，不需要 CH341 转接板，
// 也不需要 libusb 和 cgo——内核已经把总线抽象成文件描述符了。
//
// 实现用的是 I2C_RDWR ioctl（一次提交多条消息），因此每次事务都可以指定
// 从机地址，不必先 ioctl(I2C_SLAVE) 锁定地址，多个不同地址的器件可以共用同一个 Bus。
package devi2c

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"github.com/wen-ryon/teslabox/experiment/carplay-demo/i2c"
)

// ioctl 命令与标志位，取自 linux/i2c-dev.h。
const (
	i2cRDWR  = 0x0707 // struct i2c_rdwr_ioctl_data
	i2cMRead = 0x0001 // 读方向（否则为写）

	// maxMessages 是一次 ioctl 允许携带的消息数上限（内核 I2C_RDWR 限制 42）。
	maxMessages = 42
)

// i2cMsg 对应内核的 struct i2c_msg。
// 字段顺序和显式填充必须与 C 结构体一致，否则 ioctl 会读到错位的数据。
type i2cMsg struct {
	addr  uint16 // 从机地址（7 位）
	flags uint16 // i2cMRead 等
	len   uint16 // 数据字节数
	_     uint16 // C 结构体按指针宽度对齐产生的填充
	buf   uintptr
}

// i2cRdwrIoctlData 对应内核的 struct i2c_rdwr_ioctl_data。
type i2cRdwrIoctlData struct {
	msgs  uintptr
	nmsgs uint32
	_     uint32 // 填充到 8 字节对齐
}

// Bus 是一条已经打开的 I2C 总线。
type Bus struct {
	file *os.File
	path string
}

// Open 打开形如 /dev/i2c-1 的总线设备。
func Open(path string) (*Bus, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("devi2c: 打开 %s 失败: %w", path, err)
	}
	return &Bus{file: f, path: path}, nil
}

// OpenNumber 按总线编号打开，例如 1 对应 /dev/i2c-1。
func OpenNumber(number int) (*Bus, error) {
	return Open(fmt.Sprintf("/dev/i2c-%d", number))
}

// Path 返回设备路径。
func (b *Bus) Path() string { return b.path }

// Close 关闭总线。
func (b *Bus) Close() error { return b.file.Close() }

// Transaction 实现 i2c.Transport。
//
// 写阶段和读阶段作为两条 i2c_msg 在同一次 ioctl 里提交，中间由内核自动产生
// 重复起始条件（repeated start），不会被别的主机抢走总线。
func (b *Bus) Transaction(addr7 uint8, write []byte, readLen int) ([]byte, error) {
	if len(write) == 0 && readLen == 0 {
		return nil, errors.New("devi2c: 事务既没有写也没有读")
	}
	if readLen < 0 {
		return nil, fmt.Errorf("devi2c: 读取长度不能为负: %d", readLen)
	}
	if readLen > 8192 {
		// 内核单条消息的缓冲区上限是 8192 字节。
		return nil, fmt.Errorf("devi2c: 单次读取 %d 字节超出内核上限 8192", readLen)
	}

	var (
		msgs     []i2cMsg
		writeBuf []byte
		readBuf  []byte
	)

	if len(write) > 0 {
		writeBuf = make([]byte, len(write))
		copy(writeBuf, write)
		msgs = append(msgs, i2cMsg{
			addr:  uint16(addr7),
			flags: 0,
			len:   uint16(len(writeBuf)),
			buf:   uintptr(unsafe.Pointer(&writeBuf[0])),
		})
	}
	if readLen > 0 {
		readBuf = make([]byte, readLen)
		msgs = append(msgs, i2cMsg{
			addr:  uint16(addr7),
			flags: i2cMRead,
			len:   uint16(readLen),
			buf:   uintptr(unsafe.Pointer(&readBuf[0])),
		})
	}
	if len(msgs) > maxMessages {
		return nil, fmt.Errorf("devi2c: 消息数 %d 超过上限 %d", len(msgs), maxMessages)
	}

	data := i2cRdwrIoctlData{
		msgs:  uintptr(unsafe.Pointer(&msgs[0])),
		nmsgs: uint32(len(msgs)),
	}

	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		b.file.Fd(),
		uintptr(i2cRDWR),
		uintptr(unsafe.Pointer(&data)),
	)
	// ioctl 期间内核直接读写这两个切片，必须保证它们在调用返回前不被回收。
	runtime.KeepAlive(writeBuf)
	runtime.KeepAlive(readBuf)
	runtime.KeepAlive(msgs)

	if errno != 0 {
		return nil, translateErrno(errno, addr7)
	}
	return readBuf, nil
}

// translateErrno 把内核错误码翻译成 i2c 包的错误类型。
//
// ENXIO 表示这个地址上没有器件应答——对 MFi 芯片来说，
// 休眠时的第一次访问同样会表现为 ENXIO，所以上层必须重试而不是直接判死。
func translateErrno(errno syscall.Errno, addr7 uint8) error {
	switch errno {
	case syscall.ENXIO:
		return &i2c.NackError{Addr: addr7, Byte: 0, Status: 0}
	case syscall.EBUSY:
		return fmt.Errorf("devi2c: 总线忙（地址 0x%02x）: %w", addr7, errno)
	case syscall.EIO:
		return fmt.Errorf("devi2c: I/O 失败（地址 0x%02x，可能是器件未就绪）: %w", addr7, errno)
	default:
		return fmt.Errorf("devi2c: ioctl 失败（地址 0x%02x）: %w", addr7, errno)
	}
}

// Enumerate 列出系统上的 I2C 总线编号，按数字升序。
// 用于在没有硬件文档时快速找到芯片挂在哪条总线上。
func Enumerate() ([]int, error) {
	matches, err := filepath.Glob("/dev/i2c-*")
	if err != nil {
		return nil, err
	}
	var numbers []int
	for _, path := range matches {
		suffix := strings.TrimPrefix(path, "/dev/i2c-")
		n, err := strconv.Atoi(suffix)
		if err != nil {
			continue // /dev/i2c-X 这类非数字后缀跳过
		}
		numbers = append(numbers, n)
	}
	sort.Ints(numbers)
	return numbers, nil
}

// Detect 在指定总线上找一颗会应答的器件，返回其 7 位地址。
//
// 只探测 MFi 芯片常用的两个地址。注意休眠的芯片第一次访问会 NACK，
// 因此每个地址都试 probeAttempts 次。
func Detect(b *Bus, candidates []uint8, probeAttempts int) (uint8, error) {
	if len(candidates) == 0 {
		candidates = []uint8{0x10, 0x11}
	}
	if probeAttempts <= 0 {
		probeAttempts = 3
	}
	for _, addr := range candidates {
		for attempt := 0; attempt < probeAttempts; attempt++ {
			if _, err := b.Transaction(addr, []byte{0x00}, 1); err == nil {
				return addr, nil
			}
		}
	}
	return 0, i2c.ErrNoDevice
}
