# investviews-cli

**Real-estate market data for your AI assistant.** Ask Claude what flats cost in a neighbourhood, or
how prices in a city moved over the last year, and it answers with real figures — median price, price
per m², typical size — each one citing the period it comes from.

It is a command-line client for the [InvestViews public API](https://docs.investviews.ai), plus a
Claude Code plugin that teaches Claude how to use it. You can also run the CLI by hand.

## Quick start — ask Claude about property prices

**1. Get an API token.** Tokens are created in your
[InvestViews account settings](https://investviews.ai/user/account). See the
[API quickstart](https://docs.investviews.ai/quickstart.html) for details.

**2. Install the CLI.**

```sh
brew install Investviews/tap/investviews
```

On Linux, or without Homebrew, download the tarball for your platform from the
[releases page](https://github.com/Investviews/investviews-cli/releases) and put `investviews` on
your `PATH`.

**3. Store your token.** It is saved to `~/.config/investviews/config.toml`, readable only by you.

```sh
investviews auth login --token iv_live_xxxxxxxx
```

**4. Add the plugin to Claude Code.** Run these inside Claude Code:

```
/plugin marketplace add Investviews/investviews-cli
/plugin install investviews@investviews-cli
```

**5. Ask a question in plain words.**

> What do flats cost in Russafa, Valencia right now?
>
> How have property prices in Montenegro changed over the last year?
>
> Which countries do you have data for?

### What Claude does with that

1. **Finds the place — free.** It searches or browses to the exact place, and uses the parent chain
   to tell apart places with the same name (there are several neighbourhoods called "Centro").
2. **Checks there is data — free.** Each place says whether figures exist and how fresh they are, so
   Claude does not pay to ask about a place with nothing behind it.
3. **Asks for the figures — one metered call.** Your token has a quota; only these calls count
   against it. Finding and checking places never does.
4. **Answers with a source.** Every figure comes with the period it covers, so you can check it.

Every command prints a `cost:` line saying whether it was free or metered, so you can see what was
spent.

### Try the plugin without installing it

Load this repository for a single session:

```sh
git clone https://github.com/Investviews/investviews-cli.git
claude --plugin-dir investviews-cli -p "What do flats cost in Russafa, Valencia?"
```

The CLI must still be installed and logged in (steps 2 and 3).

## Use with MCP

InvestViews is an MCP (Model Context Protocol) server. It gives an AI agent the API as typed tools,
so the agent does not have to run shell commands. There are two ways to connect: the hosted server
(nothing to install) and the local server (`investviews mcp`, runs on your computer). Both serve the
same tools.

### Hosted server — nothing to install

Add this URL to your agent as a remote MCP server. In Claude Desktop and claude.ai: Customize →
Connectors → + Add → Add custom connector (older versions call it Settings → Connectors):

```
https://mcp.investviews.ai/mcp
```

Click **Connect**. A sign-in window opens on investviews.ai; sign in and click **Allow**. The agent
gets its own read-only API token. It is listed in your account under API tokens, and removing it
there disconnects the agent. The tools need a PRO plan.

In Claude Code:

```sh
claude mcp add --transport http investviews https://mcp.investviews.ai/mcp
```

The hosted server is `investviews mcp --http` (see `investviews mcp --help`). It checks each token
with the API at most once a minute and sends every API call with the caller's own token, so
metering, plan checks and revocation work exactly as for the CLI.

### Running the hosted server yourself

You do not need this to use InvestViews: the URL above is already running. It is here for anyone who
runs their own copy.

```sh
investviews mcp --http 127.0.0.1:8090 \
  --public-url https://mcp.investviews.ai \
  --auth-server https://api.investviews.ai
```

| Flag | Default | What it does |
|---|---|---|
| `--http <host:port>` | off (stdio) | serve Streamable HTTP on this address instead of stdio. Every request needs a bearer token |
| `--public-url <origin>` | `https://mcp.investviews.ai` | the public origin agents use, with no path. The protected resource is `<public-url>/mcp` |
| `--auth-server <origin>` | `https://api.investviews.ai` | the OAuth authorization server named in the protected resource metadata |

`--public-url` and `--auth-server` are refused without `--http`. The server answers on:

| Path | What |
|---|---|
| `POST /mcp` | the MCP endpoint (bearer token required; a missing or bad token gets `401` with a `WWW-Authenticate` header pointing at the metadata) |
| `GET /.well-known/oauth-protected-resource` and `.../mcp` | the protected resource metadata |
| `GET /healthz` | `ok`; makes no API call |

Put it behind a TLS proxy that does not buffer responses (some answers are streamed as server-sent
events). It stops cleanly on `SIGTERM` and lets requests in flight finish first. It reads the API
base from `INVESTVIEWS_API_URL` and, like every command, needs a home directory for its config path.

### Local server — `investviews mcp`

Use this instead if you want the server on your own computer. The agent starts it itself; you never
run `investviews mcp` by hand.

**First, install the binary and log in** — steps 2 and 3 of the quick start. The server uses the
same token as the CLI. It starts without a token, but every tool call then fails with an error that
says to run `investviews auth login`. A login made while the agent runs is used at once — no
restart needed.

**The binary must be new enough.** `investviews mcp` is not in v0.2.1 or older. Check with
`investviews version`. If `investviews mcp --help` says `unknown command "mcp"`, upgrade the binary
(`brew upgrade --cask Investviews/tap/investviews`, or a newer release tarball). The Claude Code
plugin registers the server whatever binary you have, so an old binary makes the server fail to
start.

**Claude Code.** The plugin from step 4 already registers the server. Without the plugin, add it
with one command:

```sh
claude mcp add investviews -- investviews mcp
```

**Claude Desktop.** Add this to `claude_desktop_config.json` (on macOS:
`~/Library/Application Support/Claude/claude_desktop_config.json`), then restart Claude Desktop:

```json
{
  "mcpServers": {
    "investviews": {
      "command": "investviews",
      "args": ["mcp"]
    }
  }
}
```

Claude Desktop may not see your shell's `PATH`. If the server does not start, put the full path in
`command` — `which investviews` prints it (for example `/opt/homebrew/bin/investviews`).

**Cursor.** Add the same `mcpServers` block to `~/.cursor/mcp.json` (all projects) or to
`.cursor/mcp.json` in one project.

**Token in the config instead of the file.** Any of the configs above can pass the token as an
environment variable. It outranks the stored token, like on the command line:

```json
"investviews": {
  "command": "investviews",
  "args": ["mcp"],
  "env": { "INVESTVIEWS_TOKEN": "iv_live_xxxxxxxx" }
}
```

### The tools

| tool | cost | what it does |
|---|---|---|
| `find_place` | free | resolve a place name to a `geo_id` (`q`, `country`, `level`, `limit`) |
| `browse_places` | free | walk the place tree (`parent`, `level`, `limit`, `page`) |
| `place_at_point` | free | name the H3 cell and the zones at a coordinate (`lat`, `lng`, `res`) |
| `coverage` | free | which markets are served, and how fresh each is (`country`) |
| `filters` | free | every value the filter arguments accept |
| `usage` | free | what this token has spent and has left |
| **`market_stats`** | **1 metered call** | the newest period's figures for one territory |
| **`price_history`** | **1 metered call** | the same figures, period by period (`from`, `to`) |
| **`compare_places`** | **1 metered call per place** | 2 to 5 places side by side (`geo_ids`), same filters |

`market_stats` and `price_history` take exactly one territory: `geo_id`, or `h3`, or
`lat` + `lng` + `radius_km`. They also take the filters `ad_type`, `ad_sub_type`, `rooms`,
`min_size`, `max_size`, `min_price_usd`, `max_price_usd` and `currency`. `compare_places` takes the
same filters.

- Every tool description starts with **FREE** or **METERED**, so the agent knows the cost before it
  calls.
- A metered answer ends with a `cost:` line taken from the quota headers. A free answer has no
  cost line when the server marks it uncounted; an answer with no quota headers at all gets
  `cost: unknown`.
- Every failure — no token, a bad token, quota spent, a rate limit, a bad argument — comes back as
  a tool result marked as an error, with a message the agent can show you. Waits between retries
  add up to about 10 seconds at most, and every tool call — `compare_places` with all its places
  included — is cut off after 30 seconds, so a slow API or a rate limit never blocks the chat.
- The server sends the agent short instructions with the skill's rules: find the place for free
  first, spend one metered call per question, call every figure a median of asking prices.
- Standard output carries only protocol messages. Logs go to standard error, which the agent keeps
  as the server's log.

## Using another AI agent

The CLI is an ordinary shell command, so any agent that can run commands can use it. An agent that
speaks MCP can use the server above instead. The instructions Claude follows are plain Markdown in
[`skills/investviews/SKILL.md`](skills/investviews/SKILL.md) — give that file to your agent as its
guide. Claude Code is the only agent this repository ships a ready-made plugin for.

## What is in the plugin

| file | what it is |
|---|---|
| `.claude-plugin/marketplace.json` | the marketplace, named **`investviews-cli`**, listing one plugin |
| `.claude-plugin/plugin.json` | the plugin, named **`investviews`** — hence `investviews@investviews-cli`. It also registers the MCP server (`investviews mcp`) |
| `skills/investviews/SKILL.md` | the skill: how to go from a question to a cited figure |

The skill teaches a workflow, not HTTP details: resolve the place for free, check that data exists,
spend one metered call, cite the period. An agent that cannot tell free calls from metered ones
either stops to ask permission it does not need, or burns quota it did not have to.

## Using the CLI by hand

| command | cost | what it does |
|---|---|---|
| `geo browse` | free | walk the geography from the countries down |
| `geo search <name>` | free | resolve a name to a `geo_id` |
| `geo lookup` | free | name the zones containing one cell or one point |
| `geo hexes <geo_id>` | free | list a place's H3 cells (`--ids-only` for a pipe) |
| `coverage` | free | which markets are served, and how fresh each is |
| `usage` | free | what this token has spent and has left |
| `mcp` | free to start (the tools it serves may be metered) | serve the API to an AI agent as MCP tools over stdio, or over HTTP with `--http` — see [Use with MCP](#use-with-mcp) |
| **`stats current`** | **METERED** | figures for the newest built period |
| **`stats history`** | **METERED** | the same figures, period by period |

Every run prints what it cost, taken from the quota headers the server sent back — a free call and
a metered one never look alike. Discovery is free on purpose: look a place up rather than guessing
an id, then spend one metered call once you know it holds data.

`--json` prints the response as JSON and moves the cost line to stderr, so stdout pipes into `jq`.

### Reaching a place from nothing

```sh
investviews geo browse                            # the countries
investviews geo browse --parent es                # Spain's top level
investviews geo browse --parent R349055           # that region's children
investviews stats current --geo-id R5326784       # the place you reached
```

Every row prints the `geo_id` the next call takes, so you never guess a name.

### Piping cell ids into `stats`

```sh
investviews geo hexes R344953 --all --ids-only | investviews stats current --h3 -
```

⚠️ **`--ids-only` is not optional here.** The plain `geo hexes` output is written for a reader — a
header line, an availability sentence and the cost line surround the ids — and piping that sends
those words to a **metered** endpoint as if they were cells. `--ids-only` prints the ids and nothing
else, and puts the cost line on stderr so stdout stays a clean stream. `stats` also checks every
`--h3` value against the H3 bit layout before it sends anything, so the plain pipe now fails locally
and free.

### ⚠️ `--level` means two different things

Which one depends on `--parent`:

| call | what comes back |
|---|---|
| `geo browse --parent es --level city` | **every city in Spain** — a whole-level jump, from any region |
| `geo browse --parent R349055 --level city` | **only that region's** own cities — a filter on its direct children |

The header line of every result says which of the two happened. `browse` never adds a `--level` of
its own: doing so would make the flag mean one thing at depth 1 and another at depth 2, and would
hide `region` and `province` entirely.

### ⚠️ An empty result is usually the right answer

Levels are **skipped, not shifted**. A city's direct children are macrozones, so asking a city for
microzones legitimately returns nothing, and a city with a blank province hangs straight off its
region. "No rows at this level" prints as a normal answer with a next move, and **exits 0** —
reading it as a failure abandons a place that plainly has children.

The same holds for figures: `stats current` answering with no rows is a covered place that held
nothing in that window, not an error and not a zero.

### Exit codes

| code | meaning |
|---|---|
| 0 | success, including an empty result |
| 1 | any other failure, including a usage mistake caught before the request was sent |
| 2 | money — quota exhausted, or an inactive subscription |
| 3 | credentials — no token, a bad token, or a read-only token on a write endpoint |
| 4 | we hold no data for that country at all; retrying never succeeds |

`investviews mcp` exits 0 when the agent closes its standard input. A failed tool call is a tool
result marked as an error, not an exit code.

## Install

**Release binaries.** Every `v*` tag publishes `darwin`/`linux` × `amd64`/`arm64` tarballs and a
`checksums.txt` on the [releases page](https://github.com/Investviews/investviews-cli/releases).
Download the one for your platform, check it against `checksums.txt`, unpack it and put
`investviews` on your `PATH`.

**Homebrew (macOS).**

```sh
brew install Investviews/tap/investviews
```

Homebrew installs casks on macOS only, so on Linux use the release tarball.

**Which build am I running?**

```sh
investviews version          # investviews 0.1.0 (commit …, built …, go1.25.4, darwin/arm64)
investviews version --json
```

A binary you built yourself reports `dev`. That is not a fault — it means the version stamp the
release pipeline writes was never applied.

## Build

```sh
go build -o investviews ./cmd/investviews
```

Requires Go 1.25 or newer. CI pins the exact patch release it builds with; see
`.github/workflows/test.yml`.

A local build is unstamped and reports `dev`. To see what a release build reports, without
releasing anything:

```sh
goreleaser release --snapshot --clean
./dist/investviews_darwin_arm64_v8.0/investviews version
```

## Authentication

Get a token from your InvestViews account, then store it:

```sh
investviews auth login --token iv_live_xxxxxxxx
```

The token is written to `~/.config/investviews/config.toml` with mode **0600**. If that file is
already readable by the group or by everyone, nothing is written and the command tells you to fix
the mode first — a token in a world-readable file is a leaked token.

You can also pipe the token in, which keeps it out of your shell history:

```sh
echo "$INVESTVIEWS_TOKEN" | investviews auth login
```

| command | what it does |
|---|---|
| `investviews auth login` | store a token in the config file |
| `investviews auth status` | say whether a token is configured, where it came from, and show it masked; exits 3 when none is configured |
| `investviews auth logout` | remove the stored token |

`auth status` never prints a full token — only the first and last four characters.

## Configuration

A token is resolved from these three places, highest precedence first:

| # | source | note |
|---|---|---|
| 1 | `--token` flag | per-command, never stored |
| 2 | `INVESTVIEWS_TOKEN` environment variable | per-shell, never stored |
| 3 | `~/.config/investviews/config.toml` | written by `auth login` |

`auth status` reports which of the three won.

### `INVESTVIEWS_API_URL`

Overrides the base URL the CLI talks to. It defaults to `https://api.investviews.ai/public/v1`.

```sh
INVESTVIEWS_API_URL=http://localhost:3000/public/v1 investviews auth status
```

It takes **any** base URL — it exists so you can point the CLI at a server you are running
locally. It is **not** a staging switch: there is no staging environment, and the default is
production.

### Config file

```toml
# ~/.config/investviews/config.toml
token   = "iv_live_xxxxxxxx"
api_url = "http://localhost:3000/public/v1"   # optional; INVESTVIEWS_API_URL still outranks it
```

`auth login` only ever writes `token`, and keeps an `api_url` you put there by hand.
A missing config file is fine; a malformed one is an error that names the file.

## The Go module path is lowercase on purpose

The repository is **`Investviews/investviews-cli`** (capital I) and the module path is
**`github.com/investviews/investviews-cli`** (all lowercase). The mismatch is deliberate — please
do not "correct" it.

- Go module paths are case-sensitive, and the module proxy escapes a capital letter as
  `!investviews`, so the two spellings are genuinely different strings. That is exactly why the
  choice is written down instead of being left to whoever ran `go mod init` first.
- Nothing resolves this string. `go install` and `go get` were rejected as distribution channels
  for this CLI — it ships as a Homebrew cask and release binaries — and no other project imports
  this module.
- GitHub resolves owner names case-insensitively over HTTPS, so a manual `git clone` of the
  lowercase form works anyway.
- Changing a module path rewrites every internal import in the repository, so this is a one-way
  door. It was decided once, on 2026-09-11, by the operator.

## Licence

MIT — see [LICENSE](LICENSE).
