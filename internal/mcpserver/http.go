package mcpserver

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/investviews/investviews-cli/internal/api"
)

// This file is the hosted mode: the same tools as `investviews mcp`, served
// over Streamable HTTP at https://mcp.investviews.ai/mcp to agents that sign in
// with OAuth (Claude Desktop, claude.ai, ChatGPT, Claude Code, …).
//
// The flow, from the agent's side:
//
//  1. POST /mcp with no token → 401 with
//     WWW-Authenticate: Bearer resource_metadata="<public URL>/.well-known/oauth-protected-resource".
//  2. GET that document → it names the authorization server (api.investviews.ai).
//  3. The agent runs the OAuth flow there and gets an access token. That token
//     IS a normal read-only InvestViews API token (iv_live_ro_…).
//  4. POST /mcp with Authorization: Bearer <token> → the tools work, and every
//     API call is sent with that same token.
//
// Four rules hold for this file:
//
//  1. ⚠️ THE TOKEN IS PASSED THROUGH TO THE API, ON PURPOSE. The MCP spec says a
//     server should not forward its token upstream. Here the token is an
//     InvestViews API token and the upstream is the InvestViews API, so
//     forwarding it is the design: metering, the PRO check, rate limits and
//     revocation all keep working with no new code in the API. The token is
//     read-only and the user can revoke it in their account at any time.
//
//  2. ⚠️ ONE SHARED SERVER, ONE CLIENT PER CALL. The mcp.Server is built once.
//     Each tool call reads the token from req.Extra.TokenInfo and builds its
//     API client from it (bearerClient). There is no default token
//     on a hosted server, so a call can never be sent with someone else's.
//
//  3. ⚠️ TOKENS ARE NEVER LOGGED. Not in the verifier, not in an error text,
//     not in the SDK's logs. The cache is keyed by a SHA-256 digest of the
//     token, so not even the cache holds the plaintext. A test logs every path
//     at debug level and fails if a token prefix shows up.
//
//  4. ⚠️ AN API OUTAGE IS A 503, NEVER A 401. A 401 tells every connected agent
//     "your sign-in is gone" and sends its user back through the OAuth flow.
//     When the token check cannot reach a verdict (5xx, 429, network error),
//     the answer is 503 with Retry-After, and nothing is cached.
//
// ⚠️ THE RESOURCE BINDING IS ENFORCED WHEN THE TOKEN IS ISSUED, NOT HERE. The
// authorization server only hands out a token for this resource. This server
// cannot tell such a token from any other: the check below asks the API
// whether the token is valid, and the API does not say which resource it was
// issued for. So any valid InvestViews API token works at /mcp, including
// one the user created by hand. The tools are read-only, so a token that
// works here can do nothing it could not do against the API directly.

const (
	// MCPPath is where the protocol is served.
	MCPPath = "/mcp"

	// ProtectedResourcePath is the RFC 9728 metadata document. It is ALSO
	// served at ProtectedResourcePath + MCPPath: RFC 9728 puts the document
	// for https://host/mcp at /.well-known/oauth-protected-resource/mcp, and
	// clients differ in which of the two they try first.
	ProtectedResourcePath = "/.well-known/oauth-protected-resource"

	// HealthPath answers 200 with no token and no API call — for the service
	// manager and nginx, not for agents.
	HealthPath = "/healthz"

	// DefaultPublicURL is the public origin of the hosted server, and
	// DefaultAuthServer the authorization server its metadata names.
	DefaultPublicURL  = "https://mcp.investviews.ai"
	DefaultAuthServer = "https://api.investviews.ai"

	// TokenCacheTTL is how long one token check is trusted. It is also how
	// long a token revoked in the user's account keeps working here: at most
	// one minute.
	TokenCacheTTL = 60 * time.Second

	// VerifyTimeout bounds one token check. The check is one small GET; a
	// slow answer is an outage (503), not a reason to hold the agent.
	// internal/commands/mcp.go adds it to ToolTimeout to size its stop wait.
	VerifyTimeout = 10 * time.Second

	// tokenPrefix, minTokenLen and maxTokenLen describe a plausible API
	// token, for the pre-filter in plausibleToken. Today's tokens are
	// iv_live_ro_ or iv_live_rw_ and 32 URL-safe characters (43 in all). The
	// bounds are loose on purpose: a change in the token format must not
	// lock every user out of the hosted server; the API stays the judge.
	tokenPrefix = "iv_live_"
	minTokenLen = 24
	maxTokenLen = 128

	// outageRetryAfter is the Retry-After, in seconds, sent with a 503.
	outageRetryAfter = "5"

	// tokenExtraKey is where the verifier puts the plaintext token in
	// TokenInfo.Extra, for bearerClient to read. TokenInfo lives only
	// for the request; nothing in this package or the SDK logs it.
	tokenExtraKey = "investviews_token"
)

