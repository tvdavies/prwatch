// Package github is a minimal GitHub GraphQL client with rate-limit aware
// error classification.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DefaultURL is the public GitHub GraphQL endpoint.
const DefaultURL = "https://api.github.com/graphql"

// RateInfo is what a response told us about the primary rate limit.
type RateInfo struct {
	Limit     int       `json:"limit"`
	Remaining int       `json:"remaining"`
	Used      int       `json:"used"`
	Cost      int       `json:"cost"`
	ResetAt   time.Time `json:"resetAt"`
	Known     bool      `json:"-"`
}

// RateLimitError means GitHub refused the request because of a primary or
// secondary rate limit.
type RateLimitError struct {
	Secondary  bool
	RetryAfter time.Duration
	Remaining  int // -1 when unknown
	ResetAt    time.Time
	Message    string
}

func (e *RateLimitError) Error() string {
	kind := "primary"
	if e.Secondary {
		kind = "secondary"
	}
	return fmt.Sprintf("GitHub %s rate limit: %s", kind, e.Message)
}

// AuthError means the token is missing, invalid or lacks access.
type AuthError struct{ Message string }

func (e *AuthError) Error() string { return "GitHub authentication failed: " + e.Message }

// TransientError is a network failure or server error worth retrying later.
type TransientError struct{ Err error }

func (e *TransientError) Error() string { return "GitHub request failed: " + e.Err.Error() }
func (e *TransientError) Unwrap() error { return e.Err }

// GQLError is one entry of a GraphQL errors array.
type GQLError struct {
	Type    string `json:"type"`
	Message string `json:"message"`
	Path    []any  `json:"path"`
}

// Response is a decoded GraphQL response.
type Response struct {
	Data   map[string]json.RawMessage
	Errors []GQLError
	Rate   RateInfo
}

// Client sends GraphQL requests. It is safe for concurrent use but callers
// in prwatch never issue concurrent requests.
type Client struct {
	URL       string
	HTTP      *http.Client
	UserAgent string
	tokens    *TokenSource
}

// NewClient returns a client using url (DefaultURL when empty).
func NewClient(url, userAgent string, tokens *TokenSource) *Client {
	if url == "" {
		url = DefaultURL
	}
	return &Client{URL: url, HTTP: &http.Client{Timeout: 30 * time.Second}, UserAgent: userAgent, tokens: tokens}
}

// Do executes a query. A 401 triggers one token refresh and retry.
func (c *Client) Do(ctx context.Context, query string, vars map[string]any) (*Response, error) {
	resp, err := c.do(ctx, query, vars)
	var ae *AuthError
	if errors.As(err, &ae) && c.tokens.Refresh() {
		return c.do(ctx, query, vars)
	}
	return resp, err
}

func (c *Client) do(ctx context.Context, query string, vars map[string]any) (*Response, error) {
	token, err := c.tokens.Token()
	if err != nil {
		return nil, &AuthError{Message: err.Error()}
	}
	body, err := json.Marshal(map[string]any{"query": query, "variables": vars})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.UserAgent)
	res, err := c.HTTP.Do(req)
	if err != nil {
		return nil, &TransientError{Err: err}
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 32<<20))
	if err != nil {
		return nil, &TransientError{Err: err}
	}
	rate := rateFromHeaders(res.Header)

	var decoded struct {
		Data    map[string]json.RawMessage `json:"data"`
		Errors  []GQLError                 `json:"errors"`
		Message string                     `json:"message"`
	}
	_ = json.Unmarshal(raw, &decoded)

	if rl := classifyRateLimit(res, rate, decoded.Message, decoded.Errors); rl != nil {
		return nil, rl
	}
	switch {
	case res.StatusCode == http.StatusUnauthorized:
		return nil, &AuthError{Message: orStatus(decoded.Message, res)}
	case res.StatusCode == http.StatusForbidden:
		return nil, &AuthError{Message: orStatus(decoded.Message, res)}
	case res.StatusCode >= 500:
		return nil, &TransientError{Err: errors.New(orStatus(decoded.Message, res))}
	case res.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("GitHub returned %s", orStatus(decoded.Message, res))
	}
	if decoded.Data == nil && len(decoded.Errors) > 0 {
		return nil, fmt.Errorf("GitHub GraphQL error: %s", decoded.Errors[0].Message)
	}
	if decoded.Data == nil {
		return nil, &TransientError{Err: errors.New("GitHub returned no data")}
	}
	if rl, ok := decoded.Data["rateLimit"]; ok {
		var body RateInfo
		if json.Unmarshal(rl, &body) == nil && body.Limit > 0 {
			body.Known = true
			rate = body
		}
	}
	return &Response{Data: decoded.Data, Errors: decoded.Errors, Rate: rate}, nil
}

