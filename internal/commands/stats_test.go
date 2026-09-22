package commands

import (
	"net/http"
	"strings"
	"testing"
)

const currentBody = `{"as_of":"2026-08-31","currency":"EUR","resolution":null,"period":"2026-08-01",
 "granularity":"month","window_start":"2026-08-01","window_end":"2026-08-31","fx_date":"2026-08-15",
 "territory":{"selector":"geo_id","resolution":null,"hex_count":0,"geo_id":"R344953","grain":"geo",
   "place":{"name":"València","level":"city","country":"es"}},
 "filters":{"ad_type":"real_estate_residential","ad_sub_type":"buy","rooms":[],
   "min_size":null,"max_size":null,"min_price_usd":null,"max_price_usd":null,
   "units":{"size":"m2","price":"USD"},"note":"Price filters are USD."},
 "snapshot":{"as_of":"2026-08-01","built":"2026-09-05","earliest_period":"2024-03-01"},
 "stats":[{"h3":"","estimated":false,"low_confidence":false,"name":"València","zone_level":"city",
   "median_price_per_sqm":3120,"median_price":285000,"median_area":92,
   "p05_price_per_sqm":1800,"p95_price_per_sqm":5200}]}`

// ⚠️ ONE TERRITORY PER REQUEST, refused LOCALLY. The point is not only the
// message: it is that nothing was sent, so the mistake cost no quota.
func TestStatsCurrentRefusesTwoSelectorsWithoutSpendingARequest(t *testing.T) {
	cases := [][]string{
		{"stats", "current", "--geo-id", "R344953", "--h3", "613498076398616575"},
		{"stats", "current", "--geo-id", "R344953", "--lat", "39.4", "--lng", "-0.3", "--radius-km", "5"},
		{"stats", "history", "--geo-id", "es", "--h3", "1,2"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			res := run(t, nil, args...)
			if res.err == nil {
				t.Fatal("two selectors must be refused")
			}
			if res.hits() != 0 {
				t.Errorf("a locally refused request sent %d requests; it must send none", res.hits())
			}
			contains(t, res.err.Error(), "a request names ONE territory")
			contains(t, res.err.Error(), "drop all but one")
			contains(t, res.err.Error(), "cost no quota")
		})
	}
}

func TestStatsCurrentRequiresATerritoryAndSaysHowToGetOne(t *testing.T) {
	res := run(t, nil, "stats", "current")
	if res.err == nil {
		t.Fatal("no selector must be refused")
	}
	if res.hits() != 0 {
		t.Errorf("sent %d requests", res.hits())
	}
	contains(t, res.err.Error(), "investviews geo search")
	contains(t, res.err.Error(), "both are free")
}

func TestStatsCurrentRefusesAHalfNamedCircle(t *testing.T) {
	res := run(t, nil, "stats", "current", "--lat", "39.4", "--lng", "-0.3")
	if res.err == nil {
		t.Fatal("a circle missing its radius must be refused")
	}
	contains(t, res.err.Error(), "all three of --lat, --lng and --radius-km")
	if res.hits() != 0 {
		t.Errorf("sent %d requests", res.hits())
	}
}

func TestStatsCurrentSendsEveryFilterItWasGiven(t *testing.T) {
	res := run(t, meteredJSON(currentBody),
		"stats", "current",
		"--h3", "613498076398616575,613498076402810879", "--h3", "613498076568485887",
		"--rooms", "2,3", "--ad-type", "real_estate_commercial", "--ad-sub-type", "rent",
		"--min-size", "40", "--max-size", "120",
		"--min-price-usd", "150000", "--max-price-usd", "400000",
		"--currency", "EUR")
	if res.err != nil {
		t.Fatalf("stats current: %v", res.err)
	}

	query := res.lastQuery()
	for _, want := range []string{
		"h3=613498076398616575%2C613498076402810879%2C613498076568485887",
		"rooms=2%2C3",
		"ad_type=real_estate_commercial",
		"ad_sub_type=rent",
		"min_size=40", "max_size=120",
		"min_price_usd=150000", "max_price_usd=400000",
		"currency=EUR",
	} {
		if !strings.Contains(query, want) {
			t.Errorf("query %q is missing %q", query, want)
		}
	}
	// ⚠️ res is refused alongside h3 by the client; it must not appear here
	// either, since the flag was never given.
	if strings.Contains(query, "res=") {
		t.Errorf("query %q sent a resolution nobody asked for", query)
	}
}

