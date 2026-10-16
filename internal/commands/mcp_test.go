package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/spf13/cobra"

	"github.com/investviews/investviews-cli/internal/api"
	"github.com/investviews/investviews-cli/internal/version"
)

// The `investviews mcp` tests drive the real command over injected stdin and
// stdout with newline-delimited JSON-RPC, the way an agent does. Each tool's
// own behaviour is tested in internal/mcpserver; these prove the plumbing:
// the streams, the logs, and that piped input gets its answers.

const (
	initializeFrame  = `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`
	initializedFrame = `{"jsonrpc":"2.0","method":"notifications/initialized"}`
	toolsListFrame   = `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`
)

// allTools is the full phase-1 tool set, sorted.
var allTools = []string{
	"browse_places", "compare_places", "coverage", "filters", "find_place",
	"market_stats", "place_at_point", "price_history", "usage",
}

// frame is the part of a JSON-RPC response these tests read.
type frame struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result"`
	Error   json.RawMessage `json:"error"`
}

// frames parses stdout as newline-delimited JSON-RPC. Any line that is not a
// JSON-RPC 2.0 frame fails the test: stdout is the protocol stream, and one
// stray line there breaks the client.
func frames(t *testing.T, stdout string) map[string]frame {
	t.Helper()
	byID := map[string]frame{}
	for _, line := range strings.Split(strings.TrimRight(stdout, "\n"), "\n") {
		if line == "" {
			t.Errorf("stdout holds an empty line; every line must be a JSON-RPC frame\n%s", stdout)
			continue
		}
		var f frame
		if err := json.Unmarshal([]byte(line), &f); err != nil || f.JSONRPC != "2.0" {
			t.Errorf("stdout line is not a JSON-RPC frame: %q", line)
			continue
		}
		byID[string(f.ID)] = f
	}
	return byID
}

func pipe(lines ...string) *strings.Reader {
	return strings.NewReader(strings.Join(lines, "\n") + "\n")
}

func TestMCPAnswersInitializeAndListsEveryToolOnStdout(t *testing.T) {
	res := runWithStdin(t, nil, pipe(initializeFrame, initializedFrame, toolsListFrame), "mcp")
	if res.err != nil {
		t.Fatalf("mcp returned %v; the end of stdin is a normal end", res.err)
	}

	got := frames(t, res.stdout)
	if len(got) != 2 {
		t.Fatalf("stdout holds %d frames, want 2 (initialize, tools/list)\n%s", len(got), res.stdout)
	}

	var init struct {
		ServerInfo   struct{ Name, Version string } `json:"serverInfo"`
		Instructions string                         `json:"instructions"`
	}
	if err := json.Unmarshal(got["1"].Result, &init); err != nil {
		t.Fatalf("initialize result: %v\n%s", err, got["1"].Result)
	}
	if init.ServerInfo.Name != "investviews" || init.ServerInfo.Version != version.Version {
		t.Errorf("serverInfo = %+v, want investviews %s", init.ServerInfo, version.Version)
	}
	if init.Instructions == "" {
		t.Error("initialize carries no instructions")
	}

	var list struct {
		Tools []struct{ Name string } `json:"tools"`
	}
	if err := json.Unmarshal(got["2"].Result, &list); err != nil {
		t.Fatalf("tools/list result: %v\n%s", err, got["2"].Result)
	}
	var names []string
	for _, tool := range list.Tools {
		names = append(names, tool.Name)
	}
	sort.Strings(names)
	if strings.Join(names, ",") != strings.Join(allTools, ",") {
		t.Errorf("tools/list = %v, want %v", names, allTools)
	}

	// Starting and listing are protocol-only: no API request.
	if res.hits() != 0 {
		t.Errorf("mcp sent %d API requests for initialize + tools/list, want 0", res.hits())
	}
}

