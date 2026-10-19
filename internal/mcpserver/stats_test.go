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
 "snapshot":{"as_of":"2026-08-31","built":"monthly","earliest_period":"2024-03-01"},
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
 "snapshot":{"as_of":"2026-08-31","built":"monthly","earliest_period":"2024-03-01"},
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
	mustContain(t, text, "What to do: nothing matched this selection — wider filters")
	if strings.Contains(text, "the place holds data") {
		t.Errorf("no_data_for_selection must not claim the place holds data:\n%s", text)
	}
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
		"below_minimum_sample": "too few listings in this whole area",
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
			mustContain(t, text, "No period with data in the window the API searched (from 2025-09-01)")
			// The next call follows the reason. A null date covers only the
			// searched window, so no_data_for_place still points before it;
			// and neither it nor below_minimum_sample suggests wider filters.
			switch reason {
			case "no_data_for_place":
				mustContain(t, text, `Next: price_history with geo_id "R344953" (the API named no period with data in the window it searched; other periods may still hold some), or another place — find_place or browse_places.`)
				if strings.Contains(text, "Do not spend price_history") || strings.Contains(text, "or wider filters") {
					t.Errorf("no_data_for_place forbids history or suggests wider filters:\n%s", text)
				}
			case "below_minimum_sample":
				mustContain(t, text, `Next: price_history with geo_id "R344953", or a wider area (the parent place). Wider filters will not help`)
				if strings.Contains(text, "or wider filters") {
					t.Errorf("below_minimum_sample still suggests wider filters:\n%s", text)
				}
			default:
				mustContain(t, text, `Next: price_history with geo_id "R344953", or wider filters.`)
			}
		})
	}
}

// no_data_for_selection with no period named: wider filters first, and
// price_history without a bound — a null date covers only the searched
// window, never all history.
func TestMarketStatsEmptySelectionWithoutDataSuggestsFiltersThenEarlierHistory(t *testing.T) {
	body := strings.Replace(emptyReply, `"earliest_nonempty_period":"2025-11-01"`, `"earliest_nonempty_period":null`, 1)
	body = strings.Replace(body, `"latest_nonempty_period":"2026-07-01"`, `"latest_nonempty_period":null`, 1)
	session, _ := connectFake(t, meteredJSON(body))

	text := textOf(t, callTool(t, session, "market_stats", map[string]any{"geo_id": "R344953"}))
	mustContain(t, text, `Next: wider filters, or price_history with geo_id "R344953" (the API named no period with data in the window it searched; other periods may still hold some).`)
	if strings.Contains(text, "to before") {
		t.Errorf("a null date must not bound price_history to before searched_from (pooled res 7-8 figures are not covered):\n%s", text)
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

// ── price_history ────────────────────────────────────────────────────────────

// historyHeaders is what /stats/history sends: the "history" group, a budget
// separate from "current".
func historyHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Quota-Group", "history")
	w.Header().Set("X-Quota-Limit", "5000")
	w.Header().Set("X-Quota-Used", "12")
	w.Header().Set("X-Quota-Reset", "2026-10-11T12:30:25Z")
	w.Header().Set("X-Request-Id", "req-history")
}

func historyJSON(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		historyHeaders(w)
		writeBody(w, http.StatusOK, body)
	}
}

// historyReply is three periods of one place: two with a figure (one of them
// estimated and low confidence), one empty with its own verdict.
const historyReply = `{"currency":"EUR","resolution":null,
 "territory":{"selector":"geo_id","hex_count":0,"geo_id":"R344953","grain":"geo",
   "place":{"name":"València","level":"city","country":"es"}},
 "filters":{"ad_type":"real_estate_residential","ad_sub_type":"buy","rooms":[],
   "units":{"size":"m2","price":"USD"},"note":""},
 "series":[
  {"period":"2026-06-01","granularity":"month","window_start":"2026-06-01","window_end":"2026-06-30",
   "as_of":"2026-06-30","fx_date":"2026-06-15",
   "stats":[{"name":"València","estimated":false,"low_confidence":false,
     "median_price_per_sqm":3050,"median_price":280000,"median_area":91}],
   "availability":{"status":"ok","reason":null,"message":""}},
  {"period":"2026-07-01","granularity":"month","window_start":"2026-07-01","window_end":"2026-07-31",
   "as_of":"2026-07-31","fx_date":"2026-07-15",
   "stats":[{"name":"València","estimated":true,"low_confidence":true,
     "median_price_per_sqm":3090,"median_price":282500,"median_area":90.5}],
   "availability":{"status":"ok","reason":null,"message":""}},
  {"period":"2026-08-01","granularity":"month","window_start":"2026-08-01","window_end":"2026-08-31",
   "as_of":"2026-08-31","fx_date":"2026-08-15","stats":[],
   "availability":{"status":"no_data","reason":"below_minimum_sample","message":"Too few listings."}}],
 "series_note":"Each point is an independent aggregate; points are NOT additive.",
 "snapshot":{"as_of":"2026-08-31","built":"monthly","earliest_period":"2024-03-01"},
 "availability":{"status":"ok","reason":null,"message":"","searched_from":"2024-03-01",
   "earliest_nonempty_period":"2024-03-01","latest_nonempty_period":"2026-07-01"}}`

