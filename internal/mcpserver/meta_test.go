package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// ── coverage ─────────────────────────────────────────────────────────────────

// The second market has prices but names nothing, and no source targets it
// directly: the two cases the text must not flatten into "empty".
const coverageReply = `{"window_days":365,"freshness":"computed live per request",
 "coverage":"the markets this API serves",
 "countries":[
   {"country":"es","h3_resolutions":[4,5,6,7,8],"last_ad_parsed_at":"2026-09-11T23:41:07Z",
    "searchable":true,"prices_available":true,"reports_available":true},
   {"country":"me","h3_resolutions":[8],"last_ad_parsed_at":null,
    "searchable":false,"prices_available":true,"reports_available":false}]}`

func TestCoverageListsEveryMarket(t *testing.T) {
	session, fake := connectFake(t, freeJSON(coverageReply))

	res := callTool(t, session, "coverage", map[string]any{"country": "es"})
	if res.IsError {
		t.Fatalf("coverage failed: %s", textOf(t, res))
	}
	if got := fake.last().Path; got != "/public/v1/coverage" {
		t.Errorf("path = %q, want /public/v1/coverage", got)
	}
	wantQuery(t, fake.last(), map[string]string{"country": "es"})

	var payload struct {
		WindowDays int `json:"window_days"`
		Countries  []struct {
			Country        string  `json:"country"`
			LastAdParsedAt *string `json:"last_ad_parsed_at"`
		} `json:"countries"`
	}
	structured(t, res, &payload)
	if payload.WindowDays != 365 || len(payload.Countries) != 2 {
		t.Fatalf("structuredContent = %+v, want the 2-market payload", payload)
	}
	// null stays null in the payload: it means "no direct source", not "".
	if payload.Countries[1].LastAdParsedAt != nil {
		t.Errorf("last_ad_parsed_at null became %q", *payload.Countries[1].LastAdParsedAt)
	}

	text := textOf(t, res)
	mustContain(t, text, "2 market(s), measured over a 365-day window.")
	mustContain(t, text, "- es — prices: yes, searchable: yes, resolutions: 4,5,6,7,8, last ad parsed: 2026-09-11T23:41:07Z")
	mustContain(t, text, "- me — prices: yes, searchable: no, resolutions: 8, last ad parsed: none — no source targets this market directly, not empty")
	mustContain(t, text, "valid geo_id for market_stats")
	if strings.Contains(text, "cost:") {
		t.Errorf("a free call carries a cost line:\n%s", text)
	}
}

func TestCoverageWithoutCountrySendsNoQuery(t *testing.T) {
	session, fake := connectFake(t, freeJSON(coverageReply))

	if res := callTool(t, session, "coverage", nil); res.IsError {
		t.Fatalf("coverage failed: %s", textOf(t, res))
	}
	wantQuery(t, fake.last(), map[string]string{})
}

// A valid code with no market is an answer, not a failure.
func TestCoverageNoRowForCountryIsAnAnswer(t *testing.T) {
	session, _ := connectFake(t, freeJSON(`{"window_days":365,"freshness":"","coverage":"","countries":[]}`))

	res := callTool(t, session, "coverage", map[string]any{"country": "zz"})
	if res.IsError {
		t.Fatalf("an empty coverage answer came back as an error: %s", textOf(t, res))
	}
	mustContain(t, textOf(t, res), `No market for "zz". That is a normal answer, not an error`)
}

// ── filters ──────────────────────────────────────────────────────────────────

// price publishes bin geometry, not a value list — the shape a text renderer
// that only knows "values" would drop.
const filtersReply = `{"version":"v1","docs_url":"https://docs.investviews.ai/filters.html",
 "range_semantics":"Ranges are inclusive [min, max].",
 "filters":{
   "ad_type":{"values":["real_estate_residential","real_estate_commercial"],"description":"asset class"},
   "ad_sub_type":{"values":["buy","rent"],"description":"deal type"},
   "price":{"grid":"log10","steps_per_decade":40,"base_currency":"USD"}}}`

