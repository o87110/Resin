package platform

import (
	"net/netip"
	"regexp"
	"sync"

	"github.com/Resinat/Resin/internal/node"
)

// DefaultPlatformID is the well-known UUID of the built-in Default platform.
const DefaultPlatformID = "00000000-0000-0000-0000-000000000000"

// DefaultPlatformName is the built-in platform name.
const DefaultPlatformName = "Default"

// GeoLookupFunc resolves an IP address to a lowercase ISO country code.
type GeoLookupFunc func(netip.Addr) string

// PoolRangeFunc iterates all nodes in the global pool.
type PoolRangeFunc func(fn func(node.Hash, *node.NodeEntry) bool)

// GetEntryFunc retrieves a node entry from the global pool by hash.
type GetEntryFunc func(node.Hash) (*node.NodeEntry, bool)

// Platform represents a routing platform with its filtered routable view.
type Platform struct {
	ID   string
	Name string

	// Filter configuration.
	RegexFilters        []*regexp.Regexp
	ExcludeRegexFilters []*regexp.Regexp
	RegionFilters       []string // lowercase ISO codes, supports negation "!xx"
	PriorityTiers       []*PriorityTier

	// Other config fields.
	StickyTTLNs                      int64
	ReverseProxyMissAction           string
	ReverseProxyEmptyAccountBehavior string
	ReverseProxyFixedAccountHeader   string
	ReverseProxyFixedAccountHeaders  []string
	AllocationPolicy                 AllocationPolicy

	// Routable view & its lock.
	// viewMu serializes both FullRebuild and NotifyDirty.
	view         *RoutableView
	fallbackView *RoutableView
	viewMu       sync.Mutex
}

// NewPlatform creates a Platform with an empty routable view.
func NewPlatform(id, name string, regexFilters []*regexp.Regexp, regionFilters []string) *Platform {
	return NewPlatformWithExclude(id, name, regexFilters, nil, regionFilters)
}

// NewPlatformWithExclude creates a Platform with include/exclude regex filters.
func NewPlatformWithExclude(
	id, name string,
	regexFilters []*regexp.Regexp,
	excludeRegexFilters []*regexp.Regexp,
	regionFilters []string,
) *Platform {
	return &Platform{
		ID:                  id,
		Name:                name,
		RegexFilters:        regexFilters,
		ExcludeRegexFilters: excludeRegexFilters,
		RegionFilters:       regionFilters,
		view:                NewRoutableView(),
		fallbackView:        NewRoutableView(),
	}
}

// View returns the platform's routable view as a read-only interface.
// External callers cannot Add/Remove/Clear — only FullRebuild and NotifyDirty can mutate.
func (p *Platform) View() ReadOnlyView {
	return p.view
}

// RoutingView returns the highest-priority non-empty routing view.
// When no explicit priority tiers are configured, it falls back to the full platform view.
func (p *Platform) RoutingView() ReadOnlyView {
	if len(p.PriorityTiers) == 0 {
		return p.view
	}
	for _, tier := range p.PriorityTiers {
		if tier != nil && tier.view.Size() > 0 {
			return tier.view
		}
	}
	if p.fallbackView.Size() > 0 {
		return p.fallbackView
	}
	return p.view
}

// FullRebuild clears the routable view and re-evaluates all nodes from the pool.
// Acquires viewMu — any concurrent NotifyDirty calls block until rebuild completes.
func (p *Platform) FullRebuild(
	poolRange PoolRangeFunc,
	subLookup node.SubLookupFunc,
	geoLookup GeoLookupFunc,
) {
	p.viewMu.Lock()
	defer p.viewMu.Unlock()

	p.view.Clear()
	p.clearPriorityViews()
	poolRange(func(h node.Hash, entry *node.NodeEntry) bool {
		if p.evaluateNode(entry, subLookup, geoLookup) {
			p.assignPriorityView(h, entry, subLookup, geoLookup)
			p.view.Add(h)
		}
		return true
	})
}

// NotifyDirty re-evaluates a single node and adds/removes it from the view.
// Acquires viewMu — serialized with FullRebuild.
func (p *Platform) NotifyDirty(
	h node.Hash,
	getEntry GetEntryFunc,
	subLookup node.SubLookupFunc,
	geoLookup GeoLookupFunc,
) {
	p.viewMu.Lock()
	defer p.viewMu.Unlock()

	entry, ok := getEntry(h)
	if !ok {
		// Node was deleted from pool.
		p.removePriorityViews(h)
		p.view.Remove(h)
		return
	}

	if p.evaluateNode(entry, subLookup, geoLookup) {
		p.assignPriorityView(h, entry, subLookup, geoLookup)
		p.view.Add(h)
	} else {
		p.removePriorityViews(h)
		p.view.Remove(h)
	}
}

// evaluateNode checks all filter conditions for platform routability.
func (p *Platform) evaluateNode(
	entry *node.NodeEntry,
	subLookup node.SubLookupFunc,
	geoLookup GeoLookupFunc,
) bool {
	// 0. Disabled nodes are never routable.
	if entry.IsDisabledBySubscriptions(subLookup) {
		return false
	}

	// 1. Healthy for routing (outbound ready + circuit not open).
	if !entry.IsHealthy() {
		return false
	}

	// 2. Tag include/exclude regex match.
	if !entry.MatchTagFilters(p.RegexFilters, p.ExcludeRegexFilters, subLookup) {
		return false
	}

	// 3. Egress IP must be known.
	egressIP := entry.GetEgressIP()
	if !egressIP.IsValid() {
		return false
	}

	// 4. Region filter (when configured).
	if len(p.RegionFilters) > 0 {
		region := entry.GetRegion(geoLookup)
		if !MatchRegionFilter(region, p.RegionFilters) {
			return false
		}
	}

	// 5. Has at least one latency record.
	if !entry.HasLatency() {
		return false
	}

	return true
}

// MatchRegionFilter applies include/exclude region filters.
// Positive entries (xx) build an include set; negative entries (!xx) build an exclude set.
// Unknown regions never match when region filters are configured.
// Final result is: region known AND (include empty OR region in include) AND (region not in exclude).
func MatchRegionFilter(region string, filters []string) bool {
	if len(filters) == 0 {
		return true
	}
	if region == "" {
		return false
	}

	included := false
	hasInclude := false

	for _, filter := range filters {
		if len(filter) > 0 && filter[0] == '!' {
			if region == filter[1:] {
				return false
			}
			continue
		}
		hasInclude = true
		if region == filter {
			included = true
		}
	}

	if hasInclude && !included {
		return false
	}
	return true
}

func (p *Platform) clearPriorityViews() {
	if p.fallbackView != nil {
		p.fallbackView.Clear()
	}
	for _, tier := range p.PriorityTiers {
		if tier != nil && tier.view != nil {
			tier.view.Clear()
		}
	}
}

func (p *Platform) removePriorityViews(h node.Hash) {
	if p.fallbackView != nil {
		p.fallbackView.Remove(h)
	}
	for _, tier := range p.PriorityTiers {
		if tier != nil && tier.view != nil {
			tier.view.Remove(h)
		}
	}
}

func (p *Platform) assignPriorityView(
	h node.Hash,
	entry *node.NodeEntry,
	subLookup node.SubLookupFunc,
	geoLookup GeoLookupFunc,
) {
	if len(p.PriorityTiers) == 0 {
		return
	}

	p.removePriorityViews(h)
	for _, tier := range p.PriorityTiers {
		if tier != nil && tier.matches(entry, subLookup, geoLookup) {
			tier.view.Add(h)
			return
		}
	}
	p.fallbackView.Add(h)
}
