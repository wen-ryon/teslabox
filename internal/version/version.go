// Package version 保存由发布构建注入的 TeslaBox 版本信息。
package version

// Version 在正式发布时通过 -ldflags 注入，开发构建保持 dev，避免伪装成可升级的正式版本。
var Version = "dev"

// Commit 在正式发布时通过 -ldflags 注入，用于定位设备正在运行的确切代码。
var Commit = "unknown"

// Info 是 API 对外暴露的稳定版本结构。
type Info struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

// Current 返回当前二进制的构建版本信息。
func Current() Info {
	return Info{Version: Version, Commit: Commit}
}
