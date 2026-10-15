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
//     call that needed it — not as a server that refused to start.
package mcpserver

import (
	_ "embed"

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
// commands.Deps.NewClient, which reads the saved token and returns
// ErrNoToken when there is none; tests point it at an httptest server.
type ClientFactory func() (*api.Client, error)

// server is the state every tool handler shares. It holds the factory, not a
// client — see rule 2 in the package comment.
type server struct {
	newClient ClientFactory
}

// New builds the MCP server. version is the build stamp
// (internal/version.Version) and is what `initialize` reports, so a support
// thread can ask the agent which build it is talking to.
//
// The server is returned unconnected: the caller picks the transport (stdio
// for `investviews mcp`, in-memory for tests).
func New(newClient ClientFactory, version string) *mcp.Server {
	s := &server{newClient: newClient}

	srv := mcp.NewServer(
		&mcp.Implementation{Name: Name, Version: version},
		&mcp.ServerOptions{
			Instructions: instructions,
			// ⚠️ THE TOOLS CAPABILITY IS SET EXPLICITLY. The SDK only infers it
			// once a tool is added, and a server that does not advertise it
			// makes a well-behaved client skip tools/list entirely. The tool
			// set is fixed at startup, so there is never a list change to
			// announce: ListChanged stays false.
			Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}},
		},
	)
	s.register(srv)
	return srv
}

// register adds every tool to srv. It is the one place to look for the tool
// set; each later tool file adds one line here.
func (s *server) register(srv *mcp.Server) {
	s.registerGeo(srv)     // find_place, browse_places, place_at_point — free
	s.registerMeta(srv)    // coverage, filters, usage — free
	s.registerStats(srv)   // market_stats, price_history — METERED
	s.registerCompare(srv) // compare_places — METERED, one call per place
}
