package proxy

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"

	"github.com/Resinat/Resin/internal/config"
	"github.com/Resinat/Resin/internal/netutil"
	"github.com/Resinat/Resin/internal/outbound"
	"github.com/Resinat/Resin/internal/routing"
	M "github.com/sagernet/sing/common/metadata"
)

// SOCKS5 protocol constants (RFC 1928).
const (
	socks5Version  = 0x05
	socks5AuthNone = 0x00
	socks5AuthUser = 0x02

	socks5CmdConnect = 0x01

	socks5AddrIPv4   = 0x01
	socks5AddrDomain = 0x03
	socks5AddrIPv6   = 0x04

	socks5RepSuccess             = 0x00
	socks5RepGeneralFailure      = 0x01
	socks5RepNetworkUnreachable  = 0x03
	socks5RepHostUnreachable     = 0x04
	socks5RepCommandNotSupported = 0x07
	socks5RepAddressNotSupported = 0x08
)

// SOCKS4a protocol constants.
const (
	socks4Version  = 0x04
	socks4CmdConnect = 0x01
	socks4RepGranted = 0x5A
	socks4RepReject  = 0x5B
)

// Socks5Handler handles SOCKS5 inbound connections.
type Socks5Handler struct {
	token       string
	authVer     config.AuthVersion
	router      *routing.Router
	pool        outbound.PoolAccessor
	health      HealthRecorder
	events      EventEmitter
	metricsSink MetricsEventSink
	timeout     time.Duration
}

// Socks5HandlerConfig holds dependencies for the SOCKS5 handler.
type Socks5HandlerConfig struct {
	ProxyToken  string
	AuthVersion string
	Router      *routing.Router
	Pool        outbound.PoolAccessor
	Health      HealthRecorder
	Events      EventEmitter
	MetricsSink MetricsEventSink
	Timeout     time.Duration
}

// NewSocks5Handler creates a new SOCKS5 inbound handler.
func NewSocks5Handler(cfg Socks5HandlerConfig) *Socks5Handler {
	ev := cfg.Events
	if ev == nil {
		ev = NoOpEventEmitter{}
	}
	authVer := config.NormalizeAuthVersion(cfg.AuthVersion)
	if authVer == "" {
		authVer = config.AuthVersionLegacyV0
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Second
	}
	return &Socks5Handler{
		token:       cfg.ProxyToken,
		authVer:     authVer,
		router:      cfg.Router,
		pool:        cfg.Pool,
		health:      cfg.Health,
		events:      ev,
		metricsSink: cfg.MetricsSink,
		timeout:     timeout,
	}
}

// ServeConn handles a single SOCKS5 connection (blocking).
func (h *Socks5Handler) ServeConn(conn net.Conn) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clientIP, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
	if clientIP == "" {
		clientIP = conn.RemoteAddr().String()
	}
	deadline := time.Now().Add(h.timeout)
	if err := conn.SetReadDeadline(deadline); err != nil {
		conn.Close()
		return
	}

	// Phase 1: Method selection.
	method, err := h.readMethodSelection(conn)
	if err != nil {
		conn.Close()
		return
	}

	if method == 0xFF {
		h.writeMethodSelection(conn, 0xFF)
		conn.Close()
		return
	}

	if h.token != "" && method != socks5AuthUser {
		h.writeMethodSelection(conn, 0xFF)
		conn.Close()
		return
	}

	if h.token != "" {
		h.writeMethodSelection(conn, socks5AuthUser)
	} else {
		h.writeMethodSelection(conn, socks5AuthNone)
	}

	// Phase 2: Authentication.
	var platName, account string
	if h.token != "" {
		platName, account, err = h.authenticate(conn)
		if err != nil {
			conn.Close()
			return
		}
	}

	// Clear read deadline for proxying.
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		conn.Close()
		return
	}

	// Phase 3: CONNECT request.
	target, reqErr := h.readRequest(conn)
	if reqErr != nil {
		replyCode := byte(socks5RepGeneralFailure)
		if se, ok := reqErr.(*socks5Error); ok {
			replyCode = se.code
		}
		h.sendReply(conn, replyCode)
		conn.Close()
		return
	}

	if err := h.sendReply(conn, socks5RepSuccess); err != nil {
		conn.Close()
		return
	}
	h.proxyConnect(ctx, conn, clientIP, platName, account, target, "SOCKS5")
}

