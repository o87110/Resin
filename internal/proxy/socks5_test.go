package proxy

import (
	"bytes"
	"io"
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/config"
)

func socks5UserPassPayload(user, pass string) []byte {
	payload := make([]byte, 0, 3+len(user)+len(pass))
	payload = append(payload, 0x01, byte(len(user)))
	payload = append(payload, []byte(user)...)
	payload = append(payload, byte(len(pass)))
	payload = append(payload, []byte(pass)...)
	return payload
}

func socks5ConnectRequest(target string) []byte {
	host, portText, err := net.SplitHostPort(target)
	if err != nil {
		panic(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		panic(err)
	}

	req := []byte{socks5Version, socks5CmdConnect, 0x00}
	if ip := net.ParseIP(host); ip != nil {
		if ip4 := ip.To4(); ip4 != nil {
			req = append(req, socks5AddrIPv4)
			req = append(req, ip4...)
		} else {
			req = append(req, socks5AddrIPv6)
			req = append(req, ip.To16()...)
		}
	} else {
		req = append(req, socks5AddrDomain, byte(len(host)))
		req = append(req, []byte(host)...)
	}

	req = append(req, byte(port>>8), byte(port))
	return req
}

func TestSocks5Handler_ReadMethodSelection_PrefersUserPassWhenTokenEmpty(t *testing.T) {
	handler := &Socks5Handler{authVer: config.AuthVersionV1}

	method, err := handler.readMethodSelection(bytes.NewReader([]byte{
		socks5Version, 0x02, socks5AuthNone, socks5AuthUser,
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if method != socks5AuthUser {
		t.Fatalf("method: got 0x%02x, want 0x%02x", method, socks5AuthUser)
	}
}

func TestSocks5Handler_ReadMethodSelection_RejectsMissingUserPassWhenTokenConfigured(t *testing.T) {
	handler := &Socks5Handler{token: "tok", authVer: config.AuthVersionV1}

	method, err := handler.readMethodSelection(bytes.NewReader([]byte{
		socks5Version, 0x01, socks5AuthNone,
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if method != 0xFF {
		t.Fatalf("method: got 0x%02x, want 0xFF", method)
	}
}

func TestSocks5Handler_Authenticate_V1UsesHTTPStyleCredentials(t *testing.T) {
	handler := &Socks5Handler{token: "tok", authVer: config.AuthVersionV1}
	rw := bytes.NewBuffer(socks5UserPassPayload("plat.user", "tok"))

	plat, acct, err := handler.authenticate(rw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plat != "plat" || acct != "user" {
		t.Fatalf("got plat=%q acct=%q, want plat=%q acct=%q", plat, acct, "plat", "user")
	}
	if got := rw.Bytes(); !bytes.Equal(got, []byte{0x01, 0x00}) {
		t.Fatalf("auth reply: got %v, want %v", got, []byte{0x01, 0x00})
	}
}

func TestSocks5Handler_Authenticate_LegacyUsesHTTPStyleCredentials(t *testing.T) {
	handler := &Socks5Handler{token: "tok", authVer: config.AuthVersionLegacyV0}
	rw := bytes.NewBuffer(socks5UserPassPayload("tok", "plat:acct"))

	plat, acct, err := handler.authenticate(rw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plat != "plat" || acct != "acct" {
		t.Fatalf("got plat=%q acct=%q, want plat=%q acct=%q", plat, acct, "plat", "acct")
	}
	if got := rw.Bytes(); !bytes.Equal(got, []byte{0x01, 0x00}) {
		t.Fatalf("auth reply: got %v, want %v", got, []byte{0x01, 0x00})
	}
}

func TestSocks5Handler_Authenticate_V1WithoutTokenParsesOptionalIdentity(t *testing.T) {
	handler := &Socks5Handler{authVer: config.AuthVersionV1}
	rw := bytes.NewBuffer(socks5UserPassPayload("my-platform.account-a", ""))

	plat, acct, err := handler.authenticate(rw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if plat != "my-platform" || acct != "account-a" {
		t.Fatalf(
			"got plat=%q acct=%q, want plat=%q acct=%q",
			plat,
			acct,
			"my-platform",
			"account-a",
		)
	}
	if got := rw.Bytes(); !bytes.Equal(got, []byte{0x01, 0x00}) {
		t.Fatalf("auth reply: got %v, want %v", got, []byte{0x01, 0x00})
	}
}

func TestSocks5Handler_Authenticate_V1RejectsLegacyCredentialShape(t *testing.T) {
	handler := &Socks5Handler{token: "tok", authVer: config.AuthVersionV1}
	rw := bytes.NewBuffer(socks5UserPassPayload("tok", "plat:acct"))

	_, _, err := handler.authenticate(rw)
	if err == nil {
		t.Fatal("expected authentication failure")
	}
	if got := rw.Bytes(); !bytes.Equal(got, []byte{0x01, 0x01}) {
		t.Fatalf("auth reply: got %v, want %v", got, []byte{0x01, 0x01})
	}
}

func TestSocks5Handler_E2EConnectSuccess(t *testing.T) {
	env := newProxyE2EEnv(t)
	emitter := newMockEventEmitter()
	health := &mockHealthRecorder{}

	targetLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen target: %v", err)
	}
	defer targetLn.Close()

	targetDone := make(chan struct{})
	go func() {
		defer close(targetDone)
		conn, acceptErr := targetLn.Accept()
		if acceptErr != nil {
			return
		}
		defer conn.Close()
		_, _ = io.Copy(conn, conn)
	}()

	handler := NewSocks5Handler(Socks5HandlerConfig{
		ProxyToken:  "tok",
		AuthVersion: string(config.AuthVersionV1),
		Router:      env.router,
		Pool:        env.pool,
		Health:      health,
		Events:      emitter,
		Timeout:     time.Second,
	})

	serverConn, clientConn := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeConn(serverConn)
	}()

	if _, err := clientConn.Write([]byte{socks5Version, 0x01, socks5AuthUser}); err != nil {
		t.Fatalf("write method selection: %v", err)
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(clientConn, reply); err != nil {
		t.Fatalf("read method selection reply: %v", err)
	}
	if !bytes.Equal(reply, []byte{socks5Version, socks5AuthUser}) {
		t.Fatalf("method selection reply: got %v, want %v", reply, []byte{socks5Version, socks5AuthUser})
	}

	if _, err := clientConn.Write(socks5UserPassPayload("plat.user", "tok")); err != nil {
		t.Fatalf("write auth payload: %v", err)
	}
	if _, err := io.ReadFull(clientConn, reply); err != nil {
		t.Fatalf("read auth reply: %v", err)
	}
	if !bytes.Equal(reply, []byte{0x01, 0x00}) {
		t.Fatalf("auth reply: got %v, want %v", reply, []byte{0x01, 0x00})
	}

	if _, err := clientConn.Write(socks5ConnectRequest(targetLn.Addr().String())); err != nil {
		t.Fatalf("write connect request: %v", err)
	}
	connectReply := make([]byte, 10)
	if _, err := io.ReadFull(clientConn, connectReply); err != nil {
		t.Fatalf("read connect reply: %v", err)
	}
	if connectReply[0] != socks5Version || connectReply[1] != socks5RepSuccess {
		t.Fatalf("connect reply: got %v, want success", connectReply)
	}

	const payload = "ping-through-socks"
	if _, err := clientConn.Write([]byte(payload)); err != nil {
		t.Fatalf("write tunneled payload: %v", err)
	}
	echo := make([]byte, len(payload))
	if _, err := io.ReadFull(clientConn, echo); err != nil {
		t.Fatalf("read tunneled echo: %v", err)
	}
	if string(echo) != payload {
		t.Fatalf("echo: got %q, want %q", string(echo), payload)
	}

	_ = clientConn.Close()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("ServeConn did not exit")
	}

	select {
	case <-targetDone:
	case <-time.After(time.Second):
		t.Fatal("target server did not exit")
	}

	select {
	case logEv := <-emitter.logCh:
		if logEv.HTTPMethod != "SOCKS5" {
			t.Fatalf("HTTPMethod: got %q, want %q", logEv.HTTPMethod, "SOCKS5")
		}
		if logEv.Account != "user" {
			t.Fatalf("Account: got %q, want %q", logEv.Account, "user")
		}
		if !logEv.NetOK {
			t.Fatal("NetOK: got false, want true")
		}
		if logEv.EgressBytes != int64(len(payload)) {
			t.Fatalf("EgressBytes: got %d, want %d", logEv.EgressBytes, len(payload))
		}
		if logEv.IngressBytes != int64(len(payload)) {
			t.Fatalf("IngressBytes: got %d, want %d", logEv.IngressBytes, len(payload))
		}
	case <-time.After(time.Second):
		t.Fatal("expected SOCKS5 log event")
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if health.resultCalls.Load() > 0 {
			if health.lastSuccess.Load() != 1 {
				t.Fatalf("RecordResult lastSuccess: got %d, want 1", health.lastSuccess.Load())
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("expected RecordResult call for SOCKS5 success")
}
