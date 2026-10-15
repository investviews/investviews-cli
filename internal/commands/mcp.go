package commands

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
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
func NewMCPCmd(deps Deps) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "Serve the API to an AI agent as MCP tools over stdio",
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

Standard output carries only protocol messages. Logs go to standard error.`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
	}

	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		stderr := cmd.ErrOrStderr()
		logger := slog.New(slog.NewTextHandler(stderr, nil))
		srv := mcpserver.New(mcpserver.ClientFactory(deps.client), version.Version,
			mcpserver.WithLogger(logger))

		fmt.Fprintf(stderr, "investviews mcp %s: serving MCP on stdio; logs on stderr\n", version.Version)

		inflight := newInflight(drainTimeout)
		transport := &mcp.IOTransport{
			Reader: io.NopCloser(inflight.reader(cmd.InOrStdin())),
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

// nopWriteCloser lets the command's stdout serve as the transport's writer
// without the SDK closing it: stdout outlives the session, and a test's buffer
// must stay readable after the server is done.
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// drainTimeout is the longest the server waits, after stdin ends, for answers
// to requests it has already read. It covers a metered call's retry budget
// (10 s) plus the HTTP round trips, with room to spare.
const drainTimeout = 60 * time.Second

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

// wait blocks until no request is pending or the timeout runs out.
func (f *inflight) wait() {
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
		}
	}
}

func (f *inflight) reader(r io.Reader) io.Reader {
	return &countingReader{r: r, f: f}
}

func (f *inflight) writer(w io.Writer) io.Writer {
	return &countingWriter{w: w, f: f}
}

// frameKind says whether one newline-delimited JSON-RPC frame is a request
// (method + id), a response (id, no method) or neither. A batch array or a
// frame that does not parse is neither: the SDK answers it with its own
// error, and counting it wrong could only make the drain wait for the
// timeout, never lose an answer.
func frameKind(line []byte) (request, response bool) {
	var frame struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if json.Unmarshal(line, &frame) != nil {
		return false, false
	}
	hasID := len(frame.ID) > 0 && !bytes.Equal(frame.ID, []byte("null"))
	if !hasID {
		return false, false
	}
	return frame.Method != "", frame.Method == ""
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
	r     io.Reader
	f     *inflight
	split lines
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	count := func(line []byte) {
		if req, _ := frameKind(line); req {
			c.f.add(1)
		}
	}
	c.split.feed(p[:n], count)
	if errors.Is(err, io.EOF) {
		// A last frame with no newline after it is still a request the SDK
		// will answer, so it is counted before waiting.
		c.split.feed([]byte{'\n'}, count)
		c.f.wait()
	}
	return n, err
}

type countingWriter struct {
	w     io.Writer
	f     *inflight
	split lines
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.split.feed(p[:n], func(line []byte) {
		if _, resp := frameKind(line); resp {
			c.f.add(-1)
		}
	})
	return n, err
}
