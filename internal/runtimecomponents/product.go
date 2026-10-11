package runtimecomponents

import (
	"context"
	"github.com/wen-ryon/teslabox/internal/carplaybundle"
	"github.com/wen-ryon/teslabox/internal/mediabundle"
	"log/slog"
)

// NewProduct 给 OTA 与首装提供同一个固定组件安装入口，组件在各自事务内激活。
func NewProduct(d Descriptor, root string, logger *slog.Logger) (*Manager, error) {
	return New(Config{Root: root, Components: d.Components, RequireRoot: true, Install: map[string]Installer{
		"media": func(ctx context.Context, c Component, p string) error {
			if err := secureDirectory("/opt/teslabox/media-versions"); err != nil {
				return err
			}
			return mediabundle.EnsureVersionArchive(ctx, p, c.SHA256, "/opt/teslabox")
		},
		"carplay": func(ctx context.Context, c Component, p string) error {
			return carplaybundle.EnsureArchive(ctx, p, c.SHA256, c.Version, logger)
		},
		"nas": func(ctx context.Context, c Component, p string) error {
			return InstallNAS(ctx, c, p, "/opt/teslabox/backup/versions")
		},
	}})
}
