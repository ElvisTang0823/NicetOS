//go:build windows

package proxy

import (
	"fmt"
	"os/exec"
	"strings"
)

const internetSettingsKey = `HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings`

func configurePlatformProxy(address string) (func() error, error) {
	_, port, err := splitProxyAddress(address)
	if err != nil {
		return nil, err
	}
	oldEnable, enableExists, err := readRegistryValue("ProxyEnable")
	if err != nil {
		return nil, err
	}
	oldServer, serverExists, err := readRegistryValue("ProxyServer")
	if err != nil {
		return nil, err
	}
	if err := registryCommand("add", internetSettingsKey, "/v", "ProxyEnable", "/t", "REG_DWORD", "/d", "1", "/f"); err != nil {
		return nil, err
	}
	if err := registryCommand("add", internetSettingsKey, "/v", "ProxyServer", "/t", "REG_SZ", "/d", "127.0.0.1:"+port, "/f"); err != nil {
		return nil, err
	}

	return func() error {
		if err := restoreRegistryValue("ProxyEnable", oldEnable, enableExists, "REG_DWORD"); err != nil {
			return err
		}
		return restoreRegistryValue("ProxyServer", oldServer, serverExists, "REG_SZ")
	}, nil
}

func splitProxyAddress(address string) (string, string, error) {
	parts := strings.Split(address, ":")
	if len(parts) != 2 || parts[1] == "" {
		return "", "", fmt.Errorf("invalid proxy address %q", address)
	}
	return parts[0], parts[1], nil
}

func registryCommand(arguments ...string) error {
	command := exec.Command("reg.exe", arguments...)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("registry command failed: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func readRegistryValue(name string) (string, bool, error) {
	command := exec.Command("reg.exe", "query", internetSettingsKey, "/v", name)
	output, err := command.CombinedOutput()
	if err != nil {
		// reg.exe returns exit code 1 when the requested value is absent.
		if exitError, ok := err.(*exec.ExitError); ok && exitError.ExitCode() == 1 {
			return "", false, nil
		}
		return "", false, fmt.Errorf("cannot read registry value %s: %w (%s)", name, err, strings.TrimSpace(string(output)))
	}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[0] == name {
			return fields[len(fields)-1], true, nil
		}
	}
	return "", false, nil
}

func restoreRegistryValue(name, value string, exists bool, valueType string) error {
	if !exists {
		return nil
	}
	return registryCommand("add", internetSettingsKey, "/v", name, "/t", valueType, "/d", value, "/f")
}
