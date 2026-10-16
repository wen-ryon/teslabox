//go:build !linux

package uipreferences

func syncDirectory(string) error { return nil }
