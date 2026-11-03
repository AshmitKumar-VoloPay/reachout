package auth

import (
	"sync"
	"time"
)

// Limiter counts events per key inside a sliding window. It is in memory, so with
// several server instances each keeps its own count (limits become per instance).
type Limiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	now    func() time.Time
	hits   map[string][]time.Time
}

func NewLimiter(max int, window time.Duration) *Limiter {
	return &Limiter{max: max, window: window, now: time.Now, hits: map[string][]time.Time{}}
}

// prune drops hits older than the window. Caller holds the lock.
func (l *Limiter) prune(key string) []time.Time {
	cut := l.now().Add(-l.window)
	hs := l.hits[key]
	i := 0
	for i < len(hs) && !hs[i].After(cut) {
		i++
	}
	hs = hs[i:]
	if len(hs) == 0 {
		delete(l.hits, key)
		return nil
	}
	l.hits[key] = hs
	return hs
}

// Blocked reports whether key has used up its allowance.
func (l *Limiter) Blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.prune(key)) >= l.max
}

// Add records one event for key.
func (l *Limiter) Add(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.hits) > 50000 { // bound memory under a flood of distinct keys
		for k := range l.hits {
			l.prune(k)
		}
	}
	l.hits[key] = append(l.prune(key), l.now())
}

// Reset forgets key, e.g. after a successful login.
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.hits, key)
}
