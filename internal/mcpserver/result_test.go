package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/investviews/investviews-cli/internal/api"
)

// These tests drive ok() and fail() through a real MCP round trip, with a
// "probe" tool standing in for the tools later tasks add. A helper that only
// passed a unit test could still lose its structuredContent or its isError on
// the wire; this way the client sees exactly what an agent would.

// probeFunc is the body of the probe tool: it gets the per-call client the
// same way a real tool does, and returns ok(...) or fail(...).
type probeFunc func(ctx context.Context, s *server) (*mcp.CallToolResult, any, error)

// callProbe builds a server with one tool, "probe", running fn against
// factory, calls it once, and returns the result the client received.
func callProbe(t *testing.T, factory ClientFactory, fn probeFunc) *mcp.CallToolResult {
	t.Helper()

	s := &server{newClient: factory}
	srv := mcp.NewServer(&mcp.Implementation{Name: Name, Version: testVersion}, nil)
	mcp.AddTool(srv, &mcp.Tool{Name: "probe", Description: "test probe"},
		func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
			return fn(ctx, s)
		})

	session := connectServer(t, srv)
	res, err := session.CallTool(context.Background(), &mcp.CallToolParams{Name: "probe"})
	if err != nil {
		t.Fatalf("tools/call returned a protocol error, want a tool result: %v", err)
	}
	return res
}

// textOf is the concatenated text content of a result.
func textOf(t *testing.T, res *mcp.CallToolResult) string {
	t.Helper()
	var parts []string
	for _, c := range res.Content {
		if tc, isText := c.(*mcp.TextContent); isText {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func mustContain(t *testing.T, got, want string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Errorf("text does not contain %q:\n%s", want, got)
	}
}

// usageProbe is the simplest real call: GET /usage, then ok() or fail().
func usageProbe(ctx context.Context, s *server) (*mcp.CallToolResult, any, error) {
	c, err := s.client()
	if err != nil {
		return fail(err)
	}
	res, err := c.Usage(ctx)
	if err != nil {
		return fail(err)
	}
	return ok(res, "usage fetched", res.Meta)
}

// statsProbe is a metered call: GET /stats/current for one geo_id.
func statsProbe(params api.StatsParams) probeFunc {
	return func(ctx context.Context, s *server) (*mcp.CallToolResult, any, error) {
		c, err := s.client()
		if err != nil {
			return fail(err)
		}
		res, err := c.StatsCurrent(ctx, params)
		if err != nil {
			return fail(err)
		}
		return ok(res, "stats fetched", res.Meta)
	}
}

// ── errors ───────────────────────────────────────────────────────────────────

// An API error keeps everything the server said — message, code, docs_url —
// and adds the next step. The hint is ADDED, never substituted.
func TestFailCarriesMessageDocsURLAndHint(t *testing.T) {
	f := newFakeAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		meteredHeaders(w)
		writeBody(w, http.StatusPaymentRequired, `{"error":"quota_exhausted",`+
			`"message":"Monthly allowance for the current group is spent.",`+
			`"docs_url":"https://docs.investviews.ai/errors.html#quota_exhausted"}`)
	})

	res := callProbe(t, f.factory(t), statsProbe(api.StatsParams{GeoID: "es"}))
	if !res.IsError {
		t.Fatal("a 402 must come back with isError")
	}
	text := textOf(t, res)
	mustContain(t, text, "Monthly allowance for the current group is spent.")
	mustContain(t, text, "[quota_exhausted]")
	mustContain(t, text, "https://docs.investviews.ai/errors.html#quota_exhausted")
	// The hint, with the CLI command rewritten to the tool the agent has.
	mustContain(t, text, "This cycle's allowance for the group is spent.")
	mustContain(t, text, "the `usage` tool")
	if strings.Contains(text, "investviews usage") {
		t.Errorf("the hint still names a CLI command:\n%s", text)
	}
	if res.StructuredContent != nil {
		t.Errorf("a failed call carries structuredContent: %v", res.StructuredContent)
	}
}

// An unknown place lists the suggestions WITH their geo_ids: the id is what
// makes a suggestion something the agent can spend.
func TestFailListsDidYouMeanWithGeoIDs(t *testing.T) {
	f := newFakeAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		freeHeaders(w)
		writeBody(w, http.StatusNotFound, `{"error":"unknown_place","message":"Nothing matched.","docs_url":"d",`+
			`"query":"zaragosa","did_you_mean":[`+
			`{"geo_id":"R1","name":"Zaragoza","level":"city","country":"es","ancestors":[]},`+
			`{"geo_id":"R2","name":"Zamora","level":"city","country":"es","ancestors":null}]}`)
	})

	res := callProbe(t, f.factory(t), func(ctx context.Context, s *server) (*mcp.CallToolResult, any, error) {
		c, err := s.client()
		if err != nil {
			return fail(err)
		}
		if _, err := c.GeoSearch(ctx, api.SearchParams{Q: "zaragosa"}); err != nil {
			return fail(err)
		}
		return ok(nil, "unexpected hit", api.Meta{})
	})
	if !res.IsError {
		t.Fatal("unknown_place must come back with isError")
	}
	text := textOf(t, res)
	mustContain(t, text, "Nothing matched.")
	mustContain(t, text, "Zaragoza (city, es) — geo_id R1")
	mustContain(t, text, "Zamora (city, es) — geo_id R2")
	// "browse from investviews geo" in the hint becomes the tool name.
	mustContain(t, text, "browse from `browse_places`")
}

