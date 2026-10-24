package gateway

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	rand "math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/phuslu/iploc"

	"opencode2api/internal/config"
	"opencode2api/internal/httpx"
)

type proxyTransport struct {
	index int
	name  string
	// pool marks a rotating proxy-pool entry (#pool): every attempt opens a fresh
	// tunnel instead of reusing a keep-alive connection.
	pool bool
	// client is the regular (Go TLS) client; bun mimics the official OpenCode
	// client's Bun runtime and is used for free-model traffic.
	client   *http.Client
	bun      *http.Client
	healthy  atomic.Bool
	checking atomic.Bool
	// blockedUntil holds the free tier's IP-abuse block for this exit (unix
	// nanoseconds). It is a property of the egress IP, so a response from the
	// same exit never clears it: only another exit can serve traffic meanwhile.
	blockedUntil atomic.Int64
	// latencyMs is the last successful probe's round-trip in milliseconds; -1
	// until first measured. failures counts consecutive failed probes (reset on
	// success) and drives retirement. retired permanently removes the proxy.
	latencyMs atomic.Int64
	failures  atomic.Uint32
	// rejections counts consecutive *real* requests the upstream refused for
	// this exit (a bare 400/403 while the proxy itself is perfectly reachable).
	// The health probe only measures reachability, so without this the pool can
	// never converge onto the exits upstream actually serves. Any real 2xx resets
	// it, so a good exit never accumulates.
	rejections atomic.Uint32
	retired    atomic.Bool
	lastProbe  atomic.Int64
	// region is the proxy host's ISO-3166 country code resolved locally via
	// embedded GeoIP data ("", for a hostname we cannot resolve, or "LOCAL" for
	// the direct entry). Used to steer region-locked models away from
	// restricted exit regions.
	region string
	// fallback marks a lower-priority proxy (config ProxyPool.FallbackProxyFile).
	// The anonymous lane exhausts every primary proxy before using one, and
	// fallback proxies are never retired — a bridge that is merely stopped must
	// not permanently drop its nodes.
	fallback bool
	// direct marks the local-egress transport. key_proxy_mode="direct" pins the
	// keyed lanes to it, and the anonymous lane must skip it: that lane exists to
	// leave the local egress behind (the free tier rate-limits and region-gates
	// by exit IP), so trying direct there wastes an attempt per request.
	direct bool
	// retiredAt is when this entry entered the short-term exclusion (unix
	// nanoseconds). Excluded entries are revival-probed; after reviveFails
	// failed probes or purge_after_minutes of exclusion the entry is deleted
	// from the proxy file for good.
	retiredAt   atomic.Int64
	reviveFails atomic.Uint32
	// purged marks an entry permanently deleted from the pool file. It stays
	// in memory (items is read concurrently by cursors) but is never selected
	// and is hidden from the stats.
	purged atomic.Bool
}

// blocked reports whether this exit is under the free tier's IP abuse block.
func (t *proxyTransport) blocked(now time.Time) bool {
	if t == nil {
		return false
	}
	return t.blockedUntil.Load() > now.UnixNano()
}

// markBlocked keeps this exit out of the anonymous cursor until until. The
// block is a property of the egress IP, so it is never cleared by a response
// from the same exit: only another exit can serve traffic in the meantime.
func (t *proxyTransport) markBlocked(until time.Time) {
	if t == nil {
		return
	}
	t.blockedUntil.Store(until.UnixNano())
}

// proxyRegion classifies a proxy entry by the country of its host IP using
// embedded GeoIP data (no network lookup). "direct" is the local machine.
func proxyRegion(raw string) string {
	if raw == "direct" {
		return "LOCAL"
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return ""
	}
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		if code := iploc.Country(ip); code != "" {
			return strings.ToUpper(code)
		}
	}
	return ""
}

type proxyTransports struct {
	items        []*proxyTransport
	poolCfg      config.ProxyPoolConfig
	retireAfter  int
	disableAfter int
	tiered       bool
	fastMs       int64
	midMs        int64
	// directIndex is the index of the "direct" transport (local egress), or -1
	// when the pool has no direct entry. Key traffic in "direct" mode pins here.
	directIndex int
	// statePath, when set, persists per-proxy latency/failures/retired across
	// restarts (keyed by proxy name).
	statePath string
	// poolPath, when set, is the primary proxy file this pool was loaded
	// from: the import endpoint rewrites it and the purge stage removes
	// permanently deleted entries from it. fileMu serializes its rewrites.
	poolPath     string
	fileMu       sync.Mutex
	purgedCount  atomic.Int32
	reviveCursor atomic.Uint64
	// rollCursor advances the rolling health-check batch across the pool.
	rollCursor atomic.Uint64
}

// tierOf classifies a proxy by its last measured latency: 0 fast, 1 mid,
// 2 slow, 3 unknown (never probed). Selection prefers lower tiers.
func (p *proxyTransports) tierOf(proxy *proxyTransport) int {
	if proxy == nil {
		return 3
	}
	ms := proxy.latencyMs.Load()
	if ms < 0 {
		return 3
	}
	if ms <= p.fastMs {
		return 0
	}
	if ms <= p.midMs {
		return 1
	}
	return 2
}

// anonymousNodes gives the shared OpenCode "public" credential an independent
// cooldown per proxy. Unlike key nodes, anonymous nodes are never rebound:
// changing proxy is the failover mechanism because Zen rate-limits them by IP.
type anonymousNodes struct {
	nodes        []*anonymousNode
	next         atomic.Uint64
	cooldown     time.Duration
	gateCooldown time.Duration
	transports   *proxyTransports
	// hasPool reports whether any node uses a rotating proxy pool (#pool), which
	// lets callers make more attempts than there are nodes.
	hasPool bool
	// breaker stops the whole lane after repeated failures or when every
	// exit is under the free tier's IP block: the lane then pauses for
	// backoff.base, doubling every further trip (one strike per trip) up to
	// backoff.max. A success closes the breaker. probing marks the single
	// half-open attempt allowed after the window expires.
	backoff        anonymousBackoff
	breakerUntil   atomic.Int64
	breakerStrikes atomic.Int32
	consecutive    atomic.Int32
	probing        atomic.Bool
}

// anonymousBackoff is the circuit breaker's schedule: trip after threshold
// consecutive failures, then pause base, doubling per trip up to max.
type anonymousBackoff struct {
	threshold int32
	base      time.Duration
	max       time.Duration
}

// HasPool reports whether any node uses a rotating proxy pool (#pool).
func (p *anonymousNodes) HasPool() bool {
	return p != nil && p.hasPool
}

type anonymousNode struct {
	proxy         *proxyTransport
	failures      atomic.Uint32
	cooldownUntil atomic.Int64
}

