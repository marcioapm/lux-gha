package cache

import (
	"sync"
	"time"
)

// logLimiter admits one event per key per window, so a client cannot
// flood the log by repeating a rejected request.
type logLimiter struct {
	window time.Duration
	now    func() time.Time

	mu   sync.Mutex
	last map[string]time.Time
}

// maxLimiterKeys bounds the limiter's memory; past it, expired keys are
// dropped, and if none are, the map is reset.
const maxLimiterKeys = 4096

func newLogLimiter(window time.Duration, now func() time.Time) *logLimiter {
	return &logLimiter{window: window, now: now, last: map[string]time.Time{}}
}

func (l *logLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if t, ok := l.last[key]; ok && now.Sub(t) < l.window {
		return false
	}
	if len(l.last) >= maxLimiterKeys {
		for k, t := range l.last {
			if now.Sub(t) >= l.window {
				delete(l.last, k)
			}
		}
		if len(l.last) >= maxLimiterKeys {
			clear(l.last)
		}
	}
	l.last[key] = now
	return true
}