// A transport failure (the API cannot be reached) is still a tool result, not
// a protocol error that would make the client drop the tool.
func TestFailOnATransportError(t *testing.T) {
	f := newFakeAPI(t, nil)
	factory := f.factory(t)
	f.server.Close() // nothing listens any more

	res := callProbe(t, factory, usageProbe)
	if !res.IsError {
		t.Fatal("a transport error must come back with isError")
	}
	mustContain(t, textOf(t, res), "cannot reach")
}

// A factory error — ErrNoToken in the binary — comes back on the call that
// needed the token, with its own text, and no request is sent.
func TestFailOnAFactoryError(t *testing.T) {
	f := newFakeAPI(t, nil)
	noToken := errors.New(`no API token configured; run "investviews auth login --token …" or set INVESTVIEWS_TOKEN`)

	res := callProbe(t, func() (*api.Client, error) { return nil, noToken }, usageProbe)
	if !res.IsError {
		t.Fatal("a missing token must come back with isError")
	}
	text := textOf(t, res)
	// ⚠️ The login command is for the USER to run, so it stays verbatim —
	// "--token" must not be rewritten the way tool arguments are.
	mustContain(t, text, `run "investviews auth login --token …" or set INVESTVIEWS_TOKEN`)
	if f.hits() != 0 {
		t.Errorf("sent %d requests without a token, want 0", f.hits())
	}
}

// A request refused locally names the tool's arguments, not the CLI's flags,
// and costs nothing: no request reaches the API.
func TestFailRewritesFlagNamesToArgumentNames(t *testing.T) {
	f := newFakeAPI(t, meteredJSON(`{"stats":[]}`))

	res := callProbe(t, f.factory(t), statsProbe(api.StatsParams{}))
	if !res.IsError {
		t.Fatal("a request with no territory must come back with isError")
	}
	text := textOf(t, res)
	mustContain(t, text, "exactly one of h3, geo_id, or lat/lng/radius_km")
	mustContain(t, text, "`find_place`")
	mustContain(t, text, "`browse_places`")
	for _, cli := range []string{"--h3", "--geo-id", "--lat", "--radius-km", "investviews geo"} {
		if strings.Contains(text, cli) {
			t.Errorf("text still names the CLI's %q:\n%s", cli, text)
		}
	}
	if f.hits() != 0 {
		t.Errorf("a locally refused request reached the API %d times", f.hits())
	}
}

func TestToolWordsKeepsTokenFlagAndRewritesTo(t *testing.T) {
	got := toolWords.Replace("use --from and --to; log in with --token x")
	if got != "use from and to; log in with --token x" {
		t.Errorf("got %q", got)
	}
}

// ── cost line ────────────────────────────────────────────────────────────────

// A metered call says what it spent, in Meta.Cost()'s words.
func TestOkAddsTheCostLineOnAMeteredCall(t *testing.T) {
	f := newFakeAPI(t, meteredJSON(`{"currency":"USD","stats":[]}`))

	res := callProbe(t, f.factory(t), statsProbe(api.StatsParams{GeoID: "es"}))
	if res.IsError {
		t.Fatalf("unexpected isError: %s", textOf(t, res))
	}
	text := textOf(t, res)
	mustContain(t, text, "stats fetched")
	mustContain(t, text, "cost: 1 metered request (group current, 138 of 100000 used this cycle)")
}

// A free call gets no cost line: the agent knows which tools are free.
func TestOkAddsNoCostLineOnAFreeCall(t *testing.T) {
	f := newFakeAPI(t, freeJSON(`{"plan":"starter","groups":{}}`))

	res := callProbe(t, f.factory(t), usageProbe)
	if res.IsError {
		t.Fatalf("unexpected isError: %s", textOf(t, res))
	}
	if text := textOf(t, res); strings.Contains(text, "cost:") {
		t.Errorf("a free call printed a cost line:\n%s", text)
	}
}

// No quota headers at all means the server did not say. Silence would read as
// free, so the line says "unknown".
func TestCostTextWithoutQuotaHeadersSaysUnknown(t *testing.T) {
	if got := costText(api.Meta{}); !strings.Contains(got, "unknown") {
		t.Errorf("costText(no headers) = %q", got)
	}
}

// ── structuredContent ────────────────────────────────────────────────────────