type anonymousCursor struct {
	pool   *anonymousNodes
	start  int
	offset int
	// block lists exit regions to skip entirely (region-gated models).
	block map[string]bool
	// wantFallback selects which tier this cursor walks: false = primary
	// proxies, true = fallback proxies. A cursor never mixes the two, so the
	// caller can exhaust the primary tier before touching the fallback tier.
	wantFallback bool
	// plan holds this request's walk order (built once, lazily) and planIdx is
	// the position within it.
	plan    []*anonymousNode
	planIdx int
}

// anonymousPool builds one anonymous node per proxy transport, or an empty
// pool when the anonymous tier is disabled.
func anonymousPool(enabled bool, transports *proxyTransports, cooldown time.Duration, backoff anonymousBackoff) *anonymousNodes {
	pool := &anonymousNodes{cooldown: cooldown, backoff: backoff, transports: transports}
	if !enabled || transports == nil {
		return pool
	}
	pool.nodes = make([]*anonymousNode, 0, len(transports.items))
	for _, proxy := range transports.items {
		if proxy.direct {
			continue
		}
		if proxy.pool {
			pool.hasPool = true
		}
		pool.nodes = append(pool.nodes, &anonymousNode{proxy: proxy})
	}
	return pool
}

func (p *anonymousNodes) Len() int {
	if p == nil {
		return 0
	}
	return len(p.nodes)
}

func (p *anonymousNodes) CursorFor(affinity string) anonymousCursor {
	return p.CursorForRegion(affinity, nil)
}

// blockedExits reports whether every configured exit is currently under the
// free tier's IP block, so no exit can serve the request.
func (p *anonymousNodes) blockedExits(now time.Time) bool {
	if p == nil || len(p.nodes) == 0 {
		return false
	}
	for _, node := range p.nodes {
		if node.proxy != nil && !node.proxy.blocked(now) {
			return false
		}
	}
	return true
}

// CursorForRegion is CursorFor with a set of exit regions to skip entirely,
// used to keep region-gated models off restricted-region proxies.
func (p *anonymousNodes) CursorForRegion(affinity string, block map[string]bool) anonymousCursor {
	return p.CursorForRegionTier(affinity, block, false)
}

// CursorForRegionTier is CursorForRegion restricted to one tier: the primary
// proxies (wantFallback=false) or the fallback proxies (wantFallback=true).
func (p *anonymousNodes) CursorForRegionTier(affinity string, block map[string]bool, wantFallback bool) anonymousCursor {
	if p == nil || len(p.nodes) == 0 {
		return anonymousCursor{pool: p, block: block, wantFallback: wantFallback}
	}
	start := 0
	if affinity == "" {
		start = int((p.next.Add(1) - 1) % uint64(len(p.nodes)))
	} else {
		hash := fnv.New64a()
		_, _ = hash.Write([]byte(affinity))
		start = int(hash.Sum64() % uint64(len(p.nodes)))
	}
	return anonymousCursor{pool: p, start: start, block: block, wantFallback: wantFallback}
}

// LenTier counts selectable-and-not nodes in one tier, used to bound how many
// attempts a request spends there.
func (p *anonymousNodes) LenTier(wantFallback bool) int {
	if p == nil {
		return 0
	}
	count := 0
	for _, node := range p.nodes {
		if node.proxy.fallback == wantFallback {
			count++
		}
	}
	return count
}

// AllowAttempt reports whether an anonymous attempt may run. An open breaker
// refuses every attempt. Once its window expires, exactly one caller wins the
// half-open probe, so the lane is not flooded before it is known to work
// again; every winner must release the probe through NoteSuccess, NoteFailure,
// NoteNeutral or ReleaseProbe.
func (p *anonymousNodes) AllowAttempt() bool {
	if p == nil || p.backoff.threshold < 1 {
		return true
	}
	until := p.breakerUntil.Load()
	if until == 0 {
		return true
	}
	if time.Now().UnixNano() < until {
		return false
	}
	return p.probing.CompareAndSwap(false, true)
}

// Open reports whether the breaker currently holds the lane shut. A half-open
// breaker whose window has expired reports false: the lane is being probed.
func (p *anonymousNodes) Open() bool {
	if p == nil {
		return false
	}
	until := p.breakerUntil.Load()
	return until != 0 && time.Now().UnixNano() < until
}

// ReleaseProbe returns an unused half-open probe slot. It is safe to call
// after any outcome has already closed or reopened the breaker.
func (p *anonymousNodes) ReleaseProbe() {
	if p == nil {
		return
	}
	p.probing.Store(false)
}

// NoteSuccess closes the breaker and forgets the strikes.
func (p *anonymousNodes) NoteSuccess() {
	if p == nil {
		return
	}
	p.consecutive.Store(0)
	p.breakerStrikes.Store(0)
	p.breakerUntil.Store(0)
	p.probing.Store(false)
}

// NoteNeutral releases a half-open probe and closes the breaker without
// counting a strike. It is used when the upstream answered the lane but
// rejected the request itself (for example a context-length 400): the lane is
// reachable, so it should not stay paused, and the request is not evidence of
// an upstream outage.
func (p *anonymousNodes) NoteNeutral() {
	if p == nil {
		return
	}
	p.consecutive.Store(0)
	p.breakerUntil.Store(0)
	p.probing.Store(false)
}

// NoteFailure records one failed attempt. It reports the pause window when the
// consecutive-failure threshold trips the breaker. A failure during the
// half-open probe reopens the breaker immediately with the next, longer window
// instead of waiting for another threshold's worth of traffic.
func (p *anonymousNodes) NoteFailure() (time.Duration, bool) {
	if p == nil || p.backoff.threshold < 1 {
		return 0, false
	}
	now := time.Now().UnixNano()
	until := p.breakerUntil.Load()
	if p.probing.Load() || (until != 0 && now >= until) {
		return p.trip()
	}
	if p.consecutive.Add(1) < p.backoff.threshold {
		return 0, false
	}
	return p.trip()
}

// ForceOpen trips the breaker immediately. The anonymous walk calls it when
// every exit is under the free tier's IP block: another request cannot reach a
// working exit, and probing would only extend the penalty.
func (p *anonymousNodes) ForceOpen() (time.Duration, bool) {
	if p == nil || p.backoff.threshold < 1 {
		return 0, false
	}
	return p.trip()
}

// trip opens the breaker for the next backoff window and returns it.
func (p *anonymousNodes) trip() (time.Duration, bool) {
	p.consecutive.Store(0)
	strike := p.breakerStrikes.Add(1)
	pause := p.backoff.base << min(strike-1, 30)
	if pause > p.backoff.max || pause <= 0 {
		pause = p.backoff.max
	}
	p.breakerUntil.Store(time.Now().Add(pause).UnixNano())
	p.probing.Store(false)
	return pause, true
}

