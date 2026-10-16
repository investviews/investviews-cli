// Package mcpserver is the InvestViews MCP (Model Context Protocol) server: the
// same public API the CLI wraps, offered to an AI agent as typed tools instead
// of shell commands.
//
// It is a thin layer. Every tool maps its arguments to the params of one
// internal/api call, and maps the result (or the error) back to a tool result.
// Validation, retries, paging and the error envelope all stay in internal/api,
// so the CLI and the agent can never disagree about what a request means.
//
// Two rules hold for the whole package:
//
//  1. ⚠️ IT IMPORTS internal/api AND THE SDK — NEVER internal/commands.
//     internal/commands/mcp.go imports this package to run the server, so the
//     other direction is an import cycle. Anything both need lives in
//     internal/api.
//
//  2. ⚠️ THE API CLIENT IS BUILT PER TOOL CALL, NOT ONCE AT STARTUP. The server
//     is a long-running process inside an agent. A user who runs `investviews
//     auth login` while it runs must not have to restart the agent, and a
//     missing token must come back from the factory as a tool error on the
//     call that needed it — not as a server that refused to start. The hosted
//     mode (http.go) leans on the same rule: one shared server, and each call
//     builds its client from the bearer token of the request that made it.
package mcpserver

import (
	"context"
	_ "embed"
	"log/slog"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/investviews/investviews-cli/internal/api"
)

// Name is the server name an agent sees in `initialize`. It matches the binary
// and the Claude Code plugin's mcpServers key, so the three never drift.
const Name = "investviews"

// instructions is the text an agent receives with `initialize`: a short form of
// the rules in skills/investviews/SKILL.md (free before paid, name the
// statistic, always show price per m², never the API's /reports).
//
// ⚠️ IT IS A SECOND COPY OF THE SKILL'S RULES, ON PURPOSE. An agent connected
// over MCP may never load SKILL.md — Claude Desktop and Cursor have no skill —
// so this is the only copy it reads. When a rule changes in SKILL.md, change it
// here too. It stays short because the client puts it into every conversation.
//
//go:embed instructions.md
var instructions string

// ClientFactory builds an API client for one tool call. In the binary it is
// the commands package's Deps.client, a nil-guarded wrapper around
// Deps.NewClient that reads the saved token and returns ErrNoToken when there
// is none; tests point it at an httptest server.
type ClientFactory func() (*api.Client, error)

// server is the state every tool handler shares. It holds a factory, not a
// client — see rule 2 in the package comment.
//
// clientFor builds the client for one tool call. On stdio (New) it ignores
// the request and reads the token from the user's config; in the hosted mode
// (bearerClient in http.go) it takes the token from the request itself.
type server struct {
	clientFor func(req *mcp.CallToolRequest) (*api.Client, error)
}

// New builds the MCP server. version is the build stamp
// (internal/version.Version) and is what `initialize` reports, so a support
// thread can ask the agent which build it is talking to.
//
// logger receives the SDK's own log lines (session connected, session ended,
// a notification that failed); nil discards them.
//
// ⚠️ logger MUST NOT WRITE TO STDOUT when the server runs on stdio. Stdout IS
// the protocol stream: one stray log line there is a frame the client cannot
// parse, and most clients drop the whole connection on it. `investviews mcp`
// points it at stderr, which every MCP client keeps as the server's log.
//
// The server is returned unconnected: the caller picks the transport (stdio
// for `investviews mcp`, in-memory for tests).
func New(newClient ClientFactory, version string, logger *slog.Logger) *mcp.Server {
	return build(&server{
		clientFor: func(*mcp.CallToolRequest) (*api.Client, error) { return newClient() },
	}, version, logger)
}

// build is New for either kind of server: the stdio one from New, and the
// hosted one from NewHTTPHandler. Both get the same tools, instructions and
// per-call deadline, so the local and the hosted server cannot drift apart.
func build(s *server, version string, logger *slog.Logger) *mcp.Server {
	serverOpts := &mcp.ServerOptions{
		Instructions: instructions,
		// ⚠️ THE TOOLS CAPABILITY IS SET EXPLICITLY. The SDK only infers it
		// once a tool is added, and a server that does not advertise it
		// makes a well-behaved client skip tools/list entirely. The tool
		// set is fixed at startup, so there is never a list change to
		// announce: ListChanged stays false.
		Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}},
		Logger:       logger,
	}

	srv := mcp.NewServer(&mcp.Implementation{Name: Name, Version: version}, serverOpts)
	srv.AddReceivingMiddleware(toolDeadline(toolTimeout))
	s.register(srv)
	return srv
}

// ToolTimeout is the longest one tool call may run, from the moment the server
// reads it to the moment it answers — every HTTP attempt, every retry wait,
// and for compare_places every place, together.
//
// ⚠️ IT IS ONE DEADLINE FOR THE WHOLE CALL, NOT PER REQUEST. The retry budget
// (retryBudget) caps only the SLEEPS inside one HTTP call; each attempt can
// still take the HTTP timeout, and compare_places makes up to five calls. A
// deadline on the call's context bounds all of it: when it runs out, the
// request in flight is cancelled and the tool answers with what it has.
// internal/commands/mcp.go sizes its drain wait from this value.
const ToolTimeout = 30 * time.Second

// toolTimeout is ToolTimeout as a variable, so a test can make it short.
var toolTimeout = ToolTimeout

// toolDeadline puts the per-call deadline on every tools/call request. Other
// methods (initialize, tools/list) answer from memory and get none.
func toolDeadline(timeout time.Duration) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/call" || timeout <= 0 {
				return next(ctx, method, req)
			}
			ctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			return next(ctx, method, req)
		}
	}
}

// register adds every tool to srv. It is the one place to look for the tool
// set: one line per tool file.
func (s *server) register(srv *mcp.Server) {
	s.registerGeo(srv)     // find_place, browse_places, place_at_point — free
	s.registerMeta(srv)    // coverage, filters, usage — free
	s.registerStats(srv)   // market_stats, price_history — METERED
	s.registerCompare(srv) // compare_places — METERED, one call per place
}
