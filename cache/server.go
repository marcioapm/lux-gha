package cache

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// Defaults of Options' zero fields; config applies the same values.
const (
	DefaultUpstreamResultsURL = "https://results-receiver.actions.githubusercontent.com"
	DefaultDownloadURLTTL     = 10 * time.Minute
	DefaultTTL                = 7 * 24 * time.Hour
	DefaultMaxEntryBytes      = 10 << 30
	DefaultReapInterval       = time.Hour
	// uploadURLTTL bounds the signed upload URL. It is well below
	// stagingMaxAge, so the reaper never deletes staging data a valid
	// upload URL can still write to.
	uploadURLTTL  = 6 * time.Hour
	stagingMaxAge = 24 * time.Hour
	// minPartBytes is S3's lower bound for every multipart part but the last.
	minPartBytes = 5 << 20
	// maxPartBytes is S3's upper bound for one part and one PUT.
	maxPartBytes = 5 << 30
	// maxParts is S3's multipart part limit.
	maxParts = 10000
	maxKeys  = 10
)

// maxBytes is split into two budgets the reaper enforces separately:
// uploads not yet finalized (staging) get a tenth, finalized entries the
// rest. Staging never evicts an entry, and entries never evict staging.

// StagingBudgetFor is the share of maxBytes uploads in progress may hold.
func StagingBudgetFor(maxBytes int64) int64 { return maxBytes / 10 }

// EntryBudgetFor is the share of maxBytes finalized entries may hold.
func EntryBudgetFor(maxBytes int64) int64 { return maxBytes - StagingBudgetFor(maxBytes) }

// MaxEntryBytesFor returns the largest entry a cap of maxBytes admits: the
// entry budget, so the newest entry alone always fits it and a just-saved
// entry survives a capped pass. It need not fit the staging budget: the
// newest upload (the one started last) is never trimmed, so the newest
// upload always completes; the staging budget bounds how many others
// survive alongside it. With no cap (0) it is DefaultMaxEntryBytes.
func MaxEntryBytesFor(maxBytes int64) int64 {
	if maxBytes <= 0 {
		return DefaultMaxEntryBytes
	}
	return EntryBudgetFor(maxBytes)
}

// DefaultMaxEntryBytesFor is the maxEntryBytes default for a cap of
// maxBytes: DefaultMaxEntryBytes, lowered to MaxEntryBytesFor(maxBytes).
func DefaultMaxEntryBytesFor(maxBytes int64) int64 {
	return min(DefaultMaxEntryBytes, MaxEntryBytesFor(maxBytes))
}

// Options configures a Server. PublicURL, Upstream, SigningKeys and at least
// one of RepositoryOwnerIDs and RepositoryIDs are required.
type Options struct {
	// PublicURL is the scheme and host runners reach this server at
	// (ACTIONS_RESULTS_URL); signed upload URLs are built from it.
	PublicURL string
	// Issuer is the runtime token issuer; DefaultIssuer when empty.
	Issuer string
	// IssuerClient fetches the issuer's discovery document and JWKS.
	IssuerClient *http.Client
	// Upstream receives every request that is not a cache request.
	Upstream string
	// SigningKeys sign upload URLs: the first mints, all verify.
	SigningKeys [][]byte
	// RepositoryOwnerIDs and RepositoryIDs name the repositories served;
	// every other token is denied.
	RepositoryOwnerIDs []int64
	RepositoryIDs      []int64
	Prefix             string
	DownloadURLTTL     time.Duration
	TTL                time.Duration
	// MaxBytes caps the total size of final entries; 0 is no cap.
	MaxBytes      int64
	MaxEntryBytes int64
	ReapInterval  time.Duration
	Log           *slog.Logger
	Now           func() time.Time
}

// Server serves the cache service and passes other Results traffic upstream.
type Server struct {
	st      store
	o       Options
	lay     layout
	auth    *verifier
	allow   allowlist
	sign    signer
	public  *url.URL
	proxy   *httputil.ReverseProxy
	proxies *proxyLimiter
	log     *slog.Logger
	logOnce *logLimiter
	reaper  reaper
	// twirpReadTimeout bounds reading a Twirp request; progressWindow and
	// progressBytes are the minimum-progress rule of upload and proxied
	// bodies and of responses (see limits.go).
	twirpReadTimeout time.Duration
	progressWindow   time.Duration
	progressBytes    int64
}