// Next returns the next usable node in this cursor's tier, each node at most
// once. The first call builds the walk order: when latency tiering is enabled
// the fastest tier comes first, so slower exits are only reached after the
// faster ones failed. Usability is re-checked at use time because health,
// cooldown and the free tier's IP block all change while a request runs.
func (c *anonymousCursor) Next() *anonymousNode {
	if c.pool == nil || len(c.pool.nodes) == 0 {
		return nil
	}
	if c.plan == nil {
		c.plan = c.buildPlan()
	}
	now := time.Now().UnixNano()
	for c.planIdx < len(c.plan) {
		node := c.plan[c.planIdx]
		c.planIdx++
		if c.usable(node, now) {
			return node
		}
	}
	return nil
}

// buildPlan orders this tier's nodes once per request. Tiering is a preference,
// not an exclusive filter: every node stays in the plan, fastest tier first.
func (c *anonymousCursor) buildPlan() []*anonymousNode {
	n := len(c.pool.nodes)
	plan := make([]*anonymousNode, 0, n)
	for i := 0; i < n; i++ {
		node := c.pool.nodes[(c.start+i)%n]
		if node.proxy.fallback != c.wantFallback {
			continue
		}
		plan = append(plan, node)
	}
	tp := c.pool.transports
	if tp != nil && tp.tiered {
		sort.SliceStable(plan, func(i, j int) bool {
			return tp.tierOf(plan[i].proxy) < tp.tierOf(plan[j].proxy)
		})
	}
	return plan
}

// usable reports whether a node may serve this request right now.
func (c *anonymousCursor) usable(node *anonymousNode, now int64) bool {
	if !node.proxy.healthy.Load() || node.proxy.retired.Load() {
		return false
	}
	// An exit the free tier flagged is out until its block expires: the answer
	// would be the same on every lane.
	if node.proxy.blockedUntil.Load() > now {
		return false
	}
	if node.cooldownUntil.Load() > now {
		return false
	}
	return c.block == nil || !c.block[node.proxy.region]
}

func (p *anonymousNodes) MarkSuccess(node *anonymousNode) {
	if node == nil {
		return
	}
	node.failures.Store(0)
	node.cooldownUntil.Store(0)
}

// RestoreProxy clears cooldowns for anonymous nodes bound to a recovered
// proxy, mirroring upstreamNodes.RestoreProxy for key nodes. Without this, an
// outage-era cooldown survives the proxy's recovery and the anonymous lane
// stays skipped even though the route is healthy again.
func (p *anonymousNodes) RestoreProxy(proxy *proxyTransport) int {
	if p == nil || proxy == nil {
		return 0
	}
	cleared := 0
	for _, node := range p.nodes {
		if node.proxy != proxy {
			continue
		}
		if node.cooldownUntil.Load() == 0 && node.failures.Load() == 0 {
			continue
		}
		node.failures.Store(0)
		node.cooldownUntil.Store(0)
		cleared++
	}
	return cleared
}

// maxAnonymousRetryAfterMultiplier bounds an upstream Retry-After on the
// anonymous lane to the same 8x ceiling as its exponential backoff. The lane
// is rate limited per exit IP and, unlike key lanes, has no earliest-expiry
// fallback: a quota response asking for hours would keep the exit skipped long
// after a network change gave it a fresh allowance. Key lanes keep honoring
// Retry-After in full, since their cursor already falls back to the node that
// recovers first.
const maxAnonymousRetryAfterMultiplier = 8

func (p *anonymousNodes) MarkFailure(node *anonymousNode, resp *http.Response, err error) {
	if node == nil {
		return
	}
	if err == nil && resp != nil && resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
		return
	}
	failures := node.failures.Add(1)
	delay := p.cooldown * time.Duration(1<<min(failures-1, 3))
	if resp != nil {
		if retryAfter := parseRetryAfter(resp.Header.Get("Retry-After")); retryAfter > delay {
			delay = min(retryAfter, p.cooldown*maxAnonymousRetryAfterMultiplier)
		}
		// A 403 gate refusal ("free tier ... from within OpenCode") is per exit
		// IP and persists; park the egress for longer so a mostly-flagged pool
		// converges instead of saturating its short cooldowns.
		if resp.StatusCode == http.StatusForbidden && p.gateCooldown > delay {
			delay = p.gateCooldown
		}
	}
	node.cooldownUntil.Store(time.Now().Add(delay).UnixNano())
}

func (p *proxyTransports) hasHealthy() bool {
	for _, proxy := range p.items {
		if proxy.healthy.Load() && !proxy.retired.Load() {
			return true
		}
	}
	return false
}

func (p *proxyTransports) healthCounts() (total, healthy int) {
	if p == nil {
		return 0, 0
	}
	for _, proxy := range p.items {
		if proxy.retired.Load() || proxy.purged.Load() {
			continue
		}
		total++
		if proxy.healthy.Load() {
			healthy++
		}
	}
	return total, healthy
}

// ProxyStat is a read-only snapshot of one proxy, used by the management UI.
type ProxyStat struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Region  string `json:"region"`
	Tier    int    `json:"tier"`
	Healthy bool   `json:"healthy"`
	Retired bool   `json:"retired"`
	// Fallback marks a lower-priority entry (ProxyPool.FallbackProxyFile).
	Fallback  bool   `json:"fallback"`
	LatencyMs int64  `json:"latency_ms"`
	Failures  uint32 `json:"failures"`
	// Rejections is the consecutive real-request refusal streak: the upstream
	// answered with a bare 400/403 for this exit while the proxy itself is fine.
	Rejections uint32 `json:"rejections"`
	LastProbe  string `json:"last_probe,omitempty"`
}

// Stats returns a snapshot of every proxy for diagnostics.
func (p *proxyTransports) Stats() []ProxyStat {
	if p == nil {
		return nil
	}
	out := make([]ProxyStat, 0, len(p.items))
	for _, proxy := range p.items {
		if proxy.purged.Load() {
			continue
		}
		item := ProxyStat{
			ID:         config.Fingerprint(proxy.name),
			Name:       config.RedactURL(proxy.name),
			Region:     proxy.region,
			Tier:       p.tierOf(proxy),
			Healthy:    proxy.healthy.Load(),
			Retired:    proxy.retired.Load(),
			Fallback:   proxy.fallback,
			LatencyMs:  proxy.latencyMs.Load(),
			Failures:   proxy.failures.Load(),
			Rejections: proxy.rejections.Load(),
		}
		if probe := proxy.lastProbe.Load(); probe > 0 {
			item.LastProbe = time.Unix(0, probe).Format(time.RFC3339)
		}
		out = append(out, item)
	}
	return out
}

func transportPool(proxies, fallbackProxies []string, cfg config.PerformanceConfig, poolCfg config.ProxyPoolConfig, responseHeaderTimeout time.Duration) (*proxyTransports, error) {
	p := &proxyTransports{
		items:        make([]*proxyTransport, 0, len(proxies)+len(fallbackProxies)),
		poolCfg:      poolCfg,
		retireAfter:  poolCfg.RetireAfterFailures,
		disableAfter: poolCfg.DisableAfterFailures,
		tiered:       poolCfg.Tiered,
		fastMs:       int64(poolCfg.FastLatencyMs),
		midMs:        int64(poolCfg.MidLatencyMs),
		directIndex:  -1,
	}
	// Primary proxies come first, then the lower-priority fallback tier.
	for _, raw := range proxies {
		if err := p.addProxy(raw, cfg, responseHeaderTimeout, false); err != nil {
			return nil, err
		}
	}
	for _, raw := range fallbackProxies {
		if err := p.addProxy(raw, cfg, responseHeaderTimeout, true); err != nil {
			return nil, err
		}
	}
	return p, nil
}

