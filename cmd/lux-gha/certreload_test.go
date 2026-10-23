//go:build unix

package main

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

// reloaderAt is a reloader on a test clock, serving TLS on loopback, and
// served() reports the certificate a new connection is given.
type reloaderAt struct {
	r       *certReloader
	logs    *countHandler
	mu      sync.Mutex
	now     time.Time
	addr    string
	cleanup func()
}

func newReloaderAt(t *testing.T, certFile, keyFile string) *reloaderAt {
	t.Helper()
	h := &reloaderAt{logs: &countHandler{msgs: map[string]int{}}}
	r, err := newCertReloader(certFile, keyFile, slog.New(h.logs))
	if err != nil {
		t.Fatal(err)
	}
	// The test clock starts where the reloader's last check was, so
	// advance(certCheckInterval) is exactly one interval later.
	h.now = r.checked
	r.now = func() time.Time { h.mu.Lock(); defer h.mu.Unlock(); return h.now }
	h.r = r
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok") })}
	go srv.Serve(tls.NewListener(ln, r.tlsConfig()))
	t.Cleanup(func() { srv.Close() })
	h.addr = ln.Addr().String()
	return h
}

func (h *reloaderAt) advance(d time.Duration) { h.mu.Lock(); h.now = h.now.Add(d); h.mu.Unlock() }

// jump moves the test clock to t, with the next handshake due a check.
func (h *reloaderAt) jump(t time.Time) {
	h.mu.Lock()
	h.now = t
	h.mu.Unlock()
	h.r.mu.Lock()
	h.r.checked = t.Add(-certCheckInterval)
	h.r.mu.Unlock()
}

func (h *reloaderAt) served(t *testing.T) *x509.Certificate {
	t.Helper()
	cfg := trusting()
	cfg.InsecureSkipVerify = true
	c, err := tls.Dial("tcp", h.addr, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.ConnectionState().PeerCertificates[0]
}

const reloadFailed = "cache listener: TLS certificate reload failed; serving the previous certificate"

// A renewal that writes the new certificate before its key is a valid
// certificate that does not match the key in place: the old pair stays in
// service until the key arrives.
func TestCertReloaderKeepsTheOldPairUntilTheNewKeyArrives(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	first := selfSigned(t, certFile, keyFile)
	h := newReloaderAt(t, certFile, keyFile)

	staged := filepath.Join(dir, "staged")
	if err := os.Mkdir(staged, 0o700); err != nil {
		t.Fatal(err)
	}
	second := selfSigned(t, filepath.Join(staged, "cert.pem"), filepath.Join(staged, "key.pem"))
	certPEM, _ := os.ReadFile(filepath.Join(staged, "cert.pem"))
	keyPEM, _ := os.ReadFile(filepath.Join(staged, "key.pem"))

	writeFile(t, certFile, certPEM)
	h.advance(certCheckInterval)
	if !h.served(t).Equal(first) {
		t.Fatal("a certificate without its key displaced the old pair")
	}
	if n := h.logs.count(reloadFailed); n != 1 {
		t.Fatalf("mismatched pair logged %d times, want once", n)
	}
	writeFile(t, keyFile, keyPEM)
	h.advance(certCheckInterval)
	if !h.served(t).Equal(second) {
		t.Fatal("the new pair was not served once the key arrived")
	}
}

// An unreadable key is fixed by chmod, which changes neither file's
// modification time nor size: the reloader still retries until it loads.
func TestCertReloaderRetriesAFailedLoadWithoutAFileChange(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-000 file")
	}
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	selfSigned(t, certFile, keyFile)
	h := newReloaderAt(t, certFile, keyFile)

	second := selfSigned(t, certFile, keyFile)
	if err := os.Chmod(keyFile, 0); err != nil {
		t.Fatal(err)
	}
	h.advance(certCheckInterval)
	h.served(t)
	h.advance(certCheckInterval)
	h.served(t)
	if n := h.logs.count(reloadFailed); n != 1 {
		t.Fatalf("unreadable key logged %d times, want once", n)
	}
	if err := os.Chmod(keyFile, 0o600); err != nil {
		t.Fatal(err)
	}
	h.advance(certCheckInterval)
	if !h.served(t).Equal(second) {
		t.Fatal("a load fixed by chmod alone was not retried")
	}
}

// An atomic replacement (a new file renamed over the old path) that keeps
// the size and the modification time is still a change: the inode differs.
func TestCertReloaderSeesAnAtomicReplacementWithTheSameStamp(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	selfSigned(t, certFile, keyFile)
	h := newReloaderAt(t, certFile, keyFile)

	staged := filepath.Join(dir, "staged")
	if err := os.Mkdir(staged, 0o700); err != nil {
		t.Fatal(err)
	}
	second := selfSigned(t, filepath.Join(staged, "cert.pem"), filepath.Join(staged, "key.pem"))
	for _, f := range []struct{ from, to string }{{"cert.pem", certFile}, {"key.pem", keyFile}} {
		from := filepath.Join(staged, f.from)
		old, err := os.Stat(f.to)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(from, old.ModTime(), old.ModTime()); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(from, f.to); err != nil {
			t.Fatal(err)
		}
	}
	certStat, _ := os.Stat(certFile)
	if certStat.Sys().(*syscall.Stat_t).Ino == h.r.stamp[0].ino {
		t.Skip("the file system reused the inode")
	}
	h.advance(certCheckInterval)
	if !h.served(t).Equal(second) {
		t.Fatal("an atomic replacement with the same modification time was not picked up")
	}
}

