//go:build !linux

package proxy

import (
	"bufio"
	"crypto/tls"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"time"
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
		handleProxyRequest(response, request, checker, ca)
	})}
	go func() { _ = proxy.server.Serve(listener) }()
	return proxy, nil
}

// handleProxyRequest 每個連線只檢查一次網址；無法判斷或檢查出錯時一律封鎖。
func handleProxyRequest(response http.ResponseWriter, request *http.Request, checker URLChecker, ca *certificateAuthority) {
	isConnect := request.Method == http.MethodConnect
	host := request.Host
	if !isConnect && request.URL.Host != "" {
		host = request.URL.Host
	}
	// 本機位址（例如 NicetOS 自己的 Ollama）不檢查，直接放行。
	if isLoopbackHost(host) {
		if isConnect {
			tunnelConnect(response, request)
		} else {
			handleBasicHTTP(response, request)
		}
		return
	}

	target := request.URL.String()
	if isConnect {
		target = "https://" + request.Host
	}
	log.Printf("[PROXY] Checking: %s", target)
	allowed, reason, checkErr := checkWithReason(checker, target)
	if checkErr != nil {
		log.Printf("[PROXY] Check error for %s: %v (blocking)", target, checkErr)
		allowed = false
		if strings.TrimSpace(reason) == "" {
			reason = fmt.Sprintf("NicetOS 無法完成判斷（%v），依安全規則封鎖。", checkErr)
		}
	}
	if !allowed {
		log.Printf("[PROXY] BLOCKED: %s; reason=%q", target, reason)
		if isConnect {
			handleBlockedConnect(response, request, checker, ca, reason)
			return
		}
		serveBlockedPage(response, checker, reason)
		return
	}
	log.Printf("[PROXY] ALLOWED: %s", target)
	if isConnect {
		tunnelConnect(response, request)
		return
	}
	handleBasicHTTP(response, request)
}

func isLoopbackHost(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	host = strings.Trim(strings.TrimSuffix(strings.ToLower(host), "."), "[]")
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (proxy *TransparentProxy) Serve() error {
	<-proxy.done
	return net.ErrClosed
}

// Close 可以安全地重複呼叫（Ctrl+C 和程式結束時都會呼叫一次）。
func (proxy *TransparentProxy) Close() error {
	proxy.closeOnce.Do(func() {
		proxy.closeErr = proxy.closeNow()
	})
	return proxy.closeErr
}

func (proxy *TransparentProxy) closeNow() error {
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

func writeBlockedHTTPResponse(writer io.Writer, checker URLChecker, reason string) {
	body, err := readBlockedPageBody(checker, reason)
	if err != nil {
		_, _ = io.WriteString(writer, "HTTP/1.1 403 Forbidden\r\nConnection: close\r\nContent-Length: 24\r\n\r\naccess denied by NicetOS")
		return
	}
	_, _ = fmt.Fprintf(writer, "HTTP/1.1 403 Forbidden\r\nContent-Type: text/html; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
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

// upstreamTransport 轉送一般 HTTP 請求；不讀系統 Proxy，避免繞回 NicetOS 自己。
var upstreamTransport = &http.Transport{
	Proxy:                 nil,
	DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	MaxIdleConns:          100,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ResponseHeaderTimeout: 60 * time.Second,
}

// 只在兩端之間有效、不應轉送的標頭。
var hopByHopHeaders = []string{
	"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

func removeHopByHopHeaders(header http.Header) {
	for _, value := range header.Values("Connection") {
		for _, name := range strings.Split(value, ",") {
			header.Del(strings.TrimSpace(name))
		}
	}
	for _, name := range hopByHopHeaders {
		header.Del(name)
	}
}

func handleBasicHTTP(response http.ResponseWriter, request *http.Request) {
	outgoing := request.Clone(request.Context())
	outgoing.RequestURI = ""
	removeHopByHopHeaders(outgoing.Header)
	upstream, err := upstreamTransport.RoundTrip(outgoing)
	if err != nil {
		http.Error(response, "upstream request failed", http.StatusBadGateway)
		return
	}
	defer upstream.Body.Close()
	removeHopByHopHeaders(upstream.Header)
	for key, values := range upstream.Header {
		for _, value := range values {
			response.Header().Add(key, value)
		}
	}
	response.WriteHeader(upstream.StatusCode)
	_, _ = io.Copy(response, upstream.Body)
}

// handleBasicConnect 先檢查再建立 HTTPS 通道；無法判斷或出錯時封鎖。
func handleBasicConnect(response http.ResponseWriter, request *http.Request, checker URLChecker) {
	target := "https://" + request.Host
	allowed, reason, err := checkWithReason(checker, target)
	if err != nil || !allowed {
		if err != nil && strings.TrimSpace(reason) == "" {
			reason = fmt.Sprintf("NicetOS 無法完成判斷（%v），依安全規則封鎖。", err)
		}
		serveBlockedPage(response, checker, reason)
		return
	}
	tunnelConnect(response, request)
}

// tunnelConnect 建立 HTTPS 通道，不再重複檢查網址。
func tunnelConnect(response http.ResponseWriter, request *http.Request) {
	upstream, err := net.DialTimeout("tcp", request.Host, 10*time.Second)
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
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	go func() {
		_, _ = io.Copy(upstream, buffered.Reader)
		if tcp, ok := upstream.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
	}()
	_, _ = io.Copy(client, upstream)
}
