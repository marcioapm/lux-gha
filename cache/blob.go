package cache

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// The subset of the Azure Blob service that @azure/storage-blob's
// BlockBlobClient.uploadFile sends to a signed upload URL:
//
//	PUT <url>                      x-ms-blob-type: BlockBlob, whole body (Put Blob)
//	PUT <url>&comp=block&blockid=  one block (Put Block)
//	PUT <url>&comp=blocklist       XML <BlockList> naming the blob's blocks in order (Put Block List)
//
// Each block and a whole blob is stored as one staging object; the block
// list becomes a manifest that Finalize assembles into the entry. Nothing
// here makes an entry visible.

const (
	azureVersion = "2026-02-06"
	// maxBlockBytes is Azure's Put Block limit, under S3's 5 GiB part copy limit.
	maxBlockBytes = 4000 << 20
	// maxBlobBytes is Azure's Put Blob limit.
	maxBlobBytes     = 5000 << 20
	maxBlockListBody = 4 << 20
	maxBlockIDBytes  = 64
)

type azureError struct {
	status int
	code   string
	msg    string
}

func (e *azureError) Error() string { return e.code + ": " + e.msg }

func azerr(status int, code, format string, a ...any) *azureError {
	return &azureError{status: status, code: code, msg: fmt.Sprintf(format, a...)}
}

func requestID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (s *Server) serveBlob(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	rid := requestID()
	h.Set("x-ms-request-id", rid)
	h.Set("x-ms-version", azureVersion)
	if c := r.Header.Get("x-ms-client-request-id"); c != "" {
		h.Set("x-ms-client-request-id", c)
	}
	err := s.blob(w, r)
	if err == nil {
		return
	}
	var ae *azureError
	if !errors.As(err, &ae) {
		s.log.Error("cache: upload failed", "error", err)
		ae = azerr(http.StatusInternalServerError, "InternalError", "internal error")
	}
	h.Set("x-ms-error-code", ae.code)
	h.Set("Content-Type", "application/xml")
	w.WriteHeader(ae.status)
	var b strings.Builder
	b.WriteString(xml.Header)
	b.WriteString("<Error><Code>")
	xml.EscapeText(&b, []byte(ae.code))
	b.WriteString("</Code><Message>")
	xml.EscapeText(&b, []byte(ae.msg+"\nRequestId:"+rid))
	b.WriteString("</Message></Error>")
	io.WriteString(w, b.String())
}

func (s *Server) blob(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	staging, err := s.sign.open(q.Get("sig"), s.o.Now())
	if err != nil {
		return azerr(http.StatusForbidden, "AuthenticationFailed", "%v", err)
	}
	if r.Method != http.MethodPut {
		return azerr(http.StatusMethodNotAllowed, "UnsupportedHttpVerb", "only PUT is supported")
	}
	ctx := r.Context()
	switch q.Get("comp") {
	case "":
		return s.putBlob(ctx, w, r, staging)
	case "block":
		return s.putBlock(ctx, w, r, staging, q.Get("blockid"))
	case "blocklist":
		return s.putBlockList(ctx, w, r, staging)
	}
	return azerr(http.StatusBadRequest, "UnsupportedQueryParameter", "comp=%q is not supported", q.Get("comp"))
}

// bodyLength returns the declared length, rejecting a missing one and one
// above limit before any byte is read.
func bodyLength(r *http.Request, limit int64) (int64, error) {
	if r.ContentLength < 0 {
		return 0, azerr(http.StatusLengthRequired, "MissingContentLengthHeader", "Content-Length is required")
	}
	if r.ContentLength > limit {
		return 0, azerr(http.StatusRequestEntityTooLarge, "RequestBodyTooLarge", "body of %d bytes exceeds %d", r.ContentLength, limit)
	}
	return r.ContentLength, nil
}

