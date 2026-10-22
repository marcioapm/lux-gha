package cache

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/base64"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// memStore is an in-memory store. Conditional assembly is first-writer-wins
// like S3 with If-None-Match: *.
type memStore struct {
	mu      sync.Mutex
	objs    map[string]memObj
	uploads []pendingUpload
	now     func() time.Time
	// failDelete makes deleteKeys report these keys as failed.
	failDelete map[string]bool
	// failList makes a listing of exactly this prefix deliver half its
	// objects and then fail, as a listing that breaks on a later page.
	failList map[string]error
	// failHead makes head of these keys fail.
	failHead map[string]error
	// lastWriterWins disables the conditional write.
	lastWriterWins bool
	deleteCalls    int
}

type memObj struct {
	data []byte
	mod  time.Time
	// meta is the reservation that assembled the object.
	meta string
}

func newMemStore(now func() time.Time) *memStore {
	return &memStore{objs: map[string]memObj{}, now: now}
}

func (m *memStore) head(_ context.Context, key string) (object, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.failHead[key]; err != nil {
		return object{}, err
	}
	o, ok := m.objs[key]
	if !ok {
		return object{}, errNotFound
	}
	return object{Key: key, Size: int64(len(o.data)), LastModified: o.mod}, nil
}

func (m *memStore) list(_ context.Context, prefix string, fn func(object) bool) error {
	m.mu.Lock()
	var out []object
	for k, o := range m.objs {
		if strings.HasPrefix(k, prefix) {
			out = append(out, object{Key: k, Size: int64(len(o.data)), LastModified: o.mod})
		}
	}
	failErr := m.failList[prefix]
	m.mu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	if failErr != nil {
		out = out[:len(out)/2]
	}
	for _, o := range out {
		if !fn(o) {
			return nil
		}
	}
	return failErr
}

func (m *memStore) put(_ context.Context, key string, body io.Reader, size int64, sum string) error {
	b, err := io.ReadAll(io.LimitReader(body, size))
	if err != nil {
		return err
	}
	if int64(len(b)) != size {
		return fmt.Errorf("short body: %d of %d bytes", len(b), size)
	}
	if sum != "" {
		h := md5.Sum(b)
		if base64.StdEncoding.EncodeToString(h[:]) != sum {
			return errBadDigest
		}
	}
	m.set(key, b)
	return nil
}

func (m *memStore) set(key string, b []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objs[key] = memObj{data: b, mod: m.now()}
}

func (m *memStore) setAt(key string, b []byte, mod time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objs[key] = memObj{data: b, mod: mod}
}

func (m *memStore) get(key string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	o, ok := m.objs[key]
	return o.data, ok
}

func (m *memStore) keys(prefix string) []string {
	var ks []string
	_ = m.list(context.Background(), prefix, func(o object) bool { ks = append(ks, o.Key); return true })
	return ks
}

func (m *memStore) getSmall(_ context.Context, key string, limit int64) ([]byte, error) {
	b, ok := m.get(key)
	if !ok {
		return nil, errNotFound
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("too large")
	}
	return b, nil
}

func (m *memStore) deleteKeys(_ context.Context, keys []string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.deleteCalls++
	if len(keys) > deleteBatch {
		// S3 rejects the whole DeleteObjects request.
		return nil, fmt.Errorf("MalformedXML: %d keys", len(keys))
	}
	var failed []string
	for _, k := range keys {
		if m.failDelete[k] {
			failed = append(failed, k)
			continue
		}
		delete(m.objs, k)
	}
	return failed, nil
}

func (m *memStore) assemble(_ context.Context, dst string, parts []string) error {
	var buf bytes.Buffer
	for _, p := range parts {
		b, ok := m.get(p)
		if !ok {
			return errNotFound
		}
		buf.Write(b)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.objs[dst]; ok && !m.lastWriterWins {
		return errExists
	}
	m.objs[dst] = memObj{data: buf.Bytes(), mod: m.now()}
	return nil
}

func (m *memStore) presignGet(_ context.Context, key string, ttl time.Duration) (string, error) {
	return "https://s3.example.test/cache-bucket/" + url.PathEscape(key) + "?ttl=" + ttl.String(), nil
}

func (m *memStore) listUploads(_ context.Context, prefix string, fn func(pendingUpload) bool) error {
	m.mu.Lock()
	ups := append([]pendingUpload(nil), m.uploads...)
	m.mu.Unlock()
	for _, u := range ups {
		if strings.HasPrefix(u.Key, prefix) && !fn(u) {
			break
		}
	}
	return nil
}

func (m *memStore) abortUpload(_ context.Context, u pendingUpload) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, x := range m.uploads {
		if x.UploadID == u.UploadID {
			m.uploads = append(m.uploads[:i], m.uploads[i+1:]...)
			return nil
		}
	}
	return errNotFound
}
