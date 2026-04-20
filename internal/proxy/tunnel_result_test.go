package proxy

import (
	"errors"
	"io"
	"testing"
)

func TestClassifyTunnelOutcome(t *testing.T) {
	cases := []struct {
		name         string
		ingressBytes int64
		ingressErr   error
		egressBytes  int64
		egressErr    error
		wantOK       bool
		wantStage    string
		wantErr      bool
	}{
		{
			name:         "SuccessWithBenignEOF",
			ingressBytes: 128,
			ingressErr:   io.EOF,
			egressBytes:  256,
			egressErr:    nil,
			wantOK:       true,
		},
		{
			name:         "IngressCopyErrorAfterTraffic",
			ingressBytes: 512,
			ingressErr:   errors.New("read tcp: connection reset by peer"),
			egressBytes:  256,
			egressErr:    nil,
			wantStage:    "ingress-stage",
			wantErr:      true,
		},
		{
			name:         "EgressCopyErrorAfterTraffic",
			ingressBytes: 512,
			ingressErr:   nil,
			egressBytes:  256,
			egressErr:    errors.New("write tcp: broken pipe"),
			wantStage:    "egress-stage",
			wantErr:      true,
		},
		{
			name:      "ZeroTraffic",
			wantStage: "zero-stage",
		},
		{
			name:         "MissingIngressTraffic",
			egressBytes:  64,
			egressErr:    io.EOF,
			wantStage:    "no-ingress-stage",
		},
		{
			name:         "MissingEgressTraffic",
			ingressBytes: 64,
			ingressErr:   io.EOF,
			wantStage:    "no-egress-stage",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyTunnelOutcome(
				"ingress-stage",
				"egress-stage",
				"zero-stage",
				"no-ingress-stage",
				"no-egress-stage",
				tc.ingressBytes,
				tc.ingressErr,
				tc.egressBytes,
				tc.egressErr,
			)
			if got.ok != tc.wantOK {
				t.Fatalf("ok: got %v, want %v", got.ok, tc.wantOK)
			}
			if got.stage != tc.wantStage {
				t.Fatalf("stage: got %q, want %q", got.stage, tc.wantStage)
			}
			if (got.err != nil) != tc.wantErr {
				t.Fatalf("err present: got %v, want %v", got.err != nil, tc.wantErr)
			}
		})
	}
}
