//go:build s3integration

package cache

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// The S3 integration tests build only with -tags s3integration (CI fails
// on any skipped test, so they are left out rather than skipped) and run
// against LUXGHA_CACHE_S3_ENDPOINT, an S3-compatible server, and
// LUXGHA_CACHE_S3_BUCKET, created if missing, with credentials from the
// AWS default chain, e.g. AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY.
// Each test works under its own random prefix.
func s3TestStore(t *testing.T) (*s3Store, string) {
	t.Helper()
	endpoint, bucket := os.Getenv("LUXGHA_CACHE_S3_ENDPOINT"), os.Getenv("LUXGHA_CACHE_S3_BUCKET")
	if endpoint == "" || bucket == "" {
		t.Fatal("LUXGHA_CACHE_S3_ENDPOINT and LUXGHA_CACHE_S3_BUCKET are required")
	}
	region := os.Getenv("AWS_REGION")
	if region == "" {
		region = "us-east-1"
	}
	ctx := context.Background()
	st, err := newS3Store(ctx, S3Options{Bucket: bucket, Region: region, Endpoint: endpoint})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.c.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &bucket}); err != nil {
		if _, err := st.c.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &bucket}); err != nil {
			t.Fatalf("create bucket: %v", err)
		}
	}
	var r [6]byte
	rand.Read(r[:])
	return st, fmt.Sprintf("it-%x", r)
}

func TestS3StorePrimitives(t *testing.T) {
	st, prefix := s3TestStore(t)
	ctx := context.Background()
	k := prefix + "/a/obj"
	if _, err := st.head(ctx, k); !errors.Is(err, errNotFound) {
		t.Fatalf("head missing: %v", err)
	}
	if _, err := st.getSmall(ctx, k, 10); !errors.Is(err, errNotFound) {
		t.Fatalf("get missing: %v", err)
	}
	// An unseekable body, as a request body is.
	if err := st.put(ctx, k, io.MultiReader(strings.NewReader("hello")), 5, ""); err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := st.put(ctx, k+"-md5", strings.NewReader("hello"), 5, "XUFAKrxLKna5cZ2REBfFkg=="); err != nil {
		t.Fatalf("put with MD5: %v", err)
	}
	if err := st.put(ctx, k+"-badmd5", strings.NewReader("hello"), 5, "AAAAAAAAAAAAAAAAAAAAAA=="); err == nil {
		t.Fatal("put with a wrong MD5 succeeded")
	}
	if err := st.put(ctx, k+"-short", strings.NewReader("hel"), 5, ""); err == nil {
		t.Fatal("short body stored")
	}
	o, err := st.head(ctx, k)
	if err != nil || o.Size != 5 || o.LastModified.IsZero() {
		t.Fatalf("head: %+v %v", o, err)
	}
	if b, err := st.getSmall(ctx, k, 10); err != nil || string(b) != "hello" {
		t.Fatalf("get: %q %v", b, err)
	}
	if _, err := st.getSmall(ctx, k, 4); err == nil {
		t.Fatal("over-limit read")
	}
	var listed []string
	if err := st.list(ctx, prefix+"/", func(o object) bool { listed = append(listed, o.Key); return true }); err != nil {
		t.Fatal(err)
	}
	if strings.Join(listed, ",") != k+","+k+"-md5" {
		t.Fatalf("listed %q", listed)
	}
	u, err := st.presignGet(ctx, k, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get(u)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(b) != "hello" || resp.ContentLength != 5 {
		t.Fatalf("presigned GET: %d %q", resp.StatusCode, b)
	}
	failed, err := st.deleteKeys(ctx, []string{k, k + "-md5", prefix + "/never-existed"})
	if err != nil || len(failed) != 0 {
		t.Fatalf("delete: %v %v", failed, err)
	}
	if _, err := st.head(ctx, k); !errors.Is(err, errNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}

func TestS3StoreAssembleIsFirstWriterWins(t *testing.T) {
	st, prefix := s3TestStore(t)
	ctx := context.Background()
	partA := bytes.Repeat([]byte("a"), minPartBytes)
	partB := []byte("tail")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(st.put(ctx, prefix+"/s/1", bytes.NewReader(partA), int64(len(partA)), ""))
	must(st.put(ctx, prefix+"/s/2", bytes.NewReader(partB), int64(len(partB)), ""))
	dst := prefix + "/v1/entry"
	must(st.assemble(ctx, dst, []string{prefix + "/s/1", prefix + "/s/2"}, "owner-a"))
	b, err := st.getSmall(ctx, dst, 10<<20)
	must(err)
	if !bytes.Equal(b, append(append([]byte{}, partA...), partB...)) {
		t.Fatalf("assembled %d bytes", len(b))
	}
	if o, err := st.head(ctx, dst); err != nil || o.Owner != "owner-a" {
		t.Fatalf("owner metadata: %+v %v", o, err)
	}
	if err := st.assemble(ctx, dst, []string{prefix + "/s/2"}, "owner-b"); !errors.Is(err, errExists) {
		t.Fatalf("second assemble: %v", err)
	}
	if err := st.assemble(ctx, prefix+"/v1/other", []string{prefix + "/s/missing"}, "x"); !errors.Is(err, errNotFound) {
		t.Fatalf("missing part: %v", err)
	}

	// Racing finalizes past the HEAD check: the conditional complete
	// decides, and the object is one whole input.
	const racers = 6
	for i := range racers {
		must(st.put(ctx, fmt.Sprintf("%s/r/%d", prefix, i), bytes.NewReader(bytes.Repeat([]byte{byte('0' + i)}, 1000)), 1000, ""))
	}
	race := prefix + "/v1/race"
	var wg sync.WaitGroup
	errs := make([]error, racers)
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = st.assemble(ctx, race, []string{fmt.Sprintf("%s/r/%d", prefix, i)}, fmt.Sprint(i))
		}()
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case !errors.Is(err, errExists):
			t.Fatalf("racer error: %v", err)
		}
	}
	b, err = st.getSmall(ctx, race, 2000)
	must(err)
	if len(b) != 1000 || !bytes.Equal(b, bytes.Repeat(b[:1], 1000)) {
		t.Fatal("mixed or partial object")
	}
	t.Logf("racing finalizes: %d of %d completed (1 means the server enforced If-None-Match)", won, racers)
	if won < 1 {
		t.Fatal("no finalize completed")
	}
	if os.Getenv("LUXGHA_CACHE_S3_CONDITIONAL") == "1" && won != 1 {
		t.Fatalf("%d racers completed; the server ignores If-None-Match on CompleteMultipartUpload", won)
	}
	var left []string
	st.listUploads(ctx, prefix, func(u pendingUpload) bool { left = append(left, u.Key); return true })
	if len(left) != 0 {
		t.Fatalf("multipart uploads left open: %q", left)
	}
}

