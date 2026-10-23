package cache

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeS3 is a path-style S3 endpoint serving the calls assemble and put
// make, recording what it received. Hooks decide each answer.
type fakeS3 struct {
	t   *testing.T
	srv *httptest.Server

	mu sync.Mutex
	// exists holds HEAD-able keys and their x-amz-meta-reservation.
	exists map[string]string
	// copies maps part number to the raw x-amz-copy-source.
	copies      map[int]string
	copyStarted int
	completes   []fakeComplete
	aborts      int
	creates     int
	createMeta  string

	// copyHook answers a part copy; nil copies successfully.
	copyHook func(r *http.Request, part int) (status int, code string)
	// completeStatus, when set, answers CompleteMultipartUpload.
	completeStatus int
	completeCode   string
	putStatus      int
	putCode        string
}

type fakeComplete struct {
	ifNoneMatch string
	parts       []struct {
		PartNumber int
		ETag       string
	}
}

const fakeBucket = "cache-bucket"

func newFakeS3(t *testing.T) (*fakeS3, *s3Store) {
	t.Helper()
	f := &fakeS3{t: t, exists: map[string]string{}, copies: map[int]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	// Static credentials and no shared config keep the AWS default chain
	// off the network.
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_CONFIG_FILE", t.TempDir()+"/none")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", t.TempDir()+"/none")
	st, err := newS3Store(context.Background(), S3Options{Bucket: fakeBucket, Region: "us-east-1", Endpoint: f.srv.URL})
	if err != nil {
		t.Fatal(err)
	}
	return f, st
}

func s3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, code)
}

func (f *fakeS3) serve(w http.ResponseWriter, r *http.Request) {
	key, ok := strings.CutPrefix(r.URL.Path, "/"+fakeBucket+"/")
	if !ok {
		s3Error(w, http.StatusNotFound, "NoSuchBucket")
		return
	}
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodHead:
		f.mu.Lock()
		meta, ok := f.exists[key]
		f.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", "1")
		w.Header().Set("Last-Modified", time.Now().UTC().Format(http.TimeFormat))
		w.Header().Set("x-amz-meta-reservation", meta)
	case r.Method == http.MethodPost && q.Has("uploads"):
		f.mu.Lock()
		f.creates++
		f.createMeta = r.Header.Get("x-amz-meta-reservation")
		f.mu.Unlock()
		fmt.Fprintf(w, `<InitiateMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><UploadId>up-1</UploadId></InitiateMultipartUploadResult>`, fakeBucket, key)
	case r.Method == http.MethodPut && q.Has("partNumber"):
		n, _ := strconv.Atoi(q.Get("partNumber"))
		f.mu.Lock()
		f.copyStarted++
		f.copies[n] = r.Header.Get("x-amz-copy-source")
		hook := f.copyHook
		f.mu.Unlock()
		if hook != nil {
			if status, code := hook(r, n); status != 0 {
				s3Error(w, status, code)
				return
			}
		}
		fmt.Fprintf(w, `<CopyPartResult><ETag>"etag-%d"</ETag><LastModified>2026-01-01T00:00:00.000Z</LastModified></CopyPartResult>`, n)
	case r.Method == http.MethodPost && q.Has("uploadId"):
		var body struct {
			Parts []struct {
				PartNumber int
				ETag       string
			} `xml:"Part"`
		}
		b, _ := io.ReadAll(r.Body)
		if err := xml.Unmarshal(b, &body); err != nil {
			f.t.Errorf("complete body %q: %v", b, err)
		}
		f.mu.Lock()
		f.completes = append(f.completes, fakeComplete{ifNoneMatch: r.Header.Get("If-None-Match"), parts: body.Parts})
		f.mu.Unlock()
		if f.completeStatus != 0 {
			s3Error(w, f.completeStatus, f.completeCode)
			return
		}
		fmt.Fprintf(w, `<CompleteMultipartUploadResult><Bucket>%s</Bucket><Key>%s</Key><ETag>"done"</ETag></CompleteMultipartUploadResult>`, fakeBucket, key)
	case r.Method == http.MethodDelete && q.Has("uploadId"):
		f.mu.Lock()
		f.aborts++
		f.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodPut:
		io.Copy(io.Discard, r.Body)
		if f.putStatus != 0 {
			s3Error(w, f.putStatus, f.putCode)
			return
		}
		w.Header().Set("ETag", `"put"`)
	default:
		f.t.Errorf("unexpected %s %s", r.Method, r.URL)
		s3Error(w, http.StatusNotImplemented, "NotImplemented")
	}
}

