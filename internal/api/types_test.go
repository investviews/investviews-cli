package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// ⚠️ ancestors: null and ancestors: [] are DIFFERENT ANSWERS.
//
//	[]   — a country at the root: STOP walking up.
//	null — we hold no place for that zone: TRY A DIFFERENT SELECTOR.
//
// A plain []Ancestor collapses both to a nil slice.
func TestAncestorsKeepsNullApartFromEmpty(t *testing.T) {
	var empty Ancestors
	if err := json.Unmarshal([]byte(`[]`), &empty); err != nil {
		t.Fatalf("[]: %v", err)
	}
	if !empty.Known() {
		t.Error("[] is a known answer")
	}
	if !empty.Root() {
		t.Error("[] means the place has no ancestors")
	}
	if empty.Len() != 0 {
		t.Errorf("Len = %d", empty.Len())
	}

	var null Ancestors
	if err := json.Unmarshal([]byte(`null`), &null); err != nil {
		t.Fatalf("null: %v", err)
	}
	if null.Known() {
		t.Error("null means we cannot say, not that the answer is known")
	}
	if null.Root() {
		t.Error("null must NOT read as the root answer — that is the whole trap")
	}

	// The two must not be equal in any way a caller might test.
	if empty.Known() == null.Known() {
		t.Error("Known() cannot tell [] from null")
	}

	var absent Ancestors // key missing from the payload: UnmarshalJSON never runs
	if absent.Known() || absent.Root() {
		t.Error("an absent key reads as unknown, like null")
	}
}

func TestAncestorsDecodesTheEntriesAndTheirIDs(t *testing.T) {
	var a Ancestors
	body := `[{"geo_id":"R349000","name":"València / Valencia","level":"province"},
	          {"geo_id":"es","name":"ES","level":"country"}]`
	if err := json.Unmarshal([]byte(body), &a); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !a.Known() || a.Root() {
		t.Error("a populated ancestry is known and is not the root")
	}
	if a.Len() != 2 || a.List()[0].GeoID != "R349000" {
		t.Fatalf("List = %+v", a.List())
	}
	country, ok := a.Country()
	if !ok || country.GeoID != "es" {
		t.Errorf("Country = %+v, %v — the chain ends on the country", country, ok)
	}
}

// ⚠️ ancestors was a comma-separated STRING until 2026-09-10. A client that
// types it as a string compiles, runs, and silently loses every id, so a bare
// string must be a DECODE ERROR here.
func TestAncestorsRejectsTheOldCommaSeparatedString(t *testing.T) {
	var a Ancestors
	err := json.Unmarshal([]byte(`"València, Comunitat Valenciana, es"`), &a)
	if err == nil {
		t.Fatal("a bare string must not decode: that is the pre-2026-09-10 shape")
	}
	if !strings.Contains(err.Error(), "array") {
		t.Errorf("the error should say an array is expected: %v", err)
	}
	if a.Known() {
		t.Error("a failed decode must not leave the value looking known")
	}
}

func TestAncestorsRoundTripsThroughJSON(t *testing.T) {
	cases := map[string]Ancestors{
		`null`: UnknownAncestors(),
		`[]`:   NewAncestors(nil),
		`[{"geo_id":"es","name":"ES","level":"country"}]`: NewAncestors([]Ancestor{
			{GeoID: "es", Name: "ES", Level: "country"},
		}),
	}
	for want, value := range cases {
		got, err := json.Marshal(value)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if string(got) != want {
			t.Errorf("marshal = %s, want %s", got, want)
		}
	}
}

