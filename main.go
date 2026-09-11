package main

import (
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	proxy "nicetos/go"
)

var (
	listenAddress = flag.String("listen", "127.0.0.1:8080", "proxy listen address")
	pythonCommand = flag.String("python", "python", "Python executable")
)

type siteChecker struct {
	python string
	root   string
	mu     sync.Mutex
}

func choosePythonInvocation(python string, target string) (string, []string) {
	code := "import main; print(main.check_url(__import__('sys').argv[1]))"
	python = strings.TrimSpace(python)
	if python == "" {
		python = "python"
	}

	base := strings.ToLower(filepath.Base(python))
	if base == "py" || base == "py.exe" {
		return "py", []string{"-3", "-c", code, target}
	}
	if base == "python" || base == "python.exe" {
		return "python", []string{"-c", code, target}
	}
	return python, []string{"-c", code, target}
}

func (checker *siteChecker) Check(target string) (bool, error) {
	checker.mu.Lock()
	defer checker.mu.Unlock()

	candidates := []string{checker.python}
	if checker.python == "" || checker.python == "python" || strings.EqualFold(filepath.Base(checker.python), "python") || strings.EqualFold(filepath.Base(checker.python), "python.exe") {
		candidates = append(candidates, "py")
	}

	var lastErr error
	for _, candidate := range candidates {
		commandName, commandArgs := choosePythonInvocation(candidate, target)
		command := exec.Command(commandName, commandArgs...)
		command.Dir = checker.root
		command.Env = append(os.Environ(), "PYTHONPATH="+filepath.Join(checker.root, "python"))
		output, err := command.Output()
		if err == nil {
			lines := strings.Split(strings.TrimSpace(string(output)), "\n")
			if len(lines) == 0 {
				return false, errors.New("main.py returned no result")
			}
			lastLine := strings.TrimSpace(lines[len(lines)-1])
			result := lastLine != "False"
			log.Printf("[CHECK] %s -> Python: %s -> Go: %v", target, lastLine, result)
			return result, nil
		}
		lastErr = err
		log.Printf("[CHECK] Python error for %s: %v (falling back if needed)", target, err)
	}

	return false, fmt.Errorf("main.py check failed: %w", lastErr)
}

func proxyRoot() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	// executable 可能在 go/ 目錄內（e.g., e:\NicetOS\go\NicetOS-windows-amd64.exe）
	// 或根目錄（e.g., e:\NicetOS\main 在 go run . 時）
	// 先檢查是否 main.py 在當前目錄，若沒有則往上一層找
	exeDir := filepath.Dir(filepath.Clean(executable))
	if _, err := os.Stat(filepath.Join(exeDir, "main.py")); err == nil {
		return exeDir, nil
	}
	// 若 main.py 不在 exe 目錄，試試父目錄
	parentDir := filepath.Dir(exeDir)
	if _, err := os.Stat(filepath.Join(parentDir, "main.py")); err == nil {
		return parentDir, nil
	}
	// 都找不到，就用 exe 所在目錄
	return exeDir, nil
}

func main() {
	flag.Parse()
	log.Printf("Starting NicetOS proxy on %s", *listenAddress)
	
	root, err := proxyRoot()
	if err != nil {
		log.Fatalf("Failed to determine proxy root: %v", err)
	}
	log.Printf("Proxy root directory: %s", root)

	server, err := proxy.StartTransparentProxy(*listenAddress, &siteChecker{python: *pythonCommand, root: root})
	if err != nil {
		log.Fatalf("Failed to start proxy: %v", err)
	}
	log.Println("Proxy started successfully")
	
	// 立即註冊清理，即使程序崩潰也能恢復系統設定
	defer func() {
		log.Println("cleaning up proxy settings")
		if closeErr := server.Close(); closeErr != nil {
			log.Printf("error closing server: %v", closeErr)
		}
	}()

	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-shutdown
		log.Println("stopping NicetOS proxy")
		_ = server.Close()
	}()

	log.Printf("NicetOS transparent proxy listening on %s; press Ctrl+C to stop", *listenAddress)
	if err := server.Serve(); err != nil && !errors.Is(err, net.ErrClosed) {
		log.Fatalf("Proxy serve error: %v", err)
	}
}
