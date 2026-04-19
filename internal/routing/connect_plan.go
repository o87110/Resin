package routing

import (
	"errors"
	"net/netip"
	"sort"
	"time"

	"github.com/Resinat/Resin/internal/netutil"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/platform"
	"github.com/puzpuzpuz/xsync/v4"
)

// ConnectCandidate describes one CONNECT-specific route candidate.
type ConnectCandidate struct {
	NodeHash     node.Hash
	EgressIP     netip.Addr
	NodeTag      string
	TierKind     string
	TierKey      string
	TierIndex    int
	Sticky       bool
	SameIPRetry  bool
}

// ConnectRoutePlan holds the ordered CONNECT candidates for one request.
type ConnectRoutePlan struct {
	PlatformID   string
	PlatformName string
	Candidates   []ConnectCandidate

	platform *platform.Platform
	state    *PlatformRoutingState
	account  string
}

// BuildConnectRoutePlan constructs the ordered CONNECT candidates without mutating leases.
func (r *Router) BuildConnectRoutePlan(platName, account, target string) (*ConnectRoutePlan, error) {
	plat, err := r.resolvePlatform(platName)
	if err != nil {
		return nil, err
	}

	state := r.ensurePlatformState(plat.ID)
	targetDomain := netutil.ExtractDomain(target)
	now := time.Now()
	descriptors := plat.TierViewDescriptors()

	plan := &ConnectRoutePlan{
		PlatformID:   plat.ID,
		PlatformName: plat.Name,
		platform:     plat,
		state:        state,
		account:      account,
	}

	candidates := make([]ConnectCandidate, 0, 8)
	seenNodes := make(map[node.Hash]struct{})
	seenIPs := make(map[netip.Addr]struct{})

	baseDescriptorIdx := -1
	currentIP := netip.Addr{}

	if account != "" {
		if currentLease, ok := state.Leases.GetLease(account); ok && !currentLease.IsExpired(now) {
			if sticky, stickyDescriptorIdx, stickyOK := r.connectStickyCandidate(plat, descriptors, currentLease); stickyOK {
				candidates = append(candidates, sticky)
				seenNodes[sticky.NodeHash] = struct{}{}
				if sticky.EgressIP.IsValid() {
					seenIPs[sticky.EgressIP] = struct{}{}
					currentIP = sticky.EgressIP
				}
				baseDescriptorIdx = stickyDescriptorIdx
			}

			sameIP := r.connectSameIPCandidates(plat, descriptors, currentLease.EgressIP, seenNodes, targetDomain)
			for _, candidate := range sameIP {
				candidates = append(candidates, candidate)
				seenNodes[candidate.NodeHash] = struct{}{}
				if candidate.EgressIP.IsValid() {
					seenIPs[candidate.EgressIP] = struct{}{}
					currentIP = candidate.EgressIP
				}
				if baseDescriptorIdx < 0 && candidate.TierIndex >= 0 {
					baseDescriptorIdx = r.connectDescriptorPos(descriptors, candidate.TierKey)
				}
			}
		}
	}

	if baseDescriptorIdx < 0 {
		baseDescriptorIdx = r.connectFirstNonEmptyDescriptorPos(descriptors)
	}
	if baseDescriptorIdx < 0 {
		plan.Candidates = []ConnectCandidate{}
		return plan, nil
	}

	if desc := descriptors[baseDescriptorIdx]; desc.View.Size() > 0 {
		tierCandidates := r.connectTierCandidates(plat, desc, targetDomain, seenNodes, seenIPs)
		candidates = append(candidates, tierCandidates...)
		for _, candidate := range tierCandidates {
			seenNodes[candidate.NodeHash] = struct{}{}
			if candidate.EgressIP.IsValid() {
				seenIPs[candidate.EgressIP] = struct{}{}
			}
		}
	}

	for i := baseDescriptorIdx + 1; i < len(descriptors); i++ {
		desc := descriptors[i]
		if desc.View.Size() == 0 {
			continue
		}
		tierCandidates := r.connectTierCandidates(plat, desc, targetDomain, seenNodes, seenIPs)
		candidates = append(candidates, tierCandidates...)
		for _, candidate := range tierCandidates {
			seenNodes[candidate.NodeHash] = struct{}{}
			if candidate.EgressIP.IsValid() {
				seenIPs[candidate.EgressIP] = struct{}{}
			}
		}
	}

	// Once the current IP group has been exhausted, never revisit it.
	if currentIP.IsValid() {
		filtered := candidates[:0]
		currentIPLeft := false
		for _, candidate := range candidates {
			if candidate.EgressIP == currentIP {
				if !currentIPLeft {
					filtered = append(filtered, candidate)
				}
				continue
			}
			currentIPLeft = true
			filtered = append(filtered, candidate)
		}
		candidates = filtered
	}

	plan.Candidates = candidates
	return plan, nil
}

