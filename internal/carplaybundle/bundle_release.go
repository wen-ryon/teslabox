//go:build carplaybundle

package carplaybundle

import (
	"embed"
	"fmt"
	"io"
	"strings"
)

//go:embed bundle/carplay-runtime.tar.gz bundle/carplay-runtime.tar.gz.sha256
var releaseFiles embed.FS

func openEmbeddedArchive() (io.ReadCloser, string, bool, error) {
	checksum, err := releaseFiles.ReadFile("bundle/carplay-runtime.tar.gz.sha256")
	if err != nil {
		return nil, "", false, err
	}
	fields := strings.Fields(string(checksum))
	if len(fields) != 1 || !digestPattern.MatchString(fields[0]) {
		return nil, "", false, fmt.Errorf("内置 CarPlay 摘要无效")
	}
	archive, err := releaseFiles.Open("bundle/carplay-runtime.tar.gz")
	return archive, fields[0], true, err
}
