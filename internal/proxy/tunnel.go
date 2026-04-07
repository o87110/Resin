package proxy

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"sync"
	"time"

	"github.com/Resinat/Resin/internal/netutil"
	"github.com/Resinat/Resin/internal/outbound"
	"github.com/Resinat/Resin/internal/routing"
	M "github.com/sagernet/sing/common/metadata"
)

type tunnelDeps struct {
	router      *routing.Router
	pool        outbound.PoolAccessor
	health      HealthRecorder
	metricsSink MetricsEventSink
}

type preparedTunnel struct {
	upstreamConn net.Conn
	recordResult func(bool)
}

type tunnelPrepareResult struct {
	session       *preparedTunnel
	proxyErr      *ProxyError
	upstreamStage string
	upstreamErr   error
}

type tunnelPumpOptions struct {
	requireBidirectionalTraffic bool
}

func prepareConnectTunnel(
	ctx context.Context,
	deps tunnelDeps,
	lifecycle *requestLifecycle,
	platformName string,
	account string,
	target string,
) tunnelPrepareResult {
	routed, routeErr := resolveRoutedOutbound(deps.router, deps.pool, platformName, account, target)
	if routeErr != nil {
		return tunnelPrepareResult{proxyErr: routeErr}
	}
	if lifecycle != nil {
		lifecycle.setRouteResult(routed.Route)
	}

	domain := netutil.ExtractDomain(target)
	nodeHashRaw := routed.Route.NodeHash
	if deps.health != nil {
		go deps.health.RecordLatency(nodeHashRaw, domain, nil)
	}

	rawConn, err := routed.Outbound.DialContext(ctx, "tcp", M.ParseSocksaddr(target))
	if err != nil {
		proxyErr := classifyConnectError(err)
		if proxyErr == nil {
			if lifecycle != nil {
				lifecycle.setNetOK(true)
			}
			return tunnelPrepareResult{}
		}
		if deps.health != nil {
			go deps.health.RecordResult(nodeHashRaw, false)
		}
		return tunnelPrepareResult{
			proxyErr:      proxyErr,
			upstreamStage: "connect_dial",
			upstreamErr:   err,
		}
	}
	if lifecycle != nil {
		lifecycle.setNetOK(true)
	}

	recordResult := func(ok bool) {
		if lifecycle != nil {
			lifecycle.setNetOK(ok)
		}
		if deps.health != nil {
			go deps.health.RecordResult(nodeHashRaw, ok)
		}
	}

	var upstreamBase net.Conn = rawConn
	if deps.metricsSink != nil {
		deps.metricsSink.OnConnectionLifecycle(ConnectionOutbound, ConnectionOpen)
		upstreamBase = newCountingConn(rawConn, deps.metricsSink)
	}

	upstreamConn := newTLSLatencyConn(upstreamBase, func(latency time.Duration) {
		if deps.health != nil {
			deps.health.RecordLatency(nodeHashRaw, domain, &latency)
		}
	})

	return tunnelPrepareResult{
		session: &preparedTunnel{
			upstreamConn: upstreamConn,
			recordResult: recordResult,
		},
	}
}

func pumpPreparedTunnel(
	clientConn net.Conn,
	clientReader *bufio.Reader,
	session *preparedTunnel,
	lifecycle *requestLifecycle,
	opts tunnelPumpOptions,
) {
	clientToUpstream, err := makeTunnelClientReader(clientConn, clientReader)
	if err != nil {
		session.upstreamConn.Close()
		clientConn.Close()
		lifecycle.setProxyError(ErrUpstreamRequestFailed)
		lifecycle.setUpstreamError("connect_client_prefetch_drain", err)
		session.recordResult(false)
		return
	}
	pumpPreparedTunnelReader(clientConn, clientToUpstream, session, lifecycle, opts)
}

func pumpPreparedTunnelReader(
	clientConn net.Conn,
	clientToUpstream io.Reader,
	session *preparedTunnel,
	lifecycle *requestLifecycle,
	opts tunnelPumpOptions,
) {
	if clientConn == nil || clientToUpstream == nil || session == nil || session.upstreamConn == nil || lifecycle == nil {
		return
	}

	type copyResult struct {
		n   int64
		err error
	}
	var closeBothOnce sync.Once
	closeBoth := func() {
		closeBothOnce.Do(func() {
			_ = clientConn.Close()
			_ = session.upstreamConn.Close()
		})
	}
	ingressBytesCh := make(chan copyResult, 1)
	egressBytesCh := make(chan copyResult, 1)
	go func() {
		n, copyErr := io.Copy(session.upstreamConn, clientToUpstream)
		if !isBenignTunnelCopyError(copyErr) || !closeWriteConn(session.upstreamConn) {
			closeBoth()
		}
		egressBytesCh <- copyResult{n: n, err: copyErr}
	}()
	go func() {
		n, copyErr := io.Copy(clientConn, session.upstreamConn)
		if !isBenignTunnelCopyError(copyErr) || !closeWriteConn(clientConn) {
			closeBoth()
		}
		ingressBytesCh <- copyResult{n: n, err: copyErr}
	}()

	ingressResult := <-ingressBytesCh
	egressResult := <-egressBytesCh
	closeBoth()
	lifecycle.addIngressBytes(ingressResult.n)
	lifecycle.addEgressBytes(egressResult.n)

	okResult := true
	switch {
	case !isBenignTunnelCopyError(ingressResult.err):
		okResult = false
		lifecycle.setProxyError(ErrUpstreamRequestFailed)
		lifecycle.setUpstreamError("connect_upstream_to_client_copy", ingressResult.err)
	case !isBenignTunnelCopyError(egressResult.err):
		okResult = false
		lifecycle.setProxyError(ErrUpstreamRequestFailed)
		lifecycle.setUpstreamError("connect_client_to_upstream_copy", egressResult.err)
	case opts.requireBidirectionalTraffic && (ingressResult.n == 0 || egressResult.n == 0):
		okResult = false
		lifecycle.setProxyError(ErrUpstreamRequestFailed)
		switch {
		case ingressResult.n == 0 && egressResult.n == 0:
			lifecycle.setUpstreamError("connect_zero_traffic", nil)
		case ingressResult.n == 0:
			lifecycle.setUpstreamError("connect_no_ingress_traffic", nil)
		default:
			lifecycle.setUpstreamError("connect_no_egress_traffic", nil)
		}
	}
	session.recordResult(okResult)
}

func closeWriteConn(conn net.Conn) bool {
	return closeWriteErr(conn) == nil
}

// makeTunnelClientReader returns a reader for client->upstream copy that
// preserves any bytes already buffered by a protocol reader before tunneling.
func makeTunnelClientReader(clientConn net.Conn, buffered *bufio.Reader) (io.Reader, error) {
	if buffered == nil {
		return clientConn, nil
	}
	n := buffered.Buffered()
	if n == 0 {
		return clientConn, nil
	}
	prefetched := make([]byte, n)
	if _, err := io.ReadFull(buffered, prefetched); err != nil {
		return nil, err
	}
	return io.MultiReader(bytes.NewReader(prefetched), clientConn), nil
}