// CommitConnectRouteSuccess persists sticky routing state for a successful CONNECT candidate.
func (r *Router) CommitConnectRouteSuccess(plan *ConnectRoutePlan, candidate ConnectCandidate, now time.Time) (RouteResult, error) {
	if plan == nil || plan.platform == nil || plan.state == nil {
		return RouteResult{}, errors.New("connect route plan is not initialized")
	}

	result := RouteResult{
		NodeHash:     candidate.NodeHash,
		EgressIP:     candidate.EgressIP,
		NodeTag:      candidate.NodeTag,
		LeaseCreated: false,
	}
	result = withPlatformContext(plan.platform, result)

	if plan.account == "" {
		return result, nil
	}

	nowNs := now.UnixNano()
	ttl := plan.platform.StickyTTLNs
	if ttl <= 0 {
		ttl = int64(24 * time.Hour)
	}

	var routeErr error
	_, _ = plan.state.Leases.leases.Compute(plan.account, func(current Lease, loaded bool) (Lease, xsync.ComputeOp) {
		hadPreviousLease := loaded
		invalidation := leaseInvalidationNone
		if loaded && current.IsExpired(now) {
			invalidation = leaseInvalidationExpire
			loaded = false
		}

		if loaded && current.NodeHash == candidate.NodeHash && current.EgressIP == candidate.EgressIP {
			newLease := current
			newLease.LastAccessedNs = nowNs
			r.emitLeaseEvent(LeaseEvent{
				Type:       LeaseTouch,
				PlatformID: plan.platform.ID,
				Account:    plan.account,
				NodeHash:   current.NodeHash,
				EgressIP:   current.EgressIP,
			})
			return newLease, xsync.UpdateOp
		}

		if loaded && current.EgressIP == candidate.EgressIP {
			newLease := current
			newLease.NodeHash = candidate.NodeHash
			newLease.LastAccessedNs = nowNs
			r.emitLeaseEvent(LeaseEvent{
				Type:       LeaseReplace,
				PlatformID: plan.platform.ID,
				Account:    plan.account,
				NodeHash:   candidate.NodeHash,
				EgressIP:   candidate.EgressIP,
			})
			return newLease, xsync.UpdateOp
		}

		newLease := Lease{
			NodeHash:       candidate.NodeHash,
			EgressIP:       candidate.EgressIP,
			CreatedAtNs:    nowNs,
			ExpiryNs:       now.Add(time.Duration(ttl)).UnixNano(),
			LastAccessedNs: nowNs,
		}
		if hadPreviousLease && newLease.ExpiryNs <= current.ExpiryNs {
			newLease.ExpiryNs = current.ExpiryNs + 1
		}

		if hadPreviousLease && invalidation == leaseInvalidationNone {
			invalidation = leaseInvalidationRemove
		}
		r.cleanupPreviousLease(plan.state, current, hadPreviousLease, invalidation, plan.platform.ID, plan.account)
		plan.state.IPLoadStats.Inc(newLease.EgressIP)
		r.emitLeaseEvent(LeaseEvent{
			Type:       LeaseCreate,
			PlatformID: plan.platform.ID,
			Account:    plan.account,
			NodeHash:   newLease.NodeHash,
			EgressIP:   newLease.EgressIP,
		})
		result.LeaseCreated = true
		return newLease, xsync.UpdateOp
	})
	if routeErr != nil {
		return RouteResult{}, routeErr
	}
	return result, nil
}

func (r *Router) connectStickyCandidate(
	plat *platform.Platform,
	descriptors []platform.TierViewDescriptor,
	current Lease,
) (ConnectCandidate, int, bool) {
	entry, ok := r.pool.GetEntry(current.NodeHash)
	if !ok || entry == nil || entry.GetEgressIP() != current.EgressIP || !plat.View().Contains(current.NodeHash) {
		return ConnectCandidate{}, -1, false
	}
	descriptor, pos, ok := r.connectDescriptorForHash(descriptors, current.NodeHash)
	if !ok {
		return ConnectCandidate{}, -1, false
	}
	candidate := r.makeConnectCandidate(descriptor, current.NodeHash, entry, false)
	candidate.Sticky = true
	return candidate, pos, true
}

func (r *Router) connectSameIPCandidates(
	plat *platform.Platform,
	descriptors []platform.TierViewDescriptor,
	targetIP netip.Addr,
	seenNodes map[node.Hash]struct{},
	targetDomain string,
) []ConnectCandidate {
	if !targetIP.IsValid() {
		return nil
	}

	type rankedCandidate struct {
		candidate ConnectCandidate
		score     float64
		latency   time.Duration
	}

	var ranked []rankedCandidate
	plat.View().Range(func(h node.Hash) bool {
		if _, ok := seenNodes[h]; ok {
			return true
		}
		entry, ok := r.pool.GetEntry(h)
		if !ok || entry == nil || entry.GetEgressIP() != targetIP {
			return true
		}
		descriptor, _, ok := r.connectDescriptorForHash(descriptors, h)
		if !ok {
			return true
		}
		latency, _ := sameIPCandidateLatency(entry, targetDomain, r.currentAuthorities(), r.currentP2CWindow())
		ranked = append(ranked, rankedCandidate{
			candidate: r.makeConnectCandidate(descriptor, h, entry, true),
			score:     calculateScore(h, latency, plat, r.ensurePlatformState(plat.ID).IPLoadStats, r.pool),
			latency:   latency,
		})
		return true
	})

	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score < ranked[j].score
		}
		if ranked[i].latency != ranked[j].latency {
			return ranked[i].latency < ranked[j].latency
		}
		return ranked[i].candidate.NodeHash.Hex() < ranked[j].candidate.NodeHash.Hex()
	})

	out := make([]ConnectCandidate, 0, 2)
	for _, candidate := range ranked {
		out = append(out, candidate.candidate)
		if len(out) == 2 {
			break
		}
	}
	return out
}

