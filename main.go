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

func (checker *siteChecker) Check(target string) (bool, error) {
	checker.mu.Lock()
	defer checker.mu.Unlock()

	command := exec.Command(checker.python, "-c", "import main; print(main.check_url(__import__('sys').argv[1]))", target)
	command.Dir = checker.root
	command.Env = append(os.Environ(), "PYTHONPATH="+filepath.Join(checker.root, "python"))
	output, err := command.Output()
	if err != nil {
		return false, fmt.Errorf("main.py check failed: %w", err)
	}

	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	if len(lines) == 0 {
		return false, errors.New("main.py returned no result")
	}
	return strings.TrimSpace(lines[len(lines)-1]) != "False", nil
}

func proxyRoot() (string, error) {
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.Dir(filepath.Clean(executable)), nil
}

func main() {
	flag.Parse()
	root, err := proxyRoot()
	if err != nil {
		log.Fatal(err)
	}

	server, err := proxy.StartTransparentProxy(*listenAddress, &siteChecker{python: *pythonCommand, root: root})
	if err != nil {
		log.Fatal(err)
	}
	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-shutdown
		log.Println("stopping NicetOS proxy")
		_ = server.Close()
	}()

	log.Printf("NicetOS transparent proxy listening on %s; press Ctrl+C to stop", *listenAddress)
	if err := server.Serve(); err != nil && !errors.Is(err, net.ErrClosed) {
		log.Fatal(err)
	}
}