func TestS3StoreListsAndAbortsUploads(t *testing.T) {
	st, prefix := s3TestStore(t)
	ctx := context.Background()
	key := prefix + "/staging/abandoned"
	out, err := st.c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: &st.bucket, Key: aws.String(key)})
	if err != nil {
		t.Fatal(err)
	}
	var found []pendingUpload
	if err := st.listUploads(ctx, prefix, func(u pendingUpload) bool { found = append(found, u); return true }); err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Key != key || found[0].UploadID != aws.ToString(out.UploadId) || found[0].Initiated.IsZero() {
		t.Fatalf("uploads %+v", found)
	}
	if err := st.abortUpload(ctx, found[0]); err != nil {
		t.Fatal(err)
	}
	found = nil
	st.listUploads(ctx, prefix, func(u pendingUpload) bool { found = append(found, u); return true })
	if len(found) != 0 {
		t.Fatalf("after abort %+v", found)
	}
}

// Whether the server enforces If-None-Match: * on CompleteMultipartUpload,
// probed without the HEAD pre-check that assemble does first.
func TestS3ConditionalComplete(t *testing.T) {
	st, prefix := s3TestStore(t)
	ctx := context.Background()
	key := prefix + "/cond"
	if err := st.put(ctx, key, strings.NewReader("first"), 5, ""); err != nil {
		t.Fatal(err)
	}
	mu, err := st.c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: &st.bucket, Key: &key})
	if err != nil {
		t.Fatal(err)
	}
	part, err := st.c.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: &st.bucket, Key: &key, UploadId: mu.UploadId, PartNumber: aws.Int32(1),
		Body: strings.NewReader("second"), ContentLength: aws.Int64(6),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = st.c.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: &st.bucket, Key: &key, UploadId: mu.UploadId, IfNoneMatch: aws.String("*"),
		MultipartUpload: &s3types.CompletedMultipartUpload{Parts: []s3types.CompletedPart{{PartNumber: aws.Int32(1), ETag: part.ETag}}},
	})
	b, _ := st.getSmall(ctx, key, 10)
	if err == nil {
		t.Logf("server IGNORES If-None-Match on CompleteMultipartUpload: object now %q (last writer wins)", b)
		if os.Getenv("LUXGHA_CACHE_S3_CONDITIONAL") == "1" {
			t.Fatal("conditional write required but not enforced")
		}
		return
	}
	t.Logf("server enforces If-None-Match on CompleteMultipartUpload: %v; object still %q", err, b)
	if string(b) != "first" {
		t.Fatalf("object changed to %q", b)
	}
	st.c.AbortMultipartUpload(ctx, &s3.AbortMultipartUploadInput{Bucket: &st.bucket, Key: &key, UploadId: mu.UploadId})
}

