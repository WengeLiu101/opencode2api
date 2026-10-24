package gateway

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	"opencode2api/internal/config"
	modelcatalog "opencode2api/internal/models"
)

const (
	// The probe target is the OpenCode origin itself, not a generic
	// reachability site: providers routinely block cloudflare.com on their
	// egress while still serving opencode fine, so a cloudflare probe
	// falsely retired exits that real traffic could use (measured 2026-10-22:
	// 1400 exits retired by the probe, 8/8 real requests 200 through them).
	// checkClaimedProxy counts any HTTP response as success, so a 404 from
	// the origin still proves the tunnel reaches it.
	proxyHealthCheckURL      = "https://opencode.ai/"
	proxyHealthCheckInterval = 15 * time.Minute
	proxyHealthCheckTimeout  = 10 * time.Second
)

// syncProxyResult updates proxy health from real traffic and reports whether
// it treated the outcome as a proxy-route failure; nil and #pool proxies are
// skipped and report false. Only timeouts and connection refusals mark a
// proxy unavailable: its keys are rebound elsewhere at once and a confirming
// health check starts in the background. Any upstream HTTP status (including
// one returned alongside a status error) proves the proxy route is reachable
// and restores a proxy evicted by an earlier transient failure. Errors without
// an HTTP response only start the background check, which never affects the
// return value.
func (g *Gateway) syncProxyResult(ctx context.Context, proxy *proxyTransport, status int, err error) bool {
	if proxy == nil || proxy.pool {
		return false
	}
	if status > 0 {
		wasHealthy := proxy.healthy.Swap(true)
		if !wasHealthy {
			g.restoreProxy(proxy)
		}
		return false
	}
	if isProxyFailure(err) {
		// Take the proxy out of selection immediately. The verification probe
		// below confirms the verdict asynchronously, but waiting for it means the
		// next requests keep spending a full timeout on a node we already know is
		// unreachable. A successful probe restores it (applyProxyHealthResult).
		proxy.healthy.Store(false)
		g.rebindFailedProxy(proxy)
		g.verifyProxyAfterError(ctx, proxy, "proxy transport error", status)
		return true
	}
	if err != nil {
		g.verifyProxyAfterError(ctx, proxy, "upstream request error", status)
		return false
	}
	return false
}

// verifyProxyAfterError starts a background health check of proxy, unless one
// is already in flight, and applies its result. source and status only label
// the log; the source distinguishes a missing HTTP response from another
// upstream request error.
func (g *Gateway) verifyProxyAfterError(ctx context.Context, proxy *proxyTransport, source string, status int) {
	if !proxy.checking.CompareAndSwap(false, true) {
		return
	}
	// The client request may finish or be canceled while the verification is
	// running. Keep its values but give the proxy check an independent timeout.
	checkCtx := context.WithoutCancel(ctx)
	go func() {
		result := g.transports.checkClaimedProxy(checkCtx, proxy, proxyHealthCheckURL, proxyHealthCheckTimeout)
		g.applyProxyHealthResult(result, source, status)
	}()
}

// rebindFailedProxy marks proxy unhealthy and moves the Zen and Go keys bound
// to it to other proxies (see rebindUnavailableProxy).
func (g *Gateway) rebindFailedProxy(proxy *proxyTransport) (zenMoved, goMoved int) {
	if proxy == nil {
		return 0, 0
	}
	wasHealthy := proxy.healthy.Swap(false)
	return g.rebindUnavailableProxy(proxy, wasHealthy)
}

// rebindUnavailableProxy moves the Zen and Go keys bound to proxy to other
// proxies and logs the eviction when the proxy was healthy before or any key
// moved. It does not touch the healthy flag, and anonymous nodes are never
// rebound.
func (g *Gateway) rebindUnavailableProxy(proxy *proxyTransport, wasHealthy bool) (zenMoved, goMoved int) {
	zenMoved = g.zenNodes.RebindProxy(proxy.index)
	goMoved = g.goNodes.RebindProxy(proxy.index)
	if wasHealthy || zenMoved+goMoved > 0 {
		g.logger.Warn("proxy became unavailable", "component", "proxy", "event", "proxy_unavailable", "proxy", config.RedactURL(proxy.name), "zen_keys_moved", zenMoved, "go_keys_moved", goMoved)
	}
	return zenMoved, goMoved
}

