package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// get-pip.py 是 pip 官方提供的安裝腳本；NicetOS 內建的 Python 沒有附 pip 時才會下載它。
const getPipURL = "https://bootstrap.pypa.io/get-pip.py"

const (
	pythonCheckTimeout   = 1 * time.Minute
	pipInstallTimeout    = 15 * time.Minute
	getPipDownloadLimit  = 10 << 20 // 10 MiB
	getPipDownloadWindow = 3 * time.Minute
)

// 用來確認套件能否載入的 Python 程式碼：做法和 main.py 實際使用時一樣。
const pythonPackageCheckCode = `import sys
root = sys.argv[1]
sys.path.insert(0, root)
from python.requests_loader import load_requests
requests = load_requests(root)
import bs4
print("requests", requests.__version__, "/ beautifulsoup4", bs4.__version__)`

// ensurePythonPackages 確認 requests 與 beautifulsoup4 可以使用；
// 不能用時，自動用 pip 依照 python/requirements.txt 重新安裝。
func ensurePythonPackages(root string) error {
	log.Println("[PYTHON] 檢查 Python 套件（requests、beautifulsoup4）")
	versions, err := checkPythonPackages(root)
	if err == nil {
		log.Printf("[PYTHON] 套件檢查：通過（%s）", versions)
		return nil
	}
	log.Printf("[PYTHON] 套件檢查：失敗；原因：%v", err)
	log.Println("[PYTHON] 開始用 pip 安裝套件；需要網路連線，可能需要幾分鐘")

	python, prefixArgs, bundled := pythonForPip(root)
	if python == "" {
		return errors.New("找不到可以執行 pip 的 Python")
	}

	if err := runPython(root, python, append(prefixArgs, "-m", "pip", "--version"), pythonCheckTimeout); err != nil {
		log.Println("[PYTHON] 這個 Python 沒有 pip，正在下載並安裝 pip")
		if err := installPip(root, python, prefixArgs); err != nil {
			return fmt.Errorf("安裝 pip 失敗：%w", err)
		}
	}

	requirements := filepath.Join(root, "python", "requirements.txt")
	if _, err := os.Stat(requirements); err != nil {
		return fmt.Errorf("找不到套件清單 %s：%w", requirements, err)
	}
	args := append(prefixArgs, "-m", "pip", "install",
		"--disable-pip-version-check", "--no-warn-script-location", "--upgrade")
	if bundled {
		// 只對 NicetOS 自己的 Python 強制重裝，修好缺檔或損壞的套件；
		// 不去動使用者電腦上原本安裝的 Python。
		args = append(args, "--force-reinstall")
	}
	args = append(args, "-r", requirements)
	if err := runPython(root, python, args, pipInstallTimeout); err != nil {
		return fmt.Errorf("pip 安裝套件失敗：%w", err)
	}

	versions, err = checkPythonPackages(root)
	if err != nil {
		return fmt.Errorf("pip 已執行，但套件仍無法載入：%w", err)
	}
	log.Printf("[PYTHON] pip 安裝完成，套件檢查：通過（%s）", versions)
	return nil
}

// pythonForPip 優先使用 NicetOS 內建的 Python；bundled 表示是否為內建版本。
func pythonForPip(root string) (string, []string, bool) {
	bundledPython := filepath.Join(root, "python_runtime", "python.exe")
	if info, err := os.Stat(bundledPython); err == nil && !info.IsDir() {
		return bundledPython, nil, true
	}
	python, prefixArgs := pythonExecutable(root)
	return python, prefixArgs, false
}

func checkPythonPackages(root string) (string, error) {
	python, prefixArgs := pythonExecutable(root)
	if python == "" {
		return "", errors.New("找不到 Python")
	}
	ctx, cancel := context.WithTimeout(context.Background(), pythonCheckTimeout)
	defer cancel()
	args := append(append([]string{}, prefixArgs...), "-c", pythonPackageCheckCode, root)
	command := exec.CommandContext(ctx, python, args...)
	command.Dir = root
	command.Env = pythonToolEnvironment()
	output, err := command.CombinedOutput()
	text := strings.TrimSpace(string(output))
	if err != nil {
		if len(text) > 800 {
			text = "…" + text[len(text)-800:]
		}
		return "", fmt.Errorf("%v：%s", err, text)
	}
	return text, nil
}

func runPython(root string, python string, args []string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	command := exec.CommandContext(ctx, python, args...)
	command.Dir = root
	command.Env = pythonToolEnvironment()
	command.Stdout = os.Stderr
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("超過 %s 仍未完成：%w", timeout, ctx.Err())
		}
		return err
	}
	return nil
}

func installPip(root string, python string, prefixArgs []string) error {
	temporary, err := os.MkdirTemp("", "nicetos-get-pip-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temporary)
	script := filepath.Join(temporary, "get-pip.py")
	if err := downloadFile(getPipURL, script); err != nil {
		return fmt.Errorf("下載 get-pip.py 失敗：%w", err)
	}
	args := append(append([]string{}, prefixArgs...), script,
		"--disable-pip-version-check", "--no-warn-script-location")
	return runPython(root, python, args, pipInstallTimeout)
}

func downloadFile(url string, destination string) error {
	client := &http.Client{Timeout: getPipDownloadWindow}
	response, err := client.Get(url)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("伺服器回應 %s", response.Status)
	}
	output, err := os.Create(destination)
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(output, io.LimitReader(response.Body, getPipDownloadLimit+1))
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if written > getPipDownloadLimit {
		return errors.New("下載的檔案大小異常")
	}
	return nil
}

// pythonToolEnvironment 移除會干擾內建 Python 的環境變數。
func pythonToolEnvironment() []string {
	return append(replaceEnvironment(os.Environ(),
		"PYTHONPATH", "PYTHONHOME", "PYTHONIOENCODING", "PIP_TARGET", "PIP_PREFIX", "PIP_USER"),
		"PYTHONIOENCODING=utf-8")
}
