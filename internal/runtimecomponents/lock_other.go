//go:build !linux

package runtimecomponents

import (
	"context"
	"path/filepath"
	"sync"
	"time"
)

var processLocks sync.Map

func lock(ctx context.Context, root, id string) (func(), error) {
	v, _ := processLocks.LoadOrStore(filepath.Join(root, id), &sync.Mutex{})
	mu := v.(*sync.Mutex)
	for !mu.TryLock() {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	return mu.Unlock, nil
}