func newServer(st store, o Options) (*Server, error) {
	if o.Issuer == "" {
		o.Issuer = DefaultIssuer
	}
	if o.DownloadURLTTL == 0 {
		o.DownloadURLTTL = DefaultDownloadURLTTL
	}
	if o.TTL == 0 {
		o.TTL = DefaultTTL
	}
	if o.MaxEntryBytes == 0 {
		o.MaxEntryBytes = DefaultMaxEntryBytesFor(o.MaxBytes)
	}
	if o.ReapInterval == 0 {
		o.ReapInterval = DefaultReapInterval
	}
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if len(o.SigningKeys) == 0 {
		return nil, errors.New("cache: no signing key")
	}
	for _, k := range o.SigningKeys {
		if len(k) < 32 {
			return nil, errors.New("cache: a signing key is shorter than 32 bytes")
		}
	}
	allow := newAllowlist(o.RepositoryOwnerIDs, o.RepositoryIDs)
	if allow.empty() {
		return nil, errors.New("cache: no repository IDs or repository owner IDs to serve")
	}
	if o.MaxBytes > 0 && o.MaxEntryBytes > MaxEntryBytesFor(o.MaxBytes) {
		return nil, fmt.Errorf("cache: max entry bytes %d exceed 90 %% of max bytes %d", o.MaxEntryBytes, o.MaxBytes)
	}
	pub, err := url.Parse(strings.TrimSuffix(o.PublicURL, "/"))
	if err != nil || pub.Host == "" || (pub.Scheme != "http" && pub.Scheme != "https") || pub.Path != "" {
		return nil, fmt.Errorf("cache: public URL %q must be http(s)://host[:port] without a path", o.PublicURL)
	}
	up, err := url.Parse(o.Upstream)
	if err != nil || up.Host == "" || (up.Scheme != "http" && up.Scheme != "https") {
		return nil, fmt.Errorf("cache: upstream URL %q must be an absolute http(s) URL", o.Upstream)
	}
	s := &Server{
		st:      st,
		o:       o,
		lay:     newLayout(o.Prefix),
		auth:    newVerifier(o.Issuer, o.IssuerClient, o.Now),
		allow:   allow,
		sign:    signer{keys: o.SigningKeys},
		public:  pub,
		log:     o.Log,
		logOnce: newLogLimiter(time.Minute, o.Now),
		proxies: newProxyLimiter(maxProxyPerJob, maxProxyAnonymous, maxProxyRequests),

		twirpReadTimeout: defaultTwirpReadTimeout,
		progressWindow:   defaultProgressWindow,
		progressBytes:    minProgressBytes,
	}
	s.proxy = newProxy(up, s.log)
	s.reaper = reaper{st: st, lay: s.lay, ttl: o.TTL, maxBytes: o.MaxBytes, timeout: o.ReapInterval, now: o.Now, log: s.log}
	return s, nil
}

const twirpPrefix = "/twirp/github.actions.results.api.v1.CacheService/"

// blobPath is the path of every signed upload URL. The Azure SDK reads a
// container and blob name from it (as /account/container/blob for an IP
// or localhost host, /container/blob otherwise); both forms are non-empty.
const blobPath = "/_azblob/cache/archive"

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rc := http.NewResponseController(w)
	// A keep-alive connection carries the previous response's write
	// deadline; progressWriter sets a fresh one before every write, and the
	// server's own final flush after the handler gets one too.
	_ = rc.SetWriteDeadline(time.Time{})
	defer func() { _ = rc.SetWriteDeadline(time.Now().Add(s.progressWindow)) }()
	w = newProgressWriter(w, rc, s.progressWindow, s.progressBytes)
	// A read deadline left set after the body is consumed would cancel the
	// request's context (net/http's background read fails), so it is set
	// only for a non-empty body, and progressBody clears it at the end.
	hasBody := r.ContentLength != 0
	switch {
	case strings.HasPrefix(r.URL.Path, twirpPrefix):
		if hasBody {
			// Also bounds the drain of an unread body after a rejection.
			_ = rc.SetReadDeadline(time.Now().Add(s.twirpReadTimeout))
			r.Body = newProgressBody(r, rc, 0, 0)
		}
		s.serveTwirp(w, r, strings.TrimPrefix(r.URL.Path, twirpPrefix))
	case r.URL.Path == blobPath:
		if hasBody {
			// Also bounds the drain of a body left unread by a rejection.
			_ = rc.SetReadDeadline(time.Now().Add(s.progressWindow))
			r.Body = newProgressBody(r, rc, s.progressWindow, s.progressBytes)
		}
		s.serveBlob(w, r)
	default:
		job := s.passThroughJob(r)
		release, refused := s.proxies.acquire(job)
		if release == nil {
			if s.logOnce.allow("proxy-full:" + refused) {
				s.log.Warn("cache: too many concurrent pass-through requests; answering 503 (logged once a minute)",
					"limit", refused, "remote_addr", r.RemoteAddr,
					"per_job", s.proxies.perJob, "anonymous", s.proxies.anonymous, "total", s.proxies.total)
			}
			w.Header().Set("Retry-After", "5")
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		defer release()
		// A request without a body keeps http.NoBody, which ReverseProxy
		// recognises.
		if hasBody {
			_ = rc.SetReadDeadline(time.Now().Add(s.progressWindow))
			r.Body = newProgressBody(r, rc, s.progressWindow, s.progressBytes)
		}
		s.proxy.ServeHTTP(w, r)
	}
}