func TestPriceHistoryPassesTheRangeThrough(t *testing.T) {
	session, fake := connectFake(t, historyJSON(historyReply))

	res := callTool(t, session, "price_history", map[string]any{
		"geo_id": "R344953", "from": "2026-06", "to": "2026-08-15", "rooms": []string{"2"},
	})
	if res.IsError {
		t.Fatalf("price_history failed: %s", textOf(t, res))
	}
	if got := fake.last().Path; got != "/public/v1/stats/history" {
		t.Errorf("path = %q, want /public/v1/stats/history", got)
	}
	wantQuery(t, fake.last(), map[string]string{
		"geo_id": "R344953", "from": "2026-06", "to": "2026-08-15", "rooms": "2",
	})
}

// from and to omitted are not sent: the API's own default (the last 12
// periods) applies, and nothing local invents a range.
func TestPriceHistoryWithoutARangeSendsNone(t *testing.T) {
	session, fake := connectFake(t, historyJSON(historyReply))

	res := callTool(t, session, "price_history", map[string]any{"h3": []string{"871f1d489ffffff"}})
	if res.IsError {
		t.Fatalf("price_history failed: %s", textOf(t, res))
	}
	wantQuery(t, fake.last(), map[string]string{"h3": "871f1d489ffffff"})
}

func TestPriceHistoryGivesOneLinePerPeriod(t *testing.T) {
	session, _ := connectFake(t, historyJSON(historyReply))

	res := callTool(t, session, "price_history", map[string]any{"geo_id": "R344953"})
	if res.IsError {
		t.Fatalf("price_history failed: %s", textOf(t, res))
	}

	var payload struct {
		Series []struct {
			Period string            `json:"period"`
			Stats  []json.RawMessage `json:"stats"`
		} `json:"series"`
		SeriesNote string `json:"series_note"`
	}
	structured(t, res, &payload)
	if len(payload.Series) != 3 || payload.Series[2].Period != "2026-08-01" || len(payload.Series[2].Stats) != 0 {
		t.Errorf("structuredContent series = %+v, want the three periods as sent", payload.Series)
	}

	text := textOf(t, res)
	mustContain(t, text, "València (city, es) — geo_id R344953")
	mustContain(t, text, "3 period(s) · figures in EUR")
	mustContain(t, text, "median of ASKING prices")
	mustContain(t, text, "- 2026-06-01 (month, window 2026-06-01 → 2026-06-30, as_of 2026-06-30): "+
		"median asking price per m²: 3050 EUR · median asking price: 280000 EUR · median size: 91 m²\n")
	mustContain(t, text, "- 2026-07-01 (month, window 2026-07-01 → 2026-07-31, as_of 2026-07-31): "+
		"~ median asking price per m²: 3090 EUR · median asking price: 282500 EUR · median size: 90.5 m² · low confidence\n")
	mustContain(t, text, "- 2026-08-01 (month, window 2026-08-01 → 2026-08-31, as_of 2026-08-31): "+
		"no figures — availability: no_data — below_minimum_sample: Too few listings.\n")
	mustContain(t, text, "Rows marked ~ are NOT that cell's own figures")
	mustContain(t, text, "POINTS ARE NOT ADDITIVE")
	mustContain(t, text, "never sum them")
	mustContain(t, text, "points are NOT additive.")
	mustContain(t, text, "cost: 1 metered request (group history, 12 of 5000 used this cycle)")
}

