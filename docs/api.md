# The API behind this CLI

This page maps each `investviews` command onto the HTTP endpoint it calls, and records the few
API behaviours that decide how the CLI behaves. It is a guide, not a specification.

## ⚠️ The contract is served live, and that copy is the only authority

```sh
curl -s https://api.investviews.ai/public/v1/openapi.yaml
```

No copy of the contract is vendored in this repository, on purpose. A checked-in copy drifts
silently from the running API — the documentation site did exactly that once — and a stale copy that
looks authoritative is worse than none. **Fetch the document above whenever you need the contract**:
before a release, before adding a parameter, before believing anything on this page.

It is served unauthenticated, so you can read it before you have a token.

Everything below was derived from that served document and checked against the live API on
**2026-09-12**. Where the two disagreed, the live API is recorded and the disagreement is named.

## Commands and the endpoints they call

| command | endpoint | quota group | metered |
|---|---|---|---|
| `geo browse` | `GET /geo` | `metadata` | no |
| `geo search` | `GET /geo/search` | `metadata` | no |
| `geo lookup` | `GET /geo/lookup` | `metadata` | no |
| `geo hexes` | `GET /geo/{geo_id}/hexes` | `metadata` | no |
| `coverage` | `GET /coverage` | `metadata` | no |
| `usage` | `GET /usage` | `metadata` | no |
| `stats current` | `GET /stats/current` | `current` | **yes** |
| `stats history` | `GET /stats/history` | `history` | **yes** |

`GET /ping` and `GET /openapi.yaml` are also `metadata`; `/ping` is the only endpoint that answers
without a token. The CLI does not expose either.

The base URL is `https://api.investviews.ai/public/v1`, overridable with `INVESTVIEWS_API_URL`.

### Billing, in the contract's own words

`metadata` is free **by design, not by omission**: billing a client for resolving a place before it
can query one teaches it to guess ids, and a guessed id produces a confidently wrong answer. Free
does not mean unlimited — the per-minute burst limit still applies, and `X-Quota-Limit` reads
`uncounted` rather than a made-up number.

Measured live 2026-09-12, on a read-only token:

| group | cycle limit | burst, per minute |
|---|---|---|
| `current` | 100,000 | 300 |
| `history` | 10,000 | 60 |
| `reports` | — (charged per artifact) | 5 |
| `metadata` | uncounted | 120 |

The cycle limits come from the plan on the token and differ per account; read them from
`investviews usage`, never from this table.

### ⚠️ The response says what a call cost — this table only says what we expected

Every response carries `X-Quota-Group` and `X-Quota-Limit`, plus `X-Quota-Used` and `X-Quota-Reset`
on a metered one, and `X-RateLimit-Limit` / `X-RateLimit-Remaining` always. The CLI builds its
`cost:` line from those headers, and falls back to the group in the table above only when a response
carried no quota headers at all — which happens in exactly two cases: nginx answered at the edge
(Rails never ran), or a paged walk ended before any page came back. The fallback labels itself
`expected`.

Every response also carries `X-Request-Id`. Quote it when reporting a 500.

### ⚠️ One drift found on 2026-09-12

The served contract's billing table still marks `GET /stats/history` *"(not yet shipped)"*. It **is**
shipped: it answers 200 and bills to `X-Quota-Group: history` against a limit of 10,000. The CLI
follows the live behaviour. This is the reason the page you are reading does not vendor the
contract.

## Three paging models, one per endpoint

There is no single pagination scheme. Each is what the endpoint offers.

| endpoint | model | ceiling |
|---|---|---|
| `/geo` | `limit` + `page` (offset) | `limit` max 100, default 50 |
| `/geo/search` | `limit` only — **no page, no cursor** | server clamps at 50 |
| `/geo/{geo_id}/hexes` | keyset `cursor`, response carries `next_cursor` | `limit` max 500, default 500 |

Three consequences the CLI is built around:

- **`/geo` returns no paging metadata at all** — no total, no `has_more`, no page echo. `--all` must
  therefore page until a short or empty page comes back, and the total is unknowable in advance.
