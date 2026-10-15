# InvestViews MCP server

## Overview

- Add an MCP (Model Context Protocol) server so AI agents (Claude Desktop, Claude Code, Cursor,
  later claude.ai and ChatGPT) can call InvestViews data as tools, not only through the CLI + skill.
- **Phase 1 (this plan, in detail):** `investviews mcp` — a local stdio MCP server inside this Go CLI.
  It reuses the typed API client (`internal/api`) and the token the user already saved with
  `investviews auth login`.
- **Phase 2 (outline only):** a hosted server at `https://api.investviews.ai/mcp` (streamable HTTP),
  built in the API server. First with the same bearer API token, then OAuth so claude.ai / ChatGPT
  connectors can use it. Phase 2 is planned separately in the API repository.
- Benefit: a user installs once (`brew install investviews/tap/investviews`), adds one line to the
  agent's config, and the agent gets typed tools with the skill's rules built in.

## Context (from discovery)

- `internal/api/endpoints.go` — typed client for every public endpoint: `GeoSearch`, `GeoBrowse`,
  `GeoLookup`, `PlaceHexes`, `Coverage`, `MetaFilters`, `Usage`, `StatsCurrent`, `StatsHistory`,
  `Ping`, `OpenAPI`. Client-side validation (e.g. `StatsParams.selector()` — exactly one territory
  selector; `GeoMaxLimit` / `GeoMaxPage`) refuses bad input before a request is sent.
- `internal/api/client.go` — `Meta` carries quota headers (`Metered()`, `Cost()`); retries/backoff
  live here, including 429 `Retry-After` waits of up to 2 minutes.
- `internal/api/errors.go` — `APIError.Error()` already includes message and `docs_url`; `Hint()`
  gives the next step. 402 is `QuotaError`, 429 is `RateLimitError`, a place miss is 404
  `unknown_place` with `did_you_mean`.
- `internal/api/types.go` — `CurrentStats` / `HistoryResponse` carry
  `Availability{Status ok|no_data, Reason, Message, Earliest/LatestNonemptyPeriod}`. Geo results
  carry `prices_available`, `has_data`, `current_period`. `Ancestors` and `NullableDate` have custom
  `MarshalJSON` and unexported fields; `Filters`, `Segment`, `BinApproximations` are
  `json.RawMessage`.
- `internal/commands/commands.go` — `Deps{NewClient}` is how every command gets a client; tests
  swap in a client pointed at an `httptest` server. `availability()` (geo three-state signal),
  `costLine`, `ancestry` live here. `ErrNoToken` is returned by `main.go`'s `newClient` before any
  request. `stats.go` renders `Availability` with `renderWhy`; `geo.go` renders a search miss with
  `renderSearchMiss`.