// A rewrite in place that keeps the modification time (a coarse-timestamp
// file system, or a tool that preserves it) is seen by the new size.
func TestCertReloaderSeesARewriteWithTheSameModificationTime(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	selfSigned(t, certFile, keyFile)
	h := newReloaderAt(t, certFile, keyFile)
	before := map[string]os.FileInfo{}
	for _, f := range []string{certFile, keyFile} {
		fi, err := os.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		before[f] = fi
	}
	var second *x509.Certificate
	for range 20 { // until a new pair differs in size from the old one
		second = selfSigned(t, certFile, keyFile)
		if fi, _ := os.Stat(certFile); fi.Size() != before[certFile].Size() {
			break
		}
	}
	if fi, _ := os.Stat(certFile); fi.Size() == before[certFile].Size() {
		t.Skip("no new pair of a different size")
	}
	for f, fi := range before {
		if err := os.Chtimes(f, fi.ModTime(), fi.ModTime()); err != nil {
			t.Fatal(err)
		}
	}
	h.advance(certCheckInterval)
	if !h.served(t).Equal(second) {
		t.Fatal("a rewrite of a different size with the same modification time was not picked up")
	}
}

// Concurrent handshakes across a reload: no race, every one served.
func TestCertReloaderServesConcurrentHandshakesAcrossAReload(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	selfSigned(t, certFile, keyFile)
	h := newReloaderAt(t, certFile, keyFile)
	selfSigned(t, certFile, keyFile)
	h.advance(certCheckInterval)
	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.served(t)
		}()
	}
	wg.Wait()
	if n := h.logs.count("cache listener: TLS certificate reloaded"); n != 1 {
		t.Fatalf("reload logged %d times across concurrent handshakes, want once", n)
	}
}

// The served certificate's expiry is warned about once a day within
// certExpiryWarning, and not before.
func TestCertReloaderWarnsDailyBeforeExpiry(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	cert := selfSigned(t, certFile, keyFile)
	h := newReloaderAt(t, certFile, keyFile)
	const soon = "cache listener: the TLS certificate expires soon; check its renewal"
	h.jump(cert.NotAfter.Add(-certExpiryWarning - time.Hour))
	h.served(t)
	if n := h.logs.count(soon); n != 0 {
		t.Fatalf("warned %d times outside the window", n)
	}
	h.advance(2 * time.Hour)
	h.served(t)
	h.advance(certCheckInterval)
	h.served(t)
	if n := h.logs.count(soon); n != 1 {
		t.Fatalf("warned %d times within a day, want once", n)
	}
	h.advance(24 * time.Hour)
	h.served(t)
	if n := h.logs.count(soon); n != 2 {
		t.Fatalf("warned %d times after a day, want twice", n)
	}
}

// A key file other users can read is warned about at startup.
func TestCertReloaderWarnsAboutAnOpenKeyFile(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	selfSigned(t, certFile, keyFile)
	const open = "cache listener: the TLS key file is readable or writable by other users"
	for _, c := range []struct {
		mode os.FileMode
		want int
	}{{0o600, 0}, {0o640, 1}, {0o644, 1}} {
		if err := os.Chmod(keyFile, c.mode); err != nil {
			t.Fatal(err)
		}
		logs := &countHandler{msgs: map[string]int{}}
		if _, err := newCertReloader(certFile, keyFile, slog.New(logs)); err != nil {
			t.Fatal(err)
		}
		if n := logs.count(open); n != c.want {
			t.Fatalf("mode %v: warned %d times, want %d", c.mode, n, c.want)
		}
	}
}

// A path that is not a regular file (a FIFO would block the read with the
// lock held) is refused at startup and never read on reload.
func TestCertReloaderRefusesANonRegularFile(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	selfSigned(t, certFile, keyFile)
	h := newReloaderAt(t, certFile, keyFile)
	first := h.served(t)
	if err := os.Remove(keyFile); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(keyFile, 0o600); err != nil {
		t.Skip("no mkfifo:", err)
	}
	h.advance(certCheckInterval)
	done := make(chan *x509.Certificate, 1)
	go func() { done <- h.served(t) }()
	select {
	case got := <-done:
		if !got.Equal(first) {
			t.Fatal("a FIFO key displaced the old pair")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("handshake blocked on a FIFO key file")
	}
	if _, err := newCertReloader(certFile, keyFile, slog.New(&countHandler{msgs: map[string]int{}})); err == nil {
		t.Fatal("startup accepted a FIFO key file")
	}
}
