//go:build linux

package proxy

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const originalDestinationSocketOption = 80

const (
	getSocketOptionSystemCall = 55
	socketLevelIP             = 0
	socketLevelIPv6           = 41
)

func StartTransparentProxy(address string, checker URLChecker) (*TransparentProxy, error) {
	if os.Geteuid() != 0 {
		return nil, errors.New("transparent proxy on Ubuntu must be started with sudo")
	}
	port, err := portFromAddress(address)
	if err != nil {
		return nil, err
	}
	chain := fmt.Sprintf("NICETOS_%d", os.Getpid())
	if err := configureFirewall(chain, port); err != nil {
		return nil, err
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		_ = removeFirewall(chain)
		return nil, err
	}
	proxy := &TransparentProxy{listener: listener, chain: chain, done: make(chan struct{})}
	go proxy.acceptLoop(checker)
	return proxy, nil
}

func (proxy *TransparentProxy) Serve() error {
	<-proxy.done
	return net.ErrClosed
}

func (proxy *TransparentProxy) Close() error {
	listenerErr := proxy.listener.Close()
	firewallErr := removeFirewall(proxy.chain)
	close(proxy.done)
	if listenerErr != nil {
		return listenerErr
	}
	return firewallErr
}

func (proxy *TransparentProxy) acceptLoop(checker URLChecker) {
	for {
		connection, err := proxy.listener.Accept()
		if err != nil {
			return
		}
		go handleTransparentConnection(connection, checker)
	}
}

func handleTransparentConnection(connection net.Conn, checker URLChecker) {
	defer connection.Close()
	original, err := originalDestination(connection)
	if err != nil {
		log.Printf("cannot determine original destination: %v", err)
		return
	}
	reader := bufio.NewReader(connection)
	firstByte, err := reader.Peek(1)
	if err != nil {
		return
	}
	if firstByte[0] == 0x16 {
		handleTLSConnection(connection, reader, original, checker)
		return
	}
	handleHTTPConnection(connection, reader, original, checker)
}

func handleHTTPConnection(connection net.Conn, reader *bufio.Reader, original string, checker URLChecker) {
	request, err := http.ReadRequest(reader)
	if err != nil {
		return
	}
	host := request.Host
	if host == "" {
		host = original
	}
	target := "http://" + host + request.URL.RequestURI()
	allowed, err := checker.Check(target)
	if err != nil || !allowed {
		writeProxyError(connection, http.StatusForbidden, "access denied by NicetOS")
		return
	}

	request.RequestURI = ""
	request.URL.Scheme = "http"
	request.URL.Host = host
	request.Close = true
	transport := &http.Transport{DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
		return net.DialTimeout("tcp", original, 10*time.Second)
	}}
	response, err := transport.RoundTrip(request)
	if err != nil {
		writeProxyError(connection, http.StatusBadGateway, "upstream request failed")
		return
	}
	defer response.Body.Close()
	_ = response.Write(connection)
}

func handleTLSConnection(connection net.Conn, reader *bufio.Reader, original string, checker URLChecker) {
	clientHello, err := readClientHello(reader)
	if err != nil {
		return
	}
	host := tlsServerName(clientHello)
	if host == "" {
		log.Printf("blocked HTTPS connection without server name to %s", original)
		return
	}
	allowed, err := checker.Check("https://" + host)
	if err != nil || !allowed {
		return
	}
	upstream, err := net.DialTimeout("tcp", original, 10*time.Second)
	if err != nil {
		return
	}
	defer upstream.Close()
	go io.Copy(upstream, reader)
	_, _ = io.Copy(connection, upstream)
}

func readClientHello(reader *bufio.Reader) ([]byte, error) {
	header, err := reader.Peek(5)
	if err != nil || header[0] != 0x16 {
		return nil, errors.New("not a TLS handshake")
	}
	recordLength := int(binary.BigEndian.Uint16(header[3:5]))
	if recordLength > 65535 {
		return nil, errors.New("TLS record is too large")
	}
	return reader.Peek(5 + recordLength)
}

func tlsServerName(record []byte) string {
	if len(record) < 9 {
		return ""
	}
	position := 5 + 4 + 2 + 32
	if position >= len(record) {
		return ""
	}
	sessionLength := int(record[position])
	position++
	position += sessionLength
	if position+2 > len(record) {
		return ""
	}
	cipherLength := int(binary.BigEndian.Uint16(record[position : position+2]))
	position += 2 + cipherLength
	if position >= len(record) {
		return ""
	}
	compressionLength := int(record[position])
	position++
	position += compressionLength
	if position+2 > len(record) {
		return ""
	}
	extensionsLength := int(binary.BigEndian.Uint16(record[position : position+2]))
	position += 2
	end := position + extensionsLength
	if end > len(record) {
		end = len(record)
	}
	for position+4 <= end {
		extensionType := binary.BigEndian.Uint16(record[position : position+2])
		extensionLength := int(binary.BigEndian.Uint16(record[position+2 : position+4]))
		position += 4
		if position+extensionLength > end {
			return ""
		}
		if extensionType == 0 && extensionLength >= 5 {
			nameListLength := int(binary.BigEndian.Uint16(record[position : position+2]))
			if nameListLength+2 > extensionLength || record[position+2] != 0 {
				return ""
			}
			nameLength := int(binary.BigEndian.Uint16(record[position+3 : position+5]))
			if nameLength+5 > extensionLength {
				return ""
			}
			return string(record[position+5 : position+5+nameLength])
		}
		position += extensionLength
	}
	return ""
}

