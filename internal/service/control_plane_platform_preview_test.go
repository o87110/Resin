package service

import (
	"net/netip"
	"testing"
	"time"

	"github.com/Resinat/Resin/internal/config"
	"github.com/Resinat/Resin/internal/model"
	"github.com/Resinat/Resin/internal/node"
	"github.com/Resinat/Resin/internal/subscription"
	"github.com/Resinat/Resin/internal/testutil"
	"github.com/Resinat/Resin/internal/topology"
)

type previewFilterFixture struct {
	cp          *ControlPlaneService
	hkHash      string
	usHash      string
	unknownHash string
}

func buildPreviewFilterFixture(t *testing.T) previewFilterFixture {
	t.Helper()

	subMgr := topology.NewSubscriptionManager()
	sub := subscription.NewSubscription("sub-1", "sub-1", "https://example.com/sub", true, false)
	subMgr.Register(sub)

	pool := topology.NewGlobalNodePool(topology.PoolConfig{
		SubLookup:              subMgr.Lookup,
		GeoLookup:              func(netip.Addr) string { return "" },
		MaxLatencyTableEntries: 16,
		MaxConsecutiveFailures: func() int { return 3 },
		LatencyDecayWindow:     func() time.Duration { return 10 * time.Minute },
	})

	hkRaw := []byte(`{"type":"ss","server":"1.1.1.1","port":443}`)
	hkHash := node.HashFromRawOptions(hkRaw)
	pool.AddNodeFromSub(hkHash, hkRaw, sub.ID)
	sub.ManagedNodes().StoreNode(hkHash, subscription.ManagedNode{Tags: []string{"all", "hk"}})

	usRaw := []byte(`{"type":"ss","server":"2.2.2.2","port":443}`)
	usHash := node.HashFromRawOptions(usRaw)
	pool.AddNodeFromSub(usHash, usRaw, sub.ID)
	sub.ManagedNodes().StoreNode(usHash, subscription.ManagedNode{Tags: []string{"all", "us"}})

	unknownRaw := []byte(`{"type":"ss","server":"3.3.3.3","port":443}`)
	unknownHash := node.HashFromRawOptions(unknownRaw)
	pool.AddNodeFromSub(unknownHash, unknownRaw, sub.ID)
	sub.ManagedNodes().StoreNode(unknownHash, subscription.ManagedNode{Tags: []string{"all", "unknown"}})

	hkEntry, ok := pool.GetEntry(hkHash)
	if !ok {
		t.Fatal("hk entry missing")
	}
	hkOutbound := testutil.NewNoopOutbound()
	hkEntry.Outbound.Store(&hkOutbound)
	hkEntry.SetEgressIP(netip.MustParseAddr("1.1.1.1"))
	hkEntry.SetEgressRegion("hk")

	usEntry, ok := pool.GetEntry(usHash)
	if !ok {
		t.Fatal("us entry missing")
	}
	usOutbound := testutil.NewNoopOutbound()
	usEntry.Outbound.Store(&usOutbound)
	usEntry.SetEgressIP(netip.MustParseAddr("2.2.2.2"))
	usEntry.SetEgressRegion("us")

	unknownEntry, ok := pool.GetEntry(unknownHash)
	if !ok {
		t.Fatal("unknown entry missing")
	}
	unknownOutbound := testutil.NewNoopOutbound()
	unknownEntry.Outbound.Store(&unknownOutbound)
	unknownEntry.SetEgressIP(netip.MustParseAddr("3.3.3.3"))

	cp := &ControlPlaneService{
		Pool:   pool,
		SubMgr: subMgr,
	}
	return previewFilterFixture{
		cp:          cp,
		hkHash:      hkHash.Hex(),
		usHash:      usHash.Hex(),
		unknownHash: unknownHash.Hex(),
	}
}

