package commands

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"
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
	in := strings.NewReader(initializeFrame + "\n" + initializedFrame + "\n" + toolsListFrame)
	done := make(chan result, 1)
	go func() { done <- runWithStdin(t, nil, in, "mcp") }()

	select {
	case res := <-done:
		if _, ok := frames(t, res.stdout)["2"]; !ok {
			t.Errorf("no answer to a tools/list sent without a trailing newline\n%s", res.stdout)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("mcp did not exit after stdin ended")
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
		request, resp bool
	}{
		{`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, true, false},
		{`{"jsonrpc":"2.0","id":"a","method":"ping"}`, true, false},
		{`{"jsonrpc":"2.0","method":"notifications/initialized"}`, false, false},
		{`{"jsonrpc":"2.0","id":null,"method":"x"}`, false, false},
		{`{"jsonrpc":"2.0","id":1,"result":{}}`, false, true},
		{`{"jsonrpc":"2.0","id":1,"error":{"code":-32600}}`, false, true},
		{`[{"jsonrpc":"2.0","id":1,"method":"x"}]`, false, false},
		{`not json`, false, false},
	}
	for _, c := range cases {
		req, resp := frameKind([]byte(c.line))
		if req != c.request || resp != c.resp {
			t.Errorf("frameKind(%s) = (%v, %v), want (%v, %v)", c.line, req, resp, c.request, c.resp)
		}
	}
}

// A request that never gets an answer must not hold the process forever.
func TestInflightWaitGivesUpAfterTheTimeout(t *testing.T) {
	f := newInflight(20 * time.Millisecond)
	f.add(1)
	start := time.Now()
	f.wait()
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("wait took %v with a 20ms timeout", elapsed)
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
