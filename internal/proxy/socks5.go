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
	"github.com/Resinat/Resin/internal/node"
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
	socks4Version    = 0x04
	socks4CmdConnect = 0x01
	socks4RepGranted = 0x5A
	socks4RepReject  = 0x5B

	socks4MaxFieldLen = 255
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

	allowInsecureSOCKS4 bool
}

// Socks5HandlerConfig holds dependencies for the SOCKS5 handler.
type Socks5HandlerConfig struct {
	ProxyToken          string
	AuthVersion         string
	Router              *routing.Router
	Pool                outbound.PoolAccessor
	Health              HealthRecorder
	Events              EventEmitter
	MetricsSink         MetricsEventSink
	Timeout             time.Duration
	AllowInsecureSOCKS4 bool
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
		token:               cfg.ProxyToken,
		authVer:             authVer,
		router:              cfg.Router,
		pool:                cfg.Pool,
		health:              cfg.Health,
		events:              ev,
		metricsSink:         cfg.MetricsSink,
		timeout:             timeout,
		allowInsecureSOCKS4: cfg.AllowInsecureSOCKS4,
	}
}

// ServeConn handles a single SOCKS5 connection (blocking).
func (h *Socks5Handler) ServeConn(conn net.Conn) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clientIP := clientIPFromConn(conn)
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

	h.writeMethodSelection(conn, method)

	// Phase 2: Authentication.
	var platName, account string
	if method == socks5AuthUser {
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
		h.writeSOCKS5FailureAndClose(conn, replyCode)
		return
	}

	lifecycle := newSOCKSLifecycle(h.events, clientIP, account, target, "SOCKS5")
	defer lifecycle.finish()

	connectResult := establishConnectWithFailover(
		ctx,
		h.router,
		h.pool,
		platName,
		account,
		target,
		h.health,
		"socks_dial",
	)
	lifecycle.log.ConnectAttemptTrace = connectResult.attempts
	lifecycle.log.ConnectAttemptCount, lifecycle.log.ConnectFailoverUsed = connectAttemptSummary(connectResult.attempts)
	if connectResult.routed.Route.NodeHash != node.Zero {
		lifecycle.setRouteResult(connectResult.routed.Route)
	}
	if connectResult.canceled {
		lifecycle.setNetOK(true)
		conn.Close()
		return
	}
	if connectResult.proxyErr != nil {
		lifecycle.setProxyError(connectResult.proxyErr)
		lifecycle.setHTTPStatus(connectResult.proxyErr.HTTPCode)
		if connectResult.dialErr != nil {
			lifecycle.setUpstreamError("socks_dial", connectResult.dialErr)
		}
		h.writeSOCKS5FailureAndClose(conn, proxyErrorToSOCKS5Reply(connectResult.proxyErr))
		return
	}
	routed := connectResult.routed
	upstreamConn := connectResult.conn
	domain := netutil.ExtractDomain(target)

	if err := h.sendReply(conn, socks5RepSuccess); err != nil {
		lifecycle.setProxyError(ErrUpstreamRequestFailed)
		lifecycle.setUpstreamError("socks_client_reply", err)
		lifecycle.setNetOK(false)
		conn.Close()
		upstreamConn.Close()
		return
	}

	h.relaySOCKSTunnel(ctx, conn, upstreamConn, routed.Route, domain, lifecycle)
}

// ServeConnSOCKS4 handles a single SOCKS4a connection (blocking).
func (h *Socks5Handler) ServeConnSOCKS4(conn net.Conn) {
	if !h.allowInsecureSOCKS4 || h.token != "" {
		h.writeSOCKS4RejectAndClose(conn)
		return
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	clientIP := clientIPFromConn(conn)
	deadline := time.Now().Add(h.timeout)
	if err := conn.SetReadDeadline(deadline); err != nil {
		conn.Close()
		return
	}

	target, err := h.readSOCKS4Request(conn)
	if err != nil {
		h.writeSOCKS4RejectAndClose(conn)
		return
	}

	// Clear read deadline for proxying.
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		conn.Close()
		return
	}

	var platName, account string
	lifecycle := newSOCKSLifecycle(h.events, clientIP, account, target, "SOCKS4")
	defer lifecycle.finish()

	routed, upstreamConn, proxyErr, dialErr, canceled := h.establishSOCKSUpstream(ctx, platName, account, target)
	if canceled {
		lifecycle.setNetOK(true)
		conn.Close()
		return
	}
	if proxyErr != nil {
		lifecycle.setProxyError(proxyErr)
		lifecycle.setHTTPStatus(proxyErr.HTTPCode)
		if dialErr != nil {
			lifecycle.setUpstreamError("socks_dial", dialErr)
			if h.health != nil && routed.Route.NodeHash != node.Zero {
				go h.health.RecordResult(routed.Route.NodeHash, false)
			}
		}
		h.writeSOCKS4RejectAndClose(conn)
		return
	}
	lifecycle.setRouteResult(routed.Route)

	domain := netutil.ExtractDomain(target)
	if h.health != nil {
		go h.health.RecordLatency(routed.Route.NodeHash, domain, nil)
	}

	if err := h.writeSOCKS4Reply(conn, socks4RepGranted); err != nil {
		lifecycle.setProxyError(ErrUpstreamRequestFailed)
		lifecycle.setUpstreamError("socks_client_reply", err)
		lifecycle.setNetOK(false)
		conn.Close()
		upstreamConn.Close()
		return
	}

	h.relaySOCKSTunnel(ctx, conn, upstreamConn, routed.Route, domain, lifecycle)
}

