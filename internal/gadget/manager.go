package gadget

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const languageEnglishUS = "0x409"

// Config 定义 TeslaBox 专属 USB Mass Storage Gadget 的固定身份和路径。
type Config struct {
	ConfigFSPath string
	UDCPath      string
	GadgetName   string
	UDCName      string
	ImagePath    string
	SerialNumber string
	// ReleaseGadget 只允许显式解绑同名冲突 Gadget，不会删除其 configfs 配置。
	ReleaseGadget string
}

// Binding 描述一个占用 UDC 的 Gadget。
type Binding struct {
	Gadget string `json:"gadget"`
	UDC    string `json:"udc"`
}

// Status 描述 TeslaBox Gadget 当前的 configfs 状态。
type Status struct {
	Exists        bool      `json:"exists"`
	Bound         bool      `json:"bound"`
	UDC           string    `json:"udc,omitempty"`
	UDCState      string    `json:"udc_state,omitempty"`
	CurrentSpeed  string    `json:"current_speed,omitempty"`
	HostConnected bool      `json:"host_connected"`
	ImagePath     string    `json:"image_path,omitempty"`
	SelectedUDC   string    `json:"selected_udc,omitempty"`
	OtherBindings []Binding `json:"other_bindings"`
}

// Manager 只管理固定名称的 TeslaBox Gadget，不接管其他 configfs 配置。
type Manager struct {
	config  Config
	symlink func(oldname, newname string) error
}

// NewManager 创建 USB Mass Storage Gadget 生命周期管理器。
func NewManager(config Config) *Manager {
	return &Manager{config: config, symlink: os.Symlink}
}

// Inspect 只读取 UDC 占用和 TeslaBox Gadget 状态。
func (m *Manager) Inspect() (Status, error) {
	if err := validateConfig(m.config); err != nil {
		return Status{}, err
	}
	selectedUDC, err := m.selectUDC()
	if err != nil {
		return Status{}, err
	}
	status := Status{SelectedUDC: selectedUDC, OtherBindings: []Binding{}}
	status.UDCState, err = readOptional(filepath.Join(m.config.UDCPath, selectedUDC, "state"))
	if err != nil {
		return Status{}, fmt.Errorf("读取 UDC 状态: %w", err)
	}
	status.CurrentSpeed, err = readOptional(filepath.Join(m.config.UDCPath, selectedUDC, "current_speed"))
	if err != nil {
		return Status{}, fmt.Errorf("读取 UDC 速率: %w", err)
	}
	status.HostConnected = status.UDCState != "" && status.UDCState != "not attached"
	entries, err := os.ReadDir(m.config.ConfigFSPath)
	if err != nil {
		return Status{}, fmt.Errorf("读取 USB Gadget configfs: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		udc, readErr := readTrimmed(filepath.Join(m.config.ConfigFSPath, name, "UDC"))
		if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
			return Status{}, fmt.Errorf("读取 Gadget %s 的 UDC: %w", name, readErr)
		}
		if name == m.config.GadgetName {
			status.Exists = true
			status.UDC = udc
			status.Bound = udc != ""
			image, imageErr := readTrimmed(m.lunPath("file"))
			if imageErr == nil {
				status.ImagePath = image
			} else if !errors.Is(imageErr, os.ErrNotExist) {
				return Status{}, fmt.Errorf("读取 TeslaBox Gadget backing file: %w", imageErr)
			}
			continue
		}
		if udc != "" {
			status.OtherBindings = append(status.OtherBindings, Binding{Gadget: name, UDC: udc})
		}
	}
	slices.SortFunc(status.OtherBindings, func(left, right Binding) int {
		return strings.Compare(left.Gadget, right.Gadget)
	})
	return status, nil
}

