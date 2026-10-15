package mcpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The hosted-mode tests run the real HTTP handler on an httptest server and
// talk to it the way an agent does: plain HTTP for the auth edge, and the
// SDK's Streamable HTTP client for the protocol. The public API is a second
// httptest server, so nothing here leaves the process.

const (
	testPublicURL  = "https://mcp.investviews.ai"
	testAuthServer = "https://api.investviews.ai"
	testMetaURL    = testPublicURL + "/.well-known/oauth-protected-resource"

	goodToken  = "iv_live_ro_good_0000000000000000"
	otherToken = "iv_live_ro_other_111111111111111"
	lapsedTok  = "iv_live_ro_lapsed_22222222222222"
	badToken   = "iv_live_ro_bad_33333333333333333"
)

// hostedAPI is a fake public API that knows which tokens exist. It records
// the Authorization header of every request, so a test can prove which token
// a tool call was sent with — and how many token checks reached the API.
type hostedAPI struct {
	server *httptest.Server

	mu sync.Mutex
	// status overrides the answer to /usage for every token while non-zero:
	// a test sets it to 500 or 429 to play an outage.
	status int
	seen   []string // "Bearer …" of every request, in order
}

func newHostedAPI(t *testing.T) *hostedAPI {
	t.Helper()
	h := &hostedAPI{}
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.seen = append(h.seen, r.Header.Get("Authorization"))
		status := h.status
		h.mu.Unlock()

		if status != 0 {
			writeBody(w, status, `{"error":"internal_error","message":"down"}`)
			return
		}
		switch strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") {
		case goodToken, otherToken:
			freeHeaders(w)
			writeBody(w, http.StatusOK, usageReply)
		case lapsedTok:
			writeBody(w, http.StatusPaymentRequired,
				`{"error":"subscription_inactive","message":"Your plan has ended."}`)
		default:
			writeBody(w, http.StatusUnauthorized, `{"error":"unauthorized","message":"Bad token."}`)
		}
	}))
	t.Cleanup(h.server.Close)
	return h
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
			v := newVerifier(api.server.URL+"/public/v1", TokenCacheTTL, nil, nil, clock.Now)

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
			// ⚠️ RequireBearerToken refuses a TokenInfo with no expiry.
			if want := clock.Now().Add(TokenCacheTTL); !info.Expiration.Equal(want) {
				t.Errorf("Expiration = %v, want %v", info.Expiration, want)
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

	v := newVerifier(url, TokenCacheTTL, nil, nil, time.Now)
	if _, err := v.verify(context.Background(), goodToken, nil); !errors.Is(err, errAPIUnavailable) {
		t.Fatalf("err = %v, want errAPIUnavailable", err)
	}
}

func TestVerifierCachesValidAndInvalidForTheTTL(t *testing.T) {
	api := newHostedAPI(t)
	clock := &fakeClock{now: time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)}
	v := newVerifier(api.server.URL+"/public/v1", TokenCacheTTL, nil, nil, clock.Now)
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

	// A token revoked in /user/api stops working once its entry expires.
	clock.Advance(TokenCacheTTL + time.Second)
	if _, err := v.verify(ctx, goodToken, nil); err != nil {
		t.Fatal(err)
	}
	if got := api.calls(goodToken); got != 2 {
		t.Errorf("valid token checked %d times after the TTL, want 2", got)
	}
}

// TestVerifierDoesNotCacheAnOutage: an API outage must not lock everyone out
// for a minute after the API is back, and must not force a new sign-in.
func TestVerifierDoesNotCacheAnOutage(t *testing.T) {
	api := newHostedAPI(t)
	v := newVerifier(api.server.URL+"/public/v1", TokenCacheTTL, nil, nil, time.Now)
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