// --radius is an accepted spelling of --radius-km. The API's parameter keeps
// its unit in the name.
func TestStatsCurrentAcceptsRadiusAsAnAliasForRadiusKm(t *testing.T) {
	res := run(t, meteredJSON(currentBody),
		"stats", "current", "--lat", "39.4699", "--lng", "-0.3763", "--radius", "5")
	if res.err != nil {
		t.Fatalf("stats current: %v", res.err)
	}
	if !strings.Contains(res.lastQuery(), "radius_km=5") {
		t.Errorf("query = %q", res.lastQuery())
	}
}

func TestStatsCurrentReadsCellsFromStandardInput(t *testing.T) {
	stdin := strings.NewReader("613498076398616575\n613498076402810879\n\n")
	res := runWithStdin(t, meteredJSON(currentBody), stdin, "stats", "current", "--h3", "-")
	if res.err != nil {
		t.Fatalf("stats current: %v", res.err)
	}
	if !strings.Contains(res.lastQuery(), "h3=613498076398616575%2C613498076402810879") {
		t.Errorf("query = %q", res.lastQuery())
	}
}

// ⚠️ A free call and a metered one must not look alike, and the SERVER is what
// decides which happened.
func TestStatsCurrentMarksTheCallAsMeteredFromTheServersHeaders(t *testing.T) {
	res := run(t, meteredJSON(currentBody), "stats", "current", "--geo-id", "R344953")
	if res.err != nil {
		t.Fatalf("stats current: %v", res.err)
	}
	contains(t, res.stdout, "cost: METERED — group current · 1 request · 138 of 100000 used this cycle")
	missing(t, res.stdout, "cost: FREE")
}

func TestFreeCommandsAreMarkedFree(t *testing.T) {
	res := run(t, freeJSON(countriesBody), "geo", "browse")
	if res.err != nil {
		t.Fatalf("browse: %v", res.err)
	}
	contains(t, res.stdout, "cost: FREE — group metadata, uncounted · 1 request")
	missing(t, res.stdout, "METERED")
}

// Every text answer states the period, its window, the as_of date and the
// place, because those are what an agent has to cite.
func TestStatsCurrentStatesTheWindowAndThePlace(t *testing.T) {
	res := run(t, meteredJSON(currentBody), "stats", "current", "--geo-id", "R344953")
	if res.err != nil {
		t.Fatalf("stats current: %v", res.err)
	}
	contains(t, res.stdout, "València (city, es) — geo_id R344953")
	contains(t, res.stdout, "period 2026-08-01 (month) · window 2026-08-01 → 2026-08-31 · as_of 2026-08-31")
	contains(t, res.stdout, "filters: real_estate_residential / buy")
	contains(t, res.stdout, "price filters are USD whatever the display currency is")
	contains(t, res.stdout, "3120")
	contains(t, res.stdout, "285000")
}

// ⚠️ "No data" is a 200 with an empty array. It is a normal answer, exits 0,
// and names the next move — which is history, not despair.
func TestStatsCurrentEmptyIsANormalAnswer(t *testing.T) {
	body := `{"as_of":"2026-08-31","currency":"USD","period":"2026-08-01","granularity":"month",
	 "window_start":"2026-08-01","window_end":"2026-08-31","fx_date":"2026-08-15",
	 "territory":{"selector":"geo_id","hex_count":0,"geo_id":"jp","grain":"geo"},
	 "filters":{"ad_type":"real_estate_residential","ad_sub_type":"buy","rooms":[],
	   "units":{"size":"m2","price":"USD"},"note":""},
	 "snapshot":{"as_of":"2026-08-01","built":"2026-09-05","earliest_period":"2024-03-01"},
	 "stats":[]}`

	res := run(t, meteredJSON(body), "stats", "current", "--geo-id", "jp")
	if res.err != nil {
		t.Fatalf("an empty answer must not be an error, got: %v", res.err)
	}
	contains(t, res.stdout, "That is a NORMAL answer, not an")
	contains(t, res.stdout, "History goes back to 2024-03-01.")
	contains(t, res.stdout, "Next: investviews stats history --geo-id jp")
}