func TestPreviewFilter_RegionNegation(t *testing.T) {
	fixture := buildPreviewFilterFixture(t)

	nodes, err := fixture.cp.PreviewFilter(PreviewFilterRequest{
		PlatformSpec: &PlatformSpecFilter{
			RegexFilters:  []string{".*"},
			RegionFilters: []string{"!hk"},
		},
	})
	if err != nil {
		t.Fatalf("PreviewFilter: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("nodes len = %d, want 1", len(nodes))
	}
	if nodes[0].NodeHash != fixture.usHash {
		t.Fatalf("matched node = %s, want %s", nodes[0].NodeHash, fixture.usHash)
	}
	if nodes[0].NodeHash == fixture.hkHash {
		t.Fatalf("hk node %s should have been excluded", fixture.hkHash)
	}
}

func TestPreviewFilter_RegionMixedIncludeExclude(t *testing.T) {
	fixture := buildPreviewFilterFixture(t)

	nodes, err := fixture.cp.PreviewFilter(PreviewFilterRequest{
		PlatformSpec: &PlatformSpecFilter{
			RegexFilters:  []string{".*"},
			RegionFilters: []string{"hk", "!us"},
		},
	})
	if err != nil {
		t.Fatalf("PreviewFilter: %v", err)
	}
	if len(nodes) != 1 {
		t.Fatalf("nodes len = %d, want 1", len(nodes))
	}
	if nodes[0].NodeHash != fixture.hkHash {
		t.Fatalf("matched node = %s, want %s", nodes[0].NodeHash, fixture.hkHash)
	}

	nodes, err = fixture.cp.PreviewFilter(PreviewFilterRequest{
		PlatformSpec: &PlatformSpecFilter{
			RegexFilters:  []string{".*"},
			RegionFilters: []string{"hk", "!hk"},
		},
	})
	if err != nil {
		t.Fatalf("PreviewFilter: %v", err)
	}
	if len(nodes) != 0 {
		t.Fatalf("nodes len = %d, want 0", len(nodes))
	}
}

func TestPreviewFilter_RegionNegation_UnknownRegionExcluded(t *testing.T) {
	fixture := buildPreviewFilterFixture(t)

	nodes, err := fixture.cp.PreviewFilter(PreviewFilterRequest{
		PlatformSpec: &PlatformSpecFilter{
			RegexFilters:  []string{".*"},
			RegionFilters: []string{"!hk"},
		},
	})
	if err != nil {
		t.Fatalf("PreviewFilter: %v", err)
	}

	for _, node := range nodes {
		if node.NodeHash == fixture.unknownHash {
			t.Fatalf("node with unknown region %s should not match region filters", fixture.unknownHash)
		}
	}
}

func TestPreviewFilter_ExcludeRegexFilters(t *testing.T) {
	fixture := buildPreviewFilterFixture(t)

	nodes, err := fixture.cp.PreviewFilter(PreviewFilterRequest{
		PlatformSpec: &PlatformSpecFilter{
			RegexFilters:        []string{"hk|us|unknown"},
			ExcludeRegexFilters: []string{"hk"},
			RegionFilters:       []string{},
		},
	})
	if err != nil {
		t.Fatalf("PreviewFilter: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("nodes len = %d, want 2", len(nodes))
	}
	for _, matched := range nodes {
		if matched.NodeHash == fixture.hkHash {
			t.Fatalf("hk node %s should have been excluded by regex", fixture.hkHash)
		}
	}
}

func TestPreviewFilter_InvalidExcludeRegex(t *testing.T) {
	fixture := buildPreviewFilterFixture(t)

	_, err := fixture.cp.PreviewFilter(PreviewFilterRequest{
		PlatformSpec: &PlatformSpecFilter{
			ExcludeRegexFilters: []string{"(broken"},
		},
	})
	if err == nil {
		t.Fatal("expected invalid exclude regex error")
	}
}

type previewPriorityTierFixture struct {
	cp          *ControlPlaneService
	platformID  string
	residential string
	fast        string
	fallback    string
}

func buildPreviewPriorityTierFixture(t *testing.T) previewPriorityTierFixture {
	t.Helper()

	subMgr := topology.NewSubscriptionManager()
	sub := subscription.NewSubscription("sub-preview-tier", "sub-preview-tier", "https://example.com/sub-tier", true, false)
	subMgr.Register(sub)

	pool := topology.NewGlobalNodePool(topology.PoolConfig{
		SubLookup:              subMgr.Lookup,
		GeoLookup:              func(netip.Addr) string { return "us" },
		MaxLatencyTableEntries: 16,
		MaxConsecutiveFailures: func() int { return 3 },
		LatencyDecayWindow:     func() time.Duration { return 10 * time.Minute },
	})

	seedNode := func(raw string, tags []string, ip string) string {
		hash := node.HashFromRawOptions([]byte(raw))
		sub.ManagedNodes().StoreNode(hash, subscription.ManagedNode{Tags: tags})
		entry := node.NewNodeEntry(hash, []byte(raw), time.Now(), 16)
		entry.AddSubscriptionID(sub.ID)
		entry.SetEgressIP(netip.MustParseAddr(ip))
		entry.LatencyTable.LoadEntry("cloudflare.com", node.DomainLatencyStats{
			Ewma:        50 * time.Millisecond,
			LastUpdated: time.Now(),
		})
		ob := testutil.NewNoopOutbound()
		entry.Outbound.Store(&ob)
		pool.LoadNodeFromBootstrap(entry)
		return hash.Hex()
	}

	residentialHash := seedNode(`{"type":"ss","server":"1.1.1.1","port":443}`, []string{"residential"}, "1.2.3.4")
	fastHash := seedNode(`{"type":"ss","server":"1.1.1.2","port":443}`, []string{"fast"}, "1.2.3.5")
	fallbackHash := seedNode(`{"type":"ss","server":"1.1.1.3","port":443}`, []string{"fallback"}, "1.2.3.6")

	cp := &ControlPlaneService{
		Pool:   pool,
		SubMgr: subMgr,
		EnvCfg: &config.EnvConfig{
			DefaultPlatformStickyTTL:                        30 * time.Minute,
			DefaultPlatformRegexFilters:                     []string{},
			DefaultPlatformRegionFilters:                    []string{},
			DefaultPlatformReverseProxyMissAction:           "TREAT_AS_EMPTY",
			DefaultPlatformReverseProxyEmptyAccountBehavior: "RANDOM",
			DefaultPlatformReverseProxyFixedAccountHeader:   "Authorization",
			DefaultPlatformAllocationPolicy:                 "BALANCED",
		},
	}

	cfg := cp.defaultPlatformConfig("preview-tier-saved")
	cfg.PriorityTiers = []model.PlatformPriorityTier{
		{RegexFilters: []string{"residential"}},
		{RegexFilters: []string{"fast"}},
	}

	const platformID = "preview-tier-platform"
	plat, err := cfg.toRuntime(platformID)
	if err != nil {
		t.Fatalf("cfg.toRuntime: %v", err)
	}
	pool.RebuildPlatform(plat)
	pool.RegisterPlatform(plat)

	return previewPriorityTierFixture{
		cp:          cp,
		platformID:  platformID,
		residential: residentialHash,
		fast:        fastHash,
		fallback:    fallbackHash,
	}
}

func TestPreviewPriorityTierNodes_DraftMatchesSavedRuntime(t *testing.T) {
	fixture := buildPreviewPriorityTierFixture(t)

	savedPlatformID := fixture.platformID
	savedNodes, err := fixture.cp.PreviewPriorityTierNodes(PreviewPriorityTiersRequest{
		PlatformID: &savedPlatformID,
	})
	if err != nil {
		t.Fatalf("PreviewPriorityTierNodes saved: %v", err)
	}

	draftNodes, err := fixture.cp.PreviewPriorityTierNodes(PreviewPriorityTiersRequest{
		PlatformSpec: &PreviewPriorityTierPlatformSpec{
			PriorityTiers: []model.PlatformPriorityTier{
				{RegexFilters: []string{"residential"}},
				{RegexFilters: []string{"fast"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("PreviewPriorityTierNodes draft: %v", err)
	}

	if len(savedNodes) != 3 || len(draftNodes) != 3 {
		t.Fatalf("saved=%d draft=%d, want 3/3", len(savedNodes), len(draftNodes))
	}

	toTierMap := func(nodes []PriorityTierNodeSummary) map[string]string {
		result := make(map[string]string, len(nodes))
		for _, item := range nodes {
			result[item.NodeHash] = item.TierKey + ":" + item.TierKind
		}
		return result
	}

	savedTierMap := toTierMap(savedNodes)
	draftTierMap := toTierMap(draftNodes)
	if savedTierMap[fixture.residential] != "0:explicit" || draftTierMap[fixture.residential] != "0:explicit" {
		t.Fatalf("residential tier mismatch: saved=%q draft=%q", savedTierMap[fixture.residential], draftTierMap[fixture.residential])
	}
	if savedTierMap[fixture.fast] != "1:explicit" || draftTierMap[fixture.fast] != "1:explicit" {
		t.Fatalf("fast tier mismatch: saved=%q draft=%q", savedTierMap[fixture.fast], draftTierMap[fixture.fast])
	}
	if savedTierMap[fixture.fallback] != "fallback:fallback" || draftTierMap[fixture.fallback] != "fallback:fallback" {
		t.Fatalf("fallback tier mismatch: saved=%q draft=%q", savedTierMap[fixture.fallback], draftTierMap[fixture.fallback])
	}

	tierKey := "0"
	singleTierNodes, err := fixture.cp.PreviewPriorityTierNodes(PreviewPriorityTiersRequest{
		PlatformSpec: &PreviewPriorityTierPlatformSpec{
			PriorityTiers: []model.PlatformPriorityTier{
				{RegexFilters: []string{"residential"}},
				{RegexFilters: []string{"fast"}},
			},
		},
		TierKey: &tierKey,
	})
	if err != nil {
		t.Fatalf("PreviewPriorityTierNodes single tier: %v", err)
	}
	if len(singleTierNodes) != 1 || singleTierNodes[0].NodeHash != fixture.residential {
		t.Fatalf("single tier nodes = %+v, want %s", singleTierNodes, fixture.residential)
	}
}

func TestPreviewPriorityTierNodes_PlatformPoolWithoutExplicitTiers(t *testing.T) {
	fixture := buildPreviewPriorityTierFixture(t)

	nodes, err := fixture.cp.PreviewPriorityTierNodes(PreviewPriorityTiersRequest{
		PlatformSpec: &PreviewPriorityTierPlatformSpec{},
	})
	if err != nil {
		t.Fatalf("PreviewPriorityTierNodes platform_pool: %v", err)
	}
	if len(nodes) != 3 {
		t.Fatalf("platform_pool len = %d, want 3", len(nodes))
	}
	for _, item := range nodes {
		if item.TierKey != "platform_pool" || item.TierKind != "platform_pool" || item.TierIndex != -1 {
			t.Fatalf("platform_pool item = %+v", item)
		}
	}
}

func TestPreviewPriorityTierNodes_InvalidInput(t *testing.T) {
	fixture := buildPreviewPriorityTierFixture(t)

	invalidTierKey := "missing"
	if _, err := fixture.cp.PreviewPriorityTierNodes(PreviewPriorityTiersRequest{
		PlatformSpec: &PreviewPriorityTierPlatformSpec{
			PriorityTiers: []model.PlatformPriorityTier{
				{RegexFilters: []string{"residential"}},
			},
		},
		TierKey: &invalidTierKey,
	}); err == nil {
		t.Fatal("expected invalid tier_key error")
	} else if svcErr, ok := err.(*ServiceError); !ok || svcErr.Code != "INVALID_ARGUMENT" {
		t.Fatalf("invalid tier_key error = %T %v", err, err)
	}

	if _, err := fixture.cp.PreviewPriorityTierNodes(PreviewPriorityTiersRequest{
		PlatformSpec: &PreviewPriorityTierPlatformSpec{
			RegexFilters: []string{"(broken"},
		},
	}); err == nil {
		t.Fatal("expected invalid regex error")
	} else if svcErr, ok := err.(*ServiceError); !ok || svcErr.Code != "INVALID_ARGUMENT" {
		t.Fatalf("invalid regex error = %T %v", err, err)
	}

	if _, err := fixture.cp.PreviewPriorityTierNodes(PreviewPriorityTiersRequest{
		PlatformSpec: &PreviewPriorityTierPlatformSpec{
			RegionFilters: []string{"HK"},
		},
	}); err == nil {
		t.Fatal("expected invalid region error")
	} else if svcErr, ok := err.(*ServiceError); !ok || svcErr.Code != "INVALID_ARGUMENT" {
		t.Fatalf("invalid region error = %T %v", err, err)
	}

	if _, err := fixture.cp.PreviewPriorityTierNodes(PreviewPriorityTiersRequest{
		PlatformSpec: &PreviewPriorityTierPlatformSpec{
			PriorityTiers: []model.PlatformPriorityTier{{}},
		},
	}); err == nil {
		t.Fatal("expected invalid priority tier error")
	} else if svcErr, ok := err.(*ServiceError); !ok || svcErr.Code != "INVALID_ARGUMENT" {
		t.Fatalf("invalid priority tier error = %T %v", err, err)
	}
}
