package proxy

import (
	"context"
	"net"
	"time"

	"github.com/Resinat/Resin/internal/netutil"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/outbound"
	"github.com/Resinat/Resin/internal/routing"
	M "github.com/sagernet/sing/common/metadata"
)

const (
	connectStickyDialTimeout     = 3 * time.Second
	connectSameIPDialTimeout     = 1500 * time.Millisecond
	connectDifferentIPDialTimeout = 2 * time.Second
	connectTotalBudget           = 15 * time.Second
	connectMaxDifferentIPs       = 5
)

type connectDialResult struct {
	routed   routedOutbound
	conn     net.Conn
	attempts []ConnectAttemptTraceItem
	proxyErr *ProxyError
	dialErr  error
	canceled bool
}

func establishConnectWithFailover(
	parentCtx context.Context,
	router *routing.Router,
	pool outbound.PoolAccessor,
	platformName string,
	account string,
	target string,
	health HealthRecorder,
	dialStage string,
) connectDialResult {
	plan, err := router.BuildConnectRoutePlan(platformName, account, target)
	if err != nil {
		return connectDialResult{proxyErr: mapRouteError(err)}
	}
	if plan == nil || len(plan.Candidates) == 0 {
		return connectDialResult{proxyErr: ErrNoAvailableNodes}
	}

	domain := netutil.ExtractDomain(target)
	startedAt := time.Now()
	seenIPs := make(map[string]struct{})
	result := connectDialResult{
		attempts: make([]ConnectAttemptTraceItem, 0, len(plan.Candidates)),
	}

	for idx, candidate := range plan.Candidates {
		if remaining := connectTotalBudget - time.Since(startedAt); remaining <= 0 {
			break
		}
		if candidate.EgressIP.IsValid() {
			ipKey := candidate.EgressIP.String()
			if _, ok := seenIPs[ipKey]; !ok {
				if len(seenIPs) >= connectMaxDifferentIPs {
					break
				}
				seenIPs[ipKey] = struct{}{}
			}
		}

		timeout := connectCandidateTimeout(candidate)
		if remaining := connectTotalBudget - time.Since(startedAt); remaining < timeout {
			timeout = remaining
		}
		if timeout <= 0 {
			break
		}

		trace := ConnectAttemptTraceItem{
			Index:         idx + 1,
			NodeHash:      candidate.NodeHash.Hex(),
			NodeTag:       candidate.NodeTag,
			EgressIP:      candidate.EgressIP.String(),
			TierKind:      candidate.TierKind,
			TierKey:       candidate.TierKey,
			TierIndex:     candidate.TierIndex,
			SameIPRetry:   candidate.SameIPRetry,
			DialTimeoutMs: timeout.Milliseconds(),
			UpstreamStage: dialStage,
		}

		routed, resolveErr := resolveConnectCandidateOutbound(pool, plan, candidate)
		if resolveErr != nil {
			trace.DurationMs = 0
			trace.Result = "failure"
			trace.ResinError = resolveErr.ResinError
			result.proxyErr = resolveErr
			result.attempts = append(result.attempts, trace)
			continue
		}
		result.routed = routed

		if health != nil && routed.Route.NodeHash != node.Zero {
			go health.RecordLatency(routed.Route.NodeHash, domain, nil)
		}

		attemptStartedAt := time.Now()
		attemptCtx, cancel := context.WithTimeout(parentCtx, timeout)
		conn, dialErr := routed.Outbound.DialContext(attemptCtx, "tcp", M.ParseSocksaddr(target))
		cancel()
		trace.DurationMs = time.Since(attemptStartedAt).Milliseconds()

		if dialErr != nil {
			proxyErr := classifyConnectError(dialErr)
			if proxyErr == nil {
				trace.Result = "canceled"
				result.dialErr = dialErr
				result.canceled = true
				result.attempts = append(result.attempts, trace)
				return result
			}

			trace.Result = "failure"
			trace.ResinError = proxyErr.ResinError
			result.proxyErr = proxyErr
			result.dialErr = dialErr
			if health != nil && routed.Route.NodeHash != node.Zero {
				go health.RecordResult(routed.Route.NodeHash, false)
			}
			result.attempts = append(result.attempts, trace)
			continue
		}

		committed, commitErr := router.CommitConnectRouteSuccess(plan, candidate, time.Now())
		if commitErr != nil {
			_ = conn.Close()
			return connectDialResult{
				routed:   routed,
				attempts: append(result.attempts, trace),
				proxyErr: ErrInternalError,
				dialErr:  commitErr,
			}
		}
		routed.Route = committed
		trace.Result = "success"
		result.routed = routed
		result.conn = conn
		result.proxyErr = nil
		result.dialErr = nil
		result.attempts = append(result.attempts, trace)
		return result
	}

	if result.proxyErr == nil {
		result.proxyErr = ErrNoAvailableNodes
	}
	return result
}

func connectCandidateTimeout(candidate routing.ConnectCandidate) time.Duration {
	switch {
	case candidate.Sticky:
		return connectStickyDialTimeout
	case candidate.SameIPRetry:
		return connectSameIPDialTimeout
	default:
		return connectDifferentIPDialTimeout
	}
}

func resolveConnectCandidateOutbound(
	pool outbound.PoolAccessor,
	plan *routing.ConnectRoutePlan,
	candidate routing.ConnectCandidate,
) (routedOutbound, *ProxyError) {
	entry, ok := pool.GetEntry(candidate.NodeHash)
	if !ok {
		return routedOutbound{}, ErrNoAvailableNodes
	}
	obPtr := entry.Outbound.Load()
	if obPtr == nil {
		return routedOutbound{}, ErrNoAvailableNodes
	}
	return routedOutbound{
		Route: routing.RouteResult{
			PlatformID:   plan.PlatformID,
			PlatformName: plan.PlatformName,
			NodeHash:     candidate.NodeHash,
			EgressIP:     candidate.EgressIP,
			NodeTag:      candidate.NodeTag,
		},
		Outbound: *obPtr,
	}, nil
}

func connectAttemptSummary(attempts []ConnectAttemptTraceItem) (int, bool) {
	count := len(attempts)
	return count, count > 1
}