// addProxy builds the transport for one proxy entry and appends it to the pool.
// fallback marks a lower-priority entry (see proxyTransport.fallback).
func (p *proxyTransports) addProxy(raw string, cfg config.PerformanceConfig, responseHeaderTimeout time.Duration, fallback bool) error {
	clean, isPool := config.ParseProxyEntry(raw)
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = cfg.MaxIdleConns
	transport.MaxIdleConnsPerHost = cfg.MaxIdleConnsPerHost
	transport.MaxConnsPerHost = cfg.MaxConnsPerHost
	transport.IdleConnTimeout = time.Duration(cfg.IdleConnTimeoutSeconds) * time.Second
	transport.ResponseHeaderTimeout = responseHeaderTimeout
	transport.ForceAttemptHTTP2 = !isPool
	transport.DisableKeepAlives = isPool
	if isPool {
		// Cloning DefaultTransport after its first dial inherits the already
		// configured HTTP/2 ALPN and TLSNextProto map, which ignores
		// DisableKeepAlives and multiplexes every request over one CONNECT
		// tunnel. An explicit empty TLSNextProto map and HTTP/1.1 ALPN force a
		// brand-new TCP + CONNECT tunnel per attempt.
		transport.TLSNextProto = make(map[string]func(authority string, c *tls.Conn) http.RoundTripper)
		if transport.TLSClientConfig == nil {
			transport.TLSClientConfig = &tls.Config{NextProtos: []string{"http/1.1"}}
		} else {
			tlsCfg := transport.TLSClientConfig.Clone()
			tlsCfg.NextProtos = []string{"http/1.1"}
			transport.TLSClientConfig = tlsCfg
		}
	}
	transport.DialContext = (&net.Dialer{
		Timeout:   time.Duration(cfg.ConnectTimeoutSeconds) * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext
	var proxyURL *url.URL
	if clean == "direct" {
		transport.Proxy = nil
	} else {
		u, err := url.Parse(clean)
		if err != nil {
			return fmt.Errorf("parse proxy %s: %w", config.RedactURL(raw), err)
		}
		transport.Proxy = http.ProxyURL(u)
		proxyURL = u
	}
	bun, err := httpx.NewBunTransport(httpx.BunOptions{
		Proxy:                 proxyURL,
		ConnectTimeout:        time.Duration(cfg.ConnectTimeoutSeconds) * time.Second,
		ResponseHeaderTimeout: responseHeaderTimeout,
		IdleConnTimeout:       transport.IdleConnTimeout,
		MaxIdleConnsPerHost:   cfg.MaxIdleConnsPerHost,
		DisableKeepAlives:     isPool,
	})
	if err != nil {
		return fmt.Errorf("proxy %s: %w", config.RedactURL(raw), err)
	}
	proxy := &proxyTransport{
		index: len(p.items), name: raw, pool: isPool,
		client:   &http.Client{Transport: transport},
		bun:      &http.Client{Transport: bun},
		region:   proxyRegion(raw),
		fallback: fallback,
		direct:   clean == "direct",
	}
	proxy.latencyMs.Store(-1)
	proxy.healthy.Store(true)
	p.items = append(p.items, proxy)
	if proxy.direct && p.directIndex < 0 {
		p.directIndex = proxy.index
	}
	return nil
}

type proxyHealthResult struct {
	proxy      *proxyTransport
	err        error
	failed     bool
	wasHealthy bool
	retired    bool
}

// CheckHealth concurrently rechecks only proxies already marked unhealthy.
// Healthy proxies are skipped before a check is claimed. Any HTTP response
// from the test URL proves that the route is reachable; only a timeout or
// connection refusal keeps the proxy unhealthy.
func (p *proxyTransports) CheckHealth(ctx context.Context, target string, timeout time.Duration) []proxyHealthResult {
	results := make(chan proxyHealthResult, len(p.items))
	checks := 0
	for _, proxy := range p.items {
		if proxy.retired.Load() || proxy.healthy.Load() || !proxy.checking.CompareAndSwap(false, true) {
			continue
		}
		// A real request may have restored the proxy between the first health
		// read and claiming this check.
		if proxy.healthy.Load() {
			proxy.checking.Store(false)
			continue
		}
		checks++
		go func() {
			results <- p.checkClaimedProxy(ctx, proxy, target, timeout)
		}()
	}
	out := make([]proxyHealthResult, 0, checks)
	for range checks {
		out = append(out, <-results)
	}
	return out
}

// sweepCandidates returns the proxies a sweep should probe. includeRetired is
// set for explicit operator-triggered sweeps: retired proxies are otherwise
// never probed again, so one could never come back on its own.
func (p *proxyTransports) sweepCandidates(includeRetired bool) []*proxyTransport {
	if p == nil {
		return nil
	}
	out := make([]*proxyTransport, 0, len(p.items))
	for _, proxy := range p.items {
		if proxy.retired.Load() && !includeRetired {
			continue
		}
		out = append(out, proxy)
	}
	return out
}

// reviveRetired clears the retired flag after a successful probe, so a proxy
// the pool gave up on rejoins selection once it is reachable again. Retiring is
// a heuristic, not a verdict — a node that was out of quota this month is fine
// the next.
// markRetired puts an entry into the short-term exclusion, recording when the
// exclusion began so the second stage can purge it later.
func (p *proxyTransports) markRetired(proxy *proxyTransport) {
	if p == nil || proxy == nil {
		return
	}
	if proxy.retired.CompareAndSwap(false, true) {
		proxy.retiredAt.Store(time.Now().UnixNano())
		proxy.reviveFails.Store(0)
	}
	proxy.healthy.Store(false)
}

func (p *proxyTransports) reviveRetired(proxy *proxyTransport) bool {
	if p == nil || proxy == nil || !proxy.retired.Load() {
		return false
	}
	proxy.retired.Store(false)
	proxy.retiredAt.Store(0)
	proxy.reviveFails.Store(0)
	proxy.failures.Store(0)
	proxy.rejections.Store(0)
	proxy.healthy.Store(true)
	return true
}

// isExitRefusal reports whether an upstream response means "this exit IP is not
// acceptable" rather than "this request is broken". The free tier answers both
// with a bare status: 400 for an exit it will not serve, 403 for one its gate
// refuses. 429 is deliberately excluded: that is per-exit rate limiting, which
// the cooldown already handles and which switching exits does not fix.
func isExitRefusal(status int) bool {
	return status == http.StatusBadRequest || status == http.StatusForbidden
}

// rejectExit records that a real request through this proxy was refused by the
// upstream, and retires the proxy once that has happened `threshold` times in a
// row. Returns true when this call retired it. Fallback (bridge) entries are
// exempt, matching the rule that they are never retired automatically.
func (p *proxyTransports) rejectExit(proxy *proxyTransport, threshold int) bool {
	if p == nil || proxy == nil || proxy.fallback || threshold <= 0 {
		return false
	}
	n := proxy.rejections.Add(1)
	if proxy.retired.Load() || n < uint32(threshold) {
		return false
	}
	p.markRetired(proxy)
	return true
}

// acceptExit clears the refusal streak: the upstream served this exit, so any
// earlier refusals were not about the exit itself.
func (p *proxyTransports) acceptExit(proxy *proxyTransport) {
	if proxy != nil {
		proxy.rejections.Store(0)
	}
}

// SweepAll probes every non-retired proxy once, bounded by the pool's
// configured concurrency. Unlike CheckHealth it also re-probes proxies that are
// currently healthy, so latency stays fresh and proxies that have silently died
// are detected and eventually retired.
func (p *proxyTransports) SweepAll(ctx context.Context, target string, timeout time.Duration) []proxyHealthResult {
	return p.runProbes(ctx, p.sweepCandidates(false), target, timeout, 0)
}

// SweepBatch probes the next `batch` non-retired proxies in a rotating order.
// Spreading checks over time keeps the egress/upstream request rate low instead
// of one large burst (important for metered proxy plans).
func (p *proxyTransports) SweepBatch(ctx context.Context, target string, timeout time.Duration, batch int) []proxyHealthResult {
	if p == nil || len(p.items) == 0 || batch < 1 {
		return nil
	}
	total := len(p.items)
	start := int(p.rollCursor.Add(uint64(batch))-uint64(batch)) % total
	candidates := make([]*proxyTransport, 0, batch)
	for i := 0; i < total && len(candidates) < batch; i++ {
		proxy := p.items[(start+i)%total]
		if proxy.retired.Load() {
			continue
		}
		candidates = append(candidates, proxy)
	}
	return p.runProbes(ctx, candidates, target, timeout, 0)
}

// runProbes probes the given proxies. concurrency 0 uses the pool's configured
// HealthConcurrency; an explicit value lets an operator-triggered sweep run
// faster than the gentle rolling cadence.
func (p *proxyTransports) runProbes(ctx context.Context, candidates []*proxyTransport, target string, timeout time.Duration, concurrency int) []proxyHealthResult {
	if concurrency < 1 {
		concurrency = p.poolCfg.HealthConcurrency
	}
	if concurrency < 1 {
		concurrency = 1
	}
	results := make([]proxyHealthResult, len(candidates))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for i, proxy := range candidates {
		wg.Add(1)
		go func(i int, proxy *proxyTransport) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			if !proxy.checking.CompareAndSwap(false, true) {
				return // another check is already in flight for this proxy
			}
			results[i] = p.checkClaimedProxy(ctx, proxy, target, timeout)
		}(i, proxy)
	}
	wg.Wait()
	out := make([]proxyHealthResult, 0, len(candidates))
	for _, r := range results {
		if r.proxy != nil {
			out = append(out, r)
		}
	}
	return out
}

// checkClaimedProxy performs a check after the caller has acquired checking.
func (p *proxyTransports) checkClaimedProxy(ctx context.Context, proxy *proxyTransport, target string, timeout time.Duration) proxyHealthResult {
	defer proxy.checking.Store(false)
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	start := time.Now()
	req, err := http.NewRequestWithContext(checkCtx, http.MethodGet, target, nil)
	if err == nil {
		req.Header.Set("User-Agent", httpx.V2UserAgent())
		// Probe with the Bun-disguised client, the same one free-model traffic
		// uses. The origin sits behind Cloudflare bot management, which resets
		// non-browser TLS handshakes (plain Go TLS), so probing with
		// proxy.client failed for *every* exit - healthy ones included - and
		// the rolling check retired 1400 exits that real traffic was serving
		// (2026-10-22: probe said 0/1401 healthy, real requests 200 through
		// the same pool).
		probeClient := proxy.client
		if proxy.bun != nil {
			probeClient = proxy.bun
		}
		resp, requestErr := probeClient.Do(req)
		err = requestErr
		if resp != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			_ = resp.Body.Close()
		}
	}
	proxy.lastProbe.Store(time.Now().UnixNano())
	result := proxyHealthResult{proxy: proxy, err: err, wasHealthy: proxy.healthy.Load()}
	if err == nil {
		proxy.latencyMs.Store(time.Since(start).Milliseconds())
		proxy.failures.Store(0)
		result.wasHealthy = proxy.healthy.Swap(true)
	} else if !errors.Is(err, context.Canceled) {
		// A probe is a plain HTTP GET issued *through* the proxy. If it did not
		// return a response, this proxy cannot currently serve traffic — the
		// CONNECT was refused, the tunnel broke, the connection was reset, or it
		// timed out. Every such outcome must count as a failure.
		//
		// isProxyFailure() only recognized timeouts and ECONNREFUSED, so a proxy
		// whose CONNECT is rejected for any other reason (a provider refusing
		// with its own error, e.g. an exhausted-bandwidth response) was reported
		// as "inconclusive". It then stayed healthy forever: never taken out of
		// rotation, never re-bound for key traffic. Only cancellation is exempt,
		// because that says the *caller* gave up, not that the proxy is down.
		result.failed = true
		failures := proxy.failures.Add(1)
		// Fallback entries (e.g. bridge ports for paid nodes) are never retired:
		// the bridge being stopped is a temporary condition, and retiring would
		// permanently drop nodes that are fine once it is back. They still get
		// disabled below, so a genuinely dead node leaves selection until a probe
		// succeeds again.
		if p.retireAfter > 0 && !proxy.fallback && failures >= uint32(p.retireAfter) {
			p.markRetired(proxy)
			result.retired = true
		} else {
			threshold := p.disableAfter
			if threshold < 1 {
				threshold = 1
			}
			if failures >= uint32(threshold) {
				result.wasHealthy = proxy.healthy.Swap(false)
			}
		}
	}
	return result
}