var (
	// errAPIUnavailable is the verifier's answer when the API gave no
	// verdict on the token. NewHTTPHandler turns it into a 503.
	errAPIUnavailable = errors.New("the InvestViews API is not reachable right now; retry in a few seconds")

	// errNoBearerToken is a hosted tool call that arrived with no token
	// info — a wiring fault, see bearerClient.
	errNoBearerToken = errors.New("internal: this request carries no bearer token")
)

// HTTPOptions configures the hosted server.
type HTTPOptions struct {
	// PublicURL is the origin agents reach the server at, with no path:
	// https://mcp.investviews.ai. The protected resource is PublicURL +
	// MCPPath, and it must match the resource the authorization server
	// binds its tokens to.
	PublicURL string

	// AuthServer is the OAuth authorization server's issuer:
	// https://api.investviews.ai.
	AuthServer string

	// APIURL is the public API base, version included
	// (https://api.investviews.ai/public/v1). Token checks and tool calls
	// both go there.
	APIURL string

	// Version is the build stamp `initialize` reports.
	Version string

	// Logger gets the server's and the SDK's log lines; nil discards them.
	Logger *slog.Logger

	// NewClient builds the API client for one token. Nil uses api.New with
	// APIURL — tests replace it, nothing else should.
	NewClient func(token string) (*api.Client, error)

	// CacheTTL overrides TokenCacheTTL; zero keeps it. An end-to-end test
	// shortens it to watch a revoked token stop working.
	CacheTTL time.Duration

	// HTTPClient sends the token checks. Nil uses http.DefaultClient. Each
	// check is bounded by VerifyTimeout either way.
	HTTPClient *http.Client

	// now is the clock for the token cache. Tests set it.
	now func() time.Time
}