// passThroughJob is the limiter key of a pass-through request: the job of
// a runtime token that verifies (any repository's: the pass-through serves
// every one), or anonymousJob for a request with no token, an invalid one,
// or while the issuer is unreachable. The request is forwarded either way.
func (s *Server) passThroughJob(r *http.Request) string {
	authz := r.Header.Get("Authorization")
	if authz == "" {
		return anonymousJob
	}
	job, err := s.auth.job(r.Context(), authz)
	if err != nil {
		reason := "invalid runtime token"
		if errors.Is(err, errIssuerDown) {
			reason = "token issuer unavailable"
		}
		if s.logOnce.allow("proxy-anonymous") {
			// The error is not logged: it may quote token content.
			s.log.Info("cache: pass-through request counted as anonymous (logged once a minute)",
				"reason", reason, "remote_addr", r.RemoteAddr)
		}
		return anonymousJob
	}
	return job
}

func (s *Server) serveTwirp(w http.ResponseWriter, r *http.Request, method string) {
	if r.Method != http.MethodPost {
		writeTwirpError(w, twerr(twirpBadRoute, "method %s is not POST", r.Method))
		return
	}
	c, te := requestCodec(r)
	if te != nil {
		writeTwirpError(w, te)
		return
	}
	var call func(context.Context, *identity, *http.Request, codec) (marshaler, error)
	switch method {
	case "CreateCacheEntry":
		call = s.create
	case "FinalizeCacheEntryUpload":
		call = s.finalize
	case "GetCacheEntryDownloadURL":
		call = s.download
	default:
		writeTwirpError(w, twerr(twirpBadRoute, "no method %q", method))
		return
	}
	id, err := s.auth.verify(r.Context(), r.Header.Get("Authorization"))
	if err != nil {
		if errors.Is(err, errIssuerDown) {
			if s.logOnce.allow("issuer-down") {
				s.log.Warn("cache: token issuer unavailable (logged at most once a minute)", "error", err)
			}
			writeTwirpError(w, twerr(twirpUnavailable, "token issuer unavailable"))
			return
		}
		if s.logOnce.allow("rejected-token") {
			s.log.Info("cache: rejected token (logged at most once a minute)", "method", method, "error", err)
		}
		writeTwirpError(w, twerr(twirpUnauthenticated, "invalid runtime token"))
		return
	}
	if !s.allow.permits(id) {
		if s.logOnce.allow("deny:" + id.RepoID) {
			s.log.Warn("cache: repository not served; denying its requests (logged once a minute per repository)",
				"repository_id", id.RepoID, "repository_owner_id", id.OwnerID, "method", method)
		}
		writeTwirpError(w, twerr(twirpPermissionDenied, "cache denied: this repository is not served by this cache"))
		return
	}
	resp, err := call(r.Context(), id, r, c)
	if err != nil {
		te := asTwirp(err)
		if te.code == twirpInternal {
			s.log.Error("cache: request failed", "method", method, "repository_id", id.RepoID, "error", err)
		}
		writeTwirpError(w, te)
		return
	}
	writeTwirp(w, c, resp)
}

func validateEntry(key, version string) error {
	if err := validateKey(key); err != nil {
		return twerr(twirpInvalidArgument, "%v", err)
	}
	if err := validateVersion(version); err != nil {
		return twerr(twirpInvalidArgument, "%v", err)
	}
	return nil
}