// restoreProxy moves keys back to a recovered proxy they were originally
// assigned to and clears the anonymous cooldowns bound to it. It does not set
// the healthy flag; callers do that first.
func (g *Gateway) restoreProxy(proxy *proxyTransport) (zenMoved, goMoved int) {
	if proxy == nil {
		return 0, 0
	}
	zenMoved = g.zenNodes.RestoreProxy(proxy.index)
	goMoved = g.goNodes.RestoreProxy(proxy.index)
	// The anonymous lane carries its own per-proxy cooldowns; key recovery
	// alone would leave it skipped after an outage-era failure.
	anonCleared := g.anonymous.RestoreProxy(proxy)
	if zenMoved+goMoved > 0 || anonCleared > 0 {
		g.logger.Info("proxy connectivity restored", "component", "proxy", "event", "proxy_restored", "proxy", config.RedactURL(proxy.name), "zen_keys_moved", zenMoved, "go_keys_moved", goMoved, "anonymous_cleared", anonCleared)
	}
	return zenMoved, goMoved
}

// SweepProgress is a snapshot of a running (or last finished) full sweep. The
// management UI polls it to show a progress bar and, once done, what the run
// actually changed.
type SweepProgress struct {
	Running    bool   `json:"running"`
	Source     string `json:"source"`
	StartedAt  string `json:"started_at,omitempty"`
	FinishedAt string `json:"finished_at,omitempty"`
	Total      int    `json:"total"`
	Done       int    `json:"done"`
	Passed     int    `json:"passed"`
	Failed     int    `json:"failed"`
	Retired    int    `json:"retired"`
	Restored   int    `json:"restored"`
	DurationMs int64  `json:"duration_ms"`
}

// SweepProgress returns the current or last finished sweep's progress.
func (g *Gateway) SweepProgress() SweepProgress {
	if g == nil {
		return SweepProgress{}
	}
	g.sweepMu.Lock()
	defer g.sweepMu.Unlock()
	return g.sweepState
}

func (g *Gateway) updateSweepState(mutate func(*SweepProgress)) {
	g.sweepMu.Lock()
	mutate(&g.sweepState)
	g.sweepMu.Unlock()
}

// CancelProxySweep stops a full sweep in flight. Reports whether one was running.
func (g *Gateway) CancelProxySweep() bool {
	if g == nil {
		return false
	}
	g.sweepMu.Lock()
	cancel := g.sweepCancel
	g.sweepMu.Unlock()
	if cancel == nil {
		return false
	}
	cancel()
	return true
}

// sweepChunkSize bounds how many probes are dispatched before progress is
// published, so the UI advances smoothly without a lock per probe.
const sweepChunkSize = 64

