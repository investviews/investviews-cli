---
name: investviews
description: Answer real-estate market questions about a named place using the InvestViews CLI — median price, price per square metre, typical size, and how those moved period by period, for a country, region, province, city, district or neighbourhood, plus which markets are covered and how fresh each one is. Use when asked what property costs somewhere, how prices there have changed, whether a market is served, or to turn a place name into the id those figures are addressed by. Requires the `investviews` binary and an API token.
---

# InvestViews

Market figures over real-estate listings, addressed by place.

The whole job has one shape:

> **find the place (free) → check whether asking is worth it (free) → spend ONE metered call → cite the period and the place**

Everything below exists so you do not skip a step and pay for a wrong answer.

## Setup

Install the CLI:

```sh
brew install Investviews/tap/investviews     # macOS
```

If the tap is not available, download the tarball for your platform from
<https://github.com/Investviews/investviews-cli/releases>, check it against `checksums.txt`, unpack
it, and put `investviews` on your `PATH`.

Then store a token once:

```sh
investviews auth login --token iv_live_xxxxxxxx
```

`INVESTVIEWS_TOKEN` in the environment works too and outranks the stored token. Check what is
configured with `investviews auth status` — it never prints a full token. If the user has no token,
say so and stop; nothing but `investviews version` works without one.

## 1. Free, then metered — read the `cost:` line

| command | cost |
|---|---|
| `geo browse`, `geo search`, `geo lookup`, `geo hexes` | **free**, uncounted |
| `coverage`, `usage` | **free**, uncounted |
| `stats current`, `stats history` | **METERED** |

Only the two `stats` commands spend quota. Everything else is free **by design** — the API charges
nothing for discovery so that a client looks a place up instead of guessing an id, because a guessed
id returns a confidently wrong answer.

What this means for you:

- **Never ask the user for permission to browse, search or check coverage.** Those cost nothing.
- **Do not burn metered calls exploring.** Resolve the place for free first, then spend once.
- Every run ends with a `cost:` line built from the quota headers the server actually sent. Read it.
  A free run says `cost: FREE — group metadata, uncounted · 1 request`; a metered one says
  `cost: METERED — group current · 1 request · 149 of 100000 used this cycle`. If you are unsure
  what a run cost, the answer is on that line, not in this document.
- `investviews usage` (free) shows what is left in each group.

## 2. Get a `geo_id` — never invent one

Only `geo search` takes a free-text name. Every other command takes an id that a previous call
returned. Two ways in:

```sh
investviews geo search russafa --country es      # a name you were given
investviews geo browse                           # nothing -> the 37 countries
investviews geo browse --parent es               # walk down from a country
```

Each result row prints the `geo_id` the next call takes, so you never type a name the API did not
give you.

### Names are not unique — the ancestor chain is how you choose

`geo search centro --country es` returns ten different places called "Centro" — in Madrid, Granada,
Málaga, Alicante, San Sebastián, Salamanca, Zaragoza and more. This is normal, not an error.

Every hit prints its chain:

```
2. Centro — macrozone, es
   geo_id: R9462466
   in:     Granada (city, R344685) › Granada (province, R349026) › Andalucía (region, R349044) › ES (country, es)
```

**Pick by the chain, never by the name.** If the user's question does not say which one, ask them —
show the chains and let them choose. Repeated names are common enough to assume ambiguity by
default: a 2026-08 measurement (before the current location tree, never re-measured) found **13.6 %**
of neighbourhood names repeated inside their own country. Quote that figure only with its date, or
not at all; the Madrid/Granada/Málaga example above is live and makes the point better.

### ⚠️ `--level` means two different things

Which one depends on `--parent`:

| call | what comes back |
|---|---|
| `geo browse --parent es --level city` | **every city in Spain** — a whole-level jump, from any region |
| `geo browse --parent R349055 --level city` | **only that region's** cities — a filter on its direct children |

The header line of the output says which of the two happened. Read it back rather than assuming.

### ⚠️ An empty result is usually the correct answer

Levels are **skipped, not shifted**. A city's direct children are macrozones, so
`geo browse --parent <city> --level microzone` returns nothing and **exits 0**. That is the honest
answer, not a failure.

When a `--level` filter comes back empty, **drop `--level`** and see what the place actually has
below it. Do not retry, and do not report the place as missing.

Depth is data-dependent. Reaching a neighbourhood in Spain took six calls
(countries → `es` → region → province → city → macrozone), but many branches correctly stop above
that, and `--parent es --level microzone` reaches one in two calls. **Do not promise the user a
number of steps.** The property that matters is that no name is ever guessed.

Two other notes on `geo browse`:

