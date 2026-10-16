package mcpserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/investviews/investviews-cli/internal/api"
)

// The hosted-mode tests run the real HTTP handler on an httptest server and
// talk to it the way an agent does: plain HTTP for the auth edge, and the
// SDK's Streamable HTTP client for the protocol. The public API is a second
// httptest server, so nothing here leaves the process.

const (
	testPublicURL  = "https://mcp.investviews.ai"
	testAuthServer = "https://api.investviews.ai"
	testMetaURL    = testPublicURL + "/.well-known/oauth-protected-resource"

	// Real API tokens: a prefix, then 32 URL-safe characters.
	goodToken  = "iv_live_ro_good_000000000000000000000000000"
	otherToken = "iv_live_ro_other_11111111111111111111111111"
	lapsedTok  = "iv_live_ro_lapsed_2222222222222222222222222"
	badToken   = "iv_live_ro_bad_3333333333333333333333333333"

	testUserAgent = api.DefaultUserAgent + "-mcp/test"
)

// hostedAPI is a fake public API that knows which tokens exist. It records
// the Authorization header of every request, so a test can prove which token
// a tool call was sent with — and how many token checks reached the API.
type hostedAPI struct {
	server *httptest.Server

	mu sync.Mutex
	// status overrides the answer to /usage for every token while non-zero:
	// a test sets it to 500 or 429 to play an outage.
	status  int
	seen    []string        // "Bearer …" of every request, in order
	revoked map[string]bool // tokens the user revoked in their account
}

func newHostedAPI(t *testing.T) *hostedAPI {
	t.Helper()
	h := &hostedAPI{}
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.seen = append(h.seen, r.Header.Get("Authorization"))
		status := h.status
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		revoked := h.revoked[token]
		h.mu.Unlock()

		if status != 0 {
			writeBody(w, status, `{"error":"internal_error","message":"down"}`)
			return
		}
		switch {
		case revoked:
			writeBody(w, http.StatusUnauthorized, `{"error":"unauthorized","message":"Token revoked."}`)
		case token == goodToken, token == otherToken:
			freeHeaders(w)
			writeBody(w, http.StatusOK, usageReply)
		case token == lapsedTok:
			writeBody(w, http.StatusPaymentRequired,
				`{"error":"subscription_inactive","message":"Your plan has ended."}`)
		default:
			writeBody(w, http.StatusUnauthorized, `{"error":"unauthorized","message":"Bad token."}`)
		}
	}))
	t.Cleanup(h.server.Close)
	return h
}

// revoke makes the API refuse token from now on, as a revoke in the user's
// account does.
func (h *hostedAPI) revoke(token string) {
	h.mu.Lock()
	if h.revoked == nil {
		h.revoked = map[string]bool{}
	}
	h.revoked[token] = true
	h.mu.Unlock()
}

func (h *hostedAPI) setStatus(status int) {
	h.mu.Lock()
	h.status = status
	h.mu.Unlock()
}

// total is how many requests reached the API, with any token or none.
func (h *hostedAPI) total() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.seen)
}

// calls is how many requests reached the API with token.
func (h *hostedAPI) calls(token string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, s := range h.seen {
		if s == "Bearer "+token {
			n++
		}
	}
	return n
}