// isProxyFailure deliberately recognizes only failures that say the proxy
// route is unavailable. HTTP responses and unrelated transport/protocol errors
// must not evict a proxy.
func isProxyFailure(err error) bool {
	if errors.Is(err, errFirstEventTimeout) || upstreamAccepted(err) {
		return false // The upstream accepted HTTP; slow generation is not a broken proxy.
	}
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var timeout interface{ Timeout() bool }
	return errors.As(err, &timeout) && timeout.Timeout()
}

// SetRetired manually retires (or restores) one proxy by its fingerprint id.
// Retired proxies are skipped by selection and sweeps; restoring clears the
// failure counter so the next sweep re-evaluates it. Returns the count changed.
func (p *proxyTransports) SetRetired(id string, retired bool) int {
	if p == nil || id == "" {
		return 0
	}
	changed := 0
	for _, proxy := range p.items {
		if config.Fingerprint(proxy.name) != id {
			continue
		}
		if retired {
			p.markRetired(proxy)
		} else {
			proxy.retired.Store(false)
			proxy.failures.Store(0)
			proxy.rejections.Store(0)
		}
		changed++
	}
	return changed
}

// SetRetiredMany retires or restores every proxy whose fingerprint id is in
// ids — the bulk form behind the pool UI's per-group actions. Returns changed.
func (p *proxyTransports) SetRetiredMany(ids map[string]struct{}, retired bool) int {
	if p == nil || len(ids) == 0 {
		return 0
	}
	changed := 0
	for _, proxy := range p.items {
		if _, ok := ids[config.Fingerprint(proxy.name)]; !ok {
			continue
		}
		if retired {
			p.markRetired(proxy)
		} else {
			proxy.retired.Store(false)
			proxy.failures.Store(0)
			proxy.rejections.Store(0)
		}
		changed++
	}
	return changed
}