// Start 幂等创建并绑定 TeslaBox Gadget，UDC 始终在最后一步写入。
func (m *Manager) Start() (status Status, err error) {
	status, imagePath, err := m.PreflightStart()
	if err != nil {
		return Status{}, err
	}
	if status.Bound {
		if status.UDC == status.SelectedUDC && samePath(status.ImagePath, imagePath) {
			return status, nil
		}
		return status, errors.New("TeslaBox Gadget 已绑定但配置与期望不一致，拒绝自动替换")
	}
	released, err := m.releaseAllowedBindings(status)
	if err != nil {
		return status, err
	}
	restoreReleased := len(released) > 0
	defer func() {
		if err != nil && restoreReleased {
			for _, binding := range released {
				if restoreErr := writeValue(filepath.Join(m.config.ConfigFSPath, binding.Gadget, "UDC"), binding.UDC); restoreErr != nil {
					err = errors.Join(err, fmt.Errorf("恢复 Gadget %s: %w", binding.Gadget, restoreErr))
				}
			}
		}
	}()
	if status.Exists {
		if err := m.removeUnbound(); err != nil {
			return status, fmt.Errorf("清理未绑定的 TeslaBox Gadget: %w", err)
		}
	}

	created := false
	defer func() {
		if err != nil && created {
			if cleanupErr := m.removeOwned(); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("清理未完成 Gadget: %w", cleanupErr))
			}
		}
	}()
	if err = os.Mkdir(m.gadgetPath(), 0o755); err != nil {
		return status, fmt.Errorf("创建 TeslaBox Gadget: %w", err)
	}
	created = true
	for name, value := range map[string]string{
		"idVendor":  "0x0525",
		"idProduct": "0xa4a5",
		"bcdDevice": "0x0100",
		"bcdUSB":    "0x0200",
	} {
		if err = writeValue(filepath.Join(m.gadgetPath(), name), value); err != nil {
			return status, fmt.Errorf("写入 Gadget %s: %w", name, err)
		}
	}

	deviceStrings := filepath.Join(m.gadgetPath(), "strings", languageEnglishUS)
	configStrings := filepath.Join(m.gadgetPath(), "configs", "c.1", "strings", languageEnglishUS)
	functionPath := filepath.Join(m.gadgetPath(), "functions", "mass_storage.0")
	if err = os.MkdirAll(deviceStrings, 0o755); err != nil {
		return status, fmt.Errorf("创建设备字符串目录: %w", err)
	}
	if err = os.MkdirAll(configStrings, 0o755); err != nil {
		return status, fmt.Errorf("创建 Gadget 配置目录: %w", err)
	}
	if err = os.MkdirAll(filepath.Join(functionPath, "lun.0"), 0o755); err != nil {
		return status, fmt.Errorf("创建 Mass Storage function: %w", err)
	}
	values := []struct {
		name  string
		value string
	}{
		{name: filepath.Join(deviceStrings, "serialnumber"), value: m.config.SerialNumber},
		{name: filepath.Join(deviceStrings, "manufacturer"), value: "TeslaBox"},
		{name: filepath.Join(deviceStrings, "product"), value: "TeslaCam Drive"},
		{name: filepath.Join(configStrings, "configuration"), value: "TeslaCam Mass Storage"},
		{name: filepath.Join(m.gadgetPath(), "configs", "c.1", "bmAttributes"), value: "0x80"},
		{name: filepath.Join(m.gadgetPath(), "configs", "c.1", "MaxPower"), value: "250"},
		{name: filepath.Join(functionPath, "stall"), value: "1"},
		{name: m.lunPath("cdrom"), value: "0"},
		{name: m.lunPath("ro"), value: "0"},
		{name: m.lunPath("removable"), value: "1"},
		{name: m.lunPath("nofua"), value: "0"},
		// backing file 必须最后写入，否则内核会锁定 LUN 属性并返回 EBUSY。
		{name: m.lunPath("file"), value: imagePath},
	}
	for _, item := range values {
		if err = writeValue(item.name, item.value); err != nil {
			return status, fmt.Errorf("写入 Gadget 属性 %s: %w", item.name, err)
		}
	}
	linkPath := filepath.Join(m.gadgetPath(), "configs", "c.1", "mass_storage.0")
	if err = m.symlink(functionPath, linkPath); err != nil {
		return status, fmt.Errorf("关联 Mass Storage function: %w", err)
	}
	if err = writeValue(filepath.Join(m.gadgetPath(), "UDC"), status.SelectedUDC); err != nil {
		return status, fmt.Errorf("绑定 UDC: %w", err)
	}
	created = false
	restoreReleased = false
	return m.Inspect()
}