// NewHTTPHandler builds the hosted server's whole HTTP surface: /mcp behind
// bearer auth, the protected resource metadata on both paths, and /healthz.
//
// The caller runs it with an http.Server. It keeps no per-client state
// (stateless Streamable HTTP), so the process can restart at any time and an
// agent's next request simply works.
func NewHTTPHandler(opts HTTPOptions) (http.Handler, error) {
	publicURL, err := originURL("public URL", opts.PublicURL)
	if err != nil {
		return nil, err
	}
	authServer, err := originURL("authorization server URL", opts.AuthServer)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(opts.APIURL) == "" {
		return nil, errors.New("mcp http: an API URL is required")
	}
	apiURL := strings.TrimRight(strings.TrimSpace(opts.APIURL), "/")

	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	ttl := opts.CacheTTL
	if ttl <= 0 {
		ttl = TokenCacheTTL
	}
	now := opts.now
	if now == nil {
		now = time.Now
	}
	userAgent := api.DefaultUserAgent + "-mcp/" + opts.Version
	newClient := opts.NewClient
	if newClient == nil {
		newClient = func(token string) (*api.Client, error) {
			return api.New(api.Options{BaseURL: apiURL, Token: token, UserAgent: userAgent})
		}
	}

	srv := build(&server{clientFor: bearerClient(newClient)}, opts.Version, logger)

	// ⚠️ DisableLocalhostProtection: the SDK refuses, with 403, any request
	// that reaches a loopback listener with a non-loopback Host. That is
	// exactly the nginx setup — the service listens on 127.0.0.1 and nginx
	// passes Host: mcp.investviews.ai — so every request would be refused.
	// The guard exists against DNS rebinding of a LOCAL server; this one is
	// public, sits behind nginx, and every request needs a bearer token.
	mcpHandler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{
			Stateless:                  true,
			DisableLocalhostProtection: true,
			// A tool call is a few hundred bytes of JSON. The SDK's 4 MiB
			// default is far more than any honest request needs.
			MaxRequestBodyBytes: 1 << 20,
			Logger:              logger,
		},
	)

	resource := publicURL + MCPPath
	metadataURL := publicURL + ProtectedResourcePath
	v := newVerifier(apiURL, userAgent, ttl, opts.HTTPClient, logger, now)
	// ⚠️ AllowMissingExpiration: API tokens do not expire, so TokenInfo has
	// no Expiration, and RequireBearerToken refuses that by default ("token
	// missing expiration", 401). How long a verdict holds is the cache's job.
	protected := auth.RequireBearerToken(v.verify, &auth.RequireBearerTokenOptions{
		ResourceMetadataURL:    metadataURL,
		AllowMissingExpiration: true,
	})(mcpHandler)

	metadata := auth.ProtectedResourceMetadataHandler(&oauthex.ProtectedResourceMetadata{
		Resource:               resource,
		AuthorizationServers:   []string{authServer},
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "InvestViews",
		ResourceDocumentation:  "https://docs.investviews.ai",
	})

	mux := http.NewServeMux()
	mux.Handle(MCPPath, withCORS(outageAs503(protected)))
	mux.Handle(ProtectedResourcePath, metadata)
	mux.Handle(ProtectedResourcePath+MCPPath, metadata)
	mux.HandleFunc(HealthPath, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
	})
	return mux, nil
}

// originURL checks that raw is an absolute http(s) URL with no path, and
// returns it without a trailing slash. The resource identifier is built from
// it and compared byte for byte by the authorization server, so a stray path
// or slash here is a token that never validates.
func originURL(what, raw string) (string, error) {
	raw = strings.TrimRight(strings.TrimSpace(raw), "/")
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("mcp http: %s %q is not a URL: %w", what, raw, err)
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return "", fmt.Errorf("mcp http: %s %q needs an http(s) scheme and a host", what, raw)
	}
	if u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return "", fmt.Errorf("mcp http: %s %q must be an origin, with no path (got %q)", what, raw, u.Path)
	}
	return raw, nil
}

// tokenFromInfo is the plaintext token the verifier put into info, or "".
func tokenFromInfo(info *auth.TokenInfo) string {
	if info == nil {
		return ""
	}
	token, _ := info.Extra[tokenExtraKey].(string)
	return token
}

// bearerClient is the hosted server's clientFor: the API client for the
// bearer token the request was authorized with.
//
// ⚠️ IT NEVER FALLS BACK TO ANOTHER TOKEN. A call that arrives without token
// info is refused, not answered with some default credential: on a shared
// server, a fallback token is one user's quota spent on another user's
// question. RequireBearerToken sits in front of every /mcp request, so this
// error means a wiring fault, not a user mistake.
func bearerClient(newClient func(token string) (*api.Client, error)) func(*mcp.CallToolRequest) (*api.Client, error) {
	return func(req *mcp.CallToolRequest) (*api.Client, error) {
		var info *auth.TokenInfo
		if req != nil && req.Extra != nil {
			info = req.Extra.TokenInfo
		}
		token := tokenFromInfo(info)
		if token == "" {
			return nil, errNoBearerToken
		}
		return newClient(token)
	}
}

