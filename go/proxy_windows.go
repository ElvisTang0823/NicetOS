//go:build windows

package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

const internetSettingsKey = `HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings`

// 本機位址一律不經過 NicetOS 代理，避免 NicetOS 自己呼叫 Ollama 或本機服務時繞回自己。
const nicetosProxyOverride = "<local>;localhost;127.*;[::1]"

// registryValue 記錄一個登錄值原本的狀態。
type registryValue struct {
	Value  string `json:"value"`
	Exists bool   `json:"exists"`
}

// proxySnapshot 是 NicetOS 修改前的系統 Proxy 設定，會存成檔案；
// 即使程式當掉，下次啟動也能用它還原成使用者原本的設定。
type proxySnapshot struct {
	Enable   registryValue `json:"enable"`
	Server   registryValue `json:"server"`
	Override registryValue `json:"override"`
}

func configurePlatformProxy(address string) (func() error, error) {
	_, port, err := splitProxyAddress(address)
	if err != nil {
		return nil, err
	}
	currentAddress := "127.0.0.1:" + port

	backupPath, err := proxyBackupPath()
	if err != nil {
		return nil, err
	}

	original, found, err := loadProxySnapshot(backupPath)
	if err != nil {
		log.Printf("[PROXY-WIN] 備份檔無法讀取（%v），改讀目前設定", err)
		found = false
	}
	if found {
		log.Println("[PROXY-WIN] 偵測到上次沒有正常關閉，會在結束時還原成更早之前的原始設定")
	} else {
		original, err = readProxySnapshot()
		if err != nil {
			return nil, err
		}
		// 目前設定已經指向本機代理（例如上次當掉又沒有備份檔），
		// 那就不是使用者原本的設定，結束時改成關閉 Proxy。
		if registryEnabled(original.Enable) && original.Server.Exists && isLoopbackProxy(original.Server.Value) {
			log.Printf("[PROXY-WIN] 偵測到遺留的本機代理設定 %s，結束時會關閉系統 Proxy", original.Server.Value)
			original = proxySnapshot{
				Enable:   registryValue{Value: "0", Exists: true},
				Override: original.Override,
			}
		}
		if err := saveProxySnapshot(backupPath, original); err != nil {
			return nil, fmt.Errorf("無法保存原本的 Proxy 設定：%w", err)
		}
	}

	log.Printf("[PROXY-WIN] Setting proxy to %s", currentAddress)
	override := nicetosProxyOverride
	if original.Override.Exists && strings.TrimSpace(original.Override.Value) != "" {
		override = mergeProxyOverride(original.Override.Value, nicetosProxyOverride)
	}
	apply := func() error {
		if err := registryCommand("add", internetSettingsKey, "/v", "ProxyServer", "/t", "REG_SZ", "/d", currentAddress, "/f"); err != nil {
			return fmt.Errorf("failed to set ProxyServer: %w", err)
		}
		if err := registryCommand("add", internetSettingsKey, "/v", "ProxyOverride", "/t", "REG_SZ", "/d", override, "/f"); err != nil {
			return fmt.Errorf("failed to set ProxyOverride: %w", err)
		}
		if err := registryCommand("add", internetSettingsKey, "/v", "ProxyEnable", "/t", "REG_DWORD", "/d", "1", "/f"); err != nil {
			return fmt.Errorf("failed to set ProxyEnable: %w", err)
		}
		return nil
	}
	if err := apply(); err != nil {
		_ = restoreProxySnapshot(original)
		_ = os.Remove(backupPath)
		notifyProxyChanged()
		return nil, err
	}
	notifyProxyChanged()
	log.Println("[PROXY-WIN] Proxy settings applied")

	return func() error {
		log.Println("[PROXY-WIN] Restoring proxy settings...")
		err := restoreProxySnapshot(original)
		notifyProxyChanged()
		if err != nil {
			// 保留備份檔，下次啟動還能再還原一次。
			return err
		}
		_ = os.Remove(backupPath)
		log.Println("[PROXY-WIN] Proxy settings restored")
		return nil
	}, nil
}

func proxyBackupPath() (string, error) {
	directory, err := os.UserCacheDir() // %LOCALAPPDATA%
	if err != nil || directory == "" {
		directory = os.TempDir()
	}
	directory = filepath.Join(directory, "NicetOS")
	if err := os.MkdirAll(directory, 0700); err != nil {
		return "", fmt.Errorf("無法建立 Proxy 備份資料夾：%w", err)
	}
	return filepath.Join(directory, "proxy-settings-backup.json"), nil
}

func loadProxySnapshot(path string) (proxySnapshot, bool, error) {
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return proxySnapshot{}, false, nil
	}
	if err != nil {
		return proxySnapshot{}, false, err
	}
	var snapshot proxySnapshot
	if err := json.Unmarshal(contents, &snapshot); err != nil {
		return proxySnapshot{}, false, err
	}
	return snapshot, true, nil
}

