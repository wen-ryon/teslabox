package api

import (
	"context"

	"github.com/wen-ryon/teslabox/internal/deviceadmin"
)

type wifiDetailsAdmin interface {
	WiFiDetails(context.Context, deviceadmin.WiFiStatus) deviceadmin.WiFiDetails
}

func wifiStatusWithDetails(ctx context.Context, admin DeviceAdmin, status deviceadmin.WiFiStatus) any {
	details := deviceadmin.WiFiDetails{}
	if reader, ok := admin.(wifiDetailsAdmin); ok {
		details = reader.WiFiDetails(ctx, status)
	}
	return struct {
		deviceadmin.WiFiStatus
		Details deviceadmin.WiFiDetails `json:"details"`
	}{status, details}
}