// ⚠️ Since 2026-09-20 the API says WHY an answer is empty. The CLI must print
// the API's reason — not its own old guess, which blamed the display floor
// even when the filters matched nothing or the period was not built yet — and
// the periods that DO hold data, which are the move that can succeed.
func TestStatsCurrentEmptyPrintsTheAPIsReasonAndTheDates(t *testing.T) {
	body := `{"as_of":"2026-08-31","currency":"USD","period":"2026-08-01","granularity":"month",
	 "window_start":"2026-08-01","window_end":"2026-08-31","fx_date":"2026-08-15",
	 "territory":{"selector":"geo_id","hex_count":0,"geo_id":"R344953","grain":"geo"},
	 "filters":{"ad_type":"real_estate_residential","ad_sub_type":"buy","rooms":["5+"],
	   "units":{"size":"m2","price":"USD"},"note":""},
	 "snapshot":{"as_of":"2026-08-01","built":"2026-09-05","earliest_period":"2024-03-01"},
	 "stats":[],
	 "availability":{"status":"no_data","reason":"no_data_for_selection",
	   "message":"This place holds data, but none matched your filters.",
	   "searched_from":"2025-09-01","earliest_nonempty_period":"2025-11-01",
	   "latest_nonempty_period":"2026-07-01",
	   "segment":{"ad_type":"real_estate_residential","ad_sub_type":"buy","rooms":["5+"]},"note":""}}`

	res := run(t, meteredJSON(body), "stats", "current", "--geo-id", "R344953")
	if res.err != nil {
		t.Fatalf("an empty answer must not be an error, got: %v", res.err)
	}
	contains(t, res.stdout, "Why: no_data_for_selection — This place holds data, but none matched your filters.")
	contains(t, res.stdout, "This segment has data from 2025-11-01 to 2026-07-01 (searched back to 2025-09-01).")
	if strings.Contains(res.stdout, "held nothing that met the") {
		t.Errorf("the old display-floor guess must not be printed beside the API's own reason:\n%s", res.stdout)
	}
}

// A history series whose every period is empty is a table of dashes; the
// reason is the only line in it an agent can act on.
func TestStatsHistoryAllEmptyPrintsTheReason(t *testing.T) {
	body := `{"currency":"USD","territory":{"selector":"geo_id","hex_count":0,"geo_id":"me"},
	 "filters":{"ad_type":"real_estate_residential","ad_sub_type":"buy","rooms":[],
	   "units":{"size":"m2","price":"USD"},"note":""},
	 "series":[{"period":"2026-07-01","granularity":"month","window_start":"2026-07-01",
	   "window_end":"2026-07-31","as_of":"2026-07-31","fx_date":"2026-07-15","stats":[],
	   "availability":{"status":"no_data","reason":"below_minimum_sample","message":"m"}}],
	 "series_note":"",
	 "snapshot":{"as_of":null,"built":"","earliest_period":"2024-03-01"},
	 "availability":{"status":"no_data","reason":"below_minimum_sample",
	   "message":"Too few listings in this window to publish a figure.",
	   "searched_from":"2026-07-01","earliest_nonempty_period":null,"latest_nonempty_period":null,
	   "segment":{"ad_type":"real_estate_residential","ad_sub_type":"buy","rooms":null},"note":""}}`

	res := run(t, meteredJSON(body), "stats", "history", "--geo-id", "me")
	if res.err != nil {
		t.Fatalf("history: %v", res.err)
	}
	contains(t, res.stdout, "Why: below_minimum_sample — Too few listings in this window to publish a figure.")
	if strings.Contains(res.stdout, "This segment has data from") {
		t.Errorf("no date was named, so no range may be printed:\n%s", res.stdout)
	}
}

