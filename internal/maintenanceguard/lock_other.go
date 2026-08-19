//go:build !linux

package maintenanceguard

import (
	"context"
	"sync"
)

var fallbackLock sync.Mutex

func platformAcquire(ctx context.Context, _ string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	fallbackLock.Lock()
	var once sync.Once
	return func() { once.Do(fallbackLock.Unlock) }, nil
}