- **`/geo/search` cannot be paged**, so `--all` on it is meaningless; the CLI refuses it and says
  why. `limit=1000` is not an error — the server clamps to 50 and answers 200.
- **A hex cursor is scoped to its exact query.** Reusing one across a different query is
  `400 invalid_cursor`: your position is gone, restart the listing **without** a cursor. Retrying
  the same cursor never succeeds.

At the root of `/geo` (no `parent`) the API **ignores `limit`** and returns all 37 countries. That is
why the CLI's offset walker stops on any page that is not **exactly** the limit, short or long: a
page longer than the limit means the server ignored it, so the endpoint is not paging and there is no
page 2 to ask for. Written as the rule rather than as a root special case, it also covers the next
endpoint that behaves this way.

Hex ids come back as **decimal int64 strings** (`"613498076398616575"`), not `87…` hex strings.
`/stats/*` also accepts the canonical H3 form (`8839540ad1fffff`); the CLI accepts both, and before
it sends the request it checks each value against the **H3 bit layout** — the reserved bit, the cell
mode, the mode-dependent bits, the base cell (0..121), and all 15 digit slots against the
resolution. Anything not shaped that way is refused locally and costs nothing, which is what matters
for `--h3 -`, since it reads a pipe and a rendered listing tokenises into words that look like ids.

**One thing the local check cannot rule out**, deliberately: twelve base cells are pentagons and
some digit paths below them do not exist on the grid. Ruling that out needs the pentagon table, so a
structurally well-formed id naming an impossible pentagon path is sent and answered `400` by the
server. That is the only invalid id the CLI still spends a request on — the trade is deliberate,
because wrongly refusing a real cell would break `--h3` outright while a false accept costs one
request.

## Errors

Every failure — including a wrong path, a wrong method and an unparseable body — is the same JSON
envelope: `{"error", "message", "docs_url"}`, plus fields specific to the failure (`parameter`,
`request_id`, `did_you_mean`, `geo_id`, `retry_after`, `limit`, `group`, …). `error` is a stable
machine string you may branch on. `message` is a human sentence — never branch on it. Unknown keys
are forward compatibility, not an error.

| code | status | meaning | CLI exit |
|---|---|---|---|
| `invalid_token` | 401 | no usable credential: header absent, malformed, unknown or revoked | 3 |
| `read_only_token` | 403 | a read-only token on a write endpoint | 3 |
| `subscription_inactive` | 402 | the subscription owning this token is not active | 2 |
| `quota_exhausted` | 402 | this cycle's allowance for the group is spent | 2 |
| `not_covered` | 404 | no geographic data is held for that country at all | **4** |
| `unknown_place` | 404 | nothing matched, **inside** a market we do serve | 1 |
| `rate_limited` | 429 | too many this minute — honour `Retry-After` | 1 |
| `resolution_not_in_plan` | 403 | the H3 resolution is finer than the plan allows | 1 |
| `too_many_hexes` | 422 | the territory exceeds the per-request cell budget | 1 |
| `reports_unavailable_for_country` | 422 | the report pipeline does not serve that country | 1 |
| `missing_parameter` | 400 | a required parameter was not sent | 1 |
| `invalid_parameter` | 400 | a parameter was sent but its value is unusable | 1 |
| `invalid_cursor` | 400 | the cursor was not issued for this query | 1 |
| `not_found` | 404 | no such resource or path | 1 |
| `malformed_request` | 4xx | unprocessable as sent (method, body, headers) | 1 |
| `internal_error` | 500 | a fault on our side — retry, quote `request_id` | 1 |

### ⚠️ Two pairs the contract says must never be conflated

- **`not_covered` vs `unknown_place`.** `not_covered` means the whole country is outside the data —
  retrying never succeeds, pick another market. `unknown_place` means nothing matched inside a
  market that *is* served — try the `did_you_mean` suggestions, or browse down to the place. They
  share a status code and mean opposite things, which is why `not_covered` gets its own exit code.
  Verified live: `geo search springfield --country us` exits 4, `--country jp` exits 1.
- **`invalid_parameter` vs `invalid_cursor`.** The first means "fix the value and retry"; the second
  means "your position in the listing is gone, start over". A client that treats the second as the
  first retries the same rejected cursor forever.

