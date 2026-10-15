package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
)

// The fake replies below carry the awkward shapes on purpose: ancestors as an
// array, as [] (the root) and as null (unknown); has_data on the current
// contract; current_period set and null on the older one; a zone with no
// geo_id. Each is a shape a tool tested only on the happy row would mangle.

// ── find_place ───────────────────────────────────────────────────────────────

const searchReply = `{"query":"valencia","results":[
 {"kind":"zone","geo_id":"R1","name":"Valencia","level":"city","country":"es",
  "ancestors":[{"geo_id":"R10","name":"Valencia","level":"province"},{"geo_id":"es","name":"Spain","level":"country"}],
  "prices_available":true,"has_data":true},
 {"kind":"zone","geo_id":"R2","name":"Valencia","level":"microzone","country":"es",
  "ancestors":[{"geo_id":"R20","name":"Madrid","level":"city"},{"geo_id":"es","name":"Spain","level":"country"}],
  "prices_available":true,"has_data":false},
 {"kind":"zone","geo_id":"R3","name":"Valencia de Don Juan","level":"city","country":"es",
  "ancestors":[{"geo_id":"es","name":"Spain","level":"country"}],
  "prices_available":false,"has_data":false},
 {"kind":"country","geo_id":"es","name":"Spain","level":"country","country":"es",
  "ancestors":[],"prices_available":true}
]}`

func TestFindPlaceSendsTheQueryAndListsEveryHit(t *testing.T) {
	session, fake := connectFake(t, freeJSON(searchReply))

	res := callTool(t, session, "find_place", map[string]any{
		"q": "valencia", "country": "es", "level": "city", "limit": 5,
	})
	if res.IsError {
		t.Fatalf("find_place failed: %s", textOf(t, res))
	}

	if got := fake.last().Path; got != "/public/v1/geo/search" {
		t.Errorf("path = %q, want /public/v1/geo/search", got)
	}
	wantQuery(t, fake.last(), map[string]string{"q": "valencia", "country": "es", "level": "city", "limit": "5"})

	// structuredContent is the API payload, ancestors intact.
	var payload struct {
		Query   string `json:"query"`
		Results []struct {
			GeoID     string            `json:"geo_id"`
			Ancestors []json.RawMessage `json:"ancestors"`
		} `json:"results"`
	}
	structured(t, res, &payload)
	if payload.Query != "valencia" || len(payload.Results) != 4 {
		t.Fatalf("structuredContent = %+v, want the 4-hit payload", payload)
	}
	if payload.Results[0].GeoID != "R1" || len(payload.Results[0].Ancestors) != 2 {
		t.Errorf("first hit lost its id or ancestry: %+v", payload.Results[0])
	}

	text := textOf(t, res)
	mustContain(t, text, `4 hit(s) for "valencia"`)
	mustContain(t, text, "1. Valencia — city, es")
	mustContain(t, text, "geo_id: R1")
	// The chain, with an id at every step, is what tells the two Valencias apart.
	mustContain(t, text, "in: Valencia (province, R10) › Spain (country, es)")
	mustContain(t, text, "in: Madrid (city, R20) › Spain (country, es)")
	mustContain(t, text, "in: top of the tree — no ancestors")
	// The three-state signal, one per hit.
	mustContain(t, text, "availability: ask_current — prices, data in the current period")
	mustContain(t, text, "availability: try_history — prices, but nothing in the current period — try price_history, not a dead end")
	mustContain(t, text, "availability: dead_end — no prices")
	mustContain(t, text, "availability: unknown")
	// A free call carries no cost line.
	if strings.Contains(text, "cost:") {
		t.Errorf("a free call carries a cost line:\n%s", text)
	}
}

// Unset optional arguments are not sent: the server's own defaults apply.
func TestFindPlaceSendsOnlyTheArgumentsGiven(t *testing.T) {
	session, fake := connectFake(t, freeJSON(`{"query":"x","results":[]}`))

	res := callTool(t, session, "find_place", map[string]any{"q": "ruzafa"})
	if res.IsError {
		t.Fatalf("find_place failed: %s", textOf(t, res))
	}
	wantQuery(t, fake.last(), map[string]string{"q": "ruzafa"})
	// An empty hit list is an answer, not an error.
	mustContain(t, textOf(t, res), "Nothing matched. That is an answer, not a failure")
}

