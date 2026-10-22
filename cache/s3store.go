package cache

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// S3Options names the bucket. Credentials come from the AWS default chain.
type S3Options struct {
	Bucket string
	Region string
	// Endpoint is an S3-compatible server, addressed path-style; empty is AWS.
	Endpoint string
}

type s3Store struct {
	c       *s3.Client
	presign *s3.PresignClient
	bucket  string
	// plainHTTP endpoints cannot carry a trailing checksum or sign an
	// unseekable body, so uploads there send UNSIGNED-PAYLOAD.
	plainHTTP bool
}

func newS3Store(ctx context.Context, o S3Options) (*s3Store, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(o.Region))
	if err != nil {
		return nil, fmt.Errorf("aws config: %w", err)
	}
	st := &s3Store{bucket: o.Bucket, plainHTTP: strings.HasPrefix(o.Endpoint, "http://")}
	st.c = s3.NewFromConfig(cfg, func(so *s3.Options) {
		if o.Endpoint != "" {
			so.BaseEndpoint = aws.String(o.Endpoint)
			so.UsePathStyle = true
		}
		if st.plainHTTP {
			so.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		}
	})
	st.presign = s3.NewPresignClient(st.c)
	return st, nil
}

func isNotFound(err error) bool {
	var nf *types.NotFound
	var nk *types.NoSuchKey
	if errors.As(err, &nf) || errors.As(err, &nk) {
		return true
	}
	var ae smithy.APIError
	return errors.As(err, &ae) && (ae.ErrorCode() == "NotFound" || ae.ErrorCode() == "NoSuchKey")
}

func (s *s3Store) head(ctx context.Context, key string) (object, error) {
	out, err := s.c.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		if isNotFound(err) {
			return object{}, errNotFound
		}
		return object{}, err
	}
	return object{Key: key, Size: aws.ToInt64(out.ContentLength), LastModified: aws.ToTime(out.LastModified)}, nil
}

func (s *s3Store) list(ctx context.Context, prefix string, fn func(object) bool) error {
	p := s3.NewListObjectsV2Paginator(s.c, &s3.ListObjectsV2Input{Bucket: &s.bucket, Prefix: &prefix})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, o := range page.Contents {
			if !fn(object{Key: aws.ToString(o.Key), Size: aws.ToInt64(o.Size), LastModified: aws.ToTime(o.LastModified)}) {
				return nil
			}
		}
	}
	return nil
}

func (s *s3Store) put(ctx context.Context, key string, body io.Reader, size int64, md5 string) error {
	in := &s3.PutObjectInput{Bucket: &s.bucket, Key: &key, Body: io.LimitReader(body, size), ContentLength: aws.Int64(size)}
	if md5 != "" {
		in.ContentMD5 = aws.String(md5)
	}
	var opts []func(*s3.Options)
	if s.plainHTTP {
		opts = append(opts, func(o *s3.Options) {
			o.APIOptions = append(o.APIOptions, v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware)
		})
	}
	_, err := s.c.PutObject(ctx, in, opts...)
	return err
}

func (s *s3Store) getSmall(ctx context.Context, key string, limit int64) ([]byte, error) {
	out, err := s.c.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		if isNotFound(err) {
			return nil, errNotFound
		}
		return nil, err
	}
	defer out.Body.Close()
	b, err := io.ReadAll(io.LimitReader(out.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s exceeds %d bytes", key, limit)
	}
	return b, nil
}

func (s *s3Store) deleteKeys(ctx context.Context, keys []string) ([]string, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	ids := make([]types.ObjectIdentifier, len(keys))
	for i := range keys {
		ids[i] = types.ObjectIdentifier{Key: aws.String(keys[i])}
	}
	out, err := s.c.DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: &s.bucket,
		Delete: &types.Delete{Objects: ids, Quiet: aws.Bool(true)},
	})
	if err != nil {
		return nil, err
	}
	var failed []string
	for _, e := range out.Errors {
		failed = append(failed, aws.ToString(e.Key))
	}
	return failed, nil
}

// assemble copies the parts server-side into a multipart upload of dst and
// completes it with If-None-Match: *, so the first finalize wins and dst
// appears whole or not at all. A server that ignores the condition makes
// the last complete win, still atomically.
func (s *s3Store) assemble(ctx context.Context, dst string, parts []string) (err error) {
	if _, err := s.head(ctx, dst); err == nil {
		return errExists
	} else if !errors.Is(err, errNotFound) {
		return err
	}
	mu, err := s.c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: &s.bucket, Key: &dst, ContentType: aws.String("application/octet-stream"),
	})
	if err != nil {
		return err
	}
	completed := false
	defer func() {
		if !completed {
			// A fresh context: the request's may be what failed.
			actx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			_, _ = s.c.AbortMultipartUpload(actx, &s3.AbortMultipartUploadInput{Bucket: &s.bucket, Key: &dst, UploadId: mu.UploadId})
		}
	}()
	done := make([]types.CompletedPart, len(parts))
	for i, p := range parts {
		n := int32(i + 1)
		out, err := s.c.UploadPartCopy(ctx, &s3.UploadPartCopyInput{
			Bucket: &s.bucket, Key: &dst, UploadId: mu.UploadId, PartNumber: aws.Int32(n),
			CopySource: aws.String(s.bucket + "/" + copySourceEscape(p)),
		})
		if err != nil {
			if isNotFound(err) {
				return errNotFound
			}
			return fmt.Errorf("copy part %d: %w", n, err)
		}
		done[i] = types.CompletedPart{PartNumber: aws.Int32(n), ETag: out.CopyPartResult.ETag}
	}
	_, err = s.c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: &s.bucket, Key: &dst, UploadId: mu.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: done},
		IfNoneMatch:     aws.String("*"),
	})
	if err != nil {
		var re interface{ HTTPStatusCode() int }
		if errors.As(err, &re) && (re.HTTPStatusCode() == http.StatusPreconditionFailed || re.HTTPStatusCode() == http.StatusConflict) {
			return errExists
		}
		return err
	}
	completed = true
	return nil
}

// copySourceEscape URL-escapes a key for x-amz-copy-source, keeping '/'.
func copySourceEscape(key string) string {
	var b strings.Builder
	for i := 0; i < len(key); i++ {
		c := key[i]
		if c == '/' || c == '-' || c == '_' || c == '.' || c == '~' ||
			('a' <= c && c <= 'z') || ('A' <= c && c <= 'Z') || ('0' <= c && c <= '9') {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func (s *s3Store) presignGet(ctx context.Context, key string, ttl time.Duration) (string, error) {
	req, err := s.presign.PresignGetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", err
	}
	return req.URL, nil
}

func (s *s3Store) listUploads(ctx context.Context, prefix string, fn func(pendingUpload) bool) error {
	p := s3.NewListMultipartUploadsPaginator(s.c, &s3.ListMultipartUploadsInput{Bucket: &s.bucket, Prefix: &prefix})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return err
		}
		for _, u := range page.Uploads {
			if !fn(pendingUpload{Key: aws.ToString(u.Key), UploadID: aws.ToString(u.UploadId), Initiated: aws.ToTime(u.Initiated)}) {
				return nil
			}
		}
	}
	return nil
}

func (s *s3Store) abortUpload(ctx context.Context, u pendingUpload) error {
	_, err := s.c.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: &s.bucket, Key: &u.Key, UploadId: &u.UploadID})
	return err
}