func clientIPFromConn(conn net.Conn) string {
	clientIP, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
	if clientIP == "" {
		clientIP = conn.RemoteAddr().String()
	}
	return clientIP
}

func newSOCKSLifecycle(events EventEmitter, clientIP, account, target, methodLabel string) *socks5Lifecycle {
	now := time.Now()
	return &socks5Lifecycle{
		events:   events,
		finished: RequestFinishedEvent{ProxyType: ProxyTypeForward, IsConnect: true},
		log: RequestLogEntry{
			StartedAtNs: now.UnixNano(),
			ProxyType:   ProxyTypeForward,
			ClientIP:    clientIP,
			HTTPMethod:  methodLabel,
			Account:     account,
			TargetHost:  target,
		},
	}
}

func (h *Socks5Handler) establishSOCKSUpstream(
	ctx context.Context,
	platName string,
	account string,
	target string,
) (routed routedOutbound, upstreamConn net.Conn, proxyErr *ProxyError, dialErr error, canceled bool) {
	routed, proxyErr = resolveRoutedOutbound(h.router, h.pool, platName, account, target)
	if proxyErr != nil {
		return routed, nil, proxyErr, nil, false
	}
	if routed.Route.NodeHash == node.Zero {
		return routedOutbound{}, nil, ErrInternalError, nil, false
	}

	upstreamConn, dialErr = routed.Outbound.DialContext(ctx, "tcp", M.ParseSocksaddr(target))
	if dialErr != nil {
		proxyErr = classifyConnectError(dialErr)
		if proxyErr == nil {
			return routed, nil, nil, dialErr, true
		}
		return routed, nil, proxyErr, dialErr, false
	}
	return routed, upstreamConn, nil, nil, false
}

func (h *Socks5Handler) relaySOCKSTunnel(
	ctx context.Context,
	conn net.Conn,
	upstreamConn net.Conn,
	route routing.RouteResult,
	domain string,
	lifecycle *socks5Lifecycle,
) {
	var upstreamBase net.Conn = upstreamConn
	if h.metricsSink != nil {
		h.metricsSink.OnConnectionLifecycle(ConnectionOutbound, ConnectionOpen)
		upstreamBase = newCountingConn(upstreamConn, h.metricsSink)
	}
	tunneledUpstreamConn := newTLSLatencyConn(upstreamBase, func(latency time.Duration) {
		if h.health != nil {
			h.health.RecordLatency(route.NodeHash, domain, &latency)
		}
	})

	// Bind upstream lifecycle to client disconnect.
	go func() {
		<-ctx.Done()
		tunneledUpstreamConn.Close()
	}()

	// Bidirectional tunnel.
	type copyResult struct {
		n   int64
		err error
	}
	egressCh := make(chan copyResult, 1)
	go func() {
		defer tunneledUpstreamConn.Close()
		n, copyErr := io.Copy(tunneledUpstreamConn, conn)
		egressCh <- copyResult{n: n, err: copyErr}
	}()
	ingressBytes, ingressErr := io.Copy(conn, tunneledUpstreamConn)
	conn.Close()
	tunneledUpstreamConn.Close()
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
	if h.health != nil {
		go h.health.RecordResult(route.NodeHash, okResult)
	}
}

func (h *Socks5Handler) writeSOCKS5FailureAndClose(conn net.Conn, rep byte) {
	_ = h.sendReply(conn, rep)
	conn.Close()
}

func (h *Socks5Handler) writeSOCKS4RejectAndClose(conn net.Conn) {
	_ = h.writeSOCKS4Reply(conn, socks4RepReject)
	conn.Close()
}

