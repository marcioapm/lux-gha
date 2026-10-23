package githubapp

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const APIBaseURL = "https://api.github.com"
const APIVersion = "2026-03-10"
const maxBody = 4 << 20
const maxErrorBody = 64 << 10

// BaseURL overrides are restricted to HTTPS or loopback HTTP test servers.
type Config struct {
	AppID          string
	PrivateKey     []byte
	BaseURL        string
	HTTPClient     *http.Client
	RequestTimeout time.Duration
}

type Client struct {
	issuer  string
	key     *rsa.PrivateKey
	base    string
	http    *http.Client
	timeout time.Duration
	now     func() time.Time
	mu      sync.Mutex
	tokens  map[int64]cachedToken
	flights map[int64]*tokenFlight
}
type cachedToken struct {
	value   string
	expires time.Time
}
type tokenFlight struct {
	done  chan struct{}
	token string
	err   error
}

func New(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.AppID) == "" {
		return nil, errors.New("githubapp: AppID is required")
	}
	key, err := ParsePrivateKey(cfg.PrivateKey)
	if err != nil {
		return nil, err
	}
	base := cfg.BaseURL
	if base == "" {
		base = APIBaseURL
	}
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("githubapp: invalid BaseURL")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && ip != nil && ip.IsLoopback()) {
		return nil, errors.New("githubapp: BaseURL requires HTTPS (except loopback tests)")
	}
	timeout := cfg.RequestTimeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	if timeout < 0 {
		return nil, errors.New("githubapp: invalid timeout")
	}
	hc := http.Client{}
	if cfg.HTTPClient != nil {
		hc = *cfg.HTTPClient
	}
	// Never forward credentials or replay mutations through an API redirect.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{issuer: cfg.AppID, key: key, base: strings.TrimRight(base, "/"), http: &hc, timeout: timeout, now: time.Now, tokens: make(map[int64]cachedToken), flights: make(map[int64]*tokenFlight)}, nil
}

