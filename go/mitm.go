package proxy

import (
	"bufio"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// certificateAuthority signs certificates for the local HTTPS filtering page.
type certificateAuthority struct {
	certificate tls.Certificate
	parsed      *x509.Certificate
	privateKey  *rsa.PrivateKey
	certPath    string
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
	certPath, keyPath := filepath.Join(directory, "nicetos-proxy-ca-cert.pem"), filepath.Join(directory, "nicetos-proxy-ca-key.pem")
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
			return nil, err
		}
	} else if certErr != nil || keyErr != nil {
		return nil, fmt.Errorf("both NicetOS CA certificate and key are required: certificate=%w, key=%v", certErr, keyErr)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	parsed, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	key, ok := pair.PrivateKey.(*rsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("NicetOS CA key is not RSA")
	}
	return &certificateAuthority{certificate: pair, parsed: parsed, privateKey: key, certPath: certPath, issued: make(map[string]tls.Certificate)}, nil
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

func readBlockedPageBody(checker URLChecker) ([]byte, error) {
	return os.ReadFile(checker.BlockedPagePath())
}