// Each history point carries its own bin_approximations: the requested
// filters are not the bounds a figure describes. Bounds shared by every point
// are said once; bounds that differ are said under each period.
func TestPriceHistoryStatesBinApproximations(t *testing.T) {
	approx := `"bin_approximations":{"price_usd":{"requested":[0,300000],"effective":[0,312000]}},`
	shared := strings.ReplaceAll(historyReply, `"stats":[`, approx+`"stats":[`)
	session, _ := connectFake(t, historyJSON(shared))
	text := textOf(t, callTool(t, session, "price_history", map[string]any{"geo_id": "R344953"}))
	if n := strings.Count(text, `price_usd {"requested":[0,300000],"effective":[0,312000]}`); n != 1 {
		t.Errorf("shared bounds said %d times, want once:\n%s", n, text)
	}
	mustContain(t, text, "EFFECTIVE bounds")

	// Only the second period moved its bound: each period line says its own.
	other := `"bin_approximations":{"price_usd":{"requested":[0,300000],"effective":[0,295000]}},`
	mixed := strings.Replace(historyReply, `"stats":[`, approx+`"stats":[`, 1)
	i := strings.Index(mixed, `"period":"2026-07-01"`)
	mixed = mixed[:i] + strings.Replace(mixed[i:], `"stats":[`, other+`"stats":[`, 1)
	session, _ = connectFake(t, historyJSON(mixed))
	text = textOf(t, callTool(t, session, "price_history", map[string]any{"geo_id": "R344953"}))
	mustContain(t, text, "median size: 91 m²\n  ⚠️ size/price ranges are applied at bin edges, not exactly (requested → effective): "+
		`price_usd {"requested":[0,300000],"effective":[0,312000]}`)
	mustContain(t, text, "low confidence\n  ⚠️ size/price ranges are applied at bin edges, not exactly (requested → effective): "+
		`price_usd {"requested":[0,300000],"effective":[0,295000]}`)
}

// A period holding several cells has no single figure: the text says so and
// points at the payload instead of averaging the cells.
func TestPriceHistoryMultiCellPeriodIsNotAveraged(t *testing.T) {
	reply := `{"currency":"USD","territory":{"selector":"h3","hex_count":2,"grain":"h3"},
	 "filters":{"ad_type":"real_estate_residential","ad_sub_type":"buy"},
	 "series":[{"period":"2026-08-01","granularity":"rolling_3m","window_start":"2026-06-01",
	   "window_end":"2026-08-31","as_of":"2026-08-31","stats":[
	   {"h3":"871f1d489ffffff","median_price_per_sqm":3000},
	   {"h3":"871f1d48bffffff","median_price_per_sqm":4000}]}],
	 "series_note":"","snapshot":{}}`
	session, _ := connectFake(t, historyJSON(reply))

	res := callTool(t, session, "price_history", map[string]any{
		"h3": []string{"871f1d489ffffff", "871f1d48bffffff"},
	})
	if res.IsError {
		t.Fatalf("price_history failed: %s", textOf(t, res))
	}
	text := textOf(t, res)
	mustContain(t, text, "2 cells — no single figure for the period; per-cell figures in structuredContent.series[0].stats")
	if strings.Contains(text, "3500") {
		t.Errorf("text averaged the cells:\n%s", text)
	}
}

// A multi-cell territory whose periods each hold one row may hold a DIFFERENT
// cell in each: every line names its cell, and a warning says not to read
// the lines as one trend.
func TestPriceHistoryMultiCellSingleRowsNameTheirCell(t *testing.T) {
	reply := `{"currency":"USD","territory":{"selector":"point","hex_count":7,"grain":"h3"},
	 "filters":{"ad_type":"real_estate_residential","ad_sub_type":"buy"},
	 "series":[
	  {"period":"2026-07-01","granularity":"month","window_start":"2026-07-01","window_end":"2026-07-31",
	   "as_of":"2026-07-31","stats":[{"h3":"871f1d489ffffff","name":"Ruzafa","median_price_per_sqm":3000}]},
	  {"period":"2026-08-01","granularity":"month","window_start":"2026-08-01","window_end":"2026-08-31",
	   "as_of":"2026-08-31","stats":[{"h3":"871f1d48bffffff","name":"Benimaclet","median_price_per_sqm":4000}]}],
	 "series_note":"","snapshot":{}}`
	session, _ := connectFake(t, historyJSON(reply))

	res := callTool(t, session, "price_history", map[string]any{"lat": 39.47, "lng": -0.376, "radius_km": 2})
	if res.IsError {
		t.Fatalf("price_history failed: %s", textOf(t, res))
	}
	text := textOf(t, res)
	mustContain(t, text, "⚠️ SEVERAL CELLS")
	mustContain(t, text, "as_of 2026-07-31): cell Ruzafa (h3 871f1d489ffffff): median asking price per m²: 3000 USD")
	mustContain(t, text, "as_of 2026-08-31): cell Benimaclet (h3 871f1d48bffffff): median asking price per m²: 4000 USD")
}