// CheckIDs probes every non-retired proxy whose fingerprint id is in ids so the
// UI can re-test one group on demand. Returns one result per probed proxy.
func (p *proxyTransports) CheckIDs(ctx context.Context, ids map[string]struct{}, target string, timeout time.Duration) []proxyHealthResult {
	if p == nil || len(ids) == 0 {
		return nil
	}
	candidates := make([]*proxyTransport, 0, len(ids))
	for _, proxy := range p.items {
		// Retired proxies are included on purpose: a targeted check is how an
		// operator brings one back, and the caller decides what to ask for.
		if _, ok := ids[config.Fingerprint(proxy.name)]; ok {
			candidates = append(candidates, proxy)
		}
	}
	return p.runProbes(ctx, candidates, target, timeout, 0)
}

// ClearRetired restores every retired proxy and resets failure counters.
func (p *proxyTransports) ClearRetired() int {
	if p == nil {
		return 0
	}
	changed := 0
	for _, proxy := range p.items {
		if proxy.retired.Load() {
			changed++
		}
		proxy.retired.Store(false)
		proxy.failures.Store(0)
		proxy.rejections.Store(0)
	}
	return changed
}

// RetireProxy retires or restores one proxy at the gateway level and persists.
func (g *Gateway) RetireProxy(id string, retired bool) int {
	if g == nil || g.transports == nil {
		return 0
	}
	n := g.transports.SetRetired(id, retired)
	if n > 0 {
		_ = g.transports.SaveState()
	}
	return n
}

// RetireProxies retires or restores a set of proxies by fingerprint id and
// persists the new state (bulk form of RetireProxy).
func (g *Gateway) RetireProxies(ids map[string]struct{}, retired bool) int {
	if g == nil || g.transports == nil {
		return 0
	}
	changed := g.transports.SetRetiredMany(ids, retired)
	if changed > 0 {
		_ = g.transports.SaveState()
	}
	return changed
}

// CheckProxiesNow probes the given proxies immediately and persists the
// outcome. It backs the per-group "check now" action; returns the probe count.
func (g *Gateway) CheckProxiesNow(ctx context.Context, ids map[string]struct{}) int {
	if g == nil || g.transports == nil {
		return 0
	}
	timeout := g.cfg.ProxyPool.HealthTimeout()
	if timeout <= 0 {
		timeout = proxyHealthCheckTimeout
	}
	results := g.transports.CheckIDs(ctx, ids, proxyHealthCheckURL, timeout)
	for _, result := range results {
		g.applyProxyHealthResult(result, "manual group check", 0)
	}
	if len(results) > 0 {
		_ = g.transports.SaveState()
	}
	return len(results)
}

// ClearRetiredProxies restores all retired proxies and persists.
func (g *Gateway) ClearRetiredProxies() int {
	if g == nil || g.transports == nil {
		return 0
	}
	n := g.transports.ClearRetired()
	_ = g.transports.SaveState()
	return n
}

// ProxyPoolSummary aggregates the pool for the management UI.
type ProxyPoolSummary struct {
	Total     int `json:"total"`
	Healthy   int `json:"healthy"`
	Unhealthy int `json:"unhealthy"`
	Retired   int `json:"retired"`
	// Purged is how many entries this process permanently deleted from the
	// pool file (the second stage of the two-stage retirement).
	Purged  int            `json:"purged"`
	Fast    int            `json:"fast"`
	Mid     int            `json:"mid"`
	Slow    int            `json:"slow"`
	Unknown int            `json:"unknown"`
	Tiered  bool           `json:"tiered"`
	Regions map[string]int `json:"regions"`
}

// ProxyStats exposes the proxy-pool snapshot plus a summary for the UI.
func (g *Gateway) ProxyStats() ([]ProxyStat, ProxyPoolSummary) {
	if g == nil || g.transports == nil {
		return nil, ProxyPoolSummary{}
	}
	stats := g.transports.Stats()
	sum := ProxyPoolSummary{Total: len(stats), Tiered: g.transports.tiered, Regions: map[string]int{}}
	for _, s := range stats {
		if s.Retired {
			sum.Retired++
			continue
		}
		if s.Healthy {
			sum.Healthy++
		} else {
			sum.Unhealthy++
		}
		switch s.Tier {
		case 0:
			sum.Fast++
		case 1:
			sum.Mid++
		case 2:
			sum.Slow++
		default:
			sum.Unknown++
		}
		if s.Region != "" {
			sum.Regions[s.Region]++
		}
	}
	sum.Purged = int(g.transports.purgedCount.Load())
	return stats, sum
}

// proxyPersist is the on-disk state for one proxy, keyed by its raw name.
type proxyPersist struct {
	LatencyMs int64  `json:"latency_ms"`
	Failures  uint32 `json:"failures"`
	// Rejections is the consecutive real-request refusal streak (see
	// proxyTransport.rejections). Persisted so a restart does not throw away the
	// evidence that a large pool is converging onto its usable exits.
	Rejections uint32 `json:"rejections,omitempty"`
	Retired    bool   `json:"retired"`
	// RetiredAt / ReviveFails persist the two-stage retirement so a restart
	// does not restart the exclusion clock or refund spent revival probes.
	RetiredAt   int64  `json:"retired_at,omitempty"`
	ReviveFails uint32 `json:"revive_fails,omitempty"`
	LastProbe   int64  `json:"last_probe,omitempty"`
}

type poolStateFile struct {
	Proxies map[string]proxyPersist `json:"proxies"`
}

// SetStatePath enables persistence of per-proxy state to path.
func (p *proxyTransports) SetStatePath(path string) {
	if p != nil {
		p.statePath = path
	}
}

