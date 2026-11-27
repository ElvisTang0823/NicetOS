package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"html"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const maxPageInspectionBytes = 1 << 20

type pageContentChecker interface {
	CheckWithContent(string, string, []byte) (bool, string, error)
}

type knownURLChecker interface {
	CheckKnown(string) (bool, bool, string)
}

// certificateAuthority signs certificates for the local HTTPS filtering page.
type certificateAuthority struct {
	certificate tls.Certificate
	parsed      *x509.Certificate
	privateKey  *rsa.PrivateKey
	certPath    string
	removeCert  bool
	mu          sync.Mutex
	issued      map[string]tls.Certificate
}

// readerConn preserves bytes already buffered while allowing tls.Server to read
// the same TCP connection.
type readerConn struct {
	net.Conn
	reader *bufio.Reader
}

func (connection *readerConn) Read(buffer []byte) (int, error) { return connection.reader.Read(buffer) }

func newCertificateAuthority(checker URLChecker) (*certificateAuthority, error) {
	root := filepath.Dir(filepath.Dir(checker.BlockedPagePath()))
	directory := filepath.Join(root, "data")
	certPath := filepath.Join(directory, "nicetos-proxy-ca-cert.pem")
	keyPath := filepath.Join(directory, "nicetos-proxy-ca-key.pem")
	certPEM, certErr := os.ReadFile(certPath)
	keyPEM, keyErr := os.ReadFile(keyPath)
	if os.IsNotExist(certErr) && os.IsNotExist(keyErr) {
		if err := os.MkdirAll(directory, 0700); err != nil {
			return nil, err
		}
		var err error
		certPEM, keyPEM, err = createCertificateAuthority()
		if err != nil {
			return nil, err
		}
		if err = os.WriteFile(certPath, certPEM, 0644); err != nil {
			return nil, err
		}
		if err = os.WriteFile(keyPath, keyPEM, 0600); err != nil {
			_ = os.Remove(certPath)
			return nil, err
		}
	} else if certErr != nil || keyErr != nil {
		return nil, fmt.Errorf("both NicetOS CA certificate and key are required: certificate=%w, key=%v", certErr, keyErr)
	}
	return parseCertificateAuthority(certPEM, keyPEM, certPath, false)
}

func newEphemeralCertificateAuthority() (*certificateAuthority, error) {
	certPEM, keyPEM, err := createCertificateAuthority()
	if err != nil {
		return nil, err
	}
	certFile, err := os.CreateTemp("", "nicetos-proxy-ca-*.pem")
	if err != nil {
		return nil, fmt.Errorf("create temporary CA certificate file: %w", err)
	}
	certPath := certFile.Name()
	if err := certFile.Chmod(0600); err != nil {
		_ = certFile.Close()
		_ = os.Remove(certPath)
		return nil, fmt.Errorf("restrict temporary CA certificate permissions: %w", err)
	}
	if _, err := certFile.Write(certPEM); err != nil {
		_ = certFile.Close()
		_ = os.Remove(certPath)
		return nil, fmt.Errorf("write temporary CA certificate: %w", err)
	}
	if err := certFile.Close(); err != nil {
		_ = os.Remove(certPath)
		return nil, fmt.Errorf("close temporary CA certificate: %w", err)
	}
	return parseCertificateAuthority(certPEM, keyPEM, certPath, true)
}

func parseCertificateAuthority(certPEM []byte, keyPEM []byte, certPath string, removeCert bool) (*certificateAuthority, error) {
	defer func() {
		for i := range keyPEM {
			keyPEM[i] = 0
		}
	}()
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		if removeCert {
			_ = os.Remove(certPath)
		}
		return nil, err
	}
	parsed, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		if removeCert {
			_ = os.Remove(certPath)
		}
		return nil, err
	}
	key, ok := pair.PrivateKey.(*rsa.PrivateKey)
	if !ok {
		if removeCert {
			_ = os.Remove(certPath)
		}
		return nil, fmt.Errorf("NicetOS CA key is not RSA")
	}
	return &certificateAuthority{certificate: pair, parsed: parsed, privateKey: key, certPath: certPath, removeCert: removeCert, issued: make(map[string]tls.Certificate)}, nil
}