// The asymmetry is the trap: /geo and /geo/search are NEVER null; it is null
// only inside a /geo/lookup point's zones[]. Both branches are exercised here,
// and the null one has to be a synthetic fixture — live sampling of 21 city
// points and 300 random points produced zero nulls.
func TestAncestorsNullOnlyAppearsOnALookupZone(t *testing.T) {
	rec := newClient(t, func(w http.ResponseWriter, r *http.Request) {
		freeHeaders(w)
		switch {
		case strings.HasSuffix(r.URL.Path, "/geo/lookup"):
			writeJSON(w, 200, `{
			  "kind":"point","lat":39.4699,"lng":-0.3763,"h3":"613498079267520511","h3_res":8,
			  "source":"h3","country":"es","display_name":null,
			  "zones":[
			    {"level":"city","name":"València","geo_id":"R344953","current_period":"2026-08-01",
			     "hexes_url":"/public/v1/geo/R344953/hexes",
			     "ancestors":[{"geo_id":"es","name":"ES","level":"country"}]},
			    {"level":"microzone","name":"Some unmapped zone","geo_id":null,
			     "current_period":null,"hexes_url":null,"ancestors":null}
			  ],
			  "prices_available":true,"reports_available":true}`)
		case strings.HasSuffix(r.URL.Path, "/geo/search"):
			writeJSON(w, 200, `{"query":"valencia","results":[
			  {"kind":"zone","geo_id":"R344953","name":"València","level":"city","country":"es",
			   "ancestors":[{"geo_id":"es","name":"ES","level":"country"}],
			   "h3_res":8,"hexes_url":"/public/v1/geo/R344953/hexes",
			   "prices_available":true,"current_period":"2026-08-01","reports_available":true}]}`)
		default:
			writeJSON(w, 200, `{"results":[
			  {"kind":"country","geo_id":"es","name":"ES","level":"country","country":"es",
			   "ancestors":[],"prices_available":true,"current_period":"2026-08-01"}]}`)
		}
	})
	ctx := context.Background()

	// /geo root: [] — known, and the root.
	geo, err := rec.client.GeoBrowse(ctx, GeoParams{})
	if err != nil {
		t.Fatalf("GeoBrowse: %v", err)
	}
	if !geo.Results[0].Ancestors.Known() || !geo.Results[0].Ancestors.Root() {
		t.Error("a country row carries [] — known, and the root")
	}

	// /geo/search: never null.
	search, err := rec.client.GeoSearch(ctx, SearchParams{Q: "valencia"})
	if err != nil {
		t.Fatalf("GeoSearch: %v", err)
	}
	if !search.Results[0].Ancestors.Known() {
		t.Error("/geo/search is never null")
	}

	// /geo/lookup: this is where null lives.
	point, err := rec.client.GeoLookup(ctx, LookupParams{H3: "613498079267520511"})
	if err != nil {
		t.Fatalf("GeoLookup: %v", err)
	}
	if len(point.Zones) != 2 {
		t.Fatalf("zones = %d", len(point.Zones))
	}
	if !point.Zones[0].Ancestors.Known() || !point.Zones[0].Addressable() {
		t.Error("the mapped zone is known and addressable")
	}
	if point.Zones[1].Ancestors.Known() {
		t.Error("the unmapped zone's ancestors are null: we cannot say")
	}
	if point.Zones[1].Ancestors.Root() {
		t.Error("null must not read as the root answer")
	}
	if point.Zones[1].Addressable() {
		t.Error("a zone with geo_id null carries no id to spend")
	}
}

// ⚠️ current_period null means "nothing in the CURRENT window", NOT "no data
// ever" — /stats/history may still answer. Absent is a third state again.
func TestCurrentPeriodKeepsNullApartFromAbsent(t *testing.T) {
	var withDate struct {
		CurrentPeriod NullableDate `json:"current_period"`
	}
	if err := json.Unmarshal([]byte(`{"current_period":"2026-08-01"}`), &withDate); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !withDate.CurrentPeriod.Present() || !withDate.CurrentPeriod.Valid() {
		t.Error("a date is present and valid")
	}
	if withDate.CurrentPeriod.Value() != "2026-08-01" {
		t.Errorf("Value = %q", withDate.CurrentPeriod.Value())
	}

	var null struct {
		CurrentPeriod NullableDate `json:"current_period"`
	}
	if err := json.Unmarshal([]byte(`{"current_period":null}`), &null); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !null.CurrentPeriod.Present() {
		t.Error("an explicit null was present in the payload")
	}
	if null.CurrentPeriod.Valid() {
		t.Error("null carries no date")
	}
	if !null.CurrentPeriod.IsNull() {
		t.Error("IsNull must separate the explicit null from an absent key")
	}

	var absent struct {
		CurrentPeriod NullableDate `json:"current_period"`
	}
	if err := json.Unmarshal([]byte(`{}`), &absent); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if absent.CurrentPeriod.Present() {
		t.Error("an absent key is not present")
	}
	if absent.CurrentPeriod.IsNull() {
		t.Error("absent is not the same answer as null")
	}
}