func TestFiltersListsEveryFilter(t *testing.T) {
	session, fake := connectFake(t, freeJSON(filtersReply))

	res := callTool(t, session, "filters", nil)
	if res.IsError {
		t.Fatalf("filters failed: %s", textOf(t, res))
	}
	if got := fake.last().Path; got != "/public/v1/meta/filters" {
		t.Errorf("path = %q, want /public/v1/meta/filters", got)
	}
	wantQuery(t, fake.last(), map[string]string{})

	// The raw per-filter shapes survive into structuredContent.
	var payload struct {
		Filters map[string]json.RawMessage `json:"filters"`
	}
	structured(t, res, &payload)
	var price struct {
		Grid           string `json:"grid"`
		StepsPerDecade int    `json:"steps_per_decade"`
	}
	if err := json.Unmarshal(payload.Filters["price"], &price); err != nil || price.Grid != "log10" || price.StepsPerDecade != 40 {
		t.Errorf("price filter lost its shape: %s (%v)", payload.Filters["price"], err)
	}

	text := textOf(t, res)
	mustContain(t, text, "Filter vocabulary v1.")
	mustContain(t, text, "ranges: Ranges are inclusive [min, max].")
	mustContain(t, text, "- ad_sub_type: buy, rent")
	mustContain(t, text, "- ad_type: real_estate_residential, real_estate_commercial")
	mustContain(t, text, "- price: not a value list — see structuredContent.filters.price")
	mustContain(t, text, "always USD")
	mustContain(t, text, "docs: https://docs.investviews.ai/filters.html")
	// Sorted, so the list does not move between calls.
	if strings.Index(text, "- ad_sub_type") > strings.Index(text, "- ad_type") {
		t.Errorf("filters are not sorted by name:\n%s", text)
	}
}

// ── usage ────────────────────────────────────────────────────────────────────

const usageReply = `{"token":{"name":"read-only probe","read_only":true,"masked_token":"iv_live_ro_…4f2a",
  "status":"active","created_at":"2026-08-01T10:00:00Z","last_used_at":"2026-09-12T09:14:00Z"},
 "groups":{
   "metadata":{"metered":false,"limit":null,"used":null,"remaining":null,"resets_at":null,
     "rate_per_min":120,"source":"plan","degraded":false},
   "current":{"metered":true,"limit":100000,"used":138,"remaining":99862,
     "resets_at":"2026-10-11T12:30:25Z","rate_per_min":300,"source":"balance","degraded":false},
   "history":{"metered":true,"limit":20000,"used":12,"remaining":19988,
     "resets_at":"2026-10-11T12:30:25Z","rate_per_min":300,"source":"balance","degraded":true},
   "reports":{"metered":false,"limit":null,"used":null,"remaining":null,"resets_at":null,
     "rate_per_min":30,"source":"plan","degraded":false}}}`