func (authority *certificateAuthority) close() error {
	if authority == nil || authority.certPath == "" || !authority.removeCert {
		return nil
	}
	if err := os.Remove(authority.certPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove temporary CA certificate: %w", err)
	}
	return nil
}

func createCertificateAuthority() ([]byte, []byte, error) {
	key, err := rsa.GenerateKey(rand.Reader, 3072)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "NicetOS Local HTTPS Filtering CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(5, 0, 0), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), nil
}

func (authority *certificateAuthority) tlsConfig(host string) (*tls.Config, error) {
	certificate, err := authority.certificateFor(host)
	if err != nil {
		return nil, err
	}
	return &tls.Config{Certificates: []tls.Certificate{certificate}, NextProtos: []string{"http/1.1"}, MinVersion: tls.VersionTLS12}, nil
}

func (authority *certificateAuthority) certificateFor(host string) (tls.Certificate, error) {
	host = certificateHost(host)
	if host == "" {
		return tls.Certificate{}, fmt.Errorf("empty HTTPS host")
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if certificate, ok := authority.issued[host]; ok {
		return certificate, nil
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: host}, NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(0, 7, 0), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	if ip := net.ParseIP(host); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else {
		template.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, authority.parsed, &key.PublicKey, authority.privateKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	certificate := tls.Certificate{Certificate: [][]byte{der, authority.certificate.Certificate[0]}, PrivateKey: key}
	authority.issued[host] = certificate
	return certificate, nil
}

func certificateHost(host string) string {
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		host = parsed
	}
	return strings.Trim(strings.TrimSpace(host), "[]")
}

func readBlockedPageBody(checker URLChecker, reasons ...string) ([]byte, error) {
	body, err := os.ReadFile(checker.BlockedPagePath())
	if err != nil {
		return nil, err
	}
	reason := ""
	if len(reasons) > 0 {
		reason = reasons[0]
	}
	body = []byte(strings.ReplaceAll(string(body), "{{BLOCK_REASON}}", html.EscapeString(reason)))
	return body, nil
}

func knownDecision(checker URLChecker, target string) (bool, bool, string) {
	detailed, ok := checker.(knownURLChecker)
	if !ok {
		return false, false, ""
	}
	return detailed.CheckKnown(target)
}

func inspectAndWriteResponse(writer io.Writer, checker URLChecker, target string, response *http.Response) error {
	upstreamBody := response.Body
	defer upstreamBody.Close()
	allowed, reason, err := inspectResponse(checker, target, response)
	if err != nil {
		log.Printf("[PROXY] content check failed for %s: %v", target, err)
		allowed = false
		reason = "NicetOS 無法完成網頁內容檢查，依安全規則封鎖。"
	}
	if !allowed {
		writeBlockedHTTPResponse(writer, checker, reason)
		return nil
	}
	return response.Write(writer)
}

func inspectResponse(checker URLChecker, target string, response *http.Response) (bool, string, error) {
	known, allowed, reason := knownDecision(checker, target)
	if !known {
		pageContent, err := readPageContentForInspection(response)
		if err != nil {
			return false, "", fmt.Errorf("read upstream response for inspection: %w", err)
		}
		if detailed, ok := checker.(pageContentChecker); ok {
			allowed, reason, err = detailed.CheckWithContent(target, response.Header.Get("Content-Type"), pageContent)
		} else {
			allowed, reason, err = checkWithReason(checker, target)
		}
		if err != nil {
			return false, "", fmt.Errorf("check response content: %w", err)
		}
	}
	return allowed, reason, nil
}

func readPageContentForInspection(response *http.Response) ([]byte, error) {
	contentType := strings.ToLower(response.Header.Get("Content-Type"))
	if !strings.Contains(contentType, "text/html") && !strings.Contains(contentType, "application/xhtml+xml") {
		return nil, nil
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxPageInspectionBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxPageInspectionBytes {
		response.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body), response.Body))
		return body[:maxPageInspectionBytes], nil
	}
	response.Body = io.NopCloser(bytes.NewReader(body))
	response.ContentLength = int64(len(body))
	return body, nil
}