func (c *Client) InstallationToken(ctx context.Context, installationID int64) (string, error) {
	if installationID <= 0 {
		return "", errors.New("githubapp: installation ID must be positive")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	c.mu.Lock()
	if token, ok := c.tokens[installationID]; ok && c.now().Add(time.Minute).Before(token.expires) {
		c.mu.Unlock()
		return token.value, nil
	}
	flight, ok := c.flights[installationID]
	if !ok {
		flight = &tokenFlight{done: make(chan struct{})}
		c.flights[installationID] = flight
	}
	c.mu.Unlock()
	if !ok {
		// The shared fetch is detached from the first caller's cancellation and bounded by c.timeout.
		go func() {
			token, err := c.createToken(context.WithoutCancel(ctx), installationID)
			c.mu.Lock()
			if err == nil {
				c.tokens[installationID] = token
			}
			flight.token, flight.err = token.value, err
			delete(c.flights, installationID)
			close(flight.done)
			c.mu.Unlock()
		}()
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-flight.done:
		return flight.token, flight.err
	}
}

func (c *Client) forgetToken(installationID int64, token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cached, ok := c.tokens[installationID]; ok && cached.value == token {
		delete(c.tokens, installationID)
	}
}

func (c *Client) installationRequest(ctx context.Context, installationID int64, method, path string, body any, status int, out any) (http.Header, error) {
	token, err := c.InstallationToken(ctx, installationID)
	if err != nil {
		return nil, err
	}
	header, err := c.request(ctx, method, path, token, body, status, out)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusUnauthorized {
		c.forgetToken(installationID, token)
	}
	return header, err
}

func (c *Client) createToken(ctx context.Context, id int64) (cachedToken, error) {
	jwt, err := signJWT(c.key, c.issuer, c.now())
	if err != nil {
		return cachedToken{}, err
	}
	var result struct {
		Token   string    `json:"token"`
		Expires time.Time `json:"expires_at"`
	}
	_, err = c.request(ctx, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", id), jwt, struct{}{}, http.StatusCreated, &result)
	if err != nil {
		return cachedToken{}, err
	}
	if result.Token == "" || strings.ContainsAny(result.Token, "\r\n") || !c.now().Add(time.Minute).Before(result.Expires) {
		return cachedToken{}, errors.New("githubapp: invalid installation token response")
	}
	// GitHub installation tokens last one hour; never trust a longer cache lifetime.
	expires := result.Expires
	if cap := c.now().Add(time.Hour); expires.After(cap) {
		expires = cap
	}
	return cachedToken{result.Token, expires}, nil
}

// APIError omits response bodies, URLs and credentials, including echoed secrets.
type APIError struct {
	StatusCode  int
	RateLimited bool
	// NotAccessible is a 403 whose message is GitHub's permission refusal,
	// "Resource not accessible by integration": the App lacks a permission.
	NotAccessible bool
	RetryAt       time.Time
}

func (e *APIError) Error() string {
	return fmt.Sprintf("githubapp: API returned HTTP %d (rate limited: %t)", e.StatusCode, e.RateLimited)
}

// errorMessage reads a 403 body's message and classifies it: GitHub's
// secondary rate limit, which may arrive with quota remaining and no
// Retry-After, or GitHub's refusal for a missing App permission. The
// message can echo request content, so it is only classified, never kept.
func errorMessage(body io.Reader) (secondaryLimit, notAccessible bool) {
	data, _ := io.ReadAll(io.LimitReader(body, maxErrorBody))
	var env struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(data, &env) != nil {
		return false, false
	}
	m := strings.ToLower(env.Message)
	return strings.Contains(m, "secondary rate limit"), strings.Contains(m, "resource not accessible by integration")
}

func (c *Client) request(ctx context.Context, method, path, token string, body any, status int, out any) (http.Header, error) {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, errors.New("githubapp: cannot encode request")
		}
		if len(data) > maxBody {
			return nil, errors.New("githubapp: request body too large")
		}
		reader = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, reader)
	if err != nil {
		return nil, errors.New("githubapp: cannot construct request")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	sha, wantSHA := out.(*shaResponse)
	if wantSHA {
		req.Header.Set("Accept", "application/vnd.github.sha")
	} else {
		req.Header.Set("Accept", "application/vnd.github+json")
	}
	req.Header.Set("X-GitHub-Api-Version", APIVersion)
	req.Header.Set("User-Agent", "lux-gha-githubapp")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("githubapp: HTTP request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != status {
		e := &APIError{StatusCode: resp.StatusCode}
		remaining := resp.Header.Get("X-RateLimit-Remaining")
		if resp.StatusCode == 403 {
			secondary, notAccessible := errorMessage(resp.Body)
			e.RateLimited = remaining == "0" || resp.Header.Get("Retry-After") != "" || secondary
			e.NotAccessible = notAccessible && !e.RateLimited
		}
		e.RateLimited = e.RateLimited || resp.StatusCode == 429
		if e.RateLimited {
			e.RetryAt = c.now().Add(time.Minute)
		}
		if seconds, err := strconv.ParseInt(resp.Header.Get("Retry-After"), 10, 64); err == nil && seconds >= 0 && seconds <= int64((365*24*time.Hour)/time.Second) {
			e.RetryAt = c.now().Add(time.Duration(seconds) * time.Second)
		} else if at, err := http.ParseTime(resp.Header.Get("Retry-After")); err == nil {
			e.RetryAt = at
		}
		if remaining == "0" {
			if epoch, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
				at := time.Unix(epoch, 0)
				if at.After(e.RetryAt) {
					e.RetryAt = at
				}
			}
		}
		return resp.Header, e
	}
	if cs, ok := out.(*compareStatus); ok {
		cs.status, err = readCompareStatus(io.LimitReader(resp.Body, maxBody))
		if err != nil && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return resp.Header, err
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("githubapp: cannot read response")
	}
	if len(data) > maxBody {
		return nil, errors.New("githubapp: response body too large")
	}
	if wantSHA {
		sha.sha = strings.TrimSpace(string(data))
	} else if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return nil, errInvalidJSON
		}
	}
	return resp.Header, nil
}
