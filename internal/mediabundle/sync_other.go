//go:build !linux

package mediabundle

func syncMediaDirectory(string) error { return nil }
