package cache

import (
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// The cache listener has no server-wide read or write timeout, since an
// upload block or a proxied transfer can take minutes. Each handler sets
// its own deadlines instead:
//
//   - a Twirp request is read within twirpReadTimeout of its start;
//   - an upload body, a proxied body and every response must move at least
//     progressBytes (or reach its end) within each progressWindow spent
//     waiting on the client. The window restarts only when that quota is
//     met, so a client that sends or reads one byte now and then is cut
//     after one window. Time the server spends between reads or writes
//     (an S3 write, an upstream long poll) is not charged to the client.
//
// Once a body has been read in full the read deadline is cleared, so a
// handler's own work (an S3 copy, an upstream long poll) is not cut by it.
const (
	defaultTwirpReadTimeout = 30 * time.Second
	defaultProgressWindow   = 60 * time.Second
	minProgressBytes        = 64 << 10
	// Pass-through requests are not authenticated here. Each client
	// address may run maxProxyPerClient at once; maxProxyRequests is a
	// server-wide safety net. Past either the proxy answers 503.
	maxProxyPerClient = 32
	maxProxyRequests  = 4096
)

// progress applies the minimum-progress rule to one direction of a
// connection: set moves the connection's deadline.
type progress struct {
	set    func(time.Time) error
	window time.Duration
	quota  int64
	spent  time.Duration // time charged to the client in this window
	moved  int64         // bytes moved in this window
}

// begin arms the deadline for one blocking call and returns its start.
func (p *progress) begin() time.Time {
	now := time.Now()
	_ = p.set(now.Add(p.window - p.spent))
	return now
}

// end charges the call's time and bytes, starting a new window once the
// quota is met.
func (p *progress) end(start time.Time, n int) {
	p.spent += time.Since(start)
	p.moved += int64(n)
	if p.moved >= p.quota {
		p.spent, p.moved = 0, 0
	}
}

// progressBody wraps a request body under the minimum-progress rule; the
// read deadline is cleared once the declared length or EOF has been read.
// A zero window leaves the deadline set by the caller in place.
type progressBody struct {
	io.ReadCloser
	rc        *http.ResponseController
	p         progress
	remaining int64 // -1 when the length is unknown
}

func newProgressBody(r *http.Request, rc *http.ResponseController, window time.Duration, quota int64) *progressBody {
	return &progressBody{
		ReadCloser: r.Body,
		rc:         rc,
		p:          progress{set: rc.SetReadDeadline, window: window, quota: quota},
		remaining:  r.ContentLength,
	}
}

func (b *progressBody) Read(buf []byte) (int, error) {
	var start time.Time
	if b.p.window > 0 {
		start = b.p.begin()
	}
	n, err := b.ReadCloser.Read(buf)
	if b.p.window > 0 {
		b.p.end(start, n)
	}
	if b.remaining > 0 {
		b.remaining -= int64(n)
	}
	if b.remaining == 0 || err == io.EOF {
		_ = b.rc.SetReadDeadline(time.Time{})
	}
	return n, err
}

// progressWriter applies the minimum-progress rule to a response: a
// client that stops reading, or reads below the minimum rate, is cut,
// while a response may stream for as long as it keeps moving.
type progressWriter struct {
	http.ResponseWriter
	rc *http.ResponseController
	p  progress
}

func newProgressWriter(w http.ResponseWriter, rc *http.ResponseController, window time.Duration, quota int64) *progressWriter {
	return &progressWriter{ResponseWriter: w, rc: rc, p: progress{set: rc.SetWriteDeadline, window: window, quota: quota}}
}

func (w *progressWriter) WriteHeader(code int) {
	_ = w.p.begin()
	w.ResponseWriter.WriteHeader(code)
}

func (w *progressWriter) Write(b []byte) (int, error) {
	start := w.p.begin()
	n, err := w.ResponseWriter.Write(b)
	w.p.end(start, n)
	return n, err
}

func (w *progressWriter) Flush() {
	start := w.p.begin()
	_ = w.rc.Flush()
	w.p.end(start, 0)
}

// Unwrap lets http.ResponseController, which ReverseProxy flushes through,
// reach the connection.
func (w *progressWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// proxyLimiter bounds concurrent pass-through requests per client address
// and in total.
type proxyLimiter struct {
	perClient, total int

	mu      sync.Mutex
	n       int
	clients map[string]int
}

func newProxyLimiter(perClient, total int) *proxyLimiter {
	return &proxyLimiter{perClient: perClient, total: total, clients: map[string]int{}}
}

// clientOf is the connection's peer address. Forwarded headers are not
// trusted: any client can set them.
func clientOf(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// acquire takes a slot for client; it reports which limit refused it, or
// "" with a release function.
func (l *proxyLimiter) acquire(client string) (release func(), refused string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.n >= l.total {
		return nil, "total"
	}
	if l.clients[client] >= l.perClient {
		return nil, "client"
	}
	l.n++
	l.clients[client]++
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.n--
		if l.clients[client]--; l.clients[client] == 0 {
			delete(l.clients, client)
		}
	}, ""
}

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
