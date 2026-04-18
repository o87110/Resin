package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/config"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/platform"
	M "github.com/sagernet/sing/common/metadata"
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

func socks4ConnectRequest(target string, userID string) []byte {
	host, portText, err := net.SplitHostPort(target)
	if err != nil {
		panic(err)
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		panic(err)
	}

	req := []byte{socks4Version, socks4CmdConnect, byte(port >> 8), byte(port)}
	if ip := net.ParseIP(host); ip != nil {
		req = append(req, ip.To4()...)
		req = append(req, []byte(userID)...)
		req = append(req, 0x00)
		return req
	}

	req = append(req, 0x00, 0x00, 0x00, 0x01)
	req = append(req, []byte(userID)...)
	req = append(req, 0x00)
	req = append(req, []byte(host)...)
	req = append(req, 0x00)
	return req
}

type scriptedAddr string

func (a scriptedAddr) Network() string { return "tcp" }
func (a scriptedAddr) String() string  { return string(a) }

type scriptedConn struct {
	readBuf    *bytes.Buffer
	writes     [][]byte
	writeErrAt int
	writeErr   error
	writeCount int
	closed     bool
}

func newScriptedConn(input []byte) *scriptedConn {
	return &scriptedConn{
		readBuf:  bytes.NewBuffer(input),
		writeErr: io.ErrClosedPipe,
	}
}

func (c *scriptedConn) Read(p []byte) (int, error) {
	if c.closed {
		return 0, io.EOF
	}
	return c.readBuf.Read(p)
}

func (c *scriptedConn) Write(p []byte) (int, error) {
	c.writeCount++
	if c.writeErrAt > 0 && c.writeCount == c.writeErrAt {
		return 0, c.writeErr
	}
	cp := append([]byte(nil), p...)
	c.writes = append(c.writes, cp)
	return len(p), nil
}

func (c *scriptedConn) Close() error {
	c.closed = true
	return nil
}

func (c *scriptedConn) LocalAddr() net.Addr              { return scriptedAddr("127.0.0.1:2260") }
func (c *scriptedConn) RemoteAddr() net.Addr             { return scriptedAddr("127.0.0.1:34567") }
func (c *scriptedConn) SetDeadline(time.Time) error      { return nil }
func (c *scriptedConn) SetReadDeadline(time.Time) error  { return nil }
func (c *scriptedConn) SetWriteDeadline(time.Time) error { return nil }
func (c *scriptedConn) WriteChunk(i int) []byte          { return append([]byte(nil), c.writes[i]...) }
func (c *scriptedConn) WriteChunks() int                 { return len(c.writes) }
func (c *scriptedConn) Closed() bool                     { return c.closed }
func (c *scriptedConn) CombinedWrites() []byte           { return bytes.Join(c.writes, nil) }
func (c *scriptedConn) SetWriteFailure(writeErrAt int)   { c.writeErrAt = writeErrAt }
func (c *scriptedConn) SetWriteFailureErr(err error)     { c.writeErr = err }
func (c *scriptedConn) RemainingReadableBytes() int      { return c.readBuf.Len() }

type closeTrackingConn struct {
	net.Conn
	closed bool
}

func (c *closeTrackingConn) Close() error {
	c.closed = true
	if c.Conn != nil {
		return c.Conn.Close()
	}
	return nil
}

