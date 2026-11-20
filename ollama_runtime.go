package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const bundledModelName = "gemma4-child-safety"
const ollamaInferenceCheckTimeout = 5 * time.Minute

type bundledOllama struct {
	apiURL    string
	process   *exec.Cmd
	done      chan struct{}
	waitErr   error
	closeOnce sync.Once
}

func startBundledOllama(root string) (*bundledOllama, error) {
	log.Println("[OLLAMA] 支援測試開始：檢查隨附執行檔、模型並實際執行推論")
	binDir := filepath.Join(root, "ollama", "bin")
	executable := filepath.Join(binDir, "ollama.exe")
	if info, err := os.Stat(executable); err != nil {
		return nil, fmt.Errorf("找不到隨附的 Ollama 執行檔（%s）：%w", executable, err)
	} else if info.IsDir() {
		return nil, fmt.Errorf("Ollama 執行檔路徑不是檔案：%s", executable)
	}
	log.Println("[OLLAMA] 執行檔檢查：通過")

	sourceDir := filepath.Join(root, "ollama", "models")
	modelfile := filepath.Join(sourceDir, "Modelfile")
	modelFile := filepath.Join(sourceDir, "gemma4_child_safety_q4.gguf")
	for _, path := range []string{modelfile, modelFile} {
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("找不到必要的 Gemma4 模型檔案（%s）：%w", path, err)
		}
	}
	log.Println("[OLLAMA] 模型檔檢查：通過；若沒有可用 GPU，Ollama 會嘗試使用 CPU")

	modelStore := filepath.Join(root, "ollama", "runtime-models")
	if err := os.MkdirAll(modelStore, 0755); err != nil {
		return nil, fmt.Errorf("無法建立 Ollama 模型資料夾：%w", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("無法配置 Ollama 本機 API 連接埠：%w", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		return nil, fmt.Errorf("無法釋放 Ollama 本機 API 連接埠：%w", err)
	}

	apiURL := "http://" + address
	log.Println("[OLLAMA] 正在啟動本機服務並檢查 API")
	server := exec.Command(executable, "serve")
	server.Env = ollamaEnvironment(os.Environ(), address, modelStore)
	server.Stdout = os.Stderr
	server.Stderr = os.Stderr
	if err := server.Start(); err != nil {
		return nil, fmt.Errorf("無法啟動隨附的 Ollama 服務：%w", err)
	}
	runtime := &bundledOllama{
		apiURL:  apiURL,
		process: server,
		done:    make(chan struct{}),
	}
	go func() {
		runtime.waitErr = server.Wait()
		close(runtime.done)
	}()

	if err := runtime.waitUntilReady(45 * time.Second); err != nil {
		_ = runtime.Close()
		return nil, fmt.Errorf("Ollama API 未能在 45 秒內啟動：%w", err)
	}
	log.Println("[OLLAMA] API 檢查：通過")
	if err := runtime.ensureModel(executable, sourceDir, modelfile, modelStore); err != nil {
		_ = runtime.Close()
		return nil, fmt.Errorf("Gemma4 模型準備失敗：%w", err)
	}
	log.Println("[OLLAMA] Gemma4 實際推論測試中；首次載入可能需要數分鐘，最長等待 5 分鐘")
	if err := runtime.checkModelInference(); err != nil {
		_ = runtime.Close()
		return nil, fmt.Errorf("Gemma4 實際推論測試失敗：%w", err)
	}
	log.Println("[OLLAMA] 支援測試：通過；此電腦可執行隨附 Ollama 與 Gemma4 推論")
	return runtime, nil
}

func ollamaEnvironment(environment []string, host string, modelStore string) []string {
	result := make([]string, 0, len(environment)+3)
	for _, entry := range environment {
		name, _, found := strings.Cut(entry, "=")
		if found && (strings.EqualFold(name, "OLLAMA_HOST") ||
			strings.EqualFold(name, "OLLAMA_MODELS") ||
			strings.EqualFold(name, "OLLAMA_NO_CLOUD")) {
			continue
		}
		result = append(result, entry)
	}
	return append(result,
		"OLLAMA_HOST="+host,
		"OLLAMA_MODELS="+modelStore,
		"OLLAMA_NO_CLOUD=1",
	)
}

func (runtime *bundledOllama) waitUntilReady(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: time.Second}
	for time.Now().Before(deadline) {
		select {
		case <-runtime.done:
			if runtime.waitErr != nil {
				return fmt.Errorf("project-local Ollama exited during startup: %w", runtime.waitErr)
			}
			return errors.New("project-local Ollama exited during startup")
		default:
		}

		response, err := client.Get(runtime.apiURL + "/api/tags")
		if err == nil {
			_ = response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("project-local Ollama API did not become ready within %s", timeout)
}

func (runtime *bundledOllama) ensureModel(executable string, sourceDir string, modelfile string, modelStore string) error {
	found, err := runtime.hasModel()
	if err != nil {
		return fmt.Errorf("無法讀取 Ollama 模型清單：%w", err)
	}
	if found {
		log.Printf("[OLLAMA] 模型檢查：%s 已註冊", bundledModelName)
		return nil
	}

	log.Printf("[OLLAMA] 模型尚未註冊，正在匯入隨附 GGUF；首次啟動可能需要數分鐘")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	create := exec.CommandContext(ctx, executable, "create", bundledModelName, "-f", modelfile)
	create.Dir = sourceDir
	create.Env = ollamaEnvironment(os.Environ(), strings.TrimPrefix(runtime.apiURL, "http://"), modelStore)
	create.Stdout = os.Stderr
	create.Stderr = os.Stderr
	err = create.Run()
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("註冊 Gemma4 模型逾時：%w", ctx.Err())
		}
		return fmt.Errorf("註冊 Gemma4 模型失敗：%w", err)
	}

	found, err = runtime.hasModel()
	if err != nil {
		return fmt.Errorf("無法確認 Gemma4 模型是否註冊：%w", err)
	}
	if !found {
		return fmt.Errorf("Ollama 完成註冊，但模型清單中找不到 %q", bundledModelName)
	}
	return nil
}

