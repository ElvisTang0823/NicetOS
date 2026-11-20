package proxy

import (
	"errors"
	"net"
	"net/http"
	"sync"
)

var ErrUnknownDecision = errors.New("unknown URL decision")

type URLChecker interface {
	Check(string) (bool, error)
	BlockedPagePath() string
}

type TransparentProxy struct {
	listener     net.Listener
	server       *http.Server
	done         chan struct{}
	restore      func() error
	trustRestore func() error
	ca           *certificateAuthority
	chain        string
	closeOnce    sync.Once
	closeErr     error
}

// checkWithReason 會盡量取得封鎖原因；checker 不支援時只回傳允許／封鎖。
func checkWithReason(checker URLChecker, target string) (bool, string, error) {
	if detailed, ok := checker.(interface {
		CheckWithReason(string) (bool, string, error)
	}); ok {
		return detailed.CheckWithReason(target)
	}
	allowed, err := checker.Check(target)
	return allowed, "", err
}
