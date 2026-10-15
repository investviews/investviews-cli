package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
)

// currentReply is one place, one row, with the spread and an explicit
// low_confidence stamp: the plain success the text is built for.
const currentReply = `{"as_of":"2026-08-31","currency":"EUR","resolution":null,"period":"2026-08-01",
 "granularity":"month","window_start":"2026-08-01","window_end":"2026-08-31","fx_date":"2026-08-15",
 "territory":{"selector":"geo_id","resolution":null,"hex_count":0,"geo_id":"R344953","grain":"geo",
   "place":{"name":"València","level":"city","country":"es"}},
 "filters":{"ad_type":"real_estate_residential","ad_sub_type":"buy","rooms":[],
   "min_size":null,"max_size":null,"min_price_usd":null,"max_price_usd":null,
   "units":{"size":"m2","price":"USD"},"note":"Price filters are USD."},
 "snapshot":{"as_of":"2026-08-01","built":"2026-09-05","earliest_period":"2024-03-01"},
 "stats":[{"h3":"","estimated":false,"low_confidence":false,"name":"València","zone_level":"city",
   "median_price_per_sqm":3120,"median_price":285000,"median_area":92,
   "p05_price_per_sqm":1800,"p95_price_per_sqm":5200}],
 "availability":{"status":"ok","reason":null,"message":"",
   "searched_from":"2025-09-01","earliest_nonempty_period":"2024-03-01",
   "latest_nonempty_period":"2026-08-01"}}`

