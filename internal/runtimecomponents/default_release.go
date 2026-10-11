//go:build componentdelivery

package runtimecomponents

import _ "embed"

//go:embed release/descriptor.json
var releaseDescriptor []byte

// Default 的完整描述随签名主 ELF 固定，设备不能换成动态 latest。
func Default() (Descriptor, bool, error) { d, err := Parse(releaseDescriptor); return d, true, err }
