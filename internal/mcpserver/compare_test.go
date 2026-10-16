package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// placeReply is a one-row /stats/current answer for a named place.
func placeReply(geoID, name, currency, period string, perSqm float64) string {
	return fmt.Sprintf(`{"as_of":"2026-08-31","currency":%q,"period":%q,"granularity":"month",
 "window_start":"2026-08-01","window_end":"2026-08-31","fx_date":"2026-08-15",
 "territory":{"selector":"geo_id","hex_count":0,"geo_id":%q,"grain":"geo",
   "place":{"name":%q,"level":"city","country":"es"}},
 "filters":{"ad_type":"real_estate_residential","ad_sub_type":"buy","rooms":[],
   "units":{"size":"m2","price":"USD"},"note":""},
 "snapshot":{"as_of":"2026-08-01","built":"2026-09-05","earliest_period":"2024-03-01"},
 "stats":[{"h3":"","estimated":false,"low_confidence":false,"name":%q,"zone_level":"city",
   "median_price_per_sqm":%g,"median_price":285000,"median_area":92,
   "p05_price_per_sqm":1800,"p95_price_per_sqm":5200}],
 "availability":{"status":"ok","reason":null,"message":""}}`, currency, period, geoID, name, name, perSqm)
}

// emptyPlaceReply is a covered place with no figures for these filters.
func emptyPlaceReply(geoID, name string) string {
	return fmt.Sprintf(`{"as_of":"2026-08-31","currency":"EUR","period":"2026-08-01","granularity":"month",
 "territory":{"selector":"geo_id","hex_count":0,"geo_id":%q,"grain":"geo",
   "place":{"name":%q,"level":"city","country":"es"}},
 "filters":{"ad_type":"real_estate_residential","ad_sub_type":"buy","rooms":[],
   "units":{"size":"m2","price":"USD"},"note":""},
 "snapshot":{"as_of":"2026-08-01","built":"2026-09-05","earliest_period":"2024-03-01"},
 "stats":[],
 "availability":{"status":"no_data","reason":"below_minimum_sample",
   "message":"Too few listings to publish a figure.",
   "searched_from":"2025-09-01","earliest_nonempty_period":null,"latest_nonempty_period":null}}`, geoID, name)
}

const unknownPlaceBody = `{"error":"unknown_place","message":"No place with geo_id R999.",` +
	`"docs_url":"https://docs.investviews.ai/errors.html#unknown_place","geo_id":"R999"}`

const quotaBody = `{"error":"quota_exhausted","message":"The current allowance for this cycle is spent.",` +
	`"docs_url":"https://docs.investviews.ai/errors.html#quota_exhausted"}`

// byGeoID answers each place with its own handler, keyed by the geo_id the
// request named. A place with no entry is a test bug, so it is a 500.
func byGeoID(replies map[string]http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h, found := replies[r.URL.Query().Get("geo_id")]; found {
			h(w, r)
			return
		}
		writeBody(w, http.StatusInternalServerError, `{"error":"internal_error","message":"unexpected place"}`)
	}
}

func apiError(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		meteredHeaders(w)
		writeBody(w, status, body)
	}
}

// decodedRow is one entry of structuredContent.rows, as an agent reads it.
type decodedRow struct {
	GeoID             string   `json:"geo_id"`
	Name              string   `json:"name"`
	MedianPricePerSqm *float64 `json:"median_price_per_sqm"`
	MedianPrice       *float64 `json:"median_price"`
	MedianArea        *float64 `json:"median_area"`
	Granularity       string   `json:"granularity"`
	WindowStart       *string  `json:"window_start"`
	WindowEnd         *string  `json:"window_end"`
	Availability      *struct {
		Status string  `json:"status"`
		Reason *string `json:"reason"`
	} `json:"availability"`
	Error *struct {
		Code    string `json:"code"`
		Status  int    `json:"status"`
		Message string `json:"message"`
		// RetryAfterSeconds is the server's wait on a 429.
		RetryAfterSeconds *int `json:"retry_after_seconds"`
	} `json:"error"`
}

type decodedAnswer struct {
	Rows         []decodedRow `json:"rows"`
	MeteredCalls int          `json:"metered_calls"`
	Requests     int          `json:"requests"`
	Stopped      string       `json:"stopped"`
	NotAsked     []string     `json:"not_asked"`
}

