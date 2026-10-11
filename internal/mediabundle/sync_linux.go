//go:build linux

package mediabundle

import (
	"errors"
	"os"
)

func syncMediaDirectory(p string) error {
	f, e := os.Open(p)
	if e != nil {
		return e
	}
	return errors.Join(f.Sync(), f.Close())
}