func (r *Router) connectTierCandidates(
	plat *platform.Platform,
	descriptor platform.TierViewDescriptor,
	targetDomain string,
	seenNodes map[node.Hash]struct{},
	seenIPs map[netip.Addr]struct{},
) []ConnectCandidate {
	type rankedCandidate struct {
		candidate ConnectCandidate
		score     float64
		latency   time.Duration
	}

	bestByIP := make(map[netip.Addr]rankedCandidate)
	descriptor.View.Range(func(h node.Hash) bool {
		if _, ok := seenNodes[h]; ok {
			return true
		}
		entry, ok := r.pool.GetEntry(h)
		if !ok || entry == nil {
			return true
		}
		ip := entry.GetEgressIP()
		if !ip.IsValid() {
			return true
		}
		if _, ok := seenIPs[ip]; ok {
			return true
		}

		latency, _ := sameIPCandidateLatency(entry, targetDomain, r.currentAuthorities(), r.currentP2CWindow())
		candidate := rankedCandidate{
			candidate: r.makeConnectCandidate(descriptor, h, entry, false),
			score:     calculateScore(h, latency, plat, r.ensurePlatformState(plat.ID).IPLoadStats, r.pool),
			latency:   latency,
		}
		if existing, ok := bestByIP[ip]; ok {
			if candidate.score < existing.score ||
				(candidate.score == existing.score && candidate.latency < existing.latency) ||
				(candidate.score == existing.score && candidate.latency == existing.latency &&
					candidate.candidate.NodeHash.Hex() < existing.candidate.NodeHash.Hex()) {
				bestByIP[ip] = candidate
			}
			return true
		}
		bestByIP[ip] = candidate
		return true
	})

	ranked := make([]rankedCandidate, 0, len(bestByIP))
	for _, candidate := range bestByIP {
		ranked = append(ranked, candidate)
	}
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].score != ranked[j].score {
			return ranked[i].score < ranked[j].score
		}
		if ranked[i].latency != ranked[j].latency {
			return ranked[i].latency < ranked[j].latency
		}
		return ranked[i].candidate.NodeHash.Hex() < ranked[j].candidate.NodeHash.Hex()
	})

	out := make([]ConnectCandidate, 0, len(ranked))
	for _, candidate := range ranked {
		out = append(out, candidate.candidate)
	}
	return out
}

func (r *Router) connectDescriptorForHash(descriptors []platform.TierViewDescriptor, h node.Hash) (platform.TierViewDescriptor, int, bool) {
	for i, descriptor := range descriptors {
		if descriptor.View != nil && descriptor.View.Contains(h) {
			return descriptor, i, true
		}
	}
	return platform.TierViewDescriptor{}, -1, false
}

func (r *Router) connectDescriptorPos(descriptors []platform.TierViewDescriptor, key string) int {
	for i, descriptor := range descriptors {
		if descriptor.Key == key {
			return i
		}
	}
	return -1
}

func (r *Router) connectFirstNonEmptyDescriptorPos(descriptors []platform.TierViewDescriptor) int {
	for i, descriptor := range descriptors {
		if descriptor.View != nil && descriptor.View.Size() > 0 {
			return i
		}
	}
	return -1
}

func (r *Router) makeConnectCandidate(descriptor platform.TierViewDescriptor, h node.Hash, entry *node.NodeEntry, sameIPRetry bool) ConnectCandidate {
	candidate := ConnectCandidate{
		NodeHash:    h,
		TierKind:    descriptor.Kind,
		TierKey:     descriptor.Key,
		TierIndex:   descriptor.Index,
		SameIPRetry: sameIPRetry,
	}
	if entry != nil {
		candidate.EgressIP = entry.GetEgressIP()
	}
	if r.nodeTagResolver != nil {
		candidate.NodeTag = r.nodeTagResolver(h)
	}
	return candidate
}

func (r *Router) currentAuthorities() []string {
	if r.authorities == nil {
		return nil
	}
	return r.authorities()
}

func (r *Router) currentP2CWindow() time.Duration {
	if r.p2cWindow == nil {
		return 10 * time.Minute
	}
	window := r.p2cWindow()
	if window <= 0 {
		return 10 * time.Minute
	}
	return window
}
