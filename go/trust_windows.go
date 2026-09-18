//go:build windows

package proxy

import (
	"fmt"
	"os/exec"
	"strings"
)

func configureCertificateTrust(path string) (func() error, error) {
	// A crashed earlier run may have left this CA trusted. Remove that entry
	// before adding the persistent CA again; "not found" is harmless here.
	_, _ = exec.Command("certutil.exe", "-user", "-delstore", "Root", "NicetOS Local HTTPS Filtering CA").CombinedOutput()
	output, err := exec.Command("certutil.exe", "-user", "-addstore", "Root", path).CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("add NicetOS CA to the current-user trust store: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return func() error {
		output, err := exec.Command("certutil.exe", "-user", "-delstore", "Root", "NicetOS Local HTTPS Filtering CA").CombinedOutput()
		if err != nil {
			return fmt.Errorf("remove NicetOS CA from the current-user trust store: %w (%s)", err, strings.TrimSpace(string(output)))
		}
		return nil
	}, nil
}