func compareAnswerOf(t *testing.T, res any) decodedAnswer {
	t.Helper()
	var a decodedAnswer
	raw, err := json.Marshal(res)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := json.Unmarshal(raw, &a); err != nil {
		t.Fatalf("decode structuredContent: %v\n%s", err, raw)
	}
	return a
}

// ── the comparison ───────────────────────────────────────────────────────────

func TestComparePlacesAsksEachPlaceInOrder(t *testing.T) {
	session, fake := connectFake(t, byGeoID(map[string]http.HandlerFunc{
		"R1": meteredJSON(placeReply("R1", "València", "EUR", "2026-08-01", 3120)),
		"R2": meteredJSON(placeReply("R2", "Alicante", "EUR", "2026-08-01", 2450)),
		"R3": meteredJSON(placeReply("R3", "Málaga", "EUR", "2026-08-01", 4010)),
	}))

	res := callTool(t, session, "compare_places", map[string]any{
		"geo_ids":     []string{"R1", "R2", "R3"},
		"ad_sub_type": "buy",
		"rooms":       []string{"2"},
	})
	if res.IsError {
		t.Fatalf("compare_places failed: %s", textOf(t, res))
	}

	reqs := fake.recorded()
	if len(reqs) != 3 {
		t.Fatalf("sent %d requests, want 3", len(reqs))
	}
	for i, want := range []string{"R1", "R2", "R3"} {
		if reqs[i].Path != "/public/v1/stats/current" {
			t.Errorf("request %d path = %q", i, reqs[i].Path)
		}
		// Every place gets the same filters.
		wantQuery(t, reqs[i], map[string]string{"geo_id": want, "ad_sub_type": "buy", "rooms": "2"})
	}

	a := compareAnswerOf(t, res.StructuredContent)
	if len(a.Rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(a.Rows))
	}
	for i, want := range []struct {
		geoID, name string
		perSqm      float64
	}{{"R1", "València", 3120}, {"R2", "Alicante", 2450}, {"R3", "Málaga", 4010}} {
		row := a.Rows[i]
		if row.GeoID != want.geoID || row.Name != want.name {
			t.Errorf("row %d = %s %s, want %s %s", i, row.GeoID, row.Name, want.geoID, want.name)
		}
		if row.MedianPricePerSqm == nil || *row.MedianPricePerSqm != want.perSqm ||
			row.MedianPrice == nil || row.MedianArea == nil {
			t.Errorf("row %d figures = %+v, want the three core columns", i, row)
		}
		if row.Error != nil {
			t.Errorf("row %d has an error: %+v", i, row.Error)
		}
	}
	if a.MeteredCalls != 3 || a.Requests != 3 || a.Stopped != "" {
		t.Errorf("answer = metered %d, requests %d, stopped %q; want 3, 3, none", a.MeteredCalls, a.Requests, a.Stopped)
	}

	text := textOf(t, res)
	mustContain(t, text, "1. València (city, es) — geo_id R1 · period 2026-08-01 (month, 2026-08-01 → 2026-08-31): median asking price per m²: 3120 EUR · median asking price: 285000 EUR · median size: 92 m²")
	mustContain(t, text, "2. Alicante (city, es) — geo_id R2")
	mustContain(t, text, "3. Málaga (city, es) — geo_id R3")
	mustContain(t, text, "median of ASKING prices")
	mustContain(t, text, "cost: 3 metered request(s) for 3 place(s) asked (group current, 138 of 100000 used this cycle)")
	if strings.Index(text, "València") > strings.Index(text, "Alicante") ||
		strings.Index(text, "Alicante") > strings.Index(text, "Málaga") {
		t.Errorf("rows are not in the order asked:\n%s", text)
	}
}

// Places answering in different currencies are flagged: the figures are not
// comparable as printed.
func TestComparePlacesFlagsMixedCurrenciesAndPeriods(t *testing.T) {
	session, _ := connectFake(t, byGeoID(map[string]http.HandlerFunc{
		"R1": meteredJSON(placeReply("R1", "València", "EUR", "2026-08-01", 3120)),
		"R2": meteredJSON(placeReply("R2", "Kraków", "PLN", "2026-07-01", 14000)),
	}))

	res := callTool(t, session, "compare_places", map[string]any{"geo_ids": []string{"R1", "R2"}})
	if res.IsError {
		t.Fatalf("compare_places failed: %s", textOf(t, res))
	}
	text := textOf(t, res)
	mustContain(t, text, "different currencies (EUR, PLN)")
	mustContain(t, text, "different periods (2026-07-01, 2026-08-01)")
}

