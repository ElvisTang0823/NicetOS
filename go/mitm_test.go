package proxy

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

type pageContentTestChecker struct {
	root        string
	target      string
	contentType string
	content     []byte
}

func (checker *pageContentTestChecker) Check(string) (bool, error) {
	return true, nil
}

func (checker *pageContentTestChecker) CheckWithContent(target string, contentType string, content []byte) (bool, string, error) {
	checker.target = target
	checker.contentType = contentType
	checker.content = append([]byte(nil), content...)
	return true, "", nil
}

func (checker *pageContentTestChecker) BlockedPagePath() string {
	return checker.root
}

func TestInspectResponseUsesProxyBodyAndForwardsIt(t *testing.T) {
	const target = "https://example.com/page"
	const body = "<html><title>Proxy content</title><p>Page text</p></html>"
	checker := &pageContentTestChecker{}
	response := &http.Response{
		Status:        "200 OK",
		StatusCode:    http.StatusOK,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
	}
	var output bytes.Buffer

	if err := inspectAndWriteResponse(&output, checker, target, response); err != nil {
		t.Fatalf("inspect and write response: %v", err)
	}
	if checker.target != target {
		t.Errorf("expected checker target %q, got %q", target, checker.target)
	}
	if checker.contentType != "text/html; charset=utf-8" {
		t.Errorf("expected HTML content type, got %q", checker.contentType)
	}
	if string(checker.content) != body {
		t.Errorf("expected checker to receive upstream body %q, got %q", body, checker.content)
	}
	if !strings.Contains(output.String(), body) {
		t.Errorf("expected forwarded response to preserve body %q, got %q", body, output.String())
	}
}

func TestSameHostnameRejectsMismatchedHostHeader(t *testing.T) {
	if !sameHostname("Example.com", "example.com:8443") {
		t.Fatal("expected case-insensitive SNI and Host names to match")
	}
	if sameHostname("allowed.example", "other.example") {
		t.Fatal("expected different SNI and Host names to be rejected")
	}
}

func TestRequestHostMustResolveToOriginalDestination(t *testing.T) {
	matches, err := requestHostMatchesDestination("192.0.2.10:8080", "192.0.2.10:80")
	if err != nil {
		t.Fatalf("compare matching IP destination: %v", err)
	}
	if !matches {
		t.Fatal("expected matching request host and original destination IP")
	}

	matches, err = requestHostMatchesDestination("192.0.2.11", "192.0.2.10:80")
	if err != nil {
		t.Fatalf("compare mismatched IP destination: %v", err)
	}
	if matches {
		t.Fatal("expected mismatched request host and destination IP to be rejected")
	}
}