func registerDefaultPlatformForProxyE2E(t testing.TB, env *proxyE2EEnv) {
	t.Helper()

	defaultPlat := platform.NewPlatform(platform.DefaultPlatformID, platform.DefaultPlatformName, nil, nil)
	defaultPlat.StickyTTLNs = int64(time.Hour)
	defaultPlat.ReverseProxyMissAction = "TREAT_AS_EMPTY"
	env.pool.RegisterPlatform(defaultPlat)

	hash := node.HashFromRawOptions(json.RawMessage(`{"type":"stub","server":"127.0.0.1","server_port":1}`))
	env.pool.NotifyNodeDirty(hash)
	if !defaultPlat.View().Contains(hash) {
		t.Fatal("default platform should include test node")
	}
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

func TestSocks5Handler_SOCKS4RejectsWhenDisabled(t *testing.T) {
	handler := NewSocks5Handler(Socks5HandlerConfig{Timeout: 200 * time.Millisecond})
	conn := newScriptedConn(nil)

	handler.ServeConnSOCKS4(conn)

	if !conn.Closed() {
		t.Fatal("expected connection to close after SOCKS4 reject")
	}
	if conn.WriteChunks() != 1 {
		t.Fatalf("write chunks = %d, want 1", conn.WriteChunks())
	}
	reply := conn.WriteChunk(0)
	if len(reply) != 8 || reply[1] != socks4RepReject {
		t.Fatalf("reply: got %v, want SOCKS4 reject", reply)
	}
}

func TestSocks5Handler_SOCKS4RejectsWhenProxyTokenConfigured(t *testing.T) {
	handler := NewSocks5Handler(Socks5HandlerConfig{
		Timeout:             200 * time.Millisecond,
		AllowInsecureSOCKS4: true,
		ProxyToken:          "tok",
	})
	conn := newScriptedConn(nil)

	handler.ServeConnSOCKS4(conn)

	if !conn.Closed() {
		t.Fatal("expected connection to close after protected SOCKS4 reject")
	}
	reply := conn.WriteChunk(0)
	if len(reply) != 8 || reply[1] != socks4RepReject {
		t.Fatalf("reply: got %v, want SOCKS4 reject", reply)
	}
}

func TestSocks5Handler_SOCKS5RouteFailureReturnsFailureReply(t *testing.T) {
	env := newProxyE2EEnv(t)
	hash := node.HashFromRawOptions([]byte(`{"type":"stub","server":"127.0.0.1","server_port":1}`))
	entry, ok := env.pool.GetEntry(hash)
	if !ok {
		t.Fatal("expected test node in pool")
	}
	entry.CircuitOpenSince.Store(time.Now().UnixNano())
	env.pool.NotifyNodeDirty(hash)

	health := &mockHealthRecorder{}
	handler := NewSocks5Handler(Socks5HandlerConfig{
		ProxyToken:  "tok",
		AuthVersion: string(config.AuthVersionV1),
		Router:      env.router,
		Pool:        env.pool,
		Health:      health,
		Events:      newMockEventEmitter(),
		Timeout:     time.Second,
	})

	conn := newScriptedConn(bytes.Join([][]byte{
		{socks5Version, 0x01, socks5AuthUser},
		socks5UserPassPayload("plat.user", "tok"),
		socks5ConnectRequest("example.com:443"),
	}, nil))

	handler.ServeConn(conn)

	if !conn.Closed() {
		t.Fatal("expected connection close after route failure")
	}
	if conn.WriteChunks() != 3 {
		t.Fatalf("write chunks = %d, want 3", conn.WriteChunks())
	}
	if got := conn.WriteChunk(2)[1]; got != socks5RepGeneralFailure {
		t.Fatalf("failure reply code = 0x%02x, want 0x%02x", got, socks5RepGeneralFailure)
	}
	if health.resultCalls.Load() != 0 {
		t.Fatalf("route failure should not mark node health, got %d result calls", health.resultCalls.Load())
	}
}

func TestSocks5Handler_SOCKS5DialFailureReturnsFailureReplyAndMarksNodeFailure(t *testing.T) {
	env := newProxyE2EEnv(t)
	setProxyE2EOutboundDialFunc(t, env, func(ctx context.Context, network string, dest M.Socksaddr) (net.Conn, error) {
		return nil, errors.New("dial failed")
	})

	health := &mockHealthRecorder{}
	handler := NewSocks5Handler(Socks5HandlerConfig{
		ProxyToken:  "tok",
		AuthVersion: string(config.AuthVersionV1),
		Router:      env.router,
		Pool:        env.pool,
		Health:      health,
		Events:      newMockEventEmitter(),
		Timeout:     time.Second,
	})

	conn := newScriptedConn(bytes.Join([][]byte{
		{socks5Version, 0x01, socks5AuthUser},
		socks5UserPassPayload("plat.user", "tok"),
		socks5ConnectRequest("example.com:443"),
	}, nil))

	handler.ServeConn(conn)

	if !conn.Closed() {
		t.Fatal("expected connection close after dial failure")
	}
	if conn.WriteChunks() != 3 {
		t.Fatalf("write chunks = %d, want 3", conn.WriteChunks())
	}
	if got := conn.WriteChunk(2)[1]; got != socks5RepHostUnreachable {
		t.Fatalf("failure reply code = 0x%02x, want 0x%02x", got, socks5RepHostUnreachable)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if health.resultCalls.Load() == 1 && health.lastSuccess.Load() == 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("dial failure should mark node unhealthy once, got calls=%d lastSuccess=%d", health.resultCalls.Load(), health.lastSuccess.Load())
}

func TestSocks5Handler_SOCKS5CanceledDialClosesWithoutReplyOrHealthPenalty(t *testing.T) {
	env := newProxyE2EEnv(t)
	setProxyE2EOutboundDialFunc(t, env, func(ctx context.Context, network string, dest M.Socksaddr) (net.Conn, error) {
		return nil, context.Canceled
	})

	health := &mockHealthRecorder{}
	handler := NewSocks5Handler(Socks5HandlerConfig{
		ProxyToken:  "tok",
		AuthVersion: string(config.AuthVersionV1),
		Router:      env.router,
		Pool:        env.pool,
		Health:      health,
		Events:      newMockEventEmitter(),
		Timeout:     time.Second,
	})

	conn := newScriptedConn(bytes.Join([][]byte{
		{socks5Version, 0x01, socks5AuthUser},
		socks5UserPassPayload("plat.user", "tok"),
		socks5ConnectRequest("example.com:443"),
	}, nil))

	handler.ServeConn(conn)

	if !conn.Closed() {
		t.Fatal("expected connection close after canceled dial")
	}
	if conn.WriteChunks() != 2 {
		t.Fatalf("write chunks = %d, want 2 (method selection + auth only)", conn.WriteChunks())
	}
	if health.resultCalls.Load() != 0 {
		t.Fatalf("canceled dial should not mark node health, got %d result calls", health.resultCalls.Load())
	}
}

func TestSocks5Handler_SOCKS5FailureReplyWriteFailureStillCloses(t *testing.T) {
	env := newProxyE2EEnv(t)
	hash := node.HashFromRawOptions([]byte(`{"type":"stub","server":"127.0.0.1","server_port":1}`))
	entry, ok := env.pool.GetEntry(hash)
	if !ok {
		t.Fatal("expected test node in pool")
	}
	entry.CircuitOpenSince.Store(time.Now().UnixNano())
	env.pool.NotifyNodeDirty(hash)

	handler := NewSocks5Handler(Socks5HandlerConfig{
		ProxyToken:  "tok",
		AuthVersion: string(config.AuthVersionV1),
		Router:      env.router,
		Pool:        env.pool,
		Health:      &mockHealthRecorder{},
		Events:      newMockEventEmitter(),
		Timeout:     time.Second,
	})

	conn := newScriptedConn(bytes.Join([][]byte{
		{socks5Version, 0x01, socks5AuthUser},
		socks5UserPassPayload("plat.user", "tok"),
		socks5ConnectRequest("example.com:443"),
	}, nil))
	conn.SetWriteFailure(3)

	handler.ServeConn(conn)

	if !conn.Closed() {
		t.Fatal("expected connection close when failure reply write fails")
	}
}

func TestSocks5Handler_SOCKS5SuccessReplyWriteFailureDoesNotMarkNodeFailure(t *testing.T) {
	env := newProxyE2EEnv(t)
	health := &mockHealthRecorder{}

	targetLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen target: %v", err)
	}
	defer targetLn.Close()

	accepted := make(chan struct{})
	go func() {
		defer close(accepted)
		conn, acceptErr := targetLn.Accept()
		if acceptErr == nil {
			_ = conn.Close()
		}
	}()

	handler := NewSocks5Handler(Socks5HandlerConfig{
		ProxyToken:  "tok",
		AuthVersion: string(config.AuthVersionV1),
		Router:      env.router,
		Pool:        env.pool,
		Health:      health,
		Events:      newMockEventEmitter(),
		Timeout:     time.Second,
	})

	conn := newScriptedConn(bytes.Join([][]byte{
		{socks5Version, 0x01, socks5AuthUser},
		socks5UserPassPayload("plat.user", "tok"),
		socks5ConnectRequest(targetLn.Addr().String()),
	}, nil))
	conn.SetWriteFailure(3)

	handler.ServeConn(conn)

	if !conn.Closed() {
		t.Fatal("expected connection close when success reply write fails")
	}
	if health.resultCalls.Load() != 0 {
		t.Fatalf("client reply write failure should not mark node health, got %d result calls", health.resultCalls.Load())
	}

	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("expected upstream accept to unblock after client reply write failure")
	}
}