func TestFindPlaceRefusesAnEmptyQueryWithoutARequest(t *testing.T) {
	session, fake := connectFake(t, freeJSON(searchReply))

	res := callTool(t, session, "find_place", map[string]any{"q": "   "})
	if !res.IsError {
		t.Fatal("an empty query must come back with isError")
	}
	mustContain(t, textOf(t, res), "needs a name to resolve")
	if fake.hits() != 0 {
		t.Errorf("an empty query sent %d requests, want 0", fake.hits())
	}
}

// A miss lists the suggestions with geo_ids the agent can spend.
func TestFindPlaceMissListsDidYouMean(t *testing.T) {
	session, _ := connectFake(t, func(w http.ResponseWriter, _ *http.Request) {
		freeHeaders(w)
		writeBody(w, http.StatusNotFound, `{"error":"unknown_place","message":"No place matched zaragosa.",`+
			`"docs_url":"https://docs.investviews.ai/errors.html#unknown_place","query":"zaragosa","did_you_mean":[`+
			`{"geo_id":"R50","name":"Zaragoza","level":"city","country":"es","ancestors":[]}]}`)
	})

	res := callTool(t, session, "find_place", map[string]any{"q": "zaragosa"})
	if !res.IsError {
		t.Fatal("unknown_place must come back with isError")
	}
	text := textOf(t, res)
	mustContain(t, text, "No place matched zaragosa.")
	mustContain(t, text, "https://docs.investviews.ai/errors.html#unknown_place")
	mustContain(t, text, "Zaragoza (city, es) — geo_id R50")
}

// ── browse_places ────────────────────────────────────────────────────────────

// An older server's shape: no has_data, current_period set on one row and
// null on another. The signal must still split them.
const browseCountryReply = `{
 "parent":{"kind":"country","geo_id":"es","name":"Spain","level":"country","country":"es","ancestors":[],"prices_available":true},
 "results":[
  {"kind":"zone","geo_id":"R1","name":"Valencia","level":"city","country":"es",
   "ancestors":[{"geo_id":"es","name":"Spain","level":"country"}],"prices_available":true,"current_period":"2026-09-01"},
  {"kind":"zone","geo_id":"R4","name":"Teruel","level":"city","country":"es",
   "ancestors":[{"geo_id":"es","name":"Spain","level":"country"}],"prices_available":true,"current_period":null}
 ]}`

func TestBrowsePlacesUnderACountry(t *testing.T) {
	session, fake := connectFake(t, freeJSON(browseCountryReply))

	res := callTool(t, session, "browse_places", map[string]any{
		"parent": "es", "level": "city", "limit": 2, "page": 3,
	})
	if res.IsError {
		t.Fatalf("browse_places failed: %s", textOf(t, res))
	}
	if got := fake.last().Path; got != "/public/v1/geo" {
		t.Errorf("path = %q, want /public/v1/geo", got)
	}
	wantQuery(t, fake.last(), map[string]string{"parent": "es", "level": "city", "limit": "2", "page": "3"})

	var payload struct {
		Results []struct {
			GeoID         string  `json:"geo_id"`
			CurrentPeriod *string `json:"current_period"`
		} `json:"results"`
	}
	structured(t, res, &payload)
	if len(payload.Results) != 2 || payload.Results[1].CurrentPeriod != nil {
		t.Errorf("structuredContent lost the null current_period: %+v", payload)
	}

	text := textOf(t, res)
	// Which of level's two meanings applied is said in the answer.
	mustContain(t, text, "Under Spain (country, geo_id es), every city in the country.")
	mustContain(t, text, "Valencia — city, geo_id R1 — availability: ask_current — prices, current period 2026-09-01")
	mustContain(t, text, "Teruel — city, geo_id R4 — availability: try_history")
}

func TestBrowsePlacesUnderAPlaceFiltersChildren(t *testing.T) {
	session, fake := connectFake(t, freeJSON(`{
	 "parent":{"kind":"zone","geo_id":"R1","name":"Valencia","level":"city","country":"es",
	  "ancestors":[{"geo_id":"es","name":"Spain","level":"country"}],"prices_available":true,"has_data":true},
	 "results":[]}`))

	res := callTool(t, session, "browse_places", map[string]any{"parent": "R1", "level": "microzone"})
	if res.IsError {
		t.Fatalf("browse_places failed: %s", textOf(t, res))
	}
	wantQuery(t, fake.last(), map[string]string{"parent": "R1", "level": "microzone"})
	text := textOf(t, res)
	mustContain(t, text, "direct children at level microzone")
	mustContain(t, text, "in: Spain (country, es)")
	// An empty page is a normal answer.
	mustContain(t, text, "No rows. That is a normal answer, not an error")
}

