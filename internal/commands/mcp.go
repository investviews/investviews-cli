package commands

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/investviews/investviews-cli/internal/mcpserver"
	"github.com/investviews/investviews-cli/internal/version"
)

// mcpConfigLine is the one line an agent's config needs. It is quoted in the
// help text and must match the Claude Code plugin's mcpServers entry, so a
// user who copies it from either place gets the same server.
const mcpConfigLine = `{"mcpServers": {"investviews": {"command": "investviews", "args": ["mcp"]}}}`

// NewMCPCmd builds "investviews mcp": the same API, served to an AI agent as
// MCP tools over stdio.
//
// ⚠️ STDOUT BELONGS TO THE PROTOCOL. Every byte this command writes to stdout
// must be a JSON-RPC frame — a banner, a log line or a stray newline there is
// a frame the client cannot parse, and most clients drop the connection on it.
// The start line and the SDK's logs go to STDERR, which MCP clients keep as
// the server's log.
//
// ⚠️ NO TOKEN IS NEEDED TO START. The server builds its API client per tool
// call (see internal/mcpserver), so it starts, lists its tools and answers
// `initialize` with no credential; a call that needs one returns ErrNoToken
// as a tool error the agent can relay. A user who logs in while the agent
// runs does not have to restart it.
//
// ⚠️ --http IS A DIFFERENT TRUST MODEL. On stdio the server acts for the one
// user who runs it, with their saved token. With --http it is one shared
// server for many users, and every request brings its own token — see
// serveHTTP and internal/mcpserver/http.go.
func NewMCPCmd(deps Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Serve the API to an AI agent as MCP tools over stdio (or HTTP with --http)",
		Long: `Serve the InvestViews API to an AI agent (Claude Desktop, Claude Code,
Cursor, …) as MCP (Model Context Protocol) tools, over standard input and
output.

You do not run this command yourself: the agent starts it. Add this to the
agent's MCP config:

  ` + mcpConfigLine + `

The server uses the same token as the CLI — run "investviews auth login" once,
or set INVESTVIEWS_TOKEN in the agent's config. It starts without a token; a
tool call made without one comes back as an error that says how to add it.

Tools: find_place, browse_places, place_at_point, coverage, filters and usage
are FREE. market_stats and price_history spend one metered call each;
compare_places spends one per place.

Standard output carries only protocol messages. Logs go to standard error.

HOSTED MODE. With --http the same tools are served over Streamable HTTP
instead, for agents that connect to a URL and sign in with OAuth. This is how
https://mcp.investviews.ai/mcp runs; you do not need it on your computer:

  investviews mcp --http 127.0.0.1:8090 --public-url https://mcp.investviews.ai --auth-server https://api.investviews.ai

Every request must carry its own bearer token, and every API call is sent with
that token. The saved token and INVESTVIEWS_TOKEN are never used in this mode,
and the config file is not read: INVESTVIEWS_API_URL alone picks the API.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
	}

	var hosted hostedFlags
	cmd.Flags().StringVar(&hosted.addr, "http", "",
		"serve over Streamable HTTP on this address (host:port) instead of stdio; every request needs a bearer token")
	cmd.Flags().StringVar(&hosted.publicURL, "public-url", mcpserver.DefaultPublicURL,
		"with --http: the public origin agents use, with no path; the protected resource is <public-url>/mcp")
	cmd.Flags().StringVar(&hosted.authServer, "auth-server", mcpserver.DefaultAuthServer,
		"with --http: the OAuth authorization server named in the protected resource metadata")
	// Test-only: an end-to-end test revokes a token and waits for the server
	// to notice, which takes one cache lifetime. Hidden from --help.
	cmd.Flags().DurationVar(&hosted.tokenCacheTTL, "token-cache-ttl", 0,
		"with --http: how long one token check is trusted (0 keeps the default)")
	_ = cmd.Flags().MarkHidden("token-cache-ttl")

	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		if hosted.addr == "" {
			for _, name := range []string{"public-url", "auth-server", "token-cache-ttl"} {
				if cmd.Flags().Changed(name) {
					return fmt.Errorf("--%s only applies with --http", name)
				}
			}
		} else {
			return serveHTTP(cmd, deps, hosted)
		}

		stderr := cmd.ErrOrStderr()
		logger := slog.New(slog.NewTextHandler(stderr, nil))
		srv := mcpserver.New(deps.client, version.Version, logger)

		fmt.Fprintf(stderr, "investviews mcp %s: serving MCP on stdio; logs on stderr\n", version.Version)

		inflight := newInflight(drainTimeout)
		transport := &mcp.IOTransport{
			Reader: io.NopCloser(inflight.reader(cmd.Context(), cmd.InOrStdin())),
			Writer: nopWriteCloser{inflight.writer(cmd.OutOrStdout())},
		}
		err := srv.Run(cmd.Context(), transport)
		// The client closing stdin is how a stdio session normally ends, not
		// a failure: the agent quit or restarted the server.
		if err == nil || errors.Is(err, io.EOF) || errors.Is(err, mcp.ErrConnectionClosed) {
			return nil
		}
		return err
	}
	return cmd
}

// hostedFlags are the --http mode's flags.
type hostedFlags struct {
	addr          string
	publicURL     string
	authServer    string
	tokenCacheTTL time.Duration
}

// shutdownTimeout is how long a stopping hosted server waits for requests in
// flight. A request accepted just before the stop first has its token checked
// (up to VerifyTimeout) and only then starts its tool call (up to
// ToolTimeout), so the wait covers both plus a few seconds to write the
// answer. The systemd unit's TimeoutStopSec must stay above it.
const shutdownTimeout = mcpserver.VerifyTimeout + mcpserver.ToolTimeout + 5*time.Second

// serveHTTP runs the hosted mode until the command's context ends or the
// process gets SIGINT or SIGTERM (a service manager's stop).
//
// ⚠️ THE OPERATOR'S TOKEN IS NEVER READ HERE. Only deps.APIURL is used, never
// deps.NewClient: a hosted server that fell back to a saved token would answer
// every agent with the operator's quota. Each request's own bearer token is
// the only credential (internal/mcpserver/http.go).
//
// Stdout stays empty in this mode too; the start line and logs go to stderr,
// which the service manager keeps as the journal.
func serveHTTP(cmd *cobra.Command, deps Deps, flags hostedFlags) error {
	stderr := cmd.ErrOrStderr()
	if flags.tokenCacheTTL < 0 {
		return fmt.Errorf("--token-cache-ttl must not be negative, got %s", flags.tokenCacheTTL)
	}
	apiURL := deps.APIURL()
	logger := slog.New(slog.NewTextHandler(stderr, nil))
	handler, err := mcpserver.NewHTTPHandler(mcpserver.HTTPOptions{
		PublicURL:  flags.publicURL,
		AuthServer: flags.authServer,
		APIURL:     apiURL,
		Version:    version.Version,
		Logger:     logger,
		CacheTTL:   flags.tokenCacheTTL,
	})
	if err != nil {
		return err
	}

	listener, err := net.Listen("tcp", flags.addr)
	if err != nil {
		return fmt.Errorf("cannot listen on %s: %w", flags.addr, err)
	}
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		// ReadTimeout covers reading the request only; net/http clears the
		// read deadline once the body is read, so a streamed answer is not
		// cut by it.
		ReadTimeout: 30 * time.Second,
		// No WriteTimeout: a streamed answer may legitimately take up to
		// ToolTimeout, and each tool call has its own deadline already.
		IdleTimeout: 2 * time.Minute,
		ErrorLog:    slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
	}

	ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	fmt.Fprintf(stderr, "investviews mcp %s: serving MCP over HTTP on http://%s%s (public %s%s, API %s); logs on stderr\n",
		version.Version, listener.Addr(), mcpserver.MCPPath,
		strings.TrimRight(flags.publicURL, "/"), mcpserver.MCPPath, apiURL)

	served := make(chan error, 1)
	go func() { served <- srv.Serve(listener) }()

	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		return fmt.Errorf("stopping the HTTP server: %w", err)
	}
	if err := <-served; err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// nopWriteCloser lets the command's stdout serve as the transport's writer
// without the SDK closing it: stdout outlives the session, and a test's buffer
// must stay readable after the server is done.
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// drainTimeout is the longest the server waits, after stdin ends, for answers
// to requests it has already read. Every tool call is cut off at
// mcpserver.ToolTimeout — retries, HTTP attempts and every place of
// compare_places included — so a few seconds over that is enough for the
// answer to be written.
const drainTimeout = mcpserver.ToolTimeout + 5*time.Second

// inflight counts requests read from stdin that have no answer on stdout yet,
// so the end of stdin can wait for them.
//
// ⚠️ WITHOUT IT, PIPED INPUT GETS NO ANSWERS. The SDK closes the session the
// moment its reader returns EOF, and drops every answer still being built —
// including the one to `initialize`. A real agent keeps stdin open, so it
// never sees this; a script, a smoke test or `investviews mcp < requests.jsonl`
// sees an empty stdout and exit 0. So the reader holds the EOF back until
// every request it passed on has a response (or drainTimeout runs out).
//
// Only requests are counted — a frame with both "method" and a non-null "id".
// Notifications get no answer, and a frame the server writes with a "method"
// is its own request to the client, not an answer.
type inflight struct {
	mu      sync.Mutex
	pending int
	changed chan struct{} // closed and replaced on every change of pending
	timeout time.Duration
}

func newInflight(timeout time.Duration) *inflight {
	return &inflight{changed: make(chan struct{}), timeout: timeout}
}

func (f *inflight) add(delta int) {
	f.mu.Lock()
	f.pending += delta
	close(f.changed)
	f.changed = make(chan struct{})
	f.mu.Unlock()
}

// wait blocks until no request is pending, the timeout runs out, or ctx is
// done — Ctrl-C must not sit out the timeout for a request that will never
// be answered.
func (f *inflight) wait(ctx context.Context) {
	deadline := time.After(f.timeout)
	for {
		f.mu.Lock()
		pending, changed := f.pending, f.changed
		f.mu.Unlock()
		if pending <= 0 {
			return
		}
		select {
		case <-changed:
		case <-deadline:
			return
		case <-ctx.Done():
			return
		}
	}
}

func (f *inflight) reader(ctx context.Context, r io.Reader) io.Reader {
	return &countingReader{ctx: ctx, r: r, f: f}
}

func (f *inflight) writer(w io.Writer) io.Writer {
	return &countingWriter{w: w, f: f}
}

// frameKind counts the requests (method + id) and responses (id, no method)
// in one newline-delimited JSON-RPC frame. A batch array counts each of its
// members: the SDK accepts batches under older protocol versions and answers
// them with a batch, so both sides stay balanced. A frame that does not parse
// counts nothing: the SDK answers it with its own error, and counting it
// wrong could only make the drain wait for the timeout, never lose an answer.
func frameKind(line []byte) (requests, responses int) {
	if trimmed := bytes.TrimSpace(line); len(trimmed) > 0 && trimmed[0] == '[' {
		var batch []json.RawMessage
		if json.Unmarshal(trimmed, &batch) != nil {
			return 0, 0
		}
		for _, member := range batch {
			req, resp := frameKind(member)
			requests += req
			responses += resp
		}
		return requests, responses
	}
	var frame struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if json.Unmarshal(line, &frame) != nil {
		return 0, 0
	}
	hasID := len(frame.ID) > 0 && !bytes.Equal(frame.ID, []byte("null"))
	switch {
	case !hasID:
		return 0, 0
	case frame.Method != "":
		return 1, 0
	default:
		return 0, 1
	}
}

// lines splits a byte stream into complete lines, keeping a partial last line
// for the next chunk: reads and writes do not have to end on a frame boundary.
type lines struct{ partial []byte }

func (l *lines) feed(chunk []byte, each func([]byte)) {
	l.partial = append(l.partial, chunk...)
	for {
		i := bytes.IndexByte(l.partial, '\n')
		if i < 0 {
			return
		}
		if line := bytes.TrimSpace(l.partial[:i]); len(line) > 0 {
			each(line)
		}
		l.partial = l.partial[i+1:]
	}
}

type countingReader struct {
	ctx   context.Context
	r     io.Reader
	f     *inflight
	split lines
	// eof is an EOF that came back together with bytes. It is held for the
	// next Read, so those bytes reach the SDK before the drain waits for
	// their answers.
	eof error
}

func (c *countingReader) Read(p []byte) (int, error) {
	if c.eof != nil {
		return 0, c.drain(c.eof)
	}
	n, err := c.r.Read(p)
	c.split.feed(p[:n], c.count)
	if errors.Is(err, io.EOF) {
		// ⚠️ io.Reader may return the last bytes WITH io.EOF. Waiting now
		// would hold back the very request the wait is for, so the bytes go
		// out first and the EOF is returned on the next call.
		if n > 0 {
			c.eof = err
			return n, nil
		}
		return 0, c.drain(err)
	}
	return n, err
}

// drain waits for every counted request to be answered, then hands back the
// EOF. A last frame with no newline after it is still a request the SDK will
// answer, so it is counted before waiting.
func (c *countingReader) drain(eof error) error {
	c.split.feed([]byte{'\n'}, c.count)
	c.f.wait(c.ctx)
	return eof
}

func (c *countingReader) count(line []byte) {
	if req, _ := frameKind(line); req > 0 {
		c.f.add(req)
	}
}

type countingWriter struct {
	w     io.Writer
	f     *inflight
	split lines
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.split.feed(p[:n], func(line []byte) {
		if _, resp := frameKind(line); resp > 0 {
			c.f.add(-resp)
		}
	})
	return n, err
}
