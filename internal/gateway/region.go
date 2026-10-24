package gateway

import "strings"

// regionBlockedCountries is the set of exit regions that OpenCode's
// region-gated free models refuse. It mirrors the published exclusion list and
// always includes the local machine (China) so a gated model never egresses
// locally when a foreign proxy is available.
var regionBlockedCountries = map[string]bool{
	"LOCAL": true, "CN": true, "HK": true, "MO": true,
	"RU": true, "IR": true, "KP": true, "CU": true, "SY": true,
	"BY": true, "MM": true, "LA": true, "IQ": true, "LY": true,
	"NG": true, "VE": true, "KH": true, "TM": true, "UZ": true,
	"GQ": true, "CD": true,
}

// regionLockedByName reports whether a model is served by a region-gated
// provider, detected from its name. Models that hit a region error at runtime
// are additionally learned (see markRegionLocked), so this list need not be
// exhaustive.
func regionLockedByName(model string) bool {
	m := strings.ToLower(model)
	return strings.Contains(m, "cat-jam") || strings.Contains(m, "contributor-free")
}

// regionBlockFor returns the set of exit regions to avoid for a model, or nil
// when the model is not region-gated.
func (g *Gateway) regionBlockFor(model string) map[string]bool {
	if model == "" {
		return nil
	}
	if regionLockedByName(model) || g.isRegionLocked(model) {
		return regionBlockedCountries
	}
	return nil
}

// markRegionLocked records a model that returned a region error on the
// anonymous lane, so later requests steer it away from restricted regions.
func (g *Gateway) markRegionLocked(model string) {
	if model == "" {
		return
	}
	g.regionMu.Lock()
	if g.regionLocked == nil {
		g.regionLocked = make(map[string]struct{})
	}
	if _, seen := g.regionLocked[model]; !seen {
		g.regionLocked[model] = struct{}{}
		g.logger.Info("model learned as region-gated", "component", "upstream", "event", "region_gated_model", "model", model)
	}
	g.regionMu.Unlock()
}

func (g *Gateway) isRegionLocked(model string) bool {
	g.regionMu.Lock()
	defer g.regionMu.Unlock()
	_, ok := g.regionLocked[model]
	return ok
}
