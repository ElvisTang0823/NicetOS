//go:build !windows && !linux

package proxy

func configurePlatformProxy(_ string) (func() error, error) {
	return func() error { return nil }, nil
}
