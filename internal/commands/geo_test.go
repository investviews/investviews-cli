package commands

import (
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// ─────────────────────────────────────────────────────────────────────────────
// geo browse — the cascade
// ─────────────────────────────────────────────────────────────────────────────

const countriesBody = `{"results":[
  {"kind":"country","geo_id":"es","name":"ES","level":"country","country":"es",
   "ancestors":[],"prices_available":true,"current_period":"2026-08-01"},
  {"kind":"country","geo_id":"jp","name":"JP","level":"country","country":"jp",
   "ancestors":[],"prices_available":true,"current_period":null},
  {"kind":"country","geo_id":"zz","name":"ZZ","level":"country","country":"zz",
   "ancestors":[],"prices_available":false,"current_period":null}]}`

func TestBrowseRootPrintsSpendableIDsAndTheThreeStateSignal(t *testing.T) {
	res := run(t, freeJSON(countriesBody), "geo", "browse")
	if res.err != nil {
		t.Fatalf("browse: %v", res.err)
	}
	contains(t, res.stdout, "The countries this API serves")
	contains(t, res.stdout, "GEO_ID")

	// The three states must read as three different answers, not as a
	// boolean printed three times.
	contains(t, res.stdout, "prices, current period 2026-08-01 — ask stats current")
	contains(t, res.stdout, "prices, but nothing in the current period — try stats history, not a dead end")
	contains(t, res.stdout, "no prices — dead end, do not spend a metered call")

	// The root call must not invent a parent or a level.
	if res.lastQuery() != "" {
		t.Errorf("root browse sent query %q; it must send neither parent nor level", res.lastQuery())
	}
}

// ⚠️ THE API REPLACED current_period WITH has_data ON 2026-09-20. Against the
// new API the CLI read an absent current_period and printed "this endpoint
// does not publish a current period" on EVERY row — the skill's whole
// ask/try-history/stop signal gone, with no error to notice. The same three
// answers must come back from has_data.
const countriesHasDataBody = `{"results":[
  {"kind":"country","geo_id":"es","name":"ES","level":"country","country":"es",
   "ancestors":[],"prices_available":true,"has_data":true},
  {"kind":"country","geo_id":"me","name":"ME","level":"country","country":"me",
   "ancestors":[],"prices_available":true,"has_data":false},
  {"kind":"country","geo_id":"zz","name":"ZZ","level":"country","country":"zz",
   "ancestors":[],"prices_available":false,"has_data":false}]}`

func TestBrowseReadsHasDataAsTheThreeStateSignal(t *testing.T) {
	res := run(t, freeJSON(countriesHasDataBody), "geo", "browse")
	if res.err != nil {
		t.Fatalf("browse: %v", res.err)
	}
	contains(t, res.stdout, "prices, data in the current period — ask stats current")
	contains(t, res.stdout, "prices, but nothing in the current period — try stats history, not a dead end")
	contains(t, res.stdout, "no prices — dead end, do not spend a metered call")
	if strings.Contains(res.stdout, "does not publish a current period") {
		t.Errorf("has_data was published, so the signal must not read as missing:\n%s", res.stdout)
	}
}

// --json must carry has_data through and must not invent a current_period the
// server never sent.
func TestBrowseJSONCarriesHasData(t *testing.T) {
	res := run(t, freeJSON(countriesHasDataBody), "geo", "browse", "--json")
	if res.err != nil {
		t.Fatalf("browse --json: %v", res.err)
	}
	contains(t, res.stdout, `"has_data": true`)
	contains(t, res.stdout, `"has_data": false`)
	if strings.Contains(res.stdout, "current_period") {
		t.Errorf("--json invented current_period:\n%s", res.stdout)
	}
}

// ⚠️ THE CENTRAL TEST FOR --level's TWO MEANINGS. Under a country the flag is
// a whole-level jump; under a place it filters direct children. The header
// line must say which one happened, or the output of the two is
// indistinguishable after the fact.
func TestBrowseNamesWhichMeaningOfLevelWasUsed(t *testing.T) {
	countryBody := `{"parent":{"kind":"country","geo_id":"es","name":"ES","level":"country","country":"es",
	  "ancestors":[],"prices_available":true,"current_period":"2026-08-01"},
	 "results":[{"kind":"zone","geo_id":"R344953","name":"València","level":"city","country":"es",
	  "ancestors":[{"geo_id":"es","name":"ES","level":"country"}],
	  "prices_available":true,"current_period":"2026-08-01"}]}`

	res := run(t, freeJSON(countryBody), "geo", "browse", "--parent", "es", "--level", "city")
	if res.err != nil {
		t.Fatalf("browse: %v", res.err)
	}
	contains(t, res.stdout, "Every city in ES — a WHOLE-LEVEL jump")
	contains(t, res.stdout, "not only from the top of its tree")
	if res.lastQuery() != "level=city&parent=es" {
		t.Errorf("query = %q", res.lastQuery())
	}

	placeBody := `{"parent":{"kind":"zone","geo_id":"R349055","name":"Comunidad de Madrid","level":"region",
	  "country":"es","ancestors":[{"geo_id":"es","name":"ES","level":"country"}],
	  "prices_available":true,"current_period":"2026-08-01"},
	 "results":[{"kind":"zone","geo_id":"R5326784","name":"Madrid","level":"city","country":"es",
	  "ancestors":[{"geo_id":"R349055","name":"Comunidad de Madrid","level":"region"},
	               {"geo_id":"es","name":"ES","level":"country"}],
	  "prices_available":true,"current_period":"2026-08-01"}]}`

	res = run(t, freeJSON(placeBody), "geo", "browse", "--parent", "R349055", "--level", "city")
	if res.err != nil {
		t.Fatalf("browse: %v", res.err)
	}
	contains(t, res.stdout, "Direct children of Comunidad de Madrid, filtered to level city.")
	missing(t, res.stdout, "WHOLE-LEVEL jump")
}

// ⚠️ browse must NOT inject a level of its own. Doing so would make the flag
// mean one thing at depth 1 and another at depth 2 inside this CLI, and would
// hide region and province entirely.
func TestBrowseNeverInjectsALevel(t *testing.T) {
	body := `{"parent":{"kind":"country","geo_id":"es","name":"ES","level":"country","country":"es",
	  "ancestors":[],"prices_available":true,"current_period":"2026-08-01"},
	 "results":[{"kind":"zone","geo_id":"R349055","name":"Comunidad de Madrid","level":"region",
	  "country":"es","ancestors":[{"geo_id":"es","name":"ES","level":"country"}],
	  "prices_available":true,"current_period":"2026-08-01"}]}`

	res := run(t, freeJSON(body), "geo", "browse", "--parent", "es")
	if res.err != nil {
		t.Fatalf("browse: %v", res.err)
	}
	if res.lastQuery() != "parent=es" {
		t.Errorf("query = %q; browse must not add a level the caller did not ask for", res.lastQuery())
	}
	contains(t, res.stdout, "The top level of ES — its direct children.")
	// region must be visible, not hidden behind an injected --level city.
	contains(t, res.stdout, "Comunidad de Madrid")
}

// ⚠️ LEVELS ARE SKIPPED, NOT SHIFTED. An empty result is a normal answer and
// must exit 0 — an agent that reads it as a failure abandons a place that
// plainly has children.
func TestBrowseEmptyLevelIsANormalAnswerNotAnError(t *testing.T) {
	body := `{"parent":{"kind":"zone","geo_id":"R344953","name":"València","level":"city","country":"es",
	  "ancestors":[{"geo_id":"es","name":"ES","level":"country"}],
	  "prices_available":true,"current_period":"2026-08-01"},
	 "results":[]}`

	res := run(t, freeJSON(body), "geo", "browse", "--parent", "R344953", "--level", "microzone")
	if res.err != nil {
		t.Fatalf("an empty level must not be an error, got: %v", res.err)
	}
	contains(t, res.stdout, "That is a NORMAL answer, not an error.")
	contains(t, res.stdout, "Levels are SKIPPED, not shifted")
	contains(t, res.stdout, "Next: drop --level")
	// Still a free call, and it must say so.
	contains(t, res.stdout, "cost: FREE — group metadata, uncounted")
}

func TestBrowseEmptyWithoutALevelEndsTheWalk(t *testing.T) {
	body := `{"parent":{"kind":"zone","geo_id":"R1","name":"Somewhere","level":"microzone","country":"es",
	  "ancestors":[{"geo_id":"es","name":"ES","level":"country"}],"prices_available":true,
	  "current_period":"2026-08-01"},"results":[]}`

	res := run(t, freeJSON(body), "geo", "browse", "--parent", "R1")
	if res.err != nil {
		t.Fatalf("browse: %v", res.err)
	}
	contains(t, res.stdout, "the walk ends here")
	contains(t, res.stdout, "stats current --geo-id")
}

// /geo publishes no paging metadata, so --all reads until a short page.
func TestBrowseAllWalksUntilAShortPage(t *testing.T) {
	page := 0
	res := run(t, func(w http.ResponseWriter, r *http.Request) {
		page++
		freeHeaders(w)
		if page == 1 {
			var rows []string
			for i := 0; i < 100; i++ {
				rows = append(rows, fmt.Sprintf(`{"kind":"zone","geo_id":"R%d","name":"P%d","level":"city",
				 "country":"es","ancestors":[],"prices_available":true,"current_period":"2026-08-01"}`, i, i))
			}
			writeBody(w, http.StatusOK, `{"results":[`+strings.Join(rows, ",")+`]}`)
			return
		}
		writeBody(w, http.StatusOK, `{"results":[{"kind":"zone","geo_id":"Rlast","name":"Last","level":"city",
		 "country":"es","ancestors":[],"prices_available":true,"current_period":"2026-08-01"}]}`)
	}, "geo", "browse", "--parent", "es", "--level", "city", "--all")

	if res.err != nil {
		t.Fatalf("browse --all: %v", res.err)
	}
	if res.hits() != 2 {
		t.Errorf("made %d requests, want 2", res.hits())
	}
	contains(t, res.stdout, "101 result(s)")
	contains(t, res.stdout, "2 requests")
}

// ─────────────────────────────────────────────────────────────────────────────
// geo search — names are not unique
// ─────────────────────────────────────────────────────────────────────────────

// ⚠️ THE ANCESTOR-CHAIN TEST THE PLAN REQUIRES. Two places identical on name,
// level and country must be told apart by their chain — and the chain must
// carry an id at every step, because only an id can be spent on the next call.
func TestSearchTellsTwoSameNamedPlacesApartByTheirChain(t *testing.T) {
	body := `{"query":"centro","results":[
	  {"kind":"zone","geo_id":"R100","name":"Centro","level":"macrozone","country":"es",
	   "ancestors":[{"geo_id":"R5326784","name":"Madrid","level":"city"},
	                {"geo_id":"R349055","name":"Comunidad de Madrid","level":"region"},
	                {"geo_id":"es","name":"ES","level":"country"}],
	   "prices_available":true,"current_period":"2026-08-01"},
	  {"kind":"zone","geo_id":"R200","name":"Centro","level":"macrozone","country":"es",
	   "ancestors":[{"geo_id":"R344953","name":"València","level":"city"},
	                {"geo_id":"R349054","name":"Comunitat Valenciana","level":"region"},
	                {"geo_id":"es","name":"ES","level":"country"}],
	   "prices_available":true,"current_period":"2026-08-01"}]}`

	res := run(t, freeJSON(body), "geo", "search", "centro")
	if res.err != nil {
		t.Fatalf("search: %v", res.err)
	}

	first := "Madrid (city, R5326784) › Comunidad de Madrid (region, R349055) › ES (country, es)"
	second := "València (city, R344953) › Comunitat Valenciana (region, R349054) › ES (country, es)"
	contains(t, res.stdout, first)
	contains(t, res.stdout, second)
	if first == second {
		t.Fatal("the two chains must differ; the test is meaningless otherwise")
	}

	// Both rows print the same name and level, so the chain is the ONLY
	// thing separating them — and each carries its own spendable id.
	if strings.Count(res.stdout, "Centro — macrozone, es") != 2 {
		t.Errorf("expected two identically-labelled hits\n%s", res.stdout)
	}
	contains(t, res.stdout, "geo_id: R100")
	contains(t, res.stdout, "geo_id: R200")
}

// ⚠️ A suggestion is an OBJECT carrying a geo_id, not a name. Printing only
// names would send the user back into the search that just missed; the id is
// what they can spend on the next call.
func TestSearchPrintsDidYouMeanOnAMiss(t *testing.T) {
	res := run(t, func(w http.ResponseWriter, _ *http.Request) {
		freeHeaders(w)
		writeBody(w, http.StatusNotFound, `{"error":"unknown_place","message":"Nothing matched.",
		 "docs_url":"https://docs.investviews.ai/errors.html","query":"springfield",
		 "did_you_mean":[
		   {"geo_id":"N386190007","name":"Springfield","level":"microzone","country":"gb",
		    "ancestors":[{"geo_id":"N731914594","name":"Wake Green","level":"macrozone"},
		                 {"geo_id":"R162378","name":"Birmingham","level":"city"},
		                 {"geo_id":"R58447","name":"England","level":"region"}]},
		   {"geo_id":"R9","name":"Springfield","level":"city","country":"us","ancestors":[]}]}`)
	}, "geo", "search", "springfield")

	if res.err == nil {
		t.Fatal("a search miss is a 404 and must still fail")
	}
	contains(t, res.stderr, `No place matched "springfield".`)
	contains(t, res.stderr, "Did you mean one of these 2?")
	// Both ids, because two suggestions share a name and only the id and the
	// chain tell them apart.
	contains(t, res.stderr, "geo_id: N386190007")
	contains(t, res.stderr, "geo_id: R9")
	contains(t, res.stderr, "Birmingham (city, R162378)")
	contains(t, res.stderr, "stats current --geo-id")
}

func TestSearchMissWithNoSuggestionsStillNamesTheNextMove(t *testing.T) {
	res := run(t, func(w http.ResponseWriter, _ *http.Request) {
		freeHeaders(w)
		writeBody(w, http.StatusNotFound, `{"error":"unknown_place","message":"Nothing matched.",
		 "docs_url":"https://docs.investviews.ai/errors.html","query":"zzz","did_you_mean":[]}`)
	}, "geo", "search", "zzz")

	if res.err == nil {
		t.Fatal("want an error")
	}
	contains(t, res.stderr, "The API sent no suggestions")
	contains(t, res.stderr, "investviews geo browse")
}

// ⚠️ --all on search must fail loudly. One page printed under an "everything"
// banner would tell the user they had seen it all.
func TestSearchAllIsRefusedRatherThanFaked(t *testing.T) {
	res := run(t, freeJSON(`{"query":"x","results":[]}`), "geo", "search", "valencia", "--all")
	if res.err == nil {
		t.Fatal("--all on search must be refused")
	}
	contains(t, res.err.Error(), "cannot be paged")
	if res.hits() != 0 {
		t.Errorf("a refused --all must send no request, sent %d", res.hits())
	}
}

func TestSearchPassesItsNarrowingFlags(t *testing.T) {
	res := run(t, freeJSON(`{"query":"centro","results":[]}`),
		"geo", "search", "centro", "--country", "es", "--level", "macrozone", "--limit", "5")
	if res.err != nil {
		t.Fatalf("search: %v", res.err)
	}
	if res.lastQuery() != "country=es&level=macrozone&limit=5&q=centro" {
		t.Errorf("query = %q", res.lastQuery())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// geo lookup
// ─────────────────────────────────────────────────────────────────────────────

// ⚠️ null ancestry is NOT an empty one, and this is the only endpoint that
// sends it. "unknown" means try another selector; "top of the tree" means stop
// walking up.
func TestLookupKeepsUnknownAncestryApartFromRoot(t *testing.T) {
	body := `{"kind":"point","lat":39.4699,"lng":-0.3763,"h3":"613498079267520511","h3_res":8,
	 "source":"h3_zone","country":"es","display_name":"València, Spain","prices_available":true,
	 "reports_available":true,
	 "zones":[
	   {"level":"microzone","name":"Russafa","geo_id":null,"ancestors":null,
	    "current_period":null,"hexes_url":null},
	   {"level":"country","name":"ES","geo_id":"es","ancestors":[],
	    "current_period":"2026-08-01","hexes_url":null}]}`

	res := run(t, freeJSON(body), "geo", "lookup", "--h3", "613498079267520511")
	if res.err != nil {
		t.Fatalf("lookup: %v", res.err)
	}
	contains(t, res.stdout, "ancestry unknown")
	contains(t, res.stdout, "top of the tree")
	contains(t, res.stdout, "Cell 613498079267520511 (res 8)")
	if res.lastQuery() != "h3=613498079267520511" {
		t.Errorf("query = %q", res.lastQuery())
	}
}

// The zones on a lookup carry the same signal. The column reads it from
// whichever contract answered.
func TestLookupZonesReadHasData(t *testing.T) {
	body := `{"kind":"point","lat":39.4699,"lng":-0.3763,"h3":"613498079267520511","h3_res":8,
	 "source":"h3_zone","country":"es","display_name":"València, Spain","prices_available":true,
	 "reports_available":true,
	 "zones":[
	   {"level":"microzone","name":"Russafa","geo_id":"R4231821","ancestors":[],
	    "has_data":false,"hexes_url":null},
	   {"level":"city","name":"València","geo_id":"R344953","ancestors":[],
	    "has_data":true,"hexes_url":null}]}`

	res := run(t, freeJSON(body), "geo", "lookup", "--h3", "613498079267520511")
	if res.err != nil {
		t.Fatalf("lookup: %v", res.err)
	}
	contains(t, res.stdout, "DATA THIS PERIOD")
	contains(t, res.stdout, "no — try history")
	contains(t, res.stdout, "yes")
}

func TestLookupRefusesBothSelectorsBeforeSendingAnything(t *testing.T) {
	res := run(t, nil, "geo", "lookup", "--h3", "613498079267520511", "--lat", "39.4", "--lng", "-0.3")
	if res.err == nil {
		t.Fatal("two selectors must be refused")
	}
	if res.hits() != 0 {
		t.Errorf("a refused request must cost nothing, sent %d requests", res.hits())
	}
	contains(t, res.err.Error(), "drop all but one")
}

func TestLookupSendsTheResolutionOnlyWithThePointForm(t *testing.T) {
	res := run(t, freeJSON(`{"kind":"point","lat":39.4699,"lng":-0.3763,"h3":"1","h3_res":7,
	 "source":"h3_zone","country":"es","display_name":null,"zones":[],"prices_available":false,
	 "reports_available":false}`),
		"geo", "lookup", "--lat", "39.4699", "--lng", "-0.3763", "--res", "7")
	if res.err != nil {
		t.Fatalf("lookup: %v", res.err)
	}
	if res.lastQuery() != "lat=39.4699&lng=-0.3763&res=7" {
		t.Errorf("query = %q", res.lastQuery())
	}
	contains(t, res.stdout, "No zone contains this cell")
}

// ─────────────────────────────────────────────────────────────────────────────
// geo hexes
// ─────────────────────────────────────────────────────────────────────────────

func TestHexesPrintsDecimalIDsAndTheCursorWarning(t *testing.T) {
	body := `{"geo_id":"R344953","name":"València","level":"city","country":"es","h3_res":8,
	 "hexes":["613498076398616575","613498076398616576"],"next_cursor":"eyJrIjoxfQ",
	 "prices_available":true,"reports_available":true}`

	res := run(t, freeJSON(body), "geo", "hexes", "R344953")
	if res.err != nil {
		t.Fatalf("hexes: %v", res.err)
	}
	contains(t, res.stdout, "613498076398616575")
	contains(t, res.stdout, "more cells: --cursor eyJrIjoxfQ")
	contains(t, res.stdout, "that cursor belongs to THIS query")
	if res.requests[0].Path != "/public/v1/geo/R344953/hexes" {
		t.Errorf("path = %q", res.requests[0].Path)
	}
}

func TestHexesTakesTheIDFromEitherTheFlagOrTheArgumentButNotBoth(t *testing.T) {
	body := `{"geo_id":"R1","name":"P","level":"city","country":"es","h3_res":8,"hexes":["1"],
	 "next_cursor":null,"prices_available":true,"reports_available":false}`

	if res := run(t, freeJSON(body), "geo", "hexes", "--geo-id", "R1"); res.err != nil {
		t.Fatalf("--geo-id form: %v", res.err)
	}
	res := run(t, nil, "geo", "hexes", "R1", "--geo-id", "R2")
	if res.err == nil {
		t.Fatal("the geo_id given twice must be refused")
	}
	contains(t, res.err.Error(), "given twice")
	if res.hits() != 0 {
		t.Errorf("sent %d requests for a locally refused call", res.hits())
	}

	res = run(t, nil, "geo", "hexes")
	if res.err == nil {
		t.Fatal("a missing geo_id must be refused")
	}
	contains(t, res.err.Error(), "geo search")
}

func TestHexesAllWalksTheCursorAndStopsOnNull(t *testing.T) {
	page := 0
	res := run(t, func(w http.ResponseWriter, r *http.Request) {
		page++
		freeHeaders(w)
		if page == 1 {
			writeBody(w, http.StatusOK, `{"geo_id":"R1","name":"P","level":"city","country":"es",
			 "h3_res":8,"hexes":["11","12"],"next_cursor":"c1","prices_available":true,
			 "reports_available":false}`)
			return
		}
		writeBody(w, http.StatusOK, `{"geo_id":"R1","name":"P","level":"city","country":"es",
		 "h3_res":8,"hexes":["13"],"next_cursor":null,"prices_available":true,
		 "reports_available":false}`)
	}, "geo", "hexes", "R1", "--all")

	if res.err != nil {
		t.Fatalf("hexes --all: %v", res.err)
	}
	if res.hits() != 2 {
		t.Fatalf("made %d requests, want 2", res.hits())
	}
	contains(t, res.stdout, "3 cell(s) at res 8, every page")
	missing(t, res.stdout, "more cells:")
	contains(t, res.stdout, "2 requests")
	if !strings.Contains(res.lastQuery(), "cursor=c1") {
		t.Errorf("the second page must carry the first page's cursor verbatim, got %q", res.lastQuery())
	}
}

// ⚠️ A full listing cannot start from someone else's cursor: it is bound to
// the query that issued it, so resuming would be refused or, worse, quietly
// partial.
func TestHexesAllRefusesToStartFromACursor(t *testing.T) {
	res := run(t, nil, "geo", "hexes", "R1", "--all", "--cursor", "c1")
	if res.err == nil {
		t.Fatal("--all with --cursor must be refused")
	}
	if res.hits() != 0 {
		t.Errorf("sent %d requests", res.hits())
	}
}

// ─────────────────────────────────────────────────────────────────────────────
// geo hexes --ids-only — the shape a pipe can read
// ─────────────────────────────────────────────────────────────────────────────

// ⚠️ STDOUT MUST BE THE IDS AND NOTHING ELSE. The default listing is written
// for a reader, and piping that into `stats current --h3 -` sent the header,
// the availability sentence, the cursor hint and the cost line to a METERED
// endpoint as if each word were a cell.
func TestHexesIDsOnlyPrintsNothingButTheIDs(t *testing.T) {
	body := `{"geo_id":"R344953","name":"València","level":"city","country":"es","h3_res":8,
	 "hexes":["613498076398616575","613498076402810879"],"next_cursor":null,
	 "prices_available":true,"reports_available":true}`

	res := run(t, freeJSON(body), "geo", "hexes", "R344953", "--ids-only")
	if res.err != nil {
		t.Fatalf("hexes --ids-only: %v", res.err)
	}
	if got := res.stdout; got != "613498076398616575\n613498076402810879\n" {
		t.Errorf("stdout is not a bare id list:\n%q", got)
	}
	// Everything the reader form prints must be gone from stdout.
	for _, unwanted := range []string{"València", "city", "cell(s)", "prices", "cost:", "—"} {
		missing(t, res.stdout, unwanted)
	}
	// ⚠️ but the cost line is still printed, on stderr — a free call and a
	// metered one must never look alike, whatever shape the payload is in.
	contains(t, res.stderr, "cost: FREE — group metadata, uncounted")
}

// The pipe the help text documents, run as a pipe: the ids this command
// prints must be accepted by the command it is piped into.
func TestHexesIDsOnlyOutputIsAcceptedByStatsCurrent(t *testing.T) {
	body := `{"geo_id":"R344953","name":"València","level":"city","country":"es","h3_res":8,
	 "hexes":["613498076398616575","613498076402810879","613498076568485887"],
	 "next_cursor":null,"prices_available":true,"reports_available":true}`

	listing := run(t, freeJSON(body), "geo", "hexes", "R344953", "--all", "--ids-only")
	if listing.err != nil {
		t.Fatalf("hexes --all --ids-only: %v", listing.err)
	}

	stats := runWithStdin(t, meteredJSON(currentBody), strings.NewReader(listing.stdout),
		"stats", "current", "--h3", "-")
	if stats.err != nil {
		t.Fatalf("the documented pipe must work end to end, got: %v", stats.err)
	}
	if stats.hits() != 1 {
		t.Fatalf("hits = %d, want 1", stats.hits())
	}
	want := "h3=613498076398616575%2C613498076402810879%2C613498076568485887"
	if !strings.Contains(stats.lastQuery(), want) {
		t.Errorf("query %q is missing %q", stats.lastQuery(), want)
	}
}

// ⚠️ A PARTIAL PIPE IS THE ONE FAILURE --ids-only CAN STILL CAUSE, and it is
// silent: the receiving command gets a valid list of cells that is only part
// of the place, and answers confidently about the part. It is called out on
// stderr, where it cannot contaminate the stream.
func TestHexesIDsOnlyWarnsOnStderrWhenTheListingIsPartial(t *testing.T) {
	body := `{"geo_id":"R344953","name":"València","level":"city","country":"es","h3_res":8,
	 "hexes":["613498076398616575"],"next_cursor":"eyJrIjoxfQ",
	 "prices_available":true,"reports_available":true}`

	res := run(t, freeJSON(body), "geo", "hexes", "R344953", "--ids-only")
	if res.err != nil {
		t.Fatalf("hexes --ids-only: %v", res.err)
	}
	if res.stdout != "613498076398616575\n" {
		t.Errorf("the warning must not reach stdout:\n%q", res.stdout)
	}
	contains(t, res.stderr, "partial")
	contains(t, res.stderr, "--all")
}

// --json is already machine-readable, and asking for both is not an error.
func TestHexesJSONWinsOverIDsOnly(t *testing.T) {
	body := `{"geo_id":"R1","name":"P","level":"city","country":"es","h3_res":8,
	 "hexes":["613498076398616575"],"next_cursor":null,"prices_available":true,
	 "reports_available":false}`

	res := run(t, freeJSON(body), "geo", "hexes", "R1", "--ids-only", "--json")
	if res.err != nil {
		t.Fatalf("hexes --ids-only --json: %v", res.err)
	}
	if !strings.HasPrefix(strings.TrimSpace(res.stdout), "{") {
		t.Errorf("stdout is not a JSON document:\n%s", res.stdout)
	}
	contains(t, res.stderr, "cost:")
}

// ─────────────────────────────────────────────────────────────────────────────
// the offset walker, at a root that ignores the limit
// ─────────────────────────────────────────────────────────────────────────────

// ⚠️ `geo browse --all --limit 10` used to exit 1. The root of /geo ignores
// both limit and page and answers with every country, so the walker asked for
// page 2, got the same ids, and tripped the stall guard on a query that was
// perfectly well formed. The guard is right and stays; the walker no longer
// reaches it.
func TestBrowseAllSurvivesARootThatIgnoresTheLimit(t *testing.T) {
	res := run(t, func(w http.ResponseWriter, r *http.Request) {
		freeHeaders(w)
		writeBody(w, http.StatusOK, countryListBody(37))
	}, "geo", "browse", "--all", "--limit", "10")

	if res.err != nil {
		t.Fatalf("a server that ignores the limit is not an error: %v", res.err)
	}
	if res.hits() != 1 {
		t.Errorf("hits = %d, want 1 — a page longer than the limit has no page 2", res.hits())
	}
	contains(t, res.stdout, "37 result(s)")
	missing(t, res.stdout, "paging stopped making progress")
}

func countryListBody(n int) string {
	rows := make([]string, 0, n)
	for i := 0; i < n; i++ {
		rows = append(rows, fmt.Sprintf(
			`{"kind":"country","geo_id":"c%d","name":"C%d","level":"country","country":"c%d",`+
				`"ancestors":[],"prices_available":true,"current_period":"2026-08-01",`+
				`"reports_available":true}`, i, i, i))
	}
	return `{"parent":null,"results":[` + strings.Join(rows, ",") + `]}`
}

// ─────────────────────────────────────────────────────────────────────────────
// hints that name what the caller already holds
// ─────────────────────────────────────────────────────────────────────────────

// ⚠️ The caller passed the id as --parent, so a hint reading "<its geo_id>"
// asks it to go and find the thing it is standing on. Spell the id.
func TestEmptyBrowseHintsWithTheIDThisCallUsed(t *testing.T) {
	body := `{"parent":{"geo_id":"R14727511","name":"Russafa","level":"microzone","country":"es",
	 "ancestors":[]},"results":[]}`

	res := run(t, freeJSON(body), "geo", "browse", "--parent", "R14727511")
	if res.err != nil {
		t.Fatalf("an empty listing is not an error: %v", res.err)
	}
	contains(t, res.stdout, "investviews stats current --geo-id R14727511")
	missing(t, res.stdout, "<its geo_id>")

	levelled := run(t, freeJSON(body), "geo", "browse", "--parent", "R14727511", "--level", "microzone")
	if levelled.err != nil {
		t.Fatalf("an empty level filter is not an error: %v", levelled.err)
	}
	contains(t, levelled.stdout, "investviews geo browse --parent R14727511")
}

// ⚠️ This is the one endpoint whose zones can carry no geo_id, and the
// availability line is printed ABOVE the zone table — so offering "spend a
// zone's geo_id" would promise an id the reader is then told does not exist.
// The cell's own id is always there and is always spendable.
func TestLookupOffersTheCellWhenNoZoneCarriesAnID(t *testing.T) {
	unaddressable := `{"kind":"point","lat":39.4699,"lng":-0.3763,"h3":"613498079267520511",
	 "h3_res":8,"source":"h3","country":"es","display_name":null,"prices_available":true,
	 "reports_available":true,
	 "zones":[{"level":"microzone","name":"Russafa","geo_id":null,"ancestors":null,
	   "current_period":null,"hexes_url":null}]}`

	res := run(t, freeJSON(unaddressable), "geo", "lookup", "--h3", "613498079267520511")
	if res.err != nil {
		t.Fatalf("lookup: %v", res.err)
	}
	contains(t, res.stdout, "investviews stats current --h3 613498079267520511")
	contains(t, res.stdout, "no zone here carries a geo_id")

	addressable := strings.Replace(unaddressable, `"geo_id":null`, `"geo_id":"R14727511"`, 1)
	res = run(t, freeJSON(addressable), "geo", "lookup", "--h3", "613498079267520511")
	if res.err != nil {
		t.Fatalf("lookup: %v", res.err)
	}
	contains(t, res.stdout, "or an addressable zone's geo_id below")
}

// The free endpoint gets the same check: a token that is not a cell is
// answered locally by name, not as an unknown_place 404 that reads as "this
// cell is outside our geography".
func TestLookupRefusesATokenThatIsNotACell(t *testing.T) {
	res := run(t, nil, "geo", "lookup", "--h3", "R344953")
	if res.err == nil {
		t.Fatal("a non-cell token must be refused")
	}
	if res.hits() != 0 {
		t.Errorf("sent %d requests for a locally refused call", res.hits())
	}
	contains(t, res.err.Error(), `"R344953"`)
	contains(t, res.err.Error(), "is not an H3 cell id")
}