// ⚠️ ABSENT IS NOT FALSE. An unstamped low_confidence says nothing either way,
// so it must never render as a clean bill of health.
func TestStatsCurrentDoesNotReadAnAbsentLowConfidenceAsConfident(t *testing.T) {
	body := strings.Replace(currentBody, `"low_confidence":false`, `"low_confidence":null`, 1)
	res := run(t, meteredJSON(body), "stats", "current", "--geo-id", "R344953")
	if res.err != nil {
		t.Fatalf("stats current: %v", res.err)
	}
	contains(t, res.stdout, "absent means UNSTAMPED")
}

func TestStatsCurrentFlagsEstimatedRows(t *testing.T) {
	body := strings.Replace(currentBody, `"estimated":false`, `"estimated":true`, 1)
	res := run(t, meteredJSON(body), "stats", "current", "--geo-id", "R344953")
	if res.err != nil {
		t.Fatalf("stats current: %v", res.err)
	}
	contains(t, res.stdout, "estimated")
	contains(t, res.stdout, "pooled from the")
}

// --json must leave stdout as nothing but the payload, so it pipes into jq.
func TestJSONModeKeepsTheCostLineOffStdout(t *testing.T) {
	res := run(t, meteredJSON(currentBody), "stats", "current", "--geo-id", "R344953", "--json")
	if res.err != nil {
		t.Fatalf("stats current --json: %v", res.err)
	}
	if !strings.HasPrefix(strings.TrimSpace(res.stdout), "{") {
		t.Errorf("stdout is not a JSON document:\n%s", res.stdout)
	}
	missing(t, res.stdout, "cost:")
	contains(t, res.stderr, "cost: METERED — group current")
	contains(t, res.stdout, `"median_price": 285000`)
}

// ─────────────────────────────────────────────────────────────────────────────
// stats history
// ─────────────────────────────────────────────────────────────────────────────

const historyBody = `{"currency":"EUR","resolution":8,
 "territory":{"selector":"geo_id","resolution":8,"hex_count":0,"geo_id":"R344953","grain":"geo",
   "place":{"name":"València","level":"city","country":"es"}},
 "filters":{"ad_type":"real_estate_residential","ad_sub_type":"buy","rooms":[],
   "units":{"size":"m2","price":"USD"},"note":""},
 "series":[
   {"period":"2025-02-01","granularity":"rolling_3m","window_start":"2024-12-01",
    "window_end":"2025-02-28","as_of":"2025-02-28","fx_date":"2025-01-15",
    "stats":[{"h3":"613498079267520511","estimated":false,"median_price":285000,
      "median_price_per_sqm":3120,"median_area":91}]},
   {"period":"2025-03-01","granularity":"rolling_3m","window_start":"2025-01-01",
    "window_end":"2025-03-31","as_of":"2025-03-31","fx_date":"2025-02-15",
    "stats":[{"h3":"613498079267520511","estimated":false,"median_price":291500,
      "median_price_per_sqm":3190,"median_area":92}]}],
 "series_note":"Each point is an independent aggregate over its own window; points are NOT additive.",
 "snapshot":{"as_of":"2025-03-01","built":"2025-04-05","earliest_period":"2024-03-01"}}`

func TestStatsHistoryPrintsTheSeriesAndRefusesToLookAdditive(t *testing.T) {
	res := run(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Quota-Group", "history")
		w.Header().Set("X-Quota-Limit", "20000")
		w.Header().Set("X-Quota-Used", "12")
		writeBody(w, http.StatusOK, historyBody)
	}, "stats", "history", "--geo-id", "R344953", "--from", "2025-02", "--to", "2025-03")

	if res.err != nil {
		t.Fatalf("stats history: %v", res.err)
	}
	contains(t, res.stdout, "2 period(s)")
	contains(t, res.stdout, "2025-02-01")
	contains(t, res.stdout, "2024-12-01 → 2025-02-28")
	contains(t, res.stdout, "POINTS ARE NOT ADDITIVE")
	contains(t, res.stdout, "points are NOT additive.")
	// history bills a SEPARATE budget from current, and the cost line says so.
	contains(t, res.stdout, "cost: METERED — group history")

	query := res.lastQuery()
	if !strings.Contains(query, "from=2025-02") || !strings.Contains(query, "to=2025-03") {
		t.Errorf("query = %q", query)
	}
}