- `skills/investviews/SKILL.md` — the agent rules (free vs metered calls, intake questions, name
  the statistic as a median of asking prices, always show price per m², mark estimates with `~`,
  offer a report, never call the API's `/reports`). The MCP server's `instructions` must carry the
  same rules.
- `.claude-plugin/plugin.json` and `.claude-plugin/marketplace.json` — Claude Code plugin manifest
  and marketplace entry; the plugin can declare `mcpServers`.
- Dependencies: Go 1.25, cobra, BurntSushi/toml. New: `github.com/modelcontextprotocol/go-sdk`
  v1.8.0 (official Go SDK; needs go 1.25.0).

## Development Approach

- **testing approach**: TDD — for each tool, write the test first (tool arguments, fake API reply,
  expected tool result and expected request), see it fail, then write the code.
- complete each task fully before moving to the next
- make small, focused changes
- **CRITICAL: every task MUST include new/updated tests** for code changes in that task
  - tests are not optional - they are a required part of the checklist
  - write unit tests for new functions/methods
  - write unit tests for modified functions/methods
  - add new test cases for new code paths
  - update existing test cases if behavior changes
  - tests cover both success and error scenarios
- **CRITICAL: all tests must pass before starting next task** - no exceptions
- **CRITICAL: update this plan file when scope changes during implementation**
- run tests after each change: `go test ./...`
- maintain backward compatibility: no existing command, flag or output changes

## Testing Strategy

- **unit tests**: required for every task. Each tool is tested end to end inside the process:
  an MCP client and server joined by `mcp.NewInMemoryTransports()`, the server's API client pointed
  at an `httptest` server. Assert on (a) the HTTP request the fake API received (path + query),
  (b) `structuredContent`, (c) the text content, (d) `isError` for failures.
- Fake replies must include the awkward shapes: `ancestors` as an array and as null,
  `current_period` set and null, raw JSON `filters` / `segment`.
- **no network**: no test talks to the real API.
- **e2e**: the project has no UI e2e suite. One manual smoke run with MCP Inspector is listed under
  Post-Completion.

## Progress Tracking

- mark completed items with `[x]` immediately when done
- add newly discovered tasks with ➕ prefix
- document issues/blockers with ⚠️ prefix
- update plan if implementation deviates from original scope
- keep plan in sync with actual work done

## Solution Overview

- New package `internal/mcpserver`. **It imports only `internal/api` and the SDK — never
  `internal/commands`** (`commands/mcp.go` imports `mcpserver`, so the other direction is a cycle).
  Anything both packages need moves into `internal/api` (see Task 3).
- `mcpserver.New(newClient func() (*api.Client, error), version string) *mcp.Server`. It receives
  `deps.NewClient` and calls it **per tool call**, so a `auth login` made while the server runs is
  picked up, and a missing token comes back as `ErrNoToken` from the factory — no second check.
- New cobra command `investviews mcp` (`internal/commands/mcp.go`) runs the server with
  `&mcp.IOTransport{Reader: cmd.InOrStdin(), Writer: cmd.OutOrStdout()}` (wrapped as closers), so
  tests can inject streams. Only protocol messages go to stdout; logs go to stderr.
- **Tool set: mirror + one workflow tool.**

  | tool | API call | cost |
  |---|---|---|
  | `find_place` | `GeoSearch` (`/geo/search`) | free |
  | `browse_places` | `GeoBrowse` (`/geo`) | free |
  | `place_at_point` | `GeoLookup` (`/geo/lookup`) | free |
  | `coverage` | `Coverage` (`/coverage`) | free |
  | `filters` | `MetaFilters` (`/meta/filters`) | free |
  | `usage` | `Usage` (`/usage`) | free |
  | `market_stats` | `StatsCurrent` (`/stats/current`) | 1 metered call (current) |
  | `price_history` | `StatsHistory` (`/stats/history`) | 1 metered call (history) |
  | `compare_places` | `StatsCurrent` once per place (2–5 places) | N metered calls (current) |

  `PlaceHexes` is left out on purpose: H3 cell lists are not useful in a chat. Add it later if a
  user asks.
- **Server instructions** (`internal/mcpserver/instructions.md`, `go:embed`): a short version of
  the skill rules — resolve the place with free tools first; one paid call per question; say every
  figure is a median of asking prices; show price per m²; mark estimates with `~`; ask the intake
  questions for vague requests; offer a comparison table or report, built from tool results, never
  from the API's `/reports`.
- Key decisions:
  - Reuse `internal/api` as is. The MCP layer only maps arguments → params and results → output.
  - **Output: `Out = any`, `structuredContent` set by hand** to the API payload. Do not use typed
    `Out` structs built from `api` types: the SDK infers an output schema from them and validates
    every result, and `Ancestors` / `NullableDate` / `json.RawMessage` fail that check as a protocol
    error. No `outputSchema` in phase 1.
  - Annotations: `ReadOnlyHint: true`, `IdempotentHint: true`, `OpenWorldHint: ptr(true)`.
    (`DestructiveHint` is ignored when read-only.)
  - Every tool returns `structuredContent` plus a short text summary. Metered tools add a cost line
    from `api.Meta.Cost()` — no third copy of `costLine`.
  - Errors are tool results with `isError: true` (a handler error becomes one in the SDK), not
    protocol errors. Text = `err.Error()` (message + `docs_url`) plus `Hint()` when present.
    Validation messages that name CLI flags (`--h3`, `--geo-id`) are rewritten to argument names
    (`h3`, `geo_id`) in the MCP layer.
  - Rate limits: the MCP server builds its client with a capped retry wait (≤ 10 s total) so a 429
    returns `isError` quickly instead of blocking a tool call for 2 minutes. The tool `ctx` is
    passed to every client call.

## Technical Details

- Tool input structs use typed handlers (`mcp.AddTool` with Go structs + `json` / `jsonschema`
  tags). **Every optional field has `omitempty`; optional numbers are pointers** (`*float64`,
  `*int`) so "not set" differs from 0. Only truly required fields are required.
- Field names are snake_case and match the API query names:
  - `find_place{q, country, level, limit}` — no paging; the server clamps `limit` at 50.
  - `browse_places{parent, level, limit, page}` — `limit`/`page` checked against `GeoMaxLimit` /
    `GeoMaxPage`; the description states both meanings of `level`.
  - `place_at_point{lat, lng, res}` (the API takes `res`, not `level`).
  - `market_stats{geo_id | h3[] | lat+lng+radius_km, res, ad_type, ad_sub_type, rooms[], min_size,
    max_size, min_price_usd, max_price_usd, currency}`; `price_history` adds `from`, `to`.
- Territory: exactly one of `geo_id`, `h3`, or `lat`+`lng`+`radius_km`. Validation stays in
  `StatsParams.selector()`; the error comes back as `isError` and no request is sent.
- **Two different availability signals:**
  - Geo tools (`find_place`, `browse_places`, `place_at_point`): the three-state signal from
    `prices_available` / `has_data` / `current_period` — the CLI's `availability()`, moved to
    `internal/api` (Task 3).
  - Stats tools (`market_stats`, `price_history`, `compare_places`): `Availability.Status`,
    `Reason`, `Message`, and the earliest/latest nonempty periods — the same facts `renderWhy`
    shows.
- Place miss: 404 `unknown_place` returns `isError` whose text lists the `did_you_mean`
  suggestions (name + geo id), like `renderSearchMiss`.
- `compare_places`: input `geo_ids` (2–5), shared filters. Runs `StatsCurrent` per place in order.
  Each place gives a row: name, median asking price per m², median asking price, median size,
  availability, or an error reason (e.g. unknown place). It **stops only on auth or quota errors**,
  and then still returns the rows already fetched plus an error row, so paid results are not lost.
  The description states the cost (one metered call per place) so the agent can ask first.
- Version: `Implementation{Name: "investviews", Version: version.Version}`.

## What Goes Where

- **Implementation Steps** (`[ ]` checkboxes): phase 1 — code, tests and docs in this repository.
- **Post-Completion** (no checkboxes): manual checks, the release, and phase 2 in the API
  repository.

## Implementation Steps

### Task 1: Add the MCP SDK and an empty server

**Files:**
- Modify: `go.mod`, `go.sum`
- Create: `internal/mcpserver/server.go`
- Create: `internal/mcpserver/server_test.go`
- Create: `internal/mcpserver/helpers_test.go`

- [x] write test: connect a client over the in-memory transport; `initialize` returns server name
      `investviews` and the given version; `tools/list` returns an empty list
- [x] add `github.com/modelcontextprotocol/go-sdk@v1.8.0` to `go.mod`
- [x] create `mcpserver.New(newClient, version)`
- [x] create test helper: fake API (`httptest`, records requests) + connected client session,
      reused by later tasks
- [x] run tests - must pass before next task

### Task 2: Server instructions

**Files:**
- Create: `internal/mcpserver/instructions.md`
- Modify: `internal/mcpserver/server.go`
- Modify: `internal/mcpserver/server_test.go`

- [x] write test: `initialize` result has non-empty `instructions` containing the key rules
      (median of asking prices, price per m², free tools before paid, no `/reports`)
- [x] write `instructions.md` from `skills/investviews/SKILL.md` (short; under ~60 lines)
- [x] embed it with `go:embed` and pass it in `ServerOptions.Instructions`
- [x] run tests - must pass before next task

### Task 3: Shared helpers in `internal/api`, result and error mapping

**Files:**
- Modify: `internal/api/types.go` (or a new `internal/api/signal.go`)
- Modify: `internal/commands/commands.go`, `internal/commands/geo.go` (call the moved helper)
- ➕ Create: `internal/api/signal.go` (`api.PlaceAvailability` → `PlaceSignal`), `internal/api/signal_test.go`
- ➕ Modify: `internal/api/client.go` (`Options.RetryBudget`, `Client.WithRetryBudget`), `internal/api/client_test.go`
- ➕ note: the shared helper returns the DECISION (`PlaceSignal`), not the CLI's sentence — the
  sentence names CLI commands. `geo.go` needed no change: it still calls `availability()`, which
  now reads `api.PlaceAvailability`.
- Create: `internal/mcpserver/result.go`
- Create: `internal/mcpserver/result_test.go`
- Modify: `internal/api/*_test.go` for the moved helper

- [x] move the geo three-state `availability()` into `internal/api` as an exported function; CLI
      calls it; existing CLI tests stay green with no output change
- [x] write tests: `APIError` → `isError` text with message, `docs_url` and `Hint()`; transport
      error → `isError`; factory error (`ErrNoToken`) → `isError` with its text; metered `Meta` →
      cost line from `Meta.Cost()`; free `Meta` → no cost line; flag names rewritten to argument
      names
- [x] write test: payload with array and null `ancestors`, null `current_period` and raw
      `filters` goes into `structuredContent` without a protocol error
- [x] implement `ok(payload, text, meta)` and `fail(err)` helpers (`Out = any`)
- [x] implement the per-call client: call `newClient`, apply the capped retry wait
- [x] run tests - must pass before next task

### Task 4: Free geography tools — `find_place`, `browse_places`, `place_at_point`

**Files:**
- Create: `internal/mcpserver/geo.go`
- Create: `internal/mcpserver/geo_test.go`
- ➕ Modify: `internal/mcpserver/server.go` (register), `server_test.go` (tools/list names the tools), `helpers_test.go` (`callTool`, `structured`, `wantQuery`)
- ➕ note: `place_at_point` takes `res` (H3 resolution), not `level` — `/geo/lookup` has no level parameter and returns every containing zone; lat/lng ranges are checked in the MCP layer (no request)

- [x] write tests per tool: arguments → expected request path + query; payload in
      `structuredContent`; text lists name, geo id, ancestry and the three-state signal
- [x] write test: `tools/list` input schema marks only `q` (find_place) and `lat`, `lng`
      (place_at_point) as required
- [x] write error tests: empty query; lat/lng out of range; `limit`/`page` over the max (no request);
      404 `unknown_place` with `did_you_mean` → suggestions in the text
- [x] implement the three tools with annotations
- [x] run tests - must pass before next task

### Task 5: Free meta tools — `coverage`, `filters`, `usage`

**Files:**
- Create: `internal/mcpserver/meta.go`
- Create: `internal/mcpserver/meta_test.go`
- ➕ Modify: `internal/mcpserver/server.go` (register), `server_test.go` (`wantTools`)

- [x] write tests per tool: request sent, `structuredContent`, short text (for `usage`: remaining
      per group)
- [x] write error test: 401 from the API → `isError` that says the token is invalid
- [x] implement the three tools
- [x] run tests - must pass before next task

### Task 6: Paid tool — `market_stats`

**Files:**
- Create: `internal/mcpserver/stats.go`
- Create: `internal/mcpserver/stats_test.go`
- ➕ Modify: `internal/mcpserver/server.go` (register), `server_test.go` (`wantTools`)
- ➕ note: shared pieces for Tasks 7–8 live in `stats.go`: `territoryArgs` (+ `params()`,
  `selectorText()`), `statsFilterArgs` (+ `apply()`), `availabilityText` / `verdictText` /
  `reasonMove`, `coreFigures`, `territoryText`, `appliedFiltersText`

- [x] write tests: `geo_id` / `h3` / point selectors each send the right query; filters map to
      query names; unset optional numbers are not sent; text names "median asking price per m²",
      "median asking price", "median size"; cost line present
- [x] write tests for `Availability`: `ok`, and `no_data` with reason, message and nonempty periods
- [x] write error tests: two selectors → `isError` with no request sent; 402 → quota `isError`;
      429 → rate-limit `isError` returned within the capped wait
- [x] implement `market_stats`
- [x] run tests - must pass before next task

### Task 7: Paid tool — `price_history`

**Files:**
- Modify: `internal/mcpserver/stats.go`
- Modify: `internal/mcpserver/stats_test.go`

- [ ] write tests: `from`/`to` passed through; text gives one line per period with median asking
      price per m²; says points are not additive; cost line names the history group
- [ ] write error test: range over 24 periods → API 422 → `isError` that says to split the range
- [ ] implement `price_history`
- [ ] run tests - must pass before next task

### Task 8: Workflow tool — `compare_places`

**Files:**
- Create: `internal/mcpserver/compare.go`
- Create: `internal/mcpserver/compare_test.go`

- [ ] write tests: 3 places → 3 requests in order; one row per place with the three core columns;
      `structuredContent` is the list of rows; cost line says 3 metered calls
- [ ] write tests: fewer than 2 or more than 5 places → `isError`, no request
- [ ] write tests: 404 for place 2 → error row for it, places 1 and 3 still returned;
      `no_data` place → row with its availability reason
- [ ] write test: 402 on place 2 → stop; result has row 1, an error row for place 2, and says how
      many metered calls were made
- [ ] implement `compare_places`
- [ ] run tests - must pass before next task

### Task 9: `investviews mcp` command

**Files:**
- Create: `internal/commands/mcp.go`
- Create: `internal/commands/mcp_test.go`
- Modify: `internal/commands/commands.go` (add to `All`)

- [ ] write test: the command runs the server on injected stdin/stdout; an `initialize` +
      `tools/list` exchange returns all 9 tools; stdout holds only JSON-RPC lines
- [ ] write test: SDK and command logs go to stderr, not stdout
- [ ] write test: `investviews mcp --help` shows the config line for agents
- [ ] implement the command with `mcp.IOTransport` over `cmd.InOrStdin()` / `cmd.OutOrStdout()`
- [ ] run tests - must pass before next task

### Task 10: Register the server in the Claude Code plugin

**Files:**
- Modify: `.claude-plugin/plugin.json`
- Modify: `.claude-plugin/marketplace.json`
- Modify: `skills/investviews/SKILL.md`

- [ ] add `mcpServers.investviews` → `{"command": "investviews", "args": ["mcp"]}`
- [ ] SKILL.md: one short section — when the MCP tools are present, use them instead of shelling
      out to the CLI; same rules apply; the binary must be installed first (`brew install`)
- [ ] bump the plugin version in both `plugin.json` and `marketplace.json`
- [ ] check: `go test ./...` still passes; both JSON files are valid
- [ ] run tests - must pass before next task

### Task 11: Verify acceptance criteria

- [ ] verify all 9 tools from the Solution Overview exist, with annotations and descriptions that
      state free or metered
- [ ] verify no-token, bad-token, quota, rate-limit and validation errors all come back as
      `isError` results, never protocol errors
- [ ] run full test suite: `go test ./...`
- [ ] run `go vet ./...`
- [ ] verify the existing CLI commands' tests are green and their output is unchanged

### Task 12: [Final] Update documentation

- [ ] README: "Use with MCP" section — install the binary first, then config snippets for Claude
      Code (`claude mcp add investviews -- investviews mcp`), Claude Desktop and Cursor
- [ ] `docs/api.md`: tool → endpoint table
- [ ] move this plan to `docs/plans/completed/`

## Post-Completion

*Items requiring manual intervention or external systems - no checkboxes, informational only*

**Manual verification**
- Run `npx @modelcontextprotocol/inspector investviews mcp` with a real token. Call every free
  tool, one `market_stats`, one `price_history`, one `compare_places` with 2 places. Check the
  quota moved by the expected amount.
- Add the server to Claude Desktop and ask a vague question ("is Valencia a good place to buy?").
  Check the agent asks the intake questions, names medians, and shows price per m².

**Release**
- Tag a minor release (goreleaser + Homebrew tap). The repository is public: no internal links in
  the tag message or release notes.

**Phase 2 — hosted server (outline; plan it in the API repository)**
- Endpoint `POST /mcp` on `api.investviews.ai`, streamable HTTP, stateless JSON responses.
- Same 9 tools and the same instructions text; tool logic calls the API's own services, not HTTP.
- Step 2a — auth with the existing bearer API token (`Authorization: Bearer iv_live_…`). Works for
  Claude Code, Claude Desktop config and Cursor. Same quota and metering as the REST API.
- Step 2b — OAuth 2.1 (authorization server metadata, dynamic client registration, PKCE) so
  claude.ai and ChatGPT connectors can sign a user in. Tokens map to the user's API account and
  quota.
- Keep the same tool names, arguments and output shapes as phase 1, so an agent sees no difference
  between local and hosted.
