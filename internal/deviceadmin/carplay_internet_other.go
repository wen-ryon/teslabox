//go:build !linux

package deviceadmin

import "context"

func probeCarPlayInternet(context.Context, string, []string) bool { return false }