// A period holding several cells has no single figure, and averaging them here
// would invent one.
func TestStatsHistoryDoesNotInventAFigureForAMultiCellPeriod(t *testing.T) {
	body := strings.Replace(historyBody,
		`"stats":[{"h3":"613498079267520511","estimated":false,"median_price":285000,
      "median_price_per_sqm":3120,"median_area":91}]`,
		`"stats":[{"h3":"1","estimated":false,"median_price":285000,"median_price_per_sqm":3120},
		          {"h3":"2","estimated":false,"median_price":310000,"median_price_per_sqm":3400}]`, 1)

	res := run(t, meteredJSON(body), "stats", "history", "--geo-id", "R344953")
	if res.err != nil {
		t.Fatalf("stats history: %v", res.err)
	}
	contains(t, res.stdout, "ROWS")
	contains(t, res.stdout, "no single figure per period")
	missing(t, res.stdout, "MEDIAN PRICE")
}

func TestStatsHistoryEmptyRangeIsANormalAnswer(t *testing.T) {
	body := `{"currency":"USD","territory":{"selector":"geo_id","hex_count":0,"geo_id":"es"},
	 "filters":{"ad_type":"real_estate_residential","ad_sub_type":"buy","rooms":[],
	   "units":{"size":"m2","price":"USD"},"note":""},
	 "series":[],"series_note":"",
	 "snapshot":{"as_of":null,"built":"","earliest_period":"2024-03-01"}}`

	res := run(t, meteredJSON(body), "stats", "history", "--geo-id", "es", "--from", "2020-01")
	if res.err != nil {
		t.Fatalf("an empty series must not be an error, got: %v", res.err)
	}
	contains(t, res.stdout, "No periods in this range. That is a NORMAL answer")
	contains(t, res.stdout, "History goes back to 2024-03-01")
}

// ─────────────────────────────────────────────────────────────────────────────
// --h3 is checked before it is spent
// ─────────────────────────────────────────────────────────────────────────────

// ⚠️ REAL CELLS, NOT A COUNTER. These are the first 20 ids `investviews geo
// hexes R344953 --ids-only` returned from production on 2026-09-14.
//
// Earlier versions of these tests built cell lists by adding 1, 2 or 2*i to a
// real id. Every one of those is a MALFORMED cell — the low bits are the
// unused digit slots, which must all be 7 — and the check only tolerated them
// because it read the mode bits and nothing else. A fixture that the product
// would refuse in production teaches a test nothing.
var realCells = []string{
	"613498076398616575", "613498076402810879", "613498076568485887", "613498076578971647",
	"613498076637691903", "613498076650274815", "613498076652371967", "613498076654469119",
	"613498076656566271", "613498076658663423", "613498076660760575", "613498076662857727",
	"613498076667052031", "613498076669149183", "613498076675440639", "613498076677537791",
	"613498076683829247", "613498076685926399", "613498076688023551", "613498076690120703",
}

// hexesListing is the EXACT shape `investviews geo hexes` prints for a reader,
// transcribed from a live run against València on 2026-09-13. It is the thing
// the help text used to tell people to pipe into `stats current --h3 -`, and
// every word of it arrived there looking like a cell id.
const hexesListing = `R344953 València (city, es) — 3 cell(s) at res 8, this page
prices — this endpoint does not publish a current period

613498076398616575
613498076402810879
613498076568485887

more cells: --cursor eyJrIjoxfQ (or --all to read every page)
⚠️ that cursor belongs to THIS query; do not use it against another place.

cost: FREE — group metadata, uncounted · 1 request
`

// ⚠️ THE DOCUMENTED PIPE, END TO END. Before this, the header's words went out
// as h3= values on the METERED endpoint: R344953, València, (city, es), —, 3,
// cell(s), at, 8 …
//
// The point of the test is not the message. It is `res.hits() != 0`: the whole
// mistake must be caught without a request leaving.
func TestStatsCurrentRefusesARenderedListingWithoutSpendingARequest(t *testing.T) {
	res := runWithStdin(t, meteredJSON(currentBody), strings.NewReader(hexesListing),
		"stats", "current", "--h3", "-")
	if res.err == nil {
		t.Fatal("a rendered listing is not a list of cells; it must be refused")
	}
	if res.hits() != 0 {
		t.Fatalf("a rendered listing sent %d METERED requests; it must send none", res.hits())
	}
	contains(t, res.err.Error(), `"R344953"`)
	contains(t, res.err.Error(), "is not an H3 cell id")
	contains(t, res.err.Error(), "cost no quota")
	contains(t, res.err.Error(), "--ids-only")
}

