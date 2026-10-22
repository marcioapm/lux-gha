package cache

import (
	"io"
	"net/http"
	"sync"
	"time"
)

// The cache listener has no server-wide read or write timeout, since an
// upload block or a proxied transfer can take minutes. Each handler sets
// its own deadlines instead:
//
//   - a Twirp request is read within twirpReadTimeout of its start;
//   - an upload body or a proxied body must make progress: every read and
//     every write gets idleTimeout from the moment it starts.
//
// Once a body has been read in full the read deadline is cleared, so a
// handler's own work (an S3 copy, an upstream long poll) is not cut by it.
const (
	defaultTwirpReadTimeout = 30 * time.Second
	defaultIdleTimeout      = 60 * time.Second
	// maxProxyRequests bounds concurrent pass-through requests, which are
	// not authenticated here; past it the proxy answers 503.
	maxProxyRequests = 256
)

// deadlineBody wraps a request body. With idle > 0 every Read first moves
// the connection's read deadline to idle from now; the deadline is cleared
// once the declared length or EOF has been read.
type deadlineBody struct {
	io.ReadCloser
	rc        *http.ResponseController
	idle      time.Duration
	remaining int64 // -1 when the length is unknown
}

func (b *deadlineBody) Read(p []byte) (int, error) {
	if b.idle > 0 {
		_ = b.rc.SetReadDeadline(time.Now().Add(b.idle))
	}
	n, err := b.ReadCloser.Read(p)
	if b.remaining > 0 {
		b.remaining -= int64(n)
	}
	if b.remaining == 0 || err == io.EOF {
		_ = b.rc.SetReadDeadline(time.Time{})
	}
	return n, err
}

// deadlineWriter moves the connection's write deadline to idle from now
// before every write and flush, so a client that stops reading is cut
// without capping how long a response may stream.
type deadlineWriter struct {
	http.ResponseWriter
	rc   *http.ResponseController
	idle time.Duration
}

func (w *deadlineWriter) WriteHeader(code int) {
	_ = w.rc.SetWriteDeadline(time.Now().Add(w.idle))
	w.ResponseWriter.WriteHeader(code)
}

func (w *deadlineWriter) Write(p []byte) (int, error) {
	_ = w.rc.SetWriteDeadline(time.Now().Add(w.idle))
	return w.ResponseWriter.Write(p)
}

func (w *deadlineWriter) Flush() {
	_ = w.rc.SetWriteDeadline(time.Now().Add(w.idle))
	_ = w.rc.Flush()
}

// Unwrap lets http.ResponseController, which ReverseProxy flushes through,
// reach the connection.
func (w *deadlineWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

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