func TestQueryableReadsPricesAvailableBesideCurrentPeriod(t *testing.T) {
	cases := []struct {
		name  string
		row   GeoResult
		query bool
	}{
		{"prices and a window", GeoResult{PricesAvailable: true, CurrentPeriod: Date("2026-08-01")}, true},
		{"prices, nothing this period", GeoResult{PricesAvailable: true, CurrentPeriod: NullDate()}, false},
		{"dead end", GeoResult{PricesAvailable: false, CurrentPeriod: NullDate()}, false},
		{"has_data true", GeoResult{PricesAvailable: true, HasData: boolPtr(true)}, true},
		{"has_data false", GeoResult{PricesAvailable: true, HasData: boolPtr(false)}, false},
		{"has_data true but no prices", GeoResult{PricesAvailable: false, HasData: boolPtr(true)}, false},
		{"has_data wins over a stale date", GeoResult{PricesAvailable: true, HasData: boolPtr(false), CurrentPeriod: Date("2026-08-01")}, false},
		{"neither published", GeoResult{PricesAvailable: true}, false},
	}
	for _, tc := range cases {
		if got := tc.row.Queryable(); got != tc.query {
			t.Errorf("%s: Queryable = %v, want %v", tc.name, got, tc.query)
		}
	}
}

func boolPtr(v bool) *bool { return &v }

// ─────────────────────────────────────────────────────────────────────────────
// --json re-encodes these structs, so what they DROP or INVENT is what an
// agent reading --json sees. Both contracts must round-trip faithfully.
// ─────────────────────────────────────────────────────────────────────────────

// reencode decodes a body into T and encodes it back — exactly the path
// `--json` takes.
func reencode[T any](t *testing.T, body string) string {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(body), &v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return string(out)
}

// ⚠️ THE BUG THIS PINS: a current API sends has_data and NO current_period. A
// NullableDate re-encodes its absent state as `null`, so --json used to print
// `"current_period":null` on every row — "nothing this period", everywhere,
// about places the server said HAD data — while dropping has_data outright.
func TestJSONKeepsHasDataAndInventsNoCurrentPeriod(t *testing.T) {
	body := `{"results":[
	  {"kind":"country","geo_id":"es","name":"ES","level":"country","country":"es",
	   "ancestors":[],"prices_available":true,"has_data":true},
	  {"kind":"country","geo_id":"me","name":"ME","level":"country","country":"me",
	   "ancestors":[],"prices_available":true,"has_data":false}]}`
	out := reencode[GeoResponse](t, body)

	if strings.Contains(out, "current_period") {
		t.Errorf("--json invented current_period, which the server never sent:\n%s", out)
	}
	if !strings.Contains(out, `"has_data":true`) || !strings.Contains(out, `"has_data":false`) {
		t.Errorf("--json dropped has_data:\n%s", out)
	}
}

// The old contract must still round-trip unchanged — this CLI reaches servers
// that have not been upgraded — with its three states intact.
func TestJSONKeepsTheOldCurrentPeriodStates(t *testing.T) {
	body := `{"results":[
	  {"kind":"country","geo_id":"es","name":"ES","level":"country","country":"es",
	   "ancestors":[],"prices_available":true,"current_period":"2026-08-01"},
	  {"kind":"country","geo_id":"me","name":"ME","level":"country","country":"me",
	   "ancestors":[],"prices_available":true,"current_period":null}]}`
	out := reencode[GeoResponse](t, body)

	if !strings.Contains(out, `"current_period":"2026-08-01"`) || !strings.Contains(out, `"current_period":null`) {
		t.Errorf("--json lost a current_period state:\n%s", out)
	}
	if strings.Contains(out, "has_data") {
		t.Errorf("--json invented has_data, which this server never sent:\n%s", out)
	}
}

// The zones on a /geo/lookup point carry the same signal, and the same bug.
func TestJSONZoneSummaryKeepsHasData(t *testing.T) {
	out := reencode[ZoneSummary](t, `{"level":"city","name":"València","geo_id":"R344953",
	  "ancestors":[],"has_data":true,"hexes_url":null}`)
	if strings.Contains(out, "current_period") || !strings.Contains(out, `"has_data":true`) {
		t.Errorf("zone re-encoded wrongly:\n%s", out)
	}
}