// PreflightStart 执行与真实启动相同的只读冲突和镜像检查。
func (m *Manager) PreflightStart() (Status, string, error) {
	status, err := m.Inspect()
	if err != nil {
		return Status{}, "", err
	}
	for _, binding := range status.OtherBindings {
		if binding.UDC == status.SelectedUDC {
			if binding.Gadget != m.config.ReleaseGadget {
				return status, "", fmt.Errorf("UDC %s 已被 Gadget %s 占用", binding.UDC, binding.Gadget)
			}
		}
	}
	imagePath, err := validateImage(m.config.ImagePath)
	if err != nil {
		return status, "", err
	}
	if loop, err := imageLoopDevice(imagePath); err != nil {
		return status, "", err
	} else if loop != "" {
		return status, "", fmt.Errorf("镜像正在被 loop 设备 %s 使用", loop)
	}
	return status, imagePath, nil
}

func (m *Manager) releaseAllowedBindings(status Status) ([]Binding, error) {
	released := make([]Binding, 0, 1)
	for _, binding := range status.OtherBindings {
		if binding.UDC != status.SelectedUDC || binding.Gadget != m.config.ReleaseGadget {
			continue
		}
		udcPath := filepath.Join(m.config.ConfigFSPath, binding.Gadget, "UDC")
		if err := writeValue(udcPath, ""); err != nil {
			return released, fmt.Errorf("释放 Gadget %s: %w", binding.Gadget, err)
		}
		released = append(released, binding)
	}
	return released, nil
}

// Stop 只解绑 TeslaBox Gadget，其他厂商或系统 Gadget 不受影响。
func (m *Manager) Stop() (Status, error) {
	if err := validateConfig(m.config); err != nil {
		return Status{}, err
	}
	if _, err := m.selectUDC(); err != nil {
		return Status{}, err
	}
	if _, err := os.Lstat(m.gadgetPath()); errors.Is(err, os.ErrNotExist) {
		return m.Inspect()
	} else if err != nil {
		return Status{}, fmt.Errorf("检查 TeslaBox Gadget: %w", err)
	}
	if err := m.removeOwned(); err != nil {
		return Status{}, err
	}
	return m.Inspect()
}

func (m *Manager) removeOwned() error {
	udcPath := filepath.Join(m.gadgetPath(), "UDC")
	if udc, err := readTrimmed(udcPath); err == nil {
		if udc != "" {
			if err := writeValue(udcPath, ""); err != nil {
				return fmt.Errorf("解绑 TeslaBox Gadget: %w", err)
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("检查 TeslaBox UDC 属性: %w", err)
	}
	return m.removeUnbound()
}

func (m *Manager) removeUnbound() error {
	linkPath := filepath.Join(m.gadgetPath(), "configs", "c.1", "mass_storage.0")
	if err := os.Remove(linkPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("移除 Mass Storage 链接: %w", err)
	}
	paths := []string{
		filepath.Join(m.gadgetPath(), "functions", "mass_storage.0"),
		filepath.Join(m.gadgetPath(), "configs", "c.1", "strings", languageEnglishUS),
		filepath.Join(m.gadgetPath(), "configs", "c.1"),
		filepath.Join(m.gadgetPath(), "strings", languageEnglishUS),
		m.gadgetPath(),
	}
	for _, name := range paths {
		remove := os.Remove
		if !strings.HasPrefix(filepath.ToSlash(filepath.Clean(m.config.ConfigFSPath)), "/sys/") {
			remove = os.RemoveAll
		}
		if err := remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("移除 configfs 路径 %s: %w", name, err)
		}
	}
	return nil
}

func (m *Manager) selectUDC() (string, error) {
	entries, err := os.ReadDir(m.config.UDCPath)
	if err != nil {
		return "", fmt.Errorf("读取 UDC: %w", err)
	}
	udcs := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			udcs = append(udcs, entry.Name())
		}
	}
	slices.Sort(udcs)
	if m.config.UDCName != "" {
		if slices.Contains(udcs, m.config.UDCName) {
			return m.config.UDCName, nil
		}
		return "", fmt.Errorf("指定 UDC %q 不存在", m.config.UDCName)
	}
	if len(udcs) != 1 {
		return "", fmt.Errorf("期望唯一 UDC，实际发现 %d 个: %v", len(udcs), udcs)
	}
	return udcs[0], nil
}

