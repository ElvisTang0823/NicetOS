package main

import (
	"context"
	"crypto/md5"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	proxy "nicetos/go"
)

var (
	listenAddress = flag.String("listen", "127.0.0.1:8080", "proxy listen address")
)

const hashCapacity = 142867

var listURLs = map[string]string{
	"blacklist": "https://raw.githubusercontent.com/ElvisTang0823/NicetOS/main/data/blacklist.json",
	"whitelist": "https://raw.githubusercontent.com/ElvisTang0823/NicetOS/main/data/whitelist.json",
}

type domainLists struct {
	blacklist map[string][]string
	whitelist map[string][]string
}

var ErrUnknownDecision = errors.New("unknown URL decision")

type siteChecker struct {
	root      string
	urls      map[string]string
	ollamaAPI string
	client    *http.Client
	lists     atomic.Pointer[domainLists]
	updateMu  sync.Mutex
}

func (checker *siteChecker) BlockedPagePath() string {
	return filepath.Join(checker.root, "assets", "index.html")
}

func newSiteChecker(root string) *siteChecker {
	checker := &siteChecker{
		root:   root,
		urls:   listURLs,
		client: &http.Client{Timeout: 10 * time.Second},
	}
	checker.lists.Store(&domainLists{blacklist: map[string][]string{}, whitelist: map[string][]string{}})
	return checker
}

func choosePythonInvocation(pythonCmd string, target string) (string, []string) {
	if pythonCmd == "" {
		pythonCmd = "python"
	}
	scriptCode := fmt.Sprintf("import sys; sys.path.insert(0, %q); import main; print(main.check_url(sys.argv[1]))", ".")
	if strings.EqualFold(pythonCmd, "py") {
		return pythonCmd, []string{"-3", "-c", scriptCode, target}
	}
	return pythonCmd, []string{"-c", scriptCode, target}
}

func choosePythonScriptInvocation(pythonCmd string, scriptPath string, target string) (string, []string) {
	if pythonCmd == "" {
		pythonCmd = "python"
	}
	if strings.EqualFold(pythonCmd, "py") {
		return pythonCmd, []string{"-3", scriptPath, target}
	}
	return pythonCmd, []string{scriptPath, target}
}

func replaceEnvironment(environment []string, names ...string) []string {
	result := make([]string, 0, len(environment))
	for _, entry := range environment {
		name, _, found := strings.Cut(entry, "=")
		if !found {
			result = append(result, entry)
			continue
		}
		replaced := false
		for _, target := range names {
			if strings.EqualFold(name, target) {
				replaced = true
				break
			}
		}
		if !replaced {
			result = append(result, entry)
		}
	}
	return result
}

func pythonExecutable(root string) (string, []string) {
	bundled := filepath.Join(root, "python_runtime", "python.exe")
	if info, err := os.Stat(bundled); err == nil && !info.IsDir() {
		return bundled, nil
	}
	for _, candidate := range []string{"py", "python", "python3"} {
		if command, err := exec.LookPath(candidate); err == nil {
			if strings.EqualFold(candidate, "py") {
				return command, []string{"-3"}
			}
			return command, nil
		}
	}
	return "", nil
}

func pythonCheckURL(root string, target string, ollamaAPI string) (bool, error) {
	scriptPath := filepath.Join(root, "main.py")
	if _, err := os.Stat(scriptPath); err != nil {
		return false, nil
	}

	python, prefixArgs := pythonExecutable(root)
	if python == "" {
		return false, errors.New("python interpreter not found")
	}
	args := append(prefixArgs, scriptPath, target)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	execCmd := exec.CommandContext(ctx, python, args...)
	execCmd.Env = append(replaceEnvironment(os.Environ(), "PYTHONPATH", "NICETOS_OLLAMA_API"),
		"PYTHONPATH="+root, "NICETOS_OLLAMA_API="+ollamaAPI)
	output, err := execCmd.Output()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return false, proxy.ErrUnknownDecision
		}
		return true, err
	}
	result := strings.TrimSpace(string(output))
	switch result {
	case "True":
		return true, nil
	case "False":
		return false, nil
	case "0":
		return false, proxy.ErrUnknownDecision
	default:
		return false, proxy.ErrUnknownDecision
	}
}

func pythonScriptArgs(prefixArgs []string, scriptPath string) []string {
	args := append([]string(nil), prefixArgs...)
	return append(args, scriptPath)
}

func runPythonListUpdater(root string) error {
	scriptPath := filepath.Join(root, "python", "get.py")
	if _, err := os.Stat(scriptPath); err != nil {
		return fmt.Errorf("python getter script not found: %w", err)
	}
	python, prefixArgs := pythonExecutable(root)
	if python == "" {
		return errors.New("python interpreter not found")
	}
	execCmd := exec.CommandContext(context.Background(), python, pythonScriptArgs(prefixArgs, scriptPath)...)
	execCmd.Env = append(replaceEnvironment(os.Environ(), "NICETOS_DATA_DIR"),
		"NICETOS_DATA_DIR="+filepath.Join(root, "data"))
	execCmd.Dir = root
	output, err := execCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("python script failed: %v: %s", err, strings.TrimSpace(string(output)))
	}
	if output != nil && len(strings.TrimSpace(string(output))) > 0 {
		log.Printf("[LISTS] python/get.py output: %s", strings.TrimSpace(string(output)))
	}
	return nil
}