// ── refused before any request ───────────────────────────────────────────────

func TestComparePlacesRefusesTheWrongNumberOfPlaces(t *testing.T) {
	cases := []struct {
		name   string
		geoIDs []string
		want   string
	}{
		{"one place", []string{"R1"}, "takes 2 to 5 geo_ids, got 1"},
		{"six places", []string{"R1", "R2", "R3", "R4", "R5", "R6"}, "takes 2 to 5 geo_ids, got 6"},
		{"blanks do not count", []string{"R1", " ", ""}, "got 1"},
		{"a repeat", []string{"R1", "R2", "R1"}, `names "R1" twice`},
		{"a repeat in another case", []string{"es", "R2", "ES"}, `names "ES" twice`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			session, fake := connectFake(t, meteredJSON(currentReply))

			res := callTool(t, session, "compare_places", map[string]any{"geo_ids": tc.geoIDs})
			if !res.IsError {
				t.Fatalf("want isError, got: %s", textOf(t, res))
			}
			if fake.hits() != 0 {
				t.Errorf("sent %d requests, want 0 — a refused list must cost nothing", fake.hits())
			}
			mustContain(t, textOf(t, res), tc.want)
		})
	}
}

// ── per-place failures stay per place ────────────────────────────────────────

func TestComparePlacesUnknownPlaceIsARowNotAFailure(t *testing.T) {
	session, fake := connectFake(t, byGeoID(map[string]http.HandlerFunc{
		"R1":   meteredJSON(placeReply("R1", "València", "EUR", "2026-08-01", 3120)),
		"R999": apiError(http.StatusNotFound, unknownPlaceBody),
		"R3":   meteredJSON(placeReply("R3", "Málaga", "EUR", "2026-08-01", 4010)),
	}))

	res := callTool(t, session, "compare_places", map[string]any{"geo_ids": []string{"R1", "R999", "R3"}})
	if res.IsError {
		t.Fatalf("one unknown place must not fail the comparison: %s", textOf(t, res))
	}
	if fake.hits() != 3 {
		t.Errorf("sent %d requests, want 3 — a 404 does not stop the loop", fake.hits())
	}

	a := compareAnswerOf(t, res.StructuredContent)
	if len(a.Rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(a.Rows))
	}
	if a.Rows[0].MedianPricePerSqm == nil || a.Rows[2].MedianPricePerSqm == nil {
		t.Errorf("places 1 and 3 lost their figures: %+v", a.Rows)
	}
	bad := a.Rows[1]
	if bad.GeoID != "R999" || bad.Error == nil || bad.Error.Code != "unknown_place" || bad.Error.Status != 404 {
		t.Errorf("row 2 = %+v, want an unknown_place error row", bad)
	}
	if bad.MedianPricePerSqm != nil {
		t.Errorf("an error row carries a figure: %+v", bad)
	}
	if a.MeteredCalls != 2 || a.Requests != 3 {
		t.Errorf("metered %d, requests %d; want 2, 3", a.MeteredCalls, a.Requests)
	}

	text := textOf(t, res)
	mustContain(t, text, "2. geo_id R999: ERROR — No place with geo_id R999. [unknown_place]")
	mustContain(t, text, "cost: 2 metered request(s) for 3 place(s) asked")
	mustContain(t, text, "1 request(s) failed and are not counted here")
}