// availability is how the API says WHY stats is empty. Dropping it from --json
// leaves an agent with an empty array and no way to tell "wait for the build"
// from "broaden your filters". reason must stay a literal null on "ok".
func TestJSONKeepsAvailability(t *testing.T) {
	empty := reencode[CurrentStats](t, `{"stats":[],"availability":{
	  "status":"no_data","reason":"no_data_for_selection","message":"Your filters matched nothing.",
	  "searched_from":"2025-09-01","earliest_nonempty_period":"2025-10-01","latest_nonempty_period":"2026-08-01",
	  "segment":{"ad_type":"real_estate_residential","ad_sub_type":"buy","rooms":null},"note":"n"}}`)
	for _, want := range []string{
		`"reason":"no_data_for_selection"`, `"earliest_nonempty_period":"2025-10-01"`,
		`"latest_nonempty_period":"2026-08-01"`, `"searched_from":"2025-09-01"`, `"rooms":null`,
	} {
		if !strings.Contains(empty, want) {
			t.Errorf("--json dropped %s:\n%s", want, empty)
		}
	}

	ok := reencode[CurrentStats](t, `{"stats":[],"availability":{"status":"ok","reason":null,"message":"",
	  "searched_from":null,"earliest_nonempty_period":"2026-08-01","latest_nonempty_period":"2026-08-01",
	  "segment":{"ad_type":"a","ad_sub_type":"b","rooms":null},"note":""}}`)
	if !strings.Contains(ok, `"reason":null`) {
		t.Errorf("reason must stay a literal null on ok:\n%s", ok)
	}

	// An old server sends no availability — the key must not appear.
	if old := reencode[CurrentStats](t, `{"stats":[]}`); strings.Contains(old, "availability") {
		t.Errorf("--json invented availability for a server that never sent it:\n%s", old)
	}

	history := reencode[HistoryResponse](t, `{"series":[{"period":"2026-08-01","stats":[],
	  "availability":{"status":"no_data","reason":"below_minimum_sample","message":"m"}}],
	  "availability":{"status":"no_data","reason":"below_minimum_sample","message":"m",
	  "searched_from":"2025-09-01","earliest_nonempty_period":null,"latest_nonempty_period":null,
	  "segment":{"ad_type":"a","ad_sub_type":"b","rooms":null},"note":""}}`)
	if strings.Count(history, `"reason":"below_minimum_sample"`) != 2 {
		t.Errorf("history must keep both the envelope and the per-point verdict:\n%s", history)
	}
}

func TestReasonOrUnknownReadsAMissingReasonAsNotDetermined(t *testing.T) {
	if got := (AvailabilityVerdict{}).ReasonOrUnknown(); got != "not_determined" {
		t.Errorf("got %q", got)
	}
	r := "period_not_built"
	if got := (AvailabilityVerdict{Reason: &r}).ReasonOrUnknown(); got != r {
		t.Errorf("got %q", got)
	}
}

// ⚠️ low_confidence absent is NOT false — test the key, never the value.
func TestLowConfidenceAbsenceIsDistinctFromFalse(t *testing.T) {
	var absent Stat
	if err := json.Unmarshal([]byte(`{"h3":"613498079267520511","estimated":false}`), &absent); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if absent.LowConfidence != nil {
		t.Error("a multi-cell map request omits low_confidence entirely")
	}

	var stamped Stat
	if err := json.Unmarshal([]byte(`{"h3":"1","estimated":false,"low_confidence":false}`), &stamped); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if stamped.LowConfidence == nil || *stamped.LowConfidence {
		t.Errorf("LowConfidence = %v, want an explicit false", stamped.LowConfidence)
	}
}

// Hex ids are decimal int64 STRINGS. A client that typed them as numbers would
// lose the low bits of a UInt64.
func TestHexIDsStayStrings(t *testing.T) {
	var page HexPage
	body := `{"geo_id":"R14727511","name":"Russafa","level":"microzone","country":"es","h3_res":8,
	          "hexes":["613498079267520511","613498079269617663"],"next_cursor":null,
	          "prices_available":true,"reports_available":true}`
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if page.Hexes[0] != "613498079267520511" {
		t.Errorf("hex = %q, must round-trip byte for byte", page.Hexes[0])
	}
	if page.HasMore() {
		t.Error("next_cursor null is the end of the listing")
	}
}
