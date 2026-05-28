//go:build voice

package voice

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

type PreCallResult struct {
	Workflow    string          `json:"workflow"`
	ContextVars json.RawMessage `json:"context_vars,omitempty"`
	Warnings    []string        `json:"warnings,omitempty"`
}

type LookupRunner struct {
	cfg   PreCallConfig
	cache *lookupCache
}

func NewLookupRunner(cfg PreCallConfig) LookupRunner {
	return LookupRunner{cfg: cfg, cache: newLookupCache(5 * time.Minute)}
}

func (l LookupRunner) Run(ctx context.Context, callerPhone string) PreCallResult {
	if v, ok := l.cache.get(callerPhone); ok {
		return v
	}
	out := PreCallResult{Workflow: l.defaultWorkflow()}
	if len(l.cfg.Lookups) > 0 {
		out.Warnings = append(out.Warnings, "lookup adapters not wired in v0.1; returning default routing only")
	}
	l.cache.put(callerPhone, out)
	return out
}

func (l LookupRunner) defaultWorkflow() string {
	for _, r := range l.cfg.RoutingRules {
		if r.Default != "" {
			return r.Default
		}
	}
	return ""
}

type lookupCache struct {
	mu   sync.Mutex
	ttl  time.Duration
	data map[string]cacheEntry
}

type cacheEntry struct {
	val PreCallResult
	exp time.Time
}

func newLookupCache(ttl time.Duration) *lookupCache {
	return &lookupCache{ttl: ttl, data: make(map[string]cacheEntry)}
}

func (c *lookupCache) get(k string) (PreCallResult, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.data[k]
	if !ok || time.Now().After(e.exp) {
		return PreCallResult{}, false
	}
	return e.val, true
}

func (c *lookupCache) put(k string, v PreCallResult) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data[k] = cacheEntry{val: v, exp: time.Now().Add(c.ttl)}
}