// proxySweepOnce probes every candidate once, applies the results, publishes
// progress and persists the pool state. includeRetired is set for explicit
// operator-triggered runs: a retired proxy is otherwise never probed again, so
// it could never come back on its own.
func (g *Gateway) proxySweepOnce(ctx context.Context, source string, includeRetired bool) {
	if g == nil || g.transports == nil {
		return
	}
	timeout := g.cfg.ProxyPool.HealthTimeout()
	if timeout <= 0 {
		timeout = proxyHealthCheckTimeout
	}
	candidates := g.transports.sweepCandidates(includeRetired)
	started := time.Now()
	g.updateSweepState(func(s *SweepProgress) {
		*s = SweepProgress{
			Running: true, Source: source, StartedAt: started.Format(time.RFC3339), Total: len(candidates),
		}
	})

	passed, failed, retired, restored := 0, 0, 0, 0
	for i := 0; i < len(candidates); i += sweepChunkSize {
		if ctx.Err() != nil {
			break
		}
		end := min(i+sweepChunkSize, len(candidates))
		results := g.transports.runProbes(ctx, candidates[i:end], proxyHealthCheckURL, timeout, g.cfg.ProxyPool.SweepConcurrency)
		for _, result := range results {
			wasRetired := result.proxy.retired.Load()
			g.applyProxyHealthResult(result, source, 0)
			if result.err == nil {
				passed++
				if wasRetired && g.transports.reviveRetired(result.proxy) {
					restored++
					g.logger.Info("retired proxy is reachable again",
						"component", "proxy", "event", "proxy_retired_revived", "proxy", config.RedactURL(result.proxy.name))
				}
			} else {
				failed++
			}
			if result.retired {
				retired++
			}
		}
		done := end
		g.updateSweepState(func(s *SweepProgress) {
			s.Done, s.Passed, s.Failed, s.Retired, s.Restored = done, passed, failed, retired, restored
			s.DurationMs = time.Since(started).Milliseconds()
		})
	}

	total, healthy := g.transports.healthCounts()
	if err := g.transports.SaveState(); err != nil {
		g.logger.Warn("proxy state save failed", "component", "proxy", "event", "proxy_state_save_failed", "error", err)
	}
	g.updateSweepState(func(s *SweepProgress) {
		s.Running = false
		s.FinishedAt = time.Now().Format(time.RFC3339)
		s.DurationMs = time.Since(started).Milliseconds()
	})
	g.logger.Info("proxy sweep complete", "component", "proxy", "event", "proxy_sweep",
		"source", source, "probed", len(candidates), "passed", passed, "failed", failed,
		"retired", retired, "restored", restored, "cancelled", ctx.Err() != nil,
		"duration_ms", time.Since(started).Milliseconds(), "pool_total", total, "pool_healthy", healthy)
	if total > 0 && healthy*5 < total {
		g.logger.Warn("proxy pool is mostly unavailable; free models will fall back or fail",
			"component", "proxy", "event", "proxy_pool_degraded", "pool_total", total, "pool_healthy", healthy)
	}
}

// TriggerProxySweep starts a full sweep in the background (manual action).
// Reports false when one is already running, so the caller can tell the
// operator instead of silently queueing a second pass over the whole pool.
func (g *Gateway) TriggerProxySweep() bool {
	if g == nil {
		return false
	}
	g.sweepMu.Lock()
	if g.sweepState.Running {
		g.sweepMu.Unlock()
		return false
	}
	ctx, cancel := context.WithCancel(context.Background())
	g.sweepCancel = cancel
	g.sweepMu.Unlock()
	go func() {
		defer func() {
			g.sweepMu.Lock()
			g.sweepCancel = nil
			g.sweepMu.Unlock()
			cancel()
		}()
		g.proxySweepOnce(ctx, "manual sweep", true)
	}()
	return true
}

// proxySweepBatchOnce probes one rolling batch and persists state only when
// something changed, so a metered proxy plan is not charged for idle writes.
func (g *Gateway) proxySweepBatchOnce(ctx context.Context, batch int) {
	timeout := g.cfg.ProxyPool.HealthTimeout()
	if timeout <= 0 {
		timeout = proxyHealthCheckTimeout
	}
	results := g.transports.SweepBatch(ctx, proxyHealthCheckURL, timeout, batch)
	changed := false
	for _, result := range results {
		g.applyProxyHealthResult(result, "rolling check", 0)
		if result.failed || result.retired {
			changed = true
		}
	}
	if changed {
		if err := g.transports.SaveState(); err != nil {
			g.logger.Warn("proxy state save failed", "component", "proxy", "event", "proxy_state_save_failed", "error", err)
		}
	}
}

// StartProxyHealthChecks starts the periodic proxy health checks.
func (g *Gateway) StartProxyHealthChecks(ctx context.Context) {
	if g.cfg.ProxyPool.HealthMode == "rolling" {
		tick := g.cfg.ProxyPool.HealthTick()
		batch := g.cfg.ProxyPool.HealthBatchSize
		go func() {
			// One batch soon after startup so latency begins populating, then a
			// small batch every tick — never a burst.
			select {
			case <-ctx.Done():
				return
			case <-time.After(15 * time.Second):
				g.proxySweepBatchOnce(ctx, batch)
			}
			ticker := time.NewTicker(tick)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					g.proxySweepBatchOnce(ctx, batch)
				}
			}
		}()
		return
	}
	timeout := g.cfg.ProxyPool.HealthTimeout()
	if timeout <= 0 {
		timeout = proxyHealthCheckTimeout
	}
	interval := g.cfg.ProxyPool.HealthInterval()
	if interval <= 0 {
		// Periodic full sweep disabled: keep the legacy behavior of only
		// re-checking proxies already marked unhealthy.
		go func() {
			ticker := time.NewTicker(proxyHealthCheckInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					for _, result := range g.transports.CheckHealth(ctx, proxyHealthCheckURL, timeout) {
						g.applyProxyHealthResult(result, "scheduled health check", 0)
					}
				}
			}
		}()
		return
	}
	go func() {
		// Populate latency and health soon after startup, then sweep on the
		// configured interval.
		select {
		case <-ctx.Done():
			return
		case <-time.After(20 * time.Second):
			g.proxySweepOnce(ctx, "startup sweep", false)
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				g.proxySweepOnce(ctx, "scheduled sweep", false)
			}
		}
	}()
}

