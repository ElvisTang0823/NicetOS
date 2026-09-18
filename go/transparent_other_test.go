package proxy

import (
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCertificateAuthorityIssuesVerifiableHostCertificate(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "assets"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "assets", "index.html"), []byte("blocked"), 0644); err != nil {
		t.Fatal(err)
	}
	authority, err := newCertificateAuthority(&testChecker{root: root})
	if err != nil {
		t.Fatalf("create authority: %v", err)
	}
	certificate, err := authority.certificateFor("example.com:443")
	if err != nil {
		t.Fatalf("issue certificate: %v", err)
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		t.Fatalf("parse issued certificate: %v", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(authority.parsed)
	if _, err := leaf.Verify(x509.VerifyOptions{DNSName: "example.com", Roots: roots}); err != nil {
		t.Fatalf("issued certificate did not verify for requested host: %v", err)
	}
}

type testChecker struct {
	root string
}

func (c *testChecker) Check(target string) (bool, error) {
	return false, nil
}

func (c *testChecker) BlockedPagePath() string {
	return filepath.Join(c.root, "assets", "index.html")
}

func TestBasicConnectBlockedResponseRendersHTML(t *testing.T) {
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolve workspace root: %v", err)
	}
	checker := &testChecker{root: root}
	req := httptest.NewRequest(http.MethodConnect, "https://example.com", nil)
	req.Host = "example.com"
	res := httptest.NewRecorder()

	handleBasicConnect(res, req, checker)

	if status := res.Code; status != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", status)
	}
	if contentType := res.Header().Get("Content-Type"); !strings.Contains(strings.ToLower(contentType), "text/html") {
		t.Fatalf("expected HTML content type, got %q", contentType)
	}
	body := res.Body.String()
	if !strings.Contains(body, "危險網站") || !strings.Contains(body, "NicetOS") {
		t.Fatalf("expected blocked page HTML, got %q", body)
	}
}