func writeBlockedHTTPResponse(writer io.Writer, checker URLChecker, reason string) {
	body, err := readBlockedPageBody(checker, reason)
	if err != nil {
		_, _ = io.WriteString(writer, "HTTP/1.1 403 Forbidden\r\nConnection: close\r\nContent-Length: 24\r\n\r\naccess denied by NicetOS")
		return
	}
	_, _ = fmt.Fprintf(writer, "HTTP/1.1 403 Forbidden\r\nContent-Type: text/html; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
}

func normalizedHostname(host string) (string, error) {
	host = strings.TrimSpace(host)
	if parsed, _, err := net.SplitHostPort(host); err == nil {
		host = parsed
	} else if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = strings.Trim(host, "[]")
	} else if strings.Contains(host, ":") && net.ParseIP(host) == nil {
		return "", fmt.Errorf("invalid host %q", host)
	}
	host = strings.TrimSuffix(strings.Trim(strings.ToLower(host), "[]"), ".")
	if host == "" {
		return "", fmt.Errorf("empty host")
	}
	return host, nil
}

func sameHostname(first string, second string) bool {
	firstHost, firstErr := normalizedHostname(first)
	secondHost, secondErr := normalizedHostname(second)
	return firstErr == nil && secondErr == nil && firstHost == secondHost
}

func requestHostMatchesDestination(host string, original string) (bool, error) {
	host, err := normalizedHostname(host)
	if err != nil {
		return false, err
	}
	destinationHost, _, err := net.SplitHostPort(original)
	if err != nil {
		return false, fmt.Errorf("parse original destination %q: %w", original, err)
	}
	destinationIP := net.ParseIP(destinationHost)
	if destinationIP == nil {
		return false, fmt.Errorf("original destination %q is not an IP address", destinationHost)
	}
	if hostIP := net.ParseIP(host); hostIP != nil {
		return hostIP.Equal(destinationIP), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return false, fmt.Errorf("resolve request host %q: %w", host, err)
	}
	for _, address := range addresses {
		if address.IP.Equal(destinationIP) {
			return true, nil
		}
	}
	return false, nil
}

func serveInspectedTLSConnection(connection net.Conn, reader *bufio.Reader, host string, checker URLChecker, ca *certificateAuthority, dialContext func(context.Context, string, string) (net.Conn, error)) error {
	config, err := ca.tlsConfig(host)
	if err != nil {
		return err
	}
	tlsConnection := tls.Server(&readerConn{Conn: connection, reader: reader}, config)
	if err := tlsConnection.Handshake(); err != nil {
		return err
	}
	defer tlsConnection.Close()

	request, err := http.ReadRequest(bufio.NewReader(tlsConnection))
	if err != nil {
		return err
	}
	requestHost := request.Host
	if requestHost == "" {
		requestHost = host
	}
	if !sameHostname(host, requestHost) {
		writeBlockedHTTPResponse(tlsConnection, checker, "TLS SNI 與 HTTP Host 不一致，依安全規則封鎖。")
		return nil
	}
	target := "https://" + requestHost + request.URL.RequestURI()
	if known, allowed, reason := knownDecision(checker, target); known && !allowed {
		writeBlockedHTTPResponse(tlsConnection, checker, reason)
		return nil
	}

	request.RequestURI = ""
	request.URL.Scheme = "https"
	request.URL.Host = requestHost
	request.Header.Del("Accept-Encoding")
	request.Close = true
	transport := &http.Transport{
		DialContext:       dialContext,
		ForceAttemptHTTP2: false,
		DisableKeepAlives: true,
	}
	response, err := transport.RoundTrip(request)
	if err != nil {
		return fmt.Errorf("HTTPS upstream request failed: %w", err)
	}
	response.Close = true
	if err := inspectAndWriteResponse(tlsConnection, checker, target, response); err != nil {
		return fmt.Errorf("inspect HTTPS response: %w", err)
	}
	return nil
}