func TestComparePlacesEmptyPlaceGivesItsReason(t *testing.T) {
	session, _ := connectFake(t, byGeoID(map[string]http.HandlerFunc{
		"R1": meteredJSON(placeReply("R1", "València", "EUR", "2026-08-01", 3120)),
		"R2": meteredJSON(emptyPlaceReply("R2", "Xàtiva")),
	}))

	res := callTool(t, session, "compare_places", map[string]any{"geo_ids": []string{"R1", "R2"}})
	if res.IsError {
		t.Fatalf("an empty place is an answer, not a failure: %s", textOf(t, res))
	}
	a := compareAnswerOf(t, res.StructuredContent)
	empty := a.Rows[1]
	if empty.Error != nil || empty.MedianPricePerSqm != nil {
		t.Errorf("row 2 = %+v, want no error and no figure", empty)
	}
	if empty.Availability == nil || empty.Availability.Status != "no_data" ||
		empty.Availability.Reason == nil || *empty.Availability.Reason != "below_minimum_sample" {
		t.Errorf("row 2 availability = %+v, want no_data / below_minimum_sample", empty.Availability)
	}

	text := textOf(t, res)
	mustContain(t, text, "2. Xàtiva (city, es) — geo_id R2 · period 2026-08-01 (month): no figures — availability: no_data — below_minimum_sample: Too few listings to publish a figure.")
	mustContain(t, text, "What to do: too few listings in this whole area to publish a figure")
}

// ── stop only when the next call cannot succeed ──────────────────────────────

// ⚠️ A 402 on place 2 stops the loop — place 3 would be a 402 too — but the
// figures already paid for place 1 come back with it.
func TestComparePlacesQuotaStopsButKeepsPaidRows(t *testing.T) {
	session, fake := connectFake(t, byGeoID(map[string]http.HandlerFunc{
		"R1": meteredJSON(placeReply("R1", "València", "EUR", "2026-08-01", 3120)),
		"R2": apiError(http.StatusPaymentRequired, quotaBody),
		"R3": meteredJSON(placeReply("R3", "Málaga", "EUR", "2026-08-01", 4010)),
	}))

	res := callTool(t, session, "compare_places", map[string]any{"geo_ids": []string{"R1", "R2", "R3"}})
	if res.IsError {
		t.Fatalf("a paid row is in the answer, so it must not be isError: %s", textOf(t, res))
	}
	if fake.hits() != 2 {
		t.Errorf("sent %d requests, want 2 — place 3 must not be asked after a 402", fake.hits())
	}

	a := compareAnswerOf(t, res.StructuredContent)
	if len(a.Rows) != 2 {
		t.Fatalf("rows = %d, want 2 (place 1, then the error row for place 2)", len(a.Rows))
	}
	if a.Rows[0].MedianPricePerSqm == nil || *a.Rows[0].MedianPricePerSqm != 3120 {
		t.Errorf("row 1 lost its figure: %+v", a.Rows[0])
	}
	if a.Rows[1].Error == nil || a.Rows[1].Error.Code != "quota_exhausted" {
		t.Errorf("row 2 = %+v, want a quota_exhausted error row", a.Rows[1])
	}
	if a.Stopped != "quota_exhausted" || strings.Join(a.NotAsked, ",") != "R3" {
		t.Errorf("stopped %q, not_asked %v; want quota_exhausted, [R3]", a.Stopped, a.NotAsked)
	}
	if a.MeteredCalls != 1 {
		t.Errorf("metered_calls = %d, want 1", a.MeteredCalls)
	}

	text := textOf(t, res)
	mustContain(t, text, "STOPPED after place 2 of 3 (quota_exhausted)")
	mustContain(t, text, "Not asked: R3.")
	mustContain(t, text, "the `usage` tool")
	mustContain(t, text, "cost: 1 metered request(s) for 2 place(s) asked")
}

// A quota refusal on the FIRST place leaves nothing to quote: isError.
func TestComparePlacesNothingAnsweredIsAnError(t *testing.T) {
	session, fake := connectFake(t, apiError(http.StatusPaymentRequired, quotaBody))

	res := callTool(t, session, "compare_places", map[string]any{"geo_ids": []string{"R1", "R2"}})
	if !res.IsError {
		t.Fatalf("no place answered, want isError: %s", textOf(t, res))
	}
	if fake.hits() != 1 {
		t.Errorf("sent %d requests, want 1", fake.hits())
	}
	a := compareAnswerOf(t, res.StructuredContent)
	if len(a.Rows) != 1 || a.Rows[0].Error == nil {
		t.Errorf("rows = %+v, want the one error row", a.Rows)
	}
}

