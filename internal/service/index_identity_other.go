//go:build !linux

package service

import "errors"

func persistentIndexIdentity(state Snapshot, directory string) (string, error) {
	return "", errors.New("持久化索引设备身份校验仅支持 Linux 板端")
}

func persistentIndexWritable(directory string) error { return nil }