// An empty series is a normal answer with the API's reason and the way back
// to periods that hold data.
func TestPriceHistoryEmptySeries(t *testing.T) {
	reply := `{"currency":"EUR","territory":{"selector":"geo_id","geo_id":"R344953","grain":"geo"},
	 "filters":{"ad_type":"real_estate_residential","ad_sub_type":"buy"},
	 "series":[],"series_note":"","snapshot":{"earliest_period":"2024-03-01"},
	 "availability":{"status":"no_data","reason":"period_not_built","message":"Not built yet.",
	   "searched_from":"2026-09-01","earliest_nonempty_period":null,"latest_nonempty_period":null}}`
	session, _ := connectFake(t, historyJSON(reply))

	res := callTool(t, session, "price_history", map[string]any{"geo_id": "R344953", "from": "2026-09"})
	if res.IsError {
		t.Fatalf("an empty series is a normal answer, got isError: %s", textOf(t, res))
	}
	text := textOf(t, res)
	mustContain(t, text, "No periods in this range. That is a normal answer")
	mustContain(t, text, "availability: no_data — period_not_built: Not built yet.")
	mustContain(t, text, "History goes back to 2024-03-01 — move from earlier, or drop it for the last 12 periods.")
}

// ⚠️ A range over the cap is refused with 422, not truncated. The tool error
// says what the API said AND what to do: split the range.
func TestPriceHistoryRangeOverTheCapSaysSplitIt(t *testing.T) {
	session, fake := connectFake(t, func(w http.ResponseWriter, _ *http.Request) {
		historyHeaders(w)
		writeBody(w, http.StatusUnprocessableEntity, `{"error":"invalid_parameter",`+
			`"message":"The range spans 30 periods; the cap is 24.","parameter":"from",`+
			`"docs_url":"https://docs.investviews.ai/errors.html#invalid_parameter"}`)
	})

	res := callTool(t, session, "price_history", map[string]any{
		"geo_id": "R344953", "from": "2024-01", "to": "2026-06",
	})
	if !res.IsError {
		t.Fatalf("a 422 must come back with isError, got: %s", textOf(t, res))
	}
	if fake.hits() != 1 {
		t.Errorf("hits = %d, want 1 (a 422 is not retried)", fake.hits())
	}
	text := textOf(t, res)
	mustContain(t, text, "The range spans 30 periods; the cap is 24.")
	mustContain(t, text, "https://docs.investviews.ai/errors.html#invalid_parameter")
	mustContain(t, text, "Split from..to into ranges of 24 periods or fewer and call once per range")
}

// The split note belongs to the range refusal alone: the cell budget's 422
// keeps its own hint, and is not told to split a range it never had.
func TestPriceHistoryCellBudgetIsNotARangeError(t *testing.T) {
	session, _ := connectFake(t, func(w http.ResponseWriter, _ *http.Request) {
		historyHeaders(w)
		writeBody(w, http.StatusUnprocessableEntity, `{"error":"too_many_hexes",`+
			`"message":"Too many cells.","docs_url":"d"}`)
	})

	res := callTool(t, session, "price_history", map[string]any{"lat": 39.47, "lng": -0.376, "radius_km": 50})
	if !res.IsError {
		t.Fatalf("a 422 must come back with isError, got: %s", textOf(t, res))
	}
	text := textOf(t, res)
	mustContain(t, text, "per-request cell budget")
	if strings.Contains(text, "Split from..to") {
		t.Errorf("cell-budget error was told to split the range:\n%s", text)
	}
}

