//go:build !linux

package carplaypairing

import "errors"

// NewProvisioningManager 在非 Linux 环境明确返回不可用，不模拟硬件登记。
func NewProvisioningManager(binary, directory, runtime string) (*Manager, error) {
	return nil, errors.New("CarPlay 首次登记只支持 Linux 板端")
}

// NewConfiguredProvisioningManager 在非 Linux 环境明确禁用真实登记，不暴露模拟按钮。
func NewConfiguredProvisioningManager(settingsPath, directory, runtime string) (*Manager, error) {
	return nil, errors.New("CarPlay 首次登记只支持 Linux 板端")
}
