//go:build !carplaybundle

package carplaybundle

import "io"

func openEmbeddedArchive() (io.ReadCloser, string, bool, error) {
	return nil, "", false, nil
}