// A burst limit over the retry budget stops the loop too: the next place
// would land in the same window.
func TestComparePlacesRateLimitStops(t *testing.T) {
	var calls atomic.Int32
	session, fake := connectFake(t, func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			meteredJSON(placeReply("R1", "València", "EUR", "2026-08-01", 3120))(w, r)
			return
		}
		w.Header().Set("Retry-After", "60")
		meteredHeaders(w)
		writeBody(w, http.StatusTooManyRequests, `{"error":"rate_limited","message":"Too many requests.",`+
			`"docs_url":"d","retry_after":60,"limit":300,"group":"current"}`)
	})

	res := callTool(t, session, "compare_places", map[string]any{"geo_ids": []string{"R1", "R2", "R3"}})
	if res.IsError {
		t.Fatalf("one place answered, so the call is not an error: %s", textOf(t, res))
	}
	if fake.hits() != 2 {
		t.Errorf("sent %d requests, want 2", fake.hits())
	}
	a := compareAnswerOf(t, res.StructuredContent)
	if a.Stopped != "rate_limited" {
		t.Errorf("stopped = %q, want rate_limited", a.Stopped)
	}
	if strings.Join(a.NotAsked, ",") != "R3" {
		t.Errorf("not_asked = %v, want [R3]", a.NotAsked)
	}
	if len(a.Rows) != 2 || a.Rows[0].MedianPricePerSqm == nil || *a.Rows[0].MedianPricePerSqm != 3120 {
		t.Errorf("rows = %+v, want row 1 with its figure kept", a.Rows)
	}
	mustContain(t, textOf(t, res), "NOT your quota")
	if e := a.Rows[1].Error; e == nil || e.RetryAfterSeconds == nil || *e.RetryAfterSeconds != 60 {
		t.Errorf("row 2 error = %+v, want retry_after_seconds 60", e)
	}
}

// Two places with the same period date but different aggregation windows are
// flagged, and every row carries its window, in the text and in the payload.
func TestComparePlacesFlagsDifferentWindowsForOnePeriod(t *testing.T) {
	rolling := strings.Replace(placeReply("R2", "Xàtiva", "EUR", "2026-08-01", 2100),
		`"granularity":"month",
 "window_start":"2026-08-01"`, `"granularity":"rolling_3m",
 "window_start":"2026-06-01"`, 1)
	session, _ := connectFake(t, byGeoID(map[string]http.HandlerFunc{
		"R1": meteredJSON(placeReply("R1", "València", "EUR", "2026-08-01", 3120)),
		"R2": meteredJSON(rolling),
	}))

	res := callTool(t, session, "compare_places", map[string]any{"geo_ids": []string{"R1", "R2"}})
	if res.IsError {
		t.Fatalf("compare_places failed: %s", textOf(t, res))
	}
	text := textOf(t, res)
	mustContain(t, text, "geo_id R2 · period 2026-08-01 (rolling_3m, 2026-06-01 → 2026-08-31):")
	mustContain(t, text, "share a period but were aggregated over different windows "+
		"(2026-08-01 (month, 2026-08-01 → 2026-08-31); 2026-08-01 (rolling_3m, 2026-06-01 → 2026-08-31))")
	if strings.Contains(text, "different periods") {
		t.Errorf("the periods are the same, only the windows differ:\n%s", text)
	}

	a := compareAnswerOf(t, res.StructuredContent)
	r := a.Rows[1]
	if r.Granularity != "rolling_3m" || r.WindowStart == nil || *r.WindowStart != "2026-06-01" ||
		r.WindowEnd == nil || *r.WindowEnd != "2026-08-31" {
		t.Errorf("row 2 window = %q %v %v, want rolling_3m 2026-06-01 → 2026-08-31", r.Granularity, r.WindowStart, r.WindowEnd)
	}
}

// Places on the same window raise no window warning.
func TestComparePlacesSameWindowIsNotFlagged(t *testing.T) {
	session, _ := connectFake(t, byGeoID(map[string]http.HandlerFunc{
		"R1": meteredJSON(placeReply("R1", "València", "EUR", "2026-08-01", 3120)),
		"R2": meteredJSON(emptyPlaceReply("R2", "Xàtiva")),
	}))
	text := textOf(t, callTool(t, session, "compare_places", map[string]any{"geo_ids": []string{"R1", "R2"}}))
	if strings.Contains(text, "different windows") {
		t.Errorf("a place that sent no window is not a different window:\n%s", text)
	}
}