func writeProxyError(connection net.Conn, status int, message string) {
	_, _ = fmt.Fprintf(connection, "HTTP/1.1 %d %s\r\nConnection: close\r\nContent-Length: %d\r\n\r\n%s", status, http.StatusText(status), len(message), message)
}

func originalDestination(connection net.Conn) (string, error) {
	syscallConnection, ok := connection.(syscall.Conn)
	if !ok {
		return "", errors.New("connection does not expose a file descriptor")
	}
	var destination string
	var socketErr error
	raw, err := syscallConnection.SyscallConn()
	if err != nil {
		return "", err
	}
	err = raw.Control(func(fd uintptr) {
		destination, socketErr = getOriginalDestination(fd)
	})
	if err != nil {
		return "", err
	}
	return destination, socketErr
}

func getOriginalDestination(fd uintptr) (string, error) {
	var address [28]byte
	length := uint32(len(address))
	_, _, errno := syscall.Syscall6(getSocketOptionSystemCall, fd, socketLevelIP, originalDestinationSocketOption, uintptr(unsafe.Pointer(&address[0])), uintptr(unsafe.Pointer(&length)), 0)
	if errno == 0 {
		port := binary.BigEndian.Uint16(address[2:4])
		return net.JoinHostPort(net.IP(address[4:8]).String(), strconv.Itoa(int(port))), nil
	}

	var address6 [28]byte
	length = uint32(len(address6))
	_, _, ipv6Errno := syscall.Syscall6(getSocketOptionSystemCall, fd, socketLevelIPv6, originalDestinationSocketOption, uintptr(unsafe.Pointer(&address6[0])), uintptr(unsafe.Pointer(&length)), 0)
	if ipv6Errno == 0 {
		port := binary.BigEndian.Uint16(address6[2:4])
		return net.JoinHostPort(net.IP(address6[8:24]).String(), strconv.Itoa(int(port))), nil
	}
	return "", errno
}

func portFromAddress(address string) (string, error) {
	_, port, err := net.SplitHostPort(address)
	if err != nil {
		return "", err
	}
	return port, nil
}

func firewallCommand(binary string, arguments ...string) error {
	command := exec.Command(binary, arguments...)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("%s %s: %w (%s)", binary, strings.Join(arguments, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

func configureFirewall(chain, port string) error {
	uid := strconv.Itoa(os.Geteuid())
	for _, binary := range []string{"iptables", "ip6tables"} {
		if err := firewallCommand(binary, "-t", "nat", "-N", chain); err != nil {
			return fmt.Errorf("cannot create firewall chain: %w", err)
		}
		if err := firewallCommand(binary, "-t", "nat", "-A", chain, "-m", "owner", "--uid-owner", uid, "-j", "RETURN"); err != nil {
			_ = removeFirewall(chain)
			return err
		}
		for _, targetPort := range []string{"80", "443"} {
			if err := firewallCommand(binary, "-t", "nat", "-A", chain, "-p", "tcp", "--dport", targetPort, "-j", "REDIRECT", "--to-ports", port); err != nil {
				_ = removeFirewall(chain)
				return err
			}
		}
		if err := firewallCommand(binary, "-A", "OUTPUT", "-p", "udp", "--dport", "443", "-m", "owner", "--uid-owner", uid, "-j", "ACCEPT"); err != nil {
			_ = removeFirewall(chain)
			return err
		}
		if err := firewallCommand(binary, "-A", "OUTPUT", "-p", "udp", "--dport", "443", "-j", "REJECT"); err != nil {
			_ = removeFirewall(chain)
			return err
		}
		if err := firewallCommand(binary, "-t", "nat", "-A", "OUTPUT", "-p", "tcp", "-j", chain); err != nil {
			_ = removeFirewall(chain)
			return err
		}
	}
	return nil
}

func removeFirewall(chain string) error {
	var firstErr error
	for _, binary := range []string{"iptables", "ip6tables"} {
		_ = firewallCommand(binary, "-D", "OUTPUT", "-p", "udp", "--dport", "443", "-j", "REJECT")
		_ = firewallCommand(binary, "-D", "OUTPUT", "-p", "udp", "--dport", "443", "-m", "owner", "--uid-owner", strconv.Itoa(os.Geteuid()), "-j", "ACCEPT")
		_ = firewallCommand(binary, "-t", "nat", "-D", "OUTPUT", "-p", "tcp", "-j", chain)
		_ = firewallCommand(binary, "-t", "nat", "-F", chain)
		if err := firewallCommand(binary, "-t", "nat", "-X", chain); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}
