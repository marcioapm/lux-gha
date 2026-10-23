package main

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/marcioapm/lux-gha/config"
)

// selfSigned writes a self-signed certificate for 127.0.0.1 and its key to
// certFile and keyFile, and returns the certificate.
func selfSigned(t *testing.T, certFile, keyFile string) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, _ := rand.Int(rand.Reader, big.NewInt(1<<62))
	tmpl := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "cache.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, DNSNames: []string{"cache.test"},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	writeFile(t, keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}))
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// writeFile writes data and moves the modification time forward, so a
// rewrite within the file system's timestamp granularity still changes it.
func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	var next time.Time
	if fi, err := os.Stat(path); err == nil {
		next = fi.ModTime().Add(time.Second)
	} else {
		next = time.Now()
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, next, next); err != nil {
		t.Fatal(err)
	}
}

func trusting(certs ...*x509.Certificate) *tls.Config {
	pool := x509.NewCertPool()
	for _, c := range certs {
		pool.AddCert(c)
	}
	return &tls.Config{RootCAs: pool}
}

func TestRunServesCacheOverTLS(t *testing.T) {
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "none"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "none"))
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "upstream:"+r.URL.Path)
	}))
	defer upstream.Close()
	dir := t.TempDir()
	keyFile, certFile, tlsKeyFile := filepath.Join(dir, "signing"), filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	os.WriteFile(keyFile, []byte(strings.Repeat("k", 32)+"\n"), 0o600)
	cert := selfSigned(t, certFile, tlsKeyFile)
	path := writeConfig(t, fakeLux(t, "v0.1.14"), closedGitHub, func(c map[string]any) {
		c["cache"] = map[string]any{
			"listen": "127.0.0.1:0", "publicURL": "https://127.0.0.1:1",
			"s3":             map[string]any{"bucket": "cache-bucket", "region": "us-east-1", "endpoint": "http://127.0.0.1:1"},
			"signingKeyFile": keyFile, "issuer": "http://127.0.0.1:1", "upstreamResultsURL": upstream.URL,
			"repositoryIDs": []int64{100}, "tlsCertFile": certFile, "tlsKeyFile": tlsKeyFile,
		}
	})
	addrs := make(chan [2]string, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, path, slog.New(listenHandler{addrs})) }()
	var cacheAddr string
	for cacheAddr == "" {
		select {
		case a := <-addrs:
			if a[0] == "cache serving" {
				cacheAddr = a[1]
			}
		case err := <-done:
			t.Fatalf("run returned before serving: %v", err)
		case <-time.After(10 * time.Second):
			t.Fatal("cache listener did not start")
		}
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: trusting(cert)}}
	req, _ := http.NewRequest(http.MethodPost, "https://"+cacheAddr+"/twirp/github.actions.results.api.v1.CacheService/GetCacheEntryDownloadURL", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(string(b), `"unauthenticated"`) {
		t.Fatalf("cache over TLS: %d %s", resp.StatusCode, b)
	}
	if resp.TLS == nil || resp.TLS.Version < tls.VersionTLS12 {
		t.Fatalf("connection state %+v", resp.TLS)
	}
	// Plain HTTP gets no cache answer: net/http answers a non-TLS first
	// record with a bare 400 and closes.
	plain, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + cacheAddr + "/twirp/x")
	if err == nil {
		b, _ := io.ReadAll(plain.Body)
		plain.Body.Close()
		if plain.StatusCode != http.StatusBadRequest || !strings.Contains(string(b), "HTTPS server") {
			t.Fatalf("plain HTTP answered %d %s", plain.StatusCode, b)
		}
	}
	// A TLS 1.1 client is refused.
	old := trusting(cert)
	old.MinVersion, old.MaxVersion = tls.VersionTLS10, tls.VersionTLS11 // offered, so the server's MinVersion decides
	if c, err := tls.Dial("tcp", cacheAddr, old); err == nil {
		c.Close()
		t.Fatal("TLS 1.1 handshake succeeded")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown err=%v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("run did not return after cancellation")
	}
}