// The server end to end on S3: a multi-block upload, finalize, restore by
// prefix through a presigned URL, and the reaper.
func TestS3ServerRoundTrip(t *testing.T) {
	st, prefix := s3TestStore(t)
	h := newHarness(t)
	srv, err := newServer(st, Options{
		PublicURL: h.http.URL, Issuer: h.is.URL, Upstream: "http://upstream.invalid",
		SigningKeys: [][]byte{bytes.Repeat([]byte("k"), 32)}, Prefix: prefix, RepositoryIDs: []int64{100, 200},
	})
	if err != nil {
		t.Fatal(err)
	}
	h.srv = srv
	h.http.Config.Handler = srv
	tok := h.is.token(t, "100", writeMain())
	up := h.reserve(tok, "deps-linux-1", v1)
	data := make([]byte, 2*minPartBytes+123)
	rand.Read(data)
	blocks := [][]byte{data[:minPartBytes], data[minPartBytes : 2*minPartBytes], data[2*minPartBytes:]}
	ids := make([]string, len(blocks))
	for i, b := range blocks {
		ids[i] = blockID(i)
		if resp, body := h.put(up, "comp=block&blockid="+url.QueryEscape(ids[i]), nil, b); resp.StatusCode != http.StatusCreated {
			t.Fatalf("block %d: %d %s", i, resp.StatusCode, body)
		}
	}
	if resp, body := h.put(up, "comp=blocklist", nil, blockListXML(ids...)); resp.StatusCode != http.StatusCreated {
		t.Fatalf("block list: %d %s", resp.StatusCode, body)
	}
	if f := h.finalize(tok, "deps-linux-1", v1, len(data)); !f.ok() {
		t.Fatalf("finalize: %+v", f)
	}
	r := h.restore(tok, "deps-linux-2", v1, "deps-linux-")
	if !r.ok() || r.body["matched_key"] != "deps-linux-1" {
		t.Fatalf("restore: %+v", r)
	}
	resp, err := http.Get(str(r.body["signed_download_url"]))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !bytes.Equal(got, data) {
		t.Fatalf("downloaded %d bytes, want %d identical", len(got), len(data))
	}
	var staged []string
	st.list(context.Background(), srv.lay.stagingRoot(), func(o object) bool { staged = append(staged, o.Key); return true })
	if len(staged) != 0 {
		t.Fatalf("staging left: %q", staged)
	}
	// A reaper with a TTL in the past deletes the entry.
	srv.reaper.ttl = -time.Hour
	res, _ := srv.reaper.pass(context.Background())
	if res.Entries != 1 || res.DeletedEntries != 1 || res.Errors != 0 {
		t.Fatalf("reaper %+v", res)
	}
	if r := h.restore(tok, "deps-linux-1", v1); r.body["ok"] != nil {
		t.Fatalf("restore after reap: %+v", r)
	}
}
