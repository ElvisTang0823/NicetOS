//go:build windows

package proxy

import (
	"fmt"
	"log"
	"os/exec"
	"strings"
)

const internetSettingsKey = `HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings`

func configurePlatformProxy(address string) (func() error, error) {
	_, port, err := splitProxyAddress(address)
	if err != nil {
		return nil, err
	}
	
	// 讀取當前設定
	oldEnable, enableExists, err := readRegistryValue("ProxyEnable")
	if err != nil {
		return nil, err
	}
	oldServer, serverExists, err := readRegistryValue("ProxyServer")
	if err != nil {
		return nil, err
	}
	
	currentAddress := "127.0.0.1:" + port
	wasCleaned := false
	
	// 若發現遺留的舊代理設定（前次程序崩潰時的殘留），清除它們
	if enableExists && oldEnable == "1" && serverExists && oldServer != currentAddress {
		log.Printf("[PROXY-WIN] Detected leftover proxy settings: %s (expected: %s), cleaning up", oldServer, currentAddress)
		if err := registryCommand("delete", internetSettingsKey, "/v", "ProxyEnable", "/f"); err == nil {
			log.Println("[PROXY-WIN] Cleaned ProxyEnable")
		}
		if err := registryCommand("delete", internetSettingsKey, "/v", "ProxyServer", "/f"); err == nil {
			log.Println("[PROXY-WIN] Cleaned ProxyServer")
		}
		wasCleaned = true
		enableExists = false
		serverExists = false
	}
	
	// 設置新的代理
	log.Printf("[PROXY-WIN] Setting proxy to %s", currentAddress)
	if err := registryCommand("add", internetSettingsKey, "/v", "ProxyEnable", "/t", "REG_DWORD", "/d", "1", "/f"); err != nil {
		return nil, fmt.Errorf("failed to set ProxyEnable: %w", err)
	}
	if err := registryCommand("add", internetSettingsKey, "/v", "ProxyServer", "/t", "REG_SZ", "/d", currentAddress, "/f"); err != nil {
		return nil, fmt.Errorf("failed to set ProxyServer: %w", err)
	}
	log.Println("[PROXY-WIN] Proxy settings applied")

	// 返回恢復函數
	return func() error {
		log.Println("[PROXY-WIN] Restoring proxy settings...")
		if wasCleaned {
			// 若之前清除過遺留設定，恢復時也不需特殊處理
			log.Println("[PROXY-WIN] Previously cleaned settings, nothing to restore")
			return nil
		}
		if err := restoreRegistryValue("ProxyEnable", oldEnable, enableExists, "REG_DWORD"); err != nil {
			return fmt.Errorf("failed to restore ProxyEnable: %w", err)
		}
		if err := restoreRegistryValue("ProxyServer", oldServer, serverExists, "REG_SZ"); err != nil {
			return fmt.Errorf("failed to restore ProxyServer: %w", err)
		}
		log.Println("[PROXY-WIN] Proxy settings restored")
		return nil
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
	if exists {
		// 值原本存在，恢復原值
		return registryCommand("add", internetSettingsKey, "/v", name, "/t", valueType, "/d", value, "/f")
	}
	// 值原本不存在，刪除新添加的值
	return registryCommand("delete", internetSettingsKey, "/v", name, "/f")
}