func (m *Manager) gadgetPath() string {
	return filepath.Join(m.config.ConfigFSPath, m.config.GadgetName)
}

func (m *Manager) lunPath(name string) string {
	return filepath.Join(m.gadgetPath(), "functions", "mass_storage.0", "lun.0", name)
}

func validateConfig(config Config) error {
	if !filepath.IsAbs(config.ConfigFSPath) || !filepath.IsAbs(config.UDCPath) || !filepath.IsAbs(config.ImagePath) {
		return errors.New("configfs、UDC 和镜像必须使用绝对路径")
	}
	if config.GadgetName == "" || strings.ContainsAny(config.GadgetName, "/\\\t\r\n") {
		return errors.New("Gadget 名称不合法")
	}
	if config.SerialNumber == "" || strings.ContainsAny(config.SerialNumber, "\t\r\n") {
		return errors.New("USB 序列号不合法")
	}
	if config.ReleaseGadget != "" && strings.ContainsAny(config.ReleaseGadget, "/\\\t\r\n") {
		return errors.New("允许释放的 Gadget 名称不合法")
	}
	return nil
}

func validateImage(name string) (string, error) {
	originalInfo, err := os.Lstat(name)
	if err != nil {
		return "", fmt.Errorf("检查 TeslaCam 镜像路径: %w", err)
	}
	if originalInfo.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("TeslaCam 镜像不能是符号链接")
	}
	resolved, err := filepath.EvalSymlinks(name)
	if err != nil {
		return "", fmt.Errorf("解析 TeslaCam 镜像: %w", err)
	}
	info, err := os.Lstat(resolved)
	if err != nil {
		return "", fmt.Errorf("检查 TeslaCam 镜像: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("TeslaCam 镜像不是可信普通文件")
	}
	return resolved, nil
}

func imageLoopDevice(imagePath string) (string, error) {
	entries, err := filepath.Glob("/sys/class/block/loop*/loop/backing_file")
	if err != nil {
		return "", fmt.Errorf("扫描 loop backing file: %w", err)
	}
	for _, entry := range entries {
		backingFile, readErr := readTrimmed(entry)
		if readErr != nil {
			return "", fmt.Errorf("读取 %s: %w", entry, readErr)
		}
		if samePath(backingFile, imagePath) {
			return "/dev/" + filepath.Base(filepath.Dir(filepath.Dir(entry))), nil
		}
	}
	return "", nil
}

func samePath(left, right string) bool {
	return filepath.Clean(left) == filepath.Clean(right)
}

func readTrimmed(name string) (string, error) {
	value, err := os.ReadFile(name)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(value)), nil
}

func readOptional(name string) (string, error) {
	value, err := readTrimmed(name)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return value, err
}

func writeValue(name, value string) error {
	return os.WriteFile(name, []byte(value+"\n"), 0o644)
}
