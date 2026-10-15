// Package api is the typed client for the InvestViews public API
// (https://docs.investviews.ai).
//
// It covers the eleven live endpoints, decodes the one error envelope into
// typed errors with process exit codes, retries only what is safe to retry,
// and hides the three different paging models behind per-endpoint helpers.
//
// Three things in here exist because getting them wrong produces a client that
// looks right and returns wrong answers:
//
//  1. THREE paging models, one per listing endpoint, and no single --all can
//     serve them. See paging.go.
//  2. ancestors: null is not ancestors: []. See Ancestors in types.go.
//  3. The two 429s both say error: "rate_limited" on purpose. See
//     RateLimitScope in errors.go.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultUserAgent identifies the CLI to the API.
	DefaultUserAgent = "investviews-cli"

	// DefaultTimeout bounds one HTTP attempt, not the whole retry loop.
	DefaultTimeout = 30 * time.Second

	// DefaultMaxRetries is how many times a retryable failure is sent again,
	// on top of the first attempt.
	DefaultMaxRetries = 3

	// DefaultBackoffBase and DefaultMaxBackoff bound the exponential backoff
	// used when the server gives no Retry-After.
	DefaultBackoffBase = 250 * time.Millisecond
	DefaultMaxBackoff  = 5 * time.Second

	// MaxRetryAfter caps how long a server-sent Retry-After is honoured
	// before the client gives up instead of sleeping. A CLI that blocked for
	// an hour on a header would look hung.
	MaxRetryAfter = 2 * time.Minute

	// maxBodyBytes bounds a response read, so a wrong URL answering with a
	// huge body cannot exhaust memory.
	maxBodyBytes = 32 << 20
)

// Meta is the per-response signal block. Quota headers are the authoritative
// answer to "did this call spend quota" — the endpoint's static group is only
// what we expect, this is what happened.
type Meta struct {
	Status    int
	RequestID string

	// QuotaGroup is current, history, metadata or reports.
	QuotaGroup string
	// QuotaLimit is a STRING because a free group sends the literal word
	// "uncounted". Parsing it as an integer would invent a ceiling.
	QuotaLimit string
	// QuotaUsed and QuotaReset are absent on an uncounted group — 0 there
	// would read as "you have spent none of a real allowance".
	QuotaUsed  *int64
	QuotaReset string

	RateLimitLimit     *int64
	RateLimitRemaining *int64

	// Attempts is how many HTTP requests this call took, including retries.
	Attempts int
}

// Uncounted is the literal X-Quota-Limit value on a free call.
const Uncounted = "uncounted"

// HasQuotaHeaders reports whether the response carried the quota block at all.
// nginx's edge 429 carries none of it, because Rails never ran.
func (m Meta) HasQuotaHeaders() bool { return m.QuotaGroup != "" || m.QuotaLimit != "" }

// Metered reports whether this call spent quota, as the SERVER reported it.
func (m Meta) Metered() bool {
	return m.QuotaLimit != "" && !strings.EqualFold(m.QuotaLimit, Uncounted)
}

// Cost renders the quota line a command prints, so a free call and a metered
// call never look alike in the output.
func (m Meta) Cost() string {
	if !m.HasQuotaHeaders() {
		return "cost: unknown (the response carried no quota headers)"
	}
	if !m.Metered() {
		return fmt.Sprintf("cost: free (group %s, uncounted)", m.QuotaGroup)
	}
	if m.QuotaUsed != nil {
		return fmt.Sprintf("cost: 1 metered request (group %s, %d of %s used this cycle)",
			m.QuotaGroup, *m.QuotaUsed, m.QuotaLimit)
	}
	return fmt.Sprintf("cost: 1 metered request (group %s, limit %s)", m.QuotaGroup, m.QuotaLimit)
}

// Options configures a Client. The zero value is not usable: BaseURL is
// required, so there is no second copy of the production URL in this package
// to drift from the one in internal/config.
type Options struct {
	BaseURL string
	Token   string

	HTTPClient *http.Client
	UserAgent  string

	MaxRetries  int
	BackoffBase time.Duration
	MaxBackoff  time.Duration

	// RetryBudget caps the TOTAL time one call may spend sleeping between
	// retries. Zero means no budget beyond MaxRetries and MaxRetryAfter —
	// the CLI's behaviour. See WithRetryBudget.
	RetryBudget time.Duration

	// Sleep waits for d or until ctx is done. Tests replace it to assert on
	// retry timing without waiting; nothing else does.
	Sleep func(ctx context.Context, d time.Duration) error
	// Jitter perturbs a computed backoff. Tests replace it with identity.
	Jitter func(time.Duration) time.Duration
}