func (g *Gateway) applyProxyHealthResult(result proxyHealthResult, source string, upstreamStatus int) {
	if result.err == nil {
		if !result.wasHealthy {
			g.restoreProxy(result.proxy)
		}
		g.logger.Debug("proxy health check passed", "component", "proxy", "event", "health_check_passed", "source", source, "upstream_status", upstreamStatus, "proxy", config.RedactURL(result.proxy.name))
		return
	}
	if !result.failed {
		g.logger.Debug("proxy health check was inconclusive", "component", "proxy", "event", "health_check_inconclusive", "source", source, "upstream_status", upstreamStatus, "proxy", config.RedactURL(result.proxy.name), "error", result.err)
		return
	}
	if g.transports.hasHealthy() {
		zenMoved, goMoved := g.rebindUnavailableProxy(result.proxy, result.wasHealthy)
		if result.wasHealthy || zenMoved+goMoved > 0 {
			g.logger.Warn("proxy health check failed", "component", "proxy", "event", "health_check_failed", "source", source, "upstream_status", upstreamStatus, "proxy", config.RedactURL(result.proxy.name), "zen_keys_moved", zenMoved, "go_keys_moved", goMoved, "error", result.err)
			return
		}
	}
	g.logger.Debug("proxy health check is still failing", "component", "proxy", "event", "health_check_still_failing", "source", source, "upstream_status", upstreamStatus, "proxy", config.RedactURL(result.proxy.name), "error", result.err)
}

// StartModelRefresh refreshes the model catalog at once and then every
// models.refresh_seconds until ctx is canceled. Each round concurrently lists
// the Zen models (through the anonymous lane when no Zen key can) and the Go
// models, and fetches OpenCode's protocol capabilities. The catalog is
// replaced only when at least one listing succeeded; the rotation groups are
// then resynced and the catalog cache saved.
func (g *Gateway) StartModelRefresh(ctx context.Context) {
	refresh := func() {
		var zen, goModels []string
		var capabilities modelcatalog.Capabilities
		var capabilitiesErr error
		var wg sync.WaitGroup
		wg.Add(3)
		go func() { defer wg.Done(); zen = g.refreshZen(ctx) }()
		go func() { defer wg.Done(); goModels = g.refreshTier(ctx, g.cfg.Upstream.Go, g.goNodes) }()
		go func() {
			defer wg.Done()
			capabilityCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			capabilities, capabilitiesErr = g.refreshProtocolCapabilities(capabilityCtx)
		}()
		wg.Wait()
		if ctx.Err() != nil {
			return
		}
		if capabilitiesErr != nil {
			g.logger.Warn("OpenCode capability catalog refresh failed", "component", "models", "event", "capability_refresh_failed", "error", capabilitiesErr)
		}
		if zen != nil || goModels != nil {
			g.catalog.ReplaceWithCapabilities(zen, goModels, capabilities.Protocols, capabilities.Unsupported, capabilities.Metadata)
			g.syncRotation()
			if ctx.Err() == nil {
				if err := g.catalog.SaveCache(); err != nil {
					g.logger.Warn("model catalog cache write failed", "component", "models", "event", "catalog_cache_write_failed", "error", err)
				}
			}
			g.logger.Info("model catalog refreshed", "component", "models", "event", "catalog_refreshed", "models", len(g.catalog.List()))
		}
	}
	go func() {
		refresh()
		ticker := time.NewTicker(time.Duration(g.cfg.Models.RefreshSeconds) * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				refresh()
			}
		}
	}()
}

