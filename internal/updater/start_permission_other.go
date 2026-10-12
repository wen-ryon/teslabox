//go:build !linux

package updater

import (
	"context"
	"errors"
)

func authorizedStart(context.Context, HelperConfig, func() error) error {
	return errors.New("启动许可仅支持 Linux")
}
func claimAuthorizedStart(context.Context, HelperConfig) error {
	return errors.New("启动许可仅支持 Linux")
}
