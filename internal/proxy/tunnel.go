package proxy

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"sync"
)

type preparedTunnel struct {
	upstreamConn net.Conn
	recordResult func(bool)
}

type tunnelRelayResult struct {
	ingressBytes  int64
	egressBytes   int64
	netOK         bool
	proxyErr      *ProxyError
	upstreamStage string
	upstreamErr   error
}

type tunnelPumpOptions struct {
	requireBidirectionalTraffic bool
	ingressStage                string
	egressStage                 string
	zeroTrafficStage            string
	noIngressStage              string
	noEgressStage               string
	prefetchDrainStage          string
}

func pumpPreparedTunnel(
	clientConn net.Conn,
	clientReader *bufio.Reader,
	session *preparedTunnel,
	opts tunnelPumpOptions,
) tunnelRelayResult {
	clientToUpstream, err := makeTunnelClientReader(clientConn, clientReader)
	if err != nil {
		if session != nil && session.upstreamConn != nil {
			_ = session.upstreamConn.Close()
		}
		if clientConn != nil {
			_ = clientConn.Close()
		}
		return tunnelRelayResult{
			proxyErr:      ErrUpstreamRequestFailed,
			upstreamStage: opts.stageOrDefault(opts.prefetchDrainStage, "connect_client_prefetch_drain"),
			upstreamErr:   err,
		}
	}
	return pumpPreparedTunnelReader(clientConn, clientToUpstream, session, opts)
}

func pumpPreparedTunnelReader(
	clientConn net.Conn,
	clientToUpstream io.Reader,
	session *preparedTunnel,
	opts tunnelPumpOptions,
) tunnelRelayResult {
	if clientConn == nil || clientToUpstream == nil || session == nil || session.upstreamConn == nil {
		return tunnelRelayResult{}
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

	ingressErrBenign := isBenignTunnelCopyError(ingressResult.err)
	egressErrBenign := isBenignTunnelCopyError(egressResult.err)
	// A client-side TCP reset after the upstream response has already started is
	// a shutdown artifact, not an upstream failure. This commonly happens when a
	// tunnel client exits immediately after consuming the response.
	if !egressErrBenign && ingressResult.n > 0 && isClientReadResetError(egressResult.err) {
		egressErrBenign = true
	}

	result := tunnelRelayResult{
		ingressBytes: ingressResult.n,
		egressBytes:  egressResult.n,
		netOK:        true,
	}
	ingressStage := opts.stageOrDefault(opts.ingressStage, "connect_upstream_to_client_copy")
	egressStage := opts.stageOrDefault(opts.egressStage, "connect_client_to_upstream_copy")
	zeroTrafficStage := opts.stageOrDefault(opts.zeroTrafficStage, "connect_zero_traffic")
	noIngressStage := opts.stageOrDefault(opts.noIngressStage, "connect_no_ingress_traffic")
	noEgressStage := opts.stageOrDefault(opts.noEgressStage, "connect_no_egress_traffic")
	switch {
	case !ingressErrBenign:
		result.netOK = false
		result.proxyErr = ErrUpstreamRequestFailed
		result.upstreamStage = ingressStage
		result.upstreamErr = ingressResult.err
	case !egressErrBenign:
		result.netOK = false
		result.proxyErr = ErrUpstreamRequestFailed
		result.upstreamStage = egressStage
		result.upstreamErr = egressResult.err
	case opts.requireBidirectionalTraffic && (ingressResult.n == 0 || egressResult.n == 0):
		result.netOK = false
		result.proxyErr = ErrUpstreamRequestFailed
		switch {
		case ingressResult.n == 0 && egressResult.n == 0:
			result.upstreamStage = zeroTrafficStage
		case ingressResult.n == 0:
			result.upstreamStage = noIngressStage
		default:
			result.upstreamStage = noEgressStage
		}
	}
	return result
}

func (opts tunnelPumpOptions) stageOrDefault(stage, fallback string) string {
	if stage != "" {
		return stage
	}
	return fallback
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
