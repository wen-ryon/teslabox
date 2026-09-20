package service

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func persistentIndexIdentity(state Snapshot, directory string) (string, error) {
	if state.DataDrive == nil || state.DataDrive.UUID == "" || !state.Backing.Ready {
		return "", errors.New("设备身份尚未就绪")
	}
	ancestor := filepath.Clean(directory)
	cacheInfo, err := os.Stat(ancestor)
	for errors.Is(err, os.ErrNotExist) && filepath.Dir(ancestor) != ancestor {
		ancestor = filepath.Dir(ancestor)
		cacheInfo, err = os.Stat(ancestor)
	}
	if err != nil {
		return "", err
	}
	systemInfo, err := os.Stat("/")
	if err != nil {
		return "", err
	}
	image, err := os.Open(state.Backing.ImagePath)
	if err != nil {
		return "", err
	}
	defer image.Close()
	imageInfo, err := image.Stat()
	if err != nil {
		return "", err
	}
	cacheStat, ok := cacheInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return "", errors.New("缓存文件系统未知")
	}
	systemStat, ok := systemInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return "", errors.New("系统文件系统未知")
	}
	imageStat, ok := imageInfo.Sys().(*syscall.Stat_t)
	if !ok {
		return "", errors.New("镜像身份未知")
	}
	if cacheStat.Dev != systemStat.Dev || cacheStat.Dev == imageStat.Dev || !imageInfo.Mode().IsRegular() {
		return "", errors.New("缓存必须位于系统盘且与录像盘分离")
	}
	volume, err := readIndexVolume(image, imageInfo.Size())
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return "", err
	}
	// 不使用随录像写入变化的 mtime；格式化序列号及镜像 inode 用来区分重新初始化。
	key := fmt.Sprintf("v2|%s|%s|%d|%s|%d|%d|%d|%d|%08x", state.DataDrive.UUID, state.DataDrive.Serial, state.DataDrive.SizeBytes, filepath.Clean(state.Backing.ImagePath), imageStat.Ino, imageInfo.Size(), volume.Offset, volume.Length, volume.Serial)
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:]), nil
}

func persistentIndexWritable(directory string) error {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(directory, &stat); err != nil {
		return err
	}
	if stat.Bsize <= 0 || stat.Bavail < uint64((256<<20)/stat.Bsize) {
		return errors.New("系统盘可用空间不足 256 MiB，跳过缓存写入")
	}
	return nil
}