// Client talks to one base URL with one token.
type Client struct {
	baseURL   *url.URL
	token     string
	http      *http.Client
	userAgent string

	maxRetries  int
	backoffBase time.Duration
	maxBackoff  time.Duration
	retryBudget time.Duration
	sleep       func(ctx context.Context, d time.Duration) error
	jitter      func(time.Duration) time.Duration
}

// New builds a client. BaseURL must be the full base including the version
// path, e.g. https://api.investviews.ai/public/v1.
func New(opts Options) (*Client, error) {
	if strings.TrimSpace(opts.BaseURL) == "" {
		return nil, errors.New("api: a base URL is required")
	}
	base, err := url.Parse(strings.TrimSpace(opts.BaseURL))
	if err != nil {
		return nil, fmt.Errorf("api: base URL %q is not a URL: %w", opts.BaseURL, err)
	}
	if base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("api: base URL %q needs a scheme and a host", opts.BaseURL)
	}

	c := &Client{
		baseURL:     base,
		token:       strings.TrimSpace(opts.Token),
		http:        opts.HTTPClient,
		userAgent:   opts.UserAgent,
		maxRetries:  opts.MaxRetries,
		backoffBase: opts.BackoffBase,
		maxBackoff:  opts.MaxBackoff,
		retryBudget: opts.RetryBudget,
		sleep:       opts.Sleep,
		jitter:      opts.Jitter,
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: DefaultTimeout}
	}
	if c.userAgent == "" {
		c.userAgent = DefaultUserAgent
	}
	if opts.MaxRetries == 0 {
		c.maxRetries = DefaultMaxRetries
	}
	if c.maxRetries < 0 {
		c.maxRetries = 0
	}
	if c.backoffBase <= 0 {
		c.backoffBase = DefaultBackoffBase
	}
	if c.maxBackoff <= 0 {
		c.maxBackoff = DefaultMaxBackoff
	}
	if c.sleep == nil {
		c.sleep = sleepContext
	}
	if c.jitter == nil {
		c.jitter = defaultJitter
	}
	return c, nil
}

// WithRetryBudget returns a copy of c whose calls give up instead of sleeping
// once the waits between retries would add up to more than budget. c itself is
// not changed. A budget of zero or less removes the cap.
//
// ⚠️ IT EXISTS FOR CALLERS THAT ARE NOT A TERMINAL. The CLI honours a 429's
// Retry-After for up to MaxRetryAfter (two minutes), which is right for a
// person who ran one command and can see it waiting. An agent's tool call is
// different: two minutes of silence reads as a hung tool, and the agent cannot
// tell "waiting on a burst limit" from "broken". The MCP server sets a short
// budget, so a 429 comes back fast as an error the agent can read and act on.
//
// A wait that would cross the budget is not shortened — the call returns the
// error it has. Shortening a server-sent Retry-After only buys a second 429.
func (c *Client) WithRetryBudget(budget time.Duration) *Client {
	cp := *c
	if budget < 0 {
		budget = 0
	}
	cp.retryBudget = budget
	return &cp
}

// BaseURL is the base this client talks to.
func (c *Client) BaseURL() string { return c.baseURL.String() }

// HasToken reports whether a credential is configured. /ping and
// /openapi.yaml work without one.
func (c *Client) HasToken() bool { return c.token != "" }

// request is one call, before retries.
type request struct {
	path  string
	query url.Values
	// anonymous skips the Authorization header. /ping and /openapi.yaml are
	// unauthenticated on purpose: without that, a client cannot tell "the
	// service is down" from "my token is wrong".
	anonymous bool
	accept    string
}

// get sends a GET, retries what is safe to retry, and decodes a 2xx body into
// out. out may be nil for a caller that only wants the raw bytes.
func (c *Client) get(ctx context.Context, req request, out any) (Meta, error) {
	body, meta, err := c.getRaw(ctx, req)
	if err != nil {
		return meta, err
	}
	if out == nil {
		return meta, nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return meta, fmt.Errorf("api: %s answered with a body this client could not read: %w", req.path, err)
	}
	return meta, nil
}

// getRaw is get without the JSON decode.
func (c *Client) getRaw(ctx context.Context, req request) ([]byte, Meta, error) {
	endpoint, err := c.resolve(req.path, req.query)
	if err != nil {
		return nil, Meta{}, err
	}

	var slept time.Duration
	for attempt := 0; ; attempt++ {
		body, meta, err := c.attempt(ctx, endpoint, req, attempt+1)
		if err == nil {
			return body, meta, nil
		}
		if ctx.Err() != nil {
			return nil, meta, err
		}
		if attempt >= c.maxRetries {
			return nil, meta, err
		}
		wait, ok := c.retryDelay(err, attempt)
		if !ok {
			return nil, meta, err
		}
		if c.retryBudget > 0 && slept+wait > c.retryBudget {
			return nil, meta, err
		}
		if err := c.sleep(ctx, wait); err != nil {
			return nil, meta, err
		}
		slept += wait
	}
}