// emptyReply is a covered place that answered nothing for these filters —
// with the API's reason and the periods that DO hold data.
const emptyReply = `{"as_of":"2026-08-31","currency":"USD","period":"2026-08-01","granularity":"month",
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

// ── selectors and filters → query ────────────────────────────────────────────

// Each selector sends exactly its own query keys, and nothing unset goes out:
// an unsent filter takes the API's default, which the answer echoes.
func TestMarketStatsSendsEachSelector(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want map[string]string
	}{
		{"geo_id", map[string]any{"geo_id": "R344953"}, map[string]string{"geo_id": "R344953"}},
		{"h3", map[string]any{"h3": []string{"871f1d489ffffff", "871f1d48bffffff"}},
			map[string]string{"h3": "871f1d489ffffff,871f1d48bffffff"}},
		{"circle", map[string]any{"lat": 39.47, "lng": -0.376, "radius_km": 2, "res": 7},
			map[string]string{"lat": "39.47", "lng": "-0.376", "radius_km": "2", "res": "7"}},
		// 0 is a real coordinate and must be sent, not dropped as unset.
		{"circle at zero", map[string]any{"lat": 0, "lng": 0, "radius_km": 1},
			map[string]string{"lat": "0", "lng": "0", "radius_km": "1"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			session, fake := connectFake(t, meteredJSON(currentReply))

			res := callTool(t, session, "market_stats", tc.args)
			if res.IsError {
				t.Fatalf("market_stats failed: %s", textOf(t, res))
			}
			if got := fake.last().Path; got != "/public/v1/stats/current" {
				t.Errorf("path = %q, want /public/v1/stats/current", got)
			}
			wantQuery(t, fake.last(), tc.want)
		})
	}
}

func TestMarketStatsMapsEveryFilterToItsQueryName(t *testing.T) {
	session, fake := connectFake(t, meteredJSON(currentReply))

	res := callTool(t, session, "market_stats", map[string]any{
		"geo_id":        "R344953",
		"ad_type":       "real_estate_residential",
		"ad_sub_type":   "rent",
		"rooms":         []string{"2", "3"},
		"min_size":      50,
		"max_size":      120.5,
		"min_price_usd": 100000,
		"max_price_usd": 400000,
		"currency":      "EUR",
	})
	if res.IsError {
		t.Fatalf("market_stats failed: %s", textOf(t, res))
	}
	wantQuery(t, fake.last(), map[string]string{
		"geo_id":        "R344953",
		"ad_type":       "real_estate_residential",
		"ad_sub_type":   "rent",
		"rooms":         "2,3",
		"min_size":      "50",
		"max_size":      "120.5",
		"min_price_usd": "100000",
		"max_price_usd": "400000",
		"currency":      "EUR",
	})
}

// ── the answer ───────────────────────────────────────────────────────────────

func TestMarketStatsNamesTheStatisticAndTheCost(t *testing.T) {
	session, _ := connectFake(t, meteredJSON(currentReply))

	res := callTool(t, session, "market_stats", map[string]any{"geo_id": "R344953"})
	if res.IsError {
		t.Fatalf("market_stats failed: %s", textOf(t, res))
	}

	var payload struct {
		Period string `json:"period"`
		Stats  []struct {
			MedianPricePerSqm float64 `json:"median_price_per_sqm"`
		} `json:"stats"`
		Availability struct {
			Status string  `json:"status"`
			Reason *string `json:"reason"`
		} `json:"availability"`
	}
	structured(t, res, &payload)
	if payload.Period != "2026-08-01" || len(payload.Stats) != 1 || payload.Stats[0].MedianPricePerSqm != 3120 {
		t.Errorf("structuredContent = %+v, want the one-row payload", payload)
	}
	if payload.Availability.Status != "ok" || payload.Availability.Reason != nil {
		t.Errorf("availability = %+v, want ok with a null reason", payload.Availability)
	}

	text := textOf(t, res)
	mustContain(t, text, "València (city, es) — geo_id R344953 · selector geo_id · grain geo")
	mustContain(t, text, "period 2026-08-01 (month) · window 2026-08-01 → 2026-08-31 · as_of 2026-08-31 · figures in EUR (fx 2026-08-15)")
	mustContain(t, text, "filters: real_estate_residential / buy")
	mustContain(t, text, "price filters are USD whatever the display currency is")
	mustContain(t, text, "median of ASKING prices")
	mustContain(t, text, "median asking price per m²: 3120 EUR")
	mustContain(t, text, "median asking price: 285000 EUR")
	mustContain(t, text, "median size: 92 m²")
	mustContain(t, text, "p05–p95 asking price per m²: 1800 EUR – 5200 EUR")
	// Metered: the cost line comes from the quota headers.
	mustContain(t, text, "cost: 1 metered request (group current")
	// The row was stamped (false), so no "unstamped" footnote.
	if strings.Contains(text, "UNSTAMPED") {
		t.Errorf("a stamped row got the unstamped footnote:\n%s", text)
	}
}

// ⚠️ An estimated row is pooled from neighbours: the text marks it "~", and an
// absent low_confidence is said to be unstamped, never "confident".
func TestMarketStatsMarksEstimatesAndUnstampedRows(t *testing.T) {
	body := strings.Replace(currentReply, `"estimated":false,"low_confidence":false`,
		`"estimated":true,"low_confidence":null`, 1)
	session, _ := connectFake(t, meteredJSON(body))

	text := textOf(t, callTool(t, session, "market_stats", map[string]any{"geo_id": "R344953"}))
	mustContain(t, text, "- ~ València")
	mustContain(t, text, "~ estimated — pooled from this cell and its six neighbours")
	mustContain(t, text, "Absent means UNSTAMPED")
}

func TestMarketStatsSaysWhereLongRowListsContinue(t *testing.T) {
	var rows []string
	for i := 0; i < maxTextRows+3; i++ {
		rows = append(rows, `{"h3":"c","estimated":false,"median_price_per_sqm":1}`)
	}
	body := strings.Replace(currentReply, currentStatsJSON(), "["+strings.Join(rows, ",")+"]", 1)
	session, _ := connectFake(t, meteredJSON(body))

	res := callTool(t, session, "market_stats", map[string]any{"geo_id": "R344953"})
	text := textOf(t, res)
	mustContain(t, text, "28 row(s).")
	mustContain(t, text, "… 3 more row(s) in structuredContent.stats.")

	var payload struct {
		Stats []json.RawMessage `json:"stats"`
	}
	structured(t, res, &payload)
	if len(payload.Stats) != maxTextRows+3 {
		t.Errorf("structuredContent has %d rows, want all %d", len(payload.Stats), maxTextRows+3)
	}
}

// currentStatsJSON is the stats array inside currentReply, for tests that
// swap it out.
func currentStatsJSON() string {
	start := strings.Index(currentReply, `"stats":[`) + len(`"stats":`)
	end := strings.Index(currentReply[start:], `}],`) + start + 2
	return currentReply[start:end]
}

// ── availability ─────────────────────────────────────────────────────────────

// An empty answer is normal: the text gives the API's reason, the periods that
// do hold data, what to do about the reason, and a next call that carries THIS
// request's selector.
func TestMarketStatsEmptyGivesTheReasonAndTheDates(t *testing.T) {
	session, _ := connectFake(t, meteredJSON(emptyReply))

	res := callTool(t, session, "market_stats", map[string]any{"geo_id": "R344953", "rooms": []string{"5+"}})
	if res.IsError {
		t.Fatalf("an empty answer must not be an error: %s", textOf(t, res))
	}
	text := textOf(t, res)
	mustContain(t, text, "No figures for this selection in period 2026-08-01. That is a normal answer — not an error, and not a price of zero.")
	mustContain(t, text, "availability: no_data — no_data_for_selection: This place holds data, but none matched your filters.")
	mustContain(t, text, "This segment has data from 2025-11-01 to 2026-07-01 (searched back to 2025-09-01).")
	mustContain(t, text, "What to do: the place holds data, but not for these filters — widen them")
	mustContain(t, text, "History goes back to 2024-03-01.")
	mustContain(t, text, `Next: price_history with geo_id "R344953", or wider filters.`)
	// res is refused with geo_id, so it must never be suggested here.
	if strings.Contains(text, "coarser res") {
		t.Errorf("a geo_id answer suggests res, which the tool refuses:\n%s", text)
	}

	var payload struct {
		Availability struct {
			Reason  string          `json:"reason"`
			Segment json.RawMessage `json:"segment"`
		} `json:"availability"`
	}
	structured(t, res, &payload)
	if payload.Availability.Reason != "no_data_for_selection" || len(payload.Availability.Segment) == 0 {
		t.Errorf("availability lost in structuredContent: %+v", payload.Availability)
	}
}

// Every reason in the closed vocabulary leads to its own move, and an unknown
// one is read as not_determined, never as an error.
func TestMarketStatsEmptyMovePerReason(t *testing.T) {
	cases := map[string]string{
		"period_not_built":     "use price_history",
		"no_data_for_place":    "pick another place",
		"below_minimum_sample": "too few listings",
		"not_determined":       "could not say why",
		"brand_new_reason":     "could not say why",
	}
	for reason, want := range cases {
		t.Run(reason, func(t *testing.T) {
			body := strings.Replace(emptyReply, `"no_data_for_selection"`, `"`+reason+`"`, 1)
			body = strings.Replace(body, `"earliest_nonempty_period":"2025-11-01"`, `"earliest_nonempty_period":null`, 1)
			body = strings.Replace(body, `"latest_nonempty_period":"2026-07-01"`, `"latest_nonempty_period":null`, 1)
			session, _ := connectFake(t, meteredJSON(body))

			res := callTool(t, session, "market_stats", map[string]any{"geo_id": "R344953"})
			if res.IsError {
				t.Fatalf("an empty answer must not be an error: %s", textOf(t, res))
			}
			text := textOf(t, res)
			mustContain(t, text, "availability: no_data — "+reason)
			mustContain(t, text, want)
			// No date was named: no range may be printed, and the
			// searched window is said instead.
			if strings.Contains(text, "This segment has data from") {
				t.Errorf("no date was named, so no range may be printed:\n%s", text)
			}
			mustContain(t, text, "No period with data since 2025-09-01")
		})
	}
}

// For the circle, and only the circle, a coarser res is a move; the next call
// carries the same three numbers.
func TestMarketStatsEmptyCircleSuggestsACoarserRes(t *testing.T) {
	session, _ := connectFake(t, meteredJSON(emptyReply))

	text := textOf(t, callTool(t, session, "market_stats",
		map[string]any{"lat": 39.47, "lng": -0.376, "radius_km": 2}))
	mustContain(t, text, "Next: price_history with lat 39.47, lng -0.376, radius_km 2, or a coarser res, or wider filters.")
}

// A server older than the availability block still gets a normal empty
// answer, without inventing a reason.
func TestMarketStatsEmptyWithoutAvailability(t *testing.T) {
	body := emptyReply[:strings.Index(emptyReply, `,
 "availability"`)] + "}"
	session, _ := connectFake(t, meteredJSON(body))

	res := callTool(t, session, "market_stats", map[string]any{"h3": []string{"871f1d489ffffff"}})
	if res.IsError {
		t.Fatalf("an empty answer must not be an error: %s", textOf(t, res))
	}
	text := textOf(t, res)
	mustContain(t, text, "That is a normal answer")
	mustContain(t, text, `Next: price_history with h3 ["871f1d489ffffff"], or wider filters.`)
	if strings.Contains(text, "availability:") {
		t.Errorf("no availability was sent, so none may be printed:\n%s", text)
	}
}

// ── errors ───────────────────────────────────────────────────────────────────

// ⚠️ ONE TERRITORY PER REQUEST, refused before any request: the mistake costs
// no quota. The message names arguments and tools, not CLI flags.
func TestMarketStatsRefusesBadSelectorsWithoutARequest(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"two selectors", map[string]any{"geo_id": "R344953", "h3": []string{"871f1d489ffffff"}},
			"drop all but one"},
		{"no selector", map[string]any{"ad_sub_type": "rent"},
			"exactly one of h3, geo_id, or lat/lng/radius_km"},
		{"half a circle", map[string]any{"lat": 39.47, "lng": -0.376},
			"needs all three of lat, lng and radius_km"},
		{"res with geo_id", map[string]any{"geo_id": "R344953", "res": 7},
			"res is refused alongside geo_id"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			session, fake := connectFake(t, meteredJSON(currentReply))

			res := callTool(t, session, "market_stats", tc.args)
			if !res.IsError {
				t.Fatalf("want isError, got: %s", textOf(t, res))
			}
			if fake.hits() != 0 {
				t.Errorf("sent %d requests, want 0 — a refused selector must cost nothing", fake.hits())
			}
			text := textOf(t, res)
			mustContain(t, text, tc.want)
			for _, flag := range []string{"--geo-id", "--h3", "--lat", "--radius-km", "investviews geo"} {
				if strings.Contains(text, flag) {
					t.Errorf("error text names the CLI's %q:\n%s", flag, text)
				}
			}
		})
	}
}

func TestMarketStatsQuotaExhausted(t *testing.T) {
	session, fake := connectFake(t, func(w http.ResponseWriter, _ *http.Request) {
		meteredHeaders(w)
		writeBody(w, http.StatusPaymentRequired, `{"error":"quota_exhausted",`+
			`"message":"The current allowance for this cycle is spent.",`+
			`"docs_url":"https://docs.investviews.ai/errors.html#quota_exhausted"}`)
	})

	res := callTool(t, session, "market_stats", map[string]any{"geo_id": "R344953"})
	if !res.IsError {
		t.Fatalf("a 402 must come back with isError, got: %s", textOf(t, res))
	}
	if fake.hits() != 1 {
		t.Errorf("sent %d requests, want 1 (a 402 is not retried)", fake.hits())
	}
	text := textOf(t, res)
	mustContain(t, text, "The current allowance for this cycle is spent.")
	mustContain(t, text, "[quota_exhausted]")
	mustContain(t, text, "https://docs.investviews.ai/errors.html#quota_exhausted")
	mustContain(t, text, "the `usage` tool")
}

// ⚠️ A long Retry-After comes back at once as a rate-limit error the agent can
// read — never a tool call blocked for minutes.
func TestMarketStatsRateLimitReturnsWithinTheBudget(t *testing.T) {
	session, fake := connectFake(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "60")
		meteredHeaders(w)
		writeBody(w, http.StatusTooManyRequests, `{"error":"rate_limited","message":"Too many requests.",`+
			`"docs_url":"d","retry_after":60,"limit":300,"group":"current"}`)
	})

	res := callTool(t, session, "market_stats", map[string]any{"geo_id": "R344953"})
	if !res.IsError {
		t.Fatalf("a 429 over the retry budget must come back with isError, got: %s", textOf(t, res))
	}
	mustContain(t, textOf(t, res), "NOT your quota")
	if fake.hits() != 1 {
		t.Errorf("hits = %d, want 1 — a 60s wait is over the %v budget", fake.hits(), retryBudget)
	}
}

// A short Retry-After is inside the budget: waited out, retried, answered.
func TestMarketStatsRateLimitInsideTheBudgetIsRetried(t *testing.T) {
	// Handlers run on the httptest server's goroutines: count atomically.
	var calls atomic.Int32
	session, fake := connectFake(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "1")
			meteredHeaders(w)
			writeBody(w, http.StatusTooManyRequests, `{"error":"rate_limited","message":"Too many requests.",`+
				`"docs_url":"d","retry_after":1,"limit":300,"group":"current"}`)
			return
		}
		meteredJSON(currentReply)(w, r)
	})

	res := callTool(t, session, "market_stats", map[string]any{"geo_id": "R344953"})
	if res.IsError {
		t.Fatalf("a 1s Retry-After is inside the budget and must be retried: %s", textOf(t, res))
	}
	if fake.hits() != 2 {
		t.Errorf("hits = %d, want 2", fake.hits())
	}
}

// ── announcement ─────────────────────────────────────────────────────────────

// market_stats is announced METERED, read-only, and requires nothing in its
// schema — the selectors are alternatives, checked by the tool. The shared
// argument halves are flattened into one object an agent can read.
func TestMarketStatsIsAnnouncedMetered(t *testing.T) {
	session, _ := connectFake(t, nil)

	list, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	for _, tool := range list.Tools {
		if tool.Name != "market_stats" {
			continue
		}
		if !strings.HasPrefix(tool.Description, "METERED") {
			t.Errorf("description does not open with METERED: %q", tool.Description)
		}
		a := tool.Annotations
		if a == nil || !a.ReadOnlyHint || !a.IdempotentHint || a.OpenWorldHint == nil || !*a.OpenWorldHint {
			t.Errorf("annotations = %+v, want read-only, idempotent, open-world", a)
		}
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("marshal input schema: %v", err)
		}
		var schema struct {
			Required   []string                   `json:"required"`
			Properties map[string]json.RawMessage `json:"properties"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("decode input schema: %v", err)
		}
		if len(schema.Required) != 0 {
			t.Errorf("requires %v, want nothing required", schema.Required)
		}
		var got []string
		for name := range schema.Properties {
			got = append(got, name)
		}
		sort.Strings(got)
		want := []string{"ad_sub_type", "ad_type", "currency", "geo_id", "h3", "lat", "lng",
			"max_price_usd", "max_size", "min_price_usd", "min_size", "radius_km", "res", "rooms"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("properties = %v, want %v", got, want)
		}
		return
	}
	t.Fatal("market_stats is not in tools/list")
}