const writeDeniedMessage = "cache write denied: the runtime token has no writable cache scope"

func (s *Server) create(ctx context.Context, id *identity, r *http.Request, c codec) (marshaler, error) {
	var req createRequest
	if te := decodeTwirp(r, c, &req); te != nil {
		return nil, te
	}
	if err := validateEntry(req.Key, req.Version); err != nil {
		return nil, err
	}
	if id.WriteScope == "" {
		// The client recognises this prefix only on an ok:false Create
		// (@actions/cache saveCacheV2) and reports a Twirp error there as
		// contention, so Create answers ok:false. Finalize is never reached
		// without a successful Create, so a denial there is a 403.
		return createResponse{Message: writeDeniedMessage}, nil
	}
	final := s.lay.entryKey(id.RepoID, id.WriteScope, req.Version, req.Key)
	if len(final) > maxObjectKeyBytes {
		// Escaping can triple a key's length past S3's key limit.
		return createResponse{Message: "the key is too long to store once escaped; use a shorter or ASCII key"}, nil
	}
	_, err := s.st.head(ctx, final)
	switch {
	case err == nil:
		return createResponse{Message: "a cache entry for this key and version already exists"}, nil
	case !errors.Is(err, errNotFound):
		return nil, fmt.Errorf("head entry: %w", err)
	}
	staging := s.lay.stagingKey(id.RepoID, id.WriteScope, req.Version, req.Key, id.TokenHash)
	tok := s.sign.mint(staging, s.o.Now().Add(uploadURLTTL))
	u := *s.public
	u.Path = blobPath
	u.RawQuery = url.Values{"sig": {tok}}.Encode()
	return createResponse{OK: true, SignedUploadURL: u.String()}, nil
}

// manifest is the committed block list of one reservation, written by the
// last Azure request of an upload and read by Finalize.
type manifest struct {
	Parts []manifestPart `json:"parts"`
	Size  int64          `json:"size"`
}

type manifestPart struct {
	Key  string `json:"key"`
	Size int64  `json:"size"`
}

const maxManifestBytes = 2 << 20