func TestBrowsePlacesWithNoParentListsCountries(t *testing.T) {
	session, fake := connectFake(t, freeJSON(`{"results":[
	 {"kind":"country","geo_id":"es","name":"Spain","level":"country","country":"es","ancestors":[],"prices_available":true}]}`))

	res := callTool(t, session, "browse_places", map[string]any{})
	if res.IsError {
		t.Fatalf("browse_places failed: %s", textOf(t, res))
	}
	wantQuery(t, fake.last(), map[string]string{})
	mustContain(t, textOf(t, res), "The countries this API serves")
	mustContain(t, textOf(t, res), "Spain — country, geo_id es")
}

func TestBrowsePlacesRefusesLimitAndPageOverTheMax(t *testing.T) {
	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"limit", map[string]any{"parent": "es", "limit": 101}, "limit of at most 100"},
		{"page", map[string]any{"parent": "es", "page": 10001}, "page of at most 10000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session, fake := connectFake(t, freeJSON(browseCountryReply))
			res := callTool(t, session, "browse_places", tc.args)
			if !res.IsError {
				t.Fatalf("%s over the max must come back with isError", tc.name)
			}
			mustContain(t, textOf(t, res), tc.want)
			if fake.hits() != 0 {
				t.Errorf("sent %d requests, want 0", fake.hits())
			}
		})
	}
}

// ── place_at_point ───────────────────────────────────────────────────────────

// The one endpoint whose zones can carry ancestors: null and geo_id: null.
const lookupReply = `{"kind":"point","lat":39.4699,"lng":-0.3763,"h3":"88395a1a1bfffff","h3_res":8,
 "source":"h3","country":"es","display_name":"Ruzafa, Valencia","prices_available":true,"reports_available":true,
 "zones":[
  {"level":"microzone","name":"Ruzafa","ancestors":null,"geo_id":null,"hexes_url":null},
  {"level":"city","name":"Valencia","ancestors":[{"geo_id":"es","name":"Spain","level":"country"}],
   "geo_id":"R1","has_data":true,"hexes_url":"/geo/R1/hexes"}
 ]}`

func TestPlaceAtPointSendsTheCoordinateAndListsZones(t *testing.T) {
	session, fake := connectFake(t, freeJSON(lookupReply))

	res := callTool(t, session, "place_at_point", map[string]any{"lat": 39.4699, "lng": -0.3763, "res": 8})
	if res.IsError {
		t.Fatalf("place_at_point failed: %s", textOf(t, res))
	}
	if got := fake.last().Path; got != "/public/v1/geo/lookup" {
		t.Errorf("path = %q, want /public/v1/geo/lookup", got)
	}
	wantQuery(t, fake.last(), map[string]string{"lat": "39.4699", "lng": "-0.3763", "res": "8"})

	var payload struct {
		H3    string `json:"h3"`
		Zones []struct {
			Ancestors json.RawMessage `json:"ancestors"`
			GeoID     *string         `json:"geo_id"`
		} `json:"zones"`
	}
	structured(t, res, &payload)
	if payload.H3 != "88395a1a1bfffff" || len(payload.Zones) != 2 {
		t.Fatalf("structuredContent = %+v, want the lookup payload", payload)
	}
	// ⚠️ null must stay null, not become [] (which would mean "the root").
	if string(payload.Zones[0].Ancestors) != "null" || payload.Zones[0].GeoID != nil {
		t.Errorf("unknown zone lost its null ancestors / geo_id: %s %v", payload.Zones[0].Ancestors, payload.Zones[0].GeoID)
	}

	text := textOf(t, res)
	mustContain(t, text, "Cell 88395a1a1bfffff (res 8) at 39.4699, -0.3763.")
	mustContain(t, text, "Ruzafa, Valencia (es)")
	mustContain(t, text, `spend the cell as market_stats h3 ["88395a1a1bfffff"], or a zone's geo_id below`)
	mustContain(t, text, "Ruzafa — microzone, geo_id none — not addressable, data this period: not stated")
	mustContain(t, text, "ancestry unknown — the API holds no hierarchy row for this zone; try another selector")
	mustContain(t, text, "Valencia — city, geo_id R1, data this period: yes")
}

