package config

import (
	"bufio"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// ParseProxyEntry splits an optional dynamic-pool marker ("pool+" scheme prefix
// or "#pool" URL fragment) from a configured proxy string and returns the clean
// dial URL alongside whether the entry is a rotating proxy pool (such as Resin).
func ParseProxyEntry(raw string) (clean string, pool bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" || trimmed == "direct" {
		return trimmed, false
	}
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "pool+") {
		trimmed = strings.TrimSpace(trimmed[len("pool+"):])
		pool = true
	}
	if u, err := url.Parse(trimmed); err == nil && u.Host != "" {
		if strings.EqualFold(strings.TrimSpace(u.Fragment), "pool") {
			pool = true
			u.Fragment = ""
			trimmed = u.String()
		}
	} else if idx := strings.LastIndex(trimmed, "#"); idx >= 0 && strings.EqualFold(strings.TrimSpace(trimmed[idx+1:]), "pool") {
		pool = true
		trimmed = strings.TrimSpace(trimmed[:idx])
	}
	return trimmed, pool
}

// resolveProxyFiles trims cfg.Proxies in place and stores the runtime proxy
// list: those proxies followed by the entries of cfg.ProxyFile (resolved
// against the directory of configPath when relative), deduplicated, or
// ["direct"] when nothing is left.
func resolveProxyFiles(configPath string, cfg *Config) error {
	trimList(&cfg.Proxies)
	effective := append([]string(nil), cfg.Proxies...)
	if cfg.ProxyFile != "" {
		resolved := cfg.ProxyFile
		if !filepath.IsAbs(resolved) {
			resolved = filepath.Join(filepath.Dir(configPath), resolved)
		}
		proxies, err := readProxyFile(resolved)
		if err != nil {
			return fmt.Errorf("load proxy file %s: %w", resolved, err)
		}
		effective = append(effective, proxies...)
	}

	effective = uniqueProxyEntries(effective)
	if len(effective) == 0 {
		effective = []string{"direct"}
	}
	cfg.effectiveProxies = effective

	// The fallback tier is a separate, lower-priority list (e.g. a local bridge
	// exposing paid nodes). It is resolved the same way and de-duplicated
	// against the primary set, so a proxy appearing in both stays primary.
	cfg.fallbackProxies = nil
	if file := strings.TrimSpace(cfg.ProxyPool.FallbackProxyFile); file != "" {
		resolvedFallback := file
		if !filepath.IsAbs(resolvedFallback) {
			resolvedFallback = filepath.Join(filepath.Dir(configPath), resolvedFallback)
		}
		fallback, err := readProxyFile(resolvedFallback)
		if err != nil {
			return fmt.Errorf("load fallback proxy file %s: %w", resolvedFallback, err)
		}
		primary := make(map[string]struct{}, len(effective))
		for _, item := range effective {
			primary[item] = struct{}{}
		}
		out := fallback[:0]
		seen := make(map[string]struct{}, len(fallback))
		for _, item := range fallback {
			if _, dup := primary[item]; dup {
				continue
			}
			if _, dup := seen[item]; dup {
				continue
			}
			seen[item] = struct{}{}
			out = append(out, item)
		}
		cfg.fallbackProxies = out
	}
	return nil
}

// readProxyFile returns one proxy entry per non-empty line of path, ignoring a
// byte-order mark and comments (see stripProxyLineComment). A line may be up
// to 1 MiB long.
func readProxyFile(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var proxies []string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		value := strings.TrimSpace(strings.TrimPrefix(scanner.Text(), "\ufeff"))
		value = strings.TrimSpace(stripProxyLineComment(value))
		if value != "" {
			proxies = append(proxies, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return proxies, nil
}

// stripProxyLineComment cuts line at the first "#", ";" or "//" that starts
// the line or follows a space or tab, so a #pool fragment or the "//" of a
// URL survives.
func stripProxyLineComment(line string) string {
	for i := 0; i < len(line); i++ {
		if i > 0 && line[i-1] != ' ' && line[i-1] != '\t' {
			continue
		}
		if line[i] == '#' || line[i] == ';' || (line[i] == '/' && i+1 < len(line) && line[i+1] == '/') {
			return line[:i]
		}
	}
	return line
}

// uniqueProxyEntries drops empty entries and repeats, keeping the first
// spelling of each, and filters items in place. Entries are compared by their
// clean dial URL (see ParseProxyEntry) together with the pool marker, so
// "pool+URL" and "URL#pool" are one entry, while a #pool entry next to a
// static proxy that dials the same endpoint is kept: they are different
// routes (fresh connection, no cooldown, no health eviction), not two
// spellings of one.
func uniqueProxyEntries(items []string) []string {
	out := items[:0]
	seen := make(map[string]struct{}, len(items))
	pools := make(map[string]struct{}, len(items))
	for _, item := range items {
		key, pool := ParseProxyEntry(item)
		if key == "" {
			continue
		}
		if pool {
			if _, exists := pools[key]; exists {
				continue
			}
			pools[key] = struct{}{}
			out = append(out, item)
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, item)
	}
	return out
}

// IsDirect reports whether a proxy entry is the local-egress marker.
func IsDirect(raw string) bool {
	return strings.EqualFold(strings.TrimSpace(raw), "direct")
}

// EnsureDirect returns proxies with a "direct" entry appended when it has none.
// Callers that need a guaranteed local egress (key_proxy_mode = "direct" pins the
// keyed lanes to it) must use this instead of assuming the configured list has
// one: a config whose proxies all come from proxyfile has no direct transport,
// and without this the setting would silently degrade to "spread every key
// across the pool" — a no-op that looks like it worked.
func EnsureDirect(proxies []string) []string {
	for _, item := range proxies {
		if IsDirect(item) {
			return proxies
		}
	}
	out := make([]string, 0, len(proxies)+1)
	out = append(out, proxies...)
	return append(out, "direct")
}

// trimList trims every item in place and drops the empty ones.
func trimList(items *[]string) {
	out := (*items)[:0]
	for _, item := range *items {
		if value := strings.TrimSpace(item); value != "" {
			out = append(out, value)
		}
	}
	*items = out
}