// ⚠️ The SDK closes the session the moment stdin ends; without the in-flight
// drain, a tool call that is still waiting on HTTP when the pipe closes loses
// its answer. This one has to make a real request to the fake API first.
func TestMCPPipedToolCallGetsItsAnswerBeforeExit(t *testing.T) {
	slow := func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(50 * time.Millisecond)
		freeJSON(usageBody)(w, r)
	}
	call := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"usage","arguments":{}}}`
	res := runWithStdin(t, slow, pipe(initializeFrame, initializedFrame, call), "mcp")
	if res.err != nil {
		t.Fatalf("mcp returned %v", res.err)
	}

	got := frames(t, res.stdout)
	answer, ok := got["3"]
	if !ok {
		t.Fatalf("no answer to the tools/call on stdout\n%s", res.stdout)
	}
	contains(t, string(answer.Result), "read-only probe")
	if res.hits() != 1 {
		t.Errorf("usage sent %d API requests, want 1", res.hits())
	}
}

func TestMCPAnswersALastFrameWithNoNewline(t *testing.T) {
	// Run on the test goroutine: runWithStdin may call t.Fatalf, which is
	// not allowed elsewhere. A hang here is caught by go test's -timeout.
	in := strings.NewReader(initializeFrame + "\n" + initializedFrame + "\n" + toolsListFrame)
	res := runWithStdin(t, nil, in, "mcp")
	if _, ok := frames(t, res.stdout)["2"]; !ok {
		t.Errorf("no answer to a tools/list sent without a trailing newline\n%s", res.stdout)
	}
}

func TestMCPLogsGoToStderrNotStdout(t *testing.T) {
	res := runWithStdin(t, nil, pipe(initializeFrame, initializedFrame, toolsListFrame), "mcp")

	// The command's own start line and the SDK's session logs.
	contains(t, res.stderr, "investviews mcp "+version.Version+": serving MCP on stdio")
	contains(t, res.stderr, "server session connected")
	contains(t, res.stderr, "session initialized")

	missing(t, res.stdout, "serving MCP")
	missing(t, res.stdout, "level=INFO")
	frames(t, res.stdout) // every stdout line is a frame
}

// ⚠️ The server starts with no token: the client is built per tool call, so a
// missing token is a tool error on the call that needed it, not a server that
// refused to start.
func TestMCPStartsWithNoTokenAndReportsItPerCall(t *testing.T) {
	root := &cobra.Command{Use: "investviews", SilenceUsage: true, SilenceErrors: true}
	for _, sub := range All(Deps{NewClient: func() (*api.Client, error) { return nil, ErrNoToken }}) {
		root.AddCommand(sub)
	}
	call := `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"coverage","arguments":{}}}`
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetIn(pipe(initializeFrame, initializedFrame, toolsListFrame, call))
	root.SetArgs([]string{"mcp"})

	if err := root.Execute(); err != nil {
		t.Fatalf("mcp with no token returned %v, want a running server", err)
	}
	got := frames(t, stdout.String())
	if len(got["2"].Result) == 0 {
		t.Errorf("tools/list did not answer without a token\n%s", stdout.String())
	}
	var callResult struct {
		IsError bool `json:"isError"`
		Content []struct{ Text string }
	}
	if err := json.Unmarshal(got["3"].Result, &callResult); err != nil {
		t.Fatalf("tools/call result: %v\n%s", err, stdout.String())
	}
	if !callResult.IsError || len(callResult.Content) == 0 {
		t.Fatalf("coverage with no token = %+v, want isError with text", callResult)
	}
	contains(t, callResult.Content[0].Text, "investviews auth login")
}

func TestMCPHelpShowsTheAgentConfigLine(t *testing.T) {
	res := run(t, nil, "mcp", "--help")
	if res.err != nil {
		t.Fatalf("mcp --help: %v", res.err)
	}
	contains(t, res.stdout, `{"mcpServers": {"investviews": {"command": "investviews", "args": ["mcp"]}}}`)
	contains(t, res.stdout, "investviews auth login")
	contains(t, res.stdout, "FREE")
	if res.hits() != 0 {
		t.Errorf("mcp --help sent %d API requests, want 0", res.hits())
	}
}

func TestFrameKindTellsRequestsFromResponses(t *testing.T) {
	cases := []struct {
		line          string
		request, resp int
	}{
		{`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, 1, 0},
		{`{"jsonrpc":"2.0","id":"a","method":"ping"}`, 1, 0},
		{`{"jsonrpc":"2.0","method":"notifications/initialized"}`, 0, 0},
		{`{"jsonrpc":"2.0","id":null,"method":"x"}`, 0, 0},
		{`{"jsonrpc":"2.0","id":1,"result":{}}`, 0, 1},
		{`{"jsonrpc":"2.0","id":1,"error":{"code":-32600}}`, 0, 1},
		// A batch counts each member, so a pending batch call holds the drain.
		{`[{"jsonrpc":"2.0","id":1,"method":"x"},{"jsonrpc":"2.0","method":"n"},{"jsonrpc":"2.0","id":2,"method":"y"}]`, 2, 0},
		{`[{"jsonrpc":"2.0","id":1,"result":{}},{"jsonrpc":"2.0","id":2,"error":{"code":1}}]`, 0, 2},
		{`[]`, 0, 0},
		{`[not json`, 0, 0},
		{`not json`, 0, 0},
	}
	for _, c := range cases {
		req, resp := frameKind([]byte(c.line))
		if req != c.request || resp != c.resp {
			t.Errorf("frameKind(%s) = (%d, %d), want (%d, %d)", c.line, req, resp, c.request, c.resp)
		}
	}
}

