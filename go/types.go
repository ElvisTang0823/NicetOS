package proxy

import (
	"errors"
	"net"
	"net/http"
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
}
