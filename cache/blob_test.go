package cache

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestUploadTokenMintAndOpen(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	s := signer{keys: [][]byte{[]byte("key-one-key-one-key-one-key-one!")}}
	tok := s.mint("staging/abc", now.Add(time.Hour))
	if got, err := s.open(tok, now); err != nil || got != "staging/abc" {
		t.Fatalf("open = %q, %v", got, err)
	}
	if _, err := s.open(tok, now.Add(time.Hour)); err != errExpiredToken {
		t.Fatalf("at expiry: %v", err)
	}
	other := signer{keys: [][]byte{[]byte("key-two-key-two-key-two-key-two!")}}
	if _, err := other.open(tok, now); err != errBadToken {
		t.Fatalf("other key: %v", err)
	}
	// Rotation: a new key first still accepts tokens minted with the old.
	rotated := signer{keys: [][]byte{other.keys[0], s.keys[0]}}
	if _, err := rotated.open(tok, now); err != nil {
		t.Fatalf("rotated: %v", err)
	}
	if _, err := s.open(rotated.mint("x", now.Add(time.Hour)), now); err != errBadToken {
		t.Fatal("a token minted with the new key opened with only the old")
	}
	p, m, _ := strings.Cut(tok, ".")
	payload, _ := base64.RawURLEncoding.DecodeString(p)
	payload[len(payload)-1] ^= 1
	for _, bad := range []string{
		"", "x", tok + "x", p + "." + m[:42] + "A",
		base64.RawURLEncoding.EncodeToString(payload) + "." + m,
		strings.Repeat("A", 3000) + "." + m,
	} {
		if _, err := s.open(bad, now); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func (h *harness) reserve(token, key, version string) string {
	h.t.Helper()
	c := h.create(token, key, version)
	if !c.ok() {
		h.t.Fatalf("create: %+v", c)
	}
	return str(c.body["signed_upload_url"])
}

func blockID(i int) string {
	return base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf("prefix-%06d", i)))
}

func blockListXML(ids ...string) []byte {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8" standalone="yes"?><BlockList>`)
	for _, id := range ids {
		b.WriteString("<Latest>" + id + "</Latest>")
	}
	b.WriteString("</BlockList>")
	return []byte(b.String())
}

func TestUploadURLIsOursAndAzureParsable(t *testing.T) {
	h := newHarness(t)
	u, err := url.Parse(h.reserve(h.is.token(t, "100", writeMain()), "k", v1))
	if err != nil {
		t.Fatal(err)
	}
	pub, _ := url.Parse(h.http.URL)
	if u.Host != pub.Host || u.Path != blobPath || u.Query().Get("sig") == "" {
		t.Fatalf("upload URL %s", u)
	}
	// The Azure SDK takes /account/container/blob on an IP host and
	// /container/blob otherwise; both must yield non-empty names.
	if parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/"); len(parts) != 3 || parts[2] == "" {
		t.Fatalf("path %q", u.Path)
	}
}

func TestMultiBlockUploadAssemblesInListOrder(t *testing.T) {
	h := newHarness(t)
	tok := h.is.token(t, "100", writeMain())
	up := h.reserve(tok, "big", v1)
	a := bytes.Repeat([]byte("a"), minPartBytes)
	b := bytes.Repeat([]byte("b"), minPartBytes+1)
	c := []byte("tail")
	// Staged out of order, as the SDK's parallel workers do.
	for _, blk := range []struct {
		i    int
		data []byte
	}{{2, c}, {0, a}, {1, b}} {
		sum := md5.Sum(blk.data)
		resp, body := h.put(up, "comp=block&blockid="+url.QueryEscape(blockID(blk.i)),
			map[string]string{"Content-MD5": base64.StdEncoding.EncodeToString(sum[:])}, blk.data)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("block %d: %d %s", blk.i, resp.StatusCode, body)
		}
		for _, hd := range []string{"x-ms-request-id", "x-ms-version", "ETag", "Last-Modified", "Content-MD5"} {
			if resp.Header.Get(hd) == "" {
				t.Errorf("block response lacks %s", hd)
			}
		}
	}
	// Finalize before the block list: nothing to finalize.
	if f := h.finalize(tok, "big", v1, len(a)+len(b)+len(c)); f.ok() {
		t.Fatalf("finalize before commit: %+v", f)
	}
	resp, body := h.put(up, "comp=blocklist", map[string]string{"Content-Type": "application/xml; charset=utf-8"},
		blockListXML(blockID(0), blockID(1), blockID(2)))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("block list: %d %s", resp.StatusCode, body)
	}
	// Committed but not finalized: invisible to restore.
	if r := h.restore(tok, "big", v1); r.body["ok"] != nil {
		t.Fatalf("visible before finalize: %+v", r)
	}
	if f := h.finalize(tok, "big", v1, len(a)+len(b)+len(c)); !f.ok() || str(f.body["entry_id"]) == "" {
		t.Fatalf("finalize: %+v", f)
	}
	got := h.restored(h.restore(tok, "big", v1))
	if !bytes.Equal(got, append(append(append([]byte{}, a...), b...), c...)) {
		t.Fatalf("assembled %d bytes in the wrong order", len(got))
	}
	if left := h.st.keys(h.srv.lay.stagingRoot()); len(left) != 0 {
		t.Fatalf("staging left behind: %q", left)
	}
}

