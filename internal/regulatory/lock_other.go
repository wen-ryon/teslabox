//go:build !linux

package regulatory

import "context"

// 非 Linux 构建只用于本地测试；真实无线操作由 Linux 的 flock 保护。
func acquire(ctx context.Context, _ string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return func() {}, nil
}

func syncDirectory(string) error { return nil }