// A request that never gets an answer must not hold the process forever.
func TestInflightWaitGivesUpAfterTheTimeout(t *testing.T) {
	const timeout = 50 * time.Millisecond
	f := newInflight(timeout)
	f.add(1)
	start := time.Now()
	f.wait(context.Background())
	elapsed := time.Since(start)
	if elapsed < timeout {
		t.Errorf("wait returned after %v, before the %v timeout, with a request still pending", elapsed, timeout)
	}
	if elapsed > 2*time.Second {
		t.Errorf("wait took %v with a %v timeout", elapsed, timeout)
	}
}

// wait returns as soon as the last pending request is answered.
func TestInflightWaitReturnsWhenThePendingRequestIsAnswered(t *testing.T) {
	f := newInflight(10 * time.Second)
	f.add(1)
	go func() {
		time.Sleep(20 * time.Millisecond)
		f.add(-1)
	}()
	start := time.Now()
	f.wait(context.Background())
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("wait took %v; it should return when pending drops to 0", elapsed)
	}
	f.mu.Lock()
	pending := f.pending
	f.mu.Unlock()
	if pending != 0 {
		t.Errorf("wait returned with pending = %d", pending)
	}
}

// Ctrl-C must not sit out the drain timeout.
func TestInflightWaitStopsWhenTheContextIsDone(t *testing.T) {
	f := newInflight(10 * time.Second)
	f.add(1)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	f.wait(ctx)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("wait took %v after the context was cancelled", elapsed)
	}
}

