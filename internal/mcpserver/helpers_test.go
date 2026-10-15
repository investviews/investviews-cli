package mcpserver

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/investviews/investviews-cli/internal/api"
)

// ⚠️ NOTHING IN THIS PACKAGE'S TESTS TOUCHES THE LIVE API. Every tool runs end
// to end inside the process: an MCP client and this server joined by the SDK's
// in-memory transport, with the server's API client pointed at an httptest
// server. The suite is deterministic, offline, and spends no quota.

// testVersion is the build stamp every test server reports.
const testVersion = "1.2.3-test"

type recordedRequest struct {
	Path  string
	Query string
}

// fakeAPI is an httptest server that records every request it receives. The
// recording is how a test proves a locally refused tool call cost nothing:
// hits() stays 0.
//
// The mutex is not decoration: the MCP server runs tool handlers on its own
// goroutines, so the recorder is written from one goroutine and read from the
// test's.
type fakeAPI struct {
	server *httptest.Server

	mu       sync.Mutex
	requests []recordedRequest
}

func newFakeAPI(t *testing.T, handler http.HandlerFunc) *fakeAPI {
	t.Helper()

	f := &fakeAPI{}
	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, recordedRequest{Path: r.URL.Path, Query: r.URL.RawQuery})
		f.mu.Unlock()
		if handler != nil {
			handler(w, r)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

// recorded is a copy of the requests so far, safe to range over.
func (f *fakeAPI) recorded() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest(nil), f.requests...)
}

func (f *fakeAPI) hits() int { return len(f.recorded()) }

func (f *fakeAPI) last() recordedRequest {
	reqs := f.recorded()
	if len(reqs) == 0 {
		return recordedRequest{}
	}
	return reqs[len(reqs)-1]
}

// factory is a ClientFactory pointed at this fake. Sleep never waits, so a
// retry or a 429 Retry-After costs the test no time.
func (f *fakeAPI) factory(t *testing.T) ClientFactory {
	t.Helper()
	return func() (*api.Client, error) {
		return api.New(api.Options{
			BaseURL: f.server.URL + "/public/v1",
			Token:   "iv_live_ro_test",
			Jitter:  func(d time.Duration) time.Duration { return d },
			Sleep: func(ctx context.Context, _ time.Duration) error {
				return ctx.Err()
			},
		})
	}
}

// connect starts the server built from factory and returns a client session
// that has already completed `initialize`. Both ends are closed when the test
// ends.
func connect(t *testing.T, factory ClientFactory) *mcp.ClientSession {
	t.Helper()
	return connectServer(t, New(factory, testVersion))
}

// connectServer is connect for a server the test built itself — a probe tool
// that exercises one helper without waiting for the tool that will use it.
func connectServer(t *testing.T, srv *mcp.Server) *mcp.ClientSession {
	t.Helper()

	ctx := context.Background()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()

	serverSession, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server Connect: %v", err)
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}

	t.Cleanup(func() {
		_ = session.Close()
		_ = serverSession.Wait()
	})
	return session
}

// connectFake is the usual pairing: a fake API running handler, and a session
// whose server talks to it.
func connectFake(t *testing.T, handler http.HandlerFunc) (*mcp.ClientSession, *fakeAPI) {
	t.Helper()
	f := newFakeAPI(t, handler)
	return connect(t, f.factory(t)), f
}

// freeHeaders is what every metadata endpoint sends: the literal "uncounted",
// and no X-Quota-Used.
func freeHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Quota-Group", "metadata")
	w.Header().Set("X-Quota-Limit", api.Uncounted)
	w.Header().Set("X-RateLimit-Limit", "120")
	w.Header().Set("X-RateLimit-Remaining", "119")
	w.Header().Set("X-Request-Id", "req-free")
}

// meteredHeaders is what /stats/current sends.
func meteredHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Quota-Group", "current")
	w.Header().Set("X-Quota-Limit", "100000")
	w.Header().Set("X-Quota-Used", "138")
	w.Header().Set("X-Quota-Reset", "2026-10-11T12:30:25Z")
	w.Header().Set("X-RateLimit-Limit", "300")
	w.Header().Set("X-RateLimit-Remaining", "299")
	w.Header().Set("X-Request-Id", "req-metered")
}

func writeBody(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// freeJSON answers a metadata endpoint.
func freeJSON(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		freeHeaders(w)
		writeBody(w, http.StatusOK, body)
	}
}

// meteredJSON answers a stats endpoint.
func meteredJSON(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		meteredHeaders(w)
		writeBody(w, http.StatusOK, body)
	}
}
