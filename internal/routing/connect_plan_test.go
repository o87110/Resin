package routing

import (
	"net/netip"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/model"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/platform"
)

func TestBuildConnectRoutePlan_StickySameIPThenLowerTiers(t *testing.T) {
	pool := newRouterTestPool()

	priorityTiers, err := platform.CompilePriorityTiers([]model.PlatformPriorityTier{
		{RegexFilters: []string{"tier1"}},
		{RegexFilters: []string{"tier2"}},
	})
	if err != nil {
		t.Fatalf("CompilePriorityTiers: %v", err)
	}

	plat := platform.NewConfiguredPlatform(
		"plat-connect",
		"Plat-Connect",
		nil,
		nil,
		nil,
		priorityTiers,
		int64(time.Hour),
		string(platform.ReverseProxyMissActionTreatAsEmpty),
		string(platform.ReverseProxyEmptyAccountBehaviorRandom),
		"",
		string(platform.AllocationPolicyBalanced),
	)
	pool.addPlatform(plat)

	stickyHash, stickyEntry := newRoutableEntry(t, `{"id":"sticky"}`, "198.51.100.10")
	sameIP1Hash, sameIP1Entry := newRoutableEntry(t, `{"id":"same-ip-1"}`, "198.51.100.10")
	sameIP2Hash, sameIP2Entry := newRoutableEntry(t, `{"id":"same-ip-2"}`, "198.51.100.10")
	tier1OtherHash, tier1OtherEntry := newRoutableEntry(t, `{"id":"tier1-other"}`, "198.51.100.20")
	tier2OtherHash, tier2OtherEntry := newRoutableEntry(t, `{"id":"tier2-other"}`, "198.51.100.30")
	fallbackHash, fallbackEntry := newRoutableEntry(t, `{"id":"fallback"}`, "198.51.100.40")
	for _, item := range []struct {
		hash  node.Hash
		entry *node.NodeEntry
	}{
		{stickyHash, stickyEntry},
		{sameIP1Hash, sameIP1Entry},
		{sameIP2Hash, sameIP2Entry},
		{tier1OtherHash, tier1OtherEntry},
		{tier2OtherHash, tier2OtherEntry},
		{fallbackHash, fallbackEntry},
	} {
		pool.addEntry(item.hash, item.entry)
	}

	plat.FullRebuild(
		func(fn func(node.Hash, *node.NodeEntry) bool) {
			for _, item := range []struct {
				hash  node.Hash
				entry *node.NodeEntry
			}{
				{stickyHash, stickyEntry},
				{sameIP1Hash, sameIP1Entry},
				{sameIP2Hash, sameIP2Entry},
				{tier1OtherHash, tier1OtherEntry},
				{tier2OtherHash, tier2OtherEntry},
				{fallbackHash, fallbackEntry},
			} {
				if !fn(item.hash, item.entry) {
					return
				}
			}
		},
		func(_ string, hash node.Hash) (string, bool, []string, bool) {
			switch hash {
			case stickyHash, sameIP1Hash, sameIP2Hash, tier1OtherHash:
				return "sub", true, []string{"tier1"}, true
			case tier2OtherHash:
				return "sub", true, []string{"tier2"}, true
			case fallbackHash:
				return "sub", true, []string{"fallback"}, true
			default:
				return "", false, nil, false
			}
		},
		func(_ netip.Addr) string { return "" },
	)

	// Make the same-IP ordering deterministic.
	sameIP1Entry.LatencyTable.Update("example.com", 20*time.Millisecond, 10*time.Minute)
	waitForDomainLatency(t, sameIP1Entry, "example.com")
	sameIP2Entry.LatencyTable.Update("example.com", 30*time.Millisecond, 10*time.Minute)
	waitForDomainLatency(t, sameIP2Entry, "example.com")

	router := newTestRouter(pool, nil)
	state, _ := router.states.LoadOrCompute(plat.ID, func() (*PlatformRoutingState, bool) {
		return NewPlatformRoutingState(), false
	})
	state.Leases.CreateLease("acct-connect", Lease{
		NodeHash:       stickyHash,
		EgressIP:       stickyEntry.GetEgressIP(),
		CreatedAtNs:    time.Now().Add(-time.Minute).UnixNano(),
		ExpiryNs:       time.Now().Add(time.Hour).UnixNano(),
		LastAccessedNs: time.Now().UnixNano(),
	})

	plan, err := router.BuildConnectRoutePlan(plat.Name, "acct-connect", "https://example.com")
	if err != nil {
		t.Fatalf("BuildConnectRoutePlan: %v", err)
	}

	want := []node.Hash{stickyHash, sameIP1Hash, sameIP2Hash, tier1OtherHash, tier2OtherHash, fallbackHash}
	if len(plan.Candidates) != len(want) {
		t.Fatalf("candidate count = %d, want %d", len(plan.Candidates), len(want))
	}
	for i, candidate := range plan.Candidates {
		if candidate.NodeHash != want[i] {
			t.Fatalf("candidate[%d] = %s, want %s", i, candidate.NodeHash.Hex(), want[i].Hex())
		}
	}
}

func TestCommitConnectRouteSuccess_MigratesLeaseToSuccessfulNode(t *testing.T) {
	pool := newRouterTestPool()
	plat := platform.NewPlatform("plat-commit", "Plat-Commit", nil, nil)
	plat.StickyTTLNs = int64(time.Hour)
	pool.addPlatform(plat)

	currentHash, currentEntry := newRoutableEntry(t, `{"id":"current"}`, "198.51.100.10")
	nextHash, nextEntry := newRoutableEntry(t, `{"id":"next"}`, "198.51.100.20")
	pool.addEntry(currentHash, currentEntry)
	pool.addEntry(nextHash, nextEntry)
	pool.rebuildPlatformView(plat)

	router := newTestRouter(pool, nil)
	state, _ := router.states.LoadOrCompute(plat.ID, func() (*PlatformRoutingState, bool) {
		return NewPlatformRoutingState(), false
	})
	state.Leases.CreateLease("acct-commit", Lease{
		NodeHash:       currentHash,
		EgressIP:       currentEntry.GetEgressIP(),
		CreatedAtNs:    time.Now().Add(-time.Minute).UnixNano(),
		ExpiryNs:       time.Now().Add(time.Hour).UnixNano(),
		LastAccessedNs: time.Now().UnixNano(),
	})

	plan := &ConnectRoutePlan{
		PlatformID:   plat.ID,
		PlatformName: plat.Name,
		platform:     plat,
		state:        state,
		account:      "acct-commit",
	}

	result, err := router.CommitConnectRouteSuccess(plan, ConnectCandidate{
		NodeHash:  nextHash,
		EgressIP:  nextEntry.GetEgressIP(),
		TierKind:  platform.PriorityTierViewKindPlatformPool,
		TierKey:   platform.PriorityTierViewKeyPlatformPool,
		TierIndex: -1,
	}, time.Now())
	if err != nil {
		t.Fatalf("CommitConnectRouteSuccess: %v", err)
	}
	if !result.LeaseCreated {
		t.Fatal("expected lease migration to create a new sticky binding")
	}

	updated, ok := state.Leases.GetLease("acct-commit")
	if !ok {
		t.Fatal("lease should exist after commit")
	}
	if updated.NodeHash != nextHash {
		t.Fatalf("updated lease node = %s, want %s", updated.NodeHash.Hex(), nextHash.Hex())
	}
	if updated.EgressIP != nextEntry.GetEgressIP() {
		t.Fatalf("updated lease egress ip = %s, want %s", updated.EgressIP, nextEntry.GetEgressIP())
	}
}
