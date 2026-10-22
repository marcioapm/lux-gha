package cache

import (
	"context"
	"errors"
	"io"
	"time"
)

// object is one listed or inspected stored object.
type object struct {
	Key          string
	Size         int64
	LastModified time.Time
	// Owner is the reservation that assembled an entry; head sets it,
	// list does not.
	Owner string
}

// pendingUpload is an incomplete multipart upload.
type pendingUpload struct {
	Key       string
	UploadID  string
	Initiated time.Time
}

var (
	errNotFound = errors.New("object not found")
	// errExists reports a conditional write that lost to an existing object.
	errExists = errors.New("object already exists")
	// errBadDigest reports a put whose bytes do not match its MD5.
	errBadDigest = errors.New("content MD5 mismatch")
)

// store is the object storage the cache needs. The S3 implementation is
// s3Store; tests use an in-memory one.
type store interface {
	head(ctx context.Context, key string) (object, error)
	// list calls fn for every object under prefix, in key order, until fn
	// returns false.
	list(ctx context.Context, prefix string, fn func(object) bool) error
	// put stores exactly size bytes read from body. A non-empty md5 is the
	// base64 MD5 the stored bytes must have.
	put(ctx context.Context, key string, body io.Reader, size int64, md5 string) error
	// getSmall reads an object of at most limit bytes.
	getSmall(ctx context.Context, key string, limit int64) ([]byte, error)
	// deleteKeys deletes the keys and returns the ones that failed.
	deleteKeys(ctx context.Context, keys []string) (failed []string, err error)
	// assemble writes dst as the concatenation of parts, made visible in
	// one step and recording owner (see object.Owner). It returns
	// errExists, and leaves dst unchanged, when dst already exists and the
	// store enforces conditional writes, and errNotFound when a part is
	// missing.
	assemble(ctx context.Context, dst string, parts []string, owner string) error
	presignGet(ctx context.Context, key string, ttl time.Duration) (string, error)
	listUploads(ctx context.Context, prefix string, fn func(pendingUpload) bool) error
	abortUpload(ctx context.Context, u pendingUpload) error
}
