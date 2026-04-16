package main

import (
	"bufio"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/proxy"
)

type singleConnListener struct {
	conn net.Conn
}

func (l *singleConnListener) Accept() (net.Conn, error) {
	if l.conn == nil {
		return nil, net.ErrClosed
	}
	conn := l.conn
	l.conn = nil
	return conn, nil
}

func (l *singleConnListener) Close() error {
	if l.conn != nil {
		_ = l.conn.Close()
		l.conn = nil
	}
	return nil
}

func (l *singleConnListener) Addr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4zero, Port: 0}
}

func TestSniffAndServe_RoutesHTTPToHTTPServer(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()

	app := &resinApp{
		inboundSrv: &http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Route", "http")
				w.WriteHeader(http.StatusNoContent)
			}),
		},
		socks5Srv: proxy.NewSocks5Handler(proxy.Socks5HandlerConfig{Timeout: 200 * time.Millisecond}),
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- app.sniffAndServe(&singleConnListener{conn: serverConn})
	}()

	if _, err := io.WriteString(clientConn, "GET / HTTP/1.1\r\nHost: example.com\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatalf("write HTTP request: %v", err)
	}

	reader := bufio.NewReader(clientConn)
	statusLine, err := reader.ReadString('\n')
	if err != nil {
		t.Fatalf("read status line: %v", err)
	}
	if !strings.Contains(statusLine, "204 No Content") {
		t.Fatalf("unexpected status line: %q", statusLine)
	}

	foundRoute := false
	for {
		line, readErr := reader.ReadString('\n')
		if readErr != nil {
			t.Fatalf("read header line: %v", readErr)
		}
		if strings.EqualFold(line, "X-Route: http\r\n") {
			foundRoute = true
		}
		if line == "\r\n" {
			break
		}
	}
	if !foundRoute {
		t.Fatal("expected HTTP handler response header")
	}

	if err := clientConn.Close(); err != nil {
		t.Fatalf("close client conn: %v", err)
	}

	select {
	case err := <-errCh:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("sniffAndServe error: got %v, want %v", err, net.ErrClosed)
		}
	case <-time.After(time.Second):
		t.Fatal("sniffAndServe did not exit")
	}
}

func TestSniffAndServe_RoutesSOCKS5ToHandler(t *testing.T) {
	serverConn, clientConn := net.Pipe()
	defer clientConn.Close()

	httpCalled := make(chan struct{}, 1)
	app := &resinApp{
		inboundSrv: &http.Server{
			Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				httpCalled <- struct{}{}
			}),
		},
		socks5Srv: proxy.NewSocks5Handler(proxy.Socks5HandlerConfig{Timeout: 200 * time.Millisecond}),
	}

	errCh := make(chan error, 1)
	go func() {
		errCh <- app.sniffAndServe(&singleConnListener{conn: serverConn})
	}()

	if _, err := clientConn.Write([]byte{0x05, 0x01, 0x00}); err != nil {
		t.Fatalf("write SOCKS5 method selection: %v", err)
	}

	reply := make([]byte, 2)
	if _, err := io.ReadFull(clientConn, reply); err != nil {
		t.Fatalf("read SOCKS5 method selection reply: %v", err)
	}
	if reply[0] != 0x05 || reply[1] != 0x00 {
		t.Fatalf("reply: got %v, want %v", reply, []byte{0x05, 0x00})
	}

	if err := clientConn.Close(); err != nil {
		t.Fatalf("close client conn: %v", err)
	}

	select {
	case err := <-errCh:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("sniffAndServe error: got %v, want %v", err, net.ErrClosed)
		}
	case <-time.After(time.Second):
		t.Fatal("sniffAndServe did not exit")
	}

	select {
	case <-httpCalled:
		t.Fatal("HTTP handler should not receive SOCKS5 traffic")
	default:
	}
}