func (s *Server) finalize(ctx context.Context, id *identity, r *http.Request, c codec) (marshaler, error) {
	var req finalizeRequest
	if te := decodeTwirp(r, c, &req); te != nil {
		return nil, te
	}
	if err := validateEntry(req.Key, req.Version); err != nil {
		return nil, err
	}
	if id.WriteScope == "" {
		// A 403 here, unlike Create's ok:false; see create.
		return nil, twerr(twirpPermissionDenied, writeDeniedMessage)
	}
	staging := s.lay.stagingKey(id.RepoID, id.WriteScope, req.Version, req.Key, id.TokenHash)
	b, err := s.st.getSmall(ctx, staging+"/manifest", maxManifestBytes)
	if errors.Is(err, errNotFound) {
		// The reaper deletes a reservation whole, manifest included, so an
		// upload it trimmed and one never committed look the same here.
		return finalizeResponse{Message: "no completed upload for this key and version, or the uploaded data has expired; save again"}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil || len(m.Parts) == 0 {
		return nil, fmt.Errorf("corrupt manifest %s", staging)
	}
	if m.Size != req.SizeBytes {
		return finalizeResponse{Message: fmt.Sprintf("uploaded %d bytes, but size_bytes is %d", m.Size, req.SizeBytes)}, nil
	}
	final := s.lay.entryKey(id.RepoID, id.WriteScope, req.Version, req.Key)
	parts := make([]string, len(m.Parts))
	for i, p := range m.Parts {
		parts[i] = p.Key
	}
	err = s.st.assemble(ctx, final, parts, staging)
	if errors.Is(err, errExists) {
		// A retry after a lost success finds this reservation's own entry.
		o, herr := s.st.head(ctx, final)
		if herr != nil && !errors.Is(herr, errNotFound) {
			return nil, fmt.Errorf("head entry: %w", herr)
		}
		if herr == nil && o.Owner == staging {
			s.cleanStaging(ctx, staging)
			s.log.Info("cache: saved", "repository_id", id.RepoID, "bytes", m.Size)
			return finalizeResponse{OK: true, EntryID: entryID(final)}, nil
		}
		s.cleanStaging(ctx, staging)
		return finalizeResponse{Message: "another job saved this key and version first"}, nil
	}
	if errors.Is(err, errNotFound) {
		return finalizeResponse{Message: "the uploaded data has expired; save again"}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("assemble entry: %w", err)
	}
	s.cleanStaging(ctx, staging)
	s.log.Info("cache: saved", "repository_id", id.RepoID, "bytes", m.Size)
	return finalizeResponse{OK: true, EntryID: entryID(final)}, nil
}

// entryID is a stable positive ID below 2^53, exact as a JavaScript number.
func entryID(objectKey string) int64 {
	h := sha256.Sum256([]byte(objectKey))
	return int64(binary.BigEndian.Uint64(h[:8])>>11) | 1
}

// cleanStaging deletes a reservation's objects; the reaper removes what
// this misses.
func (s *Server) cleanStaging(ctx context.Context, staging string) {
	var keys []string
	err := s.st.list(ctx, staging+"/", func(o object) bool {
		keys = append(keys, o.Key)
		return true
	})
	if err != nil {
		s.log.Warn("cache: staging cleanup failed; the reaper retries", "error", err)
		return
	}
	deleteBatched(ctx, s.st, s.log, keys)
}

func (s *Server) download(ctx context.Context, id *identity, r *http.Request, c codec) (marshaler, error) {
	var req downloadRequest
	if te := decodeTwirp(r, c, &req); te != nil {
		return nil, te
	}
	if err := validateEntry(req.Key, req.Version); err != nil {
		return nil, err
	}
	if 1+len(req.RestoreKeys) > maxKeys {
		return nil, twerr(twirpInvalidArgument, "at most %d keys", maxKeys)
	}
	for _, k := range req.RestoreKeys {
		if err := validateKey(k); err != nil {
			return nil, twerr(twirpInvalidArgument, "restore key: %v", err)
		}
	}
	if len(id.ReadScopes) == 0 {
		return nil, twerr(twirpPermissionDenied, "cache read denied: the runtime token has no readable cache scope")
	}
	obj, key, err := s.lookup(ctx, id, req)
	if errors.Is(err, errNotFound) {
		return downloadResponse{}, nil
	}
	if err != nil {
		return nil, err
	}
	u, err := s.st.presignGet(ctx, obj.Key, s.o.DownloadURLTTL)
	if err != nil {
		return nil, fmt.Errorf("presign: %w", err)
	}
	return downloadResponse{OK: true, SignedDownloadURL: u, MatchedKey: key}, nil
}

// lookup follows GitHub's documented matching: scope by scope in priority
// order, an exact match on key, then the newest entry whose key has key as
// a prefix, then the same prefix search for each restore key in order.
func (s *Server) lookup(ctx context.Context, id *identity, req downloadRequest) (object, string, error) {
	prefixes := append([]string{req.Key}, req.RestoreKeys...)
	for _, scope := range id.ReadScopes {
		exact := s.lay.entryKey(id.RepoID, scope, req.Version, req.Key)
		dir := s.lay.versionDir(id.RepoID, scope, req.Version)
		// A name longer than S3 allows was never stored, nor any it prefixes.
		if len(exact) <= maxObjectKeyBytes {
			o, err := s.st.head(ctx, exact)
			if err == nil {
				return o, req.Key, nil
			}
			if !errors.Is(err, errNotFound) {
				return object{}, "", fmt.Errorf("head entry: %w", err)
			}
		}
		for _, p := range prefixes {
			if len(dir)+len(encodeKey(p)) > maxObjectKeyBytes {
				continue
			}
			o, k, err := s.newest(ctx, dir, p)
			if err == nil {
				return o, k, nil
			}
			if !errors.Is(err, errNotFound) {
				return object{}, "", err
			}
		}
	}
	return object{}, "", errNotFound
}

func (s *Server) newest(ctx context.Context, dir, keyPrefix string) (object, string, error) {
	var best object
	var bestKey string
	err := s.st.list(ctx, dir+encodeKey(keyPrefix), func(o object) bool {
		k, ok := keyFromEntry(dir, o.Key)
		if !ok || !strings.HasPrefix(k, keyPrefix) {
			return true
		}
		// Ties on LastModified go to the greater key, so the choice does
		// not depend on listing order.
		if bestKey == "" || o.LastModified.After(best.LastModified) ||
			(o.LastModified.Equal(best.LastModified) && k > bestKey) {
			best, bestKey = o, k
		}
		return true
	})
	if err != nil {
		return object{}, "", fmt.Errorf("list entries: %w", err)
	}
	if bestKey == "" {
		return object{}, "", errNotFound
	}
	return best, bestKey, nil
}
