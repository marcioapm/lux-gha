// Package luxclient is a thin client for the Lux tenant HTTP API: submit,
// read, list and terminate one-shot generic Runs. It imports nothing from Lux;
// the wire shapes mirror Lux's internal/spec and internal/server JSON names.
package luxclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"
)

const (
	maxBody      = 16 << 20
	maxErrorBody = 64 << 10
	// Lux accepts 1..200 Runs per page on a paged GET /v1/runs.
	pageSize        = 100
	defaultMaxPages = 100
	defaultTimeout  = 30 * time.Second
)

// Config: BaseURL must be HTTPS (plain HTTP only for loopback test servers)
// with no path, query, fragment or userinfo. APIKey is a tenant API key with
// the run scope; operator keys need ?tenant= on submit, which is not supported.
type Config struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
	// RequestTimeout bounds each HTTP exchange, including each List page.
	RequestTimeout time.Duration
	// MaxPages bounds List; exceeding it is ErrTooManyPages, never truncation.
	MaxPages int
}

type Client struct {
	base     string
	key      string
	http     *http.Client
	timeout  time.Duration
	maxPages int
}

var (
	ErrRepeatedCursor        = errors.New("luxclient: list runs: server repeated a page cursor")
	ErrTooManyPages          = errors.New("luxclient: list runs: page limit exceeded")
	ErrResumePolicyNotEchoed = errors.New("luxclient: created run does not echo resumePolicy never")
	ErrIncompatible          = errors.New("luxclient: incompatible Lux server")
)

// APIError carries only the HTTP status and Lux's stable error code: Lux
// messages and details can quote request content, so they are never kept.
type APIError struct {
	Op     string
	Status int
	Code   string
}

func (e *APIError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("luxclient: %s: HTTP %d", e.Op, e.Status)
	}
	return fmt.Sprintf("luxclient: %s: HTTP %d %s", e.Op, e.Status, e.Code)
}

func New(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("luxclient: invalid BaseURL")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && ip != nil && ip.IsLoopback()) {
		return nil, errors.New("luxclient: BaseURL requires HTTPS (except loopback)")
	}
	if cfg.APIKey == "" || strings.ContainsAny(cfg.APIKey, "\r\n \t") {
		return nil, errors.New("luxclient: invalid APIKey")
	}
	timeout := cfg.RequestTimeout
	if timeout == 0 {
		timeout = defaultTimeout
	}
	maxPages := cfg.MaxPages
	if maxPages == 0 {
		maxPages = defaultMaxPages
	}
	if timeout < 0 || maxPages < 0 {
		return nil, errors.New("luxclient: invalid RequestTimeout or MaxPages")
	}
	hc := http.Client{}
	if cfg.HTTPClient != nil {
		hc = *cfg.HTTPClient
	}
	// A redirect would re-send the bearer key and could replay a submission.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{
		base:     u.Scheme + "://" + u.Host,
		key:      cfg.APIKey,
		http:     &hc,
		timeout:  timeout,
		maxPages: maxPages,
	}, nil
}

type request struct {
	op             string
	method         string
	path           string // already escaped
	query          url.Values
	body           []byte
	idempotencyKey string
	anonymous      bool
	want           []int
}

func (c *Client) do(ctx context.Context, r request, out any) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	target := c.base + r.path
	if len(r.query) > 0 {
		target += "?" + r.query.Encode()
	}
	var body io.Reader
	if r.body != nil {
		body = bytes.NewReader(r.body)
	}
	req, err := http.NewRequestWithContext(ctx, r.method, target, body)
	if err != nil {
		return fmt.Errorf("luxclient: %s: cannot build request", r.op)
	}
	// Without GetBody net/http cannot rewind the body, so it never re-sends a
	// POST on a dead keep-alive connection (it would, given Idempotency-Key).
	req.GetBody = nil
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "lux-gha")
	if r.body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if r.idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", r.idempotencyKey)
	}
	if !r.anonymous {
		req.Header.Set("Authorization", "Bearer "+c.key)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return transportError(ctx, r.op, err)
	}
	defer func() {
		_, _ = io.CopyN(io.Discard, resp.Body, maxErrorBody)
		_ = resp.Body.Close()
	}()
	if !slices.Contains(r.want, resp.StatusCode) {
		return &APIError{Op: r.op, Status: resp.StatusCode, Code: errorCode(resp.Body)}
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return transportError(ctx, r.op, err)
	}
	if len(data) > maxBody {
		return fmt.Errorf("luxclient: %s: response body too large", r.op)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("luxclient: %s: malformed response", r.op)
	}
	return nil
}

// transportError keeps context errors matchable and drops url.Error's URL.
func transportError(ctx context.Context, op string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("luxclient: %s: %w", op, ctxErr)
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	return fmt.Errorf("luxclient: %s: request failed: %w", op, err)
}

var errorCodeRe = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// errorCode reads Lux's {"error":{"code":...}} envelope; anything else is "".
func errorCode(body io.Reader) string {
	data, _ := io.ReadAll(io.LimitReader(body, maxErrorBody))
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if json.Unmarshal(data, &env) != nil || !errorCodeRe.MatchString(env.Error.Code) {
		return ""
	}
	return env.Error.Code
}