func TestSmallNonFinalBlockIsRefused(t *testing.T) {
	h := newHarness(t)
	up := h.reserve(h.is.token(t, "100", writeMain()), "k", v1)
	for i := range 2 {
		if resp, body := h.put(up, "comp=block&blockid="+url.QueryEscape(blockID(i)), nil, make([]byte, 1<<20)); resp.StatusCode != http.StatusCreated {
			t.Fatalf("block: %d %s", resp.StatusCode, body)
		}
	}
	resp, body := h.put(up, "comp=blocklist", nil, blockListXML(blockID(0), blockID(1)))
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(body, "at least 5242880 bytes") ||
		resp.Header.Get("x-ms-error-code") != "InvalidBlockList" {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
}

func TestBlockListErrors(t *testing.T) {
	h := newHarness(t)
	up := h.reserve(h.is.token(t, "100", writeMain()), "k", v1)
	h.put(up, "comp=block&blockid="+url.QueryEscape(blockID(0)), nil, []byte("x"))
	for name, body := range map[string][]byte{
		"missing block": blockListXML(blockID(0), blockID(9)),
		"duplicate":     blockListXML(blockID(0), blockID(0)),
		"empty":         blockListXML(),
		"not XML":       []byte("<BlockList"),
		"bad id":        blockListXML("!!"),
		"bad element":   []byte("<BlockList><Other>" + blockID(0) + "</Other></BlockList>"),
	} {
		if resp, b := h.put(up, "comp=blocklist", nil, body); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d %s", name, resp.StatusCode, b)
		}
	}
	if resp, _ := h.put(up, "comp=block&blockid=", nil, []byte("x")); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("empty block id: %d", resp.StatusCode)
	}
	if resp, _ := h.put(up, "comp=appendblock", nil, []byte("x")); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("unsupported comp: %d", resp.StatusCode)
	}
	if resp, _ := h.put(up, "", map[string]string{"x-ms-blob-type": "PageBlob"}, []byte("x")); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("page blob: %d", resp.StatusCode)
	}
	if resp, _ := h.put(up, "", map[string]string{"x-ms-blob-type": "BlockBlob", "Content-MD5": base64.StdEncoding.EncodeToString(make([]byte, 16))}, []byte("x")); resp.StatusCode < 400 {
		t.Errorf("wrong Content-MD5 accepted: %d", resp.StatusCode)
	}
}

func TestUploadURLIsBoundToItsReservation(t *testing.T) {
	h := newHarness(t)
	tok := h.is.token(t, "100", writeMain())
	upA := h.reserve(tok, "a", v1)
	h.putBlobOK(upA, []byte("for-a"))
	// Finalizing b, or a with another version, finds nothing uploaded.
	h.reserve(tok, "b", v1)
	if f := h.finalize(tok, "b", v1, 5); f.ok() {
		t.Fatalf("finalize b used a's upload: %+v", f)
	}
	h.reserve(tok, "a", v2)
	if f := h.finalize(tok, "a", v2, 5); f.ok() {
		t.Fatalf("finalize with another version: %+v", f)
	}
	// Another repository's token, or another job's token, cannot finalize it.
	if f := h.finalize(h.is.token(t, "200", writeMain()), "a", v1, 5); f.ok() {
		t.Fatalf("cross-repo finalize: %+v", f)
	}
	// A wrong-size finalize is refused and leaves the upload intact.
	if f := h.finalize(tok, "a", v1, 6); f.ok() || !strings.Contains(str(f.body["message"]), "uploaded 5 bytes") {
		t.Fatalf("size mismatch: %+v", f)
	}
	if f := h.finalize(tok, "a", v1, 5); !f.ok() {
		t.Fatalf("finalize a: %+v", f)
	}
}