func TestPriceHistoryRefusesTwoSelectorsWithoutARequest(t *testing.T) {
	session, fake := connectFake(t, historyJSON(historyReply))

	res := callTool(t, session, "price_history", map[string]any{
		"geo_id": "R344953", "h3": []string{"871f1d489ffffff"},
	})
	if !res.IsError {
		t.Fatalf("two selectors must be refused, got: %s", textOf(t, res))
	}
	if fake.hits() != 0 {
		t.Errorf("sent %d requests, want 0 — a refused selector costs nothing", fake.hits())
	}
	if text := textOf(t, res); strings.Contains(text, "--geo-id") || strings.Contains(text, "--h3") {
		t.Errorf("error text names CLI flags:\n%s", text)
	}
}

func TestPriceHistoryIsAnnouncedMetered(t *testing.T) {
	session, _ := connectFake(t, nil)

	list, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	for _, tool := range list.Tools {
		if tool.Name != "price_history" {
			continue
		}
		if !strings.HasPrefix(tool.Description, "METERED") || !strings.Contains(tool.Description, `"history"`) {
			t.Errorf("description must open with METERED and name the history group: %q", tool.Description)
		}
		if !strings.Contains(tool.Description, "NOT ADDITIVE") {
			t.Errorf("description does not say points are not additive: %q", tool.Description)
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
		want := []string{"ad_sub_type", "ad_type", "currency", "from", "geo_id", "h3", "lat", "lng",
			"max_price_usd", "max_size", "min_price_usd", "min_size", "radius_km", "res", "rooms", "to"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("properties = %v, want %v", got, want)
		}
		return
	}
	t.Fatal("price_history is not in tools/list")
}

// Any other 422 from /stats/history is the range refusal, even when the
// server names no parameter.
func TestPriceHistoryOther422SaysSplitIt(t *testing.T) {
	session, _ := connectFake(t, func(w http.ResponseWriter, _ *http.Request) {
		historyHeaders(w)
		writeBody(w, http.StatusUnprocessableEntity, `{"error":"invalid_parameter",`+
			`"message":"The range is too long.","docs_url":"d"}`)
	})

	res := callTool(t, session, "price_history", map[string]any{"geo_id": "R344953", "from": "2020-01"})
	if !res.IsError {
		t.Fatalf("a 422 must come back with isError, got: %s", textOf(t, res))
	}
	mustContain(t, textOf(t, res), "Split from..to")
}

// ⚠️ A 400 naming from is a bad value, not a long range: the agent must fix
// the date, not split a range that was never too long.
func TestPriceHistoryBadDateIsNotARangeError(t *testing.T) {
	session, _ := connectFake(t, func(w http.ResponseWriter, _ *http.Request) {
		historyHeaders(w)
		writeBody(w, http.StatusBadRequest, `{"error":"invalid_parameter",`+
			`"message":"from is not a date: 2026-13.","parameter":"from","docs_url":"d"}`)
	})

	res := callTool(t, session, "price_history", map[string]any{"geo_id": "R344953", "from": "2026-13"})
	if !res.IsError {
		t.Fatalf("a 400 must come back with isError, got: %s", textOf(t, res))
	}
	text := textOf(t, res)
	mustContain(t, text, "from is not a date: 2026-13.")
	if strings.Contains(text, "Split from..to") {
		t.Errorf("a bad date was told to split the range:\n%s", text)
	}
}

// A malformed cell id is refused locally, in the tool's words: no shell pipe,
// no CLI command, no request.
func TestStatsToolsRefuseABadCellInToolWords(t *testing.T) {
	for _, tool := range []string{"market_stats", "price_history"} {
		t.Run(tool, func(t *testing.T) {
			session, fake := connectFake(t, nil)
			res := callTool(t, session, tool, map[string]any{"h3": []string{"R1"}})
			if !res.IsError {
				t.Fatalf("a bad cell id must come back with isError, got: %s", textOf(t, res))
			}
			if fake.hits() != 0 {
				t.Errorf("sent %d requests, want 0", fake.hits())
			}
			text := textOf(t, res)
			mustContain(t, text, "place_at_point")
			mustContain(t, text, "cost no quota")
			if strings.Contains(text, "investviews") || strings.Contains(text, "--") {
				t.Errorf("error text names a CLI command or flag:\n%s", text)
			}
		})
	}
}

// no_data_for_place is about THIS period: when the availability names periods
// that hold data, price_history over them is the move — never wider filters.
func TestMarketStatsNoDataForPlaceWithDatesSuggestsHistory(t *testing.T) {
	body := strings.Replace(emptyReply, `"no_data_for_selection"`, `"no_data_for_place"`, 1)
	session, _ := connectFake(t, meteredJSON(body))

	text := textOf(t, callTool(t, session, "market_stats", map[string]any{"geo_id": "R344953"}))
	mustContain(t, text, "This segment has data from 2025-11-01 to 2026-07-01")
	mustContain(t, text, "in this period, under any filters")
	mustContain(t, text, `Next: price_history with geo_id "R344953" for the periods named above, or another place`)
	if strings.Contains(text, "Do not spend price_history") {
		t.Errorf("no_data_for_place with data periods still forbids price_history:\n%s", text)
	}
	if strings.Contains(text, "or wider filters") {
		t.Errorf("no_data_for_place must not suggest wider filters:\n%s", text)
	}
}

// The API's note on the availability dates — size and price filters are not
// applied to them — is printed beside the dates.
func TestMarketStatsAvailabilityNoteIsPrinted(t *testing.T) {
	body := strings.Replace(emptyReply, `"rooms":["5+"]},"note":""`,
		`"rooms":["5+"]},"note":"min_size, max_size, min_price_usd and max_price_usd are not applied to these dates."`, 1)
	session, _ := connectFake(t, meteredJSON(body))

	text := textOf(t, callTool(t, session, "market_stats", map[string]any{"geo_id": "R344953", "min_price_usd": 500000}))
	mustContain(t, text, "This segment has data from 2025-11-01 to 2026-07-01")
	mustContain(t, text, "Note: min_size, max_size, min_price_usd and max_price_usd are not applied to these dates.")
}

// market_stats states the effective bin bounds of a size or price range.
func TestMarketStatsStatesBinApproximations(t *testing.T) {
	body := strings.Replace(emptyReply, `"stats":[]`,
		`"bin_approximations":{"size_m2":{"requested":[50,100],"effective":[48,105]}}, "stats":[]`, 1)
	session, _ := connectFake(t, meteredJSON(body))

	text := textOf(t, callTool(t, session, "market_stats", map[string]any{"geo_id": "R344953", "min_size": 50, "max_size": 100}))
	mustContain(t, text, `size_m2 {"requested":[50,100],"effective":[48,105]}`)
	mustContain(t, text, "EFFECTIVE bounds")
}

// snapshot.as_of is ONE date for the whole range. The text says which: the
// newest period's window_end, not today and not every point's.
func TestPriceHistorySaysWhatSnapshotAsOfIs(t *testing.T) {
	session, _ := connectFake(t, historyJSON(historyReply))

	res := callTool(t, session, "price_history", map[string]any{"geo_id": "R344953"})
	if res.IsError {
		t.Fatalf("price_history failed: %s", textOf(t, res))
	}
	mustContain(t, textOf(t, res),
		"snapshot.as_of 2026-08-31 = the window_end of the newest period in this range, not today.")
}

// ⚠️ A range with no period gets snapshot: null from the API — there is no
// newest period to name. It must not come back out as an object of empty
// strings, which reads as a snapshot with no date.
func TestPriceHistoryNullSnapshotStaysAbsent(t *testing.T) {
	reply := `{"currency":"EUR","territory":{"selector":"geo_id","geo_id":"R344953","grain":"geo"},
	 "filters":{"ad_type":"real_estate_residential","ad_sub_type":"buy"},
	 "series":[],"series_note":"","snapshot":null,
	 "availability":{"status":"no_data","reason":"period_not_built","message":"Not built yet.",
	   "searched_from":"2026-09-01","earliest_nonempty_period":null,"latest_nonempty_period":null}}`
	session, _ := connectFake(t, historyJSON(reply))

	res := callTool(t, session, "price_history", map[string]any{"geo_id": "R344953", "from": "2026-09"})
	if res.IsError {
		t.Fatalf("an empty series is a normal answer, got isError: %s", textOf(t, res))
	}
	var payload map[string]json.RawMessage
	structured(t, res, &payload)
	if snap, has := payload["snapshot"]; has {
		t.Errorf("snapshot = %s, want it absent", snap)
	}
	text := textOf(t, res)
	mustContain(t, text, "No periods in this range. That is a normal answer")
	if strings.Contains(text, "snapshot.as_of") || strings.Contains(text, "History goes back to") {
		t.Errorf("text names a snapshot the server did not send:\n%s", text)
	}
}