func TestSocks5Handler_SOCKS4SuccessReplyWriteFailureDoesNotMarkNodeFailure(t *testing.T) {
	env := newProxyE2EEnv(t)
	registerDefaultPlatformForProxyE2E(t, env)
	health := &mockHealthRecorder{}
	upstream := &closeTrackingConn{}
	setProxyE2EOutboundDialFunc(t, env, func(ctx context.Context, network string, dest M.Socksaddr) (net.Conn, error) {
		return upstream, nil
	})

	handler := NewSocks5Handler(Socks5HandlerConfig{
		Router:              env.router,
		Pool:                env.pool,
		Health:              health,
		Events:              newMockEventEmitter(),
		Timeout:             time.Second,
		AllowInsecureSOCKS4: true,
	})

	conn := newScriptedConn(socks4ConnectRequest("127.0.0.1:80", "acct"))
	conn.SetWriteFailure(1)

	handler.ServeConnSOCKS4(conn)

	if !conn.Closed() {
		t.Fatal("expected connection close when SOCKS4 granted write fails")
	}
	if health.resultCalls.Load() != 0 {
		t.Fatalf("client reply write failure should not mark node health, got %d result calls", health.resultCalls.Load())
	}
}

func TestSocks5Handler_SOCKS4CanceledDialClosesWithoutReplyOrHealthPenalty(t *testing.T) {
	env := newProxyE2EEnv(t)
	registerDefaultPlatformForProxyE2E(t, env)
	setProxyE2EOutboundDialFunc(t, env, func(ctx context.Context, network string, dest M.Socksaddr) (net.Conn, error) {
		return nil, context.Canceled
	})

	health := &mockHealthRecorder{}
	handler := NewSocks5Handler(Socks5HandlerConfig{
		Router:              env.router,
		Pool:                env.pool,
		Health:              health,
		Events:              newMockEventEmitter(),
		Timeout:             time.Second,
		AllowInsecureSOCKS4: true,
	})

	conn := newScriptedConn(socks4ConnectRequest("127.0.0.1:80", "acct"))

	handler.ServeConnSOCKS4(conn)

	if !conn.Closed() {
		t.Fatal("expected connection close after canceled SOCKS4 dial")
	}
	if conn.WriteChunks() != 0 {
		t.Fatalf("write chunks = %d, want 0, writes=%v", conn.WriteChunks(), conn.CombinedWrites())
	}
	if health.resultCalls.Load() != 0 {
		t.Fatalf("canceled SOCKS4 dial should not mark node health, got %d result calls", health.resultCalls.Load())
	}
}