func TestS3AssembleRequests(t *testing.T) {
	f, st := newFakeS3(t)
	parts := []string{"p/staging/r/block/00", "p/staging/r/a%b c/é", "p/staging/r/block/02"}
	if err := st.assemble(context.Background(), "p/v1/entry", parts, "p/staging/r"); err != nil {
		t.Fatal(err)
	}
	if f.createMeta != "p/staging/r" {
		t.Errorf("reservation metadata %q", f.createMeta)
	}
	want := map[int]string{
		1: fakeBucket + "/p/staging/r/block/00",
		2: fakeBucket + "/p/staging/r/a%25b%20c/%C3%A9",
		3: fakeBucket + "/p/staging/r/block/02",
	}
	for n, src := range want {
		if f.copies[n] != src {
			t.Errorf("part %d copy source %q, want %q", n, f.copies[n], src)
		}
		// It decodes to the staged key.
		if dec, _ := url.PathUnescape(f.copies[n]); dec != fakeBucket+"/"+parts[n-1] {
			t.Errorf("part %d decodes to %q", n, dec)
		}
	}
	if len(f.completes) != 1 {
		t.Fatalf("%d completes", len(f.completes))
	}
	c := f.completes[0]
	if c.ifNoneMatch != "*" {
		t.Errorf("CompleteMultipartUpload If-None-Match %q", c.ifNoneMatch)
	}
	for i, p := range c.parts {
		if p.PartNumber != i+1 || p.ETag != fmt.Sprintf(`"etag-%d"`, i+1) {
			t.Errorf("completed part %d: %+v", i, p)
		}
	}
	if len(c.parts) != len(parts) || f.aborts != 0 {
		t.Errorf("parts %d aborts %d", len(c.parts), f.aborts)
	}
}

func TestS3AssembleOutcomes(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name   string
		setup  func(*fakeS3)
		want   error
		aborts int
	}{
		{"412 on complete", func(f *fakeS3) { f.completeStatus, f.completeCode = 412, "PreconditionFailed" }, errExists, 1},
		{"409 on complete", func(f *fakeS3) { f.completeStatus, f.completeCode = 409, "ConditionalRequestConflict" }, errExists, 1},
		{"part missing", func(f *fakeS3) {
			f.copyHook = func(_ *http.Request, n int) (int, string) {
				if n == 2 {
					return 404, "NoSuchKey"
				}
				return 0, ""
			}
		}, errNotFound, 1},
		{"exists before", func(f *fakeS3) { f.exists["p/v1/entry"] = "" }, errExists, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, st := newFakeS3(t)
			c.setup(f)
			err := st.assemble(ctx, "p/v1/entry", []string{"p/s/0", "p/s/1", "p/s/2"}, "r")
			if !errors.Is(err, c.want) {
				t.Fatalf("err %v, want %v", err, c.want)
			}
			if f.aborts != c.aborts {
				t.Fatalf("%d aborts, want %d", f.aborts, c.aborts)
			}
			if c.name == "exists before" && f.creates != 0 {
				t.Fatal("a multipart upload was started for an existing entry")
			}
		})
	}
}

// The first failed part copy cancels the copies in flight and starts no
// more.
func TestS3AssembleCancelsCopiesOnFirstFailure(t *testing.T) {
	f, st := newFakeS3(t)
	cancelled := make(chan struct{}, 100)
	// release frees copies that were never cancelled; the cleanup runs
	// before the fake server's, so a failing test strands no handler.
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	f.copyHook = func(r *http.Request, n int) (int, string) {
		if n == 1 {
			// Fail once the others are in flight.
			time.Sleep(100 * time.Millisecond)
			return http.StatusForbidden, "AccessDenied"
		}
		select {
		case <-r.Context().Done():
			cancelled <- struct{}{}
		case <-release:
		}
		return http.StatusInternalServerError, "InternalError"
	}
	parts := make([]string, 40)
	for i := range parts {
		parts[i] = fmt.Sprintf("p/s/%02d", i)
	}
	done := make(chan error, 1)
	go func() { done <- st.assemble(context.Background(), "p/v1/entry", parts, "r") }()
	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("assemble did not return within 5 s: the failed copy cancelled nothing")
	}
	if err == nil || errors.Is(err, errNotFound) || !strings.Contains(err.Error(), "copy part 1") {
		t.Fatalf("err %v", err)
	}
	f.mu.Lock()
	started := f.copyStarted
	f.mu.Unlock()
	if started > copyConcurrency {
		t.Fatalf("%d copies started after the first failed, want at most %d", started, copyConcurrency)
	}
	// Every copy that reached the server, but part 1, sees its request
	// cancelled; the server notices the closed connection asynchronously.
	deadline := time.After(5 * time.Second)
	for got := 0; got < started-1; got++ {
		select {
		case <-cancelled:
		case <-deadline:
			t.Fatalf("%d of %d in-flight copies were cancelled", got, started-1)
		}
	}
	if f.aborts != 1 || len(f.completes) != 0 {
		t.Fatalf("aborts %d completes %d", f.aborts, len(f.completes))
	}
}

func TestS3PutBadDigest(t *testing.T) {
	f, st := newFakeS3(t)
	f.putStatus, f.putCode = http.StatusBadRequest, "BadDigest"
	err := st.put(context.Background(), "p/s/x", strings.NewReader("hello"), 5, "AAAAAAAAAAAAAAAAAAAAAA==")
	if !errors.Is(err, errBadDigest) {
		t.Fatalf("%v", err)
	}
}

func TestS3HeadReadsReservation(t *testing.T) {
	f, st := newFakeS3(t)
	f.exists["p/v1/entry"] = "p/staging/r"
	if o, err := st.head(context.Background(), "p/v1/entry"); err != nil || o.Owner != "p/staging/r" {
		t.Fatalf("%+v %v", o, err)
	}
	if _, err := st.head(context.Background(), "p/v1/none"); !errors.Is(err, errNotFound) {
		t.Fatalf("missing: %v", err)
	}
}