// fakeClock is a settable clock for the token cache.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// hostedServer starts the hosted handler in front of api and returns its URL.
func hostedServer(t *testing.T, api *hostedAPI, tweak ...func(*HTTPOptions)) *httptest.Server {
	t.Helper()
	opts := HTTPOptions{
		PublicURL:  testPublicURL,
		AuthServer: testAuthServer,
		APIURL:     api.server.URL + "/public/v1",
		Version:    testVersion,
	}
	for _, f := range tweak {
		f(&opts)
	}
	handler, err := NewHTTPHandler(opts)
	if err != nil {
		t.Fatalf("NewHTTPHandler: %v", err)
	}
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

const initializeBody = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`

// postMCP sends one raw JSON-RPC initialize to /mcp with an optional bearer
// token and an optional Host header.
func postMCP(t *testing.T, srv *httptest.Server, token, host string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader(initializeBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if host != "" {
		req.Host = host
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /mcp: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

// bearerTransport adds one token to every request, the way an agent sends the
// access token it got from the OAuth flow.
type bearerTransport struct{ token string }

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return http.DefaultTransport.RoundTrip(r)
}

// connectHTTP opens an MCP session over Streamable HTTP with token.
func connectHTTP(t *testing.T, srv *httptest.Server, token string) *mcp.ClientSession {
	t.Helper()
	transport := &mcp.StreamableClientTransport{
		Endpoint:             srv.URL + "/mcp",
		HTTPClient:           &http.Client{Transport: bearerTransport{token: token}},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	session, err := client.Connect(context.Background(), transport, nil)
	if err != nil {
		t.Fatalf("connect over HTTP: %v", err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func TestHTTPNoTokenIs401WithResourceMetadata(t *testing.T) {
	api := newHostedAPI(t)
	srv := hostedServer(t, api)

	resp := postMCP(t, srv, "", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	got := resp.Header.Get("WWW-Authenticate")
	want := `Bearer resource_metadata="` + testMetaURL + `"`
	if got != want {
		t.Errorf("WWW-Authenticate = %q, want %q", got, want)
	}
	if api.total() != 0 {
		t.Errorf("a request with no token reached the API %d times, want 0", api.total())
	}
}

func TestHTTPMetadataOnBothPaths(t *testing.T) {
	srv := hostedServer(t, newHostedAPI(t))

	for _, path := range []string{
		"/.well-known/oauth-protected-resource",
		"/.well-known/oauth-protected-resource/mcp",
	} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s = %d, want 200", path, resp.StatusCode)
		}
		var doc struct {
			Resource             string   `json:"resource"`
			AuthorizationServers []string `json:"authorization_servers"`
			BearerMethods        []string `json:"bearer_methods_supported"`
		}
		if err := json.Unmarshal(body, &doc); err != nil {
			t.Fatalf("GET %s: body is not JSON: %v\n%s", path, err, body)
		}
		if doc.Resource != testPublicURL+"/mcp" {
			t.Errorf("%s resource = %q, want %q", path, doc.Resource, testPublicURL+"/mcp")
		}
		if len(doc.AuthorizationServers) != 1 || doc.AuthorizationServers[0] != testAuthServer {
			t.Errorf("%s authorization_servers = %v, want [%s]", path, doc.AuthorizationServers, testAuthServer)
		}
		if len(doc.BearerMethods) != 1 || doc.BearerMethods[0] != "header" {
			t.Errorf("%s bearer_methods_supported = %v, want [header]", path, doc.BearerMethods)
		}
	}
}

func TestHTTPInvalidTokenIs401(t *testing.T) {
	api := newHostedAPI(t)
	srv := hostedServer(t, api)

	resp := postMCP(t, srv, badToken, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("WWW-Authenticate"), `resource_metadata="`+testMetaURL+`"`) {
		t.Errorf("WWW-Authenticate = %q, want the resource_metadata URL", resp.Header.Get("WWW-Authenticate"))
	}
}

func TestHTTPValidTokenListsToolsAndCallsAFreeTool(t *testing.T) {
	api := newHostedAPI(t)
	srv := hostedServer(t, api)
	session := connectHTTP(t, srv, goodToken)

	list, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list over HTTP: %v", err)
	}
	if len(list.Tools) != len(wantTools) {
		t.Errorf("tools/list returned %d tools, want %d", len(list.Tools), len(wantTools))
	}

	res := callTool(t, session, "usage", nil)
	if res.IsError {
		t.Fatalf("usage over HTTP failed: %s", textOf(t, res))
	}
	var payload struct {
		Groups map[string]any `json:"groups"`
	}
	structured(t, res, &payload)
	if len(payload.Groups) == 0 {
		t.Error("usage over HTTP returned no groups")
	}
}

// TestHTTPTwoTokensUseTwoClients is the per-request client rule: one shared
// server, and each call is sent with the token of the request that made it.
// A mix-up here spends one user's quota on another's question.
func TestHTTPTwoTokensUseTwoClients(t *testing.T) {
	api := newHostedAPI(t)
	srv := hostedServer(t, api)
	first := connectHTTP(t, srv, goodToken)
	second := connectHTTP(t, srv, otherToken)

	before1, before2 := api.calls(goodToken), api.calls(otherToken)
	for range 2 {
		if res := callTool(t, first, "usage", nil); res.IsError {
			t.Fatalf("usage (first token): %s", textOf(t, res))
		}
	}
	if res := callTool(t, second, "usage", nil); res.IsError {
		t.Fatalf("usage (second token): %s", textOf(t, res))
	}
	// The token checks are cached, so every new request here is a tool call.
	if got := api.calls(goodToken) - before1; got != 2 {
		t.Errorf("tool calls sent with the first token = %d, want 2", got)
	}
	if got := api.calls(otherToken) - before2; got != 1 {
		t.Errorf("tool calls sent with the second token = %d, want 1", got)
	}
}

// TestHTTPLoopbackListenerAcceptsPublicHost is the nginx case: the service
// listens on 127.0.0.1 and nginx passes Host: mcp.investviews.ai. The SDK's
// DNS-rebinding guard would answer 403 to every request.
func TestHTTPLoopbackListenerAcceptsPublicHost(t *testing.T) {
	srv := hostedServer(t, newHostedAPI(t))
	if !strings.Contains(srv.URL, "127.0.0.1") {
		t.Skipf("httptest listens on %s, not loopback", srv.URL)
	}

	resp := postMCP(t, srv, goodToken, "mcp.investviews.ai")
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200 (body %q)", resp.StatusCode, body)
	}
}

func TestHTTPHealthzNeedsNoToken(t *testing.T) {
	api := newHostedAPI(t)
	srv := hostedServer(t, api)

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", resp.StatusCode)
	}
	if api.total() != 0 {
		t.Errorf("/healthz reached the API %d times, want 0", api.total())
	}
}

// ── the verifier ─────────────────────────────────────────────────────────────

func TestVerifierMapsEveryStatus(t *testing.T) {
	for _, tc := range []struct {
		name    string
		token   string
		outage  int
		wantErr error // nil = valid
	}{
		{name: "200 is valid", token: goodToken},
		{name: "402 plan lapsed is valid", token: lapsedTok},
		{name: "401 is invalid", token: badToken, wantErr: auth.ErrInvalidToken},
		{name: "500 is unavailable", token: goodToken, outage: 500, wantErr: errAPIUnavailable},
		{name: "502 is unavailable", token: goodToken, outage: 502, wantErr: errAPIUnavailable},
		{name: "503 is unavailable", token: goodToken, outage: 503, wantErr: errAPIUnavailable},
		{name: "429 is unavailable", token: goodToken, outage: 429, wantErr: errAPIUnavailable},
		{name: "404 is unavailable", token: goodToken, outage: 404, wantErr: errAPIUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := newHostedAPI(t)
			api.setStatus(tc.outage)
			clock := &fakeClock{now: time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)}
			v := newVerifier(api.server.URL+"/public/v1", testUserAgent, TokenCacheTTL, nil, discard, clock.Now)

			info, err := v.verify(context.Background(), tc.token, nil)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				if info != nil {
					t.Errorf("a refused token returned token info")
				}
				return
			}
			if err != nil {
				t.Fatalf("err = %v, want a valid token", err)
			}
			if got := tokenFromInfo(info); got != tc.token {
				t.Errorf("token carried in TokenInfo = %q, want the request's token", got)
			}
		})
	}
}

func TestVerifierNetworkErrorIsUnavailable(t *testing.T) {
	api := newHostedAPI(t)
	url := api.server.URL + "/public/v1"
	api.server.Close()

	v := newVerifier(url, testUserAgent, TokenCacheTTL, nil, discard, time.Now)
	if _, err := v.verify(context.Background(), goodToken, nil); !errors.Is(err, errAPIUnavailable) {
		t.Fatalf("err = %v, want errAPIUnavailable", err)
	}
}

func TestVerifierCachesValidAndInvalidForTheTTL(t *testing.T) {
	api := newHostedAPI(t)
	clock := &fakeClock{now: time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)}
	v := newVerifier(api.server.URL+"/public/v1", testUserAgent, TokenCacheTTL, nil, discard, clock.Now)
	ctx := context.Background()

	for range 3 {
		if _, err := v.verify(ctx, goodToken, nil); err != nil {
			t.Fatal(err)
		}
		if _, err := v.verify(ctx, badToken, nil); !errors.Is(err, auth.ErrInvalidToken) {
			t.Fatalf("bad token: err = %v", err)
		}
	}
	if got := api.calls(goodToken); got != 1 {
		t.Errorf("valid token checked %d times inside the TTL, want 1", got)
	}
	if got := api.calls(badToken); got != 1 {
		t.Errorf("invalid token checked %d times inside the TTL, want 1", got)
	}

	// A revoked token keeps working until its entry expires — at most one
	// TTL — and is refused after that.
	api.revoke(goodToken)
	if _, err := v.verify(ctx, goodToken, nil); err != nil {
		t.Fatalf("revoked token inside the TTL: err = %v, want the cached verdict", err)
	}
	clock.Advance(TokenCacheTTL + time.Second)
	if _, err := v.verify(ctx, goodToken, nil); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("revoked token after the TTL: err = %v, want auth.ErrInvalidToken", err)
	}
	if got := api.calls(goodToken); got != 2 {
		t.Errorf("valid token checked %d times after the TTL, want 2", got)
	}
}

// TestHTTPRevokedTokenIs401AfterTheTTL is the same rule at the HTTP edge: the
// agent gets a 401 that points it back at the metadata, so it signs in again.
func TestHTTPRevokedTokenIs401AfterTheTTL(t *testing.T) {
	api := newHostedAPI(t)
	clock := &fakeClock{now: time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)}
	srv := hostedServer(t, api, func(o *HTTPOptions) { o.now = clock.Now })

	if resp := postMCP(t, srv, goodToken, ""); resp.StatusCode != http.StatusOK {
		t.Fatalf("before the revoke: status = %d, want 200", resp.StatusCode)
	}
	api.revoke(goodToken)
	clock.Advance(TokenCacheTTL + time.Second)

	resp := postMCP(t, srv, goodToken, "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("after the revoke and the TTL: status = %d, want 401", resp.StatusCode)
	}
	if !strings.Contains(resp.Header.Get("WWW-Authenticate"), `resource_metadata="`+testMetaURL+`"`) {
		t.Errorf("WWW-Authenticate = %q, want the resource_metadata URL", resp.Header.Get("WWW-Authenticate"))
	}
}

// TestVerifierRefusesMalformedTokensWithoutAnAPICall: a string that cannot
// be an API token is refused here, so garbage never costs the API a request.
func TestVerifierRefusesMalformedTokensWithoutAnAPICall(t *testing.T) {
	api := newHostedAPI(t)
	v := newVerifier(api.server.URL+"/public/v1", testUserAgent, TokenCacheTTL, nil, discard, time.Now)

	for name, token := range map[string]string{
		"no prefix":        "abc",
		"another vendor":   "sk-0000000000000000000000000000000000000000",
		"prefix only":      "iv_live_",
		"too short":        "iv_live_ro_x",
		"too long":         "iv_live_ro_" + strings.Repeat("a", 200),
		"not URL-safe":     "iv_live_ro_good_00000000000000000000000000.",
		"a space inside":   "iv_live_ro_good 000000000000000000000000000",
		"a non-ASCII byte": "iv_live_ro_good_00000000000000000000000000é",
	} {
		if _, err := v.verify(context.Background(), token, nil); !errors.Is(err, auth.ErrInvalidToken) {
			t.Errorf("%s: err = %v, want auth.ErrInvalidToken", name, err)
		}
	}
	if api.total() != 0 {
		t.Errorf("malformed tokens reached the API %d times, want 0", api.total())
	}

	// A read/write token is well-formed: the API decides about it.
	rw := "iv_live_rw_" + strings.Repeat("a", 32)
	if _, err := v.verify(context.Background(), rw, nil); !errors.Is(err, auth.ErrInvalidToken) {
		t.Errorf("unknown rw token: err = %v, want auth.ErrInvalidToken", err)
	}
	if api.total() != 1 {
		t.Errorf("a well-formed token reached the API %d times, want 1", api.total())
	}
}

// TestVerifierSendsItsUserAgent: the token check names the server and its
// version, like every tool call does.
func TestVerifierSendsItsUserAgent(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("User-Agent")
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	v := newVerifier(srv.URL+"/public/v1", testUserAgent, TokenCacheTTL, nil, discard, time.Now)
	if _, err := v.verify(context.Background(), goodToken, nil); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got != testUserAgent {
		t.Errorf("User-Agent = %q, want %q", got, testUserAgent)
	}
}

// TestVerifierTimeoutIsUnavailable: a slow API is an outage, not a verdict.
func TestVerifierTimeoutIsUnavailable(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(slow.Close)

	v := newVerifier(slow.URL+"/public/v1", testUserAgent, TokenCacheTTL, nil, discard, time.Now)
	v.timeout = 20 * time.Millisecond
	if _, err := v.verify(context.Background(), goodToken, nil); !errors.Is(err, errAPIUnavailable) {
		t.Fatalf("err = %v, want errAPIUnavailable", err)
	}
}

// TestTokenCacheSweepsExpiredEntries: once the cache holds sweepAt entries,
// a put drops every expired one and keeps the live ones.
func TestTokenCacheSweepsExpiredEntries(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)}
	c := newTokenCache(time.Minute, clock.Now)
	key := func(i int) [32]byte { return sha256.Sum256([]byte(strconv.Itoa(i))) }

	for i := range sweepAt - 1 {
		c.put(key(i), true)
	}
	clock.Advance(30 * time.Second)
	c.put(key(-1), true) // live after the next advance
	clock.Advance(31 * time.Second)
	c.put(key(-2), false)

	if got := len(c.entries); got != 2 {
		t.Errorf("entries after the sweep = %d, want 2", got)
	}
	if valid, ok := c.get(key(-1)); !ok || !valid {
		t.Errorf("the live entry: valid=%v ok=%v, want true true", valid, ok)
	}
}

// TestOutageAs503LeavesOtherStatusesAlone: only a flagged request's 500 turns
// into a 503. A 500 from anything else passes through as is.
func TestOutageAs503LeavesOtherStatusesAlone(t *testing.T) {
	h := outageAs503(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if rec.Header().Get("Retry-After") != "" {
		t.Errorf("an unflagged 500 got Retry-After %q", rec.Header().Get("Retry-After"))
	}
}

// TestBearerClientNeverFallsBack: a hosted call without its own token is
// refused, and the factory is never asked for a client.
func TestBearerClientNeverFallsBack(t *testing.T) {
	var got []string
	clientFor := bearerClient(func(token string) (*api.Client, error) {
		got = append(got, token)
		return api.New(api.Options{BaseURL: "http://127.0.0.1:1/public/v1", Token: token})
	})

	for name, req := range map[string]*mcp.CallToolRequest{
		"nil request":            nil,
		"no extra":               {},
		"no token info":          {Extra: &mcp.RequestExtra{}},
		"token info, no extra":   {Extra: &mcp.RequestExtra{TokenInfo: &auth.TokenInfo{}}},
		"token info, other key":  {Extra: &mcp.RequestExtra{TokenInfo: &auth.TokenInfo{Extra: map[string]any{"other": goodToken}}}},
		"token info, not string": {Extra: &mcp.RequestExtra{TokenInfo: &auth.TokenInfo{Extra: map[string]any{tokenExtraKey: 42}}}},
	} {
		if _, err := clientFor(req); !errors.Is(err, errNoBearerToken) {
			t.Errorf("%s: err = %v, want errNoBearerToken", name, err)
		}
	}
	if len(got) != 0 {
		t.Fatalf("the factory was called %d times for calls with no token", len(got))
	}

	req := &mcp.CallToolRequest{Extra: &mcp.RequestExtra{TokenInfo: tokenInfo(goodToken)}}
	if _, err := clientFor(req); err != nil {
		t.Fatalf("with a token: %v", err)
	}
	if len(got) != 1 || got[0] != goodToken {
		t.Errorf("factory got %q, want [%s]", got, goodToken)
	}
}

// TestVerifierDoesNotCacheAnOutage: an API outage must not lock everyone out
// for a minute after the API is back, and must not force a new sign-in.
func TestVerifierDoesNotCacheAnOutage(t *testing.T) {
	api := newHostedAPI(t)
	v := newVerifier(api.server.URL+"/public/v1", testUserAgent, TokenCacheTTL, nil, discard, time.Now)
	ctx := context.Background()

	api.setStatus(http.StatusInternalServerError)
	if _, err := v.verify(ctx, goodToken, nil); !errors.Is(err, errAPIUnavailable) {
		t.Fatalf("during the outage: err = %v, want errAPIUnavailable", err)
	}
	api.setStatus(0)
	if _, err := v.verify(ctx, goodToken, nil); err != nil {
		t.Fatalf("after the outage: err = %v, want a valid token", err)
	}
	if got := api.calls(goodToken); got != 2 {
		t.Errorf("token checked %d times, want 2 (the outage must not be cached)", got)
	}
}

func TestHTTPOutageIs503NotA401(t *testing.T) {
	api := newHostedAPI(t)
	srv := hostedServer(t, api)

	api.setStatus(http.StatusBadGateway)
	resp := postMCP(t, srv, goodToken, "")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	// A 401 here would send every connected agent back through sign-in.
	if h := resp.Header.Get("WWW-Authenticate"); h != "" {
		t.Errorf("an outage answered with WWW-Authenticate %q, want none", h)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Error("an outage answered with no Retry-After")
	}

	api.setStatus(0)
	if resp := postMCP(t, srv, goodToken, ""); resp.StatusCode != http.StatusOK {
		t.Errorf("after the outage: status = %d, want 200", resp.StatusCode)
	}
}

// TestHTTPNeverLogsTokens runs every path that logs — a refused token, an
// outage, a good call — and checks no token reached the log.
func TestHTTPNeverLogsTokens(t *testing.T) {
	var buf safeBuffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	api := newHostedAPI(t)
	srv := hostedServer(t, api, func(o *HTTPOptions) { o.Logger = logger })

	postMCP(t, srv, badToken, "")
	api.setStatus(http.StatusInternalServerError)
	postMCP(t, srv, otherToken, "")
	api.setStatus(0)
	session := connectHTTP(t, srv, goodToken)
	callTool(t, session, "usage", nil)

	logged := buf.String()
	if logged == "" {
		t.Fatal("nothing was logged; the test would pass vacuously")
	}
	for _, token := range []string{goodToken, otherToken, badToken, "iv_live_ro_"} {
		if strings.Contains(logged, token) {
			t.Errorf("the log contains a token (%q):\n%s", token, logged)
		}
	}
}

func TestNewHTTPHandlerRefusesBadURLs(t *testing.T) {
	base := HTTPOptions{PublicURL: testPublicURL, AuthServer: testAuthServer, APIURL: "http://127.0.0.1:1/public/v1"}
	for name, tweak := range map[string]func(*HTTPOptions){
		"public URL without scheme": func(o *HTTPOptions) { o.PublicURL = "mcp.investviews.ai" },
		"public URL with a path":    func(o *HTTPOptions) { o.PublicURL = testPublicURL + "/mcp" },
		"auth server without host":  func(o *HTTPOptions) { o.AuthServer = "https://" },
		"no API URL":                func(o *HTTPOptions) { o.APIURL = "" },
	} {
		t.Run(name, func(t *testing.T) {
			opts := base
			tweak(&opts)
			if _, err := NewHTTPHandler(opts); err == nil {
				t.Error("NewHTTPHandler accepted it, want an error")
			}
		})
	}
	// A trailing slash is not a path.
	opts := base
	opts.PublicURL = testPublicURL + "/"
	if _, err := NewHTTPHandler(opts); err != nil {
		t.Errorf("trailing slash refused: %v", err)
	}
}

// discard is a logger that drops everything.
var discard = slog.New(slog.DiscardHandler)

// safeBuffer is a bytes.Buffer the server's goroutines can log into while
// the test reads it.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
