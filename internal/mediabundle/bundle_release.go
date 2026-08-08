//go:build releasebundle

package mediabundle

import (
	"embed"
	"fmt"
	"io"
	"strings"
)

//go:embed bundle/teslabox-media-debian11-arm64.tar.gz bundle/teslabox-media-debian11-arm64.tar.gz.sha256
var releaseFiles embed.FS

func openEmbeddedArchive() (io.ReadCloser, string, bool, error) {
	checksum, err := releaseFiles.ReadFile("bundle/teslabox-media-debian11-arm64.tar.gz.sha256")
	if err != nil {
		return nil, "", false, fmt.Errorf("读取内置媒体摘要: %w", err)
	}
	fields := strings.Fields(string(checksum))
	if len(fields) < 1 {
		return nil, "", false, fmt.Errorf("内置媒体摘要为空")
	}
	archive, err := releaseFiles.Open("bundle/teslabox-media-debian11-arm64.tar.gz")
	if err != nil {
		return nil, "", false, fmt.Errorf("打开内置媒体包: %w", err)
	}
	return archive, strings.ToLower(fields[0]), true, nil
}