func TestInflightCountsFramesSplitAcrossWrites(t *testing.T) {
	f := newInflight(time.Second)
	var out bytes.Buffer
	w := f.writer(&out)
	f.add(1)
	_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,`))
	if f.pending != 1 {
		t.Fatalf("pending = %d after half a frame, want 1", f.pending)
	}
	_, _ = w.Write([]byte(`"result":{}}` + "\n"))
	if f.pending != 0 {
		t.Errorf("pending = %d after the whole response, want 0", f.pending)
	}
}

// A reader may return its last bytes together with io.EOF. Those bytes must
// reach the SDK at once; the drain waits only on the call after them.
func TestCountingReaderDeliversBytesReturnedWithEOF(t *testing.T) {
	f := newInflight(10 * time.Second)
	r := f.reader(context.Background(), iotest.DataErrReader(strings.NewReader(toolsListFrame+"\n")))

	buf := make([]byte, 4096)
	start := time.Now()
	n, err := r.Read(buf)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("the read holding the last request blocked for %v", elapsed)
	}
	if err != nil || string(buf[:n]) != toolsListFrame+"\n" {
		t.Fatalf("first Read = %q, %v; want the frame and no error", buf[:n], err)
	}
	if f.pending != 1 {
		t.Fatalf("pending = %d after the request, want 1", f.pending)
	}

	f.add(-1) // the SDK answers it
	n, err = r.Read(buf)
	if n != 0 || err != io.EOF {
		t.Errorf("second Read = %d, %v; want 0, io.EOF", n, err)
	}
}

// ── hosted mode (--http) ─────────────────────────────────────────────────────
// The handler itself is tested in internal/mcpserver; these prove the command
// wiring: the flags, the start line, and a clean stop.

// lockedBuffer is a buffer the running command writes to while the test
// reads it.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// hostedRoot is a root command whose Deps point the hosted mode at apiURL.
// Its stdio factory fails the test: the hosted mode must never use the
// operator's own token.
func hostedRoot(t *testing.T, apiURL string, stdout, stderr io.Writer, args ...string) *cobra.Command {
	t.Helper()
	root := &cobra.Command{Use: "investviews", SilenceUsage: true, SilenceErrors: true}
	deps := Deps{
		NewClient: func() (*api.Client, error) {
			t.Error("the hosted mode used the stdio client factory")
			return nil, ErrNoToken
		},
		APIURL: func() string { return apiURL },
	}
	for _, sub := range All(deps) {
		root.AddCommand(sub)
	}
	root.SetOut(stdout)
	root.SetErr(stderr)
	root.SetArgs(args)
	return root
}

var listenLine = regexp.MustCompile(`serving MCP over HTTP on (http://127\.0\.0\.1:\d+)`)

func TestMCPHTTPServesUntilStopped(t *testing.T) {
	var stdout, stderr lockedBuffer
	root := hostedRoot(t, "http://127.0.0.1:1/public/v1", &stdout, &stderr, "mcp", "--http", "127.0.0.1:0")

	base, stop := startHosted(t, root, &stderr)

	resp, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /healthz = %d, want 200", resp.StatusCode)
	}

	// The defaults are the production URLs.
	resp, err = http.Get(base + "/.well-known/oauth-protected-resource")
	if err != nil {
		t.Fatalf("GET metadata: %v", err)
	}
	var doc struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&doc)
	_ = resp.Body.Close()
	if doc.Resource != "https://mcp.investviews.ai/mcp" {
		t.Errorf("resource = %q, want https://mcp.investviews.ai/mcp", doc.Resource)
	}
	if len(doc.AuthorizationServers) != 1 || doc.AuthorizationServers[0] != "https://api.investviews.ai" {
		t.Errorf("authorization_servers = %v, want [https://api.investviews.ai]", doc.AuthorizationServers)
	}

	resp, err = http.Post(base+"/mcp", "application/json", strings.NewReader(initializeFrame))
	if err != nil {
		t.Fatalf("POST /mcp: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("POST /mcp with no token = %d, want 401", resp.StatusCode)
	}

	if err := stop(); err != nil {
		t.Errorf("a stopped server returned %v, want nil", err)
	}
	if stdout.String() != "" {
		t.Errorf("the hosted mode wrote to stdout: %q", stdout.String())
	}
}

func TestMCPHTTPFlagsAreRefusedWithoutHTTP(t *testing.T) {
	for flag, value := range map[string]string{
		"--public-url":      "https://example.investviews.ai",
		"--auth-server":     "https://example.investviews.ai",
		"--token-cache-ttl": "1s",
	} {
		var stdout, stderr bytes.Buffer
		root := hostedRoot(t, "http://127.0.0.1:1/public/v1", &stdout, &stderr,
			"mcp", flag, value)
		err := root.Execute()
		if err == nil || !strings.Contains(err.Error(), "--http") {
			t.Errorf("mcp %s without --http: err = %v, want one that names --http", flag, err)
		}
	}
}

