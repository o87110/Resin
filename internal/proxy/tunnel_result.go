package proxy

type tunnelOutcome struct {
	ok    bool
	stage string
	err   error
}

func classifyTunnelOutcome(
	ingressStage string,
	egressStage string,
	zeroTrafficStage string,
	noIngressStage string,
	noEgressStage string,
	ingressBytes int64,
	ingressErr error,
	egressBytes int64,
	egressErr error,
) tunnelOutcome {
	if !isBenignTunnelCopyError(ingressErr) {
		return tunnelOutcome{stage: ingressStage, err: ingressErr}
	}
	if !isBenignTunnelCopyError(egressErr) {
		return tunnelOutcome{stage: egressStage, err: egressErr}
	}

	switch {
	case ingressBytes == 0 && egressBytes == 0:
		return tunnelOutcome{stage: zeroTrafficStage}
	case ingressBytes == 0:
		return tunnelOutcome{stage: noIngressStage}
	case egressBytes == 0:
		return tunnelOutcome{stage: noEgressStage}
	default:
		return tunnelOutcome{ok: true}
	}
}