func (s *Server) stageObject(ctx context.Context, r *http.Request, key string, limit int64) (int64, error) {
	n, err := bodyLength(r, limit)
	if err != nil {
		return 0, err
	}
	md5 := r.Header.Get("Content-MD5")
	if md5 != "" {
		if b, err := base64.StdEncoding.DecodeString(md5); err != nil || len(b) != 16 {
			return 0, azerr(http.StatusBadRequest, "InvalidHeaderValue", "Content-MD5 is not a base64 MD5")
		}
	}
	// The store reads exactly n bytes; the transport bounds the body to
	// Content-Length, so a short body fails the write.
	if err := s.st.put(ctx, key, r.Body, n, md5); err != nil {
		if errors.Is(err, errBadDigest) {
			return 0, azerr(http.StatusBadRequest, "Md5Mismatch", "the MD5 of the body does not match Content-MD5")
		}
		return 0, fmt.Errorf("stage %s: %w", key, err)
	}
	return n, nil
}

func (s *Server) created(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	now := s.o.Now().UTC()
	h.Set("ETag", fmt.Sprintf("\"0x%X\"", now.UnixNano()))
	h.Set("Last-Modified", now.Format(http.TimeFormat))
	h.Set("Date", now.Format(http.TimeFormat))
	h.Set("x-ms-request-server-encrypted", "true")
	if md5 := r.Header.Get("Content-MD5"); md5 != "" {
		h.Set("Content-MD5", md5)
	}
	w.WriteHeader(http.StatusCreated)
}

func (s *Server) entryLimit() int64 {
	return min(s.o.MaxEntryBytes, maxBlobBytes)
}

func (s *Server) putBlob(ctx context.Context, w http.ResponseWriter, r *http.Request, staging string) error {
	if bt := r.Header.Get("x-ms-blob-type"); bt != "BlockBlob" {
		return azerr(http.StatusBadRequest, "InvalidBlobType", "x-ms-blob-type %q is not BlockBlob", bt)
	}
	if r.ContentLength == 0 {
		return azerr(http.StatusBadRequest, "InvalidInput", "an empty archive is not stored")
	}
	if _, err := bodyLength(r, s.entryLimit()); err != nil {
		return err
	}
	// A Put Blob starts the upload afresh: blocks left by an earlier
	// attempt of this reservation are deleted, so they never count beside
	// the blob, and a later block list cannot name them. The earlier
	// manifest goes with them, so a Put Blob that then fails leaves no
	// manifest naming deleted blocks. Without earlier blocks a manifest can
	// only name the blob, which this request replaces under the same key.
	blocks, err := listKeys(ctx, s.st, staging+"/block/")
	if err != nil {
		return fmt.Errorf("list blocks: %w", err)
	}
	if len(blocks) > 0 {
		blocks = append(blocks, staging+"/manifest")
	}
	if failed := deleteBatched(ctx, s.st, s.log, blocks); len(failed) > 0 {
		return fmt.Errorf("delete %d earlier blocks", len(failed))
	}
	key := staging + "/blob"
	n, err := s.stageObject(ctx, r, key, s.entryLimit())
	if err != nil {
		return err
	}
	if err := s.writeManifest(ctx, staging, manifest{Parts: []manifestPart{{Key: key, Size: n}}, Size: n}); err != nil {
		return err
	}
	s.created(w, r)
	return nil
}

// blockObject names a block's staging object by the hex of its decoded ID.
func blockObject(staging, blockID string) (string, error) {
	id, err := base64.StdEncoding.DecodeString(blockID)
	if err != nil || len(id) == 0 || len(id) > maxBlockIDBytes {
		return "", azerr(http.StatusBadRequest, "InvalidQueryParameterValue", "blockid must be base64 of 1 to %d bytes", maxBlockIDBytes)
	}
	return staging + "/block/" + hex.EncodeToString(id), nil
}