- At the root (no `--parent`) the API **ignores `--limit`** and returns all 37 countries. `--limit`
  works under a `--parent`. `--all` handles this: a page that comes back **longer** than the limit
  means the server ignored it, so there is no second page to ask for and the walk stops there.
- `--all` pages through everything; on `geo search` it is meaningless and the CLI says so.

## 3. Decide whether to spend — the three-state availability signal

Every geography row carries `prices_available` and `has_data` (servers older than 2026-09-20 send
`current_period`, a date, instead — the CLI reads either and prints the same lines). Together they
are **three** answers, not two:

| what the row says | what it means | what to do |
|---|---|---|
| `prices, data in the current period — ask stats current` | there are figures for the newest built period | spend one `stats current` |
| `prices, but nothing in the current period — try stats history, not a dead end` | **NOT "no data"** — too little in the CURRENT period only | try `stats history` |
| `no prices — dead end, do not spend a metered call` | nothing is held for this place | stop; pick another place |

On an older server the first line names the period instead:
`prices, current period 2026-08-01 — ask stats current`. It means the same thing.

**The middle state is the one that gets misread.** "Nothing in the current period" is not an empty
market. Live example: Montenegro (`me`) had nothing in the current period, `stats current --geo-id me`
returned zero rows — and `stats history --geo-id me` returned real figures, $3,271/m² median for
2025-09. Treating that place as dead would have thrown away the answer.

⚠️ **The signal is necessary, never sufficient.** It is measured over the whole place, across every
`ad_type`, before your filters apply. "Data in the current period" with a narrow `--rooms` or price
band can still come back with zero rows — and the API calls "nothing this period" *conservative*,
not proof. Treat the first line as "worth asking" and the middle one as "probably not".

`stats current` on a covered place holding nothing is a **normal answer with zero rows, exit 0** —
not an error, and not a price of zero. Never report it as "the price is 0".

**An empty answer says why.** The CLI prints a `Why:` line with the API's own reason, and — when
the API names them — the periods that DO hold data for your segment. Act on the reason:

| `Why:` reason | what to do |
|---|---|
| `no_data_for_selection` | your filters matched nothing — widen them |
| `below_minimum_sample` | too few listings to publish a figure — try `stats history`, or a wider area |
| `period_not_built` | the newest period is not built yet — ask again after the next build, or use `stats history` |
| `no_data_for_place` | we hold nothing here for this segment — pick another place or segment |
| `not_determined` | the API could not tell — treat it like `below_minimum_sample` |

If the `Why:` block names a range (`This segment has data from … to …`), ask `stats history` inside
that range: that call can succeed. The reason describes **our aggregate**, never the market — never
tell a user "nothing was for sale".

## 4. Spend one metered call, then cite it

```sh
investviews stats current --geo-id R14727511
investviews stats history --geo-id R14727511 --from 2025-01
```

Useful filters, all optional: `--ad-type` (defaults to `real_estate_residential`), `--ad-sub-type`
(`buy` or `rent`, defaults to `buy`), `--rooms`, `--min-size` / `--max-size`,
`--min-price-usd` / `--max-price-usd`, `--currency`.

⚠️ **Price filters are always USD**, whatever `--currency` displays. `--currency` only converts the
figures on the way out; it never changes which listings are selected.

A territory is named by **exactly one** of `--geo-id`, `--h3`, or `--lat`/`--lng`/`--radius-km`.
Two selectors are refused locally, before any request, so the mistake costs nothing.

⚠️ **`--res` belongs to the circle alone.** It is refused locally alongside `--geo-id` (a place is
answered as one row from its own boundary, so there is no cell size to choose) and alongside `--h3`
(a cell states its own resolution). If an empty answer makes you want a coarser view of a
**place**, the move is a wider filter set or `stats history`, never `--res`.

### ⚠️ Asking about cells: pipe with `--ids-only`

```sh
investviews geo hexes R344953 --all --ids-only | investviews stats current --h3 -
```

The plain `geo hexes` output is written for a reader — a header line, an availability sentence and
a cost line around the ids — so piping it **without** `--ids-only` sends those words to the metered
endpoint as cell ids. `--ids-only` prints the ids and nothing else. Every `--h3` value is checked
against the H3 bit layout before anything is sent — mode, resolution, base cell and all 15 digit
slots — so a word from a rendered listing fails locally and free instead of spending a request. (The
one id that still gets sent is a well-formed one naming a pentagon path that does not exist; the
server answers `400`. Ruling that out locally needs the pentagon table, which the CLI does not
carry.) Use `--all` as well, or the answer describes only the first page of the place.

### Citing the answer

Always say **which place** and **which window**. The figures describe one complete period, not
today:

> Russafa (a neighbourhood of València, Spain): median **$5,187/m²**, median price **$486,162**,
> median size **95 m²** — for the period **2026-08-01** (rolling 3 months,
> **2026-06-01 → 2026-08-31**), as of **2026-08-31**, in USD.