// refreshProtocolCapabilities fetches OpenCode's capability catalog through
// each healthy proxy in turn until one succeeds. Only a gateway without any
// transport falls back to a direct client.
func (g *Gateway) refreshProtocolCapabilities(ctx context.Context) (modelcatalog.Capabilities, error) {
	if g.transports == nil || len(g.transports.items) == 0 {
		return modelcatalog.FetchCapabilities(ctx, &http.Client{Timeout: 30 * time.Second}, modelcatalog.CapabilitiesURL)
	}
	var lastErr error
	for _, proxy := range g.transports.items {
		if proxy == nil || !proxy.healthy.Load() {
			continue
		}
		capabilities, err := modelcatalog.FetchCapabilities(ctx, proxy.client, modelcatalog.CapabilitiesURL)
		if err == nil {
			return capabilities, nil
		}
		lastErr = err
	}
	if lastErr == nil {
		lastErr = errors.New("no healthy proxy available for OpenCode capability catalog")
	}
	return modelcatalog.Capabilities{}, lastErr
}

// refreshZen lists the Zen models with the Zen keys and, when that fails and
// the anonymous tier is enabled, with the public credential instead.
func (g *Gateway) refreshZen(ctx context.Context) []string {
	if models := g.refreshTier(ctx, g.cfg.Upstream.Zen, g.zenNodes); models != nil {
		return models
	}
	if !g.cfg.Anonymous {
		return nil
	}
	return g.refreshAnonymousTier(ctx, g.cfg.Upstream.Anonymous)
}

// refreshAnonymousTier lists models with the public credential, one anonymous
// node per attempt: one pass over the nodes, or up to retry.max_attempts
// attempts when #pool transports exist. base only labels the logs.
func (g *Gateway) refreshAnonymousTier(ctx context.Context, base string) []string {
	if !g.anonymous.AllowAttempt() {
		// The breaker is open, or its half-open probe is already running:
		// refreshing now would only add to the block. Keeping nil leaves the
		// previous catalog in place.
		g.logger.Debug("anonymous model catalog refresh skipped; circuit breaker is not closed", "component", "models", "event", "anonymous_refresh_paused", "upstream", config.RedactURL(base))
		return nil
	}
	defer g.anonymous.ReleaseProbe()
	cursor := g.anonymous.CursorFor("")
	limit := min(g.anonymous.Len(), g.maxAnonymousExits())
	if g.anonymous.HasPool() && g.cfg.Retry.MaxAttempts > limit {
		limit = g.cfg.Retry.MaxAttempts
	}
	for attempt := 1; attempt <= limit; attempt++ {
		node := cursor.Next()
		if node == nil {
			break
		}
		refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		var models []string
		var status int
		var err error
		// List models on the configured v2 /inference origin first, then fall
		// back to the v1 origin (see anonymousUpstreams).
		for _, upstream := range g.anonymousUpstreams() {
			models, status, err = modelcatalog.FetchModels(refreshCtx, node.proxy.client, upstream, anonymousZenKey)
			if err == nil || isProxyFailure(err) {
				break
			}
		}
		g.syncProxyResult(refreshCtx, node.proxy, status, err)
		cancel()
		if err == nil {
			g.anonymous.MarkSuccess(node)
			g.anonymous.NoteSuccess()
			return models
		}
		g.anonymous.MarkFailure(node, nil, err)
		g.logger.Debug("anonymous model catalog refresh attempt failed", "component", "models", "event", "anonymous_refresh_attempt_failed", "upstream", config.RedactURL(base), "attempt", attempt, "proxy", config.RedactURL(node.proxy.name), "error", err)
	}
	g.logger.Warn("anonymous model catalog refresh failed", "component", "models", "event", "anonymous_refresh_failed", "upstream", config.RedactURL(base))
	return nil
}