func TestRunRefusesCacheTLSWithoutALoadablePair(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "signing")
	os.WriteFile(keyFile, []byte(strings.Repeat("k", 32)+"\n"), 0o600)
	for _, c := range []struct {
		name  string
		setup func(certFile, keyFile string)
	}{
		{"missing", func(string, string) {}},
		{"garbage", func(certFile, keyFile string) {
			os.WriteFile(certFile, []byte("not a certificate"), 0o600)
			os.WriteFile(keyFile, []byte("not a key"), 0o600)
		}},
	} {
		name, files := c.name, t.TempDir()
		certFile, tlsKeyFile := filepath.Join(files, "cert.pem"), filepath.Join(files, "key.pem")
		c.setup(certFile, tlsKeyFile)
		path := writeConfig(t, fakeLux(t, "v0.1.14"), closedGitHub, func(c map[string]any) {
			c["cache"] = map[string]any{
				"listen": "127.0.0.1:0", "publicURL": "https://127.0.0.1:1",
				"s3": map[string]any{"bucket": "cache-bucket", "region": "us-east-1"}, "signingKeyFile": keyFile,
				"repositoryOwnerIDs": []int64{7}, "tlsCertFile": certFile, "tlsKeyFile": tlsKeyFile,
			}
		})
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		err := run(ctx, path, slog.New(slog.DiscardHandler))
		cancel()
		if err == nil || !strings.Contains(err.Error(), "cache.tlsCertFile") {
			t.Errorf("%s: err=%v", name, err)
		}
	}
}

// countHandler counts records by message.
type countHandler struct {
	mu   sync.Mutex
	msgs map[string]int
}

func (h *countHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *countHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *countHandler) WithGroup(string) slog.Handler            { return h }
func (h *countHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.msgs[r.Message]++
	return nil
}

func (h *countHandler) count(msg string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.msgs[msg]
}

// reloaderAt is a reloader on a test clock, serving TLS on loopback.
type reloaderAt struct {
	r    *certReloader
	logs *countHandler
	mu   sync.Mutex
	now  time.Time
	addr string
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

// served reports the certificate a new connection is given.
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

func TestCertReloaderPicksUpRotationAndKeepsLastGoodPair(t *testing.T) {
	dir := t.TempDir()
	certFile, keyFile := filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
	first := selfSigned(t, certFile, keyFile)
	h := newReloaderAt(t, certFile, keyFile)
	if !h.served(t).Equal(first) {
		t.Fatal("initial certificate not served")
	}

	second := selfSigned(t, certFile, keyFile)
	h.advance(certCheckInterval - time.Second)
	if !h.served(t).Equal(first) {
		t.Fatal("files re-read before the check interval elapsed")
	}
	h.advance(time.Second)
	if !h.served(t).Equal(second) {
		t.Fatal("rotated certificate not served")
	}
	if n := h.logs.count("cache listener: TLS certificate reloaded"); n != 1 {
		t.Fatalf("reload logged %d times", n)
	}

	writeFile(t, certFile, []byte("-----BEGIN CERTIFICATE-----\nbroken\n-----END CERTIFICATE-----\n"))
	for range 3 {
		h.advance(certCheckInterval)
		if !h.served(t).Equal(second) {
			t.Fatal("broken replacement displaced the last good certificate")
		}
	}
	if n := h.logs.count(reloadFailed); n != 1 {
		t.Fatalf("failed reload logged %d times, want once", n)
	}

	third := selfSigned(t, certFile, keyFile)
	h.advance(certCheckInterval)
	if !h.served(t).Equal(third) {
		t.Fatal("repaired files not picked up")
	}
}

// Only a loopback cache.listen, or one serving TLS, starts without the
// clear-text warning.
func TestPlainCacheListenIsWarnedAbout(t *testing.T) {
	for _, c := range []struct {
		listen string
		tls    bool
		warned bool
	}{
		{"127.0.0.1:8090", false, false}, {"[::1]:8090", false, false}, {"localhost:8090", false, false},
		{"10.0.0.5:8090", false, true}, {"0.0.0.0:8090", false, true}, {":8090", false, true},
		{"10.0.0.5:8090", true, false}, {"0.0.0.0:8090", true, false}, {"127.0.0.1:8090", true, false},
	} {
		cc := &config.Cache{Listen: c.listen}
		if c.tls {
			cc.TLSCertFile, cc.TLSKeyFile = "/c.pem", "/k.pem"
		}
		var buf strings.Builder
		warnPlainCacheListen(cc, slog.New(slog.NewTextHandler(&buf, nil)))
		if got := strings.Contains(buf.String(), "level=WARN") && strings.Contains(buf.String(), "clear text"); got != c.warned {
			t.Errorf("%s tls=%v: warned=%v, log %q", c.listen, c.tls, got, buf.String())
		}
	}
}