// plausibleToken is a cheap pre-filter: false for a string that cannot be an
// InvestViews API token, so garbage is refused (401) without costing the API
// a request. It checks the prefix, the length and the URL-safe alphabet only.
func plausibleToken(token string) bool {
	if len(token) < minTokenLen || len(token) > maxTokenLen || !strings.HasPrefix(token, tokenPrefix) {
		return false
	}
	for i := 0; i < len(token); i++ {
		c := token[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

// ── the verifier ─────────────────────────────────────────────────────────────

// verifier checks a bearer token with one free API call (GET /usage) and
// caches the verdict.
//
// The mapping:
//
//	200 → valid.
//	402 → valid. The token is fine; the plan lapsed. A tool call then gets the
//	      API's "reactivate" message, which the agent can show. Mapping it to
//	      401 would send the agent round a sign-in loop that cannot fix a plan.
//	401 → invalid (auth.ErrInvalidToken → 401 to the agent).
//	anything else, or a network error → errAPIUnavailable → 503, NOT cached.
//
// ⚠️ 429 IS AN OUTAGE HERE, NOT A VERDICT. It says nothing about the token.
// Other statuses (404 from a wrong API URL, 403 from a WAF) are this server's
// problem, not the user's, so they are 503s too — and logged with the status.
type verifier struct {
	usageURL  string
	userAgent string
	http      *http.Client
	timeout   time.Duration
	logger    *slog.Logger
	cache     *tokenCache
}

func newVerifier(apiURL, userAgent string, ttl time.Duration, httpClient *http.Client, logger *slog.Logger, now func() time.Time) *verifier {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &verifier{
		usageURL:  strings.TrimRight(apiURL, "/") + string(api.EndpointUsage),
		userAgent: userAgent,
		http:      httpClient,
		timeout:   VerifyTimeout,
		logger:    logger,
		cache:     newTokenCache(ttl, now),
	}
}

// verify is the auth.TokenVerifier. ⚠️ It must never put the token into an
// error or a log line: RequireBearerToken writes the error text into the
// response body, and the logger writes to the service's journal.
func (v *verifier) verify(ctx context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	if !plausibleToken(token) {
		v.logger.Debug("token check: malformed token refused without an API call")
		return nil, auth.ErrInvalidToken
	}
	key := sha256.Sum256([]byte(token))
	if valid, ok := v.cache.get(key); ok {
		if !valid {
			return nil, auth.ErrInvalidToken
		}
		return tokenInfo(token), nil
	}

	status, err := v.check(ctx, token)
	switch {
	case err != nil:
		// The error is from net/http; it names the usage URL, never the
		// Authorization header.
		v.logger.Warn("token check: API not reachable", "error", err)
		markOutage(ctx)
		return nil, errAPIUnavailable
	case status == http.StatusOK, status == http.StatusPaymentRequired:
		v.cache.put(key, true)
		return tokenInfo(token), nil
	case status == http.StatusUnauthorized:
		v.cache.put(key, false)
		v.logger.Debug("token check: token refused by the API")
		return nil, auth.ErrInvalidToken
	default:
		v.logger.Warn("token check: API gave no verdict", "status", status)
		markOutage(ctx)
		return nil, errAPIUnavailable
	}
}

// check sends GET /usage with token and returns the status. The body is
// drained and dropped: only the status is a verdict.
func (v *verifier) check(ctx context.Context, token string) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, v.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.usageURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", v.userAgent)
	resp, err := v.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, nil
}

// tokenInfo is what RequireBearerToken puts on the request, and what a tool
// call finds in req.Extra.TokenInfo. It has no Expiration: API tokens do not
// expire (see AllowMissingExpiration in NewHTTPHandler).
func tokenInfo(token string) *auth.TokenInfo {
	return &auth.TokenInfo{Extra: map[string]any{tokenExtraKey: token}}
}

// tokenCache holds token verdicts for ttl, keyed by the token's SHA-256.
//
// Both verdicts are cached: a valid one so a session of tool calls costs one
// check a minute, an invalid one so an agent retrying a dead token does not
// cost the API a request each time. Outages are never put in.
type tokenCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	now     func() time.Time
	entries map[[sha256.Size]byte]cacheEntry
}

type cacheEntry struct {
	valid bool
	until time.Time
}

// sweepAt is the cache size at which put drops every expired entry, so the
// map cannot grow without bound from tokens that are never seen again.
const sweepAt = 1024

func newTokenCache(ttl time.Duration, now func() time.Time) *tokenCache {
	return &tokenCache{ttl: ttl, now: now, entries: map[[sha256.Size]byte]cacheEntry{}}
}

func (c *tokenCache) get(key [sha256.Size]byte) (valid, ok bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return false, false
	}
	if !c.now().Before(e.until) {
		delete(c.entries, key)
		return false, false
	}
	return e.valid, true
}