func TestUsageGivesRemainingPerGroup(t *testing.T) {
	session, fake := connectFake(t, freeJSON(usageReply))

	res := callTool(t, session, "usage", nil)
	if res.IsError {
		t.Fatalf("usage failed: %s", textOf(t, res))
	}
	if got := fake.last().Path; got != "/public/v1/usage" {
		t.Errorf("path = %q, want /public/v1/usage", got)
	}

	// null counters stay null in the payload — never 0.
	var payload struct {
		Groups map[string]struct {
			Remaining *int64 `json:"remaining"`
		} `json:"groups"`
	}
	structured(t, res, &payload)
	if r := payload.Groups["current"].Remaining; r == nil || *r != 99862 {
		t.Errorf("current.remaining = %v, want 99862", r)
	}
	if r := payload.Groups["metadata"].Remaining; r != nil {
		t.Errorf("metadata.remaining = %d, want null", *r)
	}

	text := textOf(t, res)
	mustContain(t, text, `Token "read-only probe" (iv_live_ro_…4f2a) — active, read-only.`)
	mustContain(t, text, "- current: 99862 remaining of 100000 (used 138), resets 2026-10-11T12:30:25Z — spent by market_stats, compare_places")
	mustContain(t, text, "- history: 19988 remaining of 20000 (used 12), resets 2026-10-11T12:30:25Z — spent by price_history")
	mustContain(t, text, "- metadata: free — uncounted, no limit")
	mustContain(t, text, "- reports: free — uncounted, no limit")
	mustContain(t, text, "separate budgets")
	mustContain(t, text, "⚠️ degraded")
	// ⚠️ An uncounted group must never read as "0 remaining".
	if strings.Contains(text, "0 remaining of —") || strings.Contains(text, "metadata: 0") {
		t.Errorf("an uncounted group reads as spent:\n%s", text)
	}
	// Metered groups first, in a fixed order.
	order := []string{"- current:", "- history:", "- reports:", "- metadata:"}
	for i := 1; i < len(order); i++ {
		if strings.Index(text, order[i-1]) > strings.Index(text, order[i]) {
			t.Errorf("%q printed after %q:\n%s", order[i-1], order[i], text)
		}
	}
}

// ── errors ───────────────────────────────────────────────────────────────────

// A 401 from any meta tool says the token is invalid, and tells the agent the
// command the USER must run — verbatim, since no tool can log in.
func TestMetaToolsReportAnInvalidToken(t *testing.T) {
	for _, tool := range []string{"coverage", "filters", "usage"} {
		t.Run(tool, func(t *testing.T) {
			session, fake := connectFake(t, func(w http.ResponseWriter, _ *http.Request) {
				writeBody(w, http.StatusUnauthorized, `{"error":"invalid_token",`+
					`"message":"The API token is invalid or has been revoked.",`+
					`"docs_url":"https://docs.investviews.ai/errors.html#invalid_token"}`)
			})

			res := callTool(t, session, tool, nil)
			if !res.IsError {
				t.Fatalf("a 401 must come back with isError, got: %s", textOf(t, res))
			}
			if fake.hits() != 1 {
				t.Errorf("sent %d requests, want 1 (a 401 is not retried)", fake.hits())
			}
			text := textOf(t, res)
			mustContain(t, text, "The API token is invalid or has been revoked.")
			mustContain(t, text, "[invalid_token]")
			mustContain(t, text, "https://docs.investviews.ai/errors.html#invalid_token")
			mustContain(t, text, "investviews auth login --token")
			var payload struct {
				Error toolError `json:"error"`
			}
			structured(t, res, &payload)
			if payload.Error.Code != "invalid_token" || payload.Error.Status != 401 {
				t.Errorf("structured error = %+v, want invalid_token 401", payload.Error)
			}
		})
	}
}

// Every meta tool is announced as free and read-only, so an agent checks
// before it spends — and none has a required argument.
func TestMetaToolsAreAnnouncedFree(t *testing.T) {
	session, _ := connectFake(t, nil)

	list, err := session.ListTools(context.Background(), nil)
	if err != nil {
		t.Fatalf("tools/list: %v", err)
	}
	seen := 0
	for _, tool := range list.Tools {
		switch tool.Name {
		case "coverage", "filters", "usage":
		default:
			continue
		}
		seen++
		if !strings.HasPrefix(tool.Description, "FREE") {
			t.Errorf("%s description does not open with FREE: %q", tool.Name, tool.Description)
		}
		a := tool.Annotations
		if a == nil || !a.ReadOnlyHint || !a.IdempotentHint || a.OpenWorldHint == nil || !*a.OpenWorldHint {
			t.Errorf("%s annotations = %+v, want read-only, idempotent, open-world", tool.Name, a)
		}
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
		if len(schema.Required) != 0 {
			t.Errorf("%s requires %v, want nothing required", tool.Name, schema.Required)
		}
	}
	if seen != 3 {
		t.Errorf("found %d meta tools in tools/list, want 3", seen)
	}
}