// The same pipe with --ids-only: what the help now shows, and it must reach
// the server with exactly the cells and nothing else.
func TestStatsCurrentAcceptsAnIDsOnlyListing(t *testing.T) {
	idsOnly := "613498076398616575\n613498076402810879\n613498076568485887\n"
	res := runWithStdin(t, meteredJSON(currentBody), strings.NewReader(idsOnly),
		"stats", "current", "--h3", "-")
	if res.err != nil {
		t.Fatalf("an --ids-only listing must pipe straight in: %v", res.err)
	}
	if res.hits() != 1 {
		t.Fatalf("hits = %d, want 1", res.hits())
	}
	want := "h3=613498076398616575%2C613498076402810879%2C613498076568485887"
	if !strings.Contains(res.lastQuery(), want) {
		t.Errorf("query %q is missing %q", res.lastQuery(), want)
	}
}

// ⚠️ ALL THREE INPUT ROUTES, not only the pipe. --h3 takes a repeated flag, a
// comma list and standard input, and a check that covered one of them would
// leave the other two open.
func TestStatsCurrentChecksEveryRouteIntoH3(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		stdin string
		token string
	}{
		{"repeated flag", []string{"stats", "current", "--h3", "613498076398616575", "--h3", "cell(s)"}, "", `"cell(s)"`},
		{"comma list", []string{"stats", "current", "--h3", "613498076398616575,València"}, "", `"València"`},
		{"standard input", []string{"stats", "current", "--h3", "-"}, "613498076398616575 at res 8\n", `"at"`},
		// A bare number is a decimal token and still not a cell — this is
		// the one a digits-only check would let through, and it comes
		// straight out of "3 cell(s)".
		{"a bare count from a header", []string{"stats", "current", "--h3", "3"}, "", `"3"`},
		{"a resolution from a header", []string{"stats", "history", "--h3", "8"}, "", `"8"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := runWithStdin(t, meteredJSON(currentBody), strings.NewReader(tc.stdin), tc.args...)
			if res.err == nil {
				t.Fatal("a non-cell token must be refused")
			}
			if res.hits() != 0 {
				t.Errorf("sent %d requests; a local refusal must send none", res.hits())
			}
			contains(t, res.err.Error(), tc.token)
		})
	}
}

// ⚠️ THE SAME THREE ROUTES, WITH AN ID THAT CARRIES THE CELL MODE. A rendered
// listing's words never do, so the mode test alone caught those — but it let
// through anything mode-shaped, and a mode-shaped non-cell reaches the METERED
// endpoint and earns a server 400 instead of a free local refusal.
//
// 8839540ad1ffffe is one bit off the real cell 8839540ad1fffff: its last
// unused digit is 6 rather than 7. 613498079194120192 is the decimal form of
// the same class of fault on a res-8 València cell.
func TestStatsCurrentChecksTheWHOLEIdOnEveryRouteNotJustTheModeBits(t *testing.T) {
	cases := []struct {
		name  string
		args  []string
		stdin string
		token string
	}{
		{"repeated flag, hex form", []string{"stats", "current", "--h3", "8839540ad1fffff", "--h3", "8839540ad1ffffe"}, "", `"8839540ad1ffffe"`},
		{"comma list, hex form", []string{"stats", "current", "--h3", "8839540ad1fffff,8839540ad1ffffe"}, "", `"8839540ad1ffffe"`},
		{"standard input, hex form", []string{"stats", "current", "--h3", "-"}, "8839540ad1ffffe\n", `"8839540ad1ffffe"`},
		{"comma list, decimal form", []string{"stats", "current", "--h3", "613498079194120192"}, "", `"613498079194120192"`},
		{"stats history takes the same check", []string{"stats", "history", "--h3", "8839540ad1ffffe"}, "", `"8839540ad1ffffe"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := runWithStdin(t, meteredJSON(currentBody), strings.NewReader(tc.stdin), tc.args...)
			if res.err == nil {
				t.Fatal("a mode-shaped non-cell must be refused locally, not paid for")
			}
			if res.hits() != 0 {
				t.Errorf("sent %d METERED requests; a local refusal must send none", res.hits())
			}
			contains(t, res.err.Error(), tc.token)
		})
	}
}

