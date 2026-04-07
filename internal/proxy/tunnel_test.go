package proxy

import (
	"io"
	"net"
	"testing"
	"time"
)

func TestPumpPreparedTunnelReader_FallsBackToFullCloseWhenHalfCloseUnavailable(t *testing.T) {
	clientBase, clientPeer := net.Pipe()
	upstreamBase, upstreamPeer := net.Pipe()
	defer clientPeer.Close()
	defer upstreamPeer.Close()

	clientConn := &connCloseNotifier{
		Conn: clientBase,
		sink: newCountingConnTestSink(),
	}
	upstreamConn := newTLSLatencyConn(newCountingConn(upstreamBase, newCountingConnTestSink()), nil)

	lifecycle := newRequestLifecycleFromMetadata(
		NoOpEventEmitter{},
		"127.0.0.1:12345",
		"",
		ProxyTypeSocks5Forward,
		true,
		false,
	)

	clientPayloadDone := make(chan []byte, 1)
	go func() {
		data, _ := io.ReadAll(clientPeer)
		clientPayloadDone <- data
	}()

	done := make(chan struct{})
	go func() {
		pumpPreparedTunnelReader(
			clientConn,
			clientConn,
			&preparedTunnel{
				upstreamConn: upstreamConn,
				recordResult: func(bool) {},
			},
			lifecycle,
			tunnelPumpOptions{},
		)
		close(done)
	}()

	go func() {
		_, _ = upstreamPeer.Write([]byte("server-push"))
		_ = upstreamPeer.Close()
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("pumpPreparedTunnelReader should fall back to full close when CloseWrite is unavailable")
	}

	select {
	case payload := <-clientPayloadDone:
		if string(payload) != "server-push" {
			t.Fatalf("client payload: got %q, want %q", string(payload), "server-push")
		}
	case <-time.After(time.Second):
		t.Fatal("expected client peer to receive upstream payload and EOF")
	}
}
