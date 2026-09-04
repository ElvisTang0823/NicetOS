//go:build !linux

package proxy

import (
	"io"
	"net"
	"net/http"
)

type TransparentProxy struct {
	listener net.Listener
	server   *http.Server
	done     chan struct{}
	restore  func() error
}

func StartTransparentProxy(address string, checker URLChecker) (*TransparentProxy, error) {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}
	restore, err := configurePlatformProxy(address)
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	proxy := &TransparentProxy{listener: listener, done: make(chan struct{}), restore: restore}
	proxy.server = &http.Server{Handler: http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		target := request.URL.String()
		if request.Method == http.MethodConnect {
			target = "https://" + request.Host
		}
		allowed, checkErr := checker.Check(target)
		if checkErr != nil {
			http.Error(response, "site check unavailable", http.StatusServiceUnavailable)
			return
		}
		if !allowed {
			http.Error(response, "access denied by NicetOS", http.StatusForbidden)
			return
		}
		if request.Method == http.MethodConnect {
			handleBasicConnect(response, request)
			return
		}
		handleBasicHTTP(response, request)
	})}
	go func() { _ = proxy.server.Serve(listener) }()
	return proxy, nil
}

func (proxy *TransparentProxy) Serve() error {
	<-proxy.done
	return net.ErrClosed
}

func (proxy *TransparentProxy) Close() error {
	err := proxy.server.Close()
	if restoreErr := proxy.restore(); err == nil {
		err = restoreErr
	}
	close(proxy.done)
	return err
}

func handleBasicHTTP(response http.ResponseWriter, request *http.Request) {
	request.RequestURI = ""
	upstream, err := (&http.Transport{}).RoundTrip(request)
	if err != nil {
		http.Error(response, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer upstream.Body.Close()
	for key, values := range upstream.Header {
		for _, value := range values {
			response.Header().Add(key, value)
		}
	}
	response.WriteHeader(upstream.StatusCode)
	_, _ = io.Copy(response, upstream.Body)
}

func handleBasicConnect(response http.ResponseWriter, request *http.Request) {
	upstream, err := net.Dial("tcp", request.Host)
	if err != nil {
		http.Error(response, "upstream connection failed", http.StatusBadGateway)
		return
	}
	defer upstream.Close()
	hijacker, ok := response.(http.Hijacker)
	if !ok {
		http.Error(response, "CONNECT is not supported", http.StatusInternalServerError)
		return
	}
	client, buffered, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer client.Close()
	_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	if buffered.Reader.Buffered() > 0 {
		go io.Copy(upstream, buffered)
	} else {
		go io.Copy(upstream, client)
	}
	_, _ = io.Copy(client, upstream)
}