// A filter value the server refuses is shared by every place, so the loop
// stops instead of paying for the same refusal again.
func TestComparePlacesStopsOnARefusedFilter(t *testing.T) {
	session, fake := connectFake(t, apiError(http.StatusBadRequest, `{"error":"invalid_parameter",`+
		`"message":"rooms is not one of 1, 2, 3, 4, 5+.","parameter":"rooms","docs_url":"d"}`))

	res := callTool(t, session, "compare_places", map[string]any{
		"geo_ids": []string{"R1", "R2", "R3"}, "rooms": []string{"9"},
	})
	if fake.hits() != 1 {
		t.Errorf("sent %d requests, want 1", fake.hits())
	}
	a := compareAnswerOf(t, res.StructuredContent)
	if a.Stopped != "invalid_parameter" {
		t.Errorf("stopped = %q, want invalid_parameter", a.Stopped)
	}
	if strings.Join(a.NotAsked, ",") != "R2,R3" {
		t.Errorf("not_asked = %v, want [R2 R3]", a.NotAsked)
	}
}

// An error row's structuredContent message is in the tools' words, like its
// text: an agent reading the payload must not be told to use --geo-id.
func TestComparePlacesErrorRowMessageUsesToolWords(t *testing.T) {
	session, _ := connectFake(t, byGeoID(map[string]http.HandlerFunc{
		"R1": meteredJSON(placeReply("R1", "València", "EUR", "2026-08-01", 3120)),
		"R2": apiError(http.StatusBadRequest, `{"error":"invalid_parameter",`+
			"\"message\":\"Pass a --geo-id from `investviews geo search`.\",\"parameter\":\"geo_id\",\"docs_url\":\"d\"}"),
	}))

	res := callTool(t, session, "compare_places", map[string]any{"geo_ids": []string{"R1", "R2"}})
	a := compareAnswerOf(t, res.StructuredContent)
	if len(a.Rows) != 2 || a.Rows[1].Error == nil {
		t.Fatalf("rows = %+v, want an error row for R2", a.Rows)
	}
	msg := a.Rows[1].Error.Message
	if strings.Contains(msg, "--geo-id") || strings.Contains(msg, "investviews geo search") {
		t.Errorf("error message keeps the CLI's words: %q", msg)
	}
	mustContain(t, msg, "Pass a geo_id from `find_place`.")
}

// The filters are stated once for the whole comparison, not on every row:
// every place is sent the same ones.
func TestComparePlacesStatesTheFiltersOnce(t *testing.T) {
	session, _ := connectFake(t, byGeoID(map[string]http.HandlerFunc{
		"R1": meteredJSON(placeReply("R1", "València", "EUR", "2026-08-01", 3120)),
		"R2": meteredJSON(placeReply("R2", "Alicante", "EUR", "2026-08-01", 2450)),
	}))

	res := callTool(t, session, "compare_places", map[string]any{"geo_ids": []string{"R1", "R2"}})
	var payload struct {
		Filters *struct {
			AdSubType string `json:"ad_sub_type"`
		} `json:"filters"`
		Rows []map[string]any `json:"rows"`
	}
	structured(t, res, &payload)
	if payload.Filters == nil || payload.Filters.AdSubType != "buy" {
		t.Errorf("filters = %+v, want the applied filters once at the top", payload.Filters)
	}
	for i, row := range payload.Rows {
		if _, has := row["filters"]; has {
			t.Errorf("row %d repeats the filters", i)
		}
	}
	if n := strings.Count(textOf(t, res), "filters: "); n != 1 {
		t.Errorf("text states the filters %d times, want 1", n)
	}
}

// One deadline covers the whole comparison: a slow API stops the loop with
// timed_out instead of spending the limit again on every place.
func TestComparePlacesStopsAtTheToolDeadline(t *testing.T) {
	saved := toolTimeout
	toolTimeout = 100 * time.Millisecond
	t.Cleanup(func() { toolTimeout = saved })

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	session, fake := connectFake(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-release:
		}
	})

	start := time.Now()
	res := callTool(t, session, "compare_places", map[string]any{"geo_ids": []string{"R1", "R2", "R3"}})
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("compare_places took %v with a 100ms tool deadline", elapsed)
	}
	if !res.IsError {
		t.Fatalf("no place answered, want isError: %s", textOf(t, res))
	}
	if fake.hits() != 1 {
		t.Errorf("sent %d requests, want 1", fake.hits())
	}
	a := compareAnswerOf(t, res.StructuredContent)
	if a.Stopped != "timed_out" {
		t.Errorf("stopped = %q, want timed_out", a.Stopped)
	}
	mustContain(t, textOf(t, res), "ran out of its")
}