// ⚠️ The awkward shapes — ancestors as an array AND as null, current_period
// null, raw JSON filters, segment and bin_approximations — must reach the client as structuredContent, not as
// a protocol error from output-schema validation. Those shapes are why the
// tools use Out = any.
func TestOkCarriesAwkwardPayloadsAsStructuredContent(t *testing.T) {
	geoBody := `{"results":[` +
		`{"kind":"place","geo_id":"R1","name":"Zaragoza","level":"city","country":"es",` +
		`"ancestors":[{"geo_id":"es","name":"Spain","level":"country"}],` +
		`"prices_available":true,"current_period":null},` +
		`{"kind":"place","geo_id":"R9","name":"Nowhere","level":"zone","country":"es",` +
		`"ancestors":null,"prices_available":false,"current_period":null}]}`
	filtersBody := `{"version":"1","docs_url":"d","range_semantics":"inclusive",` +
		`"filters":{"rooms":{"values":["1","2","3+"]},"price":{"log_grid":[1,2.5,10],"extra":{"nested":true}}}}`
	statsBody := `{"currency":"USD","territory":{"selector":"geo_id","geo_id":"R1"},"stats":[],` +
		`"bin_approximations":{"price":{"edges":[1,2]}},` +
		`"availability":{"status":"no_data","reason":"below_minimum_sample","message":"m",` +
		`"segment":{"ad_type":"buy","rooms":["2"]}}}`

	f := newFakeAPI(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/geo"):
			freeJSON(geoBody)(w, r)
			return
		case strings.HasSuffix(r.URL.Path, "/meta/filters"):
			freeJSON(filtersBody)(w, r)
			return
		}
		meteredJSON(statsBody)(w, r)
	})

	res := callProbe(t, f.factory(t), func(ctx context.Context, s *server) (*mcp.CallToolResult, any, error) {
		c, err := s.client()
		if err != nil {
			return fail(err)
		}
		geo, err := c.GeoBrowse(ctx, api.GeoParams{Parent: "es"})
		if err != nil {
			return fail(err)
		}
		filters, err := c.MetaFilters(ctx)
		if err != nil {
			return fail(err)
		}
		stats, err := c.StatsCurrent(ctx, api.StatsParams{GeoID: "R1"})
		if err != nil {
			return fail(err)
		}
		return ok(map[string]any{"geo": geo, "filters": filters, "stats": stats}, "all fetched", stats.Meta)
	})
	if res.IsError {
		t.Fatalf("unexpected isError: %s", textOf(t, res))
	}

	raw, err := json.Marshal(res.StructuredContent)
	if err != nil {
		t.Fatalf("marshal structuredContent: %v", err)
	}
	var got struct {
		Geo struct {
			Results []map[string]json.RawMessage `json:"results"`
		} `json:"geo"`
		Filters struct {
			Filters map[string]json.RawMessage `json:"filters"`
		} `json:"filters"`
		Stats struct {
			BinApproximations map[string]json.RawMessage `json:"bin_approximations"`
			Availability      struct {
				Segment json.RawMessage `json:"segment"`
			} `json:"availability"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("structuredContent is not the payload: %v\n%s", err, raw)
	}
	if len(got.Geo.Results) != 2 {
		t.Fatalf("results = %d, want 2:\n%s", len(got.Geo.Results), raw)
	}
	if a := string(got.Geo.Results[0]["ancestors"]); !strings.Contains(a, `"Spain"`) {
		t.Errorf("array ancestors lost: %s", a)
	}
	// null ancestry is "unknown", not "root": it must stay null, not become [].
	if a := string(got.Geo.Results[1]["ancestors"]); a != "null" {
		t.Errorf("null ancestors = %s, want null", a)
	}
	if p := string(got.Geo.Results[0]["current_period"]); p != "null" {
		t.Errorf("null current_period = %s, want null", p)
	}
	if p := string(got.Filters.Filters["price"]); !strings.Contains(p, `"nested":true`) {
		t.Errorf("raw filters did not pass through: %s", p)
	}
	if b := string(got.Stats.BinApproximations["price"]); !strings.Contains(b, `"edges"`) {
		t.Errorf("raw bin_approximations did not pass through: %s", b)
	}
	if seg := string(got.Stats.Availability.Segment); !strings.Contains(seg, `"rooms":["2"]`) {
		t.Errorf("raw segment did not pass through: %s", seg)
	}
}

// ── retry budget ─────────────────────────────────────────────────────────────

// ⚠️ The per-call client gives up on a long Retry-After instead of blocking
// the tool call: the agent gets a rate-limit error it can read, at once.
func TestClientGivesUpOnALongRetryAfter(t *testing.T) {
	f := newFakeAPI(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "60")
		meteredHeaders(w)
		writeBody(w, http.StatusTooManyRequests, `{"error":"rate_limited","message":"Too many requests.",`+
			`"docs_url":"d","retry_after":60,"limit":300,"group":"current"}`)
	})

	res := callProbe(t, f.factory(t), statsProbe(api.StatsParams{GeoID: "es"}))
	if !res.IsError {
		t.Fatal("a 429 over the retry budget must come back with isError")
	}
	mustContain(t, textOf(t, res), "NOT your quota")
	if f.hits() != 1 {
		t.Errorf("hits = %d, want 1 — a 60s wait is over the %v budget", f.hits(), retryBudget)
	}
}
