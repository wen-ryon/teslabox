//go:build !releasebundle

package mediabundle

import "io"

func openEmbeddedArchive() (io.ReadCloser, string, bool, error) {
	return nil, "", false, nil
}