func saveProxySnapshot(path string, snapshot proxySnapshot) error {
	contents, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	temporary := path + ".new"
	if err := os.WriteFile(temporary, contents, 0600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}

func readProxySnapshot() (proxySnapshot, error) {
	var snapshot proxySnapshot
	var err error
	if snapshot.Enable, err = readRegistryValue("ProxyEnable"); err != nil {
		return snapshot, err
	}
	if snapshot.Server, err = readRegistryValue("ProxyServer"); err != nil {
		return snapshot, err
	}
	if snapshot.Override, err = readRegistryValue("ProxyOverride"); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

func restoreProxySnapshot(snapshot proxySnapshot) error {
	var firstErr error
	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	keep(restoreRegistryValue("ProxyEnable", snapshot.Enable, "REG_DWORD"))
	keep(restoreRegistryValue("ProxyServer", snapshot.Server, "REG_SZ"))
	keep(restoreRegistryValue("ProxyOverride", snapshot.Override, "REG_SZ"))
	return firstErr
}

func registryEnabled(value registryValue) bool {
	if !value.Exists {
		return false
	}
	number, err := strconv.ParseInt(strings.TrimSpace(value.Value), 0, 64)
	return err == nil && number != 0
}

func isLoopbackProxy(server string) bool {
	for _, part := range strings.Split(server, ";") {
		if _, after, found := strings.Cut(part, "="); found {
			part = after
		}
		part = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(part), "http://"), "https://")
		host, _, err := net.SplitHostPort(part)
		if err != nil {
			host = part
		}
		if host == "localhost" {
			return true
		}
		if ip := net.ParseIP(strings.Trim(host, "[]")); ip != nil && ip.IsLoopback() {
			return true
		}
	}
	return false
}

func mergeProxyOverride(existing string, extra string) string {
	seen := map[string]bool{}
	var result []string
	for _, entry := range append(strings.Split(existing, ";"), strings.Split(extra, ";")...) {
		entry = strings.TrimSpace(entry)
		if entry == "" || seen[strings.ToLower(entry)] {
			continue
		}
		seen[strings.ToLower(entry)] = true
		result = append(result, entry)
	}
	return strings.Join(result, ";")
}

// notifyProxyChanged 通知 Windows 與瀏覽器重新讀取 Proxy 設定，不必重開瀏覽器。
func notifyProxyChanged() {
	const (
		internetOptionRefresh         = 37
		internetOptionSettingsChanged = 39
	)
	wininet := syscall.NewLazyDLL("wininet.dll")
	setOption := wininet.NewProc("InternetSetOptionW")
	if setOption.Find() != nil {
		return
	}
	_, _, _ = setOption.Call(0, internetOptionSettingsChanged, 0, 0)
	_, _, _ = setOption.Call(0, internetOptionRefresh, 0, 0)
}

func splitProxyAddress(address string) (string, string, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || port == "" {
		return "", "", fmt.Errorf("invalid proxy address %q", address)
	}
	return host, port, nil
}

func registryCommand(arguments ...string) error {
	command := exec.Command("reg.exe", arguments...)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("registry command failed: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

func readRegistryValue(name string) (registryValue, error) {
	command := exec.Command("reg.exe", "query", internetSettingsKey, "/v", name)
	output, err := command.CombinedOutput()
	if err != nil {
		// reg.exe returns exit code 1 when the requested value is absent.
		var exitError *exec.ExitError
		if errors.As(err, &exitError) && exitError.ExitCode() == 1 {
			return registryValue{}, nil
		}
		return registryValue{}, fmt.Errorf("cannot read registry value %s: %w (%s)", name, err, strings.TrimSpace(string(output)))
	}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.EqualFold(fields[0], name) && strings.HasPrefix(fields[1], "REG_") {
			value := strings.Join(fields[2:], " ")
			if fields[1] == "REG_DWORD" {
				// reg.exe 會顯示成 0x1 這種十六進位格式，轉成十進位方便比對與還原。
				if number, parseErr := strconv.ParseInt(value, 0, 64); parseErr == nil {
					value = strconv.FormatInt(number, 10)
				}
			}
			return registryValue{Value: value, Exists: true}, nil
		}
	}
	return registryValue{}, nil
}

func restoreRegistryValue(name string, value registryValue, valueType string) error {
	if value.Exists {
		return registryCommand("add", internetSettingsKey, "/v", name, "/t", valueType, "/d", value.Value, "/f")
	}
	// 值原本不存在，刪除 NicetOS 新增的值；本來就不存在也沒關係。
	if current, err := readRegistryValue(name); err == nil && !current.Exists {
		return nil
	}
	return registryCommand("delete", internetSettingsKey, "/v", name, "/f")
}
