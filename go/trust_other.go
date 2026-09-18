//go:build !windows

package proxy

func configureCertificateTrust(_ string) (func() error, error) {
	return func() error { return nil }, nil
}
