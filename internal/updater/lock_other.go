//go:build !linux

package updater

import "errors"

var errInstallationBusy = errors.New("另一个安装或恢复事务正在运行")

func installationActive(string) bool { return false }

func lockInstallation(string) (func(), error) {
	return nil, errors.New("设备安装和恢复仅支持 Linux")
}