// A canonical H3 string is a documented, legal form on /stats/*, so the check
// must not narrow the CLI to decimal only.
func TestStatsCurrentAcceptsTheCanonicalH3Form(t *testing.T) {
	res := run(t, meteredJSON(currentBody), "stats", "current", "--h3", "8839540ad1fffff")
	if res.err != nil {
		t.Fatalf("a canonical H3 string is a legal cell id: %v", res.err)
	}
	if !strings.Contains(res.lastQuery(), "h3=8839540ad1fffff") {
		t.Errorf("the cell must go out verbatim, got %q", res.lastQuery())
	}
}

// ⚠️ THE CHECK MUST NOT BE RES-8-SHAPED. The API only ever emits res 8, so a
// bit test tuned to what a pipe carries would still pass every test here while
// silently refusing a coarse or fine cell a caller typed — and /stats/* takes
// any resolution. These are real cells from an independent H3 implementation
// (see internal/api/h3_test.go), one per corner of the range.
func TestStatsCurrentAcceptsCellsAwayFromRes8(t *testing.T) {
	for _, cell := range []string{
		"577481099093999615", // res 0, València
		"595483686443417599", // res 4, València
		"645023276585059220", // res 15, València
		"8009fffffffffff",    // res 0 on a pentagon base cell, canonical form
	} {
		t.Run(cell, func(t *testing.T) {
			res := run(t, meteredJSON(currentBody), "stats", "current", "--h3", cell)
			if res.err != nil {
				t.Fatalf("a real cell must be accepted whatever its resolution: %v", res.err)
			}
			if res.hits() != 1 {
				t.Fatalf("hits = %d, want 1", res.hits())
			}
		})
	}
}

// An empty pipe names no territory, but "name a territory" is not the useful
// answer: the territory WAS named, by a listing that turned out to be empty.
func TestStatsCurrentSaysWhenThePipeWasEmpty(t *testing.T) {
	res := runWithStdin(t, meteredJSON(currentBody), strings.NewReader("\n\n"),
		"stats", "current", "--h3", "-")
	if res.err == nil {
		t.Fatal("an empty pipe must be refused")
	}
	if res.hits() != 0 {
		t.Errorf("sent %d requests", res.hits())
	}
	contains(t, res.err.Error(), "found no cell ids")
	contains(t, res.err.Error(), "cost no quota")
}

// ─────────────────────────────────────────────────────────────────────────────
// the empty answer's next move must be valid for THIS selector
// ─────────────────────────────────────────────────────────────────────────────

// emptyStatsBody is an empty 200 whose territory echo is substituted per case.
func emptyStatsBody(territory string) string {
	return `{"as_of":"2026-08-31","currency":"USD","period":"2026-08-01","granularity":"month",
	 "window_start":"2026-08-01","window_end":"2026-08-31","fx_date":"2026-08-15",
	 "territory":` + territory + `,
	 "filters":{"ad_type":"real_estate_residential","ad_sub_type":"buy","rooms":[],
	   "units":{"size":"m2","price":"USD"},"note":""},
	 "snapshot":{"as_of":"2026-08-01","built":"2026-09-05","earliest_period":"2024-03-01"},
	 "stats":[]}`
}