func (s *Server) putBlock(ctx context.Context, w http.ResponseWriter, r *http.Request, staging, blockID string) error {
	key, err := blockObject(staging, blockID)
	if err != nil {
		return err
	}
	if r.ContentLength == 0 {
		return azerr(http.StatusBadRequest, "InvalidInput", "an empty block is not stored")
	}
	limit := min(s.entryLimit(), maxBlockBytes)
	if _, err := bodyLength(r, limit); err != nil {
		return err
	}
	// The reservation's staged bytes (blocks, and a blob an earlier attempt
	// left) plus this block must fit in an entry, and its blocks in
	// maxParts. Concurrent blocks can each pass this check, so the
	// overshoot is bounded by the client's concurrency times the block
	// size; Put Block List and the reaper's staging budget bound the rest.
	staged, blocks := r.ContentLength, 1
	blockPrefix := staging + "/block/"
	err = s.st.list(ctx, staging+"/", func(o object) bool {
		switch {
		case o.Key == staging+"/blob":
			staged += o.Size
		case o.Key == key || !strings.HasPrefix(o.Key, blockPrefix):
			return true
		default:
			staged += o.Size
			blocks++
		}
		return staged <= s.o.MaxEntryBytes && blocks <= maxParts
	})
	if err != nil {
		return fmt.Errorf("list blocks: %w", err)
	}
	if blocks > maxParts {
		return azerr(http.StatusBadRequest, "BlockCountExceedsLimit", "an upload holds at most %d blocks", maxParts)
	}
	if staged > s.o.MaxEntryBytes {
		return azerr(http.StatusRequestEntityTooLarge, "RequestBodyTooLarge", "the staged blocks exceed %d bytes", s.o.MaxEntryBytes)
	}
	if _, err := s.stageObject(ctx, r, key, limit); err != nil {
		return err
	}
	s.created(w, r)
	return nil
}

// blockList accepts the three element names Put Block List allows; all
// name a block staged by this reservation, since no block is committed
// before the list.
type blockList struct {
	XMLName xml.Name `xml:"BlockList"`
	Items   []struct {
		XMLName xml.Name
		ID      string `xml:",chardata"`
	} `xml:",any"`
}

func (s *Server) putBlockList(ctx context.Context, w http.ResponseWriter, r *http.Request, staging string) error {
	if _, err := bodyLength(r, maxBlockListBody); err != nil {
		return err
	}
	var bl blockList
	if err := xml.NewDecoder(io.LimitReader(r.Body, maxBlockListBody)).Decode(&bl); err != nil {
		return azerr(http.StatusBadRequest, "InvalidXmlDocument", "block list: %v", err)
	}
	if len(bl.Items) == 0 {
		return azerr(http.StatusBadRequest, "InvalidBlockList", "the block list is empty")
	}
	if len(bl.Items) > maxParts {
		return azerr(http.StatusBadRequest, "InvalidBlockList", "%d blocks exceed the limit of %d", len(bl.Items), maxParts)
	}
	// One listing of the staged blocks replaces a HEAD per listed block.
	staged := map[string]int64{}
	if err := s.st.list(ctx, staging+"/block/", func(o object) bool {
		staged[o.Key] = o.Size
		return true
	}); err != nil {
		return fmt.Errorf("list blocks: %w", err)
	}
	m := manifest{Parts: make([]manifestPart, 0, len(bl.Items))}
	seen := map[string]bool{}
	for i, it := range bl.Items {
		switch it.XMLName.Local {
		case "Latest", "Uncommitted", "Committed":
		default:
			return azerr(http.StatusBadRequest, "InvalidXmlNodeValue", "unexpected element %q", it.XMLName.Local)
		}
		key, err := blockObject(staging, strings.TrimSpace(it.ID))
		if err != nil {
			return err
		}
		if seen[key] {
			return azerr(http.StatusBadRequest, "InvalidBlockList", "block %d is listed twice", i)
		}
		seen[key] = true
		size, ok := staged[key]
		if !ok {
			return azerr(http.StatusBadRequest, "InvalidBlockList", "block %d was not uploaded", i)
		}
		// S3 needs every multipart part but the last to be at least 5 MiB.
		if i < len(bl.Items)-1 && size < minPartBytes {
			return azerr(http.StatusBadRequest, "InvalidBlockList",
				"block %d is %d bytes; every block but the last must be at least %d bytes (5 MiB)", i, size, minPartBytes)
		}
		m.Size += size
		if m.Size > s.o.MaxEntryBytes {
			return azerr(http.StatusRequestEntityTooLarge, "RequestBodyTooLarge", "the blob exceeds %d bytes", s.o.MaxEntryBytes)
		}
		m.Parts = append(m.Parts, manifestPart{Key: key, Size: size})
	}
	if err := s.writeManifest(ctx, staging, m); err != nil {
		return err
	}
	s.created(w, r)
	return nil
}

func (s *Server) writeManifest(ctx context.Context, staging string, m manifest) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err := s.st.put(ctx, staging+"/manifest", strings.NewReader(string(b)), int64(len(b)), ""); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}
	return nil
}
