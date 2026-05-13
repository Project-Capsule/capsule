package auth

import (
	"sync"
	"time"
)

// jtiCache stores accepted JWT IDs until their exp time so a captured
// token can't be replayed within its 60s window. Bounded memory ∝
// (call_rate × token_lifetime). Periodic sweep is started by Run.
type jtiCache struct {
	mu    sync.Mutex
	items map[string]int64 // jti -> exp (unix seconds)
}

func newJTICache() *jtiCache {
	return &jtiCache{items: map[string]int64{}}
}

// SeenOrAdd returns true if jti was already accepted (the caller should
// reject the request as a replay), false if this is the first sighting
// (caller should proceed). expUnix is the token's exp claim — the entry
// expires from the cache once that time has passed.
func (c *jtiCache) SeenOrAdd(jti string, expUnix int64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.items[jti]; ok {
		return true
	}
	c.items[jti] = expUnix
	return false
}

// sweep removes entries whose exp time is in the past. Runs from Run on
// a fixed interval — bounded memory is the whole point of the cache.
func (c *jtiCache) sweep(now int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for jti, exp := range c.items {
		if exp <= now {
			delete(c.items, jti)
		}
	}
}

// Run sweeps the cache every interval. Stops when stop is closed.
// Safe to call once on the main goroutine; the sweeper exits cleanly
// on stop.
func (c *jtiCache) Run(stop <-chan struct{}, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-t.C:
			c.sweep(now.Unix())
		}
	}
}