// ServeConnSOCKS4 handles a single SOCKS4a connection (blocking).
func (h *Socks5Handler) ServeConnSOCKS4(conn net.Conn) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clientIP, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
	if clientIP == "" {
		clientIP = conn.RemoteAddr().String()
	}
	deadline := time.Now().Add(h.timeout)
	if err := conn.SetReadDeadline(deadline); err != nil {
		conn.Close()
		return
	}

	target, err := h.readSOCKS4Request(conn)
	if err != nil {
		h.writeSOCKS4Reply(conn, socks4RepReject)
		conn.Close()
		return
	}

	// Clear read deadline for proxying.
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		conn.Close()
		return
	}

	// SOCKS4 has no authentication field; extract identity from null-terminated user ID.
	// Format matches existing convention: user ID can be "platform:account".
	// When proxy token is configured, SOCKS4 user ID is ignored (no secure auth in SOCKS4).
	var platName, account string
	// We don't parse SOCKS4 user ID for auth — SOCKS4 is inherently insecure.

	h.writeSOCKS4Reply(conn, socks4RepGranted)
	h.proxyConnect(ctx, conn, clientIP, platName, account, target, "SOCKS4")
}

// proxyConnect is the shared CONNECT tunnel logic for both SOCKS5 and SOCKS4.
func (h *Socks5Handler) proxyConnect(ctx context.Context, conn net.Conn, clientIP, platName, account, target, methodLabel string) {
	now := time.Now()
	lifecycle := &socks5Lifecycle{
		events:   h.events,
		finished: RequestFinishedEvent{ProxyType: ProxyTypeForward, IsConnect: true},
		log: RequestLogEntry{
			StartedAtNs: now.UnixNano(),
			ProxyType:   ProxyTypeForward,
			ClientIP:    clientIP,
			HTTPMethod:  methodLabel,
		},
	}
	defer lifecycle.finish()
	lifecycle.log.Account = account
	lifecycle.log.TargetHost = target

	routed, routeErr := resolveRoutedOutbound(h.router, h.pool, platName, account, target)
	if routeErr != nil {
		lifecycle.setProxyError(routeErr)
		lifecycle.setHTTPStatus(routeErr.HTTPCode)
		return
	}
	lifecycle.setRouteResult(routed.Route)

	domain := netutil.ExtractDomain(target)
	go h.health.RecordLatency(routed.Route.NodeHash, domain, nil)

	rawConn, err := routed.Outbound.DialContext(ctx, "tcp", M.ParseSocksaddr(target))
	if err != nil {
		proxyErr := classifyConnectError(err)
		lifecycle.setProxyError(proxyErr)
		lifecycle.setUpstreamError("socks_dial", err)
		if proxyErr != nil {
			lifecycle.setHTTPStatus(proxyErr.HTTPCode)
			go h.health.RecordResult(routed.Route.NodeHash, false)
		}
		return
	}

	var upstreamBase net.Conn = rawConn
	if h.metricsSink != nil {
		h.metricsSink.OnConnectionLifecycle(ConnectionOutbound, ConnectionOpen)
		upstreamBase = newCountingConn(rawConn, h.metricsSink)
	}
	upstreamConn := newTLSLatencyConn(upstreamBase, func(latency time.Duration) {
		h.health.RecordLatency(routed.Route.NodeHash, domain, &latency)
	})

	// Bind upstream lifecycle to client disconnect.
	go func() {
		<-ctx.Done()
		upstreamConn.Close()
	}()

	// Bidirectional tunnel.
	type copyResult struct {
		n   int64
		err error
	}
	egressCh := make(chan copyResult, 1)
	go func() {
		defer upstreamConn.Close()
		n, copyErr := io.Copy(upstreamConn, conn)
		egressCh <- copyResult{n: n, err: copyErr}
	}()
	ingressBytes, ingressErr := io.Copy(conn, upstreamConn)
	conn.Close()
	upstreamConn.Close()
	egressResult := <-egressCh

	lifecycle.addIngressBytes(ingressBytes)
	lifecycle.addEgressBytes(egressResult.n)

	okResult := ingressBytes > 0 && egressResult.n > 0
	if !okResult {
		lifecycle.setProxyError(ErrUpstreamRequestFailed)
		switch {
		case !isBenignTunnelCopyError(ingressErr):
			lifecycle.setUpstreamError("socks_upstream_to_client", ingressErr)
		case !isBenignTunnelCopyError(egressResult.err):
			lifecycle.setUpstreamError("socks_client_to_upstream", egressResult.err)
		}
	}
	lifecycle.setNetOK(okResult)
	go h.health.RecordResult(routed.Route.NodeHash, okResult)
}

// socks5Lifecycle is a lightweight lifecycle tracker for SOCKS connections.
type socks5Lifecycle struct {
	events   EventEmitter
	finished RequestFinishedEvent
	log      RequestLogEntry
}

func (l *socks5Lifecycle) finish() {
	durationNs := time.Since(time.Unix(0, l.log.StartedAtNs)).Nanoseconds()
	l.finished.DurationNs = durationNs
	l.log.DurationNs = durationNs
	l.events.EmitRequestFinished(l.finished)
	l.events.EmitRequestLog(l.log)
}