func orStatus(msg string, res *http.Response) string {
	if msg != "" {
		return fmt.Sprintf("%s (HTTP %d)", msg, res.StatusCode)
	}
	return res.Status
}

func rateFromHeaders(h http.Header) RateInfo {
	var r RateInfo
	lim, err1 := strconv.Atoi(h.Get("X-Ratelimit-Limit"))
	rem, err2 := strconv.Atoi(h.Get("X-Ratelimit-Remaining"))
	if err1 != nil || err2 != nil {
		r.Remaining = -1
		return r
	}
	r.Limit, r.Remaining, r.Known = lim, rem, true
	r.Used, _ = strconv.Atoi(h.Get("X-Ratelimit-Used"))
	if reset, err := strconv.ParseInt(h.Get("X-Ratelimit-Reset"), 10, 64); err == nil {
		r.ResetAt = time.Unix(reset, 0)
	}
	return r
}

func classifyRateLimit(res *http.Response, rate RateInfo, message string, errs []GQLError) *RateLimitError {
	var retryAfter time.Duration
	if s := res.Header.Get("Retry-After"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n >= 0 {
			retryAfter = time.Duration(n) * time.Second
		}
	}
	remaining := -1
	if rate.Known {
		remaining = rate.Remaining
	}
	mk := func(secondary bool, msg string) *RateLimitError {
		return &RateLimitError{Secondary: secondary, RetryAfter: retryAfter, Remaining: remaining, ResetAt: rate.ResetAt, Message: msg}
	}
	for _, e := range errs {
		if e.Type == "RATE_LIMITED" {
			return mk(false, e.Message)
		}
	}
	if res.StatusCode != http.StatusForbidden && res.StatusCode != http.StatusTooManyRequests {
		return nil
	}
	lower := strings.ToLower(message)
	switch {
	case strings.Contains(lower, "secondary rate limit") || strings.Contains(lower, "abuse"):
		return mk(true, message)
	case remaining == 0:
		return mk(false, message)
	case retryAfter > 0 || res.StatusCode == http.StatusTooManyRequests || strings.Contains(lower, "rate limit"):
		return mk(true, message)
	}
	return nil
}

// TokenSource resolves a GitHub token from GH_TOKEN, GITHUB_TOKEN or
// `gh auth token`, caching it. Tokens are never logged.
type TokenSource struct {
	mu        sync.Mutex
	token     string
	fromGH    bool
	refreshed bool
}

// Token returns the cached token, resolving it on first use.
func (t *TokenSource) Token() (string, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.token != "" {
		return t.token, nil
	}
	tok, fromGH, err := resolveToken()
	if err != nil {
		return "", err
	}
	t.token, t.fromGH = tok, fromGH
	return tok, nil
}

// Refresh re-reads the token once per process when it came from gh, so a
// rotated gh login is picked up. It reports whether a new token was found.
func (t *TokenSource) Refresh() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.fromGH || t.refreshed {
		return false
	}
	t.refreshed = true
	tok, _, err := resolveToken()
	if err != nil || tok == t.token {
		return false
	}
	t.token = tok
	return true
}

func resolveToken() (string, bool, error) {
	for _, env := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(env)); v != "" {
			return v, false, nil
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output()
	if err != nil {
		return "", false, errors.New("no token: set GH_TOKEN or GITHUB_TOKEN, or run `gh auth login`")
	}
	tok := strings.TrimSpace(string(out))
	if tok == "" {
		return "", false, errors.New("`gh auth token` returned nothing")
	}
	return tok, true, nil
}
