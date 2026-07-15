// Package gadget 管理 USB Gadget 环境和后续生命周期操作。
package gadget

import (
	"fmt"
	"os"
	"slices"
)

// Environment 描述创建 USB Mass Storage Gadget 前必须具备的内核环境。
type Environment struct {
	ConfigFSPath      string   `json:"configfs_path"`
	ConfigFSAvailable bool     `json:"configfs_available"`
	UDCPath           string   `json:"udc_path"`
	UDCNames          []string `json:"udc_names"`
	Ready             bool     `json:"ready"`
	Reason            string   `json:"reason,omitempty"`
}

// Prober 定义状态机所需的只读 Gadget 环境检查能力。
type Prober interface {
	Probe() (Environment, error)
}

// FSProber 从 sysfs 和 configfs 读取 Gadget 环境，不创建或绑定任何 Gadget。
type FSProber struct {
	configFSPath string
	udcPath      string
}

// NewFSProber 创建只读环境检查器。
func NewFSProber(configFSPath, udcPath string) *FSProber {
	return &FSProber{configFSPath: configFSPath, udcPath: udcPath}
}

// Probe 验证 configfs 挂载点并列出 UDC，缺失能力以状态返回而不是修改系统。
func (p *FSProber) Probe() (Environment, error) {
	environment := Environment{ConfigFSPath: p.configFSPath, UDCPath: p.udcPath, UDCNames: []string{}}
	configInfo, err := os.Stat(p.configFSPath)
	if err != nil {
		if os.IsNotExist(err) {
			environment.Reason = "USB Gadget configfs 路径不存在"
			return environment, nil
		}
		return environment, fmt.Errorf("检查 USB Gadget configfs: %w", err)
	}
	if !configInfo.IsDir() {
		environment.Reason = "USB Gadget configfs 路径不是目录"
		return environment, nil
	}
	environment.ConfigFSAvailable = true

	entries, err := os.ReadDir(p.udcPath)
	if err != nil {
		if os.IsNotExist(err) {
			environment.Reason = "UDC 类目录不存在"
			return environment, nil
		}
		return environment, fmt.Errorf("读取 UDC: %w", err)
	}
	for _, entry := range entries {
		if entry.Name() != "" && entry.Name()[0] != '.' {
			environment.UDCNames = append(environment.UDCNames, entry.Name())
		}
	}
	slices.Sort(environment.UDCNames)
	if len(environment.UDCNames) == 0 {
		environment.Reason = "未发现可绑定的 UDC"
		return environment, nil
	}
	environment.Ready = true
	return environment, nil
}
