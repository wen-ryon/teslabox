package driveinit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/wen-ryon/teslabox/internal/system"
)

const minimumDriveSize = 32 * 1000 * 1000 * 1000

var partitionStablePathPattern = regexp.MustCompile(`-part[0-9]+$`)

// Options 保存一次数据盘初始化所需的全部显式约束。
type Options struct {
	Device        string
	ConfirmSerial string
	Label         string
	MountPath     string
	Execute       bool
}

// Device 描述初始化前由内核识别出的目标整盘。
type Device struct {
	RequestedPath string
	ResolvedPath  string
	Type          string
	Transport     string
	Model         string
	Serial        string
	Size          int64
	FileSystem    string
	UUID          string
	Label         string
	MountPoint    string
	Children      []Device
}

// Command 是无需经过 shell 拼接的外部命令。
type Command struct {
	Name string
	Args []string
}

// Result 返回经过身份核验的设备和实际执行计划。
type Result struct {
	Device      Device
	Partition   string
	Commands    []Command
	WasExecuted bool
}

// Inspector 隔离符号链接解析和块设备信息读取，避免测试接触真实磁盘。
type Inspector interface {
	Inspect(ctx context.Context, stablePath string) (Device, error)
}

// LSBLKInspector 使用系统块设备信息核对稳定路径指向的真实磁盘。
type LSBLKInspector struct {
	runner system.Runner
}

// NewLSBLKInspector 创建只读的块设备检查器。
func NewLSBLKInspector(runner system.Runner) *LSBLKInspector {
	return &LSBLKInspector{runner: runner}
}

// Inspect 解析稳定路径，并只读取该路径对应的设备树。
func (i *LSBLKInspector) Inspect(ctx context.Context, stablePath string) (Device, error) {
	resolved, err := filepath.EvalSymlinks(stablePath)
	if err != nil {
		return Device{}, fmt.Errorf("解析设备稳定路径: %w", err)
	}

	output, err := i.runner.Run(ctx, "lsblk", "--json", "--tree", "--bytes", "--output", "PATH,TYPE,SIZE,TRAN,MODEL,SERIAL,FSTYPE,UUID,LABEL,MOUNTPOINT", resolved)
	if err != nil {
		return Device{}, fmt.Errorf("读取目标块设备: %w", err)
	}

	var response struct {
		BlockDevices []lsblkDevice `json:"blockdevices"`
	}
	if err := json.Unmarshal(output, &response); err != nil {
		return Device{}, fmt.Errorf("解析 lsblk 输出: %w", err)
	}
	if len(response.BlockDevices) != 1 {
		return Device{}, fmt.Errorf("目标路径应只对应一个块设备，实际为 %d 个", len(response.BlockDevices))
	}

	device := response.BlockDevices[0].device()
	device.RequestedPath = stablePath
	device.ResolvedPath = resolved
	return device, nil
}

type lsblkDevice struct {
	Path       string        `json:"path"`
	Type       string        `json:"type"`
	Size       int64         `json:"size"`
	Transport  string        `json:"tran"`
	Model      string        `json:"model"`
	Serial     string        `json:"serial"`
	FileSystem string        `json:"fstype"`
	UUID       string        `json:"uuid"`
	Label      string        `json:"label"`
	MountPoint *string       `json:"mountpoint"`
	Children   []lsblkDevice `json:"children"`
}

func (d lsblkDevice) device() Device {
	device := Device{
		ResolvedPath: strings.TrimSpace(d.Path),
		Type:         strings.TrimSpace(d.Type),
		Transport:    strings.TrimSpace(d.Transport),
		Model:        strings.TrimSpace(d.Model),
		Serial:       strings.TrimSpace(d.Serial),
		Size:         d.Size,
		FileSystem:   strings.TrimSpace(d.FileSystem),
		UUID:         strings.TrimSpace(d.UUID),
		Label:        strings.TrimSpace(d.Label),
	}
	if d.MountPoint != nil {
		device.MountPoint = strings.TrimSpace(*d.MountPoint)
	}
	for _, child := range d.Children {
		device.Children = append(device.Children, child.device())
	}
	return device
}

// Initializer 把破坏性磁盘操作限制在完成身份核验之后。
type Initializer struct {
	inspector Inspector
	runner    system.Runner
}

// New 创建显式依赖检查器和命令执行器的数据盘初始化器。
func New(inspector Inspector, runner system.Runner) *Initializer {
	return &Initializer{inspector: inspector, runner: runner}
}

