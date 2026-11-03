//go:build !linux

package panesize

func platformTTYResolver() TTYResolver {
	return psTTYResolver{}
}