// ⚠️ --res IS REFUSED ALONGSIDE BOTH --geo-id AND --h3, so an empty answer may
// only suggest it to the circle. It used to be suggested to everyone, and
// geo_id — the commonest empty case there is — was then told to run
// `stats current --geo-id … --res 6`, which fails locally with exit 1.
//
// The second half matters just as much: `investviews stats history` with no
// selector is refused too, and that is what a --h3 or circle answer used to
// print, because it built the hint from the response's geo_id and those
// answers carry none.
func TestEmptyStatsSuggestsOnlyMovesThisSelectorAccepts(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		territory string
		wantNext  string
		wantRes   bool
	}{
		{
			name:      "geo_id",
			args:      []string{"stats", "current", "--geo-id", "jp"},
			territory: `{"selector":"geo_id","hex_count":0,"geo_id":"jp","grain":"geo"}`,
			wantNext:  "investviews stats history --geo-id jp",
		},
		{
			name:      "h3",
			args:      []string{"stats", "current", "--h3", "613498076398616575,613498076402810879"},
			territory: `{"selector":"h3","resolution":8,"hex_count":2}`,
			wantNext:  "investviews stats history --h3 613498076398616575,613498076402810879",
		},
		{
			name:      "circle",
			args:      []string{"stats", "current", "--lat", "39.4699", "--lng", "-0.3763", "--radius-km", "5"},
			territory: `{"selector":"point","resolution":8,"hex_count":12,"center":{"lat":39.4699,"lng":-0.3763},"radius_km":5}`,
			wantNext:  "investviews stats history --lat 39.4699 --lng -0.3763 --radius-km 5",
			wantRes:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := run(t, meteredJSON(emptyStatsBody(tc.territory)), tc.args...)
			if res.err != nil {
				t.Fatalf("an empty answer is not an error: %v", res.err)
			}
			contains(t, res.stdout, "That is a NORMAL answer, not an")
			contains(t, res.stdout, tc.wantNext)
			if tc.wantRes {
				contains(t, res.stdout, "a coarser --res")
			} else {
				missing(t, res.stdout, "--res")
			}
			contains(t, res.stdout, "a wider filter set")
		})
	}
}

// Every command the hint prints must survive this CLI's own local validation.
// The test runs it rather than reading it: a hint is an instruction, and one
// that fails locally sends an agent into an error it did not cause.
func TestTheEmptyStatsHintIsACommandThatActuallyParses(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		territory string
	}{
		{"geo_id", []string{"stats", "current", "--geo-id", "jp"},
			`{"selector":"geo_id","hex_count":0,"geo_id":"jp","grain":"geo"}`},
		{"h3", []string{"stats", "current", "--h3", "613498076398616575"},
			`{"selector":"h3","resolution":8,"hex_count":1}`},
		{"circle", []string{"stats", "current", "--lat", "39.4699", "--lng", "-0.3763", "--radius-km", "5"},
			`{"selector":"point","resolution":8,"hex_count":12,"center":{"lat":39.4699,"lng":-0.3763},"radius_km":5}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := run(t, meteredJSON(emptyStatsBody(tc.territory)), tc.args...)
			if res.err != nil {
				t.Fatalf("stats current: %v", res.err)
			}
			suggested := hintCommand(t, res.stdout)
			replay := run(t, meteredJSON(historyBody), suggested...)
			if replay.err != nil {
				t.Fatalf("the hint printed %q, which this CLI then refuses: %v", suggested, replay.err)
			}
		})
	}
}

// hintCommand lifts the "Next: investviews …" command out of the rendered
// answer and turns it into argv, stopping at the first ", or".
func hintCommand(t *testing.T, out string) []string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "Next: investviews ") {
			continue
		}
		command := strings.TrimPrefix(line, "Next: investviews ")
		command, _, _ = strings.Cut(command, ", or")
		return strings.Fields(strings.TrimSuffix(command, "."))
	}
	t.Fatalf("no Next: line in:\n%s", out)
	return nil
}

// A long cell list is named by count rather than reprinted — but the hint must
// still not look like a command missing its selector.
func TestTheEmptyStatsHintNamesALongCellListByCount(t *testing.T) {
	res := run(t, meteredJSON(emptyStatsBody(`{"selector":"h3","resolution":8,"hex_count":20}`)),
		"stats", "current", "--h3", strings.Join(realCells, ","))
	if res.err != nil {
		t.Fatalf("stats current: %v", res.err)
	}
	contains(t, res.stdout, "--h3 <the same 20 cells>")
	missing(t, res.stdout, "--res")
}