// Run 默认仅生成计划，只有 Execute 为真时才执行破坏性命令。
func (i *Initializer) Run(ctx context.Context, options Options) (Result, error) {
	if err := validateOptions(options); err != nil {
		return Result{}, err
	}

	device, err := i.inspector.Inspect(ctx, options.Device)
	if err != nil {
		return Result{}, err
	}
	if err := validateDevice(device, options.ConfirmSerial); err != nil {
		return Result{}, err
	}

	partition := firstPartitionPath(device.ResolvedPath)
	commands := buildCommands(device.ResolvedPath, partition, options)
	result := Result{Device: device, Partition: partition, Commands: commands}
	if !options.Execute {
		return result, nil
	}

	for _, command := range commands {
		if _, err := i.runner.Run(ctx, command.Name, command.Args...); err != nil {
			return result, fmt.Errorf("初始化数据盘步骤 %s 失败: %w", command.Name, err)
		}
	}
	result.WasExecuted = true
	return result, nil
}

func validateOptions(options Options) error {
	if !IsWholeDiskStablePath(options.Device) {
		return errors.New("device 必须是 /dev/disk/by-id/ 下的整盘稳定路径")
	}
	if strings.TrimSpace(options.ConfirmSerial) == "" {
		return errors.New("confirm-serial 不能为空")
	}
	if strings.TrimSpace(options.Label) == "" || strings.ContainsAny(options.Label, " /\\\t\r\n") {
		return errors.New("label 必须是非空且不含空白或路径分隔符的文件系统标签")
	}
	if len(options.Label) > 12 {
		return errors.New("label 不能超过 XFS 允许的 12 字节")
	}
	if !path.IsAbs(options.MountPath) || path.Clean(options.MountPath) == "/" {
		return errors.New("mount-path 必须是非根目录的绝对路径")
	}
	return nil
}

func validateDevice(device Device, expectedSerial string) error {
	if device.Type != "disk" {
		return fmt.Errorf("目标不是整盘: type=%q", device.Type)
	}
	if device.Serial != expectedSerial {
		return fmt.Errorf("设备序列号不匹配: expected=%q actual=%q", expectedSerial, device.Serial)
	}
	if device.Size < minimumDriveSize {
		return fmt.Errorf("目标容量异常偏小: %d bytes", device.Size)
	}
	if unsafeUse, reason := findUnsafeUse(device); unsafeUse {
		return fmt.Errorf("目标设备正在被系统使用: %s", reason)
	}
	return nil
}

// IsWholeDiskStablePath 判断路径是否为 /dev/disk/by-id 下的整盘链接。
// 初始化只接受稳定链接，避免 /dev/sdX 等会在重启或插拔后改变的临时设备名。
func IsWholeDiskStablePath(name string) bool {
	cleaned := path.Clean(name)
	root := "/dev/disk/by-id"
	if !strings.HasPrefix(cleaned, root+"/") || cleaned == root {
		return false
	}
	base := path.Base(cleaned)
	return base != "." && base != string(filepath.Separator) && !partitionStablePathPattern.MatchString(base)
}

func findUnsafeUse(device Device) (bool, string) {
	if device.MountPoint != "" {
		return true, fmt.Sprintf("%s 已挂载到 %s", device.ResolvedPath, device.MountPoint)
	}
	if device.FileSystem == "swap" {
		return true, fmt.Sprintf("%s 正作为 swap 使用", device.ResolvedPath)
	}
	for _, child := range device.Children {
		if unsafeUse, reason := findUnsafeUse(child); unsafeUse {
			return true, reason
		}
	}
	return false, ""
}

func firstPartitionPath(devicePath string) string {
	if devicePath == "" {
		return ""
	}
	last := devicePath[len(devicePath)-1]
	if last >= '0' && last <= '9' {
		return devicePath + "p1"
	}
	return devicePath + "1"
}

func buildCommands(devicePath, partitionPath string, options Options) []Command {
	directories := []string{
		path.Join(options.MountPath, "images"),
		path.Join(options.MountPath, "snapshots"),
		path.Join(options.MountPath, "index"),
		path.Join(options.MountPath, "state"),
	}
	return []Command{
		{Name: "wipefs", Args: []string{"--all", devicePath}},
		{Name: "parted", Args: []string{"--script", devicePath, "mklabel", "gpt"}},
		{Name: "parted", Args: []string{"--script", "--align", "optimal", devicePath, "mkpart", "primary", "xfs", "1MiB", "100%"}},
		{Name: "partprobe", Args: []string{devicePath}},
		{Name: "udevadm", Args: []string{"settle"}},
		{Name: "mkfs.xfs", Args: []string{"-f", "-m", "reflink=1", "-L", options.Label, partitionPath}},
		{Name: "mkdir", Args: []string{"-p", options.MountPath}},
		{Name: "mount", Args: []string{"-o", "noatime,nosuid,nodev", partitionPath, options.MountPath}},
		{Name: "mkdir", Args: append([]string{"-p"}, directories...)},
		{Name: "sync"},
	}
}
