package platform

import (
	"fmt"
	"regexp"
	"strconv"

	"github.com/Resinat/Resin/internal/model"
	"github.com/Resinat/Resin/internal/node"
)

// MaxPriorityTiers limits the number of explicit priority layers per platform.
const MaxPriorityTiers = 8

const (
	PriorityTierViewKindExplicit     = "explicit"
	PriorityTierViewKindFallback     = "fallback"
	PriorityTierViewKindPlatformPool = "platform_pool"

	PriorityTierViewKeyFallback     = "fallback"
	PriorityTierViewKeyPlatformPool = "platform_pool"
)

// TierViewDescriptor describes one read-only tier-backed node view.
type TierViewDescriptor struct {
	Key   string
	Kind  string
	Index int
	View  ReadOnlyView
}

// PriorityTier holds one compiled priority layer and its routable view.
type PriorityTier struct {
	RegexFilters        []*regexp.Regexp
	ExcludeRegexFilters []*regexp.Regexp
	RegionFilters       []string
	view                *RoutableView
}

func newPriorityTier(
	regexFilters []*regexp.Regexp,
	excludeRegexFilters []*regexp.Regexp,
	regionFilters []string,
) *PriorityTier {
	return &PriorityTier{
		RegexFilters:        append([]*regexp.Regexp(nil), regexFilters...),
		ExcludeRegexFilters: append([]*regexp.Regexp(nil), excludeRegexFilters...),
		RegionFilters:       append([]string(nil), regionFilters...),
		view:                NewRoutableView(),
	}
}

func clonePriorityTiers(tiers []*PriorityTier) []*PriorityTier {
	if len(tiers) == 0 {
		return nil
	}
	cloned := make([]*PriorityTier, 0, len(tiers))
	for _, tier := range tiers {
		if tier == nil {
			continue
		}
		cloned = append(cloned, newPriorityTier(tier.RegexFilters, tier.ExcludeRegexFilters, tier.RegionFilters))
	}
	return cloned
}

func (t *PriorityTier) matches(entry *node.NodeEntry, subLookup node.SubLookupFunc, geoLookup GeoLookupFunc) bool {
	if t == nil {
		return false
	}
	if !entry.MatchTagFilters(t.RegexFilters, t.ExcludeRegexFilters, subLookup) {
		return false
	}
	if len(t.RegionFilters) == 0 {
		return true
	}
	region := entry.GetRegion(geoLookup)
	return MatchRegionFilter(region, t.RegionFilters)
}

func normalizePriorityTierModel(tier model.PlatformPriorityTier) model.PlatformPriorityTier {
	tier.RegexFilters = append([]string{}, tier.RegexFilters...)
	tier.ExcludeRegexFilters = append([]string{}, tier.ExcludeRegexFilters...)
	tier.RegionFilters = append([]string{}, tier.RegionFilters...)
	return tier
}

// NormalizePriorityTiers clones priority tier models and normalizes nil slices to empty slices.
func NormalizePriorityTiers(tiers []model.PlatformPriorityTier) []model.PlatformPriorityTier {
	if len(tiers) == 0 {
		return []model.PlatformPriorityTier{}
	}
	normalized := make([]model.PlatformPriorityTier, 0, len(tiers))
	for _, tier := range tiers {
		normalized = append(normalized, normalizePriorityTierModel(tier))
	}
	return normalized
}

// ValidatePriorityTiers validates tier count, non-empty rules, regexes, and region filters.
func ValidatePriorityTiers(tiers []model.PlatformPriorityTier) error {
	_, err := CompilePriorityTiers(tiers)
	return err
}

// CompilePriorityTiers compiles priority tier filters into runtime tiers.
func CompilePriorityTiers(tiers []model.PlatformPriorityTier) ([]*PriorityTier, error) {
	if len(tiers) == 0 {
		return nil, nil
	}
	if len(tiers) > MaxPriorityTiers {
		return nil, fmt.Errorf("priority_tiers: must contain at most %d items", MaxPriorityTiers)
	}

	compiled := make([]*PriorityTier, 0, len(tiers))
	for i, rawTier := range tiers {
		tier := normalizePriorityTierModel(rawTier)
		if len(tier.RegexFilters) == 0 && len(tier.ExcludeRegexFilters) == 0 && len(tier.RegionFilters) == 0 {
			return nil, fmt.Errorf(
				"priority_tiers[%d]: at least one regex_filters, exclude_regex_filters, or region_filters entry is required",
				i,
			)
		}

		regexFilters, err := compileRegexFilters(fmt.Sprintf("priority_tiers[%d].regex_filters", i), tier.RegexFilters)
		if err != nil {
			return nil, err
		}
		excludeRegexFilters, err := compileRegexFilters(
			fmt.Sprintf("priority_tiers[%d].exclude_regex_filters", i),
			tier.ExcludeRegexFilters,
		)
		if err != nil {
			return nil, err
		}
		if err := validateTierRegionFilters(i, tier.RegionFilters); err != nil {
			return nil, err
		}

		compiled = append(compiled, newPriorityTier(regexFilters, excludeRegexFilters, tier.RegionFilters))
	}
	return compiled, nil
}

func validateTierRegionFilters(tierIndex int, regionFilters []string) error {
	for i, r := range regionFilters {
		code := r
		if len(r) > 0 && r[0] == '!' {
			code = r[1:]
		}
		if !isLowerAlpha2(code) {
			return fmt.Errorf(
				"priority_tiers[%d].region_filters[%d]: must be a 2-letter lowercase ISO 3166-1 alpha-2 code (e.g. us, jp) or negation (e.g. !hk)",
				tierIndex,
				i,
			)
		}
	}
	return nil
}

// TierViewDescriptors returns the runtime node-group views for the platform.
// When the platform has no explicit priority tiers, it exposes a single
// platform_pool descriptor backed by the full platform view.
func (p *Platform) TierViewDescriptors() []TierViewDescriptor {
	if p == nil {
		return nil
	}
	if len(p.PriorityTiers) == 0 {
		return []TierViewDescriptor{{
			Key:   PriorityTierViewKeyPlatformPool,
			Kind:  PriorityTierViewKindPlatformPool,
			Index: -1,
			View:  p.view,
		}}
	}

	descriptors := make([]TierViewDescriptor, 0, len(p.PriorityTiers)+1)
	for i, tier := range p.PriorityTiers {
		if tier == nil {
			continue
		}
		descriptors = append(descriptors, TierViewDescriptor{
			Key:   strconv.Itoa(i),
			Kind:  PriorityTierViewKindExplicit,
			Index: i,
			View:  tier.view,
		})
	}
	descriptors = append(descriptors, TierViewDescriptor{
		Key:   PriorityTierViewKeyFallback,
		Kind:  PriorityTierViewKindFallback,
		Index: -1,
		View:  p.fallbackView,
	})
	return descriptors
}

// TierViewDescriptorByKey resolves one view descriptor by stable tier key.
func (p *Platform) TierViewDescriptorByKey(key string) (TierViewDescriptor, bool) {
	for _, descriptor := range p.TierViewDescriptors() {
		if descriptor.Key == key {
			return descriptor, true
		}
	}
	return TierViewDescriptor{}, false
}
