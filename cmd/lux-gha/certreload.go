package main

import (
	"crypto/tls"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// certCheckInterval bounds how often the certificate files are stat'ed.
const certCheckInterval = 30 * time.Second

// certReloader serves a PEM certificate and key pair and reloads it when
// either file's modification time or size changes. A pair that fails to
// load keeps the last good one in service and is logged once; it is retried
// only after the files change again.
type certReloader struct {
	certFile, keyFile string
	interval          time.Duration
	now               func() time.Time
	log               *slog.Logger

	mu      sync.Mutex
	cert    *tls.Certificate
	stamp   [2]fileStamp
	checked time.Time
}

// fileStamp is what a stat reveals of a change; err is set when the stat
// failed, so a file that stays missing is not reported again.
type fileStamp struct {
	mod  time.Time
	size int64
	err  string
}

func statStamp(path string) fileStamp {
	fi, err := os.Stat(path)
	if err != nil {
		return fileStamp{err: err.Error()}
	}
	return fileStamp{mod: fi.ModTime(), size: fi.Size()}
}

func newCertReloader(certFile, keyFile string, log *slog.Logger) (*certReloader, error) {
	r := &certReloader{certFile: certFile, keyFile: keyFile, interval: certCheckInterval, now: time.Now, log: log}
	r.stamp = r.stat()
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("cache.tlsCertFile, cache.tlsKeyFile: %w", err)
	}
	r.cert, r.checked = &cert, r.now()
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
	stamp := r.stat()
	if stamp == r.stamp {
		return r.cert, nil
	}
	r.stamp = stamp
	cert, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		r.log.Error("cache listener: TLS certificate reload failed; serving the previous certificate",
			"tlsCertFile", r.certFile, "tlsKeyFile", r.keyFile, "error", err)
		return r.cert, nil
	}
	r.cert = &cert
	r.log.Info("cache listener: TLS certificate reloaded", "tlsCertFile", r.certFile)
	return r.cert, nil
}

func (r *certReloader) tlsConfig() *tls.Config {
	// HTTP/1.1 only, as on the plain listener: the cache handler's
	// connection deadlines and per-peer limits were built for it.
	return &tls.Config{MinVersion: tls.VersionTLS12, GetCertificate: r.GetCertificate, NextProtos: []string{"http/1.1"}}
}
