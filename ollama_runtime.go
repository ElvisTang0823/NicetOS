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
	log.Println("[OLLAMA] compatibility check: validating the bundled Windows runtime and model files")
	binDir := filepath.Join(root, "ollama", "bin")
	executable := filepath.Join(binDir, "ollama.exe")
	if info, err := os.Stat(executable); err != nil {
		return nil, fmt.Errorf("project-local executable not found at %s: %w", executable, err)
	} else if info.IsDir() {
		return nil, fmt.Errorf("project-local executable path is a directory: %s", executable)
	}
	log.Println("[OLLAMA] bundled executable found")

	sourceDir := filepath.Join(root, "ollama", "models")
	modelfile := filepath.Join(sourceDir, "Modelfile")
	modelFile := filepath.Join(sourceDir, "gemma4_child_safety_q4.gguf")
	for _, path := range []string{modelfile, modelFile} {
		if _, err := os.Stat(path); err != nil {
			return nil, fmt.Errorf("required model source file %s: %w", path, err)
		}
	}
	log.Println("[OLLAMA] bundled model files found; CPU inference is supported when GPU acceleration is unavailable")

	modelStore := filepath.Join(root, "ollama", "runtime-models")
	if err := os.MkdirAll(modelStore, 0755); err != nil {
		return nil, fmt.Errorf("create project-local model store: %w", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("select local API port: %w", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		return nil, fmt.Errorf("release local API port: %w", err)
	}

	apiURL := "http://" + address
	log.Println("[OLLAMA] starting local runtime to verify computer compatibility")
	server := exec.Command(executable, "serve")
	server.Env = ollamaEnvironment(os.Environ(), address, modelStore)
	server.Stdout = os.Stderr
	server.Stderr = os.Stderr
	if err := server.Start(); err != nil {
		return nil, fmt.Errorf("start project-local Ollama: %w", err)
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
		return nil, err
	}
	log.Println("[OLLAMA] local API is responding")
	if err := runtime.ensureModel(executable, sourceDir, modelfile, modelStore); err != nil {
		_ = runtime.Close()
		return nil, err
	}
	log.Println("[OLLAMA] testing Gemma4 inference on this computer; first model load may take several minutes")
	if err := runtime.checkModelInference(); err != nil {
		_ = runtime.Close()
		return nil, fmt.Errorf("Gemma4 inference compatibility check failed: %w", err)
	}
	log.Println("[OLLAMA] compatibility check passed: bundled runtime and Gemma4 inference are usable")
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
		return fmt.Errorf("check project-local model registry: %w", err)
	}
	if found {
		return nil
	}

	log.Printf("[OLLAMA] importing the bundled GGUF model; first startup may take several minutes")
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
			return fmt.Errorf("register local model: %w", ctx.Err())
		}
		return fmt.Errorf("register local model: %w", err)
	}

	found, err = runtime.hasModel()
	if err != nil {
		return fmt.Errorf("verify registered model: %w", err)
	}
	if !found {
		return fmt.Errorf("Ollama create completed but model %q is not listed", bundledModelName)
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
		return fmt.Errorf("encode inference check request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, runtime.apiURL+"/api/chat", strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("create inference check request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: ollamaInferenceCheckTimeout}).Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("inference check timed out after %s: %w", ollamaInferenceCheckTimeout, ctx.Err())
		}
		return fmt.Errorf("send inference check request: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("inference check returned %s", response.Status)
	}
	var result struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode inference check response: %w", err)
	}
	if strings.TrimSpace(result.Message.Content) == "" {
		return errors.New("inference check returned an empty response")
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