// Without res, only the coordinate is sent: the server picks the resolution.
func TestPlaceAtPointOmitsAnUnsetRes(t *testing.T) {
	session, fake := connectFake(t, freeJSON(lookupReply))

	res := callTool(t, session, "place_at_point", map[string]any{"lat": 0, "lng": 0})
	if res.IsError {
		t.Fatalf("place_at_point failed: %s", textOf(t, res))
	}
	// 0,0 is a real coordinate and must be sent, not dropped as "unset".
	wantQuery(t, fake.last(), map[string]string{"lat": "0", "lng": "0"})
}

func TestPlaceAtPointWithNoPrices(t *testing.T) {
	session, _ := connectFake(t, freeJSON(`{"kind":"point","lat":1,"lng":2,"h3":"x","h3_res":8,"source":"h3",
	 "country":null,"display_name":null,"zones":[],"prices_available":false,"reports_available":false}`))

	res := callTool(t, session, "place_at_point", map[string]any{"lat": 1, "lng": 2})
	if res.IsError {
		t.Fatalf("place_at_point failed: %s", textOf(t, res))
	}
	text := textOf(t, res)
	mustContain(t, text, "availability: dead_end")
	mustContain(t, text, "No zone contains this point. That is a normal answer")
}

func TestPlaceAtPointRefusesOutOfRangeCoordinatesWithoutARequest(t *testing.T) {
	for _, tc := range []struct {
		name     string
		lat, lng float64
		want     string
	}{
		{"lat high", 139.5, 35.6, "lat must be between -90 and 90, got 139.5 — are lat and lng swapped?"},
		{"lat low", -90.01, 0, "lat must be between -90 and 90"},
		{"lng high", 40, 180.5, "lng must be between -180 and 180, got 180.5"},
		{"lng low", 40, -200, "lng must be between -180 and 180"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			session, fake := connectFake(t, freeJSON(lookupReply))
			res := callTool(t, session, "place_at_point", map[string]any{"lat": tc.lat, "lng": tc.lng})
			if !res.IsError {
				t.Fatal("an out-of-range coordinate must come back with isError")
			}
			mustContain(t, textOf(t, res), tc.want)
			if fake.hits() != 0 {
				t.Errorf("sent %d requests, want 0", fake.hits())
			}
		})
	}
}

// ── tools/list ───────────────────────────────────────────────────────────────

// Only the arguments a tool cannot work without are required. An optional one
// marked required would make the agent invent a value for it.
func TestGeoToolSchemasRequireOnlyWhatTheyNeed(t *testing.T) {
	session, _ := connectFake(t, nil)

	list, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	want := map[string][]string{
		"find_place":     {"q"},
		"browse_places":  nil,
		"place_at_point": {"lat", "lng"},
	}
	seen := 0
	for _, tool := range list.Tools {
		wantReq, isGeo := want[tool.Name]
		if !isGeo {
			continue
		}
		seen++
		raw, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatalf("%s: marshal input schema: %v", tool.Name, err)
		}
		var schema struct {
			Required []string `json:"required"`
		}
		if err := json.Unmarshal(raw, &schema); err != nil {
			t.Fatalf("%s: decode input schema: %v", tool.Name, err)
		}
		got := slices.Clone(schema.Required)
		slices.Sort(got)
		if !slices.Equal(got, wantReq) {
			t.Errorf("%s required = %v, want %v", tool.Name, got, wantReq)
		}

		// Every geo tool is free, read-only, and says so.
		if !strings.HasPrefix(tool.Description, "FREE") {
			t.Errorf("%s description does not open with FREE: %q", tool.Name, tool.Description)
		}
		a := tool.Annotations
		if a == nil || !a.ReadOnlyHint || !a.IdempotentHint || a.OpenWorldHint == nil || !*a.OpenWorldHint {
			t.Errorf("%s annotations = %+v, want read-only, idempotent, open-world", tool.Name, a)
		}
	}
	if seen != len(want) {
		t.Errorf("found %d geo tools in tools/list, want %d", seen, len(want))
	}
}
