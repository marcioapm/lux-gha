package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// certCheckInterval bounds how often the certificate files are stat'ed.
const certCheckInterval = 30 * time.Second

// certExpiryWarning is how close to its expiry the served certificate gets
// a daily warning: a renewal that keeps failing is otherwise silent until
// every job's TLS handshake fails.
const certExpiryWarning = 7 * 24 * time.Hour

// certReloader serves a PEM certificate and key pair and reloads it when
// either file changes (modification time, size or inode). A pair that fails
// to load keeps the last good one in service; the failure is logged once
// per change of the files, and the load is retried on every check until it
// succeeds, so a fix that changes neither file (chmod, chown) is picked up.
type certReloader struct {
	certFile, keyFile string
	interval          time.Duration
	now               func() time.Time
	log               *slog.Logger

	mu       sync.Mutex
	cert     *tls.Certificate
	stamp    [2]fileStamp
	checked  time.Time
	failing  bool      // the last load failed; retried on every check
	expiryAt time.Time // when the expiry warning was last logged
}

// fileStamp is what a stat reveals of a change; err is set when the stat
// failed or the path is not a regular file, so a file that stays missing is
// not reported again.
type fileStamp struct {
	mod  time.Time
	size int64
	ino  uint64
	err  string
}

func statStamp(path string) fileStamp {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{err: err.Error()}
	}
	if !fi.Mode().IsRegular() {
		return fileStamp{err: path + ": not a regular file"}
	}
	return fileStamp{mod: fi.ModTime(), size: fi.Size(), ino: inode(fi)}
}

// loadPair loads the pair and parses its leaf. A failed stat in stamp is
// returned without reading either file, so a FIFO is never opened.
func loadPair(stamp [2]fileStamp, certFile, keyFile string) (*tls.Certificate, error) {
	for _, s := range stamp {
		if s.err != "" {
			return nil, errors.New(s.err)
		}
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	if cert.Leaf == nil {
		if cert.Leaf, err = x509.ParseCertificate(cert.Certificate[0]); err != nil {
			return nil, err
		}
	}
	return &cert, nil
}

func newCertReloader(certFile, keyFile string, log *slog.Logger) (*certReloader, error) {
	r := &certReloader{certFile: certFile, keyFile: keyFile, interval: certCheckInterval, now: time.Now, log: log}
	r.stamp = r.stat()
	cert, err := loadPair(r.stamp, certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("cache.tlsCertFile, cache.tlsKeyFile: %w", err)
	}
	if fi, err := os.Stat(keyFile); err == nil && fi.Mode().Perm()&0o077 != 0 {
		log.Warn("cache listener: the TLS key file is readable or writable by other users", "tlsKeyFile", keyFile, "mode", fi.Mode().Perm().String())
	}
	r.cert, r.checked = cert, r.now()
	r.log.Info("cache listener: TLS certificate loaded", "tlsCertFile", certFile, "notAfter", cert.Leaf.NotAfter)
	return r, nil
}

func (r *certReloader) stat() [2]fileStamp {
	return [2]fileStamp{statStamp(r.certFile), statStamp(r.keyFile)}
}

func (r *certReloader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.now()
	if now.Sub(r.checked) < r.interval {
		return r.cert, nil
	}
	r.checked = now
	defer r.warnExpiry(now)
	stamp := r.stat()
	changed := stamp != r.stamp
	if !changed && !r.failing {
		return r.cert, nil
	}
	r.stamp = stamp
	cert, err := loadPair(stamp, r.certFile, r.keyFile)
	if err != nil {
		if changed {
			r.log.Error("cache listener: TLS certificate reload failed; serving the previous certificate",
				"tlsCertFile", r.certFile, "tlsKeyFile", r.keyFile, "error", err)
		}
		r.failing = true
		return r.cert, nil
	}
	r.cert, r.failing = cert, false
	r.log.Info("cache listener: TLS certificate reloaded", "tlsCertFile", r.certFile, "notAfter", cert.Leaf.NotAfter)
	return r.cert, nil
}

// warnExpiry logs, at most once a day, that the served certificate expires
// within certExpiryWarning. Call with r.mu held.
func (r *certReloader) warnExpiry(now time.Time) {
	left := r.cert.Leaf.NotAfter.Sub(now)
	if left > certExpiryWarning || now.Sub(r.expiryAt) < 24*time.Hour {
		return
	}
	r.expiryAt = now
	r.log.Warn("cache listener: the TLS certificate expires soon; check its renewal",
		"tlsCertFile", r.certFile, "notAfter", r.cert.Leaf.NotAfter, "left", left.Round(time.Hour).String())
}

func (r *certReloader) tlsConfig() *tls.Config {
	// HTTP/1.1 only, as on the plain listener: the cache handler's
	// connection deadlines and per-peer limits were built for it.
	return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: r.GetCertificate, NextProtos: []string{"http/1.1"}}
}
