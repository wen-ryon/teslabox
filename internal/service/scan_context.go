package service

import (
	"context"

	"github.com/wen-ryon/teslabox/internal/teslacam"
)

func scanVideos(ctx context.Context, scanner VideoScanner, root string, progress ...func(int)) ([]teslacam.Video, error) {
	if len(progress) > 0 {
		if reporting, ok := scanner.(interface {
			ScanWithProgress(context.Context, string, func(int)) ([]teslacam.Video, error)
		}); ok {
			return reporting.ScanWithProgress(ctx, root, progress[0])
		}
	}
	if contextual, ok := scanner.(interface {
		ScanContext(context.Context, string) ([]teslacam.Video, error)
	}); ok {
		return contextual.ScanContext(ctx, root)
	}
	return scanner.Scan(root)
}