// ── announcement ─────────────────────────────────────────────────────────────

func TestComparePlacesIsAnnouncedMetered(t *testing.T) {
	session, _ := connectFake(t, nil)

	list, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	for _, tool := range list.Tools {
		if tool.Name != "compare_places" {
			continue
		}
		if !strings.HasPrefix(tool.Description, "METERED") {
			t.Errorf("description does not open with METERED: %q", tool.Description)
		}
		mustContain(t, tool.Description, "PER PLACE")
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
		if strings.Join(schema.Required, ",") != "geo_ids" {
			t.Errorf("required = %v, want [geo_ids]", schema.Required)
		}
		for _, name := range []string{"geo_ids", "ad_type", "ad_sub_type", "rooms", "currency", "min_price_usd"} {
			if _, found := schema.Properties[name]; !found {
				t.Errorf("schema has no %q property", name)
			}
		}
		for _, name := range []string{"geo_id", "h3", "lat"} {
			if _, found := schema.Properties[name]; found {
				t.Errorf("schema has the single-territory %q property", name)
			}
		}
		return
	}
	t.Fatal("compare_places is not in tools/list")
}

// Size and price ranges are applied at bin edges: each row keeps the bounds
// the server really selected, in structuredContent and in the text.
func TestComparePlacesKeepsBinApproximations(t *testing.T) {
	approx := `"bin_approximations":{"price_usd":{"requested":[null,300000],"effective":[null,312000]}},`
	withApprox := func(body string) string {
		return strings.Replace(body, `"stats":`, approx+` "stats":`, 1)
	}
	session, _ := connectFake(t, byGeoID(map[string]http.HandlerFunc{
		"R1": meteredJSON(withApprox(placeReply("R1", "València", "EUR", "2026-08-01", 3120))),
		"R2": meteredJSON(placeReply("R2", "Alicante", "EUR", "2026-08-01", 2450)),
	}))

	res := callTool(t, session, "compare_places", map[string]any{"geo_ids": []string{"R1", "R2"}, "max_price_usd": 300000})
	var payload struct {
		Rows []struct {
			BinApproximations map[string]json.RawMessage `json:"bin_approximations"`
		} `json:"rows"`
	}
	structured(t, res, &payload)
	if got := string(payload.Rows[0].BinApproximations["price_usd"]); !strings.Contains(got, "312000") {
		t.Errorf("row 1 bin_approximations = %q, want the effective bound 312000", got)
	}
	if payload.Rows[1].BinApproximations != nil {
		t.Errorf("row 2 sent no bin_approximations, got %v", payload.Rows[1].BinApproximations)
	}
	text := textOf(t, res)
	mustContain(t, text, `price_usd {"requested":[null,300000],"effective":[null,312000]}`)
	if n := strings.Count(text, "applied at bin edges"); n != 1 {
		t.Errorf("bin edge line printed %d times, want once (row 1 only):\n%s", n, text)
	}
}

// An empty place that names periods holding data shows them, with the API's
// note that size and price filters are not applied to those dates.
func TestComparePlacesEmptyPlaceNamesItsDataPeriods(t *testing.T) {
	body := strings.Replace(emptyPlaceReply("R2", "Xàtiva"),
		`"earliest_nonempty_period":null,"latest_nonempty_period":null`,
		`"earliest_nonempty_period":"2025-11-01","latest_nonempty_period":"2026-07-01","note":"Size and price filters are not applied to these dates."`, 1)
	session, _ := connectFake(t, byGeoID(map[string]http.HandlerFunc{
		"R1": meteredJSON(placeReply("R1", "València", "EUR", "2026-08-01", 3120)),
		"R2": meteredJSON(body),
	}))

	text := textOf(t, callTool(t, session, "compare_places", map[string]any{"geo_ids": []string{"R1", "R2"}}))
	mustContain(t, text, "   This segment has data from 2025-11-01 to 2026-07-01.")
	mustContain(t, text, "   Note: Size and price filters are not applied to these dates.")
}