// LoadState restores per-proxy latency, failures and retirement from disk.
// Entries for proxies no longer in the pool are ignored; a missing file is not
// an error. Delete the state file to un-retire every proxy.
// SetPoolPath marks path as the primary proxy file this pool was loaded
// from. Only then can the import endpoint rewrite it, and the purge stage
// delete permanently excluded entries from it.
func (p *proxyTransports) SetPoolPath(path string) {
	if p == nil {
		return
	}
	p.fileMu.Lock()
	defer p.fileMu.Unlock()
	p.poolPath = path
}

// SavePoolLines rewrites the primary proxy file with lines, keeping a
// one-generation backup at poolPath + ".import.bak".
func (p *proxyTransports) SavePoolLines(lines []string) error {
	if p == nil {
		return errors.New("no proxy pool")
	}
	p.fileMu.Lock()
	defer p.fileMu.Unlock()
	if p.poolPath == "" {
		return errors.New("no proxyfile is configured; add proxyfile to the config first")
	}
	if data, err := os.ReadFile(p.poolPath); err == nil {
		_ = os.WriteFile(p.poolPath+".import.bak", data, 0o600)
	}
	out := make([]byte, 0, 64*len(lines))
	for _, line := range lines {
		out = append(out, line...)
		out = append(out, '\n')
	}
	tmp := p.poolPath + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p.poolPath)
}

// RemovePoolLines deletes the raw entries named in gone from the primary
// proxy file and reports how many lines it removed. A missing file, or names
// that are no longer present, are not an error.
func (p *proxyTransports) RemovePoolLines(gone map[string]struct{}) int {
	if p == nil || p.poolPath == "" || len(gone) == 0 {
		return 0
	}
	p.fileMu.Lock()
	defer p.fileMu.Unlock()
	data, err := os.ReadFile(p.poolPath)
	if err != nil {
		return 0
	}
	var kept []string
	removed := 0
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if _, ok := gone[trimmed]; ok {
			removed++
			continue
		}
		kept = append(kept, trimmed)
	}
	if removed == 0 {
		return 0
	}
	out := make([]byte, 0, 64*len(kept))
	for _, line := range kept {
		out = append(out, line...)
		out = append(out, '\n')
	}
	tmp := p.poolPath + ".tmp"
	if err := os.WriteFile(tmp, out, 0o600); err != nil {
		return 0
	}
	if err := os.Rename(tmp, p.poolPath); err != nil {
		return 0
	}
	return removed
}

// purgeProxy permanently deletes an excluded entry: it disappears from the
// proxy file and, through SaveState (which skips purged entries), from the
// state file. The in-memory entry stays because items is read concurrently,
// but it is never selected again and is hidden from the stats.
func (p *proxyTransports) purgeProxy(proxy *proxyTransport) bool {
	if p == nil || proxy == nil || proxy.fallback || proxy.direct {
		return false
	}
	if !proxy.purged.CompareAndSwap(false, true) {
		return false
	}
	p.purgedCount.Add(1)
	p.RemovePoolLines(map[string]struct{}{proxy.name: {}})
	_ = p.SaveState()
	return true
}

// excludedEntries lists the entries in the short-term exclusion (retired, not
// yet purged, not fallback or direct), oldest exclusion first.
func (p *proxyTransports) excludedEntries() []*proxyTransport {
	if p == nil {
		return nil
	}
	out := make([]*proxyTransport, 0, 16)
	for _, proxy := range p.items {
		if proxy.retired.Load() && !proxy.purged.Load() && !proxy.fallback && !proxy.direct {
			out = append(out, proxy)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].retiredAt.Load() < out[j].retiredAt.Load()
	})
	return out
}

func (p *proxyTransports) LoadState() error {
	if p == nil || p.statePath == "" {
		return nil
	}
	data, err := os.ReadFile(p.statePath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var st poolStateFile
	if err := json.Unmarshal(data, &st); err != nil {
		return err
	}
	for _, proxy := range p.items {
		saved, ok := st.Proxies[proxy.name]
		if !ok {
			continue
		}
		if saved.LatencyMs > 0 {
			proxy.latencyMs.Store(saved.LatencyMs)
		}
		if saved.Failures > 0 {
			proxy.failures.Store(saved.Failures)
		}
		if saved.Rejections > 0 {
			proxy.rejections.Store(saved.Rejections)
		}
		if saved.LastProbe > 0 {
			proxy.lastProbe.Store(saved.LastProbe)
		}
		if saved.Retired {
			proxy.retired.Store(true)
			proxy.healthy.Store(false)
			if saved.RetiredAt > 0 {
				proxy.retiredAt.Store(saved.RetiredAt)
			} else {
				// An exclusion restored from an older state file without a
				// timestamp starts its clock now rather than being purged at
				// once.
				proxy.retiredAt.Store(time.Now().UnixNano())
			}
			proxy.reviveFails.Store(saved.ReviveFails)
		}
	}
	return nil
}

// SaveState writes per-proxy state atomically (temp file + rename).
func (p *proxyTransports) SaveState() error {
	if p == nil || p.statePath == "" {
		return nil
	}
	st := poolStateFile{Proxies: make(map[string]proxyPersist, len(p.items))}
	for _, proxy := range p.items {
		if proxy.purged.Load() {
			// Purged entries are gone from the pool file; dropping their state
			// here keeps the state file from accumulating the dead.
			continue
		}
		st.Proxies[proxy.name] = proxyPersist{
			LatencyMs:   proxy.latencyMs.Load(),
			Failures:    proxy.failures.Load(),
			Rejections:  proxy.rejections.Load(),
			Retired:     proxy.retired.Load(),
			RetiredAt:   proxy.retiredAt.Load(),
			ReviveFails: proxy.reviveFails.Load(),
			LastProbe:   proxy.lastProbe.Load(),
		}
	}
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := p.statePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p.statePath)
}

// upstreamNode keeps a key stable while allowing its proxy binding to change
// atomically when a proxy becomes unavailable.
type upstreamNode struct {
	key            string
	index          int
	preferredProxy int
	proxyIndex     atomic.Int64
	failures       atomic.Uint32
	cooldownUntil  atomic.Int64
}

type upstreamNodes struct {
	nodes        []*upstreamNode
	transports   *proxyTransports
	next         atomic.Uint64
	cooldown     time.Duration
	bindingsMu   sync.Mutex
	bindingCount []int
}