func TestSocks5Handler_SOCKS4OversizedUserIDRejects(t *testing.T) {
	handler := NewSocks5Handler(Socks5HandlerConfig{
		Timeout:             time.Second,
		AllowInsecureSOCKS4: true,
	})
	conn := newScriptedConn(socks4ConnectRequest("127.0.0.1:80", strings.Repeat("u", socks4MaxFieldLen+1)))

	handler.ServeConnSOCKS4(conn)

	if !conn.Closed() {
		t.Fatal("expected connection close after oversized SOCKS4 user ID")
	}
	reply := conn.WriteChunk(0)
	if len(reply) != 8 || reply[1] != socks4RepReject {
		t.Fatalf("reply: got %v, want SOCKS4 reject", reply)
	}
}

func TestSocks5Handler_SOCKS4OversizedDomainRejects(t *testing.T) {
	handler := NewSocks5Handler(Socks5HandlerConfig{
		Timeout:             time.Second,
		AllowInsecureSOCKS4: true,
	})
	target := net.JoinHostPort(strings.Repeat("d", socks4MaxFieldLen+1), "80")
	conn := newScriptedConn(socks4ConnectRequest(target, "acct"))

	handler.ServeConnSOCKS4(conn)

	if !conn.Closed() {
		t.Fatal("expected connection close after oversized SOCKS4 domain")
	}
	reply := conn.WriteChunk(0)
	if len(reply) != 8 || reply[1] != socks4RepReject {
		t.Fatalf("reply: got %v, want SOCKS4 reject", reply)
	}
}