func (l *socks5Lifecycle) setHTTPStatus(code int) { l.log.HTTPStatus = code }

func (l *socks5Lifecycle) setProxyError(pe *ProxyError) {
	if pe == nil {
		return
	}
	l.log.ResinError = pe.ResinError
	if l.log.HTTPStatus == 0 {
		l.log.HTTPStatus = pe.HTTPCode
	}
}

func (l *socks5Lifecycle) setUpstreamError(stage string, err error) {
	if l.log.UpstreamStage == "" && stage != "" {
		l.log.UpstreamStage = stage
	}
	if err == nil || l.log.UpstreamErrMsg != "" {
		return
	}
	detail := summarizeUpstreamError(err)
	l.log.UpstreamErrKind = detail.Kind
	l.log.UpstreamErrno = detail.Errno
	l.log.UpstreamErrMsg = detail.Message
}

func (l *socks5Lifecycle) addIngressBytes(n int64) { if n > 0 { l.log.IngressBytes += n } }
func (l *socks5Lifecycle) addEgressBytes(n int64)  { if n > 0 { l.log.EgressBytes += n } }

func (l *socks5Lifecycle) setNetOK(ok bool) {
	l.finished.NetOK = ok
	l.log.NetOK = ok
}

func (l *socks5Lifecycle) setRouteResult(result routing.RouteResult) {
	l.finished.PlatformID = result.PlatformID
	l.log.PlatformID = result.PlatformID
	l.log.PlatformName = result.PlatformName
	l.log.NodeHash = result.NodeHash.Hex()
	l.log.NodeTag = result.NodeTag
	l.log.EgressIP = result.EgressIP.String()
}

// --- SOCKS5 protocol ---

// readMethodSelection reads the SOCKS5 method selection message.
func (h *Socks5Handler) readMethodSelection(r io.Reader) (byte, error) {
	buf := make([]byte, 2)
	if _, err := io.ReadFull(r, buf); err != nil {
		return 0, fmt.Errorf("socks5: read version/method count: %w", err)
	}
	if buf[0] != socks5Version {
		return 0, fmt.Errorf("socks5: unsupported version %d", buf[0])
	}
	nmethods := int(buf[1])
	if nmethods == 0 {
		return 0xFF, nil
	}
	methods := make([]byte, nmethods)
	if _, err := io.ReadFull(r, methods); err != nil {
		return 0, fmt.Errorf("socks5: read methods: %w", err)
	}
	for _, m := range methods {
		if m == socks5AuthNone {
			return m, nil
		}
	}
	for _, m := range methods {
		if m == socks5AuthUser {
			return m, nil
		}
	}
	return 0xFF, nil
}

func (h *Socks5Handler) writeMethodSelection(w io.Writer, method byte) {
	w.Write([]byte{socks5Version, method})
}

// authenticate performs RFC 1929 username/password sub-negotiation.
func (h *Socks5Handler) authenticate(rw io.ReadWriter) (string, string, error) {
	buf := make([]byte, 2)
	if _, err := io.ReadFull(rw, buf); err != nil {
		return "", "", fmt.Errorf("socks5: read auth header: %w", err)
	}
	if buf[0] != 0x01 {
		return "", "", fmt.Errorf("socks5: unsupported auth sub-negotiation version %d", buf[0])
	}
	uname := make([]byte, buf[1])
	if _, err := io.ReadFull(rw, uname); err != nil {
		return "", "", fmt.Errorf("socks5: read username: %w", err)
	}
	plenBuf := make([]byte, 1)
	if _, err := io.ReadFull(rw, plenBuf); err != nil {
		return "", "", fmt.Errorf("socks5: read password length: %w", err)
	}
	passwd := make([]byte, plenBuf[0])
	if _, err := io.ReadFull(rw, passwd); err != nil {
		return "", "", fmt.Errorf("socks5: read password: %w", err)
	}

	username := string(uname)
	password := string(passwd)

	if username != h.token {
		rw.Write([]byte{0x01, 0x01})
		return "", "", fmt.Errorf("socks5: authentication failed")
	}

	var platName, account string
	if h.authVer == config.AuthVersionV1 {
		platName, account = parseForwardCredentialV1WhenAuthDisabled(password)
	} else {
		platName, account = parseLegacyPlatformAccountIdentity(password)
	}

	rw.Write([]byte{0x01, 0x00})
	return platName, account, nil
}