### The two 429s

Both send `error: "rate_limited"` on purpose — the comment in nginx's config says a client needs one
branch for "slow down" regardless of which layer said it. **Branch on the extra fields, never on the
code.**

| | app layer (a real plan burst limit) | edge (nginx, per address) |
|---|---|---|
| body carries | `retry_after`, `limit`, `group` | none of them |
| headers | `Retry-After` + the full `X-RateLimit-*` and `X-Quota-*` block | `Retry-After: 1` only |
| what it is about | this account and endpoint group | the address, shared or not — Rails never ran |

The discriminator is the presence of `group`/`limit`/`retry_after` in the body, or of the quota
headers. **Neither is ever a quota problem** — that is `402 quota_exhausted`. On a 429 the quota
headers deliberately report the same numbers a 200 would, "so a 429 never looks like your allowance
changed".

## Behaviours that look like failures and are not

- **"No data" is a 200.** `GET /stats/current?geo_id=jp` answers with a full envelope and
  `"stats": []`. A covered place that held nothing in the window returns an empty array, not a 404.
- **A skipped level returns an empty list, with `parent` still present.**
  `GET /geo?parent=<city>&level=microzone` is 200 with `"results": []`, because a city's direct
  children are macrozones. Levels are skipped, not shifted.
- **`has_data: false` alongside `prices_available: true`** means too little in the *current*
  period. `/stats/history` may still answer. Live on 2026-09-12: Montenegro (`me`) had nothing in
  the current period, an empty `/stats/current`, and real figures in `/stats/history` for 2025-09
  and 2025-10.
  - ⚠️ **`has_data` replaced `current_period` on 2026-09-20.** The old field was a date — the
    period `/stats/current` would read — with `null` meaning the same as `has_data: false`. It was
    removed because `/stats` now picks the window per answer, from what the place holds *for the
    selection that asked*, so no single date on a `/geo` row could be promised. The CLI reads
    either field, so it works against a server on either side of the change.
  - `has_data` is **necessary, never sufficient**: it is measured over the whole place before your
    filters, and the API calls `false` conservative rather than authoritative.
- **An empty `/stats` answer carries `availability`** (since 2026-09-20): `status`, a `reason` from
  a closed vocabulary (`period_not_built`, `no_data_for_place`, `below_minimum_sample`,
  `no_data_for_selection`, `not_determined`), a `message`, and the earliest and latest periods that
  do hold data for the segment. `/stats/history` carries it on the envelope and a verdict on every
  point. `reason` is `null` exactly when `status` is `ok`. Treat an unknown reason as
  `not_determined`.
- **`ancestors` is `[]`, not `null`, at the root** — every country row carries an empty array. The
  schema permits `null` and the CLI models it, but a live sweep of 300 points across 12 countries
  never produced one.
- **`did_you_mean` on a search miss is an array of place objects** (`geo_id`, `name`, `level`,
  `country`, `ancestors`) when it is non-empty, and `[]` when it is not.

## One request, one territory

`/stats/*` takes exactly one of `geo_id`, `h3` or the `lat`/`lng`/`radius_km` circle. Two selectors
is `400 invalid_parameter` with a `selectors` array naming what was sent. The CLI refuses this
locally, before the request, so the mistake costs no quota.

Price filters are **always USD** — `min_price_usd` and `max_price_usd` — whatever `currency`
displays. Bare `min_price` / `max_price` are refused, not ignored: a filter in another currency would
silently select the wrong listings.

## Versioning promise

**v1 is additive-only.** Fields are added; they are never removed and never change type. New optional
parameters may appear. Treat unknown fields as forward compatibility — do not validate responses with
a closed schema.

A breaking change ships as `/public/v2` **alongside** `/public/v1`, never in place of it. When that
happens, v1 begins sending `Deprecation`, `Sunset` and `Link: …; rel="successor-version"` headers,
and keeps serving for at least six months after that first `Sunset`.

`info.version` in the contract is `v1` — it tracks the **route**, not a release of the application,
and does not move. It is therefore not a drift signal: re-fetch the document rather than comparing a
version string.