func proxyErrorToSOCKS5Reply(pe *ProxyError) byte {
	switch pe {
	case ErrUpstreamConnectFailed, ErrUpstreamTimeout:
		return socks5RepHostUnreachable
	default:
		return socks5RepGeneralFailure
	}
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

func (l *socks5Lifecycle) addIngressBytes(n int64) {
	if n > 0 {
		l.log.IngressBytes += n
	}
}
func (l *socks5Lifecycle) addEgressBytes(n int64) {
	if n > 0 {
		l.log.EgressBytes += n
	}
}

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

// readMethodSelection reads the SOCKS5 method selection message and selects
// the method Resin should use for this connection.
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
	hasNone := false
	hasUser := false
	for _, m := range methods {
		switch m {
		case socks5AuthNone:
			hasNone = true
		case socks5AuthUser:
			hasUser = true
		}
	}

	if h.token != "" {
		if hasUser {
			return socks5AuthUser, nil
		}
		return 0xFF, nil
	}

	// When proxy auth is disabled, still prefer username/password if the client
	// offers it so we can preserve optional Platform/Account extraction.
	if hasUser {
		return socks5AuthUser, nil
	}
	if hasNone {
		return socks5AuthNone, nil
	}
	return 0xFF, nil
}

func (h *Socks5Handler) writeMethodSelection(w io.Writer, method byte) {
	w.Write([]byte{socks5Version, method})
}

// authenticate performs RFC 1929 username/password sub-negotiation and maps
// the negotiated username/password pair to the same identity semantics as the
// HTTP forward proxy.
func (h *Socks5Handler) authenticate(rw io.ReadWriter) (string, string, error) {
	buf := make([]byte, 2)
	if _, err := io.ReadFull(rw, buf); err != nil {
		h.writeUserPassStatus(rw, 0x01)
		return "", "", fmt.Errorf("socks5: read auth header: %w", err)
	}
	if buf[0] != 0x01 {
		h.writeUserPassStatus(rw, 0x01)
		return "", "", fmt.Errorf("socks5: unsupported auth sub-negotiation version %d", buf[0])
	}
	uname := make([]byte, buf[1])
	if _, err := io.ReadFull(rw, uname); err != nil {
		h.writeUserPassStatus(rw, 0x01)
		return "", "", fmt.Errorf("socks5: read username: %w", err)
	}
	plenBuf := make([]byte, 1)
	if _, err := io.ReadFull(rw, plenBuf); err != nil {
		h.writeUserPassStatus(rw, 0x01)
		return "", "", fmt.Errorf("socks5: read password length: %w", err)
	}
	passwd := make([]byte, plenBuf[0])
	if _, err := io.ReadFull(rw, passwd); err != nil {
		h.writeUserPassStatus(rw, 0x01)
		return "", "", fmt.Errorf("socks5: read password: %w", err)
	}

	username := string(uname)
	password := string(passwd)
	credential := username + ":" + password

	if h.token == "" {
		var platName, account string
		if h.authVer == config.AuthVersionV1 {
			platName, account = parseForwardCredentialV1WhenAuthDisabled(credential)
		} else {
			platName, account = parseLegacyAuthDisabledIdentityCredential(credential)
		}
		h.writeUserPassStatus(rw, 0x00)
		return platName, account, nil
	}

	if h.authVer == config.AuthVersionV1 {
		token, platName, account := parseForwardCredentialV1(credential)
		if token != h.token {
			h.writeUserPassStatus(rw, 0x01)
			return "", "", fmt.Errorf("socks5: authentication failed")
		}
		h.writeUserPassStatus(rw, 0x00)
		return platName, account, nil
	}

	if username != h.token {
		h.writeUserPassStatus(rw, 0x01)
		return "", "", fmt.Errorf("socks5: authentication failed")
	}

	platName, account := parseLegacyPlatformAccountIdentity(password)
	h.writeUserPassStatus(rw, 0x00)
	return platName, account, nil
}

func (h *Socks5Handler) writeUserPassStatus(w io.Writer, status byte) {
	_, _ = w.Write([]byte{0x01, status})
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
	userID, err := readNullTerminated(r, socks4MaxFieldLen)
	if err != nil {
		return "", fmt.Errorf("socks4: read user ID: %w", err)
	}
	_ = userID // SOCKS4 user ID is not used for auth

	// Check for SOCKS4a: if IP is 0.0.0.x (x > 0), the domain follows.
	isSOCKS4a := ip[0] == 0 && ip[1] == 0 && ip[2] == 0 && ip[3] != 0
	var host string
	if isSOCKS4a {
		domain, err := readNullTerminated(r, socks4MaxFieldLen)
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
func (h *Socks5Handler) writeSOCKS4Reply(w io.Writer, code byte) error {
	reply := make([]byte, 8)
	reply[0] = 0x00 // VN (ignored)
	reply[1] = code
	// bytes 2-7 are port and IP, set to zero
	_, err := w.Write(reply)
	return err
}

func readNullTerminated(r io.Reader, maxLen int) (string, error) {
	var buf []byte
	b := make([]byte, 1)
	for {
		if _, err := io.ReadFull(r, b); err != nil {
			return "", err
		}
		if b[0] == 0 {
			break
		}
		if len(buf) >= maxLen {
			return "", fmt.Errorf("field exceeds maximum length %d", maxLen)
		}
		buf = append(buf, b[0])
	}
	return string(buf), nil
}