// readRequest reads a SOCKS5 CONNECT request and returns the target address.
func (h *Socks5Handler) readRequest(r io.Reader) (string, error) {
	buf := make([]byte, 4)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", fmt.Errorf("socks5: read request header: %w", err)
	}
	if buf[0] != socks5Version {
		return "", fmt.Errorf("socks5: unsupported version %d in request", buf[0])
	}
	if buf[1] != socks5CmdConnect {
		return "", &socks5Error{code: socks5RepCommandNotSupported}
	}

	atyp := buf[3]
	var host string
	switch atyp {
	case socks5AddrIPv4:
		ipBuf := make([]byte, 4)
		if _, err := io.ReadFull(r, ipBuf); err != nil {
			return "", fmt.Errorf("socks5: read IPv4: %w", err)
		}
		host = net.IP(ipBuf).String()
	case socks5AddrDomain:
		lenBuf := make([]byte, 1)
		if _, err := io.ReadFull(r, lenBuf); err != nil {
			return "", fmt.Errorf("socks5: read domain length: %w", err)
		}
		domain := make([]byte, lenBuf[0])
		if _, err := io.ReadFull(r, domain); err != nil {
			return "", fmt.Errorf("socks5: read domain: %w", err)
		}
		host = string(domain)
	case socks5AddrIPv6:
		ipBuf := make([]byte, 16)
		if _, err := io.ReadFull(r, ipBuf); err != nil {
			return "", fmt.Errorf("socks5: read IPv6: %w", err)
		}
		host = net.IP(ipBuf).String()
	default:
		return "", &socks5Error{code: socks5RepAddressNotSupported}
	}

	portBuf := make([]byte, 2)
	if _, err := io.ReadFull(r, portBuf); err != nil {
		return "", fmt.Errorf("socks5: read port: %w", err)
	}
	port := binary.BigEndian.Uint16(portBuf)

	return net.JoinHostPort(host, strconv.Itoa(int(port))), nil
}

// sendReply writes a SOCKS5 reply.
func (h *Socks5Handler) sendReply(w io.Writer, rep byte) error {
	_, err := w.Write([]byte{socks5Version, rep, 0x00, socks5AddrIPv4, 0, 0, 0, 0, 0, 0})
	return err
}

type socks5Error struct {
	code byte
}

func (e *socks5Error) Error() string {
	return fmt.Sprintf("socks5 error: reply code 0x%02x", e.code)
}

// --- SOCKS4a protocol ---

// readSOCKS4Request reads a SOCKS4a CONNECT request.
// Format: VN(1) CD(1) DSTPORT(2) DSTIP(4) USERID(null-terminated) [+ DOMAIN(null-terminated)]
func (h *Socks5Handler) readSOCKS4Request(r io.Reader) (string, error) {
	buf := make([]byte, 8)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", fmt.Errorf("socks4: read request header: %w", err)
	}
	if buf[0] != socks4Version {
		return "", fmt.Errorf("socks4: unsupported version %d", buf[0])
	}
	if buf[1] != socks4CmdConnect {
		return "", fmt.Errorf("socks4: unsupported command 0x%02x", buf[1])
	}

	port := binary.BigEndian.Uint16(buf[2:4])
	ip := net.IP(buf[4:8])

	// Read null-terminated user ID.
	userID, err := readNullTerminated(r)
	if err != nil {
		return "", fmt.Errorf("socks4: read user ID: %w", err)
	}
	_ = userID // SOCKS4 user ID is not used for auth

	// Check for SOCKS4a: if IP is 0.0.0.x (x > 0), the domain follows.
	isSOCKS4a := ip[0] == 0 && ip[1] == 0 && ip[2] == 0 && ip[3] != 0
	var host string
	if isSOCKS4a {
		domain, err := readNullTerminated(r)
		if err != nil {
			return "", fmt.Errorf("socks4a: read domain: %w", err)
		}
		host = domain
	} else if ip[0] == 0 && ip[1] == 0 && ip[2] == 0 && ip[3] == 0 {
		return "", fmt.Errorf("socks4: invalid destination IP 0.0.0.0")
	} else {
		host = ip.String()
	}

	return net.JoinHostPort(host, strconv.Itoa(int(port))), nil
}

// writeSOCKS4Reply writes a SOCKS4 reply.
func (h *Socks5Handler) writeSOCKS4Reply(w io.Writer, code byte) {
	reply := make([]byte, 8)
	reply[0] = 0x00 // VN (ignored)
	reply[1] = code
	// bytes 2-7 are port and IP, set to zero
	w.Write(reply)
}

func readNullTerminated(r io.Reader) (string, error) {
	var buf []byte
	b := make([]byte, 1)
	for {
		if _, err := io.ReadFull(r, b); err != nil {
			return "", err
		}
		if b[0] == 0 {
			break
		}
		buf = append(buf, b[0])
	}
	return string(buf), nil
}
