//go:build !linux

package proxy

import (
	"bufio"
	"crypto/tls"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
)

func StartTransparentProxy(address string, checker URLChecker) (*TransparentProxy, error) {
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, err
	}
	ca, err := newEphemeralCertificateAuthority()
	if err != nil {
		_ = listener.Close()
		return nil, err
	}
	restore, err := configurePlatformProxy(address)
	if err != nil {
		_ = ca.close()
		_ = listener.Close()
		return nil, err
	}
	trustRestore, err := configureCertificateTrust(ca.certPath)
	if err != nil {
		_ = restore()
		_ = ca.close()
		_ = listener.Close()
		return nil, err
	}
	proxy := &TransparentProxy{listener: listener, done: make(chan struct{}), restore: restore, trustRestore: trustRestore, ca: ca}
	proxy.server = &http.Server{Handler: http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodConnect {
			handleInspectedConnect(response, request, checker, ca)
			return
		}
		handleInspectedHTTP(response, request, checker)
	})}
	go func() { _ = proxy.server.Serve(listener) }()
	return proxy, nil
}

func (proxy *TransparentProxy) Serve() error {
	<-proxy.done
	return net.ErrClosed
}

func (proxy *TransparentProxy) Close() error {
	log.Println("[PROXY] Closing proxy server")
	err := proxy.server.Close()
	if err != nil {
		log.Printf("[PROXY] Server close error: %v", err)
	}

	// 無論伺服器是否正常關閉，都要嘗試恢復系統設定
	restoreErr := proxy.restore()
	if restoreErr != nil {
		log.Printf("[PROXY] Settings restore error: %v", restoreErr)
		if err == nil {
			err = restoreErr
		}
	}
	if proxy.trustRestore != nil {
		if trustErr := proxy.trustRestore(); trustErr != nil {
			log.Printf("[PROXY] CA trust restore error: %v", trustErr)
			if err == nil {
				err = trustErr
			}
		}
	}
	if caErr := proxy.ca.close(); caErr != nil {
		log.Printf("[PROXY] CA cleanup error: %v", caErr)
		if err == nil {
			err = caErr
		}
	}

	close(proxy.done)
	return err
}

// handleBlockedConnect completes CONNECT first, then serves the block page over
// TLS. Chromium otherwise replaces an HTTP CONNECT denial with its own error.
func handleBlockedConnect(response http.ResponseWriter, request *http.Request, checker URLChecker, ca *certificateAuthority, reason string) {
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
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	config, err := ca.tlsConfig(request.Host)
	if err != nil {
		log.Printf("[PROXY] Cannot create blocked-page certificate for %s: %v", request.Host, err)
		return
	}
	tlsConnection := tls.Server(&readerConn{Conn: client, reader: buffered.Reader}, config)
	if err := tlsConnection.Handshake(); err != nil {
		log.Printf("[PROXY] TLS handshake for blocked %s failed: %v", request.Host, err)
		return
	}
	defer tlsConnection.Close()
	if _, err := http.ReadRequest(bufio.NewReader(tlsConnection)); err != nil {
		return
	}
	writeBlockedHTTPResponse(tlsConnection, checker, reason)
}

func serveBlockedPage(response http.ResponseWriter, checker URLChecker, reason string) {
	pagePath := checker.BlockedPagePath()
	body, err := readBlockedPageBody(checker, reason)
	if err != nil {
		log.Printf("[PROXY] Failed to read blocked page %s: %v", pagePath, err)
		http.Error(response, "access denied by NicetOS", http.StatusForbidden)
		return
	}
	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	response.WriteHeader(http.StatusForbidden)
	_, _ = response.Write(body)
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

func handleInspectedHTTP(response http.ResponseWriter, request *http.Request, checker URLChecker) {
	target := request.URL.String()
	if request.Host != "" && !sameHostname(request.URL.Host, request.Host) {
		serveBlockedPage(response, checker, "HTTP request target 與 Host 不一致，依安全規則封鎖。")
		return
	}
	if known, allowed, reason := knownDecision(checker, target); known && !allowed {
		serveBlockedPage(response, checker, reason)
		return
	}
	request.RequestURI = ""
	request.Header.Del("Accept-Encoding")
	upstream, err := (&http.Transport{}).RoundTrip(request)
	if err != nil {
		http.Error(response, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer upstream.Body.Close()
	allowed, reason, err := inspectResponse(checker, target, upstream)
	if err != nil {
		log.Printf("[PROXY] failed to inspect %s: %v", target, err)
		http.Error(response, "upstream response inspection failed", http.StatusBadGateway)
		return
	}
	if !allowed {
		serveBlockedPage(response, checker, reason)
		return
	}
	for key, values := range upstream.Header {
		for _, value := range values {
			response.Header().Add(key, value)
		}
	}
	response.WriteHeader(upstream.StatusCode)
	_, _ = io.Copy(response, upstream.Body)
}

func handleInspectedConnect(response http.ResponseWriter, request *http.Request, checker URLChecker, ca *certificateAuthority) {
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
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	if err := serveInspectedTLSConnection(client, buffered.Reader, request.Host, checker, ca, nil); err != nil {
		log.Printf("[PROXY] HTTPS inspection failed for %s: %v", request.Host, err)
	}
}

func handleBasicConnect(response http.ResponseWriter, request *http.Request, checker URLChecker) {
	target := "https://" + request.Host
	allowed, reason, err := checkWithReason(checker, target)
	if err != nil {
		if errors.Is(err, ErrUnknownDecision) {
			log.Printf("[PROXY] Unknown decision for CONNECT %s; allowing through", target)
			// fall through to connect upstream
		} else {
			serveBlockedPage(response, checker, reason)
			return
		}
	} else if !allowed {
		serveBlockedPage(response, checker, reason)
		return
	}
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