Three rules that keep a citation honest:

- **Never call it a live or current price.** The newest answer is the newest *complete* period and
  can be more than a month old.
- **A row marked `estimated` is pooled from neighbouring cells (~1.2 km)**, not that cell's own
  market. Say so.
- **Never add figures across periods.** Each period is an independent aggregate, and at
  `rolling_3m` two neighbouring windows overlap by two months. Compare periods; never sum them.

## Exit codes — recover, do not guess

| code | meaning | what to do |
|---|---|---|
| 0 | success, **including an empty result** | use the answer as it is |
| 1 | any other failure, including a mistake caught before the request was sent | read the message; fix and retry |
| 2 | money — quota spent, or the subscription is not active | stop. Tell the user. `investviews usage` is free and shows the refill date |
| 3 | credentials — no token, bad token, or read-only on a write | tell the user to run `auth login`; do not retry |
| 4 | **`not_covered`** — nothing is held for that country at all | retrying NEVER works. Pick another market; `investviews coverage` lists them |

### ⚠️ Do not conflate `not_covered` with `unknown_place`

They look alike and mean opposite things:

| | `not_covered` (exit 4) | `unknown_place` (exit 1) |
|---|---|---|
| what happened | the whole country is outside the data | nothing matched **inside** a market that IS served |
| example | `geo search springfield --country us` | `geo search springfield --country jp` |
| next move | pick another country | re-search with a different spelling, or browse down to it |

Verified live 2026-09-12. Retrying an exit 4 is guaranteed to fail again; retrying an exit 1 with a
better name often works.

A `429` is **never** a quota problem — it is a burst limit. Wait and retry.

## Command reference

| command | cost | what it does |
|---|---|---|
| `geo browse [--parent ID] [--level L] [--limit N] [--page N] [--all]` | free | walk the geography from the countries down |
| `geo search <name> [--country cc] [--level L] [--limit N]` | free | resolve a name to a `geo_id` |
| `geo lookup --h3 ID` or `--lat --lng [--res N]` | free | name the zones containing one cell or point |
| `geo hexes <geo_id> [--all] [--ids-only]` | free | list a place's H3 cells; `--ids-only` prints ids alone, for a pipe |
| `coverage [--country cc]` | free | which markets are served, and how fresh each is |
| `usage` | free | what this token has spent and has left |
| **`stats current --geo-id ID`** | **METERED** | figures for the newest built period |
| **`stats history --geo-id ID [--from YYYY-MM] [--to YYYY-MM]`** | **METERED** | the same figures, period by period |

`--json` on any command prints JSON on stdout and moves the `cost:` line to stderr, so stdout pipes
into `jq`. Prefer it when you need to read a field rather than show a table.

## Worked example — "what do flats cost in Russafa, Valencia?"

```
$ investviews geo search russafa --country es
1 hit(s) for "russafa".
Same-named places are told apart by the chain under each hit, never by the name.

1. Russafa — microzone, es
   geo_id: R14727511
   in:     l'Eixample (macrozone, R4231821) › València (city, R344953) › València / Valencia (province, R349000) › Comunitat Valenciana (region, R349043) › ES (country, es)
   prices, data in the current period — ask stats current

cost: FREE — group metadata, uncounted · 1 request
```

One hit, the chain confirms it is the Valencia one, and the availability line says the newest
period holds figures. Now spend the one metered call:

```
$ investviews stats current --geo-id R14727511
Russafa (microzone, es) — geo_id R14727511 · selector geo_id · grain geo
period 2026-08-01 (rolling_3m) · window 2026-06-01 → 2026-08-31 · as_of 2026-08-31 · fx 2026-07-15 · figures in USD
filters: real_estate_residential / buy · price filters are USD whatever the display currency is

1 row(s).

ID  NAME     LEVEL  MEDIAN USD/m²  MEDIAN PRICE  MEDIAN m²  P05/m²  P95/m²  NOTES
—   Russafa  —      5187           486162        95         2749    8100    —

cost: METERED — group current · 1 request · 149 of 100000 used this cycle
```

Two calls, one of them paid, and the citation is in the header line: period `2026-08-01`, window
`2026-06-01 → 2026-08-31`, as of `2026-08-31`, USD.

## Installing this plugin

```
/plugin marketplace add Investviews/investviews-cli
/plugin install investviews@investviews-cli
```

## More

- Endpoint-level notes and the quota groups: `docs/api.md` in this repository.
- The contract itself is served live and is the only copy that cannot drift:
  <https://api.investviews.ai/public/v1/openapi.yaml>
- Error reference: <https://docs.investviews.ai/errors.html>
