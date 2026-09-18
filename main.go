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

type siteChecker struct {
	root     string
	urls     map[string]string
	client   *http.Client
	lists    atomic.Pointer[domainLists]
	updateMu sync.Mutex
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

func (checker *siteChecker) Check(target string) (bool, error) {
	domain := extractDomain(target)
	if domain == "" {
		return true, nil
	}
	lists := checker.lists.Load()
	if lists == nil {
		return true, errors.New("domain lists are not loaded")
	}
	key := domainHashKey(domain)
	for _, blocked := range lists.blacklist[key] {
		if blocked == domain {
			return false, nil
		}
	}
	return true, nil
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
	type fetchedList struct {
		name    string
		entries map[string][]string
		err     error
	}
	results := make(chan fetchedList, len(checker.urls))
	for name, sourceURL := range checker.urls {
		go func(name, sourceURL string) {
			entries, err := checker.fetchHashMap(ctx, sourceURL)
			results <- fetchedList{name: name, entries: entries, err: err}
		}(name, sourceURL)
	}
	fetched := make(map[string]map[string][]string, len(checker.urls))
	for range checker.urls {
		result := <-results
		if result.err != nil {
			log.Printf("[LISTS] %s update failed: %v; keeping current lists", result.name, result.err)
			return
		}
		fetched[result.name] = result.entries
	}
	for name, entries := range fetched {
		if err := writeHashMap(filepath.Join(checker.root, "data", name+".json"), entries); err != nil {
			log.Printf("[LISTS] %s download succeeded but could not save it: %v", name, err)
			return
		}
	}
	checker.lists.Store(&domainLists{blacklist: fetched["blacklist"], whitelist: fetched["whitelist"]})
	log.Printf("[LISTS] blacklist and whitelist updated")
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

	server, err := proxy.StartTransparentProxy(*listenAddress, checker)
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