// nodePool distributes keys over proxies in round-robin order. When there
// are fewer keys than proxies, the remaining proxies are intentionally idle and
// can take over a key immediately if an active proxy fails.
func nodePool(keys []string, transports *proxyTransports, cooldown time.Duration, direct bool) (*upstreamNodes, error) {
	if transports == nil || len(transports.items) == 0 {
		return nil, fmt.Errorf("at least one proxy transport is required")
	}
	pool := &upstreamNodes{
		nodes:        make([]*upstreamNode, 0, len(keys)),
		transports:   transports,
		cooldown:     cooldown,
		bindingCount: make([]int, len(transports.items)),
	}
	if len(keys) == 0 {
		return pool, nil
	}
	// direct mode pins every key to the "direct" transport so the account keeps a
	// stable exit IP; when the pool has no direct entry, fall back to spreading.
	pinned := -1
	if direct && transports.directIndex >= 0 {
		pinned = transports.directIndex
	}
	for i, key := range keys {
		proxyIndex := i % len(transports.items)
		if pinned >= 0 {
			proxyIndex = pinned
		}
		node := &upstreamNode{key: key, index: i, preferredProxy: proxyIndex}
		node.proxyIndex.Store(int64(proxyIndex))
		pool.nodes = append(pool.nodes, node)
		pool.bindingCount[proxyIndex]++
	}
	return pool, nil
}

// RestoreProxy moves keys that were originally assigned to a recovered proxy
// back to it. Proxies without an original key simply become healthy failover
// candidates again.
func (p *upstreamNodes) RestoreProxy(recoveredProxy int) int {
	if p == nil || p.transports == nil || recoveredProxy < 0 || recoveredProxy >= len(p.transports.items) {
		return 0
	}
	p.bindingsMu.Lock()
	defer p.bindingsMu.Unlock()

	moved := 0
	for _, node := range p.nodes {
		current := int(node.proxyIndex.Load())
		if node.preferredProxy != recoveredProxy || current == recoveredProxy {
			continue
		}
		if current >= 0 && current < len(p.bindingCount) {
			p.bindingCount[current]--
		}
		p.bindingCount[recoveredProxy]++
		node.proxyIndex.Store(int64(recoveredProxy))
		node.failures.Store(0)
		node.cooldownUntil.Store(0)
		moved++
	}
	return moved
}

func (p *upstreamNodes) Len() int { return len(p.nodes) }

// NodeByID resolves a node by its stable fingerprint. Callers only ever hold
// the fingerprint, never the key value, so the lookup cannot leak a secret.
func (p *upstreamNodes) NodeByID(id string) *upstreamNode {
	if p == nil || id == "" {
		return nil
	}
	for _, node := range p.nodes {
		if config.Fingerprint(node.key) == id {
			return node
		}
	}
	return nil
}

func (p *upstreamNodes) Proxy(node *upstreamNode) *proxyTransport {
	if p == nil || node == nil || p.transports == nil {
		return nil
	}
	index := int(node.proxyIndex.Load())
	if index < 0 || index >= len(p.transports.items) {
		return nil
	}
	return p.transports.items[index]
}

// RebindProxy moves every key currently using failedProxy to the least-loaded
// healthy proxy. Empty proxies are selected in configuration order; otherwise
// one of the least-loaded proxies is chosen at random. If every alternative is
// currently unhealthy, it still attempts the least-loaded alternative.
func (p *upstreamNodes) RebindProxy(failedProxy int) int {
	if p == nil || p.transports == nil || len(p.transports.items) < 2 || failedProxy < 0 || failedProxy >= len(p.transports.items) {
		return 0
	}
	p.bindingsMu.Lock()
	defer p.bindingsMu.Unlock()

	moved := 0
	for _, node := range p.nodes {
		if int(node.proxyIndex.Load()) != failedProxy {
			continue
		}
		target := p.replacementLocked(failedProxy, true)
		if target < 0 {
			target = p.replacementLocked(failedProxy, false)
		}
		if target < 0 {
			continue
		}
		p.bindingCount[failedProxy]--
		p.bindingCount[target]++
		node.proxyIndex.Store(int64(target))
		node.failures.Store(0)
		node.cooldownUntil.Store(0)
		moved++
	}
	return moved
}

func (p *upstreamNodes) replacementLocked(failedProxy int, healthyOnly bool) int {
	minimum := int(^uint(0) >> 1)
	candidates := make([]int, 0, len(p.transports.items)-1)
	for i := range p.transports.items {
		if i == failedProxy || p.transports.items[i].retired.Load() || healthyOnly && !p.transports.items[i].healthy.Load() {
			continue
		}
		count := p.bindingCount[i]
		if count == 0 {
			return i
		}
		if count < minimum {
			minimum = count
			candidates = candidates[:0]
			candidates = append(candidates, i)
		} else if count == minimum {
			candidates = append(candidates, i)
		}
	}
	if len(candidates) == 0 {
		return -1
	}
	return candidates[rand.IntN(len(candidates))]
}

type nodeCursor struct {
	pool *upstreamNodes
	next int
}

// Cursor reserves a different starting node for each concurrent request.
// Selection is delayed until Next, so a node marked failed by the preceding
// attempt is immediately skipped. Both Cursor and Next allocate no memory.
func (p *upstreamNodes) Cursor() nodeCursor {
	if len(p.nodes) == 0 {
		return nodeCursor{pool: p}
	}
	return nodeCursor{pool: p, next: int((p.next.Add(1) - 1) % uint64(len(p.nodes)))}
}

// CursorFor returns a cursor whose first choice is stable for the supplied
// affinity key. This keeps every turn in a conversation on the same upstream
// key while retaining Next's cooldown-aware failover behavior. Empty affinity
// keys keep the round-robin behavior used by background tasks.
func (p *upstreamNodes) CursorFor(affinity string) nodeCursor {
	if affinity == "" || len(p.nodes) == 0 {
		return p.Cursor()
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(affinity))
	return nodeCursor{pool: p, next: int(hash.Sum64() % uint64(len(p.nodes)))}
}

func (c *nodeCursor) Next() *upstreamNode {
	if c.pool == nil || len(c.pool.nodes) == 0 {
		return nil
	}
	now := time.Now().UnixNano()
	choice := -1
	var earliest int64
	for offset := 0; offset < len(c.pool.nodes); offset++ {
		i := (c.next + offset) % len(c.pool.nodes)
		until := c.pool.nodes[i].cooldownUntil.Load()
		if until <= now {
			choice = i
			break
		}
		if choice == -1 || until < earliest {
			choice, earliest = i, until
		}
	}
	if choice < 0 {
		return nil
	}
	c.next = (choice + 1) % len(c.pool.nodes)
	return c.pool.nodes[choice]
}

func (p *upstreamNodes) MarkSuccess(node *upstreamNode) {
	node.failures.Store(0)
	node.cooldownUntil.Store(0)
}

func (p *upstreamNodes) MarkFailure(node *upstreamNode, resp *http.Response, err error) {
	if err == nil && resp != nil && resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
		return
	}
	failures := node.failures.Add(1)
	multiplier := time.Duration(1 << min(failures-1, 3))
	delay := p.cooldown * multiplier
	if resp != nil {
		if retryAfter := parseRetryAfter(resp.Header.Get("Retry-After")); retryAfter > delay {
			delay = retryAfter
		}
	}
	node.cooldownUntil.Store(time.Now().Add(delay).UnixNano())
}

func parseRetryAfter(value string) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		return max(time.Until(when), 0)
	}
	return 0
}
