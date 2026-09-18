package proxy

import (
	"net"
	"net/http"
)

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