func (checker *siteChecker) Check(target string) (bool, error) {
	domain := extractDomain(target)
	if domain == "" {
		log.Printf("[CHECK] could not determine the requested domain; blocking")
		return false, nil
	}
	lists := checker.lists.Load()
	if lists == nil {
		log.Printf("[CHECK] domain lists are unavailable for %s; blocking", domain)
		return false, nil
	}
	key := domainHashKey(domain)
	for _, blocked := range lists.blacklist[key] {
		if blocked == domain {
			return false, nil
		}
	}
	for _, allowed := range lists.whitelist[key] {
		if allowed == domain {
			return true, nil
		}
	}
	allowed, err := pythonCheckURL(checker.root, target, checker.ollamaAPI)
	if err != nil {
		if errors.Is(err, proxy.ErrUnknownDecision) {
			log.Printf("[CHECK] unknown decision for %s; blocking because no safe decision is available", domain)
			return false, nil
		}
		log.Printf("[CHECK] decision failed for %s; blocking: %v", domain, err)
		return false, nil
	}
	return allowed, nil
}

func extractDomain(target string) string {
	if !strings.Contains(target, "://") {
		target = "http://" + target
	}
	parsed, err := url.Parse(target)
	if err != nil {
		return ""
	}
	domain := strings.TrimSuffix(parsed.Hostname(), ".")
	if strings.HasPrefix(domain, "www.") {
		domain = domain[4:]
	}
	return domain
}

func domainHashKey(domain string) string {
	digest := md5.Sum([]byte(domain))
	value := new(big.Int).SetBytes(digest[:])
	return new(big.Int).Mod(value, big.NewInt(hashCapacity)).String()
}

func (checker *siteChecker) loadListsFromDisk() error {
	blacklist, err := loadHashMap(filepath.Join(checker.root, "data", "blacklist.json"))
	if err != nil {
		return fmt.Errorf("load blacklist: %w", err)
	}
	whitelist, err := loadHashMap(filepath.Join(checker.root, "data", "whitelist.json"))
	if err != nil {
		return fmt.Errorf("load whitelist: %w", err)
	}
	checker.lists.Store(&domainLists{blacklist: blacklist, whitelist: whitelist})
	return nil
}

func loadHashMap(path string) (map[string][]string, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(contents, &raw); err != nil {
		return nil, err
	}
	result := make(map[string][]string, len(raw))
	for key, value := range raw {
		var entries []string
		if err := json.Unmarshal(value, &entries); err != nil {
			var entry string
			if err := json.Unmarshal(value, &entry); err != nil {
				return nil, fmt.Errorf("invalid entry for hash %s", key)
			}
			entries = []string{entry}
		}
		result[key] = entries
	}
	return result, nil
}

func (checker *siteChecker) startListRefresh(stop <-chan struct{}) {
	go func() {
		checker.refreshLists(context.Background())
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				checker.refreshLists(context.Background())
			}
		}
	}()
}

func (checker *siteChecker) refreshLists(ctx context.Context) {
	checker.updateMu.Lock()
	defer checker.updateMu.Unlock()

	if err := checker.runPythonListUpdater(); err != nil {
		log.Printf("[LISTS] Python updater failed: %v; keeping current lists", err)
		return
	}
	if err := checker.loadListsFromDisk(); err != nil {
		log.Printf("[LISTS] failed to reload lists from disk: %v", err)
		return
	}
	log.Printf("[LISTS] blacklist and whitelist updated via python/get.py")
}

func (checker *siteChecker) runPythonListUpdater() error {
	return runPythonListUpdater(checker.root)
}

func (checker *siteChecker) fetchHashMap(ctx context.Context, sourceURL string) (map[string][]string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return nil, err
	}
	response, err := checker.client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected HTTP status %s", response.Status)
	}
	contents, err := io.ReadAll(io.LimitReader(response.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	return parseHashMap(contents)
}

func parseHashMap(contents []byte) (map[string][]string, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(contents, &raw); err != nil {
		return nil, err
	}
	result := make(map[string][]string, len(raw))
	for key, value := range raw {
		var entries []string
		if err := json.Unmarshal(value, &entries); err != nil {
			var entry string
			if err := json.Unmarshal(value, &entry); err != nil {
				return nil, fmt.Errorf("invalid entry for hash %s", key)
			}
			entries = []string{entry}
		}
		result[key] = entries
	}
	return result, nil
}

func writeHashMap(path string, entries map[string][]string) error {
	contents, err := json.MarshalIndent(entries, "", "    ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	temporary := path + ".new"
	if err := os.WriteFile(temporary, contents, 0644); err != nil {
		return err
	}
	return os.Rename(temporary, path)
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
	checker := newSiteChecker(root)
	if err := checker.loadListsFromDisk(); err != nil {
		log.Fatalf("Failed to load domain lists: %v", err)
	}
	listRefreshStop := make(chan struct{})
	checker.startListRefresh(listRefreshStop)
	defer close(listRefreshStop)

	ollama, err := startBundledOllama(root)
	if err != nil {
		log.Printf("[OLLAMA] local AI unavailable: %v; non-whitelisted domains will be blocked", err)
	} else {
		defer func() {
			if closeErr := ollama.Close(); closeErr != nil {
				log.Printf("[OLLAMA] failed to stop local server: %v", closeErr)
			}
		}()
		checker.ollamaAPI = ollama.apiURL
		log.Printf("[OLLAMA] project-local model is ready at %s", ollama.apiURL)
	}

	server, err := proxy.StartTransparentProxy(*listenAddress, checker)
	if err != nil {
		log.Printf("Failed to start proxy: %v", err)
		return
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
		log.Printf("Proxy serve error: %v", err)
	}
}