func TestMCPHTTPRefusesABadPublicURL(t *testing.T) {
	var stdout, stderr bytes.Buffer
	root := hostedRoot(t, "http://127.0.0.1:1/public/v1", &stdout, &stderr,
		"mcp", "--http", "127.0.0.1:0", "--public-url", "https://mcp.investviews.ai/mcp")
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "public URL") {
		t.Errorf("err = %v, want a public URL error", err)
	}
}

var errStopTimeout = errors.New("mcp --http did not stop within 5s of its context ending")

// startHosted runs root and returns the base URL from the start line on
// stderr, and a stop func that ends the server and returns what it returned.
// The server is stopped when the test ends if the test did not stop it.
func startHosted(t *testing.T, root *cobra.Command, stderr *lockedBuffer) (string, func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- root.ExecuteContext(ctx) }()

	var once sync.Once
	var stopErr error
	stop := func() error {
		once.Do(func() {
			cancel()
			select {
			case stopErr = <-done:
			case <-time.After(5 * time.Second):
				stopErr = errStopTimeout
			}
		})
		return stopErr
	}
	t.Cleanup(func() {
		if err := stop(); errors.Is(err, errStopTimeout) {
			t.Error(err)
		}
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		if m := listenLine.FindStringSubmatch(stderr.String()); m != nil {
			return m[1], stop
		}
		select {
		case err := <-done:
			done <- err // for stop
			t.Fatalf("mcp --http exited early: %v\nstderr: %s", err, stderr.String())
		case <-time.After(10 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatalf("no start line on stderr after 5s:\n%s", stderr.String())
		}
	}
}

// The hidden --token-cache-ttl flag exists for end-to-end tests that revoke a
// token and wait for the server to notice. It must reach the token cache.
//
// The TTL is a whole second, not a few milliseconds: the two quick requests
// must land inside it even on a slow machine under -race.
func TestMCPHTTPTokenCacheTTLFlagShortensTheCache(t *testing.T) {
	var checks atomic.Int32
	fakeAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/public/v1/usage" {
			checks.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{}`)
	}))
	defer fakeAPI.Close()

	var stdout, stderr lockedBuffer
	root := hostedRoot(t, fakeAPI.URL+"/public/v1", &stdout, &stderr,
		"mcp", "--http", "127.0.0.1:0", "--token-cache-ttl", "1s")
	base, _ := startHosted(t, root, &stderr)

	post := func() {
		req, _ := http.NewRequest(http.MethodPost, base+"/mcp", strings.NewReader(initializeFrame))
		req.Header.Set("Authorization", "Bearer iv_live_ro_"+strings.Repeat("t", 32))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST /mcp: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST /mcp = %d, want 200", resp.StatusCode)
		}
	}

	post()
	post()
	if got := checks.Load(); got != 1 {
		t.Fatalf("token checks after two quick requests = %d, want 1 (cached)", got)
	}
	time.Sleep(1200 * time.Millisecond)
	post()
	if got := checks.Load(); got != 2 {
		t.Errorf("token checks after the TTL passed = %d, want 2", got)
	}
}

func TestMCPHTTPTokenCacheTTLFlagIsHidden(t *testing.T) {
	var stdout, stderr bytes.Buffer
	root := hostedRoot(t, "http://127.0.0.1:1/public/v1", &stdout, &stderr, "mcp", "--help")
	if err := root.Execute(); err != nil {
		t.Fatalf("mcp --help: %v", err)
	}
	if strings.Contains(stdout.String(), "token-cache-ttl") {
		t.Error("--token-cache-ttl shows in --help; it is a test-only flag")
	}
}

func TestMCPHTTPRefusesANegativeTokenCacheTTL(t *testing.T) {
	var stdout, stderr bytes.Buffer
	root := hostedRoot(t, "http://127.0.0.1:1/public/v1", &stdout, &stderr,
		"mcp", "--http", "127.0.0.1:0", "--token-cache-ttl", "-1s")
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "token-cache-ttl") {
		t.Errorf("err = %v, want one that names --token-cache-ttl", err)
	}
}