func TestUploadURLForgedOrExpiredIs403(t *testing.T) {
	h := newHarness(t)
	up := h.reserve(h.is.token(t, "100", writeMain()), "k", v1)
	u, _ := url.Parse(up)
	sig := u.Query().Get("sig")
	forged := *u
	forged.RawQuery = url.Values{"sig": {sig[:len(sig)-1] + map[bool]string{true: "B", false: "A"}[sig[len(sig)-1] == 'A']}}.Encode()
	none := *u
	none.RawQuery = ""
	for _, bad := range []string{forged.String(), none.String() + "?x=1"} {
		resp, body := h.put(bad, "", map[string]string{"x-ms-blob-type": "BlockBlob"}, []byte("x"))
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(body, "AuthenticationFailed") {
			t.Fatalf("%s: %d %s", bad, resp.StatusCode, body)
		}
	}
	h.clk.add(uploadURLTTL)
	if resp, _ := h.put(up, "", map[string]string{"x-ms-blob-type": "BlockBlob"}, []byte("x")); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expired URL: %d", resp.StatusCode)
	}
	if len(h.st.keys("")) != 0 {
		t.Fatalf("rejected uploads stored %q", h.st.keys(""))
	}
}

func TestUploadSizeBounds(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.MaxEntryBytes = 2*minPartBytes + 10 })
	up := h.reserve(h.is.token(t, "100", writeMain()), "k", v1)
	// A single shot over the entry bound is refused before its body is read.
	resp, body := h.put(up, "", map[string]string{"x-ms-blob-type": "BlockBlob"}, make([]byte, 2*minPartBytes+11))
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("single shot: %d %s", resp.StatusCode, body)
	}
	// Blocks: the third pushes the staged total over the bound.
	for i, n := range []int{minPartBytes, minPartBytes} {
		if resp, body := h.put(up, "comp=block&blockid="+url.QueryEscape(blockID(i)), nil, make([]byte, n)); resp.StatusCode != http.StatusCreated {
			t.Fatalf("block %d: %d %s", i, resp.StatusCode, body)
		}
	}
	if resp, _ := h.put(up, "comp=block&blockid="+url.QueryEscape(blockID(2)), nil, make([]byte, 11)); resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("overflowing block: %d", resp.StatusCode)
	}
	// Re-staging an existing block replaces it rather than adding to it.
	if resp, _ := h.put(up, "comp=block&blockid="+url.QueryEscape(blockID(1)), nil, make([]byte, minPartBytes+10)); resp.StatusCode != http.StatusCreated {
		t.Fatalf("restaged block: %d", resp.StatusCode)
	}
	if resp, _ := h.put(up, "comp=blocklist", nil, blockListXML(blockID(0), blockID(1))); resp.StatusCode != http.StatusCreated {
		t.Fatalf("block list at the bound: %d", resp.StatusCode)
	}
}

func TestEmptyArchiveRefused(t *testing.T) {
	h := newHarness(t)
	up := h.reserve(h.is.token(t, "100", writeMain()), "k", v1)
	if resp, _ := h.put(up, "", map[string]string{"x-ms-blob-type": "BlockBlob"}, nil); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty: %d", resp.StatusCode)
	}
}

// Two jobs saving one key and version: one wins, the other is told so, and
// a reader sees exactly one complete archive.
func TestConcurrentSavesOfOneKey(t *testing.T) {
	for _, lww := range []bool{false, true} {
		t.Run(fmt.Sprintf("lastWriterWins=%v", lww), func(t *testing.T) {
			h := newHarness(t)
			h.st.lastWriterWins = lww
			const jobs = 8
			toks := make([]string, jobs)
			ups := make([]string, jobs)
			for i := range jobs {
				// Distinct jobs have distinct runtime tokens.
				c := h.is.claims(t, "100", writeMain())
				c["jti"] = fmt.Sprint(i)
				toks[i] = h.is.sign(t, c)
				ups[i] = h.reserve(toks[i], "k", v1)
			}
			payload := func(i int) []byte { return bytes.Repeat([]byte{byte('a' + i)}, 1000) }
			var wg sync.WaitGroup
			oks := make([]bool, jobs)
			for i := range jobs {
				wg.Add(1)
				go func() {
					defer wg.Done()
					h.putBlobOK(ups[i], payload(i))
					oks[i] = h.finalize(toks[i], "k", v1, 1000).ok()
				}()
			}
			wg.Wait()
			won := 0
			for _, ok := range oks {
				if ok {
					won++
				}
			}
			if !lww && won != 1 {
				t.Fatalf("%d finalizes succeeded, want 1", won)
			}
			got := h.restored(h.restore(toks[0], "k", v1))
			if len(got) != 1000 || !bytes.Equal(got, bytes.Repeat(got[:1], 1000)) {
				t.Fatalf("restored a mixed or partial archive")
			}
		})
	}
}
