//go:build !componentdelivery

package runtimecomponents

// Default 保留开发与旧内嵌构建，不隐式启用没有发行描述的公网下载。
func Default() (Descriptor, bool, error) { return Descriptor{}, false, nil }