// put stores a verdict for ttl.
func (c *tokenCache) put(key [sha256.Size]byte, valid bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if len(c.entries) >= sweepAt {
		for k, e := range c.entries {
			if !now.Before(e.until) {
				delete(c.entries, k)
			}
		}
	}
	c.entries[key] = cacheEntry{valid: valid, until: now.Add(c.ttl)}
}

// withCORS lets a browser-based MCP client reach /mcp.
//
// A preflight (OPTIONS) carries no token, so it is answered here, before
// bearer auth; otherwise the browser sees a 401 and never sends the POST.
// Every other response gets Allow-Origin and Expose-Headers, so the client
// can read the status, Mcp-Session-Id and WWW-Authenticate (the 401 that
// starts OAuth). "*" is safe: auth is a bearer header only, no cookies, so a
// foreign page gains nothing it could not do with the token directly.
//
// Mcp-Method and Mcp-Name: at protocol 2026-07-28 the SDK rejects a request
// without them (Mcp-Name on tools/call), so a preflight must allow both. No
// tool here declares header parameters, so no Mcp-Param-* header is needed.
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Expose-Headers", "Mcp-Session-Id, WWW-Authenticate")
		if r.Method == http.MethodOptions {
			h.Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
			h.Set("Access-Control-Allow-Headers",
				"Authorization, Content-Type, Mcp-Session-Id, Mcp-Protocol-Version, Last-Event-ID, "+
					"Mcp-Method, Mcp-Name")
			h.Set("Access-Control-Max-Age", "86400")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ── 503 on an outage ─────────────────────────────────────────────────────────

// outageKey marks a request whose token check hit an outage.
type outageKey struct{}

// outageAs503 turns the 500 that RequireBearerToken writes for a verifier
// error into a 503 with Retry-After.
//
// ⚠️ WHY A WRAPPER. RequireBearerToken maps ErrInvalidToken to 401 and EVERY
// other verifier error to 500; it has no way to say 503. A 500 is wrong for an
// upstream outage, and some clients treat it as fatal. So the verifier flags
// the request through its context (verify runs synchronously on it), and this
// wrapper rewrites only that one status, only on flagged requests. Every other
// response passes through untouched.
func outageAs503(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flag := &outageFlag{}
		r = r.WithContext(context.WithValue(r.Context(), outageKey{}, flag))
		next.ServeHTTP(&outageWriter{ResponseWriter: w, flag: flag}, r)
	})
}

type outageFlag struct{ set bool }

// markOutage flags the request in ctx, when it came through outageAs503.
func markOutage(ctx context.Context) {
	if f, ok := ctx.Value(outageKey{}).(*outageFlag); ok {
		f.set = true
	}
}

type outageWriter struct {
	http.ResponseWriter
	flag *outageFlag
}

func (w *outageWriter) WriteHeader(status int) {
	if status == http.StatusInternalServerError && w.flag.set {
		w.Header().Set("Retry-After", outageRetryAfter)
		status = http.StatusServiceUnavailable
	}
	w.ResponseWriter.WriteHeader(status)
}

// Unwrap lets http.ResponseController reach the real writer, so the SDK can
// still flush a streamed response through this wrapper.
func (w *outageWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// Flush keeps the wrapper an http.Flusher for code that type-asserts instead
// of using http.ResponseController.
func (w *outageWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