func (runtime *bundledOllama) hasModel() (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, runtime.apiURL+"/api/tags", nil)
	if err != nil {
		return false, err
	}
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false, fmt.Errorf("unexpected Ollama response: %s", response.Status)
	}
	var result struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return false, err
	}
	for _, model := range result.Models {
		if model.Name == bundledModelName || model.Name == bundledModelName+":latest" {
			return true, nil
		}
	}
	return false, nil
}

func (runtime *bundledOllama) checkModelInference() error {
	ctx, cancel := context.WithTimeout(context.Background(), ollamaInferenceCheckTimeout)
	defer cancel()

	payload := map[string]any{
		"model": bundledModelName,
		"messages": []map[string]string{
			{"role": "user", "content": "Reply with OK only."},
		},
		"stream":     false,
		"keep_alive": "1h",
		"options":    map[string]int{"num_predict": 4},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("建立推論測試內容失敗：%w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, runtime.apiURL+"/api/chat", strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("建立推論測試請求失敗：%w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: ollamaInferenceCheckTimeout}).Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("等待 Gemma4 推論超過 %s：%w", ollamaInferenceCheckTimeout, ctx.Err())
		}
		return fmt.Errorf("無法呼叫 Ollama 推論 API：%w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Ollama 推論 API 回應錯誤：%s", response.Status)
	}
	var result struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return fmt.Errorf("無法讀取 Ollama 推論結果：%w", err)
	}
	if strings.TrimSpace(result.Message.Content) == "" {
		return errors.New("Ollama 推論成功回應，但 Gemma4 沒有回傳內容")
	}
	return nil
}

func (runtime *bundledOllama) Close() error {
	if runtime == nil {
		return nil
	}
	var closeErr error
	runtime.closeOnce.Do(func() {
		select {
		case <-runtime.done:
			if runtime.waitErr != nil && !errors.Is(runtime.waitErr, os.ErrProcessDone) {
				closeErr = runtime.waitErr
			}
		default:
			if err := runtime.process.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
				closeErr = err
			}
			<-runtime.done
		}
	})
	return closeErr
}