// refreshTier lists the models at base with up to retry.max_attempts keys
// from nodes, updating proxy health and key cooldowns after every attempt. It
// returns nil when the pool has no keys or every attempt fails.
func (g *Gateway) refreshTier(ctx context.Context, base string, nodes *upstreamNodes) []string {
	cursor := nodes.Cursor()
	for attempt := 0; attempt < g.cfg.Retry.MaxAttempts; attempt++ {
		node := cursor.Next()
		if node == nil {
			return nil
		}
		refreshCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		proxy := nodes.Proxy(node)
		if proxy == nil {
			cancel()
			return nil
		}
		models, status, err := modelcatalog.FetchModels(refreshCtx, proxy.client, base, node.key)
		g.syncProxyResult(refreshCtx, proxy, status, err)
		cancel()
		if err == nil {
			nodes.MarkSuccess(node)
			return models
		}
		nodes.MarkFailure(node, nil, err)
		g.logger.Debug("model catalog refresh attempt failed", "component", "models", "event", "refresh_attempt_failed", "upstream", config.RedactURL(base), "attempt", attempt+1, "error", err)
	}
	g.logger.Warn("model catalog refresh failed", "component", "models", "event", "refresh_failed", "upstream", config.RedactURL(base))
	return nil
}

// StartProxyExpiryChecks runs the second stage of the retirement: excluded
// (retired) primary entries are periodically probed for recovery, and entries
// that stay excluded too long, or fail too many revival probes, are
// permanently deleted from the proxy file. Fallback and direct entries are
// never purged.
func (g *Gateway) StartProxyExpiryChecks(ctx context.Context) {
	interval := time.Duration(g.cfg.ProxyPool.ReviveIntervalSeconds) * time.Second
	if interval <= 0 {
		return
	}
	go func() {
		// Wait one interval before the first round so the rolling health
		// checks populate the pool first.
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				g.reviveExcluded(ctx)
			}
		}
	}()
}

// reviveExcluded probes a bounded batch of excluded entries and either
// restores the ones that recovered or moves them toward permanent deletion.
func (g *Gateway) reviveExcluded(ctx context.Context) {
	if g == nil || g.transports == nil {
		return
	}
	excluded := g.transports.excludedEntries()
	if len(excluded) == 0 {
		return
	}
	const batch = 32
	start := int(g.transports.reviveCursor.Add(1)-1) % len(excluded)
	picks := make([]*proxyTransport, 0, batch)
	for i := 0; i < batch && i < len(excluded); i++ {
		picks = append(picks, excluded[(start+i)%len(excluded)])
	}
	timeout := g.cfg.ProxyPool.HealthTimeout()
	if timeout <= 0 {
		timeout = proxyHealthCheckTimeout
	}
	concurrency := min(8, len(picks))
	for _, result := range g.transports.runProbes(ctx, picks, proxyHealthCheckURL, timeout, concurrency) {
		proxy := result.proxy
		if proxy == nil || proxy.purged.Load() {
			continue
		}
		if result.err == nil {
			// The probe reaches the origin where real traffic once failed:
			// restore the entry and reset its exclusion clock.
			if g.transports.reviveRetired(proxy) {
				g.logger.Info("excluded proxy recovered; restored to selection",
					"component", "proxy", "event", "proxy_restored",
					"proxy", config.RedactURL(proxy.name), "latency_ms", proxy.latencyMs.Load())
			}
			continue
		}
		fails := proxy.reviveFails.Add(1)
		var age time.Duration
		if at := proxy.retiredAt.Load(); at > 0 {
			age = time.Since(time.Unix(0, at))
		}
		poolCfg := g.cfg.ProxyPool
		byProbes := poolCfg.ReviveProbes > 0 && int(fails) >= poolCfg.ReviveProbes
		byAge := poolCfg.PurgeAfterMinutes > 0 && age >= time.Duration(poolCfg.PurgeAfterMinutes)*time.Minute
		if !byProbes && !byAge {
			continue
		}
		if g.transports.purgeProxy(proxy) {
			reason := "revival_probes_exhausted"
			if byAge {
				reason = "exclusion_window_expired"
			}
			g.logger.Warn("excluded proxy purged from the pool file",
				"component", "proxy", "event", "proxy_purged",
				"proxy", config.RedactURL(proxy.name), "reason", reason,
				"revive_fails", fails, "excluded_for", age.Round(time.Second).String())
		}
	}
}