func (c *Client) attempt(ctx context.Context, endpoint string, req request, attempt int) ([]byte, Meta, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, Meta{Attempts: attempt}, fmt.Errorf("api: cannot build the request: %w", err)
	}
	accept := req.accept
	if accept == "" {
		accept = "application/json"
	}
	httpReq.Header.Set("Accept", accept)
	httpReq.Header.Set("User-Agent", c.userAgent)
	if !req.anonymous && c.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(httpReq)
	if err != nil {
		// A transport failure carries no envelope. Wrap it so the retry
		// loop can see it and a caller can still read it.
		return nil, Meta{Attempts: attempt}, &TransportError{Endpoint: endpoint, Err: err}
	}
	defer resp.Body.Close()

	body, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	meta := metaFrom(resp, attempt)
	if readErr != nil {
		return nil, meta, &TransportError{Endpoint: endpoint, Err: readErr}
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, meta, parseError(resp.StatusCode, resp.Header, body, meta)
	}
	return body, meta, nil
}

// TransportError is a failure that never reached the API — DNS, a refused
// connection, a truncated read. Retryable: a GET has no side effect to repeat.
type TransportError struct {
	Endpoint string
	Err      error
}

func (e *TransportError) Error() string {
	return fmt.Sprintf("api: cannot reach %s: %v", e.Endpoint, e.Err)
}

func (e *TransportError) Unwrap() error { return e.Err }

// retryDelay decides whether to send this request again, and after how long.
//
// ⚠️ NEVER retry a 4xx that is not 429. A 400 repeated is a 400; a 404 for an
// unknown place never becomes a hit; and an invalid_cursor retried is the
// infinite loop the contract warns about by name.
func (c *Client) retryDelay(err error, attempt int) (time.Duration, bool) {
	var rateErr *RateLimitError
	if errors.As(err, &rateErr) {
		if rateErr.HasRetry {
			if rateErr.Wait > MaxRetryAfter {
				return 0, false
			}
			return rateErr.Wait, true
		}
		return c.backoff(attempt), true
	}

	var transportErr *TransportError
	if errors.As(err, &transportErr) {
		return c.backoff(attempt), true
	}

	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status >= 500 {
		return c.backoff(attempt), true
	}
	return 0, false
}

// backoff is bounded exponential: base × 2^attempt, capped, then jittered.
func (c *Client) backoff(attempt int) time.Duration {
	if attempt < 0 {
		attempt = 0
	}
	if attempt > 20 {
		attempt = 20
	}
	d := c.backoffBase << attempt
	if d > c.maxBackoff || d <= 0 {
		d = c.maxBackoff
	}
	return c.jitter(d)
}

// defaultJitter spreads retries over the last quarter of the window, so a
// burst of clients does not come back in lockstep.
func defaultJitter(d time.Duration) time.Duration {
	if d <= 0 {
		return d
	}
	spread := d / 4
	if spread <= 0 {
		return d
	}
	return d - spread + time.Duration(rand.Int63n(int64(spread)+1))
}

func sleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// resolve joins a path onto the base URL, keeping the base's own path (the
// production base ends in /public/v1).
func (c *Client) resolve(path string, query url.Values) (string, error) {
	u := *c.baseURL
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + strings.TrimPrefix(path, "/")
	if len(query) > 0 {
		u.RawQuery = query.Encode()
	} else {
		u.RawQuery = ""
	}
	return u.String(), nil
}

func metaFrom(resp *http.Response, attempt int) Meta {
	h := resp.Header
	return Meta{
		Status:             resp.StatusCode,
		RequestID:          h.Get("X-Request-Id"),
		QuotaGroup:         h.Get("X-Quota-Group"),
		QuotaLimit:         h.Get("X-Quota-Limit"),
		QuotaUsed:          headerInt(h, "X-Quota-Used"),
		QuotaReset:         h.Get("X-Quota-Reset"),
		RateLimitLimit:     headerInt(h, "X-RateLimit-Limit"),
		RateLimitRemaining: headerInt(h, "X-RateLimit-Remaining"),
		Attempts:           attempt,
	}
}

func headerInt(h http.Header, name string) *int64 {
	raw := strings.TrimSpace(h.Get(name))
	if raw == "" {
		return nil
	}
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return nil
	}
	return &n
}

func parseInt(s string) (int, error) { return strconv.Atoi(strings.TrimSpace(s)) }
