//go:build !linux

package deviceadmin

func readEthernetDuplex(string) string { return "" }
